// Package diff holds the status and target/diff/result shapes shared by every task, so output merges into one report.
package diff

// Status is the outcome of comparing (plan) or reconciling (apply) one target.
type Status string

const (
	StatusUnchanged Status = "unchanged" // already matches desired state
	StatusDrifted   Status = "drifted"   // plan only: differs, not yet applied
	StatusApplied   Status = "applied"   // apply only: change was made
	StatusFailed    Status = "failed"    // API call errored
	// StatusSkipped means nothing to do, or intentionally not applicable; unlike Unchanged, the state may not match.
	StatusSkipped Status = "skipped"
)

// TargetKind distinguishes a group-level target from a project-level one.
type TargetKind string

const (
	TargetGroup   TargetKind = "group"
	TargetProject TargetKind = "project"
)

// Target identifies what a Diff/Result is about.
type Target struct {
	Kind TargetKind `json:"kind"`
	ID   int64      `json:"id"`
	Path string     `json:"path"` // full_path (group) or path_with_namespace (project)
	// TopLevelGroupID is recorded at plan time so apply can grant membership there; zero for group targets and old plan files.
	TopLevelGroupID int64 `json:"top_level_group_id,omitempty"`
}

// Diff is the output of a task's Plan step for one target.
type Diff struct {
	Target      Target         `json:"target"`
	Status      Status         `json:"status"` // StatusUnchanged, StatusDrifted, or StatusFailed
	Description string         `json:"description"`
	Detail      map[string]any `json:"detail,omitempty"` // structured before/after, consumed by Apply
}

// Result is the output of a task's Apply step for one target.
type Result struct {
	Target      Target `json:"target"`
	Status      Status `json:"status"` // StatusApplied, StatusUnchanged, StatusSkipped, or StatusFailed
	Description string `json:"description"`
	Error       string `json:"error,omitempty"`
}
