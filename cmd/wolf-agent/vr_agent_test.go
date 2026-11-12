package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

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
	if err != nil || !reflect.DeepEqual(st, controllers.VRStatus{HeadsetConnected: true, Pin: "123456"}) {
		t.Fatalf("wivrnStatus = %+v, %v", st, err)
	}
	// Only six digits reach the VR page (XERK-2670): the game owns the bus.
	for _, bad := range []string{"", "12345", "1234567", "12345a", "<b>12"} {
		props.SetMust(wivrnInterface, "Pin", bad)
		if got, readErr := wivrnStatus(ctx, addr); readErr != nil || got.Pin != "" {
			t.Fatalf("Pin %q: reported %q, %v", bad, got.Pin, readErr)
		}
	}
	// WiVRn's KnownKeys, a(ssx) name, key, last connection, reach the operator.
	type knownKey struct {
		Name, Key string
		Last      int64
	}
	props.SetMust(wivrnInterface, "KnownKeys", []knownKey{{"Quest 3", "MEIw", 1700000000}, {"Other", "AAAA", 0}})
	want := []controllers.VRKnownKey{{Name: "Quest 3", Key: "MEIw"}, {Name: "Other", Key: "AAAA"}}
	st, err = wivrnStatus(ctx, addr)
	if err != nil || !reflect.DeepEqual(st.KnownKeys, want) {
		t.Fatalf("KnownKeys: %+v, %v; want %+v", st.KnownKeys, err, want)
	}
	// A flood (the game shares WiVRn's keys file) is cut to maxReportedKeys;
	// a key over 1024 bytes is dropped and a long name cut. One set, larger
	// than the last: godbus's SetMust writes into a same-length old slice
	// that its exporter may still be reading (a race in the test only).
	flood := make([]knownKey, 0, maxReportedKeys+12)
	flood = append(flood, knownKey{"big", strings.Repeat("A", 2000), 0}, knownKey{"<", strings.Repeat("<", 1024), 0},
		knownKey{"a" + strings.Repeat("é", 10000), "AAAA", 0})
	for range maxReportedKeys + 10 {
		flood = append(flood, knownKey{"k", "AAAA", 0})
	}
	props.SetMust(wivrnInterface, "KnownKeys", flood)
	st, err = wivrnStatus(ctx, addr)
	if err != nil || len(st.KnownKeys) != maxReportedKeys {
		t.Fatalf("flooded KnownKeys: %d, %v; want %d", len(st.KnownKeys), err, maxReportedKeys)
	}
	// Cut to the CRD's 128 characters, not bytes (XERK-2747).
	if first := st.KnownKeys[0]; utf8.RuneCountInString(first.Name) != 128 || !utf8.ValidString(first.Name) || !strings.HasPrefix(first.Name, "aé") {
		t.Errorf("first reported key %.40q... (%d characters): want the long name cut to 128 characters, the bad keys dropped",
			first.Name, utf8.RuneCountInString(first.Name))
	}
	props.SetMust(wivrnInterface, "SessionRunning", true)
	if st, err := wivrnStatus(ctx, addr); err != nil || !st.SessionRunning {
		t.Fatalf("after SessionRunning: %+v, %v", st, err)
	}
}

// The worst reply vr-agent can send (maxReportedKeys entries of the longest
// key and the longest name it keeps: 128 four-byte runes, or 128 escaped
// ones) fits the operator's 64KiB read: past it the poll fails, which reads
// as a lost headset (QA, XERK-2669).
func TestVRAgentWorstReplyFitsOperatorRead(t *testing.T) {
	key := strings.Repeat("A", 1024)
	st := controllers.VRStatus{HeadsetConnected: true, ServerKey: strings.Repeat("ab", 56), Pin: "123456"}
	for i := range maxReportedKeys {
		name := controllers.CleanHeadsetName(strings.Repeat(`"\<>&`, 100))
		if i%2 == 0 {
			name = controllers.CleanHeadsetName(strings.Repeat("\U0001F600", 200))
		}
		st.KnownKeys = append(st.KnownKeys, controllers.VRKnownKey{Name: name, Key: key})
	}
	h := vrAgentHandler(func() string { return "secret" }, func(context.Context) (controllers.VRStatus, error) { return st, nil })
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, controllers.VRStatusPath, http.NoBody)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if n := w.Body.Len(); w.Code != http.StatusOK || n > 64<<10 {
		t.Fatalf("worst reply: %d, %d bytes; want 200 within 64KiB", w.Code, n)
	}
}

// vr-agent reports the server key the App published, and nothing for a file
// that is missing, not one X448 public key, oversized, or not a regular file
// (a FIFO must not stall the read).
func TestReadServerKey(t *testing.T) {
	dir := t.TempDir()
	der := append([]byte{0x30, 0x42, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6f, 0x03, 0x39, 0x00}, bytes.Repeat([]byte{7}, 56)...)
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readServerKey(good); got != strings.Repeat("07", 56) {
		t.Errorf("good key read as %q", got)
	}
	garbage := filepath.Join(dir, "garbage")
	_ = os.WriteFile(garbage, []byte("hello"), 0o600)
	big := filepath.Join(dir, "big")
	_ = os.WriteFile(big, bytes.Repeat([]byte("A"), maxServerKeyFile+1), 0o600)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range []string{filepath.Join(dir, "missing"), garbage, big, fifo, dir} {
			if got := readServerKey(p); got != "" {
				t.Errorf("%s read as %q", p, got)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}
