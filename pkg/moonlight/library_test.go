package moonlight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
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
			reason, err := LibraryBusyCheck(client.CoreV1().Pods(testNamespace))(context.Background())
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
		BusyCheck: func(context.Context) (string, error) {
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

func TestPodLibrary(t *testing.T) {
	terminating := libraryPod(v1alpha1types.LibraryPodName)
	terminating.DeletionTimestamp = &metav1.Time{}
	terminating.Finalizers = []string{"test"}
	unlabelled := libraryPod(v1alpha1types.LibraryPodName)
	unlabelled.Labels = nil

	for _, tc := range []struct {
		name        string
		pod         *corev1.Pod
		open        bool
		closing     bool
		wantDeleted bool
	}{
		{name: "no pod"},
		{name: "unlabelled pod of that name", pod: unlabelled},
		{name: "running", pod: libraryPod(v1alpha1types.LibraryPodName), open: true, closing: true, wantDeleted: true},
		{name: "already stopping", pod: terminating, closing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			pods := client.CoreV1().Pods(testNamespace)
			if tc.pod != nil {
				if _, err := pods.Create(t.Context(), tc.pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			lib := NewPodLibrary(pods)

			open, err := lib.Open(t.Context())
			if err != nil || open != tc.open {
				t.Errorf("Open = %v, %v; want %v", open, err, tc.open)
			}
			closing, err := lib.Close(t.Context())
			if err != nil || closing != tc.closing {
				t.Errorf("Close = %v, %v; want %v", closing, err, tc.closing)
			}
			deleted := slices.ContainsFunc(client.Actions(), func(a k8stesting.Action) bool { return a.GetVerb() == "delete" })
			if deleted != tc.wantDeleted {
				t.Errorf("deleted = %v, want %v", deleted, tc.wantDeleted)
			}
		})
	}
}

type stubLibrary struct {
	open   bool
	err    error
	closed atomic.Int32
}

func (l *stubLibrary) Open(context.Context) (bool, error) { return l.open, l.err }

func (l *stubLibrary) Close(context.Context) (bool, error) {
	l.closed.Add(1)
	return l.open, l.err
}

// Close deletes only the pod it read: a Library opened in between (a new UID)
// is someone's fresh session and is left alone, without an error.
func TestPodLibraryCloseSparesAReplacedPod(t *testing.T) {
	client := k8sfake.NewClientset()
	pods := client.CoreV1().Pods(testNamespace)
	pod := libraryPod(v1alpha1types.LibraryPodName)
	pod.UID = "old"
	if _, err := pods.Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := a.(k8stesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		pre := del.DeleteOptions.Preconditions
		if pre == nil || pre.UID == nil || *pre.UID != "old" {
			t.Errorf("delete preconditions = %+v, want UID old", pre)
		}
		return true, nil, k8serrors.NewConflict(corev1.Resource("pods"), pod.Name, errors.New("UID mismatch"))
	})

	closing, err := NewPodLibrary(pods).Close(t.Context())
	if closing || err != nil {
		t.Errorf("Close = %v, %v; want false, nil", closing, err)
	}
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
	if s.closeLibraryOffered() {
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

func (hungLibrary) Open(ctx context.Context) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func (hungLibrary) Close(ctx context.Context) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
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
