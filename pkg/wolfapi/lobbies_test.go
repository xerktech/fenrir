package wolfapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// lastBody is the last request body a wolfServer saw, re-encoded from
// generic JSON (sorted keys), so tests check Wolf's field names, not our
// struct tags.
type lastBody struct{ json string }

// wolfServer answers every request with reply.
func wolfServer(t *testing.T, reply string) (client wolfapi.Client, body *lastBody) {
	t.Helper()
	body = &lastBody{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) == 0 {
			body.json = ""
			_, _ = io.WriteString(w, reply)
			return
		}
		var generic map[string]any
		if err := json.Unmarshal(b, &generic); err != nil {
			t.Errorf("request body %q: %v", b, err)
		}
		sorted, err := json.Marshal(generic)
		if err != nil {
			t.Error(err)
		}
		body.json = string(sorted)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return wolfapi.NewClient(srv.URL, srv.Client()), body
}

// Field names and types of Wolf's CreateLobbyRequest (api/api.hpp) and
// events::VideoSettings / AudioSettings / AppCMD (stable facb8e0).
func TestCreateLobbyWireFormat(t *testing.T) {
	client, body := wolfServer(t, `{"success":true,"lobby_id":"abc"}`)
	id, err := client.CreateLobby(t.Context(), &wolfapi.CreateLobbyRequest{
		ProfileID: wolfapi.MoonlightProfileID, Name: "alex-1",
		VideoSettings: wolfapi.LobbyVideoSettings{Width: 1920, Height: 1080, RefreshRate: 60,
			WaylandNode: "/dev/dri/renderD128", RunnerNode: "/dev/dri/renderD128", BufferCaps: "video/x-raw"},
		AudioSettings:     wolfapi.LobbyAudioSettings{ChannelCount: 2},
		RunnerStateFolder: "lobby",
		Runner:            wolfapi.Runner{Type: "process", RunCmd: "sleep"},
	})
	if err != nil || id != "abc" {
		t.Fatalf("CreateLobby = %q, %v", id, err)
	}
	want := `{"audio_settings":{"channel_count":2},"multi_user":false,"name":"alex-1","profile_id":"moonlight-profile-id",` +
		`"runner":{"run_cmd":"sleep","type":"process"},"runner_state_folder":"lobby","stop_when_everyone_leaves":false,` +
		`"video_settings":{"height":1080,"refresh_rate":60,"runner_render_node":"/dev/dri/renderD128",` +
		`"video_producer_buffer_caps":"video/x-raw","wayland_render_node":"/dev/dri/renderD128","width":1920}}`
	if body.json != want {
		t.Errorf("body\n%s\nwant\n%s", body.json, want)
	}
}

// events::JoinLobbyEvent; its size_t session ID is reflected as a string.
func TestJoinLobbyWireFormat(t *testing.T) {
	client, body := wolfServer(t, `{"success":true}`)
	if err := client.JoinLobby(t.Context(), "abc", "6142509188972423790"); err != nil {
		t.Fatal(err)
	}
	want := `{"lobby_id":"abc","moonlight_session_id":"6142509188972423790"}`
	if body.json != want {
		t.Errorf("body %s, want %s", body.json, want)
	}
}

// Wolf reports failures as HTTP 500 with a JSON body.
func TestLobbyCallsReportWolfErrors(t *testing.T) {
	client, _ := wolfServer(t, `{"success":false,"error":"Lobby is full"}`)
	if err := client.JoinLobby(t.Context(), "abc", "1"); err == nil {
		t.Error("JoinLobby succeeded on a Wolf error")
	}
}

func TestListLobbies(t *testing.T) {
	client, _ := wolfServer(t, `{"success":true,"lobbies":[{"id":"abc","name":"alex-1","multi_user":false,`+
		`"pin_required":false,"connected_sessions":["1"],"runner":{"type":"process","run_cmd":"x"}}]}`)
	lobbies, err := client.ListLobbies(t.Context())
	if err != nil || len(lobbies) != 1 || lobbies[0].ID != "abc" || lobbies[0].ConnectedSessions[0] != "1" {
		t.Errorf("ListLobbies = %+v, %v", lobbies, err)
	}
}
