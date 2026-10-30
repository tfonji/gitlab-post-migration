package complianceframework

import (
	"context"
	"testing"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
)

func TestDisabledSkipsEveryGroupWithoutCallingGitLab(t *testing.T) {
	off := false
	cfg := &config.Config{ComplianceFramework: config.ComplianceFramework{Enabled: &off}}
	scope := discovery.Scope{
		TopLevelGroups: []discovery.Group{{ID: 1, FullPath: "acme"}, {ID: 2, FullPath: "globex"}},
		Projects:       []discovery.Project{{ID: 10, PathWithNamespace: "acme/app", TopLevelGroupID: 1}},
	}

	task := New(nil)
	diffs, err := task.Plan(context.Background(), scope, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 {
		t.Fatalf("want one skipped diff per group, got %+v", diffs)
	}
	for _, d := range diffs {
		if d.Status != diff.StatusSkipped || d.Description != DisabledReason {
			t.Errorf("diff = %+v", d)
		}
	}

	results, err := task.Apply(context.Background(), diffs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Status != diff.StatusSkipped {
			t.Errorf("result = %+v", r)
		}
	}
}
