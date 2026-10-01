// Command nri-nvenc-fix is an NRI plugin that makes NVENC and NVDEC work in
// containers that hold some, but not all, of a node's NVIDIA GPUs.
//
// From driver 570 on, libnvidia-encode and libnvcuvid ask the kernel driver
// for its attached GPUs and get every GPU on the host, not just the
// container's. Given several, they peer-initialise against one "primary" GPU
// and fail (NV_ENC_ERR_UNSUPPORTED_DEVICE, CUDA_ERROR_NO_DEVICE) when its
// /dev/nvidiaN is not in the container. CUDA itself is unaffected.
//
// The fix is shim/nvenc_fix.c, an ioctl interposer that drops GPUs whose
// device node the container lacks from that answer. This plugin installs the
// shim into --host-dir and, for every container of a pod annotated
// nvenc-fix.xerktech.com/inject=true, bind-mounts it read-only together with
// an /etc/ld.so.preload naming it. Every other container is left untouched.
//
// /etc/ld.so.preload rather than LD_PRELOAD: images and their scripts set
// LD_PRELOAD themselves (Selkies' joystick interposer) and would replace ours,
// while glibc always reads the file. musl ignores it, so Alpine sidecars in an
// annotated pod are unaffected.
//
// The adjustment grants nothing: two read-only files the pod could have
// shipped itself. That is why, unlike nri-input, it needs no namespace scope.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"
)

const (
	// injectAnnotation opts a pod in when set to "true".
	injectAnnotation = "nvenc-fix.xerktech.com/inject"

	shimFile    = "libnvenc_fix.so"
	preloadFile = "ld.so.preload"

	// Where the shim and the preload list appear inside the container.
	containerShimPath    = "/usr/lib/nvenc-fix/" + shimFile
	containerPreloadPath = "/etc/ld.so.preload"
)

type plugin struct {
	// hostDir holds the installed shim and preload list. Mount sources are
	// resolved on the host, so it must be the same path inside the plugin's
	// container and on the node.
	hostDir string
}

// CreateContainer implements stub.CreateContainerInterface.
//
//nolint:unparam // signature is fixed by the NRI stub interface
func (p *plugin) CreateContainer(
	_ context.Context,
	pod *api.PodSandbox,
	ctr *api.Container,
) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	adjust := p.adjustment(pod, ctr)
	if adjust != nil {
		klog.InfoS("Injecting NVENC fix", "namespace", pod.GetNamespace(), "pod", pod.GetName(), "container", ctr.GetName())
	}
	return adjust, nil, nil
}

// adjustment returns the mounts that preload the shim into ctr, or nil if
// pod has not opted in.
func (p *plugin) adjustment(pod *api.PodSandbox, ctr *api.Container) *api.ContainerAdjustment {
	if pod.GetAnnotations()[injectAnnotation] != "true" {
		return nil
	}
	for _, m := range ctr.GetMounts() {
		if m.GetDestination() == containerPreloadPath || m.GetDestination() == containerShimPath {
			// A second mount on the same destination would fail container
			// creation; the pod's own file wins.
			klog.InfoS("Container already mounts "+m.GetDestination()+", not injecting NVENC fix",
				"namespace", pod.GetNamespace(), "pod", pod.GetName(), "container", ctr.GetName())
			return nil
		}
	}

	adjust := &api.ContainerAdjustment{}
	for src, dst := range map[string]string{shimFile: containerShimPath, preloadFile: containerPreloadPath} {
		adjust.AddMount(&api.Mount{
			Destination: dst,
			Type:        "bind",
			Source:      filepath.Join(p.hostDir, src),
			Options:     []string{"rbind", "ro", "nosuid", "nodev"},
		})
	}
	return adjust
}

// install copies the shim from shimSrc into hostDir and writes the preload
// list naming its in-container path. Each file is written beside its target
// and renamed over it, so a container started meanwhile never sees half a
// file; containers already running keep the inode they bound.
func install(shimSrc, hostDir string) error {
	shim, err := os.ReadFile(shimSrc)
	if err != nil {
		return fmt.Errorf("reading shim: %w", err)
	}
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", hostDir, err)
	}
	return errors.Join(
		writeFileAtomic(filepath.Join(hostDir, shimFile), shim),
		writeFileAtomic(filepath.Join(hostDir, preloadFile), []byte(containerShimPath+"\n")),
	)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	// 0644: containers run as any user and must be able to load the shim.
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming %s: %w", tmp, err)
	}
	return nil
}

func main() {
	pluginName := flag.String("name", "nvenc-fix", "NRI plugin name")
	pluginIdx := flag.String("idx", "20", "NRI plugin index (two digits, orders plugins)")
	socketPath := flag.String("socket", api.DefaultSocketPath, "Path to the runtime's NRI socket")
	shimSrc := flag.String("shim", "/app/"+shimFile, "Shim shared object to install")
	hostDir := flag.String("host-dir", "/var/lib/nvenc-fix",
		"Directory the shim is installed into; must be the same path on the node and in this container")
	klog.InitFlags(nil)
	flag.Parse()

	if err := install(*shimSrc, *hostDir); err != nil {
		klog.Fatalf("Installing shim: %v", err)
	}
	klog.InfoS("Installed NVENC fix", "dir", *hostDir, "annotation", injectAnnotation)

	s, err := stub.New(&plugin{hostDir: *hostDir},
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
