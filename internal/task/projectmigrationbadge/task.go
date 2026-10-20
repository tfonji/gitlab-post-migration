// Package projectmigrationbadge adds an "AZDO Migrated" badge to each migrated project.
package projectmigrationbadge

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

const Name = "project-migration-badge"

type Task struct {
	Client *gitlabclient.Client
}

func New(c *gitlabclient.Client) *Task { return &Task{Client: c} }

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

func badgeName(cfg *config.Config) string {
	if cfg.ProjectMigrationBadge.Name != "" {
		return cfg.ProjectMigrationBadge.Name
	}
	return "AZDO Migrated"
}

// imageProject is the configured image project, else the pipeline's own (CI_PROJECT_PATH).
func imageProject(cfg *config.Config) string {
	if p := strings.Trim(cfg.ProjectMigrationBadge.ImageProject, "/"); p != "" {
		return p
	}
	return os.Getenv("CI_PROJECT_PATH")
}

// imageRef is the configured branch, else the default branch, else master; never a feature branch.
func imageRef(cfg *config.Config) string {
	if r := cfg.ProjectMigrationBadge.ImageRef; r != "" {
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
		return nil, fmt.Errorf("no project to read the badge image from: set project_migration_badge.image_project (e.g. group/gitlab-post-migration), or run in a pipeline, where it defaults to the pipeline's own project")
	}
	dir := strings.Trim(cfg.ProjectMigrationBadge.ImageDir, "/")
	if dir == "" {
		dir = "assets"
	}
	ref := imageRef(cfg)
	imagePath := dir + "/" + ImageFileName
	imageURL := fmt.Sprintf("%s/%s/-/raw/%s/%s", t.Client.BaseURL(), project, ref, imagePath)
	name := badgeName(cfg)
	imageErr := t.checkImage(ctx, project, imagePath, ref)

	diffs := make([]diff.Diff, 0, len(scope.Projects))
	for _, p := range scope.Projects {
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace, TopLevelGroupID: p.TopLevelGroupID}

		badges, err := t.listBadges(ctx, p.ID)
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
				"name":      name,
				"image_url": imageURL,
				"link_url":  fmt.Sprintf("%s/%s", t.Client.BaseURL(), p.PathWithNamespace),
			},
		})
	}
	return diffs, nil
}

// listBadges returns every badge the project shows, following pagination.
func (t *Task) listBadges(ctx context.Context, projectID int64) ([]*gitlab.ProjectBadge, error) {
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

// ownBadge finds the project's own badge by name (case-insensitive), ignoring group-inherited ones.
func ownBadge(badges []*gitlab.ProjectBadge, name string) *gitlab.ProjectBadge {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, b := range badges {
		if b.Kind != "group" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(b.Name)), want) {
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
	for _, d := range diffs {
		if d.Status != diff.StatusDrifted {
			results = append(results, diff.Result{Target: d.Target, Status: d.Status, Description: d.Description})
			continue
		}
		name, _ := d.Detail["name"].(string)
		imageURL, _ := d.Detail["image_url"].(string)
		linkURL, _ := d.Detail["link_url"].(string)
		if name == "" || imageURL == "" || linkURL == "" {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: "diff is missing the badge name, image or link"})
			continue
		}

		// Re-check just before adding: another run may have added the badge since the plan.
		existing, err := t.listBadges(ctx, d.Target.ID)
		if err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("listing badges: %v", err)})
			continue
		}
		if b := ownBadge(existing, name); b != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("already has the badge %q, left as is", b.Name)})
			continue
		}
		if _, _, err := t.Client.REST.ProjectBadges.AddProjectBadge(d.Target.ID, &gitlab.AddProjectBadgeOptions{
			Name: gitlab.Ptr(name), ImageURL: gitlab.Ptr(imageURL), LinkURL: gitlab.Ptr(linkURL),
		}, gitlab.WithContext(ctx)); err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("adding badge %q: %v", name, err)})
			continue
		}
		results = append(results, diff.Result{Target: d.Target, Status: diff.StatusApplied, Description: fmt.Sprintf("added badge %q", name)})
	}
	return results, nil
}
