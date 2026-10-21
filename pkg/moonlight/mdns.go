package moonlight

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"k8s.io/klog/v2"
)

// ServerName is the host name moonlight-proxy reports in /serverinfo and
// advertises over mDNS.
const ServerName = "Direwolf"

const (
	// mdnsService is what Moonlight clients browse for on the LAN.
	mdnsService = "_nvstream._tcp.local."
	// mdnsTTL is RFC 6762's recommended TTL for host and service records.
	mdnsTTL = 120
	// mdnsLegacyTTL caps records in replies to one-shot (non-5353) queriers
	// (RFC 6762 §6.7).
	mdnsLegacyTTL = 10
	mdnsPort      = 5353
	// mdnsCacheFlush is the class bit marking a record as unique, so caches
	// replace rather than add to it (RFC 6762 §10.2).
	mdnsCacheFlush = 1 << 15
	// mdnsUnicastResponse is the question class bit asking for a unicast
	// reply (RFC 6762 §5.4).
	mdnsUnicastResponse = 1 << 15
)

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

// MDNSAdvertiser answers Moonlight's LAN discovery (_nvstream._tcp) for the
// HTTP port on every up, multicast-capable interface that has an IPv4
// address.
//
// It is IPv4 only: the proxy refuses to stream to non-IPv4 clients, so a
// client must not be steered to an IPv6 address.
//
// Each reply carries only the addresses of the interface its query arrived on,
// so a LAN client never learns the node's pod-network or other internal
// addresses. Queries from sources off that interface's subnets are dropped
// (RFC 6762 §11), and a query gets at most one reply however many questions
// it holds, so the responder is no amplifier.
type MDNSAdvertiser struct {
	conn     mdnsConn
	ifaces   map[int]net.Interface
	addrsOf  func(net.Interface) ([]net.Addr, error)
	instance string // e.g. Direwolf._nvstream._tcp.local.
	host     string // e.g. talos04.local.
	port     uint16

	shutdown sync.Once
	wg       sync.WaitGroup
	done     chan struct{}
}

// mdnsConn is the subset of *ipv4.PacketConn the responder uses.
type mdnsConn interface {
	ReadFrom(b []byte) (int, *ipv4.ControlMessage, net.Addr, error)
	WriteTo(b []byte, cm *ipv4.ControlMessage, dst net.Addr) (int, error)
	Close() error
}

// AdvertiseMDNS starts advertising host (the node's host name) and port. Call
// Shutdown on the result to withdraw the advertisement.
func AdvertiseMDNS(host string, port int) (*MDNSAdvertiser, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d out of range", port)
	}
	ifaces, err := mdnsInterfaces()
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, errors.New("no up, multicast-capable interface has an IPv4 address")
	}

	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: mdnsPort})
	if err != nil {
		return nil, fmt.Errorf("binding mDNS port: %w", err)
	}
	conn := ipv4.NewPacketConn(udp)
	if err := conn.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("reading receiving interface: %w", err)
	}
	// RFC 6762 §11: receivers check the TTL to drop routed spoofs.
	if err := conn.SetMulticastTTL(255); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("setting multicast TTL: %w", err)
	}
	if err := conn.SetTTL(255); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("setting unicast TTL: %w", err)
	}

	joined := map[int]net.Interface{}
	for _, ifi := range ifaces {
		if err := conn.JoinGroup(&ifi, mdnsGroup); err != nil {
			klog.Warningf("mDNS: not advertising on %s: %s", ifi.Name, err)
			continue
		}
		joined[ifi.Index] = ifi
	}
	if len(joined) == 0 {
		_ = conn.Close()
		return nil, errors.New("joined the mDNS group on no interface")
	}

	addrsOf := func(ifi net.Interface) ([]net.Addr, error) { return ifi.Addrs() }
	a := newMDNSAdvertiser(conn, joined, addrsOf, host, uint16(port))
	for _, ifi := range joined {
		klog.Infof("mDNS: advertising %s (%s) on %s", a.instance, a.host, ifi.Name)
	}
	return a, nil
}

// newMDNSAdvertiser starts answering on conn, which has joined the mDNS group
// on ifaces.
func newMDNSAdvertiser(conn mdnsConn, ifaces map[int]net.Interface, addrsOf func(net.Interface) ([]net.Addr, error), host string, port uint16) *MDNSAdvertiser {
	a := &MDNSAdvertiser{
		conn:     conn,
		ifaces:   ifaces,
		addrsOf:  addrsOf,
		instance: dns.Fqdn(ServerName + "." + mdnsService),
		host:     mdnsHostName(host),
		port:     port,
		done:     make(chan struct{}),
	}
	a.wg.Add(2)
	go a.serve()
	go a.announce()
	return a
}

