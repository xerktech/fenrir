package wheel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBackend records wheels instead of making uinput devices.
type fakeBackend struct {
	mu    sync.Mutex
	log   *[]string
	devs  []*fakeDev
	minor uint32
}

type fakeDev struct {
	b      *fakeBackend
	node   Node
	ff     func(Message)
	mu     sync.Mutex
	writes [][]Event
	closed bool
}

func (b *fakeBackend) Create(_ *Request, ff func(Message)) (Device, Node, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.minor++
	n := Node{Sysname: "input9", Event: fmt.Sprintf("event%d", b.minor), Major: 13, Minor: 64 + b.minor}
	d := &fakeDev{b: b, node: n, ff: ff}
	b.devs = append(b.devs, d)
	*b.log = append(*b.log, "create "+n.Event)
	return d, n, nil
}

func (d *fakeDev) Write(evs []Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes = append(d.writes, evs)
	return nil
}

func (d *fakeDev) Close() error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	d.closed = true
	*d.b.log = append(*d.b.log, "close "+d.node.Event)
	return nil
}

type fakePublisher struct {
	b            *fakeBackend
	failPublish  bool
	failWithdraw bool
}

func (p *fakePublisher) Publish(n Node, _ *Request) error {
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	*p.b.log = append(*p.b.log, "publish "+n.Event)
	if p.failPublish {
		return errors.New("no")
	}
	return nil
}

func (p *fakePublisher) Withdraw(n Node) error {
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	*p.b.log = append(*p.b.log, "withdraw "+n.Event)
	if p.failWithdraw {
		return errors.New("busy")
	}
	return nil
}

func (b *fakeBackend) calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(*b.log)
}

type harness struct {
	t      *testing.T
	b      *fakeBackend
	p      *fakePublisher
	socket string
	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "wheel") //nolint:usetesting // t.TempDir's path is too long for a socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &harness{t: t, b: &fakeBackend{log: &[]string{}}, socket: filepath.Join(dir, "w.sock"), done: make(chan struct{})}
	h.p = &fakePublisher{b: h.b}
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	l, err := (&net.ListenConfig{}).Listen(ctx, "unixpacket", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Backend: h.b, Publisher: h.p}
	go func() {
		defer close(h.done)
		_ = s.Serve(ctx, l)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
}

type client struct {
	t *testing.T
	c net.Conn
}

func (h *harness) dial() *client {
	h.t.Helper()
	c, err := (&net.Dialer{}).DialContext(h.t.Context(), "unixpacket", h.socket)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	return &client{t: h.t, c: c}
}

func (c *client) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.c.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() Message {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64<<10)
	n, err := c.c.Read(buf)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var m Message
	if err := json.Unmarshal(buf[:n], &m); err != nil {
		c.t.Fatalf("decode %q: %v", buf[:n], err)
	}
	return m
}

