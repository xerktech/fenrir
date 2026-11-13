package controllers

import (
	"context"
	"errors"
	"path"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func containerNamed(spec *corev1.PodSpec, name string) *corev1.Container {
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range ctrs {
			if ctrs[i].Name == name {
				return &ctrs[i]
			}
		}
	}
	return nil
}

func envValue(ctr *corev1.Container, name string) (string, bool) {
	for _, e := range ctr.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func wheelArgs(sess *v1alpha1types.Session) []string {
	return []string{
		"--wheel-socket=/sessions/" + string(sess.UID) + "/wheel/wheel.sock",
		"--wheel-owner=1000:1000",
	}
}

// checkWheelSocket checks app's App containers get the wheel socket: a
// read-only mount of the Session's wheel directory and its path.
func checkWheelSocket(t *testing.T, sess *v1alpha1types.Session, app *corev1.Pod) {
	t.Helper()
	v := volumeNamed(&app.Spec, wheelVolume)
	if v == nil || v.HostPath == nil || v.HostPath.Path != path.Join(testAppPodHostDir, string(sess.UID), "wheel") {
		t.Fatalf("wheel volume %+v", v)
	}
	for _, ctr := range app.Spec.Containers {
		i := slices.IndexFunc(ctr.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == wheelVolume })
		env, hasEnv := envValue(&ctr, WheelSocketEnv)
		switch ctr.Name {
		case v1alpha1types.UdevRelayContainerName, v1alpha1types.VRAgentContainerName:
			if i >= 0 || hasEnv {
				t.Errorf("%s gets the wheel socket", ctr.Name)
			}
			continue
		}
		if i < 0 || !ctr.VolumeMounts[i].ReadOnly || ctr.VolumeMounts[i].MountPath != WheelDir {
			t.Errorf("%s: wheel mount %v", ctr.Name, ctr.VolumeMounts)
		}
		if env != WheelDir+"/wheel.sock" {
			t.Errorf("%s: %s=%q", ctr.Name, WheelSocketEnv, env)
		}
	}
}

// A Moonlight Session of a vrPort App gets the wheel from its Wolf pod's
// input-bridge, which alone owns uinput; the App pod gets the socket and its
// containers keep the security context they had without it.
func TestSplitVRAppPodGetsWheelSocket(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f)
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	_, plain, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	f.sc.VRWheel = true
	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	bridge := containerNamed(&wolf.Spec, v1alpha1types.InputBridgeContainerName)
	if bridge == nil || !slices.Equal(bridge.Args[len(bridge.Args)-2:], wheelArgs(sess)) {
		t.Fatalf("bridge %+v", bridge)
	}
	checkWheelSocket(t, sess, app)
	for _, ctr := range plain.Spec.Containers {
		if got := containerNamed(&app.Spec, ctr.Name); got == nil || !reflect.DeepEqual(got.SecurityContext, ctr.SecurityContext) || !reflect.DeepEqual(got.Resources, ctr.Resources) {
			t.Errorf("%s changed beyond the socket", ctr.Name)
		}
	}
	if volumeNamed(&wolf.Spec, wheelVolume) != nil {
		t.Error("Wolf pod mounts the wheel directory: its bridge reaches it through the sessions directory")
	}
	if !slices.Contains(operatorVolumes, wheelVolume) {
		t.Error("wheel volume not in operatorVolumes: an App could mount it by name")
	}
}

// Without --vr-wheel, or for a flat App, nothing changes.
func TestNoWheelWithoutFlagOrVRPort(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f)
	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	if volumeNamed(&app.Spec, wheelVolume) != nil || slices.ContainsFunc(containerNamed(&wolf.Spec, v1alpha1types.InputBridgeContainerName).Args, func(a string) bool { return a == wheelArgs(sess)[0] }) {
		t.Error("wheel without --vr-wheel")
	}

	// The flat App, flag on.
	f2 := newPortsFixture(t)
	f2.sc.AppPodHostDir = testAppPodHostDir
	f2.sc.VRWheel = true
	_, app2, err := f2.sc.buildPods(podReadySession(f2), testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	if volumeNamed(&app2.Spec, wheelVolume) != nil {
		t.Error("flat App gets the wheel")
	}
}

