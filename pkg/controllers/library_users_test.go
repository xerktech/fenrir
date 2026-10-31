package controllers

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// Per-user libraries (User.spec.library, XERK-2500): each User with one has
// their own Library pod, auth Secret, Steam lock and catalogue.

func libraryUser(name string, own bool) *v1alpha1types.User {
	u := &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS}}
	if own {
		u.Spec.Library = &v1alpha1types.UserLibrary{HomeClaimName: "home-" + name, GamesClaimName: "games-" + name}
	}
	return u
}

func librarySession(user string) *v1alpha1types.Session {
	return &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{Name: user + "-game", Namespace: libraryTestNS},
		Spec:       v1alpha1types.SessionSpec{UserReference: v1alpha1types.UserReference{Name: user}},
	}
}

func claimNames(pod *corev1.Pod) []string {
	var names []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			names = append(names, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

func TestLibraryEnsurePodForOwnLibrary(t *testing.T) {
	f := newLibraryFixture(t, nil, libraryUser("sam", true))
	pod, busy, err := f.library.EnsurePod(context.Background(), f.library.LibraryFor(libraryUser("sam", true)))
	if err != nil || busy != "" {
		t.Fatalf("EnsurePod = busy %q, err %v", busy, err)
	}
	if pod.Name != "direwolf-library-sam" || v1alpha1types.LibraryOwnerOf(pod.Labels) != "sam" ||
		pod.Labels[v1alpha1types.LibraryPodLabel] != v1alpha1types.LibraryPodLabelValue {
		t.Errorf("pod %s labels %v, want direwolf-library-sam owned by sam", pod.Name, pod.Labels)
	}
	if got := claimNames(pod); !slices.Equal(got, []string{"home-sam", "games-sam"}) {
		t.Errorf("claims = %v, want sam's own", got)
	}
	// Its own password: a desktop runs its user's code and can read it.
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "PASSWORD" && e.ValueFrom.SecretKeyRef.Name != "direwolf-library-auth-sam" {
			t.Errorf("PASSWORD from %s, want direwolf-library-auth-sam", e.ValueFrom.SecretKeyRef.Name)
		}
	}
	shared, err := f.library.AuthPassword(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	own, err := f.library.AuthPassword(context.Background(), "sam")
	if err != nil || own == shared {
		t.Errorf("sam's password %q (err %v) is the shared Library's", own, err)
	}
	// The shared Library is a separate pod with the shared claims.
	sharedPod, busy, err := f.library.EnsurePod(context.Background(), f.library.LibraryFor(nil))
	if err != nil || busy != "" {
		t.Fatalf("shared EnsurePod = busy %q, err %v", busy, err)
	}
	if sharedPod.Name != LibraryPodName || v1alpha1types.LibraryOwnerOf(sharedPod.Labels) != "" ||
		!slices.Equal(claimNames(sharedPod), []string{"steam-home", "games"}) {
		t.Errorf("shared pod %s labels %v claims %v", sharedPod.Name, sharedPod.Labels, claimNames(sharedPod))
	}
}

// The Steam lock is per library: only a session of a User playing from the
// same library holds it.
func TestLibraryLockIsPerLibrary(t *testing.T) {
	alex, sam := libraryUser("alex", false), libraryUser("sam", true)
	for _, tc := range []struct {
		name     string
		k8sObjs  []runtime.Object
		dwObjs   []runtime.Object
		owner    string
		wantBusy bool
	}{
		{name: "own session blocks own", dwObjs: []runtime.Object{sam, librarySession("sam")}, owner: "sam", wantBusy: true},
		{name: "shared session leaves own", dwObjs: []runtime.Object{alex, sam, librarySession("alex")}, owner: "sam"},
		{name: "own session leaves shared", dwObjs: []runtime.Object{alex, sam, librarySession("sam")}, owner: ""},
		{name: "shared session blocks shared", dwObjs: []runtime.Object{alex, librarySession("alex")}, owner: "", wantBusy: true},
		// Its library is unknown, so it blocks every library.
		{name: "unknown user blocks own", dwObjs: []runtime.Object{librarySession("gone")}, owner: "sam", wantBusy: true},
		{name: "terminating own pod blocks own", dwObjs: []runtime.Object{sam}, k8sObjs: []runtime.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: libraryTestNS,
			Labels: map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue, "direwolf/user": "sam"},
		}}}, owner: "sam", wantBusy: true},
		{name: "terminating shared pod leaves own", dwObjs: []runtime.Object{alex, sam}, k8sObjs: []runtime.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: libraryTestNS,
			Labels: map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue, "direwolf/user": "alex"},
		}}}, owner: "sam"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryFixture(t, tc.k8sObjs, tc.dwObjs...)
			busy, err := f.library.gameRunning(context.Background(), tc.owner)
			if err != nil || busy != tc.wantBusy {
				t.Errorf("gameRunning(%q) = %v, %v; want %v", tc.owner, busy, err, tc.wantBusy)
			}
		})
	}
}

// The page serves the signed-in user's own Library, and the shared one to a
// User without one or an unknown name.
func TestLibraryServerPicksUsersLibrary(t *testing.T) {
	for _, tc := range []struct {
		name, header, wantPod string
	}{
		{"own library", "sam", "direwolf-library-sam"},
		{"no own library", "alex", LibraryPodName},
		{"not a User", "admin", LibraryPodName},
		{"no header", "", LibraryPodName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryFixture(t, nil, libraryUser("sam", true), libraryUser("alex", false))
			header := http.Header{"Sec-Fetch-User": {"?1"}}
			if tc.header != "" {
				header.Set(DefaultLibraryUserHeader, tc.header)
			}
			rec := serveLibrary(f.server(t), "10.1.0.5:5555", header)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Starting") {
				t.Fatalf("HTTP %d %q, want the starting page", rec.Code, rec.Body.String())
			}
			pods, err := f.k8s.CoreV1().Pods(libraryTestNS).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(pods.Items) != 1 || pods.Items[0].Name != tc.wantPod {
				t.Errorf("pods = %v, want only %s", pods.Items, tc.wantPod)
			}
		})
	}
}

