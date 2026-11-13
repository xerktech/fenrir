//go:build linux

package wheel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// Uinput creates wheels through /dev/uinput.
type Uinput struct {
	// Path is /dev/uinput outside tests.
	Path string
	// SysDir is /sys/devices/virtual/input outside tests.
	SysDir string
}

// NewUinput returns the node's uinput backend.
func NewUinput() *Uinput {
	return &Uinput{Path: "/dev/uinput", SysDir: "/sys/devices/virtual/input"}
}

// ioctl numbers (linux/uinput.h), built as _IOC does.
const (
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'U'<<8 | nr }

const (
	ptrSize = unsafe.Sizeof(uintptr(0))
	// struct ff_effect: a 14-byte header padded to the union, whose largest
	// member (ff_periodic_effect) ends in a pointer.
	ffEffectSize = 16 + 24 + ptrSize
	// struct uinput_ff_upload: request_id, retval, effect, old.
	ffUploadSize = 8 + 2*ffEffectSize
	// struct uinput_ff_erase: request_id, retval, effect_id.
	ffEraseSize = 12
	// struct uinput_setup: input_id, name[80], ff_effects_max.
	setupSize = 8 + 80 + 4
	// struct uinput_abs_setup: code, padding, input_absinfo.
	absSetupSize = 4 + 24
	// struct input_event: a timeval, then type, code, value.
	inputEventSize = 2*ptrSize + 8

	uiDevCreate      = 'U'<<8 | 1 // _IO('U', 1)
	uiDevDestroy     = 'U'<<8 | 2
	uiSetEvBit       = iocWrite<<30 | 4<<16 | 'U'<<8 | 100
	uiSetKeyBit      = iocWrite<<30 | 4<<16 | 'U'<<8 | 101
	uiSetAbsBit      = iocWrite<<30 | 4<<16 | 'U'<<8 | 103
	uiSetFFBit       = iocWrite<<30 | 4<<16 | 'U'<<8 | 107
	uiFFUploadEvent  = 1
	uiFFEraseEvent   = 2
	sysnameBufLength = 64
	busUSB           = 0x03
)

var (
	uiDevSetup      = ioc(iocWrite, 3, setupSize)
	uiAbsSetup      = ioc(iocWrite, 4, absSetupSize)
	uiGetSysname    = ioc(iocRead, 44, sysnameBufLength)
	uiBeginFFUpload = ioc(iocRead|iocWrite, 200, ffUploadSize)
	uiEndFFUpload   = ioc(iocWrite, 201, ffUploadSize)
	uiBeginFFErase  = ioc(iocRead|iocWrite, 202, ffEraseSize)
	uiEndFFErase    = ioc(iocWrite, 203, ffEraseSize)
)

// ioctl retries EINTR: the Go runtime's own signals interrupted
// UI_DEV_CREATE on talos04 (2 of 541 creates, QA).
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	return ioctlInt(fd, req, int(uintptr(arg)))
}

func ioctlInt(fd int, req uintptr, v int) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(v))
		if errno == 0 {
			return nil
		}
		if errno != unix.EINTR {
			return errno
		}
	}
}

// Create makes the uinput device req describes.
func (u *Uinput) Create(req *Request, ff func(Message)) (Device, Node, error) {
	fd, err := unix.Open(u.Path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, Node{}, fmt.Errorf("open %s: %w", u.Path, err)
	}
	if err = setup(fd, req); err != nil {
		_ = unix.Close(fd)
		return nil, Node{}, err
	}
	if err = ioctlInt(fd, uiDevCreate, 0); err != nil {
		_ = unix.Close(fd)
		return nil, Node{}, fmt.Errorf("UI_DEV_CREATE: %w", err)
	}
	// The fd is non-blocking, so the runtime poller serves Read and Close
	// wakes a pending Read.
	d := &uinputDev{f: os.NewFile(uintptr(fd), u.Path), fd: fd, ff: ff, done: make(chan struct{})}
	node, err := u.node(fd)
	if err != nil {
		_ = d.destroy()
		return nil, Node{}, err
	}
	go d.readFF()
	return d, node, nil
}

