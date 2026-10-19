// Package charts holds checks on the rendered Helm chart. They read the
// manifest CI's verify_chart job renders (helm template), named by
// DIREWOLF_CHART_MANIFEST, and skip without it.
package charts

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// proxyServiceAccount is moonlight-proxy's ServiceAccount.
const proxyServiceAccount = "moonlight-proxy"

// proxyGroups are the groups every ServiceAccount, moonlight-proxy's
// included, is in: a binding to one grants it too. Its namespace's
// system:serviceaccounts:<ns> group is matched by prefix.
var proxyGroups = []string{"system:serviceaccounts", "system:authenticated"}

// readOnlyVerbs are the verbs moonlight-proxy may hold on every pod.
var readOnlyVerbs = []string{"get", "list", "watch"}

// transferVerbs hand over other identities' or roles' permissions, pods
// included, so moonlight-proxy may hold none of them on anything.
var transferVerbs = []string{"impersonate", "bind", "escalate"}

// manifestObject is the subset of any rendered object the checks read: a
// Role's rules and a binding's subjects and roleRef.
type manifestObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Rules           []rbacv1.PolicyRule     `json:"rules"`
	AggregationRule *rbacv1.AggregationRule `json:"aggregationRule"`
	Subjects        []rbacv1.Subject        `json:"subjects"`
	RoleRef         rbacv1.RoleRef          `json:"roleRef"`
}

func renderedObjects(t *testing.T) []manifestObject {
	t.Helper()
	path := os.Getenv("DIREWOLF_CHART_MANIFEST")
	if path == "" {
		t.Skip("DIREWOLF_CHART_MANIFEST unset: run on `helm template` output (CI verify_chart does)")
	}
	f, err := os.Open(path) //nolint:gosec // the manifest CI rendered
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var objs []manifestObject
	dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		var o manifestObject
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		// Fail closed: the checks read top-level objects only, and a List's
		// items would be applied unchecked.
		if strings.HasSuffix(o.Kind, "List") {
			t.Fatalf("%s holds a %s: its items are not checked", path, o.Kind)
		}
		if o.Kind != "" {
			objs = append(objs, o)
		}
	}
	if len(objs) == 0 {
		t.Fatalf("%s holds no objects", path)
	}
	return objs
}

// moonlight-proxy is internet-facing: beyond reading pods, it may only delete
// the Library pod, by name. A pod rule without resourceNames would let it
// delete (or patch) every pod in the namespace, and one on a subresource
// (pods/exec, pods/ephemeralcontainers) run code in any of them (XERK-1867).
// What can't be checked fails closed: impersonate, bind and escalate (or "*"),
// which pass on other permissions, aggregated roles, and List-wrapped objects.
func TestMoonlightProxyPodWritesAreNamed(t *testing.T) {
	objs := renderedObjects(t)
	// The roles bound to moonlight-proxy's ServiceAccount, whatever they're named.
	bound := map[string]bool{}
	for _, o := range objs {
		if o.Kind != "RoleBinding" && o.Kind != "ClusterRoleBinding" {
			continue
		}
		for _, s := range o.Subjects {
			if bindsProxy(s) {
				bound[o.RoleRef.Kind+"/"+o.RoleRef.Name] = true
			}
		}
	}
	if len(bound) == 0 {
		t.Fatalf("no binding grants %s anything: is the manifest the chart's?", proxyServiceAccount)
	}
	found := 0
	for _, o := range objs {
		if (o.Kind != "Role" && o.Kind != "ClusterRole") || !bound[o.Kind+"/"+o.Metadata.Name] {
			continue
		}
		found++
		// Its rules come from whatever ClusterRoles match the selector, at
		// runtime: none of them are checked here.
		if o.AggregationRule != nil {
			t.Errorf("%s %s bound to %s aggregates other roles: its rules can't be checked", o.Kind, o.Metadata.Name, proxyServiceAccount)
		}
		for i := range o.Rules {
			r := &o.Rules[i]
			// "*" includes impersonate, bind and escalate.
			for _, v := range r.Verbs {
				if v == "*" || slices.Contains(transferVerbs, v) {
					t.Errorf("%s %s grants %q, which can hand moonlight-proxy other permissions: %+v", o.Kind, o.Metadata.Name, v, r)
				}
			}
			pods, subresource := podAccess(r)
			if !pods || len(r.ResourceNames) > 0 {
				continue
			}
			for _, v := range r.Verbs {
				if subresource || !slices.Contains(readOnlyVerbs, v) {
					t.Errorf("%s %s grants %q on pods without resourceNames: %+v", o.Kind, o.Metadata.Name, v, r)
				}
			}
		}
	}
	if found != len(bound) {
		t.Errorf("found %d of the %d roles bound to %s: %v", found, len(bound), proxyServiceAccount, bound)
	}
}

// bindsProxy reports whether a binding subject includes moonlight-proxy's
// ServiceAccount.
func bindsProxy(s rbacv1.Subject) bool {
	switch s.Kind {
	case rbacv1.ServiceAccountKind:
		return s.Name == proxyServiceAccount
	case rbacv1.UserKind:
		// The ServiceAccount's username: system:serviceaccount:<ns>:<name>.
		return strings.HasPrefix(s.Name, "system:serviceaccount:") && strings.HasSuffix(s.Name, ":"+proxyServiceAccount)
	case rbacv1.GroupKind:
		return slices.Contains(proxyGroups, s.Name) || strings.HasPrefix(s.Name, "system:serviceaccounts:")
	}
	return false
}

// podAccess reports whether r applies to core pods, and whether it reaches a
// pod subresource (pods/x, or */x as the authorizer matches it): there even
// get counts, as get on pods/exec or pods/attach opens a session on some
// apiservers.
func podAccess(r *rbacv1.PolicyRule) (pods, subresource bool) {
	if !slices.Contains(r.APIGroups, "") && !slices.Contains(r.APIGroups, "*") {
		return false, false
	}
	for _, res := range r.Resources {
		switch {
		case res == "pods":
			pods = true
		// "*" matches every subresource too.
		case res == "*" || strings.HasPrefix(res, "pods/") || strings.HasPrefix(res, "*/"):
			pods, subresource = true, true
		}
	}
	return pods, subresource
}
