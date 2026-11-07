package controllers

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// withNativeVR gives f's controller a VR range and the example App spec.vrPort,
// without --app-pod-host-dir: a VR pod is user-namespaced on its own.
func withNativeVR(t *testing.T, f *portsFixture) *v1alpha1types.App {
	t.Helper()
	app := withVR(t, f)
	f.sc.AppPodHostDir = ""
	return app
}

// vrSession is a VR-mode Session as the library page would create it.
func vrSession(f *portsFixture) *v1alpha1types.Session {
	s := f.session("alex-vr", f.user)
	s.CreationTimestamp = metav1.Now()
	s.Spec.Mode = v1alpha1types.SessionModeVR
	s.Spec.Config = v1alpha1types.SessionInfo{}
	return s
}

// A VR Session gets one pod: the App's containers and vr-agent, with no Wolf,
// wolf-agent, pulseaudio or Wolf config, user-namespaced and off the host
// network, with the VR port as its only hostPort and no port block.
func TestVRSessionBuildsWolflessPod(t *testing.T) {
	f := newPortsFixture(t)
	withNativeVR(t, f)
	s := vrSession(f)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !f.sessionExists(t, s.Name) {
		t.Fatal("VR Session ended")
	}
	if want := (v1alpha1types.SessionPorts{VR: 21000}); s.Status.Ports != want {
		t.Errorf("status.ports = %+v, want only %+v", s.Status.Ports, want)
	}
	pod, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(t.Context(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod: %v", err)
	}
	spec := &pod.Spec
	if got := containerNames(spec); !slices.Equal(got, []string{"app", v1alpha1types.VRAgentContainerName}) {
		t.Errorf("containers = %v, want the App's and vr-agent", got)
	}
	for _, c := range spec.InitContainers {
		if isWolfPodContainer(c.Name) {
			t.Errorf("init container %s is Wolf's", c.Name)
		}
	}
	if spec.HostNetwork || spec.HostIPC || spec.HostUsers == nil || *spec.HostUsers {
		t.Errorf("hostNetwork %v hostIPC %v hostUsers %v; want a user-namespaced pod off the host network", spec.HostNetwork, spec.HostIPC, spec.HostUsers)
	}
	if spec.DNSPolicy == corev1.DNSClusterFirstWithHostNet {
		t.Error("DNS policy is the host network's")
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy %s, want Never", spec.RestartPolicy)
	}
	if _, ok := pod.Annotations[portBlockAnnotation]; ok {
		t.Error("VR pod records a port block")
	}
	if pod.Annotations[vrPortAnnotation] != "21000" || pod.Labels[v1alpha1types.SessionModeLabel] != v1alpha1types.SessionModeVR {
		t.Errorf("annotations %v labels %v", pod.Annotations, pod.Labels)
	}
	var hostPorts []string
	for _, c := range spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				hostPorts = append(hostPorts, c.Name+":"+strconv.Itoa(int(p.HostPort))+"/"+string(p.Protocol))
			}
		}
	}
	if want := []string{"app:21000/TCP", "app:21000/UDP"}; !slices.Equal(hostPorts, want) {
		t.Errorf("hostPorts = %v, want %v", hostPorts, want)
	}
	vols := map[string]corev1.Volume{}
	for _, v := range spec.Volumes {
		vols[v.Name] = v
	}
	for _, gone := range []string{"wolf-cfg", hotplugDevVolume, hotplugUdevVolume} {
		if _, ok := vols[gone]; ok {
			t.Errorf("volume %s kept", gone)
		}
	}
	if rt := vols[wolfRuntimeVolume].EmptyDir; rt == nil || rt.Medium != corev1.StorageMediumMemory || rt.SizeLimit == nil {
		t.Errorf("runtime volume %+v, want the pod's own size-limited memory emptyDir", vols[wolfRuntimeVolume])
	}
	for _, c := range spec.Containers {
		mounts := map[string]string{}
		for _, m := range c.VolumeMounts {
			mounts[m.Name] = m.MountPath
		}
		if mounts[vrBusVolume] != VRBusDir {
			t.Errorf("%s mounts the bus at %q, want %s", c.Name, mounts[vrBusVolume], VRBusDir)
		}
		_, token := mounts[vrAgentOwnTokenVolume]
		if agent := c.Name == v1alpha1types.VRAgentContainerName; token != agent {
			t.Errorf("%s mounts the token: %v", c.Name, token)
		}
		if _, ok := mounts["wolf-agent-token"]; ok {
			t.Errorf("%s mounts wolf-agent's token", c.Name)
		}
		var mode string
		for _, e := range c.Env {
			if e.Name == vrModeEnv {
				mode = e.Value
			}
		}
		if c.Name == "app" && mode != vrModeNative {
			t.Errorf("App %s = %q, want %s", vrModeEnv, mode, vrModeNative)
		}
	}
	agent := spec.Containers[len(spec.Containers)-1]
	sc := agent.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != vrAgentUID || sc.Privileged != nil ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{allCapabilities}) ||
		len(sc.Capabilities.Add) != 0 || agent.Image != f.sc.WolfAgentImage {
		t.Errorf("vr-agent = %+v", agent)
	}
	if _, err := f.sc.K8sClient.CoreV1().Secrets(portsTestNS).Get(t.Context(), vrAgentTokenSecretName(s.Name), metav1.GetOptions{}); err != nil {
		t.Errorf("token Secret: %v", err)
	}
	if v := vols[vrAgentOwnTokenVolume].Secret; v == nil || v.SecretName != vrAgentTokenSecretName(s.Name) {
		t.Errorf("vr-agent token volume %+v, want Secret %s", vols[vrAgentOwnTokenVolume], vrAgentTokenSecretName(s.Name))
	}
	if _, ok := vols["wolf-agent-token"]; ok {
		t.Error("native VR pod keeps wolf-agent's token volume")
	}
}

