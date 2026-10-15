package wolfapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// import (
// 	"context"
// 	"crypto/tls"
// 	"encoding/json"
// 	"net/http"
// 	"testing"
// 	"time"

// 	"games-on-whales.github.io/direwolf/pkg/wolfapi"
// )

// var testURL = "https://10.128.3.0:8443"

// func TestSubscribe(t *testing.T) {
// 	client := wolfapi.NewClient(testURL, &http.Client{
// 		Transport: &http.Transport{
// 			TLSClientConfig: &tls.Config{
// 				InsecureSkipVerify: true,
// 			},
// 		},
// 	})
// 	if client == nil {
// 		t.Fatal("client is nil")
// 	}

// 	t.Log("Client created")
// 	ch, err := client.SubscribeToEvents(context.Background())
// 	if err != nil {
// 		t.Fatalf("SubscribeToEvents failed: %v", err)
// 	}

// 	t.Log("Subscribed to events")
// 	for {
// 		select {
// 		case ev := <-ch:
// 			t.Log("Received event")
// 			t.Logf("Event ID: %s", ev.ID)
// 			t.Logf("Event Type: %s", ev.Event)
// 			t.Logf("Event Data: %s", ev.Data)
// 			t.Logf("Event Retry: %d", ev.Retry)
// 			t.Logf("Event Comment: %v", ev.Comment)

// 			switch wolfapi.WolfEventType(ev.Event) {
// 			case wolfapi.PauseStreamEventType:
// 				t.Log("Received PauseStreamEvent")
// 				var pauseEvent wolfapi.PauseStreamEvent
// 				if err := json.Unmarshal([]byte(ev.Data), &pauseEvent); err != nil {
// 					t.Fatalf("Failed to unmarshal PauseStreamEvent: %v", err)
// 					continue
// 				}

// 			}

// 		case <-time.After(5 * time.Minute):
// 			t.Fatal("Timeout waiting for event")
// 		}
// 	}
// }

func statusServer(t *testing.T, status int, reply string) wolfapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return wolfapi.NewClient(srv.URL, srv.Client())
}

// A non-2xx reply reports its status and Wolf's message, or the start of a
// non-JSON body, never a JSON decode error that hides the status.
func TestCallReportsHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		status      int
		want        []string
	}{
		// Longer than the body excerpt, so only Wolf's decoded message has its end.
		{"wolf error", `{"success":false,"error":"No session found` + strings.Repeat(".", 300) + `END"}`, http.StatusInternalServerError, []string{"500 Internal Server Error", "wolf: No session found", "END"}},
		{"plain text", "oops, bad gateway", http.StatusBadGateway, []string{"502 Bad Gateway", "oops, bad gateway"}},
		{"success body", `{"success":true,"sessions":[]}`, http.StatusUnauthorized, []string{"401 Unauthorized"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := statusServer(t, tc.status, tc.reply).ListSessions(t.Context())
			if err == nil {
				t.Fatal("ListSessions succeeded on a non-2xx reply")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

// The session list as Wolf (xerktech/wolf 581dba2) writes it: the session's
// fields flat, next to its state. wolf-agent's catch-up depends on both.
func TestListSessionsDecodesState(t *testing.T) {
	reply := `{"success":true,"sessions":[` +
		`{"client_ip":"10.0.0.2","aes_key":"k1","aes_iv":"iv","rtsp_fake_ip":"1.2.3.4","video_width":1920,"video_height":1080,"video_refresh_rate":60,"audio_channel_count":2,"app_id":"1","client_id":"42","state":"paused"},` +
		`{"client_ip":"10.0.0.2","aes_key":"k2","aes_iv":"iv","rtsp_fake_ip":"1.2.3.5","video_width":1920,"video_height":1080,"video_refresh_rate":60,"audio_channel_count":2,"app_id":"1","client_id":"43","state":"streaming"},` +
		`{"client_ip":"10.0.0.2","aes_key":"k3","aes_iv":"iv","rtsp_fake_ip":"1.2.3.6","video_width":1920,"video_height":1080,"video_refresh_rate":60,"audio_channel_count":2,"app_id":"1","client_id":"44","state":"setup"}]}`
	sessions, err := statusServer(t, http.StatusOK, reply).ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []wolfapi.Session{
		{ClientID: "42", AESKey: "k1", State: wolfapi.SessionPaused},
		{ClientID: "43", AESKey: "k2", State: wolfapi.SessionStreaming},
		{ClientID: "44", AESKey: "k3", State: wolfapi.SessionSetup},
	}
	if len(sessions) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(sessions), len(want))
	}
	for i, w := range want {
		if g := sessions[i]; g.ClientID != w.ClientID || g.AESKey != w.AESKey || g.State != w.State {
			t.Errorf("session %d: client_id %q aes_key %q state %q, want %q %q %q", i, g.ClientID, g.AESKey, g.State, w.ClientID, w.AESKey, w.State)
		}
	}
}

func TestCallTruncatesBody(t *testing.T) {
	_, err := statusServer(t, http.StatusInternalServerError, strings.Repeat("x", 10000)).ListApps(t.Context())
	if err == nil || len(err.Error()) > 500 {
		t.Errorf("want a short error, got %d chars", len(fmt.Sprint(err)))
	}
}

// Every method honours its ctx: a hung wolf-agent cannot hold a caller past
// its deadline.
func TestCallHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client := wolfapi.NewClient(srv.URL, srv.Client())

	// Each call stores its result in err.
	var err error
	calls := map[string]func(ctx context.Context){
		"ListSessions": func(ctx context.Context) { _, err = client.ListSessions(ctx) },
		"ListApps":     func(ctx context.Context) { _, err = client.ListApps(ctx) },
		"AddSession":   func(ctx context.Context) { _, err = client.AddSession(ctx, wolfapi.Session{}) },
		"AddApp":       func(ctx context.Context) { err = client.AddApp(ctx, wolfapi.App{}) },
		"StopSession":  func(ctx context.Context) { err = client.StopSession(ctx, "1") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			call(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want DeadlineExceeded", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("returned after %v", d)
			}
		})
	}
}
