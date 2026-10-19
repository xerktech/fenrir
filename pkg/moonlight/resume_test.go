package moonlight

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
)

// resumeRequest is an authenticated /resume from user alex.
func resumeRequest(query string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/resume?"+query, http.NoBody)
	r.RemoteAddr = "192.0.2.7:51000"
	user := &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "alex", Namespace: "ns"}}
	return r.WithContext(context.WithValue(r.Context(), userContextKey{}, user))
}

// /resume puts the client's new keys on the running Session, instead of
// replacing it (which would delete the pod the game is still running in),
// and answers once the operator has attached them.
func TestResumeReattachesRunningSession(t *testing.T) {
	dw := generatedclient.NewSimpleClientset(&v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alex-steam-abcde", Namespace: "ns", Generation: 1, UID: "uid",
			Labels: map[string]string{"direwolf/user": "alex"},
		},
		Spec: v1alpha1types.SessionSpec{Config: v1alpha1types.SessionInfo{AESKey: "old", AESIV: "1"}},
		Status: v1alpha1types.SessionStatus{
			AttachedGeneration: 1,
			DisconnectedAt:     &metav1.Time{Time: time.Now()},
			StreamURL:          "",
		},
	})
	// The API server bumps the generation on a spec change; the fake doesn't.
	bumpGeneration(dw)
	sessions := dw.DirewolfV1alpha1().Sessions("ns")

	// The operator: attach whatever generation carries the new keys.
	ctx := t.Context()
	go func() {
		for ctx.Err() == nil {
			s, err := sessions.Get(ctx, "alex-steam-abcde", metav1.GetOptions{})
			if err == nil && s.Spec.Config.AESKey == "newkey" && s.Status.AttachedGeneration < s.Generation {
				s.Status.AttachedGeneration = s.Generation
				s.Status.WolfSessionID = "7"
				s.Status.StreamURL = "rtsp://10.0.0.4:20002"
				s.Status.DisconnectedAt = nil
				_, _ = sessions.UpdateStatus(ctx, s, metav1.UpdateOptions{})
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	srv := &RESTServer{SessionClient: sessions, RESTServerOptions: RESTServerOptions{LaunchTimeout: 10 * time.Second}}
	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=newkey&rikeyid=42&corever=1"))

	if body := w.Body.String(); !strings.Contains(body, "<sessionUrl0>rtsp://10.0.0.4:20002</sessionUrl0>") {
		t.Fatalf("resume response = %d %s, want the attached stream URL", w.Code, body)
	}
	// Moonlight for Android requires <resume> in a resume reply.
	if body := w.Body.String(); !strings.Contains(body, "<resume>1</resume>") || strings.Contains(body, "<gamesession") {
		t.Errorf("resume response = %s, want <resume>1</resume> and no gamesession", body)
	}
	s, err := sessions.Get(ctx, "alex-steam-abcde", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.UID != "uid" {
		t.Error("session was replaced, not resumed")
	}
	if c := s.Spec.Config; c.AESKey != "newkey" || c.AESIV != "42" || c.ClientIP != "192.0.2.7" || !c.EncryptedRTSP {
		t.Errorf("session config = %+v, want the resume's keys, client IP and encrypted RTSP (corever=1)", c)
	}
}

// Without a session to resume, /resume is a launch (which needs an appid).
func TestResumeWithoutSessionLaunches(t *testing.T) {
	dw := generatedclient.NewSimpleClientset()
	srv := &RESTServer{SessionClient: dw.DirewolfV1alpha1().Sessions("ns"), RESTServerOptions: RESTServerOptions{LaunchTimeout: time.Second}}
	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=k&rikeyid=1"))
	if !strings.Contains(w.Body.String(), "appid required") {
		t.Errorf("response = %d %s, want the launch path's appid error", w.Code, w.Body.String())
	}
}

func TestResumeRequiresKeys(t *testing.T) {
	srv := &RESTServer{}
	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=k"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `status_code="400"`) {
		t.Errorf("response = %d %s, want HTTP 200 with status 400", w.Code, w.Body.String())
	}
}

// bumpGeneration makes the fake bump metadata.generation on spec updates, as
// the API server does.
func bumpGeneration(dw *generatedclient.Clientset) {
	dw.PrependReactor("update", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if update, ok := action.(k8stesting.UpdateAction); ok && update.GetSubresource() == "" {
			if s, isSession := update.GetObject().(*v1alpha1types.Session); isSession {
				s.Generation++
			}
		}
		return false, nil, nil
	})
}

func alexSession(name string, created time.Time) *v1alpha1types.Session {
	return &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", Generation: 1, CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{"direwolf/user": "alex"},
		},
		Spec: v1alpha1types.SessionSpec{Config: v1alpha1types.SessionInfo{AESKey: "old", AESIV: "1"}},
		Status: v1alpha1types.SessionStatus{
			AttachedGeneration: 1, WolfSessionID: "5", StreamURL: "rtsp://10.0.0.4:20002",
		},
	}
}

