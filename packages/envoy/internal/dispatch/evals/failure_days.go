package evals

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// failure_days.go is the day rows of the three failure measures, change-fail-rate,
// failed-deploy-recovery and deployment-rework-rate (dispatch://INFRA-978, D7, R9, R11 and R12):
// project rows of eval_days that no deploy repository keys, built from the record of deploys and
// failures (production_events).
//
// A project's failure measures are on where its delivery is on and it holds at least one failure
// source (failureProjectOf). Its rows begin on its failures_measured_since, and no row stands
// before that day. From it through today every day holds the all-repositories row of each of the
// three, a day with no deploy a zero row, so a window after the first day with no failure reads 0;
// and one row of each per repository the day's deploys, or the deploys of the failures restored
// that day, are in. A read names that day as each series' first day (failureFirstDays), so a window
// that starts before it reads "no data" and names it, whatever rows stand.
//
// A failure is a fact about its deploy. A project's deploys of a day are the record rows that
// finished on it and count for the project under the same rule as deploy-frequency (deployProjects
// and the readers of its owner keys under the published defaults and source selection). A posted
// deploy counts for its route's project alone, where that project holds an events source. A deploy
// the GitHub adapter dropped counts in no numerator and no denominator. Unrejected failures on
// those deploys count where their signals map to sources the project has on (signalSources); a
// person's flag counts under any source. Of each day then:
//
//   - change-fail-rate: the deploys with a counted failure over the deploys
//     (measures.ChangeFailRateDay);
//   - failed-deploy-recovery: the minutes from each counted failure's deploy to its restore, of the
//     failures restored that day, beside the count of the counted failures of the day's deploys not
//     restored yet (measures.RecoveryDay);
//   - deployment-rework-rate: the deploys named as restoring a counted failure over the deploys
//     (measures.ReworkRateDay).
//
// A part's via_repository counts the rows that reached the project through a repository rather
// than an issue or a route.
//
// The refresher keeps them (refresh.applyFailures, refresh.walkFailures). A change of the record
// rewrites, in every project whose measures are on, the days it names: the time its row counts on
// before and after (a deploy's finish, a failure's deploy's finish, a restore's time) and the finish
// day of a restoring deploy that it moves. A settings save rewrites the project's rows whole,
// or deletes them where its measures are off. A publication rewrites the rows of measured
// projects whose selection changed. The first walking pass of a process and of each
// UTC day writes every project's rows whole: so today gains its row, and what no change of the
// record names (a pull request whose issue moved to another project) reaches the rows within a day.
// Each write is a project's days in one transaction, as every computed day row is written: upserted
// where a value moved, the stale rows deleted, and the stored figures of the days folded again.

// FailureDaySlugs is the evals a project's failure rows are of, whose stored day parts the delivery
// measures fold (measures.FailureParts, read by Store.DeploymentFailures).
var FailureDaySlugs = []string{SlugChangeFailRate, SlugFailedDeployRecovery, SlugDeploymentReworkRate}

// failureFamily names the failure rows in a pass's report (builtDay) and in a refusal.
const failureFamily = "failure"

// isFailureMeasure reports whether definition is one of the three failure measures: a computed
// eval of FailureDaySlugs.
func isFailureMeasure(definition Definition) bool {
	return definition.Computed() && slices.Contains(FailureDaySlugs, definition.Slug)
}

// signalSources is the failure source each signal of the record counts under
// (delivery.FailureSources). A person's flag (prodevents.SignalPerson) counts under any of them.
var signalSources = map[string]string{
	prodevents.SignalCheckFail:     delivery.FailureSourceCheckResults,
	prodevents.SignalRevert:        delivery.FailureSourceReverts,
	prodevents.SignalIncidentIssue: delivery.FailureSourceIncidentLabel,
	prodevents.SignalAPI:           delivery.FailureSourceEvents,
}

// failureProject is one project whose failure measures are on: its failure sources, its first
// measured UTC day, and whether its events deploy source reads its posted deploys.
type failureProject struct {
	project string
	sources []string
	since   time.Time
	posted  bool
}

// failureProjectOf is settings' failure project, false where its failure measures are off:
// delivery off, no failure source, or no first measured day (failures_measured_since).
func failureProjectOf(settings delivery.ProjectSettings) (failureProject, bool) {
	if !settings.Delivery || len(settings.FailureSources) == 0 || settings.FailuresMeasuredSince == nil {
		return failureProject{}, false
	}
	project := failureProject{
		project: settings.Project, sources: settings.FailureSources, since: measures.Midnight(*settings.FailuresMeasuredSince),
		posted: holdsEventsSource(settings),
	}
	return project, true
}

// holdsEventsSource reports whether settings hold an events deploy source, whose posted deploys
// count for the project (EventsRepo, failureProject.posted).
func holdsEventsSource(settings delivery.ProjectSettings) bool {
	return slices.ContainsFunc(settings.Sources, func(source delivery.DeploySource) bool {
		return source.Kind == delivery.SourceKindEvents
	})
}

