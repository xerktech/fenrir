//go:build linux

package wheel

import (
	"encoding/binary"
	"errors"
	"os"
	"strings"
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

// An interrupted UI_DEV_CREATE has uinput tear the device back down to
// unset, so Create redoes the setup: the device it then makes has every
// capability asked for. Past createAttempts it fails, leaking nothing.
func TestUinputCreateRetriesAfterUnwind(t *testing.T) {
	u := NewUinput()
	if _, err := os.Stat(u.Path); err != nil {
		t.Skipf("no uinput: %v", err)
	}
	caps := func(n Node) string {
		files := []string{"name", "id/vendor", "id/product", "capabilities/key", "capabilities/abs", "capabilities/ff"}
		out := make([]string, 0, len(files))
		for _, f := range files {
			b, err := os.ReadFile(u.SysDir + "/" + n.Sysname + "/" + f)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, f+"="+strings.TrimSpace(string(b)))
		}
		return strings.Join(out, " ")
	}
	req := g920(1)
	dev, node, err := u.Create(&req, func(Message) {})
	if err != nil {
		t.Fatal(err)
	}
	want := caps(node)
	dev.Close()

	interrupts := 0
	t.Cleanup(func() { devCreate = func(fd int) error { return ioctlOnce(fd, uiDevCreate, 0) } })
	// The kernel's unwind: registered, then destroyed back to unset.
	devCreate = func(fd int) error {
		if interrupts == 0 {
			return ioctlOnce(fd, uiDevCreate, 0)
		}
		interrupts--
		if uerr := errors.Join(ioctlOnce(fd, uiDevCreate, 0), ioctlOnce(fd, uiDevDestroy, 0)); uerr != nil {
			return uerr
		}
		return unix.EINTR
	}
	interrupts = createAttempts - 1
	dev, node, err = u.Create(&req, func(Message) {})
	if err != nil {
		t.Fatalf("after %d interrupts: %v", createAttempts-1, err)
	}
	if got := caps(node); got != want {
		t.Errorf("recreated device:\n got %s\nwant %s", got, want)
	}
	dev.Close()
	interrupts = createAttempts
	if _, _, err = u.Create(&req, func(Message) {}); !errors.Is(err, unix.EINTR) {
		t.Errorf("after %d interrupts: %v, want EINTR", createAttempts, err)
	}
}
