package gownvidia32

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const launchBattlenet = "root/opt/direwolf/launch-battlenet"

// stubProton stands in for Proton: `run <client>` stays up as the client until
// wineserver -k, and `run <client> --exec=...` starts a fake game process whose
// command line is Wine's for WoW, for $GAME_SECONDS (none if unset). Like a
// client still starting up, it ignores the first launch.
const stubProton = `#!/bin/sh
echo "proton $*" >>"$STUB_LOG"
case "$*" in
*--exec=*)
  if [ -f "$STUB_DIR/ready" ] && [ -n "${GAME_SECONDS:-}" ]; then
    # Wine's argv[0] is the game's Windows path with its arguments.
    bash -c 'exec -a "$0" sleep "$1"' 'Z:\games\battlenet\World of Warcraft\_retail_\Wow.exe -launcherlogin -uid wow' "$GAME_SECONDS" &
    echo $! >"$STUB_DIR/game.pid"
  fi
  touch "$STUB_DIR/ready"
  # A wrapper that stays up, even past wineserver -k.
  [ -n "${EXEC_STAYS_UP:-}" ] && exec sleep 600 ;;
*)
  echo "client VKD3D_SHADER_CACHE_PATH=${VKD3D_SHADER_CACHE_PATH:-} DXVK_STATE_CACHE_PATH=${DXVK_STATE_CACHE_PATH:-}" >>"$STUB_LOG"
  [ -n "${CLIENT_EXITS:-}" ] && exit 0
  echo $$ >"$STUB_DIR/client.pid"
  exec sleep 600 ;;
esac
`

const stubWineserver = `#!/bin/sh
echo "wineserver $* WINEPREFIX=$WINEPREFIX" >>"$STUB_LOG"
[ -f "$STUB_DIR/client.pid" ] && kill "$(cat "$STUB_DIR/client.pid")"
[ -f "$STUB_DIR/game.pid" ] && kill "$(cat "$STUB_DIR/game.pid")" 2>/dev/null
exit 0
`

const stubKillall = `#!/bin/sh
echo "killall $*" >>"$STUB_LOG"
`

type battlenetRun struct {
	home, prefix, log string
	code              int
	out               string
}

// runLaunchBattlenet runs the real script against a stub Proton, as if in a
// user-namespaced App pod with Battle.net installed in the home's prefix.
func runLaunchBattlenet(t *testing.T, install bool, args []string, env ...string) battlenetRun {
	t.Helper()
	stub := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stub, "files", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{"proton": stubProton, "files/bin/wineserver": stubWineserver, "bin/killall": stubKillall}
	if err := os.MkdirAll(filepath.Join(stub, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil { //nolint:gosec // test stubs must be executable
			t.Fatal(err)
		}
	}
	uidMap := filepath.Join(stub, "uid_map")
	if err := os.WriteFile(uidMap, []byte("         0     100000      65536\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	home := t.TempDir()
	r := battlenetRun{home: home, prefix: filepath.Join(home, ".local", "share", "battlenet"),
		log: filepath.Join(stub, "log")}
	if install {
		dir := filepath.Join(r.prefix, "pfx", "drive_c", "Program Files (x86)", "Battle.net")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Battle.net.exe"), nil, 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	// A script that leaves the stub client up would otherwise hang the test.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{launchBattlenet}, args...)...) //nolint:gosec // the script under test
	cmd.WaitDelay = time.Second
	cmd.Env = append([]string{"PATH=" + filepath.Join(stub, "bin") + ":" + os.Getenv("PATH"), "HOME=" + home,
		"SWAY_STOP_ON_APP_EXIT=yes",
		"STUB_LOG=" + r.log, "STUB_DIR=" + stub,
		"BATTLENET_PROTON=" + filepath.Join(stub, "proton"), "BATTLENET_UID_MAP=" + uidMap,
		"BATTLENET_RESEND_INTERVAL=2", "BATTLENET_LAUNCH_TIMEOUT=20"}, env...)
	out, err := cmd.CombinedOutput()
	r.out = string(out)
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("launch-battlenet: %v\n%s", err, out)
	}
	if ctx.Err() != nil {
		t.Fatalf("launch-battlenet did not exit\n%s", out)
	}
	return r
}

func (r battlenetRun) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(r.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestLaunchBattlenetPlaysTheGameThenStopsBattlenet(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	calls := r.calls(t)
	client := filepath.Join(r.prefix, "pfx", "drive_c", "Program Files (x86)", "Battle.net", "Battle.net.exe")
	vkd3d := filepath.Join(r.home, ".cache", "vkd3d-proton", "battlenet-wow")
	dxvk := filepath.Join(r.home, ".cache", "dxvk", "battlenet-wow")
	want := []string{
		"proton run " + client,
		"client VKD3D_SHADER_CACHE_PATH=" + vkd3d + " DXVK_STATE_CACHE_PATH=" + dxvk,
		"proton run " + client + " --exec=launch WoW",
		"proton run " + client + " --exec=launch WoW",
		"wineserver -k WINEPREFIX=" + filepath.Join(r.prefix, "pfx"),
	}
	if len(calls) != len(want) {
		t.Fatalf("calls:\n%s\nwant:\n%s\noutput:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"), r.out)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
	for _, dir := range []string{vkd3d, dxvk} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("cache directory %s not created: %v", dir, err)
		}
	}
}

// A patch, an expired sign-in or a client that ignores the launch must end
// the session rather than leave it on Battle.net's window forever.
func TestLaunchBattlenetEndsWhenTheGameNeverStarts(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "BATTLENET_LAUNCH_TIMEOUT=3")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r.out)
	}
	calls := r.calls(t)
	if n := len(calls); n < 2 || !strings.HasPrefix(calls[n-2], "wineserver -k ") || calls[n-1] != "killall sway" {
		t.Errorf("calls %q, want wineserver -k then killall sway", calls)
	}
	if !strings.Contains(r.out, "did not start within 3s") {
		t.Errorf("output does not say why:\n%s", r.out)
	}
}

