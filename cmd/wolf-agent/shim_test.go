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
	so := buildLoopbackShim(t)

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
	http, https, other := freePort(t), freePort(t), freePort(t)
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

// TestLoopbackShimDropsWolfConnections checks the shim closes every connection
// to Wolf's HTTP/HTTPS listeners before Wolf can accept it (XERK-1737): node
// loopback is shared by every host-networked pod. A non-blocking accept (asio's)
// must end in EAGAIN once the queue is drained, through both accept4 (Python's
// socket.accept) and plain accept; other listeners are untouched.
func TestLoopbackShimDropsWolfConnections(t *testing.T) {
	so := buildLoopbackShim(t)

	const script = `
import ctypes, errno, socket, sys, threading
libc = ctypes.CDLL(None, use_errno=True)

def listener(port, family=socket.AF_INET):
    s = socket.socket(family)
    # Like asio's acceptor: closed connections leave the port in TIME_WAIT.
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0" if family == socket.AF_INET else "::", port))
    s.listen(8)
    return s

def connect(port, host="127.0.0.1"):
    c = socket.create_connection((host, port))
    c.settimeout(2)
    return c

def closed(c):
    try:
        return c.recv(1) == b""
    except ConnectionResetError:
        return True
    except socket.timeout:
        return False

def accepted(s, plain):
    if plain:
        fd = libc.accept(s.fileno(), None, None)
        return "accepted" if fd >= 0 else errno.errorcode[ctypes.get_errno()]
    try:
        s.accept()
        return "accepted"
    except BlockingIOError:
        return "EAGAIN"

for name in sys.argv[1:]:
    port = int(name)
    for plain in (False, True):
        s = listener(port)
        s.setblocking(False)
        clients = [connect(port) for _ in range(3)]
        got = accepted(s, plain)
        print(port, "plain" if plain else "accept4", got, all(closed(c) for c in clients) if got != "accepted" else "")
        for c in clients:
            c.close()
        s.close()
    s = listener(port, socket.AF_INET6)
    s.setblocking(False)
    c = connect(port, "::1")
    got = accepted(s, False)
    print(port, "ipv6", got, closed(c) if got != "accepted" else "")
    c.close()
    s.close()
    # A blocking accept never returns for a Wolf listener, but still drops.
    s = listener(port)
    kept = []  # an accepted socket that is garbage-collected closes
    def accept_forever():
        try:
            kept.append(s.accept())
        except OSError:
            pass  # s.close() below; an unhandled EBADF would race the output
    threading.Thread(target=accept_forever, daemon=True).start()
    c = connect(port)
    print(port, "blocking", closed(c))
    c.close()
    s.close()
`
	http, https, other := freePort(t), freePort(t), freePort(t)
	cmd := exec.CommandContext(t.Context(), "python3", "-c", script, http, https, other)
	cmd.Env = []string{"LD_PRELOAD=" + so, "WOLF_HTTP_PORT=" + http, "WOLF_HTTPS_PORT=" + https}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := strings.Join([]string{
		http + " accept4 EAGAIN True",
		http + " plain EAGAIN True",
		http + " ipv6 EAGAIN True",
		http + " blocking True",
		https + " accept4 EAGAIN True",
		https + " plain EAGAIN True",
		https + " ipv6 EAGAIN True",
		https + " blocking True",
		other + " accept4 accepted ",
		other + " plain accepted ",
		other + " ipv6 accepted ",
		other + " blocking False",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// buildLoopbackShim compiles shim/wolf_loopback.c the way cmd/wolf-agent's
// Dockerfile does. Python stands in for Wolf in the tests: like it, Python
// binds and accepts through libc.
func buildLoopbackShim(t *testing.T) string {
	t.Helper()
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
	return so
}

// freePort asks the kernel for a free port (binding port 0 picks one).
func freePort(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "python3", "-c",
		`import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])`).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
