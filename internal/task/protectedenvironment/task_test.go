package protectedenvironment

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

// fakeGroup serves group 5's protected environments: protected maps an
// environment name to its current (access level, required approvals).
type fakeGroup struct {
	mu        sync.Mutex
	protected map[string][2]int
	wrote     []string
}

func (f *fakeGroup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	const base = "/api/v4/groups/5/protected_environments"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/"):
		name := strings.TrimPrefix(r.URL.Path, base+"/")
		cur, ok := f.protected[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reply(200, map[string]any{
			"name":                 name,
			"deploy_access_levels": []map[string]any{{"access_level": cur[0]}},
			"approval_rules":       []map[string]any{{"access_level": cur[0], "required_approvals": cur[1]}},
		})
	case r.Method == http.MethodPost && r.URL.Path == base:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.wrote = append(f.wrote, body.Name)
		f.protected[body.Name] = [2]int{40, 1}
		reply(201, map[string]any{"name": body.Name})
	default:
		http.NotFound(w, r)
	}
}

func run(t *testing.T, f *fakeGroup, cfg *config.Config) ([]diff.Diff, []diff.Result) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	task := New(client)
	scope := discovery.Scope{TopLevelGroups: []discovery.Group{{ID: 5, FullPath: "acme"}}}
	diffs, err := task.Plan(context.Background(), scope, cfg)
	if err != nil {
		t.Fatal(err)
	}
	results, err := task.Apply(context.Background(), diffs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return diffs, results
}

func TestStagingIsProtectedLikeProduction(t *testing.T) {
	cfg, err := config.Load("../../../configs/desired-state.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Production is already protected as desired; staging is not protected yet.
	f := &fakeGroup{protected: map[string][2]int{"production": {40, 1}}}
	diffs, results := run(t, f, cfg)

	status := map[string]diff.Status{}
	for _, d := range diffs {
		status[d.Detail["environment"].(string)] = d.Status
	}
	if status["production"] != diff.StatusUnchanged || status["staging"] != diff.StatusDrifted || len(diffs) != 2 {
		t.Fatalf("plan statuses = %v", status)
	}
	if len(f.wrote) != 1 || f.wrote[0] != "staging" {
		t.Errorf("only staging should be written, wrote %v", f.wrote)
	}
	applied := 0
	for _, r := range results {
		if r.Status == diff.StatusApplied && strings.Contains(r.Description, `"staging"`) {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("results = %+v", results)
	}
}

func TestStagingWithWrongApprovalCountIsReconciled(t *testing.T) {
	cfg := &config.Config{ProtectedEnvironments: []config.ProtectedEnvironment{
		{EnvironmentName: "staging", DeployAccessLevel: "maintainer", RequiredApprovalCount: 1},
	}}
	f := &fakeGroup{protected: map[string][2]int{"staging": {30, 1}}} // developers can deploy: differs
	diffs, _ := run(t, f, cfg)
	if len(diffs) != 1 || diffs[0].Status != diff.StatusDrifted || len(f.wrote) != 1 {
		t.Errorf("diffs = %+v, wrote %v", diffs, f.wrote)
	}
}

func TestLegacySingleEnvironmentKeyStillWorks(t *testing.T) {
	cfg := &config.Config{ProtectedEnvironment: config.ProtectedEnvironment{EnvironmentName: "production", DeployAccessLevel: "maintainer"}}
	f := &fakeGroup{protected: map[string][2]int{}}
	diffs, _ := run(t, f, cfg)
	if len(diffs) != 1 || len(f.wrote) != 1 || f.wrote[0] != "production" {
		t.Errorf("diffs = %+v, wrote %v", diffs, f.wrote)
	}
}

func TestEmptyConfigDefaultsToProduction(t *testing.T) {
	f := &fakeGroup{protected: map[string][2]int{}}
	run(t, f, &config.Config{})
	if len(f.wrote) != 1 || f.wrote[0] != "production" {
		t.Errorf("wrote %v, want just production", f.wrote)
	}
}
