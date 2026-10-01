package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
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
