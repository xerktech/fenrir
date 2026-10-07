package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// lifecycleFixture is a portsFixture holding sess (stored in the fake API)
// with its block allocated and objs (e.g. its pod) in the pod informer.
func lifecycleFixture(t *testing.T, sess func(*portsFixture) *v1alpha1types.Session, objs func(*v1alpha1types.Session) []runtime.Object) (*portsFixture, *v1alpha1types.Session) {
	t.Helper()
	probe := newPortsFixture(t)
	s := sess(probe)
	var k8sObjs []runtime.Object
	if objs != nil {
		k8sObjs = objs(s)
	}
	f := newPortsFixture(t, k8sObjs...)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return f, s
}

func (f *portsFixture) sessionExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

func podReadySession(f *portsFixture) *v1alpha1types.Session {
	s := f.session("alex-1", f.user)
	s.Status.Ports = blockPorts(20000)
	s.Status.Conditions = []metav1.Condition{
		{Type: "PortsAllocated", Status: metav1.ConditionTrue, Reason: "Test"},
		{Type: podCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"},
	}
	return s
}

// The pod runs once: any sign that it finished ends the Session (which
// garbage-collects the pod and its ResourceClaims), and it is never recreated.
func TestFinishedPodEndsSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod) *corev1.Pod
	}{
		{"game container exited", func(p *corev1.Pod) *corev1.Pod {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{
				{Name: "wolf", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "steam", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}},
			}
			return p
		}},
		{"pod failed", func(p *corev1.Pod) *corev1.Pod { p.Status.Phase = corev1.PodFailed; return p }},
		{"pod on another port block", func(p *corev1.Pod) *corev1.Pod {
			p.Annotations[portBlockAnnotation] = "20007"
			return p
		}},
		{"pod deleted", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, podReadySession, func(s *v1alpha1types.Session) []runtime.Object {
				if tc.mutate == nil {
					return nil
				}
				return []runtime.Object{tc.mutate(readyPod(s))}
			})
			for range 500 {
				if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(sess.Name); err == nil || tc.mutate == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}

			if _, err := f.sc.reconcilePod(context.Background(), sess); !errors.Is(err, errSessionEnded) {
				t.Fatalf("reconcilePod = %v, want errSessionEnded", err)
			}
			if f.sessionExists(t, sess.Name) {
				t.Error("session not deleted")
			}
			if pods, _ := f.sc.K8sClient.CoreV1().Pods(portsTestNS).List(context.Background(), metav1.ListOptions{}); tc.mutate == nil && len(pods.Items) != 0 {
				t.Errorf("pod recreated: %+v", pods.Items)
			}
		})
	}
}

// A running pod is left alone.
func TestRunningPodKeepsSession(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, func(s *v1alpha1types.Session) []runtime.Object {
		return []runtime.Object{readyPod(s)}
	})
	for range 500 {
		if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(sess.Name); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pod, err := f.sc.reconcilePod(context.Background(), sess)
	if err != nil || pod == nil {
		t.Fatalf("reconcilePod = %v, %v", pod, err)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session deleted")
	}
}

// A pod of an earlier Session with the same name is not adopted or used.
func TestForeignPodIsNotUsed(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, func(s *v1alpha1types.Session) []runtime.Object {
		p := readyPod(s)
		p.OwnerReferences[0].UID = "someone-else"
		return []runtime.Object{p}
	})
	for range 500 {
		if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(sess.Name); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.sc.reconcilePod(context.Background(), sess); err == nil || errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcilePod = %v, want a retryable error", err)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session deleted over another session's pod")
	}
}

func attachedSession(f *portsFixture, base int32) *v1alpha1types.Session {
	s := f.session("alex-1", f.user)
	s.Generation = 1
	s.Status.Ports = blockPorts(base)
	s.Status.WolfSessionID = "old"
	s.Status.AttachedGeneration = 1
	s.Status.StreamURL = "rtsp://127.0.0.1:1"
	return s
}

// A disconnect keeps the Session and pod, waiting for /resume, and does not
// re-add a Wolf session on its own.
func TestDisconnectStartsGracePeriod(t *testing.T) {
	agent := newFakeAgent(t) // lists no sessions: Wolf dropped ours
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session { return attachedSession(f, agent.base(t)) },
		func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	before := time.Now()
	for range 2 {
		if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
			t.Fatal(err)
		}
	}
	st := sess.Status
	if st.DisconnectedAt == nil || st.DisconnectedAt.Time.Before(before.Truncate(time.Second)) {
		t.Errorf("DisconnectedAt = %v, want set now", st.DisconnectedAt)
	}
	if st.WolfSessionID != "" || st.StreamURL != "" {
		t.Errorf("stale stream kept: id %q url %q", st.WolfSessionID, st.StreamURL)
	}
	if agent.added != 0 {
		t.Errorf("re-attached %d times without a resume", agent.added)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session deleted on disconnect")
	}
}

