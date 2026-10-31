package moonlight

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
)

type RESTServerOptions struct {
	Port       int
	SecurePort int
	Cert       tls.Certificate

	// LaunchTimeout is how long /launch will wait for the operator to create a
	// session and populate its stream URL before giving up. A cold start (image
	// pull, wolf boot, wolf-agent readiness) can take longer than the default,
	// so this is exposed as a knob instead of hard-coded. Defaults to
	// DefaultLaunchTimeout if unset.
	LaunchTimeout time.Duration

	// MaxConcurrentSessions caps how many users may stream at once (the node
	// has one GPU). A /launch that would exceed it gets Moonlight's "busy"
	// error instead of a Session. The caller's own sessions don't count: a
	// relaunch replaces them. Defaults to 1 if unset; negative means no limit.
	MaxConcurrentSessions int

	// BusyCheck, if set, is consulted on every /launch before the session
	// limit, and again after the Session is created (backing it out if busy).
	// A non-empty reason makes the launch fail with the busy error carrying
	// that reason (e.g. the Library pod holds Steam's single-writer lock).
	BusyCheck BusyCheck

	// Library, if set, adds the "Close Library" tile (CloseLibraryAppID) to
	// the app list while the Library is open; launching it stops the Library.
	Library Library

	// PinPage serves the pairing page on its own listener. Disabled when
	// PinPage.Port is 0; there is deliberately no fallback onto Port, which
	// Moonlight clients reach directly.
	PinPage PinPageOptions

	// shutdownTimeout bounds graceful shutdown; defaults to 5s. Overridden by
	// tests.
	shutdownTimeout time.Duration
}

// BusyCheck reports whether something outside the Session count occupies the
// host. It returns a human-readable reason when busy, "" when free.
type BusyCheck func(ctx context.Context, user *v1alpha1types.User) (reason string, err error)

// ClientLaunchTimeout is how long moonlight-qt waits for /launch before giving
// up on its own. LaunchTimeout must stay below it.
const ClientLaunchTimeout = 120 * time.Second

// DefaultLaunchTimeout sits under ClientLaunchTimeout, so the client sees our
// error rather than its own generic timeout.
const DefaultLaunchTimeout = 100 * time.Second

// launchSlotTimeout bounds the API calls made while holding launchSlot, so a
// stalled API server can't block every launch indefinitely.
const launchSlotTimeout = 30 * time.Second

// busyStatusCode/busyMessage mirror what Sunshine sends when an app is
// already running, so clients show a familiar error.
const (
	busyStatusCode = 400
	busyMessage    = "An app is already running on this host"
)

// sessionEndedMessage answers a /launch or /resume whose Session was deleted
// (by the operator, a cancel from another device, or the reaper) before its
// stream started. The operator logs why.
const sessionEndedMessage = "The host ended the session before its stream started"

// refuseUnstreamableClient answers a /launch or /resume from an address Wolf
// can't stream to (its stream sockets are IPv4-only) before any Session is
// created or updated: otherwise a launch spends a cold start on a pod the
// operator then tears down, and a resume leaves the refused IP in the Session
// until the disconnect grace period reaps it. It reports whether it answered.
// The operator's streamClientIP applies the same rule.
func refuseUnstreamableClient(w http.ResponseWriter, clientIP string) bool {
	addr, err := netip.ParseAddr(clientIP)
	if err == nil && addr.Unmap().Is4() && !addr.IsUnspecified() {
		return false
	}
	refusal := &streamRefusedError{message: "Streaming needs IPv4, and this client connected from " + clientIP}
	klog.Infof("Refusing stream to %s: not IPv4", clientIP)
	sendXMLWithHTTPStatus(w, http.StatusOK, refusal.response())
	return true
}

// internalErrorMessage is all a client is told of a 5xx; the log has why.
const internalErrorMessage = "Internal error on the host"

// streamRefusedStatusCode is the XML status of a launch the operator refused.
// Moonlight shows any status other than 200 with its message.
const streamRefusedStatusCode = 503

type RESTServer struct {
	router       *http.ServeMux
	secureRouter *http.ServeMux

	manager *PairingManager

	PairingsLister generic.NamespacedLister[*v1alpha1types.Pairing]
	UserLister     generic.NamespacedLister[*v1alpha1types.User]
	AppLister      generic.NamespacedLister[*v1alpha1types.App]
	PodLister      generic.NamespacedLister[*v1.Pod]
	SessionLister  generic.NamespacedLister[*v1alpha1types.Session]

	SessionClient v1alpha1client.SessionInterface

	// launchSlot (capacity 1) serializes the busy check with Session
	// creation so two concurrent launches can't both pass the limit. A channel
	// rather than a mutex so a waiter can give up when its client does.
	launchSlot chan struct{}

	// pendingCleanups holds launch IDs of failed launches whose Sessions
	// are still to be deleted. Drained only while holding launchSlot.
	cleanupMu       sync.Mutex
	pendingCleanups []string

	RESTServerOptions
}

func NewRESTServer(
	manager *PairingManager,
	pairingsLister generic.NamespacedLister[*v1alpha1types.Pairing],
	userLister generic.NamespacedLister[*v1alpha1types.User],
	appLister generic.NamespacedLister[*v1alpha1types.App],
	sessionLister generic.NamespacedLister[*v1alpha1types.Session],
	podsLister generic.NamespacedLister[*v1.Pod],
	sessionClient v1alpha1client.SessionInterface,
	opts RESTServerOptions,
) *RESTServer {
	if opts.LaunchTimeout <= 0 {
		opts.LaunchTimeout = DefaultLaunchTimeout
	}
	if opts.MaxConcurrentSessions == 0 {
		opts.MaxConcurrentSessions = 1
	}

	ps := &RESTServer{
		router:            http.NewServeMux(),
		secureRouter:      http.NewServeMux(),
		manager:           manager,
		PairingsLister:    pairingsLister,
		UserLister:        userLister,
		AppLister:         appLister,
		SessionLister:     sessionLister,
		PodLister:         podsLister,
		SessionClient:     sessionClient,
		launchSlot:        make(chan struct{}, 1),
		RESTServerOptions: opts,
	}

	// Register routes
	ps.router.HandleFunc("/serverinfo", ps.serverInfoHandler)
	ps.router.HandleFunc("/pair", ps.pairHandler)
	ps.router.HandleFunc("/unpair", ps.unpairHandler)

	ps.router.HandleFunc("/readyz", ps.readyzHandler)
	ps.router.HandleFunc("/livez", ps.livezHandler)

	ps.secureRouter.HandleFunc("/serverinfo", ps.serverInfoHandler)
	ps.secureRouter.HandleFunc("/pair", ps.pairHandler)

	ps.secureRouter.HandleFunc("/applist", ps.appListHandler)
	ps.secureRouter.HandleFunc("/launch", ps.launchHandler)
	ps.secureRouter.HandleFunc("/resume", ps.resumeHandler)
	ps.secureRouter.HandleFunc("/cancel", ps.cancelHandler)
	ps.secureRouter.HandleFunc("/appasset", ps.appAssetHandler)

	return ps
}

