// Package projectmigrationbadge implements the "project-migration-badge"
// task: every migrated project gets a badge reading "AZDO Migrated <date>",
// so anyone landing on the project can see it came over from Azure DevOps.
//
// The badge picture is a static SVG that contains the date, so there is one
// image per date. It lives in a repository every user can read. When the
// image for the date is missing, Apply commits it to that repository's branch
// before adding any badge that points at it (the plan says so; the
// badge-image command makes the same file by hand). The badge has to link
// somewhere, so it links to the project itself.
//
// A project that already has a badge by this name is left alone, whatever its
// date: the date records when it was migrated, and a later run must not move it.
package projectmigrationbadge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-post-migration/internal/config"
	"gitlab-post-migration/internal/diff"
	"gitlab-post-migration/internal/discovery"
	"gitlab-post-migration/internal/gitlabclient"
	"gitlab-post-migration/internal/task"
)

const Name = "project-migration-badge"

// dateEnv optionally pins the migration date (YYYY-MM-DD); otherwise it is
// today, UTC.
const dateEnv = "MIGRATION_DATE"

type Task struct {
	Client *gitlabclient.Client
	// Now is the clock; tests replace it.
	Now func() time.Time

	mu        sync.Mutex
	imageDone map[string]error // image file -> outcome of ensuring it exists
}

func New(c *gitlabclient.Client) *Task {
	return &Task{Client: c, Now: time.Now, imageDone: map[string]error{}}
}

func Register(c *gitlabclient.Client) { task.Register(New(c)) }

func (t *Task) Name() string { return Name }

func badgeName(cfg *config.Config) string {
	if cfg.ProjectMigrationBadge.Name != "" {
		return cfg.ProjectMigrationBadge.Name
	}
	return "AZDO Migrated"
}

// migrationDate returns the date to stamp on the badge.
func (t *Task) migrationDate() (time.Time, error) {
	if v := os.Getenv(dateEnv); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s=%q is not a YYYY-MM-DD date", dateEnv, v)
		}
		return d, nil
	}
	n := t.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC), nil
}

// imageProject is the project holding the badge images: the configured one,
// else the project this pipeline runs in (CI_PROJECT_PATH), which is where this
// tool's own repository, and so `badge-image`'s output, lives.
func imageProject(cfg *config.Config) string {
	if p := strings.Trim(cfg.ProjectMigrationBadge.ImageProject, "/"); p != "" {
		return p
	}
	return os.Getenv("CI_PROJECT_PATH")
}

// imageRef is the branch the images are read from: the configured one, else the
// project's default branch (CI_DEFAULT_BRANCH), else "master". It must be a
// branch that stays, not the feature branch a pipeline happens to run on.
func imageRef(cfg *config.Config) string {
	if r := cfg.ProjectMigrationBadge.ImageRef; r != "" {
		return r
	}
	return orDefault(os.Getenv("CI_DEFAULT_BRANCH"), "master")
}

func (t *Task) Plan(ctx context.Context, scope discovery.Scope, cfg *config.Config) ([]diff.Diff, error) {
	b := cfg.ProjectMigrationBadge
	project := imageProject(cfg)
	if project == "" {
		return nil, fmt.Errorf("no project to read the badge images from: set project_migration_badge.image_project (e.g. group/gitlab-post-migration), or run in a pipeline, where it defaults to the pipeline's own project")
	}
	date, err := t.migrationDate()
	if err != nil {
		return nil, err
	}
	ref := imageRef(cfg)
	dir := strings.Trim(orDefault(b.ImageDir, "assets"), "/")
	imagePath := dir + "/" + ImageFileName(date)
	imageURL := fmt.Sprintf("%s/%s/-/raw/%s/%s", t.Client.BaseURL(), project, ref, imagePath)
	name := badgeName(cfg) + " " + FormatDate(date)
	imageExists, imageErr := t.imageExists(ctx, project, imagePath, ref)

	diffs := make([]diff.Diff, 0, len(scope.Projects))
	for _, p := range scope.Projects {
		target := diff.Target{Kind: diff.TargetProject, ID: p.ID, Path: p.PathWithNamespace, TopLevelGroupID: p.TopLevelGroupID}

		badges, err := t.listBadges(ctx, p.ID)
		if err != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: fmt.Sprintf("listing badges: %v", err)})
			continue
		}
		if existing := ownBadge(badges, badgeName(cfg)); existing != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusUnchanged, Description: fmt.Sprintf("already has the badge %q", existing.Name)})
			continue
		}
		if imageErr != nil {
			diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusFailed, Description: imageErr.Error()})
			continue
		}
		desc := fmt.Sprintf("no %q badge yet (will add %q)", badgeName(cfg), name)
		detail := map[string]any{
			"name":      name,
			"image_url": imageURL,
			"link_url":  fmt.Sprintf("%s/%s", t.Client.BaseURL(), p.PathWithNamespace),
		}
		if !imageExists {
			desc += fmt.Sprintf("; the image is missing and will be committed to %s (%s) first", project, ref)
			detail["create_image"] = true
			detail["image_project"] = project
			detail["image_ref"] = ref
			detail["image_path"] = imagePath
			detail["image_date"] = date.Format("2006-01-02")
		}
		diffs = append(diffs, diff.Diff{Target: target, Status: diff.StatusDrifted, Description: desc, Detail: detail})
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

