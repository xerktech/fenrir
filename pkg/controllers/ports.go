package controllers

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// Session pods run with hostNetwork on a shared node IP, so every port a
// session pod listens on must be unique on that node. Each session pod gets
// one contiguous block, laid out as the offsets below.
//
// HTTP/HTTPS are in the block although Wolf no longer listens on them (see
// withWolfHTTPServersOff): dropping them would shift every later offset of the
// blocks already recorded in Sessions' status.ports.
const (
	portOffsetHTTP = iota
	portOffsetHTTPS
	portOffsetRTSP
	portOffsetControl
	portOffsetVideoRTP
	portOffsetAudioRTP
	portOffsetWolfAgent

	sessionPortBlockSize
)

// portBlockAnnotation on a session pod records the first port of the block it
// was built for.
const portBlockAnnotation = "direwolf.games-on-whales.github.io/port-block"

// A VR server (WiVRn) needs one port, TCP (control) and UDP (stream) on the
// same number. It comes from a range of its own (--vr-port-range), not the
// session block: growing the block would move every block base and break
// Claim for the blocks running Sessions recorded in status.ports.
const (
	// vrPortEnv hands the port to the App's containers.
	vrPortEnv = "DIREWOLF_VR_PORT"
	// vrPortAnnotation on a split Session's App pod records the VR port it
	// was built with; absent, the pod serves none.
	vrPortAnnotation = "direwolf.games-on-whales.github.io/vr-port"
)

// PortRange is an inclusive range of host ports session blocks are cut from.
type PortRange struct {
	Min, Max int32
}

// ParsePortRange parses "MIN-MAX" and checks it holds at least one session
// block.
func ParsePortRange(s string) (PortRange, error) {
	return parsePortRange(s, sessionPortBlockSize)
}

// ParseVRPortRange parses "MIN-MAX" for --vr-port-range: one port per
// Session.
func ParseVRPortRange(s string) (PortRange, error) {
	return parsePortRange(s, 1)
}

// Overlaps reports whether r and o share a port.
func (r PortRange) Overlaps(o PortRange) bool {
	return r.Min <= o.Max && o.Min <= r.Max
}

func parsePortRange(s string, size int32) (PortRange, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return PortRange{}, fmt.Errorf("port range %q: want MIN-MAX", s)
	}
	minPort, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 16)
	if err != nil {
		return PortRange{}, fmt.Errorf("port range %q: %w", s, err)
	}
	maxPort, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 16)
	if err != nil {
		return PortRange{}, fmt.Errorf("port range %q: %w", s, err)
	}
	r := PortRange{Min: int32(minPort), Max: int32(maxPort)}
	// wolf-agent binds its block port without NET_BIND_SERVICE (XERK-2143).
	if r.Min < 1024 || r.count(size) < 1 {
		return PortRange{}, fmt.Errorf("port range %q must hold at least %d ports starting at 1024 or above", s, size)
	}
	return r, nil
}

// blocks is the number of session blocks r holds.
func (r PortRange) blocks() int32 {
	return r.count(sessionPortBlockSize)
}

// count is the number of size-port blocks r holds.
func (r PortRange) count(size int32) int32 {
	if r.Max < r.Min {
		return 0
	}
	return (r.Max - r.Min + 1) / size
}

// blockPorts returns the ports of the block starting at base.
func blockPorts(base int32) v1alpha1types.SessionPorts {
	return v1alpha1types.SessionPorts{
		HTTP:      base + portOffsetHTTP,
		HTTPS:     base + portOffsetHTTPS,
		RTSP:      base + portOffsetRTSP,
		Control:   base + portOffsetControl,
		VideoRTP:  base + portOffsetVideoRTP,
		AudioRTP:  base + portOffsetAudioRTP,
		WolfAgent: base + portOffsetWolfAgent,
	}
}

// portAllocator hands out non-overlapping port blocks of size ports, one per
// owner (a session's pod). It is safe for concurrent use.
//
// The allocator is in-memory; the source of truth is Session.status.ports,
// which the controller replays through Claim on startup.
type portAllocator struct {
	mu     sync.Mutex
	r      PortRange
	size   int32
	blocks map[string]int32 // owner -> block base
}

// newPortAllocator allocates session blocks (sessionPortBlockSize) from r.
func newPortAllocator(r PortRange) *portAllocator {
	return &portAllocator{r: r, size: sessionPortBlockSize, blocks: map[string]int32{}}
}

// newVRPortAllocator allocates single VR ports from r.
func newVRPortAllocator(r PortRange) *portAllocator {
	return &portAllocator{r: r, size: 1, blocks: map[string]int32{}}
}

// Allocate returns owner's session block, assigning the lowest free one if it
// has none.
func (a *portAllocator) Allocate(owner string) (v1alpha1types.SessionPorts, error) {
	base, err := a.AllocateBase(owner)
	if err != nil {
		return v1alpha1types.SessionPorts{}, err
	}
	return blockPorts(base), nil
}

// AllocateBase returns the first port of owner's block, assigning the lowest
// free one if it has none.
func (a *portAllocator) AllocateBase(owner string) (int32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if base, ok := a.blocks[owner]; ok {
		return base, nil
	}
	used := sets.New[int32]()
	for _, base := range a.blocks {
		used.Insert(base)
	}
	for i := range a.r.count(a.size) {
		base := a.r.Min + i*a.size
		if !used.Has(base) {
			a.blocks[owner] = base
			return base, nil
		}
	}
	return 0, fmt.Errorf("no free port block in %d-%d (%d in use)", a.r.Min, a.r.Max, len(a.blocks))
}

// Claim records a session block previously allocated to owner, e.g. read back
// from a Session's status after an operator restart. It fails if ports are
// not a block of this range or the block belongs to another owner. ports.VR
// is not part of the block (see vrPortEnv) and is ignored.
func (a *portAllocator) Claim(owner string, ports v1alpha1types.SessionPorts) error {
	base := ports.HTTP
	ports.VR = 0
	if ports != blockPorts(base) {
		return fmt.Errorf("ports %+v are not a block of range %d-%d", ports, a.r.Min, a.r.Max)
	}
	return a.ClaimBase(owner, base)
}

// ClaimBase records the block at base as owner's (see Claim).
func (a *portAllocator) ClaimBase(owner string, base int32) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if base < a.r.Min || (base-a.r.Min)%a.size != 0 || (base-a.r.Min)/a.size >= a.r.count(a.size) {
		return fmt.Errorf("port %d does not start a block of range %d-%d", base, a.r.Min, a.r.Max)
	}
	if held, ok := a.blocks[owner]; ok {
		if held != base {
			return fmt.Errorf("%s already holds the block at %d", owner, held)
		}
		return nil
	}
	for other, b := range a.blocks {
		if b == base {
			return fmt.Errorf("block at %d is held by %s", base, other)
		}
	}
	a.blocks[owner] = base
	return nil
}

// Retain releases the block of every owner not in live.
func (a *portAllocator) Retain(live sets.Set[string]) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for owner := range a.blocks {
		if !live.Has(owner) {
			delete(a.blocks, owner)
		}
	}
}
