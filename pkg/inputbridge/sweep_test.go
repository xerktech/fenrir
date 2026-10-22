package inputbridge

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeMounts is a mount table: the mount points that unmount removes.
type fakeMounts map[string]bool

func (m fakeMounts) under(dir string) ([]string, error) {
	var points []string
	for p := range m {
		if strings.HasPrefix(p, dir+"/") {
			points = append(points, p)
		}
	}
	return points, nil
}

func (m fakeMounts) unmount(path string) error {
	delete(m, path)
	return nil
}

// Sweep removes only directories of Sessions not listed live and older than
// the cutoff, unmounting their nodes first (XERK-2178).
func TestSweepRemovesOnlyOldDirsOfGoneSessions(t *testing.T) {
	const (
		gone   = "0b4c5f0e-6a1d-4e8e-9d0a-1f2e3d4c5b6a"
		live   = "1b4c5f0e-6a1d-4e8e-9d0a-1f2e3d4c5b6a"
		recent = "2b4c5f0e-6a1d-4e8e-9d0a-1f2e3d4c5b6a"
		other  = "not-a-session"
	)
	root := t.TempDir()
	cutoff := time.Now().Add(-time.Hour)
	old := cutoff.Add(-time.Minute)
	for _, name := range []string{gone, live, recent, other} {
		if err := os.MkdirAll(filepath.Join(root, name, "input"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "input", "event3"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if name != recent {
			if err := os.Chtimes(filepath.Join(root, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	m := fakeMounts{filepath.Join(root, gone, "input", "event3"): true}
	err := sweep(root, map[string]bool{live: true}, cutoff, m.under, m.unmount)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("left mounted: %v", m)
	}
	if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
		t.Errorf("gone Session's directory still there: %v", err)
	}
	for _, name := range []string{live, recent, other} {
		if _, err := os.Stat(filepath.Join(root, name, "input", "event3")); err != nil {
			t.Errorf("%s swept: %v", name, err)
		}
	}
}

// A node that won't unmount keeps its directory: removing under a mount
// would fail anyway, and the next bridge retries.
func TestSweepKeepsDirWhoseNodeWontUnmount(t *testing.T) {
	const gone = "0b4c5f0e-6a1d-4e8e-9d0a-1f2e3d4c5b6a"
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, gone, "input", "event3"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := fakeMounts{filepath.Join(root, gone, "input", "event3"): true}
	err := sweep(root, nil, time.Now().Add(time.Hour), m.under, func(string) error { return os.ErrPermission })
	if err == nil {
		t.Error("no error for a failed unmount")
	}
	if _, err := os.Stat(filepath.Join(root, gone)); err != nil {
		t.Errorf("directory removed despite its mount: %v", err)
	}
}

// A dead directory whose input/ is a symlink is removed without clearing
// what the link points at.
func TestSweepDoesNotFollowInputSymlink(t *testing.T) {
	const gone = "0b4c5f0e-6a1d-4e8e-9d0a-1f2e3d4c5b6a"
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "event3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, gone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, gone, "input")); err != nil {
		t.Fatal(err)
	}
	err := sweep(root, nil, time.Now().Add(time.Hour), fakeMounts{}.under, func(path string) error {
		t.Errorf("unmounted %s", path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "event3")); err != nil {
		t.Errorf("followed the link: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
		t.Errorf("dead directory kept: %v", err)
	}
}

// Every mount under a dead directory goes, deepest first, not only the nodes
// in input/: a leftover on talos04 held a tmpfs on runtime/. One that stays
// keeps the directory, as RemoveAll would empty the mounted filesystem.
func TestSweepUnmountsEverythingFirst(t *testing.T) {
	const gone = "0b4c5f0e-6a1d-4e8e-9d0a-1f2e3d4c5b6a"
	root := t.TempDir()
	dir := filepath.Join(root, gone)
	for _, d := range []string{"runtime/sub", "input"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := fakeMounts{
		filepath.Join(dir, "runtime"):        true,
		filepath.Join(dir, "runtime", "sub"): true,
		filepath.Join(dir, "input"):          true,
	}
	var order []string
	unmount := func(p string) error {
		order = append(order, strings.TrimPrefix(p, dir+"/"))
		return m.unmount(p)
	}
	if err := sweep(root, nil, time.Now().Add(time.Hour), m.under, unmount); err != nil {
		t.Fatal(err)
	}
	if i, j := slices.Index(order, "runtime/sub"), slices.Index(order, "runtime"); i < 0 || j < 0 || i > j {
		t.Errorf("unmount order %v, want runtime/sub before runtime", order)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory kept: %v", err)
	}

	// A mount that won't go keeps its directory.
	if err := os.MkdirAll(filepath.Join(dir, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime", "data"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stuck := fakeMounts{filepath.Join(dir, "runtime"): true}
	err := sweep(root, nil, time.Now().Add(time.Hour), stuck.under, func(string) error { return nil })
	if err == nil {
		t.Error("no error for a mount still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime", "data")); err != nil {
		t.Errorf("removed through a mount: %v", err)
	}
}
