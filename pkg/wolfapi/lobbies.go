package wolfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Lobby is a Wolf lobby: a compositor, audio sink and runner that outlive the
// Moonlight streams joined to it. Only the fields direwolf reads are decoded.
type Lobby struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	ConnectedSessions []string `json:"connected_sessions"`
}

// CreateLobbyRequest is the body of POST /api/v1/lobbies/create.
type CreateLobbyRequest struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
	MultiUser bool   `json:"multi_user"`
	// Must be false for a lobby that has to survive a disconnect: Wolf
	// stops the lobby, runner and display with it, once its last stream
	// leaves.
	StopWhenEveryoneLeaves bool               `json:"stop_when_everyone_leaves"`
	VideoSettings          LobbyVideoSettings `json:"video_settings"`
	AudioSettings          LobbyAudioSettings `json:"audio_settings"`
	// Relative to Wolf's HOST_APPS_STATE_FOLDER; Wolf creates it.
	RunnerStateFolder string `json:"runner_state_folder"`
	Runner            Runner `json:"runner"`
}

type LobbyVideoSettings struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	RefreshRate int    `json:"refresh_rate"`
	WaylandNode string `json:"wayland_render_node"`
	RunnerNode  string `json:"runner_render_node"`
	// GStreamer caps the lobby's compositor produces frames in.
	BufferCaps string `json:"video_producer_buffer_caps"`
}

type LobbyAudioSettings struct {
	ChannelCount int `json:"channel_count"`
}

// MoonlightProfileID is Wolf's built-in profile for Moonlight clients.
const MoonlightProfileID = "moonlight-profile-id"

// GET /api/v1/lobbies
func (c *client) ListLobbies(ctx context.Context) ([]Lobby, error) {
	var resp struct {
		Response `json:",inline"`
		Lobbies  []Lobby `json:"lobbies"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/lobbies", nil, &resp); err != nil {
		return nil, fmt.Errorf("failed to list lobbies: %w", err)
	}
	return resp.Lobbies, nil
}

// POST /api/v1/lobbies/create. Wolf answers once the lobby's Wayland socket
// is up (or after its own 20s setup timeout).
func (c *client) CreateLobby(ctx context.Context, lobby *CreateLobbyRequest) (string, error) {
	var resp struct {
		Response `json:",inline"`
		LobbyID  string `json:"lobby_id"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/v1/lobbies/create", lobby, &resp); err != nil {
		return "", fmt.Errorf("failed to create lobby: %w", err)
	}
	return resp.LobbyID, nil
}

// POST /api/v1/lobbies/join: routes the Moonlight session's input to the
// lobby's compositor and switches its running video/audio pipelines to the
// lobby's producers. A pipeline that has not started yet misses the switch.
func (c *client) JoinLobby(ctx context.Context, lobbyID, sessionID string) error {
	req := struct {
		LobbyID   string `json:"lobby_id"`
		SessionID string `json:"moonlight_session_id"`
	}{lobbyID, sessionID}
	if err := c.call(ctx, http.MethodPost, "/api/v1/lobbies/join", req, &Response{}); err != nil {
		return fmt.Errorf("failed to join lobby %s: %w", lobbyID, err)
	}
	return nil
}

// call sends body (if any) as JSON and decodes Wolf's reply into out, whose
// embedded Response reports failure. Wolf answers errors with HTTP 500 and a
// JSON body, so the body is decoded whatever the status.
func (c *client) call(ctx context.Context, method, path string, body any, out interface{ failure() error }) error {
	u, err := url.JoinPath(c.apiURL, path)
	if err != nil {
		return fmt.Errorf("building URL: %w", err)
	}
	var reqBody bytes.Buffer
	if body != nil {
		if encErr := json.NewEncoder(&reqBody).Encode(body); encErr != nil {
			return fmt.Errorf("encoding request: %w", encErr)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, &reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	return out.failure()
}

func (r *Response) failure() error {
	if !r.Success {
		return fmt.Errorf("wolf: %s", r.Error)
	}
	return nil
}
