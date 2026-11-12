package controllers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// vrFixture is the headsets host with the VR page over these Apps: ac (VR),
// flat (no vrPort), hidden-vr, and dana's own-library VR game.
type vrFixture struct {
	*libraryFixture
	s      *LibraryServer
	status VRStatus
	// statusErr fails the vr-agent read.
	statusErr error
}

func newVRFixture(t *testing.T, sessions ...*v1alpha1types.Session) *vrFixture {
	t.Helper()
	f := newLibraryFixture(t, nil, libraryUser("sam", false), libraryUser("alex", false), libraryUser("dana", true))
	s := f.server(t)
	s.HeadsetsSecret = []byte(testHeadsetsSecret)
	for _, sess := range sessions {
		if _, err := f.dw.DirewolfV1alpha1().Sessions(libraryTestNS).Create(context.Background(), sess, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var n atomic.Int32
	f.dw.PrependReactor("create", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sess, ok := action.(k8stesting.CreateAction).GetObject().(*v1alpha1types.Session)
		if ok && sess.Name == "" {
			sess.Name = fmt.Sprintf("%s%d", sess.GenerateName, n.Add(1))
			sess.UID = types.UID(sess.Name)
		}
		return false, nil, nil
	})
	apps := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	vrApp := func(name, title string, vr, hidden bool, owner string) *v1alpha1types.App {
		a := &v1alpha1types.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS}}
		a.Spec.Title, a.Spec.VRPort, a.Spec.Hidden = title, vr, hidden
		if owner != "" {
			a.Labels = map[string]string{v1alpha1types.LibraryOwnerLabel: owner}
		}
		return a
	}
	for _, a := range []*v1alpha1types.App{
		vrApp("ac", "<i>Assetto</i>", true, false, ""),
		vrApp("flat", "Flat Game", false, false, ""),
		vrApp("hidden-vr", "Hidden VR", true, true, ""),
		vrApp("dana-vr", "Dana's VR", true, false, "dana"),
		vrApp("beat", "Beat", true, false, ""),
	} {
		if err := apps.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	vf := &vrFixture{libraryFixture: f, s: s}
	s.VRLaunch = &VRLaunch{
		Apps:     generic.NewLister[*v1alpha1types.App](apps).Namespaced(libraryTestNS),
		Sessions: f.dw.DirewolfV1alpha1().Sessions(libraryTestNS),
		Pods:     f.k8s.CoreV1().Pods(libraryTestNS),
		Status: func(context.Context, *v1alpha1types.Session, *corev1.Pod) (VRStatus, error) {
			return vf.status, vf.statusErr
		},
		MaxConcurrentSessions: 1,
	}
	return vf
}

// clicked is what a browser sends for the page's own form, clicked.
var clicked = http.Header{"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-User": {"?1"}}

func (f *vrFixture) get(user string) *httptest.ResponseRecorder {
	h := http.Header{}
	if user != "" {
		h.Set(DefaultLibraryUserHeader, user)
	}
	return headsetsRequest(f.s, http.MethodGet, libraryVRPath, h)
}

func (f *vrFixture) post(user string, form url.Values, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, libraryVRPath, strings.NewReader(form.Encode()))
	req.RemoteAddr = "10.1.0.5:5555"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(LibraryProxySecretHeader, testHeadsetsSecret)
	if user != "" {
		req.Header.Set(DefaultLibraryUserHeader, user)
	}
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	return rec
}

func (f *vrFixture) play(user, app string) *httptest.ResponseRecorder {
	return f.post(user, url.Values{"action": {"play"}, "app": {app}}, clicked)
}