// A VR Session needs an App with a VR server port; without one it ends at
// once, as an InvalidSpec, before any pod.
func TestVRSessionNeedsVRPortApp(t *testing.T) {
	f := newPortsFixture(t)
	withNativeVR(t, f).Spec.VRPort = false
	s := vrSession(f)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = f.sc.Reconcile(portsTestNS, s.Name, s)
	if f.sessionExists(t, s.Name) {
		t.Error("VR Session for an App without vrPort kept")
	}
	if pods, _ := f.sc.K8sClient.CoreV1().Pods(portsTestNS).List(t.Context(), metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Errorf("pods %v", pods.Items)
	}
}

// The App hardening checks run for a VR pod too, and the bus volume is the
// operator's.
func TestVRPodKeepsAppChecks(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.PodSpec){
		"mounts the token": func(s *corev1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "wolf-agent-token", MountPath: "/x"})
		},
		"mounts the bus volume": func(s *corev1.PodSpec) {
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, corev1.VolumeMount{Name: vrBusVolume, MountPath: "/x"})
		},
		"hostPath": func(s *corev1.PodSpec) {
			s.Volumes = append(s.Volumes, corev1.Volume{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
		"privileged": func(s *corev1.PodSpec) {
			s.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: new(true)}
		},
		"hostPID":        func(s *corev1.PodSpec) { s.HostPID = true },
		"named vr-agent": func(s *corev1.PodSpec) { s.Containers[0].Name = v1alpha1types.VRAgentContainerName },
		"mounts at VRBusDir": func(s *corev1.PodSpec) {
			s.Volumes = append(s.Volumes, corev1.Volume{Name: "mine", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
			s.Containers[0].VolumeMounts = append(s.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "mine", MountPath: VRBusDir})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPortsFixture(t)
			app := withNativeVR(t, f)
			mutate(&app.Spec.Template.Spec)
			s := vrSession(f)
			s.Status.Ports.VR = 21000
			if _, _, err := f.sc.buildPods(s, ""); err == nil || !strings.Contains(err.Error(), errInvalidSpec.Error()) {
				t.Errorf("buildPods = %v, want an InvalidSpec", err)
			}
		})
	}
}

