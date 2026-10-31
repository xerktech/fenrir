package moonlight

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
)

const (
	testNamespace = "direwolf"
	trustedPeer   = "10.0.0.5:40000"
	proxySecret   = "0123456789abcdef0123456789abcdef"
)

func newTestPinPage(t *testing.T, users ...string) (*PairingManager, http.Handler) {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, name := range users {
		if err := indexer.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewPairingManager(tls.Certificate{}, nil)
	handler := NewPinPageHandler(
		manager,
		generic.NewLister[*v1alpha1types.User](indexer).Namespaced(testNamespace),
		PinPageOptions{
			TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
			ProxySecret:    []byte(proxySecret),
		},
	)
	return manager, handler
}

// startPhase1 runs pairPhase1 the way /pair would and waits until it is
// blocked on the pairing page. The returned channel yields its response.
func startPhase1(ctx context.Context, t *testing.T, m *PairingManager, cacheKey string, salt []byte) (secret string, done <-chan PairingResponse) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certHex := hex.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	resp := make(chan PairingResponse, 1)
	go func() { resp <- m.pairPhase1(ctx, cacheKey, hex.EncodeToString(salt), certHex) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range m.PendingPairings() {
			if p.Client == cacheKey {
				return p.Secret, resp
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pairing request never became pending")
	return "", nil
}

func postPin(h http.Handler, peer, user, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/pin/", strings.NewReader(body))
	req.RemoteAddr = peer
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PinProxySecretHeader, proxySecret)
	if user != "" {
		req.Header.Set(DefaultPinUserHeader, user)
	}
	for _, f := range mutate {
		f(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPinHandoffMapsPairingToAuthenticatedUser(t *testing.T) {
	m, h := newTestPinPage(t, "alice")
	salt := bytes.Repeat([]byte{0x42}, 16)
	secret, done := startPhase1(t.Context(), t, m, "client@192.0.2.10", salt)

	// The page lists the waiting request for the signed-in user.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/pin/", http.NoBody)
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("GET /pin/ = %d, body missing secret or user:\n%s", rec.Code, rec.Body)
	}

	if rec := postPin(h, trustedPeer, "alice", `{"pin":"1234","secret":"`+secret+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /pin/ = %d %s", rec.Code, rec.Body)
	}

	select {
	case resp := <-done:
		if resp.Paired != 1 {
			t.Fatalf("phase 1 failed: %+v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("phase 1 did not resume after the PIN was posted")
	}

	v, ok := m.PairingCache.Load("client@192.0.2.10")
	if !ok {
		t.Fatal("no pairing cache entry after phase 1")
	}
	entry, ok := v.(pendingPairCacheEntry)
	if !ok {
		t.Fatalf("pairing cache entry has type %T", v)
	}
	if entry.Username != "alice" {
		t.Errorf("Username = %q, want alice", entry.Username)
	}
	if want := util.Hash(salt, []byte("1234"))[:16]; !bytes.Equal(entry.AESKey, want) {
		t.Errorf("AESKey not derived from the posted PIN")
	}

	// The request is consumed: a second PIN for it is refused.
	if rec := postPin(h, trustedPeer, "alice", `{"pin":"9999","secret":"`+secret+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("second POST = %d, want 404", rec.Code)
	}
}

func TestPinPageRejects(t *testing.T) {
	cases := []struct {
		name   string
		peer   string
		user   string
		body   string // %s is replaced by the pending secret
		mutate func(*http.Request)
		want   int
	}{
		{name: "untrusted peer forging the header", peer: "192.0.2.99:1234", user: "alice", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		{name: "forwarded-for does not make a peer trusted", peer: "192.0.2.99:1234", user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set("X-Forwarded-For", "10.0.0.5") }, want: http.StatusForbidden},
		{name: "trusted peer without the proxy secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Del(PinProxySecretHeader) }, want: http.StatusForbidden},
		{name: "trusted peer with a wrong proxy secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set(PinProxySecretHeader, proxySecret[:31]+"x") }, want: http.StatusForbidden},
		{name: "trusted peer with a truncated proxy secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set(PinProxySecretHeader, proxySecret[:31]) }, want: http.StatusForbidden},
		{name: "proxy secret from an untrusted peer", peer: "192.0.2.99:1234", user: "alice", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		{name: "no identity header", peer: trustedPeer, body: `{"pin":"1234","secret":"%s"}`, want: http.StatusUnauthorized},
		{name: "identity without a User", peer: trustedPeer, user: "mallory", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		// Usernames map to User names as the operator's provisioner maps them:
		// "Alice" is not the hand-written User "alice" (v1alpha1.LookupUser).
		{name: "identity naming a User in another case", peer: trustedPeer, user: "Alice", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		{name: "form post (CSRF)", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
		{name: "cross-site fetch", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, want: http.StatusForbidden},
		{name: "non-numeric pin", peer: trustedPeer, user: "alice", body: `{"pin":"12a4","secret":"%s"}`, want: http.StatusBadRequest},
		{name: "unknown secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"deadbeef"}`, want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, h := newTestPinPage(t, "alice")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			secret, done := startPhase1(ctx, t, m, "client@192.0.2.10", make([]byte, 16))

			var mutate []func(*http.Request)
			if tc.mutate != nil {
				mutate = append(mutate, tc.mutate)
			}
			body := strings.ReplaceAll(tc.body, "%s", secret)
			if rec := postPin(h, tc.peer, tc.user, body, mutate...); rec.Code != tc.want {
				t.Fatalf("POST = %d %s, want %d", rec.Code, rec.Body, tc.want)
			}

			// The PIN must not have reached the handshake.
			if len(m.PendingPairings()) != 1 {
				t.Fatal("rejected request consumed the pending pairing")
			}
			cancel()
			if resp := <-done; resp.Paired != 0 {
				t.Fatalf("phase 1 completed after a rejected PIN: %+v", resp)
			}
		})
	}
}

func TestPinPageWithoutProxySecretRefusesEverything(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	// alice exists, so a 403 here can only come from the secret check.
	if err := indexer.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: testNamespace}}); err != nil {
		t.Fatal(err)
	}
	h := NewPinPageHandler(
		NewPairingManager(tls.Certificate{}, nil),
		generic.NewLister[*v1alpha1types.User](indexer).Namespaced(testNamespace),
		PinPageOptions{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}},
	)
	for _, sent := range []string{"", proxySecret} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/pin/", http.NoBody)
		req.RemoteAddr = trustedPeer
		req.Header.Set(PinProxySecretHeader, sent)
		req.Header.Set(DefaultPinUserHeader, "alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("secret %q: GET /pin/ = %d, want 403", sent, rec.Code)
		}
	}
}

func TestPendingPairingDroppedWhenClientGivesUp(t *testing.T) {
	m, _ := newTestPinPage(t)
	ctx, cancel := context.WithCancel(t.Context())
	_, done := startPhase1(ctx, t, m, "client@192.0.2.10", make([]byte, 16))

	cancel()
	if resp := <-done; resp.Paired != 0 {
		t.Fatalf("phase 1 succeeded without a PIN: %+v", resp)
	}
	if p := m.PendingPairings(); len(p) != 0 {
		t.Fatalf("pending pairings after cancel: %+v", p)
	}
}

// A provision-group member with no User yet is sent to the Library, which
// creates it; anyone else without a User is refused as before, and the group
// header counts only from a trusted peer with the proxy secret.
func TestPinPageUnprovisionedGroupMember(t *testing.T) {
	const libraryURL = "https://library.example.com/"
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	// Hand-written "bob" (no username annotation) belongs to username "bob"
	// only; the provisioner refuses "Bob".
	if err := indexer.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: testNamespace}}); err != nil {
		t.Fatal(err)
	}
	opts := PinPageOptions{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
		ProxySecret:    []byte(proxySecret),
		ProvisionGroup: "k8x-games",
		LibraryURL:     libraryURL,
	}
	lister := generic.NewLister[*v1alpha1types.User](indexer).Namespaced(testNamespace)
	h := NewPinPageHandler(NewPairingManager(tls.Certificate{}, nil), lister, opts)
	opts.ProvisionGroup = ""
	off := NewPinPageHandler(NewPairingManager(tls.Certificate{}, nil), lister, opts)
	const setUp = "Your account is set up the first time you open the Library"
	cases := []struct {
		name, method, peer, secret, groups string
		user                               string       // default "newbie"
		handler                            http.Handler // default h
		want                               int
		wantSetUp                          bool
	}{
		{name: "member with spaces around groups", method: http.MethodGet, peer: trustedPeer, secret: proxySecret, groups: "admins | k8x-games ", want: http.StatusForbidden, wantSetUp: true},
		{name: "provisioning off, empty groups", handler: off, method: http.MethodGet, peer: trustedPeer, secret: proxySecret, want: http.StatusForbidden},
		{name: "provisioning off, member", handler: off, method: http.MethodGet, peer: trustedPeer, secret: proxySecret, groups: "k8x-games", want: http.StatusForbidden},
		// The provisioner refuses these, so the Library would never set them up.
		{name: "member with an unprovisionable name", user: "first.last", method: http.MethodGet, peer: trustedPeer, secret: proxySecret, groups: "k8x-games", want: http.StatusForbidden},
		{name: "member whose name a hand-written User holds", user: "Bob", method: http.MethodGet, peer: trustedPeer, secret: proxySecret, groups: "k8x-games", want: http.StatusForbidden},
		{name: "member GET", method: http.MethodGet, peer: trustedPeer, secret: proxySecret, groups: "admins|k8x-games", want: http.StatusForbidden, wantSetUp: true},
		{name: "member POST", method: http.MethodPost, peer: trustedPeer, secret: proxySecret, groups: "k8x-games", want: http.StatusForbidden, wantSetUp: true},
		{name: "non-member", method: http.MethodGet, peer: trustedPeer, secret: proxySecret, groups: "admins|k8x-games-old", want: http.StatusForbidden},
		{name: "no groups", method: http.MethodGet, peer: trustedPeer, secret: proxySecret, want: http.StatusForbidden},
		{name: "untrusted peer claiming the group", method: http.MethodGet, peer: "192.0.2.99:1234", secret: proxySecret, groups: "k8x-games", want: http.StatusForbidden},
		{name: "trusted peer without the secret claiming the group", method: http.MethodGet, peer: trustedPeer, groups: "k8x-games", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "/pin/", strings.NewReader(`{"pin":"1234","secret":"x"}`))
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = tc.peer
			if tc.secret != "" {
				req.Header.Set(PinProxySecretHeader, tc.secret)
			}
			user := cmp.Or(tc.user, "newbie")
			req.Header.Set(DefaultPinUserHeader, user)
			if tc.groups != "" {
				req.Header.Set(DefaultPinGroupsHeader, tc.groups)
			}
			rec := httptest.NewRecorder()
			cmp.Or[http.Handler](tc.handler, h).ServeHTTP(rec, req)
			body := rec.Body.String()
			if rec.Code != tc.want {
				t.Fatalf("%s = %d %s, want %d", tc.method, rec.Code, body, tc.want)
			}
			if got := strings.Contains(body, setUp); got != tc.wantSetUp {
				t.Fatalf("body mentions setup = %v, want %v:\n%s", got, tc.wantSetUp, body)
			}
			if tc.wantSetUp && !strings.Contains(body, libraryURL) {
				t.Errorf("body does not link the Library:\n%s", body)
			}
			if tc.wantSetUp && tc.method == http.MethodGet {
				for _, header := range []string{"Content-Security-Policy", "X-Frame-Options", "Cache-Control"} {
					if rec.Header().Get(header) == "" {
						t.Errorf("setup page has no %s", header)
					}
				}
			}
			if !tc.wantSetUp && strings.Contains(body, "library") {
				t.Errorf("refusal mentions the Library:\n%s", body)
			}
			// Untrusted callers learn nothing, not even whether the User exists.
			if tc.secret == "" || tc.peer != trustedPeer {
				if strings.TrimSpace(body) != "forbidden" {
					t.Errorf("untrusted caller got %q, want the bare refusal", body)
				}
			} else if !tc.wantSetUp && !strings.Contains(body, "no direwolf user for this account") || strings.Contains(body, user) {
				t.Errorf("non-member got %q, want the not-allowed refusal", body)
			}
		})
	}
}

// A provisioned User whose name the username maps to (v1alpha1.UserNameFor)
// is found, so a member who opened the Library pairs as normal.
func TestPinPageFindsProvisionedUserByMappedName(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if err := indexer.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{
		Name: "sam", Namespace: testNamespace, Annotations: map[string]string{v1alpha1types.UsernameAnnotation: "Sam"},
	}}); err != nil {
		t.Fatal(err)
	}
	h := NewPinPageHandler(
		NewPairingManager(tls.Certificate{}, nil),
		generic.NewLister[*v1alpha1types.User](indexer).Namespaced(testNamespace),
		PinPageOptions{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}, ProxySecret: []byte(proxySecret), ProvisionGroup: "k8x-games"},
	)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/pin/", http.NoBody)
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, "Sam")
	req.Header.Set(DefaultPinGroupsHeader, "k8x-games")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Signed in as <strong>sam</strong>") {
		t.Fatalf("GET /pin/ = %d:\n%s", rec.Code, rec.Body)
	}
}
