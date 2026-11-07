package controllers

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// The VR page (XERK-2670) starts and stops a User's native VR Session and
// shows the address the headset dials and, while it pairs, WiVRn's PIN. It is
// served on the headsets page's own host, never the Library's: the Library
// host proxies the desktop, whose code can serve same-origin pages there
// (XERK-2750) that would read the PIN or start sessions.
const (
	libraryVRPath = "/.direwolf/vr"
	// libraryVRFormMax bounds the page's form: an action and an App name.
	libraryVRFormMax = 4096
	// libraryVRCallTimeout bounds one page request's API calls.
	libraryVRCallTimeout = 10 * time.Second
	// libraryVRStatusTimeout bounds the vr-agent read for the PIN: the page
	// is shown without it rather than held.
	libraryVRStatusTimeout = 3 * time.Second
	// libraryVRRefresh is how often a page waiting on the session reloads.
	libraryVRRefresh = 5
)

// VRLaunch turns on the VR page, on the headsets page's host.
type VRLaunch struct {
	Apps generic.NamespacedLister[*v1alpha1types.App]
	// Sessions and Pods are read live: a Session or Library pod created a
	// moment ago, maybe by another replica or moonlight-proxy, must count.
	Sessions v1alpha1client.SessionInterface
	Pods     corev1client.PodInterface
	// Status reads a ready VR pod's vr-agent (SessionController.ReadVRStatus).
	Status func(ctx context.Context, session *v1alpha1types.Session, pod *corev1.Pod) (VRStatus, error)
	// MaxConcurrentSessions caps how many users may play at once, as
	// moonlight-proxy's flag of that name does (it counts VR Sessions too);
	// keep them equal. 0 or less is no limit.
	MaxConcurrentSessions int

	// launchMu serializes this replica's checks and creates.
	launchMu sync.Mutex
}

// Messages the page shows when it refuses to start a session.
const (
	vrLibraryOpenMessage     = "Your game library is open. Close it (or wait for it to stop) before playing in VR: Steam can only run in one place at a time."
	vrLibraryStoppingMessage = "Your game library is closing. Try again in a minute or two."
	vrBusyMessage            = "Someone else is playing right now. Try again later."
	vrMoonlightMessage       = "You have a game running in Moonlight. Quit it there (or wait for it to end) before playing in VR."
	vrStoppingMessage        = "Your last VR session is stopping. Try again in a moment."
	vrNoAppMessage           = "That game can't be played in VR here."
	vrTryAgainMessage        = "Try again in a minute."
)

