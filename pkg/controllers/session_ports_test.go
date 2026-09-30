package controllers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	sigsyaml "sigs.k8s.io/yaml"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	generatedclient "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/fake"
	generatedinformers "games-on-whales.github.io/direwolf/pkg/generated/informers/externalversions"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

const portsTestNS = "direwolf"

type portsFixture struct {
	dw  *generatedclient.Clientset
	sc  *SessionController
	app string
}

// newPortsFixture builds a SessionController over fake clients seeded with
// the example App and k8sObjects, with informers synced.
func newPortsFixture(t *testing.T, k8sObjects ...runtime.Object) *portsFixture {
	t.Helper()
	steam, err := os.ReadFile("../../examples/steam.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var app v1alpha1types.App
	if err := sigsyaml.Unmarshal(steam, &app); err != nil {
		t.Fatal(err)
	}
	app.Namespace = portsTestNS

	dw := generatedclient.NewSimpleClientset(&app)
	k8s := k8sfake.NewClientset(k8sObjects...)
	dwF := generatedinformers.NewSharedInformerFactory(dw, 0)
	kF := informers.NewSharedInformerFactory(k8s, 0)
	sc := NewSessionController(k8s, nil, nil, dw.DirewolfV1alpha1().Sessions(portsTestNS),
		generic.NewInformer[*v1alpha1types.Session](dwF.Direwolf().V1alpha1().Sessions().Informer()),
		generic.NewInformer[*v1alpha1types.App](dwF.Direwolf().V1alpha1().Apps().Informer()),
		generic.NewInformer[*v1alpha1types.User](dwF.Direwolf().V1alpha1().Users().Informer()),
		generic.NewInformer[*appsv1.Deployment](kF.Apps().V1().Deployments().Informer()),
		SessionControllerOptions{SessionPortRange: PortRange{Min: 20000, Max: 20999}})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	dwF.Start(stop)
	kF.Start(stop)
	if !cacheWaitForSync(kF, dwF, 5*time.Second) {
		t.Fatal("informers failed to sync")
	}
	return &portsFixture{dw: dw, sc: sc, app: app.Name}
}

func (f *portsFixture) session(name, user string) *v1alpha1types.Session {
	return &v1alpha1types.Session{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: portsTestNS},
		Spec: v1alpha1types.SessionSpec{
			UserReference: v1alpha1types.UserReference{Name: user},
			GameReference: v1alpha1types.GameReference{Name: f.app},
			Config:        v1alpha1types.SessionInfo{AESKey: "k", AESIV: "i"},
		},
	}
}

// waitInformer waits until the session informer does (or no longer) holds name.
func (f *portsFixture) waitInformer(t *testing.T, name string, present bool) {
	t.Helper()
	for range 500 {
		_, err := f.sc.SessionInformer.Namespaced(portsTestNS).Get(name)
		if (err == nil) == present {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("informer never saw %s present=%v", name, present)
}

// Deleting the last Session of a Deployment frees its block for the next one.
func TestSessionDeletionReleasesPorts(t *testing.T) {
	ctx := context.Background()
	f := newPortsFixture(t)

	alex := f.session("alex-1", "alex")
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(ctx, alex, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, alex.Name, true)
	if err := f.sc.allocatePorts(ctx, alex); err != nil {
		t.Fatal(err)
	}
	if alex.Status.Ports != blockPorts(20000) {
		t.Fatalf("alex got %+v, want the first block", alex.Status.Ports)
	}

	if err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Delete(ctx, alex.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.waitInformer(t, alex.Name, false)
	if err := f.sc.Reconcile(portsTestNS, alex.Name, nil); err != nil {
		t.Fatal(err)
	}

	sam := f.session("sam-1", "sam")
	if err := f.sc.allocatePorts(ctx, sam); err != nil {
		t.Fatal(err)
	}
	if sam.Status.Ports != blockPorts(20000) {
		t.Fatalf("sam got %+v, want alex's released block", sam.Status.Ports)
	}
}

// Blocks recorded in status before an operator restart are not handed out again.
func TestClaimRecordedPortsOnStart(t *testing.T) {
	f := newPortsFixture(t)
	running := f.session("alex-1", "alex")
	running.Status.Ports = blockPorts(20000)

	f.sc.claimRecordedPorts([]*v1alpha1types.Session{running})

	sam := f.session("sam-1", "sam")
	if err := f.sc.allocatePorts(context.Background(), sam); err != nil {
		t.Fatal(err)
	}
	if sam.Status.Ports != blockPorts(20000+sessionPortBlockSize) {
		t.Fatalf("sam got %+v, which a running pod still holds", sam.Status.Ports)
	}
}

// The operator dials wolf-agent on the block's agent port and advertises the
// block's RTSP port on the pod (= node) IP.
func TestReconcileActiveStreamsUsesPortBlock(t *testing.T) {
	var agentHits int
	agent := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentHits++
		switch r.URL.Path {
		case "/api/v1/sessions":
			fmt.Fprint(w, `{"success":true,"sessions":[]}`)
		case "/api/v1/sessions/add":
			fmt.Fprint(w, `{"success":true,"session_id":"4242"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()
	addr, ok := agent.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", agent.Listener.Addr())
	}
	agentPort := int32(addr.Port) //nolint:gosec // a TCP port fits in int32
	base := agentPort - portOffsetWolfAgent

	selector := map[string]string{"direwolf/user": "alex"}
	f := newPortsFixture(t,
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "alex-steam", Namespace: portsTestNS, Generation: 1},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: selector}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "alex-steam-pod", Namespace: portsTestNS, Labels: selector},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning, PodIP: "127.0.0.1",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: agentTokenSecretName("alex-steam"), Namespace: portsTestNS},
			Data:       map[string][]byte{wolfAgentTokenKey: []byte("token")},
		},
	)
	sess := f.session("alex-1", "alex")
	if f.app != "steam" {
		t.Fatalf("example app is %q; fixture Deployment assumes steam", f.app)
	}
	sess.Status.Ports = blockPorts(base)

	if err := f.sc.reconcileActiveStreams(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if agentHits == 0 {
		t.Error("wolf-agent on the block's agent port was never called")
	}
	if want := fmt.Sprintf("rtsp://127.0.0.1:%d", base+portOffsetRTSP); sess.Status.StreamURL != want {
		t.Errorf("StreamURL = %q, want %q", sess.Status.StreamURL, want)
	}
	if sess.Status.WolfSessionID != "4242" {
		t.Errorf("WolfSessionID = %q", sess.Status.WolfSessionID)
	}
}
