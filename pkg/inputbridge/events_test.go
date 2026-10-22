package inputbridge

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func uev(action, devpath string) map[string]string {
	return map[string]string{"ACTION": action, "DEVPATH": devpath, "SUBSYSTEM": "input"}
}

// A relay gets every device already announced on connect, then live events;
// Clear announces the removal of what is left, newest first.
func TestEventsReachRelay(t *testing.T) {
	f := newFakeServer(t)
	dir, err := os.MkdirTemp("", "ev") //nolint:usetesting // t.TempDir's path is too long for a socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", filepath.Join(dir, "e"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.ServeEvents(ctx, l) }()

	for _, ev := range []map[string]string{uev("add", "/d/input9"), uev("add", "/d/input9/event5"), uev("add", "/d/input10")} {
		if err := f.Handle(Request{Op: OpUevent, Props: ev}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Handle(Request{Op: OpUevent, Props: uev("remove", "/d/input10")}); err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 16)
	go func() {
		_ = Subscribe(ctx, filepath.Join(dir, "e"), func(ev map[string]string) error {
			got <- ev["ACTION"] + " " + ev["DEVPATH"]
			return nil
		})
	}()
	want := []string{"add /d/input9", "add /d/input9/event5"}
	read := func(n int) []string {
		var out []string
		for range n {
			select {
			case s := <-got:
				out = append(out, s)
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out after %v", out)
			}
		}
		return out
	}
	if snap := read(2); !slices.Equal(snap, want) {
		t.Errorf("snapshot = %v, want %v", snap, want)
	}
	if err := f.Clear(); err != nil {
		t.Fatal(err)
	}
	if live := read(2); !slices.Equal(live, []string{"remove /d/input9/event5", "remove /d/input9"}) {
		t.Errorf("after Clear = %v", live)
	}
}

// Only add and remove events with a device path and subsystem are relayed.
func TestUeventRefusesMalformed(t *testing.T) {
	f := newFakeServer(t)
	for _, props := range []map[string]string{
		uev("change", "/d/x"),
		{"ACTION": "add", "SUBSYSTEM": "input"},
		{"ACTION": "add", "DEVPATH": "/d/x"},
	} {
		if err := f.Handle(Request{Op: OpUevent, Props: props}); err == nil {
			t.Errorf("relayed %v", props)
		}
	}
	if len(f.events.added) != 0 {
		t.Errorf("recorded %v", f.events.added)
	}
}

// A relay too far behind is dropped rather than stalling wolf-agent's
// requests; it resyncs from the snapshot when it redials.
func TestSlowRelayIsDropped(t *testing.T) {
	var h eventHub
	ch := h.subscribe()
	for i := range subscriberBuffer + 1 {
		h.publish(uev("add", "/d/"+string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	n := 0
	for range ch {
		n++
	}
	if n != subscriberBuffer || len(h.subs) != 0 {
		t.Errorf("drained %d, %d subscribers left", n, len(h.subs))
	}
}
