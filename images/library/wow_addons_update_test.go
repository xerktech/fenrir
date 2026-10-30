package library

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const wowAddonsUpdate = "root/usr/local/bin/wow-addons-update"

// consolePortList is the chart's wowAddons with ConsolePort, retail only.
const consolePortList = `[{"name":"ConsolePort","source":{"type":"github","repo":"seblindfors/ConsolePort","asset":"ConsolePort-*.zip"},"flavors":["retail"]}]`

// wowCurlStub serves $FAKE_RELEASE as every repo's latest release and
// $FAKE_ASSET for every download, logging each download. FAKE_OFFLINE fails both.
const wowCurlStub = `[ -n "$FAKE_OFFLINE" ] && exit 6
for a; do [ "$prev" = -o ] && out=$a; [ "$prev" = --max-filesize ] && max=$a; prev=$a; done
[ -z "$out" ] && exec cat "$FAKE_RELEASE"
echo "download $a" >>"$LOG"
[ "${max:-0}" -gt 0 ] && [ "$(wc -c <"$FAKE_ASSET")" -gt "$max" ] && exit 63
cp "$FAKE_ASSET" "$out"`

// wowPod is a Library pod's games volume with WoW installed, kept across runs.
type wowPod struct {
	t                 *testing.T
	dir, games, log   string
	retail, classic   string // each flavor's AddOns
	release, assetZip []byte
	env               []string // for the next run
}

func newWoWPod(t *testing.T) *wowPod {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(bin, "curl"), wowCurlStub)
	games := filepath.Join(dir, "games", "battlenet")
	wow := filepath.Join(games, "World of Warcraft")
	p := &wowPod{
		t: t, dir: dir, games: games, log: filepath.Join(dir, "log"),
		retail:  filepath.Join(wow, "_retail_", "Interface", "AddOns"),
		classic: filepath.Join(wow, "_classic_", "Interface", "AddOns"),
	}
	// Both flavors installed; the user added Details to retail by hand.
	p.write(filepath.Join(wow, "_classic_", "Wow.exe"), "classic")
	p.write(filepath.Join(p.retail, "Details", "Details.toc"), "hand-installed")
	return p
}

func (p *wowPod) write(path, content string) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// zipEntry is one archive member; mode is set only to mark links.
type zipEntry struct {
	name, body string
	mode       fs.FileMode
	// size, if set, is the uncompressed size the header claims.
	size uint64
}

func makeZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		if e.size != 0 {
			h.CRC32 = 0
			h.CompressedSize64 = uint64(len(e.body))
			h.UncompressedSize64 = e.size
			raw, err := w.CreateRaw(h)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = raw.Write([]byte(e.body))
			continue
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(e.body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func consolePortZip(t *testing.T, version string, folders ...string) []byte {
	t.Helper()
	entries := make([]zipEntry, 0, len(folders)+1)
	for _, f := range folders {
		entries = append(entries, zipEntry{name: f + "/" + f + ".toc", body: f + " " + version})
	}
	entries = append(entries, zipEntry{name: "ConsolePort/Libs/LibStub/LibStub.lua", body: "lib " + version})
	return makeZip(t, entries...)
}

// publish makes asset the latest release's ConsolePort-<version>.zip, with
// digest as GitHub's published one ("" for the asset's own SHA256).
func (p *wowPod) publish(version string, asset []byte, digest string) {
	p.t.Helper()
	if digest == "" {
		sum := sha256.Sum256(asset)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	name := "ConsolePort-" + version + ".zip"
	release, err := json.Marshal(map[string]any{
		"tag_name": version,
		"assets": []map[string]any{
			{"name": "release.json", "size": 10, "browser_download_url": "https://github.com/x/release.json", "digest": "sha256:" + strings.Repeat("0", 64)},
			{"name": name, "size": len(asset), "browser_download_url": "https://github.com/seblindfors/ConsolePort/releases/download/" + version + "/" + name, "digest": digest},
		},
	})
	if err != nil {
		p.t.Fatal(err)
	}
	p.release, p.assetZip = release, asset
}

// run runs the real script as the Library's service would, with WOW_ADDONS
// set to list (unset if list is nil), and returns the downloads it made.
func (p *wowPod) run(list *string, env ...string) (downloads, out string) {
	p.t.Helper()
	return p.runArgs(list, []string{wowAddonsUpdate}, false, env...)
}

// runArgs runs python3 -I args; mayFail allows a non-zero exit (a kill).
func (p *wowPod) runArgs(list *string, args []string, mayFail bool, env ...string) (downloads, out string) {
	t := p.t
	t.Helper()
	for f, b := range map[string][]byte{"release.json": p.release, "asset": p.assetZip, "log": nil} {
		if err := os.WriteFile(filepath.Join(p.dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// -B: the fault driver imports the script, and its __pycache__ would land
	// in the image source (-I ignores PYTHONDONTWRITEBYTECODE).
	cmd := exec.CommandContext(ctx, "python3", append([]string{"-I", "-B"}, args...)...) //nolint:gosec // the script under test, or its fault driver
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(p.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LOG="+p.log, "BATTLENET_GAMES="+p.games,
		"FAKE_RELEASE="+filepath.Join(p.dir, "release.json"), "FAKE_ASSET="+filepath.Join(p.dir, "asset"))
	cmd.Env = append(cmd.Env, env...)
	if list != nil {
		cmd.Env = append(cmd.Env, "WOW_ADDONS="+*list)
	}
	b, err := cmd.CombinedOutput()
	t.Logf("output:\n%s", b)
	if err != nil && !mayFail {
		t.Fatalf("wow-addons-update must never fail: %v", err)
	}
	calls, err := os.ReadFile(p.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls), string(b)
}

func (p *wowPod) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func (p *wowPod) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// assertHandInstalledKept checks the user's own addon is as they left it.
func (p *wowPod) assertHandInstalledKept() {
	p.t.Helper()
	if got := p.read(filepath.Join(p.retail, "Details", "Details.toc")); got != "hand-installed" {
		p.t.Errorf("hand-installed Details.toc = %q, want it untouched", got)
	}
}

// assertNothingOutside checks no file named evil was written anywhere.
func (p *wowPod) assertNothingOutside() {
	p.t.Helper()
	_ = filepath.WalkDir(p.dir, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && strings.Contains(d.Name(), "evil") {
			p.t.Errorf("hostile zip wrote %s", path)
		}
		return nil
	})
}

func TestWoWAddonsInstallsListedAddon(t *testing.T) {
	p := newWoWPod(t)
	p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
	calls, _ := p.run(new(consolePortList))
	if !strings.Contains(calls, "download https://github.com/seblindfors/ConsolePort/releases/download/3.3.12/ConsolePort-3.3.12.zip") {
		t.Fatalf("want the matching asset downloaded, calls:\n%s", calls)
	}
	for _, f := range []string{"ConsolePort/ConsolePort.toc", "ConsolePort_Bar/ConsolePort_Bar.toc"} {
		if got := p.read(filepath.Join(p.retail, f)); !strings.HasSuffix(got, " 3.3.12") {
			t.Errorf("retail %s = %q, want 3.3.12's", f, got)
		}
	}
	if p.exists(filepath.Join(p.classic, "ConsolePort")) {
		t.Error("ConsolePort is listed for retail only, but was installed into classic")
	}
	p.assertHandInstalledKept()
	var manifest map[string]struct{ Folders []string }
	if err := json.Unmarshal([]byte(p.read(filepath.Join(filepath.Dir(p.retail), ".direwolf-addons.json"))), &manifest); err != nil {
		t.Fatal(err)
	}
	if got := manifest["ConsolePort"].Folders; len(manifest) != 1 || strings.Join(got, ",") != "ConsolePort,ConsolePort_Bar" {
		t.Errorf("manifest = %+v, want ConsolePort's two folders only", manifest)
	}
	// The next run (the periodic one, or the next Library start) finds it current.
	if calls, _ = p.run(new(consolePortList)); calls != "" {
		t.Errorf("an installed latest release must not be downloaded again, calls:\n%s", calls)
	}
	// Nor is a staging folder left beside AddOns.
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p.retail), ".direwolf-addons-*")); len(left) != 0 {
		t.Errorf("staging left behind: %v", left)
	}
}

func TestWoWAddonsUpdatesToNewerRelease(t *testing.T) {
	p := newWoWPod(t)
	p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
	p.run(new(consolePortList))
	p.publish("3.4.0", consolePortZip(t, "3.4.0", "ConsolePort", "ConsolePort_Menu"), "")
	if calls, _ := p.run(new(consolePortList)); !strings.Contains(calls, "ConsolePort-3.4.0.zip") {
		t.Fatalf("want the newer release downloaded, calls:\n%s", calls)
	}
	for _, f := range []string{"ConsolePort/ConsolePort.toc", "ConsolePort_Menu/ConsolePort_Menu.toc", "ConsolePort/Libs/LibStub/LibStub.lua"} {
		if got := p.read(filepath.Join(p.retail, f)); !strings.HasSuffix(got, " 3.4.0") {
			t.Errorf("%s = %q, want 3.4.0's", f, got)
		}
	}
	if p.exists(filepath.Join(p.retail, "ConsolePort_Bar")) {
		t.Error("ConsolePort_Bar, dropped by the new release, was left installed")
	}
	p.assertHandInstalledKept()
}

// Each update that fails keeps the installed copy as it was.
func TestWoWAddonsFailedUpdateKeepsOldCopy(t *testing.T) {
	cases := map[string]func(p *wowPod){
		"checksum mismatch": func(p *wowPod) {
			p.publish("3.4.0", consolePortZip(p.t, "3.4.0", "ConsolePort"), "sha256:"+strings.Repeat("ab", 32))
		},
		"no digest": func(p *wowPod) {
			p.publish("3.4.0", consolePortZip(p.t, "3.4.0", "ConsolePort"), "md5:0123")
		},
		"zip slip": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "ConsolePort/../../evil.lua", body: "pwned"}), "")
		},
		"parent folder": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "../evil.lua", body: "pwned"}), "")
		},
		"absolute path": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "/tmp/evil.lua", body: "pwned"}), "")
		},
		"backslash path": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: `ConsolePort\..\..\evil.lua`, body: "pwned"}), "")
		},
		"symlink": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "ConsolePort/evil", body: "/etc", mode: fs.ModeSymlink | 0o777}), "")
		},
		"file outside a folder": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "evil.lua", body: "pwned"}), "")
		},
		"oversized": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "ConsolePort/evil.blp", body: "small", size: 2 << 30}), "")
		},
		"too many files": func(p *wowPod) {
			entries := make([]zipEntry, 1, 20002)
			entries[0] = zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"}
			for i := range 20001 {
				entries = append(entries, zipEntry{name: fmt.Sprintf("ConsolePort/f/%d", i)})
			}
			p.publish("3.4.0", makeZip(p.t, entries...), "")
		},
		"duplicate entry": func(p *wowPod) {
			p.publish("3.4.0", makeZip(p.t, zipEntry{name: "ConsolePort/ConsolePort.toc", body: "3.4.0"},
				zipEntry{name: "ConsolePort/ConsolePort.toc", body: "evil"}), "")
		},
		"not a zip": func(p *wowPod) {
			p.publish("3.4.0", []byte("not a zip"), "")
		},
		"offline": func(p *wowPod) {
			p.publish("3.4.0", consolePortZip(p.t, "3.4.0", "ConsolePort"), "")
			p.env = append(p.env, "FAKE_OFFLINE=1")
		},
		"two matching assets": func(p *wowPod) {
			p.publish("3.4.0", consolePortZip(p.t, "3.4.0", "ConsolePort"), "")
			p.release = bytes.Replace(p.release, []byte(`"release.json"`), []byte(`"ConsolePort-classic.zip"`), 1)
		},
	}
	for name, publish := range cases {
		t.Run(name, func(t *testing.T) {
			p := newWoWPod(t)
			p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
			p.run(new(consolePortList))
			publish(p)
			_, out := p.run(new(consolePortList), p.env...)
			if !strings.Contains(out, "keeping what is installed") {
				t.Errorf("want the update refused, output:\n%s", out)
			}
			for _, f := range []string{"ConsolePort/ConsolePort.toc", "ConsolePort_Bar/ConsolePort_Bar.toc"} {
				if got := p.read(filepath.Join(p.retail, f)); got != strings.Split(f, "/")[0]+" 3.3.12" {
					t.Errorf("%s = %q, want 3.3.12's kept", f, got)
				}
			}
			p.assertNothingOutside()
			p.assertHandInstalledKept()
			if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p.retail), ".direwolf-addons-*")); len(left) != 0 {
				t.Errorf("staging left behind: %v", left)
			}
		})
	}
}