// nativeWheelPod builds a native VR Session's pod with the wheel on.
func nativeWheelPod(t *testing.T) (*portsFixture, *v1alpha1types.Session, *corev1.Pod) {
	t.Helper()
	f := newPortsFixture(t)
	withVR(t, f)
	f.sc.VRWheel = true
	s := vrSession(f)
	if err := f.sc.allocatePorts(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	pod, err := f.sc.buildVRPod(s)
	if err != nil {
		t.Fatalf("buildVRPod: %v", err)
	}
	return f, s, pod
}

// A native VR pod is user-namespaced and has no Wolf pod: it mounts the
// Session's input and udev host directories as a split App pod does (the
// node arrives as a bind mount of its own; a node in the pod's volumes would
// be nodev), relays udev events in its own network namespace, and holds the
// App until the helper has set up the udev directory.
func TestNativeVRPodWithWheel(t *testing.T) {
	f, s, pod := nativeWheelPod(t)
	f.sc.VRWheel = false
	plain, err := f.sc.buildVRPod(s)
	if err != nil {
		t.Fatal(err)
	}
	if volumeNamed(&plain.Spec, hotplugDevVolume) != nil || plain.Annotations[wheelPodAnnotation] != "" {
		t.Error("wheel wiring without --vr-wheel")
	}
	spec := &pod.Spec
	if spec.HostUsers == nil || *spec.HostUsers || spec.HostNetwork {
		t.Fatal("not a user-namespaced pod off the host network")
	}
	if len(spec.InitContainers) == 0 || spec.InitContainers[0].Name != v1alpha1types.WheelWaitContainerName {
		t.Fatalf("init containers %v, want wheel-wait first", containerNames(spec))
	}
	wait := spec.InitContainers[0]
	if sc := wait.SecurityContext; sc == nil || *sc.RunAsUser != 1000 || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{allCapabilities}) {
		t.Errorf("wheel-wait security context %+v", wait.SecurityContext)
	}
	if !slices.Contains(wait.Args, "--file="+udevControlPath) {
		t.Errorf("wheel-wait args %v", wait.Args)
	}
	if containerNamed(spec, v1alpha1types.UdevRelayContainerName) == nil {
		t.Error("no udev-relay")
	}
	for name, sub := range map[string]string{hotplugDevVolume: "input", hotplugUdevVolume: "udev", udevEventsVolume: "events", wheelVolume: "wheel"} {
		if v := volumeNamed(spec, name); v == nil || v.HostPath == nil || v.HostPath.Path != path.Join(testAppPodHostDir, string(s.UID), sub) {
			t.Errorf("volume %s = %+v", name, v)
		}
	}
	app := containerNamed(spec, "app")
	for _, m := range app.VolumeMounts {
		switch m.Name {
		case hotplugDevVolume:
			if m.MountPath != InputDevPath || !m.ReadOnly || m.MountPropagation == nil || *m.MountPropagation != corev1.MountPropagationHostToContainer {
				t.Errorf("input mount %+v", m)
			}
		case hotplugUdevVolume:
			if !m.ReadOnly {
				t.Errorf("udev mount %+v", m)
			}
		}
	}
	checkWheelSocket(t, s, pod)
	// The App gets no device, capability or privilege for it.
	for _, ctr := range plain.Spec.Containers {
		if got := containerNamed(spec, ctr.Name); got == nil || !reflect.DeepEqual(got.SecurityContext, ctr.SecurityContext) {
			t.Errorf("%s's security context changed", ctr.Name)
		}
	}
	if pod.Annotations[wheelPodAnnotation] != s.Name+"-wheel" || pod.Labels[podRoleLabel] != podRoleVR || pod.Labels[sessionUIDLabel] != string(s.UID) {
		t.Errorf("annotations %v labels %v", pod.Annotations, pod.Labels)
	}
	if err := validateBuiltPod(spec, pod.Labels); err != nil {
		t.Error(err)
	}
}

