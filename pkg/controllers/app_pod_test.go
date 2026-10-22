package controllers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

const (
	testAppPodHostDir = "/var/lib/direwolf/sessions"
	testWolfPodUID    = "wolf-pod-uid"
)

// splitPods renders the example Steam Session's Wolf pod and App pod.
func splitPods(t *testing.T) (sess *v1alpha1types.Session, wolf, app *corev1.Pod) {
	t.Helper()
	f := newPortsFixture(t)
	f.sc.AppPodHostDir = testAppPodHostDir
	sess = podReadySession(f)
	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatalf("buildPods: %v", err)
	}
	if app == nil {
		t.Fatal("no App pod with AppPodHostDir set")
	}
	return sess, wolf, app
}

func containerNames(spec *corev1.PodSpec) []string {
	var names []string
	for _, ctrs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, c := range ctrs {
			names = append(names, c.Name)
		}
	}
	return names
}

func volumeNamed(spec *corev1.PodSpec, name string) *corev1.Volume {
	i := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == name })
	if i < 0 {
		return nil
	}
	return &spec.Volumes[i]
}

// The App runs alone in a user-namespaced pod off the host network; Wolf and
// the operator's sidecars stay in the host-networked pod (XERK-1920).
func TestSplitAppPodSeparatesTheApp(t *testing.T) {
	sess, wolf, app := splitPods(t)

	if got := containerNames(&app.Spec); !slices.Equal(got, []string{"app", v1alpha1types.UdevRelayContainerName}) {
		t.Errorf("App pod containers = %v, want [app udev-relay]", got)
	}
	if got := containerNames(&wolf.Spec); slices.Contains(got, "app") || !slices.Contains(got, v1alpha1types.InputBridgeContainerName) {
		t.Errorf("Wolf pod containers = %v", got)
	}
	if app.Spec.HostUsers == nil || *app.Spec.HostUsers {
		t.Error("App pod is not in a user namespace")
	}
	if app.Spec.HostNetwork || app.Spec.HostIPC || app.Spec.DNSPolicy == corev1.DNSClusterFirstWithHostNet {
		t.Errorf("App pod hostNetwork=%v hostIPC=%v dnsPolicy=%s", app.Spec.HostNetwork, app.Spec.HostIPC, app.Spec.DNSPolicy)
	}
	if !wolf.Spec.HostNetwork || wolf.Spec.HostUsers != nil {
		t.Errorf("Wolf pod hostNetwork=%v hostUsers=%v, want the host's", wolf.Spec.HostNetwork, wolf.Spec.HostUsers)
	}
	if app.Name == wolf.Name || wolf.Annotations[appPodAnnotation] != app.Name {
		t.Errorf("Wolf pod %s annotates App pod %q, App pod is %s", wolf.Name, wolf.Annotations[appPodAnnotation], app.Name)
	}
	if _, ok := app.Annotations[portBlockAnnotation]; ok {
		t.Error("App pod carries the port block annotation")
	}
	if !metav1.IsControlledBy(app, sess) || app.Labels[v1alpha1types.SessionPodLabel] != v1alpha1types.SessionPodLabelValue {
		t.Error("App pod not owned by the Session or lacks the session pod label (nri-input's grants)")
	}
	terms := app.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 || terms[0].LabelSelector.MatchLabels[sessionUIDLabel] != string(sess.UID) ||
		wolf.Labels[sessionUIDLabel] != string(sess.UID) || wolf.Labels[podRoleLabel] != terms[0].LabelSelector.MatchLabels[podRoleLabel] {
		t.Errorf("App pod affinity %+v does not select the Wolf pod (labels %v)", terms, wolf.Labels)
	}
}

// The App pod gets none of the operator's secrets or sockets: no token, no
// Wolf config (its API socket), no bridge.
func TestSplitAppPodHoldsNoOperatorSecrets(t *testing.T) {
	_, wolf, app := splitPods(t)
	for _, v := range []string{"wolf-agent-token", "wolf-cfg", inputBridgeVolume, appPodSessionsVolume} {
		if volumeNamed(&app.Spec, v) != nil {
			t.Errorf("App pod has volume %s", v)
		}
		if volumeNamed(&wolf.Spec, v) == nil {
			t.Errorf("Wolf pod lacks volume %s", v)
		}
	}
	if app.Spec.AutomountServiceAccountToken == nil || *app.Spec.AutomountServiceAccountToken {
		t.Error("App pod mounts a ServiceAccount token")
	}
}

