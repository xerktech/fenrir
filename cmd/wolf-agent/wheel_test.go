package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/inputbridge"
)

func TestParseOwner(t *testing.T) {
	if uid, gid, err := parseOwner("1000:1001"); err != nil || uid != 1000 || gid != 1001 {
		t.Errorf("parseOwner = %d, %d, %v", uid, gid, err)
	}
	// Root would hand the socket to the App's root only; never meant.
	for _, bad := range []string{"", "1000", "0:0", "1000:0", "a:b", "-1:5"} {
		if _, _, err := parseOwner(bad); err == nil {
			t.Errorf("parseOwner(%q) accepted", bad)
		}
	}
}

func TestWaitForFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "control")
	if waitForFile(p, 30*time.Millisecond, 5*time.Millisecond) {
		t.Fatal("found a missing file")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(p, nil, 0o600)
	}()
	if !waitForFile(p, 5*time.Second, 5*time.Millisecond) {
		t.Fatal("file never seen")
	}
}

// The wheel socket's directory is locked, then emptied of what the App left,
// and the socket handed to the App's user, 0600.
func TestListenWheelLocksAndHandsOver(t *testing.T) {
	var calls []string
	lockWheelDir = func(dir string) error { calls = append(calls, "lock "+filepath.Base(dir)); return nil }
	chownSocket = func(p string, uid, gid int) error {
		calls = append(calls, fmt.Sprintf("chown %s %d:%d", filepath.Base(p), uid, gid))
		return nil
	}
	t.Cleanup(func() { lockWheelDir, chownSocket = inputbridge.LockDir, os.Lchown })
	dir, err := os.MkdirTemp("", "wheel") //nolint:usetesting // t.TempDir's path is too long for a socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err = os.WriteFile(filepath.Join(dir, "planted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "wheel.sock")
	l, err := listenWheel(t.Context(), socket, "1000:1000")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err = os.Lstat(filepath.Join(dir, "planted")); !os.IsNotExist(err) {
		t.Errorf("planted file kept: %v", err)
	}
	fi, err := os.Lstat(socket)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v %v", fi, err)
	}
	c, err := (&net.Dialer{}).DialContext(t.Context(), "unixpacket", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.Close()
	if want := []string{"lock " + filepath.Base(dir), "chown wheel.sock 1000:1000"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}
