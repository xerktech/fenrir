package controllers

import (
	"context"
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

// seededPod gives a VR pod the seed env and annotation buildPod would.
func seededPod(keys, annotation string) func(*corev1.Pod) *corev1.Pod {
	return func(p *corev1.Pod) *corev1.Pod {
		p.Labels = map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue}
		p.Spec.Containers = []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: vrHeadsetsEnv, Value: keys}}}}
		p.Annotations[vrHeadsetsAnnotation] = annotation
		return p
	}
}

func (f *portsFixture) headsets(t *testing.T) []v1alpha1types.Headset {
	t.Helper()
	l, err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// A headset paired by PIN in a native VR session becomes a Headset of the
// Session's User, so the next session admits it; the pod records it, so
// revoking it ends this session too.
func TestPairedHeadsetIsRecorded(t *testing.T) {
	f, s, agent := vrLifecycle(t, seededPod("[]", ""))
	watchHeadsets(t, f)
	agent.set(VRStatus{HeadsetConnected: true, KnownKeys: []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}}}, false)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	hs := f.headsets(t)
	if len(hs) != 1 {
		t.Fatalf("headsets = %+v, want one", hs)
	}
	h := hs[0]
	if h.Name != headsetObjectName(f.user, testHeadsetKeyA) || h.Spec.UserReference.Name != f.user ||
		h.Spec.PublicKey != testHeadsetKeyA || h.Spec.DisplayName != "Quest 3" {
		t.Errorf("headset = %+v", h)
	}
	if len(h.OwnerReferences) != 1 || h.OwnerReferences[0].Kind != "User" || h.OwnerReferences[0].Name != f.user {
		t.Errorf("headset owners = %+v, want its User", h.OwnerReferences)
	}
	pod, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pod.Annotations[vrHeadsetsAnnotation], h.Name+"/"+string(h.UID); got != want {
		t.Errorf("pod %s = %q, want %q", vrHeadsetsAnnotation, got, want)
	}

	// Reported again: nothing new.
	f.waitHeadsets(t, 1)
	if err := f.sc.Reconcile(portsTestNS, s.Name, s); err != nil {
		t.Fatal(err)
	}
	if hs := f.headsets(t); len(hs) != 1 {
		t.Errorf("headsets after a second report = %d, want 1", len(hs))
	}

	// Revoking it ends the session it was paired in.
	if err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Delete(context.Background(), h.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitHeadsets(t, 0)
	if err := f.sc.reconcileHeadset(portsTestNS, h.Name, nil); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("session outlived the revoke of the headset paired in it")
	}
}

func recordFixture(t *testing.T, seeded string) (*portsFixture, *v1alpha1types.Session, *corev1.Pod) {
	t.Helper()
	f, s, _ := vrLifecycle(t, seededPod(seeded, ""))
	watchHeadsets(t, f)
	pod, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return f, s, pod
}

// A key the pod was seeded with whose Headset is gone was revoked after the
// pod started: recording it would undo the revoke.
func TestRevokedSeededKeyIsNotRecorded(t *testing.T) {
	f, s, pod := recordFixture(t, `[{"key":"`+testHeadsetKeyA+`","name":"Quest 3"}]`)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "Quest 3", Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	if hs := f.headsets(t); len(hs) != 0 {
		t.Errorf("recorded a revoked seeded key: %+v", hs)
	}
}

// Keys a Headset of the User already holds, and keys not in WiVRn's stored
// form, are not recorded; the User's count is capped.
func TestRecordPairedHeadsetsSkipsAndCaps(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	f.createHeadset(t, "mine", f.user, testHeadsetKeyA, "")
	f.waitHeadsets(t, 1)
	known := make([]VRKnownKey, 0, 4+maxUserHeadsets+2)
	known = append(known,
		VRKnownKey{Name: "dup", Key: testHeadsetKeyA},
		VRKnownKey{Name: "pem", Key: "-----BEGIN PUBLIC KEY-----\n" + testHeadsetKeyB + "\n-----END PUBLIC KEY-----\n"},
		VRKnownKey{Name: "short", Key: "AAA"},
		VRKnownKey{Name: "long", Key: strings.Repeat("A", 1028)},
	)
	for i := range maxUserHeadsets + 2 {
		known = append(known, VRKnownKey{Name: strconv.Itoa(i), Key: fmt.Sprintf("KEY%05d", i)})
	}
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err != nil {
		t.Fatal(err)
	}
	hs := f.headsets(t)
	if len(hs) != maxUserHeadsets {
		t.Fatalf("headsets = %d, want the cap %d", len(hs), maxUserHeadsets)
	}
	for _, h := range hs {
		if h.Name != "mine" && (h.Spec.PublicKey == testHeadsetKeyA || !wivrnKeyPattern.MatchString(h.Spec.PublicKey)) {
			t.Errorf("recorded %+v", h.Spec)
		}
	}
	live, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), s.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The new ones, and "mine": WiVRn admits it, so revoking it must end
	// this session too.
	if n := len(podHeadsets(live)); n != maxUserHeadsets {
		t.Errorf("pod records %d headsets, want all %d WiVRn admits", n, maxUserHeadsets)
	}
}

