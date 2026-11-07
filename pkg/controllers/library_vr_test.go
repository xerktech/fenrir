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
	s.HeadsetConfirmation = &HeadsetConfirmation{
		Sessions: generic.NewLister[*v1alpha1types.Session](cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})).Namespaced(libraryTestNS),
		Headsets: f.dw.DirewolfV1alpha1().Headsets(libraryTestNS),
		Secret:   []byte(testHeadsetsSecret),
	}
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

// Once ready the page shows the node IP, the session's VR port and, while
// WiVRn pairs, its PIN; a PIN that is not six digits is never shown.
func TestVRPageShowsAddressAndPin(t *testing.T) {
	sess := pageVRSession("sam-ac-1", "sam")
	sess.Status.Ports.VR = 21003
	f := newVRFixture(t, sess)
	f.addPod(t, readyVRPod(sess))
	f.status = VRStatus{Pin: "042137"}
	body := f.get("sam").Body.String()
	for _, want := range []string{"10.10.10.34", "21003", "042137", "Stop the VR session", `http-equiv="refresh"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	for _, pin := range []string{"<b>1</b>", "1234567"} {
		f.status = VRStatus{Pin: pin}
		if body := f.get("sam").Body.String(); strings.Contains(body, "PIN") {
			t.Errorf("PIN %q shown:\n%s", pin, body)
		}
	}
	// An unreachable agent still shows the address.
	f.status, f.statusErr = VRStatus{}, errors.New("down")
	if body := f.get("sam").Body.String(); !strings.Contains(body, "21003") || strings.Contains(body, "PIN") {
		t.Errorf("agent down:\n%s", body)
	}
	// Connected: no refresh, no PIN.
	f.status, f.statusErr = VRStatus{HeadsetConnected: true}, nil
	if body := f.get("sam").Body.String(); !strings.Contains(body, "headset is connected") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("connected:\n%s", body)
	}
	// Another user sees none of it.
	if body := f.get("alex").Body.String(); strings.Contains(body, "21003") || strings.Contains(body, "042137") {
		t.Errorf("alex sees sam's session:\n%s", body)
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
	f.status = VRStatus{Pin: "042137"}
	if body := f.get("sam").Body.String(); !strings.Contains(body, "Starting") || strings.Contains(body, "042137") {
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
