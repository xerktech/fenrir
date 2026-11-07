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
	"reflect"
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
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
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
	daemon, lookErr := exec.LookPath("dbus-daemon")
	if lookErr != nil {
		t.Skip("no dbus-daemon")
	}
	// Short: a unix socket path has ~108 bytes.
	dir, mkErr := os.MkdirTemp("", "vrbus") //nolint:usetesting // t.TempDir's path is too long for a socket
	if mkErr != nil {
		t.Fatal(mkErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// The image's agent bus config, at a test path.
	conf, readErr := os.ReadFile("../../images/gow-nvidia32/vr/dbus-session-agent.conf")
	if readErr != nil {
		t.Fatal(readErr)
	}
	addr := "unix:path=" + filepath.Join(dir, "bus")
	if !strings.Contains(string(conf), "<listen>"+controllers.VRBusAddress+"</listen>") {
		t.Fatalf("the image's native bus config does not listen on %s", controllers.VRBusAddress)
	}
	confPath := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(confPath, []byte(strings.Replace(string(conf), controllers.VRBusAddress, addr, 1)), 0o600); err != nil { //nolint:gosec // the test's own temp dir
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), daemon, "--config-file="+confPath, "--nofork", "--print-address")
	out, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
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
		"KnownKeys": {Value: []struct {
			Name, Key string
			Last      int64
		}{}, Emit: prop.EmitTrue},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if reply, nameErr := server.RequestName(wivrnService, dbus.NameFlagDoNotQueue); nameErr != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v %v", reply, nameErr)
	}
	st, err := wivrnStatus(ctx, addr)
	if err != nil || !reflect.DeepEqual(st, controllers.VRStatus{HeadsetConnected: true}) {
		t.Fatalf("wivrnStatus = %+v, %v", st, err)
	}
	// WiVRn's KnownKeys, a(ssx) name, key, last connection, reach the operator.
	type knownKey struct {
		Name, Key string
		Last      int64
	}
	props.SetMust(wivrnInterface, "KnownKeys", []knownKey{{"Quest 3", "MEIw", 1700000000}, {"Other", "AAAA", 0}})
	want := []controllers.VRKnownKey{{Name: "Quest 3", Key: "MEIw"}, {Name: "Other", Key: "AAAA"}}
	if st, err := wivrnStatus(ctx, addr); err != nil || !reflect.DeepEqual(st.KnownKeys, want) {
		t.Fatalf("KnownKeys: %+v, %v; want %+v", st.KnownKeys, err, want)
	}
	props.SetMust(wivrnInterface, "SessionRunning", true)
	if st, err := wivrnStatus(ctx, addr); err != nil || !st.SessionRunning {
		t.Fatalf("after SessionRunning: %+v, %v", st, err)
	}
}