// counts reports whether a failure of signal counts for p: a person's flag under any source, and
// every other signal where p has its source on.
func (p failureProject) counts(signal *string) bool {
	if signal == nil {
		return false
	}
	if *signal == prodevents.SignalPerson {
		return true
	}
	source, known := signalSources[*signal]
	return known && slices.Contains(p.sources, source)
}

// failureDeploy is one deploy row a project's failure rows count, and whether it reached the
// project through its repository rather than an issue or a route (via_repository).
type failureDeploy struct {
	prodevents.Event
	via bool
}

// failureRecord is what one project's failure rows are built from (readFailureRecords): every
// deploy row that finished from the project's first day on that the attribution counts for it, in
// the order they finished (index by id), and the failures counted for it on them (counted, in the
// order they were detected), with, by deploy id, how many counted failures blame it (failed), how
// many of those are not restored (open) and whether it restored one (restores), and the counted
// failures restored on each UTC day, by their index in counted (restoredOn).
type failureRecord struct {
	project    failureProject
	deploys    []failureDeploy
	index      map[string]int
	counted    []prodevents.Event
	failed     map[string]int
	open       map[string]int
	restores   map[string]bool
	restoredOn map[time.Time][]int
}

// failureRecords holds the measured projects and owner keys needed for the read that delivery
// has not yet registered. Registering them lets a future publication detect changes to their
// readers even if no delivery row has been written under the keys.
type failureRecords struct {
	records []failureRecord
	owners  []string
}

// readFailureRecords reads the deploys (including deploys another project recorded), attributes
// them under the published selection and reads the blamed failures. Each result is restricted to
// its own first day and sources. If named is non-nil it also adds the restore and restoring
// deploy's days of changed failures, including rejected ones that no longer count on those days.
func readFailureRecords(ctx context.Context, q delivery.Querier, attributor *delivery.Attributor,
	failures []failureProject, projects []string, named map[time.Time]bool, changed map[string]bool) (failureRecords, error) {
	if len(failures) == 0 {
		return failureRecords{}, nil
	}
	from := failures[0].since
	for _, project := range failures[1:] {
		if project.since.Before(from) {
			from = project.since
		}
	}
	deploys, err := prodevents.DeploysFrom(ctx, q, projects, from)
	if err != nil {
		return failureRecords{}, err
	}
	attribute, err := readShippedAttribution(ctx, q, attributor, deploys)
	if err != nil {
		return failureRecords{}, err
	}
	keys := make([][]counted, len(deploys))
	owners := map[string]bool{}
	for i, deploy := range deploys {
		keys[i] = attribute.recordProjects(deploy)
		for _, key := range keys[i] {
			if isOwner(key.project) {
				owners[key.project] = true
			}
		}
	}
	ownerKeys := slices.Sorted(maps.Keys(owners))
	readers, err := ownerReadersIn(ctx, q, ownerKeys)
	if err != nil {
		return failureRecords{}, err
	}
	ownerKeys = slices.DeleteFunc(ownerKeys, func(key string) bool { return len(readers[key]) > 0 })
	missing, err := readUnrecordedOwners(ctx, q, ownerKeys, readers)
	if err != nil {
		return failureRecords{}, err
	}
	records := make([]failureRecord, len(failures))
	var counting []string
	byProject := make(map[string]*failureRecord, len(records))
	for i, project := range failures {
		records[i] = failureRecord{
			project: project, index: map[string]int{}, failed: map[string]int{}, open: map[string]int{},
			restores: map[string]bool{}, restoredOn: map[time.Time][]int{},
		}
		byProject[project.project] = &records[i]
	}
	for i, deploy := range deploys {
		held := false
		add := func(reader counted) {
			record := byProject[reader.project]
			if record == nil || deploy.FinishedAt.Before(record.project.since) ||
				deploy.Source == prodevents.SourceAPI && !record.project.posted {
				return
			}
			record.index[deploy.ID] = len(record.deploys)
			record.deploys = append(record.deploys, failureDeploy{Event: deploy, via: reader.via})
			held = true
		}
		for _, key := range keys[i] {
			if isOwner(key.project) {
				for _, reader := range readers[key.project] {
					add(reader)
				}
			} else {
				add(key)
			}
		}
		if held {
			counting = append(counting, deploy.ID)
		}
	}
	blamed, err := prodevents.FailuresOf(ctx, q, counting)
	if err != nil {
		return failureRecords{}, err
	}
	if named != nil {
		addRestoreDays(named, changed, deploys, blamed)
	}
	for i := range records {
		records[i].count(blamed)
	}
	return failureRecords{records: records, owners: missing}, nil
}

// addRestoreDays includes the restore and rework days of changed failures. Rejecting a restored
// failure changes those days even though neither the restore time nor the restoring deploy moved.
func addRestoreDays(named map[time.Time]bool, changed map[string]bool, deploys, failures []prodevents.Event) {
	restorers := map[string]bool{}
	for _, failure := range failures {
		if !changed[failure.ID] {
			continue
		}
		if failure.RestoredAt != nil {
			named[measures.Midnight(*failure.RestoredAt)] = true
		}
		if failure.RestoredByDeploy != nil {
			restorers[*failure.RestoredByDeploy] = true
		}
	}
	for _, deploy := range deploys {
		if restorers[deploy.ID] {
			named[measures.Midnight(*deploy.FinishedAt)] = true
		}
	}
}

