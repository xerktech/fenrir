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

// WoWAddonSource is where an addon is published. Every download is checked
// against a SHA256 before it is installed:
//   - github: the latest release's asset, checked against the SHA256 GitHub
//     publishes for it.
//   - url: one fixed https URL, checked against the SHA256 listed here; for
//     addons published only where an API key is needed to look them up
//     (CurseForge, whose CDN file URLs never change). It moves only when the
//     list does.
//   - curseforge: the latest retail release of CurseForge project Project,
//     checked against the SHA1 and size the CurseForge API publishes. The
//     Library calls the API with the key in Secret wowAddonsCurseForgeSecret
//     (XERK-2489); without it these addons keep what is installed.
type WoWAddonSource struct {
	Type string `json:"type"`
	// Repo is the GitHub repository, owner/name; its latest release is used.
	Repo string `json:"repo,omitempty"`
	// Asset is a glob that must match exactly one of that release's assets, a zip.
	Asset string `json:"asset,omitempty"`
	// URL is the zip's https URL (type url).
	URL string `json:"url,omitempty"`
	// SHA256 is the zip's SHA256, lowercase hex (type url).
	SHA256 string `json:"sha256,omitempty"`
	// Project is the CurseForge project ID, its page's "Project ID" (type curseforge).
	Project int64 `json:"project,omitempty"`
}

// wowAddonsCurseForgeSecret holds the CurseForge API key (key apiKey) a
// curseforge addon needs. Optional: the Library starts without it.
const wowAddonsCurseForgeSecret = "direwolf-curseforge"

// wowAddonCurseForge is the curseforge source type.
const wowAddonCurseForge = "curseforge"

// WoWAddonFlavors are the flavors an addon may list; wow-addons-update maps
// each to its folder under World of Warcraft.
var WoWAddonFlavors = []string{"retail", "classic", "classic_era"}

var (
	wowAddonName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	wowAddonRepo  = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9_.-]{1,100}$`)
	wowAddonAsset = regexp.MustCompile(`^[A-Za-z0-9_.*?+-]{1,128}$`)
	// No query, fragment, credentials or curl glob characters.
	wowAddonURL    = regexp.MustCompile(`^https://[A-Za-z0-9.-]{1,253}(:[0-9]{1,5})?/[A-Za-z0-9._~%+-][A-Za-z0-9._~%/+-]{0,999}$`)
	wowAddonSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
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
		case len(a.Flavors) == 0:
			return nil, fmt.Errorf("addon %q lists no flavors", a.Name)
		// Only retail's file is picked; another flavor may need another.
		case a.Source.Type == wowAddonCurseForge && !slices.Equal(a.Flavors, []string{"retail"}):
			return nil, fmt.Errorf("addon %q: a curseforge source is retail only", a.Name)
		}
		if err := checkWoWAddonSource(&a.Source); err != nil {
			return nil, fmt.Errorf("addon %q: %w", a.Name, err)
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

func checkWoWAddonSource(s *WoWAddonSource) error {
	if s.Type != wowAddonCurseForge && s.Project != 0 {
		return errors.New("only a curseforge source takes a project")
	}
	switch s.Type {
	case "github":
		switch {
		case !wowAddonRepo.MatchString(s.Repo) || path.Clean(s.Repo) != s.Repo:
			return fmt.Errorf("repo %q is not owner/name", s.Repo)
		case !wowAddonAsset.MatchString(s.Asset):
			return fmt.Errorf("asset pattern %q must match %s", s.Asset, wowAddonAsset)
		case s.URL != "" || s.SHA256 != "":
			return errors.New("a github source takes no url or sha256")
		}
	case "url":
		switch {
		case !wowAddonURL.MatchString(s.URL):
			return fmt.Errorf("url %q must match %s", s.URL, wowAddonURL)
		case !wowAddonSHA256.MatchString(s.SHA256):
			return fmt.Errorf("sha256 %q is not 64 lowercase hex digits", s.SHA256)
		case s.Repo != "" || s.Asset != "":
			return errors.New("a url source takes no repo or asset")
		}
	case wowAddonCurseForge:
		switch {
		case s.Project <= 0 || s.Project >= 1<<31:
			return fmt.Errorf("project %d is not a CurseForge project ID", s.Project)
		case s.Repo != "" || s.Asset != "" || s.URL != "" || s.SHA256 != "":
			return errors.New("a curseforge source takes only a project")
		}
	default:
		return fmt.Errorf("source type %q is not supported (github, url or curseforge)", s.Type)
	}
	return nil
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

// usesCurseForge reports whether any addon needs the CurseForge API key.
func usesCurseForge(addons []WoWAddon) bool {
	return slices.ContainsFunc(addons, func(a WoWAddon) bool { return a.Source.Type == wowAddonCurseForge })
}