func (s *RESTServer) Run(ctx context.Context) error {
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.Port),
		Handler: loggingMiddleware(s.router),
	}

	secureServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.SecurePort),
		Handler: loggingMiddleware(s.authenticatedMiddleware(s.secureRouter)),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{s.Cert},
			ClientAuth:   tls.RequestClientCert,
			VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
				// Accept any certificate for now during connection negotiation
				// we have a middleware that will verify the actual ceritificate
				// used and find a user for later user.
				return nil
			},
		},
	}

	var pinServer *http.Server
	if s.PinPage.Port != 0 {
		pinServer = &http.Server{
			Addr:              fmt.Sprintf(":%d", s.PinPage.Port),
			Handler:           loggingMiddleware(NewPinPageHandler(s.manager, s.UserLister, s.PinPage)),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	var error atomic.Pointer[error]
	go func() {
		defer cancel()
		klog.Infof("HTTP server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			klog.Errorf("HTTP Server failed: %s", err)
			error.CompareAndSwap(nil, &err)
		}
	}()

	go func() {
		defer cancel()
		klog.Infof("HTTPS server listening on %s", secureServer.Addr)
		if err := secureServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
			klog.Errorf("HTTPS Server failed: %s", err)
			error.CompareAndSwap(nil, &err)
		}
	}()

	if pinServer != nil {
		go func() {
			defer cancel()
			klog.Infof("Pairing page listening on %s", pinServer.Addr)
			if err := pinServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				klog.Errorf("Pairing page server failed: %s", err)
				error.CompareAndSwap(nil, &err)
			}
		}()
	}

	<-ctx.Done()
	klog.Info("Shutting down server...")

	// Shut every listener down at once, with a deadline. A pair request can
	// block indefinitely waiting for its PIN; shutting down one server after
	// another would leave the next one (HTTPS: /launch) serving until the
	// process is killed.
	timeout := s.shutdownTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancelShutdown()
	var wg sync.WaitGroup
	for _, srv := range []*http.Server{server, secureServer, pinServer} {
		if srv == nil {
			continue
		}
		wg.Go(func() {
			if err := srv.Shutdown(shutdownCtx); err != nil {
				klog.Warningf("Server %s did not shut down gracefully (%s), closing", srv.Addr, err)
				_ = srv.Close()
			}
		})
	}
	wg.Wait()

	if err := error.Load(); err != nil {
		klog.Errorf("Server failed: %s", *err)
		return *err
	}

	return nil
}

func (s *RESTServer) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// Server is ready to serve traffic as soon as HTTP & HTTPS server is UP.
	//!TODO: (And when all informers/caches are synced)
	w.WriteHeader(http.StatusOK)
}

func (s *RESTServer) livezHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *RESTServer) serverInfoHandler(w http.ResponseWriter, r *http.Request) {
	// If this is HTTPS request, print the client certificate
	pairStatus := 0
	serverStatus := "SUNSHINE_SERVER_FREE"
	currentGame := ""

	if user := r.Context().Value(userContextKey{}); user != nil {
		user := user.(*v1alpha1types.User)

		// Return SERVER_BUSY if there exists any pod with direwolf/user = <user>
		pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
			"direwolf/user": user.Name,
		}))
		if err != nil {
			writeErrorResponse(w, 500, fmt.Errorf("failed to list pods: %s", err))
			return
		}

		if len(pods) > 0 {
			serverStatus = "SUNSHINE_SERVER_BUSY"

			currentApp, err := s.AppLister.Get(pods[0].Labels["direwolf/app"])
			if err != nil {
				writeErrorResponse(w, 500, fmt.Errorf("failed to get app: %s", err))
				return
			}

			currentGame = fmt.Sprint(currentApp.Spec.ID)
		}
		pairStatus = 1
	}

	root := ServerInfoResponse{
		Response: Response{
			StatusCode: 200,
		},
		ServerInfo: ServerInfo{
			Hostname:               ServerName,
			AppVersion:             "7.1.431.-1",
			GfeVersion:             "3.23.0.74",
			UniqueID:               "dd7c60f6-4b88-4ef1-be07-eeec72f96080", // Does this matter?
			MaxLumaPixelsHEVC:      1869449984,
			ServerCodecModeSupport: 65793, // Bitwise OR of various codecs. Just hardcoding HEVC and AV1 for now
			HttpsPort:              s.SecurePort,
			ExternalPort:           s.Port,
			MAC:                    "00:00:00:00:00:00", // Does this matter?
			LocalIP:                "127.0.0.1",         // Does this matter?
			SupportedDisplayModes: DisplayModes{
				Modes: []DisplayMode{
					{1280, 720, 120}, {1280, 720, 60}, {1280, 720, 30},
					{1920, 1080, 120}, {1920, 1080, 60}, {1920, 1080, 30},
					{2560, 1440, 120}, {2560, 1440, 90}, {2560, 1440, 60},
					{3840, 2160, 120}, {3840, 2160, 90}, {3840, 2160, 60},
					{7680, 4320, 120}, {7680, 4320, 90}, {7680, 4320, 60},
				},
			},
			PairStatus:  pairStatus,
			CurrentGame: currentGame, // Meant to be "last played game". But we still use current game
			State:       serverStatus,
		},
	}

	sendXML(w, root)
}

