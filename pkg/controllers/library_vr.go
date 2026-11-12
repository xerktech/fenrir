package controllers

import (
	"context"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// The VR page (XERK-2670) starts and stops a User's native VR Session and
// shows the address the headset dials. It is served on its own host (the
// headsets host), never the Library's: the Library host proxies the desktop,
// whose code can serve same-origin pages there (XERK-2750) that would start
// sessions.
const (
	libraryVRPath = "/.direwolf/vr"
	// libraryVRFormMax bounds the page's form: an action and an App name.
	libraryVRFormMax = 4096
	// libraryVRCallTimeout bounds one page request's API calls.
	libraryVRCallTimeout = 10 * time.Second
	// libraryVRStatusTimeout bounds the vr-agent read: the page is shown
	// without it rather than held.
	libraryVRStatusTimeout = 3 * time.Second
	// libraryVRRefresh is how often a page waiting on the session reloads;
	// libraryVRIdleRefresh once a headset is connected, to notice it leave.
	libraryVRRefresh     = 5
	libraryVRIdleRefresh = 30
	// libraryVRStatusTTL is how long a vr-agent read is reused: each costs
	// a Secret GET on the operator's shared client, and open tabs refresh.
	libraryVRStatusTTL = 3 * time.Second
)

// vrUserLabel names the User on Sessions and every pod built for one.
const vrUserLabel = "direwolf/user"

// VRLaunch turns on the VR page, on the headsets host.
type VRLaunch struct {
	Apps generic.NamespacedLister[*v1alpha1types.App]
	// Headsets, when set, refuses a start for a User with none: their
	// Session would only end (XERK-2811). Headsets pair through the front
	// door, never in a session.
	Headsets generic.NamespacedLister[*v1alpha1types.Headset]
	// Sessions and Pods are read live when starting a session: a Session or
	// pod created a moment ago, maybe by another replica or moonlight-proxy,
	// must count.
	Sessions v1alpha1client.SessionInterface
	Pods     corev1client.PodInterface
	// SessionCache and SessionPods (the session pod informer) serve page
	// views: those refresh in open tabs, and live reads would spend the
	// operator's shared API rate limit, which the session controller needs.
	SessionCache generic.NamespacedLister[*v1alpha1types.Session]
	SessionPods  generic.NamespacedLister[*corev1.Pod]
	// Status reads a ready VR pod's vr-agent (SessionController.ReadVRStatus).
	Status func(ctx context.Context, session *v1alpha1types.Session, pod *corev1.Pod) (VRStatus, error)
	// MaxConcurrentSessions caps how many users may play at once, as
	// moonlight-proxy's flag of that name does (it counts VR Sessions too);
	// keep them equal. 0 or less is no limit.
	MaxConcurrentSessions int

	// launchMu serializes this replica's checks and creates.
	launchMu sync.Mutex

	statusMu sync.Mutex
	// statuses is the last vr-agent read per Session UID.
	statuses map[types.UID]*cachedVRStatus
}

// errVRReadPanicked is what waiters get when the read they waited on panicked.
var errVRReadPanicked = errors.New("reading the VR server failed")

// cachedVRStatus is one vr-agent read; done is open while it runs.
type cachedVRStatus struct {
	at     time.Time
	status VRStatus
	err    error
	done   chan struct{}
}

// Messages the page shows when it refuses to start a session.
const (
	vrLibraryOpenMessage     = "Your game library is open. Close it (or wait for it to stop) before playing in VR: Steam can only run in one place at a time."
	vrLibraryStoppingMessage = "Your game library is closing. Try again in a minute or two."
	vrBusyMessage            = "Someone else is playing right now. Try again later."
	vrMoonlightMessage       = "You have a game running in Moonlight. Quit it there (or wait for it to end) before playing in VR."
	vrStoppingMessage        = "Your last VR session is stopping. Try again in a moment."
	vrGameStoppingMessage    = "Your last game is still shutting down. Try again in a moment."
	vrOtherAppMessage        = "Your VR session of another game is running. Stop it first to play this one."
	vrNoAppMessage           = "That game can't be played in VR here."
	vrNoHeadsetMessage       = "Pair your headset first: in the WiVRn app on your headset, open fenrir, add this server and enter the code it shows on the pairing page."
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
			refusal, err = v.Launch(ctx, user, r.PostFormValue("app"))
		case "stop":
			err = v.Stop(ctx, user)
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

// cachedUserSessions is user's Sessions from the informer, for page views.
func (v *VRLaunch) cachedUserSessions(ctx context.Context, user string) ([]v1alpha1types.Session, error) {
	if v.SessionCache == nil {
		return v.userSessions(ctx, user)
	}
	all, err := v.SessionCache.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	var res []v1alpha1types.Session
	for _, s := range all {
		if s.Spec.UserReference.Name == user {
			res = append(res, *s)
		}
	}
	slices.SortFunc(res, func(a, b v1alpha1types.Session) int { return strings.Compare(a.Name, b.Name) })
	return res, nil
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

// Launch starts user's VR Session of appName, or returns why not. Its checks
// mirror moonlight-proxy's /launch: the Library's Steam lock before and after
// the create (whichever side looks second sees the other), and the session
// limit. Unlike /launch it never replaces a running session: that would end a
// Moonlight game the user didn't quit.
func (v *VRLaunch) Launch(ctx context.Context, user *v1alpha1types.User, appName string) (string, error) {
	app, ok := v.vrApp(user, appName)
	if !ok {
		return vrNoAppMessage, nil
	}
	if v.Headsets != nil {
		all, err := v.Headsets.List(labels.Everything())
		if err != nil {
			return "", fmt.Errorf("listing headsets: %w", err)
		}
		if !slices.ContainsFunc(all, func(h *v1alpha1types.Headset) bool {
			return h.Spec.UserReference.Name == user.Name && h.DeletionTimestamp == nil
		}) {
			return vrNoHeadsetMessage, nil
		}
	}
	v.launchMu.Lock()
	defer v.launchMu.Unlock()

	own, err := v.userSessions(ctx, user.Name)
	if err != nil {
		return "", err
	}
	for _, s := range own {
		switch {
		case !s.Spec.IsVR() && s.DeletionTimestamp == nil && s.Spec.GameReference.Name == app.Name:
			// The same VR game started from Moonlight: the headset joins
			// it (SessionState), as it can pair only through the front
			// door (XERK-2811).
			return "", nil
		case !s.Spec.IsVR():
			return vrMoonlightMessage, nil
		case s.DeletionTimestamp != nil:
			return vrStoppingMessage, nil
		case s.Spec.GameReference.Name != app.Name:
			return vrOtherAppMessage, nil
		default:
			// A second click, or another tab: the page shows it.
			return "", nil
		}
	}
	// A deleted Session goes at once while its pods still terminate, and
	// still run Steam in the shared home: as the Library's gameRunning does,
	// any pod built for user's sessions holds the lock (the split App pod
	// carries only direwolf/user).
	if busy, podErr := v.userPodRunning(ctx, user.Name); podErr != nil || busy {
		if podErr != nil {
			return "", podErr
		}
		return vrGameStoppingMessage, nil
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
				"direwolf":     strconv.FormatBool(true),
				"direwolf/app": app.Name,
				vrUserLabel:    user.Name,
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
	klog.Infof("VR launch: user %s started VR session %s of app %s", user.Name, session.Name, app.Name)
	return "", nil
}

// userPodRunning is whether a pod of user's sessions exists, terminating or
// not. Read live.
func (v *VRLaunch) userPodRunning(ctx context.Context, user string) (bool, error) {
	pods, err := v.Pods.List(ctx, metav1.ListOptions{LabelSelector: vrUserLabel})
	if err != nil {
		return false, fmt.Errorf("listing session pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels[vrUserLabel] == user && p.Labels[v1alpha1types.LibraryPodLabel] == "" {
			return true, nil
		}
	}
	return false, nil
}

// cachedUserPodRunning is userPodRunning from the session pod informer, for
// page views: it sees session pods, not a split session's App pod.
func (v *VRLaunch) cachedUserPodRunning(user string) bool {
	if v.SessionPods == nil {
		return false
	}
	pods, err := v.SessionPods.List(labels.SelectorFromSet(labels.Set{vrUserLabel: user}))
	return err == nil && len(pods) > 0
}

// Stop ends user's VR Sessions; their Moonlight ones are Moonlight's to quit.
func (v *VRLaunch) Stop(ctx context.Context, user *v1alpha1types.User) error {
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
		klog.Infof("VR launch: user %s stopped VR session %s", user.Name, s.Name)
	}
	return nil
}

// VRSessionState is a User's VR Session as the VR front door reports it to
// their headset (XERK-2808), read from the informers.
type VRSessionState struct {
	// State is VRStateNone, VRStateStarting, VRStateReady, VRStateStopping
	// or VRStateOther.
	State string
	// App names the Session's App; Title is its title.
	App, Title string
	// Host and Port are where the headset connects, and ServerKey the VR
	// server's identity key it pins (SessionStatus.VRServerKey): set only
	// once State is VRStateReady.
	Host      string
	Port      int32
	ServerKey string
	// HeadsetConnected is whether a headset is connected to it.
	HeadsetConnected bool
	// Message says why the headset can't play now, for VRStateOther and a
	// VRStateNone whose last session's pods still stop.
	Message string
}

// VRSessionState.State values.
const (
	VRStateNone     = "none"
	VRStateStarting = "starting"
	VRStateReady    = "ready"
	VRStateStopping = "stopping"
	// VRStateOther: the User has a Moonlight Session, which a VR launch
	// never replaces.
	VRStateOther = "other"
)

// SessionState is user's VR Session as the VR front door reports it. Ready
// needs the VR server's identity key too: the headset pins it, and a pod
// that reports none (an image from before XERK-2808) stays starting.
func (v *VRLaunch) SessionState(ctx context.Context, user *v1alpha1types.User) (VRSessionState, error) {
	own, err := v.cachedUserSessions(ctx, user.Name)
	if err != nil {
		return VRSessionState{}, err
	}
	idx := slices.IndexFunc(own, func(s v1alpha1types.Session) bool { return s.Spec.IsVR() })
	switch {
	case idx < 0 && len(own) > 0:
		for i := range own {
			if st, ok := v.moonlightVRState(ctx, &own[i]); ok {
				return st, nil
			}
		}
		return VRSessionState{State: VRStateOther, Message: vrMoonlightMessage}, nil
	case idx < 0:
		st := VRSessionState{State: VRStateNone}
		if v.cachedUserPodRunning(user.Name) {
			st.Message = vrGameStoppingMessage
		}
		return st, nil
	}
	s := &own[idx]
	view := v.view(ctx, s)
	st := VRSessionState{App: s.Spec.GameReference.Name, Title: view.app, HeadsetConnected: view.headset}
	switch {
	case view.stopping:
		st.State = VRStateStopping
	case view.ready && IsWiVRnServerKey(s.Status.VRServerKey):
		st.State, st.Host, st.Port, st.ServerKey = VRStateReady, view.host, view.port, s.Status.VRServerKey
	default:
		st.State = VRStateStarting
	}
	return st, nil
}

// vrView is what the page shows of a VR Session.
type vrView struct {
	app      string
	stopping bool
	ready    bool
	host     string
	port     int32
	headset  bool
}

func (v *VRLaunch) view(ctx context.Context, s *v1alpha1types.Session) vrView {
	view := vrView{app: s.Spec.GameReference.Name, stopping: s.DeletionTimestamp != nil}
	if app, err := v.Apps.Get(view.app); err == nil && app.Spec.Title != "" {
		view.app = app.Spec.Title
	}
	view.headset = meta.IsStatusConditionTrue(s.Status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition)
	if view.stopping {
		return view
	}
	var pod *corev1.Pod
	var err error
	if v.SessionPods != nil {
		pod, err = v.SessionPods.Get(s.Name)
	} else {
		pod, err = v.Pods.Get(ctx, s.Name, metav1.GetOptions{})
	}
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
		if st, err := v.readStatus(ctx, s, pod); err != nil {
			klog.V(2).Infof("VR page: reading session %s's VR server: %v", s.Name, err)
		} else {
			view.headset = st.HeadsetConnected
		}
	}
	return view
}

// readStatus is s's vr-agent status, reused for libraryVRStatusTTL. One read
// per Session at a time: concurrent views wait for it, so a flood of views
// costs one Secret GET per TTL, not one each (QA, XERK-2670).
func (v *VRLaunch) readStatus(ctx context.Context, s *v1alpha1types.Session, pod *corev1.Pod) (VRStatus, error) {
	v.statusMu.Lock()
	now := time.Now()
	c, ok := v.statuses[s.UID]
	if !ok || (c.done == nil && now.Sub(c.at) >= libraryVRStatusTTL) {
		if v.statuses == nil {
			v.statuses = map[types.UID]*cachedVRStatus{}
		}
		// Drop stale entries (ended Sessions) as we go.
		for uid, old := range v.statuses {
			if old.done == nil && now.Sub(old.at) >= libraryVRStatusTTL {
				delete(v.statuses, uid)
			}
		}
		c = &cachedVRStatus{done: make(chan struct{})}
		v.statuses[s.UID] = c
		done := c.done
		v.statusMu.Unlock()
		var st VRStatus
		// Until the read returns, a panic: waiters are released however it
		// ends.
		err := errVRReadPanicked
		defer func() {
			v.statusMu.Lock()
			c.at, c.status, c.err, c.done = time.Now(), st, err, nil
			v.statusMu.Unlock()
			close(done)
		}()
		// Not the request's context: its waiters share the result.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), libraryVRStatusTimeout)
		defer cancel()
		st, err = v.Status(sctx, s, pod)
		return st, err
	}
	done := c.done
	v.statusMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return VRStatus{}, fmt.Errorf("waiting for the VR server read: %w", ctx.Err())
		}
	}
	v.statusMu.Lock()
	defer v.statusMu.Unlock()
	return c.status, c.err
}

