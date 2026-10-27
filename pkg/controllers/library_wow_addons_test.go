package controllers

import (
	"strings"
	"testing"
)

const consolePortAddon = `{"name":"ConsolePort","source":{"type":"github","repo":"seblindfors/ConsolePort","asset":"ConsolePort-*.zip"},"flavors":["retail"]}`

func TestParseWoWAddons(t *testing.T) {
	addons, err := ParseWoWAddons("[" + consolePortAddon + "]")
	if err != nil {
		t.Fatal(err)
	}
	if len(addons) != 1 || addons[0].Name != "ConsolePort" || addons[0].Source.Repo != "seblindfors/ConsolePort" ||
		addons[0].Source.Asset != "ConsolePort-*.zip" || len(addons[0].Flavors) != 1 || addons[0].Flavors[0] != "retail" {
		t.Errorf("ParseWoWAddons = %+v", addons)
	}
	// The pod gets back what the chart listed.
	if got := wowAddonsEnv(addons); got != "["+consolePortAddon+"]" {
		t.Errorf("wowAddonsEnv = %s", got)
	}
	for _, empty := range []string{"", "[]"} {
		if addons, err := ParseWoWAddons(empty); err != nil || len(addons) != 0 {
			t.Errorf("ParseWoWAddons(%q) = %v, %v; want none", empty, addons, err)
		}
	}
}

// A list misread would uninstall the addons it leaves out, so anything odd
// stops the operator instead.
func TestParseWoWAddonsRejects(t *testing.T) {
	for name, s := range map[string]string{
		"not json":         "ConsolePort",
		"not a list":       consolePortAddon,
		"trailing data":    "[" + consolePortAddon + "] []",
		"unknown field":    `[{"name":"A","source":{"type":"github","repo":"a/b","asset":"a.zip"},"flavors":["retail"],"enabled":false}]`,
		"duplicate":        "[" + consolePortAddon + "," + consolePortAddon + "]",
		"path name":        strings.Replace(consolePortAddon, `"name":"ConsolePort"`, `"name":"../ConsolePort"`, 1),
		"dot name":         strings.Replace(consolePortAddon, `"name":"ConsolePort"`, `"name":".."`, 1),
		"empty name":       strings.Replace(consolePortAddon, `"name":"ConsolePort"`, `"name":""`, 1),
		"curseforge":       strings.Replace(consolePortAddon, `"type":"github"`, `"type":"curseforge"`, 1),
		"url repo":         strings.Replace(consolePortAddon, `seblindfors/ConsolePort`, `https://evil.example/x`, 1),
		"dot repo":         strings.Replace(consolePortAddon, `seblindfors/ConsolePort`, `seblindfors/..`, 1),
		"deep repo":        strings.Replace(consolePortAddon, `seblindfors/ConsolePort`, `a/b/c`, 1),
		"slash in asset":   strings.Replace(consolePortAddon, `ConsolePort-*.zip`, `../x.zip`, 1),
		"no asset":         strings.Replace(consolePortAddon, `ConsolePort-*.zip`, ``, 1),
		"no flavors":       strings.Replace(consolePortAddon, `["retail"]`, `[]`, 1),
		"unknown flavor":   strings.Replace(consolePortAddon, `["retail"]`, `["_retail_"]`, 1),
		"traversal flavor": strings.Replace(consolePortAddon, `["retail"]`, `["../retail"]`, 1),
	} {
		if addons, err := ParseWoWAddons(s); err == nil {
			t.Errorf("%s: ParseWoWAddons(%s) = %+v, want an error", name, s, addons)
		}
	}
}
