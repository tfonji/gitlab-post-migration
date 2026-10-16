// Package protectedenvironment implements the "group-protected-environment"
// task: ensures each group-level protected environment from config (default
// just "production"; e.g. production and staging) exists with deploy access
// and approval both restricted to the configured access level (default
// Maintainer).
package protectedenvironment

import (
	"context"
	"errors"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
	"gitlab-post-migration/internal/task"
)

const Name = "group-protected-environment"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

// desired is one environment's settings, resolved from config with defaults.
type desired struct {
	name      string
	level     gitlab.AccessLevelValue
	approvals int64
}

func desiredEnvironments(cfg *config.Config) ([]desired, error) {
	var out []desired
	for _, e := range cfg.ProtectedEnvironmentList() {
		level, err := accessLevelFromString(orDefault(e.DeployAccessLevel, "maintainer"))
		if err != nil {
			return nil, err
		}
		approvals := e.RequiredApprovalCount
		if approvals <= 0 {
			approvals = 1
		}
		out = append(out, desired{name: orDefault(e.EnvironmentName, "production"), level: level, approvals: approvals})
	}
	return out, nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	envs, err := desiredEnvironments(cfg)
	if err != nil {
		return nil, err
	}

	diffs := make([]diff.Diff, 0, len(scope.TopLevelGroups)*len(envs))
	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}

		for _, env := range envs {
			detail := map[string]any{"environment": env.name}
			current, _, err := t.Client.REST.GroupProtectedEnvironments.GetGroupProtectedEnvironment(g.ID, env.name, gitlab.WithContext(ctx))
			if err != nil {
				// A 404 just means the environment isn't protected yet — that's drift, not a failure.
				if isNotFound(err) {
					diffs = append(diffs, diff.Diff{
						Target:      target,
						Status:      diff.StatusDrifted,
						Description: fmt.Sprintf("environment %q is not protected yet", env.name),
						Detail:      detail,
					})
					continue
				}
				diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("checking environment %q: %v", env.name, err), Detail: detail})
				continue
			}

			if matches(current, env.level, env.approvals) {
				diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("environment %q already protected as desired", env.name), Detail: detail})
				continue
			}

			diffs = append(diffs, diff.Diff{
				Target:      target,
				Status:      diff.StatusDrifted,
				Description: fmt.Sprintf("environment %q protection differs from desired policy", env.name),
				Detail:      detail,
			})
		}
	}
	return diffs, nil
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	envs, err := desiredEnvironments(cfg)
	if err != nil {
		return nil, err
	}
	byName := map[string]desired{}
	for _, e := range envs {
		byName[e.name] = e
	}

	results := make([]diff.Result, 0, len(diffs))
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}
		name, _ := d.Detail["environment"].(string)
		env, ok := byName[name]
		if !ok {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("diff names environment %q, which is not in the config", name)})
			continue
		}

		// ProtectGroupEnvironment is also the update path: protecting an
		// already-protected environment with new settings replaces them.
		//
		// The top-level RequiredApprovalCount field is deprecated by GitLab
		// (a real apply run hit: "422 Parameter 'required_approval_count' is
		// deprecated and shouldn't be used", linking
		// https://gitlab.com/groups/gitlab-org/-/epics/9662) -- the required
		// count now belongs solely on each ApprovalRules entry, which this
		// already sets.
		_, _, err := t.Client.REST.GroupProtectedEnvironments.ProtectGroupEnvironment(d.Target.ID, &gitlab.ProtectGroupEnvironmentOptions{
			Name: gitlab.Ptr(env.name),
			DeployAccessLevels: &[]*gitlab.GroupEnvironmentAccessOptions{
				{AccessLevel: gitlab.Ptr(env.level)},
			},
			ApprovalRules: &[]*gitlab.GroupEnvironmentApprovalRuleOptions{
				{AccessLevel: gitlab.Ptr(env.level), RequiredApprovalCount: gitlab.Ptr(env.approvals)},
			},
		}, gitlab.WithContext(ctx))
		if err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("protecting environment %q: %v", env.name, err)})
			continue
		}

		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("protected environment %q", env.name)})
	}
	return results, nil
}

// matches deliberately ignores current.RequiredApprovalCount (the
// deprecated top-level field, see the comment in Apply) and checks only
// each approval rule's own RequiredApprovalCount, which is what this task
// actually writes.
func matches(current *gitlab.GroupProtectedEnvironment, wantAccess gitlab.AccessLevelValue, wantApprovals int64) bool {
	deployOK := false
	for _, lvl := range current.DeployAccessLevels {
		if lvl.AccessLevel == wantAccess {
			deployOK = true
		}
	}
	approvalOK := false
	for _, rule := range current.ApprovalRules {
		if rule.AccessLevel == wantAccess && rule.RequiredApprovalCount == wantApprovals {
			approvalOK = true
		}
	}
	return deployOK && approvalOK
}

func accessLevelFromString(level string) (gitlab.AccessLevelValue, error) {
	switch level {
	case "maintainer":
		return gitlab.MaintainerPermissions, nil
	case "developer":
		return gitlab.DeveloperPermissions, nil
	case "owner":
		return gitlab.OwnerPermissions, nil
	default:
		return 0, fmt.Errorf("unsupported access level %q", level)
	}
}

// isNotFound checks for go-gitlab's sentinel gitlab.ErrNotFound, which
// CheckResponse (gitlab.go) returns for EVERY HTTP 404 -- never a
// *gitlab.ErrorResponse with status 404. An earlier version of this
// function checked for *ErrorResponse instead, which never matched, so
// every "not protected yet" 404 (the normal, expected state before this
// task has ever run) was misreported as a hard failure instead of drift.
func isNotFound(err error) bool {
	return errors.Is(err, gitlab.ErrNotFound)
}
