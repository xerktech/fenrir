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
