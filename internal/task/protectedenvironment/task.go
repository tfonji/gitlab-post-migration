// Package protectedenvironment protects group-level environments (production, staging, other), optionally approved by a shared group.
package protectedenvironment

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

type desired struct {
	name      string
	level     gitlab.AccessLevelValue // who may deploy
	approvals int64

	// approverGroup, if set, approves; ensureShared requires the top-level group be shared with it at shareLevel or better.
	approverGroup int64
	ensureShared  bool
	shareLevel    gitlab.AccessLevelValue
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
		d := desired{name: orDefault(e.EnvironmentName, "production"), level: level, approvals: approvals, approverGroup: e.ApproverGroupID}
		if d.approverGroup != 0 {
			d.ensureShared = e.EnsureApproverGroupShared == nil || *e.EnsureApproverGroupShared
			if d.shareLevel, err = accessLevelFromString(orDefault(e.ApproverGroupAccessLevel, "reporter")); err != nil {
				return nil, err
			}
		}
		out = append(out, d)
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
			diffs = append(diffs, t.planEnvironment(ctx, g, target, env))
		}
	}
	return diffs, nil
}

func (t *Task) planEnvironment(ctx context.Context, g discovery.Group, target diff.Target, env desired) diff.Diff {
	detail := map[string]any{"environment": env.name}
	fail := func(format string, args ...any) diff.Diff {
		return diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf(format, args...), Detail: detail}
	}

	// 1. Is the approver group shared with this top-level group?
	var parts []string
	if env.approverGroup != 0 && env.ensureShared {
		level, shared, err := t.sharedLevel(ctx, g.ID, env.approverGroup)
		switch {
		case err != nil:
			return fail("checking whether group %d is shared with this group: %v", env.approverGroup, err)
		case !shared:
			detail["share"] = true
			parts = append(parts, fmt.Sprintf("group %d is not shared with this group yet (will share it as %s)", env.approverGroup, accessLevelName(env.shareLevel)))
		case level < env.shareLevel:
			// GitLab cannot change a group share's level: remove it and share again.
			detail["share"] = true
			detail["previous_share_level"] = int64(level)
			parts = append(parts, fmt.Sprintf("group %d is shared with this group as %s, below the %s needed to approve (will re-share it as %s)",
				env.approverGroup, accessLevelName(level), accessLevelName(env.shareLevel), accessLevelName(env.shareLevel)))
		}
	}

	// 2. Is the environment protected as desired?
	current, _, err := t.Client.REST.GroupProtectedEnvironments.GetGroupProtectedEnvironment(g.ID, env.name, gitlab.WithContext(ctx))
	switch {
	case err != nil && !isNotFound(err):
		return fail("checking environment %q: %v", env.name, err)
	case err != nil:
		detail["protect"] = true
		parts = append(parts, fmt.Sprintf("environment %q is not protected yet", env.name))
	case !matches(current, env):
		detail["protect"] = true
		parts = append(parts, fmt.Sprintf("environment %q protection differs from desired policy", env.name))
	}

	if len(parts) == 0 {
		return diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("environment %q already protected as desired", env.name), Detail: detail}
	}
	return diff.Diff{Target: target, Status: diff.StatusDrifted, Description: strings.Join(parts, "; "), Detail: detail}
}

// sharedLevel reports the level the group is shared at, and whether it is shared.
func (t *Task) sharedLevel(ctx context.Context, groupID, sharedGroupID int64) (level gitlab.AccessLevelValue, shared bool, err error) {
	g, _, err := t.Client.REST.Groups.GetGroup(groupID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return 0, false, err
	}
	for _, s := range g.SharedWithGroups {
		if s.GroupID == sharedGroupID {
			return gitlab.AccessLevelValue(s.GroupAccessLevel), true, nil
		}
	}
	return 0, false, nil
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
		share, _ := d.Detail["share"].(bool)
		protect, _ := d.Detail["protect"].(bool)
		previous, reshare := d.Detail["previous_share_level"].(float64) // JSON round trip: a number
		if !reshare {
			if v, ok := d.Detail["previous_share_level"].(int64); ok {
				previous, reshare = float64(v), true
			}
		}

		var actions []string

		// Share first: the approval rule is refused for a group without access.
		if share {
			did, err := t.share(ctx, d.Target.ID, env, gitlab.AccessLevelValue(previous), reshare)
			if err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed,
					Error: fmt.Sprintf("sharing group %d with this group as %s: %v", env.approverGroup, accessLevelName(env.shareLevel), err)})
				continue
			}
			if did {
				verb := "shared"
				if reshare {
					verb = "re-shared"
				}
				actions = append(actions, fmt.Sprintf("%s group %d as %s", verb, env.approverGroup, accessLevelName(env.shareLevel)))
			}
		}

		if protect {
			// ProtectGroupEnvironment also updates; required_approval_count goes on the rule, the top-level field is deprecated.
			rule := &gitlab.GroupEnvironmentApprovalRuleOptions{AccessLevel: gitlab.Ptr(env.level), RequiredApprovalCount: gitlab.Ptr(env.approvals)}
			if env.approverGroup != 0 {
				rule = &gitlab.GroupEnvironmentApprovalRuleOptions{GroupID: gitlab.Ptr(env.approverGroup), RequiredApprovalCount: gitlab.Ptr(env.approvals)}
			}
			_, _, err := t.Client.REST.GroupProtectedEnvironments.ProtectGroupEnvironment(d.Target.ID, &gitlab.ProtectGroupEnvironmentOptions{
				Name:               gitlab.Ptr(env.name),
				DeployAccessLevels: &[]*gitlab.GroupEnvironmentAccessOptions{{AccessLevel: gitlab.Ptr(env.level)}},
				ApprovalRules:      &[]*gitlab.GroupEnvironmentApprovalRuleOptions{rule},
			}, gitlab.WithContext(ctx))
			if err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("protecting environment %q: %v", env.name, err)})
				continue
			}
			actions = append(actions, fmt.Sprintf("protected environment %q", env.name))
		}

		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: strings.Join(actions, "; ")})
	}
	return results, nil
}

