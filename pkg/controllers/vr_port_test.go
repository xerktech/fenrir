package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

var testVRRange = PortRange{Min: 21000, Max: 21001}

// withVR gives f's controller a VR range and the example App spec.vrPort
// (with requireAppPod and the split App pod, which it needs).
func withVR(t *testing.T, f *portsFixture) *v1alpha1types.App {
	t.Helper()
	app := withVRApp(t, f)
	pairTestHeadset(t, f)
	return app
}

// withVRApp is withVR with no Headset paired: the User's VR Sessions end.
func withVRApp(t *testing.T, f *portsFixture) *v1alpha1types.App {
	t.Helper()
	f.sc.VRPortRange = testVRRange
	f.sc.vrPorts = newVRPortAllocator(testVRRange)
	f.sc.AppPodHostDir = testAppPodHostDir
	app, err := f.sc.AppInformer.Namespaced(portsTestNS).Get(f.app)
	if err != nil {
		t.Fatal(err)
	}
	// The informer's copy: the controller reads it back.
	app.Spec.VRPort = true
	app.Spec.RequireAppPod = true
	return app
}

func TestVRPortAllocatorHandsOutSinglePorts(t *testing.T) {
	a := newVRPortAllocator(testVRRange)
	p1, err1 := a.AllocateBase("ns/a")
	p2, err2 := a.AllocateBase("ns/b")
	if err1 != nil || err2 != nil || p1 != 21000 || p2 != 21001 {
		t.Fatalf("allocations = %d %v, %d %v; want 21000, 21001", p1, err1, p2, err2)
	}
	if _, err := a.AllocateBase("ns/c"); err == nil {
		t.Fatal("third VR port allocated from a two-port range")
	}
	a.Retain(sets.New("ns/b"))
	if p, err := a.AllocateBase("ns/c"); err != nil || p != 21000 {
		t.Fatalf("after release = %d, %v; want 21000", p, err)
	}
	if err := a.ClaimBase("ns/d", 21001); err == nil {
		t.Fatal("claimed a port another owner holds")
	}
	if err := a.ClaimBase("ns/d", 21002); err == nil {
		t.Fatal("claimed a port outside the range")
	}
}

// The VR port lives outside the session block, so the block layout (and with
// it every block recorded in a running Session's status) is unchanged.
func TestVRPortLeavesSessionBlocksAlone(t *testing.T) {
	if sessionPortBlockSize != 7 {
		t.Fatalf("session block is %d ports; changing it moves every recorded block", sessionPortBlockSize)
	}
	a := newPortAllocator(PortRange{Min: 20000, Max: 20034})
	withVRField := blockPorts(20007)
	withVRField.VR = 21000
	if err := a.Claim("ns/vr", withVRField); err != nil {
		t.Fatalf("Claim of a block whose status also carries a VR port: %v", err)
	}
	if err := a.Claim("ns/old", blockPorts(20000)); err != nil {
		t.Fatalf("Claim of an old-layout block: %v", err)
	}
}

func TestParseVRPortRange(t *testing.T) {
	r, err := ParseVRPortRange("21000-21000")
	if err != nil || r != (PortRange{Min: 21000, Max: 21000}) {
		t.Fatalf("one-port range = %+v, %v", r, err)
	}
	for _, bad := range []string{"80-90", "21001-21000", "x"} {
		if _, err := ParseVRPortRange(bad); err == nil {
			t.Errorf("ParseVRPortRange(%q) accepted", bad)
		}
	}
	session := PortRange{Min: 20000, Max: 20034}
	if !(PortRange{Min: 20034, Max: 20040}).Overlaps(session) || (PortRange{Min: 20035, Max: 20040}).Overlaps(session) {
		t.Error("Overlaps is off at the range edge")
	}
}

