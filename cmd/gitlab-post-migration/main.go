// Command gitlab-post-migration reconciles a batch of already-migrated
// GitLab projects/groups against a static desired-state baseline. It's
// meant to be invoked entirely from GitLab CI jobs (see .gitlab-ci.yml):
// one `discover`, then one `plan`/`apply` pair per task, then one `report`
// that merges every task's output into a single pipeline-run report.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitlab-post-migration/internal/cleanup"
	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
	"gitlab-post-migration/internal/membership"
	"gitlab-post-migration/internal/report"
	"gitlab-post-migration/internal/task"

	// Task implementations register themselves via their Register(...)
	// constructor called from registerTasks below — imported here so the
	// binary links them in.
	"gitlab-post-migration/internal/task/complianceframework"
	"gitlab-post-migration/internal/task/defaultbranchrename"
	"gitlab-post-migration/internal/task/groupdefaultbranch"
	"gitlab-post-migration/internal/task/mrapprovalpolicy"
	"gitlab-post-migration/internal/task/protectedenvironment"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	setupLogging()

	var err error
	switch os.Args[1] {
	case "discover":
		err = runDiscover(os.Args[2:])
	case "cleanup":
		err = runCleanup(os.Args[2:])
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
		slog.Error("run failed", "command", os.Args[1], "error", err)
		os.Exit(1)
	}
}