// The pods share the runtime and hotplug directories through per-Session host
// directories; the App reads the hotplug ones only, and receives the bridge's
// node mounts by propagation.
func TestSplitAppPodSharesHostDirectories(t *testing.T) {
	sess, wolf, app := splitPods(t)
	for _, tc := range []struct {
		volume, sub string
		pods        []*corev1.Pod
	}{
		{hotplugUdevVolume, "udev", []*corev1.Pod{wolf, app}},
		{hotplugDevVolume, "input", []*corev1.Pod{app}},
	} {
		want := testAppPodHostDir + "/" + string(sess.UID) + "/" + tc.sub
		for _, pod := range tc.pods {
			v := volumeNamed(&pod.Spec, tc.volume)
			if v == nil || v.HostPath == nil || v.HostPath.Path != want {
				t.Errorf("%s volume %s = %+v, want hostPath %s", pod.Name, tc.volume, v, want)
			}
		}
	}
	if volumeNamed(&wolf.Spec, hotplugDevVolume) != nil {
		t.Error("Wolf pod still has the /dev/input volume: wolf-agent must publish through the bridge")
	}

	ctr := app.Spec.Containers[0]
	for _, m := range ctr.VolumeMounts {
		switch m.Name {
		case hotplugDevVolume:
			if !m.ReadOnly || m.MountPropagation == nil || *m.MountPropagation != corev1.MountPropagationHostToContainer {
				t.Errorf("App /dev/input mount = %+v, want read-only HostToContainer", m)
			}
		case hotplugUdevVolume:
			if !m.ReadOnly {
				t.Errorf("App /run/udev mount = %+v, want read-only", m)
			}
		}
	}
	got := mountsAt(t, &app.Spec, "app")
	if got[InputDevPath] != hotplugDevVolume || got["/run/udev"] != hotplugUdevVolume || got["/tmp/.X11-unix"] != "wolf-runtime" {
		t.Errorf("App mounts %v", got)
	}
}

// wolf-agent publishes nodes through the privileged bridge, which alone holds
// the sessions directory, Bidirectional.
func TestSplitAppPodWiresInputBridge(t *testing.T) {
	sess, wolf, _ := splitPods(t)
	var agent, bridge *corev1.Container
	for i := range wolf.Spec.Containers {
		switch wolf.Spec.Containers[i].Name {
		case v1alpha1types.WolfAgentContainerName:
			agent = &wolf.Spec.Containers[i]
		case v1alpha1types.InputBridgeContainerName:
			bridge = &wolf.Spec.Containers[i]
		}
	}
	if agent == nil || bridge == nil {
		t.Fatal("missing wolf-agent or input-bridge")
	}
	if !slices.Contains(agent.Args, "--input-bridge-socket="+inputBridgeSocket) {
		t.Errorf("wolf-agent args %v lack the bridge socket", agent.Args)
	}
	if m := mountsAt(t, &wolf.Spec, v1alpha1types.WolfAgentContainerName); m[InputDevPath] != "" || m["/run/udev"] != "" || m[inputBridgeSocketDir] != inputBridgeVolume {
		t.Errorf("wolf-agent mounts %v", m)
	}
	if !slices.Contains(bridge.Args, "--session-dir="+inputBridgeSessionsPath+"/"+string(sess.UID)) {
		t.Errorf("bridge args %v", bridge.Args)
	}
	for _, c := range wolf.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == appPodSessionsVolume && (c.Name != v1alpha1types.InputBridgeContainerName ||
				m.MountPropagation == nil || *m.MountPropagation != corev1.MountPropagationBidirectional) {
				t.Errorf("container %s mounts the sessions directory: %+v", c.Name, m)
			}
		}
	}
	if v := volumeNamed(&wolf.Spec, appPodSessionsVolume); v == nil || v.HostPath == nil || v.HostPath.Path != testAppPodHostDir {
		t.Errorf("sessions volume = %+v", v)
	}
}

