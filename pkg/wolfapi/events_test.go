package wolfapi_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// A stream Wolf closes must close the channel, so the caller can resubscribe:
// the SSE library ends an EOF'd stream without retrying or closing it.
func TestSubscribeToEventsClosesOnStreamEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: PauseStreamEvent\ndata: {}\n\n")
	}))
	t.Cleanup(srv.Close)

	ch, err := wolfapi.NewClient(srv.URL, srv.Client()).SubscribeToEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if len(got) != 1 || got[0] != "PauseStreamEvent" {
					t.Errorf("events %q, want [PauseStreamEvent]", got)
				}
				return
			}
			got = append(got, string(ev.Event))
		case <-timeout:
			t.Fatalf("channel not closed after the stream ended; events %q", got)
		}
	}
}

// An event far over the SSE library's 64KiB default (a PlugDeviceEvent with
// many udev hwdb entries is ~200KiB) is delivered whole, not dropped with
// the stream.
func TestSubscribeToEventsDeliversLargeEvent(t *testing.T) {
	data := `{"hwdb":"` + strings.Repeat("x", 1<<20) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: PlugDeviceEvent\ndata: %s\n\nevent: PauseStreamEvent\ndata: {}\n\n", data)
	}))
	t.Cleanup(srv.Close)

	ch, err := wolfapi.NewClient(srv.URL, srv.Client()).SubscribeToEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if len(got) != 2 || got[0] != "PlugDeviceEvent" || got[1] != "PauseStreamEvent" {
					t.Errorf("events %q, want [PlugDeviceEvent PauseStreamEvent]", got)
				}
				return
			}
			if string(ev.Event) == "PlugDeviceEvent" && string(ev.Data) != data {
				t.Errorf("PlugDeviceEvent data is %d bytes, want %d", len(ev.Data), len(data))
			}
			got = append(got, string(ev.Event))
		case <-timeout:
			t.Fatalf("channel not closed after the stream ended; events %q", got)
		}
	}
}

// A refused subscribe is reported at once, not retried inside the library.
func TestSubscribeToEventsReportsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	ch, err := wolfapi.NewClient(srv.URL, srv.Client()).SubscribeToEvents(t.Context())
	if err == nil || ch != nil {
		t.Fatalf("got channel %v, error %v; want a 401 error", ch, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("refusal took %s to report", d)
	}
}

// An unreachable Wolf is reported too.
func TestSubscribeToEventsReportsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	if _, err := wolfapi.NewClient(srv.URL, srv.Client()).SubscribeToEvents(t.Context()); err == nil {
		t.Fatal("subscribe to a closed server succeeded")
	}
}
