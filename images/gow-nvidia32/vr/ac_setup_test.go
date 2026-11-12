//go:build unix

package vr

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// acSetup is a shared home with Assetto Corsa and Proton 9.0 in Steam's
// default library, as ac-setup finds it, plus the image's siblings beside a
// copy of the script. Proton, Wine, winetricks and curl are stubs that log
// their arguments to calls.
type acSetup struct {
	dir, bin, home, apps, game, compat, calls string
	// state is the User's AC Documents (the ac-documents stateMount).
	state string
	pins  map[string]map[string]string
}

func newACSetup(t *testing.T) *acSetup {
	t.Helper()
	s := &acSetup{dir: t.TempDir(), bin: t.TempDir(), home: t.TempDir(), state: t.TempDir(), pins: map[string]map[string]string{}}
	s.calls = filepath.Join(s.bin, "calls.log")
	steam := filepath.Join(s.home, ".steam", "debian-installation")
	s.apps = filepath.Join(steam, "steamapps")
	s.game = filepath.Join(s.apps, "common", "assettocorsa")
	s.compat = filepath.Join(s.apps, "compatdata", "244210")
	proton := filepath.Join(s.apps, "common", "Proton 9.0 (Beta)")
	b, err := os.ReadFile("ac-setup")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.dir, "ac-setup"), string(b))
	for _, f := range []string{"selawk", "selawkb", "selawkl", "selawksl", "selawksb"} {
		mustWrite(t, filepath.Join(s.dir, "fonts", f+".ttf"), "font "+f)
	}
	writeExec(t, filepath.Join(s.dir, "winetricks"), `echo "winetricks $*" >>"$CALLS"
echo "LD_LIBRARY_PATH=$LD_LIBRARY_PATH WINEDLLPATH=$WINEDLLPATH" >>"$CALLS.env"
`)
	mustWrite(t, filepath.Join(steam, "config", "config.vdf"), vdfWithTool("proton_9"))
	mustWrite(t, filepath.Join(s.apps, "appmanifest_244210.acf"), "\"AppState\"\n{\n\t\"appid\"\t\t\"244210\"\n}\n")
	mustWrite(t, filepath.Join(s.apps, "appmanifest_2805730.acf"),
		"\"AppState\"\n{\n\t\"appid\"\t\t\"2805730\"\n\t\"installdir\"\t\t\"Proton 9.0 (Beta)\"\n}\n")
	mustWrite(t, filepath.Join(s.game, "acs.exe"), "ACS")
	mustWrite(t, filepath.Join(s.game, "AssettoCorsa.exe"), "LAUNCHER")
	if err := os.MkdirAll(filepath.Join(proton, "files", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Steam's prefix: what Proton makes on its first run.
	writeExec(t, filepath.Join(proton, "proton"), `echo "proton $*" >>"$CALLS"
mkdir -p "$STEAM_COMPAT_DATA_PATH/pfx/drive_c/users/steamuser/Documents"
echo reg >"$STEAM_COMPAT_DATA_PATH/pfx/system.reg"
echo 9.0-4 >"$STEAM_COMPAT_DATA_PATH/version"
`)
	writeExec(t, filepath.Join(proton, "files", "bin", "wine"), `echo "wine $*" >>"$CALLS"`+"\n")
	writeExec(t, filepath.Join(proton, "files", "bin", "wineserver"), `echo "wineserver $*" >>"$CALLS"`+"\n")
	writeExec(t, filepath.Join(s.bin, "curl"), `echo "curl $*" >>"$CALLS"
while [ $# -gt 1 ]; do [ "$1" = -o ] && { out=$2; shift; }; shift; done
exec cp "$1" "$out"
`)
	s.pin(t, "0.3.1", map[string]string{"dwrite.dll": "CSP 0.3.1", "extension/": "", "extension/config/vr_tweaks.ini": "[A]\r\n"})
	return s
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

func vdfWithTool(name string) string {
	return "\"InstallConfigStore\"\n{\n\t\"Software\"\n\t{\n\t\t\"Valve\"\n\t\t{\n\t\t\t\"Steam\"\n\t\t\t{\n" +
		"\t\t\t\t\"CompatToolMapping\"\n\t\t\t\t{\n\t\t\t\t\t\"244210\"\n\t\t\t\t\t{\n\t\t\t\t\t\t\"name\"\t\t\"" + name +
		"\"\n\t\t\t\t\t\t\"priority\"\t\t\"250\"\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t}\n}\n"
}

// pin makes a CSP archive of files (a name ending in / is a folder) and pins
// it as version in the script's ac-csp-versions.json.
func (s *acSetup) pin(t *testing.T, version string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.bin, "csp-"+version+".zip")
	mustWrite(t, path, buf.String())
	sum := sha256.Sum256(buf.Bytes())
	s.pins[version] = map[string]string{"url": path, "sha256": hex.EncodeToString(sum[:])}
	b, err := json.Marshal(s.pins)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.dir, "ac-csp-versions.json"), string(b))
}

