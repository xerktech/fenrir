package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// checkPinFlags rejects pairing-page flag values that would never match a
// request, so a typo fails at start-up instead of silently refusing users.
func checkPinFlags(userHeader, provisionGroup, groupsHeader, groupsSeparator, libraryURL string) error {
	// An invalid header name ("X Bad:") never arrives in a request.
	if !httpguts.ValidHeaderFieldName(userHeader) {
		return fmt.Errorf("--pin-user-header %q is not a valid header name", userHeader)
	}
	// As the operator checks its --provision-group: a padded group never
	// matches, so members would silently get the plain refusal.
	if provisionGroup != strings.TrimSpace(provisionGroup) {
		return fmt.Errorf("--pin-provision-group %q has surrounding whitespace", provisionGroup)
	}
	if provisionGroup != "" {
		if groupsHeader == "" || groupsSeparator == "" {
			return errors.New("--pin-provision-group needs a non-empty --pin-groups-header and --pin-groups-separator")
		}
		if !httpguts.ValidHeaderFieldName(groupsHeader) {
			return fmt.Errorf("--pin-groups-header %q is not a valid header name", groupsHeader)
		}
	}
	if libraryURL != "" {
		if u, err := url.Parse(libraryURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("--pin-library-url %q is not an absolute http(s) URL", libraryURL)
		}
	}
	return nil
}
