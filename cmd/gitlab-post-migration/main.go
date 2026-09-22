// Command gitlab-post-migration reconciles a batch of already-migrated
// GitLab projects/groups against a static desired-state baseline. It's
// meant to be invoked entirely from GitLab CI jobs (see .gitlab-ci.yml):
// one `discover`, then one `plan`/`apply` pair per task, then one `report`
// that merges every task's output into a single pipeline-run report.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tfonji/gitlab-post-migration/internal/config"
	"github.com/tfonji/gitlab-post-migration/internal/discovery"
	"github.com/tfonji/gitlab-post-migration/internal/gitlabclient"
	"github.com/tfonji/gitlab-post-migration/internal/report"
	"github.com/tfonji/gitlab-post-migration/internal/task"

	// Task implementations register themselves via their Register(...)
	// constructor called from registerTasks below — imported here so the
	// binary links them in.
	"github.com/tfonji/gitlab-post-migration/internal/task/complianceframework"
	"github.com/tfonji/gitlab-post-migration/internal/task/defaultbranchrename"
	"github.com/tfonji/gitlab-post-migration/internal/task/groupdefaultbranch"
	"github.com/tfonji/gitlab-post-migration/internal/task/mrapprovalpolicy"
	"github.com/tfonji/gitlab-post-migration/internal/task/protectedenvironment"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "discover":
		err = runDiscover(os.Args[2:])
	case "plan":
		err = runPlanOrApply(os.Args[2:], report.ModePlan)
	case "apply":
		err = runPlanOrApply(os.Args[2:], report.ModeApply)
	case "report":
		err = runReport(os.Args[2:])
	case "list-tasks":
		err = runListTasks()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `gitlab-post-migration <command> [flags]

Commands:
  discover     resolve a group/project IDs into a scope (projects.json)
  plan         diff live state against desired config for one task
  apply        reconcile the diffs from a prior plan for one task
  report       merge task result files into one pipeline-run report
  list-tasks   print every registered task name`)
}

func registerTasks(c *gitlabclient.Client) {
	defaultbranchrename.Register(c)
	mrapprovalpolicy.Register(c)
	protectedenvironment.Register(c)
	groupdefaultbranch.Register(c)
	complianceframework.Register(c)
}

func runListTasks() error {
	registerTasks(nil)
	for _, name := range task.Names() {
		fmt.Println(name)
	}
	return nil
}

func runDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	gitlabURL := fs.String("gitlab-url", envOr("CI_SERVER_URL", "https://gitlab.com"), "GitLab base URL")
	token := fs.String("token", os.Getenv("GITLAB_TOKEN"), "GitLab API token")
	groupIDFlag := fs.String("group", os.Getenv("GROUP_ID"), "top-level group ID to scan (recurses subgroups)")
	projectIDsFlag := fs.String("projects", os.Getenv("PROJECT_IDS"), "comma-separated explicit project IDs")
	out := fs.String("out", "projects.json", "output scope file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := gitlabclient.New(*gitlabURL, *token)
	if err != nil {
		return err
	}

	var groupID *int64
	if strings.TrimSpace(*groupIDFlag) != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(*groupIDFlag), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid --group %q: %w", *groupIDFlag, err)
		}
		groupID = &id
	}

	projectIDs, err := parseIntList(*projectIDsFlag)
	if err != nil {
		return fmt.Errorf("invalid --projects: %w", err)
	}
	if groupID == nil && len(projectIDs) == 0 {
		return fmt.Errorf("at least one of --group or --projects must be set")
	}

	scope, err := discovery.Resolve(context.Background(), c, groupID, projectIDs)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "discovered %d project(s) across %d top-level group(s)\n", len(scope.Projects), len(scope.TopLevelGroups))
	return writeJSON(*out, scope)
}

func runPlanOrApply(args []string, mode report.Mode) error {
	fs := flag.NewFlagSet(string(mode), flag.ExitOnError)
	gitlabURL := fs.String("gitlab-url", envOr("CI_SERVER_URL", "https://gitlab.com"), "GitLab base URL")
	token := fs.String("token", os.Getenv("GITLAB_TOKEN"), "GitLab API token")
	taskName := fs.String("task", "", "task name (see list-tasks)")
	scopeFile := fs.String("scope", "projects.json", "scope file produced by discover")
	configFile := fs.String("config", "configs/desired-state.yaml", "desired-state config file")
	planFile := fs.String("plan", "", "plan file produced by a prior `plan` run (apply only)")
	out := fs.String("out", "", "output file (default plan-<task>.json / result-<task>.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskName == "" {
		return fmt.Errorf("--task is required")
	}
	if *out == "" {
		*out = fmt.Sprintf("%s-%s.json", mode, *taskName)
	}

	c, err := gitlabclient.New(*gitlabURL, *token)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}

	registerTasks(c)
	t, ok := task.Get(*taskName)
	if !ok {
		return fmt.Errorf("unknown task %q (see list-tasks)", *taskName)
	}

	ctx := context.Background()

	if mode == report.ModePlan {
		scope, err := loadScope(*scopeFile)
		if err != nil {
			return err
		}
		diffs, err := t.Plan(ctx, scope, cfg)
		if err != nil {
			return err
		}
		run := report.TaskRun{Task: *taskName, Mode: report.ModePlan, Diffs: diffs}
		report.BuildTaskReport(run).WriteTable(os.Stderr)
		return writeJSON(*out, run)
	}

	if *planFile == "" {
		*planFile = fmt.Sprintf("plan-%s.json", *taskName)
	}
	planRun, err := report.LoadTaskRun(*planFile)
	if err != nil {
		return fmt.Errorf("reading plan file %s: %w", *planFile, err)
	}
	results, err := t.Apply(ctx, planRun.Diffs, cfg)
	if err != nil {
		return err
	}
	run := report.TaskRun{Task: *taskName, Mode: report.ModeApply, Results: results}
	report.BuildTaskReport(run).WriteTable(os.Stderr)
	return writeJSON(*out, run)
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	inGlob := fs.String("in", "result-*.json", "glob of task run files to merge (falls back to plan-*.json if none match)")
	jsonOut := fs.String("json-out", "report.json", "combined JSON report path")
	htmlOut := fs.String("html-out", "report.html", "combined HTML report path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	matches, err := filepath.Glob(*inGlob)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		matches, err = filepath.Glob("plan-*.json")
		if err != nil {
			return err
		}
	}

	runs := make([]report.TaskRun, 0, len(matches))
	for _, m := range matches {
		run, err := report.LoadTaskRun(m)
		if err != nil {
			return err
		}
		runs = append(runs, run)
	}

	r := report.Merge(runs)
	if err := r.WriteJSON(*jsonOut); err != nil {
		return err
	}
	if err := r.WriteHTML(*htmlOut); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "report: %s\n", r.Stats)
	if r.Stats.Failed > 0 {
		return fmt.Errorf("%d target(s) failed", r.Stats.Failed)
	}
	return nil
}

func loadScope(path string) (discovery.Scope, error) {
	var scope discovery.Scope
	data, err := os.ReadFile(path)
	if err != nil {
		return scope, err
	}
	if err := json.Unmarshal(data, &scope); err != nil {
		return scope, fmt.Errorf("decoding scope %s: %w", path, err)
	}
	return scope, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func parseIntList(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