func (s *acSetup) run(t *testing.T, env ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "python3", "-I", filepath.Join(s.dir, "ac-setup")) //nolint:gosec // the test's own copy
	cmd.Env = append([]string{
		"PATH=" + s.bin + ":" + os.Getenv("PATH"),
		"CALLS=" + s.calls,
		"DIREWOLF_AC_HOME=" + s.home,
		"DIREWOLF_AC_STATE=" + s.state,
		// A test run as root counts as one in its own user namespace.
		"DIREWOLF_AC_UID_MAP=0 1000 1",
		"DIREWOLF_AC_CSP=0.3.1",
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *acSetup) mustRun(t *testing.T, env ...string) string {
	t.Helper()
	out, err := s.run(t, env...)
	if err != nil {
		t.Fatalf("ac-setup: %v\n%s", err, out)
	}
	return out
}

func (s *acSetup) callLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// madePrefix is a prefix Proton finished making, as Steam leaves it.
func (s *acSetup) madePrefix(t *testing.T) {
	t.Helper()
	mustWrite(t, filepath.Join(s.compat, "pfx", "system.reg"), "reg")
	mustWrite(t, filepath.Join(s.compat, "version"), "9.0-4\n")
}

func (s *acSetup) flatCfg(name string) string {
	return filepath.Join(s.compat, "pfx", "drive_c", "users", "steamuser", "Documents", name, "cfg")
}

// A fresh install (no prefix, Steam's launcher as AssettoCorsa.exe) is set up
// in one run, and a second run does nothing at all.
func TestACSetupPreparesFreshInstallOnce(t *testing.T) {
	s := newACSetup(t)
	s.mustRun(t)
	calls := s.callLog(t)
	for _, want := range []string{
		"proton getcompatpath /\n",
		"winetricks --unattended corefonts d3dcompiler_47\n",
		`wine reg add HKLM\Software\Microsoft\Windows NT\CurrentVersion\FontSubstitutes /v Segoe UI /d Selawik /f` + "\n",
		"wineserver -w\n",
		"curl ",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in calls:\n%s", want, calls)
		}
	}
	// Proton's wine with the library paths Proton's own init_wine sets.
	tool := filepath.Join(s.apps, "common", "Proton 9.0 (Beta)", "files")
	if got, want := readString(t, s.calls+".env"), "LD_LIBRARY_PATH="+tool+"/lib64:"+tool+"/lib WINEDLLPATH="+tool+"/lib64/wine:"+tool+"/lib/wine\n"; got != want {
		t.Errorf("winetricks env = %q, want %q", got, want)
	}
	fonts := filepath.Join(s.compat, "pfx", "drive_c", "windows", "Fonts")
	for name, selawik := range map[string]string{"segoeui.ttf": "selawk", "segoeuib.ttf": "selawkb", "seguisb.ttf": "selawksb"} {
		if got := readString(t, filepath.Join(fonts, name)); got != "font "+selawik {
			t.Errorf("%s = %q, want Selawik's %s", name, got, selawik)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(s.game, "AssettoCorsa.exe"):                     "ACS",
		filepath.Join(s.game, "AssettoCorsa_original.exe"):            "LAUNCHER",
		filepath.Join(s.game, "dwrite.dll"):                           "CSP 0.3.1",
		filepath.Join(s.game, "extension", "config", "vr_tweaks.ini"): "[A]\r\n",
		filepath.Join(s.flatCfg("Assetto Corsa"), "video.ini"):        "[VIDEO]\r\nFULLSCREEN=1\r\n",
		filepath.Join(s.flatCfg("Assetto Corsa"), "controls.ini"):     "[HEADER]\r\nINPUT_METHOD=X360\r\n" + recenterIni,
	} {
		if got := readString(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if err := os.Remove(s.calls); err != nil {
		t.Fatal(err)
	}
	out := s.mustRun(t)
	if calls := s.callLog(t); calls != "" {
		t.Errorf("a set-up install ran:\n%s", calls)
	}
	if strings.Contains(out, "copying") || strings.Contains(out, "setting") {
		t.Errorf("a set-up install changed something:\n%s", out)
	}
}

// A prefix Steam already made is kept (no Proton run), and the flat config's
// other lines stay. Once the App command has linked Documents/Assetto Corsa to
// the per-User copy, the flat config is its .flat folder.
func TestACSetupKeepsExistingPrefixAndConfig(t *testing.T) {
	s := newACSetup(t)
	s.madePrefix(t)
	mustWrite(t, filepath.Join(s.flatCfg("Assetto Corsa.flat"), "video.ini"), "[VIDEO]\r\nWIDTH=1920\r\nFULLSCREEN=0\r\n[CAMERA]\r\nMODE=DEFAULT\r\n")
	perUser := t.TempDir()
	if err := os.Symlink(perUser, filepath.Dir(s.flatCfg("Assetto Corsa"))); err != nil {
		t.Fatal(err)
	}
	s.mustRun(t)
	if calls := s.callLog(t); strings.Contains(calls, "proton ") || !strings.Contains(calls, "winetricks") {
		t.Errorf("want winetricks in Steam's prefix and no new prefix:\n%s", calls)
	}
	if got, want := readString(t, filepath.Join(s.flatCfg("Assetto Corsa.flat"), "video.ini")),
		"[VIDEO]\r\nWIDTH=1920\r\nFULLSCREEN=1\r\n[CAMERA]\r\nMODE=DEFAULT\r\n"; got != want {
		t.Errorf("flat video.ini = %q, want %q", got, want)
	}
	if entries, _ := os.ReadDir(perUser); len(entries) != 0 {
		t.Errorf("the per-User folder was written through the link: %v", entries)
	}
}

// Steam puts its launcher back on an update or verify: the next launch swaps
// again and keeps the new launcher. An updated acs.exe replaces the old copy,
// which is never taken for the launcher.
func TestACSetupRedoesExeSwapAfterSteamRestoresLauncher(t *testing.T) {
	s := newACSetup(t)
	s.mustRun(t)
	if err := os.Remove(s.calls); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.game, "AssettoCorsa.exe"), "LAUNCHER 2")
	s.mustRun(t)
	mustWrite(t, filepath.Join(s.game, "acs.exe"), "ACS 2")
	s.mustRun(t)
	for path, want := range map[string]string{
		filepath.Join(s.game, "AssettoCorsa.exe"):          "ACS 2",
		filepath.Join(s.game, "AssettoCorsa_original.exe"): "LAUNCHER 2",
	} {
		if got := readString(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if calls := s.callLog(t); calls != "" {
		t.Errorf("an exe swap ran more than the swap:\n%s", calls)
	}
}

// Bumping DIREWOLF_AC_CSP reinstalls only CSP; unset leaves it alone.
func TestACSetupCSPVersionBumpRerunsOnlyCSP(t *testing.T) {
	s := newACSetup(t)
	s.mustRun(t)
	if err := os.Remove(s.calls); err != nil {
		t.Fatal(err)
	}
	if out := s.mustRun(t, "DIREWOLF_AC_CSP="); !strings.Contains(out, "CSP left as it is") {
		t.Errorf("unset: %s", out)
	}
	s.pin(t, "0.3.2", map[string]string{"dwrite.dll": "CSP 0.3.2", "extension/config/vr_tweaks.ini": "[B]\r\n"})
	s.mustRun(t, "DIREWOLF_AC_CSP=0.3.2")
	calls := s.callLog(t)
	if strings.Count(calls, "\n") != 1 || !strings.HasPrefix(calls, "curl ") {
		t.Errorf("want one CSP download and nothing else:\n%s", calls)
	}
	if got := readString(t, filepath.Join(s.game, "dwrite.dll")); got != "CSP 0.3.2" {
		t.Errorf("dwrite.dll = %q", got)
	}
	if got := readString(t, filepath.Join(s.game, "extension", "config", "vr_tweaks.ini")); got != "[B]\r\n" {
		t.Errorf("vr_tweaks.ini = %q", got)
	}
}

// A CSP the image doesn't pin, a download that doesn't match its pin, or an
// archive reaching outside the game folder installs nothing.
func TestACSetupRefusesUnpinnedOrTamperedCSP(t *testing.T) {
	s := newACSetup(t)
	if out, err := s.run(t, "DIREWOLF_AC_CSP=9.9.9"); err == nil || !strings.Contains(out, "is not a CSP version this image pins (0.3.1)") {
		t.Errorf("unpinned: err %v\n%s", err, out)
	}
	if calls := s.callLog(t); calls != "" {
		t.Errorf("an unpinned version ran:\n%s", calls)
	}
	s.pins["0.3.1"]["sha256"] = strings.Repeat("0", 64)
	b, err := json.Marshal(s.pins)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.dir, "ac-csp-versions.json"), string(b))
	if out, err := s.run(t); err == nil || !strings.Contains(out, "not the pinned") {
		t.Errorf("tampered: err %v\n%s", err, out)
	}
	s.pin(t, "0.3.1", map[string]string{"dwrite.dll": "x", "extension/../../evil.dll": "x"})
	if out, err := s.run(t); err == nil || !strings.Contains(out, "is not dwrite.dll and extension/") {
		t.Errorf("escaping archive: err %v\n%s", err, out)
	}
	for _, p := range []string{filepath.Join(s.game, "dwrite.dll"), filepath.Join(s.apps, "common", "evil.dll")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists: %v", p, err)
		}
	}
}

