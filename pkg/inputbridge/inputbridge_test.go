package inputbridge

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeServer records mounts instead of making them, and creates regular
// files for nodes: CI runs without CAP_MKNOD or CAP_SYS_ADMIN.
type fakeServer struct {
	*Server
	mounted map[string]bool
	calls   []string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{Server: NewServer(t.TempDir()), mounted: map[string]bool{}}
	f.bindSelf = func(path string) error {
		f.calls = append(f.calls, "bind "+filepath.Base(path))
		f.mounted[path] = true
		return nil
	}
	f.unmount = func(path string) error {
		if f.mounted[path] {
			f.calls = append(f.calls, "unmount "+filepath.Base(path))
			delete(f.mounted, path)
		}
		return nil
	}
	f.createNode = func(dir string, props map[string]string) error {
		return os.WriteFile(filepath.Join(dir, filepath.Base(props["DEVNAME"])), nil, 0o600)
	}
	return f
}

func event(devname, major string) map[string]string {
	return map[string]string{"DEVNAME": devname, "MAJOR": major, "MINOR": "69"}
}

// A node is created and then bind-mounted onto itself, so it propagates into
// the App pod as a mount of its own; removing unmounts it first.
func TestServerCreateBindsNode(t *testing.T) {
	f := newFakeServer(t)
	if err := f.Handle(Request{Op: OpCreate, Props: event("/dev/input/event5", "13")}); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(f.InputDir, "event5")
	if _, err := os.Stat(node); err != nil || !f.mounted[node] {
		t.Fatalf("node %s: stat %v, mounted %v", node, err, f.mounted[node])
	}
	if err := f.Handle(Request{Op: OpRemove, Props: event("/dev/input/event5", "13")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(node); !os.IsNotExist(err) || f.mounted[node] {
		t.Errorf("after remove: stat %v, mounted %v", err, f.mounted[node])
	}
	if want := []string{"bind event5", "unmount event5"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

// Recreating a node of the same name unmounts the stale one first: removing
// a mount point fails with EBUSY.
func TestServerCreateReplacesMountedNode(t *testing.T) {
	f := newFakeServer(t)
	for range 2 {
		if err := f.Handle(Request{Op: OpCreate, Props: event("/dev/input/event5", "13")}); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"bind event5", "unmount event5", "bind event5"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
}

// Only input nodes, directly in the directory; never another major.
func TestServerRefusesOtherDevices(t *testing.T) {
	f := newFakeServer(t)
	f.createNode = NewServer(f.InputDir).createNode
	for _, props := range []map[string]string{
		event("/dev/hidraw0", "241"),
		event("/dev/input/../sda", "13"),
		event("/dev/input/event5", "8"),
		event("/dev/input/a/b", "13"),
	} {
		if err := f.Handle(Request{Op: OpCreate, Props: props}); err == nil {
			t.Errorf("create %v succeeded", props)
		}
	}
	if len(f.mounted) != 0 {
		t.Errorf("mounted %v", f.mounted)
	}
	if err := f.Handle(Request{Op: "chmod"}); err == nil {
		t.Error("unknown op accepted")
	}
}

// Clear withdraws every node: Wolf destroys a stopped stream's devices without
// unplug events.
func TestServerClear(t *testing.T) {
	f := newFakeServer(t)
	for _, n := range []string{"event5", "event6", "js0"} {
		if err := f.Handle(Request{Op: OpCreate, Props: event("/dev/input/"+n, "13")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Handle(Request{Op: OpClear}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(f.InputDir)
	if len(entries) != 0 || len(f.mounted) != 0 {
		t.Errorf("left %v, mounted %v", entries, f.mounted)
	}
}

// A Client's requests reach the Server, and its errors come back.
func TestClientServerRoundTrip(t *testing.T) {
	f := newFakeServer(t)
	// Short: a unix socket path is limited to ~108 bytes.
	dir, err := os.MkdirTemp("", "ib") //nolint:usetesting // t.TempDir's path is too long for a socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.Serve(ctx, l) }()

	c := &Client{Socket: socket}
	if err := c.Create(event("/dev/input/event5", "13")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := c.Create(event("/dev/input/event6", "8")); err == nil || !strings.Contains(err.Error(), "major") {
		t.Errorf("Create of major 8 = %v, want a refusal", err)
	}
	if err := c.Remove(event("/dev/input/event5", "13")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve = %v", err)
	}
}
