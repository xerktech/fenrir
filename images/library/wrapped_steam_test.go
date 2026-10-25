package library

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const wrappedSteam = "root/usr/bin/wrapped-steam"

// steamDesktop is a Library desktop with Steam, Heroic and Xodus Nexus stubbed. The steam
// stub is a script named steam, so the real `pgrep -x steam` sees it run.
type steamDesktop struct {
	t                     *testing.T
	dir, home, log, wrap  string
	steamLife, heroicLife string
}

func newSteamDesktop(t *testing.T) *steamDesktop {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("needs util-linux flock")
	}
	if out, err := exec.CommandContext(t.Context(), "pgrep", "-x", "steam|steam\\.sh|heroic|xodus-nexus.bin").Output(); err == nil {
		t.Skipf("a real Steam or Heroic runs on this host: %s", out)
	}
	d := &steamDesktop{t: t, dir: t.TempDir(), steamLife: "3", heroicLife: "3"}
	d.home = filepath.Join(d.dir, "home")
	d.log = filepath.Join(d.dir, "log")
	bin := filepath.Join(d.dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A cold start carries wrapped-steam's preloads; a hand-off to a running
	// client does not, and returns at once as the real client's does.
	writeStub(t, filepath.Join(bin, "steam"), `if [ -n "$LD_PRELOAD" ]; then echo cold >>"$LOG"; sleep "$STEAM_LIFE"
else echo "handoff $*" >>"$LOG"; fi`)
	writeStub(t, filepath.Join(bin, "wrapped-heroic"), `echo heroic >>"$LOG"; sleep "$HEROIC_LIFE"`)
	// Logs Steam's preloads or the launch lock if it inherited either.
	writeStub(t, filepath.Join(bin, "xodus-nexus"), `echo nexus >>"$LOG"
[ -n "$LD_PRELOAD" ] && echo nexus-preload >>"$LOG"
[ -e /proc/self/fd/9 ] && echo nexus-lock >>"$LOG"; exit 0`)
	// The user-namespace probe, slowed so two launches overlap in it.
	writeStub(t, filepath.Join(bin, "bwrap"), `sleep 1; exit 1`)
	src, err := os.ReadFile(wrappedSteam)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.NewReplacer(
		"/usr/bin/steam ", filepath.Join(bin, "steam")+" ",
		"/usr/bin/wrapped-heroic ", filepath.Join(bin, "wrapped-heroic")+" ",
		"/usr/bin/xodus-nexus ", filepath.Join(bin, "xodus-nexus")+" ",
	).Replace(string(src))
	if strings.Contains(patched, "exec /usr/bin/steam") || strings.Contains(patched, "-f /usr/bin/wrapped-heroic") ||
		strings.Contains(patched, "-f /usr/bin/xodus-nexus") {
		t.Fatal("wrapped-steam runs Steam, Heroic or Nexus from a path this test does not stub")
	}
	d.wrap = filepath.Join(d.dir, "wrapped-steam")
	if err := os.WriteFile(d.wrap, []byte(patched), 0o600); err != nil { //nolint:gosec // d.wrap is under t.TempDir
		t.Fatal(err)
	}
	return d
}

