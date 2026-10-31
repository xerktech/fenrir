package controllers

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// libraryActivityFlushInterval is how often observed browser traffic is
// written to the pod. Far below the idle timeout, so the lag doesn't matter.
const libraryActivityFlushInterval = 30 * time.Second

// libraryProvisionTimeout bounds a first visit's User creation, so a slow API
// server costs one refresh of the setup page, not a hung request.
// A var so tests can shorten it.
var libraryProvisionTimeout = 10 * time.Second

// librarySetupMessage is the page a first visit gets while its User and
// Library are created.
const librarySetupMessage = "Setting up your account — creating your game library. This takes about a minute on first visit."

// libraryStartPath is where the stopped page's Start button posts.
const libraryStartPath = "/.direwolf/library/start"

// LibraryProxySecretHeader carries the secret shared with the authenticating
// proxy (Authentik sets it through a property mapping on its provider). The
// same header the pairing page uses, with its own secret.
const LibraryProxySecretHeader = "X-Direwolf-Proxy-Secret"

// MinLibraryProxySecretLen is the shortest proxy secret accepted, so a
// placeholder or truncated secret file fails at startup.
const MinLibraryProxySecretLen = 32

// DefaultLibraryUserHeader is the header Authentik's proxy outpost names the
// signed-in user in.
const DefaultLibraryUserHeader = "X-Authentik-Username"

// LibraryServer is the Library page's HTTP front end; the Ingress points
// here, not at the pod. A visit with no pod starts one (unless a game holds
// the Steam lock) and serves a "Starting…" page until the pod is ready, then
// reverse-proxies the Selkies page and its WebSocket to the pod, adding the
// pod's basic-auth credentials.
//
// The visitor's library is chosen by userHeader, which only the trusted
// proxy can set: the User of that name's own (User.spec.library), or the
// shared one for a User without one. A name with no User is refused: it would
// otherwise get the shared desktop, signed in to its users' accounts. No
// header at all (a proxy that names no one) gets the shared one. With a
// Provisioner, a name with no User whose groups header lists the provisioning
// group gets a User (and its own library) created on that visit instead.
type LibraryServer struct {
	library *LibraryController
	pods    generic.NamespacedLister[*corev1.Pod]
	users   generic.NamespacedLister[*v1alpha1types.User]
	// userHeader names the authenticated user.
	userHeader string
	// warnNoUserHeader logs, once, the first request without userHeader:
	// a misspelt header otherwise puts everyone on the shared desktop.
	warnNoUserHeader sync.Once
	// trusted are the only peers (the ingress controller behind Authentik)
	// allowed in: the desktop has no login of its own.
	trusted []netip.Prefix
	// secret must also arrive in LibraryProxySecretHeader: where the proxy
	// is an in-cluster pod (Authentik's outpost) and the CNI does not
	// enforce NetworkPolicy, the source check admits every pod in the
	// trusted range. Empty refuses every request.
	secret []byte
	proxy  *httputil.ReverseProxy

	// Provisioner creates a group member's User on first sign-in; nil when
	// provisioning is off (--provision-group).
	Provisioner *Provisioner

	// states is each library's *libraryState, by owner ("" the shared one).
	states sync.Map
}

// libraryState is what the server tracks for one library's pod.
type libraryState struct {
	owner string
	// password caches the pod's basic-auth password; cleared on a 401.
	password atomic.Pointer[string]

	// lastSeen is the unix-nano time of the last byte to or from the pod.
	lastSeen    atomic.Int64
	lastFlushed int64
	// lastVisit is the unix-nano time of the last user-opened page load.
	lastVisit        atomic.Int64
	lastFlushedVisit int64
}

func (st *libraryState) touch() {
	st.lastSeen.Store(time.Now().UnixNano())
}

type libraryTargetKey struct{}

type libraryTarget struct {
	url      *url.URL
	password string
	state    *libraryState
}

