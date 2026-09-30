package discovery

import (
	"context"
	"fmt"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/tfonji/gitlab-post-migration/internal/gitlabclient"
)

// Resolve turns pipeline input (group IDs and/or explicit project IDs) into
// a Scope: every target project, plus the deduplicated top-level groups they
// belong to. All projects under each group (recursing subgroups) are assumed
// migrated and in scope — see design discussion, no marker/topic filtering
// for v1, archived projects are skipped. A project reachable more than once
// (a group and one of its subgroups both listed, or a project ID inside a
// listed group) is included once.
func Resolve(ctx context.Context, c *gitlabclient.Client, groupIDs []int64, projectIDs []int64) (Scope, error) {
	var projects []*gitlab.Project
	seen := map[int64]bool{}

	for _, groupID := range groupIDs {
		opts := &gitlab.ListGroupProjectsOptions{
			ListOptions:      gitlab.ListOptions{PerPage: 100},
			IncludeSubGroups: gitlab.Ptr(true),
			Archived:         gitlab.Ptr(false),
		}
		for {
			page, resp, err := c.REST.Groups.ListGroupProjects(groupID, opts, gitlab.WithContext(ctx))
			if err != nil {
				return Scope{}, fmt.Errorf("listing projects for group %d: %w", groupID, err)
			}
			for _, p := range page {
				if !seen[p.ID] {
					seen[p.ID] = true
					projects = append(projects, p)
				}
			}
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}

	for _, id := range projectIDs {
		if seen[id] {
			continue
		}
		p, _, err := c.REST.Projects.GetProject(id, nil, gitlab.WithContext(ctx))
		if err != nil {
			return Scope{}, fmt.Errorf("fetching project %d: %w", id, err)
		}
		projects = append(projects, p)
		seen[id] = true
	}

	scope := Scope{}
	topLevelCache := make(map[int64]Group) // namespace ID -> resolved top-level group

	for _, p := range projects {
		var topLevelGroupID int64
		if p.Namespace != nil {
			tlg, err := resolveTopLevelGroup(ctx, c, p.Namespace.ID, p.Namespace.ParentID, p.Namespace.FullPath, topLevelCache)
			if err != nil {
				return Scope{}, fmt.Errorf("resolving top-level group for project %s: %w", p.PathWithNamespace, err)
			}
			topLevelGroupID = tlg.ID
		}
		scope.Projects = append(scope.Projects, Project{
			ID:                p.ID,
			PathWithNamespace: p.PathWithNamespace,
			DefaultBranch:     p.DefaultBranch,
			TopLevelGroupID:   topLevelGroupID,
		})
	}

	groupsByID := make(map[int64]Group, len(topLevelCache))
	for _, g := range topLevelCache {
		groupsByID[g.ID] = g
	}
	for _, g := range groupsByID {
		scope.TopLevelGroups = append(scope.TopLevelGroups, g)
	}

	return scope, nil
}

// resolveTopLevelGroup walks a namespace's ancestry up to the group with no
// parent. namespaceID/parentID/fullPath are the project's own immediate
// namespace (cheap: already on the Project payload); ancestors beyond that
// require a GetGroup call each, so results are cached across projects that
// share a namespace.
func resolveTopLevelGroup(ctx context.Context, c *gitlabclient.Client, namespaceID, parentID int64, fullPath string, cache map[int64]Group) (Group, error) {
	if g, ok := cache[namespaceID]; ok {
		return g, nil
	}

	if parentID == 0 {
		g := Group{ID: namespaceID, FullPath: fullPath}
		cache[namespaceID] = g
		return g, nil
	}

	// Walk up via the Groups API until we reach a group with ParentID == 0.
	currentID := parentID
	for {
		if g, ok := cache[currentID]; ok {
			cache[namespaceID] = g
			return g, nil
		}
		grp, _, err := c.REST.Groups.GetGroup(currentID, nil, gitlab.WithContext(ctx))
		if err != nil {
			return Group{}, fmt.Errorf("fetching group %d: %w", currentID, err)
		}
		if grp.ParentID == 0 {
			g := Group{ID: grp.ID, FullPath: grp.FullPath}
			cache[namespaceID] = g
			cache[grp.ID] = g
			return g, nil
		}
		currentID = grp.ParentID
	}
}