// share shares the approver group at env.shareLevel; a lower existing share is removed and redone, and restored if that fails.
func (t *Task) share(ctx context.Context, groupID int64, env desired, previous gitlab.AccessLevelValue, reshare bool) (did bool, err error) {
	level, shared, err := t.sharedLevel(ctx, groupID, env.approverGroup)
	if err != nil {
		return false, err
	}
	if shared && level >= env.shareLevel {
		return false, nil
	}
	if shared {
		previous = level
		if _, err := t.Client.REST.Groups.UnshareGroupFromGroup(groupID, env.approverGroup, gitlab.WithContext(ctx)); err != nil {
			return false, fmt.Errorf("removing the existing %s share: %w", accessLevelName(level), err)
		}
	}
	_, _, err = t.Client.REST.Groups.ShareGroupWithGroup(groupID, &gitlab.ShareGroupWithGroupOptions{
		GroupID:     gitlab.Ptr(env.approverGroup),
		GroupAccess: gitlab.Ptr(env.shareLevel),
	}, gitlab.WithContext(ctx))
	if err != nil {
		if shared {
			if _, _, restoreErr := t.Client.REST.Groups.ShareGroupWithGroup(groupID, &gitlab.ShareGroupWithGroupOptions{
				GroupID:     gitlab.Ptr(env.approverGroup),
				GroupAccess: gitlab.Ptr(previous),
			}, gitlab.WithContext(ctx)); restoreErr != nil {
				return false, fmt.Errorf("%w -- and restoring the previous %s share failed too (%v); share group %d with this group by hand",
					err, accessLevelName(previous), restoreErr, env.approverGroup)
			}
			return false, fmt.Errorf("%w (the previous %s share was restored)", err, accessLevelName(previous))
		}
		return false, err
	}
	return true, nil
}

// matches compares each approval rule's own count, not the deprecated top-level field.
func matches(current *gitlab.GroupProtectedEnvironment, env desired) bool {
	deployOK := false
	for _, lvl := range current.DeployAccessLevels {
		if lvl.AccessLevel == env.level {
			deployOK = true
		}
	}
	approvalOK := false
	for _, rule := range current.ApprovalRules {
		if rule.RequiredApprovalCount != env.approvals {
			continue
		}
		if env.approverGroup != 0 {
			approvalOK = approvalOK || rule.GroupID == env.approverGroup
		} else {
			approvalOK = approvalOK || rule.AccessLevel == env.level
		}
	}
	return deployOK && approvalOK
}

func accessLevelName(l gitlab.AccessLevelValue) string {
	switch l {
	case gitlab.GuestPermissions:
		return "Guest"
	case gitlab.ReporterPermissions:
		return "Reporter"
	case gitlab.DeveloperPermissions:
		return "Developer"
	case gitlab.MaintainerPermissions:
		return "Maintainer"
	case gitlab.OwnerPermissions:
		return "Owner"
	}
	return fmt.Sprintf("access level %d", l)
}

func accessLevelFromString(level string) (gitlab.AccessLevelValue, error) {
	switch level {
	case "guest":
		return gitlab.GuestPermissions, nil
	case "reporter":
		return gitlab.ReporterPermissions, nil
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

// isNotFound matches gitlab.ErrNotFound, which every HTTP 404 returns.
func isNotFound(err error) bool {
	return errors.Is(err, gitlab.ErrNotFound)
}
