package library

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // CurseForge publishes SHA1
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

// wowCurlStub serves $FAKE_RELEASE as every repo's latest release,
// $FAKE_CF_MOD and $FAKE_CF_FILE as CurseForge's API (logging each call with
// its argv, its stdin to $CF_STDIN) and $FAKE_ASSET for every download,
// logging each download. FAKE_OFFLINE fails them all.
// Like real curl, a download fails without a numeric --max-filesize; it also
// fails unless held to https through redirects with URL globbing off.
const wowCurlStub = `[ -n "$FAKE_OFFLINE" ] && exit 6
for a; do [ "$prev" = -o ] && out=$a; [ "$prev" = --max-filesize ] && max=$a
  [ "$prev $a" = "--proto =https" ] && proto=1; [ "$prev $a" = "--proto-redir =https" ] && redir=1
  [ "$a" = --globoff ] && globoff=1; prev=$a; done
case $a in https://api.curseforge.com/*)
  echo "api $*" >>"$LOG"; cat >"$CF_STDIN"
  case $a in */files/*) exec cat "$FAKE_CF_FILE";; *) exec cat "$FAKE_CF_MOD";; esac
esac
[ -z "$out" ] && exec cat "$FAKE_RELEASE"
case "$max" in ''|*[!0-9]*) echo "curl: option --max-filesize: is badly used here" >&2; exit 2;; esac
[ -n "$proto$redir$globoff" ] && [ "$proto$redir$globoff" = 111 ] || { echo "curl stub: download not held to https / globoff" >&2; exit 2; }
echo "download $a" >>"$LOG"
[ "$(wc -c <"$FAKE_ASSET")" -gt "$max" ] && exit 63
cp "$FAKE_ASSET" "$out"`

