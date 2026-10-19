package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

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
			if !tc.disconnected {
				// Wolf still streams the old keys: the client resumed before
				// its old stream's pause.
				agent.sessions = `[{"aes_key":"k","aes_iv":"i","client_id":"old"}]`
			}
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

// A superseded stream Wolf cannot stop wedges the pod (XERK-1688): its
// pipelines hang, and streams added beside it never play. The Session ends
// instead of attaching another stream to it.
func TestUnstoppableStreamEndsSession(t *testing.T) {
	agent := newFakeAgent(t)
	agent.wedged = true
	agent.sessions = `[{"aes_key":"k","aes_iv":"i","client_id":"old"},{"aes_key":"k0","aes_iv":"i0","client_id":"older"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Generation = 2
		s.Spec.Config.AESKey = "new-key"
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); !errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcileActiveStreams = %v, want errSessionEnded", err)
	}
	if f.sessionExists(t, sess.Name) {
		t.Error("session kept on a wedged Wolf")
	}
	if agent.added != 0 {
		t.Errorf("added %d streams beside the stuck one", agent.added)
	}
	if !slices.Equal(agent.stopped, []string{"old", "older"}) {
		t.Errorf("stopped %v, want every stale stream stopped once", agent.stopped)
	}
}

// A stream ID Wolf lists twice is stopped once, and the attach goes on once
// Wolf has dropped it.
func TestResumeStopsRepeatedStreamIDOnce(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"aes_key":"k","aes_iv":"i","client_id":"old"},{"aes_key":"k0","aes_iv":"i0","client_id":"old"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Generation = 2
		s.Spec.Config.AESKey = "new-key"
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(agent.stopped, []string{"old"}) || agent.added != 1 {
		t.Errorf("stopped %v, added %d; want old stopped once and one attach", agent.stopped, agent.added)
	}
}

// Not knowing whether the stale stream went is no reason to attach beside it,
// nor to end the Session: the next reconcile retries.
func TestStaleStreamRelistFailureRetries(t *testing.T) {
	agent := newFakeAgent(t)
	agent.relistFails = true
	agent.sessions = `[{"aes_key":"k","aes_iv":"i","client_id":"old"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := attachedSession(f, agent.base(t))
		s.Generation = 2
		s.Spec.Config.AESKey = "new-key"
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess))
	if err == nil || errors.Is(err, errSessionEnded) {
		t.Fatalf("reconcileActiveStreams = %v, want a retryable error", err)
	}
	if agent.added != 0 {
		t.Errorf("attached %d streams without knowing the stale one is gone", agent.added)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session ended on a failed poll")
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

// A pod the API server rejects as invalid (metadata the CRDs cannot check,
// such as keys nested in ephemeral volume templates) ends the Session at
// once: retrying cannot succeed. Other create errors are retried.
func TestInvalidPodEndsSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		createErr error
		wantEnded bool
	}{
		{"invalid", apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "alex-1", field.ErrorList{
			field.Invalid(field.NewPath("spec", "volumes").Index(0).Child("ephemeral", "volumeClaimTemplate", "metadata", "labels"), "bad key!", "name part must consist of alphanumeric characters"),
		}), true},
		{"transient", apierrors.NewServerTimeout(corev1.Resource("pods"), "create", 1), false},
		{"quota", apierrors.NewForbidden(corev1.Resource("pods"), "alex-1", errors.New("exceeded quota")), false},
		{"conflict", apierrors.NewConflict(corev1.Resource("pods"), "alex-1", errors.New("try again")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := podReadySession(f)
				s.Status.Conditions = s.Status.Conditions[:1] // PortsAllocated only: no pod yet
				return s
			}, nil)
			client := k8sfake.NewClientset()
			client.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.createErr
			})
			f.sc.K8sClient = client
			recorder := record.NewFakeRecorder(10)
			f.sc.Recorder = recorder
			_, err := f.sc.reconcilePod(context.Background(), sess)
			switch {
			case tc.wantEnded:
				if !errors.Is(err, errSessionEnded) {
					t.Fatalf("reconcilePod = %v, want errSessionEnded", err)
				}
				if len(recorder.Events) != 1 || !strings.HasPrefix(<-recorder.Events, "Warning InvalidPod ") {
					t.Error("no Warning InvalidPod Event for the ended Session")
				}
			case len(recorder.Events) > 0:
				t.Errorf("event %q for a retried Session", <-recorder.Events)
			case err == nil || errors.Is(err, errSessionEnded):
				t.Fatalf("reconcilePod = %v, want a retryable error", err)
			}
			if got := f.sessionExists(t, sess.Name); got == tc.wantEnded {
				t.Errorf("session exists = %v, want %v", got, !tc.wantEnded)
			}
		})
	}
}