// setupLogging sends structured logs to stderr (alongside the result
// tables). LOG_LEVEL=debug additionally logs every successful API call;
// the default (info) logs identity, run context, and every failed call.
func setupLogging() {
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// newClient builds the API client and logs who it runs as, so every job log
// starts with the identity and CI context behind the calls that follow.
func newClient(ctx context.Context, gitlabURL, token string) (*gitlabclient.Client, error) {
	c, err := gitlabclient.New(gitlabURL, token)
	if err != nil {
		return nil, err
	}
	gitlabclient.LogCIContext()
	c.LogIdentity(ctx)
	return c, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `gitlab-post-migration <command> [flags]

Commands:
  discover     resolve a group/project IDs into a scope (projects.json)
  cleanup      unlink the security policy project and remove the compliance
               framework from each top-level group, ahead of plan/apply
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
	groupIDFlag := fs.String("group", os.Getenv("GROUP_ID"), "comma-separated group IDs to scan (each recurses subgroups)")
	projectIDsFlag := fs.String("projects", os.Getenv("PROJECT_IDS"), "comma-separated explicit project IDs")
	out := fs.String("out", "projects.json", "output scope file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	c, err := newClient(ctx, *gitlabURL, *token)
	if err != nil {
		return err
	}

	groupIDs, err := parseIntList(*groupIDFlag)
	if err != nil {
		return fmt.Errorf("invalid --group %q: %w", *groupIDFlag, err)
	}
	projectIDs, err := parseIntList(*projectIDsFlag)
	if err != nil {
		return fmt.Errorf("invalid --projects: %w", err)
	}
	if len(groupIDs) == 0 && len(projectIDs) == 0 {
		return fmt.Errorf("at least one of --group or --projects must be set")
	}
	slog.Info("discovering", "groups", groupIDs, "projects", projectIDs)

	scope, err := discovery.Resolve(ctx, c, groupIDs, projectIDs)
	if err != nil {
		return err
	}

	slog.Info("discovered", "projects", len(scope.Projects), "top_level_groups", len(scope.TopLevelGroups))
	return writeJSON(*out, scope)
}

// runCleanup removes what would make later apply jobs fail (a linked
// security policy project, the configured compliance framework) from every
// top-level group in scope. It mutates immediately, and must run before the
// plan jobs so they read the cleaned state. It processes every group even if
// some fail, then fails the job so the pipeline doesn't proceed to plan.
func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	gitlabURL := fs.String("gitlab-url", envOr("CI_SERVER_URL", "https://gitlab.com"), "GitLab base URL")
	token := fs.String("token", os.Getenv("GITLAB_TOKEN"), "GitLab API token")
	scopeFile := fs.String("scope", "projects.json", "scope file produced by discover")
	configFile := fs.String("config", "configs/desired-state.yaml", "desired-state config file")
	out := fs.String("out", "cleanup-result.json", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("starting", "command", "cleanup")
	c, err := newClient(ctx, *gitlabURL, *token)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	scope, err := loadScope(*scopeFile)
	if err != nil {
		return err
	}

	results, err := cleanup.Run(ctx, c, scope, cfg)
	if err != nil {
		return err
	}
	run := report.TaskRun{Task: cleanup.Name, Mode: report.ModeApply, Origin: report.CurrentOrigin(), Results: results}
	tr := report.BuildTaskReport(run)
	terminal().WriteTask(tr)
	if err := writeJSON(*out, run); err != nil {
		return err
	}
	if tr.Stats.Failed > 0 {
		return fmt.Errorf("cleanup failed for %d check(s); fix and re-run before planning", tr.Stats.Failed)
	}
	return nil
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

	// A cancelled CI job (SIGTERM) cancels in-flight API calls, so apply can
	// still revert any temporary group membership before exiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("starting", "mode", mode, "task", *taskName)
	c, err := newClient(ctx, *gitlabURL, *token)
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

	if mode == report.ModePlan {
		scope, err := loadScope(*scopeFile)
		if err != nil {
			return err
		}
		diffs, err := t.Plan(ctx, scope, cfg)
		if err != nil {
			return err
		}
		run := report.TaskRun{Task: *taskName, Mode: report.ModePlan, Origin: report.CurrentOrigin(), Diffs: diffs}
		terminal().WriteTask(report.BuildTaskReport(run))
		return writeJSON(*out, run)
	}

	if *planFile == "" {
		*planFile = fmt.Sprintf("plan-%s.json", *taskName)
	}
	planRun, err := report.LoadTaskRun(*planFile)
	if err != nil {
		return fmt.Errorf("reading plan file %s: %w", *planFile, err)
	}
	logPlanOrigin(*planFile, planRun.Origin)
	results, err := applyWithMembership(ctx, c, t, planRun.Diffs, cfg)
	if results == nil && err != nil {
		return err
	}
	run := report.TaskRun{Task: *taskName, Mode: report.ModeApply, Origin: report.CurrentOrigin(), Results: results}
	terminal().WriteTask(report.BuildTaskReport(run))
	if werr := writeJSON(*out, run); werr != nil {
		return errors.Join(err, werr)
	}
	// err here is a failed membership revert: the results are written, but
	// the job must fail so someone removes the leftover membership.
	return err
}

// applyWithMembership runs t.Apply, first granting the token user
// Maintainer on every top-level group the drifted diffs belong to when the
// task needs it, and always reverting that grant afterwards -- on success,
// failure, panic, or a cancelled job. Results are returned even when only
// the revert failed, so they still get written.
func applyWithMembership(ctx context.Context, c *gitlabclient.Client, t task.Task, diffs []diff.Diff, cfg *config.Config) (results []diff.Result, err error) {
	req, ok := t.(task.GroupMembershipRequirer)
	if !ok || !req.RequiresGroupMembership() {
		return t.Apply(ctx, diffs, cfg)
	}

	groupIDs, err := driftedGroupIDs(diffs)
	if err != nil {
		return nil, err
	}
	if len(groupIDs) == 0 {
		return t.Apply(ctx, diffs, cfg)
	}

	grants, err := membership.Ensure(ctx, c, groupIDs)
	defer func() {
		// Fresh context: ctx may already be cancelled, and the revert
		// matters most exactly then.
		revertCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if rerr := membership.Revert(revertCtx, c, grants); rerr != nil {
			slog.Error("could not revert temporary group membership -- remove it by hand", "error", rerr)
			err = errors.Join(err, rerr)
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("granting token user group membership: %w", err)
	}
	return t.Apply(ctx, diffs, cfg)
}

// driftedGroupIDs returns the distinct top-level groups of the diffs apply
// will act on.
func driftedGroupIDs(diffs []diff.Diff) ([]int64, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			continue
		}
		gid := d.Target.TopLevelGroupID
		if gid == 0 {
			return nil, fmt.Errorf("plan file has no top-level group for %s (produced by an older build) -- re-run the plan job", d.Target.Path)
		}
		if !seen[gid] {
			seen[gid] = true
			ids = append(ids, gid)
		}
	}
	return ids, nil
}

// logPlanOrigin says which plan apply is about to act on. Retrying an apply
// job replays the same plan artifact, so if an earlier attempt already made
// some of its changes, the plan is stale -- the age and job here make that
// visible, and the fix is re-running the plan job.
func logPlanOrigin(path string, o *report.RunOrigin) {
	if o == nil {
		slog.Warn("plan file has no origin info (produced by an older build)", "plan", path)
		return
	}
	slog.Info("applying plan",
		"plan", path,
		"generated_at", o.GeneratedAt.Format(time.RFC3339),
		"age", time.Since(o.GeneratedAt).Round(time.Second),
		"plan_pipeline_id", o.PipelineID,
		"plan_job_id", o.JobID,
		"plan_commit", o.CommitSHA,
	)
}

func terminal() *report.Terminal {
	return report.NewTerminal(os.Stderr, report.UseColor(os.Stderr))
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

	// Cleanup results are always included on top of whichever of the
	// result/plan sets applies -- they must not count as "a result file
	// exists" and hide the plan-only view.
	cleanupFiles, err := filepath.Glob("cleanup-*.json")
	if err != nil {
		return err
	}
	matches = append(cleanupFiles, matches...)

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
	terminal().WriteReport(r)
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
