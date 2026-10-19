package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1api "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

// TestSessionControllerReconcilePath builds a session CR, runs the controller's
// reconcile helper methods and logs the resulting Pod YAML. This does not call
// the full controller Run loop, but exercises the same code paths the
// controller would use to create the Pod from the App/User/Session CRs.
func TestSessionControllerReconcilePath(t *testing.T) {
	ctx := context.Background()
	sc, fakeK8s, sess, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")

	// The nri-input plugin grants input devices only to pods carrying this label.
	if got := pod.Labels[v1alpha1types.SessionPodLabel]; got != v1alpha1types.SessionPodLabelValue {
		t.Errorf("pod label %s = %q, want %q", v1alpha1types.SessionPodLabel, got, v1alpha1types.SessionPodLabelValue)
	}
	// Job-like: one run per Session, owned by it, never restarted.
	if pod.Name != sess.Name {
		t.Errorf("pod name = %q, want the session's %q", pod.Name, sess.Name)
	}
	if !metav1.IsControlledBy(pod, sess) {
		t.Errorf("pod owners = %+v, want controlled by session %s", pod.OwnerReferences, sess.UID)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	// No per-session Service any more: the pod is on the host network.
	if svcs, _ := fakeK8s.CoreV1().Services(sess.Namespace).List(ctx, metav1.ListOptions{}); len(svcs.Items) != 0 {
		t.Errorf("reconcile created Services: %+v", svcs.Items)
	}
	if got := pod.Annotations[portBlockAnnotation]; got != strconv.Itoa(int(sess.Status.Ports.HTTP)) {
		t.Errorf("pod %s = %q, want %d", portBlockAnnotation, got, sess.Status.Ports.HTTP)
	}
	podSpec := pod.Spec
	if !podSpec.HostNetwork || podSpec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("pod hostNetwork=%v dnsPolicy=%q, want true/%q", podSpec.HostNetwork, podSpec.DNSPolicy, corev1.DNSClusterFirstWithHostNet)
	}
	if got := podSpec.NodeSelector["kubernetes.io/hostname"]; got != "talos04" {
		t.Errorf("nodeSelector kubernetes.io/hostname = %q, want talos04", got)
	}
	if !slices.ContainsFunc(podSpec.Tolerations, func(tol corev1.Toleration) bool { return tol.Key == "nvidia.com/gpu" }) {
		t.Errorf("pod does not tolerate the pinned node's taint: %+v", podSpec.Tolerations)
	}
	// talos04 exposes GPUs only through DRA (allocatable nvidia.com/gpu is 0): a
	// device-plugin request leaves the pod Pending.
	for _, c := range podSpec.Containers {
		if _, ok := c.Resources.Requests["nvidia.com/gpu"]; ok {
			t.Errorf("container %s requests nvidia.com/gpu; GPUs come from DRA claims", c.Name)
		}
		if _, ok := c.Resources.Limits["nvidia.com/gpu"]; ok {
			t.Errorf("container %s limits nvidia.com/gpu; GPUs come from DRA claims", c.Name)
		}
	}

	// wolf-agent must be started with the token file mounted from its Secret.
	var agent *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "wolf-agent" {
			agent = &podSpec.Containers[i]
		}
	}
	if agent == nil {
		t.Fatal("no wolf-agent container")
	}
	if !slices.Contains(agent.Args, "--token-file="+wolfAgentTokenMountPath+"/"+wolfAgentTokenKey) {
		t.Errorf("wolf-agent args missing --token-file: %v", agent.Args)
	}
	for _, arg := range []string{
		"--tls-cert=" + wolfAgentTokenMountPath + "/" + corev1.TLSCertKey,
		"--tls-key=" + wolfAgentTokenMountPath + "/" + corev1.TLSPrivateKeyKey,
	} {
		if !slices.Contains(agent.Args, arg) {
			t.Errorf("wolf-agent args missing %s: %v", arg, agent.Args)
		}
	}

	// Every listener must be on the session's block, so session pods sharing
	// the node IP never collide.
	ports := sess.Status.Ports
	if ports != blockPorts(20000) {
		t.Fatalf("session ports = %+v, want the first block", ports)
	}
	if !slices.Contains(agent.Args, "--port=20006") || agent.Ports[0].ContainerPort != ports.WolfAgent ||
		agent.ReadinessProbe.HTTPGet.Port.IntVal != ports.WolfAgent {
		t.Errorf("wolf-agent not on port %d: args %v ports %+v", ports.WolfAgent, agent.Args, agent.Ports)
	}
	var wolf *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "wolf" {
			wolf = &podSpec.Containers[i]
		}
	}
	if wolf == nil {
		t.Fatal("no wolf container")
	}
	wantEnv := map[string]int32{
		"WOLF_HTTP_PORT":       ports.HTTP,
		"WOLF_HTTPS_PORT":      ports.HTTPS,
		"WOLF_RTSP_SETUP_PORT": ports.RTSP,
		"WOLF_CONTROL_PORT":    ports.Control,
		"WOLF_VIDEO_PING_PORT": ports.VideoRTP,
		"WOLF_AUDIO_PING_PORT": ports.AudioRTP,
	}
	for name, port := range wantEnv {
		if !slices.Contains(wolf.Env, corev1.EnvVar{Name: name, Value: strconv.Itoa(int(port))}) {
			t.Errorf("wolf env missing %s=%d", name, port)
		}
		if !slices.ContainsFunc(wolf.Ports, func(p corev1.ContainerPort) bool { return p.ContainerPort == port }) {
			t.Errorf("wolf does not declare container port %d (%s)", port, name)
		}
	}
	var tokenVolume bool
	for _, v := range podSpec.Volumes {
		if v.Name == "wolf-agent-token" && v.Secret != nil && v.Secret.SecretName == agentTokenSecretName(sess.Name) {
			tokenVolume = true
		}
	}
	if !tokenVolume {
		t.Error("pod has no wolf-agent-token secret volume")
	}
	if _, tokenErr := testAgentToken(ctx, sc, sess); tokenErr != nil {
		t.Errorf("token secret not created by reconcilePod: %v", tokenErr)
	}

	out, err := sigsyaml.Marshal(pod)
	if err != nil {
		t.Fatalf("failed to marshal pod: %v", err)
	}
	t.Logf("Generated Pod YAML:\n%s", string(out))
}

