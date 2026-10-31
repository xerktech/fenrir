package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	opts, err := provisionerOptions("k8x-games", "", "X-Authentik-Groups", "|", `{"resources":{"limits":{"cpu":"8"}}}`,
		"local-path", "100Gi", "", "1Ti")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Home.Size.String() != "100Gi" || opts.Games.Size.String() != "1Ti" ||
		opts.Home.StorageClassName != "local-path" || opts.Games.StorageClassName != "" || opts.UserTemplate.Resources == nil {
		t.Errorf("options = %+v", opts)
	}
	if opts.AllowedUsers != nil || !opts.Enabled() {
		t.Errorf("group only: AllowedUsers = %q, Enabled = %v", opts.AllowedUsers, opts.Enabled())
	}
	for name, args := range map[string][9]string{
		"spaced group":    {" g", "", "h", "|", "", "", "1Gi", "", "1Gi"},
		"empty allowlist": {"", " , ", "h", "|", "", "", "1Gi", "", "1Gi"},
		"email allowlist": {"", "alice@example.com", "h", "|", "", "", "1Gi", "", "1Gi"},
		"same User twice": {"", "alice,Alice", "h", "|", "", "", "1Gi", "", "1Gi"},
		"no header":       {"g", "", "", "|", "", "", "1Gi", "", "1Gi"},
		"no separator":    {"g", "", "h", "", "", "", "1Gi", "", "1Gi"},
		"invalid header":  {"g", "", "X Bad:", "|", "", "", "1Gi", "", "1Gi"},
		"bad template":    {"g", "", "h", "|", `{"library":{}}`, "", "1Gi", "", "1Gi"},
		"bad home size":   {"g", "", "h", "|", "", "", "lots", "", "1Gi"},
		"zero games size": {"g", "", "h", "|", "", "", "1Gi", "", "0"},
		"negative size":   {"g", "", "h", "|", "", "", "-1Gi", "", "1Gi"},
	} {
		if _, err := provisionerOptions(args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7], args[8]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An allowlist alone turns provisioning on (proxies that send no groups);
// the chart joins provisioning.allowedUsers with commas.
func TestProvisionerOptionsAllowedUsers(t *testing.T) {
	opts, err := provisionerOptions("", "alice, Bob ,,carol-d", "Remote-Groups", ",", "", "", "1Gi", "", "1Gi")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opts.AllowedUsers, []string{"alice", "Bob", "carol-d"}) || !opts.Enabled() {
		t.Errorf("AllowedUsers = %q, Enabled = %v", opts.AllowedUsers, opts.Enabled())
	}
	if opts.GroupsHeader != "Remote-Groups" || opts.GroupsSeparator != "," {
		t.Errorf("groups header %q separator %q", opts.GroupsHeader, opts.GroupsSeparator)
	}
}

// runMain runs main() in a child test process with env and args, and returns
// its combined output. main exits the process, so it can't run in this one.
func runMain(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunMainChild$") //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), append(env, "DIREWOLF_RUN_MAIN="+strings.Join(args, "\x00"), "KUBECONFIG=/nonexistent")...)
	out, _ := cmd.CombinedOutput() // main exits non-zero on --help and fatal errors alike
	return string(out)
}

func TestRunMainChild(t *testing.T) {
	args, ok := os.LookupEnv("DIREWOLF_RUN_MAIN")
	if !ok {
		t.Skip("run by runMain only")
	}
	os.Args = append([]string{"operator"}, strings.Split(args, "\x00")...)
	main()
}

// The chart sets the user header and the allowlist through env (proxyAuth,
// provisioning.allowedUsers); the flags must default from it.
func TestMainReadsProvisioningEnv(t *testing.T) {
	if out := runMain(t, []string{"LIBRARY_USER_HEADER=Remote-User"}, "--help"); !strings.Contains(out, `(default "Remote-User")`) {
		t.Errorf("--library-user-header ignores LIBRARY_USER_HEADER:\n%s", out)
	}
	// An allowlist alone turns provisioning on, so it needs the Library.
	if out := runMain(t, []string{"PROVISION_ALLOWED_USERS=alice"}); !strings.Contains(out, "need --library-port") {
		t.Errorf("allowlist without --library-port not refused:\n%s", out)
	}
}
