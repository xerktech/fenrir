package controllers

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// seededWith is a vrLifecycle pod mutation: a VR App pod seeded with headsets.
func seededWith(headsets string) func(*corev1.Pod) *corev1.Pod {
	return func(p *corev1.Pod) *corev1.Pod {
		p.Annotations[vrHeadsetsAnnotation] = headsets
		return p
	}
}

func (f *portsFixture) livePod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	p, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *portsFixture) headsetNames(t *testing.T) []string {
	t.Helper()
	list, err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for _, h := range list.Items {
		names = append(names, h.Name)
	}
	slices.Sort(names)
	return names
}

// A native VR pod seeded with no Headsets pairs by PIN. The key WiVRn then
// reports waits in the Session's pendingHeadsets and becomes no Headset by
// itself, as the game could have planted it (XERK-2742). Once the User
// confirms it (a Headset holds it), the pod names it, so revoking it ends
// the Session, and pending clears.
func TestNativeVRKeepsPINPairingPending(t *testing.T) {
	f, s, agent := vrLifecycle(t, seededWith(""))
	watchHeadsets(t, f)
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}}}, false)

	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if got := f.headsetNames(t); len(got) != 0 {
		t.Fatalf("headsets %v created without the User's confirmation", got)
	}
	live, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := live.Status.PendingHeadsets
	if len(p) != 1 || p[0].Key != testHeadsetKeyA || p[0].Name != "Quest 3" || p[0].FirstSeen.IsZero() {
		t.Fatalf("pendingHeadsets %+v", p)
	}
	first := p[0].FirstSeen
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if s.Status.PendingHeadsets[0].FirstSeen != first {
		t.Error("firstSeen moved on the next poll")
	}
	if got := f.livePod(t, s.Name).Annotations[vrHeadsetsAnnotation]; got != "" {
		t.Errorf("pod %s = %q before confirmation", vrHeadsetsAnnotation, got)
	}

	// Confirmed on the library page; the game added a key meanwhile. A
	// pairing adds one key, so that one is never offered.
	f.createHeadset(t, "confirmed", f.user, testHeadsetKeyA, "Quest 3")
	f.waitHeadsets(t, 1)
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}, {Name: "Quest 3", Key: testHeadsetKeyB}}}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if got := f.livePod(t, s.Name).Annotations[vrHeadsetsAnnotation]; got != "confirmed/confirmed-uid" {
		t.Errorf("pod %s = %q, want the confirmed Headset", vrHeadsetsAnnotation, got)
	}
	if len(s.Status.PendingHeadsets) != 0 {
		t.Errorf("still pending: %+v", s.Status.PendingHeadsets)
	}

	// Revoking it ends the Session.
	if err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Delete(context.Background(), "confirmed", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitHeadsets(t, 0)
	waitPodAnnotation(t, f, s.Name, "confirmed/confirmed-uid")
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("session outlived its revoked, confirmed headset")
	}
}

