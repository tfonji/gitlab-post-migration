// Package complianceframework implements the "group-compliance-framework"
// task: ensures a compliance framework (name/color/description/pipeline
// config, from desired-state config) exists and matches at each top-level
// group -- creating it if missing, updating it if its attributes drifted --
// then assigns it to every project in that group, without disturbing any
// other frameworks already assigned to a project.
//
// Compliance frameworks aren't in the REST API; every call here is
// GraphQL. The read query and the create/update mutations' shapes were
// verified directly against GitLab's Ruby source
// (ee/app/graphql/mutations/compliance_management/frameworks/{create,update}.rb
// and .../types/compliance_management/compliance_framework_input_type.rb) --
// notably, pipelineConfigurationFullPath lives inside the `params` input
// object, not as a sibling of it (an earlier version of this task had it as
// a sibling and GitLab's GraphQL server rejected it: "InputObject
// 'CreateComplianceFrameworkInput' doesn't accept argument
// 'pipelineConfigurationFullPath'"). Also note: pipelineConfigurationFullPath
// is deprecated as of GitLab 17.4 in favor of pipeline execution policies,
// though it should still function.
//
// projectUpdateComplianceFrameworks (project assignment) is now also
// source-verified, against ee/app/graphql/mutations/projects/update_compliance_frameworks.rb:
// projectId is the ProjectID scalar (not plain ID -- this was also wrong
// originally, alongside the pipelineConfigurationFullPath placement bug
// above), and complianceFrameworkIds takes a list of
// ComplianceManagementFrameworkID. The one remaining unconfirmed detail is
// whether that list's elements are non-null ([ComplianceManagementFrameworkID!]!
// vs [ComplianceManagementFrameworkID]!) -- the Ruby source's `argument`
// declaration didn't make this unambiguous from a doc summary alone. If
// this mutation ever fails with a list-nullability complaint, that's why.
package complianceframework

import (
	"context"
	"fmt"

	"github.com/tfonji/gitlab-post-migration/internal/config"
	"github.com/tfonji/gitlab-post-migration/internal/diff"
	"github.com/tfonji/gitlab-post-migration/internal/discovery"
	"github.com/tfonji/gitlab-post-migration/internal/gitlabclient"
	"github.com/tfonji/gitlab-post-migration/internal/task"
)

const Name = "group-compliance-framework"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

type frameworkNode struct {
	ID                            string `json:"id"`
	Name                          string `json:"name"`
	Description                   string `json:"description"`
	Color                         string `json:"color"`
	PipelineConfigurationFullPath string `json:"pipelineConfigurationFullPath"`
}

const frameworksByGroupQuery = `
query($fullPath: ID!) {
  group(fullPath: $fullPath) {
    complianceFrameworks {
      nodes { id name description color pipelineConfigurationFullPath }
    }
  }
}`

type frameworksByGroupResponse struct {
	Group struct {
		ComplianceFrameworks struct {
			Nodes []frameworkNode `json:"nodes"`
		} `json:"complianceFrameworks"`
	} `json:"group"`
}

const projectFrameworksQuery = `
query($fullPath: ID!) {
  project(fullPath: $fullPath) {
    complianceFrameworks {
      nodes { id name }
    }
  }
}`

