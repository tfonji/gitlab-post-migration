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
	MRPolicy             MRPolicy             `yaml:"mr_policy"`
	ComplianceFramework  ComplianceFramework  `yaml:"compliance_framework"`
	ProtectedEnvironment ProtectedEnvironment `yaml:"protected_environment"`
	GroupDefaultBranch   GroupDefaultBranch   `yaml:"group_default_branch"`
	DefaultBranchRename  DefaultBranchRename  `yaml:"default_branch_rename"`
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

// ProtectedEnvironment describes the single group-level protected
// environment to enforce (currently always "production", deploy + approvers
// restricted to Maintainer).
type ProtectedEnvironment struct {
	EnvironmentName       string `yaml:"environment_name"`
	DeployAccessLevel     string `yaml:"deploy_access_level"`   // "maintainer"
	ApproverAccessLevel   string `yaml:"approver_access_level"` // "maintainer"
	RequiredApprovalCount int64  `yaml:"required_approval_count"`
}

// GroupDefaultBranch is the group setting governing the default branch name
// for NEW projects created in the group going forward.
type GroupDefaultBranch struct {
	BranchName string `yaml:"branch_name"` // "master"
}

// DefaultBranchRename is the desired default branch name for EXISTING
// migrated projects; the task creates this branch from the project's
// current default (if missing) and switches the project's default branch
// pointer to it.
type DefaultBranchRename struct {
	BranchName string `yaml:"branch_name"` // "master"
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
