package library

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const xodusNexusUpdate = "root/usr/local/bin/xodus-nexus-update"

// The Library and the nexus session image must update Nexus the same way: a
// session installs what the Library cached.
func TestXodusNexusUpdateMatchesSessionImage(t *testing.T) {
	library, err := os.ReadFile(xodusNexusUpdate)
	if err != nil {
		t.Fatal(err)
	}
	session, err := os.ReadFile("../gow-nvidia32/root/usr/local/bin/xodus-nexus-update")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(library, session) {
		t.Error("images/library and images/gow-nvidia32 ship different xodus-nexus-update scripts")
	}
}

// nexusStubs replace every command xodus-nexus-update uses to reach the
// network, another user or root-only ownership. Each logs its call.
var nexusStubs = map[string]string{
	"curl": `for a; do [ "$prev" = -o ] && out=$a; [ "$prev" = --max-filesize ] && max=$a; prev=$a; done
[ -z "$out" ] && { echo "releases $*" >>"$LOG"; exec cat "$FAKE_RELEASES"; }
echo "download $*" >>"$LOG"
[ "${max:-0}" -gt 0 ] && [ "$(wc -c <"$FAKE_APP")" -gt "$max" ] && exit 63
cp "$FAKE_APP" "$out"`,
	"getent": `[ "$2" = abc ] && echo "abc:x:1000:1000::$FAKE_HOME:/bin/bash"`,
	// Logs that the home and the unpack ran as the desktop user, never as root.
	"s6-setuidgid": `[ "$1" = abc ] || exit 111; shift; echo "as-abc $3" >>"$LOG"; exec "$@"`,
	"chown":        `echo "chown $*" >>"$LOG"`,
	// FAKE_MV_FAIL: installing the unpacked tree fails.
	"mv": `case "$FAKE_MV_FAIL:$1" in 1:*squashfs-root) exit 1;; esac; exec /bin/mv "$@"`,
}

// nexusApp is a fake AppImage that unpacks to a tree whose AppRun prints body,
// with directories only its owner can read, as --appimage-extract leaves them.
func nexusApp(body string) string {
	return `#!/bin/sh
[ "$1" = --appimage-extract ] || exit 2
mkdir -p squashfs-root/resources && printf '#!/bin/sh\necho ` + body + `\n' >squashfs-root/AppRun &&
chmod 0700 squashfs-root/AppRun squashfs-root/resources
`
}

// nexusPod is one pod's view: a stubbed host, its own /opt/xodus-nexus, and
// a desktop home that outlives runs, as the Steam home PVC outlives pods.
type nexusPod struct {
	t                   *testing.T
	dir, home, log, opt string
}

func newNexusPod(t *testing.T, installed string) *nexusPod {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range nexusStubs {
		writeStub(t, filepath.Join(bin, name), body)
	}
	p := &nexusPod{t: t, dir: dir, home: filepath.Join(dir, "home"), log: filepath.Join(dir, "log"),
		opt: filepath.Join(dir, "opt", "xodus-nexus")}
	p.reset(installed)
	return p
}

