package controllers

import (
	"context"
	stderrors "errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
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

// claimPod is a split session's App pod (no session label) mounting claim.
func claimPod(name, user, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS, Labels: map[string]string{
			"direwolf/user": user, podRoleLabel: podRoleApp,
		}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}}}},
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
		// alex was moved to their own library while playing from the shared
		// one: the pod still mounts the shared home, so it still holds it.
		{name: "pod on shared home after a library change", dwObjs: []runtime.Object{libraryUser("alex", true)}, k8sObjs: []runtime.Object{
			claimPod("s1-app", "alex", "steam-home"),
		}, owner: "", wantBusy: true},
		// Not a session's: the backup Jobs read the home beside a Library.
		{name: "backup pod on shared home", dwObjs: []runtime.Object{alex}, k8sObjs: []runtime.Object{
			func() *corev1.Pod {
				p := claimPod("backup", "", "steam-home")
				delete(p.Labels, "direwolf/user")
				return p
			}(),
		}, owner: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryFixture(t, tc.k8sObjs, tc.dwObjs...)
			lib := f.library.LibraryFor(nil)
			if tc.owner != "" {
				lib = f.library.LibraryFor(libraryUser(tc.owner, true))
			}
			busy, err := f.library.gameRunning(context.Background(), lib)
			if err != nil || busy != tc.wantBusy {
				t.Errorf("gameRunning(%q) = %v, %v; want %v", tc.owner, busy, err, tc.wantBusy)
			}
		})
	}
}

