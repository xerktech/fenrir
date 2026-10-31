package main

import "testing"

func TestCheckPinFlags(t *testing.T) {
	const (
		user = "X-Authentik-Username"
		hdr  = "X-Authentik-Groups"
		sep  = "|"
		lib  = "https://library.example.com"
	)
	for name, tc := range map[string]struct {
		userHeader, group, groupsHeader, sep, libraryURL string
		wantErr                                          bool
	}{
		"defaults, no provisioning": {user, "", hdr, sep, "", false},
		"provisioning":              {user, "players", hdr, sep, lib, false},
		"invalid user header":       {"X Bad:", "", hdr, sep, "", true},
		"empty user header":         {"", "", hdr, sep, "", true},
		"padded group":              {user, " players", hdr, sep, "", true},
		"group without header":      {user, "players", "", sep, "", true},
		"group without separator":   {user, "players", hdr, "", "", true},
		"invalid groups header":     {user, "players", "X Bad:", sep, "", true},
		"invalid header unused":     {user, "", "X Bad:", sep, "", false},
		"relative library URL":      {user, "", hdr, sep, "/library", true},
		"non-http library URL":      {user, "", hdr, sep, "ftp://library.example.com", true},
		"plain http library URL":    {user, "", hdr, sep, "http://library.example.com", false},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkPinFlags(tc.userHeader, tc.group, tc.groupsHeader, tc.sep, tc.libraryURL)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkPinFlags = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