// Shutdown sends goodbye records and stops answering.
func (a *MDNSAdvertiser) Shutdown() {
	a.shutdown.Do(func() {
		close(a.done)
		for _, ifi := range a.ifaces {
			msg := a.unsolicited(ifi, 0)
			if msg != nil {
				a.send(msg, ifi, nil)
			}
		}
		_ = a.conn.Close()
		a.wg.Wait()
	})
}

// mdnsInterfaces lists the interfaces to answer on. One with no IPv4 address
// (e.g. a pod's host-side veth) has nothing to advertise, and skipping it
// keeps the joins under the kernel's per-socket membership limit
// (net.ipv4.igmp_max_memberships, often 20).
func mdnsInterfaces() ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}
	var ifaces []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if addrs, err := ifi.Addrs(); err == nil && len(ipv4Nets(addrs)) > 0 {
			ifaces = append(ifaces, ifi)
		}
	}
	return ifaces, nil
}

// mdnsHostName turns the node's host name into its .local name.
func mdnsHostName(host string) string {
	host = strings.TrimSuffix(strings.TrimSuffix(host, "."), ".local")
	return dns.Fqdn(host + ".local")
}

// announce sends the two unsolicited responses RFC 6762 §8.3 requires, one
// second apart, on every interface.
func (a *MDNSAdvertiser) announce() {
	defer a.wg.Done()
	for i := range 2 {
		if i > 0 {
			select {
			case <-a.done:
				return
			case <-time.After(time.Second):
			}
		}
		for _, ifi := range a.ifaces {
			if msg := a.unsolicited(ifi, mdnsTTL); msg != nil {
				a.send(msg, ifi, nil)
			}
		}
	}
}

func (a *MDNSAdvertiser) serve() {
	defer a.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, cm, src, err := a.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-a.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			klog.V(4).Infof("mDNS: read: %s", err)
			continue
		}
		udpSrc, ok := src.(*net.UDPAddr)
		if !ok || cm == nil {
			continue
		}
		ifi, ok := a.ifaces[cm.IfIndex]
		if !ok {
			continue
		}
		addrs, err := a.addrsOf(ifi)
		if err != nil {
			continue
		}
		nets := ipv4Nets(addrs)
		if !onLink(udpSrc.IP, nets) {
			continue
		}
		query := new(dns.Msg)
		if err := query.Unpack(buf[:n]); err != nil {
			continue
		}
		reply, unicast := a.reply(query, udpSrc.Port != mdnsPort, nets)
		if reply == nil {
			continue
		}
		if unicast {
			a.send(reply, ifi, udpSrc)
		} else {
			a.send(reply, ifi, nil)
		}
	}
}

// reply builds the one response a query gets, or nil when nothing in it is
// ours. legacy is a one-shot querier (source port not 5353), answered by
// unicast as a plain DNS server would be. nets are the receiving interface's
// IPv4 networks, the only addresses the reply may carry.
func (a *MDNSAdvertiser) reply(query *dns.Msg, legacy bool, nets []*net.IPNet) (*dns.Msg, bool) {
	if query.Response || query.Opcode != dns.OpcodeQuery || len(query.Question) == 0 {
		return nil, false
	}
	var wantPTR, wantSRV, wantTXT, wantA, unicast bool
	for _, q := range query.Question {
		if q.Qclass&mdnsUnicastResponse != 0 {
			unicast = true
		}
		anyType := q.Qtype == dns.TypeANY
		switch {
		case strings.EqualFold(q.Name, mdnsService):
			wantPTR = wantPTR || q.Qtype == dns.TypePTR || anyType
		case strings.EqualFold(q.Name, a.instance):
			wantSRV = wantSRV || q.Qtype == dns.TypeSRV || anyType
			wantTXT = wantTXT || q.Qtype == dns.TypeTXT || anyType
		case strings.EqualFold(q.Name, a.host):
			wantA = wantA || q.Qtype == dns.TypeA || anyType
		}
	}
	if !wantPTR && !wantSRV && !wantTXT && !wantA {
		return nil, false
	}

	ttl := uint32(mdnsTTL)
	flush := uint16(mdnsCacheFlush)
	if legacy {
		ttl, flush = mdnsLegacyTTL, 0
	}
	msg := new(dns.Msg)
	if legacy {
		// A conventional DNS reply: same ID, question echoed.
		msg.SetReply(query)
		for i := range msg.Question {
			msg.Question[i].Qclass &^= mdnsUnicastResponse
		}
	} else {
		msg.Response = true
	}
	msg.Authoritative = true

	// Whatever is asked, the reply holds each record at most once.
	answer := func(rr []dns.RR) { msg.Answer = append(msg.Answer, rr...) }
	extra := func(rr []dns.RR) { msg.Extra = append(msg.Extra, rr...) }
	if wantPTR {
		answer(a.ptr(ttl))
	}
	if wantSRV {
		answer(a.srv(ttl, flush))
	}
	if wantTXT {
		answer(a.txt(ttl, flush))
	}
	if wantA {
		answer(a.addrs(ttl, flush, nets))
	}
	if wantPTR && !wantSRV {
		extra(a.srv(ttl, flush))
	}
	if wantPTR && !wantTXT {
		extra(a.txt(ttl, flush))
	}
	if (wantPTR || wantSRV) && !wantA {
		extra(a.addrs(ttl, flush, nets))
	}
	return msg, legacy || unicast
}

