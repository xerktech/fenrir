package library

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const initDesktopMenu = "root/etc/s6-overlay/s6-rc.d/init-desktop-menu/run"

// runInitDesktopMenu runs the script with a fake /defaults and returns the
// home's labwc config dir.
func runInitDesktopMenu(t *testing.T, seed func(conf string)) string {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("as root the script re-execs via s6-setuidgid; run as a non-root user (CI is)")
	}
	defaults, home := t.TempDir(), t.TempDir()
	for name, menu := range map[string]string{"menu_wayland.xml": "wayland menu with Battle.net", "menu.xml": "x11 menu"} {
		if err := os.WriteFile(filepath.Join(defaults, name), []byte(menu), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(home, ".config", "labwc")
	if seed != nil {
		if err := os.MkdirAll(conf, 0o755); err != nil {
			t.Fatal(err)
		}
		seed(conf)
	}
	cmd := exec.CommandContext(t.Context(), "bash", initDesktopMenu)
	cmd.Env = append(os.Environ(), "HOME="+home, "DESKTOP_MENU_DEFAULTS="+defaults, "PIXELFLUX_WAYLAND=true")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init-desktop-menu must never fail (a failed oneshot hangs the pod): %v\n%s", err, out)
	}
	return conf
}

// init-selkies-config restores menu.xml from menu.xml.bak at every start, so
// both must be the image's menu, or new entries never show.
func TestInitDesktopMenuRefreshesAStaleMenu(t *testing.T) {
	conf := runInitDesktopMenu(t, func(conf string) {
		for _, f := range []string{"menu.xml", "menu.xml.bak"} {
			if err := os.WriteFile(filepath.Join(conf, f), []byte("menu from the first start"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, f := range []string{"menu.xml", "menu.xml.bak"} {
		if b, err := os.ReadFile(filepath.Join(conf, f)); err != nil || string(b) != "wayland menu with Battle.net" {
			t.Errorf("%s = %q (%v), want the image's wayland menu", f, b, err)
		}
	}
}

// On a first start init-selkies-config copies the menu and makes the backup.
func TestInitDesktopMenuLeavesAFreshHomeAlone(t *testing.T) {
	conf := runInitDesktopMenu(t, nil)
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Errorf("created %s: %v", conf, err)
	}
}

func TestInitDesktopMenuIsOrderedBeforeSelkiesConfig(t *testing.T) {
	for _, p := range []string{
		"root/etc/s6-overlay/s6-rc.d/init-selkies-config/dependencies.d/init-desktop-menu",
		"root/etc/s6-overlay/s6-rc.d/init-desktop-menu/dependencies.d/init-adduser",
		"root/etc/s6-overlay/s6-rc.d/user/contents.d/init-desktop-menu",
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