// Multiplex the multiple phases of pairing into a single handler
func (s *RESTServer) pairHandler(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Handling pair request from %s", r.RemoteAddr)

	clientID := r.URL.Query().Get("uniqueid")
	clientIP, err := remoteIP(r)
	if err != nil {
		sendXML(w, failPair(err.Error()))
		return
	}
	cacheKey := fmt.Sprintf("%s@%s", clientID, clientIP)

	if clientID == "" {
		sendXML(w, failPair("uniqueid required"))
		return
	}

	if r.URL.Query().Has("salt") {
		klog.Infof("Pairing phase 1 with %s\n", cacheKey)
		salt := r.URL.Query().Get("salt")
		clientCertStr := r.URL.Query().Get("clientcert")

		sendXML(w, s.manager.pairPhase1(r.Context(), cacheKey, salt, clientCertStr))
		return
	} else if r.URL.Query().Has("clientchallenge") {
		klog.Infof("Pairing phase 2 with %s\n", cacheKey)
		clientChallenge := r.URL.Query().Get("clientchallenge")

		sendXML(w, s.manager.pairPhase2(cacheKey, clientChallenge))
		return
	} else if r.URL.Query().Has("serverchallengeresp") {
		klog.Infof("Pairing phase 3 with %s\n", cacheKey)
		serverChallengeResp := r.URL.Query().Get("serverchallengeresp")

		sendXML(w, s.manager.pairPhase3(cacheKey, serverChallengeResp))
		return
	} else if r.URL.Query().Has("clientpairingsecret") {
		klog.Infof("Pairing phase 4 with %s\n", cacheKey)
		clientPairingSecret := r.URL.Query().Get("clientpairingsecret")

		sendXML(w, s.manager.pairPhase4(cacheKey, clientPairingSecret))
		return
	} else if phrase := r.URL.Query().Get("phrase"); phrase == "pairchallenge" {
		klog.Infof("Pairing phase 5 with %s\n", cacheKey)
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			sendXML(w, failPair("Client cert required"))
			return
		}
		sendXML(w, PairingResponse{Paired: 1, Response: Response{StatusCode: 200}})
		return
	}

	sendXML(w, failPair("Invalid pairing request"))
}

func (s *RESTServer) unpairHandler(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Handling unpair request from %s", r.RemoteAddr)
	if r.Method == "GET" {
		clientIP, err := remoteIP(r)
		if err != nil {
			writeErrorResponse(w, 400, err)
			return
		}
		clientID := r.URL.Query().Get("uniqueid")
		cacheKey := fmt.Sprintf("%s@%s", clientID, clientIP)

		if err := s.manager.Unpair(cacheKey); err != nil {
			writeErrorResponse(w, 500, err)
			return
		}

		sendXML(w, Response{StatusCode: 200})
	}
}

// requestLibraryOwner is the library the request's paired User plays from
// ("" the shared one, also for a request with no User).
func requestLibraryOwner(r *http.Request) string {
	if user, ok := r.Context().Value(userContextKey{}).(*v1alpha1types.User); ok {
		return user.LibraryOwner()
	}
	return ""
}

func (s *RESTServer) appListHandler(w http.ResponseWriter, r *http.Request) {
	apps, err := s.AppLister.List(nil)
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to list apps: %s", err))
		return
	}

	owner := requestLibraryOwner(r)
	appsList := make([]App, 0, len(apps))
	for _, app := range apps {
		if app.Spec.Hidden || !v1alpha1types.AppVisibleTo(app, owner) {
			continue
		}
		appsList = append(appsList, App{
			AppSpec: app.Spec,
		})
	}

	if s.closeLibraryOffered(owner) {
		ctx, cancel := context.WithTimeout(r.Context(), libraryCallTimeout)
		open, err := s.Library.Open(ctx, owner)
		cancel()
		if err != nil {
			// The games still list; only the tile is missing.
			klog.Errorf("Failed to check whether the Library is open: %v", err)
		}
		if open {
			appsList = append(appsList, App{AppSpec: v1alpha1types.AppSpec{
				Title: closeLibraryTitle,
				ID:    CloseLibraryAppID,
			}})
		}
	}

	sendXML(w, AppListResponse{
		Response: Response{
			StatusCode: 200,
		},
		Apps: appsList,
	})
}

// remoteIP returns the Moonlight client's IP from r.RemoteAddr. moonlight-proxy
// is host-networked, so this is the real peer Wolf must stream to (it is not
// derived from headers, which the client controls). The port is stripped,
// IPv4-mapped IPv6 from a dual-stack listener is unmapped so Wolf sees the
// IPv4 form, and any zone is dropped.
func remoteIP(r *http.Request) (string, error) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("unparseable client address %q: %w", r.RemoteAddr, err)
	}
	return peer.Addr().Unmap().WithZone("").String(), nil
}

// Display mode bounds, the same as Wolf's own /launch checks. Wolf's
// AddSession enforces them too from XERK-2042; a Session outside them would
// then never start, and before it Wolf hands the mode to its encoder as is
// (XERK-2044).
const (
	maxModeDimension   = 16384
	maxModeRefreshRate = 1000
)

// parseMode parses a Moonlight launch mode, "<width>x<height>x<fps>".
func parseMode(mode string) (width, height, refreshRate int, err error) {
	invalid := fmt.Errorf("invalid mode: %s", mode)
	splitMode := strings.Split(mode, "x")
	if len(splitMode) != 3 {
		return 0, 0, 0, invalid
	}
	var values [3]int
	for i, limit := range [3]int{maxModeDimension, maxModeDimension, maxModeRefreshRate} {
		v, convErr := strconv.Atoi(splitMode[i])
		if convErr != nil || v < 1 || v > limit {
			return 0, 0, 0, invalid
		}
		values[i] = v
	}
	return values[0], values[1], values[2], nil
}

