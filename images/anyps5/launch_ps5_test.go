package anyps5

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const launchPS5 = "root/opt/direwolf/launch-ps5"

// stubRelinker stands in for AnyPS5's relinker: it logs its arguments, writes
// the "converted program" (a script that logs where and how it ran) and a
// converted module under app0/, as the real one does beside its output.
const stubRelinker = `#!/bin/sh
echo "relinker $*" >>"$STUB_LOG"
for out; do :; done
dir=$(dirname "$out")
mkdir -p "$dir/app0/sce_module"
echo converted >"$dir/app0/sce_module/libgame.guest.prx"
cat >"$out" <<'EOF'
#!/bin/sh
echo "game pwd=$(pwd) fonts=$ANYPS5_SYSTEM_FONTS cache=$ANYPS5_SHADER_CACHE_DIR fullscreen=$ANYPS5_FULLSCREEN" >>"$STUB_LOG"
exit "${GAME_STATUS:-0}"
EOF
chmod +x "$out"
`

type ps5Env struct {
	t       *testing.T
	anyps5  string
	games   string
	home    string
	stubLog string
}

func newPS5Env(t *testing.T) *ps5Env {
	t.Helper()
	root := t.TempDir()
	e := &ps5Env{
		t:       t,
		anyps5:  filepath.Join(root, "opt-anyps5"),
		games:   filepath.Join(root, "games"),
		home:    filepath.Join(root, "home"),
		stubLog: filepath.Join(root, "stub.log"),
	}
	for _, d := range []string{filepath.Join(e.anyps5, "libs"), e.games, e.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.write(filepath.Join(e.anyps5, "relinker"), stubRelinker, 0o755)
	e.setVersion("v0.1.4")
	return e
}

func (e *ps5Env) write(path, content string, mode os.FileMode) {
	e.t.Helper()
	writeFile(e.t, path, content, mode)
}

// writeFile writes a fixture, creating its directory; scripts need mode 0o755.
func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil { //nolint:gosec // fixtures under t.TempDir()
		t.Fatal(err)
	}
}

// exitCode is a finished command's exit status, or -1 if it did not exit.
func exitCode(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

func (e *ps5Env) setVersion(v string) {
	e.write(filepath.Join(e.anyps5, "VERSION"), v+"\n", 0o644)
}

// addGame writes a dump: an ELF eboot.bin, modules, sce_sys and a data file.
func (e *ps5Env) addGame(title string, withModules bool) string {
	dump := filepath.Join(e.games, title)
	e.write(filepath.Join(dump, "eboot.bin"), "\x7fELF-game", 0o644)
	e.write(filepath.Join(dump, "sce_sys", "param.json"), "{}", 0o644)
	e.write(filepath.Join(dump, "data.pak"), "data", 0o644)
	if withModules {
		e.write(filepath.Join(dump, "sce_module", "libgame.prx"), "\x7fELF-mod", 0o644)
	}
	return dump
}

func (e *ps5Env) run(title string, extraEnv ...string) (string, error) {
	e.t.Helper()
	cmd := exec.CommandContext(e.t.Context(), "bash", launchPS5)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"ANYPS5_HOME=" + e.anyps5,
		"PS5_GAMES_DIR=" + e.games,
		"PS5_TITLE_ID=" + title,
		"STUB_LOG=" + e.stubLog,
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *ps5Env) log() string {
	e.t.Helper()
	b, err := os.ReadFile(e.stubLog)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *ps5Env) resetLog() {
	if err := os.Remove(e.stubLog); err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
}

func TestLaunchPS5RejectsBadTitleIDs(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	for _, title := range []string{"", "PPSA0123", "ppsa01234", "../PPSA01234", "PPSA01234/..", "PPSA01234;true", "PPSA012345"} {
		out, err := e.run(title)
		if err == nil {
			t.Errorf("title %q: launch succeeded, want a refusal", title)
		}
		if !strings.Contains(out, "PS5_TITLE_ID must be a title ID") {
			t.Errorf("title %q: output %q does not name the bad title ID", title, out)
		}
	}
	if log := e.log(); log != "" {
		t.Errorf("relinker or game ran for a bad title ID:\n%s", log)
	}
}

func TestLaunchPS5RefusesMissingOrEncryptedGame(t *testing.T) {
	e := newPS5Env(t)
	if out, err := e.run("PPSA01234"); err == nil || !strings.Contains(out, "no game at") {
		t.Errorf("missing dump: err %v, output %q", err, out)
	}
	// A console SELF starts with its own magic, not ELF's.
	e.write(filepath.Join(e.games, "PPSA01234", "eboot.bin"), "\x4f\x15\x3d\x1dSELF", 0o644)
	if out, err := e.run("PPSA01234"); err == nil || !strings.Contains(out, "not a decrypted ELF") {
		t.Errorf("encrypted eboot: err %v, output %q", err, out)
	}
	if log := e.log(); log != "" {
		t.Errorf("relinker or game ran for an unusable dump:\n%s", log)
	}
}

func TestLaunchPS5ConvertsOnceAndRuns(t *testing.T) {
	e := newPS5Env(t)
	dump := e.addGame("PPSA01234", true)
	out := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4", "PPSA01234")
	state := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234")

	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("first launch: %v\n%s", err, o)
	}
	log := e.log()
	if got := strings.Count(log, "relinker "); got != 1 {
		t.Fatalf("relinker ran %d times on the first launch, want 1:\n%s", got, log)
	}
	if !strings.Contains(log, "relinker "+dump+"/eboot.bin "+out+".") {
		t.Errorf("relinker not given the dump's eboot.bin and a temporary output beside %s:\n%s", out, log)
	}
	wantGame := "game pwd=" + state + " fonts=/usr/share/anyps5-fonts cache=" +
		filepath.Join(e.home, ".cache", "anyps5", "shader_cache", "PPSA01234") + " fullscreen=1"
	if !strings.Contains(log, wantGame) {
		t.Errorf("game did not run in its state dir with the image's settings; want %q in:\n%s", wantGame, log)
	}

	// The converted module stays; the game's files are links into the dump.
	if b, err := os.ReadFile(filepath.Join(out, "app0", "sce_module", "libgame.guest.prx")); err != nil || string(b) != "converted\n" {
		t.Errorf("converted module replaced or missing: %q, %v", b, err)
	}
	for _, name := range []string{"eboot.bin", "sce_sys", "data.pak"} {
		target, err := os.Readlink(filepath.Join(out, "app0", name))
		if err != nil || target != filepath.Join(dump, name) {
			t.Errorf("app0/%s: link to %q, %v; want a link to the dump's", name, target, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(out, "libs")); err != nil || target != filepath.Join(e.anyps5, "libs") {
		t.Errorf("libs: link to %q, %v; want the image's libraries", target, err)
	}

	// The same dump and version: no conversion, the game runs again.
	e.resetLog()
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("second launch: %v\n%s", err, o)
	}
	if log := e.log(); strings.Contains(log, "relinker ") || !strings.Contains(log, "game pwd=") {
		t.Errorf("second launch should only run the game:\n%s", log)
	}

	// A game update (a changed eboot.bin) converts again.
	e.write(filepath.Join(dump, "eboot.bin"), "\x7fELF-game-v2", 0o644)
	e.resetLog()
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch after update: %v\n%s", err, o)
	}
	if got := strings.Count(e.log(), "relinker "); got != 1 {
		t.Errorf("relinker ran %d times after the dump changed, want 1", got)
	}
}