func NewLibraryServer(
	library *LibraryController,
	pods generic.NamespacedLister[*corev1.Pod],
	users generic.NamespacedLister[*v1alpha1types.User],
	userHeader string,
	trusted []netip.Prefix,
	secret []byte,
) *LibraryServer {
	s := &LibraryServer{library: library, pods: pods, users: users, userHeader: userHeader, trusted: trusted, secret: secret}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Every byte through a pod connection, including an upgraded
		// WebSocket's, counts as browser activity.
		// Connections are pooled by pod address, so a connection's bytes
		// are always its own pod's library's.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target, ok := ctx.Value(libraryTargetKey{}).(libraryTarget)
			if !ok {
				return nil, errors.New("dialing the Library pod: no target")
			}
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, fmt.Errorf("dialing the Library pod: %w", err)
			}
			target.state.touch()
			return &activityConn{Conn: conn, touch: target.state.touch}, nil
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	s.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			if target, ok := r.In.Context().Value(libraryTargetKey{}).(libraryTarget); ok {
				r.SetURL(target.url)
				r.Out.SetBasicAuth(libraryAuthUser, target.password)
			}
			// The desktop runs arbitrary user code; it never needs the secret.
			r.Out.Header.Del(LibraryProxySecretHeader)
			r.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				// The pod started with another password; re-read it. The
				// browser can't answer the challenge, so don't prompt it.
				if target, ok := resp.Request.Context().Value(libraryTargetKey{}).(libraryTarget); ok {
					target.state.password.Store(nil)
				}
				resp.Header.Del("WWW-Authenticate")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			klog.V(2).Infof("Library proxy error: %v", err)
			writeLibraryPage(w, http.StatusBadGateway, "The Library isn't answering yet. Retrying…", true, false)
		},
	}
	return s
}

// state is owner's libraryState, created on first use.
func (s *LibraryServer) state(owner string) *libraryState {
	st, _ := s.states.LoadOrStore(owner, &libraryState{owner: owner})
	return st.(*libraryState) //nolint:forcetypeassert // only libraryStates are stored
}

// Run serves on port until ctx is done, flushing activity to the pod.
func (s *LibraryServer) Run(ctx context.Context, port int) error {
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		ticker := time.NewTicker(libraryActivityFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.flushActivity(ctx)
			}
		}
	}()
	klog.Infof("Serving the Library page on :%d", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving the Library page: %w", err)
	}
	return nil
}

func (s *LibraryServer) flushActivity(ctx context.Context) {
	s.states.Range(func(_, v any) bool {
		v.(*libraryState).flushActivity(ctx, s.library) //nolint:forcetypeassert // only libraryStates are stored
		return true
	})
}

// flushActivity runs on the one flush goroutine only.
func (st *libraryState) flushActivity(ctx context.Context, library *LibraryController) {
	seen, visit := st.lastSeen.Load(), st.lastVisit.Load()
	if seen == 0 || (seen == st.lastFlushed && visit == st.lastFlushedVisit) {
		return
	}
	// Send the visit only when this replica saw a new one: another replica
	// may have recorded a later one, and an old one must never overwrite it.
	var visitAt time.Time
	if visit != 0 && visit != st.lastFlushedVisit {
		visitAt = time.Unix(0, visit)
	}
	if err := library.RecordActivity(ctx, st.owner, time.Unix(0, seen), visitAt); err != nil {
		klog.Errorf("Failed to record Library activity: %v", err)
		return
	}
	st.lastFlushed, st.lastFlushedVisit = seen, visit
}

// errNoSuchUser: the proxy named a user with no User.
var errNoSuchUser = errors.New("no such user")

// libraryOf is the library the request's authenticated user plays from.
func (s *LibraryServer) libraryOf(r *http.Request) (LibraryRef, error) {
	name := r.Header.Get(s.userHeader)
	_, named := r.Header[http.CanonicalHeaderKey(s.userHeader)]
	if !named || s.users == nil {
		if !named {
			s.warnNoUserHeader.Do(func() {
				klog.Warningf("Library page: a request named no user in %s, so it gets the shared Library; if the proxy sends the username under another header, fix --library-user-header", s.userHeader)
			})
		}
		return s.library.LibraryFor(nil), nil
	}
	user, err := v1alpha1types.LookupUser(s.users.Get, name)
	if apierrors.IsNotFound(err) {
		return LibraryRef{}, fmt.Errorf("%w: %q", errNoSuchUser, name)
	}
	if err != nil {
		return LibraryRef{}, fmt.Errorf("getting user %s: %w", name, err)
	}
	return s.library.LibraryFor(user), nil
}

