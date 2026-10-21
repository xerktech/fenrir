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
	tmp := t.TempDir()
	f := &fakeServer{Server: NewServer(filepath.Join(tmp, "input"), filepath.Join(tmp, "data"), tmp), mounted: map[string]bool{}}
	for _, d := range []string{f.InputDir, f.UdevDataDir} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
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
	f.checkNode = func(string) error { return nil }
	f.chown = func(*os.File) error { return nil }
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
	f.createNode = NewServer(f.InputDir, f.UdevDataDir, f.TmpDir).createNode
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

// Clear also removes what the App may have left before the bridge locked the
// directory: a directory, a symlink (not followed).
func TestServerClearRemovesPlantedEntries(t *testing.T) {
	f := newFakeServer(t)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.InputDir, "planted", "deep"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.InputDir, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(f.InputDir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("followed the symlink: %v", err)
	}
}

// A path that is not the char device just created is never bind-mounted.
func TestServerRefusesToBindNonDevice(t *testing.T) {
	f := newFakeServer(t)
	f.checkNode = isCharDevice
	if err := f.Handle(Request{Op: OpCreate, Props: event("/dev/input/event5", "13")}); err == nil {
		t.Error("bind-mounted a regular file")
	}
	if len(f.mounted) != 0 {
		t.Errorf("mounted %v", f.mounted)
	}
}

// udev entries are written and removed by name, one path element only, and
// cleared with the nodes.
func TestServerHwDbEntries(t *testing.T) {
	f := newFakeServer(t)
	if err := f.Handle(Request{Op: OpWriteHwDB, Filename: "c13:69", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(f.UdevDataDir, "c13:69"))
	if err != nil || string(got) != "E:ID_INPUT_JOYSTICK=1" {
		t.Fatalf("entry = %q, %v", got, err)
	}
	for _, name := range []string{"", ".", "..", "../control", "a/b"} {
		if err := hwdbName(name); err == nil {
			t.Errorf("hwdbName(%q) accepted", name)
		}
		if err := f.Handle(Request{Op: OpWriteHwDB, Filename: name}); err == nil {
			t.Errorf("wrote entry %q", name)
		}
	}
	if err := f.Handle(Request{Op: OpRemoveHwDB, Filename: "c13:69"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Handle(Request{Op: OpWriteHwDB, Filename: "c13:70"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(f.UdevDataDir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

// An entry is staged in TmpDir and renamed into place: the App can never
// open it before it is locked. Nothing is left in TmpDir.
func TestServerStagesEntriesOutsideTheApp(t *testing.T) {
	f := newFakeServer(t)
	var staged []string
	f.chown = func(file *os.File) error {
		staged = append(staged, filepath.Dir(file.Name()))
		if _, err := os.Stat(filepath.Join(f.UdevDataDir, "c13:69")); !os.IsNotExist(err) {
			t.Errorf("entry visible to the App before its chown: %v", err)
		}
		return nil
	}
	if err := f.Handle(Request{Op: OpWriteHwDB, Filename: "c13:69", Content: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || staged[0] != f.TmpDir {
		t.Errorf("staged in %v, want %s", staged, f.TmpDir)
	}
	if got, _ := os.ReadFile(filepath.Join(f.UdevDataDir, "c13:69")); string(got) != "a\nb" {
		t.Errorf("entry = %q", got)
	}
	entries, _ := os.ReadDir(f.TmpDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".locked-") {
			t.Errorf("left %s in TmpDir", e.Name())
		}
	}
}