// /resume bumps the generation with new keys: the old Wolf session (if still
// there) is stopped and the pod re-attached.
func TestResumeReattaches(t *testing.T) {
	for _, tc := range []struct {
		name         string
		disconnected bool
	}{
		{"after disconnect", true},
		{"while still attached", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newFakeAgent(t)
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := attachedSession(f, agent.base(t))
				if tc.disconnected {
					s.Status.WolfSessionID = ""
					s.Status.StreamURL = ""
					s.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
				}
				s.Generation = 2
				s.Spec.Config.AESKey = "new-key"
				return s
			}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

			if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
				t.Fatal(err)
			}
			st := sess.Status
			if agent.added != 1 || st.WolfSessionID != "4242" || st.AttachedGeneration != 2 || st.DisconnectedAt != nil || st.StreamURL == "" {
				t.Errorf("not re-attached: adds %d, status %+v", agent.added, st)
			}
			if wantStop := !tc.disconnected; wantStop != (len(agent.stopped) == 1 && agent.stopped[0] == "old") {
				t.Errorf("stopped %v, want old stopped = %v", agent.stopped, wantStop)
			}
		})
	}
}

func TestExpiredReason(t *testing.T) {
	sc := &SessionController{SessionControllerOptions: SessionControllerOptions{DisconnectGracePeriod: 10 * time.Minute}}
	now := time.Now()
	old := metav1.NewTime(now.Add(-time.Hour))
	at := func(d time.Duration) *metav1.Time { return &metav1.Time{Time: now.Add(-d)} }
	for _, tc := range []struct {
		name   string
		status v1alpha1types.SessionStatus
		want   bool
	}{
		{"streaming", v1alpha1types.SessionStatus{WolfSessionID: "1"}, false},
		{"never started", v1alpha1types.SessionStatus{}, true},
		{"disconnected within grace", v1alpha1types.SessionStatus{DisconnectedAt: at(9 * time.Minute)}, false},
		{"disconnected past grace", v1alpha1types.SessionStatus{DisconnectedAt: at(10 * time.Minute)}, true},
		{"run in a Deployment by an older operator", v1alpha1types.SessionStatus{
			WolfSessionID: "1",
			Conditions:    []metav1.Condition{{Type: legacyDeploymentCondition, Status: metav1.ConditionTrue}},
		}, true},
		{"pod-era session", v1alpha1types.SessionStatus{
			WolfSessionID: "1",
			Conditions: []metav1.Condition{
				{Type: legacyDeploymentCondition, Status: metav1.ConditionTrue},
				{Type: podCreatedCondition, Status: metav1.ConditionTrue},
			},
		}, false},
	} {
		s := &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: old}, Status: tc.status}
		if got := sc.expiredReason(s, now) != ""; got != tc.want {
			t.Errorf("%s: expired = %v, want %v", tc.name, got, tc.want)
		}
	}
	fresh := &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now)}}
	if r := sc.expiredReason(fresh, now); r != "" {
		t.Errorf("fresh session expired: %s", r)
	}
}

// A grace period of zero ends the session as soon as the disconnect is seen.
func TestZeroGraceEndsOnDisconnect(t *testing.T) {
	sc := &SessionController{}
	s := &v1alpha1types.Session{Status: v1alpha1types.SessionStatus{DisconnectedAt: &metav1.Time{Time: time.Now()}}}
	if sc.expiredReason(s, time.Now()) == "" {
		t.Error("zero grace period kept a disconnected session")
	}
}

// A pod the API server has but the informer has not seen yet (right after
// creation) is used, not mistaken for a deleted one.
func TestPodCreatedButNotCachedIsUsed(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, nil)
	f.sc.K8sClient = k8sfake.NewClientset(readyPod(sess))
	pod, err := f.sc.reconcilePod(context.Background(), sess)
	if err != nil || pod == nil {
		t.Fatalf("reconcilePod = %v, %v; want the live pod", pod, err)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session deleted although its pod exists")
	}
}

// Wolf streaming our keys without our record of it (the status write after
// AddSession was lost) ends the session: the stream cannot be stopped by ID.
func TestUnrecordedWolfStreamEndsSession(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"aes_key":"k","aes_iv":"i"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); !errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcileActiveStreams = %v, want errSessionEnded", err)
	}
	if f.sessionExists(t, sess.Name) {
		t.Error("session kept")
	}
	if agent.added != 0 {
		t.Error("added a second Wolf stream")
	}
}

