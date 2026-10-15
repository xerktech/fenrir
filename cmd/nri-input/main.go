// Command nri-input is an NRI plugin that grants Direwolf session pods access
// to host input devices, so Wolf can create virtual controllers via
// /dev/uinput and the game container can open the resulting /dev/input and
// /dev/hidraw nodes, without running the pod privileged.
//
// Grants are per container: only Wolf gets /dev/uinput (an App with it could
// create keyboards and mice in the host kernel), the operator's other
// containers get nothing, and every other container (the App's) gets the
// /dev/input and /dev/hidraw majors.
//
// The App's containers also get a seccomp filter denying host-kernel
// interfaces no game needs (deniedSyscalls). Steam-style Apps add SYS_ADMIN
// with seccomp Unconfined, and with bpf() such a container detached its own
// device-cgroup program and opened the node's disks (XERK-1915).
//
// Only pods carrying v1alpha1.SessionPodLabel are adjusted; every other pod is
// left untouched. Because any pod author can set a label, the plugin refuses to
// start unless --namespaces names the namespaces the operator runs sessions in,
// or --all-namespaces explicitly opts in to trusting the label cluster-wide.
package main

import (
	"context"
	"errors"
	"flag"
	"runtime"
	"slices"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

// Linux device numbers granted to session containers.
const (
	inputMajor  = 13  // /dev/input/* (evdev, js, mice)
	hidrawMajor = 241 // /dev/hidraw*; dynamically allocated, 241 on current kernels
	miscMajor   = 10
	uinputMinor = 223 // /dev/uinput is misc 10:223
	uinputPath  = "/dev/uinput"
	deviceRWM   = "rwm"
)

type plugin struct {
	// namespaces restricts adjustment to pods in these namespaces; empty
	// means any namespace (only reachable via --all-namespaces).
	namespaces []string
}

// CreateContainer implements stub.CreateContainerInterface.
//
//nolint:unparam // signature is fixed by the NRI stub interface
func (p *plugin) CreateContainer(
	_ context.Context,
	pod *api.PodSandbox,
	ctr *api.Container,
) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	adjust := p.adjustment(pod, ctr.GetName())
	if adjust == nil {
		return nil, nil, nil
	}
	if !isOperatorContainer(ctr.GetName()) {
		if policy := confinedSeccomp(ctr.GetLinux()); policy != nil {
			adjust.SetLinuxSeccompPolicy(policy)
		}
	}
	klog.InfoS("Granting input devices", "namespace", pod.GetNamespace(), "pod", pod.GetName(), "container", ctr.GetName())
	return adjust, nil, nil
}

// deniedSyscalls are host-kernel interfaces an App container never gets, even
// with SYS_ADMIN: bpf() detaches the container's device-cgroup program
// (XERK-1915), and the rest load kernel code, read other processes' keys or
// files by handle, or reach hardware. Steam's bwrap needs mount, unshare and
// clone, so those stay; seccomp can't see a mount's fstype anyway.
var deniedSyscalls = []string{
	"bpf",
	"perf_event_open",
	"kexec_load", "kexec_file_load",
	"init_module", "finit_module", "delete_module",
	"open_by_handle_at", "name_to_handle_at",
	"keyctl", "add_key", "request_key",
	"iopl", "ioperm",
	"swapon", "swapoff",
	"reboot",
	"acct",
	"lookup_dcookie",
	"quotactl", "quotactl_fd",
}

// seccompArches are the ABIs a default-allow filter must cover: the filter
// kills a syscall from an ABI it doesn't list, and Proton runs 32-bit games.
// Matches the arches of containerd's default profile. On other GOARCHes the
// list is empty, which runc reads as the native ABI only.
var seccompArches = map[string][]string{
	"amd64": {"SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_X32"},
	"arm64": {"SCMP_ARCH_AARCH64", "SCMP_ARCH_ARM"},
}

// confinedSeccomp returns a seccomp policy for a seccomp-Unconfined
// container that allows everything but deniedSyscalls, which return EPERM, or
// nil to leave the container's policy alone.
//
// Only Unconfined containers are adjusted: the adjustment replaces the whole
// policy, and containerd (2.2.6) reports a container's profile type but never
// its resolved policy, so a RuntimeDefault or Localhost one can't be extended
// without loosening it. The operator refuses App containers that add SYS_ADMIN
// under RuntimeDefault, which allows bpf() (validateNoHostDevices).
func confinedSeccomp(linux *api.LinuxContainer) *api.LinuxSeccomp {
	// A nil profile reads as RUNTIME_DEFAULT.
	if linux.GetSeccompProfile().GetProfileType() != api.SecurityProfile_UNCONFINED {
		return nil
	}
	return &api.LinuxSeccomp{
		DefaultAction: "SCMP_ACT_ALLOW",
		Architectures: seccompArches[runtime.GOARCH],
		// No ErrnoRet: containerd 2.2.6's NRI (v0.11.0) writes it through a
		// nil pointer and the whole runtime panics (XERK-1915). runc returns
		// EPERM for an ERRNO action without one.
		Syscalls: []*api.LinuxSyscall{{
			Names:  deniedSyscalls,
			Action: "SCMP_ACT_ERRNO",
		}},
	}
}

