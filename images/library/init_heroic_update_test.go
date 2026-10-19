package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const initHeroicUpdate = "root/etc/s6-overlay/s6-rc.d/init-heroic-update/run"

// heroicLatest is the version every test's release publishes.
const heroicLatest = "2.22.4"

// heroicStubs replace every command init-heroic-update uses to reach the
// network, the package database or another user. Each logs its call.
var heroicStubs = map[string]string{
	"curl": `for a; do [ "$prev" = -o ] && out=$a; [ "$prev" = --max-filesize ] && max=$a; prev=$a; done
[ -z "$out" ] && exec cat "$FAKE_RELEASE"
echo "download $*" >>"$LOG"
# As real curl: 0 means no limit.
[ "${max:-0}" -gt 0 ] && [ "$(wc -c <"$FAKE_DEB")" -gt "$max" ] && exit 63
cp "$FAKE_DEB" "$out"`,
	// Reports $FAKE_INSTALLED until apt-get installs a version.
	"dpkg-query": `v=$FAKE_INSTALLED; [ -f "$LOG.installed" ] && v=$(cat "$LOG.installed")
case "$2" in *Status*) printf 'install ok installed %s' "$v";; *) printf %s "$v";; esac`,
	// Compares versions itself: a host's dpkg may be missing or, as on
	// TrueNAS, a wrapper that refuses to run (XERK-2008). The script only
	// asks "gt", and every version here is plain dotted numbers.
	"dpkg": `if [ "$1" = --compare-versions ]; then
  [ "$3" = gt ] || exit 2
  [ "$2" != "$4" ] && [ "$(printf '%s\n' "$2" "$4" | sort -V | tail -n1)" = "$2" ]; exit
fi
echo "dpkg $*" >>"$LOG"`,
	"getent": `echo "abc:x:1000:1000::$FAKE_HOME:/bin/bash"`,
	// Logs that the home was touched as the desktop user, never as root.
	// FAKE_HUNG_READ: the cache read never returns, as on a hung filesystem.
	"s6-setuidgid": `[ "$1" = abc ] || exit 111; shift; echo "as-abc $1" >>"$LOG"
case "$FAKE_HUNG_READ:$3" in 1:*head*) exec sleep 600;; esac
exec "$@"`,
	"heroic-postinstall": `echo postinstall >>"$LOG"`,
	"apt-get": `if [ "$1" = install ]; then
  for deb; do :; done
  echo "apt-get install $(sha256sum <"$deb" | cut -d' ' -f1)" >>"$LOG"
  # Like apt resolving an unfetchable dependency: without --no-remove, by removing Heroic.
  if [ -n "$FAKE_APT_FAIL" ]; then
    case " $* " in *" --no-remove "*) ;; *) echo "removed" >"$LOG.installed";; esac
    exit 100
  fi
  basename "$deb" | sed 's/^Heroic-//; s/-linux-amd64.deb$//' >"$LOG.installed"
else echo "apt-get $*" >>"$LOG"; fi`,
}

// heroicPod is one Library pod's view: a stubbed host and a desktop home
// that outlives runs, as the Steam home PVC outlives pods.
type heroicPod struct {
	t                   *testing.T
	dir, home, log, out string
}

func newHeroicPod(t *testing.T) *heroicPod {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range heroicStubs {
		writeStub(t, filepath.Join(bin, name), body)
	}
	return &heroicPod{t: t, dir: dir, home: filepath.Join(dir, "home"), log: filepath.Join(dir, "log")}
}

// writeStub writes an executable sh script.
func writeStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// run runs the real script with Heroic installed at installed, against a
// latest release whose amd64 .deb is deb, published with digest. It returns
// the stubs' calls.
func (p *heroicPod) run(installed, deb, digest string, env ...string) string {
	return p.runSized(installed, deb, digest, len(deb), env...)
}

