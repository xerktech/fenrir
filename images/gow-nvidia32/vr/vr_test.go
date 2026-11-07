//go:build unix

// Package vr holds tests for the vr session image's scripts
// (images/gow-nvidia32 target vr; .claude/rules/vr.md).
package vr

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shortTempDir is a temporary directory short enough for unix sockets in it
// (108 bytes), which t.TempDir's test-named paths may not be.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vr") //nolint:usetesting // see above
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// writeExec writes an executable stub script.
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
}

type vrStart struct {
	dir, bin, home, state, runtime, steamLog string
}

// newVRStart lays the scripts out as the image does (siblings in one dir,
// with GoW's startup-app as steam-startup-app.sh) and stubs what the session
// provides: mountpoint (every VR config dir mounted) and dbus-run-session. The
// default vr-services stub brings up the pulse socket and active runtime the
// real services would, then stays up.
func newVRStart(t *testing.T) *vrStart {
	t.Helper()
	s := &vrStart{dir: t.TempDir(), bin: t.TempDir(), home: t.TempDir(), state: t.TempDir()}
	s.runtime = shortTempDir(t)
	s.steamLog = filepath.Join(s.dir, "steam.log")
	for _, f := range []string{"startup-app.sh", "dbus-session.conf", "dbus-session-agent.conf"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, f), b, 0o755); err != nil { //nolint:gosec // the image's scripts are executable
			t.Fatal(err)
		}
	}
	for _, d := range []string{"wivrn", "openxr", "openvr"} {
		if err := os.MkdirAll(filepath.Join(s.state, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(s.bin, "mountpoint"), "exit 0\n")
	writeExec(t, filepath.Join(s.bin, "dbus-run-session"),
		"echo \"$1\" >\"$DBUS_LOG\"\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
	s.services("exec sleep 30\n")
	s.steam("exit 0\n")
	return s
}

// services replaces vr-services with a stub that sets up the pulse socket and
// active runtime, then runs tail.
func (s *vrStart) services(tail string) {
	writeExecNoT(filepath.Join(s.dir, "vr-services"), `echo "$1 $2" >"$SERVICES_LOG"
mkdir -p "$2/pulse" "$HOME/.config/openxr/1"
python3 -I -c 'import socket,sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$2/pulse/native"
echo '{}' >"$HOME/.config/openxr/1/active_runtime.json"
`+tail)
}

// steam replaces GoW's Steam startup with a stub that logs its environment.
func (s *vrStart) steam(tail string) {
	writeExecNoT(filepath.Join(s.dir, "steam-startup-app.sh"),
		`printf 'PULSE_SERVER=%s\nPRESSURE_VESSEL_IMPORT_OPENXR_1_RUNTIMES=%s\nPRESSURE_VESSEL_FILESYSTEMS_RO=%s\nARGS=%s\n' "$PULSE_SERVER" "$PRESSURE_VESSEL_IMPORT_OPENXR_1_RUNTIMES" "$PRESSURE_VESSEL_FILESYSTEMS_RO" "$*" >"`+s.steamLog+"\"\n"+tail)
}

func writeExecNoT(path, body string) {
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		panic(err)
	}
}

func (s *vrStart) run(t *testing.T, env ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(s.dir, "startup-app.sh"), "-bigpicture") //nolint:gosec // the test's own copy
	cmd.Env = append([]string{
		"PATH=" + s.bin + ":" + os.Getenv("PATH"),
		"HOME=" + s.home,
		"DIREWOLF_STATE_ROOT=" + s.state,
		"XDG_RUNTIME_DIR=" + s.runtime,
		"DBUS_LOG=" + filepath.Join(s.dir, "dbus.log"),
		"SERVICES_LOG=" + filepath.Join(s.dir, "services.log"),
		"DIREWOLF_VR_START_TIMEOUT=5",
		"DIREWOLF_VR_HEADSETS=[]",
	}, env...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestVRStartRefusesWithoutTheSessionsPort(t *testing.T) {
	for _, port := range []string{"", "0", "65536", "99999", "9757x", " 9757", "-1", "0x10"} {
		s := newVRStart(t)
		env := []string{}
		if port != "" {
			env = append(env, "DIREWOLF_VR_PORT="+port)
		}
		out, err := s.run(t, env...)
		if err == nil || !strings.Contains(out, "DIREWOLF_VR_PORT") {
			t.Errorf("port %q: err %v, output %q; want a refusal naming DIREWOLF_VR_PORT", port, err, out)
		}
		if _, err := os.Stat(s.steamLog); err == nil {
			t.Errorf("port %q: Steam started", port)
		}
		if _, err := os.Stat(filepath.Join(s.home, ".config", "wivrn", "config.json")); err == nil {
			t.Errorf("port %q: wrote a WiVRn config", port)
		}
	}
}

// The pairing keys and the runtime files WiVRn rewrites must never land in
// the shared Steam home: each state dir must be the operator's mount.
func TestVRStartRefusesUnmountedStateDirs(t *testing.T) {
	for _, unmounted := range []string{"wivrn", "openxr", "openvr"} {
		s := newVRStart(t)
		writeExec(t, filepath.Join(s.bin, "mountpoint"),
			"case \"$2\" in */"+unmounted+") exit 1;; esac\nexit 0\n")
		out, err := s.run(t, "DIREWOLF_VR_PORT=20034")
		if err == nil || !strings.Contains(out, unmounted+" must be the per-user volume the operator mounts") {
			t.Errorf("%s unmounted: err %v, output %q; want a refusal", unmounted, err, out)
		}
		if _, err := os.Stat(s.steamLog); err == nil {
			t.Errorf("%s unmounted: Steam started", unmounted)
		}
	}
}

func TestVRStartRefusesUnknownOpenVRLayer(t *testing.T) {
	s := newVRStart(t)
	out, err := s.run(t, "DIREWOLF_VR_PORT=20034", "DIREWOLF_VR_OPENVR=/tmp/evil")
	if err == nil || !strings.Contains(out, "DIREWOLF_VR_OPENVR") {
		t.Fatalf("err %v, output %q; want a refusal", err, out)
	}
}

func TestVRStartConfiguresWiVRnAndStartsSteam(t *testing.T) {
	for _, tc := range []struct{ layer, path string }{
		{"", "/opt/xrizer"},
		{"xrizer", "/opt/xrizer"},
		{"opencomposite", "/opt/opencomposite"},
	} {
		s := newVRStart(t)
		env := []string{"DIREWOLF_VR_PORT=20034", "PRESSURE_VESSEL_FILESYSTEMS_RO=/games"}
		if tc.layer != "" {
			env = append(env, "DIREWOLF_VR_OPENVR="+tc.layer)
		}
		out, err := s.run(t, env...)
		if err != nil {
			t.Fatalf("%q: %v\n%s", tc.layer, err, out)
		}
		b, err := os.ReadFile(s.steamLog)
		if err != nil {
			t.Fatalf("%q: Steam did not start: %v\n%s", tc.layer, err, out)
		}
		audio := filepath.Join(s.runtime, "vr-audio")
		want := "PULSE_SERVER=unix:" + audio + "/pulse/native\n" +
			"PRESSURE_VESSEL_IMPORT_OPENXR_1_RUNTIMES=1\n" +
			"PRESSURE_VESSEL_FILESYSTEMS_RO=" + tc.path + ":/games\n" +
			"ARGS=-bigpicture\n"
		if string(b) != want {
			t.Errorf("%q: Steam's environment:\n%s\nwant:\n%s", tc.layer, b, want)
		}
		if b, _ := os.ReadFile(filepath.Join(s.dir, "services.log")); string(b) != "20034 "+audio+"\n" {
			t.Errorf("%q: vr-services got %q", tc.layer, b)
		}
		if b, _ := os.ReadFile(filepath.Join(s.dir, "dbus.log")); string(b) != "--config-file="+filepath.Join(s.dir, "dbus-session.conf")+"\n" {
			t.Errorf("%q: dbus-run-session got %q; want the image's bus config", tc.layer, b)
		}
		if info, err := os.Stat(audio); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%q: audio dir %v %v; want 0700", tc.layer, info, err)
		}
		cfg := readConfig(t, filepath.Join(s.home, ".config", "wivrn", "config.json"))
		if cfg["port"] != float64(20034) || cfg["openvr-compat-path"] != tc.path {
			t.Errorf("%q: config %v", tc.layer, cfg)
		}
	}
}

// A session whose VR services die is useless: it ends, with Steam still up.
func TestVRStartEndsTheSessionWhenTheServicesExit(t *testing.T) {
	s := newVRStart(t)
	s.services("sleep 1\nexit 0\n")
	s.steam("exec sleep 30\n")
	start := time.Now()
	out, err := s.run(t, "DIREWOLF_VR_PORT=20034")
	if err == nil || !strings.Contains(out, "VR services exited") {
		t.Fatalf("err %v, output %q; want the session to end", err, out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v: it waited for Steam", d)
	}
}

func TestVRStartRefusesWhenTheServicesNeverComeUp(t *testing.T) {
	s := newVRStart(t)
	writeExec(t, filepath.Join(s.dir, "vr-services"), "exit 1\n")
	out, err := s.run(t, "DIREWOLF_VR_PORT=20034")
	if err == nil || !strings.Contains(out, "VR services exited before") {
		t.Fatalf("err %v, output %q", err, out)
	}
	if _, err := os.Stat(s.steamLog); err == nil {
		t.Fatal("Steam started")
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return cfg
}

// WiVRn reads its keys in one try block, so a user key of the wrong type read
// before "port" would leave it on 9757: the config is rewritten whole.
func TestVRStartRewritesWiVRnConfigWhole(t *testing.T) {
	s := newVRStart(t)
	path := filepath.Join(s.state, "wivrn", "config.json")
	if err := os.WriteFile(path, []byte(`{"port": 9757, "publish-service": "avahi", "hid-forwarding": true,
		"application": ["steam"], "bit-depth": "ten", "tcp-only": 1e400}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := s.run(t, "DIREWOLF_VR_PORT=20034", "DIREWOLF_VR_OPENVR=opencomposite"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := map[string]any{
		"port": float64(20034), "publish-service": "none", "hid-forwarding": false,
		"openvr-compat-path": "/opt/opencomposite", "encoder": "nvenc",
	}
	got := readConfig(t, path)
	if len(got) != len(want) {
		t.Errorf("config %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// mountpoint follows links, so a link into the shared home would pass it; and
// a read-only volume loses the pairing keys ("Failed to save keys").
func TestVRStartRefusesLinkedOrReadOnlyConfigDirs(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"link", "is a symlink"},
		{"readonly", "is not writable"},
	} {
		if tc.name == "readonly" && os.Geteuid() == 0 {
			t.Log("skipping readonly: root writes a 0555 directory anyway")
			continue
		}
		s := newVRStart(t)
		dir := filepath.Join(s.state, "openvr")
		if tc.name == "link" {
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(s.home, dir); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		out, err := s.run(t, "DIREWOLF_VR_PORT=20034")
		if err == nil || !strings.Contains(out, tc.want) {
			t.Errorf("%s: err %v, output %q; want %q", tc.name, err, out, tc.want)
		}
		if _, err := os.Stat(s.steamLog); err == nil {
			t.Errorf("%s: Steam started", tc.name)
		}
	}
}

// The session bus must be a file: unix:tmpdir may be an abstract socket, which
// lives in the network namespace. In native mode it is the operator's bus
// directory (pkg/controllers VRBusAddress), which vr-agent reads.
func TestVRSessionBusListensOnAFile(t *testing.T) {
	for conf, want := range map[string]string{
		"dbus-session.conf":       "unix:dir=/tmp",
		"dbus-session-agent.conf": "unix:path=/run/direwolf-vr-bus/bus",
	} {
		b, err := os.ReadFile(conf)
		if err != nil {
			t.Fatal(err)
		}
		listens := regexp.MustCompile(`<listen>([^<]*)</listen>`).FindAllStringSubmatch(string(b), -1)
		if len(listens) != 1 || listens[0][1] != want {
			t.Errorf("%s listens on %v, want only %s", conf, listens, want)
		}
		if strings.Contains(string(b), "<include") {
			t.Errorf("%s includes another config, which may add listeners", conf)
		}
	}
}

// Native VR mode (no Wolf): sway runs headless, not nested in a display that
// isn't there, and the bus listens in the operator's bus directory, where a
// stale socket is cleared first.
func TestVRStartNativeMode(t *testing.T) {
	s := newVRStart(t)
	envLog := filepath.Join(s.dir, "env.log")
	s.steam(`printf 'WLR_BACKENDS=%s WAYLAND_DISPLAY=%s\n' "$WLR_BACKENDS" "${WAYLAND_DISPLAY-unset}" >"` + envLog + "\"\n")
	bus := shortTempDir(t)
	if err := os.WriteFile(filepath.Join(bus, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := s.run(t, "DIREWOLF_VR_PORT=20034", "DIREWOLF_VR_MODE=native", "DIREWOLF_VR_BUS_DIR="+bus, "WAYLAND_DISPLAY=wayland-1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(envLog); string(b) != "WLR_BACKENDS=headless WAYLAND_DISPLAY=unset\n" {
		t.Errorf("Steam's startup got %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(s.dir, "dbus.log")); string(b) != "--config-file="+filepath.Join(s.dir, "dbus-session-agent.conf")+"\n" {
		t.Errorf("dbus-run-session got %q; want the native bus config", b)
	}
	if _, err := os.Lstat(filepath.Join(bus, "bus")); !os.IsNotExist(err) {
		t.Errorf("stale bus socket kept: %v", err)
	}

	// A Moonlight session keeps Wolf's display, and its bus in /tmp unless
	// the operator mounts the bus directory.
	s = newVRStart(t)
	s.steam(`printf 'WLR_BACKENDS=%s WAYLAND_DISPLAY=%s\n' "${WLR_BACKENDS-unset}" "$WAYLAND_DISPLAY" >"` + envLog + "\"\n")
	if out, err := s.run(t, "DIREWOLF_VR_PORT=20034", "WAYLAND_DISPLAY=wayland-1"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(envLog); string(b) != "WLR_BACKENDS=unset WAYLAND_DISPLAY=wayland-1\n" {
		t.Errorf("Moonlight mode: Steam's startup got %q", b)
	}
	s = newVRStart(t)
	if out, err := s.run(t, "DIREWOLF_VR_PORT=20034", "DIREWOLF_VR_BUS_DIR="+bus); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(s.dir, "dbus.log")); string(b) != "--config-file="+filepath.Join(s.dir, "dbus-session-agent.conf")+"\n" {
		t.Errorf("Moonlight mode with the bus directory: dbus-run-session got %q", b)
	}
}

func TestVRStartRefusesBadNativeSetup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string][]string{
		"unknown mode":     {"DIREWOLF_VR_MODE=wolfless"},
		"no bus directory": {"DIREWOLF_VR_MODE=native", "DIREWOLF_VR_BUS_DIR=" + filepath.Join(t.TempDir(), "missing")},
		"bus is a file":    {"DIREWOLF_VR_MODE=native", "DIREWOLF_VR_BUS_DIR=" + file},
	} {
		s := newVRStart(t)
		out, err := s.run(t, append([]string{"DIREWOLF_VR_PORT=20034"}, env...)...)
		if err == nil || !strings.Contains(out, "direwolf-vr:") {
			t.Errorf("%s: started (%v)\n%s", name, err, out)
		}
		if _, err := os.Stat(s.steamLog); err == nil {
			t.Errorf("%s: Steam started", name)
		}
	}
}

// vrServices runs the real vr-services with stub daemons and returns its log
// file. ss reports the port as listening while $SERVING exists.
func vrServices(t *testing.T, serving string) (logPath, servingPath string) {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	audio := shortTempDir(t)
	logPath = filepath.Join(dir, "log")
	servingPath = filepath.Join(dir, "serving")
	if serving != "" {
		if err := os.WriteFile(servingPath, []byte(serving), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(bin, "pipewire"),
		`python3 -I -c 'import socket,sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$PIPEWIRE_RUNTIME_DIR/pipewire-0"
echo "pipewire $XDG_RUNTIME_DIR $XDG_STATE_HOME" >>"$LOG"
exec sleep 60
`)
	writeExec(t, filepath.Join(bin, "wireplumber"), "exec sleep 60\n")
	writeExec(t, filepath.Join(bin, "pipewire-pulse"), "exec sleep 60\n")
	// "listening" answers only the -l query, "established" only the state one.
	writeExec(t, filepath.Join(bin, "ss"), `want=listening
case "$*" in *established*) want=established;; esac
[ "$(cat "$SERVING" 2>/dev/null)" = "$want" ] && echo "0 0 *:20034 *:*"
exit 0
`)
	writeExec(t, filepath.Join(bin, "wivrn-server"), `echo "start $PIPEWIRE_RUNTIME_DIR $*" >>"$LOG"
trap 'echo term >>"$LOG"; exit 0' TERM
while :; do sleep 0.05; done
`)
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.CommandContext(t.Context(), "bash", "vr-services", "20034", audio)
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=/home/retro", "LOG=" + logPath, "SERVING=" + servingPath,
		"DIREWOLF_VR_CHECK_INTERVAL=0.1", "DIREWOLF_VR_CHECK_MISSES=3",
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return logPath, servingPath
}

func waitForLog(t *testing.T, path string, re *regexp.Regexp, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		b, _ := os.ReadFile(path)
		if re.Match(b) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %v in the log after %v:\n%s", re, d, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestVRServicesStartWiVRnWithPipeWire(t *testing.T) {
	logPath, _ := vrServices(t, "listening")
	b := waitForLog(t, logPath, regexp.MustCompile(`start .*\n`), 5*time.Second)
	m := regexp.MustCompile(`pipewire (\S+) (\S+)\nstart (\S+) (.*)\n`).FindStringSubmatch(b)
	if m == nil {
		t.Fatalf("log:\n%s", b)
	}
	if m[2] != m[1]+"/state" || m[3] != m[1] {
		t.Errorf("PipeWire in %s (state %s), wivrn-server's PIPEWIRE_RUNTIME_DIR %s", m[1], m[2], m[3])
	}
	if want := "-f /srv/direwolf/state/wivrn/config.json --early-active-runtime --no-publish-service --no-instructions"; m[4] != want {
		t.Errorf("wivrn-server %s, want %s", m[4], want)
	}
}

// A server that stops listening is restarted; one listening or holding a
// headset's connection (its listener closes meanwhile) is left alone.
func TestVRServicesRestartWiVRnOnlyWhenItStopsServing(t *testing.T) {
	for _, serving := range []string{"listening", "established"} {
		logPath, servingPath := vrServices(t, serving)
		waitForLog(t, logPath, regexp.MustCompile(`start `), 5*time.Second)
		time.Sleep(time.Second) // ten checks
		if b, _ := os.ReadFile(logPath); strings.Contains(string(b), "term") {
			t.Fatalf("%s: restarted a serving server:\n%s", serving, b)
		}
		if err := os.Remove(servingPath); err != nil {
			t.Fatal(err)
		}
		b := waitForLog(t, logPath, regexp.MustCompile(`(?s)term.*start `), 15*time.Second)
		if !strings.Contains(b, "not serving port 20034") {
			t.Errorf("%s: no restart reason logged:\n%s", serving, b)
		}
	}
}

func TestVRServicesExitWhenAnAudioDaemonDoes(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	audio := shortTempDir(t)
	writeExec(t, filepath.Join(bin, "pipewire"),
		`python3 -I -c 'import socket,sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$PIPEWIRE_RUNTIME_DIR/pipewire-0"
exec sleep 60
`)
	writeExec(t, filepath.Join(bin, "wireplumber"), "sleep 0.5; exit 2\n")
	writeExec(t, filepath.Join(bin, "pipewire-pulse"), "exec sleep 60\n")
	writeExec(t, filepath.Join(bin, "ss"), "exit 0\n")
	writeExec(t, filepath.Join(bin, "wivrn-server"), "exec sleep 60\n")
	out, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	cmd := exec.CommandContext(t.Context(), "bash", "vr-services", "20034", audio)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=/home/retro"}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("exited 0")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still running after wireplumber exited")
	}
}

// Nothing in the vr files may turn on what .claude/rules/vr.md forbids:
// unencrypted streams, file capabilities, uinput, Avahi, or a umask of its own
// (the steam stage's ENV UMASK=022 is inherited).
func TestVRFilesAvoidForbiddenSettings(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		// dbus-daemon's keep_umask keeps the inherited 022 rather than setting its own.
		lower := strings.ToLower(strings.ReplaceAll(string(b), "<keep_umask/>", ""))
		for _, bad := range []string{"--no-encrypt", "setcap", "/dev/uinput", "umask", "avahi-daemon"} {
			if strings.Contains(lower, bad) {
				t.Errorf("%s mentions %q", e.Name(), bad)
			}
		}
	}
}

// Every upstream input is pinned: WiVRn and OpenComposite by commit, xrizer
// by checksum, and the vr target builds in both image workflows.
func TestVRImageIsPinnedAndBuilt(t *testing.T) {
	b, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(b)
	for _, re := range []string{
		`\nFROM steam AS vr\n`,
		`\nARG WIVRN_REPO=https://github\.com/xerktech/WiVRn\n`,
		`\nARG WIVRN_COMMIT=[0-9a-f]{40}\n`,
		`test "\$\(git -C /src/wivrn rev-parse HEAD\)" = "\$\{WIVRN_COMMIT\}"`,
		`-DWIVRN_USE_PIPEWIRE=ON`,
		`\nARG OPENCOMPOSITE_COMMIT=[0-9a-f]{40}\n`,
		`test "\$\(git -C /src/oc rev-parse HEAD\)" = "\$\{OPENCOMPOSITE_COMMIT\}"`,
		`\nARG XRIZER_SHA256=[0-9a-f]{64}\n`,
		`ADD --checksum=sha256:\$\{XRIZER_SHA256\}`,
		`ln -s /opt/direwolf/vr/startup-app.sh /opt/gow/startup-app.sh`,
	} {
		if !regexp.MustCompile(re).MatchString(dockerfile) {
			t.Errorf("Dockerfile lacks %s", re)
		}
	}
	for _, wf := range []string{"../../../.github/workflows/build.yml", "../../../.github/workflows/builder.yml"} {
		b, err := os.ReadFile(wf)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "- app: vr\n            context: images/gow-nvidia32\n            target: vr\n") {
			t.Errorf("%s builds no vr target", wf)
		}
	}
}

// vrNotice runs vr-notice with stub swaymsg and pkill, feeding it the given
// wivrn-server stderr and keeping its input open (as wivrn-server does) until
// done(execs) holds or 5s pass. sway is "up" only if swayUp. Returns the
// swaymsg exec log and vr-notice's own stderr.
func vrNotice(t *testing.T, host, stderr string, swayUp bool, done func(execs string) bool) (execs, out string) {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	up := filepath.Join(dir, "up")
	if swayUp {
		if err := os.WriteFile(up, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	execLog := filepath.Join(dir, "execs")
	writeExec(t, filepath.Join(bin, "swaymsg"), `[ "$1" = -s ] || exit 2
[ -e "$UP" ] || exit 1
[ "$3" = -t ] && exit 0
shift 2
printf '%s\n' "$*" >>"$EXECS"
`)
	// Logged: the old bar must go before each new one.
	writeExec(t, filepath.Join(bin, "pkill"), "printf 'pkill %s\\n' \"$*\" >>\"$EXECS\"\nexit 1\n")
	cmd := exec.CommandContext(t.Context(), "bash", "vr-notice", "21000")
	cmd.Env = []string{
		"PATH=" + bin + ":" + os.Getenv("PATH"), "XDG_RUNTIME_DIR=" + dir, "UP=" + up, "EXECS=" + execLog,
		"DIREWOLF_VR_HOST=" + host, "DIREWOLF_VR_NOTICE_RETRY=0.05",
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(in, stderr); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(execLog); done(string(b)) {
			break
		}
	}
	_ = in.Close()
	if err := cmd.Wait(); err != nil {
		t.Errorf("vr-notice: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("vr-notice wrote %q to stdout", stdout.String())
	}
	b, _ := os.ReadFile(execLog)
	return string(b), errOut.String()
}

// The mirror shows the node's address and port, then WiVRn's PIN while it
// pairs, and drops the PIN once pairing is off. Every line still reaches stderr.
func TestVRNoticeShowsAddressAndPairingPIN(t *testing.T) {
	stderr := "WiVRn v26.9 starting\nTo pair a new headset use PIN code: 012345\n"
	if _, out := vrNotice(t, "10.10.10.34", stderr+"Cannot enable pairing while session is active.012345\n", true,
		func(e string) bool { return strings.Contains(e, "PIN 012345") }); strings.Contains(out, "012345") {
		t.Errorf("stderr = %q, carries the PIN", out)
	}
	want := "exec swaynag -y overlay -t warning -m 'VR headset: connect the WiVRn app to 10.10.10.34 port 21000 - pairing PIN 012345'"
	execs, out := vrNotice(t, "10.10.10.34", stderr, true, func(e string) bool { return strings.Contains(e, want+"\n") })
	if !strings.Contains(execs, "pkill -x swaynag\n"+want+"\n") {
		t.Errorf("swaymsg execs:\n%s\nwant pkill -x swaynag, then %s", execs, want)
	}
	if want := "WiVRn v26.9 starting\nTo pair a new headset use PIN code: (redacted from the log)\n"; out != want {
		t.Errorf("stderr = %q, want %q (no PIN digits)", out, want)
	}
	noPIN := func(e string) bool {
		l := strings.Split(strings.TrimSpace(e), "\n")
		if strings.HasPrefix(l[len(l)-1], "pkill") {
			return false
		}
		return strings.Contains(e, "PIN") && !strings.Contains(l[len(l)-1], "PIN")
	}
	execs, _ = vrNotice(t, "10.10.10.34", stderr+"Headset pairing is disabled\n", true, noPIN)
	lines := strings.Split(strings.TrimSpace(execs), "\n")
	if last := lines[len(lines)-1]; strings.Contains(last, "PIN") || !strings.Contains(last, "10.10.10.34 port 21000'") {
		t.Errorf("last bar after pairing = %q, want the address without a PIN", last)
	}
}

// Nothing but an address and a 6-digit PIN reaches sway's exec (a shell).
func TestVRNoticeKeepsOtherTextOutOfSwaysExec(t *testing.T) {
	execs, _ := vrNotice(t, "1.2.3.4'; touch /tmp/x; '", "To pair a new headset use PIN code: 1'; id; '\n", true,
		func(e string) bool { return e != "" })
	if execs == "" {
		t.Fatal("no bar shown")
	}
	for l := range strings.SplitSeq(strings.TrimSpace(execs), "\n") {
		if strings.HasPrefix(l, "pkill ") {
			continue
		}
		if strings.Count(l, "'") != 2 || strings.ContainsAny(strings.Trim(l[strings.Index(l, "'")+1:], "'"), ";,'") {
			t.Errorf("exec %q carries quotes or separators", l)
		}
		if !strings.Contains(l, "connect the WiVRn app to the server IP port 21000'") {
			t.Errorf("exec %q, want the placeholder address and no PIN", l)
		}
	}
}

// Before sway is up nothing is shown, and the notice never holds wivrn-server
// up: it exits at the end of its input.
func TestVRNoticeWaitsForSway(t *testing.T) {
	start := time.Now()
	execs, out := vrNotice(t, "10.0.0.1", "line one\nline two", false, func(string) bool { return time.Since(start) > 300*time.Millisecond })
	if execs != "" {
		t.Errorf("swaymsg exec ran without sway: %q", execs)
	}
	if out != "line one\nline two\n" {
		t.Errorf("stderr = %q, want both lines (the last has no newline)", out)
	}
}

// GoW's Steam startup sets WINEDLLOVERRIDES outright; the image makes it
// append the App's own, and fails the build if that line moves.
func TestVRImageAppendsAppDLLOverrides(t *testing.T) {
	b, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"COPY --chmod=755 vr/startup-app.sh vr/vr-services vr/vr-notice /opt/direwolf/vr/",
		"grep -qx 'export WINEDLLOVERRIDES=dxgi=n' /opt/direwolf/vr/steam-startup-app.sh",
		`export WINEDLLOVERRIDES="dxgi=n${DIREWOLF_WINEDLLOVERRIDES:+;$DIREWOLF_WINEDLLOVERRIDES}"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("Dockerfile lacks %s", want)
		}
	}
	// The replacement line, run as the script would.
	line := `export WINEDLLOVERRIDES="dxgi=n${DIREWOLF_WINEDLLOVERRIDES:+;$DIREWOLF_WINEDLLOVERRIDES}"; printf %s "$WINEDLLOVERRIDES"`
	for env, want := range map[string]string{"": "dxgi=n", "dwrite=n,b": "dxgi=n;dwrite=n,b"} {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", line)
		cmd.Env = []string{"DIREWOLF_WINEDLLOVERRIDES=" + env}
		got, err := cmd.Output()
		if err != nil || string(got) != want {
			t.Errorf("DIREWOLF_WINEDLLOVERRIDES=%q: %q %v, want %q", env, got, err, want)
		}
	}
}

// ~/.config/<name> in the shared home becomes a link to the state dir, which
// resolves to each container's own mount (XERK-2713). An empty directory there
// (an old mount point) or another link is replaced; anything else is refused.
func TestVRStartLinksConfigDirsToState(t *testing.T) {
	s := newVRStart(t)
	cfg := filepath.Join(s.home, ".config")
	if err := os.MkdirAll(filepath.Join(cfg, "wivrn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(cfg, "openxr")); err != nil {
		t.Fatal(err)
	}
	for range 2 { // and again, with the links in place
		if out, err := s.run(t, "DIREWOLF_VR_PORT=20034"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, d := range []string{"wivrn", "openxr", "openvr"} {
			if got, err := os.Readlink(filepath.Join(cfg, d)); err != nil || got != filepath.Join(s.state, d) {
				t.Errorf("~/.config/%s -> %q (%v), want %s", d, got, err, filepath.Join(s.state, d))
			}
		}
	}
	if _, err := os.Stat(filepath.Join(s.state, "wivrn", "config.json")); err != nil {
		t.Errorf("WiVRn config not in the state dir: %v", err)
	}

	s = newVRStart(t)
	kept := filepath.Join(s.home, ".config", "openvr", "keep")
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := s.run(t, "DIREWOLF_VR_PORT=20034")
	if err == nil || !strings.Contains(out, "in the way of a link") {
		t.Errorf("non-empty ~/.config/openvr: err %v, output %q; want a refusal", err, out)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("non-empty ~/.config/openvr removed: %v", err)
	}
	if _, err := os.Stat(s.steamLog); err == nil {
		t.Error("Steam started")
	}
}

// WiVRn's stored form of a key: its PEM's bare base64 body (clean_key).
const testHeadsetKey = "MEIwBQYDK2VvAzkAqk1Wq0pXbcbn0mt3CzI1nlOJlKBQ+jDH0A2FErfGpkAx6tB3PdT5AmIoU7oz+t8SN0Z+cH0b5ZM="

// The User's Headsets replace WiVRn's known keys whole at every start: a key
// left by an earlier session (a revoked headset) is gone (XERK-2669).
func TestVRStartSeedsKnownKeysFromHeadsets(t *testing.T) {
	s := newVRStart(t)
	path := filepath.Join(s.state, "wivrn", "known_keys.json")
	if err := os.WriteFile(path, []byte(`[{"key":"revoked","name":"old"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, err := json.Marshal([]map[string]string{{"key": testHeadsetKey, "name": "Quest 3"}})
	if err != nil {
		t.Fatal(err)
	}
	if out, runErr := s.run(t, "DIREWOLF_VR_PORT=20034", "DIREWOLF_VR_HEADSETS="+string(seed)); runErr != nil {
		t.Fatalf("%v\n%s", runErr, out)
	}
	var got []map[string]string
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil || len(got) != 1 || got[0]["key"] != testHeadsetKey || got[0]["name"] != "Quest 3" {
		t.Errorf("known_keys.json = %s (%v); want only the seeded key", b, err)
	}

	// None: an empty list (WiVRn pairs by PIN), never the old file.
	if out, err := s.run(t, "DIREWOLF_VR_PORT=20034"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(path); string(b) != "[]" {
		t.Errorf("known_keys.json = %s; want []", b)
	}
}

// An unparseable known_keys.json reads as no keys in WiVRn, which opens PIN
// pairing: a bad or missing seed refuses to start and writes nothing.
func TestVRStartRefusesBadHeadsetSeed(t *testing.T) {
	for _, env := range []string{
		"DIREWOLF_VR_HEADSETS=",
		"DIREWOLF_VR_HEADSETS={}",
		"DIREWOLF_VR_HEADSETS=[1]",
		`DIREWOLF_VR_HEADSETS=[{"key":"x","name":"y"}]`,
		`DIREWOLF_VR_HEADSETS=[{"key":"` + testHeadsetKey + `"}]`,
		`DIREWOLF_VR_HEADSETS=[{"key":"` + testHeadsetKey + `","name":"q","last_connection":1}]`,
		// A PEM block never matches in WiVRn, which would lock the headset out.
		`DIREWOLF_VR_HEADSETS=[{"key":"-----BEGIN PUBLIC KEY-----\n` + testHeadsetKey + `\n-----END PUBLIC KEY-----\n","name":"q"}]`,
		// nlohmann refuses a lone surrogate, and WiVRn would read no keys.
		`DIREWOLF_VR_HEADSETS=[{"key":"` + testHeadsetKey + `","name":"\ud800"}]`,
	} {
		s := newVRStart(t)
		path := filepath.Join(s.state, "wivrn", "known_keys.json")
		if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := s.run(t, "DIREWOLF_VR_PORT=20034", env)
		if err == nil || !strings.Contains(out, "DIREWOLF_VR_HEADSETS") {
			t.Errorf("%s: err %v, output %q; want a refusal naming DIREWOLF_VR_HEADSETS", env, err, out)
		}
		if _, err := os.Stat(s.steamLog); err == nil {
			t.Errorf("%s: Steam started", env)
		}
	}
	// Unset: an operator that never seeds would leave old keys in force.
	s := newVRStart(t)
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(s.dir, "startup-app.sh")) //nolint:gosec // the test's own copy
	cmd.Env = []string{"PATH=" + s.bin + ":" + os.Getenv("PATH"), "HOME=" + s.home, "DIREWOLF_STATE_ROOT=" + s.state,
		"XDG_RUNTIME_DIR=" + s.runtime, "DIREWOLF_VR_PORT=20034"}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "DIREWOLF_VR_HEADSETS is unset") {
		t.Errorf("unset: err %v, output %q", err, out)
	}
}
