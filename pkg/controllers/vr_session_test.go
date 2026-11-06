package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// A VR-mode Session (XERK-2667) ends at once until the operator can run one
// (XERK-2668): building it would start a Wolf pod no Moonlight client reaches,
// holding a port block and GPU for nothing.
func TestVRModeSessionEndsUntilSupported(t *testing.T) {
	f := newPortsFixture(t)
	s := f.session("alex-vr", f.user)
	s.CreationTimestamp = metav1.Now() // not yet reaped as unstarted
	s.Spec.Mode = v1alpha1types.SessionModeVR
	s.Spec.PairingReference = v1alpha1types.PairingReference{}
	s.Spec.GatewayReference = v1alpha1types.GatewayReference{}
	s.Spec.Config = v1alpha1types.SessionInfo{}
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatalf("Reconcile = %v, want the Session ended", err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("a VR-mode Session was kept by an operator that cannot run it")
	}
	pods, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 || s.Status.Ports != (v1alpha1types.SessionPorts{}) {
		t.Errorf("VR-mode Session got %d pods and ports %+v, want none", len(pods.Items), s.Status.Ports)
	}
}
