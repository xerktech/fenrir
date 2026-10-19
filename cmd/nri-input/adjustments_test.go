package main

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
)

var updateAdjustments = flag.Bool("update", false, "rewrite testdata/adjustments from the plugin")

// adjustmentCases are the containers whose adjustments the containerd226
// module replays through containerd 2.2.6's NRI (v0.11.0). It reads them from
// testdata/adjustments, as it cannot import this package main.
var adjustmentCases = map[string]*api.Container{
	"wolf":           {Name: v1alpha1types.WolfContainerName},
	"app":            {Name: "app"},
	"app-unconfined": {Name: "app", Linux: &api.LinuxContainer{SeccompProfile: &api.SecurityProfile{ProfileType: api.SecurityProfile_UNCONFINED}}},
}

// The adjustments in testdata/adjustments must be what the plugin sends
// today: a stale one would let the containerd226 guard pass on old output.
// After changing an adjustment: go test ./cmd/nri-input/ -run TestAdjustmentsMatchTestdata -update,
// then cd cmd/nri-input/containerd226 && go test ./...
func TestAdjustmentsMatchTestdata(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("testdata is amd64's (seccompArches), as on talos04")
	}
	dir := filepath.Join("testdata", "adjustments")
	if *updateAdjustments {
		require.NoError(t, os.RemoveAll(dir))
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	require.NoError(t, err)
	if !*updateAdjustments {
		require.Len(t, files, len(adjustmentCases), "one file per case in %s", dir)
	}
	p := &plugin{}
	for name, ctr := range adjustmentCases {
		t.Run(name, func(t *testing.T) {
			adjust, _, err := p.CreateContainer(t.Context(), sessionPod("games"), ctr)
			require.NoError(t, err)
			require.NotNil(t, adjust)
			path := filepath.Join(dir, name+".json")
			if *updateAdjustments {
				b, mErr := protojson.MarshalOptions{Multiline: true}.Marshal(adjust)
				require.NoError(t, mErr)
				require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o600))
				return
			}
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			want := &api.ContainerAdjustment{}
			require.NoError(t, protojson.Unmarshal(b, want))
			require.True(t, proto.Equal(want, adjust),
				"%s is stale: rerun with -update (see the test's comment)\nwant %v\ngot  %v", path, want, adjust)
		})
	}
}