// The helper pod runs the bridge alone, in the host user namespace, beside
// the VR pod on its node, owned by the Session; it has no API token.
func TestWheelPodRunsBridgeBesideVRPod(t *testing.T) {
	f, s, vrPod := nativeWheelPod(t)
	f.sc.SessionTolerations = []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	f.sc.SessionNodeSelector = map[string]string{"kubernetes.io/hostname": "talos04"}
	vrPod.OwnerReferences = []metav1.OwnerReference{{Kind: sessionKind, Name: s.Name, UID: s.UID, Controller: new(true)}}
	pod, err := f.sc.buildWheelPod(s, vrPod)
	if err != nil {
		t.Fatal(err)
	}
	if got := containerNames(&pod.Spec); !slices.Equal(got, []string{v1alpha1types.InputBridgeContainerName}) {
		t.Errorf("containers %v", got)
	}
	bridge := pod.Spec.Containers[0]
	if !slices.Equal(bridge.Args[len(bridge.Args)-2:], wheelArgs(s)) || bridge.SecurityContext == nil || !*bridge.SecurityContext.Privileged {
		t.Errorf("bridge %+v", bridge)
	}
	if pod.Spec.HostUsers != nil && !*pod.Spec.HostUsers {
		t.Error("helper pod user-namespaced: it could not open uinput or propagate mounts")
	}
	if pod.Spec.HostNetwork || *pod.Spec.AutomountServiceAccountToken || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("spec %+v", pod.Spec)
	}
	term := pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.TopologyKey != corev1.LabelHostname || term.LabelSelector.MatchLabels[podRoleLabel] != podRoleVR || term.LabelSelector.MatchLabels[sessionUIDLabel] != string(s.UID) {
		t.Errorf("affinity %+v", term)
	}
	if !reflect.DeepEqual(pod.Spec.Tolerations, f.sc.SessionTolerations) || pod.Spec.NodeSelector["kubernetes.io/hostname"] != "talos04" {
		t.Errorf("tolerations %v nodeSelector %v", pod.Spec.Tolerations, pod.Spec.NodeSelector)
	}
	if !metav1.IsControlledBy(pod, s) {
		t.Error("not controlled by the Session")
	}
	// The pod watch re-reconciles its Session; Moonlight never counts it.
	for k, want := range map[string]string{
		v1alpha1types.SessionPodLabel:  v1alpha1types.SessionPodLabelValue,
		v1alpha1types.SessionModeLabel: v1alpha1types.SessionModeVR,
		"direwolf/user":                s.Spec.UserReference.Name,
		"direwolf/app":                 s.Spec.GameReference.Name,
		podRoleLabel:                   podRoleWheel,
	} {
		if pod.Labels[k] != want {
			t.Errorf("label %s=%q, want %q", k, pod.Labels[k], want)
		}
	}
}

// reconcilePod creates the helper pod for a VR pod that names one, records
// it, and ends the Session once it is gone or its bridge exited: never
// recreated, as a node left behind would open the next device on its minor.
func TestWheelPodLifecycle(t *testing.T) {
	vrPodWithWheel := func(s *v1alpha1types.Session) *corev1.Pod {
		p := readyPod(s)
		delete(p.Annotations, portBlockAnnotation)
		p.Annotations[vrPortAnnotation] = "21000"
		p.Annotations[wheelPodAnnotation] = s.Name + "-wheel"
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		p.Labels[v1alpha1types.SessionModeLabel] = v1alpha1types.SessionModeVR
		return p
	}
	session := func(created bool) func(f *portsFixture) *v1alpha1types.Session {
		return func(f *portsFixture) *v1alpha1types.Session {
			s := vrSession(f)
			s.Status.Ports = v1alpha1types.SessionPorts{VR: 21000}
			s.Status.Conditions = []metav1.Condition{{Type: "PortsAllocated", Status: metav1.ConditionTrue, Reason: "Test"}, {Type: podCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"}}
			if created {
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{Type: wheelPodCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"})
			}
			return s
		}
	}
	setup := func(t *testing.T, f *portsFixture) {
		t.Helper()
		withVR(t, f)
		f.sc.VRWheel = true
	}

	t.Run("created", func(t *testing.T) {
		f, s := lifecycleFixture(t, session(false), func(s *v1alpha1types.Session) []runtime.Object {
			return []runtime.Object{vrPodWithWheel(s)}
		})
		setup(t, f)
		waitPod(t, f, s.Name)
		if _, err := f.sc.reconcilePod(context.Background(), s); err != nil {
			t.Fatalf("reconcilePod: %v", err)
		}
		if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), s.Name+"-wheel", metav1.GetOptions{}); err != nil {
			t.Fatalf("helper pod not created: %v", err)
		}
		if !conditionTrue(s, wheelPodCreatedCondition) {
			t.Error("WheelPodCreated not recorded")
		}
	})

	for name, mutate := range map[string]func(*corev1.Pod) *corev1.Pod{
		"bridge exited": func(p *corev1.Pod) *corev1.Pod {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  v1alpha1types.InputBridgeContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			}}
			return p
		},
		"helper failed":  func(p *corev1.Pod) *corev1.Pod { p.Status.Phase = corev1.PodFailed; return p },
		"helper deleted": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f, s := lifecycleFixture(t, session(true), func(s *v1alpha1types.Session) []runtime.Object {
				objs := []runtime.Object{vrPodWithWheel(s)}
				if mutate != nil {
					h := readyPod(s)
					h.Name = s.Name + "-wheel"
					delete(h.Annotations, portBlockAnnotation)
					objs = append(objs, mutate(h))
				}
				return objs
			})
			setup(t, f)
			waitPod(t, f, s.Name)
			if mutate != nil {
				waitPod(t, f, s.Name+"-wheel")
			}
			if _, err := f.sc.reconcilePod(context.Background(), s); !errors.Is(err, errSessionEnded) {
				t.Fatalf("reconcilePod = %v, want errSessionEnded", err)
			}
			if f.sessionExists(t, s.Name) {
				t.Error("Session kept")
			}
			if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), s.Name+"-wheel", metav1.GetOptions{}); mutate == nil && err == nil {
				t.Error("helper pod recreated")
			}
		})
	}
}