// start runs wrapped-steam with args, returning a wait func.
func (d *steamDesktop) start(args ...string) func() {
	d.t.Helper()
	cmd := exec.CommandContext(d.t.Context(), "bash", append([]string{d.wrap}, args...)...) //nolint:gosec // the test's own script
	cmd.Env = append(os.Environ(),
		"HOME="+d.home, "LOG="+d.log, "STEAM_LIFE="+d.steamLife, "HEROIC_LIFE="+d.heroicLife,
		"PATH="+filepath.Join(d.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	return func() {
		if err := cmd.Wait(); err != nil {
			d.t.Errorf("wrapped-steam: %v", err)
		}
	}
}

func (d *steamDesktop) calls() []string {
	d.t.Helper()
	b, err := os.ReadFile(d.log)
	if err != nil {
		d.t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// waitHeroics waits until n Heroic and n Nexus starts are logged. Both are
// setsid'd and may log after Steam exits; a write after the test ends races
// t.TempDir's cleanup ("directory not empty").
func (d *steamDesktop) waitHeroics(n int) {
	d.t.Helper()
	d.waitStarts("heroic", n)
	d.waitStarts("nexus", n)
}

func (d *steamDesktop) waitStarts(word string, n int) {
	d.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); count(d.calls(), word) < n; {
		if time.Now().After(deadline) {
			d.t.Fatalf("calls %q: want %d %s starts", d.calls(), n, word)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func count(calls []string, word string) int {
	n := 0
	for _, c := range calls {
		if c == word {
			n++
		}
	}
	return n
}

func TestWrappedSteamConcurrentColdLaunchesStartSteamOnce(t *testing.T) {
	d := newSteamDesktop(t)
	var wg sync.WaitGroup
	for _, arg := range []string{"steam://open/games", "steam://open/downloads"} {
		wait := d.start(arg)
		wg.Go(wait)
	}
	wg.Wait()
	calls := d.calls()
	if count(calls, "cold") != 1 || count(calls, "heroic") != 1 || count(calls, "handoff") != 1 {
		t.Errorf("calls %q: want Steam and Heroic started once, the other launch handed to the running client", calls)
	}
	d.waitStarts("nexus", 1)
	if calls = d.calls(); count(calls, "nexus") != 1 || count(calls, "nexus-preload") != 0 || count(calls, "nexus-lock") != 0 {
		t.Errorf("calls %q: want Nexus started once, without Steam's preloads or the launch lock", calls)
	}
}

func TestWrappedSteamLockOutlivesNoLaunch(t *testing.T) {
	d := newSteamDesktop(t)
	// Heroic outlives Steam here: had it inherited the lock, the next cold
	// launch would wait out the 30s timeout.
	d.steamLife, d.heroicLife = "0", "20"
	d.start()()
	begin := time.Now()
	d.start()()
	if took := time.Since(begin); took > 10*time.Second {
		t.Errorf("second cold launch took %v: the lock outlived the first launch", took)
	}
	d.waitHeroics(2)
	if calls := d.calls(); count(calls, "cold") != 2 {
		t.Errorf("calls %q: want two cold starts", calls)
	}
}

func TestWrappedSteamHandsOffWhileSteamShStarts(t *testing.T) {
	d := newSteamDesktop(t)
	// Valve's launcher runs steam.sh before any process is named steam.
	sh := filepath.Join(d.dir, "bin", "steam.sh")
	writeStub(t, sh, `sleep 5`)
	bootstrap := exec.CommandContext(t.Context(), sh)
	if err := bootstrap.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Process.Kill(); _ = bootstrap.Wait() })
	time.Sleep(200 * time.Millisecond) // let it exec, so pgrep sees its name
	// Only a cold launch clears the CEF debug flag; a hand-off starts nothing.
	flag := writeCEFDebugFlag(t, d.home, ".local/share/Steam")
	d.start("steam://open/games")()
	if calls := d.calls(); count(calls, "cold") != 0 || count(calls, "handoff") != 1 {
		t.Errorf("calls %q: want the launch handed to the starting client", calls)
	}
	if _, err := os.Lstat(flag); err != nil {
		t.Errorf("a hand-off touched the CEF debug flag: %v", err)
	}
}

func TestWrappedSteamColdLaunchDeletesCEFDebugFlag(t *testing.T) {
	d := newSteamDesktop(t)
	d.steamLife, d.heroicLife = "0", "0"
	// Both names of the shared install: Valve's launcher here and ~/.steam/steam.
	flags := []string{
		writeCEFDebugFlag(t, d.home, ".local/share/Steam"),
		writeCEFDebugFlag(t, d.home, ".steam/steam"),
	}
	d.start()()
	d.waitHeroics(1)
	for _, p := range flags {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a cold launch (%v): Steam would open its CEF debugger", p, err)
		}
	}
}

// writeCEFDebugFlag writes Heroic's CEF debug flag into dir under home.
func writeCEFDebugFlag(t *testing.T, home, dir string) string {
	t.Helper()
	p := filepath.Join(home, dir, ".cef-enable-remote-debugging")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Steam restarting itself in place (exit 42) skips wrapped-steam, so a flag
// Heroic writes while Steam runs must go too: a guard deletes it until Steam
// exits, then exits itself.
func TestWrappedSteamGuardDeletesCEFDebugFlagWhileSteamRuns(t *testing.T) {
	d := newSteamDesktop(t)
	d.steamLife, d.heroicLife = "4", "0"
	wait := d.start()
	for deadline := time.Now().Add(5 * time.Second); count(d.callsSoFar(), "cold") == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Steam never started")
		}
	}
	flag := writeCEFDebugFlag(t, d.home, ".local/share/Steam")
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Lstat(flag); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a flag written while Steam ran survived: a restart in place would open the CEF debugger")
		}
	}
	wait()
	d.waitHeroics(1)
	lock := filepath.Join(d.home, ".local", "state", "library", "cef-flag-guard.lock")
	for deadline := time.Now().Add(5 * time.Second); exec.CommandContext(t.Context(), "flock", "-n", lock, "true").Run() != nil; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the guard outlived Steam")
		}
	}
}