// A Headset name taken by another User's (or another key's) Headset is left
// alone: the key is not recorded, and nothing fails.
func TestRecordPairedHeadsetsLeavesTakenName(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	f.createHeadset(t, headsetObjectName(f.user, testHeadsetKeyB), "someone-else", testHeadsetKeyB, "")
	f.waitHeadsets(t, 1)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "q", Key: testHeadsetKeyB}}); err != nil {
		t.Fatal(err)
	}
	hs, err := f.sc.HeadsetInformer.List(labels.Everything())
	if err != nil || len(hs) != 1 || hs[0].Spec.UserReference.Name != "someone-else" {
		t.Errorf("headsets = %+v, %v; want only the other User's", hs, err)
	}
	live, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v := live.Annotations[vrHeadsetsAnnotation]; v != "" {
		t.Errorf("pod annotation = %q; want the other User's Headset not adopted", v)
	}
}

func TestHeadsetObjectNameIsAValidName(t *testing.T) {
	for _, user := range []string{"alex", strings.Repeat("a", 253), "first.last"} {
		n := headsetObjectName(user, testHeadsetKeyA)
		if len(n) > 253 || strings.HasPrefix(n, "-") || n != headsetObjectName(user, testHeadsetKeyA) || n == headsetObjectName(user, testHeadsetKeyB) {
			t.Errorf("headsetObjectName(%.10q...) = %q", user, n)
		}
	}
}

// A Headset recorded this session and then revoked (WiVRn still lists its
// key, as when the revoke lands during a poll) is not recorded again: that
// would undo the revoke for the next session (QA, XERK-2669).
func TestRevokedRecordedKeyIsNotReRecorded(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	name := headsetObjectName(f.user, testHeadsetKeyA)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, vrHeadsetsAnnotation, name+"/old-uid")
	if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Patch(context.Background(), pod.Name, "application/merge-patch+json", []byte(patch), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "q", Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	if hs := f.headsets(t); len(hs) != 0 {
		t.Errorf("re-recorded a revoked headset: %+v", hs)
	}
}

// A failed annotation patch (a conflict with kubelet's status writes) or a
// failed create is retried by the next poll, so a recorded Headset always
// reaches the pod's annotation and revoking it ends the session.
func TestRecordedHeadsetReachesPodAfterFailures(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	k8s, ok := f.sc.K8sClient.(*k8sfake.Clientset)
	if !ok {
		t.Fatal("not a fake clientset")
	}
	fails := 1
	k8s.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fails > 0 {
			fails--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, stderrors.New("modified"))
		}
		return false, nil, nil
	})
	creates := 0
	f.dw.PrependReactor("create", "headsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		creates++
		if creates == 2 { // key B's first create
			return true, nil, apierrors.NewServiceUnavailable("try again")
		}
		return false, nil, nil
	})
	known := []VRKnownKey{{Name: "a", Key: testHeadsetKeyA}, {Name: "b", Key: testHeadsetKeyB}}
	// Poll 1: the placeholder's patch conflicts, so nothing is created.
	// Poll 2: A is recorded, B's create fails (its placeholder is dropped).
	// Poll 3: B is recorded.
	for poll := 1; poll <= 3; poll++ {
		f.sc.headsetBackoff.until = nil // B's failed create backs off
		err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known)
		if (err != nil) != (poll < 3) {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if poll == 1 {
			if hs := f.headsets(t); len(hs) != 0 {
				t.Fatalf("poll 1 created %d headsets with no placeholder on the pod", len(hs))
			}
		}
	}
	f.waitHeadsets(t, 2)
	live, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{testHeadsetKeyA, testHeadsetKeyB} {
		if !isSeeded(live, headsetObjectName(f.user, key)) {
			t.Errorf("pod annotation %q lacks %s", live.Annotations[vrHeadsetsAnnotation], headsetObjectName(f.user, key))
		}
	}
}

