package moonlight

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		busy bool
	}{
		{"no pods", nil, false},
		{"unrelated pod", []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: testNamespace}}}, false},
		{"library pod", []*corev1.Pod{libraryPod("direwolf-library")}, true},
		{"terminating library pod", []*corev1.Pod{func() *corev1.Pod {
			p := libraryPod("direwolf-library")
			p.DeletionTimestamp = &metav1.Time{}
			p.Finalizers = []string{"test"}
			return p
		}()}, true},
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
			if (reason != "") != tc.busy {
				t.Errorf("reason = %q, want busy=%v", reason, tc.busy)
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
	closed atomic.Int32
}

func (l *stubLibrary) Open(context.Context) (bool, error) { return l.open, nil }

func (l *stubLibrary) Close(context.Context) (bool, error) {
	l.closed.Add(1)
	return l.open, nil
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
