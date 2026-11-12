package library

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const xodusNexusGamePatches = "root/usr/local/bin/xodus-nexus-game-patches"

const patchesShare = "root/usr/share/xodus-nexus/xgameruntime"

const patchRuntimeTag = "runtime-11.0-20261028-ge5afe21e"

// fakeDLL is a PE-looking module; builtin ones carry Wine's marker at 0x40,
// where Wine and Proton look for it.
func fakeDLL(builtin bool, body string) []byte {
	b := make([]byte, 128, 128+len(body))
	copy(b, "MZ")
	marker := "!This program cannot be run in DOS mode."
	if builtin {
		marker = "Wine builtin DLL"
	}
	copy(b[64:], marker)
	return append(b, body...)
}

// patchHost is a desktop home with an active Nexus runtime, one installed
// game per exe name, each with a prefix, and a fake GitHub serving the
// patched-DLL release.
type patchHost struct {
	t                                 *testing.T
	dir, home, log, gh, compat, share string
	builtin                           []byte
}

func newPatchHost(t *testing.T, exes ...string) *patchHost {
	t.Helper()
	dir := t.TempDir()
	h := &patchHost{t: t, dir: dir, home: filepath.Join(dir, "home"), log: filepath.Join(dir, "log"),
		gh: filepath.Join(dir, "gh"), share: filepath.Join(dir, "share")}
	h.compat = filepath.Join(h.home, ".local", "share", "xodus-nexus", "compatdata")
	for _, d := range []string{filepath.Join(dir, "bin"), h.gh, filepath.Join(h.share, "patches")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The real patch series and game list, so the test uses the image's id.
	for _, f := range []string{"games", "patches/0002-xuser-catch-all-endpoint.patch"} {
		b, err := os.ReadFile(filepath.Join(patchesShare, f))
		if err != nil {
			t.Fatal(err)
		}
		h.write(filepath.Join(h.share, f), b)
	}
	if len(exes) > 0 {
		h.write(filepath.Join(h.share, "games"), []byte("# listed\n"+strings.Join(exes, "\n")+"\n"))
	}
	// cp fails while FAIL_CP is set, else is the real one.
	writeStub(t, filepath.Join(dir, "bin", "cp"), `[ -n "$FAIL_CP" ] && exit 1; exec /bin/cp "$@"`)
	writeStub(t, filepath.Join(dir, "bin", "curl"), `for a; do [ "$prev" = -o ] && out=$a; prev=$a; url=$a; done
case "$url" in https://api.github.com/repos/xerktech/fenrir/releases/tags/*)
  echo "release ${url##*/}" >>"$LOG"; f="$FAKE_GH/release-${url##*/}.json"; [ -f "$f" ] || exit 22; exec cat "$f";;
esac
echo "download ${url##*/}" >>"$LOG"
f="$FAKE_GH/${url##*/}"; [ -f "$f" ] || exit 22
if [ -n "$out" ]; then cp "$f" "$out"; else cat "$f"; fi`)

	rel := filepath.Join(h.home, ".local", "share", "xodus-nexus", "runtimes", "releases", "nexus-runtime-11.0-experimental-ge5afe21e")
	h.builtin = fakeDLL(true, "upstream")
	h.write(filepath.Join(rel, "files", "lib", "wine", "x86_64-windows", "xgameruntime.dll"), h.builtin)
	h.write(filepath.Join(rel, "files", "bin", "wine"), []byte("#!/bin/sh\n"))
	h.write(filepath.Join(h.home, ".local", "share", "xodus-nexus", "runtimes", "current.json"),
		[]byte(`{"name":"nexus-runtime-11.0-experimental-ge5afe21e","winePath":"`+filepath.Join(rel, "files", "bin", "wine")+`","commit":"`+patchRuntimeTag+`"}`))
	return h
}

func (h *patchHost) write(path string, b []byte) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // path is under t.TempDir
		h.t.Fatal(err)
	}
}

