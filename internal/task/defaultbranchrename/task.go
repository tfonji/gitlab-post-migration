// Package defaultbranchrename makes master the default branch: created if missing, used as is if present, protected.
package defaultbranchrename

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

// RequiresGroupMembership: GitLab refuses branch protection (403) for an admin who is not a project member (gitlab-org/gitlab#428273).
func (t *Task) RequiresGroupMembership() bool { return true }

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	desired := desiredBranch(cfg)
	pushLevel, err := accessLevelFromString(protectPushAccessLevel(cfg))
	if err != nil {
		return nil, err
	}
	mergeLevel, err := accessLevelFromString(protectMergeAccessLevel(cfg))
	if err != nil {
		return nil, err
	}

	diffs := make([]diff.Diff, 0, len(scope.Projects))
	for _, p := range scope.Projects {
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace, TopLevelGroupID: p.TopLevelGroupID}

		proj, _, err := t.Client.REST.Projects.GetProject(p.ID, nil, gitlab.WithContext(ctx))
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}
		needsSwitch := proj.DefaultBranch != desired

		// branchAction: "create", "reuse" (same tip), "adopt" (different tip, used as is), or "" when no switch is needed.
		branchAction := ""
		branchNote := ""
		if needsSwitch {
			existing, _, err := t.Client.REST.Branches.GetBranch(p.ID, desired, gitlab.WithContext(ctx))
			switch {
			case err != nil && !isNotFound(err):
				diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("checking branch %q: %v", desired, err)})
				continue
			case err != nil:
				branchAction = "create"
			default:
				currentBranch, _, err := t.Client.REST.Branches.GetBranch(p.ID, proj.DefaultBranch, gitlab.WithContext(ctx))
				if err != nil {
					diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("checking current default branch %q: %v", proj.DefaultBranch, err)})
					continue
				}
				if branchTip(existing) == branchTip(currentBranch) {
					branchAction = "reuse"
				} else {
					branchAction = "adopt"
					branchNote = fmt.Sprintf(
						"%q already exists (tip %s) but differs from %q (tip %s); it becomes the default as it is, and commits only on %q stay on that branch",
						desired, shortSHA(existing), proj.DefaultBranch, shortSHA(currentBranch), proj.DefaultBranch,
					)
				}
			}
		}

		protectionAction := ""
		protected, _, err := t.Client.REST.ProtectedBranches.GetProtectedBranch(p.ID, desired, gitlab.WithContext(ctx))
		switch {
		case err != nil && !isNotFound(err):
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("checking protection for %q: %v", desired, err)})
			continue
		case err != nil:
			protectionAction = "create"
		case !matchesProtection(protected, pushLevel, mergeLevel):
			protectionAction = "update"
		}

		if !needsSwitch && protectionAction == "" {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("default branch already %q and protected as desired", desired)})
			continue
		}

		var descParts []string
		if needsSwitch {
			switch branchAction {
			case "create":
				descParts = append(descParts, fmt.Sprintf("default branch is %q, want %q (will create %q)", proj.DefaultBranch, desired, desired))
			case "reuse":
				descParts = append(descParts, fmt.Sprintf("default branch is %q, want %q (%q already exists and matches)", proj.DefaultBranch, desired, desired))
			case "adopt":
				descParts = append(descParts, fmt.Sprintf("default branch is %q, want %q (%s)", proj.DefaultBranch, desired, branchNote))
			}
		}
		if protectionAction == "create" {
			descParts = append(descParts, fmt.Sprintf("%q is not protected yet", desired))
		} else if protectionAction == "update" {
			descParts = append(descParts, fmt.Sprintf("%q protection differs from desired policy", desired))
		}

		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: strings.Join(descParts, "; "),
			Detail: map[string]any{
				"current_default_branch": proj.DefaultBranch,
				"desired_default_branch": desired,
				"needs_default_switch":   needsSwitch,
				"branch_action":          branchAction,
				"branch_note":            branchNote,
				"protection_action":      protectionAction,
			},
		})
	}
	return diffs, nil
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	desired := desiredBranch(cfg)
	pushLevel, err := accessLevelFromString(protectPushAccessLevel(cfg))
	if err != nil {
		return nil, err
	}
	mergeLevel, err := accessLevelFromString(protectMergeAccessLevel(cfg))
	if err != nil {
		return nil, err
	}

	results := make([]diff.Result, 0, len(diffs))
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}

		needsSwitch, _ := d.Detail["needs_default_switch"].(bool)
		branchAction, _ := d.Detail["branch_action"].(string)
		protectionAction, _ := d.Detail["protection_action"].(string)
		current, _ := d.Detail["current_default_branch"].(string)
		branchNote, _ := d.Detail["branch_note"].(string)

		var actions []string

		// Trust Plan's branch_action instead of re-deriving it: apply acts only on what was reviewed.
		if needsSwitch {
			if current == "" {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: "diff missing current_default_branch detail"})
				continue
			}
			switch branchAction {
			case "create":
				if _, _, err := t.Client.REST.Branches.CreateBranch(d.Target.ID, &gitlab.CreateBranchOptions{
					Branch: gitlab.Ptr(desired),
					Ref:    gitlab.Ptr(current),
				}, gitlab.WithContext(ctx)); err != nil {
					results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("creating branch %q: %v", desired, err)})
					continue
				}
				actions = append(actions, fmt.Sprintf("created %q from %q", desired, current))
			case "reuse":
				actions = append(actions, fmt.Sprintf("%q already existed and matched %q, reused it", desired, current))
			case "adopt":
				actions = append(actions, fmt.Sprintf("%q already existed and differs from %q, used it as is (%s)", desired, current, branchNote))
			default:
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("diff missing/invalid branch_action detail: %q", branchAction)})
				continue
			}
		}

		switch protectionAction {
		case "create":
			if _, _, err := t.Client.REST.ProtectedBranches.ProtectRepositoryBranches(d.Target.ID, &gitlab.ProtectRepositoryBranchesOptions{
				Name:             gitlab.Ptr(desired),
				PushAccessLevel:  gitlab.Ptr(pushLevel),
				MergeAccessLevel: gitlab.Ptr(mergeLevel),
				AllowForcePush:   gitlab.Ptr(false),
			}, gitlab.WithContext(ctx)); err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("protecting %q: %v", desired, err)})
				continue
			}
			actions = append(actions, fmt.Sprintf("protected %q", desired))
		case "update":
			if _, _, err := t.Client.REST.ProtectedBranches.UpdateProtectedBranch(d.Target.ID, desired, &gitlab.UpdateProtectedBranchOptions{
				AllowedToPush:  &[]*gitlab.BranchPermissionOptions{{AccessLevel: gitlab.Ptr(pushLevel)}},
				AllowedToMerge: &[]*gitlab.BranchPermissionOptions{{AccessLevel: gitlab.Ptr(mergeLevel)}},
				AllowForcePush: gitlab.Ptr(false),
			}, gitlab.WithContext(ctx)); err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("updating protection for %q: %v", desired, err)})
				continue
			}
			actions = append(actions, fmt.Sprintf("updated protection for %q", desired))
		}

		if needsSwitch {
			if _, _, err := t.Client.REST.Projects.EditProject(d.Target.ID, &gitlab.EditProjectOptions{
				DefaultBranch: gitlab.Ptr(desired),
			}, gitlab.WithContext(ctx)); err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("setting default branch to %q: %v", desired, err)})
				continue
			}
			actions = append(actions, "set as default branch")
		}

		results = append(results, diff.Result{
			Target:      d.Target,
			Status:      diff.StatusApplied,
			Description: strings.Join(actions, "; "),
		})
	}
	return results, nil
}

