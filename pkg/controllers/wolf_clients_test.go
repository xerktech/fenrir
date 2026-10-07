package controllers

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The seed is a version 6 config (so Wolf migrates it into its full default
// config, keeping these keys) whose clients carry certs Wolf can parse.
func TestWolfConfigSeed(t *testing.T) {
	seed, err := wolfConfigSeed()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hostname = ", "uuid = ", "config_version = 6\n"} {
		if !strings.Contains(seed, key) {
			t.Errorf("seed has no %q: Wolf's migration needs it", key)
		}
	}
	certs := map[string]bool{}
	rest := []byte(seed)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			t.Errorf("client cert: %v", err)
		}
		certs[string(block.Bytes)] = true
	}
	if len(certs) != wolfStreamClients || strings.Count(seed, "[[paired_clients]]") != wolfStreamClients {
		t.Errorf("seed has %d distinct certs, want %d clients:\n%s", len(certs), wolfStreamClients, seed)
	}
	for i := range wolfStreamClients {
		if !strings.Contains(seed, `app_state_folder = "`+wolfStreamClientPrefix+string(rune('0'+i))+`"`) {
			t.Errorf("seed has no client %d", i)
		}
	}
	if strings.Contains(seed, "PRIVATE KEY") {
		t.Error("seed carries a client key")
	}
}

// The init container seeds a volume once: a config without the stream
// clients is kept beside the seed, one with them is left alone.
func TestWolfConfigSeedScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	script := strings.ReplaceAll(wolfConfigSeedScript, wolfConfigPath, cfg)
	run := func(seed string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), sh, "-c", script)
		cmd.Env = append(os.Environ(), wolfConfigSeedEnv+"="+seed)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script: %v: %s", err, out)
		}
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	run("seed 1 direwolf-stream-3\n")
	if got := read(cfg); got != "seed 1 direwolf-stream-3\n" {
		t.Errorf("fresh volume: config %q, want the seed", got)
	}

	// Wolf migrated it (single-quoted strings); later pods keep it.
	migrated := "app_state_folder = 'direwolf-stream-3'\n"
	if err := os.WriteFile(cfg, []byte(migrated), 0o600); err != nil {
		t.Fatal(err)
	}
	run("seed 2 direwolf-stream-3\n")
	if got := read(cfg); got != migrated {
		t.Errorf("seeded volume: config %q, want it untouched", got)
	}

	if err := os.WriteFile(cfg, []byte("paired_clients = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("seed 3 direwolf-stream-3\n")
	if got := read(cfg); got != "seed 3 direwolf-stream-3\n" {
		t.Errorf("old config: config %q, want the seed", got)
	}
	if got := read(cfg + ".pre-direwolf"); got != "paired_clients = []\n" {
		t.Errorf("old config kept as %q", got)
	}
}
