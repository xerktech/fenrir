package moonlight

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// The VR front door (XERK-2808): an mTLS API on its own port, where a headset
// running the xerktech/WiVRn Quest app pairs once (its user types the code it
// shows into the pairing page, behind Authentik), then lists the User's VR
// Apps, launches one as a native VR Session and learns where to connect and
// which server key to pin. A headset is its TLS client key: the Headset that
// holds that key's hash (spec.clientKeySHA256) names its User, so deleting the
// Headset shuts the door on it, and the operator ends its Sessions.
//
// Pairing is a short-authentication-string exchange with a commitment, so a
// LAN attacker in the middle of the first TLS connection can't get its own
// key paired with the code the headset shows:
//
//  1. POST VRPairPath {publicKey, name, commitment = SHA-256(headset nonce)}:
//     the server answers a random server nonce.
//  2. POST VRPairConfirmPath {pairingID, nonce}: the server checks the nonce
//     against the commitment and holds the request until the user enters the
//     code, or VRPairWait passes.
//
// Both sides compute the code (vrPairingCode) from both TLS keys, the WiVRn
// key and both nonces. An attacker relaying between the two must fix the
// server nonce it shows the headset before the headset reveals its nonce, so
// it can't steer the headset's code. It can still draw server-side codes: each
// POST VRPairPath hands it a new server nonce, and it knows every other input.
// So server nonces are handed out at most one per vrPairDrawEvery, server-wide
// (d.draws): in the minutes the user takes to type the code, a relay gets a
// few dozen draws at 1 in 10^8 each. Deriving the code from the certificates
// alone would let it grind certificates offline, at any rate.
const (
	VRPairPath        = "/vr/v1/pair"
	VRPairConfirmPath = "/vr/v1/pair/confirm"
	VRAppsPath        = "/vr/v1/apps"
	VRLaunchPath      = "/vr/v1/launch"
	VRSessionPath     = "/vr/v1/session"
	VRStopPath        = "/vr/v1/stop"
	// VRPairWait is how long a confirm waits for its code to be entered.
	VRPairWait = 3 * time.Minute
	// vrPairCommitTTL drops a pairing whose confirm never came.
	vrPairCommitTTL = time.Minute
	// vrMaxPending bounds pairings in progress: pairing needs no
	// credentials, so anyone reaching the port can start one.
	vrMaxPending = 16
	// vrPairDrawEvery and vrPairDrawBurst bound how fast server nonces are
	// handed out, server-wide (see above). A user re-trying a pairing waits a
	// few seconds at most.
	vrPairDrawEvery = 5 * time.Second
	vrPairDrawBurst = 3
	// vrCodeFailures wrong codes per vrCodeFailureWindow lock a User out of
	// entering more, so an account can't guess another's waiting code.
	vrCodeFailures      = 5
	vrCodeFailureWindow = 10 * time.Minute
	// vrCodeDigits is the code's length.
	vrCodeDigits = 8
	// vrBodyMax bounds a request body: a WiVRn key is at most 1024 bytes.
	vrBodyMax = 4096
	// vrCacheWait bounds the wait for a new Headset to reach the informer,
	// so the headset's next call is admitted.
	vrCacheWait = 5 * time.Second
	// vrCallTimeout bounds an API request's own work.
	vrCallTimeout = 30 * time.Second
	// vrPairingDomain separates the code's hash from any other use.
	vrPairingDomain = "direwolf-vr-pair-v1"
)

// VRSessionInfo is a User's VR Session as the front door reports it
// (controllers.VRSessionState).
type VRSessionInfo struct {
	State            string `json:"state"`
	App              string `json:"app,omitempty"`
	Title            string `json:"title,omitempty"`
	Host             string `json:"host,omitempty"`
	Port             int32  `json:"port,omitempty"`
	ServerKey        string `json:"serverKey,omitempty"`
	HeadsetConnected bool   `json:"headsetConnected"`
	Message          string `json:"message,omitempty"`
}

// VRSessions starts, stops and reports a User's native VR Session, with the
// VR page's checks (controllers.VRLaunch).
type VRSessions interface {
	VRApps(user *v1alpha1types.User) []*v1alpha1types.App
	// Launch returns why the session can't start, or "" once it is created
	// (or already runs).
	Launch(ctx context.Context, user *v1alpha1types.User, app string) (string, error)
	Stop(ctx context.Context, user *v1alpha1types.User) error
	SessionState(ctx context.Context, user *v1alpha1types.User) (VRSessionInfo, error)
}