// The page serves the signed-in user's own Library, and the shared one to a
// User without one (or when the proxy names no one).
func TestLibraryServerPicksUsersLibrary(t *testing.T) {
	for _, tc := range []struct {
		name, header, wantPod string
	}{
		{"own library", "sam", "direwolf-library-sam"},
		{"no own library", "alex", LibraryPodName},
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

// A name with no User is refused rather than given the shared desktop, which
// is signed in to the shared library's accounts.
func TestLibraryServerRefusesUnknownUser(t *testing.T) {
	f := newLibraryFixture(t, nil, libraryUser("sam", true))
	rec := serveLibrary(f.server(t), "10.1.0.5:5555", http.Header{"Sec-Fetch-User": {"?1"}, DefaultLibraryUserHeader: {"mallory"}})
	if rec.Code != http.StatusForbidden || f.podExists(t) {
		t.Errorf("HTTP %d, Library started %v; want 403 and none", rec.Code, f.podExists(t))
	}
}

// Activity is stamped on the visitor's own Library pod, or it idles out.
func TestLibraryRecordActivityPerOwner(t *testing.T) {
	sam := libraryPod(0, true)
	sam.Name = "direwolf-library-sam"
	f := newLibraryFixture(t, []runtime.Object{libraryPod(0, true), sam})
	at := libraryTestNow.Add(time.Minute)
	if err := f.library.RecordActivity(context.Background(), "sam", at, at); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"direwolf-library-sam": true, LibraryPodName: false} {
		pod, err := f.k8s.CoreV1().Pods(libraryTestNS).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := pod.Annotations[libraryVisitAnnotation] == at.UTC().Format(time.RFC3339); got != want {
			t.Errorf("%s stamped = %v, want %v", name, got, want)
		}
	}
}

// Two libraries on one claim would each lock only their own Users: refused.
func TestLibraryClaimsConflict(t *testing.T) {
	kim := libraryUser("kim", true)
	for _, tc := range []struct {
		name string
		lib  LibraryRef
		ok   bool
	}{
		{"own claims", LibraryRef{Owner: "sam", HomePVC: "home-sam", GamesPVC: "games-sam"}, true},
		{"kim's own entry", LibraryRef{Owner: "kim", HomePVC: "home-kim", GamesPVC: "games-kim"}, true},
		{"shared home", LibraryRef{Owner: "sam", HomePVC: "steam-home", GamesPVC: "games-sam"}, false},
		{"shared games", LibraryRef{Owner: "sam", HomePVC: "home-sam", GamesPVC: "games"}, false},
		{"kim's games", LibraryRef{Owner: "sam", HomePVC: "home-sam", GamesPVC: "games-kim"}, false},
		{"one claim for both", LibraryRef{Owner: "sam", HomePVC: "x", GamesPVC: "x"}, false},
	} {
		if err := libraryClaimsConflict(tc.lib, "steam-home", "games", []v1alpha1types.User{*kim}); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	f := newLibraryFixture(t, nil, kim)
	if _, _, err := f.library.EnsurePod(context.Background(), LibraryRef{Owner: "sam", HomePVC: "home-kim", GamesPVC: "games-sam"}); err == nil || f.podExists(t) {
		t.Errorf("EnsurePod on kim's home = %v; want refused", err)
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
	users := cache.NewSharedIndexInformer(nil, &v1alpha1types.User{}, 0, cache.Indexers{})
	for _, u := range []*v1alpha1types.User{libraryUser("sam", true), libraryUser("kim", true), libraryUser("alex", false)} {
		if err := users.GetStore().Add(u); err != nil {
			t.Fatal(err)
		}
	}
	c := &SessionController{
		UserInformer:             generic.NewInformer[*v1alpha1types.User](users),
		SessionControllerOptions: SessionControllerOptions{LibraryHomePVC: "steam-home-local", LibraryGamesPVC: "games-library"},
	}
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

	// A library on the shared home or another User's claim is refused.
	for _, lib := range []*v1alpha1types.UserLibrary{
		{HomeClaimName: "steam-home-local", GamesClaimName: "games-ann"},
		{HomeClaimName: "home-ann", GamesClaimName: "games-sam"},
	} {
		ann := libraryUser("ann", true)
		ann.Spec.Library = lib
		if err := c.useUserLibrary(appSpec(), ann, steam); err == nil {
			t.Errorf("ann's library %+v accepted", lib)
		}
	}
}

// Of two Users on one claim, the first created keeps working; the newcomer
// is refused, with a page saying so rather than "try again".
func TestLibraryClaimsConflictFirstUserKeepsClaim(t *testing.T) {
	bob, carol := libraryUser("bob", true), libraryUser("carol", true)
	bob.CreationTimestamp = metav1.NewTime(libraryTestNow.Add(-time.Hour))
	carol.CreationTimestamp = metav1.NewTime(libraryTestNow)
	carol.Spec.Library.GamesClaimName = "games-bob"
	users := []v1alpha1types.User{*bob, *carol}
	if err := libraryClaimsConflict(LibraryRef{Owner: "bob", HomePVC: "home-bob", GamesPVC: "games-bob"}, "steam-home", "games", users); err != nil {
		t.Errorf("bob, first on games-bob, refused: %v", err)
	}
	if err := libraryClaimsConflict(LibraryRef{Owner: "carol", HomePVC: "home-carol", GamesPVC: "games-bob"}, "steam-home", "games", users); err == nil {
		t.Error("carol, second on games-bob, accepted")
	}

	f := newLibraryFixture(t, nil, bob, carol)
	rec := serveLibrary(f.server(t), "10.1.0.5:5555", http.Header{"Sec-Fetch-User": {"?1"}, DefaultLibraryUserHeader: {"carol"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "misconfigured") || f.podExists(t) {
		t.Errorf("carol's Library: HTTP %d %q; want 409 naming the misconfiguration", rec.Code, rec.Body.String())
	}
}

// A header the proxy sent empty names no one it knows: refused, not shared.
func TestLibraryServerRefusesEmptyUser(t *testing.T) {
	f := newLibraryFixture(t, nil)
	rec := serveLibrary(f.server(t), "10.1.0.5:5555", http.Header{"Sec-Fetch-User": {"?1"}, DefaultLibraryUserHeader: {""}})
	if rec.Code != http.StatusForbidden || f.podExists(t) {
		t.Errorf("HTTP %d; want 403 and no Library", rec.Code)
	}
}

// A session pod is never built on a claim a running Library mounts, even one
// the User's own Library check wouldn't see (an admin moved claims between
// Users while that Library ran).
func TestSessionPodRefusedOnOpenLibrarysClaim(t *testing.T) {
	bobs := libraryPod(0, true)
	bobs.Name = "direwolf-library-bob"
	bobs.Labels[v1alpha1types.LibraryOwnerLabel] = "bob"
	bobs.Spec.Volumes = []corev1.Volume{{Name: "steam-home", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "home-bob"},
	}}}
	c := &SessionController{K8sClient: k8sfake.NewClientset(bobs)}
	spec := func(claim string) *corev1.PodSpec {
		return &corev1.PodSpec{Volumes: []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}}}}
	}
	if open, err := c.libraryOpenOn(libraryTestNS, spec("home-bob")); err != nil || !strings.Contains(open, "direwolf-library-bob") {
		t.Errorf("libraryOpenOn(home-bob) = %q, %v; want bob's Library named", open, err)
	}
	if open, err := c.libraryOpenOn(libraryTestNS, spec("home-carol")); err != nil || open != "" {
		t.Errorf("libraryOpenOn(home-carol) = %q, %v; want none", open, err)
	}
}

