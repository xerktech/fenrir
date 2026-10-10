package controllers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// lobbyClient serves events and one lobby, and records joins. As in Wolf, a
// joined stream is among the lobby's connected sessions.
type lobbyClient struct {
	fakeEventsClient
	joined  chan [2]string // lobby ID, session ID
	joinErr error

	mu        sync.Mutex
	joins     int
	connected []string
	listed    []wolfapi.Session // ListSessions
	stops     []string          // StopSession calls
}

func newLobbyClient() *lobbyClient {
	return &lobbyClient{fakeEventsClient: fakeEventsClient{events: make(chan *sse.Event)}, joined: make(chan [2]string, 10)}
}

func (c *lobbyClient) ListLobbies(context.Context) ([]wolfapi.Lobby, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return []wolfapi.Lobby{{ID: "lobby-1", ConnectedSessions: slices.Clone(c.connected)}}, nil
}

func (c *lobbyClient) JoinLobby(_ context.Context, lobbyID, sessionID string) error {
	c.mu.Lock()
	c.joins++
	if c.joinErr == nil {
		c.connected = append(c.connected, sessionID)
	}
	c.mu.Unlock()
	c.joined <- [2]string{lobbyID, sessionID}
	return c.joinErr
}

func (c *lobbyClient) ListSessions(context.Context) ([]wolfapi.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.listed), nil
}

func (c *lobbyClient) StopSession(_ context.Context, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops = append(c.stops, sessionID)
	return nil
}

// leave is Wolf pausing (listed) or stopping a joined stream: it leaves the
// lobby, and a stopped one is no longer listed.
func (c *lobbyClient) leave(listed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = nil
	c.listed = nil
	if listed {
		c.listed = []wolfapi.Session{{ClientID: wolfStreamID}}
	}
}

func (c *lobbyClient) stopCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.stops)
}

func (c *lobbyClient) send(typ wolfapi.WolfEventType, data string) {
	c.events <- &sse.Event{Event: []byte(typ), Data: []byte(data)}
}

// startStream sends what Wolf fires as a client starts a stream.
func (c *lobbyClient) startStream() {
	id := wolfStreamID
	c.send(wolfapi.VideoSessionEventType, `{"session_id":"`+id+`","aes_key":"secret"}`)
	c.send(wolfapi.AudioSessionEventType, `{"session_id":"`+id+`","aes_key":"secret"}`)
	c.send(wolfapi.RTPVideoPingEventType, `{"client_ip":"10.0.0.2","client_port":1}`)
	c.send(wolfapi.RTPAudioPingEventType, `{"client_ip":"10.0.0.2","client_port":2}`)
}

func lobbyAgent(t *testing.T, client *lobbyClient) *Agent {
	t.Helper()
	a := hotplugAgent(t, client)
	a.lobby.settle = 0
	go a.Run(t.Context())
	return a
}