// fakeVRAgent serves vr-agent's status with the pinned test cert.
type fakeVRAgent struct {
	*httptest.Server
	mu     sync.Mutex
	status VRStatus
	fail   bool
	tokens []string
}

func newFakeVRAgent(t *testing.T, cert *tls.Certificate) *fakeVRAgent {
	t.Helper()
	a := &fakeVRAgent{}
	a.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.tokens = append(a.tokens, r.Header.Get("Authorization"))
		if r.URL.Path != VRStatusPath || a.fail {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(a.status)
	}))
	a.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	a.StartTLS()
	t.Cleanup(a.Close)
	return a
}

func (a *fakeVRAgent) set(st VRStatus, fail bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.fail = st, fail
}

func (a *fakeVRAgent) port(t *testing.T) int32 {
	t.Helper()
	addr, ok := a.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("not TCP")
	}
	return int32(addr.Port) //nolint:gosec // a TCP port fits
}

// vrLifecycle is a VR Session whose pod exists (from mutate, nil for none),
// pointed at a fake vr-agent.
func vrLifecycle(t *testing.T, mutate func(*corev1.Pod) *corev1.Pod) (*portsFixture, *v1alpha1types.Session, *fakeVRAgent) {
	t.Helper()
	agent := newFakeVRAgent(t, new(testAgentKeyPair(t)))
	f, s := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := vrSession(f)
		s.Status.Ports = v1alpha1types.SessionPorts{VR: 21000}
		s.Status.Conditions = []metav1.Condition{
			{Type: "PortsAllocated", Status: metav1.ConditionTrue, Reason: "Test"},
			{Type: podCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"},
		}
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object {
		// Both: a test pod (readyPod) mounts neither Secret, so it reads as
		// a pod from before XERK-2747, polled with wolf-agent's.
		objs := []runtime.Object{tokenSecret(s), vrTokenSecret(s)}
		if mutate != nil {
			p := readyPod(s)
			delete(p.Annotations, portBlockAnnotation)
			p.Annotations[vrPortAnnotation] = "21000"
			objs = append(objs, mutate(p))
		}
		return objs
	})
	withNativeVR(t, f)
	f.sc.vrAgentPort = agent.port(t)
	if mutate != nil {
		for range 500 {
			if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(s.Name); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return f, s, agent
}

func identity(p *corev1.Pod) *corev1.Pod { return p }

func headset(s *v1alpha1types.Session) *metav1.Condition {
	return meta.FindStatusCondition(s.Status.Conditions, v1alpha1types.SessionHeadsetConnectedCondition)
}

// The headset drives the Session: a ready pod waits for one (disconnectedAt
// set, so the grace period covers putting it on), a connect clears that, a
// disconnect sets it again, and the grace period running out ends it.
func TestVRSessionFollowsHeadset(t *testing.T) {
	f, s, agent := vrLifecycle(t, identity)

	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatalf("Reconcile (no headset): %v", err)
	}
	if c := headset(s); c == nil || c.Status != metav1.ConditionFalse || c.Reason != vrWaitingForHeadset || s.Status.DisconnectedAt == nil {
		t.Fatalf("ready pod: HeadsetConnected %+v disconnectedAt %v; want waiting with the grace period running", c, s.Status.DisconnectedAt)
	}
	if got := agent.tokens; len(got) == 0 || got[0] != "Bearer token" {
		t.Errorf("agent saw Authorization %v, want the Session's token", got)
	}

	agent.set(VRStatus{HeadsetConnected: true, SessionRunning: true}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if c := headset(s); c.Status != metav1.ConditionTrue || s.Status.DisconnectedAt != nil ||
		!meta.IsStatusConditionTrue(s.Status.Conditions, v1alpha1types.SessionStreamStartedCondition) {
		t.Fatalf("connected: %+v disconnectedAt %v", s.Status.Conditions, s.Status.DisconnectedAt)
	}

	// An agent we can't reach counts as a disconnect; its next answer
	// showing the headset clears it.
	agent.set(VRStatus{}, true)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if c := headset(s); c.Status != metav1.ConditionFalse || c.Reason != vrAgentLost || s.Status.DisconnectedAt == nil {
		t.Fatalf("unreachable agent: %+v disconnectedAt %v; want the grace period running", c, s.Status.DisconnectedAt)
	}
	agent.set(VRStatus{HeadsetConnected: true}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if headset(s).Status != metav1.ConditionTrue || s.Status.DisconnectedAt != nil {
		t.Fatalf("agent back: %+v", s.Status)
	}

	agent.set(VRStatus{}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if c := headset(s); c.Status != metav1.ConditionFalse || c.Reason != vrHeadsetGone || s.Status.DisconnectedAt == nil ||
		meta.IsStatusConditionTrue(s.Status.Conditions, v1alpha1types.SessionStreamStartedCondition) {
		t.Fatalf("disconnected: %+v disconnectedAt %v", s.Status.Conditions, s.Status.DisconnectedAt)
	}
	if !f.sessionExists(t, s.Name) {
		t.Fatal("ended within the grace period")
	}

	s.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-f.sc.DisconnectGracePeriod - time.Second)}
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("VR Session kept past the grace period")
	}
}

// vr-agent's cert is pinned: an agent serving any other cert never sees the
// token, and nothing changes.
func TestVRAgentCertIsPinned(t *testing.T) {
	certPEM, keyPEM, err := generateAgentCert()
	if err != nil {
		t.Fatal(err)
	}
	other, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	f, s, _ := vrLifecycle(t, identity)
	impostor := newFakeVRAgent(t, &other)
	impostor.set(VRStatus{HeadsetConnected: true}, false)
	f.sc.vrAgentPort = impostor.port(t)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if len(impostor.tokens) != 0 {
		t.Errorf("impostor got requests %v", impostor.tokens)
	}
	if headset(s).Status == metav1.ConditionTrue {
		t.Error("impostor's answer was taken")
	}
}

// The pod is Job-like in VR mode too: an exited container, a failed pod or a
// gone pod ends the Session, and the pod is never recreated.
func TestFinishedVRPodEndsSession(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod) *corev1.Pod{
		"App exited": func(p *corev1.Pod) *corev1.Pod {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: v1alpha1types.VRAgentContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "steam", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
			}
			return p
		},
		"vr-agent exited": func(p *corev1.Pod) *corev1.Pod {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: v1alpha1types.VRAgentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}},
			}
			return p
		},
		"pod failed":    func(p *corev1.Pod) *corev1.Pod { p.Status.Phase = corev1.PodFailed; return p },
		"VR port moved": func(p *corev1.Pod) *corev1.Pod { p.Annotations[vrPortAnnotation] = "21001"; return p },
		"pod deleted":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f, s, _ := vrLifecycle(t, mutate)
			if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if f.sessionExists(t, s.Name) {
				t.Error("Session kept")
			}
			if pods, _ := f.sc.K8sClient.CoreV1().Pods(portsTestNS).List(t.Context(), metav1.ListOptions{}); mutate == nil && len(pods.Items) != 0 {
				t.Errorf("pod recreated: %v", pods.Items)
			}
		})
	}
}

