// Package conntrack reads per-flow packet counters from the kernel's
// connection tracking table (/proc/net/nf_conntrack).
package conntrack

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Default paths, in the network namespace of the reading process: a
// host-networked pod sees the node's table.
const (
	TablePath = "/proc/net/nf_conntrack"
	AcctPath  = "/proc/sys/net/netfilter/nf_conntrack_acct"
)

// Counted reports whether the kernel counts packets per flow
// (net.netfilter.nf_conntrack_acct). Without it the table has no counters.
func Counted(acctPath string) (bool, error) {
	b, err := os.ReadFile(acctPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", acctPath, err)
	}
	return strings.TrimSpace(string(b)) == "1", nil
}

// UDPFlowsFrom returns, for each UDP flow in the table at path that sends
// from local port port, the packets it has sent, keyed by its peer
// ("dst=ADDR dport=PORT"): the counter of the tuple (original or reply
// direction) whose sport is port. Flows created before accounting was turned
// on have no counters and are left out. A forwarded flow whose source port
// happens to be port is included too: a caller watching for counts to rise
// can only be misled into seeing progress, never a stall.
func UDPFlowsFrom(path string, port int) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	return udpFlowsFrom(f, port)
}

// udpFlowsFrom parses lines such as
//
//	ipv4 2 udp 17 29 src=A dst=B sport=1 dport=2 packets=3 bytes=4 src=B dst=A sport=2 dport=1 packets=5 bytes=6 [ASSURED] mark=0 use=2
//
// where each tuple's counters follow its ports.
func udpFlowsFrom(r io.Reader, port int) (map[string]uint64, error) {
	want := strconv.Itoa(port)
	flows := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[2] != "udp" {
			continue
		}
		// The current tuple's peer, and whether its sport is port.
		var dst, dport string
		fromPort := false
		for _, f := range fields[3:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch k {
			case "src":
				dst, dport, fromPort = "", "", false
			case "dst":
				dst = v
			case "sport":
				fromPort = v == want
			case "dport":
				dport = v
			case "packets":
				if !fromPort {
					continue
				}
				n, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("parsing %q: %w", f, err)
				}
				flows["dst="+dst+" dport="+dport] += n
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading conntrack table: %w", err)
	}
	return flows, nil
}