func (c *lobbyClient) wantJoin(t *testing.T) {
	t.Helper()
	session := wolfStreamID
	select {
	case j := <-c.joined:
		if j != [2]string{"lobby-1", session} {
			t.Errorf("joined %v, want session %s into lobby-1", j, session)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("session %s never joined the lobby", session)
	}
}

func (c *lobbyClient) wantNoJoin(t *testing.T) {
	t.Helper()
	select {
	case j := <-c.joined:
		t.Errorf("unexpected join %v", j)
	case <-time.After(100 * time.Millisecond):
	}
}

// wolfStreamID is the session ID real Wolf gives every stream its API adds
// (the hash of an empty client cert): a pod's launch and resumes share it.
const wolfStreamID = "6142509188972423790"

// Each stream (the launch, then every /resume) joins the lobby the game runs
// on once both of its pipelines have started, and only once.
func TestAgentJoinsEachStreamToLobby(t *testing.T) {
	for _, end := range []wolfapi.WolfEventType{wolfapi.PauseStreamEventType, wolfapi.StopStreamEventType} {
		t.Run(string(end), func(t *testing.T) {
			client := newLobbyClient()
			lobbyAgent(t, client)

			client.startStream()
			client.wantJoin(t)
			// Pings keep coming for the whole stream.
			client.send(wolfapi.RTPVideoPingEventType, `{}`)
			client.send(wolfapi.RTPAudioPingEventType, `{}`)
			client.wantNoJoin(t)

			// A disconnect pauses the stream; a resume the operator does
			// before Wolf noticed stops it instead.
			client.send(end, `{"session_id":"`+wolfStreamID+`"}`)
			client.startStream()
			client.wantJoin(t)
		})
	}
}

// A new stream's setup is seen even if the old stream's end was missed.
func TestAgentRejoinsOnRepeatedSetup(t *testing.T) {
	client := newLobbyClient()
	lobbyAgent(t, client)
	client.startStream()
	client.wantJoin(t)
	client.startStream()
	client.wantJoin(t)
}

// Pings before the stream's pipelines were set up (or of only one kind) mean
// the pipelines aren't running: a join now would miss the producer switch.
func TestAgentWaitsForBothPipelines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []wolfapi.WolfEventType
	}{
		{"video ping before its setup", []wolfapi.WolfEventType{
			wolfapi.RTPVideoPingEventType, wolfapi.VideoSessionEventType, wolfapi.AudioSessionEventType, wolfapi.RTPAudioPingEventType,
		}},
		{"audio ping before its setup", []wolfapi.WolfEventType{
			wolfapi.RTPAudioPingEventType, wolfapi.VideoSessionEventType, wolfapi.AudioSessionEventType, wolfapi.RTPVideoPingEventType,
		}},
		{"video ping before video setup", []wolfapi.WolfEventType{
			wolfapi.AudioSessionEventType, wolfapi.RTPVideoPingEventType, wolfapi.VideoSessionEventType, wolfapi.RTPAudioPingEventType,
		}},
		{"audio not set up", []wolfapi.WolfEventType{
			wolfapi.VideoSessionEventType, wolfapi.RTPVideoPingEventType, wolfapi.RTPAudioPingEventType,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newLobbyClient()
			lobbyAgent(t, client)
			for _, ev := range tc.events {
				client.send(ev, `{"session_id":"`+wolfStreamID+`"}`)
			}
			client.wantNoJoin(t)

			client.send(wolfapi.AudioSessionEventType, `{"session_id":"`+wolfStreamID+`"}`)
			client.send(wolfapi.RTPAudioPingEventType, `{}`)
			client.send(wolfapi.RTPVideoPingEventType, `{}`)
			if tc.name != "audio not set up" {
				// A second AudioSession is a new stream: start over.
				client.send(wolfapi.VideoSessionEventType, `{"session_id":"`+wolfStreamID+`"}`)
				client.send(wolfapi.RTPVideoPingEventType, `{}`)
				client.send(wolfapi.RTPAudioPingEventType, `{}`)
			}
			client.wantJoin(t)
		})
	}
}

