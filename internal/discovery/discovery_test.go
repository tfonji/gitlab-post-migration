package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab-post-migration/internal/gitlabclient"
)

// fakeInstance: group 20 "acme" (top level) > group 30 "acme/team" (subgroup)
// > group 40 "acme/team/deep". Project 100 lives in acme/team; groups 30 and
// 40 hold no projects of their own otherwise.
func fakeInstance() http.Handler {
	groups := map[string]map[string]any{
		"/api/v4/groups/20": {"id": 20, "full_path": "acme", "parent_id": nil},
		"/api/v4/groups/30": {"id": 30, "full_path": "acme/team", "parent_id": 20},
		"/api/v4/groups/40": {"id": 40, "full_path": "acme/team/deep", "parent_id": 30},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if g, ok := groups[r.URL.Path]; ok {
			_ = json.NewEncoder(w).Encode(g)
			return
		}
		switch r.URL.Path {
		case "/api/v4/groups/30/projects":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 100, "path_with_namespace": "acme/team/app", "default_branch": "main",
				"namespace": map[string]any{"id": 30, "parent_id": 20, "full_path": "acme/team"},
			}})
		case "/api/v4/groups/20/projects": // includes the subgroup's project
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 100, "path_with_namespace": "acme/team/app", "default_branch": "main",
				"namespace": map[string]any{"id": 30, "parent_id": 20, "full_path": "acme/team"},
			}})
		case "/api/v4/groups/40/projects":
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // an empty subgroup
		default:
			http.NotFound(w, r)
		}
	})
}

func resolve(t *testing.T, groups, projects []int64) Scope {
	t.Helper()
	srv := httptest.NewServer(fakeInstance())
	t.Cleanup(srv.Close)
	c, err := gitlabclient.New(srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := Resolve(context.Background(), c, groups, projects)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestSubgroupWithProjectsResolvesToItsTopLevelGroup(t *testing.T) {
	scope := resolve(t, []int64{30}, nil)
	if len(scope.TopLevelGroups) != 1 || scope.TopLevelGroups[0].ID != 20 || scope.TopLevelGroups[0].FullPath != "acme" {
		t.Errorf("top-level groups = %+v, want just acme (20)", scope.TopLevelGroups)
	}
	if len(scope.Projects) != 1 || scope.Projects[0].TopLevelGroupID != 20 {
		t.Errorf("projects = %+v", scope.Projects)
	}
}

func TestEmptySubgroupStillResolvesToItsTopLevelGroup(t *testing.T) {
	scope := resolve(t, []int64{40}, nil)
	if len(scope.Projects) != 0 {
		t.Fatalf("projects = %+v", scope.Projects)
	}
	if len(scope.TopLevelGroups) != 1 || scope.TopLevelGroups[0].ID != 20 {
		t.Errorf("top-level groups = %+v, want acme (20) even though group 40 has no projects", scope.TopLevelGroups)
	}
}

func TestSeveralSubgroupsOfOneTopLevelGroupAreListedOnce(t *testing.T) {
	scope := resolve(t, []int64{30, 40, 20}, nil)
	if len(scope.TopLevelGroups) != 1 || scope.TopLevelGroups[0].ID != 20 {
		t.Errorf("top-level groups = %+v, want acme once", scope.TopLevelGroups)
	}
}
