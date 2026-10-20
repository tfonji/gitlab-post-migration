package groupmigrationbadge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
)

// fakeInstance serves badges for groups 1..2 (none; already has it) and the raw SVG.
type fakeInstance struct {
	mu          sync.Mutex
	imageExists bool
	// afterAdd makes every group report the badges added so far.
	afterAdd bool
	added    []map[string]string

	base          string                      // the instance URL, for project badges that carry our image
	projectBadges map[string][]map[string]any // project id -> its badges
	deleted       []string                    // "project/badge" removed
	denyGroupAdd  bool
}

func (f *fakeInstance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	path := r.URL.Path
	groupID := func() string {
		return strings.TrimSuffix(strings.TrimPrefix(path, "/api/v4/groups/"), "/badges")
	}
	switch {
	case strings.HasSuffix(path, "/repository/files/assets/azdo-migrated.svg/raw"):
		if !f.imageExists {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte("<svg/>"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v4/projects/") && strings.HasSuffix(path, "/badges"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v4/projects/"), "/badges")
		badges := f.projectBadges[id]
		if badges == nil {
			badges = []map[string]any{}
		}
		reply(200, badges)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/v4/projects/") && strings.Contains(path, "/badges/"):
		rest := strings.TrimPrefix(path, "/api/v4/projects/")
		f.deleted = append(f.deleted, strings.Replace(rest, "/badges/", "/", 1))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v4/groups/") && strings.HasSuffix(path, "/badges"):
		switch groupID() {
		case "2":
			reply(200, []map[string]any{{"id": 7, "name": "AZDO Migrated", "kind": "group"}})
		default:
			existing := []map[string]any{}
			if f.afterAdd {
				for _, a := range f.added {
					existing = append(existing, map[string]any{"id": 99, "name": a["name"], "kind": "group"})
				}
			}
			reply(200, existing)
		}
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v4/groups/") && strings.HasSuffix(path, "/badges"):
		if f.denyGroupAdd {
			reply(403, map[string]any{"message": "403 Forbidden"})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["group"] = groupID()
		f.added = append(f.added, body)
		reply(201, map[string]any{"id": 99, "name": body["name"]})
	default:
		http.NotFound(w, r)
	}
}

func newTask(t *testing.T, f *fakeInstance) (*Task, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	f.base = srv.URL
	t.Setenv("CI_PROJECT_PATH", "")
	t.Setenv("CI_DEFAULT_BRANCH", "")
	return New(c), srv.URL
}

func cfg(imageProject string) *config.Config {
	return &config.Config{GroupMigrationBadge: config.GroupMigrationBadge{ImageProject: imageProject}}
}

func scope() discovery.Scope {
	return discovery.Scope{TopLevelGroups: []discovery.Group{
		{ID: 1, FullPath: "acme"},
		{ID: 2, FullPath: "globex"},
		{ID: 3, FullPath: "initech"},
	}}
}

func TestBadgeIsAddedToTheTopLevelGroupAndLinksToIt(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, base := newTask(t, f)

	diffs, err := task.Plan(context.Background(), scope(), cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	status := map[int64]diff.Status{}
	for _, d := range diffs {
		status[d.Target.ID] = d.Status
	}
	// Groups 1 and 3 have none; group 2 already has it.
	if status[1] != diff.StatusDrifted || status[2] != diff.StatusUnchanged || status[3] != diff.StatusDrifted {
		t.Fatalf("statuses = %v", status)
	}

	results, err := task.Apply(context.Background(), diffs, cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 2 {
		t.Fatalf("added = %v, want badges for groups 1 and 3 only", f.added)
	}
	got := f.added[0]
	if got["name"] != "AZDO Migrated" {
		t.Errorf("name = %q", got["name"])
	}
	if want := base + "/tools/badges/-/raw/master/assets/azdo-migrated.svg"; got["image_url"] != want {
		t.Errorf("image_url = %q, want %q", got["image_url"], want)
	}
	if want := base + "/acme"; got["link_url"] != want {
		t.Errorf("link_url = %q, want the group itself, %q", got["link_url"], want)
	}
	for _, r := range results {
		if r.Status == diff.StatusFailed {
			t.Errorf("unexpected failure: %+v", r)
		}
	}
}

func TestMissingImageFailsInsteadOfAddingBrokenBadges(t *testing.T) {
	f := &fakeInstance{imageExists: false}
	task, _ := newTask(t, f)

	diffs, err := task.Plan(context.Background(), scope(), cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		switch d.Target.ID {
		case 1, 3:
			if d.Status != diff.StatusFailed || !strings.Contains(d.Description, "assets/azdo-migrated.svg not found in tools/badges (master)") {
				t.Errorf("group %d = %+v, want a failure naming the missing image", d.Target.ID, d)
			}
		case 2:
			if d.Status != diff.StatusUnchanged {
				t.Errorf("a group that already has its badge needs no image: %+v", d)
			}
		}
	}
	if _, err := task.Apply(context.Background(), diffs, cfg("tools/badges")); err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 0 {
		t.Errorf("no badge may be added without the image: %v", f.added)
	}
}

func TestImageProjectAndBranchDefaultToThePipelinesOwn(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, base := newTask(t, f)
	t.Setenv("CI_PROJECT_PATH", "tools/post-migration")
	t.Setenv("CI_DEFAULT_BRANCH", "main")

	diffs, err := task.Plan(context.Background(), scope(), cfg(""))
	if err != nil {
		t.Fatal(err)
	}
	want := base + "/tools/post-migration/-/raw/main/assets/azdo-migrated.svg"
	for _, d := range diffs {
		if d.Status == diff.StatusDrifted && d.Detail["image_url"] != want {
			t.Errorf("image_url = %v, want %s", d.Detail["image_url"], want)
		}
	}
}

func TestConfiguredImageProjectAndRefWin(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, base := newTask(t, f)
	t.Setenv("CI_PROJECT_PATH", "tools/post-migration")

	c := cfg("assets-group/badges")
	c.GroupMigrationBadge.ImageRef = "stable"
	diffs, err := task.Plan(context.Background(), scope(), c)
	if err != nil {
		t.Fatal(err)
	}
	want := base + "/assets-group/badges/-/raw/stable/assets/azdo-migrated.svg"
	for _, d := range diffs {
		if d.Status == diff.StatusDrifted && d.Detail["image_url"] != want {
			t.Errorf("image_url = %v, want %s", d.Detail["image_url"], want)
		}
	}
}

func TestNoImageProjectAnywhereIsAnError(t *testing.T) {
	task, _ := newTask(t, &fakeInstance{})
	if _, err := task.Plan(context.Background(), scope(), cfg("")); err == nil || !strings.Contains(err.Error(), "image_project") {
		t.Errorf("err = %v, want a message about image_project", err)
	}
}

func TestRunningTwiceDoesNotAddASecondBadge(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, _ := newTask(t, f)

	// One plan, applied twice (a second pipeline applying an older plan).
	one := discovery.Scope{TopLevelGroups: []discovery.Group{{ID: 1, FullPath: "acme"}}}
	diffs, err := task.Plan(context.Background(), one, cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.Apply(context.Background(), diffs, cfg("tools/badges")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.afterAdd = true
	f.mu.Unlock()
	results, err := task.Apply(context.Background(), diffs, cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 1 {
		t.Errorf("added %d badges, want exactly one", len(f.added))
	}
	if results[0].Status != diff.StatusUnchanged || !strings.Contains(results[0].Description, "left as is") {
		t.Errorf("second apply = %+v", results[0])
	}
}

func TestBadgeNameMatchIgnoresCaseAndSpaces(t *testing.T) {
	badges := []*gitlab.GroupBadge{{Name: "  azdo migrated"}}
	if ownBadge(badges, "AZDO Migrated") == nil {
		t.Error("a differently cased or padded name still counts as the migration badge")
	}
}

func TestBadgeImageIsAnSVGWithTheLabel(t *testing.T) {
	path, err := WriteImage(filepath.Join(t.TempDir(), "assets"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "azdo-migrated.svg" {
		t.Errorf("file = %s", path)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"<svg ", `aria-label="AZDO Migrated"`, ">AZDO Migrated<"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("svg missing %q:\n%s", want, data)
		}
	}
}

// withProjects adds projects to scope(): 10 and 11 in group 1, 12 in group 2, 13 in group 3.
func withProjects() discovery.Scope {
	sc := scope()
	sc.Projects = []discovery.Project{
		{ID: 10, PathWithNamespace: "acme/app", TopLevelGroupID: 1},
		{ID: 11, PathWithNamespace: "acme/custom", TopLevelGroupID: 1},
		{ID: 12, PathWithNamespace: "globex/app", TopLevelGroupID: 2},
		{ID: 13, PathWithNamespace: "initech/app", TopLevelGroupID: 3},
	}
	return sc
}

// projectBadgeFixtures gives each project its badges: ours (exact), a lookalike with another image,
// and the group's own badge as the project shows it (kind group).
func projectBadgeFixtures(f *fakeInstance) {
	ours := f.base + "/tools/badges/-/raw/master/assets/azdo-migrated.svg"
	f.projectBadges = map[string][]map[string]any{
		"10": {
			{"id": 55, "name": "AZDO Migrated", "image_url": ours, "kind": "project"},
			{"id": 56, "name": "AZDO Migrated", "image_url": ours, "kind": "group"},
		},
		"11": {{"id": 60, "name": "AZDO Migrated", "image_url": "https://elsewhere.example/other.svg", "kind": "project"}},
		"12": {{"id": 66, "name": "AZDO Migrated", "image_url": ours, "kind": "project"}},
		"13": {{"id": 77, "name": "azdo migrated", "image_url": ours, "kind": "project"}},
	}
}

func TestTheExactProjectBadgeIsRemovedOnceTheGroupHasTheBadge(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, _ := newTask(t, f)
	projectBadgeFixtures(f)

	diffs, err := task.Plan(context.Background(), withProjects(), cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		if d.Target.ID == 11 {
			t.Errorf("a project with nothing to remove needs no diff: %+v", d)
		}
	}
	if _, err := task.Apply(context.Background(), diffs, cfg("tools/badges")); err != nil {
		t.Fatal(err)
	}
	want := "10/55,12/66,13/77"
	if got := strings.Join(f.deleted, ","); got != want {
		t.Errorf("deleted = %s, want %s: the exact project badges only, never the group's copy or a lookalike", got, want)
	}
}

func TestProjectBadgesStayWhenTheGroupBadgeCannotBePlanned(t *testing.T) {
	f := &fakeInstance{imageExists: false} // groups 1 and 3 cannot get a badge; group 2 already has one
	task, _ := newTask(t, f)
	projectBadgeFixtures(f)

	diffs, err := task.Plan(context.Background(), withProjects(), cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	status := map[int64]diff.Status{}
	for _, d := range diffs {
		if d.Target.Kind == diff.TargetProject {
			status[d.Target.ID] = d.Status
		}
	}
	if status[10] != diff.StatusSkipped || status[13] != diff.StatusSkipped || status[12] != diff.StatusDrifted {
		t.Errorf("project statuses = %v", status)
	}
	if _, err := task.Apply(context.Background(), diffs, cfg("tools/badges")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deleted, ","); got != "12/66" {
		t.Errorf("deleted = %s, want only the project whose group already has the badge", got)
	}
}

func TestProjectBadgesStayWhenAddingTheGroupBadgeFails(t *testing.T) {
	f := &fakeInstance{imageExists: true, denyGroupAdd: true}
	task, _ := newTask(t, f)
	projectBadgeFixtures(f)

	diffs, err := task.Plan(context.Background(), withProjects(), cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	results, err := task.Apply(context.Background(), diffs, cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.deleted, ","); got != "12/66" {
		t.Errorf("deleted = %s, want only group 2's project, whose group already had the badge", got)
	}
	kept := 0
	for _, r := range results {
		if r.Status == diff.StatusSkipped && strings.Contains(r.Description, "its group's badge was not added") {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("results = %+v, want projects 10 and 13 kept", results)
	}
}
