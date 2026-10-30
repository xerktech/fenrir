package controllers

import (
	"strings"
	"testing"
)

const consolePortAddon = `{"name":"ConsolePort","source":{"type":"github","repo":"seblindfors/ConsolePort","asset":"ConsolePort-*.zip"},"flavors":["retail"]}`

// paw is a url source: Pawn's CurseForge CDN file, pinned by its SHA256.
const (
	pawURL = "https://mediafilez.forgecdn.net/files/9283/553/Pawn-2.14.0.zip"
	pawSHA = "16ff6a4351c6f34273c2e36dbfc2dd40e325c702dd632a058386041a517eafcd"
	paw    = `{"name":"Pawn","source":{"type":"url","url":"` + pawURL + `","sha256":"` + pawSHA + `"},"flavors":["retail"]}`
)

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
	// A url source, pinned by SHA256, round-trips with no github fields.
	addons, err = ParseWoWAddons("[" + paw + "]")
	if err != nil {
		t.Fatal(err)
	}
	if s := addons[0].Source; s.Type != "url" || s.URL != pawURL || s.SHA256 != pawSHA || s.Repo != "" || s.Asset != "" {
		t.Errorf("ParseWoWAddons = %+v", addons)
	}
	if got := wowAddonsEnv(addons); got != "["+paw+"]" {
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
		"github with url":  strings.Replace(consolePortAddon, `"asset":"ConsolePort-*.zip"`, `"asset":"ConsolePort-*.zip","url":"`+pawURL+`"`, 1),
		"github with sha":  strings.Replace(consolePortAddon, `"asset":"ConsolePort-*.zip"`, `"asset":"ConsolePort-*.zip","sha256":"`+pawSHA+`"`, 1),
		"url with repo":    strings.Replace(paw, `"type":"url"`, `"type":"url","repo":"a/b"`, 1),
		"url with asset":   strings.Replace(paw, `"type":"url"`, `"type":"url","asset":"a.zip"`, 1),
		"no url":           strings.Replace(paw, `"url":"`+pawURL+`",`, ``, 1),
		"http url":         strings.Replace(paw, `https://`, `http://`, 1),
		"file url":         strings.Replace(paw, pawURL, `file:///etc/passwd`, 1),
		"credentials":      strings.Replace(paw, `https://`, `https://u:p@`, 1),
		"no host":          strings.Replace(paw, pawURL, `https:///files/Pawn.zip`, 1),
		"space in url":     strings.Replace(paw, `Pawn-2.14.0.zip`, `Pawn 2.14.0.zip`, 1),
		"quote in url":     strings.Replace(paw, `Pawn-2.14.0.zip`, `Pawn'$(id)'.zip`, 1),
		"fragment":         strings.Replace(paw, `Pawn-2.14.0.zip`, `Pawn-2.14.0.zip#x`, 1),
		"no sha256":        strings.Replace(paw, `,"sha256":"`+pawSHA+`"`, ``, 1),
		"short sha256":     strings.Replace(paw, pawSHA, pawSHA[:63], 1),
		"upper sha256":     strings.Replace(paw, pawSHA, strings.ToUpper(pawSHA), 1),
		"sha256 prefix":    strings.Replace(paw, pawSHA, "sha256:"+pawSHA, 1),
		"long url":         strings.Replace(paw, "/files/", "/"+strings.Repeat("a", 2048)+"/", 1),
		"port too big":     strings.Replace(paw, ".net/", ".net:99999/", 1),
		"port zero":        strings.Replace(paw, ".net/", ".net:0/", 1),
		"port not number":  strings.Replace(paw, ".net/", ".net:abc/", 1),
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
