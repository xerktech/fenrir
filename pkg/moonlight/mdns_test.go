package moonlight

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

func testAdvertiser() *MDNSAdvertiser {
	return &MDNSAdvertiser{
		instance: "Direwolf._nvstream._tcp.local.",
		host:     "talos04.local.",
		port:     47989,
		addrsOf:  func(net.Interface) ([]net.Addr, error) { return nil, nil },
	}
}

func testNets(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		nets = append(nets, n)
	}
	return nets
}

func query(qs ...dns.Question) *dns.Msg {
	m := new(dns.Msg)
	m.Question = qs
	return m
}

func q(name string, qtype uint16) dns.Question {
	return dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET}
}

func aRecords(rrs []dns.RR) []string {
	var ips []string
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok {
			ips = append(ips, a.A.String())
		}
	}
	return ips
}

// Moonlight browses the service, then resolves the SRV target's A record with
// a separate query; both must be answered.
func TestMDNSReplyAnswersMoonlightDiscovery(t *testing.T) {
	a := testAdvertiser()
	nets := testNets(t, "192.168.50.1/24")

	browse, unicast := a.reply(query(q("_nvstream._tcp.local.", dns.TypePTR)), false, nets)
	if browse == nil || unicast {
		t.Fatalf("browse reply = %v (unicast %v), want a multicast reply", browse, unicast)
	}
	if len(browse.Answer) != 1 {
		t.Fatalf("browse answer = %v, want the instance PTR only", browse.Answer)
	}
	if ptr, ok := browse.Answer[0].(*dns.PTR); !ok || ptr.Ptr != a.instance {
		t.Fatalf("browse answer = %v, want the instance PTR", browse.Answer)
	}
	var srv *dns.SRV
	for _, rr := range browse.Extra {
		if s, ok := rr.(*dns.SRV); ok {
			srv = s
		}
	}
	if srv == nil || srv.Port != 47989 || srv.Target != "talos04.local." {
		t.Fatalf("browse extra SRV = %v, want talos04.local.:47989", srv)
	}
	if got := aRecords(browse.Extra); len(got) != 1 || got[0] != "192.168.50.1" {
		t.Errorf("browse extra A = %v, want [192.168.50.1]", got)
	}

	// moonlight-qt's resolver asks A and AAAA for the host, case aside.
	resolve, _ := a.reply(query(q("TALOS04.local.", dns.TypeA), q("talos04.local.", dns.TypeAAAA)), false, nets)
	if resolve == nil {
		t.Fatal("host A query got no reply")
	}
	if got := aRecords(resolve.Answer); len(got) != 1 || got[0] != "192.168.50.1" {
		t.Errorf("host answer A = %v, want [192.168.50.1]", got)
	}
}

// A reply only ever carries the receiving interface's addresses.
func TestMDNSReplyCarriesOnlyReceivingLinkAddresses(t *testing.T) {
	a := testAdvertiser()
	reply, _ := a.reply(query(q("talos04.local.", dns.TypeA)), false, testNets(t, "10.244.0.1/24"))
	if got := aRecords(reply.Answer); len(got) != 1 || got[0] != "10.244.0.1" {
		t.Errorf("A = %v, want [10.244.0.1] only", got)
	}
	msg := a.unsolicited(net.Interface{}, mdnsTTL)
	if msg != nil && len(aRecords(msg.Answer)) != 0 {
		t.Errorf("announcement on an interface without addresses carries A %v", aRecords(msg.Answer))
	}
}

// However many questions a datagram holds, it gets one reply holding each
// record at most once: the responder is no amplifier.
func TestMDNSReplyIsBoundedPerQuery(t *testing.T) {
	a := testAdvertiser()
	qs := make([]dns.Question, 0, 600)
	for range 200 {
		qs = append(qs, q("_nvstream._tcp.local.", dns.TypeANY), q(a.instance, dns.TypeANY), q(a.host, dns.TypeANY))
	}
	reply, _ := a.reply(query(qs...), false, testNets(t, "192.168.50.1/24"))
	if n := len(reply.Answer) + len(reply.Extra); n != 4 {
		t.Errorf("reply holds %d records, want 4 (PTR, SRV, TXT, A)", n)
	}
}