func (f *vrFixture) sessions(t *testing.T) []v1alpha1types.Session {
	t.Helper()
	l, err := f.dw.DirewolfV1alpha1().Sessions(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func (f *vrFixture) addPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if _, err := f.k8s.CoreV1().Pods(libraryTestNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func moonlightSession(name, user string) *v1alpha1types.Session {
	s := librarySession(user)
	s.Name = name
	return s
}

func pageVRSession(name, user string) *v1alpha1types.Session {
	s := librarySession(user)
	s.Name, s.UID = name, types.UID(name)
	s.Spec.Mode = v1alpha1types.SessionModeVR
	s.Spec.GameReference.Name = "ac"
	return s
}

// The page lists only the Apps the User may play in VR: vrPort, not hidden,
// visible from their library; titles escaped.
func TestVRPageListsVRApps(t *testing.T) {
	f := newVRFixture(t)
	rec := f.get("sam")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "&lt;i&gt;Assetto&lt;/i&gt;") || strings.Contains(body, "<i>Assetto") ||
		strings.Contains(body, "Flat Game") || strings.Contains(body, "Hidden VR") || strings.Contains(body, "Dana&#39;s VR") {
		t.Errorf("HTTP %d\n%s", rec.Code, body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q", csp)
	}
	// dana plays from her own library: she sees her own VR game.
	if body := f.get("dana").Body.String(); !strings.Contains(body, "Dana&#39;s VR") {
		t.Errorf("dana's page:\n%s", body)
	}
}

// Play creates one VR Session for the signed-in User and App, with none of
// Moonlight's fields; a second click starts no second one.
func TestVRPagePlayCreatesVRSession(t *testing.T) {
	f := newVRFixture(t)
	if rec := f.play("sam", "ac"); rec.Code != http.StatusSeeOther {
		t.Fatalf("HTTP %d\n%s", rec.Code, rec.Body)
	}
	if rec := f.play("sam", "ac"); rec.Code != http.StatusSeeOther {
		t.Fatalf("second play: HTTP %d\n%s", rec.Code, rec.Body)
	}
	got := f.sessions(t)
	if len(got) != 1 {
		t.Fatalf("%d sessions, want 1", len(got))
	}
	s := got[0]
	if !s.Spec.IsVR() || s.Spec.UserReference.Name != "sam" || s.Spec.GameReference.Name != "ac" ||
		s.Spec.PairingReference.Name != "" || s.Spec.GatewayReference.Name != "" || s.Spec.Config.AESKey != "" ||
		s.Labels["direwolf/user"] != "sam" {
		t.Errorf("session %+v", s)
	}
	if body := f.get("sam").Body.String(); !strings.Contains(body, "Starting") || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("page after play:\n%s", body)
	}
}

// Only a VR App the User can see starts, whatever the form names.
func TestVRPageRefusesOtherApps(t *testing.T) {
	f := newVRFixture(t)
	for _, app := range []string{"flat", "hidden-vr", "dana-vr", "missing", ""} {
		if rec := f.play("sam", app); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "can&#39;t be played in VR") {
			t.Errorf("%q: HTTP %d\n%s", app, rec.Code, rec.Body)
		}
	}
	if n := len(f.sessions(t)); n != 0 {
		t.Errorf("%d sessions created", n)
	}
}

// The Library holds the Steam home: while the User's Library pod exists,
// running or stopping, no VR session starts. Another library's doesn't count.
func TestVRPageRespectsLibraryLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stopping bool
		want     string
	}{{"open", false, "game library is open"}, {"stopping", true, "game library is closing"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVRFixture(t)
			pod := libraryPod(0, true)
			if tc.stopping {
				pod.DeletionTimestamp = &metav1.Time{Time: libraryTestNow}
				pod.Finalizers = []string{"test"}
			}
			f.addPod(t, pod)
			rec := f.play("sam", "ac")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
			}
			if n := len(f.sessions(t)); n != 0 {
				t.Errorf("%d sessions created", n)
			}
			// dana's own library has no Library open.
			if rec := f.play("dana", "dana-vr"); rec.Code != http.StatusSeeOther {
				t.Errorf("dana: HTTP %d\n%s", rec.Code, rec.Body)
			}
		})
	}
}

// A Library started between the check and the Create backs the Session out.
func TestVRPageBacksOutWhenLibraryStartsAfterCreate(t *testing.T) {
	f := newVRFixture(t)
	f.dw.PrependReactor("create", "sessions", func(k8stesting.Action) (bool, runtime.Object, error) {
		_, _ = f.k8s.CoreV1().Pods(libraryTestNS).Create(context.Background(), libraryPod(0, false), metav1.CreateOptions{})
		return false, nil, nil
	})
	// The back-out deletes only the Session it made (UID precondition): the
	// fake clientset ignores preconditions, so the request is checked.
	var deleteUID types.UID
	f.dw.PrependReactor("delete", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(k8stesting.DeleteActionImpl)
		if p := del.GetDeleteOptions().Preconditions; ok && p != nil && p.UID != nil {
			deleteUID = *p.UID
		}
		return false, nil, nil
	})
	defer func() {
		if deleteUID != "sam-ac-1" {
			t.Errorf("back-out delete precondition UID %q, want the created Session's", deleteUID)
		}
	}()
	rec := f.play("sam", "ac")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "game library is open") {
		t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
	}
	if n := len(f.sessions(t)); n != 0 {
		t.Errorf("%d sessions left, want the new one backed out", n)
	}
}