// ...but not when our cached Session is just older than the live one.
func TestUnrecordedWolfStreamRechecksLiveSession(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"aes_key":"k","aes_iv":"i"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })
	stale := sess.DeepCopy()
	stale.ResourceVersion = "stale"

	if err := f.sc.reconcileActiveStreams(context.Background(), stale, readyPod(stale)); err == nil || errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcileActiveStreams = %v, want a retryable error", err)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session deleted from a stale cache read")
	}
}

var sessionsResource = schema.GroupVersionResource{Group: v1alpha1types.GroupName, Version: "v1alpha1", Resource: "sessions"}

// conflictOnce makes the first status update fail with a conflict, after
// applying mutate to the stored Session (another writer racing us).
func conflictOnce(t *testing.T, f *portsFixture, mutate func(*v1alpha1types.Session)) {
	t.Helper()
	var once sync.Once
	f.dw.PrependReactor("update", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		conflicted := false
		once.Do(func() {
			conflicted = true
			s, err := f.dw.Tracker().Get(sessionsResource, portsTestNS, "alex-1")
			if err != nil {
				t.Errorf("tracker get: %v", err)
				return
			}
			current, ok := s.(*v1alpha1types.Session)
			if !ok {
				t.Errorf("tracker returned %T", s)
				return
			}
			stored := current.DeepCopy()
			mutate(stored)
			if err := f.dw.Tracker().Update(sessionsResource, stored, portsTestNS); err != nil {
				t.Errorf("tracker update: %v", err)
			}
		})
		if conflicted {
			return true, nil, apierrors.NewConflict(v1alpha1types.Resource("sessions"), "alex-1", errors.New("modified"))
		}
		return false, nil, nil
	})
}

// A conflict caused by a spec change (/resume) re-applies our status, so a
// just-added Wolf session's ID is not lost.
func TestWriteStatusReappliesOverSpecChange(t *testing.T) {
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session { return f.session("alex-1", f.user) }, nil)
	old := sess.Status.DeepCopy()
	conflictOnce(t, f, func(s *v1alpha1types.Session) { s.Spec.Config.AESKey = "newer" })

	sess.Status.WolfSessionID = "1001"
	if err := f.sc.writeStatus(context.Background(), sess, old); err != nil {
		t.Fatal(err)
	}
	got, _ := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Get(context.Background(), sess.Name, metav1.GetOptions{})
	if got.Status.WolfSessionID != "1001" || got.Spec.Config.AESKey != "newer" {
		t.Errorf("stored session = spec key %q, wolfSessionID %q; want the newer spec and our status", got.Spec.Config.AESKey, got.Status.WolfSessionID)
	}
}

// A conflict because another reconcile wrote a newer status means our read
// was stale: our status must not overwrite it.
func TestWriteStatusKeepsNewerStatus(t *testing.T) {
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session { return f.session("alex-1", f.user) }, nil)
	old := sess.Status.DeepCopy()
	conflictOnce(t, f, func(s *v1alpha1types.Session) {
		s.Status.WolfSessionID = "1002"
		s.Status.AttachedGeneration = 3
	})

	sess.Status.DisconnectedAt = &metav1.Time{Time: time.Now()}
	if err := f.sc.writeStatus(context.Background(), sess, old); err == nil {
		t.Error("stale status write reported success")
	}
	got, _ := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Get(context.Background(), sess.Name, metav1.GetOptions{})
	if got.Status.WolfSessionID != "1002" || got.Status.DisconnectedAt != nil {
		t.Errorf("newer status overwritten: %+v", got.Status)
	}
}

// Wolf is only changed on behalf of the live Session: a stale cached read
// (from before our own last status write) must not stop or add streams.
func TestStaleReadDoesNotTouchWolf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session func(*portsFixture, int32) *v1alpha1types.Session
	}{
		{"first attach", func(f *portsFixture, base int32) *v1alpha1types.Session {
			s := f.session("alex-1", f.user)
			s.Status.Ports = blockPorts(base)
			return s
		}},
		{"resume while attached", func(f *portsFixture, base int32) *v1alpha1types.Session {
			s := attachedSession(f, base)
			s.Generation = 2
			s.Spec.Config.AESKey = "new-key"
			return s
		}},
		{"resume after disconnect", func(f *portsFixture, base int32) *v1alpha1types.Session {
			s := attachedSession(f, base)
			s.Status.WolfSessionID = ""
			s.Status.DisconnectedAt = &metav1.Time{Time: time.Now()}
			s.Generation = 2
			s.Spec.Config.AESKey = "new-key"
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newFakeAgent(t)
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session { return tc.session(f, agent.base(t)) },
				func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })
			stale := sess.DeepCopy()
			stale.ResourceVersion = "stale"

			if err := f.sc.reconcileActiveStreams(context.Background(), stale, readyPod(stale)); err == nil {
				t.Error("stale read reconciled without error")
			}
			if agent.added != 0 || len(agent.stopped) != 0 {
				t.Errorf("stale read changed Wolf: %d adds, stops %v", agent.added, agent.stopped)
			}
		})
	}
}

