package controllers

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

func (f *portsFixture) createPairing(t *testing.T, name string) {
	t.Helper()
	p := &v1alpha1types.Pairing{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: portsTestNS},
		Spec:       v1alpha1types.PairingSpec{UserReference: v1alpha1types.UserReference{Name: f.user}},
	}
	if _, err := f.dw.DirewolfV1alpha1().Pairings(portsTestNS).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRevokedReason(t *testing.T) {
	ctx := context.Background()
	f := newPortsFixture(t)
	f.createPairing(t, "paired")
	for range 500 {
		if _, err := f.sc.PairingInformer.Namespaced(portsTestNS).Get("paired"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, tc := range []struct {
		name, pairing string
		revoked       bool
	}{
		{"pairing exists", "paired", false},
		{"pairing deleted", "gone", true},
		{"no pairing reference (made by hand)", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := f.session("s", f.user)
			s.Spec.PairingReference.Name = tc.pairing
			reason, err := f.sc.revokedReason(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if (reason != "") != tc.revoked {
				t.Errorf("revokedReason = %q, want revoked=%v", reason, tc.revoked)
			}
		})
	}
}

// A Pairing the API server has but our cache has not seen yet (its watch
// lags the Session's) must not end the Session.
func TestRevokedReasonConfirmsWithAPIServer(t *testing.T) {
	f := newPortsFixture(t)
	f.sc.PairingInformer = emptyPairingInformer(t)
	f.createPairing(t, "fresh")
	s := f.session("s", f.user)
	s.Spec.PairingReference.Name = "fresh"
	reason, err := f.sc.revokedReason(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("ended a session whose pairing exists but is not cached: %q", reason)
	}
}

// Deleting a client's Pairing ends the Session it is streaming through the
// running controller, without waiting for the client to quit or disconnect
// (XERK-1580).
func TestPairingDeletionEndsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newPortsFixture(t)
	f.createPairing(t, "client")
	sessions := f.dw.DirewolfV1alpha1().Sessions(portsTestNS)
	s := f.session("alex-1", f.user)
	s.Spec.PairingReference.Name = "client"
	s.Status.WolfSessionID = "1" // streaming, so the unstarted reaper leaves it
	if _, err := sessions.Create(ctx, s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, s.Name, true)

	done := make(chan error, 1)
	go func() { done <- f.sc.Run(ctx) }()
	waitPairingWatch(t, f.sc) // Run starts it

	// Kept while its client is paired.
	time.Sleep(500 * time.Millisecond)
	if _, err := sessions.Get(ctx, s.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("session ended while its client was still paired: %v", err)
	}

	if err := f.dw.DirewolfV1alpha1().Pairings(portsTestNS).Delete(ctx, "client", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if _, err := sessions.Get(ctx, s.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session outlived its client's pairing")
}

// emptyPairingInformer is a synced Pairing informer over a clientset with
// no Pairings, standing in for a cache that lags the API server.
func emptyPairingInformer(t *testing.T) generic.Informer[*v1alpha1types.Pairing] {
	t.Helper()
	factory := generatedinformers.NewSharedInformerFactory(generatedclient.NewSimpleClientset(), 0)
	inf := factory.Direwolf().V1alpha1().Pairings().Informer()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	return generic.NewInformer[*v1alpha1types.Pairing](inf)
}

// The Pairing watch alone ends a deleted Pairing's Sessions, and only
// those: a Session whose own reconcile never runs again still ends.
func TestPairingWatchEndsItsSessions(t *testing.T) {
	ctx := t.Context()
	f := newPortsFixture(t)
	f.createPairing(t, "client")
	f.createPairing(t, "another")
	sessions := f.dw.DirewolfV1alpha1().Sessions(portsTestNS)
	for name, pairing := range map[string]string{"mine": "client", "other": "another", "manual": ""} {
		s := f.session(name, f.user)
		s.Spec.PairingReference.Name = pairing
		if _, err := sessions.Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		f.waitInformer(t, name, true)
	}
	go func() { _ = f.sc.pairingController.Run(ctx) }()
	waitPairingWatch(t, f.sc)

	if err := f.dw.DirewolfV1alpha1().Pairings(portsTestNS).Delete(ctx, "client", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, "mine", false)
	for _, name := range []string{"other", "manual"} {
		if _, err := sessions.Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("session %s of another client ended: %v", name, err)
		}
	}
}

// Only NotFound means revoked: an API server error on the confirming GET
// must not end every stream whose Pairing the cache briefly lacks.
func TestRevokedReasonKeepsSessionOnAPIError(t *testing.T) {
	f := newPortsFixture(t)
	f.sc.PairingInformer = emptyPairingInformer(t)
	f.dw.PrependReactor("get", "pairings", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
	})
	s := f.session("s", f.user)
	s.Spec.PairingReference.Name = "client"
	reason, err := f.sc.revokedReason(context.Background(), s)
	if err == nil || reason != "" {
		t.Errorf("revokedReason = (%q, %v), want an error and no reason", reason, err)
	}
}

// waitPairingWatch waits until the Pairing watch has synced.
func waitPairingWatch(t *testing.T, sc *SessionController) {
	t.Helper()
	for range 500 {
		if sc.pairingController.HasSynced() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pairing watch never started")
}

// reconcilePairing for a Pairing that is gone, with s referencing it.
func (f *portsFixture) revokeOne(t *testing.T, pairing string) (*v1alpha1types.Session, error) {
	t.Helper()
	s := f.session("mine", f.user)
	s.Spec.PairingReference.Name = pairing
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, s.Name, true)
	return s, f.sc.reconcilePairing(portsTestNS, pairing, nil)
}

// A failed delete (the Session changed meanwhile) is returned, so the
// Pairing's key is retried rather than the revocation dropped.
func TestPairingDeletionRetriesFailedEnd(t *testing.T) {
	f := newPortsFixture(t)
	conflicts := 1
	f.dw.PrependReactor("delete", "sessions", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(v1alpha1types.Resource("sessions"), "mine", nil)
		}
		return false, nil, nil
	})
	s, err := f.revokeOne(t, "client")
	if err == nil {
		t.Fatal("a conflicting delete was not returned for retry")
	}
	if err := f.sc.reconcilePairing(portsTestNS, "client", nil); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("session survived the retry")
	}
}

// An API server error confirming the Pairing is gone keeps the Session.
func TestPairingDeletionKeepsSessionOnAPIError(t *testing.T) {
	f := newPortsFixture(t)
	f.dw.PrependReactor("get", "pairings", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
	})
	s, err := f.revokeOne(t, "client")
	if err == nil {
		t.Error("API error not returned for retry")
	}
	if !f.sessionExists(t, s.Name) {
		t.Error("an API error ended the session")
	}
}

// A Pairing recreated (re-paired) before its delete event is handled keeps
// the Session.
func TestPairingDeletionKeepsSessionOfRecreatedPairing(t *testing.T) {
	f := newPortsFixture(t)
	f.createPairing(t, "client")
	s, err := f.revokeOne(t, "client")
	if err != nil {
		t.Fatal(err)
	}
	if !f.sessionExists(t, s.Name) {
		t.Error("ended the session of a pairing that exists again")
	}
}