func TestWoWAddonsUninstallsUnlistedAddon(t *testing.T) {
	p := newWoWPod(t)
	p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
	p.run(new(consolePortList))
	if calls, _ := p.run(new("[]")); calls != "" {
		t.Errorf("an empty list must download nothing, calls:\n%s", calls)
	}
	for _, f := range []string{"ConsolePort", "ConsolePort_Bar"} {
		if p.exists(filepath.Join(p.retail, f)) {
			t.Errorf("%s is still installed after ConsolePort was unlisted", f)
		}
	}
	p.assertHandInstalledKept()
	if got := p.read(filepath.Join(filepath.Dir(p.retail), ".direwolf-addons.json")); strings.Contains(got, "ConsolePort") {
		t.Errorf("manifest still records ConsolePort: %s", got)
	}
}

// pinnedList lists Pawn as a url source, pinned to zip's SHA256.
func pinnedList(zip []byte) string {
	sum := sha256.Sum256(zip)
	return `[{"name":"Pawn","source":{"type":"url","url":"https://mediafilez.forgecdn.net/files/9283/553/Pawn-2.14.0.zip","sha256":"` +
		hex.EncodeToString(sum[:]) + `"},"flavors":["retail"]}]`
}

// A url source installs its one zip, only on a SHA256 match, and once
// installed needs no network (XERK-2486).
func TestWoWAddonsInstallsPinnedURL(t *testing.T) {
	p := newWoWPod(t)
	v1 := makeZip(t, zipEntry{name: "Pawn/Pawn.toc", body: "Pawn 2.14.0"})
	p.assetZip = v1
	calls, _ := p.run(new(pinnedList(v1)))
	if calls != "download https://mediafilez.forgecdn.net/files/9283/553/Pawn-2.14.0.zip\n" {
		t.Fatalf("want the pinned URL downloaded once, calls:\n%s", calls)
	}
	if got := p.read(filepath.Join(p.retail, "Pawn", "Pawn.toc")); got != "Pawn 2.14.0" {
		t.Errorf("Pawn.toc = %q, want the pinned zip's", got)
	}
	p.assertHandInstalledKept()
	// Current: nothing is fetched, not even with the network down.
	if calls, _ = p.run(new(pinnedList(v1)), "FAKE_OFFLINE=1"); calls != "" {
		t.Errorf("an installed pin must not be downloaded again, calls:\n%s", calls)
	}
	// Served something else (a CDN swap, a tampered file): refused, the pin kept.
	p.assetZip = makeZip(t, zipEntry{name: "Pawn/Pawn.toc", body: "evil"})
	pinV2 := strings.Replace(pinnedList(v1), "Pawn-2.14.0.zip", "Pawn-2.15.0.zip", 1)
	pinV2 = strings.Replace(pinV2, hex.EncodeToString(sha256Of(v1)), strings.Repeat("1", 64), 1)
	if _, out := p.run(new(pinV2)); !strings.Contains(out, "does not match its SHA256") {
		t.Errorf("want the mismatching zip refused, output:\n%s", out)
	}
	if got := p.read(filepath.Join(p.retail, "Pawn", "Pawn.toc")); got != "Pawn 2.14.0" {
		t.Errorf("Pawn.toc = %q after a refused update, want 2.14.0's kept", got)
	}
	// A new pin that matches replaces it.
	v2 := makeZip(t, zipEntry{name: "Pawn/Pawn.toc", body: "Pawn 2.15.0"})
	p.assetZip = v2
	p.run(new(strings.Replace(pinnedList(v2), "Pawn-2.14.0.zip", "Pawn-2.15.0.zip", 1)))
	if got := p.read(filepath.Join(p.retail, "Pawn", "Pawn.toc")); got != "Pawn 2.15.0" {
		t.Errorf("Pawn.toc = %q, want the new pin's", got)
	}
	// Unlisted, it is uninstalled like any managed addon.
	p.run(new("[]"))
	if p.exists(filepath.Join(p.retail, "Pawn")) {
		t.Error("Pawn is still installed after it was unlisted")
	}
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// Moving an addon to another flavor installs it there and removes it from the old one.
func TestWoWAddonsFollowsFlavors(t *testing.T) {
	p := newWoWPod(t)
	p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort"), "")
	p.run(new(consolePortList))
	p.run(new(strings.Replace(consolePortList, `["retail"]`, `["classic"]`, 1)))
	if p.exists(filepath.Join(p.retail, "ConsolePort")) {
		t.Error("ConsolePort left in retail after its flavors dropped retail")
	}
	if !p.exists(filepath.Join(p.classic, "ConsolePort", "ConsolePort.toc")) {
		t.Error("ConsolePort not installed into classic")
	}
	// A flavor not installed (classic_era here) is never created.
	p.run(new(strings.Replace(consolePortList, `["retail"]`, `["classic_era"]`, 1)))
	if p.exists(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(p.retail))), "_classic_era_")) {
		t.Error("created a WoW flavor that is not installed")
	}
}

