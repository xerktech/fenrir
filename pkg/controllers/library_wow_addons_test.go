package controllers

import (
	"strings"
	"testing"
)

const consolePortAddon = `{"name":"ConsolePort","source":{"type":"github","repo":"seblindfors/ConsolePort","asset":"ConsolePort-*.zip"},"flavors":["retail"]}`

const tomTomAddon = `{"name":"TomTom","source":{"type":"url","url":"https://mediafilez.forgecdn.net/files/7012/345/TomTom-v4.0.10-release.zip","sha256":"` +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `"},"flavors":["retail"]}`

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
	addons, err = ParseWoWAddons("[" + consolePortAddon + "," + tomTomAddon + "]")
	if err != nil {
		t.Fatal(err)
	}
	if src := addons[1].Source; src.Type != "url" || src.URL != "https://mediafilez.forgecdn.net/files/7012/345/TomTom-v4.0.10-release.zip" ||
		src.SHA256 != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("url source = %+v", src)
	}
	// Each source type gets only its own fields back.
	if got := wowAddonsEnv(addons); got != "["+consolePortAddon+","+tomTomAddon+"]" {
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
		"github with url":  strings.Replace(consolePortAddon, `"asset":`, `"url":"https://a.example/a.zip","asset":`, 1),
		"github with sha":  strings.Replace(consolePortAddon, `"asset":`, `"sha256":"`+strings.Repeat("0", 64)+`","asset":`, 1),
		"url with repo":    strings.Replace(tomTomAddon, `"url":`, `"repo":"a/b","url":`, 1),
		"url with asset":   strings.Replace(tomTomAddon, `"url":`, `"asset":"a.zip","url":`, 1),
		"url no sha":       strings.Replace(tomTomAddon, `"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`, `"sha256":""`, 1),
		"url short sha":    strings.Replace(tomTomAddon, `"sha256":"0123`, `"sha256":"`, 1),
		"url upper sha":    strings.Replace(tomTomAddon, `abcdef"`, `ABCDEF"`, 1),
		"url no url":       strings.Replace(tomTomAddon, `https://mediafilez.forgecdn.net/files/7012/345/TomTom-v4.0.10-release.zip`, ``, 1),
		"url http":         strings.Replace(tomTomAddon, `https://`, `http://`, 1),
		"url file":         strings.Replace(tomTomAddon, `https://mediafilez.forgecdn.net`, `file:///etc`, 1),
		"url no path":      strings.Replace(tomTomAddon, `/files/7012/345/TomTom-v4.0.10-release.zip`, ``, 1),
		"url credentials":  strings.Replace(tomTomAddon, `https://`, `https://user:pw@`, 1),
		"url query":        strings.Replace(tomTomAddon, `.zip"`, `.zip?x=1"`, 1),
		"url curl glob":    strings.Replace(tomTomAddon, `/345/`, `/{345,346}/`, 1),
		"url space":        strings.Replace(tomTomAddon, `TomTom-v4`, `TomTom v4`, 1),
	} {
		// Every case but the first three is one entry of a list.
		if !strings.HasPrefix(s, "[") && name != "not json" && name != "not a list" {
			s = "[" + s + "]"
		}
		if addons, err := ParseWoWAddons(s); err == nil {
			t.Errorf("%s: ParseWoWAddons(%s) = %+v, want an error", name, s, addons)
		}
	}
}
