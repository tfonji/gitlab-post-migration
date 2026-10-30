package groupdefaultbranch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
)

func plan(t *testing.T, groupDefault string, cfg *config.Config) diff.Diff {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "default_branch": groupDefault})
	}))
	t.Cleanup(srv.Close)
	client, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	scope := discovery.Scope{TopLevelGroups: []discovery.Group{{ID: 5, FullPath: "acme"}}}
	diffs, err := New(client).Plan(context.Background(), scope, cfg)
	if err != nil || len(diffs) != 1 {
		t.Fatalf("plan: %v %+v", err, diffs)
	}
	return diffs[0]
}

func TestKeepExistingLeavesGroupOnMain(t *testing.T) {
	cfg := &config.Config{GroupDefaultBranch: config.GroupDefaultBranch{BranchName: "master", KeepExisting: true}}
	if d := plan(t, "main", cfg); d.Status != diff.StatusUnchanged {
		t.Errorf("diff = %+v", d)
	}
}

func TestKeepExistingStillFixesOtherNames(t *testing.T) {
	cfg := &config.Config{GroupDefaultBranch: config.GroupDefaultBranch{BranchName: "master", KeepExisting: true}}
	if d := plan(t, "develop", cfg); d.Status != diff.StatusDrifted {
		t.Errorf("diff = %+v", d)
	}
}

func TestWithoutKeepExistingMainIsDrift(t *testing.T) {
	cfg := &config.Config{GroupDefaultBranch: config.GroupDefaultBranch{BranchName: "master"}}
	if d := plan(t, "main", cfg); d.Status != diff.StatusDrifted {
		t.Errorf("diff = %+v", d)
	}
}