// Resuming a still-attached session: the stream URL is already set (and
// does not change), so the proxy must wait for the new keys to be attached,
// not answer before the client's keys reach Wolf. It picks the newest
// session that is not being deleted.
func TestResumeWhileAttachedWaitsForNewKeys(t *testing.T) {
	now := time.Now()
	deleting := alexSession("alex-c", now.Add(-time.Minute))
	deleting.DeletionTimestamp = &metav1.Time{Time: now}
	deleting.Finalizers = []string{"test"}
	dw := generatedclient.NewSimpleClientset(
		alexSession("alex-a", now.Add(-2*time.Hour)),
		alexSession("alex-b", now.Add(-time.Hour)),
		deleting,
	)
	bumpGeneration(dw)
	sessions := dw.DirewolfV1alpha1().Sessions("ns")

	ctx := t.Context()
	go func() {
		for ctx.Err() == nil {
			s, err := sessions.Get(ctx, "alex-b", metav1.GetOptions{})
			if err == nil && s.Spec.Config.AESKey == "newkey" && s.Status.AttachedGeneration < s.Generation {
				time.Sleep(300 * time.Millisecond) // Wolf's add takes a while
				s.Status.AttachedGeneration = s.Generation
				s.Status.WolfSessionID = "6"
				_, _ = sessions.UpdateStatus(ctx, s, metav1.UpdateOptions{})
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	srv := &RESTServer{SessionClient: sessions, RESTServerOptions: RESTServerOptions{LaunchTimeout: 10 * time.Second}}
	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=newkey&rikeyid=42"))
	if !strings.Contains(w.Body.String(), "<sessionUrl0>rtsp://10.0.0.4:20002</sessionUrl0>") {
		t.Fatalf("resume response = %d %s", w.Code, w.Body.String())
	}
	b, err := sessions.Get(ctx, "alex-b", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status.AttachedGeneration != b.Generation || b.Status.WolfSessionID != "6" {
		t.Errorf("answered before the new keys were attached: %+v", b.Status)
	}
	for _, other := range []string{"alex-a", "alex-c"} {
		if s, _ := sessions.Get(ctx, other, metav1.GetOptions{}); s.Spec.Config.AESKey != "old" {
			t.Errorf("resumed %s, want only the newest live session", other)
		}
	}
}

// A retried /resume carrying the same keys cannot bump the generation, so it
// fails fast instead of waiting out the launch timeout.
func TestResumeWithUnchangedKeysFailsFast(t *testing.T) {
	s := alexSession("alex-a", time.Now())
	s.Spec.Config = v1alpha1types.SessionInfo{AESKey: "k", AESIV: "1", ClientIP: "192.0.2.7"}
	s.Status = v1alpha1types.SessionStatus{AttachedGeneration: 1, DisconnectedAt: &metav1.Time{Time: time.Now()}}
	dw := generatedclient.NewSimpleClientset(s)
	srv := &RESTServer{SessionClient: dw.DirewolfV1alpha1().Sessions("ns"), RESTServerOptions: RESTServerOptions{LaunchTimeout: 10 * time.Second}}

	start := time.Now()
	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=k&rikeyid=1"))
	if !strings.Contains(w.Body.String(), "previous stream keys") || time.Since(start) > 2*time.Second {
		t.Errorf("response after %s = %d %s, want a fast keys error", time.Since(start), w.Code, w.Body.String())
	}
}

// A refusal of the resume's own keys (e.g. it came from an IPv6 address)
// answers at once; one from an earlier generation is not this resume's
// (XERK-1336).
func TestResumeStreamRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		refusedAt     int64
		wantImmediate bool
	}{
		{name: "this resume", refusedAt: 2, wantImmediate: true},
		{name: "earlier generation", refusedAt: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dw := generatedclient.NewSimpleClientset(&v1alpha1types.Session{
				ObjectMeta: metav1.ObjectMeta{
					Name: "alex-steam-abcde", Namespace: "ns", Generation: 1,
					Labels: map[string]string{"direwolf/user": "alex"},
				},
				Status: v1alpha1types.SessionStatus{
					AttachedGeneration: 1,
					DisconnectedAt:     &metav1.Time{Time: time.Now()},
					Conditions: []metav1.Condition{{
						Type: v1alpha1types.SessionStreamStartedCondition, Status: metav1.ConditionFalse,
						Reason: v1alpha1types.SessionStreamRefusedReason, Message: "not IPv4", ObservedGeneration: tc.refusedAt,
					}},
				},
			})
			bumpGeneration(dw)
			srv := &RESTServer{SessionClient: dw.DirewolfV1alpha1().Sessions("ns"), RESTServerOptions: RESTServerOptions{LaunchTimeout: 500 * time.Millisecond}}

			start := time.Now()
			w := httptest.NewRecorder()
			srv.resumeHandler(w, resumeRequest("rikey=newkey&rikeyid=42"))
			elapsed := time.Since(start)

			if tc.wantImmediate {
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "not IPv4") || elapsed > 400*time.Millisecond {
					t.Errorf("resume = %d %s after %s, want the refusal at once", w.Code, w.Body.String(), elapsed)
				}
				return
			}
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `status_code="500"`) || strings.Contains(w.Body.String(), "alex-steam-abcde") {
				t.Errorf("resume = %d %s, want status 500 at the timeout", w.Code, w.Body.String())
			}
		})
	}
}