// VRFrontDoorOptions turns on the VR front door. Port 0 leaves it off.
type VRFrontDoorOptions struct {
	Port int
	// Address is the one address it listens on, never a wildcard: under
	// hostNetwork that would include the node's loopback.
	Address string
	// PairingURL is the pairing page's address, which the headset shows
	// with its code. Optional.
	PairingURL string

	Headsets      generic.NamespacedLister[*v1alpha1types.Headset]
	HeadsetClient v1alpha1client.HeadsetInterface
	Sessions      VRSessions
}

// vrFrontDoor serves the front door; the pairing page enters codes into it.
type vrFrontDoor struct {
	opts  VRFrontDoorOptions
	users generic.NamespacedLister[*v1alpha1types.User]
	// serverKey is SHA-256 of the serving certificate's
	// SubjectPublicKeyInfo, as the headset sees it.
	serverKey [32]byte

	// draws bounds server nonces handed out (vrPairDrawEvery).
	draws *rate.Limiter

	mu       sync.Mutex
	pending  map[string]*vrPendingPair
	failures map[string][]time.Time
}

// vrPendingPair is a pairing in progress.
type vrPendingPair struct {
	id          string
	clientKey   string
	publicKey   string
	name        string
	commitment  [32]byte
	serverNonce [32]byte
	created     time.Time
	// code is set once the headset revealed its nonce and waits.
	code string
	// claimed is set while a code entry creates its Headset.
	claimed bool
	// done gets the Headset's name once paired.
	done chan string
}

func newVRFrontDoor(opts *VRFrontDoorOptions, users generic.NamespacedLister[*v1alpha1types.User], cert *tls.Certificate) (*vrFrontDoor, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("no serving certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parsing the serving certificate: %w", err)
	}
	return &vrFrontDoor{
		opts: *opts, users: users, serverKey: sha256.Sum256(leaf.RawSubjectPublicKeyInfo),
		pending: map[string]*vrPendingPair{}, failures: map[string][]time.Time{},
		draws: rate.NewLimiter(rate.Every(vrPairDrawEvery), vrPairDrawBurst),
	}, nil
}

// handler is the front door's API. Every route needs a client certificate
// (the TLS config requires one); all but pairing need a paired one.
func (d *vrFrontDoor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+VRPairPath, d.pair)
	mux.HandleFunc("POST "+VRPairConfirmPath, d.confirm)
	mux.Handle("GET "+VRAppsPath, d.authenticated(d.apps))
	mux.Handle("GET "+VRAppsPath+"/{app}/cover", d.authenticated(d.cover))
	mux.Handle("POST "+VRLaunchPath, d.authenticated(d.launch))
	mux.Handle("GET "+VRSessionPath, d.authenticated(d.session))
	mux.Handle("POST "+VRStopPath, d.authenticated(d.stop))
	return mux
}

// tlsConfig serves cert and asks every client for a certificate, which is
// its identity: nothing about it is verified but the key it proves.
func vrTLSConfig(cert *tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{*cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// clientKey is SHA-256 of the client certificate's SubjectPublicKeyInfo, in
// lowercase hex: a headset may renew its certificate and keep its key.
func clientKey(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), true
}

func writeVRJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		klog.V(2).Infof("VR front door: writing a reply: %v", err)
	}
}

func writeVRError(w http.ResponseWriter, status int, msg string) {
	writeVRJSON(w, status, map[string]string{"error": msg})
}

// decodeVRBody reads r's JSON body into v, refusing anything else.
func decodeVRBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeVRError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, vrBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeVRError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

// headsetFor is the live Headset holding key, or nil. Two holding one key
// (two Users' pairings racing) admit neither: fail closed.
func (d *vrFrontDoor) headsetFor(key string) (*v1alpha1types.Headset, error) {
	all, err := d.opts.Headsets.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("listing headsets: %w", err)
	}
	var found *v1alpha1types.Headset
	for _, h := range all {
		if h.Spec.ClientKeySHA256 != key || h.DeletionTimestamp != nil {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("headsets %s and %s both hold client key %s", found.Name, h.Name, key)
		}
		found = h
	}
	return found, nil
}

