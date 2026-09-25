# gitlab-post-migration

Post-migration reconciler for GitLab projects that have already been moved
from Azure DevOps onto the GitLab Dedicated instance. It does **not** copy
settings between GitLab instances -- given a group or a list of project IDs,
it applies a fixed, versioned baseline of settings across every one of them.

Runs entirely as GitLab CI pipeline jobs (see [.gitlab-ci.yml](.gitlab-ci.yml)).
No database, no long-running service: every run diffs live GitLab state
against [configs/desired-state.yaml](configs/desired-state.yaml) and reports
exactly what it found/changed.

Scaffolding a project's `.gitlab-ci.yml` (and any files that go with a given
template, e.g. `settings.xml`) is a **separate tool**, not this one -- that's
an inherently per-project, human-picked decision (which template, which
bundle of files), not a "same setting applied to many projects" batch job,
so it doesn't fit this reconciler's model.

## What it does

One task per pipeline job, each independently plan-then-apply:

| Task | Scope | What it does |
|---|---|---|
| `group-mr-approval-policy` | top-level group | links a security policy project (GitLab's "merge request approval policy" feature) to the group |
| `group-compliance-framework` | top-level group + its projects | creates/reconciles a compliance framework (name/color/description/pipeline config) at the group, then assigns it to every project in the group |
| `group-protected-environment` | top-level group | protects the `production` environment tier, deploy + approval restricted to Maintainer |
| `group-default-branch-setting` | top-level group | sets the default branch name for *new* projects created in the group going forward |
| `project-default-branch-rename` | project | if the project's default branch isn't `master`, creates `master` from the current default, protects it (push/merge restricted to Maintainer), and switches the project's default branch pointer to it -- also fixes protection alone if `master` is already default but unprotected/misconfigured |

## Pipeline flow

```
build → discover → plan (parallel, one job per task) → apply (manual gate, one job per task) → report
```

- `discover` resolves `GROUP_ID` and/or `PROJECT_IDS` into `projects.json`
  (every project found, plus the deduplicated top-level groups they belong
  to -- group-level tasks iterate those groups, project-level tasks iterate
  the projects).
- Every `plan:*` job reads `projects.json`, diffs live state vs.
  `configs/desired-state.yaml`, and writes `plan-<task>.json`. Nothing is
  changed yet. The job log itself prints a table -- one row per target with
  its kind/path/status/description -- plus a stats summary line, so you can
  review exactly what a task found without opening any artifact.
- Every `apply:*` job is `when: manual` and only acts on the diffs from its
  matching `plan:*` job's artifact -- what you approve in the GitLab UI is
  exactly what runs. Its job log prints the same kind of table, showing what
  was actually done to each target.
- `apply:group-mr-approval-policy` deliberately runs last: it `needs` all
  four other apply jobs, so it isn't triggerable until they've all
  succeeded. If any of them isn't run in a given pipeline, this job is
  auto-skipped rather than becoming available on its own (GitLab treats
  `needs` on a manual job as auto-skip-if-not-triggered since 13.12).
- `report` merges every `result-*.json` (or `plan-*.json` if nothing was
  applied) into one `report.json` + `report.html` pipeline artifact, with an
  overall stats summary and a per-task breakdown.

## Configuration

- [configs/desired-state.yaml](configs/desired-state.yaml) -- the settings
  baseline, identical across every top-level group in a run. Versioned,
  reviewed like code.
- `GITLAB_TOKEN` CI/CD variable (masked/protected) -- needs `api` scope on
  the target group(s).

## Local usage

```
go build -o bin/gitlab-post-migration ./cmd/gitlab-post-migration
export GITLAB_TOKEN=...

./bin/gitlab-post-migration discover --group=12345 --out=projects.json
./bin/gitlab-post-migration list-tasks
./bin/gitlab-post-migration plan --task=project-default-branch-rename --scope=projects.json
./bin/gitlab-post-migration apply --task=project-default-branch-rename --plan=plan-project-default-branch-rename.json
./bin/gitlab-post-migration report --in="result-*.json"
```

## Known gaps / things to verify before a real run

- `group-mr-approval-policy` and `group-compliance-framework` are entirely
  GraphQL (neither security policy project linkage nor compliance
  frameworks are in the REST API). A real run caught two wrong scalar types
  (`pipelineConfigurationFullPath` placed as a sibling of `params` instead
  of inside it; `ID` used where the schema wants the custom `ProjectID`/
  `ComplianceManagementFrameworkID` scalars) -- both mutations, plus
  `group-mr-approval-policy`'s `securityPolicyProjectAssign`, are now
  verified directly against GitLab's Ruby source (see each task's package
  doc comment for the exact file paths), not just docs summaries. The one
  remaining unconfirmed detail is `complianceFrameworkIds`' list-element
  nullability on `projectUpdateComplianceFrameworks` (project assignment) --
  that mutation hasn't been exercised against a live instance yet.
- Two bugs found and fixed against a real GitLab Dedicated instance, both
  worth knowing about if you're extending these tasks or writing new ones:
  - Every `isNotFound(err)` helper originally checked for
    `*gitlab.ErrorResponse` with status 404, which go-gitlab's REST client
    never returns for a 404 -- `CheckResponse` converts every HTTP 404 to
    the sentinel `gitlab.ErrNotFound` instead. The correct check is
    `errors.Is(err, gitlab.ErrNotFound)`.
  - `group-protected-environment`'s `ProtectGroupEnvironmentOptions.RequiredApprovalCount`
    (the top-level field) is rejected by the API as deprecated
    ("Parameter 'required_approval_count' is deprecated and shouldn't be
    used", https://gitlab.com/groups/gitlab-org/-/epics/9662) -- the
    required count belongs solely on each `ApprovalRules` entry now.
  - `project-default-branch-rename`'s branch-creation check used to only
    ask "does `master` already exist?" -- if a project had a stale,
    unrelated `master` branch sitting around from long before migration,
    the task silently reused it (wrong history) and mislabeled the result
    "created" even though nothing was created. It now compares the
    existing branch's tip commit against the current default's tip: an
    exact match is reused safely, anything else surfaces as a `failed`
    plan entry naming both tips rather than switching default to a
    possibly-stale branch.
- `configs/desired-state.yaml` ships with real values you provided (security
  policy project path, compliance framework name/color/description/pipeline
  config) -- double check them before a real run, particularly the
  `pipeline_configuration_full_path` format.
- No concurrency/rate-limit tuning yet; each task processes projects
  sequentially. Fine at the tens-to-hundreds-of-projects scale discussed,
  worth revisiting if that grows.
