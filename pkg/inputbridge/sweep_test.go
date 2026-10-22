package inputbridge

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

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

	var unmounted []string
	err := sweep(root, map[string]bool{live: true}, cutoff, func(path string) error {
		unmounted = append(unmounted, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(root, gone, "input", "event3")}; !slices.Equal(unmounted, want) {
		t.Errorf("unmounted %v, want %v", unmounted, want)
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
	err := sweep(root, nil, time.Now().Add(time.Hour), func(string) error { return os.ErrPermission })
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
	err := sweep(root, nil, time.Now().Add(time.Hour), func(path string) error {
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
