package controllers

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// A client IP Wolf can't stream to is refused for good (so /launch stops
// waiting), before the pod is ready; a pod still starting is only a
// StreamStartFailed worth waiting out (XERK-1336).
func TestStreamRefusedForUnstreamableClientIP(t *testing.T) {
	for _, tc := range []struct {
		clientIP, wantReason string
	}{
		{clientIP: "2001:db8::1", wantReason: v1alpha1types.SessionStreamRefusedReason},
		{clientIP: "192.0.2.10", wantReason: "StreamStartFailed"},
	} {
		t.Run(tc.clientIP, func(t *testing.T) {
			ctx := context.Background()
			f := newPortsFixture(t)
			sessions := f.dw.DirewolfV1alpha1().Sessions(portsTestNS)
			sess := f.session("alex-1", f.user)
			sess.Generation = 3
			sess.Spec.Config.ClientIP = tc.clientIP
			// A zero creation time reads as long unstarted, and the reaper ends it.
			sess.CreationTimestamp = metav1.Now()
			sess, err := sessions.Create(ctx, sess, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// The fake pod never goes ready.
			_ = f.sc.Reconcile(portsTestNS, sess.Name, sess)
			got, err := sessions.Get(ctx, sess.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1types.SessionStreamStartedCondition)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
				t.Fatalf("StreamStarted = %+v, want False/%s", cond, tc.wantReason)
			}
			if tc.wantReason == v1alpha1types.SessionStreamRefusedReason && cond.ObservedGeneration != 3 {
				t.Errorf("observedGeneration = %d, want 3", cond.ObservedGeneration)
			}
		})
	}
}
