// Package protectedenvironment implements the "group-protected-environment"
// task: ensures a group-level protected environment (name from config,
// default "production") exists with deploy access and approval both
// restricted to the configured access level (default Maintainer).
package protectedenvironment

import (
	"context"
	"errors"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/tfonji/gitlab-post-migration/internal/config"
	"github.com/tfonji/gitlab-post-migration/internal/diff"
	"github.com/tfonji/gitlab-post-migration/internal/discovery"
	"github.com/tfonji/gitlab-post-migration/internal/gitlabclient"
	"github.com/tfonji/gitlab-post-migration/internal/task"
)

const Name = "group-protected-environment"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	envName := envName(cfg)
	accessLevel, err := accessLevelFromString(deployAccessLevel(cfg))
	if err != nil {
		return nil, err
	}
	requiredApprovals := requiredApprovalCount(cfg)

	diffs := make([]diff.Diff, 0, len(scope.TopLevelGroups))
	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}

		current, _, err := t.Client.REST.GroupProtectedEnvironments.GetGroupProtectedEnvironment(g.ID, envName, gitlab.WithContext(ctx))
		if err != nil {
			// A 404 just means the environment isn't protected yet — that's drift, not a failure.
			if isNotFound(err) {
				diffs = append(diffs, diff.Diff{
					Target:      target,
					Status:      diff.StatusDrifted,
					Description: fmt.Sprintf("environment %q is not protected yet", envName),
				})
				continue
			}
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("checking environment %q: %v", envName, err)})
			continue
		}

		if matches(current, accessLevel, requiredApprovals) {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("environment %q already protected as desired", envName)})
			continue
		}

		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("environment %q protection differs from desired policy", envName),
		})
	}
	return diffs, nil
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	envName := envName(cfg)
	accessLevel, err := accessLevelFromString(deployAccessLevel(cfg))
	if err != nil {
		return nil, err
	}
	requiredApprovals := requiredApprovalCount(cfg)

	results := make([]diff.Result, 0, len(diffs))
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
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
			Name: gitlab.Ptr(envName),
			DeployAccessLevels: &[]*gitlab.GroupEnvironmentAccessOptions{
				{AccessLevel: gitlab.Ptr(accessLevel)},
			},
			ApprovalRules: &[]*gitlab.GroupEnvironmentApprovalRuleOptions{
				{AccessLevel: gitlab.Ptr(accessLevel), RequiredApprovalCount: gitlab.Ptr(requiredApprovals)},
			},
		}, gitlab.WithContext(ctx))
		if err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("protecting environment %q: %v", envName, err)})
			continue
		}

		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("protected environment %q", envName)})
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

func envName(cfg *config.Config) string {
	if cfg.ProtectedEnvironment.EnvironmentName != "" {
		return cfg.ProtectedEnvironment.EnvironmentName
	}
	return "production"
}

func deployAccessLevel(cfg *config.Config) string {
	if cfg.ProtectedEnvironment.DeployAccessLevel != "" {
		return cfg.ProtectedEnvironment.DeployAccessLevel
	}
	return "maintainer"
}

func requiredApprovalCount(cfg *config.Config) int64 {
	if cfg.ProtectedEnvironment.RequiredApprovalCount > 0 {
		return cfg.ProtectedEnvironment.RequiredApprovalCount
	}
	return 1
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