// spec.gpu's named claim is in both pods: Wolf encodes on the game's card.
func TestSplitAppPodSharesGPUClaim(t *testing.T) {
	sess, wolf, app := splitPods(t)
	for _, pod := range []*corev1.Pod{wolf, app} {
		i := slices.IndexFunc(pod.Spec.ResourceClaims, func(rc corev1.PodResourceClaim) bool { return rc.Name == appGPUClaim })
		if i < 0 || pod.Spec.ResourceClaims[i].ResourceClaimName == nil || *pod.Spec.ResourceClaims[i].ResourceClaimName != gpuClaimName(sess.Name) {
			t.Errorf("%s resourceClaims = %+v", pod.Name, pod.Spec.ResourceClaims)
		}
	}
}

// A template-generated claim is per pod, so Wolf and the game would get
// different cards: refused as an invalid spec.
func TestSplitAppPodRefusesSharedTemplateClaim(t *testing.T) {
	f := newPortsFixture(t)
	f.sc.AppPodHostDir = testAppPodHostDir
	sess := podReadySession(f)
	pod, err := f.sc.buildPod(sess)
	if err != nil {
		t.Fatal(err)
	}
	pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{Name: "nic", ResourceClaimTemplateName: new("t")})
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == "app" || c.Name == v1alpha1types.WolfContainerName {
			c.Resources.Claims = append(c.Resources.Claims, corev1.ResourceClaim{Name: "nic"})
		}
	}
	if _, _, err := f.sc.splitAppPod(sess, pod, testWolfPodUID); !errors.Is(err, errInvalidSpec) {
		t.Errorf("splitAppPod = %v, want errInvalidSpec", err)
	}
}

// Without AppPodHostDir the App stays in the one session pod.
func TestBuildPodsWithoutHostDirKeepsOnePod(t *testing.T) {
	f := newPortsFixture(t)
	sess := podReadySession(f)
	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil || app != nil || !slices.Contains(containerNames(&wolf.Spec), "app") {
		t.Fatalf("buildPods = %v, %v, %v", wolf, app, err)
	}
	if _, ok := wolf.Annotations[appPodAnnotation]; ok {
		t.Error("single pod annotated with an App pod")
	}
}

func waitPod(t *testing.T, f *portsFixture, name string) {
	t.Helper()
	for range 500 {
		if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(name); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pod %s never reached the informer", name)
}

// A split Session's App pod is created next to its Wolf pod, and recorded.
func TestReconcilePodCreatesAppPod(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, func(s *v1alpha1types.Session) []runtime.Object {
		p := readyPod(s)
		p.Annotations[appPodAnnotation] = s.Name + "-app"
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue})
		return []runtime.Object{p}
	})
	f.sc.AppPodHostDir = testAppPodHostDir
	waitPod(t, f, sess.Name)

	if _, err := f.sc.reconcilePod(context.Background(), sess); err != nil {
		t.Fatalf("reconcilePod: %v", err)
	}
	app, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), sess.Name+"-app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("App pod not created: %v", err)
	}
	if app.Spec.HostUsers == nil || *app.Spec.HostUsers {
		t.Error("App pod not user-namespaced")
	}
	if !conditionTrue(sess, appPodCreatedCondition) {
		t.Error("AppPodCreated not recorded")
	}
}

// Until kubelet has set up the Wolf pod's runtime emptyDir, the App pod's
// mount of it would fail and back off.
func TestAppPodWaitsForWolfPodInit(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, func(s *v1alpha1types.Session) []runtime.Object {
		p := readyPod(s)
		p.Annotations[appPodAnnotation] = s.Name + "-app"
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionFalse})
		return []runtime.Object{p}
	})
	f.sc.AppPodHostDir = testAppPodHostDir
	waitPod(t, f, sess.Name)

	if _, err := f.sc.reconcilePod(context.Background(), sess); err != nil {
		t.Fatalf("reconcilePod: %v", err)
	}
	if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), sess.Name+"-app", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("App pod created before the Wolf pod initialized: %v", err)
	}
	if conditionTrue(sess, appPodCreatedCondition) {
		t.Error("AppPodCreated recorded")
	}
}