func setup(fd int, req *Request) error {
	set := func(what string, nr uintptr, v uint16) error {
		if err := ioctlInt(fd, nr, int(v)); err != nil {
			return fmt.Errorf("%s %#x: %w", what, v, err)
		}
		return nil
	}
	if len(req.Keys) > 0 {
		if err := set("UI_SET_EVBIT", uiSetEvBit, evKey); err != nil {
			return err
		}
	}
	for _, k := range req.Keys {
		if err := set("UI_SET_KEYBIT", uiSetKeyBit, k); err != nil {
			return err
		}
	}
	if len(req.Abs) > 0 {
		if err := set("UI_SET_EVBIT", uiSetEvBit, evAbs); err != nil {
			return err
		}
	}
	for _, a := range req.Abs {
		if err := set("UI_SET_ABSBIT", uiSetAbsBit, a.Code); err != nil {
			return err
		}
		var b [absSetupSize]byte
		binary.NativeEndian.PutUint16(b[0:], a.Code)
		// input_absinfo: value, minimum, maximum, fuzz, flat, resolution.
		for i, v := range []int32{a.Min, a.Min, a.Max, a.Fuzz, a.Flat, 0} {
			binary.NativeEndian.PutUint32(b[4+4*i:], uint32(v)) //nolint:gosec // bit pattern
		}
		if err := ioctl(fd, uiAbsSetup, unsafe.Pointer(&b[0])); err != nil {
			return fmt.Errorf("UI_ABS_SETUP %#x: %w", a.Code, err)
		}
	}
	if len(req.FF) > 0 {
		if err := set("UI_SET_EVBIT", uiSetEvBit, evFF); err != nil {
			return err
		}
	}
	for _, f := range req.FF {
		if err := set("UI_SET_FFBIT", uiSetFFBit, f); err != nil {
			return err
		}
	}
	var b [setupSize]byte
	for i, v := range []uint16{busUSB, req.Vendor, req.Product, req.Version} {
		binary.NativeEndian.PutUint16(b[2*i:], v)
	}
	copy(b[8:8+maxNameBytes], req.Name)
	binary.NativeEndian.PutUint32(b[88:], req.FFEffectsMax)
	if err := ioctl(fd, uiDevSetup, unsafe.Pointer(&b[0])); err != nil {
		return fmt.Errorf("UI_DEV_SETUP: %w", err)
	}
	return nil
}

// node finds the created device's event node in sysfs.
func (u *Uinput) node(fd int) (Node, error) {
	var b [sysnameBufLength]byte
	if err := ioctl(fd, uiGetSysname, unsafe.Pointer(&b[0])); err != nil {
		return Node{}, fmt.Errorf("UI_GET_SYSNAME: %w", err)
	}
	sysname, _, _ := strings.Cut(string(b[:]), "\x00")
	n := Node{Sysname: sysname}
	if !isName(sysname, "input") {
		return n, fmt.Errorf("unexpected sysname %q", sysname)
	}
	// The kernel adds the event handler's child right after create; udev
	// on the node may still be catching up, sysfs is not.
	dir := filepath.Join(u.SysDir, sysname)
	for range 50 {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return n, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if !isName(e.Name(), "event") {
				continue
			}
			dev, err := os.ReadFile(filepath.Join(dir, e.Name(), "dev"))
			if err != nil {
				return n, fmt.Errorf("read %s dev: %w", e.Name(), err)
			}
			major, minor, err := parseDev(string(dev))
			if err != nil {
				return n, fmt.Errorf("%s: %w", e.Name(), err)
			}
			n.Event, n.Major, n.Minor = e.Name(), major, minor
			return n, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return n, fmt.Errorf("no event node under %s", dir)
}

// isName reports whether name is prefix followed by a decimal number.
func isName(name, prefix string) bool {
	num, ok := strings.CutPrefix(name, prefix)
	if !ok || num == "" {
		return false
	}
	_, err := strconv.ParseUint(num, 10, 32)
	return err == nil
}

func parseDev(s string) (major, minor uint32, err error) {
	majStr, minStr, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, 0, fmt.Errorf("bad dev %q", s)
	}
	a, err1 := strconv.ParseUint(majStr, 10, 32)
	b, err2 := strconv.ParseUint(minStr, 10, 32)
	if err := errors.Join(err1, err2); err != nil {
		return 0, 0, fmt.Errorf("bad dev %q: %w", s, err)
	}
	return uint32(a), uint32(b), nil
}