// recordingController stands in for the session workqueue in tests calling
// Reconcile directly.
type recordingController struct {
	generic.Controller[*v1alpha1types.Session]
}

func (recordingController) EnqueueAfter(string, string, time.Duration) {}

// Once nothing changes, reconciling (every streamPollInterval while
// attached) does not rewrite the Session's status.
func TestSteadyStateReconcileDoesNotWriteStatus(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"aes_key":"k","aes_iv":"i"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Status.StreamURL = fmt.Sprintf("rtsp://127.0.0.1:%d", s.Status.Ports.RTSP)
		s.Status.Conditions = []metav1.Condition{{Type: podCreatedCondition, Status: metav1.ConditionTrue, Reason: "Test"}}
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s), readyPod(s)} })
	f.sc.controller = recordingController{}
	// The fake agent's port decides the block; let the allocator hand it out.
	base := sess.Status.Ports.HTTP
	f.sc.ports = newPortAllocator(PortRange{Min: base, Max: base + sessionPortBlockSize - 1})
	for range 500 {
		if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(sess.Name); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var writes int
	f.dw.PrependReactor("update", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			writes++
		}
		return false, nil, nil
	})
	sessions := f.dw.DirewolfV1alpha1().Sessions(portsTestNS)
	for i := range 3 {
		live, err := sessions.Get(context.Background(), sess.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.sc.Reconcile(portsTestNS, sess.Name, live); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	// The first reconcile records the conditions; later ones change nothing.
	if writes != 1 {
		t.Errorf("%d status writes over 3 steady reconciles, want 1", writes)
	}
}

// endSession decides from the Session as read, which may be a cached copy
// older than an attach or /resume that just landed. Its delete must carry that
// read's resourceVersion so a stale decision conflicts instead of deleting.
func TestEndSessionPreconditionsResourceVersion(t *testing.T) {
	f := newPortsFixture(t)
	var got *metav1.Preconditions
	f.dw.PrependReactor("delete", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if d, ok := action.(k8stesting.DeleteAction); ok {
			got = d.GetDeleteOptions().Preconditions
		}
		return true, nil, nil
	})
	sess := f.session("alex-1", "alex")
	sess.ResourceVersion = "42"
	if err := f.sc.endSession(context.Background(), sess, "test"); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.UID == nil || *got.UID != sess.UID ||
		got.ResourceVersion == nil || *got.ResourceVersion != "42" {
		t.Errorf("delete preconditions = %+v, want UID %s and resourceVersion 42", got, sess.UID)
	}
}

// A delete that fails (here, the Session changed since it was read) must not
// be recorded as PodCreated=False: the next reconcile would then recreate the
// gone pod instead of ending the Session.
func TestFailedEndSessionDoesNotRecreatePod(t *testing.T) {
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := podReadySession(f)
		s.CreationTimestamp = metav1.Now() // not yet past the unstarted TTL
		return s
	}, nil)
	f.waitInformer(t, sess.Name, true)
	refuse := true
	var statusWrites int
	f.dw.PrependReactor("*", "sessions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch {
		case action.GetVerb() == "delete" && refuse:
			return true, nil, apierrors.NewConflict(v1alpha1types.Resource("sessions"), sess.Name, errors.New("resourceVersion changed"))
		case action.GetVerb() == "update" && action.GetSubresource() == "status":
			statusWrites++
		}
		return false, nil, nil
	})

	cached, err := f.sc.SessionInformer.Namespaced(portsTestNS).Get(sess.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sc.Reconcile(portsTestNS, sess.Name, cached.DeepCopy()); !apierrors.IsConflict(err) {
		t.Fatalf("Reconcile = %v, want the delete's conflict", err)
	}
	if statusWrites != 0 {
		t.Errorf("%d status writes after a failed delete, want 0", statusWrites)
	}

	refuse = false
	if err := f.sc.Reconcile(portsTestNS, sess.Name, cached.DeepCopy()); err != nil {
		t.Fatalf("requeued Reconcile: %v", err)
	}
	if f.sessionExists(t, sess.Name) {
		t.Error("session not deleted on requeue")
	}
	if pods, _ := f.sc.K8sClient.CoreV1().Pods(portsTestNS).List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Errorf("pod recreated: %+v", pods.Items)
	}
}

