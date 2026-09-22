// Package report aggregates plan/apply output from every task into a single
// per-pipeline-run report: one JSON file (machine-readable) and one HTML
// file (human-readable), each with an overall stats summary plus a
// per-task breakdown.
package report

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"text/tabwriter"
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
	Diffs   []diff.Diff   `json:"diffs,omitempty"`   // set when Mode == ModePlan
	Results []diff.Result `json:"results,omitempty"` // set when Mode == ModeApply
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

// WriteTable prints every entry as an aligned table (kind/target/status/
// description, one row per target) followed by a stats summary line --
// meant for job-log output, so a run's effect is visible without opening
// the report artifact.
func (tr TaskReport) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "\n%s (%s)\n", tr.Task, tr.Mode)
	if len(tr.Entries) == 0 {
		fmt.Fprintln(w, "(no targets)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tTARGET\tSTATUS\tDESCRIPTION")
	for _, e := range tr.Entries {
		desc := e.Description
		if e.Error != "" {
			desc = e.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.TargetKind, e.TargetPath, e.Status, desc)
	}
	tw.Flush()
	fmt.Fprintf(w, "\nsummary: %s\n", tr.Stats)
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

func (r Report) WriteHTML(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return htmlTemplate.Execute(f, r)
}

var htmlTemplate = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>GitLab post-migration report</title>
<style>
body { font-family: -apple-system, sans-serif; margin: 2rem; color: #1a1a1a; }
h1 { font-size: 1.4rem; }
h2 { font-size: 1.1rem; margin-top: 2rem; }
table { border-collapse: collapse; width: 100%; margin-top: 0.5rem; }
th, td { border: 1px solid #ddd; padding: 0.4rem 0.6rem; text-align: left; font-size: 0.9rem; }
th { background: #f5f5f5; }
.stats span { display: inline-block; margin-right: 1.2rem; }
.status-unchanged { color: #666; }
.status-drifted { color: #b58900; }
.status-applied { color: #2a9d3d; }
.status-failed { color: #d9412f; font-weight: bold; }
.status-skipped { color: #888; }
</style>
</head>
<body>
<h1>GitLab post-migration report</h1>
<p>Generated {{ .GeneratedAt }}</p>
<div class="stats">
<span>Total: {{ .Stats.Total }}</span>
<span>Unchanged: {{ .Stats.Unchanged }}</span>
<span>Drifted: {{ .Stats.Drifted }}</span>
<span>Applied: {{ .Stats.Applied }}</span>
<span class="status-failed">Failed: {{ .Stats.Failed }}</span>
<span>Skipped: {{ .Stats.Skipped }}</span>
</div>
{{ range .Tasks }}
<h2>{{ .Task }} <small>({{ .Mode }})</small></h2>
<div class="stats">
<span>Total: {{ .Stats.Total }}</span>
<span>Unchanged: {{ .Stats.Unchanged }}</span>
<span>Drifted: {{ .Stats.Drifted }}</span>
<span>Applied: {{ .Stats.Applied }}</span>
<span class="status-failed">Failed: {{ .Stats.Failed }}</span>
<span>Skipped: {{ .Stats.Skipped }}</span>
</div>
<table>
<tr><th>Kind</th><th>Target</th><th>Status</th><th>Description</th><th>Error</th></tr>
{{ range .Entries }}
<tr class="status-{{ .Status }}"><td>{{ .TargetKind }}</td><td>{{ .TargetPath }}</td><td>{{ .Status }}</td><td>{{ .Description }}</td><td>{{ .Error }}</td></tr>
{{ end }}
</table>
{{ end }}
</body>
</html>
`))