// count adds to f, in their order, each of failures it counts: one that blames a deploy of f, is
// counted, and is of a signal f's project counts.
func (f *failureRecord) count(failures []prodevents.Event) {
	for _, failure := range failures {
		blamed := *failure.DeployID
		if _, held := f.index[blamed]; !held || failure.State != prodevents.StateCounted || !f.project.counts(failure.Signal) {
			continue
		}
		f.failed[blamed]++
		if failure.RestoredAt == nil {
			f.open[blamed]++
		} else {
			day := measures.Midnight(*failure.RestoredAt)
			f.restoredOn[day] = append(f.restoredOn[day], len(f.counted))
		}
		if at, held := f.restorer(failure); held {
			f.restores[f.deploys[at].ID] = true
		}
		f.counted = append(f.counted, failure)
	}
}

// restorer is the index in f.deploys of the deploy that restored failure (restored_by_deploy),
// false where none of f's deploys did.
func (f *failureRecord) restorer(failure prodevents.Event) (int, bool) {
	if failure.RestoredByDeploy == nil {
		return 0, false
	}
	at, held := f.index[*failure.RestoredByDeploy]
	return at, held
}

// failureEvalIDs is the ids of the failure measures (FailureDaySlugs) of each of projects, in
// their order.
func failureEvalIDs(projects ...string) []string {
	ids := make([]string, 0, len(projects)*len(FailureDaySlugs))
	for _, project := range projects {
		for _, slug := range FailureDaySlugs {
			ids = append(ids, EvalID(project, slug))
		}
	}
	return ids
}

// recordProjects uses the same ownership keys as attribution.deploy for a GitHub deploy. A
// posted deploy belongs only to its route's project, and a dropped deploy counts nowhere.
func (a attribution) recordProjects(deploy prodevents.Event) []counted {
	switch {
	case deploy.Status == prodevents.StatusDropped:
		return nil
	case deploy.Source == prodevents.SourceAPI:
		return []counted{{project: deploy.ProjectKey}}
	}
	var shipped []*delivery.DeliveryPullRequest
	for _, ref := range deploy.PullRequests {
		if repo, number, ok := parsePullRequestRef(ref); ok {
			shipped = append(shipped, &delivery.DeliveryPullRequest{Repo: repo, Number: number})
		}
	}
	return a.deployProjects(deploy.Repo, shipped)
}

