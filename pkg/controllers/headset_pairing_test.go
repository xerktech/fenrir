package controllers

import (
	"context"
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
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
	if v := pod.Annotations[vrHeadsetsAnnotation]; v != "" {
		t.Errorf("pod annotation = %q", v)
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
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err == nil {
		t.Fatal("first poll: no error, want the failed create and patch")
	}
	f.waitHeadsets(t, 1)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, known); err != nil {
		t.Fatal(err)
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
	name := "Quest\u202e\n3\x00" + strings.Repeat("é", 100)
	if err := f.sc.recordPairedHeadsets(context.Background(), s, pod, []VRKnownKey{{Name: name, Key: testHeadsetKeyA}}); err != nil {
		t.Fatal(err)
	}
	hs := f.headsets(t)
	if len(hs) != 1 {
		t.Fatalf("headsets = %d", len(hs))
	}
	got := hs[0].Spec.DisplayName
	if !strings.HasPrefix(got, "Quest3é") || len(got) > 128 || strings.ContainsAny(got, "\u202e\n\x00") || !utf8.ValidString(got) {
		t.Errorf("displayName = %q (%d bytes)", got, len(got))
	}
}
