package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// lobbyJoinSettle is how long after a stream's first RTP pings the agent
// waits before joining it to the lobby: the pings start the stream's
// pipelines, and Wolf only switches pipelines that are already running.
const lobbyJoinSettle = time.Second

// maxLobbyJoinAttempts bounds the retries for one stream; each retry waits for
// the next ping (every 500ms) plus lobbyJoinSettle.
const maxLobbyJoinAttempts = 5

// lobbyCallTimeout bounds each Wolf call the joiner makes.
const lobbyCallTimeout = 10 * time.Second

type lobbyJoinState int

const (
	lobbyJoinWaiting   lobbyJoinState = iota
	lobbyJoinScheduled                // waiting out settle
	lobbyJoinInFlight                 // calling Wolf
	lobbyJoinDone
)

// lobbyJoiner brings each Moonlight stream into the pod's Wolf lobby, which
// the operator creates before the first stream and which owns the Wayland
// display the game runs on. A stream's own display dies with it; the
// lobby's survives disconnects, so /resume finds the game still running.
//
// Joining switches the stream's video/audio pipelines to the lobby's
// producers, but only pipelines that are already running: they start on the
// stream's first RTP ping, after its VideoSession/AudioSession events. Joined
// earlier, the client would keep seeing the stream's own empty display. So
// the join waits for both setup events, a ping of each kind after them, then
// settle; the client sees the empty display for that long.
//
// Wolf session IDs repeat across a pod's streams (see
// wolfapi.PauseStreamEvent), so a stream is told from the next by its
// events: a stream ends with a pause or stop, and a second setup event of a
// kind already seen starts a new one. The pod streams to one client at a
// time, so pings (which carry no session ID) belong to the latest stream.
type lobbyJoiner struct {
	client wolfapi.Client
	settle time.Duration
	unplug func(wolfapi.UnplugDeviceEvent) // the agent's own unplug handling

	mu                   sync.Mutex
	stream               string // Wolf session ID being (or already) joined
	video, audio         bool   // its setup events were seen
	videoPing, audioPing bool   // pings seen after them
	state                lobbyJoinState
	attempts             int                         // joins tried for this stream
	ticket               int                         // the latest scheduled join; never reset
	held                 []wolfapi.UnplugDeviceEvent // unplugs seen while a join was in flight
}

// streamSetup records a VideoSession (video) or AudioSession event.
func (j *lobbyJoiner) streamSetup(sessionID string, video bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if sessionID != j.stream || (video && j.video) || (!video && j.audio) {
		j.reset(sessionID)
	}
	if video {
		j.video = true
	} else {
		j.audio = true
	}
}

// ping records an RTP ping and joins the stream once both of its pipelines
// have had one.
func (j *lobbyJoiner) ping(ctx context.Context, video bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case video && j.video:
		j.videoPing = true
	case !video && j.audio:
		j.audioPing = true
	}
	if j.state != lobbyJoinWaiting || !j.videoPing || !j.audioPing || j.attempts >= maxLobbyJoinAttempts {
		return
	}
	j.state = lobbyJoinScheduled
	j.attempts++
	j.ticket++
	ticket := j.ticket
	time.AfterFunc(j.settle, func() { j.join(ctx, ticket) })
}

// join runs the join scheduled as ticket, unless the stream ended or was
// replaced meanwhile.
func (j *lobbyJoiner) join(ctx context.Context, ticket int) {
	j.mu.Lock()
	if ctx.Err() != nil || j.state != lobbyJoinScheduled || j.ticket != ticket {
		j.mu.Unlock()
		return
	}
	j.state = lobbyJoinInFlight
	stream := j.stream
	j.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, lobbyCallTimeout)
	defer cancel()
	lobbyID, err := j.joinLobby(ctx, stream)

	j.mu.Lock()
	current := j.state == lobbyJoinInFlight && j.ticket == ticket
	var held []wolfapi.UnplugDeviceEvent
	switch {
	case !current:
		// The stream ended meanwhile; its devices went with it. Unplugs
		// held now belong to the next stream's join.
	case err != nil:
		held, j.held = j.held, nil
		klog.Errorf("Session %s: joining the lobby (attempt %d/%d): %v", stream, j.attempts, maxLobbyJoinAttempts, err)
		j.state = lobbyJoinWaiting
	default:
		klog.Infof("Session %s joined lobby %s", stream, lobbyID)
		j.state = lobbyJoinDone
		j.held = nil
	}
	j.mu.Unlock()
	// Not joined, so these were the stream's own unplugs.
	for _, ev := range held {
		j.unplug(ev)
	}
}

func (j *lobbyJoiner) joinLobby(ctx context.Context, stream string) (string, error) {
	lobbies, err := j.client.ListLobbies(ctx)
	if err != nil {
		return "", fmt.Errorf("listing lobbies: %w", err)
	}
	// The operator creates one lobby per pod.
	if len(lobbies) == 0 {
		return "", errors.New("wolf has no lobby")
	}
	if err := j.client.JoinLobby(ctx, lobbies[0].ID, stream); err != nil {
		return "", fmt.Errorf("joining: %w", err)
	}
	return lobbies[0].ID, nil
}

// joinedStream returns the stream joined to the lobby, if any.
func (j *lobbyJoiner) joinedStream() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != lobbyJoinDone {
		return ""
	}
	return j.stream
}

// ended forgets a paused or stopped stream.
func (j *lobbyJoiner) ended(sessionID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if sessionID == j.stream {
		j.reset("")
	}
}

// holdsUnplug reports whether the agent must not act on ev now. Joining moves
// the stream's joypads to the lobby with an unplug addressed to the stream
// (and a plug only the lobby's runner sees); once joined, Wolf re-fires a
// real unplug addressed to the lobby. Acting on the stream's own unplugs then
// would drop the game's controllers. While the join is in flight it can't
// be told which an unplug is: it is held, and handled if the join fails.
func (j *lobbyJoiner) holdsUnplug(ev wolfapi.UnplugDeviceEvent) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ev.SessionID != j.stream {
		return false
	}
	switch j.state {
	case lobbyJoinInFlight:
		j.held = append(j.held, ev)
		return true
	case lobbyJoinDone:
		return true
	case lobbyJoinWaiting, lobbyJoinScheduled:
		return false
	}
	return false
}

func (j *lobbyJoiner) reset(stream string) {
	j.ticket++ // cancels a scheduled or in-flight join
	j.stream = stream
	j.video, j.audio, j.videoPing, j.audioPing = false, false, false, false
	j.state = lobbyJoinWaiting
	j.attempts = 0
	j.held = nil
}