// The headset names itself: control and bidi characters are dropped, and
// the name is cut to the CRD's 128 bytes on a rune boundary.
func TestRecordedHeadsetDisplayName(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	name := "Quest\u202e\u2028\n3\x00" + strings.Repeat("é", 100)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: name, Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	hs := f.headsets(t)
	if len(hs) != 1 {
		t.Fatalf("headsets = %d", len(hs))
	}
	got := hs[0].Spec.DisplayName
	if !strings.HasPrefix(got, "Quest3é") || len(got) > 128 || strings.ContainsAny(got, "\u202e\u2028\n\x00") || !utf8.ValidString(got) {
		t.Errorf("displayName = %q (%d bytes)", got, len(got))
	}
}

// Nothing is recorded while a Headset the pod records is revoked, whatever
// its name: WiVRn still lists the revoked key (an admin-named Headset
// revoked during the poll), and any new Headset of it would undo the revoke.
func TestNothingRecordedWhileARecordedHeadsetIsRevoked(t *testing.T) {
	for _, entry := range []string{"mine/mine-uid", "mine/old-uid"} {
		f, s, pod := recordFixture(t, "[]")
		patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, vrHeadsetsAnnotation, entry)
		if _, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Patch(context.Background(), pod.Name, "application/merge-patch+json", []byte(patch), metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}
		if entry == "mine/old-uid" { // re-created under its name
			f.createHeadset(t, "mine", f.user, testHeadsetKeyB, "")
			f.waitHeadsets(t, 1)
		}
		known := []VRKnownKey{{Name: "revoked", Key: testHeadsetKeyA}}
		if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err != nil {
			t.Fatal(err)
		}
		for _, h := range f.headsets(t) {
			if h.Spec.PublicKey == testHeadsetKeyA {
				t.Errorf("%s: re-recorded the revoked key as %s", entry, h.Name)
			}
		}
	}
}

// A terminating Headset under the operator's name is not adopted.
func TestRecordPairedHeadsetsLeavesTerminatingName(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	f.createHeadsetObj(t, headsetObjectName(f.user, testHeadsetKeyA), f.user, testHeadsetKeyA, "", func(h *v1alpha1types.Headset) {
		h.DeletionTimestamp, h.Finalizers = &metav1.Time{Time: time.Now()}, []string{"test/hold"}
	})
	f.waitHeadsets(t, 1)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "q", Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	live, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v := live.Annotations[vrHeadsetsAnnotation]; v != "" {
		t.Errorf("pod annotation = %q; want the terminating Headset not adopted", v)
	}
}

// The owner User is read live when the cache lacks it; without it the
// Headset is not created (it would outlive the User).
func TestRecordedHeadsetOwnerReadLive(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	f.sc.UserInformer = emptyUserInformer(t)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "q", Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	hs := f.headsets(t)
	if len(hs) != 1 || len(hs[0].OwnerReferences) != 1 || hs[0].OwnerReferences[0].Name != f.user {
		t.Fatalf("headsets = %+v; want one owned by the User", hs)
	}
	if err := f.dw.DirewolfV1alpha1().Users(portsTestNS).Delete(context.Background(), f.user, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "q", Key: testHeadsetKeyB}}); err == nil {
		t.Error("recorded a headset for a missing User")
	}
	if hs := f.headsets(t); len(hs) != 1 {
		t.Errorf("headsets = %d, want no ownerless one", len(hs))
	}
}

func emptyUserInformer(t *testing.T) generic.Informer[*v1alpha1types.User] {
	t.Helper()
	factory := generatedinformers.NewSharedInformerFactory(generatedclient.NewSimpleClientset(), 0)
	inf := factory.Direwolf().V1alpha1().Users().Informer()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	return generic.NewInformer[*v1alpha1types.User](inf)
}

