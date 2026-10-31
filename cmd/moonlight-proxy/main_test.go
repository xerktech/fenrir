package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRunMainChild(t *testing.T) {
	if os.Getenv("DIREWOLF_RUN_MAIN") == "" {
		t.Skip("run by TestPinUserHeaderFromEnv only")
	}
	os.Args = []string{"moonlight-proxy", "--help"}
	main()
}

// The chart sets the pairing page's user header (proxyAuth.userHeader) and
// allowlist (provisioning.allowedUsers) through env; the flags default from it.
func TestPinUserHeaderFromEnv(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunMainChild$") //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), "DIREWOLF_RUN_MAIN=1", "PIN_USER_HEADER=Remote-User", "PIN_PROVISION_ALLOWED_USERS=alice,Bob")
	out, _ := cmd.CombinedOutput() // --help exits non-zero
	if !strings.Contains(string(out), `(default "Remote-User")`) {
		t.Errorf("--pin-user-header ignores PIN_USER_HEADER:\n%s", out)
	}
	if !strings.Contains(string(out), `(default "alice,Bob")`) {
		t.Errorf("--pin-provision-allowed-users ignores PIN_PROVISION_ALLOWED_USERS:\n%s", out)
	}
}