// reconcileFixtures runs the controller's reconcile helpers for the given User and
// App fixtures and returns the resulting session Pod.
func reconcileFixtures(t *testing.T, userPath, appPath string) (*SessionController, *k8sfake.Clientset, *v1alpha1api.Session, *corev1.Pod) {
	t.Helper()
	ctx := context.Background()

	userYamlData, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", userPath, err)
	}
	steamYamlData, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", appPath, err)
	}

	// Unmarshal into API types
	var user v1alpha1api.User
	if err := sigsyaml.Unmarshal(userYamlData, &user); err != nil {
		t.Fatalf("failed to unmarshal user yaml: %v", err)
	}
	var app v1alpha1api.App
	if err := sigsyaml.Unmarshal(steamYamlData, &app); err != nil {
		t.Fatalf("failed to unmarshal app yaml: %v", err)
	}

	// Ensure App is in same namespace as User for this test (controller resolves App by session namespace)
	app.Namespace = user.Namespace

	// Create fake clients pre-seeded with User and App
	fakeDirewolf := generatedclient.NewSimpleClientset(&user, &app)
	fakeK8s := k8sfake.NewClientset()

	// Create informer factories
	dwFactory := generatedinformers.NewSharedInformerFactory(fakeDirewolf, 0)
	k8sFactory := informers.NewSharedInformerFactory(fakeK8s, 0)

	// Build generic informers needed by NewSessionController
	sessionInformer := generic.NewInformer[*v1alpha1types.Session](dwFactory.Direwolf().V1alpha1().Sessions().Informer())
	appInformer := generic.NewInformer[*v1alpha1types.App](dwFactory.Direwolf().V1alpha1().Apps().Informer())
	userInformer := generic.NewInformer[*v1alpha1types.User](dwFactory.Direwolf().V1alpha1().Users().Informer())
	podInformer := generic.NewInformer[*corev1.Pod](k8sFactory.Core().V1().Pods().Informer())

	// Create a session client scoped to the test namespace
	sessionClient := fakeDirewolf.DirewolfV1alpha1().Sessions(user.Namespace)

	// Instantiate controller with nil gateway clients (not used in this test)
	sc := NewSessionController(
		fakeK8s,
		nil,
		nil,
		sessionClient,
		sessionInformer,
		appInformer,
		userInformer,
		fakeDirewolf.DirewolfV1alpha1().Apps(user.Namespace),
		fakeDirewolf.DirewolfV1alpha1().Users(user.Namespace),
		podInformer,
		fakeDirewolf.DirewolfV1alpha1().Pairings(user.Namespace),
		generic.NewInformer[*v1alpha1types.Pairing](dwFactory.Direwolf().V1alpha1().Pairings().Informer()),
		nil,
		SessionControllerOptions{
			SessionPortRange:    PortRange{Min: 20000, Max: 20999},
			SessionNodeSelector: map[string]string{"kubernetes.io/hostname": "talos04"},
			SessionTolerations: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpEqual, Value: "present", Effect: corev1.TaintEffectNoSchedule},
			},
		},
	)

	// Start informers and wait for caches
	stopCh := make(chan struct{})
	defer close(stopCh)
	dwFactory.Start(stopCh)
	k8sFactory.Start(stopCh)
	// Wait for cache sync
	if !cacheWaitForSync(k8sFactory, dwFactory, 5*time.Second) {
		t.Fatal("informers failed to sync")
	}

	// Create a Session CR that references the user and app
	sess := &v1alpha1api.Session{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sess-alex-steam",
			Namespace: user.Namespace,
			UID:       "sess-uid",
		},
		Spec: v1alpha1api.SessionSpec{
			UserReference:    v1alpha1api.UserReference{Name: user.Name},
			GameReference:    v1alpha1api.GameReference{Name: app.Name},
			PairingReference: v1alpha1api.PairingReference{Name: "pairing1"},
			GatewayReference: v1alpha1api.GatewayReference{Name: "gw1"},
			Config: v1alpha1api.SessionInfo{
				AESKey:             "k1",
				AESIV:              "i1",
				VideoWidth:         1920,
				VideoHeight:        1080,
				VideoRefreshRate:   60,
				SurroundAudioFlags: 0,
			},
		},
	}

	// Create the Session in the fake client so informers can list it if necessary
	if _, err := fakeDirewolf.DirewolfV1alpha1().Sessions(user.Namespace).Create(ctx, sess, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create session in fake client: %v", err)
	}

	// Run the same sequence the controller uses (without reconcileActiveStreams that talks to wolf)
	// 1) allocatePorts
	if err := sc.allocatePorts(ctx, sess); err != nil {
		t.Fatalf("allocatePorts failed: %v", err)
	}
	// Mark PortsAllocated condition so reconcilePod proceeds
	sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{Type: "PortsAllocated", Status: metav1.ConditionTrue, Reason: "Test", Message: "allocated"})

	// 3) reconcilePVC
	if _, err = sc.reconcilePVC(ctx, sess); err != nil {
		t.Fatalf("reconcilePVC failed: %v", err)
	}

	// 4) reconcilePod (creates the Pod)
	pod, err := sc.reconcilePod(ctx, sess)
	if err != nil {
		t.Fatalf("reconcilePod failed: %v", err)
	}
	return sc, fakeK8s, sess, pod
}

// cacheWaitForSync waits for both informer factories to sync (typed and direwolf)
func cacheWaitForSync(k8sFactory informers.SharedInformerFactory, dwFactory generatedinformers.SharedInformerFactory, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ch := make(chan bool, 1)
	go func() {
		// Wait for both factories' known informers to sync
		k8sFactory.WaitForCacheSync(ctx.Done())
		dwFactory.WaitForCacheSync(ctx.Done())
		ch <- true
	}()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// TestSessionPodGPUFromDRAClaim checks the GPU reaches the session pod only through
// the App's DRA ResourceClaim: the pod keeps spec.resourceClaims, and both the game
// container and the wolf sidecar (NVENC) reference the claim.
func TestSessionPodGPUFromDRAClaim(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/nvidia_devices/user.yaml", "../../examples/nvidia_devices/firefox.yaml") //nolint:dogsled // only the Pod matters here
	spec := pod.Spec

	want := corev1.PodResourceClaim{Name: "gpu", ResourceClaimTemplateName: new("nvidia-gpu")}
	if len(spec.ResourceClaims) != 1 || !reflect.DeepEqual(spec.ResourceClaims[0], want) {
		t.Errorf("pod resourceClaims = %+v, want [%+v]", spec.ResourceClaims, want)
	}
	if spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %q, want unset", *spec.RuntimeClassName)
	}

	claimed := map[string]bool{}
	for _, c := range spec.Containers {
		if slices.Contains(c.Resources.Claims, corev1.ResourceClaim{Name: "gpu"}) {
			claimed[c.Name] = true
		}
		for _, e := range c.Env {
			if e.Name == "NVIDIA_VISIBLE_DEVICES" {
				t.Errorf("container %s has NVIDIA_VISIBLE_DEVICES=%q", c.Name, e.Value)
			}
		}
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Errorf("container %s is privileged", c.Name)
		}
	}
	for _, name := range []string{"app", "wolf"} {
		if !claimed[name] {
			t.Errorf("container %s does not reference the gpu claim", name)
		}
	}
	for _, name := range []string{"wolf-agent", "pulseaudio"} {
		if claimed[name] {
			t.Errorf("sidecar %s should not reference the gpu claim", name)
		}
	}
}

func TestMergeResourceRequirementsKeepsClaims(t *testing.T) {
	defaults := corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}
	overrides := &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}, {Name: "nic", Request: "vf"}}}

	got := mergeResourceRequirements(defaults, overrides)
	want := []corev1.ResourceClaim{{Name: "gpu"}, {Name: "nic", Request: "vf"}}
	if !reflect.DeepEqual(got.Claims, want) {
		t.Errorf("merged claims = %+v, want %+v", got.Claims, want)
	}
	if len(defaults.Claims) != 1 {
		t.Errorf("defaults mutated: %+v", defaults.Claims)
	}
}