// A User's Moonlight game is never ended for VR, and the session limit
// counts other users' Sessions as moonlight-proxy's does.
func TestVRPageRefusesWhileBusy(t *testing.T) {
	t.Run("own moonlight session", func(t *testing.T) {
		f := newVRFixture(t, moonlightSession("sam-flat", "sam"))
		if rec := f.play("sam", "ac"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "running in Moonlight") {
			t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
		}
		if n := len(f.sessions(t)); n != 1 {
			t.Errorf("%d sessions, want only the Moonlight one", n)
		}
	})
	t.Run("another user", func(t *testing.T) {
		f := newVRFixture(t, moonlightSession("alex-flat", "alex"))
		if rec := f.play("sam", "ac"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Someone else is playing") {
			t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
		}
		f.s.VRLaunch.MaxConcurrentSessions = -1
		if rec := f.play("sam", "ac"); rec.Code != http.StatusSeeOther {
			t.Errorf("unlimited: HTTP %d\n%s", rec.Code, rec.Body)
		}
	})
	t.Run("own vr session stopping", func(t *testing.T) {
		old := pageVRSession("sam-ac-old", "sam")
		old.DeletionTimestamp, old.Finalizers = &metav1.Time{Time: libraryTestNow}, []string{"test"}
		f := newVRFixture(t, old)
		if rec := f.play("sam", "ac"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "stopping") {
			t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
		}
	})
}

// A deleted Session goes at once while its pods terminate, still running
// Steam: any pod of the User's holds the lock (QA, XERK-2670). Another
// User's pod doesn't, nor does a Library pod.
func TestVRPageWaitsForUsersPods(t *testing.T) {
	f := newVRFixture(t)
	pod := func(name, user string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS,
			Labels: map[string]string{"direwolf/user": user}, DeletionTimestamp: &metav1.Time{Time: libraryTestNow}, Finalizers: []string{"test"}}}
	}
	f.addPod(t, pod("alex-old-app", "alex"))
	f.addPod(t, pod("sam-old-app", "sam"))
	if rec := f.play("sam", "ac"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "still shutting down") {
		t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
	}
	if n := len(f.sessions(t)); n != 0 {
		t.Errorf("%d sessions created", n)
	}
	if err := f.k8s.CoreV1().Pods(libraryTestNS).Delete(context.Background(), "sam-old-app", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec := f.play("sam", "ac"); rec.Code != http.StatusSeeOther {
		t.Errorf("after the pod went: HTTP %d\n%s", rec.Code, rec.Body)
	}
}

// Playing another game while a VR session runs says so; it starts nothing.
func TestVRPageRefusesSecondGame(t *testing.T) {
	f := newVRFixture(t, pageVRSession("sam-ac-1", "sam"))
	if rec := f.play("sam", "beat"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "another game is running") {
		t.Errorf("HTTP %d\n%s", rec.Code, rec.Body)
	}
	if n := len(f.sessions(t)); n != 1 {
		t.Errorf("%d sessions", n)
	}
}

// Stop ends only the signed-in User's VR Sessions.
func TestVRPageStop(t *testing.T) {
	f := newVRFixture(t, pageVRSession("sam-ac-1", "sam"), pageVRSession("alex-ac-1", "alex"))
	if rec := f.post("sam", url.Values{"action": {"stop"}}, clicked); rec.Code != http.StatusSeeOther {
		t.Fatalf("HTTP %d\n%s", rec.Code, rec.Body)
	}
	got := f.sessions(t)
	if len(got) != 1 || got[0].Name != "alex-ac-1" {
		t.Errorf("sessions left %v, want only alex's", got)
	}
	g := newVRFixture(t, moonlightSession("sam-flat", "sam"))
	if rec := g.post("sam", url.Values{"action": {"stop"}}, clicked); rec.Code != http.StatusSeeOther || len(g.sessions(t)) != 1 {
		t.Errorf("stop ended a Moonlight session: HTTP %d", rec.Code)
	}
}

