// Package groupdefaultbranch implements the "group-default-branch-setting"
// task: sets the default branch name new projects get when created inside
// each top-level group going forward. It does not touch existing projects'
// default branch — that's handled per project by defaultbranchrename.
package groupdefaultbranch

import (
	"context"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/tfonji/gitlab-post-migration/internal/config"
	"github.com/tfonji/gitlab-post-migration/internal/diff"
	"github.com/tfonji/gitlab-post-migration/internal/discovery"
	"github.com/tfonji/gitlab-post-migration/internal/gitlabclient"
	"github.com/tfonji/gitlab-post-migration/internal/task"
)

const Name = "group-default-branch-setting"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	desired := desiredBranch(cfg)

	diffs := make([]diff.Diff, 0, len(scope.TopLevelGroups))
	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}

		grp, _, err := t.Client.REST.Groups.GetGroup(g.ID, nil, gitlab.WithContext(ctx))
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}

		if grp.DefaultBranch == desired {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("group default branch already %q", desired)})
			continue
		}

		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("group default branch is %q, want %q", grp.DefaultBranch, desired),
		})
	}
	return diffs, nil
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	desired := desiredBranch(cfg)

	results := make([]diff.Result, 0, len(diffs))
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}

		_, _, err := t.Client.REST.Groups.UpdateGroup(d.Target.ID, &gitlab.UpdateGroupOptions{
			DefaultBranch: gitlab.Ptr(desired),
		}, gitlab.WithContext(ctx))
		if err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: err.Error()})
			continue
		}

		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("set group default branch to %q", desired)})
	}
	return results, nil
}

func desiredBranch(cfg *config.Config) string {
	if cfg.GroupDefaultBranch.BranchName != "" {
		return cfg.GroupDefaultBranch.BranchName
	}
	return "master"
}
