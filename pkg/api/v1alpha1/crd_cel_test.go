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
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	sigsyaml "sigs.k8s.io/yaml"
)

// The App CRD rejects template metadata the API server would reject on the
// session pod or PVC the operator copies it onto (XERK-1375). Runs the API
// server's own CRD validation (rule compilation and cost budget), schema
// validator and CEL validator over the generated CRD.
func TestAppCRDValidatesEmbeddedMetadata(t *testing.T) {
	validate := crdValidator(t, "apps")
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
		{name: "label prefix 253 long", labels: map[string]any{strings.Repeat("a.", 126) + "a/x": "x"}},
		{name: "label prefix 254 long", labels: map[string]any{strings.Repeat("a.", 126) + "ab/x": "x"}, wantErr: "metadata.labels keys"},
		{name: "two slashes", labels: map[string]any{"a/b/c": "x"}, wantErr: "metadata.labels keys"},
		{name: "empty prefix", labels: map[string]any{"/x": "x"}, wantErr: "metadata.labels keys"},
		{name: "label value", labels: map[string]any{"app": "bad value"}, wantErr: "metadata.labels.app"},
		{name: "label value too long", labels: map[string]any{"app": long + "a"}, wantErr: "metadata.labels.app"},
		{name: "annotation prefix 253 long", annotations: map[string]any{strings.Repeat("a.", 126) + "a/x": "x"}},
		{name: "annotation prefix 254 long", annotations: map[string]any{strings.Repeat("a.", 126) + "ab/x": "x"}, wantErr: "metadata.annotations keys"},
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
			errs := validate(obj)
			switch {
			case tc.wantErr == "" && len(errs) > 0:
				t.Errorf("%s spec.%s: unexpected errors: %v", tc.name, field, errs.ToAggregate())
			case tc.wantErr != "" && (len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.wantErr)):
				t.Errorf("%s spec.%s: errors %v, want one containing %q", tc.name, field, errs.ToAggregate(), tc.wantErr)
			}
		}
	}
}

// crdValidator runs the API server's own CRD validation (rule compilation
// and cost budget) over the generated CRD of the plural resource and returns
// a func applying its schema and CEL validators to an object.
func crdValidator(t *testing.T, plural string) func(obj map[string]any) field.ErrorList {
	t.Helper()
	data, err := os.ReadFile("../../../crds/direwolf.games-on-whales.github.io_" + plural + ".yaml")
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
	return func(obj map[string]any) field.ErrorList {
		errs := schemavalidation.ValidateCustomResource(nil, obj, schemaValidator)
		celErrs, _ := celValidator.Validate(context.Background(), nil, s, obj, nil, celconfig.RuntimeCELCostBudget)
		return append(errs, celErrs...)
	}
}

// spec.gpu.memory must be positive, checked without CEL's quantity library,
// which a 1.28 API server (the chart's floor) lacks (XERK-1521).
func TestAppCRDValidatesGPUMemory(t *testing.T) {
	validate := crdValidator(t, "apps")
	for _, tc := range []struct {
		memory  any
		wantErr bool
	}{
		{memory: "8Gi"}, {memory: "1"}, {memory: int64(1)}, {memory: "+512Mi"}, {memory: "0.5Gi"},
		{memory: ".5Gi"}, {memory: "1e9"}, {memory: "100m"}, {memory: "0001k"},
		{memory: "0", wantErr: true}, {memory: int64(0), wantErr: true}, {memory: "0Gi", wantErr: true},
		{memory: "0.0", wantErr: true}, {memory: "0e9", wantErr: true}, {memory: "-1Gi", wantErr: true},
		{memory: int64(-1), wantErr: true}, {memory: "+0", wantErr: true},
	} {
		obj := map[string]any{
			"apiVersion": "direwolf.games-on-whales.github.io/v1alpha1",
			"kind":       "App",
			"metadata":   map[string]any{"name": "a", "namespace": "ns"},
			"spec": map[string]any{
				"title": "a", "id": int64(1), "isHDRSupported": false, "appAssetWebP": "AA==",
				"gpu": map[string]any{"memory": tc.memory},
			},
		}
		errs := validate(obj)
		switch {
		case !tc.wantErr && len(errs) > 0:
			t.Errorf("memory %v: unexpected errors: %v", tc.memory, errs.ToAggregate())
		case tc.wantErr && (len(errs) != 1 || !strings.Contains(errs[0].Error(), "memory must be positive")):
			t.Errorf("memory %v: errors %v, want only \"memory must be positive\"", tc.memory, errs.ToAggregate())
		}
	}
}

// spec.config.clientIP is bounded: streamClientIP quotes a bad one (twice,
// with ParseAddr's own error) in the StreamStarted condition, and a message
// over 32768 characters would fail every status write of the Session.
func TestSessionCRDBoundsClientIP(t *testing.T) {
	validate := crdValidator(t, "sessions")
	ref := map[string]any{"name": "x"}
	session := func(clientIP, message string) map[string]any {
		obj := map[string]any{
			"apiVersion": "direwolf.games-on-whales.github.io/v1alpha1",
			"kind":       "Session",
			"metadata":   map[string]any{"name": "s", "namespace": "ns"},
			"spec": map[string]any{
				"config": map[string]any{
					"clientIP": clientIP, "aesKey": "k", "aesIV": "i", "surroundAudioFlags": int64(0),
					"videoWidth": int64(1920), "videoHeight": int64(1080), "videoRefreshRate": int64(60),
				},
				"gameReference": ref, "gateway": ref, "pairingReference": ref, "userReference": ref,
			},
		}
		if message != "" {
			obj["status"] = map[string]any{"conditions": []any{map[string]any{
				"type": "StreamStarted", "status": "False", "reason": "StreamRefused", "message": message,
				"lastTransitionTime": "2026-01-01T00:00:00Z",
			}}}
		}
		return obj
	}
	// The longest address moonlight-proxy writes (IPv6, zone stripped), and
	// an IPv4-mapped one at INET6_ADDRSTRLEN-1.
	for _, ip := range []string{"192.0.2.10", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "0000:0000:0000:0000:0000:ffff:255.255.255.255"} {
		if errs := validate(session(ip, "")); len(errs) > 0 {
			t.Errorf("clientIP %q: unexpected errors: %v", ip, errs.ToAggregate())
		}
	}
	if errs := validate(session(strings.Repeat("1", 46), "")); len(errs) != 1 || !strings.Contains(errs[0].Error(), "clientIP") {
		t.Errorf("46-character clientIP: errors %v, want one on clientIP", errs.ToAggregate())
	}
	// What an unbounded clientIP would have led to.
	// (The status also lacks its required ports; only the message matters.)
	if errs := validate(session("192.0.2.10", strings.Repeat("x", 32769))); !strings.Contains(errs.ToAggregate().Error(), "conditions[0].message: Too long") {
		t.Errorf("32769-character condition message: errors %v, want one on message", errs.ToAggregate())
	}
}
