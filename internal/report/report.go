// Package report aggregates plan/apply output from every task into a single
// per-pipeline-run report: one JSON file (machine-readable) and one HTML
// file (human-readable), each with an overall stats summary plus a
// per-task breakdown.
package report

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"strings"
	"time"

	"github.com/tfonji/gitlab-post-migration/internal/diff"
)

// Mode is whether a task run represents a plan (dry-run) or an apply.
type Mode string

const (
	ModePlan  Mode = "plan"
	ModeApply Mode = "apply"
)

// TaskRun is what each `plan`/`apply` CLI invocation writes to
// result-<task>.json; the `report` command reads one of these per task and
// merges them.
type TaskRun struct {
	Task    string        `json:"task"`
	Mode    Mode          `json:"mode"`
	Origin  *RunOrigin    `json:"origin,omitempty"`  // where/when this file was produced
	Diffs   []diff.Diff   `json:"diffs,omitempty"`   // set when Mode == ModePlan
	Results []diff.Result `json:"results,omitempty"` // set when Mode == ModeApply
}

// RunOrigin records where a plan/result file came from, so apply can log
// which plan it's acting on -- e.g. to spot a retried apply job replaying a
// plan whose changes were already partly made.
type RunOrigin struct {
	GeneratedAt time.Time `json:"generated_at"`
	PipelineID  string    `json:"pipeline_id,omitempty"`
	JobID       string    `json:"job_id,omitempty"`
	CommitSHA   string    `json:"commit_sha,omitempty"`
}

// CurrentOrigin describes the running process, from GitLab CI's predefined
// variables when present.
func CurrentOrigin() *RunOrigin {
	return &RunOrigin{
		GeneratedAt: time.Now().UTC(),
		PipelineID:  os.Getenv("CI_PIPELINE_ID"),
		JobID:       os.Getenv("CI_JOB_ID"),
		CommitSHA:   os.Getenv("CI_COMMIT_SHA"),
	}
}

