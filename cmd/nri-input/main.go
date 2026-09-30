// Command nri-input is an NRI plugin that grants Direwolf session pods access
// to host input devices, so Wolf can create virtual controllers via
// /dev/uinput and the game container can open the resulting /dev/input and
// /dev/hidraw nodes, without running the pod privileged.
//
// Only pods carrying v1alpha1.SessionPodLabel are adjusted; every other pod is
// left untouched. Because any pod author can set a label, --namespaces should
// be used to restrict the grant to the namespaces the operator runs sessions in.
package main

import (
	"context"
	"flag"
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
	// means any namespace.
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
	adjust := p.adjustment(pod)
	if adjust != nil {
		klog.InfoS("Granting input devices", "namespace", pod.GetNamespace(), "pod", pod.GetName(), "container", ctr.GetName())
	}
	return adjust, nil, nil
}

// adjustment returns the device adjustment for a container in pod, or nil if
// the pod is not a session pod this plugin should touch.
func (p *plugin) adjustment(pod *api.PodSandbox) *api.ContainerAdjustment {
	if pod.GetLabels()[v1alpha1types.SessionPodLabel] != v1alpha1types.SessionPodLabelValue {
		return nil
	}
	if len(p.namespaces) > 0 && !slices.Contains(p.namespaces, pod.GetNamespace()) {
		return nil
	}

	adjust := &api.ContainerAdjustment{}
	// Creates the /dev/uinput node in the container (the runtime also adds
	// an "rw" cgroup rule for it; the explicit rule below adds "m").
	adjust.AddDevice(&api.LinuxDevice{
		Path:  uinputPath,
		Type:  "c",
		Major: miscMajor,
		Minor: uinputMinor,
	})
	adjust.Linux.Resources = &api.LinuxResources{
		Devices: []*api.LinuxDeviceCgroup{
			{Allow: true, Type: "c", Major: api.Int64(inputMajor), Access: deviceRWM},
			{Allow: true, Type: "c", Major: api.Int64(hidrawMajor), Access: deviceRWM},
			{Allow: true, Type: "c", Major: api.Int64(miscMajor), Minor: api.Int64(uinputMinor), Access: deviceRWM},
		},
	}
	return adjust
}

func main() {
	pluginName := flag.String("name", "direwolf-nri-input", "NRI plugin name")
	pluginIdx := flag.String("idx", "10", "NRI plugin index (two digits, orders plugins)")
	socketPath := flag.String("socket", api.DefaultSocketPath, "Path to the runtime's NRI socket")
	namespaces := flag.String("namespaces", "", "Comma-separated namespaces whose session pods may get input devices (empty: any)")
	klog.InitFlags(nil)
	flag.Parse()

	p := &plugin{}
	if *namespaces != "" {
		p.namespaces = strings.Split(*namespaces, ",")
	} else {
		klog.Warning("--namespaces not set: any pod labelled " + v1alpha1types.SessionPodLabel + " gets input devices")
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
