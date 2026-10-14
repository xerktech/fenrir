package fakeudev

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDeviceNodePath(t *testing.T) {
	cases := map[string]string{
		"/dev/input/event3": "/d/event3",
		"/dev/input/js0":    "/d/js0",
		"/dev/hidraw0":      "",
		"/dev/input/":       "",
		"/dev/input/..":     "",
		"/dev/input/a/b":    "",
		"/dev/input/../sda": "",
		"":                  "",
	}
	for devname, want := range cases {
		got, err := deviceNodePath("/d", devname)
		if want == "" {
			if err == nil {
				t.Errorf("deviceNodePath(%q) = %q, want error", devname, got)
			}
		} else if err != nil || got != want {
			t.Errorf("deviceNodePath(%q) = %q, %v; want %q", devname, got, err, want)
		}
	}
}

// stubMakeCharNode records CreateDeviceNode's node creations instead of
// making them, so these tests mean the same without CAP_MKNOD.
func stubMakeCharNode(t *testing.T) *[]string {
	t.Helper()
	var made []string
	orig := makeCharNode
	makeCharNode = func(path string, major, minor uint32) error {
		made = append(made, fmt.Sprintf("%s %d:%d", path, major, minor))
		return nil
	}
	t.Cleanup(func() { makeCharNode = orig })
	return &made
}

func TestCreateDeviceNodeRefuses(t *testing.T) {
	made := stubMakeCharNode(t)
	dir := t.TempDir()
	for _, props := range []map[string]string{
		{"DEVNAME": "/dev/input/event3", "MAJOR": "8", "MINOR": "0"}, // a disk
		{"DEVNAME": "/dev/input/event3", "MAJOR": "0", "MINOR": "0"}, // unresolved
		{"DEVNAME": "/dev/input/event3", "MAJOR": "13", "MINOR": "x"},
		{"DEVNAME": "/dev/input/event3", "MAJOR": "13"},
		{"DEVNAME": "/dev/input/event3", "MAJOR": "4294967309", "MINOR": "1"}, // 2^32+13
		{"DEVNAME": "/dev/hidraw0", "MAJOR": "13", "MINOR": "67"},
		{"DEVNAME": "/dev/input/../sda", "MAJOR": "13", "MINOR": "67"},
	} {
		if err := CreateDeviceNode(dir, props); err == nil {
			t.Errorf("CreateDeviceNode(%v) succeeded, want error", props)
		}
	}
	if len(*made) != 0 {
		t.Errorf("refused creates made nodes: %v", *made)
	}
}

func TestCreateDeviceNodeReplacesStale(t *testing.T) {
	made := stubMakeCharNode(t)
	dir := t.TempDir()
	stale := filepath.Join(dir, "event67")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateDeviceNode(dir, map[string]string{"DEVNAME": "/dev/input/event67", "MAJOR": "13", "MINOR": "67"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{stale + " 13:67"}; !slices.Equal(*made, want) {
		t.Errorf("made %v, want %v", *made, want)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale node not removed before mknod: %v", err)
	}
}

func TestClearDirDoesNotFollowSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c13:67"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "event5")); err != nil {
		t.Fatal(err)
	}
	if err := ClearDir(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d entries left", len(entries))
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep" {
		t.Errorf("symlink target touched: %q, %v", b, err)
	}
	if err := ClearDir(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("missing dir: %v", err)
	}
}