// A VR Session is never reaped as an unstarted Moonlight launch (it has no
// Wolf session ID): only one whose pod never became ready, after
// vrUnstartedTTL.
func TestVRSessionUnstartedReaper(t *testing.T) {
	f := newPortsFixture(t)
	now := time.Now()
	s := vrSession(f)
	s.CreationTimestamp = metav1.NewTime(now.Add(-unstartedSessionTTL - time.Minute))
	if r := f.sc.expiredReason(s, now); r != "" {
		t.Errorf("reaped after the Moonlight TTL: %s", r)
	}
	s.CreationTimestamp = metav1.NewTime(now.Add(-vrUnstartedTTL - time.Minute))
	if r := f.sc.expiredReason(s, now); r == "" {
		t.Error("a VR Session whose pod never got ready was kept")
	}
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: v1alpha1types.SessionHeadsetConnectedCondition, Status: metav1.ConditionTrue, Reason: "x"})
	if r := f.sc.expiredReason(s, now); r != "" {
		t.Errorf("a playing VR Session was reaped: %s", r)
	}
}

// An operator restart re-claims a VR Session's port, and no block: it has none.
func TestClaimRecordedPortsVROnly(t *testing.T) {
	f := newPortsFixture(t)
	withNativeVR(t, f)
	s := vrSession(f)
	s.Status.Ports = v1alpha1types.SessionPorts{VR: 21000}
	f.sc.claimRecordedPorts([]*v1alpha1types.Session{s})
	if err := f.sc.vrPorts.ClaimBase("ns/other", 21000); err == nil {
		t.Error("VR port not re-claimed")
	}
	other := f.session("blake-1", "blake")
	if err := f.sc.allocatePorts(t.Context(), other); err != nil || other.Status.Ports.HTTP != 20000 {
		t.Errorf("next Moonlight Session got %+v, %v; want the first block (the VR Session holds none)", other.Status.Ports, err)
	}
}

