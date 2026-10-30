package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestComplianceFrameworkIsEnabledByDefault(t *testing.T) {
	var cfg Config
	if !cfg.ComplianceFramework.IsEnabled() {
		t.Fatal("unset must mean enabled")
	}
}

func TestOverridesWinOverTheFile(t *testing.T) {
	on := true
	cfg := Config{
		ComplianceFramework: ComplianceFramework{Enabled: &on},
		GroupDefaultBranch:  GroupDefaultBranch{BranchName: "master"},
		DefaultBranchRename: DefaultBranchRename{BranchName: "master"},
	}
	err := cfg.ApplyOverrides(env(map[string]string{
		EnvComplianceFrameworkEnabled: "false",
		EnvDefaultBranch:              "main",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ComplianceFramework.IsEnabled() {
		t.Error("COMPLIANCE_FRAMEWORK_ENABLED=false must disable it")
	}
	if cfg.GroupDefaultBranch.BranchName != "main" || cfg.DefaultBranchRename.BranchName != "main" {
		t.Errorf("DEFAULT_BRANCH must drive both levels: %+v %+v", cfg.GroupDefaultBranch, cfg.DefaultBranchRename)
	}
}

func TestBlankOverridesKeepTheFile(t *testing.T) {
	off := false
	cfg := Config{
		ComplianceFramework: ComplianceFramework{Enabled: &off},
		DefaultBranchRename: DefaultBranchRename{BranchName: "main"},
	}
	if err := cfg.ApplyOverrides(env(nil)); err != nil {
		t.Fatal(err)
	}
	if cfg.ComplianceFramework.IsEnabled() || cfg.DefaultBranchRename.BranchName != "main" {
		t.Errorf("blank variables must not change the file's values: %+v", cfg)
	}
}

func TestInvalidOverridesAreRejected(t *testing.T) {
	cases := map[string]map[string]string{
		"branch":  {EnvDefaultBranch: "develop"},
		"enabled": {EnvComplianceFrameworkEnabled: "maybe"},
	}
	for name, vars := range cases {
		var cfg Config
		err := cfg.ApplyOverrides(env(vars))
		if err == nil || !strings.Contains(err.Error(), "must be") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	t.Setenv(EnvDefaultBranch, "main")
	cfg, err := Load("../../configs/desired-state.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultBranchRename.BranchName != "main" || cfg.GroupDefaultBranch.BranchName != "main" {
		t.Errorf("%+v %+v", cfg.DefaultBranchRename, cfg.GroupDefaultBranch)
	}
}

func TestKeepExistingOverrideSetsBothLevels(t *testing.T) {
	var cfg Config
	if err := cfg.ApplyOverrides(env(map[string]string{EnvKeepExistingDefaultBranch: "true"})); err != nil {
		t.Fatal(err)
	}
	if !cfg.GroupDefaultBranch.KeepExisting || !cfg.DefaultBranchRename.KeepExisting {
		t.Errorf("%+v %+v", cfg.GroupDefaultBranch, cfg.DefaultBranchRename)
	}
	if err := cfg.ApplyOverrides(env(map[string]string{EnvKeepExistingDefaultBranch: "yes please"})); err == nil {
		t.Error("an invalid value must be rejected")
	}
}

func TestShippedDefaults(t *testing.T) {
	cfg, err := Load("../../configs/desired-state.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultBranchRename.BranchName != "master" || cfg.GroupDefaultBranch.BranchName != "master" {
		t.Errorf("default branch should be master: %+v %+v", cfg.DefaultBranchRename, cfg.GroupDefaultBranch)
	}
	if cfg.DefaultBranchRename.KeepExisting || cfg.GroupDefaultBranch.KeepExisting {
		t.Error("keep_existing should default to false")
	}
}