type projectFrameworksResponse struct {
	Project struct {
		ComplianceFrameworks struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"complianceFrameworks"`
	} `json:"project"`
}

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	want := cfg.ComplianceFramework
	if want.Name == "" {
		return nil, fmt.Errorf("compliance_framework.name is not set in desired-state config")
	}

	var diffs []diff.Diff

	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}

		var resp frameworksByGroupResponse
		if err := t.Client.GraphQL(ctx, frameworksByGroupQuery, map[string]any{"fullPath": g.FullPath}, &resp); err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}

		var found *frameworkNode
		for i := range resp.Group.ComplianceFrameworks.Nodes {
			if resp.Group.ComplianceFrameworks.Nodes[i].Name == want.Name {
				found = &resp.Group.ComplianceFrameworks.Nodes[i]
				break
			}
		}

		switch {
		case found == nil:
			diffs = append(diffs, diff.Diff{
				Target:      target,
				Status:      diff.StatusDrifted,
				Description: fmt.Sprintf("compliance framework %q does not exist yet", want.Name),
				Detail:      map[string]any{"action": "create"},
			})
		case found.Description != want.Description || found.Color != want.Color || found.PipelineConfigurationFullPath != want.PipelineConfigurationFullPath:
			diffs = append(diffs, diff.Diff{
				Target:      target,
				Status:      diff.StatusDrifted,
				Description: fmt.Sprintf("compliance framework %q exists but its attributes differ", want.Name),
				Detail:      map[string]any{"action": "update", "framework_id": found.ID},
			})
		default:
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("compliance framework %q already matches", want.Name)})
		}
	}

	groupFullPathByID := make(map[int64]string, len(scope.TopLevelGroups))
	for _, g := range scope.TopLevelGroups {
		groupFullPathByID[g.ID] = g.FullPath
	}

	for _, p := range scope.Projects {
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace}

		var resp projectFrameworksResponse
		if err := t.Client.GraphQL(ctx, projectFrameworksQuery, map[string]any{"fullPath": p.PathWithNamespace}, &resp); err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: err.Error()})
			continue
		}

		alreadyAssigned := false
		existingIDs := make([]string, 0, len(resp.Project.ComplianceFrameworks.Nodes))
		for _, f := range resp.Project.ComplianceFrameworks.Nodes {
			if f.Name == want.Name {
				alreadyAssigned = true
			} else {
				existingIDs = append(existingIDs, f.ID)
			}
		}
		if alreadyAssigned {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("compliance framework %q already assigned", want.Name)})
			continue
		}

		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("compliance framework %q not assigned", want.Name),
			Detail: map[string]any{
				"group_full_path":        groupFullPathByID[p.TopLevelGroupID],
				"existing_framework_ids": existingIDs,
			},
		})
	}
	return diffs, nil
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	want := cfg.ComplianceFramework
	results := make([]diff.Result, 0, len(diffs))
	frameworkIDByGroupPath := map[string]string{}

	// Pass 1: ensure the framework exists and matches at every group.
	for _, d := range diffs {
		if d.Target.Kind != diff.TargetGroup {
			continue
		}
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}

		action, _ := d.Detail["action"].(string)
		var id string
		var err error
		if action == "update" {
			existingID, _ := d.Detail["framework_id"].(string)
			id, err = t.updateFramework(ctx, existingID, want)
		} else {
			id, err = t.createFramework(ctx, d.Target.Path, want)
		}
		if err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: err.Error()})
			continue
		}

		frameworkIDByGroupPath[d.Target.Path] = id
		verb := "created"
		if action == "update" {
			verb = "updated"
		}
		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("%s compliance framework %q", verb, want.Name)})
	}

	// Pass 2: assign to every project. A group whose framework diff was
	// Unchanged/not-in-this-run won't be in frameworkIDByGroupPath yet, so
	// look it up live the first time it's needed.
	for _, d := range diffs {
		if d.Target.Kind != diff.TargetProject {
			continue
		}
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}

		groupPath, _ := d.Detail["group_full_path"].(string)
		frameworkID := frameworkIDByGroupPath[groupPath]
		if frameworkID == "" {
			id, err := t.lookupFrameworkID(ctx, groupPath, want.Name)
			if err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: err.Error()})
				continue
			}
			frameworkID = id
			frameworkIDByGroupPath[groupPath] = id
		}

		existingIDs := toStringSlice(d.Detail["existing_framework_ids"])
		ids := append(append([]string{}, existingIDs...), frameworkID)

		if err := t.assignFrameworks(ctx, d.Target.ID, ids); err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: err.Error()})
			continue
		}
		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("assigned compliance framework %q", want.Name)})
	}
	return results, nil
}

const createFrameworkMutation = `
mutation($namespacePath: ID!, $name: String!, $description: String!, $color: String!, $pipelineConfigurationFullPath: String) {
  createComplianceFramework(input: {
    namespacePath: $namespacePath,
    params: {
      name: $name,
      description: $description,
      color: $color,
      pipelineConfigurationFullPath: $pipelineConfigurationFullPath
    }
  }) {
    framework { id }
    errors
  }
}`

type createFrameworkResponse struct {
	CreateComplianceFramework struct {
		Framework *struct {
			ID string `json:"id"`
		} `json:"framework"`
		Errors []string `json:"errors"`
	} `json:"createComplianceFramework"`
}

func (t *Task) createFramework(ctx context.Context, groupFullPath string, want config.ComplianceFramework) (string, error) {
	var resp createFrameworkResponse
	err := t.Client.GraphQL(ctx, createFrameworkMutation, map[string]any{
		"namespacePath":                 groupFullPath,
		"name":                          want.Name,
		"description":                   want.Description,
		"color":                         want.Color,
		"pipelineConfigurationFullPath": want.PipelineConfigurationFullPath,
	}, &resp)
	if err != nil {
		return "", fmt.Errorf("creating compliance framework: %w", err)
	}
	if len(resp.CreateComplianceFramework.Errors) > 0 {
		return "", fmt.Errorf("creating compliance framework: %v", resp.CreateComplianceFramework.Errors)
	}
	if resp.CreateComplianceFramework.Framework == nil {
		return "", fmt.Errorf("creating compliance framework: no framework returned")
	}
	return resp.CreateComplianceFramework.Framework.ID, nil
}

const updateFrameworkMutation = `
mutation($id: ComplianceManagementFrameworkID!, $name: String!, $description: String!, $color: String!, $pipelineConfigurationFullPath: String) {
  updateComplianceFramework(input: {
    id: $id,
    params: {
      name: $name,
      description: $description,
      color: $color,
      pipelineConfigurationFullPath: $pipelineConfigurationFullPath
    }
  }) {
    framework { id }
    errors
  }
}`

type updateFrameworkResponse struct {
	UpdateComplianceFramework struct {
		Framework *struct {
			ID string `json:"id"`
		} `json:"framework"`
		Errors []string `json:"errors"`
	} `json:"updateComplianceFramework"`
}

func (t *Task) updateFramework(ctx context.Context, id string, want config.ComplianceFramework) (string, error) {
	var resp updateFrameworkResponse
	err := t.Client.GraphQL(ctx, updateFrameworkMutation, map[string]any{
		"id":                            id,
		"name":                          want.Name,
		"description":                   want.Description,
		"color":                         want.Color,
		"pipelineConfigurationFullPath": want.PipelineConfigurationFullPath,
	}, &resp)
	if err != nil {
		return "", fmt.Errorf("updating compliance framework: %w", err)
	}
	if len(resp.UpdateComplianceFramework.Errors) > 0 {
		return "", fmt.Errorf("updating compliance framework: %v", resp.UpdateComplianceFramework.Errors)
	}
	return id, nil
}

func (t *Task) lookupFrameworkID(ctx context.Context, groupFullPath, name string) (string, error) {
	var resp frameworksByGroupResponse
	if err := t.Client.GraphQL(ctx, frameworksByGroupQuery, map[string]any{"fullPath": groupFullPath}, &resp); err != nil {
		return "", fmt.Errorf("looking up compliance framework %q at %s: %w", name, groupFullPath, err)
	}
	for _, f := range resp.Group.ComplianceFrameworks.Nodes {
		if f.Name == name {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("compliance framework %q not found at %s", name, groupFullPath)
}

const assignFrameworksMutation = `
mutation($projectId: ProjectID!, $frameworkIds: [ComplianceManagementFrameworkID!]!) {
  projectUpdateComplianceFrameworks(input: {
    projectId: $projectId,
    complianceFrameworkIds: $frameworkIds
  }) {
    errors
  }
}`

type assignFrameworksResponse struct {
	ProjectUpdateComplianceFrameworks struct {
		Errors []string `json:"errors"`
	} `json:"projectUpdateComplianceFrameworks"`
}

func (t *Task) assignFrameworks(ctx context.Context, projectID int64, frameworkIDs []string) error {
	projectGID := fmt.Sprintf("gid://gitlab/Project/%d", projectID)
	var resp assignFrameworksResponse
	if err := t.Client.GraphQL(ctx, assignFrameworksMutation, map[string]any{
		"projectId":    projectGID,
		"frameworkIds": frameworkIDs,
	}, &resp); err != nil {
		return err
	}
	if len(resp.ProjectUpdateComplianceFrameworks.Errors) > 0 {
		return fmt.Errorf("%v", resp.ProjectUpdateComplianceFrameworks.Errors)
	}
	return nil
}

func toStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