func TestLaunchBattlenetEndsWhenBattlenetExits(t *testing.T) {
	r := runLaunchBattlenet(t, true, nil, "BATTLENET_PRODUCT=wow", "CLIENT_EXITS=1")
	if r.code != 1 || !strings.Contains(r.out, "Battle.net exited before wow started") {
		t.Fatalf("exit %d, want 1 for the client exiting\n%s", r.code, r.out)
	}
	if calls := r.calls(t); calls[len(calls)-1] != "killall sway" {
		t.Errorf("calls %q, want the session ended", calls)
	}
}

// A launch wrapper that never exits must neither stall the wait for the game
// nor keep the session up after it.
func TestLaunchBattlenetOutlivesStuckLaunchWrappers(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "EXEC_STAYS_UP=1")
	if r.code != 0 || !strings.Contains(r.out, "wow exited; stopping Battle.net") {
		t.Fatalf("exit %d, want 0 after the game\n%s", r.code, r.out)
	}
}

// Outside sway (gamescope, or no compositor), the launcher's own exit ends it.
func TestLaunchBattlenetLeavesSwayAloneOutsideSway(t *testing.T) {
	r := runLaunchBattlenet(t, false, []string{"wow"}, "SWAY_STOP_ON_APP_EXIT=")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r.out)
	}
	if calls := r.calls(t); calls != nil {
		t.Errorf("calls %q, want none", calls)
	}
}

// Other processes whose command lines name the game, or another product's
// game, are not the game (with no game started, the launch times out).
func TestLaunchBattlenetIgnoresLookalikeProcesses(t *testing.T) {
	path := `Z:\games\battlenet\World of Warcraft\_retail_\Wow.exe`
	for _, argv0 := range []string{"bash " + path + " -launcherlogin -uid wow",
		path + " -launcherlogin -uid wow_classic", `Z:\games\WowB.exe -uid wow`} {
		decoy := exec.CommandContext(t.Context(), "bash", "-c", `exec -a "$0" sleep 30`, argv0)
		if err := decoy.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = decoy.Process.Kill(); _ = decoy.Wait() })
	}
	r := runLaunchBattlenet(t, true, []string{"wow"}, "BATTLENET_LAUNCH_TIMEOUT=3")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1: a lookalike counted as the game\n%s", r.code, r.out)
	}
}

func TestLaunchBattlenetNeedsBattlenetInstalled(t *testing.T) {
	r := runLaunchBattlenet(t, false, []string{"wow"})
	if r.code != 1 || !strings.Contains(r.out, "install it from the Library") {
		t.Fatalf("exit %d, want 1 for a missing install\n%s", r.code, r.out)
	}
	if calls := r.calls(t); !slices.Equal(calls, []string{"killall sway"}) {
		t.Errorf("calls %q, want only the session ended", calls)
	}
}

// The product reaches Battle.net's command line and a process pattern.
func TestLaunchBattlenetRefusesUnknownProducts(t *testing.T) {
	for _, product := range []string{"", "WoW", "wow ", "wow;id", `wow" --exec="launch Pro`, "pro", ".*"} {
		r := runLaunchBattlenet(t, true, []string{product}, "GAME_SECONDS=3")
		if r.code != 2 {
			t.Errorf("product %q: exit %d, want 2\n%s", product, r.code, r.out)
		}
		if calls := r.calls(t); !slices.Equal(calls, []string{"killall sway"}) {
			t.Errorf("product %q: calls %q, want only the session ended", product, calls)
		}
	}
}

