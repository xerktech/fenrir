package v1alpha1

import (
	"context"
	"os"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	sigsyaml "sigs.k8s.io/yaml"
)

// The App CRD rejects template metadata the API server would reject on the
// session pod or PVC the operator copies it onto (XERK-1375). Runs the API
// server's own CRD validation (rule compilation and cost budget), schema
// validator and CEL validator over the generated CRD.
func TestAppCRDValidatesEmbeddedMetadata(t *testing.T) {
	data, err := os.ReadFile("../../../crds/direwolf.games-on-whales.github.io_apps.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v1crd apiextensionsv1.CustomResourceDefinition
	if err = sigsyaml.Unmarshal(data, &v1crd); err != nil {
		t.Fatal(err)
	}
	var crd apiextensions.CustomResourceDefinition
	if err = apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1crd, &crd, nil); err != nil {
		t.Fatal(err)
	}
	crd.Status.StoredVersions = []string{"v1alpha1"}
	if errs := validation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) > 0 {
		t.Fatalf("the API server would refuse the CRD: %v", errs.ToAggregate())
	}
	// Conversion hoists a lone version's schema to spec.validation.
	props := crd.Spec.Validation.OpenAPIV3Schema
	s, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(s, true, celconfig.PerCallLimit)
	schemaValidator, _, err := schemavalidation.NewSchemaValidator(props)
	if err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("a", 63)
	for _, tc := range []struct {
		name        string
		labels      map[string]any
		annotations map[string]any
		wantErr     string
	}{
		{name: "valid", labels: map[string]any{"app": "steam", "example.com/a_b.c-d": "", "x": long}, annotations: map[string]any{"Example.COM/Note": "any value at all!"}},
		{name: "no metadata"},
		{name: "label key with space", labels: map[string]any{"bad key!": "x"}, wantErr: "metadata.labels keys"},
		{name: "label name too long", labels: map[string]any{long + "a": "x"}, wantErr: "metadata.labels keys"},
		{name: "label prefix too long", labels: map[string]any{strings.Repeat("a.", 127) + "a/x": "x"}, wantErr: "metadata.labels keys"},
		{name: "two slashes", labels: map[string]any{"a/b/c": "x"}, wantErr: "metadata.labels keys"},
		{name: "empty prefix", labels: map[string]any{"/x": "x"}, wantErr: "metadata.labels keys"},
		{name: "label value", labels: map[string]any{"app": "bad value"}, wantErr: "metadata.labels.app"},
		{name: "label value too long", labels: map[string]any{"app": long + "a"}, wantErr: "metadata.labels.app"},
		{name: "annotation key", annotations: map[string]any{"bad key!": "x"}, wantErr: "metadata.annotations keys"},
	} {
		for _, field := range []string{"template", "volumeClaimTemplate"} {
			meta := map[string]any{}
			if tc.labels != nil {
				meta["labels"] = tc.labels
			}
			if tc.annotations != nil {
				meta["annotations"] = tc.annotations
			}
			tmpl := map[string]any{"spec": map[string]any{}}
			if field == "template" {
				tmpl["spec"] = map[string]any{"containers": []any{map[string]any{"name": "game"}}}
			}
			if tc.name != "no metadata" {
				tmpl["metadata"] = meta
			}
			obj := map[string]any{
				"apiVersion": "direwolf.games-on-whales.github.io/v1alpha1",
				"kind":       "App",
				"metadata":   map[string]any{"name": "a", "namespace": "ns"},
				"spec": map[string]any{
					"title": "a", "id": int64(1), "isHDRSupported": false, "appAssetWebP": "AA==",
					field: tmpl,
				},
			}
			errs := schemavalidation.ValidateCustomResource(nil, obj, schemaValidator)
			celErrs, _ := celValidator.Validate(context.Background(), nil, s, obj, nil, celconfig.RuntimeCELCostBudget)
			errs = append(errs, celErrs...)
			switch {
			case tc.wantErr == "" && len(errs) > 0:
				t.Errorf("%s spec.%s: unexpected errors: %v", tc.name, field, errs.ToAggregate())
			case tc.wantErr != "" && (len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.wantErr)):
				t.Errorf("%s spec.%s: errors %v, want one containing %q", tc.name, field, errs.ToAggregate(), tc.wantErr)
			}
		}
	}
}
