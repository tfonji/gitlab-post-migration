package defaultbranchrename

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
)

// fakeProject serves project 1 (default branch main, tip bbb); masterTip is master's tip, "" if absent.
type fakeProject struct {
	masterTip string

	mu           sync.Mutex
	calls        []string
	defaultAfter string
}

func (f *fakeProject) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	branch := func(name, tip string) map[string]any {
		return map[string]any{"name": name, "commit": map[string]any{"id": tip}}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch p := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/1"); {
	case p == "" && r.Method == http.MethodGet:
		reply(200, map[string]any{"id": 1, "default_branch": "main"})
	case p == "" && r.Method == http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.defaultAfter, _ = body["default_branch"].(string)
		f.calls = append(f.calls, "set-default "+f.defaultAfter)
		reply(200, map[string]any{"id": 1, "default_branch": f.defaultAfter})
	case p == "/repository/branches/main":
		reply(200, branch("main", "bbbbbbbbbbbb"))
	case p == "/repository/branches/master" && r.Method == http.MethodGet:
		if f.masterTip == "" {
			http.NotFound(w, r)
			return
		}
		reply(200, branch("master", f.masterTip))
	case p == "/repository/branches" && r.Method == http.MethodPost:
		f.calls = append(f.calls, "create-branch")
		reply(201, branch("master", "bbbbbbbbbbbb"))
	case strings.HasPrefix(p, "/protected_branches/") && r.Method == http.MethodGet:
		http.NotFound(w, r)
	case p == "/protected_branches" && r.Method == http.MethodPost:
		f.calls = append(f.calls, "protect")
		reply(201, map[string]any{"name": "master"})
	default:
		http.NotFound(w, r)
	}
}

func runTask(t *testing.T, f *fakeProject) (diff.Diff, diff.Result) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	task := New(client)
	cfg := &config.Config{}
	scope := discovery.Scope{Projects: []discovery.Project{{ID: 1, PathWithNamespace: "acme/app", DefaultBranch: "main"}}}

	diffs, err := task.Plan(context.Background(), scope, cfg)
	if err != nil || len(diffs) != 1 {
		t.Fatalf("plan: %v %+v", err, diffs)
	}
	results, err := task.Apply(context.Background(), diffs, cfg)
	if err != nil || len(results) != 1 {
		t.Fatalf("apply: %v %+v", err, results)
	}
	return diffs[0], results[0]
}

func TestMasterThatIsBehindBecomesTheDefaultAnyway(t *testing.T) {
	f := &fakeProject{masterTip: "aaaaaaaaaaaa"} // master exists, tip differs from main's
	d, r := runTask(t, f)

	if d.Status != diff.StatusDrifted {
		t.Fatalf("plan must not refuse an out-of-date master: %s %q", d.Status, d.Description)
	}
	if !strings.Contains(d.Description, "differs") || !strings.Contains(d.Description, "becomes the default as it is") {
		t.Errorf("plan should explain what will happen: %q", d.Description)
	}
	if r.Status != diff.StatusApplied {
		t.Fatalf("apply: %s %s", r.Status, r.Error)
	}
	if f.defaultAfter != "master" {
		t.Errorf("default branch = %q, want master", f.defaultAfter)
	}
	for _, c := range f.calls {
		if c == "create-branch" {
			t.Error("an existing master must not be recreated or reset")
		}
	}
	if !strings.Contains(r.Description, "used it as is") || !strings.Contains(r.Description, "set as default branch") {
		t.Errorf("result should say master was used as is: %q", r.Description)
	}
}

func TestMasterThatMatchesIsReused(t *testing.T) {
	f := &fakeProject{masterTip: "bbbbbbbbbbbb"}
	_, r := runTask(t, f)
	if r.Status != diff.StatusApplied || f.defaultAfter != "master" || !strings.Contains(r.Description, "reused it") {
		t.Errorf("result = %+v, default = %q", r, f.defaultAfter)
	}
}

func TestMissingMasterIsCreatedFromTheDefault(t *testing.T) {
	f := &fakeProject{}
	_, r := runTask(t, f)
	created := false
	for _, c := range f.calls {
		created = created || c == "create-branch"
	}
	if r.Status != diff.StatusApplied || !created || f.defaultAfter != "master" {
		t.Errorf("result = %+v, calls = %v, default = %q", r, f.calls, f.defaultAfter)
	}
}
