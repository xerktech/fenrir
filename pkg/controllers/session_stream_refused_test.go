package controllers

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A never-attached Session whose pod already exists (Reconcile ends one
// without a pod) is refused for its client IP before the pod is ready, so
// /launch stops waiting; a pod still starting is only worth waiting out
// (XERK-1336).
func TestStreamRefusedBeforePodReady(t *testing.T) {
	for _, tc := range []struct {
		clientIP    string
		wantRefused bool
	}{
		{clientIP: "2001:db8::1", wantRefused: true},
		{clientIP: "192.0.2.10"},
	} {
		t.Run(tc.clientIP, func(t *testing.T) {
			f := newPortsFixture(t)
			sess := f.session("alex-1", f.user)
			sess.Spec.Config.ClientIP = tc.clientIP
			pod := readyPod(sess)
			pod.Status = corev1.PodStatus{Phase: corev1.PodPending}

			err := f.sc.reconcileActiveStreams(context.Background(), sess, pod)
			if refused := errors.Is(err, errStreamRefused); refused != tc.wantRefused {
				t.Fatalf("reconcileActiveStreams = %v, want refused=%v", err, tc.wantRefused)
			}
			// Shown to the Moonlight user: no internal names.
			const want = "stream refused: client IP 2001:db8::1 is not an IPv4 address; streaming needs IPv4"
			if tc.wantRefused && err.Error() != want {
				t.Errorf("refusal = %q, want %q", err, want)
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