// A failed join is retried on later pings, a bounded number of times.
func TestAgentRetriesFailedJoin(t *testing.T) {
	client := newLobbyClient()
	client.joinErr = errors.New("Lobby join timed out")
	lobbyAgent(t, client)

	client.startStream()
	for range maxLobbyJoinAttempts + 3 {
		client.send(wolfapi.RTPVideoPingEventType, `{}`)
		client.send(wolfapi.RTPAudioPingEventType, `{}`)
		time.Sleep(20 * time.Millisecond)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.joins != maxLobbyJoinAttempts {
		t.Errorf("%d join attempts, want %d", client.joins, maxLobbyJoinAttempts)
	}
}

func unplugEvent(session string) string {
	return `{"session_id":"` + session + `","udev_events":[],"udev_hw_db_entries":[["c13:69",[]]]}`
}

func deviceEntry(t *testing.T, a *Agent) string {
	t.Helper()
	entry := filepath.Join(a.udevDataPath, "c13:69")
	if err := os.WriteFile(entry, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return entry
}

// Joining moves the stream's joypads to the lobby with an unplug addressed
// to the stream; once joined, real unplugs are re-fired at the lobby. Only
// the latter may remove the game's devices.
func TestAgentKeepsDevicesMovedToLobby(t *testing.T) {
	client := newLobbyClient()
	a := lobbyAgent(t, client)
	entry := deviceEntry(t, a)
	unplug := func(session string) {
		client.send(wolfapi.UnplugDeviceEventType, unplugEvent(session))
		client.send("Done", "") // unbuffered: the unplug has been handled
	}

	client.startStream()
	client.wantJoin(t)
	unplug(wolfStreamID)
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("the join's unplug removed the device: %v", err)
	}
	unplug("lobby-1")
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Errorf("the lobby's unplug kept the device: %v", err)
	}
}

// Until the join is under way, a stream's unplugs are its own: Wolf relays
// unplugs to the lobby only for streams already in it.
func TestAgentUnplugBeforeJoin(t *testing.T) {
	client := newLobbyClient()
	a := lobbyAgent(t, client)
	a.lobby.settle = time.Hour // stuck in the settle window
	entry := deviceEntry(t, a)
	client.startStream()
	client.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	client.send("Done", "")
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Errorf("unplug ignored before the join: %v", err)
	}
}

// blockingJoinClient holds JoinLobby until release, then fails it.
type blockingJoinClient struct {
	*lobbyClient
	release chan struct{}
}

func (c *blockingJoinClient) JoinLobby(_ context.Context, lobbyID, sessionID string) error {
	c.joined <- [2]string{lobbyID, sessionID}
	<-c.release
	return errors.New("Lobby or session not found")
}

