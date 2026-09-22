// Package task defines the plugin contract every post-migration setting
// implements, plus the registry the CLI dispatches "--task=<name>" against.
package task

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/tfonji/gitlab-post-migration/internal/config"
	"github.com/tfonji/gitlab-post-migration/internal/diff"
	"github.com/tfonji/gitlab-post-migration/internal/discovery"
)

// Task is implemented once per post-migration setting. Plan must be
// side-effect free: it only reads live GitLab state and compares it to cfg.
// Apply must only act on the diffs it's given (the ones produced by a prior
// Plan, e.g. from an artifact), not recompute its own — that's what keeps
// "plan then apply" honest: what you approve is what runs.
type Task interface {
	// Name is the --task value and the pipeline job suffix, e.g.
	// "project-default-branch-rename".
	Name() string

	// Plan compares live state for every target in scope against cfg and
	// returns one diff.Diff per target.
	Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error)

	// Apply reconciles exactly the diffs passed in (StatusDrifted ones do
	// work; others pass through as Skipped/Unchanged) and returns one
	// diff.Result per input diff.
	Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error)
}

var (
	mu       sync.Mutex
	registry = map[string]Task{}
)

// Register adds a task to the registry. Called from each task subpackage's
// init(), so importing a task package for its side effect is enough to make
// it selectable by name.
func Register(t Task) {
	mu.Lock()
	defer mu.Unlock()
	name := t.Name()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("task: duplicate registration for %q", name))
	}
	registry[name] = t
}

func Get(name string) (Task, bool) {
	mu.Lock()
	defer mu.Unlock()
	t, ok := registry[name]
	return t, ok
}

// Names returns every registered task name, sorted, e.g. for CLI --help.
func Names() []string {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