type uinputDev struct {
	f    *os.File
	fd   int
	ff   func(Message)
	done chan struct{}

	mu     sync.Mutex
	closed bool
}

func (d *uinputDev) Write(evs []Event) error {
	b := make([]byte, 0, len(evs)*int(inputEventSize))
	for _, e := range evs {
		var ev [inputEventSize]byte
		// The kernel stamps events itself; the timeval stays zero.
		binary.NativeEndian.PutUint16(ev[2*ptrSize:], e.Type)
		binary.NativeEndian.PutUint16(ev[2*ptrSize+2:], e.Code)
		binary.NativeEndian.PutUint32(ev[2*ptrSize+4:], uint32(e.Value)) //nolint:gosec // bit pattern
		b = append(b, ev[:]...)
	}
	if _, err := d.f.Write(b); err != nil {
		return fmt.Errorf("write events: %w", err)
	}
	return nil
}

func (d *uinputDev) Close() error {
	err := d.destroy()
	<-d.done
	return err
}

func (d *uinputDev) destroy() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	err := ioctlInt(d.fd, uiDevDestroy, 0)
	return errors.Join(err, d.f.Close())
}

// readFF answers the kernel's FF requests until the device is closed: an
// upload or erase blocks the game until it is acknowledged, so the helper
// acknowledges it at once (WiVRn and the headset can't refuse one usefully)
// and passes it on.
func (d *uinputDev) readFF() {
	defer close(d.done)
	buf := make([]byte, 64*inputEventSize)
	for {
		n, err := d.f.Read(buf)
		if err != nil {
			return
		}
		for off := 0; off+int(inputEventSize) <= n; off += int(inputEventSize) {
			ev := buf[off : off+int(inputEventSize)]
			typ := binary.NativeEndian.Uint16(ev[2*ptrSize:])
			code := binary.NativeEndian.Uint16(ev[2*ptrSize+2:])
			value := int32(binary.NativeEndian.Uint32(ev[2*ptrSize+4:])) //nolint:gosec // bit pattern
			if m, ok := d.handleFF(typ, code, value); ok {
				d.ff(m)
			}
		}
	}
}

func (d *uinputDev) handleFF(typ, code uint16, value int32) (Message, bool) {
	switch {
	case typ == evUinput && code == uiFFUploadEvent:
		var b [ffUploadSize]byte
		binary.NativeEndian.PutUint32(b[0:], uint32(value)) //nolint:gosec // request id
		if err := ioctl(d.fd, uiBeginFFUpload, unsafe.Pointer(&b[0])); err != nil {
			klog.Errorf("wheel: UI_BEGIN_FF_UPLOAD: %v", err)
			return Message{}, false
		}
		effect := decodeEffect(b[8 : 8+ffEffectSize])
		binary.NativeEndian.PutUint32(b[4:], 0) // retval
		if err := ioctl(d.fd, uiEndFFUpload, unsafe.Pointer(&b[0])); err != nil {
			klog.Errorf("wheel: UI_END_FF_UPLOAD: %v", err)
			return Message{}, false
		}
		return Message{Kind: FFUpload, Effect: &effect}, true
	case typ == evUinput && code == uiFFEraseEvent:
		var b [ffEraseSize]byte
		binary.NativeEndian.PutUint32(b[0:], uint32(value)) //nolint:gosec // request id
		if err := ioctl(d.fd, uiBeginFFErase, unsafe.Pointer(&b[0])); err != nil {
			klog.Errorf("wheel: UI_BEGIN_FF_ERASE: %v", err)
			return Message{}, false
		}
		id := int16(binary.NativeEndian.Uint32(b[8:])) //nolint:gosec // effect ids are < ff_effects_max
		binary.NativeEndian.PutUint32(b[4:], 0)
		if err := ioctl(d.fd, uiEndFFErase, unsafe.Pointer(&b[0])); err != nil {
			klog.Errorf("wheel: UI_END_FF_ERASE: %v", err)
			return Message{}, false
		}
		return Message{Kind: FFErase, EffectID: &id}, true
	case typ == evFF:
		return ffEvent(code, value), true
	}
	return Message{}, false
}