func TestMDNSReplyIgnoresOthers(t *testing.T) {
	a := testAdvertiser()
	nets := testNets(t, "192.168.50.1/24")
	for name, m := range map[string]*dns.Msg{
		"other service": query(q("_airplay._tcp.local.", dns.TypePTR)),
		"other host":    query(q("other.local.", dns.TypeA)),
		"wrong type":    query(q("talos04.local.", dns.TypeAAAA)),
		"response":      {MsgHdr: dns.MsgHdr{Response: true}, Question: []dns.Question{q("talos04.local.", dns.TypeA)}},
	} {
		if reply, _ := a.reply(m, false, nets); reply != nil {
			t.Errorf("%s: got reply %v, want none", name, reply)
		}
	}
}

// Unicast-response questions and one-shot queriers are answered by unicast;
// the latter as a plain DNS server would, with its ID and question.
func TestMDNSReplyUnicast(t *testing.T) {
	a := testAdvertiser()
	nets := testNets(t, "192.168.50.1/24")
	qu := q("talos04.local.", dns.TypeA)
	qu.Qclass |= mdnsUnicastResponse
	if _, unicast := a.reply(query(qu), false, nets); !unicast {
		t.Error("QU question answered by multicast")
	}

	m := query(q("talos04.local.", dns.TypeA))
	m.Id = 4242
	reply, unicast := a.reply(m, true, nets)
	if !unicast || reply.Id != 4242 || len(reply.Question) != 1 {
		t.Fatalf("legacy reply = %v (unicast %v), want unicast with ID and question", reply, unicast)
	}
	if h := reply.Answer[0].Header(); h.Ttl > mdnsLegacyTTL || h.Class&mdnsCacheFlush != 0 {
		t.Errorf("legacy record header = %v, want TTL <= %d and no cache-flush bit", h, mdnsLegacyTTL)
	}
}

// Queries routed in from other subnets get no answer (RFC 6762 §11).
func TestMDNSOnLink(t *testing.T) {
	nets := testNets(t, "192.168.50.1/24")
	for src, want := range map[string]bool{
		"192.168.50.77": true,
		"169.254.3.4":   true,
		"198.51.100.5":  false,
		"10.244.0.9":    false,
	} {
		if got := onLink(net.ParseIP(src), nets); got != want {
			t.Errorf("onLink(%s) = %v, want %v", src, got, want)
		}
	}
}

func TestMDNSHostName(t *testing.T) {
	for in, want := range map[string]string{
		"talos04":              "talos04.local.",
		"talos04.local":        "talos04.local.",
		"talos04.local.":       "talos04.local.",
		"Talos04.xerktech.com": "Talos04.xerktech.com.local.",
	} {
		if got := mdnsHostName(in); got != want {
			t.Errorf("mdnsHostName(%q) = %q, want %q", in, got, want)
		}
	}
}

type mdnsPacket struct {
	buf []byte
	cm  *ipv4.ControlMessage
	to  net.Addr
}

// fakeMDNSConn feeds the responder queued packets and records what it sends.
type fakeMDNSConn struct {
	in     chan mdnsPacket
	out    chan mdnsPacket
	closed chan struct{}
}

func newFakeMDNSConn() *fakeMDNSConn {
	return &fakeMDNSConn{in: make(chan mdnsPacket), out: make(chan mdnsPacket, 64), closed: make(chan struct{})}
}

func (c *fakeMDNSConn) ReadFrom(b []byte) (int, *ipv4.ControlMessage, net.Addr, error) {
	select {
	case p := <-c.in:
		return copy(b, p.buf), p.cm, p.to, nil
	case <-c.closed:
		return 0, nil, nil, net.ErrClosed
	}
}

// WriteTo fails once closed, as a real socket does, so a goodbye sent after
// Close is lost here too.
func (c *fakeMDNSConn) WriteTo(b []byte, cm *ipv4.ControlMessage, dst net.Addr) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.out <- mdnsPacket{buf: append([]byte(nil), b...), cm: cm, to: dst}
	return len(b), nil
}

func (c *fakeMDNSConn) Close() error {
	close(c.closed)
	return nil
}

