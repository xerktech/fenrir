package wolfapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/r3labs/sse/v2"
	"gopkg.in/cenkalti/backoff.v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

type Session struct {
	AppID             string         `json:"app_id,omitempty"`
	AudioChannelCount int            `json:"audio_channel_count"`
	ClientID          string         `json:"client_id,omitempty"` // a paired client's ID; omitted, Wolf uses a certless dummy ("" throws 'Unhandled exception: stoull')
	ClientIP          string         `json:"client_ip"`
	ClientSettings    ClientSettings `json:"client_settings,omitempty"`
	VideoHeight       int            `json:"video_height"`
	VideoRefreshRate  int            `json:"video_refresh_rate"`
	VideoWidth        int            `json:"video_width"`

	AESKey string `json:"aes_key"`
	AESIV  string `json:"aes_iv"`

	RTSPFakeIP string `json:"rtsp_fake_ip,omitempty"`
	// Sunshine's encrypted RTSP (xerktech/wolf, XERK-2118). Wolf lists it
	// for its sessions; an older Wolf ignores it and omits it.
	RTSPEncrypted bool `json:"rtsp_encrypted,omitempty"`

	// State is what the stream is doing, as Wolf lists it (one of the
	// SessionState values). Read only: Wolf ignores it on add.
	State SessionState `json:"state,omitempty"`

	// overrides
	H264GSTPipeline string `json:"h264_gst_pipeline,omitempty"`
	HEVCGSTPipeline string `json:"hevc_gst_pipeline,omitempty"`
	AV1GSTPipeline  string `json:"av1_gst_pipeline,omitempty"`
	OpusGSTPipeline string `json:"opus_gst_pipeline,omitempty"`
}

// SessionState is a listed stream's state (xerktech/wolf, XERK-1947). A
// stopped stream is no longer listed.
type SessionState string

const (
	// SessionSetup: waiting for the client to set up and ping both its video
	// and audio streams.
	SessionSetup SessionState = "setup"
	// SessionStreaming: both the video and audio pipelines are running.
	SessionStreaming SessionState = "streaming"
	// SessionPaused: the client paused or dropped the stream.
	SessionPaused SessionState = "paused"
)

type Runner struct {
	Type   string `json:"type"`
	RunCmd string `json:"run_cmd,omitempty"`
}

type ClientSettings struct {
	ControllersOverride []string `json:"controllers_override"`
	//!TODO Float is lossy type. Possible to use decimal?
	HScrollAcceleration float64 `json:"h_scroll_acceleration"`
	MouseAcceleration   float64 `json:"mouse_acceleration"`
	RunGID              int     `json:"run_gid"`
	RunUID              int     `json:"run_uid"`
	VScrollAcceleration float64 `json:"v_scroll_acceleration"`
	// Required by Wolf's AddSession since games-on-whales/wolf 4ebbd98f
	// (2026-06): a missing field fails the whole request with "Field named
	// 'motion_controller_override' not found", so no session ever starts.
	MotionControllerOverride string `json:"motion_controller_override"`
}
type Client interface {
	AddSession(ctx context.Context, session Session) (AddSessionResponse, error)
	AddApp(ctx context.Context, app App) error
	StopSession(ctx context.Context, sessionID string) error
	ListSessions(ctx context.Context) ([]Session, error)
	ListApps(ctx context.Context) ([]App, error)
	ListClients(ctx context.Context) ([]PairedClient, error)
	ListLobbies(ctx context.Context) ([]Lobby, error)
	CreateLobby(ctx context.Context, lobby *CreateLobbyRequest) (string, error)
	JoinLobby(ctx context.Context, lobbyID, sessionID string) error
	// SubscribeToEvents's channel is closed when the stream ends; the caller
	// resubscribes.
	SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error)
}

type client struct {
	apiURL     string
	httpClient *http.Client
}

func NewClient(
	apiURL string,
	httpClient *http.Client,
) Client {
	return &client{
		apiURL:     apiURL,
		httpClient: httpClient,
	}
}

// POST /api/v1/sessions/add
func (c *client) AddSession(
	ctx context.Context,
	session Session,
) (AddSessionResponse, error) {
	var resp AddSessionResponse
	if err := c.call(ctx, http.MethodPost, "/api/v1/sessions/add", session, &resp); err != nil {
		return AddSessionResponse{}, fmt.Errorf("failed to add session: %w", err)
	}
	return resp, nil
}