// Every link a game could plant in the shared home is refused, or replaced
// where a file is written, and nothing it points at is touched.
func TestACSetupRefusesPlantedLinks(t *testing.T) {
	type plant struct {
		path    func(s *acSetup) string
		target  string // "dir" or "file"
		refused bool
	}
	for name, p := range map[string]plant{
		"game folder":             {func(s *acSetup) string { return s.game }, "dir", true},
		"CSP extension folder":    {func(s *acSetup) string { return filepath.Join(s.game, "extension") }, "dir", true},
		"CSP extension subfolder": {func(s *acSetup) string { return filepath.Join(s.game, "extension", "config") }, "dir", true},
		"prefix Fonts":            {func(s *acSetup) string { return filepath.Join(s.compat, "pfx", "drive_c", "windows", "Fonts") }, "dir", true},
		"flat Documents":          {func(s *acSetup) string { return filepath.Dir(s.flatCfg("Assetto Corsa")) }, "dir+flat", true},
		"flat cfg":                {func(s *acSetup) string { return s.flatCfg("Assetto Corsa") }, "dir", true},
		"steamapps":               {func(s *acSetup) string { return s.apps }, "dir", true},
		"dwrite.dll":              {func(s *acSetup) string { return filepath.Join(s.game, "dwrite.dll") }, "file", false},
		"AssettoCorsa.exe":        {func(s *acSetup) string { return filepath.Join(s.game, "AssettoCorsa.exe") }, "file", false},
		"game marker":             {func(s *acSetup) string { return filepath.Join(s.game, "direwolf-ac-setup.json") }, "file", false},
		"prefix marker":           {func(s *acSetup) string { return filepath.Join(s.compat, "direwolf-ac-setup.json") }, "file", false},
		"flat video.ini":          {func(s *acSetup) string { return filepath.Join(s.flatCfg("Assetto Corsa"), "video.ini") }, "file", false},
		"segoeui.ttf": {func(s *acSetup) string {
			return filepath.Join(s.compat, "pfx", "drive_c", "windows", "Fonts", "segoeui.ttf")
		}, "file", false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newACSetup(t)
			// A prefix so that its folders can be planted before the run.
			s.madePrefix(t)
			path := p.path(s)
			outside := t.TempDir()
			target := outside
			if p.target == "file" {
				target = filepath.Join(outside, "victim")
				mustWrite(t, target, "victim")
			}
			if p.target == "dir+flat" {
				// Planted where the per-User link would be: the .flat
				// folder is then the flat config, and it is the link.
				path = filepath.Join(filepath.Dir(path), "Assetto Corsa.flat")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(filepath.Dir(path), "Assetto Corsa")); err != nil {
					t.Fatal(err)
				}
			}
			if p.target != "file" && name != "steamapps" && name != "game folder" {
				// The real folder's content moves aside, so only the link is new.
				_ = os.RemoveAll(path)
			}
			if name == "steamapps" || name == "game folder" {
				if err := os.Rename(path, filepath.Join(outside, "moved")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(path)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			out, err := s.run(t)
			if p.refused {
				if err == nil || !regexp.MustCompile(`is a link or not a folder; ending the session`).MatchString(out) {
					t.Errorf("err %v; want a refusal:\n%s", err, out)
				}
			} else {
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
					t.Errorf("%s is not a file now: %v %v", path, fi, err)
				}
				if got := readString(t, target); got != "victim" {
					t.Errorf("written through the link: %q", got)
				}
			}
			if p.target != "file" {
				if entries, _ := os.ReadDir(outside); len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != "moved") {
					t.Errorf("written through the link: %v", entries)
				}
			}
		})
	}
}