func waitPodAnnotation(t *testing.T, f *portsFixture, name, want string) {
	t.Helper()
	for range 500 {
		if p, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(name); err == nil && p.Annotations[vrHeadsetsAnnotation] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pod %s never cached %s=%q", name, vrHeadsetsAnnotation, want)
}

// Only a pod that paired by PIN lists keys; bad keys are skipped, the list is
// capped, and a key leaving WiVRn leaves the list. Nothing creates Headsets.
func TestTrackPairedHeadsets(t *testing.T) {
	many := make([]VRKnownKey, 0, maxPendingHeadsets+2)
	for i := range maxPendingHeadsets + 2 {
		many = append(many, VRKnownKey{Key: strings.Repeat(string(rune('A'+i)), 8)})
	}
	for _, tc := range []struct {
		name    string
		seeded  string
		annot   bool
		known   []VRKnownKey
		pending int
		errs    bool
	}{
		{"seeded with a headset", "quest/quest-uid", true, []VRKnownKey{{Key: testHeadsetKeyB}}, 0, false},
		{"not a VR pod", "", false, []VRKnownKey{{Key: testHeadsetKeyB}}, 0, false},
		{"two keys", "", true, []VRKnownKey{{Key: testHeadsetKeyA}, {Key: testHeadsetKeyB}, {Key: testHeadsetKeyA}}, 2, false},
		{"a PEM block", "", true, []VRKnownKey{{Key: "-----BEGIN PUBLIC KEY-----\n" + testHeadsetKeyA}}, 0, true},
		{"too long", "", true, []VRKnownKey{{Key: strings.Repeat("A", 1028)}}, 0, true},
		{"capped", "", true, many, maxPendingHeadsets, true},
		{"no keys", "", true, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPortsFixture(t)
			watchHeadsets(t, f)
			s := f.session("alex-1", f.user)
			s.Status.PendingHeadsets = []v1alpha1types.PendingHeadset{{Key: "R09ORQ==", FirstSeen: metav1.Now()}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: portsTestNS, Annotations: map[string]string{}}}
			if tc.annot {
				pod.Annotations[vrHeadsetsAnnotation] = tc.seeded
			}
			err := f.sc.trackPairedHeadsets(context.Background(), s, pod, tc.known)
			if (err != nil) != tc.errs {
				t.Errorf("err = %v, want error %v", err, tc.errs)
			}
			want := tc.pending
			if !tc.annot || tc.seeded != "" {
				want = 1 // untouched
			}
			if len(s.Status.PendingHeadsets) != want {
				t.Errorf("pending %+v, want %d", s.Status.PendingHeadsets, want)
			}
			if got := f.headsetNames(t); len(got) != 0 {
				t.Errorf("created headsets %v", got)
			}
		})
	}
}

// A pending key's name is cut to the CRD's 128 characters, not bytes.
func TestHeadsetDisplayNameCountsCharacters(t *testing.T) {
	if got := headsetDisplayName(strings.Repeat("é", 200)); len([]rune(got)) != headsetNameMaxLen {
		t.Errorf("%d characters, want %d", len([]rune(got)), headsetNameMaxLen)
	}
	if got := headsetDisplayName("ok\xffname"); got != "okname" {
		t.Errorf("%q", got)
	}
}

// The annotation patch carries the pod's resourceVersion, so a stale pod
// (another poll recorded first) fails it rather than overwrite the record.
// The fake API server ignores it; this checks it is sent.
func TestAnnotateHeadsetsNeedsCurrentPod(t *testing.T) {
	f := newPortsFixture(t)
	s := f.session("alex-1", f.user)
	pod := readyPod(s)
	pod.Annotations[vrHeadsetsAnnotation] = ""
	created, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	fake, ok := f.sc.K8sClient.(*k8sfake.Clientset)
	if !ok {
		t.Fatal("not a fake clientset")
	}
	fake.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if pa, ok := a.(k8stesting.PatchAction); ok {
			_ = json.Unmarshal(pa.GetPatch(), &sent)
		}
		return false, nil, nil
	})
	created.ResourceVersion = "42" // the fake API server sets none
	if err := f.sc.annotateHeadsets(context.Background(), created, "h/u"); err != nil {
		t.Fatal(err)
	}
	md, _ := sent["metadata"].(map[string]any)
	if md["resourceVersion"] != "42" {
		t.Errorf("patch %v carries no resourceVersion precondition (pod at %q)", sent, created.ResourceVersion)
	}
}

// vrTokenSecret is tokenSecret under the split App pod's vr-agent name.
func vrTokenSecret(s *v1alpha1types.Session) *corev1.Secret {
	sec := tokenSecret(s)
	sec.Name = vrAgentTokenSecretName(s.Name)
	return sec
}

