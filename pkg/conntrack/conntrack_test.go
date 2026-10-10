package conntrack

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tuple whose sport is the port counts, in either direction; other
// ports, protocols, and flows without counters don't.
func TestUDPFlowsFrom(t *testing.T) {
	table := strings.Join([]string{
		// Client-originated (its pings come first): Wolf's packets are the reply's.
		"ipv4     2 udp      17 119 src=10.0.0.9 dst=10.10.10.34 sport=51000 dport=20004 packets=40 bytes=800 src=10.10.10.34 dst=10.0.0.9 sport=20004 dport=51000 packets=1000 bytes=900000 [ASSURED] mark=0 zone=0 use=2",
		// Wolf-originated.
		"ipv6     10 udp      17 119 src=fd00::1 dst=fd00::9 sport=20004 dport=51001 packets=7 bytes=700 src=fd00::9 dst=fd00::1 sport=51001 dport=20004 packets=1 bytes=10 mark=0 zone=0 use=2",
		// Another session's video port.
		"ipv4     2 udp      17 119 src=10.0.0.8 dst=10.10.10.34 sport=51002 dport=20104 packets=5 bytes=50 src=10.10.10.34 dst=10.0.0.8 sport=20104 dport=51002 packets=999 bytes=1 [ASSURED] mark=0 zone=0 use=2",
		// TCP on the same port number.
		"ipv4     2 tcp      6 300 ESTABLISHED src=10.10.10.34 dst=10.0.0.9 sport=20004 dport=443 packets=50 bytes=1 src=10.0.0.9 dst=10.10.10.34 sport=443 dport=20004 packets=50 bytes=1 [ASSURED] mark=0 zone=0 use=2",
		// Created before accounting was on: no counters.
		"ipv4     2 udp      17 119 src=10.0.0.7 dst=10.10.10.34 sport=51003 dport=20004 src=10.10.10.34 dst=10.0.0.7 sport=20004 dport=51003 [ASSURED] mark=0 zone=0 use=2",
		"",
	}, "\n")
	path := filepath.Join(t.TempDir(), "nf_conntrack")
	if err := os.WriteFile(path, []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := UDPFlowsFrom(path, 20004)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"dst=10.0.0.9 dport=51000": 1000, "dst=fd00::9 dport=51001": 7}
	if !maps.Equal(got, want) {
		t.Errorf("UDPFlowsFrom = %v, want %v", got, want)
	}
	if got, err := UDPFlowsFrom(path, 2000); err != nil || len(got) != 0 {
		t.Errorf("UDPFlowsFrom(prefix port) = %v, %v; want none", got, err)
	}
}

func TestUDPFlowsFromRejectsBadCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nf_conntrack")
	line := "ipv4 2 udp 17 1 src=a dst=b sport=1 dport=2 packets=x bytes=1\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UDPFlowsFrom(path, 1); err == nil {
		t.Error("UDPFlowsFrom accepted a non-numeric counter")
	}
}

func TestCounted(t *testing.T) {
	dir := t.TempDir()
	for value, want := range map[string]bool{"0\n": false, "1\n": true} {
		path := filepath.Join(dir, "acct")
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := Counted(path); err != nil || got != want {
			t.Errorf("Counted(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	if _, err := Counted(filepath.Join(dir, "missing")); err == nil {
		t.Error("Counted of a missing file did not fail")
	}
}
