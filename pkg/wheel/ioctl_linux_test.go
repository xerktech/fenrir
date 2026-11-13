//go:build linux

package wheel

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl's buffers live on the stack of its caller, which may move (grow)
// between taking the pointer and the syscall. ioctl must hand Syscall
// uintptr(arg) in the call itself, so the runtime keeps the pointer current;
// converted earlier, the kernel wrote to the old copy (QA lost 120 of 16,000
// FIONREAD results). The pads and the recursion put the ioctl at every
// offset from a goroutine's stack boundary.
func TestIoctlSurvivesStackMoves(t *testing.T) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(p[0]); unix.Close(p[1]) })
	if _, err := unix.Write(p[1], []byte("hello")); err != nil {
		t.Fatal(err)
	}
	lost := 0
	for k := range ioctlPads {
		for d := range 400 {
			ch := make(chan bool)
			go func() { ch <- ioctlRecurse(d, k, p[0]) }()
			if !<-ch {
				lost++
			}
		}
	}
	if lost > 0 {
		t.Fatalf("ioctl's result went to a moved stack %d times", lost)
	}
}

var ioctlSunk byte

//go:noinline
func ioctlSink(b []byte) { ioctlSunk += b[0] }

// ioctlTarget reports whether FIONREAD (TIOCINQ) read the pipe's 5 bytes.
//
//go:noinline
func ioctlTarget(fd int) bool {
	var b [8]byte
	if err := ioctl(fd, unix.TIOCINQ, unsafe.Pointer(&b[0])); err != nil {
		panic(err)
	}
	return binary.NativeEndian.Uint32(b[:]) == 5
}

//go:noinline
func ioctlRecurse(d, k, fd int) bool {
	var p [24]byte
	p[0] = 1
	ioctlSink(p[:])
	if d == 0 {
		return ioctlPads[k](fd)
	}
	return ioctlRecurse(d-1, k, fd)
}

var ioctlPads = []func(int) bool{ioctlPad0, ioctlPad1, ioctlPad2, ioctlPad3, ioctlPad4, ioctlPad5, ioctlPad6, ioctlPad7, ioctlPad8, ioctlPad9, ioctlPad10, ioctlPad11, ioctlPad12, ioctlPad13, ioctlPad14, ioctlPad15}

//go:noinline
func ioctlPad0(fd int) bool { var p [1]byte; p[0] = 1; r := ioctlTarget(fd); ioctlSink(p[:]); return r }

//go:noinline
func ioctlPad1(fd int) bool { var p [9]byte; p[0] = 1; r := ioctlTarget(fd); ioctlSink(p[:]); return r }

//go:noinline
func ioctlPad2(fd int) bool {
	var p [17]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad3(fd int) bool {
	var p [25]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad4(fd int) bool {
	var p [33]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad5(fd int) bool {
	var p [41]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad6(fd int) bool {
	var p [49]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad7(fd int) bool {
	var p [57]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad8(fd int) bool {
	var p [65]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad9(fd int) bool {
	var p [73]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad10(fd int) bool {
	var p [81]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad11(fd int) bool {
	var p [89]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad12(fd int) bool {
	var p [97]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad13(fd int) bool {
	var p [105]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad14(fd int) bool {
	var p [113]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}

//go:noinline
func ioctlPad15(fd int) bool {
	var p [121]byte
	p[0] = 1
	r := ioctlTarget(fd)
	ioctlSink(p[:])
	return r
}