// A folder the user installed by hand is never replaced or removed, even
// when it has a listed addon's name.
func TestWoWAddonsNeverReplacesHandInstalled(t *testing.T) {
	p := newWoWPod(t)
	p.write(filepath.Join(p.retail, "ConsolePort_Bar", "ConsolePort_Bar.toc"), "mine")
	p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
	_, out := p.run(new(consolePortList))
	if !strings.Contains(out, "not installed by direwolf") {
		t.Errorf("want the install refused, output:\n%s", out)
	}
	if p.exists(filepath.Join(p.retail, "ConsolePort")) {
		t.Error("installed part of an addon whose other folder is the user's")
	}
	p.run(new("[]"))
	if got := p.read(filepath.Join(p.retail, "ConsolePort_Bar", "ConsolePort_Bar.toc")); got != "mine" {
		t.Errorf("hand-installed ConsolePort_Bar = %q, want it untouched", got)
	}
}

// A list the script can't read, or none at all, must not read as "none":
// that would uninstall every managed addon.
func TestWoWAddonsKeepsAddonsWithoutValidList(t *testing.T) {
	for name, l := range map[string]*string{
		"unset":        nil,
		"not json":     new("ConsolePort"),
		"null":         new("null"),
		"bad flavor":   new(strings.Replace(consolePortList, `["retail"]`, `["../x"]`, 1)),
		"path as name": new(strings.Replace(consolePortList, `"ConsolePort"`, `"../ConsolePort"`, 1)),
		"other source": new(strings.Replace(consolePortList, `"github"`, `"curseforge"`, 1)),
		"http url":     new(strings.Replace(pinnedList(nil), "https://", "http://", 1)),
		"url creds":    new(strings.Replace(pinnedList(nil), "https://", "https://u:p@", 1)),
		"url quote":    new(strings.Replace(pinnedList(nil), "Pawn-2.14.0.zip", "Pawn'.zip", 1)),
		"url no sha":   new(strings.Replace(pinnedList(nil), `"sha256":"`, `"sha":"`, 1)),
		"url and repo": new(strings.Replace(pinnedList(nil), `"type":"url"`, `"type":"url","repo":"a/b"`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			p := newWoWPod(t)
			p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort"), "")
			p.run(new(consolePortList))
			if calls, _ := p.run(l); calls != "" {
				t.Errorf("want no downloads, calls:\n%s", calls)
			}
			if !p.exists(filepath.Join(p.retail, "ConsolePort", "ConsolePort.toc")) {
				t.Error("ConsolePort was uninstalled")
			}
		})
	}
}

