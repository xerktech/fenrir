package main

import (
	"errors"
	"fmt"
	"net"
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

// checkVRFrontDoorFlags refuses a VR front door that would listen on a
// wildcard or loopback address (under hostNetwork, the node's), or whose
// codes nobody could enter.
func checkVRFrontDoorFlags(port int, address string, pinPort int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("--vr-port %d is not a port", port)
	}
	if pinPort == 0 {
		return errors.New("--vr-port needs --pin-port: headsets' pairing codes are entered on the pairing page")
	}
	ip := net.ParseIP(address)
	switch {
	case ip == nil:
		return fmt.Errorf("--vr-address %q is not an IP address", address)
	case ip.IsUnspecified() || ip.IsLoopback():
		return fmt.Errorf("--vr-address %s: the VR front door listens on one routable address, never a wildcard or loopback one", address)
	}
	return nil
}
