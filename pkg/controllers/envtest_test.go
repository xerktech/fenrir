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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// envtestClient starts etcd and kube-apiserver from KUBEBUILDER_ASSETS (as
// setup-envtest prints it; CI's test workflow sets it) and returns a
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

// The chart's floor apiserver (CI envtest runs it) keeps the fields that
// confine the split App pod and native VR pods, and a GPU claim's memory
// share. Older apiservers drop them silently (XERK-2755): below 1.33 the pod
// runs in the host user namespace, below 1.36 the claim takes the whole card.
func TestFloorKeepsSentFieldsEnvtest(t *testing.T) {
	client := envtestClient(t)
	ctx := context.Background()
	unmasked := corev1.UnmaskedProcMount
	created, err := client.CoreV1().Pods("default").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "confined"},
		Spec: corev1.PodSpec{
			HostUsers: new(false),
			Containers: []corev1.Container{{
				Name: "app", Image: "example.invalid/app",
				SecurityContext: &corev1.SecurityContext{ProcMount: &unmasked},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Spec.HostUsers == nil || *created.Spec.HostUsers {
		t.Errorf("stored hostUsers = %v, want false", created.Spec.HostUsers)
	}
	if sc := created.Spec.Containers[0].SecurityContext; sc == nil || sc.ProcMount == nil || *sc.ProcMount != unmasked {
		t.Errorf("stored procMount dropped: %+v", sc)
	}

	memory := resource.MustParse("4Gi")
	app := &v1alpha1types.App{Spec: v1alpha1types.AppSpec{GPU: &v1alpha1types.AppGPU{Memory: &memory}}}
	session := &v1alpha1types.Session{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", UID: "uid"}}
	claim, err := client.ResourceV1().ResourceClaims("default").Create(ctx, buildGPUClaim(app, session), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored := claim.Spec.Devices.Requests[0].Exactly
	if stored == nil || stored.Capacity == nil || !stored.Capacity.Requests[gpuClaimMemoryCapacity].Equal(memory) {
		t.Errorf("stored GPU claim lost its memory request: %+v", stored)
	}
}