type vrUserKey struct{}

// authenticated admits a client whose key a Headset holds, as that
// Headset's User.
func (d *vrFrontDoor) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := clientKey(r)
		if !ok {
			writeVRError(w, http.StatusUnauthorized, "client certificate required")
			return
		}
		h, err := d.headsetFor(key)
		if err != nil {
			klog.Errorf("VR front door: %v", err)
		}
		if h == nil {
			writeVRError(w, http.StatusUnauthorized, "this headset is not paired")
			return
		}
		user, err := d.users.Get(h.Spec.UserReference.Name)
		if err != nil {
			writeVRError(w, http.StatusUnauthorized, "this headset is not paired")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), vrCallTimeout)
		defer cancel()
		next(w, r.WithContext(context.WithValue(ctx, vrUserKey{}, user)))
	})
}

// vrUser is the User authenticated set on r's context.
func vrUser(r *http.Request) *v1alpha1types.User {
	u, _ := r.Context().Value(vrUserKey{}).(*v1alpha1types.User)
	return u
}

type vrApp struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Cover is the path of its cover image (PNG), on this server.
	Cover string `json:"cover,omitempty"`
}

func (d *vrFrontDoor) apps(w http.ResponseWriter, r *http.Request) {
	apps := d.opts.Sessions.VRApps(vrUser(r))
	res := make([]vrApp, 0, len(apps))
	for _, a := range apps {
		app := vrApp{ID: a.Name, Title: cmp.Or(a.Spec.Title, a.Name)}
		if len(a.Spec.AppAssetWebP) > 0 {
			app.Cover = VRAppsPath + "/" + a.Name + "/cover"
		}
		res = append(res, app)
	}
	writeVRJSON(w, http.StatusOK, map[string]any{"apps": res})
}

// vrAppFor is name if user may play it in VR.
func (d *vrFrontDoor) vrAppFor(user *v1alpha1types.User, name string) *v1alpha1types.App {
	apps := d.opts.Sessions.VRApps(user)
	if i := slices.IndexFunc(apps, func(a *v1alpha1types.App) bool { return a.Name == name }); i >= 0 {
		return apps[i]
	}
	return nil
}

func (d *vrFrontDoor) cover(w http.ResponseWriter, r *http.Request) {
	app := d.vrAppFor(vrUser(r), r.PathValue("app"))
	if app == nil || len(app.Spec.AppAssetWebP) == 0 {
		writeVRError(w, http.StatusNotFound, "no such VR app cover")
		return
	}
	writeBoxArt(w, app)
}

func (d *vrFrontDoor) launch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		App string `json:"app"`
	}
	if !decodeVRBody(w, r, &req) {
		return
	}
	user := vrUser(r)
	refusal, err := d.opts.Sessions.Launch(r.Context(), user, req.App)
	if err != nil {
		klog.Errorf("VR front door: user %s: launching %q: %v", user.Name, req.App, err)
		writeVRError(w, http.StatusInternalServerError, "Try again in a minute.")
		return
	}
	if refusal != "" {
		writeVRError(w, http.StatusConflict, refusal)
		return
	}
	d.writeSession(w, r.WithContext(context.WithValue(r.Context(), vrLaunchedKey{}, true)), http.StatusAccepted)
}

// vrLaunchedKey marks a request whose launch just succeeded.
type vrLaunchedKey struct{}

func (d *vrFrontDoor) session(w http.ResponseWriter, r *http.Request) {
	d.writeSession(w, r, http.StatusOK)
}

func (d *vrFrontDoor) writeSession(w http.ResponseWriter, r *http.Request, status int) {
	user := vrUser(r)
	st, err := d.opts.Sessions.SessionState(r.Context(), user)
	if err != nil {
		klog.Errorf("VR front door: user %s: %v", user.Name, err)
		writeVRError(w, http.StatusInternalServerError, "Try again in a minute.")
		return
	}
	// The state is read from the informer, which may not have seen the
	// Session the launch just created.
	if launched, _ := r.Context().Value(vrLaunchedKey{}).(bool); launched && st.State == "none" {
		st = VRSessionInfo{State: "starting", Message: st.Message}
	}
	writeVRJSON(w, status, st)
}