// GET /api/v1/sessions
func (c *client) ListSessions(ctx context.Context) ([]Session, error) {
	var resp SessionsResponse
	if err := c.call(ctx, http.MethodGet, "/api/v1/sessions", nil, &resp); err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	return resp.Sessions, nil
}

// GET /api/v1/apps
func (c *client) ListApps(ctx context.Context) ([]App, error) {
	var resp AppsResponse
	if err := c.call(ctx, http.MethodGet, "/api/v1/apps", nil, &resp); err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	return resp.Apps, nil
}

// PairedClient is a client in Wolf's config. Only the fields direwolf reads
// are decoded.
type PairedClient struct {
	ClientID       string `json:"client_id"`
	AppStateFolder string `json:"app_state_folder"`
}

// GET /api/v1/clients
func (c *client) ListClients(ctx context.Context) ([]PairedClient, error) {
	var resp struct {
		Response `json:",inline"`
		Clients  []PairedClient `json:"clients"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/clients", nil, &resp); err != nil {
		return nil, fmt.Errorf("failed to list clients: %w", err)
	}
	return resp.Clients, nil
}

// This is no longer used, I will probably remove it in the future
// POST /api/v1/apps/add
func (c *client) AddApp(ctx context.Context, app App) error {
	if err := c.call(ctx, http.MethodPost, "/api/v1/apps/add", app, &Response{}); err != nil {
		return fmt.Errorf("failed to add app: %w", err)
	}
	return nil
}

// POST /api/v1/sessions/stop
func (c *client) StopSession(ctx context.Context, sessionID string) error {
	req := struct {
		SessionID string `json:"session_id"`
	}{sessionID}
	if err := c.call(ctx, http.MethodPost, "/api/v1/sessions/stop", req, &Response{}); err != nil {
		return fmt.Errorf("failed to stop session: %w", err)
	}
	return nil
}

// maxEventSize bounds one Wolf SSE event. The library's 64KiB default ends
// the stream on a bigger one and that event is lost; a PlugDeviceEvent
// carrying many udev hwdb entries reaches ~200KiB, and losing it means the
// controller never appears. The buffer only grows to what an event needs.
// Not higher: the reader rescans its buffer on every read, so a 16MiB event
// takes seconds of CPU and stalls the agent's event loop; 4MiB is ~0.3s.
const maxEventSize = 4 << 20

// SubscribeToEvents opens one connection to Wolf's event stream. It returns
// once Wolf has answered; the channel is closed when that connection ends,
// for whatever reason, so the caller sees every disconnect and resubscribes.
// Why an answered stream ended (a read error, an event over maxEventSize) is
// logged here: a closed channel can't carry it, and it is the only trace of
// the events lost with the stream (XERK-1449).
// The SSE library's own retry is off: it never runs when the stream ends
// with EOF, and it would hide a failing first connection for 15 minutes.
// With it off, the library returns the read error as is.
func (c *client) SubscribeToEvents(ctx context.Context) (<-chan *sse.Event, error) {
	events := make(chan *sse.Event)
	// Sent once: nil when Wolf answers OK, else why it didn't.
	answered := make(chan error, 1)
	// Set by the validator, which runs on the subscribing goroutine below.
	streaming := false
	sseClient := sse.NewClient(c.apiURL+"/api/v1/events", func(cl *sse.Client) {
		cl.Connection = c.httpClient
		cl.ReconnectStrategy = &backoff.StopBackOff{}
		sse.ClientMaxBufferSize(maxEventSize)(cl)
		cl.ResponseValidator = func(_ *sse.Client, resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return fmt.Errorf("could not connect to event stream: %s", resp.Status)
			}
			streaming = true
			answered <- nil
			return nil
		}
	})

	go func() {
		defer close(events)
		err := sseClient.SubscribeRawWithContext(ctx, func(ev *sse.Event) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		})
		switch {
		case !streaming:
			if err == nil {
				err = errors.New("event stream ended before a response")
			}
			answered <- err
		case err != nil && ctx.Err() == nil:
			utilruntime.HandleErrorWithContext(ctx, err, "Wolf event stream ended")
		}
	}()

	if err := <-answered; err != nil {
		return nil, err
	}
	return events, nil
}

type Response struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type SessionsResponse struct {
	Response `json:",inline"`
	Sessions []Session `json:"sessions"`
}

type AddSessionResponse struct {
	Response  `json:",inline"`
	SessionID string `json:"session_id"`
	// Whether Wolf encrypts the stream's RTSP: the client must get an
	// rtspenc:// URL then, and only then. False from a Wolf without it.
	RTSPEncrypted bool `json:"rtsp_encrypted"`
}

type App struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	SupportHDR             bool   `json:"support_hdr"`
	IconPNGPath            string `json:"icon_png_path"`
	StartVirtualCompositor bool   `json:"start_virtual_compositor"`
	StartAudioServer       bool   `json:"start_audio_server"`
	RenderNode             string `json:"render_node"`
	Runner                 Runner `json:"runner"`

	H264GSTPipeline string `json:"h264_gst_pipeline"`
	HEVCGSTPipeline string `json:"hevc_gst_pipeline"`
	AV1GSTPipeline  string `json:"av1_gst_pipeline"`
	OpusGSTPipeline string `json:"opus_gst_pipeline"`
}

type AppsResponse struct {
	Response `json:",inline"`
	Apps     []App `json:"apps"`
}

type WolfEventType string

const (
	PauseStreamEventType  WolfEventType = "wolf::core::events::PauseStreamEvent"
	StopStreamEventType   WolfEventType = "wolf::core::events::StopStreamEvent"
	PlugDeviceEventType   WolfEventType = "wolf::core::events::PlugDeviceEvent"
	UnplugDeviceEventType WolfEventType = "wolf::core::events::UnplugDeviceEvent"
	// EndSupersededStreamEventType fires (xerktech/wolf, XERK-1948) in place
	// of a pause when the ENet peer of a stream a newer one replaced under the
	// same session ID disconnects: Wolf ends only that old stream's pipelines.
	EndSupersededStreamEventType WolfEventType = "wolf::core::events::EndSupersededStreamEvent"
	// VideoSessionEventType and AudioSessionEventType fire when the client's
	// RTSP setup asks for the video/audio stream; the pipeline itself starts
	// on the stream's first RTP ping.
	VideoSessionEventType WolfEventType = "wolf::core::events::VideoSession"
	AudioSessionEventType WolfEventType = "wolf::core::events::AudioSession"
	// RTPVideoPingEventType and RTPAudioPingEventType are the client's RTP
	// pings, sent every 500ms for the whole stream. They carry the client
	// address, not the session ID.
	RTPVideoPingEventType WolfEventType = "wolf::core::events::RTPVideoPingEvent"
	RTPAudioPingEventType WolfEventType = "wolf::core::events::RTPAudioPingEvent"
)

// PauseStreamEvent also decodes StopStreamEvent.
//
// A stream's session ID is a hash of its paired client's cert. Every attach
// of a pod uses the same client (wolfStreamClientID), so IDs repeat across a
// pod's streams: tell streams apart by events, not ID.
type PauseStreamEvent struct {
	SessionID string `json:"session_id"`
}

// StreamSetupEvent is the session ID of a VideoSession or AudioSession event;
// the rest (AudioSession carries the AES key) is left undecoded.
type StreamSetupEvent struct {
	SessionID string `json:"session_id"`
	// The stream's AES key, which Wolf's session list also carries; only
	// the AudioSession event has it. Unlike the session ID, it is never
	// reused by a later stream.
	AESKey string `json:"aes_key,omitempty"`
}

// PlugDeviceEvent is fired by wolf when it hotplugs a virtual input device
// (joypad, mouse, keyboard, ...) for a session. It carries the raw udev
// event properties and hardware database entries that would normally be
// delivered by udevd; in the Docker runner wolf injects them into the app
// container via the fake-udev binary. In Kubernetes the wolf-agent performs
// the equivalent work (see pkg/fakeudev).
type PlugDeviceEvent struct {
	SessionID string `json:"session_id"`
	// One map of KEY=VALUE udev properties per emitted event.
	UdevEvents []map[string]string `json:"udev_events"`
	// Pairs of [filename, lines] to write under /run/udev/data.
	UdevHwDbEntries []UdevHwDbEntry `json:"udev_hw_db_entries"`
}

type UnplugDeviceEvent PlugDeviceEvent

// UdevHwDbEntry decodes wolf's [name, [lines...]] JSON tuples.
type UdevHwDbEntry struct {
	Filename string
	Content  []string
}

func (e *UdevHwDbEntry) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("expected [filename, lines] tuple, got %d elements", len(raw))
	}
	if err := json.Unmarshal(raw[0], &e.Filename); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &e.Content)
}