// The service runs the script as the desktop user, at start and then
// periodically, and never as root.
func TestWoWAddonsServiceRunsAsDesktopUser(t *testing.T) {
	run, err := os.ReadFile("root/etc/s6-overlay/s6-rc.d/svc-wow-addons/run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(run), "s6-setuidgid abc wow-addons-update") || !strings.Contains(string(run), "sleep ") {
		t.Errorf("svc-wow-addons must loop running wow-addons-update as abc:\n%s", run)
	}
	for _, f := range []string{
		"root/etc/s6-overlay/s6-rc.d/svc-wow-addons/type",
		"root/etc/s6-overlay/s6-rc.d/user/contents.d/svc-wow-addons",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
}

// A swap that fails partway puts every old folder back and keeps the addon
// recorded as direwolf's, so a later release still updates it.
func TestWoWAddonsFailedSwapRollsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames a read-only folder anyway")
	}
	p := newWoWPod(t)
	p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
	p.run(new(consolePortList))
	// Moving a folder to another parent needs write access to it: ConsolePort
	// is swapped out, then ConsolePort_Bar fails.
	bar := filepath.Join(p.retail, "ConsolePort_Bar")
	if err := os.Chmod(bar, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bar, 0o755) })
	p.publish("3.4.0", consolePortZip(t, "3.4.0", "ConsolePort", "ConsolePort_Bar"), "")
	if _, out := p.run(new(consolePortList)); !strings.Contains(out, "keeping what is installed") {
		t.Fatalf("want the swap to fail, output:\n%s", out)
	}
	for _, f := range []string{"ConsolePort/ConsolePort.toc", "ConsolePort_Bar/ConsolePort_Bar.toc"} {
		if got := p.read(filepath.Join(p.retail, f)); got != strings.Split(f, "/")[0]+" 3.3.12" {
			t.Errorf("%s = %q after a failed swap, want 3.3.12's", f, got)
		}
	}
	if err := os.Chmod(bar, 0o755); err != nil {
		t.Fatal(err)
	}
	p.run(new(consolePortList))
	if got := p.read(filepath.Join(p.retail, "ConsolePort_Bar", "ConsolePort_Bar.toc")); got != "ConsolePort_Bar 3.4.0" {
		t.Errorf("ConsolePort_Bar = %q, want it still managed and updated to 3.4.0", got)
	}
}