// serveVR shows the signed-in User's VR session or the games they can play in
// VR (GET), or starts or stops their session (POST). ServeHTTP has checked
// the peer and the headsets host's secret, so userHeader is the proxy's.
func (s *LibraryServer) serveVR(w http.ResponseWriter, r *http.Request) {
	v := s.VRLaunch
	if v == nil {
		http.NotFound(w, r)
		return
	}
	user, ok := s.headsetsHostUser(w, r, "Sign in to play in VR.")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), libraryVRCallTimeout)
	defer cancel()

	switch r.Method {
	case http.MethodGet:
		v.showPage(ctx, w, user, http.StatusOK, "")
	case http.MethodPost:
		// The page's own form, clicked: as the headsets page requires.
		if !userSubmittedForm(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, libraryVRFormMax)
		var refusal string
		var err error
		switch r.PostFormValue("action") {
		case "play":
			refusal, err = v.launch(ctx, user, r.PostFormValue("app"))
		case "stop":
			err = v.stop(ctx, user)
		default:
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err != nil {
			klog.Errorf("VR page: user %s: %v", user.Name, err)
			writeLibraryPage(w, http.StatusInternalServerError, vrTryAgainMessage, false, false)
			return
		}
		if refusal != "" {
			v.showPage(ctx, w, user, http.StatusConflict, refusal)
			return
		}
		http.Redirect(w, r, libraryVRPath, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// headsetsHostUser is the User the headsets host's proxy names, or false
// once it has answered the request itself.
func (s *LibraryServer) headsetsHostUser(w http.ResponseWriter, r *http.Request, signIn string) (*v1alpha1types.User, bool) {
	name := r.Header.Get(s.userHeader)
	if name == "" || s.users == nil {
		writeLibraryPage(w, http.StatusForbidden, signIn, false, false)
		return nil, false
	}
	user, err := v1alpha1types.LookupUser(s.users.Get, name)
	if apierrors.IsNotFound(err) {
		writeLibraryPage(w, http.StatusForbidden, "You have no streaming account here. Ask the admin to add one.", false, false)
		return nil, false
	} else if err != nil {
		klog.Errorf("Headsets host: getting user %s: %v", name, err)
		writeLibraryPage(w, http.StatusInternalServerError, vrTryAgainMessage, false, false)
		return nil, false
	}
	return user, true
}

// userSubmittedForm is whether r is a page's own form, submitted by the
// user: same-origin (the Library is on a sibling host, so "same-site" is the
// desktop's code, XERK-2750), a navigation (never a script's fetch),
// user-activated (QA, XERK-2747). Browsers set these headers; a script
// cannot. Browsers that send none are refused too.
func userSubmittedForm(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") == "same-origin" && r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-User") == "?1"
}

// userSessions is user's Sessions, read live.
func (v *VRLaunch) userSessions(ctx context.Context, user string) ([]v1alpha1types.Session, error) {
	list, err := v.Sessions.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	var res []v1alpha1types.Session
	for _, s := range list.Items {
		if s.Spec.UserReference.Name == user {
			res = append(res, s)
		}
	}
	return res, nil
}

// libraryRefusal is why user's Library keeps a VR session from starting, or
// "": the Library and its library's game sessions share one Steam home
// (.claude/rules/library.md). A terminating Library pod still runs Steam.
func (v *VRLaunch) libraryRefusal(ctx context.Context, user *v1alpha1types.User) (string, error) {
	pod, err := v.Pods.Get(ctx, v1alpha1types.LibraryPodNameFor(user.LibraryOwner()), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("getting the Library pod: %w", err)
	case pod.DeletionTimestamp != nil:
		return vrLibraryStoppingMessage, nil
	}
	return vrLibraryOpenMessage, nil
}

// vrApp is appName if user may play it in VR here.
func (v *VRLaunch) vrApp(user *v1alpha1types.User, appName string) (*v1alpha1types.App, bool) {
	if appName == "" {
		return nil, false
	}
	app, err := v.Apps.Get(appName)
	if err != nil || !app.Spec.VRPort || app.Spec.Hidden || !v1alpha1types.AppVisibleTo(app, user.LibraryOwner()) {
		return nil, false
	}
	return app, true
}

// launch starts user's VR Session of appName, or returns why not. Its checks
// mirror moonlight-proxy's /launch: the Library's Steam lock before and after
// the create (whichever side looks second sees the other), and the session
// limit. Unlike /launch it never replaces a running session: that would end a
// Moonlight game the user didn't quit.
func (v *VRLaunch) launch(ctx context.Context, user *v1alpha1types.User, appName string) (string, error) {
	app, ok := v.vrApp(user, appName)
	if !ok {
		return vrNoAppMessage, nil
	}
	v.launchMu.Lock()
	defer v.launchMu.Unlock()

	own, err := v.userSessions(ctx, user.Name)
	if err != nil {
		return "", err
	}
	for _, s := range own {
		switch {
		case !s.Spec.IsVR():
			return vrMoonlightMessage, nil
		case s.DeletionTimestamp != nil:
			return vrStoppingMessage, nil
		default:
			// A second click, or another tab: the page shows it.
			return "", nil
		}
	}
	if reason, refErr := v.libraryRefusal(ctx, user); refErr != nil || reason != "" {
		return reason, refErr
	}
	if v.MaxConcurrentSessions > 0 {
		all, listErr := v.Sessions.List(ctx, metav1.ListOptions{})
		if listErr != nil {
			return "", fmt.Errorf("listing sessions: %w", listErr)
		}
		others := map[string]struct{}{}
		for _, s := range all.Items {
			if s.Spec.UserReference.Name != user.Name {
				others[s.Spec.UserReference.Name] = struct{}{}
			}
		}
		if len(others) >= v.MaxConcurrentSessions {
			return vrBusyMessage, nil
		}
	}

	session, err := v.Sessions.Create(ctx, &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", user.Name, app.Name),
			Labels: map[string]string{
				"direwolf":      strconv.FormatBool(true),
				"direwolf/app":  app.Name,
				"direwolf/user": user.Name,
			},
		},
		Spec: v1alpha1types.SessionSpec{
			Mode:          v1alpha1types.SessionModeVR,
			UserReference: v1alpha1types.UserReference{Name: user.Name},
			GameReference: v1alpha1types.GameReference{Name: app.Name},
		},
	}, metav1.CreateOptions{FieldManager: "direwolf-library"})
	if err != nil {
		return "", fmt.Errorf("creating the VR session: %w", err)
	}

	// The Library may have started between the check and the Create.
	reason, err := v.libraryRefusal(ctx, user)
	if err != nil || reason != "" {
		uid := session.UID
		delErr := v.Sessions.Delete(context.WithoutCancel(ctx), session.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			return "", fmt.Errorf("backing out VR session %s: %w", session.Name, delErr)
		}
		return reason, err
	}
	klog.Infof("VR page: user %s started VR session %s of app %s", user.Name, session.Name, app.Name)
	return "", nil
}