// A native VR Session ending with its wheel helper's bridge dead, or the
// helper pod gone, deletes the VR pod without grace: the bridge's nodes stay
// mounted there (XERK-2859). A running or clean bridge, or a Session that
// never made a helper, leaves the VR pod to garbage collection.
func TestUncleanWheelBridgeExitKillsVRPod(t *testing.T) {
	bridge := func(code int32) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: v1alpha1types.InputBridgeContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}
		}
	}
	running := func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: v1alpha1types.InputBridgeContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	}
	for _, tc := range []struct {
		name       string
		helper     func(*corev1.Pod) // nil: the helper pod is gone
		notCreated bool
		wantKill   bool
	}{
		{name: "bridge SIGKILLed", helper: bridge(137), wantKill: true},
		{name: "helper evicted", helper: func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }, wantKill: true},
		{name: "helper gone", wantKill: true},
		{name: "bridge running", helper: running},
		{name: "bridge exited cleanly", helper: bridge(0)},
		{name: "no helper made", notCreated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := vrSession(f)
				if !tc.notCreated {
					s.Status.Conditions = []metav1.Condition{{Type: wheelPodCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"}}
				}
				return s
			}, func(s *v1alpha1types.Session) []runtime.Object {
				objs := []runtime.Object{readyPod(s)}
				if tc.helper != nil {
					h := readyPod(s)
					h.Name = s.Name + "-wheel"
					tc.helper(h)
					objs = append(objs, h)
				}
				return objs
			})
			waitPod(t, f, s.Name)
			if tc.helper != nil {
				waitPod(t, f, s.Name+"-wheel")
			}
			if err := f.sc.endSession(context.Background(), s, "test"); err != nil {
				t.Fatalf("endSession: %v", err)
			}
			fake, ok := f.sc.K8sClient.(*k8sfake.Clientset)
			if !ok {
				t.Fatalf("K8sClient is %T", f.sc.K8sClient)
			}
			var grace *int64
			for _, a := range fake.Actions() {
				d, ok := a.(k8stesting.DeleteAction)
				if !ok || d.GetResource().Resource != "pods" {
					continue
				}
				if !tc.wantKill || d.GetName() != s.Name {
					t.Fatalf("unexpected delete of pod %q", d.GetName())
				}
				grace = d.GetDeleteOptions().GracePeriodSeconds
			}
			if killed := grace != nil; killed != tc.wantKill || killed && *grace != 1 {
				t.Errorf("VR pod deleted with grace %v, want kill %v with grace 1", grace, tc.wantKill)
			}
		})
	}
}