// renameFaults runs the real script with its Nth os.rename failing: "kill"
// SIGKILLs it there (a pod deleted mid-swap), "double" raises, and so does
// the rename after it (the rollback's first restore).
const renameFaults = `import importlib.machinery, importlib.util, os, signal, sys
script, mode, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
loader = importlib.machinery.SourceFileLoader("wow_addons_update", script)
m = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
loader.exec_module(m)
real, calls = os.rename, [0]
def rename(a, b):
    calls[0] += 1
    if calls[0] == n and mode == "kill":
        os.kill(os.getpid(), signal.SIGKILL)
    if calls[0] in (n, n + 1):
        raise OSError("injected")
    return real(a, b)
os.rename = rename
m.main()
`

// However an update dies or fails mid-swap, the next run updates the addon:
// its folders are never left looking hand-installed.
func TestWoWAddonsSurvivesInterruptedSwap(t *testing.T) {
	driver := filepath.Join(t.TempDir(), "faults.py")
	if err := os.WriteFile(driver, []byte(renameFaults), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"kill", "double"} {
		// v1 to v2 makes 5 renames: ConsolePort out and in, ConsolePort_Bar
		// out, ConsolePort_Menu in, ConsolePort_World in.
		for n := 1; n <= 5; n++ {
			t.Run(fmt.Sprintf("%s at rename %d", mode, n), func(t *testing.T) {
				p := newWoWPod(t)
				p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort", "ConsolePort_Bar"), "")
				p.run(new(consolePortList))
				// Two new folders: one is live before the other moves in.
				p.publish("3.4.0", consolePortZip(t, "3.4.0", "ConsolePort", "ConsolePort_Menu", "ConsolePort_World"), "")
				p.runArgs(new(consolePortList), []string{driver, wowAddonsUpdate, mode, strconv.Itoa(n)}, true)
				_, out := p.run(new(consolePortList))
				if strings.Contains(out, "not installed by direwolf") {
					t.Fatalf("the interrupted update left folders unrecorded, output:\n%s", out)
				}
				for _, f := range []string{"ConsolePort/ConsolePort.toc", "ConsolePort_Menu/ConsolePort_Menu.toc", "ConsolePort_World/ConsolePort_World.toc"} {
					if got := p.read(filepath.Join(p.retail, f)); !strings.HasSuffix(got, " 3.4.0") {
						t.Errorf("%s = %q, want 3.4.0's", f, got)
					}
				}
				if p.exists(filepath.Join(p.retail, "ConsolePort_Bar")) {
					t.Error("ConsolePort_Bar, dropped by 3.4.0, is still installed")
				}
				p.assertHandInstalledKept()
			})
		}
	}
}