// isOperatorContainer reports whether ctrName is one of the operator's own
// containers rather than the App's.
func isOperatorContainer(ctrName string) bool {
	switch ctrName {
	case v1alpha1types.WolfContainerName, v1alpha1types.WolfAgentContainerName,
		v1alpha1types.PulseAudioContainerName, v1alpha1types.InitContainerName,
		v1alpha1types.LoopbackShimContainerName, v1alpha1types.SteamRelinkContainerName:
		return true
	}
	return false
}

// adjustment returns the device adjustment for the container named ctrName in
// pod, or nil if it gets no devices.
func (p *plugin) adjustment(pod *api.PodSandbox, ctrName string) *api.ContainerAdjustment {
	if pod.GetLabels()[v1alpha1types.SessionPodLabel] != v1alpha1types.SessionPodLabelValue {
		return nil
	}
	if len(p.namespaces) > 0 && !slices.Contains(p.namespaces, pod.GetNamespace()) {
		return nil
	}

	inputRules := []*api.LinuxDeviceCgroup{
		{Allow: true, Type: "c", Major: api.Int64(inputMajor), Access: deviceRWM},
		{Allow: true, Type: "c", Major: api.Int64(hidrawMajor), Access: deviceRWM},
	}
	adjust := &api.ContainerAdjustment{Linux: &api.LinuxContainerAdjustment{}}
	switch ctrName {
	case v1alpha1types.WolfContainerName:
		// Creates the /dev/uinput node in the container (the runtime also adds
		// an "rw" cgroup rule for it; the explicit rule below adds "m").
		adjust.AddDevice(&api.LinuxDevice{
			Path:  uinputPath,
			Type:  "c",
			Major: miscMajor,
			Minor: uinputMinor,
		})
		adjust.Linux.Resources = &api.LinuxResources{Devices: append(inputRules,
			&api.LinuxDeviceCgroup{Allow: true, Type: "c", Major: api.Int64(miscMajor), Minor: api.Int64(uinputMinor), Access: deviceRWM},
		)}
	case v1alpha1types.WolfAgentContainerName, v1alpha1types.PulseAudioContainerName,
		v1alpha1types.InitContainerName, v1alpha1types.LoopbackShimContainerName, v1alpha1types.SteamRelinkContainerName:
		return nil
	default:
		// The App's containers open the nodes wolf-agent mknods into their
		// shared /dev/input.
		adjust.Linux.Resources = &api.LinuxResources{Devices: inputRules}
	}
	return adjust
}

// newPlugin validates the namespace scope. It fails closed: the session label
// is pod-author controlled, so an unscoped plugin needs an explicit
// allNamespaces opt-in.
func newPlugin(namespaces string, allNamespaces bool) (*plugin, error) {
	p := &plugin{namespaces: parseNamespaces(namespaces)}
	switch {
	case len(p.namespaces) > 0 && allNamespaces:
		return nil, errors.New("--namespaces and --all-namespaces are mutually exclusive")
	case len(p.namespaces) == 0 && !allNamespaces:
		return nil, errors.New("--namespaces is required (or pass --all-namespaces to trust the " +
			v1alpha1types.SessionPodLabel + " label in every namespace)")
	}
	return p, nil
}

// parseNamespaces splits a comma-separated namespace list, trimming spaces and
// dropping empty entries.
func parseNamespaces(s string) []string {
	var out []string
	for ns := range strings.SplitSeq(s, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

func main() {
	pluginName := flag.String("name", "direwolf-nri-input", "NRI plugin name")
	pluginIdx := flag.String("idx", "10", "NRI plugin index (two digits, orders plugins)")
	socketPath := flag.String("socket", api.DefaultSocketPath, "Path to the runtime's NRI socket")
	namespaces := flag.String("namespaces", "", "Comma-separated namespaces whose session pods may get input devices")
	allNamespaces := flag.Bool("all-namespaces", false,
		"Grant input devices to labelled pods in any namespace (anyone able to create a pod can then read and inject host input)")
	klog.InitFlags(nil)
	flag.Parse()

	p, err := newPlugin(*namespaces, *allNamespaces)
	if err != nil {
		klog.Fatal(err)
	}
	if len(p.namespaces) == 0 {
		klog.Warning("--all-namespaces set: any pod labelled " + v1alpha1types.SessionPodLabel + " gets input devices")
	} else {
		klog.InfoS("Restricting input devices to namespaces", "namespaces", p.namespaces)
	}

	s, err := stub.New(p,
		stub.WithPluginName(*pluginName),
		stub.WithPluginIdx(*pluginIdx),
		stub.WithSocketPath(*socketPath),
	)
	if err != nil {
		klog.Fatalf("Failed to create NRI stub: %v", err)
	}
	if err := s.Run(context.Background()); err != nil {
		klog.Fatalf("NRI plugin exited: %v", err)
	}
}
