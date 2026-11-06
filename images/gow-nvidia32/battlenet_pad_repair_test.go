package gownvidia32

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const battlenetPadRepair = "root/opt/direwolf/battlenet-pad-repair.py"

const regHead = "WINE REGISTRY Version 2\n;; All keys relative to \\\\Machine\n\n#arch=win64\n\n"

// Keys shaped like Wine's, for the Wolf pad (045e:02ea) and a keyboard.
const (
	regKeyboard = "[System\\\\CurrentControlSet\\\\Enum\\\\HID\\\\VID_046D&PID_C31C\\\\1&2d595ca7&0&0000] 1730000000\n" +
		"#time=1db2f0c\n\"Driver\"=\"{745a17a0-74d3-11d0-b6fe-00a0c90f57da}\\\\0001\"\n\n"
	regPadParams = "[System\\\\CurrentControlSet\\\\Enum\\\\HID\\\\VID_045E&PID_02EA&XI_00\\\\1&2d595ca7&0&0000\\\\Device Parameters] 1730000000\n" +
		"#time=1db2f0c\n\n"
	regPadKey = "[System\\\\CurrentControlSet\\\\Enum\\\\HID\\\\VID_045E&PID_02EA&XI_00\\\\1&2d595ca7&0&0000] 1730000000\n" +
		"#time=1db2f0c\n\"ClassGUID\"=\"{745a17a0-74d3-11d0-b6fe-00a0c90f57da}\"\n" +
		"\"Driver\"=\"{745a17a0-74d3-11d0-b6fe-00a0c90f57da}\\\\0000\"\n" +
		"\"HardwareID\"=hex(7):48,00,49,00,44,00,5c,00,\\\n  00,00,00,00\n\n"
	regPadParams001 = "[System\\\\ControlSet001\\\\Enum\\\\HID\\\\VID_045E&PID_02EA&XI_01\\\\2&1\\\\Device Parameters] 1730000000\n" +
		"#time=1db2f0c\n\n"
	regPadClassLower = "[System\\\\CurrentControlSet\\\\Control\\\\DeviceClasses\\\\{4d1e55b2-f16f-11cf-88cb-001111000030}\\\\##?#hid#vid_045e&pid_02ea&xi_00#1&2d595ca7&0&0000#{4d1e55b2-f16f-11cf-88cb-001111000030}] 1730000000\n" +
		"#time=1db2f0c\n\"DeviceInstance\"=\"HID\\\\VID_045E&PID_02EA&XI_00\\\\1&2d595ca7&0&0000\"\n\n"
	regPadBus = "[System\\\\CurrentControlSet\\\\Enum\\\\WINEXINPUT\\\\VID_045E&PID_02EA&XI_00\\\\1&2d595ca7&0&0000] 1730000000\n" +
		"#time=1db2f0c\n\"Driver\"=\"{4d36e97d-e325-11ce-bfc1-08002be10318}\\\\0002\"\n\n"
	regPadWinebus = "[System\\\\ControlSet001\\\\Enum\\\\WINEBUS\\\\VID_045E&PID_02EA\\\\0&0&0] 1730000000\n" +
		"#time=1db2f0c\n\n"
	regPadClass = "[System\\\\CurrentControlSet\\\\Control\\\\DeviceClasses\\\\{6c53d5fd-6480-440f-b618-476750c5e1a6}\\\\##?#HID#VID_045E&PID_02EA&XI_00#1&2d595ca7&0&0000#{6c53d5fd-6480-440f-b618-476750c5e1a6}\\\\#] 1730000000\n" +
		"#time=1db2f0c\n\"SymbolicLink\"=\"\\\\\\\\?\\\\HID#VID_045E&PID_02EA&XI_00#1&2d595ca7&0&0000#{6c53d5fd-6480-440f-b618-476750c5e1a6}\"\n\n"
	// Another pad model, and a key whose name only contains the pad's IDs.
	regOtherPad  = "[System\\\\CurrentControlSet\\\\Enum\\\\HID\\\\VID_045E&PID_02EA0&XI_00\\\\x] 1730000000\n#time=1\n\n"
	regLookalike = "[Software\\\\Wine\\\\VID_045E&PID_02EA&XI_00\\\\x] 1730000000\n#time=1\n\n"
)

