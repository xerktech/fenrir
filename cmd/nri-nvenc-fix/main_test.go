package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"
)

func annotatedPod(value string) *api.PodSandbox {
	return &api.PodSandbox{
		Name:        "session-pod",
		Namespace:   "streaming",
		Annotations: map[string]string{injectAnnotation: value},
	}
}

// devices is a container holding the given device nodes.
func devices(paths ...string) *api.Container {
	ctr := &api.Container{Name: "app", Linux: &api.LinuxContainer{}}
	for _, p := range paths {
		ctr.Linux.Devices = append(ctr.Linux.Devices, &api.LinuxDevice{Path: p, Type: "c"})
	}
	return ctr
}

// gpuContainer holds gpu-0 as a DRA claim hands it out on talos04 (seen over NRI).
func gpuContainer(mounts ...*api.Mount) *api.Container {
	ctr := devices("/dev/nvidia-modeset", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidiactl",
		"/dev/nvidia0", "/dev/dri/card0", "/dev/dri/renderD128")
	ctr.Mounts = mounts
	return ctr
}

func TestAdjustmentGPUClaim(t *testing.T) {
	p := &plugin{hostDir: "/var/lib/nvenc-fix"}
	require.NotNil(t, p.adjustment(&api.PodSandbox{Name: "vllm", Namespace: "ai"}, gpuContainer()))
	require.NotNil(t, p.adjustment(annotatedPod("yes"), gpuContainer()), "only true/false override")
}

func TestAdjustmentAnnotatedPod(t *testing.T) {
	adjust := (&plugin{hostDir: "/var/lib/nvenc-fix"}).adjustment(annotatedPod("true"), &api.Container{Name: "wolf"})
	require.NotNil(t, adjust)

	got := map[string]*api.Mount{}
	for _, m := range adjust.GetMounts() {
		got[m.Destination] = m
	}
	require.Len(t, got, 2)

	shim := got["/usr/lib/nvenc-fix"]
	require.NotNil(t, shim)
	require.Equal(t, "/var/lib/nvenc-fix/lib", shim.Source)

	preload := got["/etc/ld.so.preload"]
	require.NotNil(t, preload)
	require.Equal(t, "/var/lib/nvenc-fix/ld.so.preload", preload.Source)

	for _, m := range got {
		require.Equal(t, "bind", m.Type)
		require.Subset(t, m.Options, []string{"ro", "nosuid", "nodev"})
	}
}

func TestAdjustmentUntouched(t *testing.T) {
	mountAt := func(dst string) *api.Container {
		return &api.Container{Name: "app", Mounts: []*api.Mount{{Destination: dst}}}
	}
	tests := map[string]struct {
		pod *api.PodSandbox
		ctr *api.Container
	}{
		"no annotations, no GPU":             {pod: &api.PodSandbox{Name: "other", Namespace: "streaming"}, ctr: &api.Container{Name: "app"}},
		"annotation not true, no GPU":        {pod: annotatedPod("yes"), ctr: &api.Container{Name: "app"}},
		"opted out with a GPU":               {pod: annotatedPod("false"), ctr: gpuContainer()},
		"control nodes only":                 {pod: &api.PodSandbox{Name: "other"}, ctr: devices("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-modeset")},
		"a DRI node only (iGPU)":             {pod: &api.PodSandbox{Name: "other"}, ctr: devices("/dev/dri/renderD128")},
		"GPU but own /etc/ld.so.preload":     {pod: &api.PodSandbox{Name: "other"}, ctr: gpuContainer(&api.Mount{Destination: "/etc/ld.so.preload"})},
		"own /etc/ld.so.preload":             {pod: annotatedPod("true"), ctr: mountAt("/etc/ld.so.preload")},
		"volume at /etc":                     {pod: annotatedPod("true"), ctr: mountAt("/etc/")},
		"volume at the shim dir":             {pod: annotatedPod("true"), ctr: mountAt("/usr/lib/nvenc-fix")},
		"volume at a parent of the shim dir": {pod: annotatedPod("true"), ctr: mountAt("/usr/lib")},
		"volume under the shim dir":          {pod: annotatedPod("true"), ctr: mountAt("/usr/lib/nvenc-fix/sub")},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, (&plugin{hostDir: "/var/lib/nvenc-fix"}).adjustment(tc.pod, tc.ctr))
		})
	}
}

func TestAdjustmentUnrelatedMounts(t *testing.T) {
	ctr := &api.Container{Name: "app", Mounts: []*api.Mount{
		{Destination: "/etc/hosts"},  // a sibling, not a parent
		{Destination: "/usr/lib64"},  // shares a prefix, not a path component
		{Destination: "/home/retro"}, // unrelated
	}}
	require.NotNil(t, (&plugin{hostDir: "/var/lib/nvenc-fix"}).adjustment(annotatedPod("true"), ctr))
}

func writeShims(t *testing.T, content string) string {
	t.Helper()
	src := t.TempDir()
	for _, plat := range platforms {
		require.NoError(t, os.MkdirAll(filepath.Join(src, plat), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(src, plat, shimFile), []byte(plat+"-"+content), 0o600))
	}
	return src
}

func TestInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nvenc-fix")
	require.NoError(t, install(writeShims(t, "v1"), dir))

	for _, d := range []string{dir, filepath.Join(dir, "lib"), filepath.Join(dir, "lib", "x86_64")} {
		info, err := os.Stat(d)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), d)
	}
	for _, plat := range platforms {
		path := filepath.Join(dir, "lib", plat, "libnvenc_fix.so")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, plat+"-v1", string(data))
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	}
	for alias, plat := range platformAliases {
		data, err := os.ReadFile(filepath.Join(dir, "lib", alias, "libnvenc_fix.so"))
		require.NoError(t, err, alias)
		require.Equal(t, plat+"-v1", string(data))
	}

	preload, err := os.ReadFile(filepath.Join(dir, "ld.so.preload"))
	require.NoError(t, err)
	require.Equal(t, "/usr/lib/nvenc-fix/$PLATFORM/libnvenc_fix.so\n", string(preload))

	// A restart with a new image replaces the installed shims and links.
	require.NoError(t, install(writeShims(t, "v2"), dir))
	data, err := os.ReadFile(filepath.Join(dir, "lib", "haswell", "libnvenc_fix.so"))
	require.NoError(t, err)
	require.Equal(t, "x86_64-v2", string(data))
}

func TestInstallIgnoresLeftoverTemp(t *testing.T) {
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "lib", "x86_64", "libnvenc_fix.so")
	require.NoError(t, os.MkdirAll(filepath.Dir(shimPath), 0o755))
	// A 0600 leftover must not decide the installed mode...
	require.NoError(t, os.WriteFile(shimPath+".tmp", nil, 0o600))
	// ...and a symlink leftover must not be followed.
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.Symlink(victim, filepath.Join(dir, "ld.so.preload.tmp")))

	require.NoError(t, install(writeShims(t, "v1"), dir))

	info, err := os.Stat(shimPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	info, err = os.Lstat(filepath.Join(dir, "ld.so.preload"))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.NoFileExists(t, victim)
}

func TestInstallMissingShim(t *testing.T) {
	require.Error(t, install(t.TempDir(), t.TempDir()))
}
