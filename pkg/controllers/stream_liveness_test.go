package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// streamingFixture is an attached Session whose stream Wolf still lists, and
// the agent reporting videoPackets for it.
func streamingFixture(t *testing.T, videoPackets string) (*fakeAgent, *portsFixture, *v1alpha1types.Session) {
	t.Helper()
	agent := newFakeAgent(t)
	agent.sessions = `[{"aes_key":"k","aes_iv":"i","client_id":"old"}]`
	agent.videoPackets = videoPackets
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Spec.Config.AESKey, s.Spec.Config.AESIV = "k", "i"
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })
	return agent, f, sess
}

// age makes the session's last count change d older.
func (c *SessionController) age(sess *v1alpha1types.Session, d time.Duration) {
	c.videoProgress.mu.Lock()
	defer c.videoProgress.mu.Unlock()
	key := sess.Namespace + "/" + sess.Name
	p := c.videoProgress.m[key]
	p.changedAt = p.changedAt.Add(-d)
	c.videoProgress.m[key] = p
}

// A stream Wolf still lists but that sent no video for streamStallTimeout
// froze (XERK-1741): its Session ends.
func TestFrozenStreamEndsSession(t *testing.T) {
	_, f, sess := streamingFixture(t, `{"counted":true,"packets":500}`)
	ctx := context.Background()

	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	f.sc.age(sess, streamStallTimeout-time.Second)
	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); err != nil {
		t.Fatalf("ended before streamStallTimeout: %v", err)
	}
	f.sc.age(sess, time.Second)
	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); !errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcileActiveStreams = %v, want errSessionEnded", err)
	}
	if f.sessionExists(t, sess.Name) {
		t.Error("session kept with a frozen stream")
	}
}

// A count that keeps rising is a live stream, however long it runs.
func TestFlowingStreamKeepsSession(t *testing.T) {
	agent, f, sess := streamingFixture(t, `{"counted":true,"packets":500}`)
	ctx := context.Background()
	for i := range 3 {
		agent.mu.Lock()
		agent.videoPackets = fmt.Sprintf(`{"counted":true,"packets":%d}`, 500+i)
		agent.mu.Unlock()
		if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); err != nil {
			t.Fatal(err)
		}
		f.sc.age(sess, time.Hour)
	}
	if !f.sessionExists(t, sess.Name) || sess.Status.StreamURL == "" {
		t.Errorf("flowing stream lost its session or stream URL (%q)", sess.Status.StreamURL)
	}
}

// Nothing that leaves the count unknown, or a stream not started yet (no
// packets), ends a Session.
func TestUnknownVideoCountKeepsSession(t *testing.T) {
	for name, body := range map[string]string{
		"pod without the endpoint": "",
		"node without accounting":  `{"counted":false,"packets":0}`,
		"stream not started":       `{"counted":true,"packets":0}`,
		"garbled":                  `{`,
	} {
		t.Run(name, func(t *testing.T) {
			_, f, sess := streamingFixture(t, body)
			for range 2 {
				if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
					t.Fatal(err)
				}
				if f.sc.videoProgress.m != nil {
					f.sc.age(sess, time.Hour)
				}
			}
			if !f.sessionExists(t, sess.Name) {
				t.Error("session ended without a known stall")
			}
		})
	}
}

// A new attach (a /resume) starts its own count: the old stream's stale
// timestamp must not end the new one.
func TestReattachRestartsStallClock(t *testing.T) {
	var c SessionController
	sess := &v1alpha1types.Session{}
	sess.Namespace, sess.Name, sess.UID = "ns", "alex-1", "u1"
	sess.Status.WolfSessionID, sess.Status.AttachedGeneration = "10", 1
	now := time.Now()
	c.videoProgress.stalledFor(sess, 500, now)
	if d := c.videoProgress.stalledFor(sess, 500, now.Add(time.Minute)); d != time.Minute {
		t.Fatalf("stalledFor = %s, want 1m", d)
	}
	sess.Status.WolfSessionID, sess.Status.AttachedGeneration = "11", 2
	if d := c.videoProgress.stalledFor(sess, 500, now.Add(2*time.Minute)); d != 0 {
		t.Errorf("stalledFor after a re-attach = %s, want 0", d)
	}
	sess.UID = "u2" // a new Session of the same name
	if d := c.videoProgress.stalledFor(sess, 500, now.Add(3*time.Minute)); d != 0 {
		t.Errorf("stalledFor for a recreated Session = %s, want 0", d)
	}
	c.videoProgress.forget("ns", "alex-1")
	if len(c.videoProgress.m) != 0 {
		t.Errorf("forget left %v", c.videoProgress.m)
	}
}

// wolf-agent's endpoint: counted only with a video port and accounting on.
func TestReadVideoPackets(t *testing.T) {
	dir := t.TempDir()
	acct, table := filepath.Join(dir, "acct"), filepath.Join(dir, "table")
	line := "ipv4 2 udp 17 119 src=c dst=n sport=5 dport=20004 packets=1 bytes=1 src=n dst=c sport=20004 dport=5 packets=42 bytes=1 [ASSURED]\n"
	if err := os.WriteFile(table, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		acct string
		port int
		want VideoPackets
	}{
		{"1\n", 20004, VideoPackets{Counted: true, Packets: 42}},
		{"0\n", 20004, VideoPackets{}},
		{"1\n", 0, VideoPackets{}},
	} {
		if err := os.WriteFile(acct, []byte(tc.acct), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadVideoPackets(tc.port, acct, table)
		if err != nil || got != tc.want {
			t.Errorf("ReadVideoPackets(%d) with acct %q = %+v, %v; want %+v", tc.port, tc.acct, got, err, tc.want)
		}
	}

	rec := httptest.NewRecorder()
	VideoPacketsHandler(func() (VideoPackets, error) { return VideoPackets{}, errors.New("unreadable") }).
		ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, VideoPacketsPath, http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("handler on a read error = %d, want 500", rec.Code)
	}
}

// The pod tells wolf-agent which port to count.
func TestSessionPodPassesVideoPort(t *testing.T) {
	_, f, sess := streamingFixture(t, "")
	pod, err := f.sc.buildPod(sess)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("--video-port=%d", sess.Status.Ports.VideoRTP)
	for _, c := range pod.Spec.Containers {
		if c.Name == "wolf-agent" {
			if slices.Contains(c.Args, want) {
				return
			}
			t.Fatalf("wolf-agent args %v lack %s", c.Args, want)
		}
	}
	t.Fatal("no wolf-agent container")
}