// Server-side apply keys resources.claims by name alone, so one name must never
// appear twice; a whole-claim entry wins over a single-request one.
func TestAppendResourceClaimsOneEntryPerName(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dst, add  []corev1.ResourceClaim
		wantClaim []corev1.ResourceClaim
	}{
		{"request then whole", []corev1.ResourceClaim{{Name: "gpu", Request: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu"}}},
		{"whole then request", []corev1.ResourceClaim{{Name: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu", Request: "gpu"}}, []corev1.ResourceClaim{{Name: "gpu"}}},
		{"two requests", nil, []corev1.ResourceClaim{{Name: "gpu", Request: "a"}, {Name: "gpu", Request: "b"}}, []corev1.ResourceClaim{{Name: "gpu"}}},
		{"same request", nil, []corev1.ResourceClaim{{Name: "gpu", Request: "a"}, {Name: "gpu", Request: "a"}}, []corev1.ResourceClaim{{Name: "gpu", Request: "a"}}},
		{"distinct names", []corev1.ResourceClaim{{Name: "gpu"}}, []corev1.ResourceClaim{{Name: "nic", Request: "vf"}}, []corev1.ResourceClaim{{Name: "gpu"}, {Name: "nic", Request: "vf"}}},
	} {
		if got := appendResourceClaims(tc.dst, tc.add...); !reflect.DeepEqual(got, tc.wantClaim) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.wantClaim)
		}
	}
}

// mountsAt maps each mount path of the named container to its volume name.
func mountsAt(t *testing.T, spec *corev1.PodSpec, name string) map[string]string {
	t.Helper()
	for _, c := range spec.Containers {
		if c.Name != name {
			continue
		}
		out := map[string]string{}
		for _, m := range c.VolumeMounts {
			if prev, dup := out[m.MountPath]; dup {
				t.Errorf("container %s mounts %s twice (%s, %s)", name, m.MountPath, prev, m.Name)
			}
			out[m.MountPath] = m.Name
		}
		return out
	}
	t.Fatalf("no container %s", name)
	return nil
}

func TestSessionPodSharesHotplugVolumes(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/nvidia_devices/user.yaml", "../../examples/nvidia_devices/firefox.yaml") //nolint:dogsled // only the Pod matters here
	spec := pod.Spec

	for _, v := range []string{hotplugDevVolume, hotplugUdevVolume} {
		i := slices.IndexFunc(spec.Volumes, func(vol corev1.Volume) bool { return vol.Name == v })
		if i < 0 || spec.Volumes[i].EmptyDir == nil {
			t.Errorf("volume %s missing or not an emptyDir", v)
		}
	}
	for _, name := range []string{"app", "wolf-agent"} {
		got := mountsAt(t, &spec, name)
		if got["/dev/input"] != hotplugDevVolume || got["/run/udev"] != hotplugUdevVolume {
			t.Errorf("container %s mounts /dev/input=%q /run/udev=%q", name, got["/dev/input"], got["/run/udev"])
		}
	}
	for _, name := range []string{"wolf", "pulseaudio"} {
		got := mountsAt(t, &spec, name)
		if _, ok := got["/dev/input"]; ok {
			t.Errorf("container %s should not mount /dev/input", name)
		}
	}
}

// Without udevd's control file libudev never subscribes to udev events, and
// SDL misses every controller plugged after the game starts (XERK-1337).
func TestSessionPodCreatesUdevControl(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/nvidia_devices/user.yaml", "../../examples/nvidia_devices/firefox.yaml") //nolint:dogsled // only the Pod matters here
	i := slices.IndexFunc(pod.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == "init" })
	if i < 0 {
		t.Fatalf("no init container: %+v", pod.Spec.InitContainers)
	}
	ic := pod.Spec.InitContainers[i]
	// libudev's own path, not udevControlPath: a typo there must fail.
	const control = "/run/udev/control"
	if !slices.ContainsFunc(ic.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == hotplugUdevVolume && m.MountPath == path.Dir(control)
	}) {
		t.Errorf("init mounts %+v, want %s at %s", ic.VolumeMounts, hotplugUdevVolume, path.Dir(control))
	}
	if script := strings.Join(ic.Command, " "); !strings.Contains(script, "touch "+control+"\n") {
		t.Errorf("init command does not create %s: %q", control, script)
	}
}

func TestSessionPodKeepsAppsOwnInputMount(t *testing.T) {
	data, err := os.ReadFile("../../examples/steam.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var app v1alpha1api.App
	if err = sigsyaml.Unmarshal(data, &app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Template.Spec.Volumes = append(app.Spec.Template.Spec.Volumes, corev1.Volume{
		Name:         "input-events",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	appCtr := &app.Spec.Template.Spec.Containers[0]
	appCtr.VolumeMounts = append(appCtr.VolumeMounts, corev1.VolumeMount{Name: "input-events", MountPath: "/dev/input"})
	if data, err = sigsyaml.Marshal(&app); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(t.TempDir(), "app.yaml")
	if err = os.WriteFile(appPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", appPath) //nolint:dogsled // only the Pod matters here
	got := mountsAt(t, &pod.Spec, "app")
	if got["/dev/input"] != "input-events" {
		t.Errorf("app /dev/input = %q, want the App's own input-events", got["/dev/input"])
	}
	if got["/run/udev"] != hotplugUdevVolume {
		t.Errorf("app /run/udev = %q, want %s", got["/run/udev"], hotplugUdevVolume)
	}
}

// cmd/nri-input grants devices by container name: a renamed operator container
// would fall through to the App's grant, or Wolf would lose /dev/uinput.
func TestSessionPodOperatorContainerNames(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	var got []string
	for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, ctr := range ctrs {
			if ctr.Name != "app" {
				got = append(got, ctr.Name)
			}
		}
	}
	want := []string{
		v1alpha1types.InitContainerName,
		v1alpha1types.LoopbackShimContainerName,
		v1alpha1types.WolfAgentContainerName,
		v1alpha1types.PulseAudioContainerName,
		v1alpha1types.WolfContainerName,
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("operator containers = %v, want %v", got, want)
	}
}

// cmd/nri-input grants the whole input major (13:*): an App able to mknod
// could open the host's keyboards (XERK-1338).
func TestSessionPodDropsAppMknod(t *testing.T) {
	// steam.yaml adds MKNOD to its app container.
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	for _, ctr := range pod.Spec.Containers {
		var caps *corev1.Capabilities
		if ctr.SecurityContext != nil {
			caps = ctr.SecurityContext.Capabilities
		}
		dropped := caps != nil && slices.Contains(caps.Drop, "MKNOD")
		switch ctr.Name {
		case "app":
			if !dropped || slices.Contains(caps.Add, "MKNOD") {
				t.Errorf("app capabilities = %+v, want MKNOD dropped and not added", caps)
			}
		case "wolf-agent":
			// It mknods the hotplugged controllers.
			if dropped {
				t.Errorf("wolf-agent must keep MKNOD")
			}
		}
	}
}

func TestSessionPodMountsNoServiceAccountToken(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	if got := pod.Spec.AutomountServiceAccountToken; got == nil || *got {
		t.Errorf("automountServiceAccountToken = %v, want false", got)
	}
}

func TestSessionPodKeepsAppsServiceAccountTokenChoice(t *testing.T) {
	data, err := os.ReadFile("../../examples/steam.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var app v1alpha1api.App
	if err = sigsyaml.Unmarshal(data, &app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Template.Spec.AutomountServiceAccountToken = new(true)
	if data, err = sigsyaml.Marshal(&app); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(t.TempDir(), "app.yaml")
	if err = os.WriteFile(appPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", appPath) //nolint:dogsled // only the Pod matters here
	if got := pod.Spec.AutomountServiceAccountToken; got == nil || !*got {
		t.Errorf("automountServiceAccountToken = %v, want the App's true", got)
	}
}

func TestSessionPodDropsAppInitMknod(t *testing.T) {
	data, err := os.ReadFile("../../examples/nvidia_devices/firefox.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var app v1alpha1api.App
	if err = sigsyaml.Unmarshal(data, &app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "app-init", Image: "busybox"}}
	if data, err = sigsyaml.Marshal(&app); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(t.TempDir(), "app.yaml")
	if err = os.WriteFile(appPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, pod := reconcileFixtures(t, "../../examples/nvidia_devices/user.yaml", appPath) //nolint:dogsled // only the Pod matters here
	for _, ctr := range pod.Spec.InitContainers {
		dropped := ctr.SecurityContext != nil && ctr.SecurityContext.Capabilities != nil &&
			slices.Contains(ctr.SecurityContext.Capabilities.Drop, "MKNOD")
		// The operator's own init container runs its fixed command.
		if want := ctr.Name == "app-init"; dropped != want {
			t.Errorf("init container %s: MKNOD dropped = %v, want %v", ctr.Name, dropped, want)
		}
	}
}

func TestDropMknod(t *testing.T) {
	ctr := corev1.Container{SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
		Add: []corev1.Capability{"cap_mknod", "SYS_NICE", "ALL"},
		// containerd reads this as CAP_CAP_MKNOD, dropping nothing.
		Drop: []corev1.Capability{"NET_RAW", "CAP_MKNOD"},
	}}}
	dropMknod(&ctr)
	caps := ctr.SecurityContext.Capabilities
	if want := []corev1.Capability{"SYS_NICE", "ALL"}; !slices.Equal(caps.Add, want) {
		t.Errorf("Add = %v, want %v", caps.Add, want)
	}
	if want := []corev1.Capability{"NET_RAW", "CAP_MKNOD", "MKNOD"}; !slices.Equal(caps.Drop, want) {
		t.Errorf("Drop = %v, want %v", caps.Drop, want)
	}

	var bare corev1.Container
	dropMknod(&bare)
	dropMknod(&bare)
	if got := bare.SecurityContext.Capabilities.Drop; !slices.Equal(got, []corev1.Capability{"MKNOD"}) {
		t.Errorf("bare container Drop = %v, want [MKNOD]", got)
	}
}

func TestWolfCommandPicksClaimedRenderNode(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dri := t.TempDir()
	script := strings.ReplaceAll(wolfCommand[2], "/dev/dri/", dri+"/")
	script = strings.ReplaceAll(script, wolfLoopbackShimPath, loadableLib(t))
	script = strings.ReplaceAll(script, "exec /entrypoint.sh", `echo "$WOLF_RENDER_NODE"`)
	run := func(env ...string) string {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "" {
		t.Errorf("no render node: WOLF_RENDER_NODE = %q, want unset", got)
	}
	// [ -c ] needs a char device; /dev/null stands in for the claimed node.
	if err := os.Symlink("/dev/null", dri+"/renderD129"); err != nil {
		t.Fatal(err)
	}
	if got, want := run(), dri+"/renderD129"; got != want {
		t.Errorf("WOLF_RENDER_NODE = %q, want %q", got, want)
	}
	// The Wolf image's own ENV points at a node this card doesn't have.
	if got, want := run("WOLF_RENDER_NODE="+dri+"/renderD128"), dri+"/renderD129"; got != want {
		t.Errorf("image default kept: WOLF_RENDER_NODE = %q, want %q", got, want)
	}
	// A node that exists (set by the App or a User policy) is kept.
	if got := run("WOLF_RENDER_NODE=/dev/null"); got != "/dev/null" {
		t.Errorf("existing WOLF_RENDER_NODE overridden: got %q", got)
	}
}

func TestSessionPodStartsWolfThroughRenderNodeWrapper(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	for _, c := range pod.Spec.Containers {
		if c.Name == "wolf" {
			if !slices.Equal(c.Command, wolfCommand) {
				t.Errorf("wolf command = %q, want wolfCommand", c.Command)
			}
			return
		}
	}
	t.Fatal("no wolf container")
}

// Wolf's HTTP/HTTPS must not listen on the node IP (XERK-1682): Wolf starts
// only with the loopback shim, copied in from the wolf-agent image.
func TestSessionPodPreloadsLoopbackShim(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	var agentImage string
	for _, c := range pod.Spec.Containers {
		if c.Name == "wolf-agent" {
			agentImage = c.Image
		}
	}
	var copied bool
	for _, c := range pod.Spec.InitContainers {
		if c.Name == wolfLoopbackShimVolume {
			copied = c.Image == agentImage &&
				slices.Equal(c.Command, []string{"cp", wolfLoopbackShimImagePath, wolfLoopbackShimPath}) &&
				slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
					return m.Name == wolfLoopbackShimVolume && m.MountPath == wolfLoopbackShimDir
				})
		}
	}
	if !copied {
		t.Errorf("no init container copying the shim from the wolf-agent image: %+v", pod.Spec.InitContainers)
	}
	if got := mountsAt(t, &pod.Spec, "wolf")[wolfLoopbackShimDir]; got != wolfLoopbackShimVolume {
		t.Errorf("wolf %s = %q, want %s", wolfLoopbackShimDir, got, wolfLoopbackShimVolume)
	}
	// Read-only, or Wolf could swap the shim for a fresh exec of itself.
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == wolfLoopbackShimVolume && !m.ReadOnly {
				t.Errorf("%s mounts the shim read-write", c.Name)
			}
		}
	}

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	shim := filepath.Join(t.TempDir(), "shim.so")
	script := strings.ReplaceAll(wolfCommand[2], wolfLoopbackShimPath, shim)
	script = strings.ReplaceAll(script, "exec /entrypoint.sh", `echo "$LD_PRELOAD"`)
	run := func(env ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run(); err == nil {
		t.Errorf("Wolf started without the shim (LD_PRELOAD=%q)", out)
	}
	// An unloadable shim (here empty) is ignored by ld.so, so it must refuse too.
	if err := os.WriteFile(shim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err == nil {
		t.Errorf("Wolf started with an unloadable shim (LD_PRELOAD=%q)", out)
	}
	lib := loadableLib(t)
	script = strings.ReplaceAll(script, shim, lib)
	shim = lib
	if got, err := run(); err != nil || got != shim {
		t.Errorf("LD_PRELOAD = %q (%v), want %q", got, err, shim)
	}
	if got, err := run("LD_PRELOAD=/user.so"); err != nil || got != shim+" /user.so" {
		t.Errorf("with a User's LD_PRELOAD: LD_PRELOAD = %q (%v), want the shim first", got, err)
	}
}

// loadableLib returns a shared object ld.so can preload, standing in for the
// loopback shim in wolfCommand.
func loadableLib(t *testing.T) string {
	t.Helper()
	for _, pattern := range []string{"/lib/*-linux-gnu/libc.so.6", "/usr/lib/*-linux-gnu/libc.so.6", "/lib64/libc.so.6", "/usr/lib64/libc.so.6"} {
		if m, _ := filepath.Glob(pattern); len(m) > 0 {
			return m[0]
		}
	}
	t.Skip("no glibc to preload")
	return ""
}

func TestSessionPodKeepsAppsOwnHome(t *testing.T) {
	// A Steam App mounts the Steam home it shares with the Library at
	// /home/retro; a second (wolf-data) mount there makes the pod invalid.
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-own-home.yaml") //nolint:dogsled // only the Pod matters here
	if got := mountsAt(t, &pod.Spec, "app")[AppHomePath]; got != "steam-home" {
		t.Errorf("app %s = %q, want the App's own steam-home", AppHomePath, got)
	}
}

// A Library pod that ended without stop() leaves ~/.steam/steam pointing at
// /config, which dangles here; the session pod repoints it itself (XERK-1951).
func TestSessionPodRelinksSharedSteamHome(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-own-home.yaml") //nolint:dogsled // only the Pod matters here
	var relink *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == v1alpha1types.SteamRelinkContainerName {
			relink = &pod.Spec.InitContainers[i]
		}
	}
	if relink == nil {
		t.Fatal("no steam-relink init container for an App sharing the Steam home")
	}
	if len(relink.VolumeMounts) != 1 || relink.VolumeMounts[0].Name != "steam-home" || relink.VolumeMounts[0].MountPath != AppHomePath {
		t.Errorf("steam-relink mounts %v, want only steam-home at %s", relink.VolumeMounts, AppHomePath)
	}
	if sc := relink.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser == 0 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("steam-relink must not run as root: %+v", sc)
	}
}

func TestSteamRelinkContainerKeepsAppMount(t *testing.T) {
	mount := corev1.VolumeMount{Name: "steam-home", MountPath: "home/retro/", SubPath: "homes/retro", ReadOnly: true}
	relink := steamRelinkContainer([]corev1.Container{
		{Name: "other", VolumeMounts: []corev1.VolumeMount{{Name: "wolf-data", MountPath: AppHomePath}}},
		{
			Name:         "app",
			Env:          []corev1.EnvVar{{Name: "PATH", Value: "/opt/game/bin"}},
			EnvFrom:      []corev1.EnvFromSource{{Prefix: "P_"}},
			VolumeMounts: []corev1.VolumeMount{mount},
		},
	}, "img")
	if relink == nil {
		t.Fatal("no relink container")
	}
	want := mount
	want.MountPath = AppHomePath
	if len(relink.VolumeMounts) != 1 || !reflect.DeepEqual(relink.VolumeMounts[0], want) {
		t.Errorf("mounts %+v, want %+v", relink.VolumeMounts, want)
	}
	// An App PATH would hide the image's sh, and a failed start ends the Session.
	if relink.Env != nil || relink.EnvFrom != nil {
		t.Errorf("steam-relink took the App's env: %v %v", relink.Env, relink.EnvFrom)
	}
	// kubelet expands subPathExpr from the mounting container's own env,
	// which steam-relink doesn't take (an App PATH could hide sh).
	mount.SubPath, mount.SubPathExpr = "", "homes/$(UNAME)"
	if c := steamRelinkContainer([]corev1.Container{{VolumeMounts: []corev1.VolumeMount{mount}}}, "img"); c != nil {
		t.Errorf("steam-relink added for a subPathExpr home: %+v", c.VolumeMounts)
	}
	sc := relink.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("steam-relink not confined: %+v", sc)
	}
}

func TestSessionPodSkipsRelinkForOwnHome(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	for _, c := range pod.Spec.InitContainers {
		if c.Name == v1alpha1types.SteamRelinkContainerName {
			t.Error("steam-relink added to a pod with the per-App home")
		}
	}
}

// Runs the session side's real command against a home mounted where the
// session sees it, with nothing at the Library's path.
func TestSteamRelinkContainerCommand(t *testing.T) {
	root := t.TempDir()
	home, libHome := filepath.Join(root, "home-retro"), filepath.Join(root, "config")
	steamDir := filepath.Join(home, ".steam")
	if err := os.MkdirAll(filepath.Join(steamDir, "debian-installation"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(libHome+"/.steam/debian-installation", filepath.Join(steamDir, "steam")); err != nil {
		t.Fatal(err)
	}
	relink := steamRelinkContainer([]corev1.Container{{VolumeMounts: []corev1.VolumeMount{{Name: "steam-home", MountPath: AppHomePath}}}}, "img")
	if relink == nil {
		t.Fatal("no relink container")
	}
	if want := []string{AppHomePath, libraryHome, AppHomePath}; !slices.Equal(relink.Command[len(relink.Command)-3:], want) {
		t.Fatalf("relink args %q, want %q", relink.Command[len(relink.Command)-3:], want)
	}
	command := append(slices.Clone(relink.Command[:len(relink.Command)-3]), home, libHome, home)
	if out, err := exec.CommandContext(context.Background(), command[0], command[1:]...).CombinedOutput(); err != nil { //nolint:gosec // the code's own script
		t.Fatalf("%v: %s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(steamDir, "steam")); err != nil || !fi.IsDir() {
		t.Errorf("~/.steam/steam does not resolve to a directory: %v", err)
	}

	// A failure is logged, never fatal: a failed init container ends the Session.
	if err := os.Remove(filepath.Join(steamDir, "steam")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(libHome+"/.steam/debian-installation", filepath.Join(steamDir, "steam")); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ln"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), command[0], command[1:]...) //nolint:gosec // the code's own script
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("relink exited non-zero on failure: %v: %s", err, out)
	} else if !strings.Contains(string(out), "failed") {
		t.Errorf("relink failure not logged: %q", out)
	}
}

func TestSessionPodMountsAppHomeByDefault(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	if got := mountsAt(t, &pod.Spec, "app")[AppHomePath]; got != "wolf-data" {
		t.Errorf("app %s = %q, want wolf-data", AppHomePath, got)
	}
}

func TestValidateNoHotplugOverride(t *testing.T) {
	for p, wantErr := range map[string]bool{
		"/dev/input":        true,
		"/dev/input/":       true,
		"/dev/input/event":  true,
		"dev/input":         true,
		"run/udev/data":     true,
		"dev/../dev/input":  true,
		"/var/run/udev":     true,
		"var/run/udev/data": true,
		"/run/udev":         true,
		"/run/udev/data":    true,
		"/dev/inputs":       false,
		"/run":              false,
		"/etc/wolf":         false,
	} {
		err := validateNoHotplugOverride([]corev1.VolumeMount{{Name: "v", MountPath: p}})
		if (err != nil) != wantErr {
			t.Errorf("mount at %s: err = %v, want error %v", p, err, wantErr)
		}
	}
}

func TestWithHotplugMountsSeesRelativePaths(t *testing.T) {
	got := withHotplugMounts([]corev1.VolumeMount{{Name: "own", MountPath: "dev/input"}})
	if len(got) != 2 || got[1].MountPath != "/run/udev" {
		t.Errorf("mounts = %+v, want the App's dev/input kept and only /run/udev added", got)
	}
}

func TestBuildPodRejectsWolfAgentHotplugOverride(t *testing.T) {
	sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")
	user, err := sc.UserInformer.Namespaced(sess.Namespace).Get(sess.Spec.UserReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	// The informer's copy: buildPod reads it back below.
	user.Spec.Volumes = append(user.Spec.Volumes, corev1.Volume{Name: "udev-override", VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}})
	user.Spec.SidecarPolicies = &v1alpha1api.SidecarPolicies{WolfAgent: &v1alpha1api.SidecarPolicy{
		VolumeMounts: []corev1.VolumeMount{{Name: "udev-override", MountPath: "run/udev/data"}},
	}}
	if _, err := sc.buildPod(sess); err == nil || !strings.Contains(err.Error(), "reserves for hotplugged devices") {
		t.Errorf("buildPod err = %v, want the hotplug override rejected", err)
	}
}

// The operator's volumes hold wolf-agent's token and TLS key, Wolf's API
// socket and config, and Wolf's preload shim: an App container mounting one
// by name could drive Wolf or impersonate wolf-agent (XERK-1366).
func TestBuildPodRejectsAppMountOfOperatorVolume(t *testing.T) {
	mounts := map[string]corev1.VolumeMount{
		"plain":       {MountPath: "/x"},
		"subPath":     {MountPath: "/x", SubPath: "tls.key"},
		"subPathExpr": {MountPath: "/x", SubPathExpr: "$(POD_NAME)"},
	}
	for _, name := range []string{"wolf-agent-token", "wolf-cfg", "wolf-data", wolfLoopbackShimVolume} {
		for kind, mount := range mounts {
			for _, initCtr := range []bool{false, true} {
				sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")
				app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
				if err != nil {
					t.Fatal(err)
				}
				// The informer's copy: buildPod reads it back below.
				mount.Name = name
				if initCtr {
					app.Spec.Template.Spec.InitContainers = append(app.Spec.Template.Spec.InitContainers,
						corev1.Container{Name: "steal", Image: "busybox", VolumeMounts: []corev1.VolumeMount{mount}})
				} else {
					ctr := &app.Spec.Template.Spec.Containers[0]
					ctr.VolumeMounts = append(ctr.VolumeMounts, mount)
				}
				if _, err := sc.buildPod(sess); err == nil || !strings.Contains(err.Error(), "reserved for the operator") {
					t.Errorf("%s mount of %s (init container: %v): buildPod err = %v, want it rejected", kind, name, initCtr, err)
				}
			}
		}
	}
}

func TestBuildPodRejectsAppDeviceOfOperatorVolume(t *testing.T) {
	for _, initCtr := range []bool{false, true} {
		sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/firefox.yaml")
		app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
		if err != nil {
			t.Fatal(err)
		}
		// The informer's copy: buildPod reads it back below.
		dev := corev1.VolumeDevice{Name: "wolf-data", DevicePath: "/dev/xvda"}
		if initCtr {
			app.Spec.Template.Spec.InitContainers = append(app.Spec.Template.Spec.InitContainers,
				corev1.Container{Name: "steal", Image: "busybox", VolumeDevices: []corev1.VolumeDevice{dev}})
		} else {
			ctr := &app.Spec.Template.Spec.Containers[0]
			ctr.VolumeDevices = append(ctr.VolumeDevices, dev)
		}
		if _, err := sc.buildPod(sess); err == nil || !strings.Contains(err.Error(), "reserved for the operator") {
			t.Errorf("init container %v: buildPod err = %v, want the wolf-data volumeDevice rejected", initCtr, err)
		}
	}
}

// cmd/nri-input grants App containers every input node by number, so a host
// /dev or a privileged container hands the game the host's input (XERK-1915).
func TestBuildPodRejectsAppHostDevices(t *testing.T) {
	hostPath := func(p string) func(*corev1.PodSpec, *corev1.Container) {
		return func(spec *corev1.PodSpec, ctr *corev1.Container) {
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: p},
			}})
			ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: "host", MountPath: "/x"})
		}
	}
	tests := map[string]func(*corev1.PodSpec, *corev1.Container){
		"privileged": func(_ *corev1.PodSpec, ctr *corev1.Container) {
			ctr.SecurityContext = &corev1.SecurityContext{Privileged: new(true)}
		},
		"sys_admin, seccomp unset": func(_ *corev1.PodSpec, ctr *corev1.Container) {
			ctr.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"cap_sys_admin"}}}
		},
		"bpf, pod runtime default": func(spec *corev1.PodSpec, ctr *corev1.Container) {
			spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
			ctr.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"BPF"}}}
		},
		"sys_rawio, container runtime default over pod unconfined": func(spec *corev1.PodSpec, ctr *corev1.Container) {
			spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
			ctr.SecurityContext = &corev1.SecurityContext{
				Capabilities:   &corev1.Capabilities{Add: []corev1.Capability{"SYS_RAWIO"}},
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			}
		},
		"all, seccomp unset": func(_ *corev1.PodSpec, ctr *corev1.Container) {
			ctr.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"ALL"}}}
		},
		// Both expose wolf-agent's /proc/<pid>/root (XERK-1781).
		"host pid": func(spec *corev1.PodSpec, _ *corev1.Container) { spec.HostPID = true },
		"share process namespace": func(spec *corev1.PodSpec, _ *corev1.Container) {
			spec.ShareProcessNamespace = new(true)
		},
		"host root":   hostPath("/"),
		"host dev":    hostPath("/dev/"),
		"host input":  hostPath("/dev/input"),
		"host uinput": hostPath("/dev/uinput"),
		"unclean dev": hostPath("/var/../dev//input"),
		// Every other host path too: these expose the agent token (XERK-1921).
		"host proc":  hostPath("/proc"),
		"host pid 1": hostPath("/proc/1"),
		"kubelet":    hostPath("/var/lib/kubelet/pods"),
		"containerd": hostPath("/run/containerd"),
		"pod logs":   hostPath("/var/log/pods"),
		"other":      hostPath("/devices"),
		"relative":   hostPath("proc"),
		"unmounted": func(spec *corev1.PodSpec, _ *corev1.Container) {
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/kubelet"},
			}})
		},
	}
	// Every capability that opens a syscall cmd/nri-input denies, also when
	// it isn't the first one added.
	for _, c := range []corev1.Capability{"SYS_ADMIN", "BPF", "PERFMON", "SYS_MODULE", "SYS_RAWIO", "SYS_BOOT", "SYS_PACCT", "DAC_READ_SEARCH", "ALL"} {
		tests["add "+string(c)] = func(_ *corev1.PodSpec, ctr *corev1.Container) {
			ctr.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW", c}}}
		}
	}
	// Padded or prefixed: containerd likely reads none of these as the gated
	// cap, but the check stays on the safe side of whatever it does.
	for _, c := range []corev1.Capability{" SYS_ADMIN", "SYS_ADMIN\t", " cap_bpf "} {
		tests["add "+strconv.Quote(string(c))] = func(_ *corev1.PodSpec, ctr *corev1.Container) {
			ctr.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{c}}}
		}
	}
	tests["gated cap first"] = func(_ *corev1.PodSpec, ctr *corev1.Container) {
		ctr.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN", "NET_RAW"}}}
	}
	for name, mutate := range tests {
		for _, initCtr := range []bool{false, true} {
			sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/firefox.yaml")
			app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
			if err != nil {
				t.Fatal(err)
			}
			// The informer's copy: buildPod reads it back below.
			spec := &app.Spec.Template.Spec
			if initCtr {
				spec.InitContainers = append(spec.InitContainers, corev1.Container{Name: "steal", Image: "busybox"})
				mutate(spec, &spec.InitContainers[len(spec.InitContainers)-1])
			} else {
				mutate(spec, &spec.Containers[0])
			}
			if _, err := sc.buildPod(sess); !errors.Is(err, errInvalidSpec) {
				t.Errorf("%s (init container: %v): buildPod err = %v, want it rejected", name, initCtr, err)
			}
		}
	}

	// An unprivileged container and a SYS_ADMIN one under Unconfined seccomp
	// (cmd/nri-input filters it) stay allowed.
	sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/firefox.yaml")
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	app.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: new(false)}
	app.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
	app.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}
	app.Spec.Template.Spec.ShareProcessNamespace = new(false)
	if _, err := sc.buildPod(sess); err != nil {
		t.Errorf("buildPod err = %v, want it allowed", err)
	}
}

