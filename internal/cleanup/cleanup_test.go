package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
)

type fakeGroup struct {
	policyProject string // full path of the linked project, "" if none
	frameworks    []fakeFramework
}

type fakeFramework struct {
	ID, Name  string
	IsDefault bool
}

// fakeGraphQL answers the cleanup queries/mutations from in-memory group
// state, recording the order of mutations and enforcing GitLab's rule that a
// default framework can't be destroyed.
type fakeGraphQL struct {
	groups    map[string]*fakeGroup
	calls     []string
	failQuery string // substring of a query/mutation to fail with a transport error
}

func (f *fakeGraphQL) GraphQL(_ context.Context, query string, vars map[string]any, out any) error {
	if f.failQuery != "" && strings.Contains(query, f.failQuery) {
		return errors.New("boom")
	}
	var data any
	switch {
	case strings.Contains(query, "securityPolicyProject {"):
		g := f.groups[vars["fullPath"].(string)]
		var project any
		if g.policyProject != "" {
			project = map[string]any{"id": "gid://gitlab/Project/9", "fullPath": g.policyProject}
		}
		data = map[string]any{"group": map[string]any{"securityPolicyProject": project}}
	case strings.Contains(query, "securityPolicyProjectUnassign"):
		f.calls = append(f.calls, "unassign "+vars["fullPath"].(string))
		f.groups[vars["fullPath"].(string)].policyProject = ""
		data = map[string]any{"securityPolicyProjectUnassign": map[string]any{"errors": []string{}}}
	case strings.Contains(query, "complianceFrameworks {"):
		g := f.groups[vars["fullPath"].(string)]
		nodes := []map[string]any{}
		for _, fw := range g.frameworks {
			nodes = append(nodes, map[string]any{"id": fw.ID, "name": fw.Name, "default": fw.IsDefault})
		}
		data = map[string]any{"group": map[string]any{"complianceFrameworks": map[string]any{"nodes": nodes}}}
	case strings.Contains(query, "updateComplianceFramework"):
		id := vars["id"].(string)
		f.calls = append(f.calls, "unset-default "+id)
		for _, g := range f.groups {
			for i := range g.frameworks {
				if g.frameworks[i].ID == id {
					g.frameworks[i].IsDefault = false
				}
			}
		}
		data = map[string]any{"updateComplianceFramework": map[string]any{"errors": []string{}}}
	case strings.Contains(query, "destroyComplianceFramework"):
		id := vars["id"].(string)
		f.calls = append(f.calls, "destroy "+id)
		errs := []string{}
		for _, g := range f.groups {
			for i, fw := range g.frameworks {
				if fw.ID != id {
					continue
				}
				if fw.IsDefault {
					errs = append(errs, "Cannot delete the default framework")
				} else {
					g.frameworks = append(g.frameworks[:i], g.frameworks[i+1:]...)
				}
				break
			}
		}
		data = map[string]any{"destroyComplianceFramework": map[string]any{"errors": errs}}
	default:
		return errors.New("unexpected query: " + query)
	}
	raw, _ := json.Marshal(data)
	return json.Unmarshal(raw, out)
}

func runCleanup(t *testing.T, f *fakeGraphQL, groupPaths ...string) []diff.Result {
	t.Helper()
	var scope discovery.Scope
	for i, p := range groupPaths {
		scope.TopLevelGroups = append(scope.TopLevelGroups, discovery.Group{ID: int64(i + 1), FullPath: p})
	}
	cfg := &config.Config{ComplianceFramework: config.ComplianceFramework{Name: "security-compliant"}}
	results, err := Run(context.Background(), f, scope, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func TestRunUnlinksPolicyAndRemovesDefaultFrameworkInOrder(t *testing.T) {
	f := &fakeGraphQL{groups: map[string]*fakeGroup{
		"acme": {
			policyProject: "sec/policies",
			frameworks: []fakeFramework{
				{ID: "gid://f/1", Name: "security-compliant", IsDefault: true},
				{ID: "gid://f/2", Name: "other"},
			},
		},
	}}
	results := runCleanup(t, f, "acme")

	if got := strings.Join(f.calls, ", "); got != "unassign acme, unset-default gid://f/1, destroy gid://f/1" {
		t.Errorf("mutation order = %q", got)
	}
	if len(results) != 2 || results[0].Status != diff.StatusApplied || results[1].Status != diff.StatusApplied {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(results[0].Description, "sec/policies") {
		t.Errorf("policy result = %q", results[0].Description)
	}
	if !strings.Contains(results[1].Description, "unset default and removed") {
		t.Errorf("framework result = %q", results[1].Description)
	}
	if fw := f.groups["acme"].frameworks; len(fw) != 1 || fw[0].Name != "other" {
		t.Errorf("only the configured framework should be removed, left: %+v", fw)
	}
}

func TestRunNonDefaultFrameworkIsDestroyedDirectly(t *testing.T) {
	f := &fakeGraphQL{groups: map[string]*fakeGroup{
		"acme": {frameworks: []fakeFramework{{ID: "gid://f/1", Name: "security-compliant"}}},
	}}
	results := runCleanup(t, f, "acme")

	if got := strings.Join(f.calls, ", "); got != "destroy gid://f/1" {
		t.Errorf("mutations = %q", got)
	}
	if results[0].Status != diff.StatusUnchanged || results[1].Description != `removed compliance framework "security-compliant"` {
		t.Errorf("results = %+v", results)
	}
}

func TestRunNothingToCleanIsUnchanged(t *testing.T) {
	f := &fakeGraphQL{groups: map[string]*fakeGroup{"acme": {}}}
	results := runCleanup(t, f, "acme")

	if len(f.calls) != 0 {
		t.Errorf("no mutations expected, got %v", f.calls)
	}
	for _, r := range results {
		if r.Status != diff.StatusUnchanged {
			t.Errorf("result = %+v", r)
		}
	}
}

func TestRunFailureOnOneGroupDoesNotStopTheOthers(t *testing.T) {
	f := &fakeGraphQL{
		groups: map[string]*fakeGroup{
			"a": {policyProject: "sec/policies"},
			"b": {policyProject: "sec/policies"},
		},
		failQuery: "securityPolicyProjectUnassign",
	}
	results := runCleanup(t, f, "a", "b")

	failed := 0
	for _, r := range results {
		if r.Status == diff.StatusFailed {
			failed++
			if r.Error == "" {
				t.Errorf("failed result has no error: %+v", r)
			}
		}
	}
	if failed != 2 || len(results) != 4 {
		t.Errorf("want both groups' unlink to fail yet all 4 checks reported; got %d failed of %d: %+v", failed, len(results), results)
	}
}

func TestRunRequiresFrameworkName(t *testing.T) {
	if _, err := Run(context.Background(), &fakeGraphQL{}, discovery.Scope{}, &config.Config{}); err == nil {
		t.Fatal("expected an error when compliance_framework.name is unset")
	}
}