func (s *RESTServer) launchHandler(w http.ResponseWriter, r *http.Request) {
	clientIP, err := remoteIP(r)
	if err != nil {
		writeErrorResponse(w, 400, err)
		return
	}

	// 2025/03/03 11:34:48 HTTP/2.0 GET /launch map[additionalStates:[1] appid:[firefox] localAudioPlayMode:[0] mode:[1920x1080x60] rikey:[773448F67992470C5C62848D361E1025] rikeyid:[1311662065] sops:[0] surroundAudioInfo:[196610] uniqueid:[0123456789ABCDEF]] 127.0.0.1:65314
	// 2025/03/03 11:34:48 &{GET /launch?uniqueid=0123456789ABCDEF&appid=firefox&mode=1920x1080x60&additionalStates=1&sops=0&rikey=773448F67992470C5C62848D361E1025&rikeyid=1311662065&localAudioPlayMode=0&surroundAudioInfo=196610 HTTP/2.0 2 0 map[Accept:[*/*] Accept-Encoding:[gzip, deflate, br] Accept-Language:[en-US,en;q=0.9] User-Agent:[Moonlight/1243 CFNetwork/1568.100.1 Darwin/24.0.0]] 0x14000296570 <nil> 0 [] false 127.0.0.1:47984 map[] map[] <nil> map[] 127.0.0.1:65314 /launch?uniqueid=0123456789ABCDEF&appid=firefox&mode=1920x1080x60&additionalStates=1&sops=0&rikey=773448F67992470C5C62848D361E1025&rikeyid=1311662065&localAudioPlayMode=0&surroundAudioInfo=196610 0x1400016a540 <nil> <nil> /launch 0x140001ce0f0 0x14000186540 [] map[]}
	appID := r.URL.Query().Get("appid")
	// additionalStates := r.URL.Query().Get("additionalStates") // ???
	mode := r.URL.Query().Get("mode")
	rikey := r.URL.Query().Get("rikey")
	rikeyID := r.URL.Query().Get("rikeyid")
	// sops := r.URL.Query().Get("sops") // legacy GFE auto-optimize game settings. i dont think wolf has equivalent
	surroundAudioInfo := r.URL.Query().Get("surroundAudioInfo")

	if appID == "" {
		writeErrorResponse(w, 400, fmt.Errorf("appid required"))
		return
	}

	if appID == strconv.Itoa(CloseLibraryAppID) && s.closeLibraryOffered(requestLibraryOwner(r)) {
		s.closeLibrary(w, r)
		return
	}

	if rikey == "" {
		writeErrorResponse(w, 400, fmt.Errorf("rikey required"))
		return
	}

	if rikeyID == "" {
		writeErrorResponse(w, 400, fmt.Errorf("rikeyid required"))
		return
	}

	if mode == "" {
		mode = "1920x1080x60"
	}

	width, height, refreshRate, err := parseMode(mode)
	if err != nil {
		writeErrorResponse(w, 400, err)
		return
	}

	if surroundAudioInfo == "" {
		surroundAudioInfo = "196610"
	}

	surroundFlags, err := strconv.Atoi(surroundAudioInfo)
	if err != nil {
		writeErrorResponse(w, 400, fmt.Errorf("invalid surroundAudioInfo: %s", surroundAudioInfo))
		return
	}

	app, err := s.getAppByID(appID, requestLibraryOwner(r))
	if err != nil {
		writeErrorResponse(w, 404, err)
		return
	}

	if refuseUnstreamableClient(w, clientIP) {
		return
	}

	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)
	pairing := r.Context().Value(pairingContextKey{}).(*v1alpha1types.Pairing)

	// One deadline for the whole launch (queueing for the slot, the slot
	// work and the readiness wait), so the client always gets our answer
	// before its own ClientLaunchTimeout fires.
	launchCtx, cancelLaunch := context.WithTimeout(r.Context(), s.LaunchTimeout)
	defer cancelLaunch()

	// Tags the Session so a failed Create whose object was stored anyway
	// (e.g. the response timed out) can still be found and removed.
	launchID := utilrand.String(16)

	session, busyReason, err := s.createSession(launchCtx, user, func(ctx context.Context) (*v1alpha1types.Session, error) {
		//!TOOD: May want to wait here, since we need the Service to stop pointing
		// at the old pod. It is very likely to happen before operator syncs and
		// can create session, but perhaps should still check after operator returns
		// the session URL.
		if stopErr := s.stopSessionsForUser(ctx, user, false); stopErr != nil && !k8serrors.IsNotFound(stopErr) {
			return nil, fmt.Errorf("failed to stop existing sessions: %w", stopErr)
		}

		klog.Infof("Launching app %s for user %s", app.Name, user.Name)
		return s.SessionClient.Create(
			ctx,
			&v1alpha1types.Session{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: fmt.Sprintf("%s-%s-", user.Name, app.Name),
					Namespace:    pairing.Namespace,
					Labels: map[string]string{
						"direwolf":      "true",
						"direwolf/app":  app.Name,
						"direwolf/user": user.Name,
						launchIDLabel:   launchID,
					},
					Annotations: map[string]string{
						"direwolf/pairing": pairing.Name,
					},
				},
				Spec: v1alpha1types.SessionSpec{
					GameReference: v1alpha1types.GameReference{
						Name: app.Name,
					},
					PairingReference: v1alpha1types.PairingReference{
						Name: pairing.Name,
					},
					UserReference: v1alpha1types.UserReference{
						Name: user.Name,
					},
					//!TODO: Unused. v1alpha2 Gateway types are not widely supported
					GatewayReference: v1alpha1types.GatewayReference{
						Name:      "unused",
						Namespace: "unused",
					},
					Config: v1alpha1types.SessionInfo{
						ClientIP:           clientIP,
						AESIV:              rikeyID,
						AESKey:             rikey,
						SurroundAudioFlags: surroundFlags,
						EncryptedRTSP:      clientSupportsEncryptedRTSP(r),
						VideoWidth:         width,
						VideoHeight:        height,
						VideoRefreshRate:   refreshRate,
					},
				},
			},
			metav1.CreateOptions{
				FieldManager: "direwolf-launch",
			},
		)
	}, func() {
		// The Create may have been stored even though it errored (e.g. the
		// response timed out); remove it before the next launch counts it.
		// Detached: the launch's deadline has usually passed by now.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(launchCtx), launchSlotTimeout)
		defer cancel()
		if cleanupErr := s.deleteLaunch(ctx, launchID); cleanupErr != nil {
			klog.Errorf("Failed to clean up failed launch %s: %s", launchID, cleanupErr)
		}
	})
	if busyReason != "" {
		klog.Infof("Refusing launch of app %s for user %s: %s", app.Name, user.Name, busyReason)
		// HTTP 200 with the error in the XML status, as Sunshine does:
		// moonlight-qt treats a non-2xx HTTP status as a transport error and
		// would not show the message.
		sendXMLWithHTTPStatus(w, http.StatusOK, busyResponse(busyReason))
		return
	}
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to launch app: %w", err))
		return
	}

	// Wait for session to be created by the direwolf controller.
	//
	// The budget must absorb a cold start of the session pod: image pulls
	// (multi-GB app images), wolf boot and wolf-agent readiness easily take
	// 30-60s, while 25s aborted every first launch (the client then cancels
	// and the half-started session is torn down). launchCtx derives from
	// r.Context(), so if the Moonlight client gives up and disconnects the
	// wait is cancelled early regardless of this timeout.
	var streamURL string
	err = wait.PollUntilContextCancel(launchCtx, 250*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		session, err := s.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			// The operator ended it, e.g. for a client IP it can never
			// stream to (it deletes those before starting their pod).
			return false, &streamRefusedError{message: sessionEndedMessage}
		}
		if err != nil {
			return false, err
		}

		if refusal := streamRefusal(session); refusal != nil {
			return false, refusal
		}
		if session.Status.StreamURL == "" {
			return false, nil
		}
		streamURL = session.Status.StreamURL
		return true, nil
	})
	if err != nil {
		// Don't leave a Session that never became ready: it would count
		// against the session limit and lock every other user out until
		// its owner cancels. Queued before answering, so whichever launch
		// takes the slot next removes it before counting; and in the
		// background, since moonlight-qt waits for the whole response and a
		// slow cleanup must not hold it past the client's own timeout.
		s.queueCleanup(launchID)
		go func() {
			s.launchSlot <- struct{}{}
			defer func() { <-s.launchSlot }()
			// Detached and bounded: launchCtx is done by now.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(launchCtx), launchSlotTimeout)
			defer cancel()
			s.drainCleanups(ctx)
		}()
		if refusal := (*streamRefusedError)(nil); errors.As(err, &refusal) {
			klog.Infof("Launch of app %s for user %s refused: %s", app.Name, user.Name, refusal)
			sendXMLWithHTTPStatus(w, http.StatusOK, refusal.response())
			return
		}
		writeErrorResponse(w, 500, fmt.Errorf("failed to launch app: %w", err))
		return
	}

	resp := LaunchResponse{
		Response: Response{
			StatusCode: 200,
		},
		RTSPSessionURL: streamURL,
	}
	// resumeHandler serves a /resume with nothing to resume from here, and
	// Moonlight reads a resume reply's <resume>, not <gamesession>.
	if r.URL.Path == "/resume" {
		resp.Resume = 1
	} else {
		resp.GameSession = 1
	}
	sendXML(w, resp)
}

