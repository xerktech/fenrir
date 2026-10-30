package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
)

// WoWAddon is one World of Warcraft addon the Library keeps installed and
// up to date (the chart's wowAddons). The Library image's wow-addons-update
// installs it from Source into each listed flavor's Interface/AddOns on the
// games volume; an addon no longer listed is uninstalled there.
type WoWAddon struct {
	Name    string         `json:"name"`
	Source  WoWAddonSource `json:"source"`
	Flavors []string       `json:"flavors"`
}

// WoWAddonSource is where an addon is published. Every download is checked
// against a SHA256 before it is installed:
//   - "github": the latest release of Repo, checked against the SHA256 GitHub
//     publishes for the asset.
//   - "url": one fixed zip at URL, checked against the SHA256 the list pins.
//     It never updates by itself; change URL and SHA256 together. This is how
//     addons published only on CurseForge are listed: its API needs a key,
//     but its CDN serves each file at a fixed URL without one (XERK-2486).
type WoWAddonSource struct {
	Type string `json:"type"`
	// Repo is the GitHub repository, owner/name; its latest release is used.
	Repo string `json:"repo,omitempty"`
	// Asset is a glob that must match exactly one of that release's assets, a zip.
	Asset string `json:"asset,omitempty"`
	// URL is the zip a "url" source installs: https, no credentials.
	URL string `json:"url,omitempty"`
	// SHA256 is the hex SHA256 that zip must have.
	SHA256 string `json:"sha256,omitempty"`
}

// WoWAddonFlavors are the flavors an addon may list; wow-addons-update maps
// each to its folder under World of Warcraft.
var WoWAddonFlavors = []string{"retail", "classic", "classic_era"}

var (
	wowAddonName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	wowAddonRepo  = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9_.-]{1,100}$`)
	wowAddonAsset = regexp.MustCompile(`^[A-Za-z0-9_.*?+-]{1,128}$`)
	// Unreserved characters and the few delimiters CDN paths and queries use:
	// no spaces, quotes, shell metacharacters, fragments or controls.
	// RE2 caps a repeat at 1000, so the length limit is wowAddonURLMax instead.
	wowAddonURL    = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?@&+,;=%-]+$`)
	wowAddonSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const wowAddonURLMax = 2048

// validWoWAddonURL reports whether s is a plain https URL to a host, with no
// credentials. wow-addons-update checks the same: it is handed the list again.
func validWoWAddonURL(s string) bool {
	if len(s) > wowAddonURLMax || !wowAddonURL.MatchString(s) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

// ParseWoWAddons reads the --library-wow-addons JSON list. Empty means none.
// It is strict: wow-addons-update uninstalls every managed addon a list
// leaves out, so a list it misread would uninstall them.
func ParseWoWAddons(s string) ([]WoWAddon, error) {
	if s == "" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	var addons []WoWAddon
	if err := dec.Decode(&addons); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	if dec.More() {
		return nil, errors.New("trailing data after the list")
	}
	seen := map[string]bool{}
	for i, a := range addons {
		switch {
		case !wowAddonName.MatchString(a.Name):
			return nil, fmt.Errorf("addon %d: name %q must match %s", i, a.Name, wowAddonName)
		case seen[a.Name]:
			return nil, fmt.Errorf("addon %q is listed twice", a.Name)
		case a.Source.Type != "github" && a.Source.Type != "url":
			return nil, fmt.Errorf("addon %q: source type %q is not supported (github or url)", a.Name, a.Source.Type)
		case a.Source.Type == "github" && (a.Source.URL != "" || a.Source.SHA256 != ""):
			return nil, fmt.Errorf("addon %q: a github source takes no url or sha256", a.Name)
		case a.Source.Type == "github" && (!wowAddonRepo.MatchString(a.Source.Repo) || path.Clean(a.Source.Repo) != a.Source.Repo):
			return nil, fmt.Errorf("addon %q: repo %q is not owner/name", a.Name, a.Source.Repo)
		case a.Source.Type == "github" && !wowAddonAsset.MatchString(a.Source.Asset):
			return nil, fmt.Errorf("addon %q: asset pattern %q must match %s", a.Name, a.Source.Asset, wowAddonAsset)
		case a.Source.Type == "url" && (a.Source.Repo != "" || a.Source.Asset != ""):
			return nil, fmt.Errorf("addon %q: a url source takes no repo or asset", a.Name)
		case a.Source.Type == "url" && !validWoWAddonURL(a.Source.URL):
			return nil, fmt.Errorf("addon %q: url %q is not a plain https URL", a.Name, a.Source.URL)
		case a.Source.Type == "url" && !wowAddonSHA256.MatchString(a.Source.SHA256):
			return nil, fmt.Errorf("addon %q: sha256 %q must be 64 lowercase hex digits", a.Name, a.Source.SHA256)
		case len(a.Flavors) == 0:
			return nil, fmt.Errorf("addon %q lists no flavors", a.Name)
		}
		for _, f := range a.Flavors {
			if !slices.Contains(WoWAddonFlavors, f) {
				return nil, fmt.Errorf("addon %q: flavor %q is not one of %v", a.Name, f, WoWAddonFlavors)
			}
		}
		seen[a.Name] = true
	}
	return addons, nil
}

// wowAddonsEnv is the Library pod's WOW_ADDONS: always set, "[]" for none,
// so dropping the last addon uninstalls it too.
func wowAddonsEnv(addons []WoWAddon) string {
	if addons == nil {
		addons = []WoWAddon{}
	}
	b, err := json.Marshal(addons)
	if err != nil {
		// Plain strings and slices always marshal.
		panic(err)
	}
	return string(b)
}
