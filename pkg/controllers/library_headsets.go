package controllers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// The headset confirmation page (XERK-2742), on a host of its own (XERK-2750). A VR session seeded
// with no Headsets pairs by PIN, and the keys its WiVRn then knows wait in
// the Session's pendingHeadsets: the game can plant one, so none is trusted
// past that Session until its User, signed in through Authentik, confirms
// the headset they paired here. Confirming creates the Headset.
const (
	libraryHeadsetsPath = "/.direwolf/headsets"
	// libraryHeadsetsFormMax bounds the confirm form: one key.
	libraryHeadsetsFormMax = 4096
)

// HeadsetConfirmation turns on the headset confirmation page.
type HeadsetConfirmation struct {
	Sessions generic.NamespacedLister[*v1alpha1types.Session]
	Headsets v1alpha1client.HeadsetInterface
	// Secret is what the page's own proxy (its own host) sends in
	// LibraryProxySecretHeader. It must differ from the Library's: a
	// request with it is served the page and never the Library.
	Secret []byte
	// confirm serializes confirms: pendingFor drops a Session's keys once
	// one is confirmed, but two confirms read it before either created its
	// Headset, so both were saved (XERK-2709). One operator replica serves
	// this page, so a mutex is enough.
	confirm sync.Mutex
}

func (h *HeadsetConfirmation) hasProxySecret(r *http.Request) bool {
	if len(h.Secret) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(LibraryProxySecretHeader)), h.Secret) == 1
}

// pendingFor is the keys awaiting user's confirmation across their Sessions,
// one per key, oldest first. A Session one of whose keys the User already
// confirmed offers none: a PIN pairing adds one key, so the rest are the
// game's (the operator drops them at its next poll).
func (h *HeadsetConfirmation) pendingFor(ctx context.Context, user string) ([]v1alpha1types.PendingHeadset, error) {
	sessions, err := h.Sessions.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	var res []v1alpha1types.PendingHeadset
	for _, s := range sessions {
		if s.Spec.UserReference.Name != user || s.DeletionTimestamp != nil {
			continue
		}
		confirmed, err := h.anyConfirmed(ctx, user, s.Status.PendingHeadsets)
		if err != nil {
			return nil, err
		}
		if confirmed {
			continue
		}
		for _, p := range s.Status.PendingHeadsets {
			if !slices.ContainsFunc(res, func(q v1alpha1types.PendingHeadset) bool { return q.Key == p.Key }) {
				res = append(res, p)
			}
		}
	}
	slices.SortFunc(res, func(a, b v1alpha1types.PendingHeadset) int { return a.FirstSeen.Compare(b.FirstSeen.Time) })
	return res, nil
}

// anyConfirmed is whether user already confirmed one of pending.
func (h *HeadsetConfirmation) anyConfirmed(ctx context.Context, user string, pending []v1alpha1types.PendingHeadset) (bool, error) {
	for _, p := range pending {
		hs, err := h.Headsets.Get(ctx, recordedHeadsetName(user, p.Key), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, fmt.Errorf("getting headset: %w", err)
		case hs.Spec.UserReference.Name == user && hs.Spec.PublicKey == p.Key:
			return true, nil
		}
	}
	return false, nil
}

// headsetFingerprint is a short digest of key, to tell entries apart. WiVRn
// shows none on the headset to compare it with.
func headsetFingerprint(key string) string {
	der, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		der = []byte(key)
	}
	sum := sha256.Sum256(der)
	hexed := hex.EncodeToString(sum[:8])
	var parts []string
	for i := 0; i < len(hexed); i += 4 {
		parts = append(parts, hexed[i:i+4])
	}
	return strings.Join(parts, " ")
}