// An unplug while the join is in flight is held: handled if the join fails
// (it was the stream's own), so no stale node is left behind.
func TestAgentReplaysUnplugAfterFailedJoin(t *testing.T) {
	client := &blockingJoinClient{lobbyClient: newLobbyClient(), release: make(chan struct{})}
	a := hotplugAgent(t, client)
	a.lobby.settle = 0
	go a.Run(t.Context())
	entry := deviceEntry(t, a)

	client.startStream()
	client.wantJoin(t)
	client.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	client.send("Done", "")
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("unplug acted on mid-join: %v", err)
	}
	close(client.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entry); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("held unplug not handled after the join failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A stream that ends before its pings (or during settle) is never joined:
// the stream after it shares its session ID, and would be joined before its
// pipelines run.
func TestAgentForgetsEndedStream(t *testing.T) {
	t.Run("before pings", func(t *testing.T) {
		client := newLobbyClient()
		lobbyAgent(t, client)
		client.send(wolfapi.VideoSessionEventType, `{"session_id":"`+wolfStreamID+`"}`)
		client.send(wolfapi.AudioSessionEventType, `{"session_id":"`+wolfStreamID+`"}`)
		client.send(wolfapi.StopStreamEventType, `{"session_id":"`+wolfStreamID+`"}`)
		client.send(wolfapi.RTPVideoPingEventType, `{}`)
		client.send(wolfapi.RTPAudioPingEventType, `{}`)
		client.wantNoJoin(t)
	})
	t.Run("during settle", func(t *testing.T) {
		client := newLobbyClient()
		a := lobbyAgent(t, client)
		a.lobby.mu.Lock()
		a.lobby.settle = 50 * time.Millisecond
		a.lobby.mu.Unlock()
		client.startStream()
		client.send(wolfapi.PauseStreamEventType, `{"session_id":"`+wolfStreamID+`"}`)
		time.Sleep(100 * time.Millisecond)
		client.wantNoJoin(t)
	})
}

// A settle timer left by an ended stream must not join the next stream
// early, before its own settle.
func TestAgentStaleTimerDoesNotJoinNextStreamEarly(t *testing.T) {
	client := newLobbyClient()
	a := hotplugAgent(t, client)
	a.lobby.settle = 300 * time.Millisecond
	go a.Run(t.Context())
	client.startStream()
	time.Sleep(20 * time.Millisecond)
	client.send(wolfapi.PauseStreamEventType, `{"session_id":"`+wolfStreamID+`"}`)
	time.Sleep(200 * time.Millisecond)
	client.startStream()
	bPings := time.Now()
	select {
	case <-client.joined:
		if d := time.Since(bPings); d < 250*time.Millisecond {
			t.Errorf("stream B joined %v after its pings, settle is 300ms (stale timer)", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B never joined")
	}
}

// seqJoinClient blocks its first join until rel1 (then succeeds) and its
// second until rel2 (then fails).
type seqJoinClient struct {
	*lobbyClient
	mu2        sync.Mutex
	n          int
	rel1, rel2 chan struct{}
}

func (c *seqJoinClient) JoinLobby(_ context.Context, lobbyID, sessionID string) error {
	c.mu2.Lock()
	c.n++
	n := c.n
	c.mu2.Unlock()
	c.joined <- [2]string{lobbyID, sessionID}
	switch n {
	case 1:
		<-c.rel1
		return nil
	case 2:
		<-c.rel2
		return errors.New("B failed")
	}
	return nil
}

// overlap runs stream A's join, which outlasts A's pause and stream B's
// in-flight join (holding an unplug for B); A's returns OK, then B's fails.
func overlap(t *testing.T) (client *seqJoinClient, entry string) {
	t.Helper()
	client = &seqJoinClient{lobbyClient: newLobbyClient(), rel1: make(chan struct{}), rel2: make(chan struct{})}
	a := hotplugAgent(t, client)
	a.lobby.settle = 0
	go a.Run(t.Context())
	client.startStream()
	client.wantJoin(t) // A in flight
	client.send(wolfapi.PauseStreamEventType, `{"session_id":"`+wolfStreamID+`"}`)
	client.startStream()
	client.wantJoin(t) // B in flight
	entry = deviceEntry(t, a)
	client.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	client.send("Done", "")
	close(client.rel1) // A's stale join returns OK
	time.Sleep(50 * time.Millisecond)
	close(client.rel2) // B's join fails
	time.Sleep(50 * time.Millisecond)
	return client, entry
}

// An ended stream's late join success must not mark the next stream joined:
// the next stream's failed join is retried.
func TestAgentStaleJoinResultDoesNotClobberNextStream(t *testing.T) {
	client, _ := overlap(t)
	client.send(wolfapi.RTPVideoPingEventType, `{}`)
	client.send(wolfapi.RTPAudioPingEventType, `{}`)
	client.wantJoin(t)
}

// The next stream's held unplugs are handled when its join fails, even though
// the ended stream's join returned meanwhile.
func TestAgentStaleJoinDoesNotStealHeldUnplugs(t *testing.T) {
	_, entry := overlap(t)
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Errorf("B's held unplug was never handled after B's join failed: %v", err)
	}
}

// resubClient hands out a fresh event channel per subscribe: closing one
// makes the agent resubscribe (XERK-1367).
type resubClient struct {
	*lobbyClient
	subs chan chan *sse.Event
}

func (c *resubClient) SubscribeToEvents(context.Context) (<-chan *sse.Event, error) {
	ch := make(chan *sse.Event)
	c.subs <- ch
	return ch, nil
}

func sendOn(ch chan *sse.Event, typ wolfapi.WolfEventType, data string) {
	ch <- &sse.Event{Event: []byte(typ), Data: []byte(data)}
}

// The joiner's state outlives a resubscribe.
func TestAgentResubscribeBetweenSetupAndPings(t *testing.T) {
	client := &resubClient{lobbyClient: newLobbyClient(), subs: make(chan chan *sse.Event, 4)}
	a := hotplugAgent(t, client)
	a.lobby.settle = 0
	a.minResubscribeDelay = time.Millisecond
	go a.Run(t.Context())
	ch := <-client.subs
	sendOn(ch, wolfapi.VideoSessionEventType, `{"session_id":"`+wolfStreamID+`"}`)
	sendOn(ch, wolfapi.AudioSessionEventType, `{"session_id":"`+wolfStreamID+`"}`)
	close(ch)
	ch = <-client.subs
	sendOn(ch, wolfapi.RTPVideoPingEventType, `{}`)
	sendOn(ch, wolfapi.RTPAudioPingEventType, `{}`)
	client.wantJoin(t)
	// joined state survives another resubscribe: the stream's own unplug stays held
	entry := deviceEntry(t, a)
	close(ch)
	ch = <-client.subs
	sendOn(ch, wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	sendOn(ch, "Done", "")
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("joined state lost across resubscribe: %v", err)
	}
	// next stream after resubscribe still joins
	sendOn(ch, wolfapi.PauseStreamEventType, `{"session_id":"`+wolfStreamID+`"}`)
	close(ch)
	ch = <-client.subs
	for _, e := range []wolfapi.WolfEventType{wolfapi.VideoSessionEventType, wolfapi.AudioSessionEventType} {
		sendOn(ch, e, `{"session_id":"`+wolfStreamID+`"}`)
	}
	sendOn(ch, wolfapi.RTPVideoPingEventType, `{}`)
	sendOn(ch, wolfapi.RTPAudioPingEventType, `{}`)
	client.wantJoin(t)
}

func TestAgentResubscribeDuringSettle(t *testing.T) {
	client := &resubClient{lobbyClient: newLobbyClient(), subs: make(chan chan *sse.Event, 4)}
	a := hotplugAgent(t, client)
	a.lobby.settle = 100 * time.Millisecond
	a.minResubscribeDelay = time.Millisecond
	go a.Run(t.Context())
	ch := <-client.subs
	for _, e := range []wolfapi.WolfEventType{wolfapi.VideoSessionEventType, wolfapi.AudioSessionEventType} {
		sendOn(ch, e, `{"session_id":"`+wolfStreamID+`"}`)
	}
	sendOn(ch, wolfapi.RTPVideoPingEventType, `{}`)
	sendOn(ch, wolfapi.RTPAudioPingEventType, `{}`)
	close(ch)
	<-client.subs
	client.wantJoin(t)
}

// nJoinClient blocks join i until rel[i] is closed, then returns errs[i];
// joins past len(errs) succeed at once.
type nJoinClient struct {
	*lobbyClient
	mu   sync.Mutex
	n    int
	rel  []chan struct{}
	errs []error
}

func newNJoin(errs ...error) *nJoinClient {
	c := &nJoinClient{lobbyClient: newLobbyClient(), errs: errs}
	for range errs {
		c.rel = append(c.rel, make(chan struct{}))
	}
	return c
}

func (c *nJoinClient) JoinLobby(_ context.Context, lobbyID, sessionID string) error {
	c.mu.Lock()
	i := c.n
	c.n++
	c.mu.Unlock()
	c.joined <- [2]string{lobbyID, sessionID}
	if i < len(c.rel) {
		<-c.rel[i]
		return c.errs[i]
	}
	return nil
}

// joinReturned waits for a released join's result to be handled.
func joinReturned() { time.Sleep(60 * time.Millisecond) }

func pauseStream(c *nJoinClient) {
	c.send(wolfapi.PauseStreamEventType, `{"session_id":"`+wolfStreamID+`"}`)
}

// B's join succeeds after A's stale return: B's moved-to-lobby unplug must never be acted on.
func TestAgentStaleThenNextJoinKeepsDevice(t *testing.T) {
	c := newNJoin(nil, nil)
	a := hotplugAgent(t, c)
	a.lobby.settle = 0
	go a.Run(t.Context())
	c.startStream()
	c.wantJoin(t)
	pauseStream(c)
	c.startStream()
	c.wantJoin(t)
	entry := deviceEntry(t, a)
	c.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	c.send("Done", "")
	close(c.rel[0])
	joinReturned()
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("stale A return acted on B's held unplug while B in flight: %v", err)
	}
	close(c.rel[1])
	joinReturned()
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("B joined OK but its moved-to-lobby unplug was acted on: %v", err)
	}
	c.wantNoJoin(t)
}