// A scan of a User's own Library makes Apps of their own, and leaves the
// shared library's alone (and the other way round).
func TestCatalogueScanOwnLibrary(t *testing.T) {
	shared := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{
		Name: "steam-999", Namespace: libraryTestNS, UID: "u999",
		Labels: map[string]string{v1alpha1types.CatalogueLabel: v1alpha1types.CatalogueStoreSteam},
	}}
	f := newCatalogueFixture(t, baseApp("steam-base", nil), baseApp("heroic-base", nil), shared)
	pod := libraryPod(0, true)
	pod.Name = "direwolf-library-sam"
	pod.Labels[v1alpha1types.LibraryOwnerLabel] = "sam"
	if err := f.catalogue.Scan(context.Background(), pod, 0); err != nil {
		t.Fatal(err)
	}
	f.app(t, "steam-999") // not in sam's library, but not sam's App either
	dota := f.app(t, "sam-steam-570")
	if v1alpha1types.LibraryOwnerOf(dota.Labels) != "sam" || dota.Labels[v1alpha1types.CatalogueLabel] != "steam" {
		t.Errorf("sam-steam-570 labels = %v", dota.Labels)
	}
	if !v1alpha1types.AppVisibleTo(dota, "sam") || v1alpha1types.AppVisibleTo(dota, "") {
		t.Error("sam's App is not visible to sam alone")
	}
	if f.catalogue.ScanDue("sam") || !f.catalogue.ScanDue("") {
		t.Error("sam's scan was recorded against the wrong library")
	}

	// The shared library's scan keeps sam's Apps and replaces steam-999.
	if err := f.catalogue.Scan(context.Background(), libraryPod(0, true), 0); err != nil {
		t.Fatal(err)
	}
	f.app(t, "sam-steam-570")
	f.app(t, "steam-570")
	if _, err := f.dw.DirewolfV1alpha1().Apps(libraryTestNS).Get(context.Background(), "steam-999", metav1.GetOptions{}); err == nil {
		t.Error("shared scan kept the uninstalled steam-999")
	}

	// Refresh re-copies the base onto sam's App, which keeps its owner.
	if err := f.catalogue.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v1alpha1types.LibraryOwnerOf(f.app(t, "sam-steam-570").Labels) != "sam" {
		t.Error("refresh dropped sam's App's owner")
	}
}

func TestCatalogueOwnedAppName(t *testing.T) {
	long := strings.Repeat("a", 40)
	for _, id := range []string{"570", "Quail", strings.Repeat("x", 80)} {
		for _, owner := range []string{"", "sam", long} {
			name := catalogueOwnedAppName(owner, v1alpha1types.CatalogueStoreBattlenet, id)
			if len(name) > 63 || (owner != "" && !strings.HasPrefix(name, owner+"-battlenet-")) {
				t.Errorf("catalogueOwnedAppName(%q, battlenet, %q) = %q", owner, id, name)
			}
		}
	}
	if catalogueOwnedAppName("", "steam", "570") != catalogueAppName("steam", "570") {
		t.Error("the shared library's names changed")
	}
}

func TestSessionPodUsesUsersLibrary(t *testing.T) {
	c := &SessionController{SessionControllerOptions: SessionControllerOptions{LibraryHomePVC: "steam-home-local", LibraryGamesPVC: "games-library"}}
	appSpec := func() *corev1.PodSpec {
		return &corev1.PodSpec{Volumes: []corev1.Volume{
			{Name: "steam-home", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "steam-home-local"}}},
			{Name: "games", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "games-library"}}},
			{Name: "saves", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "other"}}},
			{Name: "shm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}}
	}
	steam := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "steam"}}

	spec := appSpec()
	if err := c.useUserLibrary(spec, libraryUser("sam", true), steam); err != nil {
		t.Fatal(err)
	}
	if got := claimNames(&corev1.Pod{Spec: *spec}); !slices.Equal(got, []string{"home-sam", "games-sam", "other"}) {
		t.Errorf("sam's claims = %v", got)
	}
	spec = appSpec()
	if err := c.useUserLibrary(spec, libraryUser("alex", false), steam); err != nil {
		t.Fatal(err)
	}
	if got := claimNames(&corev1.Pod{Spec: *spec}); !slices.Equal(got, []string{"steam-home-local", "games-library", "other"}) {
		t.Errorf("alex's claims = %v, want the shared ones", got)
	}

	samsGame := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "sam-steam-570", Labels: map[string]string{
		v1alpha1types.CatalogueLabel: "steam", v1alpha1types.LibraryOwnerLabel: "sam",
	}}}
	sharedGame := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: "steam-570", Labels: map[string]string{
		v1alpha1types.CatalogueLabel: "steam",
	}}}
	for _, tc := range []struct {
		user *v1alpha1types.User
		app  *v1alpha1types.App
	}{{libraryUser("alex", false), samsGame}, {libraryUser("sam", true), sharedGame}, {libraryUser("kim", true), samsGame}} {
		if err := c.useUserLibrary(appSpec(), tc.user, tc.app); err == nil {
			t.Errorf("%s launched %s from another library", tc.user.Name, tc.app.Name)
		}
	}
}