// branchTip returns a branch's tip SHA, or "" when it has no commit info.
func branchTip(b *gitlab.Branch) string {
	if b == nil || b.Commit == nil {
		return ""
	}
	return b.Commit.ID
}

func shortSHA(b *gitlab.Branch) string {
	sha := branchTip(b)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// matchesProtection checks only push/merge access level; force push and code owner approval are not configurable here.
func matchesProtection(p *gitlab.ProtectedBranch, wantPush, wantMerge gitlab.AccessLevelValue) bool {
	pushOK := false
	for _, lvl := range p.PushAccessLevels {
		if lvl.AccessLevel == wantPush {
			pushOK = true
		}
	}
	mergeOK := false
	for _, lvl := range p.MergeAccessLevels {
		if lvl.AccessLevel == wantMerge {
			mergeOK = true
		}
	}
	return pushOK && mergeOK
}

func desiredBranch(cfg *config.Config) string {
	if cfg.DefaultBranchRename.BranchName != "" {
		return cfg.DefaultBranchRename.BranchName
	}
	return "master"
}

func protectPushAccessLevel(cfg *config.Config) string {
	if cfg.DefaultBranchRename.ProtectPushAccessLevel != "" {
		return cfg.DefaultBranchRename.ProtectPushAccessLevel
	}
	return "maintainer"
}

func protectMergeAccessLevel(cfg *config.Config) string {
	if cfg.DefaultBranchRename.ProtectMergeAccessLevel != "" {
		return cfg.DefaultBranchRename.ProtectMergeAccessLevel
	}
	return "maintainer"
}

func accessLevelFromString(level string) (gitlab.AccessLevelValue, error) {
	switch level {
	case "none":
		return gitlab.NoPermissions, nil
	case "developer":
		return gitlab.DeveloperPermissions, nil
	case "maintainer":
		return gitlab.MaintainerPermissions, nil
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