// games installs one Nexus game per id with its exe as the executable, each
// with a prefix unless listed in noPrefix, and records them in launcher-state.json.
func (h *patchHost) games(games map[string]string, noPrefix ...string) {
	h.t.Helper()
	entries := make([]string, 0, len(games))
	for id, exe := range games {
		path := filepath.Join(h.dir, "games", id)
		h.write(filepath.Join(path, "MicrosoftGame.Config"),
			[]byte(`<Game configVersion="1"><ExecutableList><Executable Name="`+exe+`" Id="Game"/></ExecutableList></Game>`))
		entries = append(entries, `{"id":"`+id+`","title":"`+id+`","path":"`+path+`"}`)
		skip := false
		for _, n := range noPrefix {
			skip = skip || n == id
		}
		if !skip {
			h.write(filepath.Join(h.compat, id, "pfx", "drive_c", "windows", "system32", "xgameruntime.dll"), h.builtin)
			h.write(filepath.Join(h.compat, id, "pfx", "user.reg"), []byte("WINE REGISTRY Version 2\n"))
		}
	}
	h.write(filepath.Join(h.home, ".config", "xodus-nexus", "launcher-state.json"),
		[]byte(`{"settings":{},"games":[`+strings.Join(entries, ",")+`]}`))
}

// publish serves dll as the release for this runtime and the image's patch
// series, with digest as GitHub's SHA-256 (dll's own when empty).
func (h *patchHost) publish(dll []byte, digest string) string {
	h.t.Helper()
	id := strings.TrimSpace(h.cmd("bash", "../../hack/nexus-xgameruntime/patch-id.sh", filepath.Join(h.share, "patches")))
	tag := "nexus-xgameruntime-" + patchRuntimeTag + "-" + id
	if digest == "" {
		digest = "sha256:" + sha256Hex(string(dll))
	}
	h.write(filepath.Join(h.gh, "release-"+tag+".json"), []byte(`{"assets":[{"name":"xgameruntime.dll","browser_download_url":"https://github.com/xerktech/fenrir/releases/download/`+tag+`/xgameruntime.dll","size":`+strconv.Itoa(len(dll))+`,"digest":"`+digest+`"}]}`))
	h.write(filepath.Join(h.gh, "xgameruntime.dll"), dll)
	return tag
}