// createSession runs create unless the host is busy for user, in which case
// it returns a non-empty reason and create is not called. The check and the
// create happen under launchSlot so the session limit holds under concurrency.
//
// If create fails, onCreateError runs in the background while the slot is
// still held, so the next launch can't count a half-created Session, and the
// caller can answer its client without waiting for the cleanup.
func (s *RESTServer) createSession(ctx context.Context, user *v1alpha1types.User, create func(ctx context.Context) (*v1alpha1types.Session, error), onCreateError func()) (*v1alpha1types.Session, string, error) {
	select {
	case s.launchSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, "", fmt.Errorf("waiting for a launch slot: %w", ctx.Err())
	}
	createFailed := false
	defer func() {
		if !createFailed {
			<-s.launchSlot
			return
		}
		go func() {
			defer func() { <-s.launchSlot }()
			onCreateError()
		}()
	}()

	ctx, cancel := context.WithTimeout(ctx, launchSlotTimeout)
	defer cancel()

	// A failed launch's Session must be gone before we count.
	s.drainCleanups(ctx)

	if s.BusyCheck != nil {
		reason, err := s.BusyCheck(ctx, user)
		if err != nil {
			return nil, "", fmt.Errorf("busy check failed: %w", err)
		}
		if reason != "" {
			return nil, reason, nil
		}
	}

	if s.MaxConcurrentSessions > 0 {
		// Read from the API server, not the informer: a session created by the
		// previous holder of launchMu may not have reached the cache yet.
		sessions, err := s.SessionClient.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, "", fmt.Errorf("failed to list sessions: %w", err)
		}
		// The limit is on users streaming, not Session objects.
		others := map[string]struct{}{}
		for _, session := range sessions.Items {
			if session.Spec.UserReference.Name != user.Name {
				others[session.Spec.UserReference.Name] = struct{}{}
			}
		}
		if len(others) >= s.MaxConcurrentSessions {
			return nil, busyMessage, nil
		}
	}

	session, err := create(ctx)
	createFailed = err != nil
	if err != nil || s.BusyCheck == nil {
		return session, "", err
	}

	// Check again now the Session exists: whatever BusyCheck guards (the
	// Library) may have started between the first check and the Create.
	// It checks for Sessions after creating itself, so whichever side looks
	// second sees the other. Backing out goes through onCreateError.
	reason, err := s.BusyCheck(ctx, user)
	if err != nil || reason != "" {
		createFailed = true
		if err != nil {
			return nil, "", fmt.Errorf("busy check failed: %w", err)
		}
		return nil, reason, nil
	}
	return session, "", nil
}

func (s *RESTServer) queueCleanup(launchID string) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	s.pendingCleanups = append(s.pendingCleanups, launchID)
}

// drainCleanups deletes the Sessions of queued failed launches. The caller
// must hold launchSlot. It stops when ctx is done and leaves what it didn't
// finish queued for the next slot holder, so it never outlasts the caller's
// own budget.
func (s *RESTServer) drainCleanups(ctx context.Context) {
	s.cleanupMu.Lock()
	ids := s.pendingCleanups
	s.pendingCleanups = nil
	s.cleanupMu.Unlock()

	for i, id := range ids {
		err := s.deleteLaunch(ctx, id)
		if err != nil && ctx.Err() != nil {
			s.cleanupMu.Lock()
			s.pendingCleanups = append(s.pendingCleanups, ids[i:]...)
			s.cleanupMu.Unlock()
			return
		}
		if err != nil {
			// Not retried; the operator's unstarted-session reaper is the backstop.
			klog.Errorf("Failed to clean up failed launch %s: %s", id, err)
		}
	}
}

// launchIDLabel marks a Session with the /launch request that created it.
const launchIDLabel = "direwolf/launch-id"

// deleteLaunch removes the Session(s) created by one /launch.
func (s *RESTServer) deleteLaunch(ctx context.Context, launchID string) error {
	sessions, err := s.SessionClient.List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{launchIDLabel: launchID}).String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}
	for _, session := range sessions.Items {
		if err := s.SessionClient.Delete(ctx, session.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete session %s: %w", session.Name, err)
		}
	}
	return nil
}

// streamRefusedError is the operator refusing ever to stream a Session as
// configured (SessionStreamRefusedReason): /launch and /resume answer at once
// rather than wait out LaunchTimeout.
type streamRefusedError struct {
	message    string
	generation int64
}

func (e *streamRefusedError) Error() string { return e.message }

// response tells the client why, as an XML status: moonlight-qt shows a
// non-2xx HTTP status only as a transport error.
func (e *streamRefusedError) response() Response {
	return Response{StatusCode: streamRefusedStatusCode, StatusMessage: e.message}
}