// unsolicited is the full record set for ifi, as announced and (with ttl 0)
// withdrawn.
func (a *MDNSAdvertiser) unsolicited(ifi net.Interface, ttl uint32) *dns.Msg {
	addrs, err := a.addrsOf(ifi)
	if err != nil {
		return nil
	}
	msg := new(dns.Msg)
	msg.Response = true
	msg.Authoritative = true
	msg.Answer = append(msg.Answer, a.ptr(ttl)...)
	msg.Answer = append(msg.Answer, a.srv(ttl, mdnsCacheFlush)...)
	msg.Answer = append(msg.Answer, a.txt(ttl, mdnsCacheFlush)...)
	msg.Answer = append(msg.Answer, a.addrs(ttl, mdnsCacheFlush, ipv4Nets(addrs))...)
	return msg
}

func (a *MDNSAdvertiser) ptr(ttl uint32) []dns.RR {
	return []dns.RR{&dns.PTR{
		Hdr: dns.RR_Header{Name: mdnsService, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
		Ptr: a.instance,
	}}
}

func (a *MDNSAdvertiser) srv(ttl uint32, flush uint16) []dns.RR {
	return []dns.RR{&dns.SRV{
		Hdr:    dns.RR_Header{Name: a.instance, Rrtype: dns.TypeSRV, Class: dns.ClassINET | flush, Ttl: ttl},
		Port:   a.port,
		Target: a.host,
	}}
}

// txt is empty: Moonlight reads nothing from it, but DNS-SD requires the
// record (RFC 6763 §6.1).
func (a *MDNSAdvertiser) txt(ttl uint32, flush uint16) []dns.RR {
	return []dns.RR{&dns.TXT{
		Hdr: dns.RR_Header{Name: a.instance, Rrtype: dns.TypeTXT, Class: dns.ClassINET | flush, Ttl: ttl},
		Txt: []string{""},
	}}
}

func (a *MDNSAdvertiser) addrs(ttl uint32, flush uint16, nets []*net.IPNet) []dns.RR {
	rrs := make([]dns.RR, 0, len(nets))
	for _, n := range nets {
		rrs = append(rrs, &dns.A{
			Hdr: dns.RR_Header{Name: a.host, Rrtype: dns.TypeA, Class: dns.ClassINET | flush, Ttl: ttl},
			A:   n.IP,
		})
	}
	return rrs
}

// send multicasts msg on ifi, or unicasts it to dst.
func (a *MDNSAdvertiser) send(msg *dns.Msg, ifi net.Interface, dst *net.UDPAddr) {
	buf, err := msg.Pack()
	if err != nil {
		klog.V(4).Infof("mDNS: pack: %s", err)
		return
	}
	// A unicast reply is routed: dst is on-link, so that reaches it on the
	// link it asked from.
	var cm *ipv4.ControlMessage
	to := net.Addr(dst)
	if dst == nil {
		cm, to = &ipv4.ControlMessage{IfIndex: ifi.Index}, mdnsGroup
	}
	if _, err := a.conn.WriteTo(buf, cm, to); err != nil {
		klog.V(4).Infof("mDNS: send on %s: %s", ifi.Name, err)
	}
}

// ipv4Nets keeps the non-loopback IPv4 networks of an interface's addresses.
func ipv4Nets(addrs []net.Addr) []*net.IPNet {
	var nets []*net.IPNet
	for _, addr := range addrs {
		n, ok := addr.(*net.IPNet)
		if !ok || n.IP.IsLoopback() {
			continue
		}
		if ip4 := n.IP.To4(); ip4 != nil {
			nets = append(nets, &net.IPNet{IP: ip4, Mask: n.Mask[len(n.Mask)-net.IPv4len:]})
		}
	}
	return nets
}

// onLink reports whether src sits on one of the receiving interface's
// subnets, or is IPv4 link-local. Anything else was routed to us and gets no
// answer (RFC 6762 §11).
func onLink(src net.IP, nets []*net.IPNet) bool {
	if src.IsLinkLocalUnicast() {
		return true
	}
	for _, n := range nets {
		if n.Contains(src) {
			return true
		}
	}
	return false
}
