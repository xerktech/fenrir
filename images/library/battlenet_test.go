package library

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	battlenet     = "root/usr/bin/battlenet"
	initBattlenet = "root/etc/s6-overlay/s6-rc.d/init-battlenet/run"
)

// battlenetHost is a desktop home, a games volume and a stub Proton whose
// `run reg add` writes the value into the prefix's user.reg, as Wine does.
type battlenetHost struct {
	t                             *testing.T
	dir, home, games, log, proton string
}

func newBattlenetHost(t *testing.T) *battlenetHost {
	t.Helper()
	dir := t.TempDir()
	h := &battlenetHost{t: t, dir: dir, home: filepath.Join(dir, "home"),
		games: filepath.Join(dir, "games", "battlenet"), log: filepath.Join(dir, "log"),
		proton: filepath.Join(dir, "proton-cachyos")}
	for _, d := range []string{filepath.Join(dir, "bin"), filepath.Join(h.proton, "files", "bin"), h.home, h.games} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub(t, filepath.Join(h.proton, "proton"), `echo "proton $* xalia=$PROTON_USE_XALIA client=$STEAM_COMPAT_CLIENT_INSTALL_PATH" >>"$LOG"
[ -n "$PROTON_FAIL" ] && exit 1
if [ "$2" = reg ]; then mkdir -p "$STEAM_COMPAT_DATA_PATH/pfx" && echo '"UseTakeFocus"="N"' >>"$STEAM_COMPAT_DATA_PATH/pfx/user.reg"; fi
exit 0`)
	writeStub(t, filepath.Join(h.proton, "files", "bin", "wineserver"), `echo "wineserver $* $WINEPREFIX" >>"$LOG"`)
	writeStub(t, filepath.Join(dir, "bin", "pgrep"), `case "$*" in *Setup*) [ -n "$SETUP_RUNNING" ] ;; *) [ -n "$CLIENT_RUNNING" ] ;; esac`)
	return h
}

func (h *battlenetHost) prefix() string { return filepath.Join(h.home, ".local", "share", "battlenet") }

func (h *battlenetHost) config() string {
	return filepath.Join(h.prefix(), "pfx", "drive_c", "users", "steamuser", "AppData", "Roaming", "Battle.net", "Battle.net.config")
}

func (h *battlenetHost) client() string {
	return filepath.Join(h.prefix(), "pfx", "drive_c", "Program Files (x86)", "Battle.net", "Battle.net.exe")
}