// stop ends user's VR Sessions; their Moonlight ones are Moonlight's to quit.
func (v *VRLaunch) stop(ctx context.Context, user *v1alpha1types.User) error {
	own, err := v.userSessions(ctx, user.Name)
	if err != nil {
		return err
	}
	for _, s := range own {
		if !s.Spec.IsVR() || s.DeletionTimestamp != nil {
			continue
		}
		uid := s.UID
		err := v.Sessions.Delete(ctx, s.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting VR session %s: %w", s.Name, err)
		}
		klog.Infof("VR page: user %s stopped VR session %s", user.Name, s.Name)
	}
	return nil
}

// vrView is what the page shows of a VR Session.
type vrView struct {
	app      string
	stopping bool
	ready    bool
	host     string
	port     int32
	pin      string
	headset  bool
	pending  int
}

func (v *VRLaunch) view(ctx context.Context, s *v1alpha1types.Session) vrView {
	view := vrView{app: s.Spec.GameReference.Name, stopping: s.DeletionTimestamp != nil, pending: len(s.Status.PendingHeadsets)}
	if app, err := v.Apps.Get(view.app); err == nil && app.Spec.Title != "" {
		view.app = app.Spec.Title
	}
	view.headset = meta.IsStatusConditionTrue(s.Status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition)
	if view.stopping {
		return view
	}
	pod, err := v.Pods.Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Warningf("VR page: getting pod %s: %v", s.Name, err)
		}
		return view
	}
	// The pod's own Session only: a pod of that name from before is not it.
	if !metav1.IsControlledBy(pod, s) || pod.DeletionTimestamp != nil || !podReady(pod) ||
		pod.Status.HostIP == "" || s.Status.Ports.VR == 0 {
		return view
	}
	view.ready, view.host, view.port = true, pod.Status.HostIP, s.Status.Ports.VR
	if v.Status != nil {
		sctx, cancel := context.WithTimeout(ctx, libraryVRStatusTimeout)
		defer cancel()
		st, err := v.Status(sctx, s, pod)
		switch {
		case err != nil:
			klog.V(2).Infof("VR page: reading session %s's VR server: %v", s.Name, err)
		case IsWiVRnPin(st.Pin):
			view.pin = st.Pin
		}
		if err == nil {
			view.headset = st.HeadsetConnected
		}
	}
	return view
}

// showPage writes the page for user, with notice (a refusal) above it.
func (v *VRLaunch) showPage(ctx context.Context, w http.ResponseWriter, user *v1alpha1types.User, status int, notice string) {
	own, err := v.userSessions(ctx, user.Name)
	if err != nil {
		klog.Errorf("VR page: user %s: %v", user.Name, err)
		writeLibraryPage(w, http.StatusInternalServerError, vrTryAgainMessage, false, false)
		return
	}
	var b strings.Builder
	if notice != "" {
		fmt.Fprintf(&b, `<p class="notice">%s</p>`, html.EscapeString(notice))
	}
	refresh := false
	idx := slices.IndexFunc(own, func(s v1alpha1types.Session) bool { return s.Spec.IsVR() })
	switch {
	case idx >= 0:
		view := v.view(ctx, &own[idx])
		refresh = writeVRSession(&b, &view)
	case len(own) > 0:
		b.WriteString(`<p>` + html.EscapeString(vrMoonlightMessage) + `</p>`)
	default:
		writeVRApps(&b, v.apps(user))
	}
	writeVRPage(w, status, b.String(), refresh)
}

