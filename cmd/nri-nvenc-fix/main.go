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
// The preload entry is /usr/lib/nvenc-fix/$PLATFORM/libnvenc_fix.so, which
// glibc expands per process: 64-bit processes load the shim, 32-bit ones (the
// Steam client, Wine) an empty i686 object. A single 64-bit path would make
// every 32-bit process print "wrong ELF class" on each exec. glibc reports
// x86_64 CPUs as x86_64, haswell or xeon_phi, so those are symlinks.
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
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"
)

const (
	// injectAnnotation opts a pod in when set to "true".
	injectAnnotation = "nvenc-fix.xerktech.com/inject"

	shimFile    = "libnvenc_fix.so"
	shimDir     = "lib" // per-$PLATFORM subdirectories, under --shim-dir and --host-dir
	preloadFile = "ld.so.preload"

	// Where the shim directory and the preload list appear inside the container.
	containerShimDir     = "/usr/lib/nvenc-fix"
	containerPreloadPath = "/etc/ld.so.preload"
	preloadEntry         = containerShimDir + "/$PLATFORM/" + shimFile
)

// The $PLATFORM directories built into the image.
const (
	platform64 = "x86_64"
	platform32 = "i686"
)

var platforms = []string{platform64, platform32}

// platformAliases are the other $PLATFORM values glibc reports on x86, each
// pointing at a built directory.
var platformAliases = map[string]string{
	"haswell":  platform64,
	"xeon_phi": platform64,
	"i386":     platform32,
	"i486":     platform32,
	"i586":     platform32,
}

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
		if dst := m.GetDestination(); covers(dst, containerPreloadPath) || covers(dst, containerShimDir) {
			// Our mount on or under a pod volume either collides with it or
			// cannot create its mountpoint in a read-only one; both fail
			// container creation. The pod's own volume wins.
			klog.InfoS("Container mounts "+dst+", not injecting NVENC fix",
				"namespace", pod.GetNamespace(), "pod", pod.GetName(), "container", ctr.GetName())
			return nil
		}
	}

	adjust := &api.ContainerAdjustment{}
	for src, dst := range map[string]string{shimDir: containerShimDir, preloadFile: containerPreloadPath} {
		adjust.AddMount(&api.Mount{
			Destination: dst,
			Type:        "bind",
			Source:      filepath.Join(p.hostDir, src),
			Options:     []string{"rbind", "ro", "nosuid", "nodev"},
		})
	}
	return adjust
}

// covers reports whether a mount at dst is target or one of its parents.
func covers(dst, target string) bool {
	dst = filepath.Clean(dst)
	return dst == target || strings.HasPrefix(target, strings.TrimSuffix(dst, "/")+"/")
}

// install copies srcDir/<platform>/libnvenc_fix.so into hostDir/lib, links the
// platform aliases, and writes the preload list. Each file is written beside its
// target and renamed over it, so a process never sees half a file; one that
// already mapped the old shim keeps it.
func install(srcDir, hostDir string) error {
	var errs []error
	for _, plat := range platforms {
		shim, err := os.ReadFile(filepath.Join(srcDir, plat, shimFile))
		if err != nil {
			return fmt.Errorf("reading shim: %w", err)
		}
		dir := filepath.Join(hostDir, shimDir, plat)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		errs = append(errs, writeFileAtomic(filepath.Join(dir, shimFile), shim))
	}
	for alias, plat := range platformAliases {
		errs = append(errs, symlinkAtomic(plat, filepath.Join(hostDir, shimDir, alias)))
	}
	errs = append(errs, writeFileAtomic(filepath.Join(hostDir, preloadFile), []byte(preloadEntry+"\n")))
	return errors.Join(errs...)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := removeLeftover(tmp); err != nil {
		return err
	}
	// Created exclusively, so a leftover symlink can't redirect the write. 0644:
	// containers run as any user and must be able to load the shim.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	_, err = f.Write(data)
	// Chmod as well: the umask applies to OpenFile's mode.
	if err = errors.Join(err, f.Chmod(0o644), f.Close()); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	return rename(tmp, path)
}

func symlinkAtomic(target, path string) error {
	tmp := path + ".tmp"
	if err := removeLeftover(tmp); err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("linking %s: %w", tmp, err)
	}
	return rename(tmp, path)
}

// removeLeftover deletes a temp file an interrupted install left behind: reusing
// it would keep its mode, or follow it if it is a symlink.
func removeLeftover(tmp string) error {
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", tmp, err)
	}
	return nil
}

func rename(tmp, path string) error {
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming %s: %w", tmp, err)
	}
	return nil
}

func main() {
	pluginName := flag.String("name", "nvenc-fix", "NRI plugin name")
	pluginIdx := flag.String("idx", "20", "NRI plugin index (two digits, orders plugins)")
	socketPath := flag.String("socket", api.DefaultSocketPath, "Path to the runtime's NRI socket")
	shimSrc := flag.String("shim-dir", "/app/"+shimDir, "Directory holding <platform>/"+shimFile+" to install")
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