// A's own held unplug (held before A ended) must not leak into B's failure replay:
// device names are reused by the next stream.
func TestAgentEndedStreamsHeldUnplugDoesNotLeak(t *testing.T) {
	c := newNJoin(nil, errors.New("B failed"))
	a := hotplugAgent(t, c)
	a.lobby.settle = 0
	go a.Run(t.Context())
	c.startStream()
	c.wantJoin(t)
	c.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID)) // A's, held
	c.send("Done", "")
	pauseStream(c)
	c.startStream()
	c.wantJoin(t)
	entry := deviceEntry(t, a) // B's device, same name
	close(c.rel[0])
	joinReturned()
	close(c.rel[1])
	joinReturned()
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("A's held unplug replayed on B's failure, removed B's device: %v", err)
	}
}

// Three streams: A and B stale, C's held unplug is C's alone.
func TestAgentHeldUnplugsAcrossThreeStreams(t *testing.T) {
	for _, cFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "C fails", false: "C ok"}[cFails], func(t *testing.T) {
			var cErr error
			if cFails {
				cErr = errors.New("C failed")
			}
			c := newNJoin(nil, errors.New("B failed"), cErr)
			a := hotplugAgent(t, c)
			a.lobby.settle = 0
			go a.Run(t.Context())
			c.startStream()
			c.wantJoin(t) // A
			pauseStream(c)
			c.startStream()
			c.wantJoin(t)                                                    // B
			c.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID)) // B's held
			c.send("Done", "")
			pauseStream(c)
			c.startStream()
			c.wantJoin(t) // C
			entry := deviceEntry(t, a)
			c.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID)) // C's held
			c.send("Done", "")
			close(c.rel[0])
			joinReturned()
			close(c.rel[1])
			joinReturned()
			if _, err := os.Stat(entry); err != nil {
				t.Fatalf("stale A/B return acted on C's held unplug: %v", err)
			}
			close(c.rel[2])
			joinReturned()
			_, err := os.Stat(entry)
			if cFails && !os.IsNotExist(err) {
				t.Fatalf("C failed but its held unplug was not replayed: %v", err)
			}
			if !cFails && err != nil {
				t.Fatalf("C joined but its unplug was acted on: %v", err)
			}
			if cFails {
				c.send(wolfapi.RTPVideoPingEventType, `{}`)
				c.send(wolfapi.RTPAudioPingEventType, `{}`)
				c.wantJoin(t) // C retried
			} else {
				c.wantNoJoin(t)
			}
		})
	}
}