// g920 is the create request the epic's contract sends for a G920.
func g920(id uint32) Request {
	return Request{
		Op: OpCreate, ID: id, Name: "Logitech G920 Driving Force Racing Wheel",
		Vendor: 0x046d, Product: 0xc262, Version: 0x0111,
		Abs: []AbsAxis{
			{Code: 0x00, Max: 65535, Flat: 0}, {Code: 0x01, Max: 255}, {Code: 0x02, Max: 255}, {Code: 0x05, Max: 255},
			{Code: 0x10, Min: -1, Max: 1}, {Code: 0x11, Min: -1, Max: 1},
		},
		Keys:         []uint16{0x120, 0x121, 0x122, 0x123, 0x124, 0x125, 0x126, 0x127, 0x128, 0x129, 0x12a, 0x2c0},
		FF:           []uint16{ffConstant, ffSpring, ffDamper, ffGain, ffAutocenter},
		FFEffectsMax: 16,
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A create makes and publishes the wheel and answers created; events reach
// it framed by a SYN_REPORT; the kernel's FF requests come back on the
// socket; closing the connection withdraws the node before the device goes,
// so its minor is never reused under a node still in the App's /dev/input.
func TestWheelLifecycle(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(g920(7))
	if m := c.recv(); m.Op != OpCreated || m.ID != 7 {
		t.Fatalf("got %+v, want created 7", m)
	}
	c.send(Request{Op: OpEvents, ID: 7, Events: []Event{{Type: evAbs, Code: 0, Value: 32768}, {Type: evKey, Code: 0x120, Value: 1}}})
	d := h.b.devs[0]
	waitFor(t, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.writes) == 1 })
	want := []Event{{Type: evAbs, Code: 0, Value: 32768}, {Type: evKey, Code: 0x120, Value: 1}, {Type: evSyn, Code: synReport}}
	if !slices.Equal(d.writes[0], want) {
		t.Errorf("wrote %v, want %v", d.writes[0], want)
	}

	effect := Effect{Type: ffConstant, ID: 0, Constant: &Constant{Level: -12000}}
	d.ff(Message{Kind: FFUpload, Effect: &effect})
	if m := c.recv(); m.Op != OpFF || m.ID != 7 || m.Kind != FFUpload || m.Effect == nil || m.Effect.Constant.Level != -12000 {
		t.Errorf("got %+v, want the upload", m)
	}
	d.ff(ffEvent(0, 1))
	if m := c.recv(); m.Kind != FFPlay || *m.EffectID != 0 || *m.Value != 1 {
		t.Errorf("got %+v, want play 0", m)
	}

	c.c.Close()
	waitFor(t, func() bool { return len(h.b.calls()) == 4 })
	if got, want := h.b.calls(), []string{"create event1", "publish event1", "withdraw event1", "close event1"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// Shutting the server down destroys every wheel too.
func TestServerStopDestroysWheels(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(g920(1))
	c.recv()
	h.stop()
	if got := h.b.calls(); !slices.Contains(got, "close event1") || !slices.Contains(got, "withdraw event1") {
		t.Errorf("calls = %v, want the wheel withdrawn and closed", got)
	}
}

// Anything in the App can reach the socket, so a create is held to a wheel:
// no keyboard key, mouse button or digitizer tool, no multitouch axis, no
// FF_CUSTOM (its samples are a pointer into the game), a bounded name and
// effect count.
func TestCreateRefusesNonWheels(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	for name, mutate := range map[string]func(*Request){
		"keyboard key":    func(r *Request) { r.Keys = append(r.Keys, 28) }, // KEY_ENTER
		"mouse button":    func(r *Request) { r.Keys = append(r.Keys, 0x110) },
		"digitizer tool":  func(r *Request) { r.Keys = append(r.Keys, 0x140) },
		"key past range":  func(r *Request) { r.Keys = append(r.Keys, 0x2e8) },
		"multitouch axis": func(r *Request) { r.Abs = append(r.Abs, AbsAxis{Code: 0x35, Max: 1}) },
		"repeated axis":   func(r *Request) { r.Abs = append(r.Abs, AbsAxis{Code: 0x00, Max: 1}) },
		"empty range":     func(r *Request) { r.Abs[0].Min = r.Abs[0].Max },
		"ff custom":       func(r *Request) { r.FF = append(r.FF, 0x5d) },
		"ff rel":          func(r *Request) { r.FF = append(r.FF, 0x02) },
		"too many slots":  func(r *Request) { r.FFEffectsMax = MaxFFEffects + 1 },
		"no slots":        func(r *Request) { r.FFEffectsMax = 0 },
		"slots no ff":     func(r *Request) { r.FF = nil },
		"no name":         func(r *Request) { r.Name = "" },
		"long name":       func(r *Request) { r.Name = strings.Repeat("x", 80) },
		"control in name": func(r *Request) { r.Name = "a\nb" },
		"no inputs":       func(r *Request) { r.Abs, r.Keys = nil, nil },
	} {
		req := g920(3)
		mutate(&req)
		c.send(req)
		if m := c.recv(); m.Op != OpError || m.ID != 3 {
			t.Errorf("%s: got %+v, want an error", name, m)
		}
	}
	if got := h.b.calls(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

// Events carry only key, axis and sync types (no EV_REL mouse motion, no
// EV_MSC, no FF the client could play on itself); a bad batch is refused
// whole.
func TestEventsRefuseOtherTypes(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(g920(1))
	c.recv()
	for _, typ := range []uint16{0x02, 0x04, evFF, 0x11} {
		c.send(Request{Op: OpEvents, ID: 1, Events: []Event{{Type: evAbs}, {Type: typ}}})
		if m := c.recv(); m.Op != OpError {
			t.Errorf("type %#x: got %+v, want an error", typ, m)
		}
	}
	c.send(Request{Op: OpEvents, ID: 2, Events: []Event{{Type: evAbs}}})
	if m := c.recv(); m.Op != OpError || !strings.Contains(m.Msg, "no wheel") {
		t.Errorf("unknown wheel: got %+v", m)
	}
	c.send(Request{Op: OpEvents, ID: 1, Events: make([]Event, maxEventsPerPacket+1)})
	if m := c.recv(); m.Op != OpError {
		t.Errorf("oversized batch: got %+v", m)
	}
	if n := len(h.b.devs[0].writes); n != 0 {
		t.Errorf("%d writes reached the device", n)
	}
}

// A Session holds at most MaxWheels, across connections; destroying one
// frees its slot.
func TestWheelCap(t *testing.T) {
	h := newHarness(t)
	a, b := h.dial(), h.dial()
	for i := range uint32(MaxWheels) {
		c := a
		if i%2 == 1 {
			c = b
		}
		c.send(g920(i))
		if m := c.recv(); m.Op != OpCreated {
			t.Fatalf("wheel %d: %+v", i, m)
		}
	}
	a.send(g920(99))
	if m := a.recv(); m.Op != OpError {
		t.Errorf("wheel past the cap: %+v", m)
	}
	a.send(Request{Op: OpDestroy, ID: 0})
	a.send(g920(99))
	if m := a.recv(); m.Op != OpCreated {
		t.Errorf("after a destroy: %+v", m)
	}
}

// A create for an id the connection holds replaces that wheel, withdrawn and
// destroyed first; a destroy of an unknown id is silent. So a client that
// recreates a wheel after an error can't loop.
func TestCreateReplacesAndDestroyIsIdempotent(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(g920(5))
	c.recv()
	c.send(g920(5))
	if m := c.recv(); m.Op != OpCreated || m.ID != 5 {
		t.Fatalf("replace: %+v", m)
	}
	want := []string{"create event1", "publish event1", "withdraw event1", "close event1", "create event2", "publish event2"}
	if got := h.b.calls(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	c.send(Request{Op: OpDestroy, ID: 99})
	c.send(Request{Op: OpEvents, ID: 99, Events: []Event{{Type: evAbs}}})
	if m := c.recv(); m.Op != OpError || !strings.Contains(m.Msg, "no wheel 99") {
		t.Errorf("got %+v, want only the events error", m)
	}
	// An invalid replacement leaves the old wheel alone.
	bad := g920(5)
	bad.Keys = []uint16{28}
	c.send(bad)
	c.recv()
	if h.b.devs[1].closed {
		t.Error("invalid create destroyed the wheel it would replace")
	}
}

// Past MaxConns connections the helper hangs up.
func TestConnCap(t *testing.T) {
	h := newHarness(t)
	for range MaxConns {
		c := h.dial()
		c.send(g920(1))
		c.recv()
	}
	extra := h.dial()
	_ = extra.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := extra.c.Read(make([]byte, 16)); err == nil && n > 0 {
		t.Errorf("connection past the cap answered")
	}
}

// A flood past the rate is dropped, not queued.
func TestEventRateLimit(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(g920(1))
	c.recv()
	batch := make([]Event, maxEventsPerPacket)
	for i := range batch {
		batch[i] = Event{Type: evAbs, Value: 1}
	}
	const sent = 3 * EventBurst / maxEventsPerPacket
	for range sent {
		c.send(Request{Op: OpEvents, ID: 1, Events: batch})
	}
	// A destroy queued behind them answers only once they're handled.
	c.send(Request{Op: OpDestroy, ID: 1})
	waitFor(t, func() bool { return slices.Contains(h.b.calls(), "close event1") })
	d := h.b.devs[0]
	d.mu.Lock()
	defer d.mu.Unlock()
	if n := len(d.writes); n == 0 || n >= sent {
		t.Errorf("%d of %d batches written, want the burst only", n, sent)
	}
}

// A wheel whose node won't publish is destroyed again; one whose node
// won't withdraw is kept, holding its minor.
func TestPublishFailures(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	h.p.failPublish = true
	c.send(g920(1))
	if m := c.recv(); m.Op != OpError {
		t.Fatalf("got %+v", m)
	}
	if got, want := h.b.calls(), []string{"create event1", "publish event1", "withdraw event1", "close event1"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	h.p.failPublish, h.p.failWithdraw = false, true
	c.send(g920(2))
	c.recv()
	c.send(Request{Op: OpDestroy, ID: 2})
	if m := c.recv(); m.Op != OpError {
		t.Errorf("destroy: got %+v, want an error", m)
	}
	if h.b.devs[1].closed {
		t.Error("device closed under a node still published")
	}
	// The id is free again, but the stuck wheel keeps its slot.
	h.p.failWithdraw = false
	for i := range uint32(MaxWheels - 1) {
		c.send(g920(10 + i))
		if m := c.recv(); m.Op != OpCreated {
			t.Fatalf("wheel %d: %+v", i, m)
		}
	}
	c.send(g920(2))
	if m := c.recv(); m.Op != OpError || !strings.Contains(m.Msg, "at most") {
		t.Errorf("stuck wheel gave its slot back: %+v", m)
	}
	h.stop()
	if h.b.devs[1].closed {
		t.Error("stuck device closed")
	}
}

// A publish that fails and leaves something it can't withdraw (maybe the
// mounted node) keeps the device open too.
func TestFailedPublishThatWontWithdrawKeepsDevice(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	h.p.failPublish, h.p.failWithdraw = true, true
	c.send(g920(1))
	if m := c.recv(); m.Op != OpError {
		t.Fatalf("got %+v", m)
	}
	if h.b.devs[0].closed {
		t.Error("device closed under a node that may still be mounted")
	}
}

func TestBadPackets(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	if _, err := c.c.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	if m := c.recv(); m.Op != OpError {
		t.Errorf("bad json: %+v", m)
	}
	c.send(Request{Op: "mknod"})
	if m := c.recv(); m.Op != OpError {
		t.Errorf("unknown op: %+v", m)
	}
}
