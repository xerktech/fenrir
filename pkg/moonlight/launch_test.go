package moonlight

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	"games-on-whales.github.io/direwolf/pkg/generic"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

const testNamespace = "direwolf"

type launchFixture struct {
	server  *RESTServer
	client  *fake.Clientset
	created atomic.Int32
}

// newLaunchFixture builds a RESTServer over a fake clientset seeded with one
// App (ID 1) and the given existing sessions. Created sessions get a name and
// a stream URL immediately, standing in for the operator.
func newLaunchFixture(t *testing.T, opts RESTServerOptions, existing ...*v1alpha1types.Session) *launchFixture {
	t.Helper()

	app := &v1alpha1types.App{
		ObjectMeta: metav1.ObjectMeta{Name: "game", Namespace: testNamespace},
		Spec:       v1alpha1types.AppSpec{ID: 1},
	}

	objs := []runtime.Object{}
	sessionIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, s := range existing {
		objs = append(objs, s)
		if err := sessionIndexer.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	appIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := appIndexer.Add(app); err != nil {
		t.Fatal(err)
	}

	f := &launchFixture{client: fake.NewSimpleClientset(objs...)}
	f.client.PrependReactor("create", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		s := action.(k8stesting.CreateAction).GetObject().(*v1alpha1types.Session)
		n := f.created.Add(1)
		s.Name = fmt.Sprintf("%s%d", s.GenerateName, n)
		s.Status.StreamURL = "rtsp://example:48010"
		return false, nil, nil
	})

	emptyIndexer := func() cache.Indexer {
		return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	}
	if opts.LaunchTimeout == 0 {
		opts.LaunchTimeout = 5 * time.Second
	}
	f.server = NewRESTServer(
		nil,
		generic.NewLister[*v1alpha1types.Pairing](emptyIndexer()).Namespaced(testNamespace),
		generic.NewLister[*v1alpha1types.User](emptyIndexer()).Namespaced(testNamespace),
		generic.NewLister[*v1alpha1types.App](appIndexer).Namespaced(testNamespace),
		generic.NewLister[*v1alpha1types.Session](sessionIndexer).Namespaced(testNamespace),
		generic.NewLister[*v1.Pod](emptyIndexer()).Namespaced(testNamespace),
		f.client.DirewolfV1alpha1().Sessions(testNamespace),
		opts,
	)
	return f
}

func sessionFor(user string) *v1alpha1types.Session {
	return &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name:      user + "-existing",
			Namespace: testNamespace,
			Labels:    map[string]string{"direwolf/user": user},
		},
		Spec: v1alpha1types.SessionSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
		},
	}
}

// launch drives /launch as the given (already authenticated) user and returns
// the HTTP status and the parsed XML root.
func (f *launchFixture) launch(t *testing.T, user string) (int, Response) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/launch?appid=1&rikey=00&rikeyid=1&mode=1920x1080x60", nil)
	ctx := context.WithValue(req.Context(), userContextKey{}, &v1alpha1types.User{
		ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: testNamespace},
	})
	ctx = context.WithValue(ctx, pairingContextKey{}, &v1alpha1types.Pairing{
		ObjectMeta: metav1.ObjectMeta{Name: user + "-pairing", Namespace: testNamespace},
	})
	rec := httptest.NewRecorder()
	f.server.launchHandler(rec, req.WithContext(ctx))

	// Errorf, not Fatalf: this also runs on non-test goroutines.
	var resp Response
	if err := xml.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Errorf("unparseable response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, resp
}

func (f *launchFixture) sessionCount(t *testing.T) int {
	t.Helper()
	list, err := f.client.DirewolfV1alpha1().Sessions(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func assertBusy(t *testing.T, code int, resp Response, wantMessage string) {
	t.Helper()
	// HTTP 200 so moonlight-qt parses the XML and shows the message.
	if code != http.StatusOK {
		t.Errorf("HTTP status = %d, want 200", code)
	}
	if resp.StatusCode != busyStatusCode || resp.StatusMessage != wantMessage {
		t.Errorf("response = %d %q, want %d %q", resp.StatusCode, resp.StatusMessage, busyStatusCode, wantMessage)
	}
}

func TestNewRESTServerDefaults(t *testing.T) {
	s := NewRESTServer(nil, nil, nil, nil, nil, nil, nil, RESTServerOptions{})
	if s.LaunchTimeout != 100*time.Second {
		t.Errorf("LaunchTimeout = %s, want 100s (under moonlight-qt's 120s)", s.LaunchTimeout)
	}
	if s.MaxConcurrentSessions != 1 {
		t.Errorf("MaxConcurrentSessions = %d, want 1", s.MaxConcurrentSessions)
	}
}

func TestLaunchBusyWhenAnotherUserStreams(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{}, sessionFor("alice"))

	code, resp := f.launch(t, "bob")

	assertBusy(t, code, resp, busyMessage)
	if f.created.Load() != 0 {
		t.Errorf("created %d sessions, want none", f.created.Load())
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want alice's 1 untouched", n)
	}
}

func TestLaunchRelaunchBySameUserIsNotBusy(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{}, sessionFor("alice"))

	code, resp := f.launch(t, "alice")

	if code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Fatalf("relaunch = HTTP %d / %d %q, want success", code, resp.StatusCode, resp.StatusMessage)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want 1 (old replaced by new)", n)
	}
}

