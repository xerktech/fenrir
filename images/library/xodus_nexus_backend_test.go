package library

import (
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const xodusNexusBackend = "root/usr/local/bin/xodus-nexus-backend"

// fakeCLI builds a real x86_64 ELF standing in for xodus-cli: --help lists the
// commands and flags Nexus's update requires but omit, and "run --help"
// prints runHelp.
func fakeCLI(t *testing.T, runHelp, omit string) []byte {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("the backend updater installs only x86_64 backends")
	}
	dir := t.TempDir()
	src := `package main

import (
	"fmt"
	"os"
)

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "run":
		fmt.Println(` + "`" + runHelp + "`" + `)
	case len(os.Args) > 1 && os.Args[1] == "streaming":
		if ` + "`" + omit + "`" + ` != "--selected-content-id" {
			fmt.Println("      --selected-content-id <ID>")
		}
	default:
		fmt.Println("Usage: xodus-cli [OPTIONS] <COMMAND>\n\nCommands:")
		for _, c := range []string{"account", "login", "logout", "download", "streaming", "resolve-package", "run"} {
			if c != ` + "`" + omit + "`" + ` {
				fmt.Printf("  %-16s  does %s\n", c, c)
			}
		}
		if ` + "`" + omit + "`" + ` != "--external-webview-stdio" {
			fmt.Println("\nOptions:\n      --external-webview-stdio")
		}
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cli")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "main.go")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GO111MODULE=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// backendRelease is one backend-v<version> release as Nexus publishes it.
type backendRelease struct {
	version, contract string
	files             map[string][]byte // package members; nil uses a valid set
	tarArgs           []string          // extra tar arguments (more members)
	manifest          func(map[string]any)
	sum               string // the .sha512sum's content; "" for the archive's own
	missing           string // an asset suffix left out of the release
}

// backendHost is a desktop home, an installed launcher and a fake GitHub.
type backendHost struct {
	t                        *testing.T
	dir, home, log, asar, gh string
	cli                      []byte
}

func newBackendHost(t *testing.T) *backendHost {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	gh := filepath.Join(dir, "gh")
	for _, d := range []string{bin, gh} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The release list for the API URL, else the file named after the URL's
	// last component, as GitHub serves release assets.
	writeStub(t, filepath.Join(bin, "curl"), `for a; do [ "$prev" = -o ] && out=$a; prev=$a; url=$a; done
case "$url" in https://api.github.com/*) echo "releases" >>"$LOG"; exec cat "$FAKE_GH/releases.json";; esac
echo "download ${url##*/}" >>"$LOG"
f="$FAKE_GH/${url##*/}"; [ -f "$f" ] || exit 22
if [ -n "$out" ]; then cp "$f" "$out"; else cat "$f"; fi`)
	h := &backendHost{t: t, dir: dir, home: filepath.Join(dir, "home"), log: filepath.Join(dir, "log"),
		asar: filepath.Join(dir, "app.asar"), gh: gh, cli: fakeCLI(t, "--materialize-chain", "")}
	h.launcher("0.2.13", "linux-run-v25")
	return h
}

// launcher writes an app.asar whose assets/backend-source.lock.json names
// version and contract, laid out as Electron's asar packs it.
func (h *backendHost) launcher(version, contract string) {
	t := h.t
	t.Helper()
	lock := []byte(`{"version":"` + version + `","compatibility":"` + contract + `"}`)
	header, err := json.Marshal(map[string]any{"files": map[string]any{
		"package.json": map[string]any{"size": 2, "offset": "0"},
		"assets": map[string]any{"files": map[string]any{
			"backend-source.lock.json": map[string]any{"size": len(lock), "offset": "2"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	padded := (len(header) + 3) &^ 3
	b := binary.LittleEndian.AppendUint32(nil, 4)
	for _, n := range []int{8 + padded, 4 + padded, len(header)} {
		b = binary.LittleEndian.AppendUint32(b, uint32(n)) //nolint:gosec // a test header of a few hundred bytes
	}
	b = append(b, header...)
	b = append(b, make([]byte, padded-len(header))...)
	b = append(b, "{}"...)
	b = append(b, lock...)
	if err = os.WriteFile(h.asar, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *backendHost) managed() string {
	return filepath.Join(h.home, ".config", "xodus-nexus", "xodus-backends")
}

// pointer writes current.json as an earlier Nexus left it, with an installed
// binary unless p names one.
func (h *backendHost) pointer(p map[string]string) {
	t := h.t
	t.Helper()
	if _, ok := p["binaryPath"]; !ok {
		cli := filepath.Join(h.dir, "installed-"+p["version"], "xodus-cli")
		if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
			t.Fatal(err)
		}
		writeStub(t, cli, "exit 0")
		p = maps.Clone(p)
		p["binaryPath"] = cli
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(h.managed(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(h.managed(), "current.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// publish serves the releases, newest last.
func (h *backendHost) publish(releases ...backendRelease) {
	t := h.t
	t.Helper()
	list := make([]map[string]any, 0, len(releases)+1)
	list = append(list, map[string]any{"tag_name": "v0.4.1", "assets": []map[string]any{
		{"name": "Xodus-Nexus-0.4.1-x86_64.AppImage", "browser_download_url": "https://github.com/domsvoboda/xodus-nexus/releases/download/v0.4.1/Xodus-Nexus-0.4.1-x86_64.AppImage"},
	}})
	for _, r := range releases {
		pkg := "xodus-nexus-backend-" + r.version + "-linux-x86_64"
		tag := "backend-v" + r.version
		files := r.files
		if files == nil {
			files = map[string][]byte{"xodus-cli": h.cli, "xodus-service": h.cli, "LICENSE": []byte("GPL-3.0\n")}
		}
		src := t.TempDir()
		if err := os.Mkdir(filepath.Join(src, pkg), 0o755); err != nil {
			t.Fatal(err)
		}
		hashes := map[string]string{}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(src, pkg, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			hashes[name] = sha256Hex(string(data))
		}
		archive := filepath.Join(t.TempDir(), pkg+".tar.xz")
		args := append([]string{"-cJf", archive, "-C", src, pkg}, r.tarArgs...)
		if b, err := exec.CommandContext(t.Context(), "tar", args...).CombinedOutput(); err != nil {
			t.Fatalf("tar: %v\n%s", err, b)
		}
		data, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		s := sha512.Sum512(data)
		sha := hex.EncodeToString(s[:])
		manifest := map[string]any{"schema": 1, "version": r.version, "commit": strings.Repeat("a", 40),
			"recipeSha256": strings.Repeat("b", 64), "compatibility": r.contract, "authUi": "electron",
			"platform": "linux", "arch": "x64", "archive": pkg + ".tar.xz", "archiveSha512": sha, "files": hashes}
		if r.manifest != nil {
			r.manifest(manifest)
		}
		m, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		sum := r.sum
		if sum == "" {
			sum = sha + "  " + pkg + ".tar.xz\n"
		}
		assets := map[string][]byte{".tar.xz": data, ".manifest.json": m, ".sha512sum": []byte(sum),
			"-source.tar.xz": []byte("source"), "-source.sha512sum": []byte("sum")}
		var listed []map[string]any
		for suffix, content := range assets {
			if suffix == r.missing {
				continue
			}
			if err = os.WriteFile(filepath.Join(h.gh, pkg+suffix), content, 0o600); err != nil {
				t.Fatal(err)
			}
			listed = append(listed, map[string]any{"name": pkg + suffix,
				"browser_download_url": "https://github.com/domsvoboda/xodus-nexus/releases/download/" + tag + "/" + pkg + suffix})
		}
		list = append(list, map[string]any{"tag_name": tag, "assets": listed})
	}
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(h.gh, "releases.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// run runs the real script and returns the stubs' calls and its output.
func (h *backendHost) run() string {
	t := h.t
	t.Helper()
	if err := os.WriteFile(h.log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", xodusNexusBackend)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(h.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+h.home, "LOG="+h.log, "FAKE_GH="+h.gh, "NEXUS_ASAR="+h.asar)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("xodus-nexus-backend must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	calls, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(calls) + string(out)
}

// current is the pointer Nexus reads, or nil with none.
func (h *backendHost) current() map[string]string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.managed(), "current.json"))
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

var oldBackend = map[string]string{"version": "0.2.11", "linuxRunPatch": "linux-run-v22", "delivery": "release"}

// An older release backend is replaced by the newest release the launcher
// accepts, installed and pointed at as Nexus's own update does; the next
// start downloads nothing.
func TestXodusNexusBackendUpdatesOldBackend(t *testing.T) {
	h := newBackendHost(t)
	h.pointer(oldBackend)
	h.publish(backendRelease{version: "0.2.12", contract: "linux-run-v25"},
		backendRelease{version: "0.2.13", contract: "linux-run-v25"},
		backendRelease{version: "0.2.14", contract: "linux-run-v25", missing: "-source.tar.xz"})
	calls := h.run()
	if !strings.Contains(calls, "updated the backend 0.2.11 -> 0.2.13") {
		t.Fatalf("calls:\n%s", calls)
	}
	p := h.current()
	pkg := "xodus-nexus-backend-0.2.13-linux-x86_64"
	matches, _ := filepath.Glob(filepath.Join(h.managed(), "releases", pkg+"-*"))
	if len(matches) != 1 {
		t.Fatalf("releases: %v", matches)
	}
	release := matches[0]
	if p["version"] != "0.2.13" || p["linuxRunPatch"] != "linux-run-v25" || p["delivery"] != "release" ||
		p["binaryPath"] != filepath.Join(release, "xodus-cli") || p["commit"] != strings.Repeat("a", 40) ||
		p["recipeSha256"] != strings.Repeat("b", 64) || !strings.HasSuffix(p["builtAt"], "Z") {
		t.Errorf("current.json %v", p)
	}
	if fi, err := os.Stat(filepath.Join(h.managed(), "current.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("current.json mode: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(release, "metadata.json")); err != nil {
		t.Errorf("metadata.json: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(release, "xodus-cli")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("xodus-cli mode: %v %v", fi, err)
	}
	if staging, _ := filepath.Glob(filepath.Join(h.managed(), ".download-*")); len(staging) != 0 {
		t.Errorf("left %q behind", staging)
	}

	calls = h.run()
	if strings.Contains(calls, "download") || !strings.Contains(calls, "backend 0.2.13 is up to date") {
		t.Errorf("want the installed backend left alone:\n%s", calls)
	}
}

// With no backend yet (first run), the newest release is installed too.
func TestXodusNexusBackendInstallsWhenMissing(t *testing.T) {
	h := newBackendHost(t)
	h.publish(backendRelease{version: "0.2.13", contract: "linux-run-v25"})
	h.run()
	if p := h.current(); p["version"] != "0.2.13" {
		t.Errorf("current.json %v", p)
	}
}

// Nothing is installed that the launcher would not play with, nothing older
// than the launcher accepts, and a locally built backend is kept.
func TestXodusNexusBackendKeepsBackend(t *testing.T) {
	for test, tc := range map[string]struct {
		pointer  map[string]string
		releases []backendRelease
		want     string
	}{
		"other launch contract": {oldBackend, []backendRelease{{version: "0.2.14", contract: "linux-run-v26"}}, "no backend release uses"},
		"below the launcher":    {oldBackend, []backendRelease{{version: "0.2.12", contract: "linux-run-v25"}}, "cannot find a backend release"},
		"locally built": {map[string]string{"version": "0.2.11", "delivery": "local"},
			[]backendRelease{{version: "0.2.13", contract: "linux-run-v25"}}, "locally built"},
		"already newer": {map[string]string{"version": "0.2.15", "delivery": "release"},
			[]backendRelease{{version: "0.2.13", contract: "linux-run-v25"}}, "up to date"},
	} {
		t.Run(test, func(t *testing.T) {
			h := newBackendHost(t)
			h.pointer(tc.pointer)
			h.publish(tc.releases...)
			if calls := h.run(); !strings.Contains(calls, tc.want) || strings.Contains(calls, ".tar.xz\n") {
				t.Errorf("want %q and no archive download:\n%s", tc.want, calls)
			}
			if p := h.current(); p["version"] != tc.pointer["version"] {
				t.Errorf("current.json changed: %v", p)
			}
		})
	}
}

// Anything that doesn't check out installs nothing and keeps the old backend.
func TestXodusNexusBackendRefusesUnverified(t *testing.T) {
	pkg := "xodus-nexus-backend-0.2.13-linux-x86_64"
	for test, tc := range map[string]struct {
		release func(h *backendHost) backendRelease
		want    string
	}{
		"sha512sum mismatch": {func(*backendHost) backendRelease {
			return backendRelease{sum: strings.Repeat("c", 128) + "  " + pkg + ".tar.xz\n"}
		}, "does not match its manifest"},
		"archive differs from manifest": {func(*backendHost) backendRelease {
			return backendRelease{manifest: func(m map[string]any) {
				m["archiveSha512"] = strings.Repeat("d", 128)
			}, sum: strings.Repeat("d", 128) + "  " + pkg + ".tar.xz\n"}
		}, "does not match its SHA-512"},
		"file differs from manifest": {func(*backendHost) backendRelease {
			return backendRelease{manifest: func(m map[string]any) {
				files, ok := m["files"].(map[string]string)
				if !ok {
					panic("manifest files")
				}
				files["LICENSE"] = strings.Repeat("e", 64)
			}}
		}, "failed verification"},
		"other architecture": {func(*backendHost) backendRelease {
			return backendRelease{manifest: func(m map[string]any) { m["arch"] = "arm64" }}
		}, "not a valid compatible release"},
		"extra member": {func(*backendHost) backendRelease {
			return backendRelease{tarArgs: []string{"--transform", "s,LICENSE$,../escape,"}}
		}, "unexpected layout"},
		"not an ELF": {func(h *backendHost) backendRelease {
			return backendRelease{files: map[string][]byte{"xodus-cli": []byte("#!/bin/sh\necho --materialize-chain\n" + strings.Repeat("#", 64)), "xodus-service": h.cli, "LICENSE": []byte("x")}}
		}, "failed verification"},
		"cannot launch games": {func(h *backendHost) backendRelease {
			cli := fakeCLI(h.t, "--exe", "")
			return backendRelease{files: map[string][]byte{"xodus-cli": cli, "xodus-service": cli, "LICENSE": []byte("x")}}
		}, "failed verification"},
		"no external sign-in": {func(h *backendHost) backendRelease {
			cli := fakeCLI(h.t, "--materialize-chain", "--external-webview-stdio")
			return backendRelease{files: map[string][]byte{"xodus-cli": cli, "xodus-service": cli, "LICENSE": []byte("x")}}
		}, "failed verification"},
		"no package selection": {func(h *backendHost) backendRelease {
			cli := fakeCLI(h.t, "--materialize-chain", "resolve-package")
			return backendRelease{files: map[string][]byte{"xodus-cli": cli, "xodus-service": cli, "LICENSE": []byte("x")}}
		}, "failed verification"},
		"no content selection": {func(h *backendHost) backendRelease {
			cli := fakeCLI(h.t, "--materialize-chain", "--selected-content-id")
			return backendRelease{files: map[string][]byte{"xodus-cli": cli, "xodus-service": cli, "LICENSE": []byte("x")}}
		}, "failed verification"},
	} {
		t.Run(test, func(t *testing.T) {
			h := newBackendHost(t)
			h.pointer(oldBackend)
			r := tc.release(h)
			r.version, r.contract = "0.2.13", "linux-run-v25"
			h.publish(r)
			if calls := h.run(); !strings.Contains(calls, tc.want) {
				t.Errorf("want %q:\n%s", tc.want, calls)
			}
			if p := h.current(); p["version"] != "0.2.11" {
				t.Errorf("current.json changed: %v", p)
			}
			if matches, _ := filepath.Glob(filepath.Join(h.managed(), "releases", pkg+"-*")); len(matches) != 0 {
				t.Errorf("release installed: %v", matches)
			}
			if staging, _ := filepath.Glob(filepath.Join(h.managed(), ".download-*")); len(staging) != 0 {
				t.Errorf("left %q behind", staging)
			}
		})
	}
}

// Without a readable launcher lock nothing is fetched.
func TestXodusNexusBackendNeedsLauncherLock(t *testing.T) {
	h := newBackendHost(t)
	h.pointer(oldBackend)
	if err := os.WriteFile(h.asar, []byte("not an asar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if calls := h.run(); strings.Contains(calls, "releases") || !strings.Contains(calls, "backend lock") {
		t.Errorf("calls:\n%s", calls)
	}
}

// A newer release on another launch contract (published before its launcher)
// is passed over for the newest one the launcher plays with.
func TestXodusNexusBackendFallsBackToLauncherContract(t *testing.T) {
	h := newBackendHost(t)
	h.pointer(oldBackend)
	h.publish(backendRelease{version: "0.2.13", contract: "linux-run-v25"},
		backendRelease{version: "0.2.14", contract: "linux-run-v26"})
	calls := h.run()
	if !strings.Contains(calls, "passing over backend 0.2.14") || !strings.Contains(calls, "0.2.11 -> 0.2.13") {
		t.Errorf("calls:\n%s", calls)
	}
	if p := h.current(); p["version"] != "0.2.13" || p["linuxRunPatch"] != "linux-run-v25" {
		t.Errorf("current.json %v", p)
	}
}

// A pointer at the latest version whose binary is gone is reinstalled, as
// Nexus itself would offer the update.
func TestXodusNexusBackendReinstallsMissingBinary(t *testing.T) {
	h := newBackendHost(t)
	h.pointer(map[string]string{"version": "0.2.13", "delivery": "release", "binaryPath": filepath.Join(h.dir, "gone", "xodus-cli")})
	h.publish(backendRelease{version: "0.2.13", contract: "linux-run-v25"})
	if calls := h.run(); !strings.Contains(calls, "updated the backend 0.2.13 -> 0.2.13") {
		t.Errorf("calls:\n%s", calls)
	}
	if p := h.current(); !strings.HasPrefix(p["binaryPath"], h.managed()) {
		t.Errorf("current.json %v", p)
	}
}

// A FIFO where the pointer belongs never blocks the Library start; it is
// replaced by a pointer to the newest backend.
func TestXodusNexusBackendIgnoresNonFilePointer(t *testing.T) {
	h := newBackendHost(t)
	if err := os.MkdirAll(h.managed(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(h.managed(), "current.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.publish(backendRelease{version: "0.2.13", contract: "linux-run-v25"})
	h.run()
	if p := h.current(); p["version"] != "0.2.13" {
		t.Errorf("current.json %v", p)
	}
}

// A release directory already present (from an earlier install) is used only
// if it still verifies.
func TestXodusNexusBackendVerifiesExistingRelease(t *testing.T) {
	h := newBackendHost(t)
	h.pointer(oldBackend)
	h.publish(backendRelease{version: "0.2.13", contract: "linux-run-v25"})
	h.run()
	release := filepath.Dir(h.current()["binaryPath"])
	if err := os.WriteFile(filepath.Join(release, "LICENSE"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pointer(oldBackend)
	if calls := h.run(); !strings.Contains(calls, "failed verification") {
		t.Errorf("calls:\n%s", calls)
	}
	if p := h.current(); p["version"] != "0.2.11" {
		t.Errorf("current.json changed: %v", p)
	}
}

// A planted link at any temporary pointer name is never written through.
func TestXodusNexusBackendPointerTempIsUnpredictable(t *testing.T) {
	h := newBackendHost(t)
	h.pointer(oldBackend)
	victim := filepath.Join(h.dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(h.managed(), "current.json.tmp")); err != nil {
		t.Fatal(err)
	}
	h.publish(backendRelease{version: "0.2.13", contract: "linux-run-v25"})
	h.run()
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim overwritten: %q", b)
	}
	if fi, err := os.Lstat(filepath.Join(h.managed(), "current.json")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("current.json: %v %v", fi, err)
	}
}