// A failed join's replayed unplugs are not replayed again by the retry's failure:
// the device may have been re-plugged under the same name in between.
func TestAgentRetryFailureDoesNotReplayAgain(t *testing.T) {
	c := newNJoin(errors.New("1 failed"), errors.New("2 failed"))
	a := hotplugAgent(t, c)
	a.lobby.settle = 0
	go a.Run(t.Context())
	c.startStream()
	c.wantJoin(t)
	entry := deviceEntry(t, a)
	c.send(wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	c.send("Done", "")
	close(c.rel[0])
	joinReturned()
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("held unplug not replayed after failure: %v", err)
	}
	entry = deviceEntry(t, a) // re-plugged, same name
	c.send(wolfapi.RTPVideoPingEventType, `{}`)
	c.send(wolfapi.RTPAudioPingEventType, `{}`)
	c.wantJoin(t)
	close(c.rel[1])
	joinReturned()
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("retry failure replayed the first attempt's unplug again: %v", err)
	}
}

// joinedOnFreshSubscription joins a stream to the lobby with a published
// device, then closes the event stream, leaving the agent waiting to
// resubscribe; it returns the next subscription's channel and the device.
func joinedOnFreshSubscription(t *testing.T, client *resubClient, beforeResubscribe func()) (a *Agent, ch chan *sse.Event, entry string) {
	t.Helper()
	a = hotplugAgent(t, client)
	a.lobby.settle = 0
	a.minResubscribeDelay = 50 * time.Millisecond
	go a.Run(t.Context())
	ch = <-client.subs
	for _, e := range []wolfapi.WolfEventType{wolfapi.VideoSessionEventType, wolfapi.AudioSessionEventType} {
		sendOn(ch, e, `{"session_id":"`+wolfStreamID+`"}`)
	}
	sendOn(ch, wolfapi.RTPVideoPingEventType, `{}`)
	sendOn(ch, wolfapi.RTPAudioPingEventType, `{}`)
	client.wantJoin(t)
	entry = deviceEntry(t, a)
	close(ch)
	// Wolf's events while the agent waits to resubscribe are lost.
	beforeResubscribe()
	ch = <-client.subs
	// Unbuffered: once taken, the catch-up before it is done.
	sendOn(ch, "Done", "")
	return a, ch, entry
}

