package controllers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// lobbyClient serves events and one lobby, and records joins.
type lobbyClient struct {
	fakeEventsClient
	joined  chan [2]string // lobby ID, session ID
	joinErr error

	mu    sync.Mutex
	joins int
}

func newLobbyClient() *lobbyClient {
	return &lobbyClient{fakeEventsClient: fakeEventsClient{events: make(chan *sse.Event)}, joined: make(chan [2]string, 10)}
}

func (c *lobbyClient) ListLobbies(context.Context) ([]wolfapi.Lobby, error) {
	return []wolfapi.Lobby{{ID: "lobby-1"}}, nil
}

func (c *lobbyClient) JoinLobby(_ context.Context, lobbyID, sessionID string) error {
	c.mu.Lock()
	c.joins++
	c.mu.Unlock()
	c.joined <- [2]string{lobbyID, sessionID}
	return c.joinErr
}

func (c *lobbyClient) StopSession(context.Context, string) error { return nil }

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