// The game runs on the lobby's display, which outlives the Moonlight
// streams: the first attach creates the lobby, before the first stream, so
// that the lobby's socket is wayland-1, the display the game waits for.
func TestFirstAttachCreatesLobbyBeforeStream(t *testing.T) {
	agent := newFakeAgent(t)
	agent.lobbies = "[]"
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Generation = 1
		s.Status.Ports = blockPorts(agent.base(t))
		s.Spec.Config.VideoWidth, s.Spec.Config.VideoHeight, s.Spec.Config.VideoRefreshRate = 2560, 1440, 120
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	created, added := slices.Index(agent.calls, "/api/v1/lobbies/create"), slices.Index(agent.calls, "/api/v1/sessions/add")
	if created < 0 || added < created {
		t.Fatalf("calls %v: want the lobby created before the stream", agent.calls)
	}
	l := agent.lobby
	if l.StopWhenEveryoneLeaves {
		t.Error("lobby stops when its stream leaves: a disconnect would end the game")
	}
	if v := l.VideoSettings; v.Width != 2560 || v.Height != 1440 || v.RefreshRate != 120 || v.WaylandNode != "/dev/dri/renderD129" || v.RunnerNode != "/dev/dri/renderD129" {
		t.Errorf("video settings %+v: want the session's mode on Wolf's render node", v)
	}
	if l.Runner.Type != "process" || l.Runner.RunCmd == "" || l.ProfileID != wolfapi.MoonlightProfileID {
		t.Errorf("lobby %+v", l)
	}
	if st := sess.Status; st.WolfSessionID != "4242" || st.StreamURL == "" {
		t.Errorf("not attached: %+v", st)
	}
}

// A lobby already there (an attach whose status write was lost, or a resume)
// is the pod's: no second one, whose display the game isn't on.
func TestAttachReusesLobby(t *testing.T) {
	agent := newFakeAgent(t)
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Generation = 1
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	if agent.lobby != nil || agent.added != 1 {
		t.Errorf("lobby create %+v, adds %d: want the existing lobby used", agent.lobby, agent.added)
	}
}

// Resuming a pod whose lobby is gone can't bring the game back: its display
// died with the lobby. End the Session rather than stream an empty display.
func TestResumeWithoutLobbyEndsSession(t *testing.T) {
	agent := newFakeAgent(t)
	agent.lobbies = "[]"
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Status.WolfSessionID = ""
		s.Status.StreamURL = ""
		s.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
		s.Generation = 2
		s.Spec.Config.AESKey = "new-key"
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); !errors.Is(err, errSessionEnded) {
		t.Fatalf("err = %v, want errSessionEnded", err)
	}
	if agent.added != 0 || agent.lobby != nil {
		t.Errorf("adds %d, lobby %+v: want neither", agent.added, agent.lobby)
	}
	if f.sessionExists(t, sess.Name) {
		t.Error("session kept without its game display")
	}
}

// A resume's stream must not share the session ID of the stream it
// supersedes: that stream's dead client is timed out by Wolf later, and its
// pause would end every stream with the ID (XERK-1380). The IDs are the
// seeded stream clients', picked by generation.
func TestResumeUsesAnotherWolfClient(t *testing.T) {
	agent := newFakeAgent(t)
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Generation = 1
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	var used []string
	for gen := int64(1); gen <= 5; gen++ {
		sess.Generation = gen
		sess.Spec.Config.AESKey = fmt.Sprintf("key-%d", gen)
		if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
			t.Fatal(err)
		}
		used = append(used, agent.last.ClientID)
	}
	if want := []string{"11", "12", "13", "10", "11"}; !slices.Equal(used, want) {
		t.Errorf("attaches used Wolf clients %v, want %v", used, want)
	}
}

// A Wolf without the stream clients (its config predates them) still
// streams, with Wolf's certless dummy client.
func TestAttachWithoutWolfClientsUsesDummy(t *testing.T) {
	agent := newFakeAgent(t)
	agent.clients = `[{"client_id":"99","app_state_folder":"someone"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Generation = 1
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	if agent.added != 1 || agent.last.ClientID != "" {
		t.Errorf("adds %d with client %q, want one with the dummy", agent.added, agent.last.ClientID)
	}
}
