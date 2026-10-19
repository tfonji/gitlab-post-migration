// Package config loads the desired-state baseline that tasks reconcile
// projects/groups against.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the desired-state baseline, identical across every top-level
// group processed in a run (see design discussion: settings are uniform,
// not per-group).
type Config struct {
	MRPolicy            MRPolicy            `yaml:"mr_policy"`
	ComplianceFramework ComplianceFramework `yaml:"compliance_framework"`
	// ProtectedEnvironments lists every group-level protected environment (a
	// deployment tier: production, staging, ...) to enforce. The single
	// ProtectedEnvironment key still works and is used when the list is empty.
	ProtectedEnvironments []ProtectedEnvironment `yaml:"protected_environments"`
	ProtectedEnvironment  ProtectedEnvironment   `yaml:"protected_environment"`
	GroupDefaultBranch    GroupDefaultBranch     `yaml:"group_default_branch"`
	DefaultBranchRename   DefaultBranchRename    `yaml:"default_branch_rename"`

	ProjectMigrationBadge ProjectMigrationBadge `yaml:"project_migration_badge"`
}

// MRPolicy identifies the security policy project (GitLab's "merge request
// approval policy" feature) to link to each top-level group. This is
// distinct from the group-level boolean MR approval settings (e.g. "allow
// author approval") -- those are a separate, REST-exposed feature this
// task does not touch.
type MRPolicy struct {
	SecurityPolicyProjectPath string `yaml:"security_policy_project_path"`
}

// ComplianceFramework is created (if missing) or reconciled (if its
// attributes drift) at each top-level group, then assigned to every
// project in that group.
type ComplianceFramework struct {
	Name                          string `yaml:"name"`
	Color                         string `yaml:"color"`
	Description                   string `yaml:"description"`
	PipelineConfigurationFullPath string `yaml:"pipeline_configuration_full_path"` // "<file>.yml@<group>/<project>"
}

// ProtectedEnvironment describes one group-level protected environment to
// enforce, e.g. "production" or "staging" (GitLab keys group-level protected
// environments by deployment tier), with deploy and approval restricted to the
// given access level.
type ProtectedEnvironment struct {
	EnvironmentName       string `yaml:"environment_name"`
	DeployAccessLevel     string `yaml:"deploy_access_level"`   // "maintainer"
	ApproverAccessLevel   string `yaml:"approver_access_level"` // "maintainer"
	RequiredApprovalCount int64  `yaml:"required_approval_count"`

	// ApproverGroupID, when set, makes that group the approver (instead of a
	// role): the approval rule names the group. A group-level rule only works
	// for a group that has access to the top-level group, so with
	// EnsureApproverGroupShared (the default when ApproverGroupID is set) the
	// task first checks the top-level group is shared with it and shares it at
	// ApproverGroupAccessLevel (default "reporter") if not.
	ApproverGroupID           int64  `yaml:"approver_group_id"`
	EnsureApproverGroupShared *bool  `yaml:"ensure_approver_group_shared"`
	ApproverGroupAccessLevel  string `yaml:"approver_group_access_level"` // "reporter"
}

// GroupDefaultBranch is the group setting governing the default branch name
// for NEW projects created in the group going forward.
type GroupDefaultBranch struct {
	BranchName string `yaml:"branch_name"` // "master"
}

// DefaultBranchRename is the desired default branch name for EXISTING
// migrated projects; the task creates this branch from the project's
// current default (if missing), protects it, and switches the project's
// default branch pointer to it.
type DefaultBranchRename struct {
	BranchName              string `yaml:"branch_name"`                // "master"
	ProtectPushAccessLevel  string `yaml:"protect_push_access_level"`  // "maintainer"
	ProtectMergeAccessLevel string `yaml:"protect_merge_access_level"` // "maintainer"
}

// ProtectedEnvironmentList returns the environments to enforce: the
// protected_environments list, else the legacy single protected_environment,
// else "production" with the task's defaults.
func (c *Config) ProtectedEnvironmentList() []ProtectedEnvironment {
	if len(c.ProtectedEnvironments) > 0 {
		return c.ProtectedEnvironments
	}
	return []ProtectedEnvironment{c.ProtectedEnvironment}
}

// ProjectMigrationBadge describes the "AZDO Migrated <date>" badge added to
// every migrated project. The badge picture is an SVG kept in a repository
// that every user can read (internal visibility), at
// <instance>/<ImageProject>/-/raw/<ImageRef>/<ImageDir>/azdo-migrated-<date>.svg.
type ProjectMigrationBadge struct {
	Name         string `yaml:"name"`          // "AZDO Migrated"; the date is appended
	ImageProject string `yaml:"image_project"` // "group/project" holding the SVGs; default: the pipeline's own project
	ImageRef     string `yaml:"image_ref"`     // default: the project's default branch, else "master"
	ImageDir     string `yaml:"image_dir"`     // "assets"
}

func Load(path string) (*Config, error) {
	var cfg Config
	if err := loadYAML(path, &cfg); err != nil {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}
	return &cfg, nil
}

func loadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}