// wowPod is a Library pod's games volume with WoW installed, kept across runs.
type wowPod struct {
	t                 *testing.T
	dir, games, log   string
	retail, classic   string // each flavor's AddOns
	release, assetZip []byte
	cfMod, cfFile     []byte   // CurseForge's API answers
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
	for f, b := range map[string][]byte{"release.json": p.release, "asset": p.assetZip, "log": nil, "cf-mod": p.cfMod, "cf-file": p.cfFile} {
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
		"FAKE_RELEASE="+filepath.Join(p.dir, "release.json"), "FAKE_ASSET="+filepath.Join(p.dir, "asset"),
		"FAKE_CF_MOD="+filepath.Join(p.dir, "cf-mod"), "FAKE_CF_FILE="+filepath.Join(p.dir, "cf-file"),
		"CF_STDIN="+filepath.Join(p.dir, "cf-stdin"))
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

// tomTomList lists TomTom from a fixed URL pinned to asset's SHA256.
func tomTomList(url string, asset []byte) string {
	sum := sha256.Sum256(asset)
	return `[{"name":"TomTom","source":{"type":"url","url":"` + url + `","sha256":"` + hex.EncodeToString(sum[:]) + `"},"flavors":["retail"]}]`
}

// A url source installs the pinned file, fetches nothing while it is
// current, and moves only when the list does.
func TestWoWAddonsInstallsPinnedURL(t *testing.T) {
	const v1 = "https://mediafilez.forgecdn.net/files/7001/100/TomTom-v4.0.9.zip"
	p := newWoWPod(t)
	zip1 := makeZip(t, zipEntry{name: "TomTom/TomTom.toc", body: "4.0.9"})
	p.assetZip = zip1
	if calls, _ := p.run(new(tomTomList(v1, zip1))); calls != "download "+v1+"\n" {
		t.Fatalf("want only the pinned URL downloaded, calls:\n%s", calls)
	}
	if got := p.read(filepath.Join(p.retail, "TomTom", "TomTom.toc")); got != "4.0.9" {
		t.Errorf("TomTom.toc = %q, want 4.0.9's", got)
	}
	// Current: no network at all, so offline changes nothing.
	if calls, out := p.run(new(tomTomList(v1, zip1)), "FAKE_OFFLINE=1"); calls != "" || !strings.Contains(out, "TomTom TomTom-v4.0.9.zip is up to date") {
		t.Errorf("a current pinned addon must not be fetched, calls:\n%s", calls)
	}
	// The file at the URL no longer matching the pin keeps what is installed.
	p.assetZip = makeZip(t, zipEntry{name: "TomTom/TomTom.toc", body: "tampered"})
	const v2 = "https://mediafilez.forgecdn.net/files/7002/200/TomTom-v4.1.0.zip"
	zip2 := makeZip(t, zipEntry{name: "TomTom/TomTom.toc", body: "4.1.0"})
	if _, out := p.run(new(tomTomList(v2, zip2))); !strings.Contains(out, "does not match its SHA256") {
		t.Errorf("want the mismatch refused, output:\n%s", out)
	}
	if got := p.read(filepath.Join(p.retail, "TomTom", "TomTom.toc")); got != "4.0.9" {
		t.Errorf("after a mismatch TomTom.toc = %q, want 4.0.9's kept", got)
	}
	p.assetZip = zip2
	if calls, _ := p.run(new(tomTomList(v2, zip2))); calls != "download "+v2+"\n" {
		t.Fatalf("want the new pin downloaded, calls:\n%s", calls)
	}
	if got := p.read(filepath.Join(p.retail, "TomTom", "TomTom.toc")); got != "4.1.0" {
		t.Errorf("TomTom.toc = %q, want 4.1.0's", got)
	}
	p.assertHandInstalledKept()
}

// cfList lists Baganator as a curseforge source.
const cfList = `[{"name":"Baganator","source":{"type":"curseforge","project":914482},"flavors":["retail"]}]`

// publishCF makes asset Baganator's latest retail release on CurseForge, file
// id named name, with sha1 as its published SHA1 ("" for the asset's own).
// Newer classic and beta files are listed too, and must be passed over.
func (p *wowPod) publishCF(id int, name string, asset []byte, sha1 string) {
	p.t.Helper()
	if sha1 == "" {
		sum := sha1Sum(asset)
		sha1 = hex.EncodeToString(sum[:])
	}
	mod, err := json.Marshal(map[string]any{"data": map[string]any{
		"id": 914482, "gameId": 1,
		"latestFilesIndexes": []map[string]any{
			{"fileId": id + 2, "gameVersionTypeId": 67408, "releaseType": 1, "filename": "classic.zip"},
			{"fileId": id + 1, "gameVersionTypeId": 517, "releaseType": 2, "filename": "beta.zip"},
			{"fileId": id, "gameVersionTypeId": 517, "releaseType": 1, "filename": name},
			{"fileId": id - 5, "gameVersionTypeId": 517, "releaseType": 1, "filename": "old.zip"},
		},
	}})
	if err != nil {
		p.t.Fatal(err)
	}
	file, err := json.Marshal(map[string]any{"data": map[string]any{
		"id": id, "modId": 914482, "isAvailable": true, "fileName": name, "fileLength": len(asset),
		"hashes":      []map[string]any{{"value": sha1, "algo": 1}, {"value": strings.Repeat("0", 32), "algo": 2}},
		"downloadUrl": "https://edge.forgecdn.net/files/x/" + name,
	}})
	if err != nil {
		p.t.Fatal(err)
	}
	p.cfMod, p.cfFile, p.assetZip = mod, file, asset
}

// A curseforge source follows the project's latest retail release, found
// with the API key, and installs it only on a SHA1 match (XERK-2489).
func TestWoWAddonsFollowsCurseForgeRelease(t *testing.T) {
	const key = "CURSEFORGE_API_KEY=$2a$10$secretkey"
	p := newWoWPod(t)
	v839 := makeZip(t, zipEntry{name: "Baganator/Baganator.toc", body: "Baganator 839"})
	p.publishCF(9353264, "Baganator-839.zip", v839, "")
	// No key: nothing is called, and nothing is installed.
	if calls, out := p.run(new(cfList)); calls != "" || !strings.Contains(out, "CURSEFORGE_API_KEY is not set") {
		t.Errorf("want no calls without a key, calls:\n%s", calls)
	}
	calls, _ := p.run(new(cfList), key)
	if strings.Contains(calls, "secretkey") {
		t.Errorf("the API key is in curl's argv:\n%s", calls)
	}
	// curl would send the key header on to wherever a redirect points.
	if strings.Contains(calls, "-L") || strings.Contains(calls, "SL ") || strings.Contains(calls, "--location") {
		t.Errorf("the API calls follow redirects:\n%s", calls)
	}
	if !strings.HasSuffix(calls, "download https://mediafilez.forgecdn.net/files/9353/264/Baganator-839.zip\n") ||
		!strings.Contains(calls, "https://api.curseforge.com/v1/mods/914482/files/9353264\n") {
		t.Fatalf("want the retail release's CDN file downloaded, calls:\n%s", calls)
	}
	if got := p.read(filepath.Join(p.dir, "cf-stdin")); got != "x-api-key: $2a$10$secretkey\n" {
		t.Errorf("curl's header input = %q, want the key", got)
	}
	if got := p.read(filepath.Join(p.retail, "Baganator", "Baganator.toc")); got != "Baganator 839" {
		t.Errorf("Baganator.toc = %q, want 839's", got)
	}
	p.assertHandInstalledKept()
	// Current: looked up, not downloaded again.
	if calls, _ = p.run(new(cfList), key); strings.Contains(calls, "download") {
		t.Errorf("a current release must not be downloaded again, calls:\n%s", calls)
	}
	// A release that does not match its published SHA1 is refused, 839 kept.
	evil := makeZip(t, zipEntry{name: "Baganator/Baganator.toc", body: "evil"})
	p.publishCF(9353300, "Baganator-840.zip", evil, strings.Repeat("1", 40))
	if _, out := p.run(new(cfList), key); !strings.Contains(out, "does not match its SHA1") {
		t.Errorf("want the mismatching zip refused, output:\n%s", out)
	}
	if got := p.read(filepath.Join(p.retail, "Baganator", "Baganator.toc")); got != "Baganator 839" {
		t.Errorf("Baganator.toc = %q after a refused update, want 839's kept", got)
	}
	// A matching release replaces it; its name is quoted into the CDN path,
	// and the id's last three digits lose their leading zeros as the CDN's do.
	v841 := makeZip(t, zipEntry{name: "Baganator/Baganator.toc", body: "Baganator 841"})
	p.publishCF(9354007, "Baganator 841.zip", v841, "")
	if calls, _ = p.run(new(cfList), key); !strings.HasSuffix(calls, "download https://mediafilez.forgecdn.net/files/9354/7/Baganator%20841.zip\n") {
		t.Errorf("want 841 downloaded, calls:\n%s", calls)
	}
	if got := p.read(filepath.Join(p.retail, "Baganator", "Baganator.toc")); got != "Baganator 841" {
		t.Errorf("Baganator.toc = %q, want 841's", got)
	}
	// Offline, or the project gone, keeps what is installed.
	p.run(new(cfList), key, "FAKE_OFFLINE=1")
	if got := p.read(filepath.Join(p.retail, "Baganator", "Baganator.toc")); got != "Baganator 841" {
		t.Errorf("Baganator.toc = %q offline, want 841's kept", got)
	}
}

// Anything odd in CurseForge's answers is refused before a download.
func TestWoWAddonsRefusesOddCurseForgeAnswers(t *testing.T) {
	zip := makeZip(t, zipEntry{name: "Baganator/Baganator.toc", body: "Baganator 839"})
	for name, edit := range map[string]func(p *wowPod){
		"other game": func(p *wowPod) { p.cfMod = []byte(strings.Replace(string(p.cfMod), `"gameId":1`, `"gameId":432`, 1)) },
		"other mod": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"modId":914482`, `"modId":1`, 1))
		},
		"no retail": func(p *wowPod) {
			p.cfMod = []byte(strings.ReplaceAll(string(p.cfMod), `"gameVersionTypeId":517`, `"gameVersionTypeId":1`))
		},
		"no sha1": func(p *wowPod) { p.cfFile = []byte(strings.Replace(string(p.cfFile), `"algo":1`, `"algo":3`, 1)) },
		"slash name": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"fileName":"Baganator-839.zip"`, `"fileName":"../x.zip"`, 1))
		},
		"not zip": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"fileName":"Baganator-839.zip"`, `"fileName":"x.exe"`, 1))
		},
		"unavailable": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"isAvailable":true`, `"isAvailable":false`, 1))
		},
		"huge": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"fileLength":`, `"fileLength":999999999,"was":`, 1))
		},
		"not json": func(p *wowPod) { p.cfMod = []byte("<html>") },
		// RecursionError in Python's json: not an exception the others share.
		"deep json": func(p *wowPod) { p.cfMod = []byte(strings.Repeat("[", 200000)) },
		"int index": func(p *wowPod) {
			p.cfMod = []byte(`{"data":{"id":914482,"gameId":1,"latestFilesIndexes":[7]}}`)
		},
		"string hash": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"hashes":[`, `"hashes":["x"],"was":[`, 1))
		},
		// Downloaded, then refused.
		"wrong size": func(p *wowPod) {
			p.cfFile = []byte(strings.Replace(string(p.cfFile), `"fileLength":`, `"fileLength":`+strconv.Itoa(len(zip)+1)+`,"was":`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newWoWPod(t)
			p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort"), "")
			p.publishCF(9353264, "Baganator-839.zip", zip, "")
			edit(p)
			// ConsolePort, listed after it, is still processed.
			list := strings.TrimSuffix(cfList, "]") + "," + strings.TrimPrefix(consolePortList, "[")
			calls, out := p.run(new(list), "CURSEFORGE_API_KEY=k")
			if !strings.Contains(out, "cannot update Baganator") || p.exists(filepath.Join(p.retail, "Baganator")) {
				t.Errorf("want Baganator refused, output:\n%s", out)
			}
			if !strings.Contains(calls, "download https://github.com/seblindfors/ConsolePort/") {
				t.Errorf("ConsolePort, listed after Baganator, was skipped; calls:\n%s", calls)
			}
		})
	}
}

