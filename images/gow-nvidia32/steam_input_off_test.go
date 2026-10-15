// Package gownvidia32 holds tests for the Steam image's init scripts.
package gownvidia32

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const steamInputOff = "root/opt/direwolf/steam-input-off.py"

const libraryFolders = `"libraryfolders"
{
	"0"
	{
		"path"		"/home/retro/.steam/debian-installation"
		"apps"
		{
			"71250"		"1953701331"
			"213610"		"3253731096"
		}
	}
	"1"
	{
		"path"		"/games"
		"apps"
		{
			"1621690"		"998270361"
			"x\"y"		"1"
		}
	}
}
`

// localConfig is shaped like Steam's own: 213610 already has Steam Input
// off in both places, 71250 has it on, 1621690 has no entry at all.
const localConfig = `"UserLocalConfigStore"
{
	"Software"
	{
		"Valve"
		{
			"Steam"
			{
				"apps"
				{
					"213610"
					{
						"UseSteamControllerConfig"		"0"
						"LaunchOptions"		"bash -c 'echo \"$1\"' -- %command%"
					}
					"71250"
					{
						"LastPlayed"		"1792031310"
					}
				}
			}
		}
	}
	"apps"
	{
		"213610"
		{
			"UseSteamControllerConfig"		"0"
		}
		"71250"
		{
			"usesteamcontrollerconfig"		"1"
		}
	}
}
`

const wantConfig = `"UserLocalConfigStore"
{
	"Software"
	{
		"Valve"
		{
			"Steam"
			{
				"apps"
				{
					"213610"
					{
						"UseSteamControllerConfig"		"0"
						"LaunchOptions"		"bash -c 'echo \"$1\"' -- %command%"
					}
					"71250"
					{
						"LastPlayed"		"1792031310"
						"UseSteamControllerConfig"		"0"
					}
					"1621690"
					{
						"UseSteamControllerConfig"		"0"
					}
				}
			}
		}
	}
	"apps"
	{
		"213610"
		{
			"UseSteamControllerConfig"		"0"
		}
		"71250"
		{
			"usesteamcontrollerconfig"		"0"
		}
		"1621690"
		{
			"UseSteamControllerConfig"		"0"
		}
	}
}
`

