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
	for _, f := range []string{"games", "patches/0001-xasync-late-completion-reused-block.patch", "patches/0002-xuser-catch-all-endpoint.patch"} {
		b, err := os.ReadFile(filepath.Join(patchesShare, f))
		if err != nil {
			t.Fatal(err)
		}
		h.write(filepath.Join(h.share, f), b)
	}
	if len(exes) > 0 {
		h.write(filepath.Join(h.share, "games"), []byte("# listed\n"+strings.Join(exes, "\n")+"\n"))
	}
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

// run runs the real script and returns the stubs' calls and its output.
func (h *patchHost) run() string {
	h.t.Helper()
	return runOneshot(h.t, xodusNexusGamePatches, h.log,
		"PATH="+filepath.Join(h.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+h.home, "FAKE_GH="+h.gh, "XODUS_NEXUS_PATCH_SHARE="+h.share)
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
