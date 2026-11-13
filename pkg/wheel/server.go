package wheel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/klog/v2"
)

// Node is a created wheel's event node: its sysfs name (inputN), its node
// name (eventN) and device numbers.
type Node struct {
	Sysname      string
	Event        string
	Major, Minor uint32
}

// Device is one uinput wheel.
type Device interface {
	// Write emits events, ending with a SYN_REPORT.
	Write(evs []Event) error
	// Close destroys the device and stops its FF reports.
	Close() error
}

// Backend creates uinput wheels: uinput outside tests.
type Backend interface {
	// Create makes a wheel per req (already validated) and calls ff with
	// each FF request the kernel passes it, until Close. ff must not block.
	Create(req *Request, ff func(Message)) (Device, Node, error)
}

// Publisher puts a wheel's node and udev entry into the Session's /dev/input
// and takes them out again: the input-bridge outside tests.
type Publisher interface {
	Publish(n Node, req *Request) error
	Withdraw(n Node) error
}

// Defaults for Server.
const (
	// MaxWheels bounds the wheels the whole Session holds.
	MaxWheels = MaxWheelsPerConn
	// MaxConns bounds concurrent connections: WiVRn holds one.
	MaxConns = 4
	// EventsPerSecond is the sustained event rate per connection. A wheel
	// and pedals at 1 kHz with every axis changing is ~5,000; WiVRn sends
	// at the headset's rate, ~100-500 Hz.
	EventsPerSecond = 4000
	// EventBurst absorbs a burst after a stall.
	EventBurst = 4000
	// maxPacket bounds one request packet; a longer one is truncated and
	// refused as bad JSON.
	maxPacket = 64 << 10
	// sendQueue bounds the messages queued for a client that doesn't read.
	sendQueue    = 256
	writeTimeout = 2 * time.Second
)

// Server answers wheel requests on a SOCK_SEQPACKET listener.
type Server struct {
	Backend   Backend
	Publisher Publisher

	mu     sync.Mutex
	wheels int
	conns  int
}

// Serve accepts connections on l until ctx ends, then destroys every wheel.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		conn, err := l.Accept()
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			return fmt.Errorf("accept: %w", err)
		}
		if !s.take(&s.conns, MaxConns) {
			klog.Warningf("wheel: refusing a connection past %d", MaxConns)
			conn.Close()
			continue
		}
		wg.Go(func() {
			defer s.give(&s.conns)
			s.serveConn(ctx, conn)
		})
	}
}

func (s *Server) take(n *int, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if *n >= limit {
		return false
	}
	*n++
	return true
}

func (s *Server) give(n *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*n--
}

type wheel struct {
	dev  Device
	node Node
}

// conn is one client's connection and the wheels it holds.
type conn struct {
	s      *Server
	c      net.Conn
	out    chan Message
	limit  *rate.Limiter
	wheels map[uint32]*wheel
	// Whether a dropped packet was logged since the last one passed.
	limited bool
}

func (s *Server) serveConn(ctx context.Context, nc net.Conn) {
	c := &conn{
		s:      s,
		c:      nc,
		out:    make(chan Message, sendQueue),
		limit:  rate.NewLimiter(EventsPerSecond, EventBurst),
		wheels: map[uint32]*wheel{},
	}
	ctx, cancel := context.WithCancel(ctx)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.write(ctx)
	}()
	go func() {
		<-ctx.Done()
		nc.Close()
	}()
	c.read()
	cancel()
	// Wheels first: their FF reports stop before the queue goes.
	c.destroyAll()
	<-writerDone
}

func (c *conn) read() {
	buf := make([]byte, maxPacket+1)
	for {
		n, err := c.c.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			// SOCK_SEQPACKET: a zero-length read is the peer's close.
			return
		}
		var req Request
		if n > maxPacket {
			c.send(Message{Op: OpError, Msg: "packet too long"})
			continue
		}
		if err := json.Unmarshal(buf[:n], &req); err != nil {
			c.send(Message{Op: OpError, Msg: "bad request: " + err.Error()})
			continue
		}
		if err := c.handle(&req); err != nil {
			c.send(Message{Op: OpError, ID: req.ID, Msg: err.Error()})
		}
	}
}