type Stats struct {
	Total     int `json:"total"`
	Unchanged int `json:"unchanged"`
	Drifted   int `json:"drifted"`
	Applied   int `json:"applied"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func (s *Stats) add(status diff.Status) {
	s.Total++
	switch status {
	case diff.StatusUnchanged:
		s.Unchanged++
	case diff.StatusDrifted:
		s.Drifted++
	case diff.StatusApplied:
		s.Applied++
	case diff.StatusFailed:
		s.Failed++
	case diff.StatusSkipped:
		s.Skipped++
	}
}

func (s *Stats) merge(other Stats) {
	s.Total += other.Total
	s.Unchanged += other.Unchanged
	s.Drifted += other.Drifted
	s.Applied += other.Applied
	s.Failed += other.Failed
	s.Skipped += other.Skipped
}

// String renders a one-line stats summary, e.g.
// "total=20 unchanged=15 drifted=4 applied=0 failed=1 skipped=0".
func (s Stats) String() string {
	return fmt.Sprintf("total=%d unchanged=%d drifted=%d applied=%d failed=%d skipped=%d",
		s.Total, s.Unchanged, s.Drifted, s.Applied, s.Failed, s.Skipped)
}

type Entry struct {
	TargetKind  diff.TargetKind `json:"target_kind"`
	TargetPath  string          `json:"target_path"`
	Status      diff.Status     `json:"status"`
	Description string          `json:"description"`
	Error       string          `json:"error,omitempty"`
}

type TaskReport struct {
	Task    string  `json:"task"`
	Mode    Mode    `json:"mode"`
	Stats   Stats   `json:"stats"`
	Entries []Entry `json:"entries"`
}

// Report is the single combined artifact for a whole pipeline run.
type Report struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Stats       Stats        `json:"stats"`
	Tasks       []TaskReport `json:"tasks"`
}

// BuildTaskReport converts one TaskRun into a TaskReport (stats + entries).
// It's the same conversion Merge does per task, exposed separately so the
// CLI can print a table right after plan/apply -- in the job log, not just
// in the final `report` job's artifact.
func BuildTaskReport(run TaskRun) TaskReport {
	tr := TaskReport{Task: run.Task, Mode: run.Mode}
	if run.Mode == ModePlan {
		for _, d := range run.Diffs {
			tr.Stats.add(d.Status)
			tr.Entries = append(tr.Entries, Entry{
				TargetKind: d.Target.Kind, TargetPath: d.Target.Path,
				Status: d.Status, Description: d.Description,
			})
		}
	} else {
		for _, res := range run.Results {
			tr.Stats.add(res.Status)
			tr.Entries = append(tr.Entries, Entry{
				TargetKind: res.Target.Kind, TargetPath: res.Target.Path,
				Status: res.Status, Description: res.Description, Error: res.Error,
			})
		}
	}
	return tr
}

// Merge combines one TaskRun per task into a single Report.
func Merge(runs []TaskRun) Report {
	r := Report{GeneratedAt: time.Now().UTC()}
	for _, run := range runs {
		tr := BuildTaskReport(run)
		r.Stats.merge(tr.Stats)
		r.Tasks = append(r.Tasks, tr)
	}
	return r
}

func LoadTaskRun(path string) (TaskRun, error) {
	var run TaskRun
	data, err := os.ReadFile(path)
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return run, fmt.Errorf("decoding task run %s: %w", path, err)
	}
	return run, nil
}

func (r Report) WriteJSON(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// reportView is the shape the HTML template actually renders -- WriteHTML
// derives it from Report so the template stays a dumb renderer (badge
// classes, banner text, and the per-target-kind split are all decided in
// Go, not in template logic).
type reportView struct {
	GeneratedAt time.Time
	Stats       Stats
	BannerClass string
	BannerText  string
	HealthOK    int
	HealthWarn  int
	HealthBad   int
	Tasks       []taskView
}

type taskView struct {
	Task       string
	Mode       Mode
	Slug       string
	Stats      Stats
	BadgeClass string
	BadgeText  string
	Groups     []entryView
	Projects   []entryView
}

type entryView struct {
	TargetPath  string
	LabelClass  string
	LabelText   string
	Description string
	Error       string
}

func buildReportView(r Report) reportView {
	v := reportView{GeneratedAt: r.GeneratedAt, Stats: r.Stats}
	v.BannerClass, v.BannerText = reportBanner(r)

	for _, t := range r.Tasks {
		tv := taskView{
			Task:  t.Task,
			Mode:  t.Mode,
			Slug:  slugify(t.Task),
			Stats: t.Stats,
		}
		tv.BadgeClass, tv.BadgeText = taskBadge(t)

		for _, e := range t.Entries {
			ev := entryView{TargetPath: e.TargetPath, Description: e.Description, Error: e.Error}
			ev.LabelClass, ev.LabelText = entryLabel(e)
			if e.TargetKind == diff.TargetGroup {
				tv.Groups = append(tv.Groups, ev)
			} else {
				tv.Projects = append(tv.Projects, ev)
			}
		}

		switch {
		case t.Stats.Failed > 0:
			v.HealthBad++
		case t.Stats.Applied > 0 || t.Stats.Drifted > 0:
			v.HealthWarn++
		default:
			v.HealthOK++
		}

		v.Tasks = append(v.Tasks, tv)
	}
	return v
}

// reportBanner picks the top-of-page banner. A failure anywhere outranks
// everything else; otherwise a report containing any plan-mode task is
// flagged as not-yet-final (plan and apply results never mix within one
// report in practice -- see runReport's glob fallback -- but this stays
// correct even if they did).
func reportBanner(r Report) (class, text string) {
	if r.Stats.Failed > 0 {
		return "has-failures", fmt.Sprintf("✗ %d target(s) failed — review required", r.Stats.Failed)
	}
	if hasPlanTask(r.Tasks) {
		if r.Stats.Applied > 0 {
			return "dry-run", "🔍 Plan mode — the only changes made so far are from cleanup. Status below shows what apply would do."
		}
		return "dry-run", "🔍 Plan mode — no changes have been made. Status below shows what apply would do."
	}
	return "clean", fmt.Sprintf("✓ Apply complete — %d applied, %d unchanged, %d skipped", r.Stats.Applied, r.Stats.Unchanged, r.Stats.Skipped)
}

func taskBadge(t TaskReport) (class, text string) {
	if t.Stats.Failed > 0 {
		return "has-failures", fmt.Sprintf("✗ %d failed", t.Stats.Failed)
	}
	if t.Mode == ModePlan {
		if t.Stats.Drifted > 0 {
			return "dry-run", fmt.Sprintf("~ %d drifted", t.Stats.Drifted)
		}
		return "clean", "✓ no drift"
	}
	if t.Stats.Applied > 0 {
		return "has-changes", fmt.Sprintf("✓ %d applied", t.Stats.Applied)
	}
	return "clean", "✓ no changes needed"
}

func entryLabel(e Entry) (class, text string) {
	switch e.Status {
	case diff.StatusApplied:
		return "applied", "Applied"
	case diff.StatusDrifted:
		return "drifted", "Drifted"
	case diff.StatusUnchanged:
		return "neutral", "Unchanged"
	case diff.StatusSkipped:
		return "neutral", "Skipped"
	case diff.StatusFailed:
		return "failed", "Failed"
	default:
		return "neutral", string(e.Status)
	}
}

func slugify(s string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ".", "-", "_", "-")
	return r.Replace(s)
}

func (r Report) WriteHTML(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return htmlTemplate.Execute(f, buildReportView(r))
}

var htmlTemplate = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>GitLab Post-Migration Report</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f5f5f5; color: #333; font-size: 14px; }
  header { background: #1a1a2e; color: white; padding: 20px 32px; display: flex; align-items: center; justify-content: space-between; }
  header h1 { font-size: 20px; font-weight: 600; }
  header .meta { font-size: 12px; color: #aaa; }
  .status-banner { padding: 12px 32px; font-weight: 600; font-size: 14px; }
  .status-banner.has-failures { background: #fdecea; color: #c0392b; border-left: 4px solid #c0392b; }
  .status-banner.clean { background: #e8f8f0; color: #1e8449; border-left: 4px solid #1e8449; }
  .status-banner.dry-run { background: #eaf4ff; color: #1a73e8; border-left: 4px solid #1a73e8; }
  .container { max-width: 1100px; margin: 24px auto; padding: 0 24px; }
  .stats-grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 16px; margin-bottom: 24px; }
  .stats-card { background: white; border-radius: 8px; box-shadow: 0 1px 4px rgba(0,0,0,.08); padding: 16px 20px; }
  .stats-card h3 { font-size: 11px; font-weight: 700; text-transform: uppercase; letter-spacing: .08em; color: #888; margin-bottom: 12px; }
  .stats-numbers { display: flex; gap: 20px; flex-wrap: wrap; }
  .stats-num { text-align: center; }
  .stats-num .val { font-size: 28px; font-weight: 700; line-height: 1; }
  .stats-num .lbl { font-size: 11px; color: #888; margin-top: 3px; }
  .stats-num.good .val    { color: #1e8449; }
  .stats-num.warn .val    { color: #e67e22; }
  .stats-num.neutral .val { color: #333; }
  .stats-num.bad .val     { color: #c0392b; }
  .task-stats-table { width: 100%; border-collapse: collapse; font-size: 12px; }
  .task-stats-table th { text-align: right; padding: 4px 8px; font-size: 11px; font-weight: 700; text-transform: uppercase; letter-spacing: .05em; color: #888; border-bottom: 1px solid #eee; }
  .task-stats-table th:first-child { text-align: left; }
  .task-stats-table td { padding: 5px 8px; text-align: right; border-bottom: 1px solid #f5f5f5; font-family: monospace; }
  .task-stats-table td:first-child { text-align: left; font-family: monospace; color: #555; }
  .task-stats-table tr:last-child td { border-bottom: none; }
  .task-stats-table tr:hover td { background: #fafafa; }
  .tst-applied { color: #1e8449; }
  .tst-drifted { color: #e67e22; }
  .tst-failed  { color: #c0392b; font-weight: 600; }
  .tst-zero    { color: #ddd; }
  .toggle-all { background: none; border: 1px solid #ddd; border-radius: 6px; padding: 6px 14px; font-size: 12px; cursor: pointer; color: #555; margin-bottom: 16px; }
  .toggle-all:hover { background: #f5f5f5; }
  .task-card { background: white; border-radius: 8px; box-shadow: 0 1px 4px rgba(0,0,0,.08); margin-bottom: 16px; overflow: hidden; }
  .task-header { padding: 14px 20px; cursor: pointer; display: flex; align-items: center; justify-content: space-between; user-select: none; }
  .task-header:hover { background: #fafafa; }
  .task-header h2 { font-size: 15px; font-weight: 600; font-family: monospace; }
  .task-header .mode { font-size: 11px; color: #888; font-weight: 400; margin-left: 8px; text-transform: uppercase; }
  .task-header .badges { display: flex; gap: 8px; align-items: center; }
  .badge { padding: 2px 8px; border-radius: 12px; font-size: 11px; font-weight: 600; }
  .badge.has-changes { background: #e8f4fd; color: #1a73e8; }
  .badge.has-failures { background: #fdecea; color: #c0392b; }
  .badge.clean { background: #e8f8f0; color: #1e8449; }
  .badge.dry-run { background: #eaf4ff; color: #1a73e8; }
  .chevron { transition: transform .2s; color: #999; }
  .task-body { padding: 0 20px 16px; display: none; }
  .task-body.open { display: block; }
  .section-title { font-size: 11px; font-weight: 700; text-transform: uppercase; letter-spacing: .08em; color: #888; margin: 16px 0 8px; }
  .item-list { margin-top: 2px; }
  .item-row { display: flex; align-items: baseline; gap: 8px; padding: 3px 0; font-size: 13px; flex-wrap: wrap; }
  .item-label { font-size: 11px; font-weight: 700; padding: 1px 6px; border-radius: 10px; text-transform: uppercase; white-space: nowrap; }
  .item-label.applied  { background: #e8f8f0; color: #1e8449; }
  .item-label.drifted  { background: #fff8e1; color: #e67e22; }
  .item-label.neutral  { background: #f5f5f5; color: #999; }
  .item-label.failed   { background: #fdecea; color: #c0392b; }
  .item-key { color: #333; font-family: monospace; }
  .item-desc { color: #777; font-size: 12px; }
  .item-err { color: #c0392b; font-size: 12px; }
  .empty-section { color: #aaa; font-size: 12px; font-style: italic; }
</style>
</head>
<body>
<header>
  <h1>GitLab Post-Migration Report</h1>
  <div class="meta">Generated: {{ .GeneratedAt.Format "2006-01-02 15:04:05 MST" }}</div>
</header>
<div class="status-banner {{ .BannerClass }}">{{ .BannerText }}</div>
<div class="container">

  <div class="stats-grid">
    <div class="stats-card">
      <h3>Run Summary</h3>
      <div class="stats-numbers">
        <div class="stats-num good"><div class="val">{{ .Stats.Applied }}</div><div class="lbl">Applied</div></div>
        <div class="stats-num warn"><div class="val">{{ .Stats.Drifted }}</div><div class="lbl">Drifted</div></div>
        <div class="stats-num neutral"><div class="val">{{ .Stats.Unchanged }}</div><div class="lbl">Unchanged</div></div>
        <div class="stats-num neutral"><div class="val">{{ .Stats.Skipped }}</div><div class="lbl">Skipped</div></div>
        <div class="stats-num bad"><div class="val">{{ .Stats.Failed }}</div><div class="lbl">Failed</div></div>
      </div>
    </div>

    <div class="stats-card">
      <h3>Task Health</h3>
      <div class="stats-numbers">
        <div class="stats-num good"><div class="val">{{ .HealthOK }}</div><div class="lbl">In Sync</div></div>
        <div class="stats-num warn"><div class="val">{{ .HealthWarn }}</div><div class="lbl">Had Changes</div></div>
        <div class="stats-num bad"><div class="val">{{ .HealthBad }}</div><div class="lbl">Had Failures</div></div>
      </div>
    </div>

    <div class="stats-card">
      <h3>By Task</h3>
      <table class="task-stats-table">
        <thead><tr><th>Task</th><th>Applied</th><th>Drifted</th><th>Failed</th></tr></thead>
        <tbody>
        {{ range .Tasks }}
          <tr>
            <td><a href="#task-{{ .Slug }}" style="color:#555;text-decoration:none">{{ .Task }}</a></td>
            {{ if .Stats.Applied }}<td class="tst-applied">{{ .Stats.Applied }}</td>{{ else }}<td class="tst-zero">—</td>{{ end }}
            {{ if .Stats.Drifted }}<td class="tst-drifted">{{ .Stats.Drifted }}</td>{{ else }}<td class="tst-zero">—</td>{{ end }}
            {{ if .Stats.Failed }}<td class="tst-failed">{{ .Stats.Failed }}</td>{{ else }}<td class="tst-zero">—</td>{{ end }}
          </tr>
        {{ end }}
        </tbody>
      </table>
    </div>
  </div>

  <button class="toggle-all" onclick="toggleAll()">Expand All</button>

  {{ range .Tasks }}
  <div class="task-card" id="task-{{ .Slug }}">
    <div class="task-header" onclick="toggleTask(this)">
      <h2>{{ .Task }}<span class="mode">{{ .Mode }}</span></h2>
      <div class="badges"><span class="badge {{ .BadgeClass }}">{{ .BadgeText }}</span><span class="chevron">▼</span></div>
    </div>
    <div class="task-body">
      <div class="section-title">Groups</div>
      {{ if .Groups }}
      <div class="item-list">
        {{ range .Groups }}
        <div class="item-row">
          <span class="item-label {{ .LabelClass }}">{{ .LabelText }}</span>
          <span class="item-key">{{ .TargetPath }}</span>
          <span class="item-desc">{{ .Description }}</span>
          {{ if .Error }}<span class="item-err">⚠ {{ .Error }}</span>{{ end }}
        </div>
        {{ end }}
      </div>
      {{ else }}
      <div class="empty-section">— none</div>
      {{ end }}

      <div class="section-title">Projects</div>
      {{ if .Projects }}
      <div class="item-list">
        {{ range .Projects }}
        <div class="item-row">
          <span class="item-label {{ .LabelClass }}">{{ .LabelText }}</span>
          <span class="item-key">{{ .TargetPath }}</span>
          <span class="item-desc">{{ .Description }}</span>
          {{ if .Error }}<span class="item-err">⚠ {{ .Error }}</span>{{ end }}
        </div>
        {{ end }}
      </div>
      {{ else }}
      <div class="empty-section">— none</div>
      {{ end }}
    </div>
  </div>
  {{ end }}

</div>
<script>
function toggleTask(header) {
  const body = header.nextElementSibling;
  const chevron = header.querySelector('.chevron');
  const isOpen = body.classList.toggle('open');
  if (chevron) chevron.style.transform = isOpen ? 'rotate(180deg)' : '';
}
function toggleAll() {
  const bodies = document.querySelectorAll('.task-body');
  const btn = document.querySelector('.toggle-all');
  const anyOpen = Array.from(bodies).some(b => b.classList.contains('open'));
  bodies.forEach(b => b.classList.toggle('open', !anyOpen));
  document.querySelectorAll('.chevron').forEach(c => c.style.transform = anyOpen ? '' : 'rotate(180deg)');
  btn.textContent = anyOpen ? 'Expand All' : 'Collapse All';
}
document.querySelectorAll('.task-card').forEach(card => {
  if (card.querySelector('.badge.has-failures')) {
    const body = card.querySelector('.task-body');
    const chevron = card.querySelector('.chevron');
    if (body) body.classList.add('open');
    if (chevron) chevron.style.transform = 'rotate(180deg)';
  }
});
</script>
</body></html>
`))
