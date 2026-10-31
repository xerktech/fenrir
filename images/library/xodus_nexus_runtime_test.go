package library

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const xodusNexusRuntime = "root/usr/local/bin/xodus-nexus-runtime"

// Upstream's runtime as domsvoboda/nexus-runtime publishes it, which Nexus
// prefers, and the launcher repository's older one, its fallback.
const (
	runtimeName  = "nexus-runtime-11.0-experimental-ge5afe21e"
	runtimeTag   = "runtime-11.0-20261028-ge5afe21e"
	fallbackName = "xodus-nexus-runtime-11.0-20261022-gf4adc821"
	fallbackTag  = "runtime-11.0-20261022-gf4adc821"
	runtimeRepo  = "nexus-runtime"
	launcherRepo = "xodus-nexus"
)

// runtimeArchive builds a runtime tar.xz whose top directory is top and whose
// files/bin/wine prints version, and returns its bytes.
func runtimeArchive(t *testing.T, top, version string) []byte {
	t.Helper()
	return runtimeArchiveWith(t, top, `[ "$1" = --version ] && echo `+version)
}

// runtimeArchiveWith is runtimeArchive with wine's script body given, and
// extra tar arguments (members renamed, more directories) after it.
func runtimeArchiveWith(t *testing.T, top, wine string, tarArgs ...string) []byte {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, top, "files", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(bin, "wine"), wine)
	if err := os.Mkdir(filepath.Join(dir, "xodus-nexus-runtime-other"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "rt.tar.xz")
	args := append([]string{"-cJf", out, "-C", dir}, tarArgs...)
	args = append(args, top)
	if b, err := exec.CommandContext(t.Context(), "tar", args...).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// runtimeHost is a desktop home and a fake GitHub serving one runtime release.
type runtimeHost struct {
	t                    *testing.T
	dir, home, log, root string
}

func newRuntimeHost(t *testing.T) *runtimeHost {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// An API URL serves that repository's <repo>.json release list (HTTP 404
	// without one, or the code in <repo>.status), else the file named after the
	// URL's last component, as GitHub serves release assets.
	writeStub(t, filepath.Join(bin, "curl"), `for a; do [ "$prev" = -o ] && out=$a; prev=$a; url=$a; done
case "$url" in https://api.github.com/repos/domsvoboda/*/releases*)
  repo=${url#https://api.github.com/repos/domsvoboda/}; repo=${repo%%/*}
  echo "releases $repo" >>"$LOG"
  if [ -f "$FAKE_GH/$repo.status" ]; then cat "$FAKE_GH/$repo.status"; exit 0; fi
  [ -f "$FAKE_GH/$repo.json" ] || { printf 404; exit 0; }
  cp "$FAKE_GH/$repo.json" "$out"; printf 200; exit 0;;
esac
echo "download ${url##*/}" >>"$LOG"
f="$FAKE_GH/${url##*/}"; [ -f "$f" ] || exit 22
if [ -n "$out" ]; then cp "$f" "$out"; else cat "$f"; fi`)
	home := filepath.Join(dir, "home")
	return &runtimeHost{t: t, dir: dir, home: home, log: filepath.Join(dir, "log"),
		root: filepath.Join(home, ".local", "share", "xodus-nexus", "runtimes")}
}

// publish serves archive as runtimeName's x86_64 asset in nexus-runtime, with
// sum as its .sha512sum file (the archive's own when empty) and digest as
// GitHub's SHA256 (the archive's own when empty).
func (h *runtimeHost) publish(archive []byte, sum, digest string) {
	h.t.Helper()
	h.publishTo(runtimeRepo, runtimeName, runtimeTag, archive, sum, digest)
}

// publishTo is publish into repo's release list, as runtime release tag.
func (h *runtimeHost) publishTo(repo, runtimeName, tag string, archive []byte, sum, digest string) {
	t := h.t
	t.Helper()
	gh := filepath.Join(h.dir, "gh")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	name := runtimeName + "-x86_64.tar.xz"
	if sum == "" {
		s := sha512.Sum512(archive)
		sum = hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	if digest == "" {
		digest = "sha256:" + sha256Hex(string(archive))
	}
	sumName := runtimeName + "-x86_64.sha512sum"
	releases := []map[string]any{
		{"tag_name": "v0.3.11", "published_at": "2026-10-22T20:01:48Z", "assets": []map[string]any{
			{"name": "Xodus-Nexus-0.3.11-x86_64.AppImage", "browser_download_url": "https://github.com/x/a", "size": 1, "digest": "sha256:" + strings.Repeat("0", 64)},
		}},
		{"tag_name": tag, "published_at": "2026-10-22T20:00:27Z", "assets": []map[string]any{
			{"name": name, "browser_download_url": "https://github.com/x/" + name, "size": len(archive), "digest": digest},
			{"name": sumName, "browser_download_url": "https://github.com/x/" + sumName, "size": len(sum), "digest": "sha256:" + sha256Hex(sum)},
		}},
	}
	data, err := json.Marshal(releases)
	if err != nil {
		t.Fatal(err)
	}
	for f, b := range map[string][]byte{repo + ".json": data, name: archive, sumName: []byte(sum)} {
		if err = os.WriteFile(filepath.Join(gh, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// run runs the real script and returns the stubs' calls and its output.
func (h *runtimeHost) run() string {
	t := h.t
	t.Helper()
	if err := os.WriteFile(h.log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", xodusNexusRuntime)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(h.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+h.home, "LOG="+h.log, "FAKE_GH="+filepath.Join(h.dir, "gh"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xodus-nexus-runtime must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	calls, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls) + string(out)
}

// current is the runtime current.json points Nexus at, or "" with none.
func (h *runtimeHost) current() map[string]string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, "current.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var p map[string]string
	if err = json.Unmarshal(b, &p); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// With no runtime, the newest runtime release is installed where Nexus
// installs it, and current.json points at its wine; the next start leaves it.
func TestXodusNexusRuntimeInstallsWhenMissing(t *testing.T) {
	h := newRuntimeHost(t)
	h.publish(runtimeArchive(t, runtimeName, "wine-11.0"), "", "")
	calls := h.run()
	if !strings.Contains(calls, "installed "+runtimeName) {
		t.Fatalf("calls:\n%s", calls)
	}
	wine := filepath.Join(h.root, "releases", runtimeName, "files", "bin", "wine")
	if p := h.current(); p["name"] != runtimeName || p["winePath"] != wine || p["commit"] != runtimeTag {
		t.Errorf("current.json %v, want %s at %s", p, runtimeName, wine)
	}
	if fi, err := os.Stat(filepath.Join(h.root, "current.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("current.json mode: %v %v", fi, err)
	}
	if staging, _ := filepath.Glob(filepath.Join(h.root, ".staging-*")); len(staging) != 0 {
		t.Errorf("left %q behind", staging)
	}

	calls = h.run()
	if strings.Contains(calls, "releases") || !strings.Contains(calls, "runtime is installed") {
		t.Errorf("want an installed runtime left alone, without asking GitHub:\n%s", calls)
	}
}

// A runtimeRoot the user set in Nexus is where the runtime goes.
func TestXodusNexusRuntimeUsesConfiguredRoot(t *testing.T) {
	h := newRuntimeHost(t)
	h.root = filepath.Join(h.home, "elsewhere")
	state := filepath.Join(h.home, ".config", "xodus-nexus", "launcher-state.json")
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte(`{"settings":{"runtimeRoot":"`+h.root+`"},"games":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.publish(runtimeArchive(t, runtimeName, "wine-11.0"), "", "")
	h.run()
	if p := h.current(); p["name"] != runtimeName {
		t.Errorf("current.json under %s: %v", h.root, p)
	}
}

// Anything that doesn't check out installs nothing.
func TestXodusNexusRuntimeRefusesUnverified(t *testing.T) {
	name := runtimeName + "-x86_64.tar.xz"
	for test, tc := range map[string]struct {
		top, version, sum, digest, want string
		archive                         func(*testing.T) []byte
	}{
		"wine exits non-zero": {want: "fails --version", archive: func(t *testing.T) []byte {
			t.Helper()
			return runtimeArchiveWith(t, runtimeName, `echo wine-11.0; exit 1`)
		}},
		"member outside the top directory": {want: "unexpected layout", archive: func(t *testing.T) []byte {
			t.Helper()
			return runtimeArchiveWith(t, runtimeName, `echo wine-11.0`, "-P", "--transform", "s,/files/bin,/../../escape,")
		}},
		"absolute member": {want: "unexpected layout", archive: func(t *testing.T) []byte {
			t.Helper()
			return runtimeArchiveWith(t, runtimeName, `echo wine-11.0`, "-P", "--transform", "s,^"+runtimeName+"/files/bin,/tmp/escape,")
		}},
		"two top directories": {want: "unexpected layout", archive: func(t *testing.T) []byte {
			t.Helper()
			return runtimeArchiveWith(t, runtimeName, `echo wine-11.0`, "xodus-nexus-runtime-other")
		}},
		"digest mismatch":          {digest: "sha256:" + strings.Repeat("1", 64), want: "published SHA256"},
		"sha512 mismatch":          {sum: strings.Repeat("a", 128) + "  " + name + "\n", want: "does not match its .sha512sum"},
		"sum names another file":   {sum: strings.Repeat("a", 128) + "  other.tar.xz\n", want: "does not name"},
		"unexpected top directory": {top: "evil", want: "unexpected layout"},
		"wine fails --version":     {version: "nope", want: "fails --version"},
	} {
		t.Run(test, func(t *testing.T) {
			top, version := runtimeName, "wine-11.0"
			if tc.top != "" {
				top = tc.top
			}
			if tc.version != "" {
				version = tc.version
			}
			h := newRuntimeHost(t)
			archive := runtimeArchive
			if tc.archive != nil {
				archive = func(t *testing.T, _, _ string) []byte { t.Helper(); return tc.archive(t) }
			}
			h.publish(archive(t, top, version), tc.sum, tc.digest)
			if calls := h.run(); !strings.Contains(calls, tc.want) {
				t.Errorf("want %q:\n%s", tc.want, calls)
			}
			if p := h.current(); p != nil {
				t.Errorf("current.json written: %v", p)
			}
			if _, err := os.Stat(filepath.Join(h.root, "releases", runtimeName)); !os.IsNotExist(err) {
				t.Errorf("release installed: %v", err)
			}
		})
	}
}

// A current.json whose wine is gone (a deleted release) is reinstalled.
func TestXodusNexusRuntimeReplacesBrokenPointer(t *testing.T) {
	h := newRuntimeHost(t)
	if err := os.MkdirAll(h.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "current.json"), []byte(`{"name":"old","winePath":"/nonexistent/wine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.publish(runtimeArchive(t, runtimeName, "wine-11.0"), "", "")
	h.run()
	if p := h.current(); p["name"] != runtimeName {
		t.Errorf("current.json %v, want %s", p, runtimeName)
	}
}

// Like Nexus's checkNexus: without a nexus-runtime repository, or with no
// usable release in it, the launcher repository's runtime is installed.
func TestXodusNexusRuntimeFallsBackToLauncherReleases(t *testing.T) {
	for test, runtimeList := range map[string]string{
		"repository missing": "",
		"no runtime release": `[{"tag_name":"v1","assets":[]}]`,
		// Nexus skips an archive without an https .sha512sum.
		"no checksum": `[{"tag_name":"t","assets":[{"name":"` + runtimeName + `-x86_64.tar.xz","browser_download_url":"https://github.com/x/a"}]}]`,
		"http checksum": `[{"tag_name":"t","assets":[{"name":"` + runtimeName + `-x86_64.tar.xz","browser_download_url":"https://github.com/x/a"},` +
			`{"name":"` + runtimeName + `-x86_64.sha512sum","browser_download_url":"http://github.com/x/b"}]}]`,
	} {
		t.Run(test, func(t *testing.T) {
			h := newRuntimeHost(t)
			h.publishTo(launcherRepo, fallbackName, fallbackTag, runtimeArchive(t, fallbackName, "wine-11.0"), "", "")
			if runtimeList != "" {
				if err := os.WriteFile(filepath.Join(h.dir, "gh", runtimeRepo+".json"), []byte(runtimeList), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := h.run()
			if !strings.Contains(calls, "releases "+runtimeRepo+"\nreleases "+launcherRepo+"\n") {
				t.Errorf("want nexus-runtime asked first, then xodus-nexus:\n%s", calls)
			}
			if p := h.current(); p["name"] != fallbackName || p["commit"] != fallbackTag {
				t.Errorf("current.json %v, want %s", p, fallbackName)
			}
		})
	}
}

// With a nexus-runtime release, the launcher repository is never asked: its
// runtimes are older ones Nexus keeps only as a fallback.
func TestXodusNexusRuntimePrefersRuntimeRepository(t *testing.T) {
	h := newRuntimeHost(t)
	h.publish(runtimeArchive(t, runtimeName, "wine-11.0"), "", "")
	h.publishTo(launcherRepo, fallbackName, fallbackTag, runtimeArchive(t, fallbackName, "wine-11.0"), "", "")
	calls := h.run()
	if strings.Contains(calls, "releases "+launcherRepo) {
		t.Errorf("asked xodus-nexus despite a nexus-runtime release:\n%s", calls)
	}
	if p := h.current(); p["name"] != runtimeName {
		t.Errorf("current.json %v, want %s", p, runtimeName)
	}
}

// A nexus-runtime list that doesn't parse or has the wrong shape, or the
// release Nexus would pick failing the checks here, installs nothing rather
// than the older fallback. (Nexus throws on most of these; on a few, such as
// [1], it would fall back, and stopping is the safer of the two.)
func TestXodusNexusRuntimeNoFallbackPastBadRuntimeList(t *testing.T) {
	for test, runtimeList := range map[string]string{
		"not JSON":      "<html>oops",
		"not an array":  `{"message":"x"}`,
		"not objects":   `[1]`,
		"null asset":    `[{"tag_name":"t","assets":[null]}]`,
		"assets object": `[{"tag_name":"t","assets":{"a":1}}]`,
		"tag Nexus takes, not this script": `[{"tag_name":"release/2026 10","published_at":"2026-10-28T00:00:00Z","assets":[` +
			`{"name":"` + runtimeName + `-x86_64.tar.xz","browser_download_url":"https://github.com/x/a","size":1,"digest":"sha256:` + strings.Repeat("0", 64) + `"},` +
			`{"name":"` + runtimeName + `-x86_64.sha512sum","browser_download_url":"https://github.com/x/b","size":1}]}]`,
		"unverifiable": `[{"tag_name":"` + runtimeTag + `","published_at":"2026-10-28T00:00:00Z","assets":[` +
			`{"name":"` + runtimeName + `-x86_64.tar.xz","browser_download_url":"https://github.com/x/a","size":1},` +
			`{"name":"` + runtimeName + `-x86_64.sha512sum","browser_download_url":"https://github.com/x/b","size":1}]}]`,
	} {
		t.Run(test, func(t *testing.T) {
			h := newRuntimeHost(t)
			h.publishTo(launcherRepo, fallbackName, fallbackTag, runtimeArchive(t, fallbackName, "wine-11.0"), "", "")
			if err := os.WriteFile(filepath.Join(h.dir, "gh", runtimeRepo+".json"), []byte(runtimeList), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := h.run()
			if strings.Contains(calls, "releases "+launcherRepo) {
				t.Errorf("fell back to xodus-nexus:\n%s", calls)
			}
			if strings.Contains(calls, "download ") || !strings.Contains(calls, "cannot find a verifiable runtime release in domsvoboda/nexus-runtime") {
				t.Errorf("want a stop before any download:\n%s", calls)
			}
			if p := h.current(); p != nil {
				t.Errorf("current.json written: %v", p)
			}
		})
	}
}

// Any failure other than a missing nexus-runtime repository installs nothing,
// as Nexus stops there too, rather than an older fallback runtime.
func TestXodusNexusRuntimeStopsOnReleaseListError(t *testing.T) {
	h := newRuntimeHost(t)
	h.publishTo(launcherRepo, fallbackName, fallbackTag, runtimeArchive(t, fallbackName, "wine-11.0"), "", "")
	if err := os.WriteFile(filepath.Join(h.dir, "gh", runtimeRepo+".status"), []byte("503"), 0o600); err != nil {
		t.Fatal(err)
	}
	if calls := h.run(); !strings.Contains(calls, "HTTP 503") || strings.Contains(calls, "releases "+launcherRepo) {
		t.Errorf("want a stop on HTTP 503 without the fallback:\n%s", calls)
	}
	if p := h.current(); p != nil {
		t.Errorf("current.json written: %v", p)
	}
}

// runtimeRelease is one release of name with a verifiable archive and
// .sha512sum served from the fake GitHub.
func (h *runtimeHost) runtimeRelease(name, tag, published string, extra map[string]any) map[string]any {
	t := h.t
	t.Helper()
	archive := runtimeArchive(t, name, "wine-11.0")
	s := sha512.Sum512(archive)
	sum := hex.EncodeToString(s[:]) + "  " + name + "-x86_64.tar.xz\n"
	for f, b := range map[string][]byte{name + "-x86_64.tar.xz": archive, name + "-x86_64.sha512sum": []byte(sum)} {
		if err := os.WriteFile(filepath.Join(h.dir, "gh", f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := map[string]any{"tag_name": tag, "published_at": published, "assets": []map[string]any{
		{"name": name + "-x86_64.tar.xz", "browser_download_url": "https://github.com/x/" + name + "-x86_64.tar.xz", "size": len(archive), "digest": "sha256:" + sha256Hex(string(archive))},
		{"name": name + "-x86_64.sha512sum", "browser_download_url": "https://github.com/x/" + name + "-x86_64.sha512sum", "size": len(sum)},
	}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// As Nexus picks: the newest publication, whatever the list order, the first
// on a tie, never a draft or prerelease, and never an archive whose own URL
// is not https (Nexus then takes the next release).
func TestXodusNexusRuntimePicksAsNexus(t *testing.T) {
	const older, newer = "nexus-runtime-11.0-experimental-gaaaaaaa", "nexus-runtime-11.0-experimental-gbbbbbbb"
	for test, tc := range map[string]struct {
		releases func(h *runtimeHost) []map[string]any
		want     string
	}{
		"newest publication, listed last": {want: newer, releases: func(h *runtimeHost) []map[string]any {
			return []map[string]any{h.runtimeRelease(older, "t1", "2026-10-26T00:00:00Z", nil), h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", nil)}
		}},
		"newest publication, listed first": {want: newer, releases: func(h *runtimeHost) []map[string]any {
			return []map[string]any{h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", nil), h.runtimeRelease(older, "t1", "2026-10-26T00:00:00Z", nil)}
		}},
		"first on a tie": {want: older, releases: func(h *runtimeHost) []map[string]any {
			return []map[string]any{h.runtimeRelease(older, "t1", "2026-10-28T00:00:00Z", nil), h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", nil)}
		}},
		"newer draft skipped": {want: older, releases: func(h *runtimeHost) []map[string]any {
			return []map[string]any{h.runtimeRelease(older, "t1", "2026-10-26T00:00:00Z", nil), h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", map[string]any{"draft": true})}
		}},
		"newer prerelease skipped": {want: older, releases: func(h *runtimeHost) []map[string]any {
			return []map[string]any{h.runtimeRelease(older, "t1", "2026-10-26T00:00:00Z", nil), h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", map[string]any{"prerelease": true})}
		}},
		"first archive in a release": {want: older, releases: func(h *runtimeHost) []map[string]any {
			r, n := h.runtimeRelease(older, "t1", "2026-10-28T00:00:00Z", nil), h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", nil)
			r["assets"] = append(r["assets"].([]map[string]any), n["assets"].([]map[string]any)...)
			return []map[string]any{r}
		}},
		"picked release without a digest stops": {want: "", releases: func(h *runtimeHost) []map[string]any {
			n := h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", nil)
			delete(n["assets"].([]map[string]any)[0], "digest")
			return []map[string]any{h.runtimeRelease(older, "t1", "2026-10-26T00:00:00Z", nil), n}
		}},
		"newer http archive skipped": {want: older, releases: func(h *runtimeHost) []map[string]any {
			n := h.runtimeRelease(newer, "t2", "2026-10-28T00:00:00Z", nil)
			n["assets"].([]map[string]any)[0]["browser_download_url"] = "http://github.com/x/" + newer + "-x86_64.tar.xz"
			return []map[string]any{h.runtimeRelease(older, "t1", "2026-10-26T00:00:00Z", nil), n}
		}},
	} {
		t.Run(test, func(t *testing.T) {
			h := newRuntimeHost(t)
			if err := os.MkdirAll(filepath.Join(h.dir, "gh"), 0o755); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(tc.releases(h))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(h.dir, "gh", runtimeRepo+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			calls := h.run()
			if p := h.current(); p["name"] != tc.want {
				t.Errorf("current.json %v, want %s:\n%s", p, tc.want, calls)
			}
		})
	}
}