// In a Moonlight Session the VR App runs in the split App pod; its vr-agent
// is polled, with its own token, while that pod paired by PIN, and the key
// left pending there.
func TestMoonlightVRKeepsPINPairingPending(t *testing.T) {
	agent := newFakeVRAgent(t, new(testAgentKeyPair(t)))
	sess, objs := vrSessionPods("")
	f, s := lifecycleFixture(t, sess, func(s *v1alpha1types.Session) []runtime.Object {
		res := objs(s)
		app, ok := res[1].(*corev1.Pod)
		if !ok {
			t.Fatal("vrSessionPods changed shape")
		}
		app.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: v1alpha1types.VRAgentContainerName}}
		return append(res, vrTokenSecret(s))
	})
	f.sc.AppPodHostDir = testAppPodHostDir
	f.sc.vrAgentPort = agent.port(t)
	watchHeadsets(t, f)
	waitPod(t, f, s.Name)
	waitPod(t, f, s.Name+"-app")
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}}}, false)

	wolf, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.sc.trackAppPodPairing(context.Background(), s, wolf)
	if got := f.headsetNames(t); len(got) != 0 {
		t.Fatalf("headsets %v created without confirmation", got)
	}
	if p := s.Status.PendingHeadsets; len(p) != 1 || p[0].Key != testHeadsetKeyA {
		t.Fatalf("pending %+v", p)
	}
	if got := agent.tokens; len(got) != 1 || got[0] != "Bearer token" {
		t.Errorf("agent saw %v", got)
	}

	// Confirmed: the pod names it, so it no longer paired by PIN and is not
	// polled again.
	f.createHeadset(t, "confirmed", f.user, testHeadsetKeyA, "")
	f.waitHeadsets(t, 1)
	f.sc.trackAppPodPairing(context.Background(), s, wolf)
	waitPodAnnotation(t, f, s.Name+"-app", "confirmed/confirmed-uid")
	f.sc.trackAppPodPairing(context.Background(), s, wolf)
	if got := agent.tokens; len(got) != 2 {
		t.Errorf("polled %d times, want 2 (none after confirmation)", len(got))
	}
}

// With no vr-agent token Secret of its own the App pod is not polled with
// wolf-agent's: that token drives Wolf.
func TestMoonlightVRNeverSendsWolfAgentToken(t *testing.T) {
	agent := newFakeVRAgent(t, new(testAgentKeyPair(t)))
	sess, objs := vrSessionPods("")
	f, s := lifecycleFixture(t, sess, func(s *v1alpha1types.Session) []runtime.Object {
		res := objs(s)
		app, ok := res[1].(*corev1.Pod)
		if !ok {
			t.Fatal("vrSessionPods changed shape")
		}
		app.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: v1alpha1types.VRAgentContainerName}}
		return append(res, tokenSecret(s))
	})
	f.sc.vrAgentPort = agent.port(t)
	watchHeadsets(t, f)
	waitPod(t, f, s.Name+"-app")
	wolf := readyPod(s)
	wolf.Annotations[appPodAnnotation] = s.Name + "-app"
	f.sc.trackAppPodPairing(context.Background(), s, wolf)
	if len(agent.tokens) != 0 {
		t.Errorf("polled with %v", agent.tokens)
	}
}

// An App pod made before vr-agent was added to it is never polled.
func TestMoonlightVRWithoutAgentIsNotPolled(t *testing.T) {
	agent := newFakeVRAgent(t, new(testAgentKeyPair(t)))
	sess, objs := vrSessionPods("")
	f, s := lifecycleFixture(t, sess, func(s *v1alpha1types.Session) []runtime.Object { return append(objs(s), tokenSecret(s)) })
	f.sc.vrAgentPort = agent.port(t)
	watchHeadsets(t, f)
	waitPod(t, f, s.Name+"-app")
	wolf := readyPod(s)
	wolf.Annotations[appPodAnnotation] = s.Name + "-app"
	f.sc.trackAppPodPairing(context.Background(), s, wolf)
	if len(agent.tokens) != 0 {
		t.Error("polled an App pod with no vr-agent")
	}
}

