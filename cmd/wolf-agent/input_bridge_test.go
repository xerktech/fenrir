package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"games-on-whales.github.io/direwolf/pkg/inputbridge"
)

// resetUdevDir empties the directory and leaves control and data locked
// against the App (XERK-1920). Needs root to chown.
func TestResetUdevDirLocksControlAndData(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	dir := t.TempDir()
	if err := os.Symlink("/", filepath.Join(dir, "planted")); err != nil {
		t.Fatal(err)
	}
	if err := resetUdevDir(dir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("entries %v, want control and data", entries)
	}
	for _, name := range []string{"control", "data"} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := fi.Sys().(*syscall.Stat_t); st.Uid != inputbridge.LockedOwner || st.Gid != inputbridge.LockedOwner {
			t.Errorf("%s owned %d:%d", name, st.Uid, st.Gid)
		}
	}
}
