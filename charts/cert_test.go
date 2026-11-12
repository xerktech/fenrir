package charts

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// moonlight-proxy's serving certificate is pinned by every paired client:
// Moonlight pins the whole certificate, VR headsets its public key. So it
// must never be renewed (long lifetime, still before 2106) and a reissue
// must keep its key (XERK-2818).
func TestServingCertNeverRotates(t *testing.T) {
	renderedObjects(t)                                      // skips without the manifest
	f, err := os.Open(os.Getenv("DIREWOLF_CHART_MANIFEST")) //nolint:gosec // the manifest CI rendered
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var o unstructured.Unstructured
		if err := dec.Decode(&o.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if o.GetKind() != "Certificate" || o.GetName() != "direwolf-cert" {
			continue
		}
		raw, _, _ := unstructured.NestedString(o.Object, "spec", "duration")
		d, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("spec.duration %q: %v", raw, err)
		}
		const year = 365 * 24 * time.Hour
		if d < 40*year || time.Now().Add(d).Year() >= 2106 {
			t.Errorf("spec.duration %s: want decades, expiring before 2106", raw)
		}
		if p, _, _ := unstructured.NestedString(o.Object, "spec", "privateKey", "rotationPolicy"); p != "Never" {
			t.Errorf("spec.privateKey.rotationPolicy %q, want Never", p)
		}
		return
	}
	t.Fatal("no direwolf-cert Certificate rendered")
}