func (h *patchHost) cmd(name string, args ...string) string {
	h.t.Helper()
	out, err := exec.CommandContext(h.t.Context(), name, args...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

// run runs the real script, with env added, and returns the stubs' calls and
// its output.
func (h *patchHost) run(env ...string) string {
	h.t.Helper()
	return runOneshot(h.t, xodusNexusGamePatches, h.log, append([]string{
		"PATH=" + filepath.Join(h.dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + h.home, "FAKE_GH=" + h.gh, "XODUS_NEXUS_PATCH_SHARE=" + h.share}, env...)...)
}

// runOneshot runs an s6 oneshot script with env (and LOG=log for its stubs),
// failing the test if it exits non-zero, and returns the stubs' calls and the
// script's output.
func runOneshot(t *testing.T, script, log string, env ...string) string {
	t.Helper()
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.WaitDelay = time.Second
	cmd.Env = append(append(os.Environ(), "LOG="+log), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s must never fail (a failed oneshot hangs the pod): %v\n%s", filepath.Base(script), err, out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls) + string(out)
}

func (h *patchHost) system32(id string) []byte {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.compat, id, "pfx", "drive_c", "windows", "system32", "xgameruntime.dll"))
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func (h *patchHost) userReg(id string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.compat, id, "pfx", "user.reg"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

const overrideKey = `[Software\\Wine\\AppDefaults\\007FirstLight.exe\\DllOverrides]`

// A listed game gets the DLL built for exactly this runtime and patch series
// in its own System32, plus the per-exe override; other games keep upstream's.
// A second start changes nothing and downloads nothing.
func TestXodusNexusGamePatchesInstallsForListedGamesOnly(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe", "other": "Other.exe"})
	patched := fakeDLL(false, "patched")
	tag := h.publish(patched, "")

	calls := h.run()
	if got := h.system32("g007"); !bytes.Equal(got, patched) {
		t.Fatalf("007's system32 DLL not the patched one:\n%s", calls)
	}
	if got := h.system32("other"); !bytes.Equal(got, h.builtin) {
		t.Errorf("an unlisted game's DLL changed")
	}
	reg := h.userReg("g007")
	if strings.Count(reg, overrideKey) != 1 || !strings.Contains(reg, overrideKey+" ") ||
		!strings.Contains(reg, "\n\"xgameruntime\"=\"native,builtin\"\n") {
		t.Errorf("007's override missing or malformed:\n%s", reg)
	}
	if strings.Contains(h.userReg("other"), "DllOverrides") {
		t.Errorf("an unlisted game got an override")
	}
	if !strings.Contains(calls, "release "+tag) || !strings.Contains(calls, "patched "+tag) {
		t.Errorf("calls:\n%s", calls)
	}

	calls = h.run()
	if strings.Contains(calls, "download ") || strings.Contains(calls, "release ") || strings.Count(h.userReg("g007"), overrideKey) != 1 {
		t.Errorf("a second start downloaded again or duplicated the override:\n%s\n%s", calls, h.userReg("g007"))
	}
}

// Without a release for this runtime and patch series (a new runtime CI has
// not built for yet, or patches that no longer apply), a previously patched
// game goes back to the active runtime's builtin.
func TestXodusNexusGamePatchesRestoresBuiltinWithoutARelease(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	h.write(filepath.Join(h.compat, "g007", "pfx", "drive_c", "windows", "system32", "xgameruntime.dll"), fakeDLL(false, "stale"))
	calls := h.run()
	if got := h.system32("g007"); !bytes.Equal(got, h.builtin) {
		t.Errorf("want upstream's builtin back:\n%s", calls)
	}
	if !strings.Contains(calls, "upstream builtin "+patchRuntimeTag) {
		t.Errorf("calls:\n%s", calls)
	}
}

// A download that doesn't verify, or is a Wine builtin, is never installed.
func TestXodusNexusGamePatchesRefusesUnverified(t *testing.T) {
	for test, tc := range map[string]struct {
		dll    []byte
		digest string
	}{
		"digest mismatch":   {fakeDLL(false, "patched"), "sha256:" + strings.Repeat("1", 64)},
		"builtin module":    {fakeDLL(true, "patched"), ""},
		"no digest at all":  {fakeDLL(false, "patched"), "none"},
		"malformed digest":  {fakeDLL(false, "patched"), "md5:00"},
		"empty module file": {[]byte{}, ""},
	} {
		t.Run(test, func(t *testing.T) {
			h := newPatchHost(t)
			h.games(map[string]string{"g007": "007FirstLight.exe"})
			h.publish(tc.dll, tc.digest)
			calls := h.run()
			if got := h.system32("g007"); !bytes.Equal(got, h.builtin) {
				t.Errorf("installed an unverified DLL:\n%s", calls)
			}
			if _, err := os.Stat(filepath.Join(h.home, ".cache", "xodus-nexus", "xgameruntime")); err == nil {
				entries, _ := filepath.Glob(filepath.Join(h.home, ".cache", "xodus-nexus", "xgameruntime", "*", "xgameruntime.dll"))
				if len(entries) > 0 {
					t.Errorf("cached an unverified DLL: %v", entries)
				}
			}
		})
	}
}

// A listed game without a prefix (never launched) is left for the next start;
// matching the executable name ignores case, as Windows does.
func TestXodusNexusGamePatchesSkipsMissingPrefixAndIgnoresCase(t *testing.T) {
	h := newPatchHost(t, "007firstlight.EXE")
	h.games(map[string]string{"g007": "007FirstLight.exe", "fresh": "007FirstLight.exe"}, "fresh")
	patched := fakeDLL(false, "patched")
	h.publish(patched, "")
	calls := h.run()
	if got := h.system32("g007"); !bytes.Equal(got, patched) {
		t.Errorf("case-insensitive match failed:\n%s", calls)
	}
	if !strings.Contains(calls, "no prefix yet") {
		t.Errorf("calls:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(h.compat, "fresh")); err == nil {
		t.Errorf("made a prefix for a game that has none")
	}
}

// No active runtime, or one without a builtin xgameruntime.dll, changes nothing.
func TestXodusNexusGamePatchesNeedsAnActiveRuntime(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	stale := fakeDLL(false, "stale")
	h.write(filepath.Join(h.compat, "g007", "pfx", "drive_c", "windows", "system32", "xgameruntime.dll"), stale)
	if err := os.Remove(filepath.Join(h.home, ".local", "share", "xodus-nexus", "runtimes", "current.json")); err != nil {
		t.Fatal(err)
	}
	calls := h.run()
	if got := h.system32("g007"); !bytes.Equal(got, stale) || !strings.Contains(calls, "no active runtime") {
		t.Errorf("changed a game's DLL without an active runtime:\n%s", calls)
	}
}

// The per-exe key may already exist, holding other DLLs or another
// xgameruntime value: the value is added or corrected, nothing else changes.
func TestXodusNexusGamePatchesFixesAnExistingOverrideKey(t *testing.T) {
	for test, tc := range map[string]struct{ section, keep string }{
		"key with other DLLs":     {overrideKey + " 1786134207\n#time=1dd\n\"d3d11\"=\"native\"\n", overrideKey + " 1786134207\n#time=1dd\n"},
		"key with builtin value":  {overrideKey + " 1786134207\n\"XGameRuntime\"=\"builtin\"\n", overrideKey + " 1786134207"},
		"key in another case":     {strings.ToLower(overrideKey) + " 1786134207\n", strings.ToLower(overrideKey)},
		"key followed by another": {overrideKey + " 1\n\n[Software\\\\Wine\\\\Other] 2\n\"x\"=\"y\"\n", `"x"="y"`},
	} {
		t.Run(test, func(t *testing.T) {
			h := newPatchHost(t)
			h.games(map[string]string{"g007": "007FirstLight.exe"})
			reg := filepath.Join(h.compat, "g007", "pfx", "user.reg")
			h.write(reg, []byte("WINE REGISTRY Version 2\n\n"+tc.section))
			h.publish(fakeDLL(false, "patched"), "")
			calls := h.run()
			got := h.userReg("g007")
			lower := strings.ToLower(got)
			if strings.Count(lower, strings.ToLower(overrideKey)) != 1 || strings.Count(lower, `"xgameruntime"=`) != 1 ||
				!strings.Contains(got, `"xgameruntime"="native,builtin"`) || !strings.Contains(got, tc.keep) {
				t.Errorf("user.reg:\n%s\ncalls:\n%s", got, calls)
			}
			// The value lands inside the 007 key, before any following key.
			at := strings.Index(lower, strings.ToLower(overrideKey))
			next := strings.Index(got[at+1:], "\n[")
			if v := strings.Index(got[at:], `"xgameruntime"="native,builtin"`); v < 0 || (next >= 0 && v > next+1) {
				t.Errorf("value outside the 007 key:\n%s", got)
			}
		})
	}
}

// A game patched before gets upstream's builtin back once it is no longer
// listed, or when no patches are left; the record of patched prefixes empties.
func TestXodusNexusGamePatchesRestoresDelistedGames(t *testing.T) {
	for _, test := range []string{"game delisted", "patches deleted"} {
		t.Run(test, func(t *testing.T) {
			h := newPatchHost(t)
			h.games(map[string]string{"g007": "007FirstLight.exe"})
			h.publish(fakeDLL(false, "patched"), "")
			h.run()
			if bytes.Equal(h.system32("g007"), h.builtin) {
				t.Fatal("not patched to start with")
			}
			if test == "game delisted" {
				h.write(filepath.Join(h.share, "games"), []byte("# none\n"))
			} else {
				for _, p := range []string{"0002-xuser-catch-all-endpoint.patch"} {
					if err := os.Remove(filepath.Join(h.share, "patches", p)); err != nil {
						t.Fatal(err)
					}
				}
			}
			calls := h.run()
			if !bytes.Equal(h.system32("g007"), h.builtin) || !strings.Contains(calls, "g007: no longer listed") {
				t.Errorf("want upstream's builtin back:\n%s", calls)
			}
			if b, _ := os.ReadFile(filepath.Join(h.home, ".cache", "xodus-nexus", "xgameruntime", "installed")); len(b) != 0 {
				t.Errorf("record still lists %q", b)
			}
		})
	}
}

// Prefixes live beside a custom runtimeRoot, as Nexus places them.
func TestXodusNexusGamePatchesFollowsRuntimeRoot(t *testing.T) {
	h := newPatchHost(t)
	old := filepath.Join(h.home, ".local", "share", "xodus-nexus")
	moved := filepath.Join(h.dir, "nexus-data")
	if err := os.Rename(old, moved); err != nil {
		t.Fatal(err)
	}
	h.compat = filepath.Join(moved, "compatdata")
	cur := filepath.Join(moved, "runtimes", "current.json")
	b, err := os.ReadFile(cur)
	if err != nil {
		t.Fatal(err)
	}
	h.write(cur, []byte(strings.ReplaceAll(string(b), old, moved)))
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	h.write(filepath.Join(h.home, ".config", "xodus-nexus", "launcher-state.json"),
		[]byte(`{"settings":{"runtimeRoot":"`+filepath.Join(moved, "runtimes")+`"},"games":[{"id":"g007","path":"`+filepath.Join(h.dir, "games", "g007")+`"}]}`))
	patched := fakeDLL(false, "patched")
	h.publish(patched, "")
	if calls := h.run(); !bytes.Equal(h.system32("g007"), patched) {
		t.Errorf("custom runtimeRoot's prefix not patched:\n%s", calls)
	}
}

// Odd entries don't stop the rest: a malformed game entry is skipped, and a
// config naming the exe with single quotes or a subdirectory still matches.
func TestXodusNexusGamePatchesToleratesOddEntries(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	h.write(filepath.Join(h.dir, "games", "g007", "MicrosoftGame.Config"),
		[]byte(`<Game><ExecutableList><Executable Id='Game' Name='Retail\007FirstLight.exe'/></ExecutableList></Game>`))
	h.write(filepath.Join(h.home, ".config", "xodus-nexus", "launcher-state.json"),
		[]byte(`{"settings":{},"games":["bad",null,{"id":"../x","path":"/"},{"id":"g007","path":"`+filepath.Join(h.dir, "games", "g007")+`"}]}`))
	patched := fakeDLL(false, "patched")
	h.publish(patched, "")
	if calls := h.run(); !bytes.Equal(h.system32("g007"), patched) || !strings.Contains(h.userReg("g007"), overrideKey) {
		t.Errorf("odd entries stopped patching:\n%s", calls)
	}
}

// A symlink where the DLL goes is replaced by a file, never written through.
func TestXodusNexusGamePatchesReplacesASymlinkedDLL(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	outside := filepath.Join(h.dir, "outside.dll")
	h.write(outside, []byte("outside"))
	dll := filepath.Join(h.compat, "g007", "pfx", "drive_c", "windows", "system32", "xgameruntime.dll")
	if err := os.Remove(dll); err != nil {
		t.Fatal(err)
	}
	patched := fakeDLL(false, "patched")
	h.write(outside, patched) // same content: a hash check alone would skip it
	if err := os.Symlink(outside, dll); err != nil {
		t.Fatal(err)
	}
	h.publish(patched, "")
	calls := h.run()
	if fi, err := os.Lstat(dll); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("DLL is still a symlink (%v):\n%s", err, calls)
	}
	if b, _ := os.ReadFile(outside); !bytes.Equal(b, patched) {
		t.Errorf("wrote through the symlink")
	}
}

// A patched prefix stays recorded across starts where it can't be updated
// (its user.reg is missing, or installing fails), so delisting the game later
// still puts the builtin back; a failed restore keeps it recorded too.
func TestXodusNexusGamePatchesKeepsTheRecordUntilRestored(t *testing.T) {
	record := func(h *patchHost) string {
		b, _ := os.ReadFile(filepath.Join(h.home, ".cache", "xodus-nexus", "xgameruntime", "installed"))
		return strings.TrimSpace(string(b))
	}
	for _, test := range []string{"user.reg missing", "install fails"} {
		t.Run(test, func(t *testing.T) {
			h := newPatchHost(t)
			h.games(map[string]string{"g007": "007FirstLight.exe"})
			h.publish(fakeDLL(false, "patched"), "")
			h.run()
			if record(h) != "g007" {
				t.Fatalf("record %q after patching", record(h))
			}
			reg := filepath.Join(h.compat, "g007", "pfx", "user.reg")
			var env []string
			if test == "user.reg missing" {
				if err := os.Rename(reg, reg+".away"); err != nil {
					t.Fatal(err)
				}
			} else {
				// A new DLL to install, which then fails.
				h.publish(fakeDLL(false, "patched-2"), "")
				if err := os.RemoveAll(filepath.Join(h.home, ".cache", "xodus-nexus", "xgameruntime", "nexus-xgameruntime-"+patchRuntimeTag+"-"+strings.TrimSpace(h.cmd("bash", "../../hack/nexus-xgameruntime/patch-id.sh", filepath.Join(h.share, "patches"))))); err != nil {
					t.Fatal(err)
				}
				env = []string{"FAIL_CP=1"}
			}
			if calls := h.run(env...); record(h) != "g007" {
				t.Fatalf("record %q after a start that couldn't update the prefix:\n%s", record(h), calls)
			}
			if test == "user.reg missing" {
				if err := os.Rename(reg+".away", reg); err != nil {
					t.Fatal(err)
				}
			}
			h.write(filepath.Join(h.share, "games"), []byte("# none\n"))
			if calls := h.run("FAIL_CP=1"); record(h) != "g007" {
				t.Fatalf("record %q after a failed restore:\n%s", record(h), calls)
			}
			if calls := h.run(); !bytes.Equal(h.system32("g007"), h.builtin) || record(h) != "" {
				t.Errorf("want the builtin back and an empty record (%q):\n%s", record(h), calls)
			}
		})
	}
}

// Sessions re-run the Library's step before the game starts (XERK-2654), so
// the session image ships the same script, game list and patch series.
func TestXodusNexusGamePatchesMatchSessionImage(t *testing.T) {
	for _, f := range []string{"root/usr/local/bin/xodus-nexus-game-patches", patchesShare + "/games",
		patchesShare + "/patches/0002-xuser-catch-all-endpoint.patch"} {
		library, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		session, err := os.ReadFile("../gow-nvidia32/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(library, session) {
			t.Errorf("images/library and images/gow-nvidia32 ship different %s", f)
		}
	}
	lib, _ := filepath.Glob(filepath.Join(patchesShare, "patches", "*"))
	ses, _ := filepath.Glob(filepath.Join("..", "gow-nvidia32", patchesShare, "patches", "*"))
	if len(lib) != len(ses) {
		t.Errorf("patch series differ: library %d files, session %d", len(lib), len(ses))
	}
	dockerfile, err := os.ReadFile("../gow-nvidia32/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"COPY --chmod=755 root/usr/local/bin/xodus-nexus-game-patches /usr/local/bin/xodus-nexus-game-patches",
		"COPY --chmod=644 root/usr/share/xodus-nexus/xgameruntime/ /usr/share/xodus-nexus/xgameruntime/",
		"COPY --chmod=755 root/etc/cont-init.d/31-xodus-nexus-game-patches.sh /etc/cont-init.d/"} {
		if !strings.Contains(string(dockerfile), line) {
			t.Errorf("the session image lacks %q", line)
		}
	}
}

// The Library and every Nexus session run this over one shared home
// (XERK-2654): concurrent runs serialise on a lock there, so all of them end
// with the patched DLL and none fails to install it.
func TestXodusNexusGamePatchesConcurrentRunsAgree(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	patched := fakeDLL(false, "patched")
	h.publish(patched, "")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmds := make([]*exec.Cmd, 4)
	outs := make([]*bytes.Buffer, len(cmds))
	for i := range cmds {
		cmd := exec.CommandContext(ctx, "bash", xodusNexusGamePatches)
		cmd.Env = append(os.Environ(), "LOG=/dev/null",
			"PATH="+filepath.Join(h.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
			"HOME="+h.home, "FAKE_GH="+h.gh, "XODUS_NEXUS_PATCH_SHARE="+h.share)
		outs[i] = &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = outs[i], outs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil || strings.Contains(outs[i].String(), "cannot") {
			t.Errorf("run %d: %v\n%s", i, err, outs[i])
		}
	}
	if !bytes.Equal(h.system32("g007"), patched) || strings.Count(h.userReg("g007"), overrideKey) != 1 {
		t.Errorf("after concurrent runs: patched=%v\n%s", bytes.Equal(h.system32("g007"), patched), h.userReg("g007"))
	}
}

// A patch that can't be read stops the run: its series would hash to an id
// no release is built for, and every listed game would get the builtin.
func TestXodusNexusGamePatchesStopsOnAnUnreadablePatch(t *testing.T) {
	h := newPatchHost(t)
	h.games(map[string]string{"g007": "007FirstLight.exe"})
	patched := fakeDLL(false, "patched")
	h.publish(patched, "")
	h.run()
	if err := os.Mkdir(filepath.Join(h.share, "patches", "0003-unreadable.patch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if calls := h.run(); !bytes.Equal(h.system32("g007"), patched) || !strings.Contains(calls, "cannot read the patch series") {
		t.Errorf("want the run stopped and 007 kept patched:\n%s", calls)
	}
}