// serveHeadsets lists the signed-in User's pending headsets (GET) or
// confirms one (POST). ServeHTTP has checked the peer and the page's secret,
// so userHeader is the proxy's. Only a named User may confirm, and only a
// key pending in one of their own Sessions.
func (s *LibraryServer) serveHeadsets(w http.ResponseWriter, r *http.Request) {
	if s.HeadsetConfirmation == nil {
		http.NotFound(w, r)
		return
	}
	name := r.Header.Get(s.userHeader)
	if name == "" || s.users == nil {
		writeLibraryPage(w, http.StatusForbidden, "Sign in to confirm a headset.", false, false)
		return
	}
	user, err := v1alpha1types.LookupUser(s.users.Get, name)
	if apierrors.IsNotFound(err) {
		writeLibraryPage(w, http.StatusForbidden, "You have no streaming account here. Ask the admin to add one.", false, false)
		return
	} else if err != nil {
		klog.Errorf("Headsets page: getting user %s: %v", name, err)
		writeLibraryPage(w, http.StatusInternalServerError, "Try again in a minute.", false, false)
		return
	}
	pending, err := s.HeadsetConfirmation.pendingFor(r.Context(), user.Name)
	if err != nil {
		klog.Errorf("Headsets page: %v", err)
		writeLibraryPage(w, http.StatusInternalServerError, "Try again in a minute.", false, false)
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeHeadsetsPage(w, pending)
	case http.MethodPost:
		// This page's own form, submitted by the user: same-origin (the
		// Library is on a sibling host, so "same-site" is the desktop's
		// code, XERK-2750), a navigation (never a script's fetch),
		// user-activated (QA, XERK-2747). Browsers set these headers; a
		// script cannot. Browsers that send none are refused too.
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" || r.Header.Get("Sec-Fetch-Mode") != "navigate" ||
			r.Header.Get("Sec-Fetch-User") != "?1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, libraryHeadsetsFormMax)
		key := r.PostFormValue("key")
		// Read again under the lock: a confirm that finished meanwhile may
		// have taken this key's Session.
		s.HeadsetConfirmation.confirm.Lock()
		defer s.HeadsetConfirmation.confirm.Unlock()
		pending, err = s.HeadsetConfirmation.pendingFor(r.Context(), user.Name)
		if err != nil {
			klog.Errorf("Headsets page: %v", err)
			writeLibraryPage(w, http.StatusInternalServerError, "Try again in a minute.", false, false)
			return
		}
		if key != "" {
			// Confirmed already (a second click): done.
			done, err := s.HeadsetConfirmation.anyConfirmed(r.Context(), user.Name, []v1alpha1types.PendingHeadset{{Key: key}})
			if err != nil {
				klog.Errorf("Headsets page: %v", err)
				writeLibraryPage(w, http.StatusInternalServerError, "Try again in a minute.", false, false)
				return
			}
			if done {
				http.Redirect(w, r, libraryHeadsetsPath, http.StatusSeeOther)
				return
			}
		}
		i := slices.IndexFunc(pending, func(p v1alpha1types.PendingHeadset) bool { return p.Key == key })
		if key == "" || i < 0 {
			writeLibraryPage(w, http.StatusConflict, "That headset is no longer waiting. Pair it again in a VR session.", false, false)
			return
		}
		if err := s.confirmHeadset(r, user, pending[i]); err != nil {
			klog.Errorf("Headsets page: %v", err)
			writeLibraryPage(w, http.StatusInternalServerError, "The headset could not be saved. Try again in a minute.", false, false)
			return
		}
		http.Redirect(w, r, libraryHeadsetsPath, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// confirmHeadset creates u's Headset for p. Confirming again finds the one
// made before; one of that name for another user or key is refused.
func (s *LibraryServer) confirmHeadset(r *http.Request, u *v1alpha1types.User, p v1alpha1types.PendingHeadset) error {
	user := u.Name
	name := recordedHeadsetName(user, p.Key)
	_, err := s.HeadsetConfirmation.Headsets.Create(r.Context(), &v1alpha1types.Headset{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			// Garbage collected with its User: without an owner it would
			// outlive the User and pass to a new one of its name.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1types.GroupVersion.String(), Kind: "User", Name: u.Name, UID: u.UID,
			}},
		},
		Spec: v1alpha1types.HeadsetSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
			PublicKey:     p.Key,
			DisplayName:   CleanHeadsetName(p.Name),
		},
	}, metav1.CreateOptions{FieldManager: "direwolf-library"})
	if apierrors.IsAlreadyExists(err) {
		h, getErr := s.HeadsetConfirmation.Headsets.Get(r.Context(), name, metav1.GetOptions{})
		switch {
		case getErr != nil:
			return fmt.Errorf("getting headset %s: %w", name, getErr)
		case h.Spec.UserReference.Name != user || h.Spec.PublicKey != p.Key:
			return fmt.Errorf("headset %s exists for another user or key", name)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating headset %s: %w", name, err)
	}
	klog.Infof("Headsets page: user %s confirmed headset %s", user, name)
	return nil
}

func writeHeadsetsPage(w http.ResponseWriter, pending []v1alpha1types.PendingHeadset) {
	var b strings.Builder
	switch len(pending) {
	case 0:
		b.WriteString(`<p>No headset is waiting. Pair one by PIN in a VR session, then reload this page.</p>`)
	case 1:
		b.WriteString(`<p>A headset is waiting. Confirm it only if you paired it by PIN just now: the game can add a key of its own. Confirmed, it connects without a PIN next time.</p>`)
	default:
		b.WriteString(`<p><strong>More than one headset is waiting.</strong> A PIN pairing adds one: confirm only the one you paired just now. Any other was added by the game; leave it.</p>`)
	}
	for _, p := range pending {
		name := p.Name
		if name == "" {
			name = "Unnamed headset"
		}
		fmt.Fprintf(&b, `<form method="post" action="%s"><p>%s<br><small>fingerprint %s · seen %s</small></p><input type="hidden" name="key" value="%s"><button type="submit">This is my headset</button></form>`,
			libraryHeadsetsPath, html.EscapeString(name), headsetFingerprint(p.Key),
			html.EscapeString(p.FirstSeen.UTC().Format("2006-01-02 15:04 MST")), html.EscapeString(p.Key))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Headsets</title>
<style>body{font-family:system-ui,sans-serif;background:#111;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0;padding:0 16px}p{max-width:32em;text-align:center;font-size:1.2em}button{font-size:1.1em;padding:.6em 1.4em}main{display:grid;justify-items:center;gap:1em}small{color:#aaa}</style>
</head><body><main><h1>VR headsets</h1>%s</main></body></html>
`, b.String())
}
