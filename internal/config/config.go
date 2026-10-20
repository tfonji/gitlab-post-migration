// Package config loads the desired-state baseline tasks reconcile against.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the desired-state baseline, identical for every top-level group in a run.
type Config struct {
	MRPolicy            MRPolicy            `yaml:"mr_policy"`
	ComplianceFramework ComplianceFramework `yaml:"compliance_framework"`
	// ProtectedEnvironments lists the protected environments; the single ProtectedEnvironment key is used when it is empty.
	ProtectedEnvironments []ProtectedEnvironment `yaml:"protected_environments"`
	ProtectedEnvironment  ProtectedEnvironment   `yaml:"protected_environment"`
	GroupDefaultBranch    GroupDefaultBranch     `yaml:"group_default_branch"`
	DefaultBranchRename   DefaultBranchRename    `yaml:"default_branch_rename"`

	GroupMigrationBadge GroupMigrationBadge `yaml:"group_migration_badge"`
}

// MRPolicy names the security policy project linked to each top-level group.
type MRPolicy struct {
	SecurityPolicyProjectPath string `yaml:"security_policy_project_path"`
}

// ComplianceFramework is created or reconciled at each top-level group and assigned to its projects.
type ComplianceFramework struct {
	Name                          string `yaml:"name"`
	Color                         string `yaml:"color"`
	Description                   string `yaml:"description"`
	PipelineConfigurationFullPath string `yaml:"pipeline_configuration_full_path"` // "<file>.yml@<group>/<project>"
}

// ProtectedEnvironment is one group-level protected environment (a deployment tier) with its deploy and approval settings.
type ProtectedEnvironment struct {
	EnvironmentName       string `yaml:"environment_name"`
	DeployAccessLevel     string `yaml:"deploy_access_level"`   // "maintainer"
	ApproverAccessLevel   string `yaml:"approver_access_level"` // "maintainer"
	RequiredApprovalCount int64  `yaml:"required_approval_count"`

	// ApproverGroupID makes that group the approver, shared into the top-level group first unless disabled.
	ApproverGroupID           int64  `yaml:"approver_group_id"`
	EnsureApproverGroupShared *bool  `yaml:"ensure_approver_group_shared"`
	ApproverGroupAccessLevel  string `yaml:"approver_group_access_level"` // "reporter"
}

// GroupDefaultBranch is the default branch name for new projects in the group.
type GroupDefaultBranch struct {
	BranchName string `yaml:"branch_name"` // "master"
}

// DefaultBranchRename is the default branch for existing projects: created if missing, protected, then made the default.
type DefaultBranchRename struct {
	BranchName              string `yaml:"branch_name"`                // "master"
	ProtectPushAccessLevel  string `yaml:"protect_push_access_level"`  // "maintainer"
	ProtectMergeAccessLevel string `yaml:"protect_merge_access_level"` // "maintainer"
}

// ProtectedEnvironmentList returns the list, else the legacy single key, else production defaults.
func (c *Config) ProtectedEnvironmentList() []ProtectedEnvironment {
	if len(c.ProtectedEnvironments) > 0 {
		return c.ProtectedEnvironments
	}
	return []ProtectedEnvironment{c.ProtectedEnvironment}
}

// GroupMigrationBadge configures the "AZDO Migrated" badge; the optional fields say where its SVG is read from.
type GroupMigrationBadge struct {
	Name         string `yaml:"name"`          // "AZDO Migrated"
	ImageProject string `yaml:"image_project"` // default: the pipeline's own project
	ImageRef     string `yaml:"image_ref"`     // default: the default branch, else "master"
	ImageDir     string `yaml:"image_dir"`     // default: "assets"
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
