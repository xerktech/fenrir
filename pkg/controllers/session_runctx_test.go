package controllers

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Cancelling Run's ctx (shutdown, leader loss) cancels a wolf-agent poll in
// flight, rather than leaving it to wolfAgentTimeout.
func TestRunCtxCancelsAgentPoll(t *testing.T) {
	polling := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	agent := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		select {
		case polling <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-time.After(2 * wolfAgentTimeout):
		}
	}))
	// Run claims recorded port blocks, so the agent must listen inside the
	// fixture's range (20000-20999): take the first free block.
	var ln net.Listener
	for base := 20000; ln == nil && base+sessionPortBlockSize <= 21000; base += sessionPortBlockSize {
		ln, _ = (&net.ListenConfig{}).Listen(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", base+portOffsetWolfAgent))
	}
	if ln == nil {
		t.Fatal("no free port block in 20000-20999")
	}
	agent.Listener.Close()
	agent.Listener = ln
	agent.TLS = &tls.Config{Certificates: []tls.Certificate{testAgentKeyPair(t)}}
	agent.StartTLS()
	t.Cleanup(agent.Close)
	fa := &fakeAgent{Server: agent}

	sess := newPortsFixture(t).session("alex-1", "alex")
	sess.Status.Ports = blockPorts(fa.base(t))
	sess.CreationTimestamp = metav1.Now() // else the unstarted-session reaper ends it
	pod := readyPod(sess)
	pod.Labels = map[string]string{"direwolf/session": "true"}
	f := newPortsFixture(t, tokenSecret(sess), pod)
	if _, err := f.dw.DirewolfV1alpha1().Sessions(portsTestNS).Create(t.Context(), sess, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.sc.Run(ctx) }()
	select {
	case <-polling:
	case <-time.After(wolfAgentTimeout):
		cancel()
		t.Fatal("the agent was never polled")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(wolfAgentTimeout / 2):
		t.Fatal("cancelling Run's ctx did not cancel the in-flight agent poll")
	}
	select {
	case <-done:
	case <-time.After(wolfAgentTimeout):
		t.Error("Run did not return")
	}
}