// A Session deleted while /resume waits (cancelled from another device, or
// reaped) gets a message, not a transport error (XERK-1336).
func TestResumeSessionEndedWhileWaiting(t *testing.T) {
	dw := generatedclient.NewSimpleClientset(alexSession("alex-a", time.Now()))
	bumpGeneration(dw)
	sessions := dw.DirewolfV1alpha1().Sessions("ns")
	// Gone once the resume has written its keys. (A reactor must not call
	// the fake: reactors run under its lock.)
	var updated atomic.Bool
	dw.PrependReactor("update", "sessions", func(k8stesting.Action) (bool, runtime.Object, error) {
		updated.Store(true)
		return false, nil, nil
	})
	dw.PrependReactor("get", "sessions", func(k8stesting.Action) (bool, runtime.Object, error) {
		if updated.Load() {
			return true, nil, k8serrors.NewNotFound(v1alpha1types.Resource("sessions"), "alex-a")
		}
		return false, nil, nil
	})
	srv := &RESTServer{SessionClient: sessions, RESTServerOptions: RESTServerOptions{LaunchTimeout: 5 * time.Second}}

	start := time.Now()
	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=newkey&rikeyid=42"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `status_message="The host ended the session before its stream started"`) ||
		time.Since(start) > 2*time.Second {
		t.Errorf("resume = %d %s after %s, want the ended message at once", w.Code, w.Body.String(), time.Since(start))
	}
}

