package controllers

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// headsetsServer is a Library page with headset confirmation, over Sessions
// of sam (keys pendingA, and the HTML-ish name) and alex (key B).
func headsetsServer(t *testing.T, samPending ...v1alpha1types.PendingHeadset) (*libraryFixture, *LibraryServer) {
	t.Helper()
	f := newLibraryFixture(t, nil, libraryUser("sam", false), libraryUser("alex", false))
	s := f.server(t)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	add := func(name, user string, pending []v1alpha1types.PendingHeadset) {
		sess := &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: libraryTestNS}}
		sess.Spec.UserReference.Name = user
		sess.Status.PendingHeadsets = pending
		if err := idx.Add(sess); err != nil {
			t.Fatal(err)
		}
	}
	add("sam-1", "sam", samPending)
	add("alex-1", "alex", []v1alpha1types.PendingHeadset{{Key: testHeadsetKeyB, Name: "alex's", FirstSeen: metav1.Now()}})
	s.HeadsetConfirmation = &HeadsetConfirmation{
		Sessions: generic.NewLister[*v1alpha1types.Session](idx).Namespaced(libraryTestNS),
		Headsets: f.dw.DirewolfV1alpha1().Headsets(libraryTestNS),
	}
	return f, s
}

var pendingA = v1alpha1types.PendingHeadset{Key: testHeadsetKeyA, Name: `<b>Quest</b>`, FirstSeen: metav1.Now()}

func confirmRequest(s *LibraryServer, user, key string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, libraryHeadsetsPath,
		strings.NewReader(url.Values{"key": {key}}.Encode()))
	req.RemoteAddr = "10.1.0.5:5555"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(LibraryProxySecretHeader, testLibrarySecret)
	if user != "" {
		req.Header.Set(DefaultLibraryUserHeader, user)
	}
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func (f *libraryFixture) headsets(t *testing.T) []v1alpha1types.Headset {
	t.Helper()
	l, err := f.dw.DirewolfV1alpha1().Headsets(libraryTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// The signed-in User sees only their own pending keys, names escaped, with
// a fingerprint; never another user's.
func TestHeadsetsPageListsOwnPending(t *testing.T) {
	_, s := headsetsServer(t, pendingA)
	rec := serveLibraryRequest(s, http.MethodGet, libraryHeadsetsPath, "10.1.0.5:5555", http.Header{DefaultLibraryUserHeader: {"sam"}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "&lt;b&gt;Quest&lt;/b&gt;") || strings.Contains(body, "<b>Quest") ||
		!strings.Contains(body, headsetFingerprint(testHeadsetKeyA)) || strings.Contains(body, "alex&#39;s") || strings.Contains(body, testHeadsetKeyB) {
		t.Errorf("HTTP %d\n%s", rec.Code, body)
	}
	if strings.Contains(body, "More than one") {
		t.Error("warned with one pending")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q", csp)
	}
}

// Two pending keys: a PIN pairing adds one, so the page warns.
func TestHeadsetsPageWarnsOfSeveral(t *testing.T) {
	_, s := headsetsServer(t, pendingA, v1alpha1types.PendingHeadset{Key: "R09ORQ==", FirstSeen: metav1.NewTime(time.Now().Add(-time.Minute))})
	rec := serveLibraryRequest(s, http.MethodGet, libraryHeadsetsPath, "10.1.0.5:5555", http.Header{DefaultLibraryUserHeader: {"sam"}})
	if !strings.Contains(rec.Body.String(), "More than one headset is waiting") {
		t.Errorf("no warning:\n%s", rec.Body)
	}
}

// Confirming creates the User's Headset for that key, once; confirming
// again is no error.
func TestHeadsetsPageConfirms(t *testing.T) {
	f, s := headsetsServer(t, pendingA)
	for range 2 {
		rec := confirmRequest(s, "sam", testHeadsetKeyA, http.Header{"Sec-Fetch-Site": {"same-origin"}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != libraryHeadsetsPath {
			t.Fatalf("HTTP %d %s", rec.Code, rec.Body)
		}
	}
	hs := f.headsets(t)
	if len(hs) != 1 || hs[0].Name != recordedHeadsetName("sam", testHeadsetKeyA) || hs[0].Spec.UserReference.Name != "sam" ||
		hs[0].Spec.PublicKey != testHeadsetKeyA || hs[0].Spec.DisplayName != pendingA.Name {
		t.Errorf("headsets %+v", hs)
	}
}

// Nothing else confirms: another user's pending key, a key pending nowhere,
// a cross-site form, no or an unknown user, a GET, a server without
// confirmation, or a request without the proxy secret.
func TestHeadsetsPageRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, user, key string
		header          http.Header
		want            int
		noConfirm       bool
	}{
		{"another user's key", "sam", testHeadsetKeyB, nil, http.StatusConflict, false},
		{"a key pending nowhere", "sam", "R09ORQ==", nil, http.StatusConflict, false},
		{"cross-site", "sam", testHeadsetKeyA, http.Header{"Sec-Fetch-Site": {"cross-site"}}, http.StatusForbidden, false},
		{"no user", "", testHeadsetKeyA, nil, http.StatusForbidden, false},
		{"unknown user", "nobody", testHeadsetKeyA, nil, http.StatusForbidden, false},
		{"no proxy secret", "sam", testHeadsetKeyA, http.Header{LibraryProxySecretHeader: {"wrong"}}, http.StatusForbidden, false},
		{"confirmation off", "sam", testHeadsetKeyA, nil, http.StatusNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := headsetsServer(t, pendingA)
			if tc.noConfirm {
				s.HeadsetConfirmation = nil
			}
			rec := confirmRequest(s, tc.user, tc.key, tc.header)
			if rec.Code != tc.want {
				t.Errorf("HTTP %d, want %d", rec.Code, tc.want)
			}
			if hs := f.headsets(t); len(hs) != 0 {
				t.Errorf("created %+v", hs)
			}
		})
	}
	t.Run("GET", func(t *testing.T) {
		f, s := headsetsServer(t, pendingA)
		serveLibraryRequest(s, http.MethodGet, libraryHeadsetsPath+"?key="+url.QueryEscape(testHeadsetKeyA), "10.1.0.5:5555", http.Header{DefaultLibraryUserHeader: {"sam"}})
		if hs := f.headsets(t); len(hs) != 0 {
			t.Errorf("a GET created %+v", hs)
		}
	})
	t.Run("name taken by another user's Headset", func(t *testing.T) {
		f, s := headsetsServer(t, pendingA)
		h := &v1alpha1types.Headset{ObjectMeta: metav1.ObjectMeta{Name: recordedHeadsetName("sam", testHeadsetKeyA), Namespace: libraryTestNS}}
		h.Spec.UserReference.Name = "alex"
		h.Spec.PublicKey = testHeadsetKeyA
		if _, err := f.dw.DirewolfV1alpha1().Headsets(libraryTestNS).Create(context.Background(), h, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if rec := confirmRequest(s, "sam", testHeadsetKeyA, nil); rec.Code != http.StatusInternalServerError {
			t.Errorf("HTTP %d, want 500", rec.Code)
		}
	})
}