// An App with spec.vrPort gets one port, recorded in status.ports and handed
// to its App pod (env and TCP+UDP hostPorts), never to the Wolf pod.
func TestVRPortReachesAppPod(t *testing.T) {
	ctx := context.Background()
	f := newPortsFixture(t)
	withVR(t, f)
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(ctx, sess); err != nil {
		t.Fatal(err)
	}
	wantBlock := blockPorts(20000)
	wantBlock.VR = 21000
	if sess.Status.Ports != wantBlock {
		t.Fatalf("status.ports = %+v, want %+v", sess.Status.Ports, wantBlock)
	}

	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatalf("buildPods: %v", err)
	}
	if app.Annotations[vrPortAnnotation] != "21000" {
		t.Errorf("App pod annotations = %v, want %s=21000", app.Annotations, vrPortAnnotation)
	}
	if _, ok := wolf.Annotations[vrPortAnnotation]; ok {
		t.Error("Wolf pod carries the VR port annotation")
	}
	var tcp, udp bool
	for _, p := range app.Spec.Containers[0].Ports {
		if p.ContainerPort == 21000 && p.HostPort == 21000 {
			tcp = tcp || p.Protocol == corev1.ProtocolTCP
			udp = udp || p.Protocol == corev1.ProtocolUDP
		}
	}
	if !tcp || !udp {
		t.Errorf("App container %s ports = %+v, want 21000 TCP and UDP as hostPorts", app.Spec.Containers[0].Name, app.Spec.Containers[0].Ports)
	}
	if app.Spec.HostNetwork {
		t.Error("App pod is host-networked")
	}
	for _, pod := range []*corev1.Pod{wolf, app} {
		for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
			for _, c := range ctrs {
				var got []string
				for _, e := range c.Env {
					if e.Name == vrPortEnv {
						got = append(got, e.Value)
					}
				}
				operator := isWolfPodContainer(c.Name) || c.Name == v1alpha1types.UdevRelayContainerName || c.Name == v1alpha1types.VRAgentContainerName
				switch {
				case operator && len(got) != 0:
					t.Errorf("operator container %s/%s got %s", pod.Name, c.Name, vrPortEnv)
				case !operator && (len(got) != 1 || got[0] != "21000"):
					t.Errorf("App container %s/%s has %s = %v, want [21000]", pod.Name, c.Name, vrPortEnv, got)
				}
				for _, p := range c.Ports {
					if pod == wolf && p.ContainerPort == 21000 {
						t.Errorf("Wolf pod container %s declares the VR port", c.Name)
					}
				}
			}
		}
	}
}

// An App's own DIREWOLF_VR_PORT is replaced, and the operator's comes first
// so the App's variables can refer to it.
func TestWithVRPortReplacesAppValue(t *testing.T) {
	spec := corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app",
		Env:  []corev1.EnvVar{{Name: "A", Value: "$(DIREWOLF_VR_PORT)"}, {Name: vrPortEnv, Value: "9943"}},
	}}}
	orig := spec.DeepCopy()
	withVRPort(&spec, 21000)
	env := spec.Containers[0].Env
	if len(env) != 2 || env[0] != (corev1.EnvVar{Name: vrPortEnv, Value: "21000"}) || env[1].Name != "A" {
		t.Errorf("env = %+v", env)
	}
	if orig.Containers[0].Env[1].Value != "9943" {
		t.Error("withVRPort wrote into the original env slice")
	}
	none := orig.DeepCopy()
	withVRPort(none, 0)
	if len(none.Containers[0].Env) != 2 || len(none.Containers[0].Ports) != 0 {
		t.Error("withVRPort(0) changed the spec")
	}
}

func TestAppWithoutVRPortGetsNone(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f).Spec.VRPort = false
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if sess.Status.Ports.VR != 0 {
		t.Fatalf("status.ports.vr = %d for an App without vrPort", sess.Status.Ports.VR)
	}
	_, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.Annotations[vrPortAnnotation]; ok {
		t.Error("App pod annotated with a VR port")
	}
}

// A vrPort App this operator can't serve is refused as an invalid spec,
// which ends the Session rather than waiting forever.
func TestVRPortRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*portsFixture, *v1alpha1types.App)
		want  string
	}{
		{"no range", func(f *portsFixture, _ *v1alpha1types.App) { f.sc.vrPorts = nil }, "--vr-port-range"},
		{"no requireAppPod", func(_ *portsFixture, a *v1alpha1types.App) { a.Spec.RequireAppPod = false }, "requireAppPod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPortsFixture(t)
			app := withVR(t, f)
			sess := f.session("alex-1", f.user)
			if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			tc.setup(f, app)
			sess.Status.Ports.VR = 0
			if _, _, err := f.sc.buildPods(sess, testWolfPodUID); !errors.Is(err, errInvalidSpec) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildPods = %v, want errInvalidSpec naming %s", err, tc.want)
			}
		})
	}
}

