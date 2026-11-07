package controllers

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

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

// A native VR pod seeded with no Headsets pairs by PIN; the key WiVRn then
// reports becomes the User's Headset and the pod's seeded record, so the
// next session needs no PIN and revoking it ends this one (XERK-2740).
func TestNativeVRRecordsPINPairing(t *testing.T) {
	f, s, agent := vrLifecycle(t, seededWith(""))
	watchHeadsets(t, f)
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}}}, false)

	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	name := recordedHeadsetName(f.user, testHeadsetKeyA)
	h, err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no Headset recorded: %v", err)
	}
	if h.Spec.UserReference.Name != f.user || h.Spec.PublicKey != testHeadsetKeyA || h.Spec.DisplayName != "Quest 3" {
		t.Errorf("recorded %+v", h.Spec)
	}
	if got := f.livePod(t, s.Name).Annotations[vrHeadsetsAnnotation]; got != name+"/"+string(h.UID) {
		t.Errorf("pod %s = %q, want the recorded Headset", vrHeadsetsAnnotation, got)
	}

	// Revoking it ends the Session; it is not recorded again.
	if err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitHeadsets(t, 0)
	waitPodAnnotation(t, f, s.Name, name+"/"+string(h.UID))
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("session outlived its revoked, recorded headset")
	}
	if got := f.headsetNames(t); len(got) != 0 {
		t.Errorf("headsets %v re-recorded after revocation", got)
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

// Only a pod that paired by PIN records, and only the one key a pairing
// adds: anything else in WiVRn's list did not come from the PIN.
func TestRecordPairedHeadsetsRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seeded string
		annot  bool
		known  []VRKnownKey
		errs   bool
	}{
		{"seeded with a headset", "quest/quest-uid", true, []VRKnownKey{{Key: testHeadsetKeyA}, {Key: testHeadsetKeyB}}, false},
		{"not a VR pod", "", false, []VRKnownKey{{Key: testHeadsetKeyB}}, false},
		{"two keys", "", true, []VRKnownKey{{Key: testHeadsetKeyA}, {Key: testHeadsetKeyB}}, true},
		{"a PEM block", "", true, []VRKnownKey{{Key: "-----BEGIN PUBLIC KEY-----\n" + testHeadsetKeyA}}, true},
		{"too long", "", true, []VRKnownKey{{Key: strings.Repeat("A", 1028)}}, true},
		{"no keys", "", true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPortsFixture(t)
			watchHeadsets(t, f)
			s := f.session("alex-1", f.user)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: portsTestNS, Annotations: map[string]string{}}}
			if tc.annot {
				pod.Annotations[vrHeadsetsAnnotation] = tc.seeded
			}
			err := f.sc.recordPairedHeadsets(context.Background(), s, pod, tc.known)
			if (err != nil) != tc.errs {
				t.Errorf("err = %v, want error %v", err, tc.errs)
			}
			if got := f.headsetNames(t); len(got) != 0 {
				t.Errorf("recorded %v", got)
			}
		})
	}
}

// A key the User already has a Headset for (the cache lagged when the pod
// was seeded) is recorded as that Headset, not a second one. One recorded by
// an earlier poll whose pod patch failed is reused; one of that name with
// another key is refused.
func TestRecordPairedHeadsetsReuses(t *testing.T) {
	f := newPortsFixture(t)
	watchHeadsets(t, f)
	f.createHeadset(t, "mine", f.user, testHeadsetKeyA, "")
	f.waitHeadsets(t, 1)
	s := f.session("alex-1", f.user)
	pod := readyPod(s)
	pod.Annotations[vrHeadsetsAnnotation] = ""
	if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	if got := f.livePod(t, pod.Name).Annotations[vrHeadsetsAnnotation]; got != "mine/mine-uid" {
		t.Errorf("annotation %q, want the existing Headset", got)
	}
	if got := f.headsetNames(t); !slices.Equal(got, []string{"mine"}) {
		t.Errorf("headsets %v", got)
	}

	// Already made under its name by an earlier poll: reused.
	name := recordedHeadsetName(f.user, testHeadsetKeyB)
	f.createHeadset(t, name, f.user, testHeadsetKeyB, "")
	pod = f.livePod(t, pod.Name)
	pod.Annotations[vrHeadsetsAnnotation] = ""
	pod, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Update(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Key: testHeadsetKeyB}}); err != nil {
		t.Fatal(err)
	}
	if got := f.livePod(t, pod.Name).Annotations[vrHeadsetsAnnotation]; got != name+"/"+name+"-uid" {
		t.Errorf("annotation %q, want %s", got, name)
	}

	// Another user's object under the name a key hashes to is never taken.
	other := f.session("sam-1", "sam")
	clash := recordedHeadsetName("sam", testHeadsetKeyA)
	f.createHeadset(t, clash, "someone", testHeadsetKeyA, "")
	pod = f.livePod(t, pod.Name)
	pod.Annotations[vrHeadsetsAnnotation] = ""
	if err := f.sc.recordPairedHeadsets(context.Background(), other, pod, []VRKnownKey{{Key: testHeadsetKeyA}}); err == nil {
		t.Error("took another user's Headset")
	}
}

// In a Moonlight Session the VR App runs in the split App pod; its vr-agent
// is polled while that pod paired by PIN, and the pairing recorded there.
func TestMoonlightVRRecordsPINPairing(t *testing.T) {
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
	f.sc.recordAppPodPairing(context.Background(), s, wolf)
	name := recordedHeadsetName(f.user, testHeadsetKeyA)
	if got := f.headsetNames(t); !slices.Equal(got, []string{name}) {
		t.Fatalf("headsets %v, want %s", got, name)
	}
	if got := f.livePod(t, s.Name+"-app").Annotations[vrHeadsetsAnnotation]; got != name+"/" {
		t.Errorf("App pod %s = %q", vrHeadsetsAnnotation, got)
	}
	if got := agent.tokens; len(got) != 1 || got[0] != "Bearer token" {
		t.Errorf("agent saw %v", got)
	}

	// Recorded: the pod no longer paired by PIN, so it is not polled again.
	waitPodAnnotation(t, f, s.Name+"-app", name+"/")
	f.sc.recordAppPodPairing(context.Background(), s, wolf)
	if got := agent.tokens; len(got) != 1 {
		t.Errorf("polled again after recording: %d", len(got))
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
	f.sc.recordAppPodPairing(context.Background(), s, wolf)
	if len(agent.tokens) != 0 {
		t.Error("polled an App pod with no vr-agent")
	}
}

// A VR App's split App pod gets vr-agent (with the Session's token, which no
// App container mounts) and its bus directory; it never gates the pod's
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
	if v := volumeNamed(&app.Spec, vrAgentTokenVolume); v == nil || v.Secret == nil || v.Secret.SecretName != agentTokenSecretName(sess.Name) {
		t.Errorf("token volume %+v", v)
	}
	if v := volumeNamed(&app.Spec, vrBusVolume); v == nil || v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("bus volume %+v", v)
	}
	for _, c := range app.Spec.Containers {
		token := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == vrAgentTokenVolume })
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
	if c := f.sc.vrAgentContainer(true); c.ReadinessProbe == nil {
		t.Error("native vr-agent lost its readiness probe")
	}
}