// seed writes files (path under steam -> content) and returns the Steam dir.
func seed(t *testing.T, files map[string]string) string {
	t.Helper()
	steam := t.TempDir()
	for name, data := range files {
		p := filepath.Join(steam, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		// Not mkstemp's 0600: the replaced file must keep its own mode.
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return steam
}

func run(t *testing.T, steam string) (string, error) {
	t.Helper()
	b, err := exec.CommandContext(t.Context(), "python3", "-I", steamInputOff, steam).CombinedOutput()
	return string(b), err
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const userConfig = "userdata/29419923/config/localconfig.vdf"

func TestSteamInputOffDisablesEveryInstalledApp(t *testing.T) {
	steam := seed(t, map[string]string{
		"steamapps/libraryfolders.vdf": libraryFolders,
		userConfig:                     localConfig,
	})
	out, err := run(t, steam)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// 213610 was already off in both places.
	if !strings.Contains(out, "disabled for 2 apps") {
		t.Errorf("output %q, want the 2 changed apps counted", out)
	}
	if got := readFile(t, filepath.Join(steam, userConfig)); got != wantConfig {
		t.Errorf("localconfig.vdf:\n%s\nwant:\n%s", got, wantConfig)
	}
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(steam, userConfig)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("config dir has %d entries, want only localconfig.vdf (no temp file)", len(entries))
	}
	fi, err := os.Stat(filepath.Join(steam, userConfig))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want the original 0640", fi.Mode().Perm())
	}

	// A second run (the next session) changes nothing.
	if out, err := run(t, steam); err != nil || out != "" {
		t.Fatalf("rerun: %v %q", err, out)
	}
	if got := readFile(t, filepath.Join(steam, userConfig)); got != wantConfig {
		t.Errorf("rerun changed localconfig.vdf:\n%s", got)
	}
}

func TestSteamInputOffCreatesMissingSections(t *testing.T) {
	steam := seed(t, map[string]string{
		"steamapps/libraryfolders.vdf": libraryFolders,
		userConfig:                     "\"UserLocalConfigStore\"\n{\n\t\"friends\"\t\t\"x\"\n}\n",
	})
	if out, err := run(t, steam); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, filepath.Join(steam, userConfig))
	for _, want := range []string{
		"\t\"apps\"\n\t{\n\t\t\"1621690\"\n\t\t{\n\t\t\t\"UseSteamControllerConfig\"\t\t\"0\"\n\t\t}\n",
		"\t\t\t\"Steam\"\n\t\t\t{\n\t\t\t\t\"apps\"\n\t\t\t\t{\n\t\t\t\t\t\"1621690\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Only digit IDs become keys: libraryfolders.vdf is user-writable.
	if strings.Contains(got, `x\"y`) {
		t.Errorf("non-numeric app ID written:\n%s", got)
	}
}

func TestSteamInputOffLeavesUnparseableFileAlone(t *testing.T) {
	for name, cfg := range map[string]string{
		"truncated":    "\"UserLocalConfigStore\"\n{\n\t\"apps\"\n\t{\n",
		"unterminated": "\"UserLocalConfigStore\"\n{\n\t\"a\t\t\"b\n}\n",
		"no store":     "\"Other\"\n{\n}\n",
		"deep":         "\"UserLocalConfigStore\"\n" + strings.Repeat("{\"k\"\n", 100) + strings.Repeat("}", 100),
	} {
		t.Run(name, func(t *testing.T) {
			steam := seed(t, map[string]string{
				"steamapps/libraryfolders.vdf": libraryFolders,
				userConfig:                     cfg,
			})
			out, err := run(t, steam)
			if err == nil {
				t.Errorf("want a non-zero exit; output %q", out)
			}
			if strings.Contains(out, "Traceback") {
				t.Errorf("crashed:\n%s", out)
			}
			if got := readFile(t, filepath.Join(steam, userConfig)); got != cfg {
				t.Errorf("file changed:\n%s", got)
			}
		})
	}
}

func TestSteamInputOffWithoutSteamLogin(t *testing.T) {
	if out, err := run(t, t.TempDir()); err != nil || out != "" {
		t.Fatalf("fresh home: %v %q", err, out)
	}
	// Logged in, but no libraryfolders.vdf: report it, touch nothing.
	steam := seed(t, map[string]string{userConfig: localConfig})
	out, err := run(t, steam)
	if err == nil || strings.Contains(out, "Traceback") {
		t.Fatalf("want a clean non-zero exit: %v %q", err, out)
	}
	if got := readFile(t, filepath.Join(steam, userConfig)); got != localConfig {
		t.Errorf("file changed:\n%s", got)
	}
}

const hook = "root/etc/cont-init.d/20-steam-input-off.sh"

// sourceHook sources the cont-init hook the way GOW's entrypoint does (root,
// `set -e`), with gosu stubbed to run the command as is, and with the script
// at its image path swapped for the repo's. It reports whether the entrypoint
// would have carried on to start Steam.
func sourceHook(t *testing.T, home string) (string, bool) {
	t.Helper()
	bin := t.TempDir()
	stub := "#!/bin/sh\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gosu"), []byte(stub), 0o700); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	script, err := filepath.Abs(steamInputOff)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(hook)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(src), "/opt/direwolf/steam-input-off.py", script)
	if patched == string(src) {
		t.Fatal("hook no longer runs /opt/direwolf/steam-input-off.py")
	}
	h := filepath.Join(t.TempDir(), "hook.sh")
	if err = os.WriteFile(h, []byte(patched), 0o600); err != nil { //nolint:gosec // h is under t.TempDir
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		`set -e; gow_log() { echo "LOG: $*"; }; source "$1"; echo STARTED`, "entrypoint", h)
	cmd.Env = append(os.Environ(), "HOME="+home, "UNAME=retro", "PATH="+bin+":"+os.Getenv("PATH"))
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("entrypoint exited: %v", err)
	}
	return string(b), strings.Contains(string(b), "STARTED")
}

func TestSteamInputOffHookRunsScript(t *testing.T) {
	home := t.TempDir()
	steam := filepath.Join(home, ".steam", "steam")
	for name, data := range map[string]string{
		"steamapps/libraryfolders.vdf": libraryFolders,
		userConfig:                     localConfig,
	} {
		p := filepath.Join(steam, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, started := sourceHook(t, home)
	if !started {
		t.Fatalf("Steam would not start:\n%s", out)
	}
	if got := readFile(t, filepath.Join(steam, userConfig)); got != wantConfig {
		t.Errorf("hook did not update localconfig.vdf:\n%s", got)
	}
}

func TestSteamInputOffHookNeverStopsTheEntrypoint(t *testing.T) {
	// Fresh home: no Steam yet, nothing to do.
	if out, started := sourceHook(t, t.TempDir()); !started || strings.Contains(out, "LOG:") {
		t.Errorf("fresh home:\n%s", out)
	}
	// A broken Steam home fails the script; the entrypoint must carry on.
	home := t.TempDir()
	p := filepath.Join(home, ".steam", "steam", userConfig)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, started := sourceHook(t, home)
	if !started || !strings.Contains(out, "LOG: Could not turn Steam Input off") {
		t.Errorf("broken home:\n%s", out)
	}
}
