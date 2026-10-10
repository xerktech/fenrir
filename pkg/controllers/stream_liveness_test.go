package controllers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	p.risenAt = p.risenAt.Add(-d)
	c.videoProgress.m[key] = p
}

// A stream Wolf still lists but that sent no video for streamStallTimeout
// froze (XERK-1741): its Session ends.
func TestFrozenStreamEndsSession(t *testing.T) {
	agent, f, sess := streamingFixture(t, `{"counted":true,"flows":{"dst=c dport=1":500}}`)
	ctx := context.Background()

	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	agent.setVideoPackets(`{"counted":true,"flows":{"dst=c dport=1":900}}`)
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
	agent, f, sess := streamingFixture(t, `{"counted":true,"flows":{}}`)
	ctx := context.Background()
	for i := range 4 {
		// The new flow rises while a previous attach's expires.
		old := ""
		if i < 2 {
			old = `"dst=c dport=1":18638,`
		}
		agent.setVideoPackets(fmt.Sprintf(`{"counted":true,"flows":{%s"dst=c dport=2":%d}}`, old, 500+i))
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
		"stream not started":       `{"counted":true}`,
		// A /resume whose client has not started video yet: the previous
		// attach's flow is still in the table at its final count.
		"resumed, video not started": `{"counted":true,"flows":{"dst=c dport=1":18638}}`,
		"garbled":                    `{`,
	} {
		t.Run(name, func(t *testing.T) {
			agent, f, sess := streamingFixture(t, body)
			for range 2 {
				if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
					t.Fatal(err)
				}
				if f.sc.videoProgress.m != nil {
					f.sc.age(sess, time.Hour)
				}
				// A flow expiring is not a start either.
				agent.setVideoPackets(strings.Replace(body, `"dst=c dport=1":18638`, "", 1))
			}
			if !f.sessionExists(t, sess.Name) {
				t.Error("session ended without a known stall")
			}
		})
	}
}

// A new attach (a /resume), even one that only bumps attachedGeneration,
// starts its own clock and baseline: the old stream's stall must not end the
// new one, and its leftover flow is not the new stream's start.
func TestReattachRestartsStallClock(t *testing.T) {
	flows := func(n uint64) map[string]uint64 { return map[string]uint64{"dst=c dport=1": n} }
	for _, tc := range []struct {
		name   string
		attach func(*v1alpha1types.Session)
	}{
		{"new generation", func(s *v1alpha1types.Session) { s.Status.AttachedGeneration++ }},
		{"new Wolf stream", func(s *v1alpha1types.Session) { s.Status.WolfSessionID += "1" }},
		{"new Session", func(s *v1alpha1types.Session) { s.UID += "1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c SessionController
			sess := &v1alpha1types.Session{}
			sess.Namespace, sess.Name, sess.UID = "ns", "alex-1", "u1"
			sess.Status.WolfSessionID, sess.Status.AttachedGeneration = "10", 1
			now := time.Now()
			c.videoProgress.stalledFor(sess, flows(400), now)
			c.videoProgress.stalledFor(sess, flows(500), now)
			if d := c.videoProgress.stalledFor(sess, flows(500), now.Add(time.Minute)); d != time.Minute {
				t.Fatalf("stalledFor = %s, want 1m", d)
			}
			tc.attach(sess)
			for i := range 2 {
				if d := c.videoProgress.stalledFor(sess, flows(500), now.Add(time.Duration(i+2)*time.Minute)); d != 0 {
					t.Errorf("stalledFor = %s, want 0", d)
				}
			}
		})
	}
}

// A flow's count falling (it expired and came back) is not video.
func TestFallingCountIsNoProgress(t *testing.T) {
	var c SessionController
	sess := &v1alpha1types.Session{}
	sess.Namespace, sess.Name = "ns", "alex-1"
	now := time.Now()
	c.videoProgress.stalledFor(sess, map[string]uint64{"dst=c dport=1": 400}, now)
	c.videoProgress.stalledFor(sess, map[string]uint64{"dst=c dport=1": 500}, now)
	if d := c.videoProgress.stalledFor(sess, map[string]uint64{"dst=c dport=1": 3}, now.Add(time.Minute)); d != time.Minute {
		t.Errorf("stalledFor after a fall = %s, want 1m", d)
	}
}

// A stream whose only packets were sent before the operator's first poll
// still rose from the baseline taken before the attach, so it is caught when
// it freezes.
func TestAttachTakesVideoBaseline(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"aes_key":"k","aes_iv":"i","client_id":"old"}]`
	agent.videoPackets = `{"counted":true,"flows":{"dst=c dport=1":18638}}`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Generation = 2
		s.Spec.Config.AESKey, s.Spec.Config.AESIV = "new-key", "i"
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })
	ctx := context.Background()
	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	if agent.added != 1 {
		t.Fatalf("added %d streams, want the resume's", agent.added)
	}
	// Wolf lists the new stream; it sent 3 packets, then froze.
	agent.mu.Lock()
	agent.sessions = `[{"aes_key":"new-key","aes_iv":"i","client_id":"4242"}]`
	agent.mu.Unlock()
	agent.setVideoPackets(`{"counted":true,"flows":{"dst=c dport=1":18638,"dst=c dport=2":3}}`)
	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	f.sc.age(sess, streamStallTimeout)
	if err := f.sc.reconcileActiveStreams(ctx, sess, readyPod(sess)); !errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcileActiveStreams = %v, want errSessionEnded", err)
	}
}

// A deleted Session's progress is dropped.
func TestDeletedSessionForgetsProgress(t *testing.T) {
	_, f, sess := streamingFixture(t, `{"counted":true,"flows":{"dst=c dport=1":5}}`)
	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	if len(f.sc.videoProgress.m) != 1 {
		t.Fatalf("progress %v, want the session's", f.sc.videoProgress.m)
	}
	if err := f.sc.Reconcile(sess.Namespace, sess.Name, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.sc.videoProgress.m) != 0 {
		t.Errorf("Reconcile of a deleted Session left %v", f.sc.videoProgress.m)
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
		{"1\n", 20004, VideoPackets{Counted: true, Flows: map[string]uint64{"dst=c dport=5": 42}}},
		{"0\n", 20004, VideoPackets{}},
		{"1\n", 0, VideoPackets{}},
	} {
		if err := os.WriteFile(acct, []byte(tc.acct), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadVideoPackets(tc.port, acct, table)
		if err != nil || got.Counted != tc.want.Counted || !maps.Equal(got.Flows, tc.want.Flows) {
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
