// Package task defines the contract each post-migration setting implements, plus the registry the CLI dispatches --task=<name> against.
package task

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
)

// Task is one post-migration setting; Apply acts only on the diffs Plan produced, so what is approved is what runs.
type Task interface {
	// Name is the --task value and job suffix, e.g. project-default-branch-rename.
	Name() string

	// Plan compares live state for every target in scope to cfg, returning one Diff per target.
	Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error)

	// Apply reconciles the drifted diffs; others pass through, returning one Result per diff.
	Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error)
}

// GroupMembershipRequirer marks tasks whose Apply needs the token user to be a member (granted temporarily by internal/membership).
type GroupMembershipRequirer interface {
	RequiresGroupMembership() bool
}

var (
	mu       sync.Mutex
	registry = map[string]Task{}
)

// Register adds a task to the registry; each task package calls it so importing the package makes it selectable.
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