// runSized is run with the asset's published size given separately.
func (p *heroicPod) runSized(installed, deb, digest string, size int, env ...string) string {
	t := p.t
	t.Helper()
	latest := heroicLatest
	name := "Heroic-" + latest + "-linux-amd64.deb"
	release, err := json.Marshal(map[string]any{
		"tag_name": "v" + latest,
		"assets": []map[string]any{
			{"name": "Heroic-" + latest + "-linux-arm64.deb", "browser_download_url": "https://github.com/x/arm", "digest": "sha256:" + strings.Repeat("0", 64)},
			{"name": name, "browser_download_url": "https://github.com/x/" + name, "size": size, "digest": digest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for f, s := range map[string]string{"release.json": string(release), "deb": deb, "log": ""} {
		if err = os.WriteFile(filepath.Join(p.dir, f), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Bounded, so a script that hangs fails the test rather than the suite.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", initHeroicUpdate)
	// Its stubs' children may hold the output open after bash is killed.
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(p.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LOG="+p.log, "FAKE_HOME="+p.home, "FAKE_INSTALLED="+installed,
		"FAKE_RELEASE="+filepath.Join(p.dir, "release.json"), "FAKE_DEB="+filepath.Join(p.dir, "deb"))
	cmd.Env = append(cmd.Env, env...)
	_ = os.Remove(p.log + ".installed")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init-heroic-update must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	calls, err := os.ReadFile(p.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("output:\n%s", out)
	p.out = string(out)
	return string(calls)
}

func (p *heroicPod) cached() string {
	return filepath.Join(p.home, ".cache", "direwolf-heroic", "Heroic-"+heroicLatest+"-linux-amd64.deb")
}

func TestHeroicUpdateInstallsNewerRelease(t *testing.T) {
	p := newHeroicPod(t)
	const deb = "heroic 2.22.4"
	calls := p.run("2.22.3", deb, "sha256:"+sha256Hex(deb))
	want := "download -fsSL"
	if !strings.Contains(calls, want) {
		t.Fatalf("want a download, calls:\n%s", calls)
	}
	if !strings.Contains(calls, "apt-get install "+sha256Hex(deb)+"\npostinstall\n") {
		t.Fatalf("want the published .deb installed, then heroic-postinstall, calls:\n%s", calls)
	}
	if b, err := os.ReadFile(p.cached()); err != nil || string(b) != deb {
		t.Fatalf("want the .deb cached for the next pod: %q, %v", b, err)
	}
}

func TestHeroicUpdateSkipsCurrentOrOlderRelease(t *testing.T) {
	for _, installed := range []string{"2.22.4", "2.23.0", "2.100.0"} {
		p := newHeroicPod(t)
		const deb = "heroic 2.22.4"
		if calls := p.run(installed, deb, "sha256:"+sha256Hex(deb)); calls != "" {
			t.Fatalf("installed %s: want no download or install, calls:\n%s", installed, calls)
		}
	}
}

func TestHeroicUpdateRejectsDigestMismatch(t *testing.T) {
	p := newHeroicPod(t)
	calls := p.run("2.22.3", "tampered", "sha256:"+sha256Hex("heroic 2.22.4"))
	if strings.Contains(calls, "install") {
		t.Fatalf("must not install a .deb that fails its digest, calls:\n%s", calls)
	}
	if _, err := os.Stat(p.cached()); err == nil {
		t.Fatal("must not cache a .deb that fails its digest")
	}
}

func TestHeroicUpdateRejectsMissingDigest(t *testing.T) {
	for _, digest := range []string{"", "md5:0123", "sha256:" + strings.Repeat("A", 64)} {
		p := newHeroicPod(t)
		if calls := p.run("2.22.3", "heroic", digest); calls != "" {
			t.Fatalf("digest %q: a release without a sha256 digest must be skipped, calls:\n%s", digest, calls)
		}
	}
}

func TestHeroicUpdateUsesVerifiedCache(t *testing.T) {
	p := newHeroicPod(t)
	const deb = "heroic 2.22.4"
	digest := "sha256:" + sha256Hex(deb)
	p.run("2.22.3", deb, digest)

	// The next pod starts from the image's Heroic again, with the cache in its home.
	calls := p.run("2.22.3", deb, digest)
	if strings.Contains(calls, "download") {
		t.Fatalf("a verified cached .deb must not be downloaded again, calls:\n%s", calls)
	}
	if !strings.Contains(calls, "apt-get install "+sha256Hex(deb)) {
		t.Fatalf("want the cached .deb installed, calls:\n%s", calls)
	}
}

func TestHeroicUpdateRedownloadsTamperedCache(t *testing.T) {
	p := newHeroicPod(t)
	const deb = "heroic 2.22.4"
	digest := "sha256:" + sha256Hex(deb)
	if err := os.MkdirAll(filepath.Dir(p.cached()), 0o755); err != nil {
		t.Fatal(err)
	}
	// The desktop runs user code, which can write the cache.
	if err := os.WriteFile(p.cached(), []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := p.run("2.22.3", deb, digest)
	if strings.Contains(calls, sha256Hex("planted")) {
		t.Fatalf("installed the planted .deb, calls:\n%s", calls)
	}
	if !strings.Contains(calls, "download") || !strings.Contains(calls, "apt-get install "+sha256Hex(deb)) {
		t.Fatalf("want a fresh download installed, calls:\n%s", calls)
	}
	if b, _ := os.ReadFile(p.cached()); string(b) != deb {
		t.Fatalf("want the cache replaced with the verified .deb, got %q", b)
	}
}

func TestHeroicUpdateKeepsInstalledWhenOffline(t *testing.T) {
	p := newHeroicPod(t)
	writeStub(t, filepath.Join(p.dir, "bin", "curl"), "exit 6")
	if calls := p.run("2.22.3", "heroic", ""); calls != "" {
		t.Fatalf("want nothing installed when GitHub is unreachable, calls:\n%s", calls)
	}
}

func TestHeroicUpdateReadsCacheOnlyAsDesktopUser(t *testing.T) {
	p := newHeroicPod(t)
	const deb = "heroic 2.22.4"
	calls := p.run("2.22.3", deb, "sha256:"+sha256Hex(deb))
	// The cache read and the cache write: the home is user-writable.
	if n := strings.Count(calls, "as-abc sh\n"); n != 2 {
		t.Fatalf("want the cache read and written as abc, got %d, calls:\n%s", n, calls)
	}
}

func TestHeroicUpdateIgnoresPlantedSpecialFiles(t *testing.T) {
	plants := map[string]func(t *testing.T, path string) error{
		"fifo": func(t *testing.T, path string) error {
			t.Helper()
			return exec.CommandContext(t.Context(), "mkfifo", path).Run()
		},
		"/dev/zero": func(_ *testing.T, path string) error { return os.Symlink("/dev/zero", path) },
		// Sparse: as big as you like, without the disk to back it.
		"oversized": func(_ *testing.T, path string) error { return os.Truncate(path, 1<<40) },
	}
	for name, plant := range plants {
		t.Run(name, func(t *testing.T) {
			p := newHeroicPod(t)
			const deb = "heroic 2.22.4"
			path := p.cached()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if name == "oversized" {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := plant(t, path); err != nil {
				t.Skipf("cannot plant %s: %v", name, err)
			}
			start := time.Now()
			calls := p.run("2.22.3", deb, "sha256:"+sha256Hex(deb))
			if d := time.Since(start); d > 30*time.Second {
				t.Fatalf("took %s: a planted %s must not stall the desktop's start", d, name)
			}
			if !strings.Contains(calls, "download") || !strings.Contains(calls, "apt-get install "+sha256Hex(deb)) {
				t.Fatalf("want a fresh download installed, calls:\n%s", calls)
			}
		})
	}
}

// A cache read that hangs, as on a hung filesystem, must be given up on: the
// desktop does not start until the script ends. The [ -f ] check cannot
// help there; only the timeout does (XERK-1867).
func TestHeroicUpdateGivesUpOnHungCacheRead(t *testing.T) {
	realTimeout, err := exec.LookPath("timeout")
	if err != nil {
		t.Skipf("no timeout(1): %v", err)
	}
	p := newHeroicPod(t)
	// The script's own limits, cut to a second so the test is quick.
	writeStub(t, filepath.Join(p.dir, "bin", "timeout"), `echo "timeout $1" >>"$LOG"; shift; exec `+realTimeout+` 1 "$@"`)
	const deb = "heroic 2.22.4"
	calls := p.run("2.22.3", deb, "sha256:"+sha256Hex(deb), "FAKE_HUNG_READ=1")
	// The read is the first command the script times out (the cache write
	// comes later): a longer limit would hold the desktop's start longer.
	if i := strings.Index(calls, "timeout "); i < 0 || !strings.HasPrefix(calls[i:], "timeout 60\nas-abc sh\n") {
		t.Fatalf("want the cache read under a 60s timeout, calls:\n%s", calls)
	}
	if !strings.Contains(calls, "download") || !strings.Contains(calls, "apt-get install "+sha256Hex(deb)) {
		t.Fatalf("want a fresh download installed, calls:\n%s", calls)
	}
}

func TestHeroicUpdateReportsFailedInstall(t *testing.T) {
	p := newHeroicPod(t)
	const deb = "heroic 2.22.4"
	calls := p.run("2.22.3", deb, "sha256:"+sha256Hex(deb), "FAKE_APT_FAIL=1")
	if strings.Contains(calls, "--fix-broken") || strings.Contains(calls, " -f ") {
		t.Fatalf("must not let apt fix (remove) packages, calls:\n%s", calls)
	}
	if !strings.Contains(calls, "apt-get update") || strings.Count(calls, "apt-get install") != 2 {
		t.Fatalf("want one retry after fetching package lists, calls:\n%s", calls)
	}
	if !strings.Contains(calls, "postinstall") {
		t.Fatalf("heroic-postinstall must run after a failed install too, calls:\n%s", calls)
	}
	if !strings.Contains(p.out, "failed (heroic: install ok installed 2.22.3)") {
		t.Fatalf("want 2.22.3 kept installed and reported:\n%s", p.out)
	}
	if strings.Contains(p.out, "updated Heroic") {
		t.Fatalf("a failed install must not report an update:\n%s", p.out)
	}
}

func TestHeroicUpdateRejectsBadSize(t *testing.T) {
	const deb = "heroic 2.22.4"
	digest := "sha256:" + sha256Hex(deb)
	// Published smaller than the download: --max-filesize stops it.
	// Zero or 1 GiB and over: not a plausible .deb.
	for _, size := range []int{len(deb) - 1, 0, 1 << 30} {
		p := newHeroicPod(t)
		if calls := p.runSized("2.22.3", deb, digest, size); strings.Contains(calls, "install") {
			t.Fatalf("size %d: must not install, calls:\n%s", size, calls)
		}
	}
}