func runPadRepair(t *testing.T, reg string) (got, log string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "system.reg")
	if err := os.WriteFile(path, []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "python3", "-I", battlenetPadRepair, path).CombinedOutput()
	if err != nil {
		t.Fatalf("battlenet-pad-repair: %v\n%s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), string(out)
}

// A pad key left with only its Device Parameters subkey is dropped with every
// other key of the pad, on every bus and interface; nothing else changes.
func TestBattlenetPadRepairDropsHalfInstalledPad(t *testing.T) {
	keep := regKeyboard + regOtherPad + regLookalike
	for _, tc := range []struct {
		name, reg, want string
	}{
		{"only Device Parameters", regHead + regKeyboard + regPadParams + regPadBus + regPadWinebus + regPadClass + regOtherPad + regLookalike,
			regHead + keep},
		{"key with no Driver", regHead + regKeyboard + strings.Replace(regPadKey, "\"Driver\"", "\"NotDriver\"", 1) + regPadParams + regOtherPad + regLookalike,
			regHead + keep},
		{"ControlSet001, lowercase DeviceClasses", regHead + regKeyboard + regPadParams001 + regPadClassLower + regOtherPad,
			regHead + regKeyboard + regOtherPad},
		{"lowercase Enum", regHead + regKeyboard + strings.ToLower(regPadParams) + regOtherPad,
			regHead + regKeyboard + regOtherPad},
		{"healthy", regHead + regKeyboard + regPadKey + regPadParams + regPadBus + regPadClass,
			regHead + regKeyboard + regPadKey + regPadParams + regPadBus + regPadClass},
		{"no pad", regHead + regKeyboard, regHead + regKeyboard},
		{"other files", "garbage\n" + regPadParams, "garbage\n" + regPadParams},
	} {
		got, out := runPadRepair(t, tc.reg)
		if got != tc.want {
			t.Errorf("%s: got\n%s\nwant\n%s\nlog: %s", tc.name, got, tc.want, out)
		}
	}
}

// The original is kept beside it, and the file keeps its mode.
func TestBattlenetPadRepairKeepsABackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system.reg")
	reg := regHead + regPadParams
	if err := os.WriteFile(path, []byte(reg), 0o640); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "python3", "-I", battlenetPadRepair, path).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, err := os.ReadFile(path + ".direwolf-pad-repair.bak"); err != nil || string(b) != reg {
		t.Errorf("backup %q (%v), want the original", b, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v (%v), want 0640", st.Mode(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("left %d files, want system.reg and its backup", len(entries))
	}
}

// launch-battlenet repairs the prefix before it starts the client.
func TestLaunchBattlenetRepairsHalfInstalledPad(t *testing.T) {
	var reg string
	r := runLaunchBattlenetWith(t, true, func(games string) {
		reg = filepath.Join(filepath.Dir(games), ".local", "share", "battlenet", "pfx", "system.reg")
		if err := os.WriteFile(reg, []byte(regHead+regKeyboard+regPadParams), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}, []string{"wow"}, "GAME_SECONDS=3")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if got, err := os.ReadFile(reg); err != nil || string(got) != regHead+regKeyboard {
		t.Errorf("system.reg %q (%v), want the pad's keys gone\n%s", got, err, r.out)
	}
}

// A running wineserver of the user rewrites system.reg from memory, so the
// repair is skipped while one runs.
func TestLaunchBattlenetSkipsPadRepairBesideWineserver(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	// A process whose name is wineserver: a link to sleep.
	ws := filepath.Join(t.TempDir(), "wineserver")
	if err := os.Symlink(sleep, ws); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), ws, "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var reg string
	const broken = regHead + regKeyboard + regPadParams
	r := runLaunchBattlenetWith(t, true, func(games string) {
		reg = filepath.Join(filepath.Dir(games), ".local", "share", "battlenet", "pfx", "system.reg")
		if err := os.WriteFile(reg, []byte(broken), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}, []string{"wow"}, "GAME_SECONDS=3")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if got, err := os.ReadFile(reg); err != nil || string(got) != broken {
		t.Errorf("system.reg %q (%v), want it untouched\n%s", got, err, r.out)
	}
	if !strings.Contains(r.out, "pad registry check skipped") {
		t.Errorf("no skip logged\n%s", r.out)
	}
}

// A repair that fails is logged, and the game still launches.
func TestLaunchBattlenetLaunchesWhenPadRepairFails(t *testing.T) {
	r := runLaunchBattlenetWith(t, true, func(games string) {
		// A directory where system.reg should be: reading it fails.
		reg := filepath.Join(filepath.Dir(games), ".local", "share", "battlenet", "pfx", "system.reg")
		if err := os.Mkdir(reg, 0o755); err != nil {
			t.Fatal(err)
		}
	}, []string{"wow"}, "GAME_SECONDS=3")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "pad registry check failed") || !strings.Contains(r.out, "wow is running") {
		t.Errorf("want the failure logged and the game run\n%s", r.out)
	}
	if strings.Contains(r.out, "Traceback") {
		t.Errorf("logged a traceback\n%s", r.out)
	}
}

// A linked system.reg keeps its link; the target is repaired.
func TestBattlenetPadRepairFollowsALink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "real.reg"), filepath.Join(dir, "system.reg")
	if err := os.WriteFile(target, []byte(regHead+regPadParams), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.reg", link); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "python3", "-I", battlenetPadRepair, link).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("system.reg is no longer a link (%v)", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != regHead {
		t.Errorf("target %q (%v), want the pad's keys gone", b, err)
	}
}