// A running Heroic is left alone: the cold launch starts only Steam.
func TestWrappedSteamKeepsRunningHeroic(t *testing.T) {
	d := newSteamDesktop(t)
	d.steamLife = "0"
	heroic := filepath.Join(d.dir, "bin", "heroic")
	writeStub(t, heroic, `sleep 10`)
	running := exec.CommandContext(t.Context(), heroic)
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Process.Kill(); _ = running.Wait() })
	time.Sleep(200 * time.Millisecond) // let it exec, so pgrep sees its name
	d.start()()
	d.waitStarts("nexus", 1)
	time.Sleep(500 * time.Millisecond) // a setsid'd Heroic would have logged by now
	if calls := d.calls(); count(calls, "cold") != 1 || count(calls, "heroic") != 0 {
		t.Errorf("calls %q: want Steam started and no second Heroic", calls)
	}
}

// callsSoFar is calls before anything has logged.
func (d *steamDesktop) callsSoFar() []string {
	d.t.Helper()
	b, err := os.ReadFile(d.log)
	if err != nil && !os.IsNotExist(err) {
		d.t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// Across an exit-42 restart only steam.sh runs, for seconds: the guard must
// keep going while it does.
func TestWrappedSteamGuardOutlastsSteamShRestartGap(t *testing.T) {
	d := newSteamDesktop(t)
	d.steamLife, d.heroicLife = "1", "0"
	wait := d.start()
	for deadline := time.Now().Add(5 * time.Second); count(d.callsSoFar(), "cold") == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Steam never started")
		}
	}
	// Started after the cold launch, which would otherwise hand off to it.
	sh := filepath.Join(d.dir, "bin", "steam.sh")
	writeStub(t, sh, `sleep 6`)
	restart := exec.CommandContext(t.Context(), sh)
	if err := restart.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restart.Process.Kill(); _ = restart.Wait() })
	wait()
	d.waitHeroics(1)
	time.Sleep(1500 * time.Millisecond) // past the guard's next check with no steam running
	flag := writeCEFDebugFlag(t, d.home, ".steam/steam")
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Lstat(flag); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the guard stopped while steam.sh ran: the restarted client would open the CEF debugger")
		}
	}
}

// A cold launch right after Steam quit finds the last guard's lock still held
// for a moment: it waits for it rather than run unguarded.
func TestWrappedSteamGuardWaitsOutLastGuard(t *testing.T) {
	d := newSteamDesktop(t)
	d.steamLife, d.heroicLife = "4", "0"
	lock := filepath.Join(d.home, ".local", "state", "library", "cef-flag-guard.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	last := exec.CommandContext(t.Context(), "flock", lock, "sleep", "1.5")
	if err := last.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = last.Wait() })
	time.Sleep(100 * time.Millisecond) // let it take the lock
	wait := d.start()
	for deadline := time.Now().Add(5 * time.Second); count(d.callsSoFar(), "cold") == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Steam never started")
		}
	}
	time.Sleep(time.Second) // the cold launch's own delete is behind us
	flag := writeCEFDebugFlag(t, d.home, ".local/share/Steam")
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Lstat(flag); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no guard after a relaunch that found the lock held")
		}
	}
	wait()
	d.waitHeroics(1)
}

// A running Nexus is left alone, like Heroic.
func TestWrappedSteamKeepsRunningNexus(t *testing.T) {
	d := newSteamDesktop(t)
	d.steamLife, d.heroicLife = "0", "0"
	nexus := filepath.Join(d.dir, "bin", "xodus-nexus.bin")
	writeStub(t, nexus, `sleep 10`)
	running := exec.CommandContext(t.Context(), nexus)
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Process.Kill(); _ = running.Wait() })
	time.Sleep(200 * time.Millisecond) // let it exec, so pgrep sees its name
	d.start()()
	d.waitStarts("heroic", 1)
	time.Sleep(500 * time.Millisecond) // a setsid'd Nexus would have logged by now
	if calls := d.calls(); count(calls, "nexus") != 0 {
		t.Errorf("calls %q: want no second Nexus", calls)
	}
}
