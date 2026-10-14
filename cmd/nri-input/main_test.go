package main

import (
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

func sessionPod(namespace string) *api.PodSandbox {
	return &api.PodSandbox{
		Name:      "session-pod",
		Namespace: namespace,
		Labels:    map[string]string{v1alpha1types.SessionPodLabel: v1alpha1types.SessionPodLabelValue},
	}
}

type rule struct {
	major  int64
	minor  int64 // -1 for wildcard
	access string
}

func allowRules(adjust *api.ContainerAdjustment) []rule {
	var got []rule
	for _, d := range adjust.GetLinux().GetResources().GetDevices() {
		if !d.Allow || d.Type != "c" {
			continue
		}
		r := rule{major: d.GetMajor().GetValue(), minor: -1, access: d.Access}
		if d.GetMinor() != nil {
			r.minor = d.GetMinor().GetValue()
		}
		got = append(got, r)
	}
	return got
}

func TestAdjustmentWolfGetsUinput(t *testing.T) {
	adjust := (&plugin{}).adjustment(sessionPod("games"), v1alpha1types.WolfContainerName)
	require.NotNil(t, adjust)

	require.Len(t, adjust.GetLinux().GetDevices(), 1)
	dev := adjust.GetLinux().GetDevices()[0]
	require.Equal(t, "/dev/uinput", dev.Path)
	require.Equal(t, "c", dev.Type)
	require.Equal(t, int64(10), dev.Major)
	require.Equal(t, int64(223), dev.Minor)

	require.ElementsMatch(t, []rule{{13, -1, "rwm"}, {241, -1, "rwm"}, {10, 223, "rwm"}}, allowRules(adjust))
}

// An App with /dev/uinput could create keyboards and mice in the host kernel
// (XERK-1433); it only needs to open the nodes wolf-agent plugs in.
func TestAdjustmentAppGetsInputWithoutUinput(t *testing.T) {
	for _, name := range []string{"app", "app-init", "sidecar"} {
		t.Run(name, func(t *testing.T) {
			adjust := (&plugin{}).adjustment(sessionPod("games"), name)
			require.NotNil(t, adjust)
			require.Empty(t, adjust.GetLinux().GetDevices(), "no device nodes")
			require.ElementsMatch(t, []rule{{13, -1, "rwm"}, {241, -1, "rwm"}}, allowRules(adjust))
		})
	}
}

func TestAdjustmentOperatorSidecarsGetNothing(t *testing.T) {
	for _, name := range []string{
		v1alpha1types.WolfAgentContainerName,
		v1alpha1types.PulseAudioContainerName,
		v1alpha1types.InitContainerName,
		v1alpha1types.LoopbackShimContainerName,
	} {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, (&plugin{}).adjustment(sessionPod("games"), name))
		})
	}
}

func TestAdjustmentUntouchedPods(t *testing.T) {
	tests := map[string]struct {
		plugin *plugin
		pod    *api.PodSandbox
	}{
		"no labels": {
			plugin: &plugin{},
			pod:    &api.PodSandbox{Name: "other", Namespace: "games"},
		},
		"other labels only": {
			plugin: &plugin{},
			pod: &api.PodSandbox{Name: "other", Namespace: "games", Labels: map[string]string{
				"app": "direwolf-worker", "direwolf/user": "alice",
			}},
		},
		"wrong label value": {
			plugin: &plugin{},
			pod: &api.PodSandbox{Name: "other", Namespace: "games", Labels: map[string]string{
				v1alpha1types.SessionPodLabel: "false",
			}},
		},
		"labelled pod outside allowed namespaces": {
			plugin: &plugin{namespaces: []string{"games"}},
			pod:    sessionPod("default"),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Nil(t, tc.plugin.adjustment(tc.pod, "app"))
		})
	}
}

func TestAdjustmentAllowedNamespace(t *testing.T) {
	p := &plugin{namespaces: []string{"other", "games"}}
	require.NotNil(t, p.adjustment(sessionPod("games"), "app"))
}

func TestParseNamespaces(t *testing.T) {
	require.Equal(t, []string{"other", "games"}, parseNamespaces(" other, games ,,"))
	require.Empty(t, parseNamespaces(""))
	require.Empty(t, parseNamespaces(" , "))
}

func TestNewPluginScope(t *testing.T) {
	_, err := newPlugin("", false)
	require.Error(t, err, "unscoped plugin must fail closed")
	_, err = newPlugin(" , ", false)
	require.Error(t, err, "blank namespace list must fail closed")
	_, err = newPlugin("games", true)
	require.Error(t, err, "flags are mutually exclusive")

	p, err := newPlugin("games", false)
	require.NoError(t, err)
	require.Equal(t, []string{"games"}, p.namespaces)

	p, err = newPlugin("", true)
	require.NoError(t, err)
	require.Empty(t, p.namespaces)
}
