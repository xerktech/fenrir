package wolfapi_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

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

// captureErrors swaps utilruntime's error handlers for a recorder.
func captureErrors(t *testing.T) func() []error {
	t.Helper()
	var mu sync.Mutex
	var errs []error
	saved := utilruntime.ErrorHandlers
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{
		func(_ context.Context, err error, _ string, _ ...any) {
			mu.Lock()
			defer mu.Unlock()
			errs = append(errs, err)
		},
	}
	t.Cleanup(func() { utilruntime.ErrorHandlers = saved })
	return func() []error {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(errs)
	}
}

// drain reads ch until it closes.
func drain(t *testing.T, ch <-chan *sse.Event) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("channel not closed after the stream ended")
		}
	}
}

// Why a stream ended is logged: an event over maxEventSize ends it, and that
// event and every one after it are lost with only a closed channel to show
// for it (XERK-1449).
func TestSubscribeToEventsLogsStreamError(t *testing.T) {
	errs := captureErrors(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: PlugDeviceEvent\ndata: %s\n\n", strings.Repeat("x", 5<<20))
	}))
	t.Cleanup(srv.Close)

	ch, err := wolfapi.NewClient(srv.URL, srv.Client()).SubscribeToEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)
	if got := errs(); len(got) != 1 || !errors.Is(got[0], bufio.ErrTooLong) {
		t.Errorf("logged %v, want one %v", got, bufio.ErrTooLong)
	}
}

// A stream Wolf closes cleanly, or one the caller ends, logs no error.
func TestSubscribeToEventsLogsNoErrorOnCleanEnd(t *testing.T) {
	errs := captureErrors(t)
	closes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: PauseStreamEvent\ndata: {}\n\n")
	}))
	t.Cleanup(closes.Close)
	holds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_ = http.NewResponseController(w).Flush() // a failed flush fails the subscribe
		<-r.Context().Done()
	}))
	t.Cleanup(holds.Close)

	ch, err := wolfapi.NewClient(closes.URL, closes.Client()).SubscribeToEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)

	ctx, cancel := context.WithCancel(t.Context())
	ch, err = wolfapi.NewClient(holds.URL, holds.Client()).SubscribeToEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	drain(t, ch)

	// A deadline ends the read with an error the library passes through.
	ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	ch, err = wolfapi.NewClient(holds.URL, holds.Client()).SubscribeToEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)

	if got := errs(); len(got) != 0 {
		t.Errorf("clean end logged %v", got)
	}
}