// Without a range a vrPort App's Session still gets its block (buildPod then
// refuses it), and exhausting the VR range waits like a full block range.
func TestVRPortAllocationFailures(t *testing.T) {
	ctx := context.Background()
	f := newPortsFixture(t)
	withVR(t, f)
	f.sc.vrPorts = nil
	s := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(ctx, s); err != nil || s.Status.Ports.VR != 0 || s.Status.Ports.HTTP == 0 {
		t.Fatalf("no range: allocatePorts = %v, ports %+v", err, s.Status.Ports)
	}

	f.sc.vrPorts = newVRPortAllocator(PortRange{Min: 21000, Max: 21000})
	if err := f.sc.allocatePorts(ctx, s); err != nil || s.Status.Ports.VR != 21000 {
		t.Fatalf("allocatePorts = %v, ports %+v", err, s.Status.Ports)
	}
	other := f.session("sam-1", f.user)
	if err := f.sc.allocatePorts(ctx, other); err == nil {
		t.Fatalf("second Session got VR port %d from a one-port range", other.Status.Ports.VR)
	}
}

// An App that turns vrPort on under a running Session gets no port held for
// a pod that will never serve it.
func TestVRPortNotAddedToBuiltPod(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f)
	s := podReadySession(f)
	if err := f.sc.allocatePorts(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.Status.Ports.VR != 0 {
		t.Fatalf("VR port %d allocated after PodCreated", s.Status.Ports.VR)
	}
}

// VR ports recorded before an operator restart are not handed out again,
// and Sessions recorded without one (the old layout) keep their blocks.
func TestClaimRecordedVRPortsOnStart(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f)
	running := f.session("alex-1", f.user)
	running.Status.Ports = blockPorts(20000)
	running.Status.Ports.VR = 21000
	old := f.session("kim-1", f.user)
	old.Status.Ports = blockPorts(20000 + sessionPortBlockSize)

	f.sc.claimRecordedPorts([]*v1alpha1types.Session{running, old})

	sam := f.session("sam-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sam); err != nil {
		t.Fatal(err)
	}
	if sam.Status.Ports.VR != 21001 || sam.Status.Ports.HTTP != 20000+2*sessionPortBlockSize {
		t.Fatalf("sam got %+v, which running pods still hold", sam.Status.Ports)
	}
	if err := f.sc.allocatePorts(context.Background(), running); err != nil || running.Status.Ports.VR != 21000 {
		t.Fatalf("running Session re-claim = %v, ports %+v", err, running.Status.Ports)
	}
}

// An App pod built for another VR port than status.ports.vr (re-allocated,
// e.g. after a restart with a changed range) ends the Session, as a moved
// block ends one; an App pod built with none is left alone.
func TestMovedVRPortEndsSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		built string // "": no annotation
		ends  bool
	}{
		{"moved", "21001", true},
		{"same", "21000", false},
		{"built without", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := podReadySession(f)
				s.Status.Ports.VR = 21000
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{Type: appPodCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"})
				return s
			}, func(s *v1alpha1types.Session) []runtime.Object {
				wolf := readyPod(s)
				wolf.Annotations[appPodAnnotation] = s.Name + "-app"
				app := readyPod(s)
				app.Name = s.Name + "-app"
				delete(app.Annotations, portBlockAnnotation)
				if tc.built != "" {
					app.Annotations[vrPortAnnotation] = tc.built
				}
				return []runtime.Object{wolf, app}
			})
			f.sc.AppPodHostDir = testAppPodHostDir
			waitPod(t, f, sess.Name)
			waitPod(t, f, sess.Name+"-app")

			_, err := f.sc.reconcilePod(context.Background(), sess)
			if ended := errors.Is(err, errSessionEnded); ended != tc.ends {
				t.Fatalf("reconcilePod = %v, want ended=%v", err, tc.ends)
			}
			if f.sessionExists(t, sess.Name) == tc.ends {
				t.Errorf("session exists = %v, want %v", !tc.ends, !tc.ends)
			}
		})
	}
}
