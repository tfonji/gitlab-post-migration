package inventory

import (
	"strings"
	"testing"

	"gitlab-post-migration/internal/discovery"
)

func scope() discovery.Scope {
	return discovery.Scope{
		Projects: []discovery.Project{
			{ID: 205, PathWithNamespace: "bank/zeta"},
			{ID: 9, PathWithNamespace: "bank/alpha"},
			{ID: 101, PathWithNamespace: "bank/sub/beta"},
		},
		TopLevelGroups: []discovery.Group{{ID: 30, FullPath: "bank"}, {ID: 4, FullPath: "agency"}},
	}
}

func TestIDListsAreCommaSeparatedAndSortedNumerically(t *testing.T) {
	inv := Build(scope())
	if inv.ProjectIDs != "9,101,205" || inv.GroupIDs != "4,30" {
		t.Errorf("project ids = %q, group ids = %q", inv.ProjectIDs, inv.GroupIDs)
	}
}

func TestMappingsAreSortedByNameAndMapNameToID(t *testing.T) {
	inv := Build(scope())
	names := []string{}
	for _, p := range inv.Projects {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "bank/alpha,bank/sub/beta,bank/zeta" {
		t.Errorf("projects = %v", names)
	}
	if inv.Projects[1].ID != 101 || inv.Groups[0].Name != "agency" || inv.Groups[0].ID != 4 {
		t.Errorf("mapping wrong: %+v / %+v", inv.Projects, inv.Groups)
	}
}

func TestTextHasPasteableListsAndBothMappings(t *testing.T) {
	text := Build(scope()).Text()
	for _, want := range []string{
		"PROJECT_IDS=9,101,205\n",
		"GROUP_ID=4,30\n",
		"PROJECTS (3), name -> id\nbank/alpha -> 9\n",
		"GROUPS (2), name -> id\nagency -> 4\nbank -> 30\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}

func TestEmptyScopeGivesEmptyListsNotAnError(t *testing.T) {
	inv := Build(discovery.Scope{})
	if inv.ProjectIDs != "" || inv.GroupIDs != "" || len(inv.Projects) != 0 || inv.Projects == nil {
		t.Errorf("inventory = %+v", inv)
	}
	if !strings.Contains(inv.Text(), "PROJECTS (0)") {
		t.Errorf("text = %q", inv.Text())
	}
}