// The placeholder is on the pod before the Headset exists, so a Headset
// created but never given its UID on the pod (the operator died, or the
// patch failed) is still revoked: deleting it ends the Session, and no
// later poll re-creates it (QA, XERK-2669).
func TestRevokeAfterLostUIDPatchHolds(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	k8s, ok := f.sc.K8sClient.(*k8sfake.Clientset)
	if !ok {
		t.Fatal("not a fake clientset")
	}
	patches := 0
	k8s.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 2 { // the UID's patch, after the placeholder's
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, stderrors.New("modified"))
		}
		return false, nil, nil
	})
	known := []VRKnownKey{{Name: "q", Key: testHeadsetKeyA}}
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err == nil {
		t.Fatal("want the UID patch's conflict")
	}
	name := headsetObjectName(f.user, testHeadsetKeyA)
	f.waitHeadsets(t, 1)
	live, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := live.Annotations[vrHeadsetsAnnotation]; !strings.HasPrefix(got, name+"/"+placeholderPrefix) || strings.Contains(got, ",") {
		t.Fatalf("annotation = %q, want only the placeholder for %s", got, name)
	}
	// The placeholder holds while its Headset lives.
	if reason, err := f.sc.headsetRevokedReason(context.Background(), live); err != nil || reason != "" {
		t.Fatalf("headsetRevokedReason = %q, %v; want kept", reason, err)
	}
	if err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitHeadsets(t, 0)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err != nil {
		t.Fatal(err)
	}
	if hs := f.headsets(t); len(hs) != 0 {
		t.Errorf("revoke undone: %+v", hs)
	}
	// The Session ends once the placeholder is past pending.
	live.Annotations[vrHeadsetsAnnotation] = placeholderEntry(name, time.Now().Add(-placeholderPending-time.Second))
	if reason, err := f.sc.headsetRevokedReason(context.Background(), live); err != nil || reason == "" {
		t.Errorf("headsetRevokedReason = %q, %v; want revoked", reason, err)
	}
}

func TestSeededEntriesReplacePlaceholder(t *testing.T) {
	for _, tc := range []struct{ value, entry, want string }{
		{"", "a/~5", "a/~5"},
		{"a/~5", "a/1", "a/1"},
		{"x/9,a/~5", "a/1", "x/9,a/1"},
		{"a/1", "a/1", "a/1"},
		{"ab/~5", "a/1", "ab/~5,a/1"},
	} {
		if got := withSeededEntries(tc.value, tc.entry); got != tc.want {
			t.Errorf("withSeededEntries(%q, %q) = %q, want %q", tc.value, tc.entry, got, tc.want)
		}
	}
	if got := withoutSeededEntry("x/9,a/~5", "a/~5"); got != "x/9" {
		t.Errorf("withoutSeededEntry = %q", got)
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

// podWrites counts pod patches on f's fake clientset.
func podWrites(t *testing.T, f *portsFixture) *int {
	t.Helper()
	k8s, ok := f.sc.K8sClient.(*k8sfake.Clientset)
	if !ok {
		t.Fatal("not a fake clientset")
	}
	n := new(int)
	k8s.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		*n++
		return false, nil, nil
	})
	return n
}

// A taken name, or a create that fails, costs no pod write on later polls:
// each write re-queues the Session, which would poll in a loop (QA).
func TestRecordPairedHeadsetsQuietOnTakenOrFailing(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	writes := podWrites(t, f)
	f.createHeadset(t, headsetObjectName(f.user, testHeadsetKeyB), "someone-else", testHeadsetKeyB, "")
	f.dw.PrependReactor("create", "headsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "headsets"}, "x", stderrors.New("no"))
	})
	known := []VRKnownKey{{Name: "taken", Key: testHeadsetKeyB}, {Name: "failing", Key: testHeadsetKeyA}}
	_ = f.sc.recordPairedHeadsets(context.Background(), s, pod, known) // A: reserve, fail, drop
	first := *writes
	for range 3 {
		if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err != nil {
			t.Fatal(err)
		}
	}
	if *writes != first || first > 2 {
		t.Errorf("pod writes: %d on the first poll, %d after 3 more; want <= 2, then none", first, *writes)
	}
}

