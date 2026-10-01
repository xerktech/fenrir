package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// The agent must keep handling Wolf's events after a failed subscribe and
// after Wolf closes the stream (XERK-1367): the SSE library never retries a
// stream that ends with EOF, and Run used to return on the first failure.
func TestAgentResubscribes(t *testing.T) {
	var requests atomic.Int32
	stopped := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			switch requests.Add(1) {
			case 1:
				// A first subscribe that fails.
				w.WriteHeader(http.StatusUnauthorized)
			case 2:
				// A stream Wolf closes cleanly.
				fmt.Fprint(w, "event: Other\ndata: {}\n\n")
			default:
				fmt.Fprintf(w, "event: %s\ndata: {\"session_id\":\"42\"}\n\n", wolfapi.PauseStreamEventType)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}
				<-r.Context().Done()
			}
		case "/api/v1/sessions/stop":
			select {
			case stopped <- "42":
			default:
			}
			fmt.Fprint(w, `{"success":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	a := hotplugAgent(t, wolfapi.NewClient(srv.URL, srv.Client()))
	a.minResubscribeDelay, a.maxResubscribeDelay = time.Millisecond, 10*time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatalf("pause after resubscribing never stopped the session; %d subscribes", requests.Load())
	}
	if n := requests.Load(); n < 3 {
		t.Errorf("%d subscribes, want 3", n)
	}
	// Run returns once its context ends, even mid-stream.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}