func TestLaunchBackToBackBySameUserLeavesOneSession(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{})

	// The informer never sees the first session; the second launch must
	// still replace it.
	f.launch(t, "alice")
	code, resp := f.launch(t, "alice")

	if code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Fatalf("relaunch = HTTP %d / %d %q, want success", code, resp.StatusCode, resp.StatusMessage)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want 1", n)
	}
}

func TestLaunchFreeHostSucceeds(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{})

	code, resp := f.launch(t, "bob")

	if code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Fatalf("launch = HTTP %d / %d %q, want success", code, resp.StatusCode, resp.StatusMessage)
	}
	if f.created.Load() != 1 {
		t.Errorf("created %d sessions, want 1", f.created.Load())
	}
}

func TestLaunchLimitAboveOne(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{MaxConcurrentSessions: 2}, sessionFor("alice"))

	if code, resp := f.launch(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second session = HTTP %d / %d %q, want success under limit 2", code, resp.StatusCode, resp.StatusMessage)
	}
	code, resp := f.launch(t, "carol")
	assertBusy(t, code, resp, busyMessage)
}

func TestLaunchUnlimited(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{MaxConcurrentSessions: -1}, sessionFor("alice"), sessionFor("bob"))

	if code, resp := f.launch(t, "carol"); resp.StatusCode != http.StatusOK {
		t.Fatalf("launch = HTTP %d / %d %q, want success with no limit", code, resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchBusyCheck(t *testing.T) {
	const reason = "The Library is running"
	f := newLaunchFixture(t, RESTServerOptions{
		MaxConcurrentSessions: -1,
		BusyCheck: func(context.Context) (string, error) {
			return reason, nil
		},
	})

	code, resp := f.launch(t, "bob")

	assertBusy(t, code, resp, reason)
	if f.created.Load() != 0 {
		t.Errorf("created %d sessions, want none", f.created.Load())
	}
}

func TestLaunchBusyCheckError(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{
		BusyCheck: func(context.Context) (string, error) {
			return "", fmt.Errorf("boom")
		},
	})

	code, _ := f.launch(t, "bob")

	// Fail closed: an unknown state must not start a second stream.
	if code != http.StatusInternalServerError {
		t.Errorf("HTTP status = %d, want 500", code)
	}
	if f.created.Load() != 0 {
		t.Errorf("created %d sessions, want none", f.created.Load())
	}
}

func TestLaunchFailureDeletesSession(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{LaunchTimeout: 300 * time.Millisecond})
	// The operator never publishes a stream URL.
	f.client.PrependReactor("get", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.GetAction).GetName()
		return true, &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}, nil
	})

	if code, _ := f.launch(t, "alice"); code != http.StatusInternalServerError {
		t.Fatalf("HTTP status = %d, want 500 on timeout", code)
	}
	if n := f.sessionCount(t); n != 0 {
		t.Fatalf("session count = %d, want the unready session deleted", n)
	}
	// And so it doesn't lock out the next user.
	if _, resp := f.launch(t, "bob"); resp.StatusCode == busyStatusCode {
		t.Errorf("bob got busy after alice's launch failed")
	}
}

func TestLaunchCountsUsersNotSessions(t *testing.T) {
	second := sessionFor("alice")
	second.Name = "alice-second"
	f := newLaunchFixture(t, RESTServerOptions{MaxConcurrentSessions: 2}, sessionFor("alice"), second)

	if code, resp := f.launch(t, "bob"); resp.StatusCode != http.StatusOK {
		t.Fatalf("launch = HTTP %d / %d %q, want success: alice is one user", code, resp.StatusCode, resp.StatusMessage)
	}
}

func TestLaunchWaiterHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	f := newLaunchFixture(t, RESTServerOptions{
		BusyCheck: func(context.Context) (string, error) {
			select {
			case entered <- struct{}{}:
				<-release
			default:
			}
			return "", nil
		},
	})
	aliceDone := make(chan struct{})
	go func() {
		defer close(aliceDone)
		f.launch(t, "alice")
	}()
	<-entered
	// Let alice finish before the test returns; she reports through t.
	defer func() {
		close(release)
		<-aliceDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := f.server.createSession(ctx, &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "bob"}}, func() (*v1alpha1types.Session, error) {
			return nil, nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("createSession succeeded while the slot was held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter ignored its context and stayed blocked on the launch slot")
	}
}

func TestLaunchConcurrentUsersRespectLimit(t *testing.T) {
	f := newLaunchFixture(t, RESTServerOptions{})

	const users = 8
	var wg sync.WaitGroup
	var ok, busy atomic.Int32
	for i := range users {
		wg.Go(func() {
			_, resp := f.launch(t, fmt.Sprintf("user%d", i))
			switch resp.StatusCode {
			case http.StatusOK:
				ok.Add(1)
			case busyStatusCode:
				busy.Add(1)
			}
		})
	}
	wg.Wait()

	if ok.Load() != 1 || busy.Load() != users-1 {
		t.Errorf("ok=%d busy=%d, want 1 and %d", ok.Load(), busy.Load(), users-1)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("session count = %d, want 1", n)
	}
}
