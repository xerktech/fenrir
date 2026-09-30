package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if tok, err := readToken(write("ok", "abc123\n")); err != nil || tok != "abc123" {
		t.Errorf("readToken(ok) = %q, %v; want abc123, nil", tok, err)
	}
	for name, path := range map[string]string{
		"unset":      "",
		"missing":    filepath.Join(dir, "nope"),
		"empty":      write("empty", ""),
		"whitespace": write("ws", " \n\t"),
		"directory":  dir,
	} {
		if tok, err := readToken(path); err == nil {
			t.Errorf("readToken(%s) = %q, nil; want error", name, tok)
		}
	}
}

// TestSelfClientSendsToken guards the in-pod agent controller: it reaches
// Wolf through this process's own authenticated proxy, so it must send the token.
func TestSelfClientSendsToken(t *testing.T) {
	srv := httptest.NewTLSServer(wolfapi.RequireBearerToken("s3cret",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "sessions": []any{}})
		})))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := selfClient(port, "s3cret").ListSessions(context.Background()); err != nil {
		t.Errorf("with token: %v", err)
	}
	if _, err := selfClient(port, "wrong").ListSessions(context.Background()); err == nil {
		t.Error("with wrong token: expected error")
	}
}