// readyVRPod is sess's pod, ready on talos04.
func readyVRPod(sess *v1alpha1types.Session) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: libraryTestNS,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1types.GroupVersion.String(), Kind: "Session",
			Name: sess.Name, UID: sess.UID, Controller: new(true)}}}}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.HostIP, pod.Status.PodIP = "10.10.10.34", "10.244.0.9"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

// Once ready the page shows the node IP and the session's VR port, and
// never a PIN: headsets pair through the front door (XERK-2811).
func TestVRPageShowsAddress(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess)
	f.addPod(t, readyVRPod(sess))
	f.s.VRLaunch.statuses = nil // a fresh read, not the cached one
	body := f.get("sam").Body.String()
	for _, want := range []string{"10.10.10.34", "21003", "Stop the VR session", `http-equiv="refresh"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "PIN") || strings.Contains(body, "/.direwolf/headsets") {
		t.Errorf("page offers PIN pairing:\n%s", body)
	}
	// An unreachable agent still shows the address.
	f.status, f.statusErr = VRStatus{}, errors.New("down")
	f.s.VRLaunch.statuses = nil // a fresh read, not the cached one
	if body := f.get("sam").Body.String(); !strings.Contains(body, "21003") {
		t.Errorf("agent down:\n%s", body)
	}
	// Connected: a slow refresh.
	f.status, f.statusErr = VRStatus{HeadsetConnected: true}, nil
	f.s.VRLaunch.statuses = nil // a fresh read, not the cached one
	if body := f.get("sam").Body.String(); !strings.Contains(body, "headset is connected") || !strings.Contains(body, `content="30"`) {
		t.Errorf("connected:\n%s", body)
	}
	// Another user sees none of it.
	if body := f.get("alex").Body.String(); strings.Contains(body, "21003") || strings.Contains(body, "headset is connected") {
		t.Errorf("alex sees sam's session:\n%s", body)
	}
}

// A User with no Headset is told to pair one, and gets no Session: it
// would only end, as the image refuses an empty key list (XERK-2811).
func TestVRPageRefusesUserWithoutHeadset(t *testing.T) {
	f := newVRFixture(t)
	headsets := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, h := range []*v1alpha1types.Headset{
		{ObjectMeta: metav1.ObjectMeta{Name: "alex-quest", Namespace: libraryTestNS}, Spec: v1alpha1types.HeadsetSpec{UserReference: v1alpha1types.UserReference{Name: "alex"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "sam-old", Namespace: libraryTestNS, DeletionTimestamp: &metav1.Time{Time: time.Now()}},
			Spec: v1alpha1types.HeadsetSpec{UserReference: v1alpha1types.UserReference{Name: "sam"}}},
	} {
		if err := headsets.Add(h); err != nil {
			t.Fatal(err)
		}
	}
	f.s.VRLaunch.Headsets = generic.NewLister[*v1alpha1types.Headset](headsets).Namespaced(libraryTestNS)
	if rec := f.play("sam", "ac"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Pair your headset first") {
		t.Errorf("sam (no headset): HTTP %d\n%s", rec.Code, rec.Body)
	}
	if n := len(f.sessions(t)); n != 0 {
		t.Fatalf("%d sessions created for a User with no headset", n)
	}
	if rec := f.play("alex", "ac"); rec.Code >= 400 {
		t.Errorf("alex (paired): HTTP %d\n%s", rec.Code, rec.Body)
	}
	if n := len(f.sessions(t)); n != 1 {
		t.Errorf("%d sessions after alex's start, want 1", n)
	}
}

// No address before the pod is Ready on a node, or before the port is known.
func TestVRPageWaitsForReadyPod(t *testing.T) {
	for name, edit := range map[string]func(*corev1.Pod, *v1alpha1types.Session){
		"not ready": func(p *corev1.Pod, _ *v1alpha1types.Session) {
			p.Status.Conditions[0].Status = corev1.ConditionFalse
		},
		"no host IP": func(p *corev1.Pod, _ *v1alpha1types.Session) { p.Status.HostIP = "" },
		"no port":    func(_ *corev1.Pod, s *v1alpha1types.Session) { s.Status.Ports.VR = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			sess := pageVRSession("sam-ac-1", "sam")
			sess.Status.Ports.VR = 21003
			pod := readyVRPod(sess)
			edit(pod, sess)
			f := newVRFixture(t, sess)
			f.addPod(t, pod)
			f.status = VRStatus{HeadsetConnected: true}
			if body := f.get("sam").Body.String(); !strings.Contains(body, "Starting") || strings.Contains(body, "Address") {
				t.Errorf("page:\n%s", body)
			}
		})
	}
}

// vr-agent is read with a deadline and at most once per libraryVRStatusTTL:
// each read costs a Secret GET on the operator's shared client.
func TestVRPageBoundsAgentReads(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess)
	f.addPod(t, readyVRPod(sess))
	var reads atomic.Int32
	f.s.VRLaunch.Status = func(ctx context.Context, _ *v1alpha1types.Session, _ *corev1.Pod) (VRStatus, error) {
		reads.Add(1)
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > libraryVRStatusTimeout {
			t.Errorf("vr-agent read without its %s deadline", libraryVRStatusTimeout)
		}
		return VRStatus{HeadsetConnected: true}, nil
	}
	for range 3 {
		if body := f.get("sam").Body.String(); !strings.Contains(body, "headset is connected") {
			t.Fatalf("page:\n%s", body)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d vr-agent reads, want 1", n)
	}
}

// Concurrent views of one Session share one vr-agent read: each read is a
// Secret GET on the operator's shared client, and a scripted flood of views
// starved other users' starts (QA, XERK-2670).
func TestVRPageSharesConcurrentAgentReads(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess)
	f.addPod(t, readyVRPod(sess))
	var reads atomic.Int32
	release := make(chan struct{})
	f.s.VRLaunch.Status = func(context.Context, *v1alpha1types.Session, *corev1.Pod) (VRStatus, error) {
		reads.Add(1)
		<-release
		return VRStatus{HeadsetConnected: true}, nil
	}
	const views = 20
	bodies := make(chan string, views)
	for range views {
		go func() { bodies <- f.get("sam").Body.String() }()
	}
	// Let every view reach the read before it finishes.
	for f.s.VRLaunch.waiting() < 1 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range views {
		if body := <-bodies; !strings.Contains(body, "headset is connected") {
			t.Fatalf("page:\n%s", body)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d vr-agent reads for %d concurrent views, want 1", n, views)
	}
}

// The shared read outlives the request that started it: a closed tab must
// not hand its waiters a cancelled read. A read that panics still releases
// them.
func TestVRAgentReadOutlivesFirstRequest(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	pod := readyVRPod(sess)
	started, release := make(chan struct{}), make(chan struct{})
	v := &VRLaunch{Status: func(ctx context.Context, _ *v1alpha1types.Session, _ *corev1.Pod) (VRStatus, error) {
		close(started)
		<-release
		if err := ctx.Err(); err != nil {
			return VRStatus{}, fmt.Errorf("read: %w", err)
		}
		return VRStatus{HeadsetConnected: true}, nil
	}}
	first, cancelFirst := context.WithCancel(t.Context())
	go func() { _, _ = v.readStatus(first, sess, pod) }()
	<-started
	waiter := make(chan VRStatus, 1)
	go func() {
		st, _ := v.readStatus(t.Context(), sess, pod)
		waiter <- st
	}()
	cancelFirst()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if st := <-waiter; !st.HeadsetConnected {
		t.Errorf("waiter got %+v, want the read's status", st)
	}

	panicky := &VRLaunch{Status: func(context.Context, *v1alpha1types.Session, *corev1.Pod) (VRStatus, error) {
		panic("boom")
	}}
	func() {
		defer func() { _ = recover() }()
		_, _ = panicky.readStatus(t.Context(), sess, pod)
	}()
	if _, err := panicky.readStatus(t.Context(), sess, pod); !errors.Is(err, errVRReadPanicked) {
		t.Errorf("after a panicked read: %v, want errVRReadPanicked at once", err)
	}
}

// A User whose last game's pod still terminates is told so before clicking.
func TestVRPageSaysLastGameIsStopping(t *testing.T) {
	f := newVRFixture(t)
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if err := pods.Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sam-old", Namespace: libraryTestNS,
		Labels: map[string]string{vrUserLabel: "sam"}}}); err != nil {
		t.Fatal(err)
	}
	f.s.VRLaunch.SessionPods = generic.NewLister[*corev1.Pod](pods).Namespaced(libraryTestNS)
	if body := f.get("sam").Body.String(); !strings.Contains(body, "still shutting down") || !strings.Contains(body, `content="5"`) {
		t.Errorf("sam:\n%s", body)
	}
	if body := f.get("alex").Body.String(); strings.Contains(body, "still shutting down") {
		t.Errorf("alex:\n%s", body)
	}
}

// Page views read the informers, never the API server: open tabs reload
// every few seconds (QA, XERK-2670).
func TestVRPageViewsReadTheCache(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t)
	pod := readyVRPod(sess)
	nsIndex := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	sessions := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nsIndex)
	pods := cache.NewIndexer(cache.MetaNamespaceKeyFunc, nsIndex)
	if err := sessions.Add(sess); err != nil {
		t.Fatal(err)
	}
	if err := pods.Add(pod); err != nil {
		t.Fatal(err)
	}
	f.s.VRLaunch.SessionCache = generic.NewLister[*v1alpha1types.Session](sessions).Namespaced(libraryTestNS)
	f.s.VRLaunch.SessionPods = generic.NewLister[*corev1.Pod](pods).Namespaced(libraryTestNS)
	f.status = VRStatus{HeadsetConnected: true}
	f.dw.ClearActions()
	f.k8s.ClearActions()
	body := f.get("sam").Body.String()
	if !strings.Contains(body, "21003") || !strings.Contains(body, `content="30"`) {
		t.Errorf("page:\n%s", body)
	}
	if a := append(f.dw.Actions(), f.k8s.Actions()...); len(a) != 0 {
		t.Errorf("page view made API calls: %v", a)
	}
}

// A pod of the Session's name it doesn't control (a predecessor's) is not
// its pod: nothing is read from it.
func TestVRPageIgnoresPodOfAnotherSession(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess)
	pod := readyVRPod(sess)
	pod.OwnerReferences[0].UID = "old"
	f.addPod(t, pod)
	f.status = VRStatus{HeadsetConnected: true}
	if body := f.get("sam").Body.String(); !strings.Contains(body, "Starting") || strings.Contains(body, "21003") {
		t.Errorf("page:\n%s", body)
	}
}

// The page needs the headsets host's proxy, a named User, and for a POST the
// page's own clicked form; it is never served on the Library's host.
func TestVRPageRefuses(t *testing.T) {
	f := newVRFixture(t)
	if rec := f.get(""); rec.Code != http.StatusForbidden {
		t.Errorf("no user: HTTP %d", rec.Code)
	}
	if rec := f.get("nobody"); rec.Code != http.StatusForbidden {
		t.Errorf("unknown user: HTTP %d", rec.Code)
	}
	for name, h := range map[string]http.Header{
		"none":      nil,
		"same-site": {"Sec-Fetch-Site": {"same-site"}, "Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-User": {"?1"}},
		"fetch":     {"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-User": {"?1"}},
		"no user":   {"Sec-Fetch-Site": {"same-origin"}, "Sec-Fetch-Mode": {"navigate"}},
	} {
		if rec := f.post("sam", url.Values{"action": {"play"}, "app": {"ac"}}, h); rec.Code != http.StatusForbidden {
			t.Errorf("%s: HTTP %d", name, rec.Code)
		}
	}
	if n := len(f.sessions(t)); n != 0 {
		t.Errorf("%d sessions created", n)
	}
	// The Library's host: its own secret, no page.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := serveLibraryRequest(f.s, method, libraryVRPath, "10.1.0.5:5555", http.Header{DefaultLibraryUserHeader: {"sam"}})
		if rec.Code != http.StatusNotFound {
			t.Errorf("Library host %s: HTTP %d", method, rec.Code)
		}
	}
	// Off: the headsets host serves no VR page.
	f.s.VRLaunch = nil
	if rec := f.get("sam"); rec.Code != http.StatusNotFound {
		t.Errorf("off: HTTP %d", rec.Code)
	}
}

// waiting is how many vr-agent reads are in flight.
func (v *VRLaunch) waiting() int {
	v.statusMu.Lock()
	defer v.statusMu.Unlock()
	n := 0
	for _, c := range v.statuses {
		if c.done != nil {
			n++
		}
	}
	return n
}

// The front door's view of a User's VR Session (XERK-2808): ready only once
// the pod is ready and the server's key is known, as the headset pins it.
func TestVRSessionStateForFrontDoor(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess, moonlightSession("alex-flat-1", "alex"))
	user := func(name string) *v1alpha1types.User {
		return &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}

	st, err := f.s.VRLaunch.SessionState(t.Context(), user("sam"))
	if err != nil || st.State != VRStateStarting || st.Host != "" || st.App != "ac" {
		t.Fatalf("no pod: %+v %v", st, err)
	}
	f.addPod(t, readyVRPod(sess))
	if st, _ = f.s.VRLaunch.SessionState(t.Context(), user("sam")); st.State != VRStateStarting || st.Port != 0 {
		t.Errorf("ready pod, no server key: %+v", st)
	}
	key := strings.Repeat("ab", 56)
	sess.Status.VRServerKey = key
	if _, err := f.dw.DirewolfV1alpha1().Sessions(libraryTestNS).UpdateStatus(t.Context(), sess, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ = f.s.VRLaunch.SessionState(t.Context(), user("sam"))
	if st.State != VRStateReady || st.Host != "10.10.10.34" || st.Port != 21003 || st.ServerKey != key {
		t.Errorf("ready: %+v", st)
	}
	if st, _ = f.s.VRLaunch.SessionState(t.Context(), user("alex")); st.State != VRStateOther || st.Message == "" {
		t.Errorf("moonlight session: %+v", st)
	}
	if st, _ = f.s.VRLaunch.SessionState(t.Context(), user("dana")); st.State != VRStateNone {
		t.Errorf("no session: %+v", st)
	}
}

// testHeadsetsSecret is what the headsets host's proxy sends.
const testHeadsetsSecret = "headsets-0123456789abcdef0123456789"

// headsetsRequest is a request through the headsets host's proxy.
func headsetsRequest(s *LibraryServer, method, target string, header http.Header) *httptest.ResponseRecorder {
	h := http.Header{LibraryProxySecretHeader: {testHeadsetsSecret}}
	maps.Copy(h, header)
	return serveLibraryRequest(s, method, target, "10.1.0.5:5555", h)
}

// The headsets host and the Library never share an origin (XERK-2750): the
// headsets host serves only the VR page, and nothing of the Library; the
// confirm page it served before XERK-2811 is gone.
func TestHeadsetsHostServesOnlyTheVRPage(t *testing.T) {
	f := newVRFixture(t)
	sam := http.Header{DefaultLibraryUserHeader: {"sam"}}
	if rec := headsetsRequest(f.s, http.MethodGet, "/", sam); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != libraryVRPath {
		t.Errorf("headsets host /: HTTP %d to %q, want a redirect to the VR page", rec.Code, rec.Header().Get("Location"))
	}
	visit := http.Header{DefaultLibraryUserHeader: {"sam"}, "Sec-Fetch-User": {"?1"}, "Sec-Fetch-Mode": {"navigate"}}
	for _, path := range []string{"/.direwolf/headsets", "/devmode/", "/api", libraryStartPath} {
		if rec := headsetsRequest(f.s, http.MethodGet, path, visit); rec.Code != http.StatusNotFound {
			t.Errorf("headsets host %s: HTTP %d, want 404", path, rec.Code)
		}
	}
	if rec := headsetsRequest(f.s, http.MethodPost, libraryStartPath, sam); rec.Code != http.StatusNotFound {
		t.Errorf("headsets host start: HTTP %d, want 404", rec.Code)
	}
}

// A VR game started from Moonlight is the headset's to join (XERK-2811): the
// front door reports it starting until its App pod is Ready and its server
// key recorded, then ready with that pod's address and key, and a launch of
// the same game joins it rather than refusing. Another game is refused, and
// the headset never stops it.
func TestVRFrontDoorJoinsMoonlightVRSession(t *testing.T) {
	sess := moonlightSession("sam-ac-1", "sam")
	sess.UID = "sam-ac-1"
	sess.Spec.GameReference.Name = "ac"
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess)
	sam := &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "sam"}}

	st, err := f.s.VRLaunch.SessionState(t.Context(), sam)
	if err != nil || st.State != VRStateStarting || st.App != "ac" || st.Host != "" || st.Message == "" {
		t.Fatalf("no pods: %+v %v", st, err)
	}
	if refusal, err := f.s.VRLaunch.Launch(t.Context(), sam, "ac"); err != nil || refusal != "" {
		t.Errorf("launch of the same game: %q %v, want it joined", refusal, err)
	}
	if refusal, _ := f.s.VRLaunch.Launch(t.Context(), sam, "beat"); refusal != vrMoonlightMessage {
		t.Errorf("launch of another game: %q, want %q", refusal, vrMoonlightMessage)
	}
	if n := len(f.sessions(t)); n != 1 {
		t.Fatalf("%d sessions, want only the Moonlight one", n)
	}

	wolf := readyVRPod(sess)
	wolf.Annotations = map[string]string{appPodAnnotation: sess.Name + "-app"}
	wolf.Status.HostIP = "10.10.10.99" // never the address: the App pod serves VR
	app := readyVRPod(sess)
	app.Name = sess.Name + "-app"
	f.addPod(t, wolf)
	f.addPod(t, app)
	if st, _ = f.s.VRLaunch.SessionState(t.Context(), sam); st.State != VRStateStarting || st.Port != 0 {
		t.Errorf("ready pods, no server key: %+v", st)
	}
	key := strings.Repeat("ab", 56)
	sess.Status.VRServerKey = key
	if _, err := f.dw.DirewolfV1alpha1().Sessions(libraryTestNS).UpdateStatus(t.Context(), sess, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, _ = f.s.VRLaunch.SessionState(t.Context(), sam)
	if st.State != VRStateReady || st.Host != "10.10.10.34" || st.Port != 21003 || st.ServerKey != key {
		t.Errorf("ready: %+v", st)
	}
	// Only a Ready App pod the Session controls is served, never one that
	// is not ready or is another Session's.
	for name, edit := range map[string]func(*corev1.Pod){
		"not ready":       func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionFalse },
		"no host IP":      func(p *corev1.Pod) { p.Status.HostIP = "" },
		"another session": func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"being deleted":   func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} },
	} {
		bad := app.DeepCopy()
		edit(bad)
		if _, err := f.k8s.CoreV1().Pods(libraryTestNS).Update(t.Context(), bad, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if st, _ := f.s.VRLaunch.SessionState(t.Context(), sam); st.State != VRStateStarting || st.Host != "" || st.ServerKey != "" {
			t.Errorf("%s: %+v, want starting with no address", name, st)
		}
	}
	if _, err := f.k8s.CoreV1().Pods(libraryTestNS).Update(t.Context(), app, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := f.s.VRLaunch.Stop(t.Context(), sam); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sessions(t)); n != 1 {
		t.Errorf("headset stop ended the Moonlight session")
	}
	// Being quit in Moonlight: stopping.
	sess.DeletionTimestamp, sess.Finalizers = &metav1.Time{Time: time.Now()}, []string{"test/hold"}
	if _, err := f.dw.DirewolfV1alpha1().Sessions(libraryTestNS).Update(t.Context(), sess, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.s.VRLaunch.SessionState(t.Context(), sam); st.State != VRStateStopping || st.Host != "" {
		t.Errorf("deleting: %+v, want stopping", st)
	}
}

// A Moonlight session of a flat game stays "other".
func TestVRFrontDoorLeavesFlatMoonlightSession(t *testing.T) {
	sess := moonlightSession("sam-flat-1", "sam")
	sess.Spec.GameReference.Name = "flat"
	f := newVRFixture(t, sess)
	sam := &v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "sam"}}
	if st, _ := f.s.VRLaunch.SessionState(t.Context(), sam); st.State != VRStateOther {
		t.Errorf("flat Moonlight session: %+v", st)
	}
	if refusal, _ := f.s.VRLaunch.Launch(t.Context(), sam, "ac"); refusal != vrMoonlightMessage {
		t.Errorf("launch: %q", refusal)
	}
}
