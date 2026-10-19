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

// fakeGroup serves group 5's protected environments: name -> (access level, required approvals).
type fakeGroup struct {
	mu        sync.Mutex
	protected map[string][2]int
	ruleGroup map[string]int64 // environment -> approver group of its current approval rule
	wrote     []string

	shared    []map[string]any // group 5's shared_with_groups
	denyShare bool
	// failShareAfterUnshare makes the first share POST after an unshare fail.
	failShareAfterUnshare bool
	unshared              bool
	calls                 []string       // ordered: "share", "protect"
	protect               map[string]any // body of the last protect call
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
		rule := map[string]any{"access_level": cur[0], "required_approvals": cur[1]}
		if gid := f.ruleGroup[name]; gid != 0 {
			rule = map[string]any{"group_id": gid, "required_approvals": cur[1]}
		}
		reply(200, map[string]any{
			"name":                 name,
			"deploy_access_levels": []map[string]any{{"access_level": cur[0]}},
			"approval_rules":       []map[string]any{rule},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/groups/5":
		reply(200, map[string]any{"id": 5, "full_path": "acme", "shared_with_groups": f.shared})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v4/groups/5/share/"):
		f.calls = append(f.calls, "unshare")
		f.shared = nil
		f.unshared = true
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v4/groups/5/share":
		if f.failShareAfterUnshare && f.unshared {
			f.failShareAfterUnshare = false
			http.Error(w, `{"message":"422 cannot share"}`, http.StatusUnprocessableEntity)
			return
		}
		if f.denyShare {
			http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
			return
		}
		var body struct {
			GroupID     int64 `json:"group_id"`
			GroupAccess int64 `json:"group_access"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, "share")
		f.shared = append(f.shared, map[string]any{"group_id": body.GroupID, "group_access_level": body.GroupAccess})
		reply(201, map[string]any{"id": 5})
	case r.Method == http.MethodPost && r.URL.Path == base:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["name"].(string)
		f.protect = body
		f.calls = append(f.calls, "protect")
		f.wrote = append(f.wrote, name)
		f.protected[name] = [2]int{40, 1}
		reply(201, map[string]any{"name": name})
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
	if status["production"] != diff.StatusUnchanged || status["staging"] != diff.StatusDrifted || status["other"] != diff.StatusDrifted || len(diffs) != 3 {
		t.Fatalf("plan statuses = %v", status)
	}
	if len(f.wrote) != 2 || f.wrote[0] != "staging" || f.wrote[1] != "other" {
		t.Errorf("production is already right, so only staging and other are written; wrote %v", f.wrote)
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

func otherConfig() *config.Config {
	return &config.Config{ProtectedEnvironments: []config.ProtectedEnvironment{{
		EnvironmentName: "other", DeployAccessLevel: "developer", RequiredApprovalCount: 1,
		ApproverGroupID: 23328, ApproverGroupAccessLevel: "reporter",
	}}}
}

func TestOtherSharesTheApproverGroupFirstThenProtects(t *testing.T) {
	f := &fakeGroup{protected: map[string][2]int{}}
	diffs, results := run(t, f, otherConfig())

	if diffs[0].Status != diff.StatusDrifted || !strings.Contains(diffs[0].Description, "not shared with this group yet") {
		t.Fatalf("plan = %+v", diffs[0])
	}
	if got := strings.Join(f.calls, ","); got != "share,protect" {
		t.Errorf("call order = %s, want share then protect", got)
	}
	if len(f.shared) != 1 || f.shared[0]["group_id"] != int64(23328) || f.shared[0]["group_access_level"] != int64(20) {
		t.Errorf("shared = %v, want group 23328 as Reporter (20)", f.shared)
	}

	// Developers + Maintainers deploy (access level 30); the group approves.
	deploy := f.protect["deploy_access_levels"].([]any)[0].(map[string]any)
	rule := f.protect["approval_rules"].([]any)[0].(map[string]any)
	if deploy["access_level"] != float64(30) || rule["group_id"] != float64(23328) || rule["required_approvals"] != float64(1) {
		t.Errorf("protect body = %v", f.protect)
	}
	if _, roleBased := rule["access_level"]; roleBased {
		t.Errorf("the approval rule must name the group, not a role: %v", rule)
	}
	if results[0].Status != diff.StatusApplied || !strings.Contains(results[0].Description, "shared group 23328 as Reporter") {
		t.Errorf("result = %+v", results[0])
	}
}

func TestOtherDoesNotReshareAnAlreadySharedGroup(t *testing.T) {
	f := &fakeGroup{
		protected: map[string][2]int{},
		shared:    []map[string]any{{"group_id": 23328, "group_access_level": 30}}, // Developer: more than enough
	}
	run(t, f, otherConfig())
	if got := strings.Join(f.calls, ","); got != "protect" {
		t.Errorf("calls = %s, want just protect", got)
	}
}

func TestOtherAlreadyShareAndProtectedIsUnchanged(t *testing.T) {
	f := &fakeGroup{
		protected: map[string][2]int{"other": {30, 1}},
		ruleGroup: map[string]int64{"other": 23328},
		shared:    []map[string]any{{"group_id": 23328, "group_access_level": 20}},
	}
	diffs, _ := run(t, f, otherConfig())
	if diffs[0].Status != diff.StatusUnchanged || len(f.calls) != 0 {
		t.Errorf("diff = %+v, calls = %v", diffs[0], f.calls)
	}
}

func TestOtherWithARoleApprovalRuleIsReconciledToTheGroup(t *testing.T) {
	f := &fakeGroup{
		protected: map[string][2]int{"other": {30, 1}}, // approval is a role, not the group
		shared:    []map[string]any{{"group_id": 23328, "group_access_level": 20}},
	}
	diffs, _ := run(t, f, otherConfig())
	if diffs[0].Status != diff.StatusDrifted || len(f.wrote) != 1 {
		t.Errorf("diff = %+v, wrote %v", diffs[0], f.wrote)
	}
}

func TestOtherSharedBelowReporterIsReSharedAsReporter(t *testing.T) {
	f := &fakeGroup{
		protected: map[string][2]int{},
		shared:    []map[string]any{{"group_id": 23328, "group_access_level": 10}}, // Guest
	}
	diffs, results := run(t, f, otherConfig())
	if diffs[0].Status != diff.StatusDrifted || !strings.Contains(diffs[0].Description, "will re-share it as Reporter") {
		t.Errorf("plan = %+v", diffs[0])
	}
	if got := strings.Join(f.calls, ","); got != "unshare,share,protect" {
		t.Errorf("call order = %s, want unshare, share, protect", got)
	}
	if len(f.shared) != 1 || f.shared[0]["group_access_level"] != int64(20) {
		t.Errorf("shared = %v, want one share at Reporter (20)", f.shared)
	}
	if results[0].Status != diff.StatusApplied || !strings.Contains(results[0].Description, "re-shared group 23328 as Reporter") {
		t.Errorf("result = %+v", results[0])
	}
}

func TestOtherRestoresTheOldShareWhenReSharingFails(t *testing.T) {
	f := &fakeGroup{
		protected:             map[string][2]int{},
		shared:                []map[string]any{{"group_id": 23328, "group_access_level": 10}},
		failShareAfterUnshare: true,
	}
	_, results := run(t, f, otherConfig())
	if results[0].Status != diff.StatusFailed || !strings.Contains(results[0].Error, "previous Guest share was restored") {
		t.Errorf("result = %+v", results[0])
	}
	if len(f.shared) != 1 || f.shared[0]["group_access_level"] != int64(10) {
		t.Errorf("the Guest share must be back: %v", f.shared)
	}
	if len(f.wrote) != 0 {
		t.Errorf("the environment must not be protected: wrote %v", f.wrote)
	}
}

func TestOtherIsNotProtectedWhenTheShareFails(t *testing.T) {
	f := &fakeGroup{protected: map[string][2]int{}, denyShare: true}
	_, results := run(t, f, otherConfig())
	if results[0].Status != diff.StatusFailed || !strings.Contains(results[0].Error, "sharing group 23328") {
		t.Errorf("result = %+v", results[0])
	}
	if len(f.wrote) != 0 {
		t.Errorf("the environment must not be protected without the share: wrote %v", f.wrote)
	}
}

func TestDesiredStateProtectsProductionStagingAndOther(t *testing.T) {
	cfg, err := config.Load("../../../configs/desired-state.yaml")
	if err != nil {
		t.Fatal(err)
	}
	envs, err := desiredEnvironments(cfg)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range envs {
		names = append(names, e.name)
	}
	if strings.Join(names, ",") != "production,staging,other" {
		t.Fatalf("environments = %v", names)
	}
	other := envs[2]
	if other.level != 30 || other.approverGroup != 23328 || !other.ensureShared || other.shareLevel != 20 {
		t.Errorf("other = %+v", other)
	}
}