// A Session gone or ending between the resume's List and its key update
// gets the same message as one gone while waiting, not a 500 naming the
// Session (XERK-1867).
func TestResumeSessionEndedBeforeUpdate(t *testing.T) {
	notFound := k8serrors.NewNotFound(v1alpha1types.Resource("sessions"), "alex-a")
	for name, react := range map[string]struct {
		verb   string
		result func() (runtime.Object, error)
	}{
		"get not found":    {"get", func() (runtime.Object, error) { return nil, notFound }},
		"update not found": {"update", func() (runtime.Object, error) { return nil, notFound }},
		"terminating at get": {"get", func() (runtime.Object, error) {
			s := alexSession("alex-a", time.Now())
			s.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			return s, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dw := generatedclient.NewSimpleClientset(alexSession("alex-a", time.Now()))
			dw.PrependReactor(react.verb, "sessions", func(k8stesting.Action) (bool, runtime.Object, error) {
				obj, err := react.result()
				return true, obj, err
			})
			srv := &RESTServer{SessionClient: dw.DirewolfV1alpha1().Sessions("ns"), RESTServerOptions: RESTServerOptions{LaunchTimeout: 5 * time.Second}}

			w := httptest.NewRecorder()
			srv.resumeHandler(w, resumeRequest("rikey=newkey&rikeyid=42"))
			if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, `status_message="`+sessionEndedMessage+`"`) || strings.Contains(body, "alex-a") {
				t.Errorf("resume = %d %s, want the ended message", w.Code, body)
			}
		})
	}
}

// A 5xx tells the client only that the host failed: the API error behind it
// (Session names, RBAC detail) stays in the log. 4xx messages are kept.
func TestServerErrorHidesDetail(t *testing.T) {
	dw := generatedclient.NewSimpleClientset(alexSession("alex-a", time.Now()))
	dw.PrependReactor("get", "sessions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(v1alpha1types.Resource("sessions"), "alex-a", errors.New("rbac says no"))
	})
	srv := &RESTServer{SessionClient: dw.DirewolfV1alpha1().Sessions("ns"), RESTServerOptions: RESTServerOptions{LaunchTimeout: 5 * time.Second}}

	w := httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=newkey&rikeyid=42"))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `status_code="500"`) || !strings.Contains(body, `status_message="`+internalErrorMessage+`"`) {
		t.Errorf("resume = %d %s, want status 500 with %q", w.Code, body, internalErrorMessage)
	}
	for _, leak := range []string{"alex-a", "forbidden", "rbac"} {
		if strings.Contains(body, leak) {
			t.Errorf("resume reply leaks %q: %s", leak, body)
		}
	}

	w = httptest.NewRecorder()
	srv.resumeHandler(w, resumeRequest("rikey=k"))
	if !strings.Contains(w.Body.String(), "rikey and rikeyid required") {
		t.Errorf("400 reply = %s, want its message kept", w.Body.String())
	}
}

// A resume from an address Wolf can't stream to is refused before its IP is
// written into the Session, which would otherwise sit refused until the
// disconnect grace period reaps it (XERK-1867).
func TestResumeRefusesNonIPv4Client(t *testing.T) {
	dw := generatedclient.NewSimpleClientset(alexSession("alex-a", time.Now()))
	sessions := dw.DirewolfV1alpha1().Sessions("ns")
	srv := &RESTServer{SessionClient: sessions, RESTServerOptions: RESTServerOptions{LaunchTimeout: 5 * time.Second}}

	r := resumeRequest("rikey=newkey&rikeyid=42")
	r.RemoteAddr = "[2001:db8::1]:51000"
	w := httptest.NewRecorder()
	srv.resumeHandler(w, r)
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, `status_message="Streaming needs IPv4, and this client connected from 2001:db8::1"`) {
		t.Errorf("resume = %d %s, want the IPv4 refusal", w.Code, body)
	}
	s, err := sessions.Get(t.Context(), "alex-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec.Config.AESKey != "old" || s.Spec.Config.ClientIP != "" {
		t.Errorf("session config = %+v, want it untouched", s.Spec.Config)
	}
}
