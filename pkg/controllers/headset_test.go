package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

const (
	testHeadsetKeyA = "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
	testHeadsetKeyB = "-----BEGIN PUBLIC KEY-----\nBBBB\n-----END PUBLIC KEY-----\n"
)

// watchHeadsets turns on f's Headset handling over f's own clientset.
func watchHeadsets(t *testing.T, f *portsFixture) {
	t.Helper()
	factory := generatedinformers.NewSharedInformerFactory(f.dw, 0)
	inf := factory.Direwolf().V1alpha1().Headsets().Informer()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	f.sc.WatchHeadsets(f.dw.DirewolfV1alpha1().Headsets(portsTestNS), generic.NewInformer[*v1alpha1types.Headset](inf))
}

func (f *portsFixture) createHeadset(t *testing.T, name, user, key, display string) {
	t.Helper()
	h := &v1alpha1types.Headset{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: portsTestNS},
		Spec: v1alpha1types.HeadsetSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
			PublicKey:     key,
			DisplayName:   display,
		},
	}
	if _, err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Create(context.Background(), h, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (f *portsFixture) waitHeadsets(t *testing.T, n int) {
	t.Helper()
	for range 500 {
		if hs, _ := f.sc.HeadsetInformer.List(labels.Everything()); len(hs) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("headset cache never held %d", n)
}

// headsetEnv returns c's vrHeadsetsEnv values.
func headsetEnv(c *corev1.Container) []string {
	var got []string
	for _, e := range c.Env {
		if e.Name == vrHeadsetsEnv {
			got = append(got, e.Value)
		}
	}
	return got
}

// A VR App's containers get exactly their User's Headsets, sorted by name,
// in WiVRn's known_keys format; the App pod records them; the Wolf pod and
// the operator's containers get neither.
func TestVRPodSeededWithUsersHeadsets(t *testing.T) {
	f := newPortsFixture(t)
	withVR(t, f)
	watchHeadsets(t, f)
	f.createHeadset(t, "quest-b", f.user, testHeadsetKeyB, "")
	f.createHeadset(t, "quest-a", f.user, testHeadsetKeyA, "Quest 3")
	f.createHeadset(t, "theirs", "someone-else", testHeadsetKeyB, "")
	f.waitHeadsets(t, 3)

	sess := f.session("alex-1", f.user)
	if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	wolf, app, err := f.sc.buildPods(sess, testWolfPodUID)
	if err != nil {
		t.Fatal(err)
	}
	if got := app.Annotations[vrHeadsetsAnnotation]; got != "quest-a,quest-b" {
		t.Errorf("App pod %s = %q, want quest-a,quest-b", vrHeadsetsAnnotation, got)
	}
	if _, ok := wolf.Annotations[vrHeadsetsAnnotation]; ok {
		t.Error("Wolf pod carries the headsets annotation")
	}
	want := []wivrnKnownKey{{Key: testHeadsetKeyA, Name: "Quest 3"}, {Key: testHeadsetKeyB, Name: "quest-b"}}
	for _, pod := range []*corev1.Pod{wolf, app} {
		for _, ctrs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
			for _, c := range ctrs {
				got := headsetEnv(&c)
				operator := isWolfPodContainer(c.Name) || c.Name == v1alpha1types.UdevRelayContainerName
				if operator {
					if len(got) != 0 {
						t.Errorf("operator container %s/%s got %s", pod.Name, c.Name, vrHeadsetsEnv)
					}
					continue
				}
				if len(got) != 1 {
					t.Errorf("App container %s/%s has %s %v, want one", pod.Name, c.Name, vrHeadsetsEnv, got)
					continue
				}
				var keys []wivrnKnownKey
				if err := json.Unmarshal([]byte(got[0]), &keys); err != nil || len(keys) != 2 || keys[0] != want[0] || keys[1] != want[1] {
					t.Errorf("App container %s/%s %s = %s, want %v", pod.Name, c.Name, vrHeadsetsEnv, got[0], want)
				}
			}
		}
	}
}

// No Headsets (or no WatchHeadsets): an empty list, so WiVRn pairs by PIN,
// never "unset" (the image refuses that) or the keys left on the volume.
func TestVRPodWithoutHeadsetsSeedsNone(t *testing.T) {
	for _, watch := range []bool{false, true} {
		f := newPortsFixture(t)
		withVR(t, f)
		if watch {
			watchHeadsets(t, f)
		}
		sess := f.session("alex-1", f.user)
		if err := f.sc.allocatePorts(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
		_, app, err := f.sc.buildPods(sess, testWolfPodUID)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := app.Annotations[vrHeadsetsAnnotation]; !ok || got != "" {
			t.Errorf("watch=%v: annotation %q (present %v), want present and empty", watch, got, ok)
		}
		if got := headsetEnv(&app.Spec.Containers[0]); len(got) != 1 || got[0] != "[]" {
			t.Errorf("watch=%v: %s = %v, want [[]]", watch, vrHeadsetsEnv, got)
		}
	}
}

// A flat App gets no headsets, nor the annotation.
func TestFlatAppGetsNoHeadsets(t *testing.T) {
	f := newPortsFixture(t)
	watchHeadsets(t, f)
	f.createHeadset(t, "quest", f.user, testHeadsetKeyA, "")
	f.waitHeadsets(t, 1)
	sess := podReadySession(f)
	pod, err := f.sc.buildPod(sess)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pod.Annotations[vrHeadsetsAnnotation]; ok {
		t.Error("flat pod carries the headsets annotation")
	}
	for _, c := range pod.Spec.Containers {
		if len(headsetEnv(&c)) != 0 {
			t.Errorf("flat container %s got %s", c.Name, vrHeadsetsEnv)
		}
	}
}

// The App's own value would let any key in: it is replaced, and comes first.
func TestWithVRHeadsetsReplacesAppValue(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init", Env: []corev1.EnvVar{{Name: vrHeadsetsEnv, Value: `[{"key":"evil","name":"x"}]`}}}},
		Containers:     []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "A", Value: "1"}, {Name: vrHeadsetsEnv, Value: "[]"}}}},
	}
	if _, err := withVRHeadsets(&spec, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		if got := headsetEnv(&c); len(got) != 1 || got[0] != "[]" || c.Env[0].Name != vrHeadsetsEnv {
			t.Errorf("%s env = %v", c.Name, c.Env)
		}
	}
}

