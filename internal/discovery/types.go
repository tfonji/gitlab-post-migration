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

// Scope is every resolved project plus their top-level groups, persisted to projects.json by `discover` for all plan jobs.
type Scope struct {
	Projects       []Project `json:"projects"`
	TopLevelGroups []Group   `json:"top_level_groups"`
}