func TestLaunchBattlenetRefusesBadTimings(t *testing.T) {
	for _, env := range []string{"BATTLENET_LAUNCH_TIMEOUT=0", "BATTLENET_LAUNCH_TIMEOUT=$(id)",
		"BATTLENET_RESEND_INTERVAL=-1", "BATTLENET_RESEND_INTERVAL=1.5"} {
		if r := runLaunchBattlenet(t, true, []string{"wow"}, env); r.code != 2 {
			t.Errorf("%s: exit %d, want 2\n%s", env, r.code, r.out)
		}
	}
}

// In the host user namespace the pod may be host-networked, where Battle.net's
// 127.0.0.1 listeners would be the node's (XERK-2362).
// An unreadable map counts as the host's.
func TestLaunchBattlenetRefusesTheHostUserNamespace(t *testing.T) {
	hostMap := filepath.Join(t.TempDir(), "uid_map")
	if err := os.WriteFile(hostMap, []byte("         0          0 4294967295\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, uidMap := range []string{hostMap, filepath.Join(t.TempDir(), "missing")} {
		r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "BATTLENET_UID_MAP="+uidMap)
		if r.code != 2 || !strings.Contains(r.out, "requireAppPod") {
			t.Fatalf("%s: exit %d, want 2 naming requireAppPod\n%s", uidMap, r.code, r.out)
		}
		if calls := r.calls(t); !slices.Equal(calls, []string{"killall sway"}) {
			t.Errorf("%s: calls %q, want only the session ended", uidMap, calls)
		}
	}
}

func TestLaunchBattlenetKeepsExplicitShaderCaches(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3",
		"VKD3D_SHADER_CACHE_PATH=/user/vkd3d", "DXVK_STATE_CACHE_PATH=/user/dxvk")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.out)
	}
	if calls := r.calls(t); len(calls) < 2 ||
		calls[1] != "client VKD3D_SHADER_CACHE_PATH=/user/vkd3d DXVK_STATE_CACHE_PATH=/user/dxvk" {
		t.Errorf("calls %q, want the caller's caches", calls)
	}
}

// The session and the Library run one prefix in the shared home: a different
// Proton would upgrade it under the other.
func TestBattlenetProtonMatchesLibrary(t *testing.T) {
	pin := regexp.MustCompile(`(?m)^ARG (PROTON_CACHYOS_(?:VERSION|SHA512))=(\S+)$`)
	pins := func(path string) map[string]string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, match := range pin.FindAllStringSubmatch(string(b), -1) {
			m[match[1]] = match[2]
		}
		if len(m) != 2 {
			t.Fatalf("%s: Proton-CachyOS pins %v, want VERSION and SHA512", path, m)
		}
		return m
	}
	here, library := pins("Dockerfile"), pins("../library/Dockerfile")
	for k, v := range library {
		if here[k] != v {
			t.Errorf("%s = %s here, %s in the Library", k, here[k], v)
		}
	}
}

func TestBattlenetImageStartsLaunchBattlenet(t *testing.T) {
	stage := dockerfileStage(t, "game-session AS battlenet")
	for _, line := range []string{
		"COPY --chmod=755 root/opt/direwolf/launch-battlenet /opt/direwolf/launch-battlenet",
		"COPY --chmod=755 root/opt/gow/battlenet-startup-app.sh /opt/gow/startup-app.sh",
		"COPY --from=proton-cachyos /opt/proton-cachyos /opt/proton-cachyos",
	} {
		if !strings.Contains(stage, "\n"+line+"\n") {
			t.Errorf("battlenet stage lacks %q", line)
		}
	}
	startup, err := os.ReadFile("root/opt/gow/battlenet-startup-app.sh")
	if err != nil {
		t.Fatal(err)
	}
	// sway's exec line is a shell line: nothing from the App's env may join it.
	if !strings.HasSuffix(string(startup), "\nlauncher /opt/direwolf/launch-battlenet\n") {
		t.Error("battlenet-startup-app.sh must end with a bare `launcher /opt/direwolf/launch-battlenet`")
	}
}

// dockerfileStage returns the Dockerfile stage whose FROM line ends in header.
func dockerfileStage(t *testing.T, header string) string {
	t.Helper()
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for stage := range strings.SplitSeq(string(dockerfile), "\nFROM ") {
		if strings.HasPrefix(stage, header+"\n") {
			return stage
		}
	}
	t.Fatalf("no %q stage in the Dockerfile", header)
	return ""
}