// Without Proton 9.0 installed no prefix is made; a pin to another tool only
// warns.
func TestACSetupNeedsProton9AndWarnsOnAnotherPin(t *testing.T) {
	s := newACSetup(t)
	mustWrite(t, filepath.Join(s.home, ".steam", "debian-installation", "config", "config.vdf"), vdfWithTool("proton_experimental"))
	out := s.mustRun(t)
	if !strings.Contains(out, "warning: Steam runs Assetto Corsa with proton_experimental, not proton_9") {
		t.Errorf("no pin warning:\n%s", out)
	}
	s = newACSetup(t)
	if out := s.mustRun(t); strings.Contains(out, "warning") {
		t.Errorf("warned on the proton_9 pin:\n%s", out)
	}
	s = newACSetup(t)
	if err := os.Remove(filepath.Join(s.apps, "appmanifest_2805730.acf")); err != nil {
		t.Fatal(err)
	}
	if out, err := s.run(t); err == nil || !strings.Contains(out, "Proton 9.0 is not installed") {
		t.Errorf("err %v\n%s", err, out)
	}
}

// Root's files in the shared home break the Library, and Wine refuses a
// prefix it doesn't own: the setup runs as PUID only.
func TestACSetupRefusesRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	s := newACSetup(t)
	if out, err := s.run(t, "DIREWOLF_AC_UID_MAP=0 0 4294967295"); err == nil || !strings.Contains(out, "run as PUID, not root") {
		t.Errorf("err %v\n%s", err, out)
	}
	if calls := s.callLog(t); calls != "" {
		t.Errorf("root ran:\n%s", calls)
	}
}

