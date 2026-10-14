package controllers

import (
	"context"
	"errors"
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
		clientIP, wantReason, wantMessage string
	}{
		{
			clientIP: "2001:db8::1", wantReason: v1alpha1types.SessionStreamRefusedReason,
			// Shown to the Moonlight user: no internal names.
			wantMessage: "stream refused: client IP 2001:db8::1 is not an IPv4 address; streaming needs IPv4",
		},
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
			if tc.wantReason == v1alpha1types.SessionStreamRefusedReason &&
				(cond.ObservedGeneration != 3 || cond.Message != tc.wantMessage) {
				t.Errorf("observedGeneration, message = %d, %q, want 3, %q", cond.ObservedGeneration, cond.Message, tc.wantMessage)
			}
		})
	}
}

// A resume refused for its client IP must still mark the old stream
// disconnected, or the grace-period reaper never ends the Session and its pod
// (XERK-1336).
func TestRefusedResumeIsStillDisconnected(t *testing.T) {
	agent := newFakeAgent(t) // lists no streams: the client is gone
	probe := newPortsFixture(t)
	sess := probe.session("alex-1", "alex")
	sess.Status.Ports = blockPorts(agent.base(t))
	sess.Generation = 2
	sess.Spec.Config.ClientIP = "2001:db8::1"
	sess.Status.AttachedGeneration = 1
	sess.Status.WolfSessionID = "old"
	sess.Status.StreamURL = "rtsp://127.0.0.1:1"
	f := newPortsFixture(t, tokenSecret(sess))
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(context.Background(), sess, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess))
	if !errors.Is(err, errStreamRefused) {
		t.Fatalf("reconcileActiveStreams = %v, want a refusal", err)
	}
	if sess.Status.WolfSessionID != "" || sess.Status.StreamURL != "" || sess.Status.DisconnectedAt == nil {
		t.Errorf("status = %+v, want the old stream recorded as disconnected", sess.Status)
	}
	if agent.added != 0 {
		t.Errorf("Wolf got %d adds, want none", agent.added)
	}
}
