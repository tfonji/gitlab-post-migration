package report

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tfonji/gitlab-post-migration/internal/diff"
)

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
)

const ruleWidth = 70

// Terminal renders plan/apply output for job logs and consoles: a colored,
// symbol-prefixed view grouped by task and target kind, with targets that
// need no attention collapsed into a single count line.
type Terminal struct {
	w     io.Writer
	color bool
}

func NewTerminal(w io.Writer, color bool) *Terminal {
	return &Terminal{w: w, color: color}
}

// UseColor decides whether ANSI color should be emitted to f: never when
// NO_COLOR is set, always when FORCE_COLOR is set or under GitLab CI (whose
// job log viewer renders ANSI even though stderr isn't a TTY there), and
// otherwise only when f is an interactive terminal.
func UseColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" || os.Getenv("GITLAB_CI") == "true" {
		return true
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// WriteTask prints one task's result -- called right after plan/apply so a
// run's effect is visible in that job's log without opening the report.
func (t *Terminal) WriteTask(tr TaskReport) {
	t.println("")
	t.println(t.paint(ansiBold+ansiCyan, fmt.Sprintf("%s (%s)", strings.ToUpper(tr.Task), tr.Mode)))
	t.rule()
	t.writeTaskBody(tr, "")
	t.rule()
	t.writeSummary(tr.Stats, tr.Mode == ModePlan)
	t.println("")
}

// WriteReport prints every task of a merged Report followed by one overall
// summary line, for the `report` job's log.
func (t *Terminal) WriteReport(r Report) {
	t.println("")
	t.println(t.paint(ansiBold+ansiCyan, "GITLAB POST-MIGRATION REPORT"))
	t.rule()
	for _, tr := range r.Tasks {
		t.println("")
		t.println(t.paint(ansiBold+ansiCyan, fmt.Sprintf("%s (%s)", tr.Task, tr.Mode)))
		t.writeTaskBody(tr, "  ")
	}
	t.println("")
	t.rule()
	t.writeSummary(r.Stats, hasPlanTask(r.Tasks))
	t.println("")
}

func (t *Terminal) writeTaskBody(tr TaskReport, indent string) {
	if len(tr.Entries) == 0 {
		t.println(indent + t.paint(ansiDim, "(no targets)"))
		return
	}
	sections := []struct {
		title string
		match func(diff.TargetKind) bool
	}{
		{"Groups", func(k diff.TargetKind) bool { return k == diff.TargetGroup }},
		{"Projects", func(k diff.TargetKind) bool { return k != diff.TargetGroup }},
	}
	for _, s := range sections {
		var active []Entry
		unchanged, skipped := 0, 0
		for _, e := range tr.Entries {
			if !s.match(e.TargetKind) {
				continue
			}
			switch {
			case e.Status == diff.StatusUnchanged && e.Error == "":
				unchanged++
			case e.Status == diff.StatusSkipped && e.Error == "":
				skipped++
			default:
				active = append(active, e)
			}
		}
		if len(active) == 0 && unchanged == 0 && skipped == 0 {
			continue
		}
		t.println(indent + t.paint(ansiBold, s.title))
		if unchanged+skipped > 0 {
			t.println(indent + "  " + t.paint(ansiDim, fmt.Sprintf("· %d unchanged, %d skipped", unchanged, skipped)))
		}
		for _, e := range active {
			t.writeEntry(indent+"  ", e)
		}
	}
}

func (t *Terminal) writeEntry(indent string, e Entry) {
	symbol, color := entrySymbolColor(e.Status)
	line := fmt.Sprintf("%s %s", t.paint(color, symbol), e.TargetPath)
	if e.Description != "" {
		line += " — " + e.Description
	}
	if e.Error != "" {
		line += ": " + t.paint(ansiRed, e.Error)
	}
	t.println(indent + line)
}

func (t *Terminal) writeSummary(s Stats, plan bool) {
	counts := fmt.Sprintf("%d applied, %d drifted, %d unchanged, %d skipped", s.Applied, s.Drifted, s.Unchanged, s.Skipped)
	switch {
	case s.Failed > 0:
		t.println(t.paint(ansiBold+ansiRed, fmt.Sprintf("✗ %d failed", s.Failed)) + " — " + counts)
	case plan:
		note := " (no changes made)"
		if s.Applied > 0 {
			note = " (applied changes are from cleanup; the rest is not applied yet)"
		}
		t.println(t.paint(ansiBold+ansiBlue, "~ Plan complete") + " — " + counts + note)
	default:
		t.println(t.paint(ansiBold+ansiGreen, "✓ Apply complete") + " — " + counts)
	}
}

func entrySymbolColor(s diff.Status) (symbol, color string) {
	switch s {
	case diff.StatusApplied:
		return "+", ansiGreen
	case diff.StatusDrifted:
		return "~", ansiYellow
	case diff.StatusFailed:
		return "✗", ansiRed
	case diff.StatusSkipped:
		return "-", ansiDim
	default:
		return "·", ansiDim
	}
}

func hasPlanTask(tasks []TaskReport) bool {
	for _, t := range tasks {
		if t.Mode == ModePlan {
			return true
		}
	}
	return false
}

func (t *Terminal) paint(code, s string) string {
	if !t.color {
		return s
	}
	return code + s + ansiReset
}

func (t *Terminal) rule() {
	t.println(strings.Repeat("─", ruleWidth))
}

func (t *Terminal) println(s string) {
	fmt.Fprintln(t.w, s)
}