// An App or User spec that buildPod rejects fails every retry the same way, so
// it ends the Session at once (XERK-1782); a failed lookup stays retryable, and
// a pod the cache has not seen yet is kept.
func TestInvalidSpecEndsSession(t *testing.T) {
	sidecar := func(mount corev1.VolumeMount) *v1alpha1types.SidecarPolicy {
		return &v1alpha1types.SidecarPolicy{VolumeMounts: []corev1.VolumeMount{mount}}
	}
	badMount := corev1.VolumeMount{Name: "undefined", MountPath: "/x"}
	for _, tc := range []struct {
		name      string
		mutate    func(*v1alpha1types.App, *v1alpha1types.User, *v1alpha1types.Session)
		livePod   func(*corev1.Pod) // nil: none
		getErr    error             // from the live pod GET
		wantEnded bool
	}{
		{"operator volume mount", func(app *v1alpha1types.App, _ *v1alpha1types.User, _ *v1alpha1types.Session) {
			ctr := &app.Spec.Template.Spec.Containers[0]
			ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: "wolf-agent-token", MountPath: "/x"})
		}, nil, nil, true},
		{"gpu memory", func(app *v1alpha1types.App, _ *v1alpha1types.User, _ *v1alpha1types.Session) {
			app.Spec.GPU = &v1alpha1types.AppGPU{Memory: new(resource.MustParse("0"))}
		}, nil, nil, true},
		// Admitted by the CRD (it checks only the string), Value() 0.
		{"gpu memory overflow", func(app *v1alpha1types.App, _ *v1alpha1types.User, _ *v1alpha1types.Session) {
			app.Spec.GPU = &v1alpha1types.AppGPU{Memory: new(resource.MustParse("1e1000"))}
		}, nil, nil, true},
		{"resources over user policy", func(app *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			app.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
			user.Spec.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
		}, nil, nil, true},
		{"wolfAgent volume", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{WolfAgent: sidecar(badMount)}
		}, nil, nil, true},
		{"wolfAgent hotplug override", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.Volumes = append(user.Spec.Volumes, corev1.Volume{Name: "input"})
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{WolfAgent: sidecar(corev1.VolumeMount{Name: "input", MountPath: InputDevPath})}
		}, nil, nil, true},
		{"pulseaudio volume", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{PulseAudio: sidecar(badMount)}
		}, nil, nil, true},
		{"wolf volume", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{Wolf: sidecar(badMount)}
		}, nil, nil, true},
		{"uncached pod", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{Wolf: sidecar(badMount)}
		}, func(*corev1.Pod) {}, nil, false},
		// checkPod still judges it.
		{"uncached failed pod", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{Wolf: sidecar(badMount)}
		}, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }, nil, true},
		{"pod lookup fails", func(_ *v1alpha1types.App, user *v1alpha1types.User, _ *v1alpha1types.Session) {
			user.Spec.SidecarPolicies = &v1alpha1types.SidecarPolicies{Wolf: sidecar(badMount)}
		}, nil, apierrors.NewServiceUnavailable("down"), false},
		{"missing app", func(_ *v1alpha1types.App, _ *v1alpha1types.User, s *v1alpha1types.Session) {
			s.Spec.GameReference.Name = "missing"
		}, nil, nil, false},
		{"missing user", func(_ *v1alpha1types.App, _ *v1alpha1types.User, s *v1alpha1types.Session) {
			s.Spec.UserReference.Name = "missing"
		}, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := podReadySession(f)
				s.Status.Conditions = s.Status.Conditions[:1] // PortsAllocated only: no pod yet
				return s
			}, nil)
			app, err := f.sc.AppInformer.Namespaced(portsTestNS).Get(f.app)
			if err != nil {
				t.Fatal(err)
			}
			user, err := f.sc.UserInformer.Namespaced(portsTestNS).Get(f.user)
			if err != nil {
				t.Fatal(err)
			}
			// The informers' copies: buildPod reads them back.
			tc.mutate(app, user, sess)
			var objs []runtime.Object
			if tc.livePod != nil {
				// Created, but neither recorded in status nor in the pod informer.
				pod := readyPod(sess)
				tc.livePod(pod)
				objs = append(objs, pod)
			}
			client := k8sfake.NewClientset(objs...)
			if tc.getErr != nil {
				client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.getErr
				})
			}
			f.sc.K8sClient = client
			recorder := record.NewFakeRecorder(10)
			f.sc.Recorder = recorder
			pod, err := f.sc.reconcilePod(context.Background(), sess)
			// Ended for its spec (not by checkPod): the App says why.
			if tc.wantEnded && tc.livePod == nil {
				select {
				case e := <-recorder.Events:
					if !strings.HasPrefix(e, "Warning InvalidSpec Session "+sess.Name+" of user ") {
						t.Errorf("event %q, want a Warning InvalidSpec", e)
					}
				default:
					t.Error("no Warning Event for the ended Session")
				}
			} else if len(recorder.Events) > 0 {
				t.Errorf("event %q for a Session not ended for its spec", <-recorder.Events)
			}
			switch {
			case tc.wantEnded:
				if !errors.Is(err, errSessionEnded) {
					t.Fatalf("reconcilePod = %v, want errSessionEnded", err)
				}
			case tc.livePod != nil:
				if err != nil || pod == nil {
					t.Fatalf("reconcilePod = %v, %v, want the live pod", pod, err)
				}
			case err == nil || errors.Is(err, errSessionEnded):
				t.Fatalf("reconcilePod = %v, want a retryable error", err)
			}
			if got := f.sessionExists(t, sess.Name); got == tc.wantEnded {
				t.Errorf("session exists = %v, want %v", got, !tc.wantEnded)
			}
			if pods, _ := client.CoreV1().Pods(portsTestNS).List(context.Background(), metav1.ListOptions{}); len(pods.Items) != len(objs) {
				t.Errorf("%d pods, want %d", len(pods.Items), len(objs))
			}
		})
	}
}

