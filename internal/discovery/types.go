package discovery

// Project is a resolved target project, scoped down to the fields tasks need.
type Project struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	TopLevelGroupID   int64  `json:"top_level_group_id"`
}

// Group is a resolved top-level group.
type Group struct {
	ID       int64  `json:"id"`
	FullPath string `json:"full_path"`
}

// Scope is the full set of targets a pipeline run operates over: every
// resolved project, plus the deduplicated set of top-level groups those
// projects belong to (group-level tasks iterate TopLevelGroups; project-level
// tasks iterate Projects). It's persisted to projects.json by `discover` and
// read back by every `plan` job, so all task jobs in a run see the same
// resolved scope.
type Scope struct {
	Projects       []Project `json:"projects"`
	TopLevelGroups []Group   `json:"top_level_groups"`
}
