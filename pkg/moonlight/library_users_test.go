package moonlight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// Per-user libraries (User.spec.library, XERK-2500).

func ownLibraryUser(name string, own bool) *v1alpha1types.User {
	u := &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	if own {
		u.Spec.Library = &v1alpha1types.UserLibrary{HomeClaimName: "home-" + name, GamesClaimName: "games-" + name}
	}
	return u
}

func requestAs(t *testing.T, user *v1alpha1types.User, target string) *http.Request {
	t.Helper()
	ctx := context.WithValue(t.Context(), userContextKey{}, user)
	return httptest.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
}

// Each User sees the hand-written Apps and the games of the library they play
// from; two Users' copies of a game share a Moonlight ID, and each launches
// their own.
func TestAppListShowsOwnLibrarysGames(t *testing.T) {
	game := func(name, owner string) *v1alpha1types.App {
		a := testApp(name, 1<<30+570, false, nil)
		a.Labels = map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreSteam}
		if owner != "" {
			a.Labels[v1alpha1types.LibraryOwnerLabel] = owner
		}
		return a
	}
	s := appServer(t, testApp("desktop", 1, false, nil), game("steam-570", ""), game("sam-steam-570", "sam"))
	for _, tc := range []struct {
		user       *v1alpha1types.User
		want, hide string
	}{
		{ownLibraryUser("alex", false), "steam-570", "sam-steam-570"},
		{ownLibraryUser("sam", true), "sam-steam-570", "<AppTitle>steam-570<"},
		{ownLibraryUser("kim", true), "desktop", "steam-570"},
	} {
		rec := httptest.NewRecorder()
		s.appListHandler(rec, requestAs(t, tc.user, "/applist"))
		body := rec.Body.String()
		if !strings.Contains(body, "<AppTitle>desktop</AppTitle>") || !strings.Contains(body, tc.want) || strings.Contains(body, tc.hide) {
			t.Errorf("%s's applist = %s, want desktop and %s, not %s", tc.user.Name, body, tc.want, tc.hide)
		}
	}
	app, err := s.getAppByID("1073742394", "sam")
	if err != nil || app.Name != "sam-steam-570" {
		t.Errorf("sam's launch of the shared ID = %v, %v; want sam-steam-570", app, err)
	}
	if app, err := s.getAppByID("1073742394", "kim"); err == nil {
		t.Errorf("kim launched %s from another library", app.Name)
	}
}

// A Library blocks only the launches of Users playing from its library.
func TestLibraryBusyCheckIsPerLibrary(t *testing.T) {
	own := libraryPod("direwolf-library-sam")
	own.Labels[v1alpha1types.LibraryOwnerLabel] = "sam"
	for _, tc := range []struct {
		name string
		pods []*corev1.Pod
		user *v1alpha1types.User
		want string
	}{
		{"own Library blocks its owner", []*corev1.Pod{own}, ownLibraryUser("sam", true), libraryBusyMessage},
		{"own Library leaves shared users", []*corev1.Pod{own}, ownLibraryUser("alex", false), ""},
		{"own Library leaves other owners", []*corev1.Pod{own}, ownLibraryUser("kim", true), ""},
		{"shared Library leaves owners", []*corev1.Pod{libraryPod("direwolf-library")}, ownLibraryUser("sam", true), ""},
		{"shared Library blocks shared users", []*corev1.Pod{libraryPod("direwolf-library")}, ownLibraryUser("alex", false), libraryBusyMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			for _, p := range tc.pods {
				if _, err := client.CoreV1().Pods(testNamespace).Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			reason, err := LibraryBusyCheck(client.CoreV1().Pods(testNamespace))(t.Context(), tc.user)
			if err != nil || reason != tc.want {
				t.Errorf("reason = %q, %v; want %q", reason, err, tc.want)
			}
		})
	}
}

// "Close Library" closes the visitor's own Library, never another's.
func TestPodLibraryIsPerOwner(t *testing.T) {
	own := libraryPod("direwolf-library-sam")
	own.Labels[v1alpha1types.LibraryOwnerLabel] = "sam"
	client := k8sfake.NewClientset(own, libraryPod("direwolf-library"))
	lib := NewPodLibrary(client.CoreV1().Pods(testNamespace))
	if open, err := lib.Open(t.Context(), "kim"); open || err != nil {
		t.Errorf("kim's Library open = %v, %v", open, err)
	}
	if closing, err := lib.Close(t.Context(), "sam"); !closing || err != nil {
		t.Fatalf("Close(sam) = %v, %v", closing, err)
	}
	pods, err := client.CoreV1().Pods(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 || pods.Items[0].Name != "direwolf-library" {
		t.Errorf("pods left = %v, %v; want only the shared Library", pods.Items, err)
	}
}
