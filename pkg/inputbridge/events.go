package inputbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// The App pod has its own network namespace, so the node's udevd no longer
// announces Wolf's devices to it, and libudev consumers (Steam's Big Picture)
// miss pads plugged after they started. wolf-agent passes each udev event to
// the bridge (OpUevent) once its node and entry are published; the bridge
// streams them to the App pod's udev-relay sidecar, which broadcasts them
// there (fakeudev.Announcer). The stream runs on its own socket, which only
// the relay mounts: the App container can't announce or see anything through
// it.

// eventHub keeps the devices currently announced and the relays listening.
type eventHub struct {
	mu sync.Mutex
	// Announced adds not yet removed, in order, keyed by DEVPATH.
	added []map[string]string
	subs  map[chan map[string]string]struct{}
}

// subscriberBuffer bounds the events queued for one relay; a relay that far
// behind is dropped and resyncs from the snapshot when it reconnects.
const subscriberBuffer = 256

// publish records ev and sends it to every relay.
func (h *eventHub) publish(ev map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	path := ev["DEVPATH"]
	h.added = slices.DeleteFunc(h.added, func(a map[string]string) bool { return a["DEVPATH"] == path })
	if ev["ACTION"] == "add" {
		h.added = append(h.added, ev)
	}
	h.sendLocked(ev)
}

// removeAll announces the removal of every device still announced, newest
// first: Wolf destroys a stopped stream's devices without unplug events.
func (h *eventHub) removeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range slices.Backward(h.added) {
		ev := maps.Clone(a)
		ev["ACTION"] = "remove"
		h.sendLocked(ev)
	}
	h.added = nil
}

func (h *eventHub) sendLocked(ev map[string]string) {
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns a channel starting with an add for every device already
// announced, so a relay that (re)connects late misses no pad.
func (h *eventHub) subscribe() chan map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan map[string]string, subscriberBuffer+len(h.added))
	for _, a := range h.added {
		ch <- a
	}
	if h.subs == nil {
		h.subs = map[chan map[string]string]struct{}{}
	}
	h.subs[ch] = struct{}{}
	return ch
}

func (h *eventHub) unsubscribe(ch chan map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// uevent checks and publishes one udev event from wolf-agent.
func (s *Server) uevent(props map[string]string) error {
	if props["ACTION"] != "add" && props["ACTION"] != "remove" {
		return fmt.Errorf("udev event action %q (only add and remove)", props["ACTION"])
	}
	if props["DEVPATH"] == "" || props["SUBSYSTEM"] == "" {
		return fmt.Errorf("udev event without DEVPATH or SUBSYSTEM: %q", props["DEVPATH"])
	}
	s.events.publish(maps.Clone(props))
	return nil
}

// ServeEvents streams udev events, one JSON object per line, to each relay
// connecting on l until ctx ends.
func (s *Server) ServeEvents(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		conn, err := l.Accept()
		switch {
		case err != nil && ctx.Err() != nil:
			return nil //nolint:nilerr // the listener was closed for ctx
		case err != nil:
			return fmt.Errorf("accept: %w", err)
		}
		go s.streamEvents(ctx, conn)
	}
}

func (s *Server) streamEvents(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)
	enc := json.NewEncoder(conn)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				klog.Warning("input-bridge: dropped a udev relay that fell behind")
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := enc.Encode(ev); err != nil {
				klog.Warningf("input-bridge: udev relay: %v", err)
				return
			}
		}
	}
}

// Subscribe connects to a bridge's event socket and calls fn for each udev
// event until the stream ends or fn fails.
func Subscribe(ctx context.Context, socket string, fn func(map[string]string) error) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("input-bridge events: %w", err)
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), maxRequestBytes)
	for sc.Scan() {
		var ev map[string]string
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return fmt.Errorf("input-bridge events: %w", err)
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("input-bridge events: %w", err)
	}
	return nil
}