// vrSessionPods is a running split VR Session whose App pod was seeded with
// headsets (comma list, as the annotation holds it).
func vrSessionPods(headsets string) (sess func(*portsFixture) *v1alpha1types.Session, objs func(*v1alpha1types.Session) []runtime.Object) {
	return func(f *portsFixture) *v1alpha1types.Session {
			s := podReadySession(f)
			s.Status.Ports.VR = 21000
			s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{Type: appPodCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"})
			return s
		}, func(s *v1alpha1types.Session) []runtime.Object {
			wolf := readyPod(s)
			wolf.Annotations[appPodAnnotation] = s.Name + "-app"
			app := readyPod(s)
			app.Name = s.Name + "-app"
			app.Labels = map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue}
			delete(app.Annotations, portBlockAnnotation)
			app.Annotations[vrPortAnnotation] = "21000"
			app.Annotations[vrHeadsetsAnnotation] = headsets
			return []runtime.Object{wolf, app}
		}
}

// A Session whose pod was seeded with a Headset that is gone ends on its
// next reconcile, so a delete the operator missed still revokes it.
func TestRevokedHeadsetEndsSessionOnReconcile(t *testing.T) {
	for _, tc := range []struct {
		name, seeded string
		ends         bool
	}{
		{"all present", "quest", false},
		{"seeded with none", "", false},
		{"one revoked", "quest,gone", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, objs := vrSessionPods(tc.seeded)
			f, s := lifecycleFixture(t, sess, objs)
			f.sc.AppPodHostDir = testAppPodHostDir
			watchHeadsets(t, f)
			f.createHeadset(t, "quest", f.user, testHeadsetKeyA, "")
			f.waitHeadsets(t, 1)
			waitPod(t, f, s.Name)
			waitPod(t, f, s.Name+"-app")

			_, err := f.sc.reconcilePod(context.Background(), s)
			if ended := errors.Is(err, errSessionEnded); ended != tc.ends {
				t.Fatalf("reconcilePod = %v, want ended=%v", err, tc.ends)
			}
			if f.sessionExists(t, s.Name) == tc.ends {
				t.Errorf("session exists = %v, want %v", !tc.ends, !tc.ends)
			}
		})
	}
}

