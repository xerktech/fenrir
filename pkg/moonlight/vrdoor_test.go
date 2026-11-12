package moonlight

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

const testWiVRnKey = "MEMwBQYDK2VvAzoAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"direwolf"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeVRSessions records what the front door asks of the launcher.
type fakeVRSessions struct {
	mu       sync.Mutex
	apps     []*v1alpha1types.App
	launched []string
	stopped  []string
	refusal  string
	state    VRSessionInfo
}

func (f *fakeVRSessions) VRApps(*v1alpha1types.User) []*v1alpha1types.App { return f.apps }
func (f *fakeVRSessions) Launch(_ context.Context, u *v1alpha1types.User, app string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusal != "" {
		return f.refusal, nil
	}
	f.launched = append(f.launched, u.Name+"/"+app)
	return "", nil
}
func (f *fakeVRSessions) Stop(_ context.Context, u *v1alpha1types.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, u.Name)
	return nil
}
func (f *fakeVRSessions) SessionState(context.Context, *v1alpha1types.User) (VRSessionInfo, error) {
	return f.state, nil
}

type vrDoorFixture struct {
	door     *vrFrontDoor
	server   *httptest.Server
	pinPage  http.Handler
	headsets cache.Indexer
	client   *fake.Clientset
	sessions *fakeVRSessions
}

func newVRDoorFixture(t *testing.T, users ...string) *vrDoorFixture {
	t.Helper()
	userIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, name := range users {
		if err := userIdx.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: k8stypes.UID("uid-" + name)}}); err != nil {
			t.Fatal(err)
		}
	}
	f := &vrDoorFixture{
		headsets: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		client:   fake.NewSimpleClientset(),
		sessions: &fakeVRSessions{apps: []*v1alpha1types.App{{ObjectMeta: metav1.ObjectMeta{Name: "assetto-corsa-vr"}, Spec: v1alpha1types.AppSpec{Title: "Assetto Corsa"}}}},
	}
	// Created Headsets reach the "informer" at once.
	f.client.PrependReactor("create", "headsets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if h, ok := a.(k8stesting.CreateAction).GetObject().(*v1alpha1types.Headset); ok {
			h = h.DeepCopy()
			h.Namespace = testNamespace
			_ = f.headsets.Add(h)
		}
		return false, nil, nil
	})
	userLister := generic.NewLister[*v1alpha1types.User](userIdx).Namespaced(testNamespace)
	cert := testCert(t)
	door, err := newVRFrontDoor(&VRFrontDoorOptions{
		Port: 1, Address: "127.0.0.1", PairingURL: "https://pair.example/pin/",
		Headsets:      generic.NewLister[*v1alpha1types.Headset](f.headsets).Namespaced(testNamespace),
		HeadsetClient: f.client.DirewolfV1alpha1().Headsets(testNamespace),
		Sessions:      f.sessions,
	}, userLister, &cert)
	if err != nil {
		t.Fatal(err)
	}
	f.door = door
	// Tests pair many times over; TestVRFrontDoorLimitsDraws covers the limit.
	door.draws = rate.NewLimiter(rate.Inf, 0)
	f.server = httptest.NewUnstartedServer(door.handler())
	f.server.TLS = vrTLSConfig(&cert)
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.pinPage = newPinPageHandler(NewPairingManager(tls.Certificate{}, nil), userLister, &PinPageOptions{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		ProxySecret:    []byte(proxySecret),
	}, door)
	return f
}

// headset is a test headset: its TLS client and WiVRn key.
type headset struct {
	client *http.Client
	key    [32]byte
	wivrn  string
}

func (f *vrDoorFixture) newHeadset(t *testing.T) *headset {
	t.Helper()
	cert := testCert(t)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return &headset{
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true, //nolint:gosec // the headset pins on first use
		}}},
		key:   sha256.Sum256(leaf.RawSubjectPublicKeyInfo),
		wivrn: testWiVRnKey,
	}
}

// vrReply is a front door reply: its status, JSON body and TLS state.
type vrReply struct {
	status     int
	body       map[string]any
	tls        *tls.ConnectionState
	retryAfter string
}

// str is the body's string field key, or "".
func (r vrReply) str(key string) string {
	v, _ := r.body[key].(string)
	return v
}