// A placeholder holds while its create may be in flight (another worker can
// read the pod before the Headset exists), then counts as revoked.
func TestPlaceholderPendingThenRevoked(t *testing.T) {
	f := newPortsFixture(t)
	watchHeadsets(t, f)
	name := headsetObjectName(f.user, testHeadsetKeyA)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: portsTestNS, Annotations: map[string]string{
		vrHeadsetsAnnotation: placeholderEntry(name, time.Now()),
	}}}
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason != "" {
		t.Errorf("fresh placeholder: %q, %v; want held", reason, err)
	}
	pod.Annotations[vrHeadsetsAnnotation] = placeholderEntry(name, time.Now().Add(-2*placeholderPending))
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason == "" {
		t.Errorf("stale placeholder: %q, %v; want revoked", reason, err)
	}
	// Held by its own Headset, whatever its age; not by another key's.
	f.createHeadset(t, name, f.user, testHeadsetKeyA, "")
	f.waitHeadsets(t, 1)
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason != "" {
		t.Errorf("placeholder with its Headset: %q, %v; want held", reason, err)
	}
	other := headsetObjectName(f.user, testHeadsetKeyB)
	f.createHeadset(t, other, f.user, testHeadsetKeyA, "")
	f.waitHeadsets(t, 2)
	pod.Annotations[vrHeadsetsAnnotation] = placeholderEntry(other, time.Now().Add(-2*placeholderPending))
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason == "" {
		t.Errorf("placeholder under another key's Headset: %q, %v; want revoked", reason, err)
	}
}

// A failed create backs off briefly at first, doubling on repeats up to the
// cap: one transient error must not cost a short session its pairing.
func TestHeadsetBackoffGrows(t *testing.T) {
	var b headsetBackoff
	got := make([]time.Duration, 0, 7)
	for range 7 {
		b.set("h")
		got = append(got, b.backoff["h"])
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("backoffs = %v, want %v", got, want)
	}
	if !b.active("h") || b.active("other") {
		t.Error("active wrong")
	}
	b.clear("h")
	if b.active("h") {
		t.Error("cleared name still backed off")
	}
	b.set("h")
	if b.backoff["h"] != headsetBackoffMin {
		t.Errorf("after clear: %v, want the minimum", b.backoff["h"])
	}
}

// Our Headset that exists but is not yet in the cache (created by an earlier
// poll) is adopted: added to the pod, not created again.
func TestRecordPairedHeadsetsAdoptsUncachedOwnHeadset(t *testing.T) {
	f, s, pod := recordFixture(t, "[]")
	f.sc.HeadsetInformer = emptyHeadsetInformer(t)
	name := headsetObjectName(f.user, testHeadsetKeyA)
	f.createHeadset(t, name, f.user, testHeadsetKeyA, "")
	creates := 0
	f.dw.PrependReactor("create", "headsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		creates++
		return false, nil, nil
	})
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: "q", Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	live, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if creates != 0 || live.Annotations[vrHeadsetsAnnotation] != name+"/"+name+"-uid" {
		t.Errorf("creates %d, annotation %q; want adopted as %s/%s-uid", creates, live.Annotations[vrHeadsetsAnnotation], name, name)
	}
}

// A placeholder whose Headset is being deleted does not hold.
func TestPlaceholderOfTerminatingHeadsetIsRevoked(t *testing.T) {
	f := newPortsFixture(t)
	watchHeadsets(t, f)
	name := headsetObjectName(f.user, testHeadsetKeyA)
	f.createHeadsetObj(t, name, f.user, testHeadsetKeyA, "", func(h *v1alpha1types.Headset) {
		h.DeletionTimestamp, h.Finalizers = &metav1.Time{Time: time.Now()}, []string{"test/hold"}
	})
	f.waitHeadsets(t, 1)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: portsTestNS, Annotations: map[string]string{
		vrHeadsetsAnnotation: placeholderEntry(name, time.Now().Add(-2*placeholderPending)),
	}}}
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason == "" {
		t.Errorf("placeholder of a terminating Headset: %q, %v; want revoked", reason, err)
	}
}

func emptyHeadsetInformer(t *testing.T) generic.Informer[*v1alpha1types.Headset] {
	t.Helper()
	factory := generatedinformers.NewSharedInformerFactory(generatedclient.NewSimpleClientset(), 0)
	inf := factory.Direwolf().V1alpha1().Headsets().Informer()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	return generic.NewInformer[*v1alpha1types.Headset](inf)
}