// next returns the next packet sent anywhere but the mDNS group, skipping
// the start-up announcements.
func (c *fakeMDNSConn) nextUnicast(t *testing.T) mdnsPacket {
	t.Helper()
	for {
		select {
		case p := <-c.out:
			if p.to.String() != mdnsGroup.String() {
				return p
			}
		case <-time.After(5 * time.Second):
			t.Fatal("responder sent no unicast reply")
		}
	}
}

// serve drops queries from off-link sources and answers on-link ones, and
// Shutdown withdraws the records with a goodbye on every link.
func TestMDNSAdvertiserServeAndShutdown(t *testing.T) {
	conn := newFakeMDNSConn()
	lan := net.Interface{Index: 7, Name: "lan0"}
	addrsOf := func(net.Interface) ([]net.Addr, error) {
		_, n, _ := net.ParseCIDR("192.168.50.0/24")
		n.IP = net.IPv4(192, 168, 50, 1)
		return []net.Addr{n}, nil
	}
	adv := newMDNSAdvertiser(conn, map[int]net.Interface{lan.Index: lan}, addrsOf, "talos04", 47989)

	m := new(dns.Msg)
	m.SetQuestion("talos04.local.", dns.TypeA)
	buf, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	cm := &ipv4.ControlMessage{IfIndex: lan.Index}
	offLink := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 5), Port: 40000}
	onLink := &net.UDPAddr{IP: net.IPv4(192, 168, 50, 9), Port: 40000}
	// serve handles packets in order, so the on-link reply comes after the
	// off-link query was dealt with.
	conn.in <- mdnsPacket{buf: buf, cm: cm, to: offLink}
	conn.in <- mdnsPacket{buf: buf, cm: cm, to: onLink}
	if p := conn.nextUnicast(t); p.to.String() != onLink.String() {
		t.Fatalf("reply went to %s, want only %s (off-link queries get none)", p.to, onLink)
	}

	adv.Shutdown()
	var goodbye bool
	for len(conn.out) > 0 {
		p := <-conn.out
		reply := new(dns.Msg)
		if reply.Unpack(p.buf) != nil || p.to.String() != mdnsGroup.String() || p.cm == nil || p.cm.IfIndex != lan.Index {
			continue
		}
		for _, rr := range reply.Answer {
			if ptr, ok := rr.(*dns.PTR); ok && ptr.Hdr.Ttl == 0 {
				goodbye = true
			}
		}
	}
	if !goodbye {
		t.Error("Shutdown sent no TTL-0 PTR on lan0")
	}
}

// End to end over a real socket: a one-shot multicast query, as
// `dig -p 5353 @224.0.0.251` sends, is answered by unicast with the address of
// the link it went out on.
func TestAdvertiseMDNSAnswersQueries(t *testing.T) {
	adv, err := AdvertiseMDNS("direwolf-test", 39989)
	if err != nil {
		// Another responder (avahi) holding 5353 makes the bind fail.
		t.Skipf("no mDNS socket on this host: %s", err)
	}
	defer adv.Shutdown()

	var ifi net.Interface
	var ip string
	for _, i := range adv.ifaces {
		addrs, _ := i.Addrs()
		if nets := ipv4Nets(addrs); len(nets) > 0 {
			ifi, ip = i, nets[0].IP.String()
		}
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	conn := ipv4.NewPacketConn(udp)
	if err = conn.SetMulticastInterface(&ifi); err != nil {
		t.Fatal(err)
	}
	m := new(dns.Msg)
	m.SetQuestion("direwolf-test.local.", dns.TypeA)
	m.Id = 4242
	buf, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.WriteTo(buf, nil, mdnsGroup); err != nil {
		t.Fatal(err)
	}
	if err := udp.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 9000)
	for {
		n, _, err := udp.ReadFrom(resp)
		if err != nil {
			t.Fatalf("no reply on %s: %s", ifi.Name, err)
		}
		reply := new(dns.Msg)
		if reply.Unpack(resp[:n]) != nil || reply.Id != 4242 {
			continue
		}
		if got := aRecords(reply.Answer); len(got) != 1 || got[0] != ip {
			t.Errorf("A = %v, want [%s]", got, ip)
		}
		return
	}
}
