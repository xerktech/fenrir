package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"

	"games-on-whales.github.io/direwolf/pkg/controllers"
)

// The status needs the token; readiness follows WiVRn answering on the bus.
func TestVRAgentHandler(t *testing.T) {
	var fail bool
	h := vrAgentHandler(func() string { return "secret" }, func(context.Context) (controllers.VRStatus, error) {
		if fail {
			return controllers.VRStatus{}, errors.New("no bus at /run/x")
		}
		return controllers.VRStatus{HeadsetConnected: true}, nil
	})
	get := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tok := range []string{"", "wrong"} {
		if w := get(controllers.VRStatusPath, tok); w.Code != http.StatusUnauthorized {
			t.Errorf("token %q: %d, want 401", tok, w.Code)
		}
	}
	w := get(controllers.VRStatusPath, "secret")
	var st controllers.VRStatus
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || !st.HeadsetConnected {
		t.Errorf("status: %d %s", w.Code, w.Body)
	}
	if w := get("/readyz", ""); w.Code != http.StatusOK {
		t.Errorf("readyz %d", w.Code)
	}
	fail = true
	if w := get("/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with no WiVRn: %d", w.Code)
	}
	if w := get(controllers.VRStatusPath, "secret"); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "/run/x") {
		t.Errorf("status with no WiVRn: %d %q; want 503 without the bus error", w.Code, w.Body)
	}
	if w := get("/livez", ""); w.Code != http.StatusOK {
		t.Errorf("livez %d", w.Code)
	}
}

// wivrnStatus reads the real properties over a real session bus, from an
// object shaped like WiVRn's.
func TestWiVRnStatusOverDBus(t *testing.T) {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	dir, err := os.MkdirTemp("", "vrbus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// The image's native bus config, at a test path.
	conf, err := os.ReadFile("../../images/gow-nvidia32/vr/dbus-session-agent.conf")
	if err != nil {
		t.Fatal(err)
	}
	addr := "unix:path=" + filepath.Join(dir, "bus")
	if !strings.Contains(string(conf), "<listen>"+controllers.VRBusAddress+"</listen>") {
		t.Fatalf("the image's native bus config does not listen on %s", controllers.VRBusAddress)
	}
	confPath := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(confPath, []byte(strings.Replace(string(conf), controllers.VRBusAddress, addr, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), daemon, "--config-file="+confPath, "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatalf("dbus-daemon: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := wivrnStatus(ctx, addr); err == nil {
		t.Fatal("status read with no WiVRn on the bus")
	}

	server, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	props, err := prop.Export(server, wivrnPath, prop.Map{wivrnInterface: {
		"HeadsetConnected": {Value: true, Emit: prop.EmitTrue},
		"SessionRunning":   {Value: false, Emit: prop.EmitTrue},
		"Pin":              {Value: "123456", Emit: prop.EmitTrue},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := server.RequestName(wivrnService, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v %v", reply, err)
	}
	st, err := wivrnStatus(ctx, addr)
	if err != nil || st != (controllers.VRStatus{HeadsetConnected: true}) {
		t.Fatalf("wivrnStatus = %+v, %v", st, err)
	}
	props.SetMust(wivrnInterface, "SessionRunning", true)
	if st, err := wivrnStatus(ctx, addr); err != nil || !st.SessionRunning {
		t.Fatalf("after SessionRunning: %+v, %v", st, err)
	}
}
