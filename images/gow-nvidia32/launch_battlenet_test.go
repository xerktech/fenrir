package gownvidia32

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const launchBattlenet = "root/opt/direwolf/launch-battlenet"

// stubProton stands in for Proton. `run <client>` is the client: it listens
// on $BATTLENET_WEB_PORT after $CLIENT_UP_AFTER seconds (never with
// $CLIENT_NEVER_UP) and stays up until wineserver -k. `run <client> --exec=...`
// starts a fake game process, whose command line is Wine's for WoW, for
// $GAME_SECONDS (none if unset), $GAME_DELAY seconds after the launch. Like
// the real client, it ignores a launch sent before it is up; with
// $IGNORE_FIRST it also ignores the first one sent after.
const stubProton = `#!/bin/sh
echo "proton $*" >>"$STUB_LOG"
case "$*" in
*--exec=*)
  if [ -n "${IGNORE_FIRST:-}" ] && [ ! -f "$STUB_DIR/ignored" ]; then
    touch "$STUB_DIR/ignored"
  elif { [ -f "$STUB_DIR/up" ] || [ -n "${CLIENT_NEVER_UP:-}" ]; } && [ -n "${GAME_SECONDS:-}" ]; then
    # Wine's argv[0] is the game's Windows path with its arguments.
    bash -c 'sleep "$2"; exec -a "$0" sleep "$1"' \
      "${GAME_ARGV0:-Z:/games/battlenet/World of Warcraft/_retail_/Wow.exe -launcherlogin -uid wow}" \
      "$GAME_SECONDS" "${GAME_DELAY:-0}" &
    echo $! >"$STUB_DIR/game.pid"
  fi
  # A wrapper that stays up, even past wineserver -k.
  [ -n "${EXEC_STAYS_UP:-}" ] && exec sleep 600 ;;
*)
  echo "client VKD3D_SHADER_CACHE_PATH=${VKD3D_SHADER_CACHE_PATH:-} DXVK_STATE_CACHE_PATH=${DXVK_STATE_CACHE_PATH:-}" >>"$STUB_LOG"
  [ -n "${CLIENT_EXITS:-}" ] && exit 0
  echo $$ >"$STUB_DIR/client.pid"
  [ -n "${CLIENT_NEVER_UP:-}" ] && exec sleep 600
  exec python3 -c '
import socket, sys, time
time.sleep(float(sys.argv[1]))
s = socket.socket()
s.bind(("127.0.0.1", int(sys.argv[2])))
s.listen()
open(sys.argv[3], "w").close()
time.sleep(600)
' "${CLIENT_UP_AFTER:-1}" "$BATTLENET_WEB_PORT" "$STUB_DIR/up" ;;
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

// stubSwaymsg logs each command. Its tree has the client's window, Wine's
// desktop window, one already on the scratchpad, and the game's own window
// (its pid from game.pid).
const stubSwaymsg = `#!/bin/sh
if [ "$1" = -t ] && [ "$2" = get_tree ]; then
  game=$(cat "$STUB_DIR/game.pid" 2>/dev/null || echo 0)
  cat <<JSON
{"type":"root","nodes":[{"type":"output","nodes":[
 {"type":"workspace","name":"__i3_scratch","nodes":[],"floating_nodes":[
  {"id":104,"type":"floating_con","pid":3,"window_properties":{"class":"steam_proton"}}]},
 {"type":"workspace","name":"main","nodes":[
  {"id":101,"type":"con","name":"Battle.net","pid":1,"window_properties":{"class":"steam_proton"}},
  {"id":103,"type":"con","name":"World of Warcraft","pid":$game,"window_properties":{"class":"steam_proton"}}],
  "floating_nodes":[{"id":102,"type":"floating_con","pid":2,"window_properties":{"class":"steam_proton"}}]}]}]}
JSON
  exit 0
