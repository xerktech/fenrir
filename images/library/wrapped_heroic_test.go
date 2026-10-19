package library

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const wrappedHeroic = "root/usr/bin/wrapped-heroic"

// runWrappedHeroic runs wrapped-heroic against home with Heroic stubbed, and
// returns its output. Heroic must start whatever the config holds.
func runWrappedHeroic(t *testing.T, home string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	writeStub(t, filepath.Join(dir, "heroic"), `echo "heroic $*" >>"$LOG"`)
	writeStub(t, filepath.Join(dir, "bwrap"), `exit 1`)
	src, err := os.ReadFile(wrappedHeroic)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(src), "/opt/Heroic/heroic", filepath.Join(dir, "heroic"))
	if patched == string(src) {
		t.Fatal("wrapped-heroic no longer runs /opt/Heroic/heroic")
	}
	script := filepath.Join(dir, "wrapped-heroic")
	if err = os.WriteFile(script, []byte(patched), 0o600); err != nil { //nolint:gosec // script is under t.TempDir
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", script, "--arg")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME=", "LOG="+log,
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapped-heroic: %v\n%s", err, out)
	}
	if calls := readFile(t, log); !strings.Contains(calls, "heroic --no-sandbox --arg") {
		t.Errorf("Heroic not started as expected: %q", calls)
	}
	return string(out)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeHeroicConfig(t *testing.T, home, data string) string {
	t.Helper()
	p := filepath.Join(home, ".config", "heroic", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o640); err != nil { //nolint:gosec // p is under t.TempDir
		t.Fatal(err)
	}
	return p
}

func TestWrappedHeroicTurnsOffSteamShortcuts(t *testing.T) {
	home := t.TempDir()
	p := writeHeroicConfig(t, home, `{"defaultSettings":{"addSteamShortcuts":true,"defaultInstallPath":"/games"},"version":"v0"}`)
	runWrappedHeroic(t, home)
	var got struct {
		DefaultSettings struct {
			AddSteamShortcuts  *bool  `json:"addSteamShortcuts"`
			DefaultInstallPath string `json:"defaultInstallPath"`
		} `json:"defaultSettings"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(readFile(t, p)), &got); err != nil {
		t.Fatal(err)
	}
	if s := got.DefaultSettings; s.AddSteamShortcuts == nil || *s.AddSteamShortcuts || s.DefaultInstallPath != "/games" || got.Version != "v0" {
		t.Errorf("config.json:\n%s\nwant only addSteamShortcuts turned off", readFile(t, p))
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want the original 0640", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("config dir has %d entries, want only config.json (no temp file)", len(entries))
	}
}

func TestWrappedHeroicLeavesOtherConfigsAlone(t *testing.T) {
	// Fresh home: Heroic's own default is already off; create nothing.
	home := t.TempDir()
	runWrappedHeroic(t, home)
	if _, err := os.Stat(filepath.Join(home, ".config", "heroic")); !os.IsNotExist(err) {
		t.Errorf("fresh home gained a Heroic config dir: %v", err)
	}
	for name, cfg := range map[string]string{
		"already off": `{"defaultSettings":{"addSteamShortcuts":false}}`,
		"unparseable": `{"defaultSettings":`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			p := writeHeroicConfig(t, home, cfg)
			out := runWrappedHeroic(t, home)
			if got := readFile(t, p); got != cfg {
				t.Errorf("config.json changed to %q", got)
			}
			if name == "unparseable" && !strings.Contains(out, "could not turn off") {
				t.Errorf("no warning for an unparseable config: %q", out)
			}
		})
	}
}
