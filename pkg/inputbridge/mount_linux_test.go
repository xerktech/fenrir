//go:build linux

package inputbridge

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Only mount points at or under dir count, unescaped (XERK-2178).
func TestParseMountsUnder(t *testing.T) {
	const info = `76 1 0:40 / /var rw - xfs /dev/sda rw
3749 76 0:594 / /sessions/4673be1c/runtime rw,nosuid,nodev,relatime shared:543 - tmpfs none rw
3750 76 0:5 /input/event3 /sessions/4673be1c/input/event3 rw shared:1 - devtmpfs dev rw
3751 76 0:5 / /sessions/4673be1c/a\040b rw - tmpfs none rw
3752 76 0:5 / /sessions/4673be1c rw - tmpfs none rw
3753 76 0:5 / /sessions/4673be1c2/runtime rw - tmpfs none rw
`
	got, err := parseMountsUnder(strings.NewReader(info), "/sessions/4673be1c")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/sessions/4673be1c/runtime", "/sessions/4673be1c/input/event3", "/sessions/4673be1c/a b", "/sessions/4673be1c"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The bridge's own directory on exit goes through the same unmount-first
// path; with nothing mounted it is simply removed.
func TestRemoveSessionDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uid")
	if err := os.MkdirAll(filepath.Join(dir, "runtime", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSessionDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory kept: %v", err)
	}
}

// Sweeps of one root take turns: a second waits until the first unlocks.
func TestLockDirExcludes(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan func())
	go func() {
		u, err := lockDir(dir)
		if err != nil {
			t.Error(err)
		}
		locked <- u
	}()
	select {
	case <-locked:
		t.Fatal("second lock taken while the first is held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-locked:
		if u != nil {
			u()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not taken after unlock")
	}
}