// The runtime directory is the Wolf pod's size-limited memory emptyDir, not
// a directory on the node's disk the App could fill (XERK-2177); the App pod
// reaches it through kubelet's directory for it, which must already exist.
func TestSplitAppPodSharesRuntimeInMemory(t *testing.T) {
	_, wolf, app := splitPods(t)
	v := volumeNamed(&wolf.Spec, wolfRuntimeVolume)
	if v == nil || v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory ||
		v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse(runtimeSizeLimit)) != 0 {
		t.Errorf("Wolf runtime volume = %+v, want a %s memory emptyDir", v, runtimeSizeLimit)
	}
	want := "/var/lib/kubelet/pods/" + testWolfPodUID + "/volumes/kubernetes.io~empty-dir/" + wolfRuntimeVolume
	v = volumeNamed(&app.Spec, wolfRuntimeVolume)
	if v == nil || v.HostPath == nil || v.HostPath.Path != want || v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathDirectory {
		t.Errorf("App runtime volume = %+v, want hostPath Directory %s", v, want)
	}
	for _, pod := range []*corev1.Pod{wolf, app} {
		for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
			for _, c := range ctrs {
				for _, m := range c.VolumeMounts {
					if m.Name == wolfRuntimeVolume && m.SubPath != runtimeSubPath {
						t.Errorf("%s/%s mounts the runtime volume at subPath %q, want %q", pod.Name, c.Name, m.SubPath, runtimeSubPath)
					}
				}
			}
		}
	}
}

func conditionTrue(s *v1alpha1types.Session, typ string) bool {
	for _, c := range s.Status.Conditions {
		if c.Type == typ {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// Like the Wolf pod, the App pod runs once: gone after being created, or with
// an exited container, it ends the Session and is not recreated.
func TestFinishedAppPodEndsSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		appPod func(*corev1.Pod) *corev1.Pod
	}{
		{"game exited", func(p *corev1.Pod) *corev1.Pod {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}},
			}
			return p
		}},
		{"app pod failed", func(p *corev1.Pod) *corev1.Pod { p.Status.Phase = corev1.PodFailed; return p }},
		{"app pod deleted", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := podReadySession(f)
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{Type: appPodCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"})
				return s
			}, func(s *v1alpha1types.Session) []runtime.Object {
				wolf := readyPod(s)
				wolf.Annotations[appPodAnnotation] = s.Name + "-app"
				objs := []runtime.Object{wolf}
				if tc.appPod != nil {
					app := readyPod(s)
					app.Name = s.Name + "-app"
					delete(app.Annotations, portBlockAnnotation)
					objs = append(objs, tc.appPod(app))
				}
				return objs
			})
			f.sc.AppPodHostDir = testAppPodHostDir
			waitPod(t, f, sess.Name)
			if tc.appPod != nil {
				waitPod(t, f, sess.Name+"-app")
			}

			if _, err := f.sc.reconcilePod(context.Background(), sess); !errors.Is(err, errSessionEnded) {
				t.Fatalf("reconcilePod = %v, want errSessionEnded", err)
			}
			if f.sessionExists(t, sess.Name) {
				t.Error("session not deleted")
			}
			if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), sess.Name+"-app", metav1.GetOptions{}); tc.appPod == nil && err == nil {
				t.Error("App pod recreated")
			}
		})
	}
}

// The stream waits for the App pod as for the Wolf pod.
func TestStreamWaitsForAppPod(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, func(s *v1alpha1types.Session) []runtime.Object {
		wolf := readyPod(s)
		wolf.Annotations[appPodAnnotation] = s.Name + "-app"
		app := readyPod(s)
		app.Name = s.Name + "-app"
		app.Status.Phase = corev1.PodPending
		app.Status.Conditions = nil
		return []runtime.Object{wolf, app}
	})
	waitPod(t, f, sess.Name+"-app")
	wolf, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(sess.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = f.sc.reconcileActiveStreams(context.Background(), sess, wolf)
	if err == nil || !strings.Contains(err.Error(), "app pod") {
		t.Errorf("reconcileActiveStreams = %v, want app pod not ready", err)
	}
}

