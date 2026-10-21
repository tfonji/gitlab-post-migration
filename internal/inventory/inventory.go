// Package inventory lists the projects and top-level groups a run will act on, as comma lists and name-to-ID mappings.
package inventory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gitlab-post-migration/internal/discovery"
)

// Entry maps a full path to its ID.
type Entry struct {
	Name string `json:"name"`
	ID   int64  `json:"id"`
}

// Inventory is the scope as lists a person can paste into the next run, plus the mappings.
type Inventory struct {
	ProjectIDs string  `json:"project_ids"`
	GroupIDs   string  `json:"group_ids"`
	Projects   []Entry `json:"projects"`
	Groups     []Entry `json:"groups"`
}

// Build derives the inventory from the scope: ID lists sorted numerically, mappings sorted by name.
func Build(scope discovery.Scope) Inventory {
	inv := Inventory{Projects: []Entry{}, Groups: []Entry{}}
	for _, p := range scope.Projects {
		inv.Projects = append(inv.Projects, Entry{Name: p.PathWithNamespace, ID: p.ID})
	}
	for _, g := range scope.TopLevelGroups {
		inv.Groups = append(inv.Groups, Entry{Name: g.FullPath, ID: g.ID})
	}
	inv.ProjectIDs, inv.GroupIDs = idList(inv.Projects), idList(inv.Groups)
	sortByName(inv.Projects)
	sortByName(inv.Groups)
	return inv
}

func idList(entries []Entry) string {
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func sortByName(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// Text renders the inventory for the job log and the text artifact; the two lists carry the
// pipeline variable names they can be pasted into.
func (inv Inventory) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "PROJECT_IDS=%s\n", inv.ProjectIDs)
	fmt.Fprintf(&b, "GROUP_ID=%s\n", inv.GroupIDs)
	section := func(title string, entries []Entry) {
		fmt.Fprintf(&b, "\n%s (%d), name -> id\n", title, len(entries))
		for _, e := range entries {
			fmt.Fprintf(&b, "%s -> %d\n", e.Name, e.ID)
		}
	}
	section("PROJECTS", inv.Projects)
	section("GROUPS", inv.Groups)
	return b.String()
}
