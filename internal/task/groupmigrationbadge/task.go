// Package groupmigrationbadge adds an "AZDO Migrated" badge to each top-level group (its projects and subgroups inherit it)
// and removes the exact same badge from projects, where an earlier run put it.
package groupmigrationbadge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
	"gitlab-post-migration/internal/task"
)

const Name = "group-migration-badge"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

func badgeName(cfg *config.Config) string {
	if cfg.GroupMigrationBadge.Name != "" {
		return cfg.GroupMigrationBadge.Name
	}
	return "AZDO Migrated"
}

// imageProject is the configured image project, else the pipeline's own (CI_PROJECT_PATH).
func imageProject(cfg *config.Config) string {
	if p := strings.Trim(cfg.GroupMigrationBadge.ImageProject, "/"); p != "" {
		return p
	}
	return os.Getenv("CI_PROJECT_PATH")
}

// imageRef is the configured branch, else the default branch, else master; never a feature branch.
func imageRef(cfg *config.Config) string {
	if r := cfg.GroupMigrationBadge.ImageRef; r != "" {
		return r
	}
	if r := os.Getenv("CI_DEFAULT_BRANCH"); r != "" {
		return r
	}
	return "master"
}

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	project := imageProject(cfg)
	if project == "" {
		return nil, fmt.Errorf("no project to read the badge image from: set group_migration_badge.image_project (e.g. group/gitlab-post-migration), or run in a pipeline, where it defaults to the pipeline's own project")
	}
	dir := strings.Trim(cfg.GroupMigrationBadge.ImageDir, "/")
	if dir == "" {
		dir = "assets"
	}
	ref := imageRef(cfg)
	imagePath := dir + "/" + ImageFileName
	imageURL := fmt.Sprintf("%s/%s/-/raw/%s/%s", t.Client.BaseURL(), project, ref, imagePath)
	name := badgeName(cfg)
	imageErr := t.checkImage(ctx, project, imagePath, ref)

	diffs := make([]diff.Diff, 0, len(scope.TopLevelGroups))
	for _, g := range scope.TopLevelGroups {
		target := diff.Target{Kind: diff.TargetGroup, ID: g.ID, Path: g.FullPath}

		badges, err := t.listBadges(ctx, g.ID)
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("listing badges: %v", err)})
			continue
		}
		if ownBadge(badges, name) != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("already has the badge %q", name)})
			continue
		}
		if imageErr != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: imageErr.Error()})
			continue
		}
		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("no %q badge yet (will add it)", name),
			Detail: map[string]any{
				"action":    actionAdd,
				"name":      name,
				"image_url": imageURL,
				"link_url":  fmt.Sprintf("%s/%s", t.Client.BaseURL(), g.FullPath),
			},
		})
	}

	// Group diffs come first, so apply knows which groups got the badge before removing the project copies.
	failedGroups := map[int64]bool{}
	for _, d := range diffs {
		if d.Status == diff.StatusFailed {
			failedGroups[d.Target.ID] = true
		}
	}
	return append(diffs, t.planProjectCleanup(ctx, scope, name, imageURL, failedGroups)...), nil
}

const (
	actionAdd    = "add"
	actionRemove = "remove"
)

// planProjectCleanup lists the exact badge to remove from projects; a project with none gets no diff.
func (t *Task) planProjectCleanup(ctx context.Context, scope discovery.Scope, name, imageURL string, failedGroups map[int64]bool) []diff.Diff {
	var diffs []diff.Diff
	for _, p := range scope.Projects {
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace, TopLevelGroupID: p.TopLevelGroupID}
		badges, err := t.listProjectBadges(ctx, p.ID)
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("listing badges: %v", err)})
			continue
		}
		b := exactProjectBadge(badges, name, imageURL)
		if b == nil {
			continue
		}
		if failedGroups[p.TopLevelGroupID] {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusSkipped, Description: fmt.Sprintf("has the project badge %q, kept: its group's badge could not be planned", b.Name)})
			continue
		}
		diffs = append(diffs, diff.Diff{
			Target:      target,
			Status:      diff.StatusDrifted,
			Description: fmt.Sprintf("has the project-level badge %q (will remove it: the group badge covers it)", b.Name),
			Detail:      map[string]any{"action": actionRemove, "badge_id": b.ID, "name": b.Name},
		})
	}
	return diffs
}

