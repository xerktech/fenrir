// Package library holds tests for the Library image's shell scripts.
package library

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const initSteamHome = "root/etc/s6-overlay/s6-rc.d/init-steam-home/run"

const (
	libSteam = ".local/share/Steam"
	debSteam = ".steam/debian-installation"
)

func manifest(state, dir string) string {
	return "\"AppState\"\n{\n\t\"StateFlags\"\t\t\"" + state + "\"\n\t\"installdir\"\t\t\"" + dir + "\"\n}\n"
}

// runInit runs the image's init-steam-home with HOME set to a fresh home
// seeded with files (path -> content; a trailing / makes a directory), and
// returns the home and the script's output.
func runInit(t *testing.T, files map[string]string, setup func(home string)) (string, string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("as root the script re-execs via s6-setuidgid; run as a non-root user (CI is)")
	}
	home := t.TempDir()
	for name, data := range files {
		p := filepath.Join(home, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if setup != nil {
		setup(home)
	}
	cmd := exec.Command("bash", initSteamHome)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init-steam-home must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	return home, string(out)
}

func assertLinked(t *testing.T, home string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(home, libSteam))
	if err != nil || got != "../../.steam/debian-installation" {
		t.Fatalf("%s -> %q (%v), want the relative link to the sessions' install", libSteam, got, err)
	}
	if fi, err := os.Stat(filepath.Join(home, libSteam)); err != nil || !fi.IsDir() {
		t.Fatalf("%s does not resolve to a directory: %v", libSteam, err)
	}
}

func exists(home, p string) bool {
	_, err := os.Lstat(filepath.Join(home, p))
	return err == nil
}

func asideDir(t *testing.T, home string) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(home, libSteam+".library-*"))
	if len(m) != 1 {
		t.Fatalf("want one aside dir, got %v", m)
	}
	rel, _ := filepath.Rel(home, m[0])
	return rel
}

func TestInitSteamHomeFresh(t *testing.T) {
	home, _ := runInit(t, nil, nil)
	assertLinked(t, home)
}

func TestInitSteamHomeLibraryOnlyBecomesShared(t *testing.T) {
	home, _ := runInit(t, map[string]string{libSteam + "/steam.sh": "x"}, nil)
	assertLinked(t, home)
	if !exists(home, debSteam+"/steam.sh") {
		t.Fatal("the Library's install was not moved to the shared path")
	}
	if m, _ := filepath.Glob(filepath.Join(home, libSteam+".library-*")); len(m) != 0 {
		t.Fatalf("nothing should be moved aside, got %v", m)
	}
}

func TestInitSteamHomeIdempotent(t *testing.T) {
	home, _ := runInit(t, map[string]string{debSteam + "/steam.sh": "x"}, nil)
	assertLinked(t, home)
	cmd := exec.Command("bash", initSteamHome)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("second run: %v %q", err, out)
	}
	assertLinked(t, home)
}

