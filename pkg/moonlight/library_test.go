package moonlight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
)

func libraryPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace,
		Labels: map[string]string{v1alpha1types.LibraryPodLabel: v1alpha1types.LibraryPodLabelValue},
	}}
}

func TestLibraryBusyCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods []*corev1.Pod
		want string
	}{
		{"no pods", nil, ""},
		{"unrelated pod", []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: testNamespace}}}, ""},
		{"library pod", []*corev1.Pod{libraryPod("direwolf-library")}, libraryBusyMessage},
		{"terminating library pod", []*corev1.Pod{func() *corev1.Pod {
			p := libraryPod("direwolf-library")
			p.DeletionTimestamp = &metav1.Time{}
			p.Finalizers = []string{"test"}
			return p
		}()}, libraryStoppingMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			for _, p := range tc.pods {
				if _, err := client.CoreV1().Pods(testNamespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			reason, err := LibraryBusyCheck(client.CoreV1().Pods(testNamespace))(context.Background(), &v1alpha1types.User{})
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

// The Library started between the pre-create check and the Create: the
// launch must back its Session out and answer busy, not stream alongside it.
func TestLaunchBacksOutWhenBusyAfterCreate(t *testing.T) {
	var calls atomic.Int32
	f := newLaunchFixture(t, &RESTServerOptions{
		MaxConcurrentSessions: -1,
		BusyCheck: func(context.Context, *v1alpha1types.User) (string, error) {
			if calls.Add(1) == 1 {
				return "", nil
			}
			return libraryBusyMessage, nil
		},
	})

	code, resp := f.launch(t, "bob")

	assertBusy(t, code, resp, libraryBusyMessage)
	f.waitForSessionCount(t, 0)
}

// testPairing is the Pairing podLibraryFixture's Close requests are made for.
const testPairing = "bob-pairing"

// podLibraryFixture is a podLibrary over fake pods (pods) and a fake Pairing
// testPairing.
func podLibraryFixture(t *testing.T, pods ...*corev1.Pod) (lib Library, client *k8sfake.Clientset, direwolf *fake.Clientset) {
	t.Helper()
	client = k8sfake.NewClientset()
	for _, p := range pods {
		if _, err := client.CoreV1().Pods(testNamespace).Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	direwolf = fake.NewSimpleClientset(&v1alpha1types.Pairing{
		ObjectMeta: metav1.ObjectMeta{Name: testPairing, Namespace: testNamespace},
	})
	return NewPodLibrary(client.CoreV1().Pods(testNamespace), direwolf.DirewolfV1alpha1().Pairings(testNamespace)), client, direwolf
}

// closeRequest is the Library pod UID testPairing asks to close, or "".
func closeRequest(t *testing.T, direwolf *fake.Clientset) string {
	t.Helper()
	p, err := direwolf.DirewolfV1alpha1().Pairings(testNamespace).Get(t.Context(), testPairing, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Annotations[v1alpha1types.LibraryCloseAnnotation]
}

// Close never deletes a pod (moonlight-proxy may not): it records the open
// Library's UID on the requester's Pairing for the operator to act on.
func TestPodLibrary(t *testing.T) {
	terminating := libraryPod(v1alpha1types.LibraryPodName)
	terminating.DeletionTimestamp = &metav1.Time{}
	terminating.Finalizers = []string{"test"}
	unlabelled := libraryPod(v1alpha1types.LibraryPodName)
	unlabelled.Labels = nil
	running := libraryPod(v1alpha1types.LibraryPodName)
	running.UID = "running-uid"

	for _, tc := range []struct {
		name        string
		pod         *corev1.Pod
		open        bool
		closing     bool
		wantRequest string
	}{
		{name: "no pod"},
		{name: "unlabelled pod of that name", pod: unlabelled},
		{name: "running", pod: running, open: true, closing: true, wantRequest: "running-uid"},
		{name: "already stopping", pod: terminating, closing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pods []*corev1.Pod
			if tc.pod != nil {
				pods = append(pods, tc.pod)
			}
			lib, client, direwolf := podLibraryFixture(t, pods...)

			open, err := lib.Open(t.Context(), "")
			if err != nil || open != tc.open {
				t.Errorf("Open = %v, %v; want %v", open, err, tc.open)
			}
			closing, err := lib.Close(t.Context(), "", testPairing)
			if err != nil || closing != tc.closing {
				t.Errorf("Close = %v, %v; want %v", closing, err, tc.closing)
			}
			if got := closeRequest(t, direwolf); got != tc.wantRequest {
				t.Errorf("close request = %q, want %q", got, tc.wantRequest)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "create" {
					t.Errorf("Library touched pods with %s", a.GetVerb())
				}
			}
		})
	}
}

// A Pairing that can't take the request (revoked mid-request) fails the close.
func TestPodLibraryCloseFailsWithoutPairing(t *testing.T) {
	lib, _, _ := podLibraryFixture(t, libraryPod(v1alpha1types.LibraryPodName))
	if closing, err := lib.Close(t.Context(), "", "gone"); closing || err == nil {
		t.Errorf("Close = %v, %v; want false and an error", closing, err)
	}
}

type stubLibrary struct {
	open   bool
	err    error
	closed atomic.Int32
}

func (l *stubLibrary) Open(context.Context, string) (bool, error) { return l.open, l.err }

func (l *stubLibrary) Close(context.Context, string, string) (bool, error) {
	l.closed.Add(1)
	return l.open, l.err
}

// A failed Library check costs only the tile, never the game list; a failed
// close tells the user so, without the API error's detail.
func TestCloseLibraryTileFailures(t *testing.T) {
	lib := &stubLibrary{open: true, err: errors.New(`pods "direwolf-library" is forbidden: User "system:serviceaccount:secret-ns:sa"`)}
	f := newLaunchFixture(t, &RESTServerOptions{Library: lib})

	rec := httptest.NewRecorder()
	f.server.appListHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/applist", http.NoBody))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<ID>1</ID>") {
		t.Errorf("applist = %d %s, want the games listed", rec.Code, rec.Body.String())
	}

	f.appID = strconv.Itoa(CloseLibraryAppID)
	_, resp := f.launch(t, "bob")
	if resp.StatusMessage != libraryCloseFailedMessage {
		t.Errorf("launch message = %q, want %q", resp.StatusMessage, libraryCloseFailedMessage)
	}
}

// An App that holds the tile's ID wins: it is listed and launched as itself.
func TestAppWithCloseLibraryIDShadowsTile(t *testing.T) {
	lib := &stubLibrary{open: true}
	s := appServer(t, testApp("mine", CloseLibraryAppID, false, nil))
	s.Library = lib

	rec := httptest.NewRecorder()
	s.appListHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/applist", http.NoBody))
	if body := rec.Body.String(); strings.Contains(body, closeLibraryTitle) || !strings.Contains(body, "<AppTitle>mine</AppTitle>") {
		t.Errorf("applist = %s, want the App and no tile", body)
	}
	if s.closeLibraryOffered("") {
		t.Error("tile still offered for launch")
	}

	// Launching that ID starts the App and leaves the Library alone.
	f := newLaunchFixture(t, &RESTServerOptions{Library: lib})
	f.server.AppLister = s.AppLister
	f.appID = strconv.Itoa(CloseLibraryAppID)
	if code, resp := f.launch(t, "bob"); code != http.StatusOK || resp.StatusCode != http.StatusOK {
		t.Errorf("launch = HTTP %d, %d %q; want the App started", code, resp.StatusCode, resp.StatusMessage)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Errorf("%d Sessions, want the App's one", n)
	}
	if lib.closed.Load() != 0 {
		t.Error("launching the App closed the Library")
	}
}

// The tile is listed only while the Library is open, and launching it closes
// the Library with a message instead of starting a Session.
func TestCloseLibraryTile(t *testing.T) {
	for _, open := range []bool{true, false} {
		t.Run(fmt.Sprintf("open=%v", open), func(t *testing.T) {
			lib := &stubLibrary{open: open}
			f := newLaunchFixture(t, &RESTServerOptions{Library: lib})

			rec := httptest.NewRecorder()
			f.server.appListHandler(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/applist", http.NoBody))
			listed := strings.Contains(rec.Body.String(), fmt.Sprintf("<AppTitle>Close Library</AppTitle><ID>%d</ID>", CloseLibraryAppID))
			if listed != open {
				t.Errorf("tile listed = %v, want %v: %s", listed, open, rec.Body.String())
			}

			f.appID = strconv.Itoa(CloseLibraryAppID)
			code, resp := f.launch(t, "bob")
			want := libraryNotOpenMessage
			if open {
				want = libraryClosingMessage
			}
			if code != http.StatusOK || resp.StatusCode != libraryClosingStatusCode || resp.StatusMessage != want {
				t.Errorf("launch = HTTP %d, %d %q; want 200, %d %q", code, resp.StatusCode, resp.StatusMessage, libraryClosingStatusCode, want)
			}
			if lib.closed.Load() != 1 {
				t.Errorf("Close called %d times, want 1", lib.closed.Load())
			}
			if n := f.sessionCount(t); n != 0 {
				t.Errorf("%d Sessions created, want none", n)
			}
		})
	}
}

// hungLibrary answers only when its caller's context ends, like a hung API
// server.
type hungLibrary struct{}

func (hungLibrary) Open(ctx context.Context, _ string) (bool, error) {
	<-ctx.Done()
	return false, context.Cause(ctx) //nolint:wrapcheck // stands in for a client-go error
}

func (hungLibrary) Close(ctx context.Context, _, _ string) (bool, error) {
	<-ctx.Done()
	return false, context.Cause(ctx) //nolint:wrapcheck // stands in for a client-go error
}

// A hung API server costs /applist and the tile at most libraryCallTimeout.
func TestCloseLibraryTileBoundsHungAPIServer(t *testing.T) {
	f := newLaunchFixture(t, &RESTServerOptions{Library: hungLibrary{}})
	f.appID = strconv.Itoa(CloseLibraryAppID)
	for name, call := range map[string]func(){
		"applist": func() {
			f.server.appListHandler(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/applist", http.NoBody))
		},
		"tile launch": func() { f.launch(t, "bob") },
	} {
		done := make(chan struct{})
		go func() { call(); close(done) }()
		select {
		case <-done:
		case <-time.After(libraryCallTimeout + 3*time.Second):
			t.Fatalf("%s still waiting on a hung API server", name)
		}
	}
}