func (h *headset) do(t *testing.T, f *vrDoorFixture, method, path string, body any) vrReply {
	t.Helper()
	rd := io.Reader(http.NoBody)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := vrReply{status: resp.StatusCode, tls: resp.TLS, retryAfter: resp.Header.Get("Retry-After")}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// startPairing runs step 1 and returns the confirm body and the code the
// headset would show.
func (h *headset) startPairing(t *testing.T, f *vrDoorFixture) (confirm map[string]string, code string) {
	t.Helper()
	var nonce [32]byte
	_, _ = rand.Read(nonce[:])
	commit := sha256.Sum256(nonce[:])
	r := h.do(t, f, http.MethodPost, VRPairPath, map[string]string{"publicKey": h.wivrn, "name": "Quest 3", "commitment": hex.EncodeToString(commit[:])})
	if r.status != http.StatusOK {
		t.Fatalf("pair: %d %v", r.status, r.body)
	}
	if r.str("pairingURL") != "https://pair.example/pin/" {
		t.Errorf("pairingURL %q", r.str("pairingURL"))
	}
	sn, ok := parseHex32(r.str("serverNonce"))
	if !ok || r.str("pairingID") == "" {
		t.Fatalf("pair reply %v", r.body)
	}
	// The server key as this headset saw it over TLS.
	serverKey := sha256.Sum256(r.tls.PeerCertificates[0].RawSubjectPublicKeyInfo)
	code = vrPairingCode(serverKey, h.key, h.wivrn, sn, nonce)
	return map[string]string{"pairingID": r.str("pairingID"), "nonce": hex.EncodeToString(nonce[:])}, code
}

// confirmAsync starts step 2 and returns its status.
func (h *headset) confirmAsync(ctx context.Context, f *vrDoorFixture, body map[string]string) <-chan int {
	done := make(chan int, 1)
	go func() {
		b, err := json.Marshal(body)
		if err != nil {
			done <- -1
			return
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+VRPairConfirmPath, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.client.Do(req)
		if err != nil {
			done <- -1
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	return done
}

// waitForCode waits until the pairing awaits its code.
func (f *vrDoorFixture) waitForCode(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.door.mu.Lock()
		for _, p := range f.door.pending {
			if p.code != "" {
				f.door.mu.Unlock()
				return
			}
		}
		f.door.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pairing awaits a code")
}

// enterCode posts code on the pairing page as user.
func (f *vrDoorFixture) enterCode(user, code string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, pinVRPath, strings.NewReader(`{"code":"`+code+`"}`))
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, user)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.pinPage.ServeHTTP(rec, req)
	return rec
}

func (f *vrDoorFixture) pair(t *testing.T, h *headset, user string) {
	t.Helper()
	body, code := h.startPairing(t, f)
	done := h.confirmAsync(t.Context(), f, body)
	f.waitForCode(t)
	if rec := f.enterCode(user, code[:4]+" "+code[4:]); rec.Code != http.StatusOK {
		t.Fatalf("entering the code: %d %s", rec.Code, rec.Body)
	}
	if st := <-done; st != http.StatusOK {
		t.Fatalf("confirm: %d", st)
	}
}

func TestVRFrontDoorPairsHeadsetByCode(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	if r := h.do(t, f, http.MethodGet, VRAppsPath, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("unpaired apps: %d", r.status)
	}
	f.pair(t, h, "alice")

	list, err := f.client.DirewolfV1alpha1().Headsets(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("headsets: %v %v", list, err)
	}
	got := list.Items[0]
	if got.Spec.UserReference.Name != "alice" || got.Spec.PublicKey != testWiVRnKey || got.Spec.DisplayName != "Quest 3" ||
		got.Spec.ClientKeySHA256 != hex.EncodeToString(h.key[:]) {
		t.Errorf("headset spec %+v", got.Spec)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Kind != "User" || got.OwnerReferences[0].Name != "alice" {
		t.Errorf("owner %+v", got.OwnerReferences)
	}

	r := h.do(t, f, http.MethodGet, VRAppsPath, nil)
	if r.status != http.StatusOK {
		t.Fatalf("apps: %d", r.status)
	}
	if got, err := json.Marshal(r.body["apps"]); err != nil || string(got) != `[{"id":"assetto-corsa-vr","title":"Assetto Corsa"}]` {
		t.Errorf("apps %s (%v)", got, err)
	}

	// Paired already: a second pairing with the same key is refused.
	var nonce [32]byte
	commit := sha256.Sum256(nonce[:])
	if r := h.do(t, f, http.MethodPost, VRPairPath, map[string]string{"publicKey": testWiVRnKey, "commitment": hex.EncodeToString(commit[:])}); r.status != http.StatusConflict {
		t.Errorf("re-pair: %d", r.status)
	}

	// Another headset is not let in.
	if r := f.newHeadset(t).do(t, f, http.MethodGet, VRAppsPath, nil); r.status != http.StatusUnauthorized {
		t.Errorf("other headset: %d", r.status)
	}

	// Revoked: deleting the Headset shuts the door.
	_ = f.headsets.Delete(&got)
	if r := h.do(t, f, http.MethodGet, VRAppsPath, nil); r.status != http.StatusUnauthorized {
		t.Errorf("revoked headset: %d", r.status)
	}
}

// A relay that pairs its own key with the server shows the headset a code
// that is not the server's: the code binds both TLS keys, the WiVRn key and
// both nonces.
func TestVRPairingCodeBindsEverything(t *testing.T) {
	var sk, ck, sn, hn [32]byte
	_, _ = rand.Read(sk[:])
	_, _ = rand.Read(ck[:])
	_, _ = rand.Read(sn[:])
	_, _ = rand.Read(hn[:])
	base := vrPairingCode(sk, ck, "key", sn, hn)
	if len(base) != vrCodeDigits || strings.Trim(base, "0123456789") != "" {
		t.Fatalf("code %q", base)
	}
	flip := func(b [32]byte) [32]byte { b[0] ^= 1; return b }
	for name, code := range map[string]string{
		"server key":   vrPairingCode(flip(sk), ck, "key", sn, hn),
		"client key":   vrPairingCode(sk, flip(ck), "key", sn, hn),
		"wivrn key":    vrPairingCode(sk, ck, "kez", sn, hn),
		"server nonce": vrPairingCode(sk, ck, "key", flip(sn), hn),
		"nonce":        vrPairingCode(sk, ck, "key", sn, flip(hn)),
	} {
		if code == base {
			t.Errorf("%s does not change the code", name)
		}
	}
	// A fixed vector, for the headset app's implementation (XERK-2809).
	var z [32]byte
	if got := vrPairingCode(z, z, "", z, z); got != fixedVRCode(t) {
		t.Errorf("all-zero code %s", got)
	}
}

// fixedVRCode recomputes the documented derivation independently.
func fixedVRCode(t *testing.T) string {
	t.Helper()
	var z [32]byte
	empty := sha256.Sum256(nil)
	msg := append([]byte("direwolf-vr-pair-v1\x00"), z[:]...)
	msg = append(msg, z[:]...)
	msg = append(msg, empty[:]...)
	msg = append(msg, z[:]...)
	msg = append(msg, z[:]...)
	sum := sha256.Sum256(msg)
	var n uint64
	for _, b := range sum[:8] {
		n = n<<8 | uint64(b)
	}
	s := strconv.FormatUint(n%100_000_000, 10)
	return strings.Repeat("0", 8-len(s)) + s
}

func TestVRFrontDoorRefusesBadPairing(t *testing.T) {
	f := newVRDoorFixture(t, "alice", "mallory")
	h := f.newHeadset(t)

	// A nonce not matching the commitment.
	body, _ := h.startPairing(t, f)
	body["nonce"] = strings.Repeat("00", 32)
	if r := h.do(t, f, http.MethodPost, VRPairConfirmPath, body); r.status != http.StatusForbidden {
		t.Errorf("wrong nonce: %d", r.status)
	}

	// Another client can't confirm this headset's pairing.
	body, _ = h.startPairing(t, f)
	if r := f.newHeadset(t).do(t, f, http.MethodPost, VRPairConfirmPath, body); r.status != http.StatusNotFound {
		t.Errorf("other client's confirm: %d", r.status)
	}

	// Bad keys.
	for _, req := range []map[string]string{
		{"publicKey": "-----BEGIN PUBLIC KEY-----", "commitment": strings.Repeat("00", 32)},
		{"publicKey": testWiVRnKey, "commitment": "00"},
	} {
		if r := h.do(t, f, http.MethodPost, VRPairPath, req); r.status != http.StatusBadRequest {
			t.Errorf("%v: %d", req, r.status)
		}
	}

	// Wrong codes lock the user out after vrCodeFailures, and a right one
	// is then refused too.
	body, code := h.startPairing(t, f)
	done := h.confirmAsync(t.Context(), f, body)
	f.waitForCode(t)
	wrong := "00000000"
	if wrong == code {
		wrong = "11111111"
	}
	for i := range vrCodeFailures {
		if rec := f.enterCode("mallory", wrong); rec.Code != http.StatusNotFound {
			t.Fatalf("wrong code %d: %d", i, rec.Code)
		}
	}
	if rec := f.enterCode("mallory", code); rec.Code != http.StatusTooManyRequests {
		t.Errorf("after %d wrong codes: %d", vrCodeFailures, rec.Code)
	}
	if rec := f.enterCode("alice", "1234"); rec.Code != http.StatusBadRequest {
		t.Errorf("short code: %d", rec.Code)
	}
	// Another user is not locked out.
	if rec := f.enterCode("alice", code); rec.Code != http.StatusOK {
		t.Errorf("alice's code: %d %s", rec.Code, rec.Body)
	}
	if st := <-done; st != http.StatusOK {
		t.Errorf("confirm %d", st)
	}
	// The code is spent.
	if rec := f.enterCode("alice", code); rec.Code != http.StatusNotFound {
		t.Errorf("spent code: %d", rec.Code)
	}
}

func TestVRFrontDoorPairingPageRefusesCrossSite(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, pinVRPath, strings.NewReader(`{"code":"12345678"}`))
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, "alice")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	f.pinPage.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", rec.Code)
	}
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, pinVRPath, strings.NewReader(`{"code":"12345678"}`))
	req.RemoteAddr = trustedPeer
	req.Header.Set(DefaultPinUserHeader, "alice")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	f.pinPage.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no proxy secret: %d", rec.Code)
	}
}