// run runs the real wrapper against the stub Proton and returns the calls.
func (h *battlenetHost) run(env []string, args ...string) string {
	t := h.t
	t.Helper()
	if err := os.WriteFile(h.log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(battlenet)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(src), "/opt/proton-cachyos", h.proton)
	if patched == string(src) {
		t.Fatal("battlenet no longer runs /opt/proton-cachyos")
	}
	script := filepath.Join(h.dir, "battlenet")
	if err = os.WriteFile(script, []byte(patched), 0o600); err != nil { //nolint:gosec // script is under t.TempDir
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{script}, args...)...) //nolint:gosec // the wrapper under test
	cmd.Env = append([]string{
		"PATH=" + filepath.Join(h.dir, "bin") + ":/usr/bin:/bin",
		"HOME=" + h.home, "LOG=" + h.log, "BATTLENET_GAMES=" + h.games,
	}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("battlenet: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls) + string(out)
}

// battlenetConfig is the part of Battle.net.config the wrapper sets, plus
// settings it must keep.
type battlenetConfig struct {
	Client struct {
		HardwareAcceleration string
		AutoLogin            string
		Install              struct{ DefaultInstallPath string }
	}
	WoW map[string]any `json:"wow"`
}

func (h *battlenetHost) readConfig() battlenetConfig {
	h.t.Helper()
	b, err := os.ReadFile(h.config())
	if err != nil {
		h.t.Fatal(err)
	}
	var c battlenetConfig
	if err = json.Unmarshal(b, &c); err != nil {
		h.t.Fatalf("%v\n%s", err, b)
	}
	return c
}

// --prepare creates the prefix with keyboard focus left to labwc, waits for
// Wine to write it out, and does nothing once that is done.
func TestBattlenetPrepareCreatesPrefixOnce(t *testing.T) {
	h := newBattlenetHost(t)
	calls := h.run(nil, "--prepare")
	for _, want := range []string{
		`proton run reg add HKCU\Software\Wine\X11 Driver /v UseTakeFocus /d N /f xalia=0 client=` + filepath.Join(h.home, ".local", "share", "Steam"),
		"wineserver -w " + filepath.Join(h.prefix(), "pfx"),
		"prefix ready",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if calls = h.run(nil, "--prepare"); strings.Contains(calls, "proton") {
		t.Errorf("a prepared prefix was prepared again:\n%s", calls)
	}
	if _, err := os.Stat(h.config()); !os.IsNotExist(err) {
		t.Errorf("--prepare touched Battle.net's config: %v", err)
	}
}

// A failing Proton never fails --prepare: init-battlenet must not hold the desktop.
func TestBattlenetPrepareFailureExitsZero(t *testing.T) {
	h := newBattlenetHost(t)
	if calls := h.run([]string{"PROTON_FAIL=1"}, "--prepare"); !strings.Contains(calls, "could not prepare") {
		t.Errorf("calls:\n%s", calls)
	}
}

// The first start runs the image's installer, after turning hardware
// acceleration off and pointing new installs at the games volume.
func TestBattlenetFirstStartInstalls(t *testing.T) {
	h := newBattlenetHost(t)
	calls := h.run(nil)
	if want := `proton run /opt/battlenet/Battle.net-Setup.exe --lang=enUS --installpath=C:\Program Files (x86)\Battle.net`; !strings.Contains(calls, want) {
		t.Errorf("calls lack %q:\n%s", want, calls)
	}
	client := h.readConfig().Client
	if client.HardwareAcceleration != "false" {
		t.Errorf("HardwareAcceleration %q", client.HardwareAcceleration)
	}
	if p := client.Install.DefaultInstallPath; p != "Z:"+h.games+"/" {
		t.Errorf("DefaultInstallPath %v, want Z:%s/", p, h.games)
	}
}

// An installed client is started with the arguments; the config keeps its
// other settings and a user-chosen install folder, and acceleration stays off.
func TestBattlenetStartsInstalledClientAndKeepsSettings(t *testing.T) {
	h := newBattlenetHost(t)
	for path, body := range map[string]string{
		h.client(): "",
		h.config(): "\ufeff" + `{"Client":{"HardwareAcceleration":"true","AutoLogin":"true","Install":{"DefaultInstallPath":"D:/Blizzard/"}},"wow":{"Region":"US"}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := h.run(nil, "--exec=launch WoW")
	if want := "proton run " + h.client() + " --exec=launch WoW"; !strings.Contains(calls, want) {
		t.Errorf("calls lack %q:\n%s", want, calls)
	}
	c := h.readConfig()
	if c.Client.HardwareAcceleration != "false" || c.Client.AutoLogin != "true" ||
		c.Client.Install.DefaultInstallPath != "D:/Blizzard/" || c.WoW["Region"] != "US" {
		t.Errorf("config %v", c)
	}
}

// While a client runs, its config is left for it (it rewrites it on exit),
// and the start is handed to it.
func TestBattlenetLeavesRunningClientsConfig(t *testing.T) {
	h := newBattlenetHost(t)
	if err := os.MkdirAll(filepath.Dir(h.client()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.client(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := h.run([]string{"CLIENT_RUNNING=1"})
	if !strings.Contains(calls, "proton run "+h.client()) {
		t.Errorf("calls:\n%s", calls)
	}
	if _, err := os.Stat(h.config()); !os.IsNotExist(err) {
		t.Errorf("wrote the running client's config: %v", err)
	}
}

// A config that is not a JSON object is left alone, and Battle.net still starts.
func TestBattlenetLeavesUnreadableConfig(t *testing.T) {
	h := newBattlenetHost(t)
	if err := os.MkdirAll(filepath.Dir(h.config()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.config(), []byte(`["x"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := h.run(nil)
	if !strings.Contains(calls, "could not update") || !strings.Contains(calls, "Battle.net-Setup.exe") {
		t.Errorf("calls:\n%s", calls)
	}
	if b, _ := os.ReadFile(h.config()); string(b) != `["x"]` {
		t.Errorf("config rewritten: %s", b)
	}
}

// runInitBattlenet runs the oneshot with root's tools stubbed, the games
// volume at games, and returns the stubs' calls.
func runInitBattlenet(t *testing.T, games string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	writeStub(t, filepath.Join(dir, "getent"), `echo "abc:x:1000:1000::/config:/bin/bash"`)
	writeStub(t, filepath.Join(dir, "chown"), `echo "chown $*" >>"$LOG"`)
	writeStub(t, filepath.Join(dir, "s6-setuidgid"), `echo "as $1 HOME=$HOME: $2 $3" >>"$LOG"; exit 3`)
	cmd := exec.CommandContext(t.Context(), "bash", initBattlenet)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "LOG=" + log, "BATTLENET_GAMES=" + games}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init-battlenet must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls) + string(out)
}

// The oneshot makes the game directory for the desktop user, then prepares
// the prefix as that user; a failed prepare does not fail it.
func TestInitBattlenetMakesGamesDirAndPreparesAsUser(t *testing.T) {
	games := filepath.Join(t.TempDir(), "battlenet")
	calls := runInitBattlenet(t, games)
	for _, want := range []string{"chown -h abc:abc " + games, "as abc HOME=/config: /usr/bin/battlenet --prepare", "timed out or failed"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if fi, err := os.Stat(games); err != nil || !fi.IsDir() {
		t.Errorf("games dir: %v %v", fi, err)
	}
}

// A planted link where the game directory goes is never followed by root,
// and an unmounted volume is skipped; the prefix is still prepared.
func TestInitBattlenetLeavesLinkAndMissingVolume(t *testing.T) {
	vol := t.TempDir()
	games := filepath.Join(vol, "battlenet")
	if err := os.Symlink("/etc", games); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{games, filepath.Join(vol, "missing", "battlenet")} {
		calls := runInitBattlenet(t, g)
		if strings.Contains(calls, "chown") || !strings.Contains(calls, "--prepare") {
			t.Errorf("%s: calls:\n%s", g, calls)
		}
	}
}

// A second start while the installer runs starts no second installer.
func TestBattlenetStartsOneInstaller(t *testing.T) {
	h := newBattlenetHost(t)
	calls := h.run([]string{"SETUP_RUNNING=1"})
	if strings.Contains(calls, "Battle.net-Setup.exe --lang") || !strings.Contains(calls, "installer is already running") {
		t.Errorf("calls:\n%s", calls)
	}
}