// ownBadge finds the project's own badge (not one inherited from a group)
// whose name starts with the configured name, ignoring case and surrounding
// spaces. Any date after the name matches: the date records when the project
// was migrated and must not move.
func ownBadge(badges []*gitlab.ProjectBadge, name string) *gitlab.ProjectBadge {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, b := range badges {
		if b.Kind != "group" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(b.Name)), want) {
			return b
		}
	}
	return nil
}

// imageExists reports whether the badge SVG is in the repository, once per run.
// An error means it could not be determined (not that it is missing).
func (t *Task) imageExists(ctx context.Context, project, path, ref string) (bool, error) {
	_, _, err := t.Client.REST.RepositoryFiles.GetRawFile(project, path, &gitlab.GetRawFileOptions{Ref: gitlab.Ptr(ref)}, gitlab.WithContext(ctx))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, gitlab.ErrNotFound):
		return false, nil
	}
	return false, fmt.Errorf("checking badge image %s in %s: %v", path, project, err)
}

// ensureImage commits the badge SVG for a date to the image repository unless it
// is already there, once per run. It is safe to run twice at the same time: if
// the other run wins, the file exists and that is all that is needed.
func (t *Task) ensureImage(ctx context.Context, project, ref, path, dateStr string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := project + "@" + ref + ":" + path
	if err, ok := t.imageDone[key]; ok {
		return err
	}
	err := t.createImage(ctx, project, ref, path, dateStr)
	t.imageDone[key] = err
	return err
}

func (t *Task) createImage(ctx context.Context, project, ref, path, dateStr string) error {
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return fmt.Errorf("diff has a bad image date %q", dateStr)
	}
	if exists, err := t.imageExists(ctx, project, path, ref); err != nil || exists {
		return err
	}
	_, _, err = t.Client.REST.RepositoryFiles.CreateFile(project, path, &gitlab.CreateFileOptions{
		Branch:        gitlab.Ptr(ref),
		Content:       gitlab.Ptr(SVG(date)),
		CommitMessage: gitlab.Ptr(fmt.Sprintf("Add %q badge image for %s [skip ci]", labelText, FormatDate(date))),
	}, gitlab.WithContext(ctx))
	if err != nil {
		// A parallel run may have just added it.
		if exists, _ := t.imageExists(ctx, project, path, ref); exists {
			return nil
		}
		return fmt.Errorf("committing the badge image %s to %s (%s): %v -- let Maintainers push to that branch, or commit it by hand with `badge-image`", path, project, ref, err)
	}
	return nil
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
		if create, _ := d.Detail["create_image"].(bool); create {
			project, _ := d.Detail["image_project"].(string)
			ref, _ := d.Detail["image_ref"].(string)
			path, _ := d.Detail["image_path"].(string)
			date, _ := d.Detail["image_date"].(string)
			if err := t.ensureImage(ctx, project, ref, path, date); err != nil {
				results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: err.Error()})
				continue
			}
		}

		// The plan may be older than another run: re-check right before adding,
		// so a project that got its badge in the meantime keeps that one (and
		// its date) instead of getting a second.
		existing, err := t.listBadges(ctx, d.Target.ID)
		if err != nil {
			results = append(results, diff.Result{Target: d.Target, Status: diff.StatusFailed, Error: fmt.Sprintf("listing badges: %v", err)})
			continue
		}
		if b := ownBadge(existing, badgeName(cfg)); b != nil {
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

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