func TestVRFrontDoorLaunchesAndStops(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	f.pair(t, h, "alice")
	f.sessions.state = VRSessionInfo{State: "ready", App: "assetto-corsa-vr", Host: "10.10.10.34", Port: 21000, ServerKey: strings.Repeat("ab", 56)}

	r := h.do(t, f, http.MethodPost, VRLaunchPath, map[string]string{"app": "assetto-corsa-vr"})
	if r.status != http.StatusAccepted || r.str("state") != "ready" || r.str("serverKey") != strings.Repeat("ab", 56) || r.body["port"] != float64(21000) {
		t.Errorf("launch: %d %v", r.status, r.body)
	}
	if len(f.sessions.launched) != 1 || f.sessions.launched[0] != "alice/assetto-corsa-vr" {
		t.Errorf("launched %v", f.sessions.launched)
	}
	if r := h.do(t, f, http.MethodGet, VRSessionPath, nil); r.status != http.StatusOK || r.str("host") != "10.10.10.34" {
		t.Errorf("session: %d %v", r.status, r.body)
	}
	if r := h.do(t, f, http.MethodPost, VRStopPath, nil); r.status != http.StatusNoContent || len(f.sessions.stopped) != 1 {
		t.Errorf("stop: %d %v", r.status, f.sessions.stopped)
	}

	f.sessions.refusal = "Someone else is playing right now. Try again later."
	if r := h.do(t, f, http.MethodPost, VRLaunchPath, map[string]string{"app": "assetto-corsa-vr"}); r.status != http.StatusConflict || r.str("error") != f.sessions.refusal {
		t.Errorf("refused launch: %d %v", r.status, r.body)
	}
	// A cover of an App the user can't play in VR is not served.
	if r := h.do(t, f, http.MethodGet, VRAppsPath+"/other/cover", nil); r.status != http.StatusNotFound {
		t.Errorf("other app's cover: %d", r.status)
	}
}