// A started VR Session whose pod goes NotReady (vr-agent's readiness is WiVRn
// answering) is still polled, and one that stays unreadable is ended after
// the grace period: nothing else would ever end it (QA, XERK-2668).
func TestVRSessionEndsWhenAgentStaysUnreachable(t *testing.T) {
	f, s, agent := vrLifecycle(t, func(p *corev1.Pod) *corev1.Pod {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		return p
	})
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type: v1alpha1types.SessionHeadsetConnectedCondition, Status: metav1.ConditionTrue, Reason: vrHeadsetConnected,
	})
	agent.set(VRStatus{}, true)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if len(agent.tokens) == 0 || s.Status.DisconnectedAt == nil || headset(s).Status == metav1.ConditionTrue {
		t.Fatalf("NotReady started pod: polled %d times, status %+v", len(agent.tokens), s.Status)
	}
	// Further failed polls keep the clock: restarting it would never end
	// the Session.
	first := s.Status.DisconnectedAt.DeepCopy()
	first.Time = first.Add(-time.Minute)
	s.Status.DisconnectedAt = first
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if !s.Status.DisconnectedAt.Equal(first) {
		t.Fatalf("a failed poll moved disconnectedAt from %v to %v", first, s.Status.DisconnectedAt)
	}
	s.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-f.sc.DisconnectGracePeriod - time.Second)}
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("a VR Session nobody can read was kept past the grace period")
	}
}

// A pod mounting vr-agent's own Secret is polled with that Secret's token,
// never wolf-agent's (XERK-2747); one from before mounts and gets wolf-agent's.
func TestVRAgentPolledWithItsOwnToken(t *testing.T) {
	f, s, agent := vrLifecycle(t, func(p *corev1.Pod) *corev1.Pod {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: vrAgentOwnTokenVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: vrAgentTokenSecretName(p.Name)},
		}})
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: v1alpha1types.VRAgentContainerName,
			VolumeMounts: []corev1.VolumeMount{{Name: vrAgentOwnTokenVolume, MountPath: wolfAgentTokenMountPath}}})
		return p
	})
	if err := f.sc.K8sClient.CoreV1().Secrets(portsTestNS).Delete(t.Context(), agentTokenSecretName(s.Name), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if got := agent.tokens; len(got) == 0 || got[0] != "Bearer token" {
		t.Errorf("agent saw Authorization %v, want vr-agent's token", got)
	}
	if c := headset(s); c == nil || c.Reason == vrAgentLost {
		t.Errorf("HeadsetConnected %+v: polled without its own Secret", c)
	}
}

// vr-agent joins the App's session bus as the App's own user, from its
// PUID/PGID: the bus admits only its owner's uid, so a fixed 1000 left an App
// of another PUID with a vr-agent that never went ready (XERK-2709). An id
// the spec can't tell ends the Session up front.
func TestVRAgentRunsAsTheAppsUser(t *testing.T) {
	f := newPortsFixture(t)
	app := withNativeVR(t, f)
	app.Spec.Template.Spec.Containers[0].Env = append(app.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "PUID", Value: "1001"}, corev1.EnvVar{Name: "PGID", Value: "1002"})
	s := vrSession(f)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pod, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(t.Context(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != v1alpha1types.VRAgentContainerName {
			continue
		}
		if sc := c.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1001 || sc.RunAsGroup == nil || *sc.RunAsGroup != 1002 {
			t.Errorf("vr-agent securityContext %+v, want 1001:1002", c.SecurityContext)
		}
		return
	}
	t.Fatal("no vr-agent")
}