func (c *conn) handle(req *Request) error {
	switch req.Op {
	case OpCreate:
		return c.create(req)
	case OpEvents:
		return c.events(req)
	case OpDestroy:
		w, ok := c.wheels[req.ID]
		if !ok {
			return fmt.Errorf("no wheel %d", req.ID)
		}
		delete(c.wheels, req.ID)
		return c.destroy(w)
	}
	return fmt.Errorf("unknown op %q", req.Op)
}

func (c *conn) create(req *Request) error {
	if _, ok := c.wheels[req.ID]; ok {
		return fmt.Errorf("wheel %d exists", req.ID)
	}
	if err := validateCreate(req); err != nil {
		return err
	}
	if !c.s.take(&c.s.wheels, MaxWheels) {
		return fmt.Errorf("at most %d wheels", MaxWheels)
	}
	id := req.ID
	dev, node, err := c.s.Backend.Create(req, func(m Message) {
		m.Op, m.ID = OpFF, id
		c.send(m)
	})
	if err != nil {
		c.s.give(&c.s.wheels)
		return fmt.Errorf("create: %w", err)
	}
	if err := c.s.Publisher.Publish(node, req); err != nil {
		// Withdraw whatever part was published.
		err = errors.Join(err, c.s.Publisher.Withdraw(node), dev.Close())
		c.s.give(&c.s.wheels)
		return fmt.Errorf("publish: %w", err)
	}
	c.wheels[id] = &wheel{dev: dev, node: node}
	klog.InfoS("wheel: created", "id", id, "name", req.Name, "node", node.Event,
		"vendor", fmt.Sprintf("%04x", req.Vendor), "product", fmt.Sprintf("%04x", req.Product))
	c.send(Message{Op: OpCreated, ID: id})
	return nil
}

func (c *conn) events(req *Request) error {
	w, ok := c.wheels[req.ID]
	if !ok {
		return fmt.Errorf("no wheel %d", req.ID)
	}
	if err := validateEvents(req.Events); err != nil {
		return err
	}
	if !c.limit.AllowN(time.Now(), len(req.Events)) {
		// Dropped, as a device past its rate would drop them; logged once
		// per run of drops.
		if !c.limited {
			klog.Warningf("wheel: %d over %d events/s, dropping", req.ID, EventsPerSecond)
			c.limited = true
		}
		return nil
	}
	c.limited = false
	if err := w.dev.Write(withReport(req.Events)); err != nil {
		return fmt.Errorf("wheel %d: %w", req.ID, err)
	}
	return nil
}

// destroy withdraws w's node before the kernel frees its minor for another
// device: a node left mounted would then open that one.
func (c *conn) destroy(w *wheel) error {
	defer c.s.give(&c.s.wheels)
	werr := c.s.Publisher.Withdraw(w.node)
	if werr != nil {
		// Keep the device (and its minor) rather than free the minor under
		// a node still mounted in the App.
		klog.Errorf("wheel: withdrawing %s, keeping the device: %v", w.node.Event, werr)
		return fmt.Errorf("withdraw: %w", werr)
	}
	klog.InfoS("wheel: destroyed", "node", w.node.Event)
	if err := w.dev.Close(); err != nil {
		return fmt.Errorf("destroy %s: %w", w.node.Event, err)
	}
	return nil
}

func (c *conn) destroyAll() {
	for id, w := range c.wheels {
		delete(c.wheels, id)
		if err := c.destroy(w); err != nil {
			klog.Errorf("wheel: %v", err)
		}
	}
}

// send queues m; a client that doesn't read loses messages rather than
// stall the kernel's FF requests.
func (c *conn) send(m Message) { //nolint:gocritic // a copy per queued message
	select {
	case c.out <- m:
	default:
		klog.Warningf("wheel: client not reading, dropped %s %s", m.Op, m.Kind)
	}
}

func (c *conn) write(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.out:
			b, err := json.Marshal(m)
			if err != nil {
				klog.Errorf("wheel: encoding %s: %v", m.Op, err)
				continue
			}
			_ = c.c.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := c.c.Write(b); err != nil {
				c.c.Close()
				return
			}
		}
	}
}
