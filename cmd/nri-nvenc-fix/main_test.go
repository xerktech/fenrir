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

func TestAdjustmentAnnotatedPod(t *testing.T) {
	adjust := (&plugin{hostDir: "/var/lib/nvenc-fix"}).adjustment(annotatedPod("true"), &api.Container{Name: "wolf"})
	require.NotNil(t, adjust)

	got := map[string]*api.Mount{}
	for _, m := range adjust.GetMounts() {
		got[m.Destination] = m
	}
	require.Len(t, got, 2)

	shim := got["/usr/lib/nvenc-fix/libnvenc_fix.so"]
	require.NotNil(t, shim)
	require.Equal(t, "/var/lib/nvenc-fix/libnvenc_fix.so", shim.Source)

	preload := got["/etc/ld.so.preload"]
	require.NotNil(t, preload)
	require.Equal(t, "/var/lib/nvenc-fix/ld.so.preload", preload.Source)

	for _, m := range got {
		require.Equal(t, "bind", m.Type)
		require.Contains(t, m.Options, "ro")
	}
}

func TestAdjustmentUntouched(t *testing.T) {
	tests := map[string]struct {
		pod *api.PodSandbox
		ctr *api.Container
	}{
		"no annotations": {
			pod: &api.PodSandbox{Name: "other", Namespace: "streaming"},
			ctr: &api.Container{Name: "app"},
		},
		"annotation not true": {
			pod: annotatedPod("false"),
			ctr: &api.Container{Name: "app"},
		},
		"container mounts its own ld.so.preload": {
			pod: annotatedPod("true"),
			ctr: &api.Container{Name: "app", Mounts: []*api.Mount{{Destination: "/etc/ld.so.preload"}}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, (&plugin{hostDir: "/var/lib/nvenc-fix"}).adjustment(tc.pod, tc.ctr))
		})
	}
}

func TestInstall(t *testing.T) {
	src := filepath.Join(t.TempDir(), "libnvenc_fix.so")
	require.NoError(t, os.WriteFile(src, []byte("shim-v1"), 0o600))
	dir := filepath.Join(t.TempDir(), "nvenc-fix")

	require.NoError(t, install(src, dir))

	shim, err := os.ReadFile(filepath.Join(dir, "libnvenc_fix.so"))
	require.NoError(t, err)
	require.Equal(t, "shim-v1", string(shim))
	info, err := os.Stat(filepath.Join(dir, "libnvenc_fix.so"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	preload, err := os.ReadFile(filepath.Join(dir, "ld.so.preload"))
	require.NoError(t, err)
	require.Equal(t, "/usr/lib/nvenc-fix/libnvenc_fix.so\n", string(preload))

	// A restart with a new image replaces the installed shim.
	require.NoError(t, os.WriteFile(src, []byte("shim-v2"), 0o600))
	require.NoError(t, install(src, dir))
	shim, err = os.ReadFile(filepath.Join(dir, "libnvenc_fix.so"))
	require.NoError(t, err)
	require.Equal(t, "shim-v2", string(shim))
}

func TestInstallMissingShim(t *testing.T) {
	require.Error(t, install(filepath.Join(t.TempDir(), "absent.so"), t.TempDir()))
}
