package main

import (
	"bytes"
	"errors"
	"fmt"
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
// The IPv4-mapped any address (::ffff:0.0.0.0) is a wildcard too (XERK-1972);
// specific addresses, mapped or not, are left alone.
func TestLoopbackShim(t *testing.T) {
	so := buildLoopbackShim(t)

	// Port 0 picks a free port, so ask the kernel for one first and bind that.
	const script = `
import ctypes, os, socket, struct, sys
IP_FREEBIND = 15  # lets the documentation address below be bound
for family, addr in ((socket.AF_INET, "0.0.0.0"), (socket.AF_INET6, "::"), (socket.AF_INET6, "::ffff:0.0.0.0"),
                     (socket.AF_INET6, "::ffff:127.0.0.2"), (socket.AF_INET6, "2001:db8::")):
    s = socket.socket(family)
    s.setsockopt(socket.SOL_IP, IP_FREEBIND, 1)
    s.bind((addr, int(sys.argv[1])))
    print(s.getsockname()[0])
    s.close()
# Python always binds with the full sockaddr_in6; Linux also takes the RFC 2133
# length (24, no sin6_scope_id).
libc = ctypes.CDLL(None, use_errno=True)
for addr in ("::", "::ffff:0.0.0.0"):
    s = socket.socket(socket.AF_INET6)
    sa = struct.pack("=HHI16s", socket.AF_INET6, socket.htons(int(sys.argv[1])), 0, socket.inet_pton(socket.AF_INET6, addr))
    if libc.bind(s.fileno(), sa, len(sa)) != 0:
        sys.exit("bind: " + os.strerror(ctypes.get_errno()))
    print(s.getsockname()[0])
    s.close()
`
	bound := func(port string, env ...string) ([]string, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "python3", "-c", script, port)
		cmd.Env = append([]string{"LD_PRELOAD=" + so}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if addrInUse(out) {
				return nil, fmt.Errorf("%w: bind on %s\n%s", errAddrInUse, port, out)
			}
			t.Fatalf("bind on %s: %v\n%s", port, err, out)
		}
		return strings.Fields(string(out)), nil
	}
	// Wildcards move to loopback; the specific addresses stay put.
	narrowed := []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "::ffff:127.0.0.2", "2001:db8::",
		"::1", "::ffff:127.0.0.1"}
	untouched := []string{"0.0.0.0", "::", "::ffff:0.0.0.0", "::ffff:127.0.0.2", "2001:db8::",
		"::", "::ffff:0.0.0.0"}
	withFreePorts(t, func(http, https, other string) error {
		env := []string{"WOLF_HTTP_PORT=" + http, "WOLF_HTTPS_PORT=" + https}
		// Wolf keeps the port as an unsigned short: 65536 more is the same port.
		wrapped, err := strconv.Atoi(https)
		if err != nil {
			t.Fatal(err)
		}
		// Bind everything before comparing, so a retry never follows a failure.
		got := map[string][]string{}
		for name, args := range map[string][]string{
			"http":    append([]string{http}, env...),
			"https":   append([]string{https}, env...),
			"wrapped": {https, "WOLF_HTTPS_PORT=" + strconv.Itoa(wrapped+65536)},
			"other":   append([]string{other}, env...),
		} {
			if got[name], err = bound(args[0], args[1:]...); err != nil {
				return err
			}
		}
		for _, name := range []string{"http", "https"} {
			if !slices.Equal(got[name], narrowed) {
				t.Errorf("Wolf %s port bound to %v, want %v", name, got[name], narrowed)
			}
		}
		if !slices.Equal(got["wrapped"], narrowed) {
			t.Errorf("Wolf port %s given as %d bound to %v, want %v", https, wrapped+65536, got["wrapped"], narrowed)
		}
		if !slices.Equal(got["other"], untouched) {
			t.Errorf("other port %s bound to %v, want %v", other, got["other"], untouched)
		}
		return nil
	})
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
            pass  # s.shutdown() below ends the accept
    t = threading.Thread(target=accept_forever, daemon=True)
    t.start()
    c = connect(port)
    # A Wolf port's accept is still looping in the shim; another port's returned.
    print(port, "blocking", closed(c), "looping" if t.is_alive() else "returned")
    c.close()
    # End the accept before closing: a thread still looping in the shim would
    # re-enter accept on the reused fd number and take the next listener's
    # connection, which it then closes (the flake).
    s.shutdown(socket.SHUT_RDWR)
    t.join(5)
    if t.is_alive():
        sys.exit("blocking accept did not end")
    s.close()
`
	withFreePorts(t, func(http, https, other string) error {
		cmd := exec.CommandContext(t.Context(), "python3", "-c", script, http, https, other)
		cmd.Env = []string{"LD_PRELOAD=" + so, "WOLF_HTTP_PORT=" + http, "WOLF_HTTPS_PORT=" + https}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if addrInUse(out) {
				return fmt.Errorf("%w\n%s", errAddrInUse, out)
			}
			t.Fatalf("%v\n%s", err, out)
		}
		got := strings.TrimSpace(string(out))
		want := strings.Join([]string{
			http + " accept4 EAGAIN True",
			http + " plain EAGAIN True",
			http + " ipv6 EAGAIN True",
			http + " blocking True looping",
			https + " accept4 EAGAIN True",
			https + " plain EAGAIN True",
			https + " ipv6 EAGAIN True",
			https + " blocking True looping",
			other + " accept4 accepted ",
			other + " plain accepted ",
			other + " ipv6 accepted ",
			other + " blocking False returned",
		}, "\n")
		if got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
		return nil
	})
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

var errAddrInUse = errors.New("address already in use")

// addrInUse reports whether a Python script failed binding a taken port.
func addrInUse(out []byte) bool {
	return bytes.Contains(out, []byte("Address already in use"))
}

// withFreePorts runs try with three free ports, and again with fresh ones while
// it returns errAddrInUse. A port freePort released is in the ephemeral range,
// so any process's outgoing connection may take it as its source port before
// the test binds it (XERK-1978). The shim reads the Wolf ports at load, so
// they must be picked before Python starts and cannot come from binding port 0.
// Each retry is logged with its output, so a shim that leaks a listener still
// shows up.
func withFreePorts(t *testing.T, try func(http, https, other string) error) {
	t.Helper()
	var err error
	for attempt := range 5 {
		if attempt > 0 {
			t.Logf("retrying with fresh ports: %v", err)
		}
		// Released ports can be picked again; the three must differ.
		http, https, other := freePort(t), freePort(t), freePort(t)
		if http == https || http == other || https == other {
			err = fmt.Errorf("picked a port twice: %s, %s, %s", http, https, other)
			continue
		}
		if err = try(http, https, other); !errors.Is(err, errAddrInUse) {
			return
		}
	}
	t.Fatalf("no attempt had three free ports; last: %v", err)
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
