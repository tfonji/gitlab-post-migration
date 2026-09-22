// Package mrapprovalpolicy implements the "group-mr-approval-policy" task:
// links a security policy project (GitLab's "merge request approval
// policy" / scan-result-policy feature) to each top-level group. This is a
// GraphQL-only, group-level linkage -- not the REST-exposed boolean MR
// approval settings (allow_author_approval, etc.), which is a different
// GitLab feature entirely.
//
// Query/mutation shapes were verified against GitLab's own
// terraform-provider-gitlab source
// (resource_gitlab_group_security_policy_attachment.go: securityPolicyProjectAssign,
// securityPolicyProject); the mutation's exact argument scalar types were
// then further verified against GitLab's Ruby source
// (ee/app/graphql/mutations/security_policy/assign_security_policy_project.rb):
// fullPath is the String scalar (not ID -- an earlier version of this task
// had it as ID, which is a real type-name mismatch a GraphQL server
// rejects; caught the same class of bug in the complianceframework task's
// mutations, this one just hadn't been exercised against a live instance
// yet), and securityPolicyProjectId is the ProjectID scalar (also not the
// plain ID this task originally used).
package mrapprovalpolicy

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

const Name = "group-mr-approval-policy"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

const currentPolicyProjectQuery = `
query($fullPath: ID!) {
  group(fullPath: $fullPath) {
    securityPolicyProject { id }
  }
}`

type currentPolicyProjectResponse struct {
	Group struct {
		SecurityPolicyProject *struct {
			ID string `json:"id"`
		} `json:"securityPolicyProject"`
	} `json:"group"`
}

const assignPolicyProjectMutation = `
mutation($fullPath: String!, $policyProjectId: ProjectID!) {
  securityPolicyProjectAssign(input: { fullPath: $fullPath, securityPolicyProjectId: $policyProjectId }) {
    errors
  }
}`

type assignPolicyProjectResponse struct {
	SecurityPolicyProjectAssign struct {
		Errors []string `json:"errors"`
	} `json:"securityPolicyProjectAssign"`
}

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	path := cfg.MRPolicy.SecurityPolicyProjectPath
	if path == "" {
		return nil, fmt.Errorf("mr_policy.security_policy_project_path is not set in desired-state config")
	}

	policyProject, _, err := t.Client.REST.Projects.GetProject(path, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("resolving security policy project %q: %w", path, err)
	}
	wantGID := fmt.Sprintf("gid://gitlab/Project/%d", policyProject.ID)

	diffs := make([]diff.Diff, 0, len(scope.TopLevelGroups))
	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}

		var resp currentPolicyProjectResponse
		if err := t.Client.GraphQL(ctx, currentPolicyProjectQuery, map[string]any{"fullPath": g.FullPath}, &resp); err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}

		if resp.Group.SecurityPolicyProject != nil && resp.Group.SecurityPolicyProject.ID == wantGID {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("security policy project already set to %q", path)})
			continue
		}

		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("security policy project not set to %q", path),
			Detail:      map[string]any{"policy_project_gid": wantGID},
		})
	}
	return diffs, nil
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	results := make([]diff.Result, 0, len(diffs))
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}

		gid, _ := d.Detail["policy_project_gid"].(string)
		if gid == "" {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: "diff missing policy_project_gid detail"})
			continue
		}

		var resp assignPolicyProjectResponse
		if err := t.Client.GraphQL(ctx, assignPolicyProjectMutation, map[string]any{
			"fullPath":        d.Target.Path,
			"policyProjectId": gid,
		}, &resp); err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: err.Error()})
			continue
		}
		if len(resp.SecurityPolicyProjectAssign.Errors) > 0 {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("%v", resp.SecurityPolicyProjectAssign.Errors)})
			continue
		}

		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: "linked MR approval policy project"})
	}
	return results, nil
}