func (s *LibraryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !s.isTrusted(peer.Addr().Unmap()) {
		klog.Warningf("Library page: refusing request from untrusted peer %s", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !s.hasProxySecret(r) {
		klog.Warningf("Library page: refusing request from %s without the proxy secret", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	start := r.URL.Path == libraryStartPath
	if start && (r.Method != http.MethodPost || r.Header.Get("Sec-Fetch-Site") == "cross-site") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	lib, err := s.libraryOf(r)
	// provisioning: this visit's User was provisioned but is not yet cached.
	provisioning := false
	if errors.Is(err, errNoSuchUser) && s.mayProvision(r) {
		var user *v1alpha1types.User
		// Only a visit the user asked for creates a User: a tab's script
		// reload after an admin deleted theirs must not re-create it.
		user, err = s.provision(r, start || userNavigation(r))
		if errors.Is(err, errNoSuchUser) {
			writeLibraryPage(w, http.StatusServiceUnavailable, "The Library is stopped.", false, true)
			return
		}
		if err != nil {
			s.writeProvisionError(w, err)
			return
		}
		lib = s.library.LibraryFor(user)
		provisioning = user.Labels[v1alpha1types.ProvisionedLabel] == v1alpha1types.ProvisionedLabelValue
	}
	if errors.Is(err, errNoSuchUser) {
		klog.Warningf("Library page: refusing %v", err)
		writeLibraryPage(w, http.StatusForbidden, "You have no streaming account here. Ask the admin to add one.", false, false)
		return
	}
	if err != nil {
		klog.Errorf("Library page: %v", err)
		writeLibraryPage(w, http.StatusInternalServerError, "The Library is unavailable. Try again in a minute.", false, false)
		return
	}
	st := s.state(lib.Owner)
	pod, err := s.pods.Get(v1alpha1types.LibraryPodNameFor(lib.Owner))
	if err != nil {
		pod = nil
	}
	if pod == nil {
		// A provisioning visit starts the Library even from the setup page's
		// own refresh: only while a provisioned User exists live but is not
		// yet cached, seconds after the sign-in that created it.
		if !start && !userNavigation(r) && !provisioning {
			// Selkies reloads its tab when the stream drops, so a tab left
			// open across an idle shutdown would restart the Library at once.
			writeLibraryPage(w, http.StatusServiceUnavailable, "The Library is stopped.", false, true)
			return
		}
		var busy string
		pod, busy, err = s.library.EnsurePod(r.Context(), lib)
		if errors.Is(err, errLibraryClaims) {
			klog.Errorf("Not starting the Library: %v", err)
			writeLibraryPage(w, http.StatusConflict, "Your library's storage is misconfigured. Ask the admin to fix it.", false, false)
			return
		}
		if err != nil {
			klog.Errorf("Failed to start the Library: %v", err)
			writeLibraryPage(w, http.StatusInternalServerError, "The Library could not be started. Try again in a minute.", false, false)
			return
		}
		if busy != "" {
			writeLibraryPage(w, http.StatusConflict, busy, false, false)
			return
		}
	}
	if start || userNavigation(r) {
		// Someone is there: restart the MaxRuntime clock.
		st.lastVisit.Store(time.Now().UnixNano())
	}
	if start {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	switch {
	case pod.DeletionTimestamp != nil:
		writeLibraryPage(w, http.StatusServiceUnavailable, "The Library is shutting down…", true, false)
	case provisioning && (!podReady(pod) || pod.Status.PodIP == ""):
		writeLibraryPage(w, http.StatusServiceUnavailable, librarySetupMessage, true, false)
	case !podReady(pod) || pod.Status.PodIP == "":
		writeLibraryPage(w, http.StatusServiceUnavailable, "Starting the Library…", true, false)
	default:
		password, err := s.authPassword(r.Context(), st)
		if err != nil {
			klog.Errorf("Failed to read the Library password: %v", err)
			writeLibraryPage(w, http.StatusInternalServerError, "The Library is unavailable. Try again in a minute.", false, false)
			return
		}
		st.touch()
		target := libraryTarget{
			url:      &url.URL{Scheme: "http", Host: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(libraryHTTPPort))},
			password: password,
			state:    st,
		}
		s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), libraryTargetKey{}, target)))
	}
}

