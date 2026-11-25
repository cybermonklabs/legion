package asof

import "slices"

// projectDefaultsRecordedFrom is when Dispatch began to record a project's default assignee in
// events: 2026-11-15T07:01:28Z, the time of production's first
// settings.project_default_assignee.updated (event 563,436). Migration 20261111210103 filled the
// column with no event before then, so a default no event at or before the cut names is the
// present one only for a cut from this time on (decision 2).
const projectDefaultsRecordedFrom = `'2026-11-15T07:01:28Z'::timestamptz`

// projectDefaultRepoMigration is migration 20261119000230, which added projects.default_repo and
// filled it from the default row of each project's repository mapping. projectDefaultRepoBegan is
// the time schema_migrations records for it, as a SQL expression: from then on every write of the
// column appends a project.updated that carries the project (api/project_default_repository.go),
// whose payload names default_repo, or leaves it out where the default is cleared (omitempty);
// before then a project's default repository was its mapping's default row.
const (
	projectDefaultRepoMigration = "20261119000230"
	projectDefaultRepoBegan     = `(select m.applied_at from public.schema_migrations m where m.version = ` + projectDefaultRepoMigration + `)`
)

// projectTypes are the events of a project's create and update, which carry the project, and
// defaultAssigneeTypes the event of its default assignee's change: the events of a project's own
// settings that projectsShadow reads. The predicate of the partial index events_project_setting
// names exactly projectSettingTypes, in this order, so that the planner can prove that the index
// serves each read of them, which names one of the two lists: a change here needs a new index.
var (
	projectTypes         = []string{"project.created", "project.updated"}
	defaultAssigneeTypes = []string{"settings.project_default_assignee.updated"}
	projectSettingTypes  = slices.Concat(projectTypes, defaultAssigneeTypes)
)

// projectList and defaultAssigneeList are projectTypes and defaultAssigneeTypes as SQL lists
// (sqlList), each quoted once.
var (
	projectList         = sqlList(projectTypes)
	defaultAssigneeList = sqlList(defaultAssigneeTypes)
)

// ProjectSettingTypes are the event types projectsShadow reads a project's settings from, in the
// order of the predicate of events_project_setting.
func ProjectSettingTypes() []string { return slices.Clone(projectSettingTypes) }

