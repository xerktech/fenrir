package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"

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

// scriptedEventsClient answers each SubscribeToEvents with the next step and
// records when it was called; past the script it blocks until ctx ends.
type scriptedEventsClient struct {
	wolfapi.Client
	steps []func(context.Context) (<-chan *sse.Event, error)
	calls chan time.Time
}

func (f *scriptedEventsClient) SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error) {
	f.calls <- time.Now()
	if len(f.steps) == 0 {
		<-ctx.Done()
		return nil, fmt.Errorf("subscribe: %w", ctx.Err())
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	return step(ctx)
}

func refused(context.Context) (<-chan *sse.Event, error) {
	return nil, errors.New("401 Unauthorized")
}

// streamFor is a stream that stays up for d, then closes.
func streamFor(d time.Duration) func(context.Context) (<-chan *sse.Event, error) {
	return func(context.Context) (<-chan *sse.Event, error) {
		ch := make(chan *sse.Event)
		time.AfterFunc(d, func() { close(ch) })
		return ch, nil
	}
}

func runScripted(t *testing.T, a *Agent, client *scriptedEventsClient) []time.Time {
	t.Helper()
	want := len(client.steps) + 1
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.Run(ctx)
	var calls []time.Time
	for len(calls) < want {
		select {
		case c := <-client.calls:
			calls = append(calls, c)
		case <-time.After(10 * time.Second):
			t.Fatalf("%d subscribes, want %d", len(calls), want)
		}
	}
	return calls
}

// Repeated failures must back off, not retry at the minimum delay forever.
func TestAgentResubscribeBacksOff(t *testing.T) {
	client := &scriptedEventsClient{
		steps: []func(context.Context) (<-chan *sse.Event, error){refused, refused, refused, refused},
		calls: make(chan time.Time, 8),
	}
	a := NewAgent(client)
	a.minResubscribeDelay, a.maxResubscribeDelay = 20*time.Millisecond, 80*time.Millisecond
	calls := runScripted(t, a, client)
	// Delays 20, 40, 80, 80ms; timers never fire early.
	if d := calls[4].Sub(calls[0]); d < 220*time.Millisecond {
		t.Errorf("4 retries took %s, want at least 220ms of backoff", d)
	}
	if d := calls[4].Sub(calls[3]); d < 80*time.Millisecond {
		t.Errorf("last retry after %s, want the 80ms cap", d)
	}
}

// A stream that stayed up resets the backoff: after a Wolf restart the agent
// resubscribes at the minimum delay, not the cap.
func TestAgentResubscribeResetsAfterLongStream(t *testing.T) {
	const minDelay, maxDelay = 20 * time.Millisecond, 160 * time.Millisecond
	// Delays 20, 40, 80, 160, 160ms: at the cap before the stream.
	client := &scriptedEventsClient{
		steps: []func(context.Context) (<-chan *sse.Event, error){refused, refused, refused, refused, refused, streamFor(maxDelay)},
		calls: make(chan time.Time, 8),
	}
	a := NewAgent(client)
	a.minResubscribeDelay, a.maxResubscribeDelay = minDelay, maxDelay
	calls := runScripted(t, a, client)
	if d := calls[5].Sub(calls[4]); d < maxDelay {
		t.Fatalf("delay before the stream %s, want the %s cap", d, maxDelay)
	}
	if d := calls[6].Sub(calls[5]) - maxDelay; d > maxDelay/2 {
		t.Errorf("resubscribed %s after a long stream closed, want about the %s minimum", d, minDelay)
	}
}

// Cancelling Run must not wait for Wolf to answer a StopSession.
func TestAgentRunReturnsDuringStopSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: %s\ndata: {\"session_id\":\"42\"}\n\n", wolfapi.PauseStreamEventType)
		}
		// StopSession, and the stream once its event is sent: a Wolf that
		// never answers.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	a := hotplugAgent(t, wolfapi.NewClient(srv.URL, srv.Client()))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}