// refRecorder keeps the objects Events are recorded on.
type refRecorder struct {
	record.FakeRecorder
	refs []*corev1.ObjectReference
}

func (r *refRecorder) Eventf(obj runtime.Object, _, _, _ string, _ ...any) {
	if ref, ok := obj.(*corev1.ObjectReference); ok {
		r.refs = append(r.refs, ref)
	}
}

// The Event names the App with its UID, or kubectl describe app omits it.
func TestWarnAppReferencesAppWithUID(t *testing.T) {
	f, sess := lifecycleFixture(t, podReadySession, nil)
	app, err := f.sc.AppInformer.Namespaced(portsTestNS).Get(f.app)
	if err != nil {
		t.Fatal(err)
	}
	app.UID = "app-uid" // the informer's copy, which warnApp reads
	recorder := &refRecorder{}
	f.sc.Recorder = recorder
	f.sc.warnApp(sess, "InvalidSpec", "bad")
	want := corev1.ObjectReference{APIVersion: v1alpha1types.GroupVersion.String(), Kind: "App",
		Namespace: portsTestNS, Name: f.app, UID: "app-uid"}
	if len(recorder.refs) != 1 || *recorder.refs[0] != want {
		t.Errorf("Event on %+v, want %+v", recorder.refs, want)
	}
}

// A Session is not ended for an invalid App the informer holds while the
// live App differs (an admin's fix not yet cached): it waits for the cache.
func TestInvalidSpecRechecksLiveAppAndUser(t *testing.T) {
	for _, stale := range []string{"app", "user"} {
		t.Run(stale, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := podReadySession(f)
				s.Status.Conditions = s.Status.Conditions[:1] // PortsAllocated only: no pod yet
				return s
			}, nil)
			app, err := f.sc.AppInformer.Namespaced(portsTestNS).Get(f.app)
			if err != nil {
				t.Fatal(err)
			}
			user, err := f.sc.UserInformer.Namespaced(portsTestNS).Get(f.user)
			if err != nil {
				t.Fatal(err)
			}
			// The informer's copy is invalid and older than the live object.
			ctr := &app.Spec.Template.Spec.Containers[0]
			ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: "wolf-agent-token", MountPath: "/x"})
			if stale == "app" {
				app.ResourceVersion = "stale"
			} else {
				user.ResourceVersion = "stale"
			}
			recorder := record.NewFakeRecorder(10)
			f.sc.Recorder = recorder
			_, err = f.sc.reconcilePod(context.Background(), sess)
			if err == nil || errors.Is(err, errSessionEnded) {
				t.Fatalf("reconcilePod = %v, want a retryable error", err)
			}
			if !f.sessionExists(t, sess.Name) {
				t.Error("session ended over a stale cache")
			}
			if len(recorder.Events) > 0 {
				t.Errorf("event %q for a Session kept", <-recorder.Events)
			}
		})
	}
}

