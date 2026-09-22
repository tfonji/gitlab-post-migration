// Package diff holds the status vocabulary and target/diff/result shapes
// shared by every task, so plan and apply output is uniform across the
// whole pipeline and can be merged into one report.
package diff

// Status is the outcome of comparing (plan) or reconciling (apply) a single
// target against the desired state.
type Status string

const (
	StatusUnchanged Status = "unchanged" // already matches desired state
	StatusDrifted   Status = "drifted"   // plan only: differs, not yet applied
	StatusApplied   Status = "applied"   // apply only: change was made
	StatusFailed    Status = "failed"    // API call errored
	// StatusSkipped covers two cases: at apply time, "plan said unchanged,
	// nothing to do"; at plan time, a task can also use it directly for a
	// target it determined is intentionally not applicable (e.g.
	// project-ci-template-mr on a project with no recognized build file) --
	// distinct from StatusUnchanged, which means the desired state exists
	// and already matches.
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
