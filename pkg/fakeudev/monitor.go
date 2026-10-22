package fakeudev

import (
	"encoding/binary"
	"slices"
	"strings"
)

// A user-namespaced App pod (XERK-1920) has its own network namespace, so it
// no longer hears the node's udevd announce Wolf's devices, and libudev
// consumers (Steam's Big Picture, SDL) miss every pad plugged after they
// started. The udev-relay sidecar sends the announcement there itself, as
// Wolf's fake-udev does in its Docker runner: a libudev monitor message on
// NETLINK_KOBJECT_UEVENT group MonitorGroupUdev. In that namespace it reaches
// only the App pod.

// MonitorGroupUdev is libudev's netlink multicast group (the kernel's own
// uevents are group 1).
const MonitorGroupUdev = 2

const (
	monitorMagic      = 0xfeedcafe
	monitorHeaderSize = 40
)

// MonitorMessage encodes props (one udev event's properties: ACTION, DEVPATH,
// SUBSYSTEM, ...) as a libudev monitor message: systemd's
// monitor_netlink_header, then KEY=VALUE\0 pairs. Only the magic and the
// filter hashes are big-endian; libudev filters on the SUBSYSTEM and DEVTYPE
// hashes in-kernel. Ported from Wolf's src/fake-udev.
func MonitorMessage(props map[string]string) []byte {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var body strings.Builder
	for _, k := range keys {
		body.WriteString(k)
		body.WriteByte('=')
		body.WriteString(props[k])
		body.WriteByte(0)
	}

	msg := make([]byte, monitorHeaderSize, monitorHeaderSize+body.Len())
	copy(msg, "libudev\x00")
	binary.BigEndian.PutUint32(msg[8:], monitorMagic)
	binary.NativeEndian.PutUint32(msg[12:], monitorHeaderSize)
	binary.NativeEndian.PutUint32(msg[16:], monitorHeaderSize)
	binary.NativeEndian.PutUint32(msg[20:], uint32(body.Len())) //nolint:gosec // a few hundred bytes
	if s := props["SUBSYSTEM"]; s != "" {
		binary.BigEndian.PutUint32(msg[24:], murmurHash2([]byte(s)))
	}
	if d := props["DEVTYPE"]; d != "" {
		binary.BigEndian.PutUint32(msg[28:], murmurHash2([]byte(d)))
	}
	// Tag bloom filter (msg[32:40]) left 0, as Wolf's fake-udev does.
	return append(msg, body.String()...)
}

// murmurHash2 is Austin Appleby's MurmurHash2, which libudev uses for its
// filter hashes (systemd's string_hash32, seed 0).
func murmurHash2(data []byte) uint32 {
	const m = 0x5bd1e995
	const r = 24
	h := uint32(len(data)) //nolint:gosec // short strings
	for len(data) >= 4 {
		k := binary.LittleEndian.Uint32(data)
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
		data = data[4:]
	}
	switch len(data) {
	case 3:
		h ^= uint32(data[2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[0])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return h
}