// The live home (XERK-1559): the Library finished games the sessions only
// started downloading. They must stay installed, or the catalogue deletes
// their Apps.
func TestInitSteamHomeBothMergesInstalledGames(t *testing.T) {
	home, _ := runInit(t, map[string]string{
		// Installed on both: the sessions' copy stays.
		libSteam + "/steamapps/appmanifest_10.acf": manifest("4", "Both"),
		libSteam + "/steamapps/common/Both/lib":    "library",
		debSteam + "/steamapps/appmanifest_10.acf": manifest("4", "Both"),
		debSteam + "/steamapps/common/Both/ses":    "session",
		// Installed only in the Library; the session has a partial download.
		libSteam + "/steamapps/appmanifest_20.acf":            manifest("4", "Game Two"),
		libSteam + "/steamapps/common/Game Two/bin":           "full",
		libSteam + "/steamapps/compatdata/20/pfx":             "prefix",
		libSteam + "/steamapps/shadercache/20/c":              "lib cache",
		debSteam + "/steamapps/appmanifest_20.acf":            manifest("1026", "Game Two"),
		debSteam + "/steamapps/common/Game Two/":              "",
		debSteam + "/steamapps/shadercache/20/c":              "ses cache",
		debSteam + "/steamapps/downloading/20/part":           "partial",
		debSteam + "/steamapps/downloading/state_20_21.patch": "",
		// Only in the Library, not in the session at all.
		libSteam + "/steamapps/appmanifest_30.acf": manifest("4", "Three"),
		libSteam + "/steamapps/common/Three/bin":   "full",
		// Not fully installed in the Library: left aside.
		libSteam + "/steamapps/appmanifest_40.acf": manifest("1026", "Four"),
		libSteam + "/steamapps/common/Four/":       "",
		// A user-writable manifest naming a path outside common/.
		libSteam + "/steamapps/appmanifest_50.acf": manifest("4", ".."),
		libSteam + "/steamapps/appmanifest_x.acf":  manifest("4", "Three"),
		libSteam + "/config/loginusers.vdf":        "login",
	}, nil)
	assertLinked(t, home)
	aside := asideDir(t, home)
	d := debSteam + "/steamapps/"

	for _, p := range []string{
		d + "common/Both/ses", d + "common/Game Two/bin", d + "compatdata/20/pfx",
		d + "common/Three/bin", d + "appmanifest_30.acf",
		aside + "/steamapps/common/Both/lib", aside + "/config/loginusers.vdf",
		aside + "/steamapps/appmanifest_40.acf", aside + "/steamapps/appmanifest_50.acf",
		aside + "/.replaced/common/Game Two", aside + "/.replaced/shadercache/20/c",
		aside + "/.replaced/downloading/20/part", aside + "/.replaced/downloading/state_20_21.patch",
	} {
		if !exists(home, p) {
			t.Errorf("missing %s", p)
		}
	}
	for _, p := range []string{d + "appmanifest_40.acf", d + "appmanifest_50.acf", d + "downloading/20"} {
		if exists(home, p) {
			t.Errorf("unexpected %s", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, d+"appmanifest_20.acf")); !strings.Contains(string(b), `"4"`) {
		t.Errorf("appmanifest_20 not the installed one: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(home, d+"shadercache/20/c")); string(b) != "lib cache" {
		t.Errorf("shadercache/20 = %q, want the Library's", b)
	}
}

func TestInitSteamHomeReplacesOtherLink(t *testing.T) {
	home, out := runInit(t, map[string]string{".local/share/": ""}, func(home string) {
		if err := os.Symlink("/nowhere", filepath.Join(home, libSteam)); err != nil {
			t.Fatal(err)
		}
	})
	assertLinked(t, home)
	if !strings.Contains(out, "replacing link") {
		t.Errorf("replacing a foreign link must be logged, got %q", out)
	}
}

// Anything it can't handle leaves the home alone and exits 0 (runInit
// fails the test on a non-zero exit).
func TestInitSteamHomeGivesUpQuietly(t *testing.T) {
	t.Run("sessions' install is a link", func(t *testing.T) {
		home, _ := runInit(t, map[string]string{libSteam + "/steam.sh": "x", ".steam/": ""}, func(home string) {
			if err := os.Symlink("../.local/share/Steam", filepath.Join(home, debSteam)); err != nil {
				t.Fatal(err)
			}
		})
		if fi, err := os.Lstat(filepath.Join(home, libSteam)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s should be left a real dir: %v", libSteam, err)
		}
	})
	t.Run("sessions' install is a file", func(t *testing.T) {
		home, _ := runInit(t, map[string]string{debSteam: "x"}, nil)
		if exists(home, libSteam) {
			t.Fatal("nothing should be linked")
		}
	})
	t.Run("unwritable home", func(t *testing.T) {
		home, _ := runInit(t, map[string]string{".local/share/": ""}, func(home string) {
			if err := os.Chmod(filepath.Join(home, ".local/share"), 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, ".local/share"), 0o755) })
		})
		if exists(home, libSteam) {
			t.Fatal("nothing should be linked")
		}
	})
}