// showPage writes the page for user, with notice (a refusal) above it.
func (v *VRLaunch) showPage(ctx context.Context, w http.ResponseWriter, user *v1alpha1types.User, status int, notice string) {
	own, err := v.cachedUserSessions(ctx, user.Name)
	if err != nil {
		klog.Errorf("VR page: user %s: %v", user.Name, err)
		writeLibraryPage(w, http.StatusInternalServerError, vrTryAgainMessage, false, false)
		return
	}
	var b strings.Builder
	if notice != "" {
		fmt.Fprintf(&b, `<p class="notice">%s</p>`, html.EscapeString(notice))
	}
	refresh := 0
	idx := slices.IndexFunc(own, func(s v1alpha1types.Session) bool { return s.Spec.IsVR() })
	switch {
	case idx >= 0:
		view := v.view(ctx, &own[idx])
		refresh = writeVRSession(&b, &view)
	case len(own) > 0:
		b.WriteString(`<p>` + html.EscapeString(vrMoonlightMessage) + `</p>`)
	default:
		if v.cachedUserPodRunning(user.Name) {
			b.WriteString(`<p class="notice">` + html.EscapeString(vrGameStoppingMessage) + `</p>`)
			refresh = libraryVRRefresh
		}
		writeVRApps(&b, v.VRApps(user))
	}
	writeVRPage(w, status, b.String(), refresh)
}

