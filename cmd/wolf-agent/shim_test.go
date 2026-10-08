package main

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestLoopbackShim preloads shim/wolf_loopback.c into a process that binds the
// way Wolf does (0.0.0.0) and checks only Wolf's HTTP/HTTPS ports move to
// loopback. Python stands in for Wolf: like it, Python binds through libc.
func TestLoopbackShim(t *testing.T) {
	for _, tool := range []string{"cc", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	so := filepath.Join(t.TempDir(), "libwolf_loopback.so")
	if out, err := exec.CommandContext(t.Context(), "cc", "-O2", "-Wall", "-Werror", "-shared", "-fPIC",
		"-o", so, "shim/wolf_loopback.c", "-ldl").CombinedOutput(); err != nil {
		t.Fatalf("building shim: %v\n%s", err, out)
	}

	// Port 0 picks a free port, so ask the kernel for one first and bind that.
	const script = `
import socket, sys
for family, any_addr in ((socket.AF_INET, "0.0.0.0"), (socket.AF_INET6, "::")):
    s = socket.socket(family)
    s.bind((any_addr, int(sys.argv[1])))
    print(s.getsockname()[0])
    s.close()
`
	bound := func(port string, env ...string) []string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "python3", "-c", script, port)
		cmd.Env = append([]string{"LD_PRELOAD=" + so}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bind on %s: %v\n%s", port, err, out)
		}
		return strings.Fields(string(out))
	}
	freePort := func() string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "python3", "-c",
			`import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])`).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}

	http, https, other := freePort(), freePort(), freePort()
	env := []string{"WOLF_HTTP_PORT=" + http, "WOLF_HTTPS_PORT=" + https}
	loopback := []string{"127.0.0.1", "::1"}
	for _, port := range []string{http, https} {
		if got := bound(port, env...); !slices.Equal(got, loopback) {
			t.Errorf("Wolf port %s bound to %v, want %v", port, got, loopback)
		}
	}
	// Wolf keeps the port as an unsigned short: 65536 more is the same port.
	wrapped, err := strconv.Atoi(https)
	if err != nil {
		t.Fatal(err)
	}
	if got := bound(https, "WOLF_HTTPS_PORT="+strconv.Itoa(wrapped+65536)); !slices.Equal(got, loopback) {
		t.Errorf("Wolf port %s given as %d bound to %v, want %v", https, wrapped+65536, got, loopback)
	}
	if got, want := bound(other, env...), []string{"0.0.0.0", "::"}; !slices.Equal(got, want) {
		t.Errorf("other port %s bound to %v, want %v", other, got, want)
	}
}
