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
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
)

// fakeInstance serves project badges for projects 1..3 and the raw badge SVG.
//   - project 1: no badges
//   - project 2: already has its own "AZDO Migrated Sept. 1, 2026" badge
//   - project 3: only a badge inherited from its group with the same name
type fakeInstance struct {
	mu          sync.Mutex
	imageExists bool
	// badgesAfterFirstAdd makes project 1 report the badges that were added.
	badgesAfterFirstAdd bool
	added               []map[string]string // badges added: project -> body
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
	switch {
	case strings.HasSuffix(path, "/repository/files/assets/azdo-migrated-2026-10-19.svg/raw") ||
		strings.HasSuffix(path, "/repository/files/assets%2Fazdo-migrated-2026-10-19.svg/raw"):
		if !f.imageExists {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte("<svg/>"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v4/projects/") && strings.HasSuffix(path, "/badges"):
		switch strings.TrimSuffix(strings.TrimPrefix(path, "/api/v4/projects/"), "/badges") {
		case "2":
			reply(200, []map[string]any{{"id": 7, "name": "AZDO Migrated Sept. 1, 2026", "kind": "project"}})
		case "3":
			reply(200, []map[string]any{{"id": 8, "name": "AZDO Migrated Aug. 2, 2026", "kind": "group"}})
		default:
			existing := []map[string]any{}
			if f.badgesAfterFirstAdd {
				for _, a := range f.added {
					existing = append(existing, map[string]any{"id": 99, "name": a["name"], "kind": "project"})
				}
			}
			reply(200, existing)
		}
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/v4/projects/") && strings.HasSuffix(path, "/badges"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["project"] = strings.TrimSuffix(strings.TrimPrefix(path, "/api/v4/projects/"), "/badges")
		f.added = append(f.added, body)
		reply(201, map[string]any{"id": 99, "name": body["name"]})
	default:
		http.NotFound(w, r)
	}
}

var oct19 = time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)

func newTask(t *testing.T, f *fakeInstance) (*Task, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	task := New(c)
	task.Now = func() time.Time { return oct19 }
	t.Setenv(dateEnv, "")
	return task, srv.URL
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

func TestBadgeIsAddedWithTheDateAndLinksToTheProject(t *testing.T) {
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
	if got["name"] != "AZDO Migrated Oct. 19, 2026" {
		t.Errorf("name = %q", got["name"])
	}
	if want := base + "/tools/badges/-/raw/master/assets/azdo-migrated-2026-10-19.svg"; got["image_url"] != want {
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

func TestAnExistingBadgeKeepsItsOriginalDate(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, _ := newTask(t, f)
	diffs, _ := task.Plan(context.Background(), scope(), cfg("tools/badges"))
	for _, d := range diffs {
		if d.Target.ID == 2 && (d.Status != diff.StatusUnchanged || !strings.Contains(d.Description, "Sept. 1, 2026")) {
			t.Errorf("project 2 = %+v, want unchanged and still the Sept. 1 badge", d)
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
			if d.Status != diff.StatusFailed || !strings.Contains(d.Description, "badge-image") {
				t.Errorf("project %d = %+v, want a failure naming badge-image", d.Target.ID, d)
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

func TestImageProjectMustBeConfigured(t *testing.T) {
	task, _ := newTask(t, &fakeInstance{})
	if _, err := task.Plan(context.Background(), scope(), cfg("")); err == nil || !strings.Contains(err.Error(), "image_project") {
		t.Errorf("err = %v, want a message about image_project", err)
	}
}

func TestMigrationDateCanBePinned(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, _ := newTask(t, f)

	t.Setenv(dateEnv, "2026-10-19") // the same day, set explicitly while the clock says otherwise
	task.Now = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	diffs, err := task.Plan(context.Background(), scope(), cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		if d.Status == diff.StatusDrifted && d.Detail["name"] != "AZDO Migrated Oct. 19, 2026" {
			t.Errorf("name = %v", d.Detail["name"])
		}
	}

	t.Setenv(dateEnv, "19/10/2026")
	if _, err := task.Plan(context.Background(), scope(), cfg("tools/badges")); err == nil {
		t.Error("a malformed date must be an error")
	}
}

func TestFormatDate(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-19": "Oct. 19, 2026",
		"2026-01-05": "Jan. 5, 2026",
		"2026-03-02": "March 2, 2026",
		"2026-05-31": "May 31, 2026",
		"2026-09-09": "Sept. 9, 2026",
		"2026-12-25": "Dec. 25, 2026",
	} {
		d, _ := time.Parse("2006-01-02", in)
		if got := FormatDate(d); got != want {
			t.Errorf("FormatDate(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestBadgeImageIsAnSVGWithTheLabelAndDate(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteImage(filepath.Join(dir, "assets"), oct19)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "azdo-migrated-2026-10-19.svg" {
		t.Errorf("file = %s", path)
	}
	data, _ := os.ReadFile(path)
	svg := string(data)
	for _, want := range []string{"<svg ", "AZDO Migrated", "Oct. 19, 2026", `aria-label="AZDO Migrated Oct. 19, 2026"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("svg missing %q:\n%s", want, svg)
		}
	}
}

func TestRunningTwiceDoesNotAddASecondBadge(t *testing.T) {
	f := &fakeInstance{imageExists: true}
	task, _ := newTask(t, f)

	// One plan, applied twice (a second pipeline applying an older plan).
	diffs, err := task.Plan(context.Background(), discovery.Scope{Projects: []discovery.Project{{ID: 1, PathWithNamespace: "acme/app"}}}, cfg("tools/badges"))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.badgesAfterFirstAdd = true
	f.mu.Unlock()
	if _, err := task.Apply(context.Background(), diffs, cfg("tools/badges")); err != nil {
		t.Fatal(err)
	}
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
	badges := []*gitlab.ProjectBadge{{Name: "  azdo migrated Jan. 2, 2026", Kind: "project"}}
	if ownBadge(badges, "AZDO Migrated") == nil {
		t.Error("a differently cased or padded name still counts as the migration badge")
	}
}