// The front door answers only clients with a certificate.
func TestVRFrontDoorNeedsClientCertificate(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+VRAppsPath, http.NoBody)
	if resp, err := c.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("served a client without a certificate: %d", resp.StatusCode)
	}
}

// Run listens on the one address it is given, beside the other servers.
func TestRunServesVRFrontDoorOnItsAddress(t *testing.T) {
	var mu sync.Mutex
	var addrs []string
	srv := NewRESTServer(nil, nil, nil, nil, nil, nil, nil, RESTServerOptions{
		Port: 1, SecurePort: 2, Cert: testCert(t),
		PinPage:     PinPageOptions{Port: 3},
		VRFrontDoor: VRFrontDoorOptions{Port: 4, Address: "10.1.2.3"},
	})
	srv.listen = func(ctx context.Context, addr string) (net.Listener, error) {
		mu.Lock()
		addrs = append(addrs, addr)
		mu.Unlock()
		return (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	}
	srv.shutdownTimeout = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(addrs)
		mu.Unlock()
		if n == 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, a := range addrs {
		found = found || a == "10.1.2.3:4"
	}
	if !found {
		t.Errorf("listened on %v, want 10.1.2.3:4 among them", addrs)
	}

	// Without the pairing page, codes could never be entered.
	srv = NewRESTServer(nil, nil, nil, nil, nil, nil, nil, RESTServerOptions{Cert: testCert(t), VRFrontDoor: VRFrontDoorOptions{Port: 4, Address: "10.1.2.3"}})
	if err := srv.Run(t.Context()); err == nil {
		t.Error("ran a VR front door with no pairing page")
	}
}

// Server nonces are handed out at a bounded rate, server-wide: each is a
// draw a relay can match against the code the headset shows.
func TestVRFrontDoorLimitsDraws(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	f.door.draws = rate.NewLimiter(rate.Every(vrPairDrawEvery), vrPairDrawBurst)
	var nonce [32]byte
	commit := sha256.Sum256(nonce[:])
	req := map[string]string{"publicKey": testWiVRnKey, "commitment": hex.EncodeToString(commit[:])}
	for i := range vrPairDrawBurst {
		if r := f.newHeadset(t).do(t, f, http.MethodPost, VRPairPath, req); r.status != http.StatusOK {
			t.Fatalf("draw %d: %d", i, r.status)
		}
	}
	// From any client: the limit is not per key or source.
	if r := f.newHeadset(t).do(t, f, http.MethodPost, VRPairPath, req); r.status != http.StatusTooManyRequests || r.retryAfter != "5" {
		t.Errorf("draw past the burst: %d, Retry-After %q", r.status, r.retryAfter)
	}
}

// A key two Headsets hold, or a terminating Headset's, admits nobody.
func TestVRFrontDoorRefusesAmbiguousOrTerminatingHeadset(t *testing.T) {
	f := newVRDoorFixture(t, "alice", "bob")
	h := f.newHeadset(t)
	f.pair(t, h, "bob")
	key := hex.EncodeToString(h.key[:])
	other := &v1alpha1types.Headset{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testNamespace},
		Spec: v1alpha1types.HeadsetSpec{UserReference: v1alpha1types.UserReference{Name: "alice"}, PublicKey: testWiVRnKey, ClientKeySHA256: key}}
	if err := f.headsets.Add(other); err != nil {
		t.Fatal(err)
	}
	if r := h.do(t, f, http.MethodGet, VRAppsPath, nil); r.status != http.StatusUnauthorized {
		t.Errorf("key on two headsets: %d", r.status)
	}
	_ = f.headsets.Delete(other)
	if r := h.do(t, f, http.MethodGet, VRAppsPath, nil); r.status != http.StatusOK {
		t.Fatalf("one headset again: %d", r.status)
	}
	objs := f.headsets.List()
	hs, ok := objs[0].(*v1alpha1types.Headset)
	if !ok || len(objs) != 1 {
		t.Fatalf("headsets %v", objs)
	}
	hs = hs.DeepCopy()
	hs.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	_ = f.headsets.Update(hs)
	if r := h.do(t, f, http.MethodGet, VRAppsPath, nil); r.status != http.StatusUnauthorized {
		t.Errorf("terminating headset: %d", r.status)
	}
}