// The User picks a host path and the App mounts it by name: the route around
// the App-template hostPath ban (XERK-1950).
func TestBuildPodRejectsAppMountOfUserHostPath(t *testing.T) {
	for _, initCtr := range []bool{false, true} {
		for _, device := range []bool{false, true} {
			sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/firefox.yaml")
			user, err := sc.UserInformer.Namespaced(sess.Namespace).Get(sess.Spec.UserReference.Name)
			if err != nil {
				t.Fatal(err)
			}
			// The informer's copy: buildPod reads it back below.
			user.Spec.Volumes = append(user.Spec.Volumes, corev1.Volume{Name: "dev-input", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev/input"},
			}})
			app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
			if err != nil {
				t.Fatal(err)
			}
			// The informer's copy: buildPod reads it back below.
			ctr := corev1.Container{Name: "steal", Image: "busybox"}
			if device {
				ctr.VolumeDevices = []corev1.VolumeDevice{{Name: "dev-input", DevicePath: "/dev/xvda"}}
			} else {
				ctr.VolumeMounts = []corev1.VolumeMount{{Name: "dev-input", MountPath: "/hostinput"}}
			}
			spec := &app.Spec.Template.Spec
			if initCtr {
				spec.InitContainers = append(spec.InitContainers, ctr)
			} else {
				spec.Containers = append(spec.Containers, ctr)
			}
			if _, err := sc.buildPod(sess); !errors.Is(err, errInvalidSpec) || !strings.Contains(err.Error(), "hostPath volume \"dev-input\"") {
				t.Errorf("init container %v, device %v: buildPod err = %v, want the User's hostPath mount rejected", initCtr, device, err)
			}
		}
	}

	// A User volume that is not a hostPath stays mountable by the game.
	sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/firefox.yaml")
	user, err := sc.UserInformer.Namespaced(sess.Namespace).Get(sess.Spec.UserReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	user.Spec.Volumes = append(user.Spec.Volumes, corev1.Volume{Name: "saves", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "saves"},
	}})
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	app.Spec.Template.Spec.Containers[0].VolumeMounts = append(app.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "saves", MountPath: "/saves"})
	if _, err := sc.buildPod(sess); err != nil {
		t.Errorf("buildPod err = %v, want a User PVC mount allowed", err)
	}
}

