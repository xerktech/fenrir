package inputbridge

import (
	"os"
	"path/filepath"
	"testing"
)

func wheelProps() map[string]string {
	return map[string]string{
		"DEVNAME": "/dev/input/event30", "MAJOR": "13", "MINOR": "94",
		"DEVPATH": "/devices/virtual/input/input77/event30", "SUBSYSTEM": "input",
	}
}

// The wheel helper's node and udev entry outlive Clear, which wolf-agent
// sends whenever a Moonlight stream stops, and the socket can't touch them;
// Withdraw takes them out.
func TestPublishedNodeSurvivesClear(t *testing.T) {
	f := newFakeServer(t)
	if err := f.Publish(wheelProps(), "c13:94", []string{"E:ID_INPUT_JOYSTICK=1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Handle(Request{Op: OpCreate, Props: event("/dev/input/event5", "13")}); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(f.InputDir, "event30")
	entry := filepath.Join(f.UdevDataDir, "c13:94")
	for _, req := range []Request{
		{Op: OpCreate, Props: wheelProps()},
		{Op: OpRemove, Props: wheelProps()},
		{Op: OpWriteHwDB, Filename: "c13:94", Content: []string{"x"}},
		{Op: OpRemoveHwDB, Filename: "c13:94"},
		{Op: OpUevent, Props: map[string]string{"ACTION": "remove", "DEVPATH": wheelProps()["DEVPATH"], "SUBSYSTEM": "input"}},
	} {
		if err := f.Handle(req); err == nil {
			t.Errorf("%s of the wheel's %s/%s accepted", req.Op, req.Props["DEVNAME"], req.Filename)
		}
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.InputDir, "event5")); !os.IsNotExist(err) {
		t.Errorf("Wolf's node survived Clear: %v", err)
	}
	if _, err := os.Stat(node); err != nil || !f.mounted[node] {
		t.Errorf("wheel node after Clear: stat %v, mounted %v", err, f.mounted[node])
	}
	if b, err := os.ReadFile(entry); err != nil || string(b) != "E:ID_INPUT_JOYSTICK=1" {
		t.Errorf("wheel udev entry after Clear: %q, %v", b, err)
	}
	if got := len(f.events.added); got != 1 {
		t.Errorf("announced after Clear: %d devices, want the wheel", got)
	}

	if err := f.Withdraw(wheelProps(), "c13:94"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(node); !os.IsNotExist(err) || f.mounted[node] {
		t.Errorf("wheel node after Withdraw: stat %v, mounted %v", err, f.mounted[node])
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Errorf("wheel udev entry after Withdraw: %v", err)
	}
	if len(f.events.added) != 0 {
		t.Errorf("still announced after Withdraw: %v", f.events.added)
	}
	// Withdrawn, the name is the socket's again.
	if err := f.Handle(Request{Op: OpCreate, Props: wheelProps()}); err != nil {
		t.Errorf("create after Withdraw: %v", err)
	}
}

// Publish refuses what the socket would: only major 13 /dev/input nodes and
// one-element udev names.
func TestPublishRefusesForeignNodes(t *testing.T) {
	f := newFakeServer(t)
	bad := wheelProps()
	bad["MAJOR"] = "10"
	if err := f.Publish(bad, "c10:94", nil); err == nil {
		t.Error("major 10 published")
	}
	if err := f.Publish(wheelProps(), "../x", nil); err == nil {
		t.Error("udev name ../x accepted")
	}
}
