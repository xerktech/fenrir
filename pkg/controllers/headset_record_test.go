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

// A pending key's name is cut to the CRD's 128 characters, not bytes.
func TestHeadsetDisplayNameCountsCharacters(t *testing.T) {
	if got := CleanHeadsetName(strings.Repeat("é", 200)); len([]rune(got)) != headsetNameMaxLen {
		t.Errorf("%d characters, want %d", len([]rune(got)), headsetNameMaxLen)
	}
	if got := CleanHeadsetName("ok\xffname"); got != "okname" {
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

// A Headset the User confirms and revokes before the next poll names it on
// the pod still ends the Session: WiVRn trusts the key there (QA,
// XERK-2747). Its name is the pending key's recordedHeadsetName.
func TestRevokeBeforePollEndsSession(t *testing.T) {
	f, s, agent := vrLifecycle(t, seededWith(""))
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