// streamRefusal is the Session's stream refusal, or nil. Other StreamStarted
// failures (e.g. pod not ready yet during a cold start) are worth waiting out.
func streamRefusal(session *v1alpha1types.Session) *streamRefusedError {
	cond := meta.FindStatusCondition(session.Status.Conditions, v1alpha1types.SessionStreamStartedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1types.SessionStreamRefusedReason {
		return nil
	}
	return &streamRefusedError{message: cond.Message, generation: cond.ObservedGeneration}
}

// closeLibrary answers a launch of the "Close Library" tile: it closes the
// Library of the library the user plays from. Any paired user of the shared
// library may close its Library: one shared desktop, and while it runs none of
// them can play. Nothing streams, so the answer is always a message.
func (s *RESTServer) closeLibrary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), libraryCallTimeout)
	defer cancel()
	closing, err := s.Library.Close(ctx, requestLibraryOwner(r))
	message := libraryNotOpenMessage
	switch {
	case err != nil:
		// The API error names the namespace and ServiceAccount; the user
		// gets a plain message, the log the detail.
		klog.Errorf("Failed to close the Library: %v", err)
		message = libraryCloseFailedMessage
	case closing:
		if user, ok := r.Context().Value(userContextKey{}).(*v1alpha1types.User); ok {
			klog.Infof("User %s closed the Library from Moonlight", user.Name)
		}
		message = libraryClosingMessage
	}
	sendXMLWithHTTPStatus(w, http.StatusOK, Response{StatusCode: libraryClosingStatusCode, StatusMessage: message})
}

// closeLibraryOffered reports whether the "Close Library" tile exists for a
// User of owner's library: a Library is configured, and no App they can see
// claims its ID.
func (s *RESTServer) closeLibraryOffered(owner string) bool {
	if s.Library == nil {
		return false
	}
	_, err := s.getAppByID(strconv.Itoa(CloseLibraryAppID), owner)
	return err != nil
}

// clientSupportsEncryptedRTSP reports whether the Moonlight client can speak
// Sunshine's encrypted RTSP: it sends corever>=1 on /launch and /resume.
func clientSupportsEncryptedRTSP(r *http.Request) bool {
	// Digits only, as Wolf parses it: Atoi alone would also take "+1".
	value := r.URL.Query().Get("corever")
	if strings.HasPrefix(value, "+") {
		return false
	}
	corever, err := strconv.Atoi(value)
	return err == nil && corever >= 1
}

func busyResponse(reason string) Response {
	return Response{StatusCode: busyStatusCode, StatusMessage: reason}
}

// resumeHandler re-attaches the client to its running session. The operator
// keeps a session's pod through a client disconnect (its grace period), so the
// game is still running. The client's new stream keys go into the Session's
// spec; the operator sees the generation bump and adds a Wolf session for
// them in the same pod. With no session to resume, this is a fresh launch.
func (s *RESTServer) resumeHandler(w http.ResponseWriter, r *http.Request) {
	rikey := r.URL.Query().Get("rikey")
	rikeyID := r.URL.Query().Get("rikeyid")
	if rikey == "" || rikeyID == "" {
		writeErrorResponse(w, 400, errors.New("rikey and rikeyid required"))
		return
	}
	clientIP, err := remoteIP(r)
	if err != nil {
		writeErrorResponse(w, 400, err)
		return
	}
	user, ok := r.Context().Value(userContextKey{}).(*v1alpha1types.User)
	if !ok {
		writeErrorResponse(w, 401, errors.New("user not found"))
		return
	}
	if refuseUnstreamableClient(w, clientIP) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.LaunchTimeout)
	defer cancel()

	// From the API server, not the informer: the operator may have just
	// ended the session.
	sessions, err := s.SessionClient.List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{"direwolf/user": user.Name}).String(),
	})
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to list sessions: %w", err))
		return
	}
	var session *v1alpha1types.Session
	for i := range sessions.Items {
		candidate := &sessions.Items[i]
		if candidate.DeletionTimestamp == nil &&
			(session == nil || session.CreationTimestamp.Before(&candidate.CreationTimestamp)) {
			session = candidate
		}
	}
	if session == nil {
		klog.Infof("No session to resume for user %s, launching", user.Name)
		s.launchHandler(w, r)
		return
	}

	var generation int64
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, getErr := s.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
		// Gone or ending since the List (cancelled from another device, or
		// reaped): answered as the wait below answers it.
		if k8serrors.IsNotFound(getErr) || (getErr == nil && live.DeletionTimestamp != nil) {
			return &streamRefusedError{message: sessionEndedMessage}
		}
		if getErr != nil {
			return fmt.Errorf("failed to get session: %w", getErr)
		}
		live.Spec.Config.AESKey = rikey
		live.Spec.Config.AESIV = rikeyID
		live.Spec.Config.ClientIP = clientIP
		live.Spec.Config.EncryptedRTSP = clientSupportsEncryptedRTSP(r)
		updated, updateErr := s.SessionClient.Update(ctx, live, metav1.UpdateOptions{FieldManager: "direwolf-resume"})
		if k8serrors.IsNotFound(updateErr) {
			return &streamRefusedError{message: sessionEndedMessage}
		}
		if updateErr != nil {
			return fmt.Errorf("failed to update session: %w", updateErr)
		}
		// Same keys as before (a retried request): the generation does not
		// move, so the operator would never re-attach a disconnected stream.
		if updated.Generation == live.Generation && updated.Status.WolfSessionID == "" {
			return &streamRefusedError{message: "The resume reused the previous stream keys; retry the resume"}
		}
		generation = updated.Generation
		return nil
	})
	if refusal := (*streamRefusedError)(nil); errors.As(err, &refusal) {
		klog.Infof("Resume of session %s for user %s refused: %s", session.Name, user.Name, refusal)
		sendXMLWithHTTPStatus(w, http.StatusOK, refusal.response())
		return
	}
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to resume session %s: %w", session.Name, err))
		return
	}
	klog.Infof("Resuming session %s for user %s", session.Name, user.Name)

	// Wait for the operator to attach the new keys.
	var streamURL string
	err = wait.PollUntilContextCancel(ctx, 250*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		live, getErr := s.SessionClient.Get(ctx, session.Name, metav1.GetOptions{})
		if k8serrors.IsNotFound(getErr) {
			// Cancelled from another device, or reaped.
			return false, &streamRefusedError{message: sessionEndedMessage}
		}
		if getErr != nil {
			return false, fmt.Errorf("failed to get session: %w", getErr)
		}
		if refusal := streamRefusal(live); refusal != nil && refusal.generation >= generation {
			return false, refusal
		}
		if live.Status.AttachedGeneration < generation || live.Status.WolfSessionID == "" || live.Status.StreamURL == "" {
			return false, nil
		}
		streamURL = live.Status.StreamURL
		return true, nil
	})
	if refusal := (*streamRefusedError)(nil); errors.As(err, &refusal) {
		klog.Infof("Resume of session %s for user %s refused: %s", session.Name, user.Name, refusal)
		sendXMLWithHTTPStatus(w, http.StatusOK, refusal.response())
		return
	}
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to resume session %s: %w", session.Name, err))
		return
	}

	sendXML(w, LaunchResponse{
		Response: Response{
			StatusCode: 200,
		},
		RTSPSessionURL: streamURL,
		Resume:         1,
	})
}

