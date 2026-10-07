// Package cleanup clears the group-level state that makes later apply jobs
// fail -- a linked security policy project, and the configured compliance
// framework -- so the apply tasks can set both up again from a clean slate.
//
// Unlike a task, cleanup acts immediately: it runs as its own pipeline job
// BEFORE every plan job, so each plan reads the already-cleaned state and
// what a human later approves is exactly what apply runs.
//
// Mutation shapes were verified against GitLab's Ruby source:
//   - securityPolicyProjectUnassign: ee/app/graphql/mutations/security_policy/
//     unassign_security_policy_project.rb (fullPath: String).
//   - destroyComplianceFramework: .../mutations/compliance_management/
//     frameworks/destroy.rb (id: ComplianceManagementFrameworkID!); its service
//     refuses to delete the group's default framework ("Cannot delete the
//     default framework"), hence the unset-default step first.
//   - updateComplianceFramework with params.default=false unsets the default:
//     .../compliance_management/frameworks/update_service.rb.
//
// None of these has been exercised against a live instance yet.
package cleanup

import (
	"context"
	"fmt"

	"github.com/tfonji/gitlab-post-migration/internal/config"
	"github.com/tfonji/gitlab-post-migration/internal/diff"
	"github.com/tfonji/gitlab-post-migration/internal/discovery"
)

// Name is the task name cleanup results are reported under.
const Name = "cleanup"

// GraphQLClient is the one call cleanup needs; *gitlabclient.Client has it.
type GraphQLClient interface {
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
}

const policyProjectQuery = `
query($fullPath: ID!) {
  group(fullPath: $fullPath) {
    securityPolicyProject { id fullPath }
  }
}`

type policyProjectResponse struct {
	Group struct {
		SecurityPolicyProject *struct {
			ID       string `json:"id"`
			FullPath string `json:"fullPath"`
		} `json:"securityPolicyProject"`
	} `json:"group"`
}

const unassignPolicyProjectMutation = `
mutation($fullPath: String!) {
  securityPolicyProjectUnassign(input: { fullPath: $fullPath }) {
    errors
  }
}`

type unassignPolicyProjectResponse struct {
	SecurityPolicyProjectUnassign struct {
		Errors []string `json:"errors"`
	} `json:"securityPolicyProjectUnassign"`
}

const frameworksQuery = `
query($fullPath: ID!) {
  group(fullPath: $fullPath) {
    complianceFrameworks {
      nodes { id name default }
    }
  }
}`

type frameworksResponse struct {
	Group struct {
		ComplianceFrameworks struct {
			Nodes []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Default bool   `json:"default"`
			} `json:"nodes"`
		} `json:"complianceFrameworks"`
	} `json:"group"`
}

const unsetDefaultFrameworkMutation = `
mutation($id: ComplianceManagementFrameworkID!) {
  updateComplianceFramework(input: { id: $id, params: { default: false } }) {
    errors
  }
}`

type updateFrameworkResponse struct {
	UpdateComplianceFramework struct {
		Errors []string `json:"errors"`
	} `json:"updateComplianceFramework"`
}

const destroyFrameworkMutation = `
mutation($id: ComplianceManagementFrameworkID!) {
  destroyComplianceFramework(input: { id: $id }) {
    errors
  }
}`

type destroyFrameworkResponse struct {
	DestroyComplianceFramework struct {
		Errors []string `json:"errors"`
	} `json:"destroyComplianceFramework"`
}

// Run cleans every top-level group in scope and returns one result per
// check: Applied when something was removed, Unchanged when there was
// nothing to remove, Failed when a call errored. A failure on one group
// doesn't stop the others.
func Run(ctx context.Context, c GraphQLClient, scope discovery.Scope, cfg *config.Config) ([]diff.Result, error) {
	frameworkName := cfg.ComplianceFramework.Name
	if frameworkName == "" {
		return nil, fmt.Errorf("compliance_framework.name is not set in desired-state config")
	}

	var results []diff.Result
	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}
		results = append(results, unlinkPolicyProject(ctx, c, target))
		results = append(results, removeFramework(ctx, c, target, frameworkName))
	}
	return results, nil
}

func unlinkPolicyProject(ctx context.Context, c GraphQLClient, target diff.Target) diff.Result {
	fail := func(err error) diff.Result {
		return diff.Result{Target: target, Status: diff.StatusFailed, Description: "unlink security policy project", Error: err.Error()}
	}

	var current policyProjectResponse
	if err := c.GraphQL(ctx, policyProjectQuery, map[string]any{"fullPath": target.Path}, &current); err != nil {
		return fail(err)
	}
	linked := current.Group.SecurityPolicyProject
	if linked == nil {
		return diff.Result{Target: target, Status: diff.StatusUnchanged, Description: "no security policy project linked"}
	}

	var resp unassignPolicyProjectResponse
	if err := c.GraphQL(ctx, unassignPolicyProjectMutation, map[string]any{"fullPath": target.Path}, &resp); err != nil {
		return fail(err)
	}
	if errs := resp.SecurityPolicyProjectUnassign.Errors; len(errs) > 0 {
		return fail(fmt.Errorf("%v", errs))
	}
	return diff.Result{Target: target, Status: diff.StatusApplied, Description: fmt.Sprintf("unlinked security policy project %q", linked.FullPath)}
}

func removeFramework(ctx context.Context, c GraphQLClient, target diff.Target, name string) diff.Result {
	desc := fmt.Sprintf("remove compliance framework %q", name)
	fail := func(err error) diff.Result {
		return diff.Result{Target: target, Status: diff.StatusFailed, Description: desc, Error: err.Error()}
	}

	var current frameworksResponse
	if err := c.GraphQL(ctx, frameworksQuery, map[string]any{"fullPath": target.Path}, &current); err != nil {
		return fail(err)
	}
	var id string
	var isDefault bool
	for _, f := range current.Group.ComplianceFrameworks.Nodes {
		if f.Name == name {
			id, isDefault = f.ID, f.Default
			break
		}
	}
	if id == "" {
		return diff.Result{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("compliance framework %q not set", name)}
	}

	done := fmt.Sprintf("removed compliance framework %q", name)
	if isDefault {
		var update updateFrameworkResponse
		if err := c.GraphQL(ctx, unsetDefaultFrameworkMutation, map[string]any{"id": id}, &update); err != nil {
			return fail(fmt.Errorf("unsetting it as the group default: %w", err))
		}
		if errs := update.UpdateComplianceFramework.Errors; len(errs) > 0 {
			return fail(fmt.Errorf("unsetting it as the group default: %v", errs))
		}
		done = fmt.Sprintf("unset default and removed compliance framework %q", name)
	}

	var destroy destroyFrameworkResponse
	if err := c.GraphQL(ctx, destroyFrameworkMutation, map[string]any{"id": id}, &destroy); err != nil {
		return fail(err)
	}
	if errs := destroy.DestroyComplianceFramework.Errors; len(errs) > 0 {
		return fail(fmt.Errorf("%v", errs))
	}
	return diff.Result{Target: target, Status: diff.StatusApplied, Description: done}
}
