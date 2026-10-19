// Package containerd226 replays nri-input's adjustments through the NRI that
// talos04's containerd (2.2.6) vendors, v0.11.0, the way containerd applies
// them: decoded from the wire, then written into the OCI spec by
// generate.Adjust. The plugin builds them with a newer NRI, and a field
// v0.11.0 mishandles shows nowhere else but as a containerd panic on the node
// (XERK-1915: a seccomp rule's errnoRet, XERK-1867).
//
// It is its own module so it can pin that NRI, and it reads the adjustments
// from ../testdata/adjustments, which the main module's
// TestAdjustmentsMatchTestdata keeps current. Bump the pins in go.mod with
// the node's containerd, never on their own.
package containerd226

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	nri "github.com/containerd/nri/pkg/api"
	nrigen "github.com/containerd/nri/pkg/runtime-tools/generate"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"google.golang.org/protobuf/encoding/protojson"
)

// adjust decodes the named adjustment as v0.11.0 and applies it to a default
// Linux spec, as containerd's WithContainerAdjustment does.
func adjust(t *testing.T, name string) *rspec.Spec {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "adjustments", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// Strict: containerd decodes the wire format, which drops a field v0.11.0
	// does not know without a word, and the plugin's intent with it.
	a := &nri.ContainerAdjustment{}
	if err = protojson.Unmarshal(b, a); err != nil {
		t.Fatalf("NRI v0.11.0 cannot read %s (containerd 2.2.6 would drop what it does not know): %v", name, err)
	}
	g, err := generate.New("linux")
	if err != nil {
		t.Fatal(err)
	}
	// No seccomp: the plugin only confines Unconfined containers.
	g.Config.Linux.Seccomp = nil
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NRI v0.11.0 panics applying %s, as containerd 2.2.6 would: %v", name, r)
		}
	}()
	if err = nrigen.SpecGenerator(&g).Adjust(a); err != nil {
		t.Fatalf("NRI v0.11.0 refuses %s: %v", name, err)
	}
	return g.Config
}

func TestEveryAdjustmentIsReplayed(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "testdata", "adjustments", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".json"))
	}
	if want := []string{"app-unconfined", "app", "wolf"}; !slices.Equal(names, want) {
		t.Fatalf("adjustments %v, want %v: add a test here for a new one", names, want)
	}
}

// hasCgroupRule reports whether spec allows rwm on the c major:minor device
// (minor -1: any).
func hasCgroupRule(spec *rspec.Spec, major, minor int64) bool {
	return slices.ContainsFunc(spec.Linux.Resources.Devices, func(d rspec.LinuxDeviceCgroup) bool {
		return d.Allow && d.Type == "c" && d.Access == "rwm" &&
			d.Major != nil && *d.Major == major &&
			((minor < 0 && d.Minor == nil) || (d.Minor != nil && *d.Minor == minor))
	})
}

func TestWolfGetsUinput(t *testing.T) {
	spec := adjust(t, "wolf")
	if !slices.ContainsFunc(spec.Linux.Devices, func(d rspec.LinuxDevice) bool {
		return d.Path == "/dev/uinput" && d.Type == "c" && d.Major == 10 && d.Minor == 223
	}) {
		t.Errorf("no /dev/uinput node: %+v", spec.Linux.Devices)
	}
	for _, r := range [][2]int64{{13, -1}, {241, -1}, {10, 223}} {
		if !hasCgroupRule(spec, r[0], r[1]) {
			t.Errorf("no rwm cgroup rule for c %d:%d: %+v", r[0], r[1], spec.Linux.Resources.Devices)
		}
	}
	if spec.Linux.Seccomp != nil {
		t.Errorf("Wolf's seccomp policy is the operator's, got %+v", spec.Linux.Seccomp)
	}
}

func TestAppGetsInputDevices(t *testing.T) {
	spec := adjust(t, "app")
	for _, major := range []int64{13, 241} {
		if !hasCgroupRule(spec, major, -1) {
			t.Errorf("no rwm cgroup rule for c %d:*: %+v", major, spec.Linux.Resources.Devices)
		}
	}
	if hasCgroupRule(spec, 10, 223) {
		t.Error("an App container got /dev/uinput")
	}
	if spec.Linux.Seccomp != nil {
		t.Errorf("a RuntimeDefault container's policy must be left alone, got %+v", spec.Linux.Seccomp)
	}
}

func TestUnconfinedAppGetsDenyList(t *testing.T) {
	spec := adjust(t, "app-unconfined")
	sc := spec.Linux.Seccomp
	if sc == nil {
		t.Fatal("no seccomp policy")
	}
	if sc.DefaultAction != rspec.ActAllow {
		t.Errorf("default action %q, want allow", sc.DefaultAction)
	}
	for _, arch := range []rspec.Arch{rspec.ArchX86_64, rspec.ArchX86, rspec.ArchX32} {
		if !slices.Contains(sc.Architectures, arch) {
			t.Errorf("architectures %v lack %s: the filter would kill its syscalls", sc.Architectures, arch)
		}
	}
	if len(sc.Syscalls) != 1 {
		t.Fatalf("syscall rules %+v, want one", sc.Syscalls)
	}
	rule := sc.Syscalls[0]
	if rule.Action != rspec.ActErrno || !slices.Contains(rule.Names, "bpf") {
		t.Errorf("rule %+v, want bpf among the ERRNO names", rule)
	}
}