// VRApps is the Apps user may play in VR, by title.
func (v *VRLaunch) VRApps(user *v1alpha1types.User) []*v1alpha1types.App {
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
		fmt.Fprintf(b, `<form method="post" action="%s"><input type="hidden" name="action" value="play"><input type="hidden" name="app" value="%s"><button type="submit">Play %s</button></form>`,
			libraryVRPath, html.EscapeString(a.Name), html.EscapeString(appTitle(a)))
	}
}

// writeVRSession describes view and returns how often, in seconds, the page
// should reload to follow it.
func writeVRSession(b *strings.Builder, view *vrView) int {
	title := html.EscapeString(view.app)
	stop := `<form method="post" action="` + libraryVRPath + `"><input type="hidden" name="action" value="stop"><button type="submit">Stop the VR session</button></form>`
	switch {
	case view.stopping:
		fmt.Fprintf(b, `<p>Stopping %s…</p>`, title)
		return libraryVRRefresh
	case !view.ready:
		fmt.Fprintf(b, `<p>Starting %s… The first start can take several minutes.</p>%s`, title, stop)
		return libraryVRRefresh
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
	b.WriteString(`<p><small>With no headset connected the session stops by itself after a while.</small></p>`)
	b.WriteString(stop)
	if view.headset {
		return libraryVRIdleRefresh
	}
	return libraryVRRefresh
}

func writeVRPage(w http.ResponseWriter, status int, body string, refresh int) {
	meta := ""
	if refresh > 0 {
		meta = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, refresh)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">%s<title>Play in VR</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0;padding:0 16px}p{max-width:32em;text-align:center;font-size:1.2em}button{font-size:1.1em;padding:.6em 1.4em}main{display:grid;justify-items:center;gap:1em}small{color:#aaa}a{color:#9cf}.big{font-size:2em;font-family:ui-monospace,monospace;margin:0}dl{display:grid;grid-template-columns:auto auto;gap:.3em 1em;align-items:baseline}dt{color:#aaa}dd{margin:0}.notice{color:#fc6}</style>
</head><body><main><h1>Play in VR</h1>%s</main></body></html>
`, meta, body)
}

// vrMirrorMessage is a Moonlight-started VR session's state message.
const vrMirrorMessage = "Started from Moonlight: quit it there to stop it."

// moonlightVRState is s as the front door reports it, if s is a Moonlight
// Session of a VR App (XERK-2811): ready once its split App pod is Ready
// and the operator recorded its VR server key, so the headset connects to
// it pinned, as to a native session. The headset can't stop it: Stop never
// touches a Moonlight Session.
func (v *VRLaunch) moonlightVRState(ctx context.Context, s *v1alpha1types.Session) (VRSessionState, bool) {
	app, err := v.Apps.Get(s.Spec.GameReference.Name)
	if s.Spec.IsVR() || err != nil || !app.Spec.VRPort {
		return VRSessionState{}, false
	}
	st := VRSessionState{State: VRStateStarting, App: app.Name, Title: appTitle(app), Message: vrMirrorMessage}
	if s.DeletionTimestamp != nil {
		st.State = VRStateStopping
		return st, true
	}
	wolf, err := v.sessionPod(ctx, s.Name)
	if err != nil || !metav1.IsControlledBy(wolf, s) || wolf.Annotations[appPodAnnotation] == "" {
		return st, true
	}
	pod, err := v.sessionPod(ctx, wolf.Annotations[appPodAnnotation])
	if err != nil || !metav1.IsControlledBy(pod, s) || pod.DeletionTimestamp != nil || !podReady(pod) ||
		pod.Status.HostIP == "" || s.Status.Ports.VR == 0 || !IsWiVRnServerKey(s.Status.VRServerKey) {
		return st, true
	}
	st.State, st.Host, st.Port, st.ServerKey = VRStateReady, pod.Status.HostIP, s.Status.Ports.VR, s.Status.VRServerKey
	return st, true
}

// sessionPod is the session pod name, from the informer when there is one.
func (v *VRLaunch) sessionPod(ctx context.Context, name string) (*corev1.Pod, error) {
	var pod *corev1.Pod
	var err error
	if v.SessionPods != nil {
		pod, err = v.SessionPods.Get(name)
	} else {
		pod, err = v.Pods.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("getting pod %s: %w", name, err)
	}
	return pod, nil
}
