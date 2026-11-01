package anyps5

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const launchPS5 = "root/opt/direwolf/launch-ps5"

// stubRelinker stands in for AnyPS5's relinker: it logs its arguments, writes
// the "converted program" (a script that logs where and how it ran) and a
// converted module under app0/, as the real one does beside its output. Like
// the real one (v0.1.4, NativeModules.cpp), it refuses an input with no module
// directory beside it and any --module-dir outside the input's directory.
const stubRelinker = `#!/bin/sh
echo "relinker $*" >>"$STUB_LOG"
case " $* " in *" --module-dir "*) echo "FAIL: --module-dir must be a directory inside the guest module parent directory" >&2; exit 2 ;; esac
for out; do :; done
in=$1
src=$(dirname "$in")
[ -d "$src/sce_module" ] || [ -d "$src/sce_modules" ] || [ -d "$src/prx" ] || { echo "FAIL: no module directory beside $in" >&2; exit 2; }
dir=$(dirname "$out")
mkdir -p "$dir/app0/sce_module"
echo converted >"$dir/app0/sce_module/libgame.guest.prx"
cat >"$out" <<'EOF'
#!/bin/sh
app0=absent; [ -f app0/eboot.bin ] && app0=$(cat app0/eboot.bin)
mkdir -p data && echo saved >data/save.dat
fd9=closed; [ -e /proc/$$/fd/9 ] && fd9=open
[ -e /proc/$$/fd/8 ] && fd9=fd8-open
app0type=link; [ -d app0 ] && [ ! -L app0 ] && app0type=dir
echo "game pwd=$(pwd) app0=$app0 app0type=$app0type fd9=$fd9 fonts=$ANYPS5_SYSTEM_FONTS cache=$ANYPS5_SHADER_CACHE_DIR fullscreen=$ANYPS5_FULLSCREEN" >>"$STUB_LOG"
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
	wantGame := "game pwd=" + state + " app0=\x7fELF-game app0type=dir fd9=closed fonts=/usr/share/anyps5-fonts cache=" +
		filepath.Join(e.home, ".cache", "anyps5", "shader_cache", "PPSA01234") + " fullscreen=1"
	if !strings.Contains(log, wantGame) {
		t.Errorf("game did not run in its state dir, with app0 reachable and the image's settings; want %q in:\n%s", wantGame, log)
	}
	// AnyPS5 makes the working directory the console's root: ./app0 is a real
	// directory there (a chdir into it must stay inside the root) of links to
	// the conversion's entries, and what the game writes stays outside it.
	if target, err := os.Readlink(filepath.Join(state, "app0", "sce_module")); err != nil || target != filepath.Join(out, "app0", "sce_module") {
		t.Errorf("state app0/sce_module: link to %q, %v; want the conversion's", target, err)
	}
	if target, err := os.Readlink(filepath.Join(state, "app0", "data.pak")); err != nil || target != filepath.Join(dump, "data.pak") {
		t.Errorf("state app0/data.pak: link to %q, %v; want the dump's", target, err)
	}
	if _, err := os.Stat(filepath.Join(state, "data", "save.dat")); err != nil {
		t.Errorf("game data not kept in the state dir: %v", err)
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

	// Only the program is cached per version; saves are not in it.
	if _, err := os.Stat(filepath.Join(out, "data")); !os.IsNotExist(err) {
		t.Errorf("game data written into the versioned conversion: %v", err)
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
	// The game's app0 now points at the new conversion's modules.
	state := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234")
	want := filepath.Join(cache, "v0.1.5", "PPSA01234", "app0", "sce_module")
	if target, err := os.Readlink(filepath.Join(state, "app0", "sce_module")); err != nil || target != want {
		t.Errorf("app0/sce_module after a bump: link to %q, %v; want %q", target, err, want)
	}
}

// app0 drops links the current conversion lacks (a file removed from the dump)
// and keeps anything that is not a link.
func TestLaunchPS5App0FollowsTheConversion(t *testing.T) {
	e := newPS5Env(t)
	dump := e.addGame("PPSA01234", true)
	state := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234")
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if err := os.Remove(filepath.Join(dump, "data.pak")); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(dump, "eboot.bin"), "\x7fELF-game-v2", 0o644)
	e.write(filepath.Join(state, "app0", "written-by-game"), "x", 0o644)
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("relaunch: %v\n%s", err, o)
	}
	if _, err := os.Lstat(filepath.Join(state, "app0", "data.pak")); !os.IsNotExist(err) {
		t.Errorf("link to a removed dump file kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "app0", "written-by-game")); err != nil {
		t.Errorf("non-link entry in app0 removed: %v", err)
	}
}

// Anything in app0 that is not a link stays, even under a name the dump has.
func TestLaunchPS5KeepsNonLinksInApp0(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	state := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234")
	e.write(filepath.Join(state, "app0", "data.pak"), "mine", 0o644)
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if b, err := os.ReadFile(filepath.Join(state, "app0", "data.pak")); err != nil || string(b) != "mine" {
		t.Errorf("non-link app0/data.pak replaced: %q, %v", b, err)
	}
}

// A launch killed mid-rebuild leaves its temporary link; the next launch
// must clear it, whether it points at a file or a directory, and never write
// through it.
func TestLaunchPS5ClearsAKilledApp0Rebuild(t *testing.T) {
	for _, into := range []string{"eboot.bin", "sce_sys"} {
		e := newPS5Env(t)
		dump := e.addGame("PPSA01234", true)
		state := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234")
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dump, into), filepath.Join(state, ".app0-link")); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadDir(filepath.Join(dump, "sce_sys"))
		if err != nil {
			t.Fatal(err)
		}
		if o, runErr := e.run("PPSA01234"); runErr != nil {
			t.Errorf("leftover link to %s: launch failed: %v\n%s", into, runErr, o)
		}
		after, err := os.ReadDir(filepath.Join(dump, "sce_sys"))
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Errorf("leftover link to %s: launch wrote into the dump: %d entries, want %d", into, len(after), len(before))
		}
		if _, err := os.Lstat(filepath.Join(state, ".app0-link")); !os.IsNotExist(err) {
			t.Errorf("leftover link to %s: temporary link kept: %v", into, err)
		}
	}
}

// Relaunches rebuild app0 under a running game: no entry may ever be missing.
func TestLaunchPS5App0NeverMissesAnEntry(t *testing.T) {
	e := newPS5Env(t)
	dump := e.addGame("PPSA01234", true)
	for i := range 50 {
		e.write(filepath.Join(dump, "file"+strconv.Itoa(i)), "x", 0o644)
	}
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	app0 := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234", "app0")
	entries, err := os.ReadDir(app0)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	missing := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				missing <- n
				return
			default:
			}
			for _, entry := range entries {
				if _, err := os.Lstat(filepath.Join(app0, entry.Name())); err != nil {
					n++
				}
			}
		}
	}()
	for range 10 {
		if o, err := e.run("PPSA01234"); err != nil {
			t.Errorf("relaunch: %v\n%s", err, o)
		}
	}
	close(stop)
	if n := <-missing; n != 0 {
		t.Errorf("app0 entries missing %d times during relaunches", n)
	}
}

// The sweep rechecks staleness once it holds the lock: a launch may have used
// the conversion between find and flock. A flock wrapper stands in for that
// launch, touching the conversion just before the sweep's flock -n.
func TestLaunchPS5SweepRechecksUnderTheLock(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	e.addGame("PPSA05678", true)
	for _, title := range []string{"PPSA01234", "PPSA05678"} {
		if o, err := e.run(title); err != nil {
			t.Fatalf("launch %s: %v\n%s", title, err, o)
		}
	}
	cache := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4")
	stamp := filepath.Join(cache, "PPSA05678", ".converted")
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stamp, stale, stale); err != nil {
		t.Fatal(err)
	}
	realFlock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("no flock")
	}
	bin := filepath.Join(t.TempDir(), "bin")
	writeFile(t, filepath.Join(bin, "flock"), "#!/bin/sh\n[ \"$1\" = -n ] && touch "+stamp+"\nexec "+realFlock+" \"$@\"\n", 0o755)
	if o, err := e.run("PPSA01234", "PATH="+bin+":"+os.Getenv("PATH")); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if _, err := os.Stat(filepath.Join(cache, "PPSA05678", "app.elf")); err != nil {
		t.Errorf("conversion used after find looked was swept: %v", err)
	}
}

// A session on the old image and one on the new start the same game together
// during a rollout: both must run.
func TestLaunchPS5ConcurrentLaunchesAcrossVersions(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	for i := range 40 {
		e.write(filepath.Join(e.games, "PPSA01234", "file"+strconv.Itoa(i)), "x", 0o644)
	}
	other := filepath.Join(t.TempDir(), "opt-anyps5-new")
	e.write(filepath.Join(other, "relinker"), stubRelinker, 0o755)
	e.write(filepath.Join(other, "VERSION"), "v0.1.5\n", 0o644)
	if err := os.MkdirAll(filepath.Join(other, "libs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for trial := range 5 {
		errs := make(chan error, 2)
		for _, home := range []string{e.anyps5, other} {
			go func() {
				o, err := e.run("PPSA01234", "ANYPS5_HOME="+home)
				if err != nil {
					err = fmt.Errorf("%w: %s", err, o)
				}
				errs <- err
			}()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Errorf("trial %d: %v", trial, err)
			}
		}
	}
}

func TestLaunchPS5ConvertsModulelessDumpFromACopy(t *testing.T) {
	e := newPS5Env(t)
	dump := e.addGame("PPSA01234", false)
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	log := e.log()
	if strings.Contains(log, "--module-dir") || strings.Contains(log, "relinker "+dump+"/eboot.bin") {
		t.Errorf("relinker should get a copy of eboot.bin beside an empty sce_module:\n%s", log)
	}
	if !strings.Contains(log, "game pwd=") {
		t.Errorf("game did not run:\n%s", log)
	}
	out := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4", "PPSA01234")
	if _, err := os.Stat(filepath.Join(out, "src")); !os.IsNotExist(err) {
		t.Errorf("staging copy left in the conversion: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(out, "app0", "eboot.bin")); err != nil || target != filepath.Join(dump, "eboot.bin") {
		t.Errorf("app0/eboot.bin: link to %q, %v; want the dump's", target, err)
	}
}

func TestLaunchPS5RejectsPathLikeVersions(t *testing.T) {
	for _, v := range []string{"..", ".", ".hidden", "v1/../..", ""} {
		e := newPS5Env(t)
		e.addGame("PPSA01234", true)
		keep := filepath.Join(e.home, ".cache", "PPSA01234", "keep")
		e.write(keep, "x", 0o644)
		e.setVersion(v)
		if out, err := e.run("PPSA01234"); err == nil || !strings.Contains(out, "bad AnyPS5 version") {
			t.Errorf("version %q: err %v, output %q", v, err, out)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("version %q: file outside the cache removed: %v", v, err)
		}
	}
}

func TestLaunchPS5LinksDotfilesAndFollowsLinkedEboot(t *testing.T) {
	e := newPS5Env(t)
	dump := e.addGame("PPSA01234", true)
	e.write(filepath.Join(dump, "..hidden"), "x", 0o644)
	e.write(filepath.Join(dump, ".dot"), "x", 0o644)
	// eboot.bin as a link to the real file, as some dump layouts have.
	target := filepath.Join(e.games, "store", "eboot-v1.bin")
	e.write(target, "\x7fELF-game", 0o644)
	if err := os.Remove(filepath.Join(dump, "eboot.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dump, "eboot.bin")); err != nil {
		t.Fatal(err)
	}
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	out := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4", "PPSA01234")
	for _, name := range []string{"..hidden", ".dot"} {
		if _, err := os.Readlink(filepath.Join(out, "app0", name)); err != nil {
			t.Errorf("app0/%s not linked: %v", name, err)
		}
	}
	// Updating the linked file (a game update) converts again.
	e.write(target, "\x7fELF-game-v2", 0o644)
	e.resetLog()
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("relaunch: %v\n%s", err, o)
	}
	if got := strings.Count(e.log(), "relinker "); got != 1 {
		t.Errorf("relinker ran %d times after the linked eboot changed, want 1", got)
	}
}

func TestLaunchPS5ClearsAKilledConversion(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	partial := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4", "PPSA01234.partial")
	e.write(filepath.Join(partial, "app.elf"), "half", 0o644)
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("killed conversion's leftovers kept: %v", err)
	}
}

func TestLaunchPS5ConcurrentFirstLaunchesConvertOnce(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	// A slow relinker, so the second launch arrives mid-conversion.
	e.write(filepath.Join(e.anyps5, "relinker"), strings.Replace(stubRelinker, "for out;", "sleep 1\nfor out;", 1), 0o755)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := e.run("PPSA01234")
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("launch: %v", err)
		}
	}
	if got := strings.Count(e.log(), "relinker "); got != 1 {
		t.Errorf("relinker ran %d times for two concurrent first launches, want 1", got)
	}
}

// The sweep must not remove a conversion while a launch holds its title's
// lock (it may be converting that title again), nor the lock file itself.
func TestLaunchPS5SweepSkipsLockedConversions(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	e.addGame("PPSA05678", true)
	for _, title := range []string{"PPSA01234", "PPSA05678"} {
		if o, err := e.run(title); err != nil {
			t.Fatalf("launch %s: %v\n%s", title, err, o)
		}
	}
	cache := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4")
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(cache, "PPSA05678", ".converted"), stale, stale); err != nil {
		t.Fatal(err)
	}
	// Another launch of PPSA05678 holds its lock for the whole run below.
	lock := filepath.Join(cache, "PPSA05678.lock")
	// One process holding the lock (flock(1) would leave it held by its child).
	holder := exec.CommandContext(t.Context(), "bash", "-c", `exec 9>>"$1"; flock 9; exec sleep 30`, "holder", lock)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for exec.CommandContext(t.Context(), "flock", "-n", lock, "true").Run() == nil {
		if time.Now().After(deadline) {
			t.Fatal("lock holder never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if _, err := os.Stat(filepath.Join(cache, "PPSA05678", "app.elf")); err != nil {
		t.Errorf("conversion swept while its lock was held: %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("lock file removed: %v", err)
	}
	// Once the lock is free, the week-old conversion goes; its lock file stays.
	_ = holder.Process.Kill()
	_ = holder.Wait()
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if _, err := os.Stat(filepath.Join(cache, "PPSA05678")); !os.IsNotExist(err) {
		t.Errorf("unlocked week-old conversion kept: %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("lock file removed: %v", err)
	}
}

// A stale .partial of another title is swept; a fresh one is kept.
func TestLaunchPS5SweepsStalePartials(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	cache := filepath.Join(e.home, ".cache", "anyps5", "v0.1.4")
	stalePartial := filepath.Join(cache, "PPSA05678.partial")
	freshPartial := filepath.Join(cache, "PPSA09999.partial")
	e.write(filepath.Join(stalePartial, "app.elf"), "half", 0o644)
	e.write(filepath.Join(freshPartial, "app.elf"), "half", 0o644)
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stalePartial, stale, stale); err != nil {
		t.Fatal(err)
	}
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if _, err := os.Stat(stalePartial); !os.IsNotExist(err) {
		t.Errorf("week-old partial kept: %v", err)
	}
	if _, err := os.Stat(freshPartial); err != nil {
		t.Errorf("fresh partial swept: %v", err)
	}
}

// A state dir from a build that linked app0 whole becomes a real directory.
func TestLaunchPS5ReplacesALinkedApp0(t *testing.T) {
	e := newPS5Env(t)
	e.addGame("PPSA01234", true)
	state := filepath.Join(e.home, ".local", "share", "anyps5", "PPSA01234")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.games, filepath.Join(state, "app0")); err != nil {
		t.Fatal(err)
	}
	if o, err := e.run("PPSA01234"); err != nil {
		t.Fatalf("launch: %v\n%s", err, o)
	}
	if !strings.Contains(e.log(), "app0type=dir") {
		t.Errorf("app0 still a link:\n%s", e.log())
	}
	if _, err := os.Stat(filepath.Join(e.games, "PPSA01234", "eboot.bin")); err != nil {
		t.Errorf("replacing the old app0 link touched its target: %v", err)
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