// readShippedAttribution reads the shared delivery attribution lists for the pull requests the
// GitHub deploys ship, by key. All its queries run on the caller's read-only snapshot for items.
func readShippedAttribution(ctx context.Context, q delivery.Querier, attributor *delivery.Attributor, deploys []prodevents.Event) (attribution, error) {
	var pulls []delivery.PullRequestKey
	for _, deploy := range deploys {
		if deploy.Source == prodevents.SourceAPI || deploy.Status == prodevents.StatusDropped {
			continue
		}
		for _, ref := range deploy.PullRequests {
			if repo, number, ok := parsePullRequestRef(ref); ok {
				pulls = append(pulls, delivery.PullRequestKey{Repo: repo, Number: number})
			}
		}
	}
	return readAttributionIn(ctx, q, attributor, nil, pulls, func(ctx context.Context, reads ...func(context.Context) error) error {
		for _, read := range reads {
			if err := read(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// deploysFrom is the index in f.deploys of the first deploy that finished at or after at.
func (f failureRecord) deploysFrom(at time.Time) int {
	first, _ := slices.BinarySearchFunc(f.deploys, at, func(deploy failureDeploy, at time.Time) int {
		return deploy.FinishedAt.Compare(at)
	})
	return first
}

// day is the project's failure rows of day, a UTC midnight: none before its first day, and from it
// the all-repositories row of each of the three measures and one row of each per repository the
// day's deploys, or the deploys of the failures restored that day, are in (a posted deploy that
// names no repository is in no repository's rows).
func (f failureRecord) day(day time.Time) ProjectDay {
	built := ProjectDay{Project: f.project.project, Day: day}
	if day.Before(f.project.since) {
		return built
	}
	type cut struct {
		deploys, viaDeploys   []measures.FailureDeploy
		restored, viaRestored []measures.RestoredFailure
		open                  int
	}
	cuts := map[string]*cut{"": {}}
	cutsOf := func(repo string) []*cut {
		held := []*cut{cuts[""]}
		if repo != "" {
			if cuts[repo] == nil {
				cuts[repo] = &cut{}
			}
			held = append(held, cuts[repo])
		}
		return held
	}
	end := day.AddDate(0, 0, 1)
	for _, deploy := range f.deploys[f.deploysFrom(day):] {
		if !deploy.FinishedAt.Before(end) {
			break
		}
		one := measures.FailureDeploy{Failures: f.failed[deploy.ID], Restores: f.restores[deploy.ID]}
		for _, c := range cutsOf(deploy.Repo) {
			c.deploys = append(c.deploys, one)
			if deploy.via {
				c.viaDeploys = append(c.viaDeploys, one)
			}
			c.open += f.open[deploy.ID]
		}
	}
	for _, at := range f.restoredOn[day] {
		failure := f.counted[at]
		deploy := f.deploys[f.index[*failure.DeployID]]
		one := measures.RestoredFailure{DeployFinishedAt: *deploy.FinishedAt, RestoredAt: *failure.RestoredAt}
		for _, c := range cutsOf(deploy.Repo) {
			c.restored = append(c.restored, one)
			if deploy.via {
				c.viaRestored = append(c.viaRestored, one)
			}
		}
	}
	for _, repo := range slices.Sorted(maps.Keys(cuts)) {
		c := cuts[repo]
		add := func(slug string, parts, viaParts []measures.DayPart) {
			via := make(map[string]int, len(viaParts))
			for _, part := range viaParts {
				via[part.Series] = part.N
			}
			for _, part := range parts {
				part.ViaRepository = via[part.Series]
				built.Rows = append(built.Rows, DayRow{
					ProjectKey: f.project.project, EvalID: EvalID(f.project.project, slug), Repo: repo, Listed: true, DayPart: part,
				})
			}
		}
		add(SlugChangeFailRate, measures.ChangeFailRateDay(day, c.deploys), measures.ChangeFailRateDay(day, c.viaDeploys))
		add(SlugFailedDeployRecovery, measures.RecoveryDay(day, c.restored, c.open), measures.RecoveryDay(day, c.viaRestored, 0))
		add(SlugDeploymentReworkRate, measures.ReworkRateDay(day, c.deploys), measures.ReworkRateDay(day, c.viaDeploys))
	}
	return built
}

// failureWrite is one write of a project's failure rows: its days, each whole, in day order, and
// whether they are every day its rows hold (whole), so that a stored row of any other day goes too.
type failureWrite struct {
	project       string
	days          []ProjectDay
	whole         bool
	rewrite       bool // The write brings the rows to a settings save.
	firstDayMoved bool
	owners        []string // Unregistered keys used to attribute this write's deploys.
}

// failureStaleDays is how many days one statement of deleteStaleFailureDays reads the stored rows
// of: a month of a project's rows, each day three all-repositories rows and three for each
// repository, well inside statementBytes.
const failureStaleDays = 31

// writeFailureDays writes write in one transaction, as writeProjectDay writes a day: every day's
// rows upserted where a value moved (upsertDays) and the project's stored rows of the three evals
// on those days that the days no longer hold deleted (deleteStaleFailureDays); for a whole write,
// every stored row of them on any other day too; and the stored figures of the days folded again,
// each run of them apart (foldFiguresInTx), of every day for a whole write. Its change rows wait in
// the change log's outbox and are moved into the log once it has committed (drainChanges), so the
// evals view of the project wakes. A row of another project, another day or another eval is refused
// before anything is written.
func (s *Store) writeFailureDays(ctx context.Context, write failureWrite, builtBy string) error {
	if len(write.days) == 0 && !write.whole {
		return nil
	}
	evalIDs := failureEvalIDs(write.project)
	var rows []DayRow
	spans := writtenSpans{}
	for _, day := range write.days {
		if err := checkProjectDay(failureFamily, day, FailureDaySlugs, nil); err != nil {
			return err
		}
		rows = append(rows, day.Rows...)
		for _, id := range evalIDs {
			spans.add(write.project, id, day.Day, day.Day)
		}
	}
	if err := s.inTx(ctx, "the failure rows of "+write.project, func(tx pgx.Tx) error {
		if write.rewrite {
			if err := MarkSettingsRewrite(ctx, tx); err != nil {
				return err
			}
		}
		if len(rows) > 0 {
			if err := upsertDays(ctx, tx, rows, builtBy); err != nil {
				return err
			}
		}
		if err := deleteStaleFailureDays(ctx, tx, write.project, evalIDs, write.days); err != nil {
			return err
		}
		if write.whole {
			// Every day outside the days written: before the first day and after today, or every
			// day where the project's measures are off and no day is written. The figures of each
			// day a row goes from are folded again with the days written.
			outside := `delete from eval_days where project_key = $1 and eval_id = any($2) and writer = $3`
			args := []any{write.project, evalIDs, writerComputation}
			if len(write.days) > 0 {
				outside += ` and (day < $4::date or day > $5::date)`
				args = append(args, write.days[0].Day, write.days[len(write.days)-1].Day)
			}
			rows, err := tx.Query(ctx, outside+` returning eval_id, day`, args...)
			if err != nil {
				return fmt.Errorf("delete the failure rows %s holds outside its days: %w", write.project, err)
			}
			var evalID string
			var day time.Time
			if _, err := pgx.ForEachRow(rows, []any{&evalID, &day}, func() error {
				spans.add(write.project, evalID, day, day)
				return nil
			}); err != nil {
				return fmt.Errorf("delete the failure rows %s holds outside its days: %w", write.project, err)
			}
		}
		if err := recordOwners(ctx, tx, write.owners); err != nil {
			return err
		}
		if err := foldFiguresInTx(ctx, tx, builtBy, spans.sets()...); err != nil {
			return err
		}
		if write.firstDayMoved {
			return markFailureFirstDayMoved(ctx, tx, write.project)
		}
		return nil
	}); err != nil {
		return err
	}
	s.drainChanges(ctx)
	return nil
}

// deleteStaleFailureDays deletes, in tx, each of the computation's stored rows of project's evals
// evalIDs on the days of days that those days do not hold (deleteStaleDays' rule, over several days
// at once), failureStaleDays days a statement.
func deleteStaleFailureDays(ctx context.Context, tx pgx.Tx, project string, evalIDs []string, days []ProjectDay) error {
	for chunk := range slices.Chunk(days, failureStaleDays) {
		var dates, heldDays []time.Time
		var evals, deployRepos, series, repos []string
		for _, day := range chunk {
			dates = append(dates, day.Day)
			for _, row := range day.Rows {
				evals, deployRepos, series, repos = append(evals, row.EvalID), append(deployRepos, row.DeployRepo), append(series, row.Series), append(repos, row.Repo)
				heldDays = append(heldDays, row.Day)
			}
		}
		if _, err := tx.Exec(ctx, `
			with gone as materialized (
				select eval_id, deploy_repo, series, repo, day from eval_days
				where project_key = $1 and eval_id = any($2) and writer = $3 and day = any($4::date[])
				except
				select * from unnest($5::text[], $6::text[], $7::text[], $8::text[], $9::date[]) as held(eval_id, deploy_repo, series, repo, day)
			)
			delete from eval_days stored
			using gone
			where stored.project_key = $1 and stored.eval_id = gone.eval_id and stored.writer = $3
				and stored.deploy_repo = gone.deploy_repo and stored.series = gone.series and stored.repo = gone.repo
				and stored.day = gone.day
		`, project, evalIDs, writerComputation, dates, evals, deployRepos, series, repos, heldDays); err != nil {
			return fmt.Errorf("delete the failure rows %s no longer holds: %w", project, err)
		}
	}
	return nil
}

// deleteFailureDays deletes, in one transaction, every stored failure row of projects, each a
// project whose failure measures are off, and folds again the stored figures of each run of the
// days a row went from (foldFiguresInTx), every project's in one fold. Its change rows wait in the
// change log's outbox and are moved into the log once it has committed (drainChanges). It writes
// nothing for no project.
func (s *Store) deleteFailureDays(ctx context.Context, projects []string, builtBy string) error {
	if len(projects) == 0 {
		return nil
	}
	evalIDs := failureEvalIDs(projects...)
	if err := s.inTx(ctx, fmt.Sprintf("the failure rows of %v", projects), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			delete from eval_days where project_key = any($1) and eval_id = any($2) and writer = $3
			returning project_key, eval_id, day
		`, projects, evalIDs, writerComputation)
		if err != nil {
			return fmt.Errorf("delete the failure rows of %v, whose failure measures are off: %w", projects, err)
		}
		spans := writtenSpans{}
		var project, evalID string
		var day time.Time
		if _, err := pgx.ForEachRow(rows, []any{&project, &evalID, &day}, func() error {
			spans.add(project, evalID, day, day)
			return nil
		}); err != nil {
			return fmt.Errorf("delete the failure rows of %v, whose failure measures are off: %w", projects, err)
		}
		return foldFiguresInTx(ctx, tx, builtBy, spans.sets()...)
	}); err != nil {
		return err
	}
	s.drainChanges(ctx)
	return nil
}

// clearFailures deletes every failure row of projects, each a project whose failure measures are
// off (deleteFailureDays), as a whole write of each, which the walk then leaves for the day
// (r.failuresWritten).
func (r *refresh) clearFailures(ctx context.Context, projects []string) error {
	if err := r.store.deleteFailureDays(ctx, projects, r.build); err != nil {
		return err
	}
	for _, project := range projects {
		r.failuresWritten[project] = r.today
	}
	return nil
}

// writeFailures writes the failure rows of record's project, one whose measures are on
// (writeFailureDays), as of the pass: every day from its first day through today where named is
// nil (a whole write, which the walk then leaves for the day: r.failuresWritten), and otherwise
// each day named holds, up to today. write describes the cause and unrecorded keys, to which
// this function adds the days it builds.
func (r *refresh) writeFailures(ctx context.Context, record failureRecord, named map[time.Time]bool, write failureWrite) error {
	project := record.project.project
	var days []time.Time
	if named == nil {
		for day := record.project.since; !day.After(r.today); day = day.AddDate(0, 0, 1) {
			days = append(days, day)
		}
	} else {
		for day := range named {
			if !day.After(r.today) {
				days = append(days, day)
			}
		}
		slices.SortFunc(days, time.Time.Compare)
	}
	write.project, write.whole = project, named == nil
	for _, day := range days {
		write.days = append(write.days, record.day(day))
		r.report.days[builtDay{family: failureFamily, project: project, day: day}] = true
	}
	if err := r.store.writeFailureDays(ctx, write, r.build); err != nil {
		return err
	}
	if named == nil {
		r.failuresWritten[project] = r.today
	}
	return nil
}

// applyFailures writes each failure project affected by the page: whole for settings saves,
// rebuilds and changes of its published deploy attribution; otherwise the days named by the
// production record's changes. A save that turns the measures off removes the project's rows.
func (r *refresh) applyFailures(ctx context.Context, w *pageWork) error {
	named, changed := map[time.Time]bool{}, map[string]bool{}
	for _, rows := range w.prodEvents {
		for id, days := range rows {
			changed[id] = true
			for _, day := range days {
				named[day] = true
			}
		}
	}
	whole := map[string]bool{}
	var written []string
	var on []failureProject
	for _, project := range r.projects {
		failures, measured := r.failures[project]
		switch {
		case w.rebuild != "" || w.settingsOf[project] || measured && slices.Contains(w.failureSelection, project):
			whole[project] = true
		case measured && len(changed) > 0:
		default:
			continue
		}
		written = append(written, project)
		if measured {
			on = append(on, failures)
		}
	}
	read, err := readFailureRecords(ctx, r.store.database.Pool, r.store.attributor, on, r.projects, named, changed)
	if err != nil {
		return err
	}
	records := make(map[string]failureRecord, len(read.records))
	for _, record := range read.records {
		records[record.project.project] = record
	}
	for _, project := range written {
		record, measured := records[project]
		write := failureWrite{rewrite: w.settingsOf[project], firstDayMoved: w.failureFirstDayOf[project]}
		switch {
		case !measured && write.rewrite:
			write.project, write.whole = project, true
			err = r.store.writeFailureDays(ctx, write, r.build)
			if err == nil {
				r.failuresWritten[project] = r.today
			}
		case !measured:
			err = r.clearFailures(ctx, []string{project})
		case whole[project]:
			write.owners = read.owners
			err = r.writeFailures(ctx, record, nil, write)
			read.owners = nil
		default:
			write.owners = read.owners
			err = r.writeFailures(ctx, record, named, write)
			read.owners = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// walkFailures writes each project's failure rows whole once a UTC day: on the first walking pass
// of a process and of each UTC day, of each project no whole write of this process reached that day
// (r.failuresWritten, the loop's). The rows of every such project whose measures are off are
// deleted in one write (clearFailures), and then each other project's are a write of its own
// (writeFailures). Each is a write of the walks, which stop between two of them for an owed wake
// (yields) and leave the rest to the next pass.
func (r *refresh) walkFailures(ctx context.Context) error {
	var off []string
	for _, project := range r.projects {
		if _, on := r.failures[project]; !on && !r.failuresWritten[project].Equal(r.today) {
			off = append(off, project)
		}
	}
	if len(off) > 0 {
		if r.yields(ctx) {
			return nil
		}
		if err := r.clearFailures(ctx, off); err != nil {
			return err
		}
		r.walkBuilt++
	}
	for _, project := range r.projects {
		failures, on := r.failures[project]
		if !on || r.failuresWritten[project].Equal(r.today) {
			continue
		}
		if r.yields(ctx) {
			return nil
		}
		read, err := readFailureRecords(ctx, r.store.database.Pool, r.store.attributor, []failureProject{failures}, r.projects, nil, nil)
		if err != nil {
			return err
		}
		if err := r.writeFailures(ctx, read.records[0], nil, failureWrite{owners: read.owners}); err != nil {
			return err
		}
		r.walkBuilt++
	}
	return nil
}

// failureFirstDays sets, in firsts (FirstDaysIn's), the first day of each series of entries'
// failure measures (isFailureMeasure): the project's failures_measured_since where its measures
// are on (failuresSinceIn), and none otherwise, whatever rows stand (R9). So a range that starts
// before that day reads "no data" and names it, a day after today included, on which no row
// stands yet.
func failureFirstDays(ctx context.Context, q delivery.Querier, entries []Entry, firsts map[string]map[string]time.Time) error {
	var projects []string
	for _, entry := range entries {
		if isFailureMeasure(entry.Definition) && !slices.Contains(projects, entry.Definition.ProjectKey) {
			projects = append(projects, entry.Definition.ProjectKey)
		}
	}
	if len(projects) == 0 {
		return nil
	}
	since, err := failuresSinceIn(ctx, q, projects)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !isFailureMeasure(entry.Definition) {
			continue
		}
		id := entry.Definition.EvalID
		delete(firsts, id)
		day := since[entry.Definition.ProjectKey]
		if day == nil {
			continue
		}
		firsts[id] = make(map[string]time.Time, len(entry.Series))
		for _, series := range entry.Series {
			firsts[id][series.Series] = *day
		}
	}
	return nil
}

// failuresSinceIn reads in q, by project, each project of projects (every project where projects
// is nil) that has delivery on: with the first day of its failure rows where its failure measures
// are on (failureProjectOf's rule: delivery on and a failure source), the UTC midnight of its
// failures_measured_since, and nil where they are off. The day is read as stored, a date, which no
// session time zone moves.
func failuresSinceIn(ctx context.Context, q delivery.Querier, projects []string) (map[string]*time.Time, error) {
	rows, err := q.Query(ctx, `
		select s.project_key, case when cardinality(s.failure_sources) > 0 then s.failures_measured_since end
		from project_delivery_settings s join projects p on p.key = s.project_key
		where s.delivery and ($1::text[] is null or s.project_key = any($1))
	`, projects)
	if err != nil {
		return nil, fmt.Errorf("read the first days of the failure measures: %w", err)
	}
	since := map[string]*time.Time{}
	var project string
	var day *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&project, &day}, func() error {
		since[project] = nil
		if day != nil {
			since[project] = new(*day)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the first days of the failure measures: %w", err)
	}
	return since, nil
}

// DeploymentFailures is the stored day parts of the three failure measures a deployment-wide read
// folds (Store.DeploymentFailures): those of the one delivery-on project, with its first day.
type DeploymentFailures struct {
	parts measures.FailureParts
	since *time.Time
}

// Within is d's parts of days, a range of whole UTC days, as an eval read of that range folds the
// project's stored rows: none where the range starts before the project's first day or the project
// has none ("no data", FirstDaysIn's rule), and otherwise the parts of the range's days.
func (d DeploymentFailures) Within(days Range) measures.FailureParts {
	if d.since == nil || days.From.Before(*d.since) {
		return measures.FailureParts{}
	}
	in := func(parts []measures.DayPart) []measures.DayPart {
		var kept []measures.DayPart
		for _, part := range parts {
			if !part.Day.Before(days.From) && part.Day.Before(days.To) {
				kept = append(kept, part)
			}
		}
		return kept
	}
	return measures.FailureParts{ChangeFail: in(d.parts.ChangeFail), Recovery: in(d.parts.Recovery), Rework: in(d.parts.Rework)}
}

// DeploymentFailures reads, in one snapshot, the stored all-repositories parts of the three
// failure measures on the days of days, and the first day, of the one project that has delivery on
// (dispatch://INFRA-978, D9): a deployment-wide read answers the failure measures only where one
// project's rows are the deployment's, since a deploy that counts for two projects would count
// twice in a sum of theirs. With no such project, or several, or one whose measures are off, it
// holds none.
func (s *Store) DeploymentFailures(ctx context.Context, days Range) (DeploymentFailures, error) {
	tx, err := s.BeginSnapshot(ctx)
	if err != nil {
		return DeploymentFailures{}, fmt.Errorf("begin the read of the deployment's failure measures: %w", err)
	}
	defer tx.Rollback(ctx)
	since, err := failuresSinceIn(ctx, tx, nil)
	if err != nil {
		return DeploymentFailures{}, err
	}
	if len(since) != 1 {
		return DeploymentFailures{}, nil
	}
	var project string
	var first *time.Time
	for key, day := range since {
		project, first = key, day
	}
	if first == nil {
		return DeploymentFailures{}, nil
	}
	read := DeploymentFailures{since: first}
	evalIDs := failureEvalIDs(project)
	rows, err := tx.Query(ctx, `
		select `+dayColumns+` from eval_days
		where project_key = $1 and eval_id = any($2) and writer = $3 and deploy_repo = '' and repo = '' and listed
		  and day >= $4::date and day < $5::date
		order by day, eval_id, series
	`, project, evalIDs, writerComputation, days.From, days.To)
	if err != nil {
		return DeploymentFailures{}, fmt.Errorf("read the failure rows of %s: %w", project, err)
	}
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayRow, error) { return scanDay(row) })
	if err != nil {
		return DeploymentFailures{}, fmt.Errorf("read the failure rows of %s: %w", project, err)
	}
	for _, row := range stored {
		switch slugOf(project, row.EvalID) {
		case SlugChangeFailRate:
			read.parts.ChangeFail = append(read.parts.ChangeFail, row.DayPart)
		case SlugFailedDeployRecovery:
			read.parts.Recovery = append(read.parts.Recovery, row.DayPart)
		case SlugDeploymentReworkRate:
			read.parts.Rework = append(read.parts.Rework, row.DayPart)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DeploymentFailures{}, fmt.Errorf("end the read of the deployment's failure measures: %w", err)
	}
	return read, nil
}

// The relations a row of the record has to a failure measure's numbers (FailureItem.Relation).
const (
	// RelationFailed is a deploy a counted failure blames: change-fail-rate's numerator.
	RelationFailed = "failed"
	// RelationRestoring is a deploy that restored a counted failure: deployment-rework-rate's
	// numerator.
	RelationRestoring = "restoring"
	// RelationDeployed is a deploy in the measure's denominator alone.
	RelationDeployed = "deployed"
	// RelationBlamed is the deploy a failure failed-deploy-recovery counts blames.
	RelationBlamed = "blamed"
	// RelationCounted is a counted failure on a deploy of the range (change-fail-rate).
	RelationCounted = "counted"
	// RelationRestored is a counted failure restored in the range (failed-deploy-recovery, timed),
	// or restored by a deploy of the range (deployment-rework-rate).
	RelationRestored = "restored"
	// RelationNotRestored is a counted failure of a deploy of the range that is not restored yet
	// (failed-deploy-recovery's not_restored).
	RelationNotRestored = "not_restored"
)

// FailureItem is one row of the record behind a failure measure's day rows of a range
// (FailureRecord.Items): a deploy or a failure, its relation to the measure's numbers, the
// repository of the deploy it is counted through (the repository rows it is in), the moment it
// counts on, how many counted failures blame a deploy, and a restored failure's minutes to restore.
type FailureItem struct {
	Event    prodevents.Event
	Relation string
	Repo     string
	At       time.Time
	Failures int
	Minutes  *float64
}

// FailureRecord is the record behind one failure measure's day rows of a project, read once
// (FailureRecordIn), whose Items answers each range's rows from it.
type FailureRecord struct {
	slug     string
	failures failureProject
	record   failureRecord
}

// FailureRecordIn reads in q the record behind slug's day rows, one of the failure measures of
// project, using attributor and the published selection in q, as the day builder does. It returns
// nil where the measures are off or slug is none of them.
func FailureRecordIn(ctx context.Context, q delivery.Querier, attributor *delivery.Attributor, project, slug string) (*FailureRecord, error) {
	settings, err := delivery.GetProjectSettings(ctx, q, project)
	if errors.Is(err, delivery.ErrNoSettings) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	failures, on := failureProjectOf(settings)
	if !on || !slices.Contains(FailureDaySlugs, slug) {
		return nil, nil
	}
	projects, err := ProjectsIn(ctx, q)
	if err != nil {
		return nil, err
	}
	read, err := readFailureRecords(ctx, q, attributor, []failureProject{failures}, projects, nil, nil)
	if err != nil {
		return nil, err
	}
	return &FailureRecord{slug: slug, failures: failures, record: read.records[0]}, nil
}

// Items is the rows of the record behind the measure's day rows on the days of days from the
// project's first day on, as its rows count them (failureRecord.day), none for a nil record (its
// measures off). Deploys first, in the order they finished, then failures:
//
//   - change-fail-rate: each deploy of the range, failed or deployed, and each counted failure
//     that blames one (counted), on its deploy's finish;
//   - failed-deploy-recovery: each counted failure restored in the range (restored, at its restore,
//     with its minutes) and each counted failure of a deploy of the range not restored yet
//     (not_restored, on its deploy's finish), and the deploys they blame (blamed);
//   - deployment-rework-rate: each deploy of the range, restoring or deployed, and each counted
//     failure a restoring one restored (restored, on that deploy's finish).
func (f *FailureRecord) Items(days Range) []FailureItem {
	if f == nil {
		return nil
	}
	slug, failures, record := f.slug, f.failures, f.record
	from := days.From
	if from.Before(failures.since) {
		from = failures.since
	}
	var deploys, counted []FailureItem
	blamed := map[string]bool{}
	blame := func(failure prodevents.Event) {
		deploy := record.deploys[record.index[*failure.DeployID]]
		if !blamed[deploy.ID] {
			blamed[deploy.ID] = true
			deploys = append(deploys, FailureItem{
				Event: deploy.Event, Relation: RelationBlamed, Repo: deploy.Repo, At: *deploy.FinishedAt, Failures: record.failed[deploy.ID],
			})
		}
	}
	inRange := func(at time.Time) bool { return !at.Before(from) && at.Before(days.To) }
	for _, deploy := range record.deploys[record.deploysFrom(from):] {
		if !deploy.FinishedAt.Before(days.To) {
			break
		}
		item := FailureItem{Event: deploy.Event, Relation: RelationDeployed, Repo: deploy.Repo, At: *deploy.FinishedAt, Failures: record.failed[deploy.ID]}
		switch slug {
		case SlugChangeFailRate:
			if item.Failures > 0 {
				item.Relation = RelationFailed
			}
			deploys = append(deploys, item)
		case SlugDeploymentReworkRate:
			if record.restores[deploy.ID] {
				item.Relation = RelationRestoring
			}
			deploys = append(deploys, item)
		}
	}
	for _, failure := range record.counted {
		deploy := record.deploys[record.index[*failure.DeployID]]
		switch slug {
		case SlugChangeFailRate:
			if inRange(*deploy.FinishedAt) {
				counted = append(counted, FailureItem{Event: failure, Relation: RelationCounted, Repo: deploy.Repo, At: *deploy.FinishedAt})
			}
		case SlugFailedDeployRecovery:
			switch {
			case failure.RestoredAt != nil && inRange(*failure.RestoredAt):
				minutes := measures.RestoreMinutes(measures.RestoredFailure{DeployFinishedAt: *deploy.FinishedAt, RestoredAt: *failure.RestoredAt})
				counted = append(counted, FailureItem{Event: failure, Relation: RelationRestored, Repo: deploy.Repo, At: *failure.RestoredAt, Minutes: &minutes})
				blame(failure)
			case failure.RestoredAt == nil && inRange(*deploy.FinishedAt):
				counted = append(counted, FailureItem{Event: failure, Relation: RelationNotRestored, Repo: deploy.Repo, At: *deploy.FinishedAt})
				blame(failure)
			}
		case SlugDeploymentReworkRate:
			if at, held := record.restorer(failure); held && inRange(*record.deploys[at].FinishedAt) {
				restoring := record.deploys[at]
				counted = append(counted, FailureItem{Event: failure, Relation: RelationRestored, Repo: restoring.Repo, At: *restoring.FinishedAt})
			}
		}
	}
	slices.SortStableFunc(deploys, func(a, b FailureItem) int { return a.At.Compare(b.At) })
	return slices.Concat(deploys, counted)
}