func (s *RESTServer) cancelHandler(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey{}).(*v1alpha1types.User)

	// Detached from r.Context(): finish the cancel even if the client hangs up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), launchSlotTimeout)
	defer cancel()
	err := s.stopSessionsForUser(ctx, user, true)
	if err != nil && !k8serrors.IsNotFound(err) {
		writeErrorResponse(w, 500, fmt.Errorf("failed to cancel session: %s", err))
		return
	}
	sendXML(w, CancelResponse{Response: Response{StatusCode: 200}, Cancel: 1})
}

func (s *RESTServer) stopSessionsForUser(ctx context.Context, user *v1alpha1types.User, shouldWait bool) error {
	// Live List, not the informer: a Session this user created moments ago
	// (a quick relaunch) may not be cached yet and would be left running.
	sessions, err := s.SessionClient.List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{"direwolf/user": user.Name}).String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	didDelete := false
	for _, session := range sessions.Items {
		delErr := s.SessionClient.Delete(ctx, session.Name, metav1.DeleteOptions{})
		if k8serrors.IsNotFound(delErr) {
			// Deleted by someone else since the List: still stop the rest.
			continue
		}
		if delErr != nil {
			return fmt.Errorf("failed to delete session: %w", delErr)
		}
		didDelete = true
	}

	if !didDelete {
		klog.Warningf("Not stopping any sessions. No sessions found for user %s", user.Name)
	}

	// Waited for even when no Session was left to delete: its pod may still be
	// terminating, and serverinfo reports busy until it is gone, which
	// Moonlight's quit-then-launch reads as a failed quit.
	if shouldWait {
		// Wait for pods to be cleaned up
		err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 25*time.Second, true, func(context.Context) (bool, error) {
			pods, err := s.PodLister.List(labels.SelectorFromSet(labels.Set{
				"direwolf/user": user.Name,
			}))
			if err != nil {
				return false, err
			}
			return len(pods) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("failed to wait for pods to be cleaned up: %w", err)
		}
	}

	if !didDelete {
		return k8serrors.NewNotFound(v1alpha1types.Resource("session"), user.Name)
	}
	return nil
}

func (s *RESTServer) appAssetHandler(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("appid")
	if appID == "" {
		writeAssetError(w, 400, errors.New("appid required"))
		return
	}

	app, err := s.getAppByID(appID, requestLibraryOwner(r))
	if err != nil {
		writeAssetError(w, 404, fmt.Errorf("app not found: %w", err))
		return
	}

	// Encoded before any header is written, so a failure is an error status
	// rather than a 200 with an empty body, and Content-Length is really sent.
	img, err := decodeAppAsset(app.Spec.AppAssetWebP)
	if err != nil {
		writeAssetError(w, http.StatusInternalServerError, fmt.Errorf("decode app asset for %s: %w", appID, err))
		return
	}

	pngData := new(bytes.Buffer)
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err = encoder.Encode(pngData, fitBoxArt(img)); err != nil {
		writeAssetError(w, http.StatusInternalServerError, fmt.Errorf("encode app asset for %s: %w", appID, err))
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(pngData.Len()))
	w.WriteHeader(http.StatusOK)
	if _, err = io.Copy(w, pngData); err != nil {
		klog.Infof("Failed to write png: %s", err)
		return
	}
	klog.Infof("Sent app asset for %s", appID)
}

// Box art is shown at a few hundred pixels, so a cover is served no larger
// than this. A catalogue cover can be far bigger (Epic's are 1200x1600), and
// encoding that as PNG on every request took ~8s: Moonlight gave up and
// showed no cover (XERK-1343).
const (
	boxArtMaxWidth  = 600
	boxArtMaxHeight = 800
)

// fitBoxArt scales img down, keeping its aspect ratio, to fit within
// boxArtMaxWidth x boxArtMaxHeight. A cover that already fits is unchanged.
// ApproxBiLinear, not CatmullRom: the kernel scalers allocate a dstW x srcH
// float buffer (~78MB for a 4096-tall cover), and Moonlight fetches four
// covers at once against the proxy's 256Mi limit.
func fitBoxArt(img image.Image) image.Image {
	return fitWithin(img, boxArtMaxWidth, boxArtMaxHeight)
}