func TestLaunchPS5ConvertsAgainForNewVersionAndSweepsStaleOnes(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	e.addGame("PPSA05678", true)
	cache := filepath.Join(e.home, ".cache", "anyps5")
	for _, title := range []string{"PPSA01234", "PPSA05678"} {
		if o, err := e.run(title); err != nil {
			t.Fatalf("launch %s on v0.1.4: %v\n%s", title, err, o)
		}
	}
	// PPSA05678's v0.1.4 conversion was last used 8 days ago; PPSA01234's is fresh.
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(cache, "v0.1.4", "PPSA05678", ".converted"), stale, stale); err != nil {
		t.Fatal(err)
	}

	e.setVersion("v0.1.5")
	e.resetLog()
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch on v0.1.5: %v\n%s", err, o)
	}
	if got := strings.Count(e.log(), "relinker "); got != 1 {
		t.Errorf("relinker ran %d times on a new AnyPS5 version, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(cache, "v0.1.5", "PPSA01234", "app.elf")); err != nil {
		t.Errorf("no v0.1.5 conversion: %v", err)
	}
	// A session still on the older image may be running the fresh one.
	if _, err := os.Stat(filepath.Join(cache, "v0.1.4", "PPSA01234", "app.elf")); err != nil {
		t.Errorf("recently used v0.1.4 conversion was swept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "v0.1.4", "PPSA05678")); !os.IsNotExist(err) {
		t.Errorf("week-old conversion kept: %v", err)
	}
}

func TestLaunchPS5GivesModulelessDumpAnEmptyModuleDir(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", false)
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	log := e.log()
	if !strings.Contains(log, "relinker --module-dir ") || !strings.Contains(log, "/no-modules ") {
		t.Errorf("relinker not given an empty --module-dir for a dump without modules:\n%s", log)
	}
	out := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4", "PPSA01234")
	if _, err := os.Stat(filepath.Join(out, "no-modules")); !os.IsNotExist(err) {
		t.Errorf("empty module dir left in the conversion: %v", err)
	}
}

func TestLaunchPS5ReturnsGameStatus(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	_, err := e.run("PPSA01234", "GAME_STATUS=134")
	if exitCode(err) != 134 {
		t.Errorf("crashed game: got %v, want exit status 134", err)
	}
}

// ps5-session must stop sway however the game ends: GoW's own `&& killall
// sway` skips a crashed game's, leaving the session up with nothing in it.
func TestPS5SessionStopsSwayAfterFailedGame(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	bin := filepath.Join(dir, "bin")
	writeFile(t, filepath.Join(bin, "killall"), "#!/bin/sh\necho \"killall $*\" >>"+log+"\n", 0o755)
	script, err := os.ReadFile("root/opt/direwolf/ps5-session")
	if err != nil {
		t.Fatal(err)
	}
	// Point it at a launch-ps5 that fails.
	fake := filepath.Join(dir, "launch-ps5")
	writeFile(t, fake, "#!/bin/sh\nexit 3\n", 0o755)
	body := strings.Replace(string(script), "/opt/direwolf/launch-ps5", fake, 1)
	if body == string(script) {
		t.Fatal("ps5-session no longer runs /opt/direwolf/launch-ps5")
	}
	session := filepath.Join(dir, "ps5-session")
	writeFile(t, session, body, 0o755)
	cmd := exec.CommandContext(t.Context(), "sh", session)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	if code := exitCode(cmd.Run()); code != 3 {
		t.Errorf("exit status %d, want the game's 3", code)
	}
	if b, _ := os.ReadFile(log); string(b) != "killall sway\n" {
		t.Errorf("sway not stopped after a failed game: %q", b)
	}
}
