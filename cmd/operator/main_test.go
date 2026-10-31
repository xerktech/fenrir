package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadProxySecret(t *testing.T) {
	secret32 := strings.Repeat("s", 32)
	for name, tc := range map[string]struct {
		content string
		want    string // "" means an error
	}{
		"exactly the minimum":  {secret32, secret32},
		"surrounding space":    {" \n" + secret32 + "\n", secret32},
		"one byte short":       {secret32[1:], ""},
		"short plus a newline": {secret32[1:] + "\n", ""},
		"whitespace only":      {strings.Repeat(" ", 40), ""},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readProxySecret(path)
			if tc.want == "" {
				if err == nil {
					t.Errorf("readProxySecret = %q, want an error", got)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Errorf("readProxySecret = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, err := readProxySecret(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Error("want an error")
		}
	})
}

func TestProvisionerOptions(t *testing.T) {
	opts, err := provisionerOptions("k8x-games", "X-Authentik-Groups", "|", `{"resources":{"limits":{"cpu":"8"}}}`,
		"local-path", "100Gi", "", "1Ti")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Home.Size.String() != "100Gi" || opts.Games.Size.String() != "1Ti" ||
		opts.Home.StorageClassName != "local-path" || opts.Games.StorageClassName != "" || opts.UserTemplate.Resources == nil {
		t.Errorf("options = %+v", opts)
	}
	for name, args := range map[string][8]string{
		"no header":       {"g", "", "|", "", "", "1Gi", "", "1Gi"},
		"no separator":    {"g", "h", "", "", "", "1Gi", "", "1Gi"},
		"bad template":    {"g", "h", "|", `{"library":{}}`, "", "1Gi", "", "1Gi"},
		"bad home size":   {"g", "h", "|", "", "", "lots", "", "1Gi"},
		"zero games size": {"g", "h", "|", "", "", "1Gi", "", "0"},
		"negative size":   {"g", "h", "|", "", "", "-1Gi", "", "1Gi"},
	} {
		if _, err := provisionerOptions(args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