// mayProvision reports whether r's user, who has no User, gets one: only when
// provisioning is on and the groups header names the group. ServeHTTP has
// already checked the peer and the proxy secret, so the header is the proxy's.
func (s *LibraryServer) mayProvision(r *http.Request) bool {
	if s.Provisioner == nil {
		return false
	}
	return s.Provisioner.InGroup(r.Header.Get(s.Provisioner.GroupsHeader))
}

// provision returns r's User from the API server, creating it (and its
// claims) when create is set; without create a missing one is errNoSuchUser.
// The live read covers the seconds a new User takes to reach the cache.
// Bounded: the setup page refreshes, so a slow call just retries.
func (s *LibraryServer) provision(r *http.Request, create bool) (*v1alpha1types.User, error) {
	ctx, cancel := context.WithTimeout(r.Context(), libraryProvisionTimeout)
	defer cancel()
	username := r.Header.Get(s.userHeader)
	if create {
		return s.Provisioner.Ensure(ctx, username)
	}
	user, err := v1alpha1types.LookupUser(func(name string) (*v1alpha1types.User, error) {
		return s.Provisioner.userAPI.Get(ctx, name, metav1.GetOptions{})
	}, username)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %q", errNoSuchUser, username)
	}
	if err != nil {
		return nil, fmt.Errorf("getting user %s: %w", username, err)
	}
	return user, nil
}

func (s *LibraryServer) writeProvisionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errProvisionRefused) {
		klog.Warningf("Library page: not provisioning: %v", err)
		writeLibraryPage(w, http.StatusForbidden,
			"Your account name can't be used for a streaming account. Ask the admin to set one up for you.", false, false)
		return
	}
	klog.Errorf("Library page: provisioning failed: %v", err)
	writeLibraryPage(w, http.StatusInternalServerError,
		"Setting up your account failed. Retrying in a moment; if this keeps happening, ask the admin.", true, false)
}

// userNavigation reports whether r is a page load the user asked for (typed
// URL, link, reload button): browsers send Sec-Fetch-User only for those,
// never for a script's location.reload().
func userNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Upgrade") == "" && r.Header.Get("Sec-Fetch-User") == "?1"
}

func (s *LibraryServer) authPassword(ctx context.Context, st *libraryState) (string, error) {
	if p := st.password.Load(); p != nil {
		return *p, nil
	}
	password, err := s.library.AuthPassword(ctx, st.owner)
	if err != nil {
		return "", err
	}
	st.password.Store(&password)
	return password, nil
}

func (s *LibraryServer) hasProxySecret(r *http.Request) bool {
	if len(s.secret) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(LibraryProxySecretHeader)), s.secret) == 1
}

func (s *LibraryServer) isTrusted(addr netip.Addr) bool {
	for _, prefix := range s.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// writeLibraryPage serves a minimal status page; refresh reloads it until the
// Library is up, startButton offers to start it.
func writeLibraryPage(w http.ResponseWriter, status int, message string, refresh, startButton bool) {
	button := ""
	if startButton {
		button = `<form method="post" action="` + libraryStartPath + `"><button type="submit">Start the Library</button></form>`
	}
	meta := ""
	if refresh {
		meta = `<meta http-equiv="refresh" content="3">`
		w.Header().Set("Retry-After", "3")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">%s<title>Library</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0;padding:0 16px}p{max-width:32em;text-align:center;font-size:1.2em}button{font-size:1.1em;padding:.6em 1.4em}main{display:grid;justify-items:center}</style>
</head><body><main><p>%s</p>%s</main></body></html>
`, meta, html.EscapeString(message), button)
}

// activityConn calls touch on every read or write.
type activityConn struct {
	net.Conn
	touch func()
}

func (c *activityConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.touch()
	}
	return n, err //nolint:wrapcheck // net.Conn passthrough; io.EOF must stay io.EOF
}

func (c *activityConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.touch()
	}
	return n, err //nolint:wrapcheck // net.Conn passthrough
}