// A VR App's split App pod gets vr-agent (with its own token Secret, never
// wolf-agent's, which no App container mounts) and its bus directory; it never gates the pod's
// readiness, which the stream waits on before the App can start WiVRn. A
// flat App's pod gets neither.
func TestSplitVRAppPodRunsVRAgent(t *testing.T) {
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
	if slices.Contains(containerNames(&wolf.Spec), v1alpha1types.VRAgentContainerName) {
		t.Error("Wolf pod runs vr-agent")
	}
	i := slices.IndexFunc(app.Spec.Containers, func(c corev1.Container) bool { return c.Name == v1alpha1types.VRAgentContainerName })
	if i < 0 {
		t.Fatal("App pod has no vr-agent")
	}
	agent := app.Spec.Containers[i]
	if agent.ReadinessProbe != nil {
		t.Error("vr-agent gates the split App pod's readiness")
	}
	if v := volumeNamed(&app.Spec, vrAgentOwnTokenVolume); v == nil || v.Secret == nil || v.Secret.SecretName != vrAgentTokenSecretName(sess.Name) {
		t.Errorf("token volume %+v", v)
	}
	if volumeNamed(&app.Spec, vrAgentTokenVolume) != nil {
		t.Error("App pod holds the wolf-agent token")
	}
	// No App template may mount them (validateNoOperatorVolumeMounts).
	for _, v := range []string{vrAgentOwnTokenVolume, vrBusVolume} {
		if !slices.Contains(operatorVolumes, v) {
			t.Errorf("%s is not in operatorVolumes", v)
		}
	}
	if v := volumeNamed(&app.Spec, vrBusVolume); v == nil || v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("bus volume %+v", v)
	}
	for _, c := range app.Spec.Containers {
		token := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == vrAgentOwnTokenVolume })
		bus := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == vrBusVolume && m.MountPath == VRBusDir })
		switch c.Name {
		case v1alpha1types.VRAgentContainerName:
			if !token || !bus {
				t.Errorf("vr-agent mounts token %v bus %v", token, bus)
			}
		case v1alpha1types.UdevRelayContainerName:
			if token || bus {
				t.Errorf("udev-relay mounts token %v bus %v", token, bus)
			}
		default:
			if token || !bus {
				t.Errorf("App container %s mounts token %v bus %v; want the bus only", c.Name, token, bus)
			}
		}
	}

	flatApp := withVR(t, f) // the informer's copy: the controller reads it back
	flatApp.Spec.VRPort = false
	flat := f.session("alex-2", f.user)
	_, flatPod, err := f.sc.buildPods(flat, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(containerNames(&flatPod.Spec), v1alpha1types.VRAgentContainerName) || volumeNamed(&flatPod.Spec, vrBusVolume) != nil {
		t.Error("flat App pod runs vr-agent")
	}
	// Native mode keeps its readiness gate.
	if c := f.sc.vrAgentContainer(true, vrAgentTokenVolume); c.ReadinessProbe == nil {
		t.Error("native vr-agent lost its readiness probe")
	}
}

// The split App pod's vr-agent token Secret is created, owned by the
// Session, before the App pod that mounts it.
func TestReconcileAppPodCreatesVRAgentToken(t *testing.T) {
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := podReadySession(f)
		s.Status.Ports.VR = 21000
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object {
		p := readyPod(s)
		p.Annotations[appPodAnnotation] = s.Name + "-app"
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue})
		return []runtime.Object{p}
	})
	withVR(t, f)
	waitPod(t, f, sess.Name)
	if _, err := f.sc.reconcilePod(context.Background(), sess); err != nil {
		t.Fatalf("reconcilePod: %v", err)
	}
	if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), sess.Name+"-app", metav1.GetOptions{}); err != nil {
		t.Fatalf("App pod not created: %v", err)
	}
	sec, err := f.sc.K8sClient.CoreV1().Secrets(portsTestNS).Get(context.Background(), vrAgentTokenSecretName(sess.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no vr-agent token Secret: %v", err)
	}
	if owner := metav1.GetControllerOf(sec); owner == nil || owner.UID != sess.UID || len(sec.Data[wolfAgentTokenKey]) == 0 || len(sec.Data[corev1.TLSCertKey]) == 0 {
		t.Errorf("Secret %+v", sec)
	}
}
