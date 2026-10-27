package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// WoWAddonSource is where an addon is published. Only GitHub releases are
// supported: GitHub publishes each asset's SHA256, so a download is checked
// against it. CurseForge and Wago need API keys and are not supported.
type WoWAddonSource struct {
	Type string `json:"type"`
	// Repo is the GitHub repository, owner/name; its latest release is used.
	Repo string `json:"repo"`
	// Asset is a glob that must match exactly one of that release's assets, a zip.
	Asset string `json:"asset"`
}

// WoWAddonFlavors are the flavors an addon may list; wow-addons-update maps
// each to its folder under World of Warcraft.
var WoWAddonFlavors = []string{"retail", "classic", "classic_era"}

var (
	wowAddonName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	wowAddonRepo  = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9_.-]{1,100}$`)
	wowAddonAsset = regexp.MustCompile(`^[A-Za-z0-9_.*?+-]{1,128}$`)
)

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
		case a.Source.Type != "github":
			return nil, fmt.Errorf("addon %q: source type %q is not supported (only github)", a.Name, a.Source.Type)
		case !wowAddonRepo.MatchString(a.Source.Repo) || path.Clean(a.Source.Repo) != a.Source.Repo:
			return nil, fmt.Errorf("addon %q: repo %q is not owner/name", a.Name, a.Source.Repo)
		case !wowAddonAsset.MatchString(a.Source.Asset):
			return nil, fmt.Errorf("addon %q: asset pattern %q must match %s", a.Name, a.Source.Asset, wowAddonAsset)
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
