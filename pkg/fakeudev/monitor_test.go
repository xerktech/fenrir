package fakeudev

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Wolf's fake-udev test vectors (tests/platforms/linux/fake-udev.cpp).
func TestMonitorMessageMatchesWolfsFakeUdev(t *testing.T) {
	if got := murmurHash2([]byte("input"), 0); got != 3248653424 {
		t.Errorf(`hash("input") = %d, want 3248653424`, got)
	}
	props := map[string]string{
		".INPUT_CLASS": "joystick", "ACTION": "add", "CURRENT_TAGS": ":seat:uaccess:",
		"DEVNAME": "/dev/input/event23", "DEVPATH": "/devices/virtual/input/input387/event23",
		"ID_INPUT": "1", "ID_INPUT_JOYSTICK": "1", "ID_SERIAL": "noserial", "MAJOR": "13",
		"MINOR": "87", "SEQNUM": "7", "SUBSYSTEM": "input", "TAGS": ":seat:uaccess:",
		"USEC_INITIALIZED": "1695908821",
	}
	wantBody := ".INPUT_CLASS=joystick\x00ACTION=add\x00CURRENT_TAGS=:seat:uaccess:\x00" +
		"DEVNAME=/dev/input/event23\x00DEVPATH=/devices/virtual/input/input387/event23\x00" +
		"ID_INPUT=1\x00ID_INPUT_JOYSTICK=1\x00ID_SERIAL=noserial\x00MAJOR=13\x00MINOR=87\x00" +
		"SEQNUM=7\x00SUBSYSTEM=input\x00TAGS=:seat:uaccess:\x00USEC_INITIALIZED=1695908821\x00"
	msg := MonitorMessage(props)
	if !bytes.HasPrefix(msg, []byte("libudev\x00")) || binary.BigEndian.Uint32(msg[8:]) != 0xfeedcafe {
		t.Fatalf("header = % x", msg[:12])
	}
	if off, n := binary.NativeEndian.Uint32(msg[16:]), binary.NativeEndian.Uint32(msg[20:]); off != 40 || int(n) != len(wantBody) {
		t.Errorf("properties at %d len %d, want 40 len %d", off, n, len(wantBody))
	}
	if got := binary.BigEndian.Uint32(msg[24:]); got != 3248653424 {
		t.Errorf("subsystem hash = %d", got)
	}
	if got := binary.BigEndian.Uint32(msg[28:]); got != 0 {
		t.Errorf("devtype hash = %d, want 0 without DEVTYPE", got)
	}
	if string(msg[40:]) != wantBody {
		t.Errorf("body = %q", msg[40:])
	}
}