fi
echo "swaymsg $* SWAYSOCK=$SWAYSOCK" >>"$STUB_LOG"
`

type battlenetRun struct {
	home, prefix, log string
	games             string // $BATTLENET_GAMES
	code              int
	out               string
}

// runLaunchBattlenet runs the real script against a stub Proton, as if in a
// user-namespaced App pod with Battle.net installed in the home's prefix.
func runLaunchBattlenet(t *testing.T, install bool, args []string, env ...string) battlenetRun {
	t.Helper()
	return runLaunchBattlenetWith(t, install, nil, args, env...)
}

// runLaunchBattlenetWith is runLaunchBattlenet with setup run on the run's
// directories (the games volume is $BATTLENET_GAMES, not yet created) first.
func runLaunchBattlenetWith(t *testing.T, install bool, setup func(battlenetRun), args []string, env ...string) battlenetRun {
	t.Helper()
	stub := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stub, "files", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{"proton": stubProton, "files/bin/wineserver": stubWineserver, "bin/killall": stubKillall, "bin/swaymsg": stubSwaymsg}
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
		log: filepath.Join(stub, "log"), games: filepath.Join(home, "games")}
	if install {
		dir := filepath.Join(r.prefix, "pfx", "drive_c", "Program Files (x86)", "Battle.net")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Battle.net.exe"), nil, 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	if setup != nil {
		setup(r)
	}
	// A script that leaves the stub client up would otherwise hang the test.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{launchBattlenet}, args...)...) //nolint:gosec // the script under test
	cmd.WaitDelay = time.Second
	cmd.Env = append([]string{"PATH=" + filepath.Join(stub, "bin") + ":" + os.Getenv("PATH"), "HOME=" + home,
		"SWAY_STOP_ON_APP_EXIT=yes", "BATTLENET_GAMES=" + r.games,
		"STUB_LOG=" + r.log, "STUB_DIR=" + stub,
		"BATTLENET_PROTON=" + filepath.Join(stub, "proton"), "BATTLENET_UID_MAP=" + uidMap,
		"BATTLENET_WEB_PORT=" + freePort(t), "BATTLENET_SETTLE=0", "BATTLENET_READY_WAIT=10",
		"BATTLENET_RESEND_INTERVAL=30", "BATTLENET_LAUNCH_TIMEOUT=20"}, env...)
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

// freePort returns a loopback TCP port nothing listens on.
func freePort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not TCP", l.Addr())
	}
	return strconv.Itoa(addr.Port)
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

// Wine shows the path the client started the game with: Battle.net uses
// forward slashes, but either must count, with or without arguments.
func TestLaunchBattlenetSeesTheGameByEitherPath(t *testing.T) {
	for _, argv0 := range []string{
		`Z:/games/battlenet/World of Warcraft/_retail_/Wow.exe -launcherlogin -uid wow`,
		`Z:\games\battlenet\World of Warcraft\_retail_\Wow.exe -launcherlogin -uid wow`,
		`z:/games/battlenet/World of Warcraft/_retail_/WOW.EXE`,
		`Wow.exe -launcherlogin -uid wow`,
	} {
		r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "GAME_ARGV0="+argv0)
		if r.code != 0 || !strings.Contains(r.out, "wow is running") {
			t.Errorf("%s: exit %d, want the game seen\n%s", argv0, r.code, r.out)
		}
	}
}

// Every launch the client takes starts another copy of the game, each logging
// the last one out: one launch once the client is up, even when the game is
// slow to appear.
func TestLaunchBattlenetLaunchesOnceTheClientIsUp(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "GAME_DELAY=6",
		"CLIENT_UP_AFTER=3", "BATTLENET_RESEND_INTERVAL=15")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if n := strings.Count(r.out, "sent launch "); n != 1 || !strings.Contains(r.out, "Battle.net is up") {
		t.Errorf("%d launches, want 1 after the client is up\n%s", n, r.out)
	}
}

// Under sway the client's and Wine's desktop window would share the screen
// with the game's: while the game runs they go to the scratchpad, never the
// game's own or one already there. Outside sway nothing is asked.
func TestLaunchBattlenetMovesOtherWindowsOutOfTheGamesWay(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "SWAYSOCK=/run/sway.sock")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	moved := map[string]bool{}
	for _, c := range r.calls(t) {
		if id, ok := strings.CutPrefix(c, "swaymsg [con_id="); ok {
			id, _, _ = strings.Cut(id, "]")
			moved[id] = true
			if !strings.HasSuffix(c, "] move scratchpad SWAYSOCK=/run/sway.sock") {
				t.Errorf("unexpected sway command %q", c)
			}
		}
	}
	if !moved["101"] || !moved["102"] || moved["103"] || moved["104"] {
		t.Errorf("moved %v, want the client's (101) and Wine's desktop (102) windows only\n%q", moved, r.calls(t))
	}
	if slices.ContainsFunc(runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3").calls(t),
		func(c string) bool { return strings.HasPrefix(c, "swaymsg ") }) {
		t.Error("ran swaymsg without SWAYSOCK")
	}
}

// A launch the client still ignored is sent again.
func TestLaunchBattlenetResendsAnIgnoredLaunch(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "IGNORE_FIRST=1",
		"BATTLENET_RESEND_INTERVAL=3")
	if r.code != 0 || strings.Count(r.out, "sent launch ") != 2 {
		t.Fatalf("exit %d, want 0 after a second launch\n%s", r.code, r.out)
	}
}

// If the client never shows its port (another one, or no /proc/net/tcp), it is
// launched anyway after BATTLENET_READY_WAIT.
func TestLaunchBattlenetLaunchesAnywayAfterTheReadyWait(t *testing.T) {
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "CLIENT_NEVER_UP=1",
		"BATTLENET_READY_WAIT=2")
	if r.code != 0 || !strings.Contains(r.out, "not seen up within 2s; launching anyway") {
		t.Fatalf("exit %d, want 0 after launching anyway\n%s", r.code, r.out)
	}
}

// Other processes whose command lines name the game, or another product's
// game, are not the game (with no game started, the launch times out).
func TestLaunchBattlenetIgnoresLookalikeProcesses(t *testing.T) {
	for _, argv0 := range []string{
		`bash Z:/games/battlenet/World of Warcraft/_retail_/Wow.exe -launcherlogin -uid wow`,
		`bash Z:\games\battlenet\World of Warcraft\_retail_\Wow.exe -launcherlogin -uid wow`,
		`Z:/games/battlenet/World of Warcraft/_classic_/WowClassic.exe -uid wow_classic`,
		`Z:\games\WowB.exe -uid wow`, `Z:/games/Wow.exe.bak`,
		`C:/Program Files (x86)/Battle.net/BlizzardError.exe -exe Z:/games/battlenet/World of Warcraft/_retail_/Wow.exe`,
		`C:/Program Files (x86)/Battle.net/Battle.net.exe --game=Z:/games/battlenet/World of Warcraft/_retail_/Wow.exe`} {
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
		"BATTLENET_RESEND_INTERVAL=-1", "BATTLENET_RESEND_INTERVAL=1.5", "BATTLENET_WEB_PORT=http",
		"BATTLENET_READY_WAIT=0", "BATTLENET_SETTLE=-1"} {
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

// WoW ignores a pad until GamePadEnable is on (XERK-2365): it is turned on in
// WTF/Config.wtf before launch unless the file already names it, whatever its
// value. A file that is not all SET lines is never touched, and nothing is
// created for a game that is not installed.
func TestLaunchBattlenetTurnsOnWoWGamepadUnlessSet(t *testing.T) {
	const on = `SET GamePadEnable "1"`
	for _, tc := range []struct {
		name      string
		installed bool
		config    *string // nil: no Config.wtf
		want      *string // nil: no Config.wtf afterwards
	}{
		{"absent", true, nil, ptr(on + "\n")},
		{"not installed", false, nil, nil},
		{"present on", true, ptr("SET gxApi \"D3D12\"\nSET GamePadEnable \"1\"\n"), ptr("SET gxApi \"D3D12\"\nSET GamePadEnable \"1\"\n")},
		{"present off", true, ptr("SET GamePadEnable \"0\"\nSET gxApi \"D3D12\"\n"), ptr("SET GamePadEnable \"0\"\nSET gxApi \"D3D12\"\n")},
		{"present off, other case", true, ptr("SET gamepadenable \"0\"\n"), ptr("SET gamepadenable \"0\"\n")},
		{"other CVars", true, ptr("SET portal \"US\"\nSET textLocale \"enUS\"\n"), ptr("SET portal \"US\"\nSET textLocale \"enUS\"\n" + on + "\n")},
		{"empty", true, ptr(""), ptr(on + "\n")},
		{"no final newline", true, ptr(`SET portal "US"`), ptr("SET portal \"US\"\n" + on + "\n")},
		{"CRLF", true, ptr("SET portal \"US\"\r\n"), ptr("SET portal \"US\"\r\n" + on + "\r\n")},
		{"unparseable", true, ptr("SET portal \"US\"\ngarbage\n"), ptr("SET portal \"US\"\ngarbage\n")},
		{"unparseable, binary", true, ptr("SET portal \"US\"\n\x00\xff\n"), ptr("SET portal \"US\"\n\x00\xff\n")},
		{"unparseable, indented", true, ptr("  SET GamePadEnable \"0\"\n"), ptr("  SET GamePadEnable \"0\"\n")},
	} {
		var wtf string
		r := runLaunchBattlenetWith(t, true, func(r battlenetRun) {
			game := filepath.Join(r.games, "World of Warcraft", "_retail_")
			wtf = filepath.Join(game, "WTF", "Config.wtf")
			if !tc.installed {
				return
			}
			if err := os.MkdirAll(filepath.Dir(wtf), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.config != nil {
				if err := os.WriteFile(wtf, []byte(*tc.config), 0o644); err != nil { //nolint:gosec // test fixture
					t.Fatal(err)
				}
			}
		}, []string{"wow"}, "GAME_SECONDS=3")
		if r.code != 0 {
			t.Errorf("%s: exit %d, want 0\n%s", tc.name, r.code, r.out)
		}
		got, err := os.ReadFile(wtf)
		switch {
		case tc.want == nil && !errors.Is(err, os.ErrNotExist):
			t.Errorf("%s: Config.wtf %q (%v), want none", tc.name, got, err)
		case tc.want != nil && (err != nil || string(got) != *tc.want):
			t.Errorf("%s: Config.wtf %q (%v), want %q\n%s", tc.name, got, err, *tc.want, r.out)
		}
		if !tc.installed {
			if _, err := os.Stat(r.games); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: created %s for a game that is not installed", tc.name, r.games)
			}
		}
	}
}

// A Config.wtf that is a link or a directory is left alone, and its failure
// never stops the game.
func TestLaunchBattlenetLeavesOddConfigWtfAlone(t *testing.T) {
	r := runLaunchBattlenetWith(t, true, func(r battlenetRun) {
		if err := os.MkdirAll(filepath.Join(r.games, "World of Warcraft", "_retail_", "WTF", "Config.wtf"), 0o755); err != nil {
			t.Fatal(err)
		}
	}, []string{"wow"}, "GAME_SECONDS=3")
	if r.code != 0 || !strings.Contains(r.out, "GamePadEnable unchanged") {
		t.Fatalf("exit %d, want 0 with the directory left alone\n%s", r.code, r.out)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	r = runLaunchBattlenetWith(t, true, func(r battlenetRun) {
		wtf := filepath.Join(r.games, "World of Warcraft", "_retail_", "WTF")
		if err := os.MkdirAll(wtf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(wtf, "Config.wtf")); err != nil {
			t.Fatal(err)
		}
	}, []string{"wow"}, "GAME_SECONDS=3")
	if _, err := os.Lstat(target); r.code != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exit %d, dangling link's target %v, want 0 and no file written through it\n%s", r.code, err, r.out)
	}
}

func ptr(s string) *string { return &s }