func (d *vrFrontDoor) stop(w http.ResponseWriter, r *http.Request) {
	user := vrUser(r)
	if err := d.opts.Sessions.Stop(r.Context(), user); err != nil {
		klog.Errorf("VR front door: user %s: stopping: %v", user.Name, err)
		writeVRError(w, http.StatusInternalServerError, "Try again in a minute.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseHex32 decodes 64 hex digits.
func parseHex32(s string) ([32]byte, bool) {
	var out [32]byte
	if len(s) != 64 {
		return out, false
	}
	_, err := hex.Decode(out[:], []byte(s))
	return out, err == nil
}

// sweepLocked drops pairings whose confirm never came, or whose code
// outlived its wait (a failed Headset create keeps it). d.mu is held.
func (d *vrFrontDoor) sweepLocked(now time.Time) {
	for id, p := range d.pending {
		switch {
		case p.code == "" && now.Sub(p.created) > vrPairCommitTTL,
			!p.claimed && now.Sub(p.created) > vrPairCommitTTL+VRPairWait+vrCallTimeout:
			delete(d.pending, id)
		}
	}
}

// pair starts a pairing: it records the headset's keys and commitment and
// answers the server nonce.
func (d *vrFrontDoor) pair(w http.ResponseWriter, r *http.Request) {
	key, ok := clientKey(r)
	if !ok {
		writeVRError(w, http.StatusUnauthorized, "client certificate required")
		return
	}
	var req struct {
		PublicKey  string `json:"publicKey"`
		Name       string `json:"name"`
		Commitment string `json:"commitment"`
	}
	if !decodeVRBody(w, r, &req) {
		return
	}
	commitment, ok := parseHex32(req.Commitment)
	if !ok || !v1alpha1types.IsWiVRnKey(req.PublicKey) {
		writeVRError(w, http.StatusBadRequest, "publicKey must be WiVRn's key (its PEM's base64 body) and commitment 64 hex digits")
		return
	}
	if h, err := d.headsetFor(key); err != nil || h != nil {
		if err != nil {
			klog.Errorf("VR front door: %v", err)
		}
		writeVRError(w, http.StatusConflict, "this headset is already paired")
		return
	}
	// Each answer is a draw a relay could match against the code the headset
	// shows (see the top of this file): never more than vrPairDrawEvery.
	if !d.draws.Allow() {
		w.Header().Set("Retry-After", strconv.Itoa(int(vrPairDrawEvery/time.Second)))
		writeVRError(w, http.StatusTooManyRequests, "pairing is busy; try again in a few seconds")
		return
	}
	p := &vrPendingPair{
		clientKey: key, publicKey: req.PublicKey, name: v1alpha1types.CleanHeadsetName(req.Name),
		commitment: commitment, created: time.Now(), done: make(chan string, 1),
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		writeVRError(w, http.StatusInternalServerError, "Try again in a minute.")
		return
	}
	if _, err := rand.Read(p.serverNonce[:]); err != nil {
		writeVRError(w, http.StatusInternalServerError, "Try again in a minute.")
		return
	}
	p.id = hex.EncodeToString(id[:])

	d.mu.Lock()
	d.sweepLocked(p.created)
	// One pairing per client key: a new one replaces any it left.
	for oid, o := range d.pending {
		if o.clientKey == key && !o.claimed {
			delete(d.pending, oid)
		}
	}
	if len(d.pending) >= vrMaxPending {
		d.mu.Unlock()
		writeVRError(w, http.StatusServiceUnavailable, "too many headsets are pairing; try again in a few minutes")
		return
	}
	d.pending[p.id] = p
	d.mu.Unlock()

	resp := map[string]string{"pairingID": p.id, "serverNonce": hex.EncodeToString(p.serverNonce[:])}
	if d.opts.PairingURL != "" {
		resp["pairingURL"] = d.opts.PairingURL
	}
	klog.Infof("VR front door: headset %s (%q) started pairing", key[:16], p.name)
	writeVRJSON(w, http.StatusOK, resp)
}

// vrPairingCode is the code both sides show for a pairing: the first 8 bytes
// of SHA-256 over the domain, a NUL, the server's and the client's TLS key
// hashes, SHA-256 of the WiVRn key (its base64 text), the server nonce and
// the headset nonce, as a big-endian integer mod 10^8, in 8 digits with
// leading zeros. The headset app computes the same (XERK-2809).
func vrPairingCode(serverKey [32]byte, clientKey [32]byte, wivrnKey string, serverNonce, headsetNonce [32]byte) string {
	h := sha256.New()
	h.Write([]byte(vrPairingDomain))
	h.Write([]byte{0})
	h.Write(serverKey[:])
	h.Write(clientKey[:])
	wk := sha256.Sum256([]byte(wivrnKey))
	h.Write(wk[:])
	h.Write(serverNonce[:])
	h.Write(headsetNonce[:])
	sum := h.Sum(nil)
	return fmt.Sprintf("%0*d", vrCodeDigits, binary.BigEndian.Uint64(sum[:8])%100_000_000)
}

// vrKeyFingerprint is how the pairing page shows the front door's key
// (SHA-256 of its SPKI), for the user to compare with the key the headset
// pinned (XERK-2813): the first 16 bytes in hex, in groups of 4. The code
// proves nothing to the headset, since a fake front door computes it too; the
// page is behind Authentik, so only it can tell the user which key is real.
// The Quest app shows the same for the key it pinned: keep the two in step.
// 128 bits, so no LAN attacker grinds a key with a matching fingerprint.
func vrKeyFingerprint(sum [32]byte) string {
	hexed := hex.EncodeToString(sum[:16])
	parts := make([]string, 0, len(hexed)/4)
	for i := 0; i < len(hexed); i += 4 {
		parts = append(parts, hexed[i:i+4])
	}
	return strings.Join(parts, " ")
}

// confirm checks the headset's nonce against its commitment, then waits for
// its user to enter the code on the pairing page.
func (d *vrFrontDoor) confirm(w http.ResponseWriter, r *http.Request) {
	key, ok := clientKey(r)
	if !ok {
		writeVRError(w, http.StatusUnauthorized, "client certificate required")
		return
	}
	var req struct {
		PairingID string `json:"pairingID"`
		Nonce     string `json:"nonce"`
	}
	if !decodeVRBody(w, r, &req) {
		return
	}
	nonce, ok := parseHex32(req.Nonce)
	if !ok {
		writeVRError(w, http.StatusBadRequest, "nonce must be 64 hex digits")
		return
	}
	sum := sha256.Sum256(nonce[:])

	d.mu.Lock()
	p := d.pending[req.PairingID]
	switch {
	case p == nil || p.clientKey != key:
		d.mu.Unlock()
		writeVRError(w, http.StatusNotFound, "that pairing is no longer waiting; start pairing again")
		return
	case p.code != "":
		// Revealed once: a second reveal could be an attacker's retry.
		delete(d.pending, p.id)
		d.mu.Unlock()
		writeVRError(w, http.StatusConflict, "that pairing was already confirmed; start pairing again")
		return
	case subtle.ConstantTimeCompare(sum[:], p.commitment[:]) != 1:
		delete(d.pending, p.id)
		d.mu.Unlock()
		writeVRError(w, http.StatusForbidden, "the nonce does not match the commitment; start pairing again")
		return
	}
	ck, _ := parseHex32(key)
	p.code = vrPairingCode(d.serverKey, ck, p.publicKey, p.serverNonce, nonce)
	d.mu.Unlock()

	timer := time.NewTimer(VRPairWait)
	defer timer.Stop()
	select {
	case name := <-p.done:
		d.waitForHeadset(r.Context(), name)
		klog.Infof("VR front door: headset %s paired as %s", key[:16], name)
		writeVRJSON(w, http.StatusOK, map[string]any{"paired": true})
		return
	case <-timer.C:
	case <-r.Context().Done():
	}
	d.mu.Lock()
	claimed := p.claimed
	if !claimed {
		delete(d.pending, p.id)
	}
	d.mu.Unlock()
	if claimed {
		// The code was entered as the wait ran out: see it through.
		select {
		case name := <-p.done:
			d.waitForHeadset(r.Context(), name)
			writeVRJSON(w, http.StatusOK, map[string]any{"paired": true})
			return
		case <-r.Context().Done():
			return
		case <-time.After(vrCallTimeout):
			// The Headset's create failed; the code stays valid a while.
		}
	}
	writeVRError(w, http.StatusRequestTimeout, "nobody entered the code in time; start pairing again")
}

// waitForHeadset waits, briefly, until the informer has Headset name, so
// the headset's next call is admitted.
func (d *vrFrontDoor) waitForHeadset(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vrCacheWait)
	defer cancel()
	for {
		if _, err := d.opts.Headsets.Get(name); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var (
	errVRNoSuchCode   = errors.New("no headset is waiting with that code; check it, or start pairing again on the headset")
	errVRTooManyCodes = errors.New("too many wrong codes; wait a few minutes and try again")
)

// enterCode pairs the headset waiting with code to user, as the pairing page
// asks. A wrong code counts toward the User's limit.
func (d *vrFrontDoor) enterCode(ctx context.Context, user *v1alpha1types.User, code string) error {
	now := time.Now()
	d.mu.Lock()
	recent := slices.DeleteFunc(d.failures[user.Name], func(t time.Time) bool { return now.Sub(t) > vrCodeFailureWindow })
	d.failures[user.Name] = recent
	if len(recent) >= vrCodeFailures {
		d.mu.Unlock()
		return errVRTooManyCodes
	}
	var match *vrPendingPair
	for _, p := range d.pending {
		// Every waiting code is compared, matching or not.
		if p.code != "" && !p.claimed && subtle.ConstantTimeCompare([]byte(p.code), []byte(code)) == 1 {
			match = p
		}
	}
	if match == nil {
		d.failures[user.Name] = append(recent, now)
		d.mu.Unlock()
		return errVRNoSuchCode
	}
	match.claimed = true
	d.mu.Unlock()

	name, err := d.createHeadset(ctx, user, match)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		match.claimed = false
		return err
	}
	delete(d.pending, match.id)
	match.done <- name
	return nil
}

// frontDoorHeadsetName names the Headset of user's client key: the same on a
// retry, so a repeat finds the one it made.
func frontDoorHeadsetName(user, clientKey string) string {
	sum := sha256.Sum256([]byte("front-door\x00" + user + "\x00" + clientKey))
	return "headset-" + hex.EncodeToString(sum[:10])
}

// createHeadset records p as user's Headset.
func (d *vrFrontDoor) createHeadset(ctx context.Context, user *v1alpha1types.User, p *vrPendingPair) (string, error) {
	// Another User may have paired this key meanwhile.
	if h, err := d.headsetFor(p.clientKey); err != nil || (h != nil && h.Spec.UserReference.Name != user.Name) {
		return "", errors.New("this headset is already paired")
	}
	name := frontDoorHeadsetName(user.Name, p.clientKey)
	spec := v1alpha1types.HeadsetSpec{
		UserReference:   v1alpha1types.UserReference{Name: user.Name},
		PublicKey:       p.publicKey,
		DisplayName:     p.name,
		ClientKeySHA256: p.clientKey,
	}
	_, err := d.opts.HeadsetClient.Create(ctx, &v1alpha1types.Headset{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			// Garbage collected with its User: without an owner it would
			// outlive the User and pass to a new one of its name.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1types.GroupVersion.String(), Kind: "User", Name: user.Name, UID: user.UID,
			}},
		},
		Spec: spec,
	}, metav1.CreateOptions{FieldManager: "direwolf-vr-front-door"})
	if apierrors.IsAlreadyExists(err) {
		h, getErr := d.opts.HeadsetClient.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("getting headset %s: %w", name, getErr)
		}
		if h.Spec.UserReference.Name != user.Name || h.Spec.ClientKeySHA256 != p.clientKey || h.Spec.PublicKey != p.publicKey {
			return "", fmt.Errorf("headset %s exists for another key", name)
		}
		// Still being revoked: it would be gone in a moment, and the headset
		// with it. Pair again once it is.
		if h.DeletionTimestamp != nil {
			return "", fmt.Errorf("headset %s is being deleted", name)
		}
		err = nil
	}
	if err != nil {
		return "", fmt.Errorf("creating headset %s: %w", name, err)
	}
	klog.Infof("VR front door: user %s paired headset %s (%q)", user.Name, name, p.name)
	return name, nil
}

// normalizeVRCode is code without the spaces or dashes a user may type.
func normalizeVRCode(code string) (string, bool) {
	code = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, code)
	if len(code) != vrCodeDigits {
		return "", false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return code, true
}
