package controllers

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// seededWithNone is a vrLifecycle pod mutation: a VR App pod seeded with no
// headsets, so it pairs by PIN.
func seededWithNone(p *corev1.Pod) *corev1.Pod {
	p.Annotations[vrHeadsetsAnnotation] = ""
	return p
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
	f, s, agent := vrLifecycle(t, seededWithNone)
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
			// Two, so "untouched" differs from tracking any one key (QA).
			s.Status.PendingHeadsets = []v1alpha1types.PendingHeadset{{Key: "R09ORQ==", FirstSeen: metav1.Now()}, {Key: "VFdP", FirstSeen: metav1.Now()}}
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
				want = 2 // untouched
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

// The operator cleans a reported name itself: an older vr-agent sends it raw,
// and one over the CRD's 128 characters would fail the status write.
func TestTrackPairedHeadsetsCleansNames(t *testing.T) {
	f := newPortsFixture(t)
	watchHeadsets(t, f)
	s := f.session("alex-1", f.user)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: portsTestNS, Annotations: map[string]string{vrHeadsetsAnnotation: ""}}}
	raw := "Quest\u202e\n" + strings.Repeat("é", 100)
	if err := f.sc.trackPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: raw, Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	if len(s.Status.PendingHeadsets) != 1 || s.Status.PendingHeadsets[0].Name != CleanHeadsetName(raw) {
		t.Errorf("pending %+v, want the name cleaned", s.Status.PendingHeadsets)
	}
}

// A pending key's name is cleaned as vr-agent cleans it: no control or
// bidi runes, cut to the CRD's 128 characters.
func TestCleanHeadsetName(t *testing.T) {
	got := CleanHeadsetName("Quest\u202e\u2028\n3\x00\xff" + strings.Repeat("é", 200))
	// Cut to the CRD's 128 characters, not bytes (XERK-2747).
	if !strings.HasPrefix(got, "Quest3é") || utf8.RuneCountInString(got) != 128 || !utf8.ValidString(got) {
		t.Errorf("CleanHeadsetName = %q (%d characters)", got, utf8.RuneCountInString(got))
	}
}

func TestIsWiVRnKey(t *testing.T) {
	for key, want := range map[string]bool{
		testHeadsetKeyA: true, "MEIw": true, "MEI=": true, "": false, "AAA": false, "AAAAA": false,
		strings.Repeat("A", 1024): true, strings.Repeat("A", 1028): false, "AA=A": false,
	} {
		if got := IsWiVRnKey(key); got != want {
			t.Errorf("IsWiVRnKey(%.20q) = %v, want %v", key, got, want)
		}
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
	if c := f.sc.vrAgentContainer(true, vrAgentTokenVolume, vrAgentUID, vrAgentUID); c.ReadinessProbe == nil {
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

// A Headset the User confirms and revokes before the next poll names it on
// the pod still ends the Session: WiVRn trusts the key there (QA,
// XERK-2747). Its name is the pending key's recordedHeadsetName.
func TestRevokeBeforePollEndsSession(t *testing.T) {
	f, s, agent := vrLifecycle(t, seededWithNone)
	watchHeadsets(t, f)
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}}}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if c, err := f.sc.SessionInformer.Namespaced(portsTestNS).Get(s.Name); err == nil && len(c.Status.PendingHeadsets) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	name := recordedHeadsetName(f.user, testHeadsetKeyA)

	// A Headset event while it exists (the confirm itself) ends nothing.
	f.createHeadset(t, name, f.user, testHeadsetKeyA, "Quest 3")
	f.waitHeadsets(t, 1)
	if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
		t.Fatal(err)
	}
	if !f.sessionExists(t, s.Name) {
		t.Fatal("confirming ended the Session")
	}
	// Nor does an event of another Headset of the User.
	if err := f.sc.reconcileHeadset(portsTestNS, "unrelated", nil); err != nil || !f.sessionExists(t, s.Name) {
		t.Fatalf("an unrelated Headset's event: %v, session exists %v", err, f.sessionExists(t, s.Name))
	}

	if err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("session outlived its confirmed headset revoked before the pod recorded it")
	}
}

