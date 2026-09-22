// Package defaultbranchrename implements the "project-default-branch-rename"
// task: for every project whose default branch isn't the desired name,
// create that branch from the current default (if it doesn't already exist)
// and switch the project's default branch pointer to it.
package defaultbranchrename

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

const Name = "project-default-branch-rename"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task {
	return &Task{Client: c}
}

func Register(c *gitlabclient.Client) {
	task.Register(New(c))
}

func (t *Task) Name() string { return Name }

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	desired := desiredBranch(cfg)

	diffs := make([]diff.Diff, 0, len(scope.Projects))
	for _, p := range scope.Projects {
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace}

		proj, _, err := t.Client.REST.Projects.GetProject(p.ID, nil, gitlab.WithContext(ctx))
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}

		if proj.DefaultBranch == desired {
			diffs = append(diffs, diff.Diff{
				Target:      target,
				Status:      diff.StatusUnchanged,
				Description: fmt.Sprintf("default branch is already %q", desired),
			})
			continue
		}

		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("default branch is %q, want %q", proj.DefaultBranch, desired),
			Detail: map[string]any{
				"current_default_branch": proj.DefaultBranch,
				"desired_default_branch": desired,
			},
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

		current, _ := d.Detail["current_default_branch"].(string)
		if current == "" {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: "diff missing current_default_branch detail"})
			continue
		}

		if _, _, err := t.Client.REST.Branches.GetBranch(d.Target.ID, desired, gitlab.WithContext(ctx)); err != nil {
			if _, _, createErr := t.Client.REST.Branches.CreateBranch(d.Target.ID, &gitlab.CreateBranchOptions{
				Branch: gitlab.Ptr(desired),
				Ref:    gitlab.Ptr(current),
			}, gitlab.WithContext(ctx)); createErr != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("creating branch %q: %v", desired, createErr)})
				continue
			}
		}

		if _, _, err := t.Client.REST.Projects.EditProject(d.Target.ID, &gitlab.EditProjectOptions{
			DefaultBranch: gitlab.Ptr(desired),
		}, gitlab.WithContext(ctx)); err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("setting default branch to %q: %v", desired, err)})
			continue
		}

		results = append(results, diff.Result{
			Target:      d.Target,
			Status:      diff.StatusApplied,
			Description: fmt.Sprintf("created %q from %q and set it as the default branch", desired, current),
		})
	}
	return results, nil
}

func desiredBranch(cfg *config.Config) string {
	if cfg.DefaultBranchRename.BranchName != "" {
		return cfg.DefaultBranchRename.BranchName
	}
	return "master"
}