// apps is the Apps user may play in VR, by title.
func (v *VRLaunch) apps(user *v1alpha1types.User) []*v1alpha1types.App {
	all, err := v.Apps.List(labels.Everything())
	if err != nil {
		klog.Errorf("VR page: listing apps: %v", err)
		return nil
	}
	var res []*v1alpha1types.App
	for _, a := range all {
		if _, ok := v.vrApp(user, a.Name); ok {
			res = append(res, a)
		}
	}
	slices.SortFunc(res, func(a, b *v1alpha1types.App) int { return strings.Compare(appTitle(a), appTitle(b)) })
	return res
}

func appTitle(a *v1alpha1types.App) string {
	if a.Spec.Title != "" {
		return a.Spec.Title
	}
	return a.Name
}

func writeVRApps(b *strings.Builder, apps []*v1alpha1types.App) {
	if len(apps) == 0 {
		b.WriteString(`<p>No game can be played in VR here yet.</p>`)
		return
	}
	b.WriteString(`<p>Pick a game. Then open the WiVRn app on your headset and connect to the address shown here.</p>`)
	for _, a := range apps {
		fmt.Fprintf(b, `<form method="post" action="%s"><input type="hidden" name="action" value="play"><input type="hidden" name="app" value="%s"><button type="submit">Play %s in VR</button></form>`,
			libraryVRPath, html.EscapeString(a.Name), html.EscapeString(appTitle(a)))
	}
}

// writeVRSession describes view and reports whether the page should reload
// to follow it.
func writeVRSession(b *strings.Builder, view *vrView) bool {
	title := html.EscapeString(view.app)
	stop := `<form method="post" action="` + libraryVRPath + `"><input type="hidden" name="action" value="stop"><button type="submit">Stop the VR session</button></form>`
	switch {
	case view.stopping:
		fmt.Fprintf(b, `<p>Stopping %s…</p>`, title)
		return true
	case !view.ready:
		fmt.Fprintf(b, `<p>Starting %s… The first start can take several minutes.</p>%s`, title, stop)
		return true
	}
	addr := html.EscapeString(view.host)
	port := strconv.Itoa(int(view.port))
	if view.headset {
		fmt.Fprintf(b, `<p>%s is running. Your headset is connected.</p>`, title)
	} else {
		fmt.Fprintf(b, `<p>%s is ready. In the WiVRn app on your headset, add a server by address and connect:</p>`, title)
	}
	fmt.Fprintf(b, `<dl><dt>Address</dt><dd class="big">%s</dd><dt>Port</dt><dd class="big">%s</dd></dl>`, addr, port)
	if !view.headset {
		b.WriteString(`<p><small>The server isn't announced on the network, so it won't appear in the headset's list by itself: enter it by hand (` +
			html.EscapeString(net.JoinHostPort(view.host, port)) + `).</small></p>`)
	}
	if view.pin != "" {
		fmt.Fprintf(b, `<p>When the headset asks for a PIN, enter</p><p class="big">%s</p>`, html.EscapeString(view.pin))
	}
	if view.pending > 0 {
		fmt.Fprintf(b, `<p>Paired a headset by PIN? <a href="%s">Confirm it</a> so it connects without a PIN next time.</p>`, libraryHeadsetsPath)
	}
	b.WriteString(`<p><small>With no headset connected the session stops by itself after a while.</small></p>`)
	b.WriteString(stop)
	return !view.headset
}

func writeVRPage(w http.ResponseWriter, status int, body string, refresh bool) {
	meta := ""
	if refresh {
		meta = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, libraryVRRefresh)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">%s<title>Play in VR</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0;padding:0 16px}p{max-width:32em;text-align:center;font-size:1.2em}button{font-size:1.1em;padding:.6em 1.4em}main{display:grid;justify-items:center;gap:1em}small{color:#aaa}a{color:#9cf}.big{font-size:2em;font-family:ui-monospace,monospace;margin:0}dl{display:grid;grid-template-columns:auto auto;gap:.3em 1em;align-items:baseline}dt{color:#aaa}dd{margin:0}.notice{color:#fc6}</style>
</head><body><main><h1>Play in VR</h1>%s<p><small><a href="%s">Your headsets</a></small></p></main></body></html>
`, meta, body, libraryHeadsetsPath)
}