// projectsShadow is decision 2's projects: each project created at or before the cut.
//   - default_assignee: the newest settings.project_default_assignee.updated at the cut; else the
//     present value, when no such event of the project is later than the cut and the cut is at or
//     after projectDefaultsRecordedFrom; else null, which no row holds (the column is not null):
//     the default is unknown, and the field it decides is absent (reason D).
//   - gated_steps: the newest project.created or project.updated at the cut (each carries the
//     project, api/project_gated_steps.go), '{}' in a payload written before the column existed;
//     else the present value when no such event is later than the cut; else '{}'.
//   - default_repo: the newest project.created or project.updated at the cut written at or after
//     projectDefaultRepoBegan, its default_repo (null where the payload leaves it out); else, for a
//     cut at or after that time, the present value when no such event is later than the cut; else
//     the default row of the project's repository mapping (repoProjectsShadow) at the cut, or at
//     that time for a later cut, which is the value the migration filled the column with.
//   - next_number: the present value less the project's issue.created events after the cut,
//     since each create numbers its issue in the transaction that appends it (api/issue_create.go).
//   - last_seq: the newest seq of the project's own events at the cut, as the append sets it.
//   - key, name and created_at: no write changes them.
//   - owner, visibility, slack_channels and slack_channels_changed_at: the row's, as they stand
//     now. They decide who may see the project and where its credentials may post, and no event
//     records them (project.access_changed names the change and no value). Who may see what is
//     read as it stands now at a cut, as graph_nodes' visibility is (CutReadTables), so an answer
//     at a cut gives nothing its reader may not see today. No read at a cut reads them yet.
//
// Each column read from events is a scalar subquery of the select list, so a statement that
// joins a project for its key alone reads none of its events.
func projectsShadow(cut string) string {
	return `select p.key as key,
		p.name as name,
		p.next_number - (
			select count(*) from public.events e
			where e.type = 'issue.created' and e.created_at > ` + cut + ` and e.payload->>'project' = p.key
		)::integer as next_number,
		p.created_at as created_at,
		coalesce((select e.seq from public.events e where e.project_key = p.key and e.created_at <= ` + cut + `
			order by e.seq desc limit 1), 0) as last_seq,
		coalesce(
			(select e.payload->>'assignee' from public.events e
				where e.project_key = p.key and e.type in ` + defaultAssigneeList + ` and e.created_at <= ` + cut + `
				order by e.seq desc limit 1),
			case when ` + cut + ` >= ` + projectDefaultsRecordedFrom + ` and not exists (
				select from public.events e
				where e.project_key = p.key and e.type in ` + defaultAssigneeList + ` and e.created_at > ` + cut + `)
			then p.default_assignee end) as default_assignee,
		coalesce(
			(select ` + textArray(`e.payload->'gated_steps'`) + ` from public.events e
				where e.project_key = p.key and e.type in ` + projectList + ` and e.created_at <= ` + cut + `
				order by e.seq desc limit 1),
			case when not exists (
				select from public.events e
				where e.project_key = p.key and e.type in ` + projectList + ` and e.created_at > ` + cut + `)
			then p.gated_steps else '{}'::text[] end) as gated_steps,
		p.owner as owner,
		p.visibility as visibility,
		p.slack_channels as slack_channels,
		p.slack_channels_changed_at as slack_channels_changed_at,
		case
			when exists (
				select from public.events e
				where e.project_key = p.key and e.type in ` + projectList + ` and e.created_at <= ` + cut + `
					and e.created_at >= ` + projectDefaultRepoBegan + `)
			then (select e.payload->>'default_repo' from public.events e
				where e.project_key = p.key and e.type in ` + projectList + ` and e.created_at <= ` + cut + `
					and e.created_at >= ` + projectDefaultRepoBegan + `
				order by e.seq desc limit 1)
			when ` + cut + ` >= ` + projectDefaultRepoBegan + ` and not exists (
				select from public.events e
				where e.project_key = p.key and e.type in ` + projectList + ` and e.created_at > ` + cut + `)
			then p.default_repo
			else (select r.repo from (` + repoProjectsShadow(`least(`+cut+`, `+projectDefaultRepoBegan+`)`) + `) r
				where r.project = p.key and r.is_default limit 1)
		end as default_repo
	from public.projects p
	where p.created_at <= ` + cut
}

// repoProjectsShadow is decision 2's repo_projects: the mapping of each repository as its newest
// settings.repo_project.updated at the cut gives it ({"mapping": model.RepoProject, "deleted"}),
// none where that event is a delete. Every write of a mapping appends one, a move one for each
// project and a default's switch one for each mapping it demotes (api/repo_projects.go); a
// move's two share a transaction, and the id orders them as written. A mapping no event has ever
// named (a row written before the event existed) is the row as it stands when it was made at or
// before the cut.
func repoProjectsShadow(cut string) string {
	return `select m.mapping->>'repo' as repo,
		m.mapping->>'project' as project,
		coalesce(m.mapping->'created_by', 'null'::jsonb) as created_by,
		coalesce((m.mapping->>'created_at')::timestamptz, m.created_at) as created_at,
		coalesce((m.mapping->>'default')::boolean, false) as is_default,
		m.mapping->>'renamed_to' as renamed_to
	from (
		select distinct on (e.payload->'mapping'->>'repo')
			e.payload->'mapping' as mapping, e.payload->>'deleted' as deleted, e.created_at
		from public.events e
		where e.type = 'settings.repo_project.updated' and e.created_at <= ` + cut + `
			and jsonb_typeof(e.payload->'mapping') = 'object'
		order by e.payload->'mapping'->>'repo', e.id desc
	) m
	where m.deleted is distinct from 'true'
	union all
	select r.repo, r.project, r.created_by, r.created_at, r.is_default, r.renamed_to
	from public.repo_projects r
	where r.created_at <= ` + cut + ` and not exists (
		select from public.events e
		where e.type = 'settings.repo_project.updated' and e.payload->'mapping'->>'repo' = r.repo)`
}

// priorityTuplesShadow is each tuple written at or before the cut. The log is append-only, and
// at is the writer's clock (api/priorities.go, writeIssueAdvances), later than its transaction's
// start: a tuple whose write began before the cut and stated a later time is not seen (decision
// 2, the error is that transaction's length).
func priorityTuplesShadow(cut string) string {
	return "select * from public.priority_tuples where at <= " + cut
}