// Keeps operatorVolumes in step with buildPod: a volume added there and not
// listed would be mountable by the App.
func TestOperatorVolumesListsEveryOperatorVolume(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	data, err := os.ReadFile("../../examples/steam.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var app v1alpha1api.App
	if err = sigsyaml.Unmarshal(data, &app); err != nil {
		t.Fatal(err)
	}
	userData, err := os.ReadFile("../../examples/user.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var user v1alpha1api.User
	if err = sigsyaml.Unmarshal(userData, &user); err != nil {
		t.Fatal(err)
	}
	notOperator := map[string]bool{}
	for _, v := range app.Spec.Template.Spec.Volumes {
		notOperator[v.Name] = true
	}
	for _, v := range user.Spec.Volumes {
		notOperator[v.Name] = true
	}
	for _, v := range pod.Spec.Volumes {
		if !notOperator[v.Name] && !slices.Contains(operatorVolumes, v.Name) {
			t.Errorf("pod volume %q is the operator's but not in operatorVolumes", v.Name)
		}
	}
}

func TestSessionPodCarriesAppTemplateMetadata(t *testing.T) {
	sc, _, sess, pod := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	if got := pod.Labels["xerktech.com/gpu-claim"]; got != "direwolf-gpu" {
		t.Errorf("pod label xerktech.com/gpu-claim = %q, want the App template's direwolf-gpu", got)
	}
	if got := pod.Annotations["example.com/note"]; got != "kept" {
		t.Errorf("pod annotation example.com/note = %q, want the App template's kept", got)
	}
	if got := pod.Labels["direwolf/app"]; got != sess.Spec.GameReference.Name {
		t.Errorf("pod label direwolf/app = %q, want %q", got, sess.Spec.GameReference.Name)
	}
	if got := pod.Labels["direwolf/user"]; got != sess.Spec.UserReference.Name {
		t.Errorf("pod label direwolf/user = %q, want the operator's %q over the template's", got, sess.Spec.UserReference.Name)
	}

	// The operator's own labels must not leak back into the informer's App.
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"xerktech.com/gpu-claim": "direwolf-gpu", "direwolf/user": "spoof"}; !reflect.DeepEqual(app.Spec.Template.Labels, want) {
		t.Errorf("cached App template labels = %v, want them untouched: %v", app.Spec.Template.Labels, want)
	}
}

func TestSessionPVCCarriesAppTemplateMetadata(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	pvc, err := k8s.CoreV1().PersistentVolumeClaims(sess.Namespace).Get(context.Background(), sc.pvcName(sess), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	if got := pvc.Labels["example.com/backup"]; got != "daily" {
		t.Errorf("PVC label example.com/backup = %q, want the App template's daily", got)
	}
	if got := pvc.Annotations["example.com/note"]; got != "pvc-kept" {
		t.Errorf("PVC annotation example.com/note = %q, want the App template's pvc-kept", got)
	}
	if got := pvc.Labels["direwolf/app"]; got != sess.Spec.GameReference.Name {
		t.Errorf("PVC label direwolf/app = %q, want the operator's %q over the template's", got, sess.Spec.GameReference.Name)
	}

	// The operator's own labels must not leak back into the informer's App.
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"example.com/backup": "daily", "direwolf/app": "spoof"}; !reflect.DeepEqual(app.Spec.VolumeClaimTemplate.Labels, want) {
		t.Errorf("cached App volumeClaimTemplate labels = %v, want them untouched: %v", app.Spec.VolumeClaimTemplate.Labels, want)
	}
}

// TestSessionPVCApplyForcesConflicts relabels the session PVC under another
// field manager, as `kubectl label --overwrite` does. Without Force the next
// apply conflicts on that key and every reconcile fails (XERK-1376).
func TestSessionPVCApplyForcesConflicts(t *testing.T) {
	sc, k8s, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "testdata/app-pod-labels.yaml")
	ctx := context.Background()
	pvcs := k8s.CoreV1().PersistentVolumeClaims(sess.Namespace)
	pvc, err := pvcs.Get(ctx, sc.pvcName(sess), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	pvc.Labels["example.com/backup"] = "weekly"
	pvc.Labels["direwolf/user"] = "someone-else"
	if _, err = pvcs.Update(ctx, pvc, metav1.UpdateOptions{FieldManager: "kubectl-label"}); err != nil {
		t.Fatalf("relabel PVC: %v", err)
	}

	if _, err = sc.reconcilePVC(ctx, sess); err != nil {
		t.Fatalf("reconcilePVC after an out-of-band relabel: %v", err)
	}
	pvc, err = pvcs.Get(ctx, sc.pvcName(sess), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	if got := pvc.Labels["example.com/backup"]; got != "daily" {
		t.Errorf("PVC label example.com/backup = %q, want the App template's daily back", got)
	}
	if got := pvc.Labels["direwolf/user"]; got != sess.Spec.UserReference.Name {
		t.Errorf("PVC label direwolf/user = %q, want the operator's %q back", got, sess.Spec.UserReference.Name)
	}
}

// TestBuildPodLeavesCachedObjects builds pods for two sessions of one App at
// once, as the controller's two workers can. The App and User are the informer
// cache's own, so building a pod must copy their maps and slices, not write
// into them; under -race any such write is reported.
func TestBuildPodLeavesCachedObjects(t *testing.T) {
	sc, _, sess, _ := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml")
	app, err := sc.AppInformer.Namespaced(sess.Namespace).Get(sess.Spec.GameReference.Name)
	if err != nil {
		t.Fatalf("get cached app: %v", err)
	}
	user, err := sc.UserInformer.Namespaced(sess.Namespace).Get(sess.Spec.UserReference.Name)
	if err != nil {
		t.Fatalf("get cached user: %v", err)
	}
	app.Spec.Template.Labels = map[string]string{"team": "games"}
	app.Spec.Template.Annotations = map[string]string{"note": "x"}
	// Decoded slices carry spare capacity, which an append would write into.
	agentMounts := make([]corev1.VolumeMount, 1, 4)
	agentMounts[0] = corev1.VolumeMount{Name: "extra", MountPath: "/extra"}
	user.Spec.SidecarPolicies = &v1alpha1api.SidecarPolicies{
		WolfAgent: &v1alpha1api.SidecarPolicy{VolumeMounts: agentMounts},
	}
	user.Spec.Volumes = append(user.Spec.Volumes, corev1.Volume{
		Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	var wg sync.WaitGroup
	for i := range 2 {
		s := sess.DeepCopy()
		s.Name = fmt.Sprintf("sess-%d", i)
		wg.Go(func() {
			pod, err := sc.buildPod(s)
			if err != nil {
				t.Errorf("buildPod: %v", err)
				return
			}
			if pod.Labels["team"] != "games" || pod.Annotations["note"] != "x" {
				t.Errorf("pod labels %v / annotations %v lost the App's own", pod.Labels, pod.Annotations)
			}
		})
	}
	wg.Wait()

	if want := map[string]string{"team": "games"}; !reflect.DeepEqual(app.Spec.Template.Labels, want) {
		t.Errorf("cached App template labels = %v, want %v", app.Spec.Template.Labels, want)
	}
	if want := map[string]string{"note": "x"}; !reflect.DeepEqual(app.Spec.Template.Annotations, want) {
		t.Errorf("cached App template annotations = %v, want %v", app.Spec.Template.Annotations, want)
	}
	if spare := agentMounts[:2][1]; spare.Name != "" {
		t.Errorf("buildPod wrote %q into the cached User's wolf-agent mounts", spare.Name)
	}
}

// A stream's pipeline starts on its own producer and switches to the lobby's
// (lobbyBufferCaps); with zero copy their caps differ and on VA every second
// switch kills the stream's video (XERK-1363).
func TestSessionPodRunsWolfWithoutZeroCopy(t *testing.T) {
	_, _, _, pod := reconcileFixtures(t, "../../examples/user.yaml", "../../examples/steam.yaml") //nolint:dogsled // only the Pod matters here
	for _, c := range pod.Spec.Containers {
		if c.Name != "wolf" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "WOLF_USE_ZERO_COPY" {
				if e.Value != "FALSE" { // Wolf matches exactly "FALSE"
					t.Errorf("WOLF_USE_ZERO_COPY = %q, want FALSE", e.Value)
				}
				return
			}
		}
		t.Fatal("wolf container runs with zero copy")
	}
	t.Fatal("no wolf container")
}

// An App mount at /dev/input replaces the operator's hotplug dir, so the game
// would see whatever the mount holds (a hostPath hands it every input device on
// the node); cmd/nri-input withholds /dev/uinput from Apps (XERK-1433).
func TestExampleAppsMountNoInputDevices(t *testing.T) {
	top, _ := filepath.Glob("../../examples/*.yaml")
	nested, _ := filepath.Glob("../../examples/*/*.yaml")
	for _, path := range slices.Concat(top, nested) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for doc := range strings.SplitSeq(string(data), "\n---") {
			var app v1alpha1api.App
			if err := sigsyaml.Unmarshal([]byte(doc), &app); err != nil || app.Kind != "App" || app.Spec.Template == nil {
				continue
			}
			spec := app.Spec.Template.Spec
			for _, ctr := range slices.Concat(spec.InitContainers, spec.Containers) {
				for _, m := range ctr.VolumeMounts {
					if m.MountPath == "/dev/input" || m.MountPath == "/dev/uinput" {
						t.Errorf("%s: App %s container %s mounts %s", path, app.Name, ctr.Name, m.MountPath)
					}
				}
			}
		}
	}
}