// The App pod's own udev-relay announces hotplugged pads there: its network
// namespace doesn't hear the node's udevd. It alone mounts the bridge's event
// socket, and holds NET_ADMIN only (the pod user namespace's).
func TestSplitAppPodRelaysUdevEvents(t *testing.T) {
	sess, wolf, app := splitPods(t)
	var relay *corev1.Container
	for i := range app.Spec.Containers {
		if app.Spec.Containers[i].Name == v1alpha1types.UdevRelayContainerName {
			relay = &app.Spec.Containers[i]
		}
	}
	if relay == nil {
		t.Fatal("no udev-relay in the App pod")
	}
	sc := relay.SecurityContext
	if sc == nil || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Add, []corev1.Capability{"NET_ADMIN"}) ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("udev-relay securityContext = %+v", sc)
	}
	v := volumeNamed(&app.Spec, udevEventsVolume)
	if v == nil || v.HostPath == nil || v.HostPath.Path != testAppPodHostDir+"/"+string(sess.UID)+"/events" {
		t.Errorf("events volume = %+v", v)
	}
	for _, c := range app.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == udevEventsVolume && c.Name != v1alpha1types.UdevRelayContainerName {
				t.Errorf("container %s mounts the udev event socket", c.Name)
			}
		}
	}
	var bridge *corev1.Container
	for i := range wolf.Spec.Containers {
		if wolf.Spec.Containers[i].Name == v1alpha1types.InputBridgeContainerName {
			bridge = &wolf.Spec.Containers[i]
		}
	}
	want := "--events-socket=" + inputBridgeSessionsPath + "/" + string(sess.UID) + "/events/" + udevEventsSocket
	if bridge == nil || !slices.Contains(bridge.Args, want) {
		t.Errorf("bridge args lack %s", want)
	}
}

// Every volume the split adds is the operator's: an App container mounting
// one by name (the udev event socket, the bridge's) would be refused.
func TestSplitAppPodVolumesAreOperatorVolumes(t *testing.T) {
	f := newPortsFixture(t)
	f.sc.AppPodHostDir = testAppPodHostDir
	sess := podReadySession(f)
	combined, err := f.sc.buildPod(sess)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]bool{}
	for _, v := range combined.Spec.Volumes {
		before[v.Name] = true
	}
	wolf, app, err := f.sc.splitAppPod(sess, combined, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range []*corev1.Pod{wolf, app} {
		for _, v := range pod.Spec.Volumes {
			if !before[v.Name] && !slices.Contains(operatorVolumes, v.Name) {
				t.Errorf("%s volume %q is added by the split but not in operatorVolumes", pod.Name, v.Name)
			}
		}
	}
}

// The bridge is told every Session the operator knows of, its own first, and
// sweeps only directories older than its Session less sweepMargin (XERK-2178).
func TestInputBridgeSweepsOnlyGoneSessions(t *testing.T) {
	f := newPortsFixture(t)
	f.sc.AppPodHostDir = testAppPodHostDir
	other := f.session("blair-1", f.user)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, other.Name, true)
	sess := podReadySession(f)
	sess.CreationTimestamp = metav1.NewTime(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))

	wolf, _, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	var bridge *corev1.Container
	for i := range wolf.Spec.Containers {
		if wolf.Spec.Containers[i].Name == v1alpha1types.InputBridgeContainerName {
			bridge = &wolf.Spec.Containers[i]
		}
	}
	if bridge == nil {
		t.Fatal("no input-bridge")
	}
	for _, want := range []string{
		"--live-sessions=" + string(sess.UID),
		"--live-sessions=" + string(other.UID),
		"--sweep-before=2026-10-21T11:50:00Z",
	} {
		if !slices.Contains(bridge.Args, want) {
			t.Errorf("bridge args %v lack %s", bridge.Args, want)
		}
	}
}
