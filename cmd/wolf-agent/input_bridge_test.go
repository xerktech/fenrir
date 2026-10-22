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
	// Root, and not in a user namespace that leaves LockedOwner unmapped.
	if probe, err := os.CreateTemp(t.TempDir(), "probe"); err != nil {
		t.Fatal(err)
	} else if err := probe.Chown(inputbridge.LockedOwner, inputbridge.LockedOwner); err != nil {
		t.Skipf("can't chown to LockedOwner here: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink("/", filepath.Join(dir, "planted")); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := resetUdevDir(dir, tmp); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left %v in tmpDir", left)
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

// control and data are made in tmpDir, outside the App's mounts, and only
// renamed into dir: with no tmpDir to stage in, nothing is created in dir.
func TestResetUdevDirStagesOutsideTheApp(t *testing.T) {
	dir := t.TempDir()
	if err := resetUdevDir(dir, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("reset without a staging directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("made %v in place", entries)
	}
}
