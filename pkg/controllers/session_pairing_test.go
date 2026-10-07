package controllers

import (
	"context"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

type enqueueRecorder struct {
	generic.Controller[*v1alpha1types.Session]
	keys []string
}

func (r *enqueueRecorder) Enqueue(namespace, name string) {
	r.keys = append(r.keys, namespace+"/"+name)
}

// A Pairing deletion re-reconciles that client's Sessions only, rather than
// leaving them to the next unrelated reconcile.
func TestPairingDeletionEnqueuesItsSessions(t *testing.T) {
	ctx := context.Background()
	f := newPortsFixture(t)
	for name, pairing := range map[string]string{"mine": "client", "other": "another", "manual": ""} {
		s := f.session(name, f.user)
		s.Spec.PairingReference.Name = pairing
		if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		f.waitInformer(t, name, true)
	}
	rec := &enqueueRecorder{}
	f.sc.controller = rec

	if err := f.sc.reconcilePairing(portsTestNS, "client", &v1alpha1types.Pairing{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.keys) != 0 {
		t.Fatalf("an existing pairing enqueued %v", rec.keys)
	}
	if err := f.sc.reconcilePairing(portsTestNS, "client", nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{portsTestNS + "/mine"}; !slices.Equal(rec.keys, want) {
		t.Errorf("enqueued %v, want %v", rec.keys, want)
	}
}
