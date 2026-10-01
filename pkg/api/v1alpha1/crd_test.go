package v1alpha1

import (
	"os"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"
)

// The API server prunes every field a structural schema does not declare. An
// App's spec.template.metadata used to be a bare {type: object}, so the labels
// and annotations set there were dropped on admission and never reached the
// session pod. Guard the generated CRD (crd:generateEmbeddedObjectMeta=true).
type schemaProps struct {
	Properties map[string]schemaProps `json:"properties"`
}

func TestAppCRDKeepsTemplateMetadata(t *testing.T) {
	data, err := os.ReadFile("../../../crds/direwolf.games-on-whales.github.io_apps.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema schemaProps `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := sigsyaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, field := range []string{"template", "volumeClaimTemplate"} {
		meta := spec.Properties[field].Properties["metadata"]
		for _, key := range []string{"labels", "annotations"} {
			if _, ok := meta.Properties[key]; !ok {
				t.Errorf("spec.%s.metadata declares no %s: the API server prunes them", field, key)
			}
		}
	}
}