// A nonce is revealed once: a second reveal drops the pairing.
func TestVRFrontDoorRefusesSecondReveal(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	body, code := h.startPairing(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_ = h.confirmAsync(ctx, f, body)
	f.waitForCode(t)
	if r := h.do(t, f, http.MethodPost, VRPairConfirmPath, body); r.status != http.StatusConflict {
		t.Errorf("second reveal: %d", r.status)
	}
	if rec := f.enterCode("alice", code); rec.Code != http.StatusNotFound {
		t.Errorf("code after a second reveal: %d", rec.Code)
	}
}

// Re-pairing a key whose Headset is still being deleted fails rather than
// reporting a pairing that is about to vanish.
func TestVRFrontDoorRefusesRepairOverTerminatingHeadset(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	key := hex.EncodeToString(h.key[:])
	ts := metav1.Now()
	if _, err := f.client.DirewolfV1alpha1().Headsets(testNamespace).Create(t.Context(), &v1alpha1types.Headset{
		ObjectMeta: metav1.ObjectMeta{Name: frontDoorHeadsetName("alice", key), DeletionTimestamp: &ts, Finalizers: []string{"x"}},
		Spec:       v1alpha1types.HeadsetSpec{UserReference: v1alpha1types.UserReference{Name: "alice"}, PublicKey: testWiVRnKey, ClientKeySHA256: key},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	body, code := h.startPairing(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_ = h.confirmAsync(ctx, f, body)
	f.waitForCode(t)
	if rec := f.enterCode("alice", code); rec.Code != http.StatusInternalServerError {
		t.Errorf("re-pair over a terminating headset: %d %s", rec.Code, rec.Body)
	}
}

// A launch whose Session the informer hasn't seen yet reports starting, and
// a missing cover is a JSON error like every other front door reply.
func TestVRFrontDoorLaunchReplyAndCoverErrors(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	f.pair(t, h, "alice")
	f.sessions.state = VRSessionInfo{State: "none"}
	if r := h.do(t, f, http.MethodPost, VRLaunchPath, map[string]string{"app": "assetto-corsa-vr"}); r.status != http.StatusAccepted || r.str("state") != "starting" {
		t.Errorf("launch with a lagging informer: %d %v", r.status, r.body)
	}
	if r := h.do(t, f, http.MethodGet, VRSessionPath, nil); r.str("state") != "none" {
		t.Errorf("session read: %v", r.body)
	}
	if r := h.do(t, f, http.MethodGet, VRAppsPath+"/assetto-corsa-vr/cover", nil); r.status != http.StatusNotFound || r.str("error") == "" {
		t.Errorf("missing cover: %d %v", r.status, r.body)
	}
}

// The pairing page, behind Authentik, is the only place a user learns the
// real front door's key: a fake front door computes the code too, so the
// headset must show the key it pinned for the user to compare (XERK-2813).
func TestVRPairingPageShowsServerKey(t *testing.T) {
	f := newVRDoorFixture(t, "alice")
	h := f.newHeadset(t)
	// The key the headset sees on the wire, as it would pin it.
	r := h.do(t, f, http.MethodGet, VRAppsPath, nil)
	if r.tls == nil || len(r.tls.PeerCertificates) == 0 {
		t.Fatal("no server certificate")
	}
	want := vrKeyFingerprint(sha256.Sum256(r.tls.PeerCertificates[0].RawSubjectPublicKeyInfo))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/pin/", nil)
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, "alice")
	rec := httptest.NewRecorder()
	f.pinPage.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), ">127.0.0.1:1<") ||
		!strings.Contains(rec.Body.String(), vrKeyMismatchAdvice) {
		t.Errorf("page %d lacks key %q or address:\n%s", rec.Code, want, rec.Body)
	}

	body, code := h.startPairing(t, f)
	done := h.confirmAsync(t.Context(), f, body)
	f.waitForCode(t)
	if rec := f.enterCode("alice", code); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) ||
		!strings.Contains(rec.Body.String(), vrKeyMismatchAdvice) {
		t.Errorf("code entry %d does not name the key %q: %s", rec.Code, want, rec.Body)
	}
	<-done
}

func TestVRKeyFingerprint(t *testing.T) {
	// A fixed vector, for the headset app's implementation: SHA-256 of "".
	if got := vrKeyFingerprint(sha256.Sum256(nil)); got != "e3b0 c442 98fc 1c14 9afb f4c8 996f b924" {
		t.Errorf("fingerprint %q", got)
	}
}