func TestVRAgentIDs(t *testing.T) {
	app := func(env ...corev1.EnvVar) corev1.Container { return corev1.Container{Name: "app", Env: env} }
	for _, tc := range []struct {
		name     string
		ctrs     []corev1.Container
		uid, gid int64
		bad      bool
	}{
		{"unset", []corev1.Container{app()}, 1000, 1000, false},
		{"PUID only", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "1001"})}, 1001, 1000, false},
		{"both", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "1001"}, corev1.EnvVar{Name: "PGID", Value: "100"})}, 1001, 100, false},
		{"agreeing containers", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "1001"}), {Name: "side", Env: []corev1.EnvVar{{Name: "PUID", Value: "1001"}}}}, 1001, 1000, false},
		{"operator containers ignored", []corev1.Container{app(), {Name: v1alpha1types.UdevRelayContainerName, Env: []corev1.EnvVar{{Name: "PUID", Value: "0"}}}}, 1000, 1000, false},
		{"empty, as GoW reads it", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: ""})}, 1000, 1000, false},
		{"listed twice: the last wins", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "1000"}, corev1.EnvVar{Name: "PUID", Value: "1001"})}, 1001, 1000, false},
		{"root", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "0"})}, 0, 0, true},
		{"not a number", []corev1.Container{app(corev1.EnvVar{Name: "PGID", Value: "games"})}, 0, 0, true},
		{"too large", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "4294967296"})}, 0, 0, true},
		{"from a source", []corev1.Container{app(corev1.EnvVar{Name: "PUID", ValueFrom: &corev1.EnvVarSource{}})}, 0, 0, true},
		{"disagreeing containers", []corev1.Container{app(corev1.EnvVar{Name: "PUID", Value: "1001"}), {Name: "side", Env: []corev1.EnvVar{{Name: "PUID", Value: "1002"}}}}, 0, 0, true},
	} {
		uid, gid, err := vrAgentIDs(&corev1.PodSpec{Containers: tc.ctrs})
		if (err != nil) != tc.bad || (!tc.bad && (uid != tc.uid || gid != tc.gid)) {
			t.Errorf("%s: %d:%d, %v; want %d:%d, error %v", tc.name, uid, gid, err, tc.uid, tc.gid, tc.bad)
		}
	}
}

// An App whose PUID vr-agent can't take ends its Session at once, not after
// the 15-minute unstarted reaper.
func TestVRSessionWithUnusablePUIDEnds(t *testing.T) {
	f := newPortsFixture(t)
	app := withNativeVR(t, f)
	app.Spec.Template.Spec.Containers[0].Env = append(app.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "PUID", Value: "0"})
	s := vrSession(f)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = f.sc.Reconcile(portsTestNS, s.Name, s)
	if f.sessionExists(t, s.Name) {
		t.Error("VR Session with PUID=0 kept")
	}
}

// The VR reaper records why on the App: the VR page can't show a deleted
// Session, so without it the Session just vanished (XERK-2758).
func TestVRSessionUnstartedReaperWarnsApp(t *testing.T) {
	f, s := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := vrSession(f)
		s.CreationTimestamp = metav1.NewTime(time.Now().Add(-vrUnstartedTTL - time.Minute))
		return s
	}, nil)
	withNativeVR(t, f)
	recorder := record.NewFakeRecorder(10)
	f.sc.Recorder = recorder
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatalf("Reconcile = %v, want the Session ended", err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("a VR Session whose pod never got ready was kept")
	}
	if len(recorder.Events) != 1 || !strings.HasPrefix(<-recorder.Events, "Warning VRPodNeverReady Session "+s.Name) {
		t.Error("no Warning VRPodNeverReady Event for the ended Session")
	}
}
