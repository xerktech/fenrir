package charts

import (
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"games-on-whales.github.io/direwolf/pkg/controllers"
)

// operatorDeployment is the rendered operator Deployment.
func operatorDeployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	renderedObjects(t)                                      // skips without the manifest
	f, err := os.Open(os.Getenv("DIREWOLF_CHART_MANIFEST")) //nolint:gosec // the manifest CI rendered
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var d appsv1.Deployment
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if d.Kind == "Deployment" && d.Name == "direwolf-operator" {
			return &d
		}
	}
	t.Fatal("no direwolf-operator Deployment rendered")
	return nil
}

// The chart's provisioning values reach the operator (CI renders them set,
// see build.yml's Render chart), in a form its flags accept.
func TestProvisioningValuesReachOperator(t *testing.T) {
	d := operatorDeployment(t)
	env := map[string]string{}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	if env["PROVISION_GROUP"] == "" {
		t.Skip("rendered with provisioning off: render with build.yml's Render chart values")
	}
	for name, want := range map[string]string{
		"PROVISION_GROUP":               "k8x-games",
		"LIBRARY_GROUPS_HEADER":         controllers.DefaultLibraryGroupsHeader,
		"LIBRARY_GROUPS_SEPARATOR":      controllers.DefaultLibraryGroupsSeparator,
		"PROVISION_HOME_STORAGE_CLASS":  "local-path",
		"PROVISION_HOME_SIZE":           "100Gi",
		"PROVISION_GAMES_STORAGE_CLASS": "local-path",
		"PROVISION_GAMES_SIZE":          "1Ti",
	} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", name, got, ok, want)
		}
	}
	spec, err := controllers.ParseProvisionUserTemplate(env["PROVISION_USER_TEMPLATE"])
	if err != nil || spec.Resources == nil || spec.Resources.Limits.Cpu().String() != "8" {
		t.Errorf("PROVISION_USER_TEMPLATE %q = %+v, %v; want the rendered template", env["PROVISION_USER_TEMPLATE"], spec, err)
	}
}

// The operator creates Users and their PVCs.
func TestOperatorMayProvision(t *testing.T) {
	type groupResource struct{ group, resource string }
	need := map[groupResource]bool{{"direwolf.games-on-whales.github.io", "users"}: false, {"", "persistentvolumeclaims"}: false}
	for _, o := range renderedObjects(t) {
		if o.Kind != "Role" || o.Metadata.Name != "direwolf-operator" {
			continue
		}
		for _, r := range o.Rules {
			if !slices.Contains(r.Verbs, "create") && !slices.Contains(r.Verbs, "*") {
				continue
			}
			for key := range need {
				if slices.Contains(r.APIGroups, key.group) &&
					(slices.Contains(r.Resources, key.resource) || slices.Contains(r.Resources, "*")) {
					need[key] = true
				}
			}
		}
	}
	for key, ok := range need {
		if !ok {
			t.Errorf("operator Role cannot create %s", key)
		}
	}
}