func sha1Sum(b []byte) [20]byte {
	return sha1.Sum(b) //nolint:gosec // CurseForge publishes SHA1
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
		"other source": new(strings.Replace(consolePortList, `"github"`, `"other"`, 1)),
		"cf and repo":  new(strings.Replace(consolePortList, `"github"`, `"curseforge"`, 1)),
		"cf classic":   new(strings.Replace(cfList, `["retail"]`, `["retail","classic"]`, 1)),
		"cf str id":    new(strings.Replace(cfList, `914482`, `"914482"`, 1)),
		"cf bool id":   new(strings.Replace(cfList, `914482`, `true`, 1)),
		"cf zero id":   new(strings.Replace(cfList, `914482`, `0`, 1)),
		"cf and url":   new(strings.Replace(cfList, `"project"`, `"url":"https://a.b/c.zip","project"`, 1)),
		"github proj":  new(strings.Replace(consolePortList, `"type":"github"`, `"type":"github","project":1`, 1)),
		"url project":  new(strings.Replace(tomTomList("https://a.example/t.zip", nil), `"url":`, `"project":1,"url":`, 1)),
		"url, no pin":  new(`[{"name":"TomTom","source":{"type":"url","url":"https://a.example/t.zip"},"flavors":["retail"]}]`),
		"url, http":    new(strings.Replace(tomTomList("https://a.example/t.zip", nil), "https://", "http://", 1)),
		"url, query":   new(tomTomList("https://a.example/t.zip?x=1", nil)),
		"url and repo": new(strings.Replace(tomTomList("https://a.example/t.zip", nil), `"url":`, `"repo":"a/b","url":`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			p := newWoWPod(t)
			p.publish("3.3.12", consolePortZip(t, "3.3.12", "ConsolePort"), "")
			p.run(new(consolePortList))
			calls, out := p.run(l)
			if calls != "" {
				t.Errorf("want no downloads, calls:\n%s", calls)
			}
			// Not vacuous: ConsolePort is current, so a list misread as valid
			// would download nothing either.
			if !strings.Contains(out, "leaving addons alone") {
				t.Errorf("want the list refused, output:\n%s", out)
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
