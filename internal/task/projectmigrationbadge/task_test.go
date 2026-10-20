package projectmigrationbadge

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

// fakeInstance serves badges for projects 1..3 (none; its own; only one inherited from its group) and the raw SVG.
type fakeInstance struct {
	mu          sync.Mutex
	imageExists bool
	// afterAdd makes every project report the badges added so far.
	afterAdd bool
	added    []map[string]string
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
	projectID := func() string {
		return strings.TrimSuffix(strings.TrimPrefix(path, "/api/v4/projects/"), "/badges")
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
		switch projectID() {
		case "2":
			reply(200, []map[string]any{{"id": 7, "name": "AZDO Migrated", "kind": "project"}})
		case "3":
			reply(200, []map[string]any{{"id": 8, "name": "AZDO Migrated", "kind": "group"}})
		default:
			existing := []map[string]any{}
			if f.afterAdd {
				for _, a := range f.added {
					existing = append(existing, map[string]any{"id": 99, "name": a["name"], "kind": "project"})
				}
			}
			reply(200, existing)
		}
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v4/projects/") && strings.HasSuffix(path, "/badges"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["project"] = projectID()
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
	t.Setenv("CI_PROJECT_PATH", "")
	t.Setenv("CI_DEFAULT_BRANCH", "")
	return New(c), srv.URL
}

func cfg(imageProject string) *config.Config {
	return &config.Config{ProjectMigrationBadge: config.ProjectMigrationBadge{ImageProject: imageProject}}
}

func scope() discovery.Scope {
	return discovery.Scope{Projects: []discovery.Project{
		{ID: 1, PathWithNamespace: "acme/app"},
		{ID: 2, PathWithNamespace: "acme/old-app"},
		{ID: 3, PathWithNamespace: "acme/inherits"},
	}}
}

func TestBadgeIsAddedAndLinksToTheProject(t *testing.T) {
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
	// 1 has none, 2 already has its own, 3 only inherits one from its group.
	if status[1] != diff.StatusDrifted || status[2] != diff.StatusUnchanged || status[3] != diff.StatusDrifted {
		t.Fatalf("statuses = %v", status)
	}

	results, err := task.Apply(context.Background(), diffs, cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.added) != 2 {
		t.Fatalf("added = %v, want badges for projects 1 and 3 only", f.added)
	}
	got := f.added[0]
	if got["name"] != "AZDO Migrated" {
		t.Errorf("name = %q", got["name"])
	}
	if want := base + "/tools/badges/-/raw/master/assets/azdo-migrated.svg"; got["image_url"] != want {
		t.Errorf("image_url = %q, want %q", got["image_url"], want)
	}
	if want := base + "/acme/app"; got["link_url"] != want {
		t.Errorf("link_url = %q, want the project itself, %q", got["link_url"], want)
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
				t.Errorf("project %d = %+v, want a failure naming the missing image", d.Target.ID, d)
			}
		case 2:
			if d.Status != diff.StatusUnchanged {
				t.Errorf("a project that already has its badge needs no image: %+v", d)
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
	c.ProjectMigrationBadge.ImageRef = "stable"
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
	one := discovery.Scope{Projects: []discovery.Project{{ID: 1, PathWithNamespace: "acme/app"}}}
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
	badges := []*gitlab.ProjectBadge{{Name: "  azdo migrated", Kind: "project"}}
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