// A Headset the API server has but the cache has not seen yet must not end
// the Session.
func TestHeadsetRevokedReasonConfirmsWithAPIServer(t *testing.T) {
	f := newPortsFixture(t)
	empty := generatedinformers.NewSharedInformerFactory(generatedclient.NewSimpleClientset(), 0)
	inf := empty.Direwolf().V1alpha1().Headsets().Informer()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	empty.Start(stop)
	empty.WaitForCacheSync(stop)
	f.sc.WatchHeadsets(f.dw.DirewolfV1alpha1().Headsets(portsTestNS), generic.NewInformer[*v1alpha1types.Headset](inf))
	f.createHeadset(t, "fresh", f.user, testHeadsetKeyA, "")

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: portsTestNS, Annotations: map[string]string{vrHeadsetsAnnotation: "fresh"}}}
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason != "" {
		t.Errorf("headsetRevokedReason = %q, %v; want kept", reason, err)
	}
	pod.Annotations[vrHeadsetsAnnotation] = "never-made"
	if reason, err := f.sc.headsetRevokedReason(context.Background(), pod); err != nil || reason == "" {
		t.Errorf("headsetRevokedReason = %q, %v; want revoked", reason, err)
	}
}

// Deleting a Headset ends, through the running controller, the Sessions
// whose pods were seeded with it, and only those.
func TestHeadsetDeletionEndsItsSessions(t *testing.T) {
	ctx := t.Context()
	sess, objs := vrSessionPods("quest")
	f, s := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := sess(f)
		s.Status.WolfSessionID = "1" // streaming, so the unstarted reaper leaves it
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object {
		res := objs(s)
		other := readyPod(s)
		other.Name, other.UID = "other-app", "other-pod"
		other.OwnerReferences[0].Name, other.OwnerReferences[0].UID = "other", "other-uid"
		other.Labels = map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue}
		other.Annotations[vrHeadsetsAnnotation] = "another"
		return append(res, other)
	})
	other := f.session("other", f.user)
	other.Status.WolfSessionID = "1"
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, s.Name, true)
	f.waitInformer(t, other.Name, true)
	watchHeadsets(t, f)
	f.createHeadset(t, "quest", f.user, testHeadsetKeyA, "")
	f.createHeadset(t, "another", f.user, testHeadsetKeyB, "")
	f.waitHeadsets(t, 2)

	if err := f.dw.DirewolfV1alpha1().Headsets(portsTestNS).Delete(ctx, "quest", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitHeadsets(t, 1)
	if err := f.sc.reconcileHeadset(portsTestNS, "quest", nil); err != nil {
		t.Fatal(err)
	}
	if f.sessionExists(t, s.Name) {
		t.Error("session outlived its revoked headset")
	}
	if !f.sessionExists(t, other.Name) {
		t.Error("ended a session seeded with another headset")
	}
	// An existing Headset is no revocation.
	if err := f.sc.reconcileHeadset(portsTestNS, "another", &v1alpha1types.Headset{}); err != nil || !f.sessionExists(t, other.Name) {
		t.Errorf("reconcileHeadset(present) = %v, session exists %v", err, f.sessionExists(t, other.Name))
	}
}
