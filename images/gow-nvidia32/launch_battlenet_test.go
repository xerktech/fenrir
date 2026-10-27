package gownvidia32

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const launchBattlenet = "root/opt/direwolf/launch-battlenet"

// stubProton stands in for Proton: `run <client>` stays up as the client until
// wineserver -k, and `run <client> --exec=...` starts a fake game process whose
// command line is Wine's for WoW, for $GAME_SECONDS (none if unset).
const stubProton = `#!/bin/sh
echo "proton $*" >>"$STUB_LOG"
case "$*" in
*--exec=*)
  if [ -n "${GAME_SECONDS:-}" ]; then
    # Wine's argv[0] is the game's Windows path with its arguments.
    bash -c 'exec -a "$0" sleep "$1"' 'Z:\games\battlenet\World of Warcraft\_retail_\Wow.exe -launcherlogin -uid wow' "$GAME_SECONDS" &
    echo $! >"$STUB_DIR/game.pid"
  fi ;;
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
	for name, body := range map[string]string{"proton": stubProton, "files/bin/wineserver": stubWineserver} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	uidMap := filepath.Join(stub, "uid_map")
	if err := os.WriteFile(uidMap, []byte("         0     100000      65536\n"), 0o644); err != nil {
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
		if err := os.WriteFile(filepath.Join(dir, "Battle.net.exe"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{launchBattlenet}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"STUB_LOG=" + r.log, "STUB_DIR=" + stub,
		"BATTLENET_PROTON=" + filepath.Join(stub, "proton"), "BATTLENET_UID_MAP=" + uidMap,
		"BATTLENET_RESEND_INTERVAL=1", "BATTLENET_LAUNCH_TIMEOUT=20"}, env...)
	out, err := cmd.CombinedOutput()
	r.out = string(out)
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("launch-battlenet: %v\n%s", err, out)
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
	if last := calls[len(calls)-1]; !strings.HasPrefix(last, "wineserver -k ") {
		t.Errorf("last call %q, want wineserver -k", last)
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
}

func TestLaunchBattlenetNeedsBattlenetInstalled(t *testing.T) {
	r := runLaunchBattlenet(t, false, []string{"wow"})
	if r.code != 1 || !strings.Contains(r.out, "install it from the Library") {
		t.Fatalf("exit %d, want 1 for a missing install\n%s", r.code, r.out)
	}
	if calls := r.calls(t); calls != nil {
		t.Errorf("ran %q", calls)
	}
}

// The product reaches Battle.net's command line and a process pattern.
func TestLaunchBattlenetRefusesUnknownProducts(t *testing.T) {
	for _, product := range []string{"", "WoW", "wow ", "wow;id", `wow" --exec="launch Pro`, "pro", ".*"} {
		r := runLaunchBattlenet(t, true, []string{product}, "GAME_SECONDS=3")
		if r.code != 2 {
			t.Errorf("product %q: exit %d, want 2\n%s", product, r.code, r.out)
		}
		if calls := r.calls(t); calls != nil {
			t.Errorf("product %q: ran %q", product, calls)
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
func TestLaunchBattlenetRefusesTheHostUserNamespace(t *testing.T) {
	hostMap := filepath.Join(t.TempDir(), "uid_map")
	if err := os.WriteFile(hostMap, []byte("         0          0 4294967295\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runLaunchBattlenet(t, true, []string{"wow"}, "GAME_SECONDS=3", "BATTLENET_UID_MAP="+hostMap)
	if r.code != 2 || !strings.Contains(r.out, "requireAppPod") {
		t.Fatalf("exit %d, want 2 naming requireAppPod\n%s", r.code, r.out)
	}
	if calls := r.calls(t); calls != nil {
		t.Errorf("ran %q", calls)
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