// Wolf streaming our keys without our record of it (an AddSession that timed
// out while Wolf completed it, XERK-2116, or whose status write was lost) is
// adopted by the ID Wolf lists it under, not ended. Shaped as a /resume: the
// adoption must record the new generation and clear the disconnect, or the
// Session re-adopts every poll and is reaped after the grace period.
func TestUnrecordedWolfStreamIsAdopted(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"client_id":"4242","aes_key":"k","aes_iv":"i"}]`
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Generation = 2
		s.Status.Ports = blockPorts(agent.base(t))
		s.Status.AttachedGeneration = 1
		s.Status.DisconnectedAt = new(metav1.Now())
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatalf("reconcileActiveStreams = %v", err)
	}
	if !f.sessionExists(t, sess.Name) {
		t.Error("session ended")
	}
	if agent.added != 0 || len(agent.stopped) != 0 {
		t.Errorf("touched Wolf's stream: %d added, stopped %v", agent.added, agent.stopped)
	}
	st := sess.Status
	if st.WolfSessionID != "4242" || st.AttachedGeneration != 2 || st.StreamURL == "" || st.DisconnectedAt != nil {
		t.Errorf("status = %+v, want stream 4242 attached at generation 2, not disconnected", st)
	}
}

// Two streams under our keys (a retry added one before a timed-out add
// landed) share one ID, so neither can be stopped alone: the session ends,
// whether or not one of the adds was recorded.
func TestDuplicateWolfStreamsEndSession(t *testing.T) {
	for _, recorded := range []string{"", "4242"} {
		t.Run("recorded="+recorded, func(t *testing.T) {
			agent := newFakeAgent(t)
			agent.sessions = `[{"client_id":"4242","aes_key":"k","aes_iv":"i"},{"client_id":"4242","aes_key":"k","aes_iv":"i"}]`
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := f.session("alex-1", f.user)
				s.Status.Ports = blockPorts(agent.base(t))
				s.Status.WolfSessionID = recorded
				return s
			}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

			if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); !errors.Is(err, errSessionEnded) {
				t.Fatalf("reconcileActiveStreams = %v, want errSessionEnded", err)
			}
		})
	}
}

// ...but not when our cached Session is just older than the live one.
func TestDuplicateWolfStreamsRechecksLiveSession(t *testing.T) {
	agent := newFakeAgent(t)
	agent.sessions = `[{"client_id":"4242","aes_key":"k","aes_iv":"i"},{"client_id":"4242","aes_key":"k","aes_iv":"i"}]`
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

// An add Wolf answers after the generic agent timeout still attaches
// (XERK-2116): a CPU-starved node took 10-12s.
func TestSlowAddSessionAttaches(t *testing.T) {
	agent := newFakeAgent(t)
	agent.addDelay = wolfAgentTimeout + time.Second
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
		t.Fatalf("reconcileActiveStreams = %v", err)
	}
	if sess.Status.WolfSessionID != "4242" {
		t.Errorf("WolfSessionID = %q, want 4242", sess.Status.WolfSessionID)
	}
}

// One Wolf lists without an ID cannot be adopted or stopped: the session ends.
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

// Every attach streams with the first seeded stream client, also on a
// volume seeded with the old rotation's four: the xerktech/wolf fork keeps a
// dead client's late timeout from ending a resume that reuses its ID
// (XERK-1948, XERK-1964).
func TestResumeKeepsOneWolfClient(t *testing.T) {
	agent := newFakeAgent(t)
	f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
		s := f.session("alex-1", f.user)
		s.Generation = 1
		s.Status.Ports = blockPorts(agent.base(t))
		return s
	}, func(s *v1alpha1types.Session) []runtime.Object { return []runtime.Object{tokenSecret(s)} })

	var used []string
	for gen := int64(1); gen <= 3; gen++ {
		sess.Generation = gen
		sess.Spec.Config.AESKey = fmt.Sprintf("key-%d", gen)
		if err := f.sc.reconcileActiveStreams(context.Background(), sess, readyPod(sess)); err != nil {
			t.Fatal(err)
		}
		used = append(used, agent.last.ClientID)
	}
	if want := []string{"10", "10", "10"}; !slices.Equal(used, want) {
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

// A Session whose clientIP Wolf could never stream to ends before its pod
// is created (port block, image pull, GPU claim). Once a pod exists, a bad IP
// from a /resume only fails the attach: a later /resume may still save the
// game. That holds even when PodCreated is False or missing (a failed
// reconcile, a lost status write) while the pod runs.
func TestUnstreamableClientIPEndsSessionBeforePod(t *testing.T) {
	for _, tc := range []struct {
		name, ip  string
		hasPod    bool
		podCond   *metav1.ConditionStatus // nil: no PodCreated condition
		wantEnded bool
	}{
		{name: "empty", ip: "", wantEnded: true},
		{name: "ipv6", ip: "2001:db8::1", wantEnded: true},
		{name: "unspecified", ip: "0.0.0.0", wantEnded: true},
		{name: "garbage", ip: "not-an-ip", wantEnded: true},
		{name: "ipv4", ip: "192.0.2.10"},
		{name: "ipv6 with pod", ip: "2001:db8::1", hasPod: true, podCond: ptr.To(metav1.ConditionTrue)},
		{name: "ipv6 with pod after failed reconcile", ip: "2001:db8::1", hasPod: true, podCond: ptr.To(metav1.ConditionFalse)},
		{name: "ipv6 with pod after lost status write", ip: "2001:db8::1", hasPod: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := f.session("alex-1", f.user)
				if tc.hasPod {
					s = podReadySession(f)
					s.Status.Conditions = s.Status.Conditions[:1] // PortsAllocated
					if tc.podCond != nil {
						s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{Type: podCreatedCondition, Status: *tc.podCond, Reason: "Test"})
					}
				}
				s.CreationTimestamp = metav1.Now() // not yet reaped as unstarted
				s.Spec.Config.ClientIP = tc.ip
				return s
			}, func(s *v1alpha1types.Session) []runtime.Object {
				if tc.hasPod {
					return []runtime.Object{readyPod(s)}
				}
				return nil
			})
			_ = f.sc.Reconcile(portsTestNS, sess.Name, sess.DeepCopy())
			if got := f.sessionExists(t, sess.Name); got == tc.wantEnded {
				t.Errorf("session exists = %v, want %v", got, !tc.wantEnded)
			}
			pods, err := f.sc.K8sClient.CoreV1().Pods(portsTestNS).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantEnded {
				if len(pods.Items) != 0 {
					t.Errorf("created %d pods for a session that cannot stream", len(pods.Items))
				}
				if n := len(f.sc.ports.blocks); n != 0 {
					t.Errorf("allocated %d port blocks for a session that cannot stream", n)
				}
			} else if len(pods.Items) != 1 {
				t.Errorf("%d pods, want 1", len(pods.Items))
			}
		})
	}
}

// The pod is looked up live when the cache misses it, a failed lookup is
// retried rather than read as "no pod", and an earlier same-named Session's
// pod does not count as this Session's.
func TestUnstreamableClientIPLooksUpPod(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cached    func(*v1alpha1types.Session) []runtime.Object
		live      func(*v1alpha1types.Session) *k8sfake.Clientset
		wantErr   bool
		wantEnded bool
	}{
		{name: "pod only in live API", live: func(s *v1alpha1types.Session) *k8sfake.Clientset {
			p := readyPod(s)
			p.Annotations[portBlockAnnotation] = "20000" // the block Reconcile allocates
			return k8sfake.NewClientset(p)
		}},
		{name: "live lookup fails", wantErr: true, live: func(*v1alpha1types.Session) *k8sfake.Clientset {
			c := k8sfake.NewClientset()
			c.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewInternalError(errors.New("etcd down"))
			})
			return c
		}},
		{name: "foreign pod", wantEnded: true, cached: func(s *v1alpha1types.Session) []runtime.Object {
			p := readyPod(s)
			p.OwnerReferences[0].UID = "someone-else"
			return []runtime.Object{p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := lifecycleFixture(t, func(f *portsFixture) *v1alpha1types.Session {
				s := f.session("alex-1", f.user)
				s.CreationTimestamp = metav1.Now()
				s.Spec.Config.ClientIP = "2001:db8::1"
				return s
			}, tc.cached)
			if tc.cached != nil {
				for range 500 {
					if _, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(sess.Name); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if tc.live != nil {
				f.sc.K8sClient = tc.live(sess)
			}
			err := f.sc.Reconcile(portsTestNS, sess.Name, sess.DeepCopy())
			if tc.wantErr && err == nil {
				t.Error("Reconcile = nil, want the lookup error")
			}
			if got := f.sessionExists(t, sess.Name); got == tc.wantEnded {
				t.Errorf("session exists = %v, want %v", got, !tc.wantEnded)
			}
			if tc.wantErr || tc.wantEnded {
				if n := len(f.sc.ports.blocks); n != 0 {
					t.Errorf("allocated %d port blocks", n)
				}
			}
		})
	}
}
