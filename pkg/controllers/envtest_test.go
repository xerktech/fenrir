package controllers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// envtestClient starts etcd and kube-apiserver from KUBEBUILDER_ASSETS (as
// setup-envtest prints it; CI's crd-floor workflow sets it) and returns a
// client of it. The fake clientset ignores preconditions such as a patch's
// resourceVersion; tests of those need a real API server. Skipped without
// the assets.
func envtestClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	bin := os.Getenv("KUBEBUILDER_ASSETS")
	if bin == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run under setup-envtest")
	}
	dir := t.TempDir()
	port := func() string {
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return strconv.Itoa(l.Addr().(*net.TCPAddr).Port) //nolint:forcetypeassert // a TCP listener
	}
	start := func(name string, args ...string) {
		cmd := exec.CommandContext(context.WithoutCancel(t.Context()), filepath.Join(bin, name), args...) //nolint:gosec // test binaries from setup-envtest
		log, err := os.Create(filepath.Join(dir, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if t.Failed() {
				if out, err := os.ReadFile(log.Name()); err == nil {
					t.Logf("%s log:\n%s", name, out)
				}
			}
		})
	}
	etcd, peer, api := port(), port(), port()
	start("etcd", "--data-dir", filepath.Join(dir, "etcd"),
		"--listen-client-urls", "http://127.0.0.1:"+etcd, "--advertise-client-urls", "http://127.0.0.1:"+etcd,
		"--listen-peer-urls", "http://127.0.0.1:"+peer)

	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(filepath.Join(dir, "tokens.csv"), []byte(token+",admin,admin,system:masters\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The service-account key: kube-apiserver insists on one. Its own
	// self-signed serving cert is fine, as the client skips verification.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	saKey := filepath.Join(dir, "sa.key")
	if err = os.WriteFile(saKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	start("kube-apiserver", "--etcd-servers", "http://127.0.0.1:"+etcd,
		"--bind-address", "127.0.0.1", "--secure-port", api, "--cert-dir", filepath.Join(dir, "certs"),
		"--token-auth-file", filepath.Join(dir, "tokens.csv"), "--authorization-mode", "AlwaysAllow",
		"--service-account-issuer", "https://kubernetes.default.svc",
		"--service-account-key-file", saKey, "--service-account-signing-key-file", saKey,
		"--service-cluster-ip-range", "10.0.0.0/24")

	client, err := kubernetes.NewForConfig(&rest.Config{
		Host:            "https://127.0.0.1:" + api,
		BearerToken:     token,
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		// Ready, and the default namespace's ServiceAccount exists: pod
		// admission refuses pods without it, and no controller-manager
		// runs here to make it.
		_, err := client.CoreV1().ServiceAccounts("default").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, metav1.CreateOptions{})
		if err == nil || apierrors.IsAlreadyExists(err) {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("kube-apiserver never became ready: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// annotateHeadsets' resourceVersion precondition holds on a real API server:
// a stale pod (another poll or kubelet wrote since it was read) fails the
// patch with a conflict and leaves the live record alone (XERK-2747).
func TestAnnotateHeadsetsConflictsOnStalePodEnvtest(t *testing.T) {
	client := envtestClient(t)
	ctx := context.Background()
	pods := client.CoreV1().Pods("default")
	stale, err := pods.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "vr", Annotations: map[string]string{vrHeadsetsAnnotation: ""}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	moved := stale.DeepCopy()
	moved.Annotations[vrHeadsetsAnnotation] = "other/other-uid"
	if _, err = pods.Update(ctx, moved, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	sc := &SessionController{K8sClient: client}
	if err = sc.annotateHeadsets(ctx, stale, "h/u"); !apierrors.IsConflict(err) {
		t.Fatalf("patch from a stale pod: %v, want a conflict", err)
	}
	live, err := pods.Get(ctx, "vr", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := live.Annotations[vrHeadsetsAnnotation]; got != "other/other-uid" {
		t.Fatalf("live %s = %q after a refused patch", vrHeadsetsAnnotation, got)
	}
	if err := sc.annotateHeadsets(ctx, live, "h/u"); err != nil {
		t.Fatalf("patch from the current pod: %v", err)
	}
}