// listProjectBadges returns every badge the project shows (its own and inherited), following pagination.
func (t *Task) listProjectBadges(ctx context.Context, projectID int64) ([]*gitlab.ProjectBadge, error) {
	opts := &gitlab.ListProjectBadgesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var all []*gitlab.ProjectBadge
	for {
		page, resp, err := t.Client.REST.ProjectBadges.ListProjectBadges(projectID, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// exactProjectBadge finds the project's own badge with this exact name and image; the group's inherited badge, and
// anyone's differently named or differently pictured badge, never match.
func exactProjectBadge(badges []*gitlab.ProjectBadge, name, imageURL string) *gitlab.ProjectBadge {
	for _, b := range badges {
		if b.Kind == "group" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(b.Name), strings.TrimSpace(name)) && strings.TrimSpace(b.ImageURL) == imageURL {
			return b
		}
	}
	return nil
}

// listBadges returns every badge of the group, following pagination.
func (t *Task) listBadges(ctx context.Context, groupID int64) ([]*gitlab.GroupBadge, error) {
	opts := &gitlab.ListGroupBadgesOptions{ListOptions: gitlab.ListOptions{PerPage: 100}}
	var all []*gitlab.GroupBadge
	for {
		page, resp, err := t.Client.REST.GroupBadges.ListGroupBadges(groupID, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// ownBadge finds the badge by name (case-insensitive).
func ownBadge(badges []*gitlab.GroupBadge, name string) *gitlab.GroupBadge {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, b := range badges {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(b.Name)), want) {
			return b
		}
	}
	return nil
}

// checkImage fails when the badge SVG is not in the repository, so no badge points at nothing.
func (t *Task) checkImage(ctx context.Context, project, path, ref string) error {
	_, _, err := t.Client.REST.RepositoryFiles.GetRawFile(project, path, &gitlab.GetRawFileOptions{Ref: gitlab.Ptr(ref)}, gitlab.WithContext(ctx))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitlab.ErrNotFound):
		return fmt.Errorf("badge image %s not found in %s (%s); commit it there first (`badge-image` writes it)", path, project, ref)
	}
	return fmt.Errorf("checking badge image %s in %s: %v", path, project, err)
}

func (t *Task) Apply(ctx context.Context, diffs []diff.Diff, cfg *config.Config) ([]diff.Result, error) {
	results := make([]diff.Result, 0, len(diffs))
	failedGroups := map[int64]bool{}
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			if d.Status == diff.StatusFailed && d.Target.Kind == diff.TargetGroup {
				failedGroups[d.Target.ID] = true
			}
			continue
		}
		action, _ := d.Detail["action"].(string)
		var res diff.Result
		switch action {
		case actionRemove:
			res = t.remove(ctx, d, failedGroups)
		default:
			res = t.add(ctx, d, cfg)
			if res.Status == diff.StatusFailed {
				failedGroups[d.Target.ID] = true
			}
		}
		results = append(results, res)
	}
	return results, nil
}

func (t *Task) add(ctx context.Context, d diff.Diff, cfg *config.Config) diff.Result {
	fail := func(format string, args ...any) diff.Result {
		return diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf(format, args...)}
	}
	name, _ := d.Detail["name"].(string)
	imageURL, _ := d.Detail["image_url"].(string)
	linkURL, _ := d.Detail["link_url"].(string)
	if name == "" || imageURL == "" || linkURL == "" {
		return fail("diff is missing the badge name, image or link")
	}

	// Re-check just before adding: another run may have added the badge since the plan.
	existing, err := t.listBadges(ctx, d.Target.ID)
	if err != nil {
		return fail("listing badges: %v", err)
	}
	if b := ownBadge(existing, name); b != nil {
		return diff.Result{Target: d.Target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("already has the badge %q, left as is", b.Name)}
	}
	if _, _, err := t.Client.REST.GroupBadges.AddGroupBadge(d.Target.ID, &gitlab.AddGroupBadgeOptions{
		Name: gitlab.Ptr(name), ImageURL: gitlab.Ptr(imageURL), LinkURL: gitlab.Ptr(linkURL),
	}, gitlab.WithContext(ctx)); err != nil {
		return fail("adding badge %q: %v", name, err)
	}
	return diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("added badge %q", name)}
}

// remove deletes a project's copy of the badge, but only once its group has the badge.
func (t *Task) remove(ctx context.Context, d diff.Diff, failedGroups map[int64]bool) diff.Result {
	name, _ := d.Detail["name"].(string)
	id, ok := d.Detail["badge_id"].(float64) // a number after the JSON round trip
	if !ok {
		if v, isInt := d.Detail["badge_id"].(int64); isInt {
			id, ok = float64(v), true
		}
	}
	if !ok {
		return diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: "diff is missing the badge id"}
	}
	if failedGroups[d.Target.TopLevelGroupID] {
		return diff.Result{Target: d.Target, Status: diff.StatusSkipped, Description: fmt.Sprintf("kept the project badge %q: its group's badge was not added", name)}
	}
	if _, err := t.Client.REST.ProjectBadges.DeleteProjectBadge(d.Target.ID, int64(id), gitlab.WithContext(ctx)); err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			return diff.Result{Target: d.Target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("the project badge %q was already gone", name)}
		}
		return diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("removing badge %q: %v", name, err)}
	}
	return diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("removed the project-level badge %q", name)}
}