// A key of a Headset the User already had (the game can read it from the
// seed env) is recorded on the pod, but does not drop the Session's real
// pairing from the list (QA, XERK-2747).
func TestOldHeadsetKeyKeepsPairingPending(t *testing.T) {
	f := newPortsFixture(t)
	watchHeadsets(t, f)
	f.createHeadset(t, "older", f.user, testHeadsetKeyB, "")
	f.waitHeadsets(t, 1)
	s := f.session("alex-1", f.user)
	pod := readyPod(s)
	pod.Annotations[vrHeadsetsAnnotation] = ""
	if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	known := []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}, {Name: "game", Key: testHeadsetKeyB}}
	if err := f.sc.trackPairedHeadsets(context.Background(), s, pod, known); err != nil {
		t.Fatal(err)
	}
	if p := s.Status.PendingHeadsets; len(p) != 1 || p[0].Key != testHeadsetKeyA {
		t.Errorf("pending %+v, want the PIN-paired key kept", p)
	}
	if got := f.livePod(t, pod.Name).Annotations[vrHeadsetsAnnotation]; got != "older/older-uid" {
		t.Errorf("pod %s = %q, want the older Headset WiVRn admits", vrHeadsetsAnnotation, got)
	}
}

// The revoke-before-poll path: a terminating Headset counts as revoked; a
// failed GET is retried, never read as gone; a Moonlight Session (its split
// App pod pairs by PIN too, XERK-2740) ends as well (QA, XERK-2747).
func TestRevokeBeforePollEdgeCases(t *testing.T) {
	setup := func(t *testing.T) (*portsFixture, *v1alpha1types.Session, string) {
		t.Helper()
		f, s, agent := vrLifecycle(t, seededWithNone)
		watchHeadsets(t, f)
		agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Key: testHeadsetKeyA}}}, false)
		if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
			t.Fatal(err)
		}
		for range 500 {
			if c, err := f.sc.SessionInformer.Namespaced(portsTestNS).Get(s.Name); err == nil && len(c.Status.PendingHeadsets) == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return f, s, recordedHeadsetName(f.user, testHeadsetKeyA)
	}

	t.Run("terminating", func(t *testing.T) {
		f, s, name := setup(t)
		f.createHeadsetObj(t, name, f.user, testHeadsetKeyA, "", func(h *v1alpha1types.Headset) {
			h.Finalizers = []string{"example.com/hold"}
			h.DeletionTimestamp = new(metav1.Now())
		})
		if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
			t.Fatal(err)
		}
		if f.sessionExists(t, s.Name) {
			t.Error("session outlived its terminating headset")
		}
	})

	// A Headset re-created under the name for another key (or user) is no
	// confirm of this Session's key: WiVRn still trusts the old one there
	// (XERK-2709).
	t.Run("re-created for another key", func(t *testing.T) {
		f, s, name := setup(t)
		f.createHeadsetObj(t, name, f.user, testHeadsetKeyB, "", func(*v1alpha1types.Headset) {})
		if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
			t.Fatal(err)
		}
		if f.sessionExists(t, s.Name) {
			t.Error("session outlived its headset re-created for another key")
		}
	})

	t.Run("re-created for another user", func(t *testing.T) {
		f, s, name := setup(t)
		f.createHeadsetObj(t, name, "someone-else", testHeadsetKeyA, "", func(*v1alpha1types.Headset) {})
		if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
			t.Fatal(err)
		}
		if f.sessionExists(t, s.Name) {
			t.Error("session outlived its headset re-created for another user")
		}
	})

	t.Run("still confirmed", func(t *testing.T) {
		f, s, name := setup(t)
		f.createHeadsetObj(t, name, f.user, testHeadsetKeyA, "", func(*v1alpha1types.Headset) {})
		if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
			t.Fatal(err)
		}
		if !f.sessionExists(t, s.Name) {
			t.Error("a confirmed headset's event ended its session")
		}
	})

	t.Run("GET fails", func(t *testing.T) {
		f, s, name := setup(t)
		f.dw.PrependReactor("get", "headsets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, stderrors.New("apiserver unavailable")
		})
		if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err == nil {
			t.Error("no error to retry on a failed GET")
		}
		if !f.sessionExists(t, s.Name) {
			t.Error("a failed GET ended the session")
		}
	})

	t.Run("Moonlight session", func(t *testing.T) {
		f, s, name := setup(t)
		live, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Get(context.Background(), s.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		live.Spec.Mode = v1alpha1types.SessionModeMoonlight
		if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Update(context.Background(), live, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		for range 500 {
			if c, err := f.sc.SessionInformer.Namespaced(portsTestNS).Get(s.Name); err == nil && !c.Spec.IsVR() {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err != nil {
			t.Fatal(err)
		}
		// Its split App pod pairs by PIN too (XERK-2740): the revoke ends it.
		if f.sessionExists(t, s.Name) {
			t.Error("a Moonlight Session outlived its revoked, confirmed headset")
		}
	})
}

// Every VR Session holding the revoked key pending ends, and a failed end of
// one is returned for retry without sparing the others (QA, XERK-2747).
func TestRevokeBeforePollEndsEverySession(t *testing.T) {
	f, s, agent := vrLifecycle(t, seededWithNone)
	watchHeadsets(t, f)
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Key: testHeadsetKeyA}}}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	other := s.DeepCopy()
	other.Name, other.UID, other.ResourceVersion = s.Name+"-2", "", ""
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		a, errA := f.sc.SessionInformer.Namespaced(portsTestNS).Get(s.Name)
		b, errB := f.sc.SessionInformer.Namespaced(portsTestNS).Get(other.Name)
		if errA == nil && errB == nil && len(a.Status.PendingHeadsets) == 1 && len(b.Status.PendingHeadsets) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Whichever Session is ended first fails once.
	failed := ""
	f.dw.PrependReactor("delete", "sessions", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if failed == "" {
			failed = a.(k8stesting.DeleteAction).GetName() //nolint:forcetypeassert // a delete action
			return true, nil, stderrors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	name := recordedHeadsetName(f.user, testHeadsetKeyA)
	if err := f.sc.reconcileHeadset(portsTestNS, name, nil); err == nil {
		t.Error("a failed end returned no error to retry")
	}
	if failed == "" {
		t.Fatal("no Session was ended")
	}
	for _, n := range []string{s.Name, other.Name} {
		if want := n == failed; f.sessionExists(t, n) != want {
			t.Errorf("session %s exists %v, want %v (failed delete: %s)", n, !want, want, failed)
		}
	}
}

// In a split App pod vr-agent runs as the App's user too (XERK-2709). One it
// can't take keeps the Moonlight stream: vr-agent there only offers pairings
// for confirmation, and nothing waits on it, so it falls back to 1000.
func TestSplitVRAppPodVRAgentTakesAppsUser(t *testing.T) {
	for _, tc := range []struct {
		puid     string
		uid, gid int64
	}{{"1001", 1001, 1002}, {"0", vrAgentUID, vrAgentUID}} {
		f := newPortsFixture(t)
		app := withVR(t, f)
		ctr := &app.Spec.Template.Spec.Containers[0]
		ctr.Env = append(ctr.Env, corev1.EnvVar{Name: "PUID", Value: tc.puid}, corev1.EnvVar{Name: "PGID", Value: "1002"})
		sess := f.session("alex-1", f.user)
		if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
		_, pod, err := f.sc.buildPods(sess, testWolfPodUID)
		if err != nil {
			t.Fatalf("PUID %s: %v", tc.puid, err)
		}
		i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == v1alpha1types.VRAgentContainerName })
		if i < 0 {
			t.Fatalf("PUID %s: App pod has no vr-agent", tc.puid)
		}
		if sc := pod.Spec.Containers[i].SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != tc.uid ||
			sc.RunAsGroup == nil || *sc.RunAsGroup != tc.gid {
			t.Errorf("PUID %s: vr-agent securityContext %+v, want %d:%d", tc.puid, sc, tc.uid, tc.gid)
		}
	}
}