// fitWithin scales img down, keeping its aspect ratio, to fit within
// maxW x maxH. An image that already fits is unchanged.
func fitWithin(img image.Image, maxW, maxH int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW && h <= maxH {
		return img
	}
	scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

// decodeAppAsset decodes an App's cover: WebP, or the PNG/JPEG box art the
// catalogue fetches. A JPEG is turned upright per its EXIF orientation, which
// image/jpeg ignores (a portrait photo would otherwise be served sideways).
func decodeAppAsset(data []byte) (image.Image, error) {
	var errs []error
	for _, decode := range []func(io.Reader) (image.Image, error){webp.Decode, png.Decode, jpeg.Decode} {
		img, err := decode(bytes.NewReader(data))
		if err == nil {
			if o := jpegOrientation(data); o > 1 {
				img = orient(img, o)
			}
			return img, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// jpegOrientation returns the EXIF Orientation (1-8) of a JPEG, or 1 when
// data is not a JPEG or carries none. It reads only IFD0's 0x0112 tag from the
// APP1 "Exif" segment; anything malformed counts as upright.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xDA || marker == 0xD9 { // start of scan / end of image
			return 1
		}
		size := int(data[i+2])<<8 | int(data[i+3])
		if size < 2 || i+2+size > len(data) {
			return 1
		}
		if seg := data[i+4 : i+2+size]; marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return exifOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

// exifOrientation reads the Orientation tag from IFD0 of a TIFF header.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[ifd:]))
	for e := ifd + 2; e+12 <= len(tiff) && count > 0; e, count = e+12, count-1 {
		// Tag 0x0112, type SHORT (3): the value sits in the entry's first two value bytes.
		if order.Uint16(tiff[e:]) == 0x0112 && order.Uint16(tiff[e+2:]) == 3 {
			if o := int(order.Uint16(tiff[e+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 1
		}
	}
	return 1
}

// orient returns img turned upright for EXIF orientation o (2-8). For
// orientations that swap width and height (5-8), img is first scaled to fit
// the box art's swapped bounds, so the copy made here stays box-art sized.
func orient(img image.Image, o int) image.Image {
	if o >= 5 {
		img = fitWithin(img, boxArtMaxHeight, boxArtMaxWidth)
	} else {
		img = fitBoxArt(img)
	}
	// Copied pixel by pixel from an RGBA: At/Set allocate a color each.
	src, ok := img.(*image.RGBA)
	if !ok {
		src = image.NewRGBA(img.Bounds())
		draw.Draw(src, src.Bounds(), img, img.Bounds().Min, draw.Src)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			// The source pixel shown at (x, y) once upright.
			var sx, sy int
			switch o {
			case 2: // mirrored horizontally
				sx, sy = w-1-x, y
			case 3: // rotated 180
				sx, sy = w-1-x, h-1-y
			case 4: // mirrored vertically
				sx, sy = x, h-1-y
			case 5: // transposed
				sx, sy = y, x
			case 6: // needs 90 clockwise
				sx, sy = y, h-1-x
			case 7: // transversed
				sx, sy = w-1-y, h-1-x
			case 8: // needs 90 counter-clockwise
				sx, sy = w-1-y, x
			default:
				return img
			}
			copy(dst.Pix[dst.PixOffset(x, y):][:4], src.Pix[src.PixOffset(b.Min.X+sx, b.Min.Y+sy):])
		}
	}
	return dst
}

// writeErrorResponse answers err as HTTP 200 with status in the XML
// status_code, as Sunshine does: moonlight-qt turns a non-2xx HTTP status into
// a transport error and never shows the message.
//
// A 5xx's detail (API server errors naming Sessions, namespaces, RBAC) goes
// only to the log; the client, which then shows it, gets internalErrorMessage.
func writeErrorResponse(w http.ResponseWriter, status int, err error) {
	klog.Error(err)

	message := err.Error()
	if status >= http.StatusInternalServerError {
		message = internalErrorMessage
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml.Header))

	// First attempt to serialize as XML with the message. If that fails, just
	// send unfallible statuscode
	if bytes, err := xml.Marshal(Response{
		StatusCode:    status,
		StatusMessage: message,
	}); err == nil {
		w.Write(bytes)
	} else {
		klog.ErrorS(err, "Failed to marshal XML. Just sending status code")
		w.Write(fmt.Appendf(nil, `<root status_code="%d"></root>`, status))
	}

	klog.ErrorS(err, "Sent error response", "status", status)
}

// sendXML answers resp as HTTP 200 whatever its status_code (e.g. a failed
// pairing step), for the reason writeErrorResponse gives.
func sendXML(w http.ResponseWriter, resp Responsable) {
	sendXMLWithHTTPStatus(w, http.StatusOK, resp)
}

// writeAssetError answers a failed /appasset. It is an image, not XML, so the
// error is a real HTTP status: the client then shows no cover rather than
// trying to decode an error body.
func writeAssetError(w http.ResponseWriter, status int, err error) {
	klog.ErrorS(err, "Sent app asset error", "status", status)
	http.Error(w, http.StatusText(status), status)
}

func sendXMLWithHTTPStatus(w http.ResponseWriter, httpStatus int, resp Responsable) { //nolint:misspell // existing type name
	bytes, err := xml.Marshal(resp)
	if err != nil {
		writeErrorResponse(w, 500, fmt.Errorf("failed to marshal XML: %s", err))
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(httpStatus)
	w.Write([]byte(xml.Header))
	w.Write(bytes)

	// Status only: bodies carry the server's half of the pairing handshake
	// (challengeresponse, pairingsecret).
	klog.Infof("Sent response: status %d", resp.GetStatusCode())
}

// secretQueryParams carry key material: the stream's AES key/IV (/launch,
// /resume) and the pairing handshake's secrets. Anyone with the logs and a
// capture of the stream could decrypt it, or replay a pairing step.
var secretQueryParams = []string{
	"rikey", "rikeyid",
	"salt", "clientcert", "clientchallenge", "serverchallengeresp", "clientpairingsecret",
	"pin",
}

// redactedQuery returns q with secretQueryParams' values replaced, for logging.
func redactedQuery(q url.Values) url.Values {
	out := maps.Clone(q)
	for _, k := range secretQueryParams {
		if _, ok := out[k]; ok {
			out[k] = []string{"REDACTED"}
		}
	}
	return out
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if r.URL.Path != "/serverinfo" {

			klog.Infof("%s %s %s %v %s", r.Proto, r.Method, r.URL.Path, redactedQuery(r.URL.Query()), r.RemoteAddr)
			next.ServeHTTP(w, r)
			klog.Infof("Completed in %s", time.Since(start))
		} else {
			next.ServeHTTP(w, r)
		}
	})
}

type userContextKey struct{}
type pairingContextKey struct{}

// Grabs fingerprint of client cert, finds associated user in backend and attaches
// it to the request context.
func (s *RESTServer) authenticatedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeErrorResponse(w, 401, fmt.Errorf("client cert required"))
			return
		}

		fingerprint := hex.EncodeToString(util.Hash(r.TLS.PeerCertificates[0].Raw))
		pairing, err := s.PairingsLister.Get(fingerprint)
		if err != nil {
			writeErrorResponse(w, 401, fmt.Errorf("client %s not paired", fingerprint))
			return
		}

		user, err := s.UserLister.Get(pairing.Spec.UserReference.Name)
		if err != nil {
			writeErrorResponse(w, 401, fmt.Errorf("user not found"))
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		ctx = context.WithValue(ctx, pairingContextKey{}, pairing)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// getAppByID is the App with Moonlight ID appID a User of owner's library can
// see. Two Users' own copies of a catalogued game share its ID.
func (s *RESTServer) getAppByID(appID, owner string) (*v1alpha1types.App, error) {
	intParsedAppID, err := strconv.Atoi(appID)
	if err != nil {
		return nil, fmt.Errorf("app id must be an integer")
	}

	apps, err := s.AppLister.List(nil)
	if err != nil {
		return nil, err
	}

	// A hidden App is not in the app list, so it can't be launched either.
	for _, app := range apps {
		if app.Spec.ID == intParsedAppID && !app.Spec.Hidden && v1alpha1types.AppVisibleTo(app, owner) {
			return app, nil
		}
	}

	return nil, fmt.Errorf("app not found")
}
