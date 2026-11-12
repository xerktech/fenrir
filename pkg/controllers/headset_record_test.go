package controllers

import (
	"context"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

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

// A VR App's split App pod gets vr-agent (with its own token Secret, never
// wolf-agent's, which no App container mounts) and its bus directory, for
// the server key (XERK-2811); it never gates the pod's readiness, which the
// stream waits on before the App can start WiVRn. A flat App's pod gets
// neither.
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
	if c := f.sc.vrAgentContainer(true, vrAgentUID, vrAgentUID); c.ReadinessProbe == nil {
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
	f.sc.recordAppPodServerKey(context.Background(), s, wolf)
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
	f.sc.recordAppPodServerKey(context.Background(), s, wolf)
	if len(agent.tokens) != 0 {
		t.Error("polled an App pod with no vr-agent")
	}
}

// A Moonlight Session's split App pod reports its VR server key once, with
// vr-agent's own token, for the front door to hand the headset to pin
// (XERK-2811); once recorded it is not polled again.
func TestMoonlightVRRecordsServerKey(t *testing.T) {
	agent := newFakeVRAgent(t, new(testAgentKeyPair(t)))
	sess, objs := vrSessionPods("quest/quest-uid")
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
	waitPod(t, f, s.Name)
	waitPod(t, f, s.Name+"-app")
	wolf, err := f.sc.podController.Informer().Namespaced(portsTestNS).Get(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 56)
	agent.set(VRStatus{ServerKey: "not-a-key"}, false)
	f.sc.recordAppPodServerKey(context.Background(), s, wolf)
	if s.Status.VRServerKey != "" {
		t.Fatalf("malformed key recorded: %q", s.Status.VRServerKey)
	}
	agent.set(VRStatus{ServerKey: key}, false)
	f.sc.recordAppPodServerKey(context.Background(), s, wolf)
	if s.Status.VRServerKey != key {
		t.Fatalf("vrServerKey = %q, want %q", s.Status.VRServerKey, key)
	}
	agent.set(VRStatus{ServerKey: strings.Repeat("cd", 56)}, false)
	f.sc.recordAppPodServerKey(context.Background(), s, wolf)
	if s.Status.VRServerKey != key {
		t.Errorf("vrServerKey replaced: %q", s.Status.VRServerKey)
	}
	if got := agent.tokens; len(got) != 2 || got[0] != "Bearer token" {
		t.Errorf("agent saw %v, want two polls with vr-agent's token, none after recording", got)
	}
}
