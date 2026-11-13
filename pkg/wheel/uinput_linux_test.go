//go:build linux

package wheel

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ioctl numbers equal linux/uinput.h's on 64-bit Linux.
func TestUinputIoctlNumbers(t *testing.T) {
	if ptrSize != 8 {
		t.Skip("numbers below are for 64-bit")
	}
	for name, tc := range map[string]struct{ got, want uintptr }{
		"UI_DEV_CREATE":      {uiDevCreate, 0x5501},
		"UI_DEV_DESTROY":     {uiDevDestroy, 0x5502},
		"UI_DEV_SETUP":       {uiDevSetup, 0x405c5503},
		"UI_ABS_SETUP":       {uiAbsSetup, 0x401c5504},
		"UI_SET_EVBIT":       {uiSetEvBit, 0x40045564},
		"UI_SET_KEYBIT":      {uiSetKeyBit, 0x40045565},
		"UI_SET_ABSBIT":      {uiSetAbsBit, 0x40045567},
		"UI_SET_FFBIT":       {uiSetFFBit, 0x4004556b},
		"UI_GET_SYSNAME(64)": {uiGetSysname, 0x8040552c},
		"UI_BEGIN_FF_UPLOAD": {uiBeginFFUpload, 0xc06855c8},
		"UI_END_FF_UPLOAD":   {uiEndFFUpload, 0x406855c9},
		"UI_BEGIN_FF_ERASE":  {uiBeginFFErase, 0xc00c55ca},
		"UI_END_FF_ERASE":    {uiEndFFErase, 0x400c55cb},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %#x, want %#x", name, tc.got, tc.want)
		}
	}
	if inputEventSize != 24 || ffEffectSize != 48 {
		t.Errorf("input_event %d (want 24), ff_effect %d (want 48)", inputEventSize, ffEffectSize)
	}
}

// On a node with /dev/uinput (talos04, as root): a G920-shaped wheel is
// created, its event node found in sysfs, events reach a reader of the node,
// and an FF upload and play from the node come back as messages.
func TestUinputRoundTrip(t *testing.T) {
	u := NewUinput()
	if _, err := os.Stat(u.Path); err != nil {
		t.Skipf("no uinput: %v", err)
	}
	req := g920(1)
	msgs := make(chan Message, 16)
	dev, node, err := u.Create(&req, func(m Message) { msgs <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	if node.Major != 13 || !isName(node.Event, "event") {
		t.Fatalf("node %+v", node)
	}
	// The host's devtmpfs has the node; open it there.
	path := "/dev/input/" + node.Event
	var fd int
	for range 100 {
		if fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Skipf("can't open %s here: %v", path, err)
	}
	defer unix.Close(fd)

	if err := dev.Write([]Event{{Type: evAbs, Code: 0, Value: 40000}, {Type: evSyn}}); err != nil {
		t.Fatal(err)
	}
	ev := make([]byte, inputEventSize)
	if _, err := unix.Read(fd, ev); err != nil {
		t.Fatal(err)
	}
	if typ, v := binary.NativeEndian.Uint16(ev[16:]), int32(binary.NativeEndian.Uint32(ev[20:])); typ != evAbs || v != 40000 { //nolint:gosec // test
		t.Errorf("read type %#x value %d", typ, v)
	}

	// EVIOCSFF: the game's upload, which blocks until the helper acks it.
	eff := make([]byte, ffEffectSize)
	binary.NativeEndian.PutUint16(eff[0:], ffConstant)
	binary.NativeEndian.PutUint16(eff[2:], 0xffff) // id -1: new
	binary.NativeEndian.PutUint16(eff[16:], uint16(0x2000))
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), iocWrite<<30|ffEffectSize<<16|'E'<<8|0x80, uintptr(unsafe.Pointer(&eff[0]))); errno != 0 {
		t.Fatalf("EVIOCSFF: %v", errno)
	}
	m := <-msgs
	if m.Kind != FFUpload || m.Effect.Constant == nil || m.Effect.Constant.Level != 0x2000 {
		t.Errorf("upload: %+v", m)
	}
	play := make([]byte, inputEventSize)
	binary.NativeEndian.PutUint16(play[16:], evFF)
	binary.NativeEndian.PutUint16(play[18:], uint16(m.Effect.ID)) //nolint:gosec // test
	binary.NativeEndian.PutUint32(play[20:], 1)
	if _, err := unix.Write(fd, play); err != nil {
		t.Fatal(err)
	}
	if p := <-msgs; p.Kind != FFPlay || *p.EffectID != m.Effect.ID || *p.Value != 1 {
		t.Errorf("play: %+v", p)
	}
}