// A pause Wolf sends while the event stream is down is lost for good (Wolf
// replays nothing), but it takes the stream out of the lobby. After
// resubscribing, the agent must stop it as the pause would have, so the
// operator sees the disconnect, and drop its devices (XERK-1378).
func TestAgentCatchesUpPauseMissedWhileResubscribing(t *testing.T) {
	client := &resubClient{lobbyClient: newLobbyClient(), subs: make(chan chan *sse.Event, 4)}
	_, _, entry := joinedOnFreshSubscription(t, client, func() { client.leave(true) })
	if got := client.stopCalls(); !slices.Equal(got, []string{wolfStreamID}) {
		t.Errorf("StopSession calls %v, want [%s]", got, wolfStreamID)
	}
	if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("device of the missed-paused stream kept: %v", err)
	}
}

// A stream Wolf stopped while the event stream was down is no longer listed:
// nothing to stop, but its devices are gone.
func TestAgentCatchesUpStopMissedWhileResubscribing(t *testing.T) {
	client := &resubClient{lobbyClient: newLobbyClient(), subs: make(chan chan *sse.Event, 4)}
	_, _, entry := joinedOnFreshSubscription(t, client, func() { client.leave(false) })
	if got := client.stopCalls(); len(got) != 0 {
		t.Errorf("StopSession calls %v for a stream Wolf no longer lists", got)
	}
	if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("device of the missed-stopped stream kept: %v", err)
	}
}

// A joined stream still in the lobby after a resubscribe is left alone, and
// keeps its joined state.
func TestAgentResubscribeKeepsStreamStillInLobby(t *testing.T) {
	client := &resubClient{lobbyClient: newLobbyClient(), subs: make(chan chan *sse.Event, 4)}
	a, ch, entry := joinedOnFreshSubscription(t, client, func() {})
	if got := client.stopCalls(); len(got) != 0 {
		t.Errorf("StopSession calls %v for a stream still in the lobby", got)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("device of a live stream dropped: %v", err)
	}
	if s := a.lobby.joinedStream(); s != wolfStreamID {
		t.Errorf("joined stream %q after resubscribe, want %s", s, wolfStreamID)
	}
	// Its own unplug is still held as a joined stream's.
	sendOn(ch, wolfapi.UnplugDeviceEventType, unplugEvent(wolfStreamID))
	sendOn(ch, "Done", "")
	if _, err := os.Stat(entry); err != nil {
		t.Errorf("joined state lost across resubscribe: %v", err)
	}
}
