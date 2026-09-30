package controllers

import (
	"testing"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func TestWolfSessionForClientIP(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "ipv4", in: "192.0.2.10", want: "192.0.2.10"},
		{name: "ipv6", in: "2001:db8::1", want: "2001:db8::1"},
		{name: "ipv4-mapped ipv6 is unmapped", in: "::ffff:192.0.2.10", want: "192.0.2.10"},
		// No placeholder: a wrong peer silently breaks the stream.
		{name: "empty", in: "", wantErr: true},
		{name: "host:port", in: "192.0.2.10:47989", wantErr: true},
		{name: "old ipv6 split bug", in: "[", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &v1alpha1types.Session{}
			session.Spec.Config.ClientIP = tc.in
			session.Spec.Config.AESKey = "key"
			session.Spec.Config.AESIV = "iv"

			got, err := wolfSessionFor(session, "10.0.0.4")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("wolfSessionFor(%q) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.ClientIP != tc.want {
				t.Errorf("ClientIP = %q, want %q", got.ClientIP, tc.want)
			}
			if got.RTSPFakeIP != "10.0.0.4" || got.AESKey != "key" || got.AESIV != "iv" {
				t.Errorf("unexpected session %+v", got)
			}
		})
	}
}