// reset is a new pod: the image's Nexus at installed, the home kept.
func (p *nexusPod) reset(installed string) {
	p.t.Helper()
	if err := os.RemoveAll(filepath.Dir(p.opt)); err != nil {
		p.t.Fatal(err)
	}
	if err := os.MkdirAll(p.opt, 0o755); err != nil {
		p.t.Fatal(err)
	}
	writeStub(p.t, filepath.Join(p.opt, "AppRun"), "echo image")
	if err := os.WriteFile(filepath.Join(p.opt, ".direwolf-version"), []byte(installed+"\n"), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

type nexusRelease struct {
	tag, asset, app, digest string
	prerelease              bool
}

func launcherRelease(version, app string) nexusRelease {
	return nexusRelease{tag: "v" + version, asset: "Xodus-Nexus-" + version + "-x86_64.AppImage", app: app,
		digest: "sha256:" + sha256Hex(app)}
}

// run runs the real script against releases (listed in this order) and
// returns the stubs' calls and its output. Every download serves app.
func (p *nexusPod) run(releases []nexusRelease, app string, args ...string) string {
	return p.runEnv(releases, app, nil, args...)
}

// runEnv is run with extra environment for the stubs.
func (p *nexusPod) runEnv(releases []nexusRelease, app string, env []string, args ...string) string {
	t := p.t
	t.Helper()
	list := make([]map[string]any, 0, len(releases))
	for _, r := range releases {
		list = append(list, map[string]any{"tag_name": r.tag, "prerelease": r.prerelease, "assets": []map[string]any{
			{"name": r.asset, "browser_download_url": "https://github.com/x/" + r.asset, "size": len(r.app), "digest": r.digest},
		}})
	}
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	for f, s := range map[string]string{"releases.json": string(data), "app": app, "log": ""} {
		if err = os.WriteFile(filepath.Join(p.dir, f), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.ReadFile(xodusNexusUpdate)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(src), "/opt/xodus-nexus", p.opt)
	script := filepath.Join(p.dir, "xodus-nexus-update")
	if err = os.WriteFile(script, []byte(patched), 0o600); err != nil { //nolint:gosec // script is under t.TempDir
		t.Fatal(err)
	}
	// Bounded, so a script that hangs fails the test rather than the suite.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{script, "abc"}, args...)...) //nolint:gosec // the script under test
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(p.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LOG="+p.log, "FAKE_HOME="+p.home,
		"FAKE_RELEASES="+filepath.Join(p.dir, "releases.json"), "FAKE_APP="+filepath.Join(p.dir, "app"))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xodus-nexus-update must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	calls, err := os.ReadFile(p.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls) + string(out)
}

// installed is the recorded version and the AppRun output of the pod's Nexus.
func (p *nexusPod) installed() (version, runs string) {
	p.t.Helper()
	v, err := os.ReadFile(filepath.Join(p.opt, ".direwolf-version"))
	if err != nil {
		p.t.Fatal(err)
	}
	out, err := exec.CommandContext(p.t.Context(), filepath.Join(p.opt, "AppRun")).Output() //nolint:gosec // the test's own tree
	if err != nil {
		p.t.Fatal(err)
	}
	return strings.TrimSpace(string(v)), strings.TrimSpace(string(out))
}

// cachedApp is where the Library caches the tests' 0.3.12 AppImage.
func (p *nexusPod) cachedApp() string {
	return filepath.Join(p.home, ".cache/direwolf-xodus-nexus/Xodus-Nexus-0.3.12-x86_64.AppImage")
}

// A newer launcher release is downloaded, cached as the desktop user,
// unpacked as the desktop user, handed to root and installed. The repo's
// newer backend release is not the launcher.
func TestXodusNexusUpdateInstallsNewerRelease(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	app := nexusApp("v0.3.12")
	releases := []nexusRelease{
		{tag: "backend-v0.2.12", asset: "xodus-backend.tar.zst", app: "x", digest: "sha256:" + sha256Hex("x")},
		{tag: "v0.4.0", asset: "Xodus-Nexus-0.4.0-x86_64.AppImage", app: "pre", digest: "sha256:" + sha256Hex("pre"), prerelease: true},
		{tag: "v0.4.1-rc1", asset: "Xodus-Nexus-0.4.1-rc1-x86_64.AppImage", app: "rc", digest: "sha256:" + sha256Hex("rc")},
		launcherRelease("0.3.12", app),
		launcherRelease("0.3.11", nexusApp("v0.3.11")),
	}
	calls := p.run(releases, app)
	// The cache is read and written as the desktop user; root owns what it installs.
	for _, want := range []string{"as-abc [ -f", "download", "as-abc rm -rf", "as-abc cd", "chown -R 0:0 ", "updated Xodus Nexus 0.3.11 -> 0.3.12"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if v, runs := p.installed(); v != "0.3.12" || runs != "v0.3.12" {
		t.Errorf("installed %q running %q, want 0.3.12", v, runs)
	}
	if fi, err := os.Stat(filepath.Join(p.opt, "resources")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("unpacked tree not made world-readable: %v %v", fi, err)
	}
	if b, err := os.ReadFile(p.cachedApp()); err != nil || string(b) != app {
		t.Errorf("AppImage not cached: %v", err)
	}
	if matches, _ := filepath.Glob(p.opt + ".update.*"); len(matches) != 0 {
		t.Errorf("left %q behind", matches)
	}

	// The next pod reuses the verified cache instead of downloading.
	p.reset("0.3.11")
	calls = p.run(releases, app)
	if strings.Contains(calls, "download") || !strings.Contains(calls, "using the cached") {
		t.Errorf("want the cached AppImage reused:\n%s", calls)
	}
	if v, _ := p.installed(); v != "0.3.12" {
		t.Errorf("installed %q, want 0.3.12", v)
	}
}

func TestXodusNexusUpdateKeepsCurrentVersion(t *testing.T) {
	p := newNexusPod(t, "0.3.12")
	calls := p.run([]nexusRelease{launcherRelease("0.3.11", nexusApp("old")), launcherRelease("0.3.12", nexusApp("same"))}, "")
	if strings.Contains(calls, "download") || !strings.Contains(calls, "is up to date") {
		t.Errorf("want no update:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.12" || runs != "image" {
		t.Errorf("installed %q running %q, want the image's 0.3.12", v, runs)
	}
}

// An AppImage that doesn't match its published digest is never unpacked.
func TestXodusNexusUpdateRejectsDigestMismatch(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	r := launcherRelease("0.3.12", nexusApp("evil"))
	r.digest = "sha256:" + sha256Hex("something else")
	calls := p.run([]nexusRelease{r}, r.app)
	if !strings.Contains(calls, "does not match its published SHA256") || strings.Contains(calls, "as-abc cd") {
		t.Errorf("want the download refused before unpacking:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.11" || runs != "image" {
		t.Errorf("installed %q running %q, want the image's 0.3.11", v, runs)
	}
}

// A session installs only what the Library cached, re-verified, and never
// downloads; with nothing cached it does not even ask GitHub.
func TestXodusNexusUpdateCachedOnly(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	app := nexusApp("v0.3.12")
	calls := p.run([]nexusRelease{launcherRelease("0.3.12", app)}, app, "--cached-only")
	if strings.Contains(calls, "releases") || strings.Contains(calls, "download") {
		t.Errorf("nothing cached: want no GitHub call:\n%s", calls)
	}
	if v, _ := p.installed(); v != "0.3.11" {
		t.Errorf("installed %q, want 0.3.11", v)
	}

	// The Library caches 0.3.12; 0.3.13 is out since, but the session runs
	// the Library's version.
	p.run([]nexusRelease{launcherRelease("0.3.12", app)}, app)
	p.reset("0.3.11")
	newer := nexusApp("v0.3.13")
	calls = p.run([]nexusRelease{launcherRelease("0.3.13", newer), launcherRelease("0.3.12", app)}, newer, "--cached-only")
	if strings.Contains(calls, "download") || !strings.Contains(calls, "releases/tags/v0.3.12") {
		t.Errorf("want the cached release looked up by its tag, and no download:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.12" || runs != "v0.3.12" {
		t.Errorf("installed %q running %q, want the Library's 0.3.12", v, runs)
	}

	// A tampered cache is refused, not replaced by a download.
	if err := os.WriteFile(p.cachedApp(), []byte(nexusApp("evil")), 0o600); err != nil {
		t.Fatal(err)
	}
	p.reset("0.3.11")
	calls = p.run([]nexusRelease{launcherRelease("0.3.12", app)}, app, "--cached-only")
	if strings.Contains(calls, "download") || !strings.Contains(calls, "does not match") {
		t.Errorf("want the tampered cache refused:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.11" || runs != "image" {
		t.Errorf("installed %q running %q, want the image's 0.3.11", v, runs)
	}
}

// Offline (or an unreadable release list) keeps the image's Nexus.
func TestXodusNexusUpdateOfflineKeepsInstalled(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	writeStub(t, filepath.Join(p.dir, "bin", "curl"), "exit 6")
	if err := os.WriteFile(p.log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := p.run(nil, "")
	if !strings.Contains(calls, "cannot read the releases") {
		t.Errorf("calls:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.11" || runs != "image" {
		t.Errorf("installed %q running %q, want the image's 0.3.11", v, runs)
	}
}

// A cache entry that is not a plain, bounded file (here /dev/zero, endless)
// is read no further than the asset's size; the release is downloaded.
func TestXodusNexusUpdateBoundsCacheRead(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	if err := os.MkdirAll(filepath.Dir(p.cachedApp()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", p.cachedApp()); err != nil {
		t.Fatal(err)
	}
	app := nexusApp("v0.3.12")
	calls := p.run([]nexusRelease{launcherRelease("0.3.12", app)}, app)
	if !strings.Contains(calls, "download") {
		t.Errorf("want a download:\n%s", calls)
	}
	if v, _ := p.installed(); v != "0.3.12" {
		t.Errorf("installed %q, want 0.3.12", v)
	}
}

// A tree that would keep a setuid/setgid entry is never installed.
func TestXodusNexusUpdateRefusesSetIDEntries(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	// chmod -R u=rwX,go=rX keeps a directory's setgid bit.
	app := nexusApp("v0.3.12") + "chmod 2755 squashfs-root/resources\n"
	calls := p.run([]nexusRelease{launcherRelease("0.3.12", app)}, app)
	if !strings.Contains(calls, "unsafe permissions") {
		t.Errorf("want the tree refused:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.11" || runs != "image" {
		t.Errorf("installed %q running %q, want the image's 0.3.11", v, runs)
	}
}

// A failed install puts the image's Nexus back.
func TestXodusNexusUpdateRestoresOnFailedInstall(t *testing.T) {
	p := newNexusPod(t, "0.3.11")
	app := nexusApp("v0.3.12")
	calls := p.runEnv([]nexusRelease{launcherRelease("0.3.12", app)}, app, []string{"FAKE_MV_FAIL=1"})
	if !strings.Contains(calls, "cannot install") {
		t.Errorf("calls:\n%s", calls)
	}
	if v, runs := p.installed(); v != "0.3.11" || runs != "image" {
		t.Errorf("installed %q running %q, want the image's 0.3.11 restored", v, runs)
	}
}
