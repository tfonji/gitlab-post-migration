// Package defaultbranchrename implements the "project-default-branch-rename"
// task: for every project whose default branch isn't the desired name,
// create that branch from the current default (if it doesn't already
// exist), protect it (creating or updating the protection to match desired
// push/merge access), and switch the project's default branch pointer to
// it.
package defaultbranchrename

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace}

		proj, _, err := t.Client.REST.Projects.GetProject(p.ID, nil, gitlab.WithContext(ctx))
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}
		needsSwitch := proj.DefaultBranch != desired

		// branchAction records what Apply should do about the branch
		// itself: "create" (desired doesn't exist yet), "reuse" (it
		// already exists AND its tip matches the current default's tip,
		// e.g. a retry after a partial prior run), or "" when no switch
		// is needed at all. A pre-existing desired branch whose tip does
		// NOT match current is a conflict this task refuses to resolve
		// silently -- see the StatusFailed branch below.
		branchAction := ""
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
					diffs = append(diffs, diff.Diff{
						Target: target,
						Status: diff.StatusFailed,
						Description: fmt.Sprintf(
							"%q already exists (tip %s) but differs from %q's tip (%s) -- won't switch the default branch to it automatically; delete the stale %q or fast-forward it first",
							desired, shortSHA(existing), proj.DefaultBranch, shortSHA(currentBranch), desired,
						),
					})
					continue
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

		var actions []string

		// Trust Plan's branch_action rather than re-deriving it here: Apply
		// only acts on what was reviewed, and re-checking independently is
		// exactly what let a pre-existing, diverged branch get silently
		// reused and mislabeled "created" before this fix.
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

// branchTip returns a branch's tip commit SHA, or "" if the branch has no
// commit info for some reason (shouldn't happen for a real branch, but
// avoids a nil-pointer panic rather than assuming).
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

// matchesProtection checks only push/merge access level -- allow_force_push
// and code_owner_approval_required aren't configurable by this task yet, so
// they're not part of the drift check.
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

// isNotFound checks for go-gitlab's sentinel gitlab.ErrNotFound, which
// CheckResponse (gitlab.go) returns for EVERY HTTP 404 -- never a
// *gitlab.ErrorResponse with status 404 (see the same fix in
// protectedenvironment and gitlab-ci-bootstrap's bootstrap.go).
func isNotFound(err error) bool {
	return errors.Is(err, gitlab.ErrNotFound)
}