// Through the session reconcile: a Library open on the App's claim ends the
// Session before any pod; a failed Library lookup is retried, never ignored.
func TestOpenLibraryClaimEndsSession(t *testing.T) {
	library := libraryPod(0, true)
	library.Namespace = portsTestNS
	library.Spec.Volumes = []corev1.Volume{{Name: "steam-home", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "steam-home"},
	}}}
	for _, tc := range []struct {
		name      string
		objs      []runtime.Object
		listErr   bool
		wantEnded bool
	}{
		{name: "Library open on the claim", objs: []runtime.Object{library}, wantEnded: true},
		{name: "no Library", wantEnded: false},
		{name: "Library lookup fails", listErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := podReadySession(f)
				s.Status.Conditions = s.Status.Conditions[:1] // PortsAllocated only: no pod yet
				return s
			}, nil)
			app, err := f.sc.AppInformer.Namespaced(portsTestNS).Get(f.app)
			if err != nil {
				t.Fatal(err)
			}
			app.Spec.Template.Spec.Volumes = append(app.Spec.Template.Spec.Volumes, corev1.Volume{Name: "home", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "steam-home"},
			}})
			client := k8sfake.NewClientset(tc.objs...)
			if tc.listErr {
				client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("down")
				})
			}
			f.sc.K8sClient = client
			f.sc.Recorder = record.NewFakeRecorder(10)
			_, err = f.sc.reconcilePod(context.Background(), sess)
			if ended := stderrors.Is(err, errSessionEnded); ended != tc.wantEnded {
				t.Fatalf("reconcilePod = %v, want ended %v", err, tc.wantEnded)
			}
			if tc.listErr && err == nil {
				t.Fatal("reconcilePod built a pod without checking for an open Library")
			}
			if got := f.sessionExists(t, sess.Name); got == tc.wantEnded {
				t.Errorf("session exists = %v, want %v", got, !tc.wantEnded)
			}
		})
	}
}

// Users created in one apply share a creation second: the name decides.
func TestLibraryClaimsConflictTieBreaksByName(t *testing.T) {
	bob, carol := libraryUser("bob", true), libraryUser("carol", true)
	carol.Spec.Library.GamesClaimName = "games-bob"
	users := []v1alpha1types.User{*bob, *carol}
	if err := libraryClaimsConflict(LibraryRef{Owner: "bob", HomePVC: "home-bob", GamesPVC: "games-bob"}, "steam-home", "games", users); err != nil {
		t.Errorf("bob refused: %v", err)
	}
	if err := libraryClaimsConflict(LibraryRef{Owner: "carol", HomePVC: "home-carol", GamesPVC: "games-bob"}, "steam-home", "games", users); err == nil {
		t.Error("carol accepted on bob's claim")
	}
}

// One claim for home and games is a misconfiguration too: 409, not 500.
func TestLibraryServerSameClaimTwiceIsMisconfigured(t *testing.T) {
	ann := libraryUser("ann", true)
	ann.Spec.Library.GamesClaimName = ann.Spec.Library.HomeClaimName
	f := newLibraryFixture(t, nil, ann)
	rec := serveLibrary(f.server(t), "10.1.0.5:5555", http.Header{"Sec-Fetch-User": {"?1"}, DefaultLibraryUserHeader: {"ann"}})
	if rec.Code != http.StatusConflict || f.podExists(t) {
		t.Errorf("HTTP %d; want 409 and no Library", rec.Code)
	}
}
