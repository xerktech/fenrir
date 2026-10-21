package inputbridge

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The lock is the boundary against the App's root (it can remount its
// read-only mounts writable): directories and entries must end up owned by
// LockedOwner, directories 0755. Needs root to chown.
func TestLockOwnsDirsAndEntries(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	// Outside the App pod's idmap of 65536 IDs, whatever the constant says.
	if LockedOwner < 1<<16 {
		t.Fatalf("LockedOwner %d is an ID the App pod's idmap maps", LockedOwner)
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "data")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := LockDir(dir); err != nil {
		t.Fatal(err)
	}
	s := NewServer(t.TempDir(), dir, tmp)
	if err := s.Handle(Request{Op: OpWriteHwDB, Filename: "c13:69", Content: []string{"E:ID_INPUT=1"}}); err != nil {
		t.Fatal(err)
	}
	for path, wantMode := range map[string]os.FileMode{dir: os.ModeDir | 0o755, filepath.Join(dir, "c13:69"): 0o644} {
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := fi.Sys().(*syscall.Stat_t)
		if st.Uid != LockedOwner || st.Gid != LockedOwner {
			t.Errorf("%s owned %d:%d, want %d", path, st.Uid, st.Gid, uint32(LockedOwner))
		}
		if fi.Mode() != wantMode {
			t.Errorf("%s mode %v, want %v", path, fi.Mode(), wantMode)
		}
	}
}