// The image's pins are https downloads with a sha256 each.
func TestACCSPVersionsArePinned(t *testing.T) {
	var pins map[string]struct{ URL, SHA256 string }
	if err := json.Unmarshal([]byte(readString(t, "ac-csp-versions.json")), &pins); err != nil {
		t.Fatal(err)
	}
	if len(pins) == 0 {
		t.Fatal("no CSP versions")
	}
	for v, p := range pins {
		if !strings.HasPrefix(p.URL, "https://") || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p.SHA256) {
			t.Errorf("%s: %+v", v, p)
		}
	}
}

// The deployed install was swapped by hand (XERK-2666), so no marker names
// its copy of acs.exe. An acs.exe update must not take that copy for Steam's
// launcher and drop the real one (QA, XERK-2726).
func TestACSetupKeepsLauncherOfHandSwappedInstall(t *testing.T) {
	s := newACSetup(t)
	mustWrite(t, filepath.Join(s.game, "AssettoCorsa.exe"), "ACS")
	mustWrite(t, filepath.Join(s.game, "AssettoCorsa_original.exe"), "LAUNCHER")
	s.mustRun(t)
	mustWrite(t, filepath.Join(s.game, "acs.exe"), "ACS 2")
	s.mustRun(t)
	for path, want := range map[string]string{
		filepath.Join(s.game, "AssettoCorsa.exe"):          "ACS 2",
		filepath.Join(s.game, "AssettoCorsa_original.exe"): "LAUNCHER",
	} {
		if got := readString(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// A prefix whose making was cut short (no version file, which Proton writes
// last) is made again.
func TestACSetupRemakesUnfinishedPrefix(t *testing.T) {
	s := newACSetup(t)
	mustWrite(t, filepath.Join(s.compat, "pfx", "system.reg"), "reg")
	s.mustRun(t)
	if calls := s.callLog(t); !strings.Contains(calls, "proton getcompatpath /") {
		t.Errorf("an unfinished prefix was kept:\n%s", calls)
	}
}

// Reads never follow a link either: a planted marker claiming every step
// done is ignored, and a link at acs.exe is no acs.exe.
func TestACSetupReadsNoPlantedLinks(t *testing.T) {
	s := newACSetup(t)
	s.madePrefix(t)
	done := filepath.Join(t.TempDir(), "done.json")
	mustWrite(t, done, `{"prefix": 1, "flat-cfg": 1}`)
	if err := os.Symlink(done, filepath.Join(s.compat, "direwolf-ac-setup.json")); err != nil {
		t.Fatal(err)
	}
	s.mustRun(t)
	if calls := s.callLog(t); !strings.Contains(calls, "winetricks") {
		t.Errorf("a planted marker was read:\n%s", calls)
	}
	s = newACSetup(t)
	acs := filepath.Join(t.TempDir(), "acs.exe")
	mustWrite(t, acs, "EVIL")
	if err := os.Remove(filepath.Join(s.game, "acs.exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(acs, filepath.Join(s.game, "acs.exe")); err != nil {
		t.Fatal(err)
	}
	if out, err := s.run(t); err == nil || !strings.Contains(out, "the game folder has no acs.exe") {
		t.Errorf("err %v\n%s", err, out)
	}
}

// A link inside CSP's archive is refused: unpacked, it would point anywhere.
func TestACSetupRefusesLinkInCSPArchive(t *testing.T) {
	s := newACSetup(t)
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, mode := range map[string]os.FileMode{"dwrite.dll": 0o644, "extension/evil": os.ModeSymlink | 0o777} {
		h := &zip.FileHeader{Name: name}
		h.SetMode(mode)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("/etc")); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.bin, "csp-link.zip")
	mustWrite(t, path, buf.String())
	sum := sha256.Sum256(buf.Bytes())
	s.pins["0.3.1"] = map[string]string{"url": path, "sha256": hex.EncodeToString(sum[:])}
	b, err := json.Marshal(s.pins)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(s.dir, "ac-csp-versions.json"), string(b))
	if out, err := s.run(t); err == nil || !strings.Contains(out, "is not dwrite.dll and extension/") {
		t.Errorf("err %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(s.game, "extension", "evil")); !os.IsNotExist(err) {
		t.Errorf("the archive's link was unpacked: %v", err)
	}
}

// Two VR launches at once: the second waits for the first's lock, saying so,
// before touching anything.
func TestACSetupWaitsForAnotherRun(t *testing.T) {
	s := newACSetup(t)
	if err := os.MkdirAll(s.compat, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(s.compat, "direwolf-ac-setup.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := s.run(t)
		done <- result{out, err}
	}()
	time.Sleep(time.Second)
	if calls := s.callLog(t); calls != "" {
		t.Errorf("ran beside another run:\n%s", calls)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || !strings.Contains(r.out, "waiting for another session's ac-setup") {
		t.Errorf("err %v\n%s", r.err, r.out)
	}
}

const recenterIni = "[__CM_RESET_CAMERA_VR]\r\nXBOXBUTTON=RTHUMB_PRESS\r\n"

// VR recenter goes on the pad's right-stick click in the flat cfg and in the
// User's seeded VR cfg, every launch, keeping what the User bound and every
// other line; a button another control holds is left alone.
func TestACSetupBindsVRRecenterToPad(t *testing.T) {
	s := newACSetup(t)
	vr := filepath.Join(s.state, "cfg", "controls.ini")
	mustWrite(t, vr, "[HEADER]\r\nINPUT_METHOD=X360\r\n[GEARUP]\r\nXBOXBUTTON=A\r\n")
	s.mustRun(t)
	if got, want := readString(t, vr), "[HEADER]\r\nINPUT_METHOD=X360\r\n[GEARUP]\r\nXBOXBUTTON=A\r\n"+recenterIni; got != want {
		t.Errorf("VR controls.ini = %q, want %q", got, want)
	}
	// The User's own binding (or an unbound one) is theirs.
	for _, own := range []string{"XBOXBUTTON=Y", "XBOXBUTTON="} {
		body := "[__CM_RESET_CAMERA_VR]\r\n" + own + "\r\nJOY=0\r\n"
		mustWrite(t, vr, body)
		if out := s.mustRun(t); strings.Contains(out, "recenter") {
			t.Errorf("%s: %s", own, out)
		}
		if got := readString(t, vr); got != body {
			t.Errorf("%s: VR controls.ini = %q", own, got)
		}
	}
	// A second copy of the section keeps the User's binding too.
	twice := "[__CM_RESET_CAMERA_VR]\r\nJOY=0\r\n[__CM_RESET_CAMERA_VR]\r\nXBOXBUTTON=Y\r\n"
	mustWrite(t, vr, twice)
	s.mustRun(t)
	if got := readString(t, vr); got != twice {
		t.Errorf("twice: VR controls.ini = %q", got)
	}
	// A set-up file is not rewritten on later launches.
	mustWrite(t, vr, recenterIni)
	before, err := os.Stat(vr)
	if err != nil {
		t.Fatal(err)
	}
	s.mustRun(t)
	if after, err := os.Stat(vr); err != nil || !os.SameFile(before, after) {
		t.Errorf("a bound controls.ini was rewritten: %v", err)
	}
	taken := "[ACTION_CHANGE_CAMERA]\r\nXBOXBUTTON=rthumb_press ; camera\r\n"
	mustWrite(t, vr, taken)
	if out := s.mustRun(t); !strings.Contains(out, "[ACTION_CHANGE_CAMERA] has it") {
		t.Errorf("no log of the taken button:\n%s", out)
	}
	if got := readString(t, vr); got != taken {
		t.Errorf("a taken button was bound again: %q", got)
	}
	// No VR cfg yet (the App command seeds it after ac-setup): none is made.
	s = newACSetup(t)
	s.mustRun(t)
	if _, err := os.Lstat(filepath.Join(s.state, "cfg")); !os.IsNotExist(err) {
		t.Errorf("ac-setup made the VR cfg, so the App command won't seed it: %v", err)
	}
	// A link at the VR cfg folder is skipped, never written through.
	s = newACSetup(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(s.state, "cfg")); err != nil {
		t.Fatal(err)
	}
	s.mustRun(t)
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("written through the link: %v", entries)
	}
	// A folder planted at controls.ini or at its temporary name, in the VR
	// or the flat cfg (before its first setup too), is logged and skipped,
	// not the end of every launch.
	for _, plant := range []func(s *acSetup) string{
		func(s *acSetup) string { return filepath.Join(s.state, "cfg", "controls.ini") },
		func(s *acSetup) string { return filepath.Join(s.state, "cfg", ".controls.ini.direwolf-tmp") },
		func(s *acSetup) string { return filepath.Join(s.flatCfg("Assetto Corsa"), "controls.ini") },
	} {
		s = newACSetup(t)
		mustWrite(t, filepath.Join(plant(s), "x"), "planted")
		for range 2 {
			if out := s.mustRun(t); !strings.Contains(out, "Is a directory") {
				t.Errorf("%s: no log of the planted folder:\n%s", plant(s), out)
			}
		}
		// A failed flat step stays unrecorded, so the next launch retries it.
		flat := strings.HasPrefix(plant(s), s.compat)
		if m := readString(t, filepath.Join(s.compat, "direwolf-ac-setup.json")); strings.Contains(m, `"flat-cfg"`) == flat {
			t.Errorf("%s: compat marker %s", plant(s), m)
		}
	}
	// So is a cfg folder made unreadable or unwritable (a non-root run: root
	// ignores modes), and a link planted at the flat cfg once it is set up.
	t.Run("modes", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file modes")
		}
		for _, mode := range []os.FileMode{0, 0o300, 0o500} {
			ms := newACSetup(t)
			ms.mustRun(t)
			for _, d := range []string{filepath.Join(ms.state, "cfg"), ms.flatCfg("Assetto Corsa")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(d, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
			}
			for range 2 {
				if out := ms.mustRun(t); !strings.Contains(out, "not binding VR recenter") {
					t.Errorf("mode %o: no log:\n%s", mode, out)
				}
			}
		}
	})
	// The VR cfg is still bound when the flat one fails.
	s = newACSetup(t)
	s.mustRun(t)
	vr = filepath.Join(s.state, "cfg", "controls.ini")
	mustWrite(t, vr, "")
	elsewhere = t.TempDir()
	if err := os.RemoveAll(s.flatCfg("Assetto Corsa")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, s.flatCfg("Assetto Corsa")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if out := s.mustRun(t); !strings.Contains(out, "not binding VR recenter in the flat cfg") {
			t.Errorf("flat cfg link: no log:\n%s", out)
		}
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("written through the flat cfg link: %v", entries)
	}
	if got := readString(t, vr); got != recenterIni {
		t.Errorf("VR controls.ini after a flat failure = %q", got)
	}
	// So is a socket, which open() refuses (ENXIO) rather than reading as no
	// file. Bound from inside the folder: a socket path has a length limit.
	for name, cfg := range map[string]func(s *acSetup) string{
		"VR":   func(s *acSetup) string { return filepath.Join(s.state, "cfg") },
		"flat": func(s *acSetup) string { return s.flatCfg("Assetto Corsa") },
	} {
		t.Run("socket in "+name+" cfg", func(t *testing.T) {
			s := newACSetup(t)
			if err := os.MkdirAll(cfg(s), 0o755); err != nil {
				t.Fatal(err)
			}
			// Restored when the subtest ends.
			t.Chdir(cfg(s))
			l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", "controls.ini")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			for range 2 {
				if out := s.mustRun(t); !strings.Contains(out, "No such device or address") {
					t.Errorf("no log of the planted socket:\n%s", out)
				}
			}
		})
	}
}

// A CSP bump removes the files the old archive installed and the new one
// lacks, and nothing else: not files others put in extension/, nor anything a
// planted marker names outside what a CSP archive may hold.
func TestACSetupCSPBumpRemovesDroppedFiles(t *testing.T) {
	s := newACSetup(t)
	s.pin(t, "0.3.1", map[string]string{
		"dwrite.dll": "CSP 0.3.1", "extension/config/vr_tweaks.ini": "[A]\r\n",
		"extension/config/old.ini": "old", "extension/old/gone.lua": "gone",
	})
	s.mustRun(t)
	mine := filepath.Join(s.game, "extension", "lua", "apps", "mine.lua")
	mustWrite(t, mine, "the User's app")
	mustWrite(t, filepath.Join(s.game, "keep.txt"), "not CSP's")
	marker := filepath.Join(s.game, "direwolf-ac-setup.json")
	var m map[string]any
	if err := json.Unmarshal([]byte(readString(t, marker)), &m); err != nil {
		t.Fatal(err)
	}
	// Planted: names outside CSP's, a folder, and names no file can have.
	mustWrite(t, filepath.Join(s.game, "extension", "mine", "x"), "the User's")
	files, ok := m["csp-files"].([]any)
	if !ok {
		t.Fatalf("csp-files = %v", m["csp-files"])
	}
	m["csp-files"] = append(files, "keep.txt", "../acs.exe", "extension/../acs.exe", 7,
		"extension/mine", "extension/a\x00b")
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// A lone surrogate, which Go's encoder would replace.
	mustWrite(t, marker, strings.Replace(string(b), `"keep.txt"`, `"extension/\ud800","keep.txt"`, 1))
	s.pin(t, "0.3.2", map[string]string{"dwrite.dll": "CSP 0.3.2", "extension/config/vr_tweaks.ini": "[B]\r\n"})
	s.mustRun(t, "DIREWOLF_AC_CSP=0.3.2")
	for _, gone := range []string{"extension/config/old.ini", "extension/old/gone.lua"} {
		if _, err := os.Lstat(filepath.Join(s.game, gone)); !os.IsNotExist(err) {
			t.Errorf("%s stayed: %v", gone, err)
		}
	}
	for path, want := range map[string]string{
		mine: "the User's app",
		filepath.Join(s.game, "extension", "mine", "x"): "the User's",
		filepath.Join(s.game, "keep.txt"):               "not CSP's",
		filepath.Join(s.apps, "common", "acs.exe"):      "",
		filepath.Join(s.game, "acs.exe"):                "ACS",
		filepath.Join(s.game, "dwrite.dll"):             "CSP 0.3.2",
	} {
		if want == "" {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Errorf("%s exists: %v", path, err)
			}
			continue
		}
		if got := readString(t, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if err := json.Unmarshal([]byte(readString(t, marker)), &m); err != nil {
		t.Fatal(err)
	}
	if got, ok := m["csp-files"].([]any); !ok || len(got) != 2 {
		t.Errorf("csp-files = %v, want the new archive's two files", m["csp-files"])
	}
}
