// Package evalfixture loads the histories the eval registry's checks read (dispatch://INFRA-2916,
// k6, k12 and k15, and the base whose first paint k8's and k13's negative controls read) into a
// database that already holds Dispatch's schema: the local stack's, which migrates its database
// when it starts, or the base's, main at the commit this head merges. cmd is the command a
// checker types.
//
// The modes:
//
//   - Legion (-legion): legionseed's Legion history, at least the asked number of finished issues,
//     with phase, slot and claim observations that start partway through it (LegionSpec), two
//     drops and a child in design that legionseed's trees lack (observedCases), and the project's
//     legion_settings row, which names the project's daemon.
//     It answers the two day-aligned windows k6 reads (Windows): one that starts before the
//     project's first observation and one that starts after its first slot and claim
//     observations.
//   - Base (-base): the same history into a database of the base, main at the commit this head
//     merges (git merge-base <head> origin/main), for the base's first paint that evalbench
//     measures. The Legion history is written into the same tables in both, since every table it
//     writes is one main has; the tables this head derives from those rows (DerivedTables) the
//     fixture never writes, so the mode differs from Legion only in the schema it requires of the
//     database.
//   - Attribution (-attribution): k15's case of D1 (Attribution).
//   - Delivery (-delivery): deliveryseed's delivery history of the project over the days its
//     caller names (the command's -days, a week by default), or deliveryseed's own default of
//     120 days (deliveryseed.DefaultDays) for a caller that names none.
//   - MergePR (-merge-pr): one pull request of those rows merged at a given moment, k12's one
//     change.
//
// Legion adds to one project and writes that project's row only where it has none, so it combines
// with Attribution, Delivery and MergePR; Base writes the same history alone, into a database of
// the base. Delivery adds to one project too, beside deliveryseed's second project, and gives the
// project the deploy repository as its default repository where it has none; Attribution writes
// its three projects with their default repositories; and each of the two replaces the one
// delivery_settings row, so they are not combined with each other. MergePR writes no project's
// rows.
//
// Every time is relative to the moment the caller names (-now), so two loads with one -now hold
// the same history at the same times. The rows bypass the server's own writers (legionseed's
// events bypass events.Broker.Append), so a page open while they land may need a reload; the
// refresher reads them on its next pass, as it reads every table. The Legion history's events are
// dated in the past, so the statement that writes them also nulls the keyword fill's stored
// reach (legionseed's write), and a serving fill writes their rows with no breach of a reach it
// stored before them.
package evalfixture

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// actor is the actor every row this package writes that records one names.
var actor = model.Actor{Kind: "system", ID: "evalfixture"}

// StateURL is the daemon address a fixture project's legion_settings row names. It is under
// `.invalid`, which no name server answers (RFC 2606), so every whole read of it Dispatch's Legion
// state intake makes fails and records the failure, no daemon there publishes a state event, and
// nothing adds an observation to the history the fixture wrote.
const StateURL = "http://legion-daemon.invalid:13370"

// The Legion history's shape (LegionSpec): six hours between two trees, so a history of a few
// hundred issues spans more than two weeks, and the older half of its trees unobserved, so the
// project's first observation falls in its middle with whole days on both sides.
const (
	treeSpacing = 6 * time.Hour
	// DefaultIssues is how many finished issues the Legion history holds when the caller asks
	// for no number: above the 200 k6 counts.
	DefaultIssues = 240
)

// LegionTables are the tables the Legion history is written into; main has every one of them at
// the commit this head merges, with the columns the history writes (delivery_pull_requests the
// merged pull requests of legionseed.Spec.Merged).
var LegionTables = []string{"projects", "issues", "events", "handoffs", "legion_observations", "delivery_pull_requests", "legion_settings"}

// DerivedTables are the tables of this head's Legion evals that this head derives from
// LegionTables' rows: the filings, the session titles, the registry, its day rows with their
// members, owed days and owner keys, the published default repositories, the refresher's state,
// its rebuilds' staging and the rows' stored figures (the session titles by a trigger on events,
// the owner keys by the trigger on eval_days, the rest by the server's facts consumer and
// refresher). The fixture writes none of them in any mode. The Legion mode needs every one of
// them. The base, main at the commit this head merges, lacks those its catalog does not hold,
// which a caller reads from the database (ExistingTables) rather than from a list here, since a
// merge of main can bring some of them; it never holds HeadTable.
var DerivedTables = []string{
	"issue_filings", "session_titles", "session_titles_backfill", "eval_versions", "eval_series",
	"eval_readings", "eval_days", "eval_day_members", "eval_days_owed", "eval_day_owners", "eval_days_defaults",
	"eval_days_cursor", "eval_days_walks", "eval_days_serving", "eval_days_settings",
	"eval_rebuilds", "eval_rebuild_walks", "eval_rebuild_firsts", "eval_rebuild_days", "eval_rebuild_members",
	"eval_rebuild_range_figures", "eval_rebuild_day_figures", "eval_rebuild_reverify",
	"eval_range_figures", "eval_day_figures", "eval_figures_built", "eval_figure_cuts_stale",
}

// HeadTable is the table that tells a database of this head from one of the base: the registry's
// day rows, which only this head's migrations create.
const HeadTable = "eval_days"

// LegionSpec is the Legion history of project the Legion mode writes: enough trees for at least
// issues finished issues, ending at now, with the older half of the trees unobserved and every
// observed root waiting for its slot (legionseed.Spec's Unobserved and Queued), finished issues
// that waited in `icebox` and in `backlog`, a merged pull request for each issue the merger
// closes and filings of all four classes (InBacklogOrIcebox, Merged and Filings, what k6 reads),
// numbered after the offset issue numbers the project has already.
func LegionSpec(project string, issues int, now time.Time, offset int) legionseed.Spec {
	if issues <= 0 {
		issues = DefaultIssues
	}
	trees := (issues + legionseed.FinishedPerTree - 1) / legionseed.FinishedPerTree
	return legionseed.Spec{
		Project: project, Trees: trees, Now: now.UTC(),
		Spacing: treeSpacing, Unobserved: trees / 2, Queued: true, IssueOffset: offset,
		InBacklogOrIcebox: true, Merged: true, Filings: true,
	}
}

// observedCases writes into the Legion history legionseed wrote three cases its trees lack, which
// the method texts of the Legion evals state and k6 compares. Two are drops: when the daemon stops
// reporting an issue, the Legion state intake writes `null` for every field of the issue at one
// moment, and the phase and the slot the issue held run on through it to the next observation that
// names a phase.
// The newest tree's root is dropped from the second hour of its tree to the third, holding its
// slot in `admitted` (design) with its architect's claim `working`, and the tree's first child from
// the sixth hour to the eighth, holding its slot in `implementing` with its implementer's claim
// `working`; each drop ends at a read that carries the issue again with the phase, the slot and the
// claim it held. The third is a child in design: the same child observed in `admitted` from the
// tenth hour to the eleventh, holding its slot. The hours count from the root's creation, and a
// case is written only where the issue holds, from the case's first hour to its last, the phase,
// the slot and the claim the case needs, so a history of another shape is refused rather than left
// without the case.
func observedCases(ctx context.Context, pool *store.Pool, history legionseed.Fixture) error {
	if len(history.Roots) == 0 {
		return errors.New("the history holds no tree, so no drop and no child in design can be written into it")
	}
	root := history.Roots[0]
	var start time.Time
	var child string
	if err := pool.QueryRow(ctx, `
		select r.created_at, coalesce((select c.key from issues c where c.parent_key = r.key order by c.number limit 1), '')
		from issues r
		where r.key = $1`, root).Scan(&start, &child); err != nil {
		return fmt.Errorf("read the newest tree of %s: %w", history.Project, err)
	}
	hour := func(hours int) time.Time { return start.Add(time.Duration(hours) * time.Hour) }
	// held is the value of the issue's newest observation of field recorded at or before at,
	// `null` where it has none.
	held := func(issue, field string, at time.Time) (string, error) {
		var value string
		if err := pool.QueryRow(ctx, `
			select coalesce((
				select value::text from legion_observations
				where project = $1 and issue_key = $2 and field = $3 and observed_at <= $4
				order by observed_at desc, id desc limit 1), 'null')`,
			history.Project, issue, field, at).Scan(&value); err != nil {
			return "", fmt.Errorf("read the %s %s of %s held at %s: %w", history.Project, field, issue, at.Format(time.RFC3339), err)
		}
		return value, nil
	}
	var issues, fields, values []string
	var moments []time.Time
	record := func(issue, field, value string, at time.Time) {
		issues, fields, values, moments = append(issues, issue), append(fields, field), append(values, value), append(moments, at)
	}
	for _, one := range []struct {
		issue, phase string
		from, to     time.Time
		// claim is the role whose claim is `working` through a drop, "" for the child in design.
		claim string
	}{
		{root, legion.PhaseAdmitted, hour(2), hour(3), "architect"},
		{child, legion.PhaseImplementing, hour(6), hour(8), "implementer"},
		{child, legion.PhaseImplementing, hour(10), hour(11), ""},
	} {
		named := []string{legion.FieldPhase, legion.FieldSlot}
		want := map[string]string{legion.FieldPhase: fmt.Sprintf("%q", one.phase)}
		if one.claim != "" {
			named = append(named, legion.ClaimFieldPrefix+one.claim)
			want[legion.ClaimFieldPrefix+one.claim] = `"working"`
		}
		holding := map[string]string{}
		for _, field := range named {
			first, err := held(one.issue, field, one.from)
			if err != nil {
				return err
			}
			last, err := held(one.issue, field, one.to)
			if err != nil {
				return err
			}
			if first != last || first == "null" || (want[field] != "" && first != want[field]) {
				return fmt.Errorf("%s's %s reads %s at %s and %s at %s, and the case written there needs %s held through both: the history is not of the shape whose cases this mode writes",
					one.issue, field, first, one.from.Format(time.RFC3339), last, one.to.Format(time.RFC3339), cmp.Or(want[field], "a slot"))
			}
			holding[field] = first
		}
		if one.claim == "" {
			record(one.issue, legion.FieldPhase, fmt.Sprintf("%q", legion.PhaseAdmitted), one.from)
			record(one.issue, legion.FieldPhase, holding[legion.FieldPhase], one.to)
			continue
		}
		for _, field := range named {
			record(one.issue, field, "null", one.from)
			record(one.issue, field, holding[field], one.to)
		}
	}
	// The cases go in as every insert into legion_observations does: under the observations' lock
	// (legion.QueueLockObservations), held to its own transaction's commit, since a fixture loaded
	// into a serving database lands beside the Legion state intake's writes, and the Legion facts
	// consumer pages the table by id.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		legion.QueueLockObservations(batch)
		store.QueueExec(batch, "write the drops and the child in design of "+history.Project, `
			insert into legion_observations (project, issue_key, field, value, observed_at)
			select $1, issue, field, value::jsonb, at
			from unnest($2::text[], $3::text[], $4::text[], $5::timestamptz[]) as written(issue, field, value, at)`,
			history.Project, issues, fields, values, moments)
		return tx.SendBatch(ctx, batch).Close()
	}); err != nil {
		return fmt.Errorf("write the drops and the child in design of %s: %w", history.Project, err)
	}
	return nil
}

// Window is one range of whole UTC days, [From, To), as the eval routes read one.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Days is how many days the window covers.
func (w Window) Days() int {
	return int(w.To.Sub(w.From) / (24 * time.Hour))
}

// maxWindow is the widest window the eval routes read (api/delivery.go's maxDeliveryWindow).
const maxWindow = 84 * 24 * time.Hour

// Windows is the two windows k6 reads over a Legion history. Before starts on the day of its first
// finished issue, which is the first day a Legion measure has input, so it starts before the
// project's first observation. After starts at the first UTC midnight at or after the project's
// first slot and first claim observations, from which slot wait and idle time have input (R6), so
// every measure holds values over it, and ends at the midnight after the history's end. Before
// ends where After starts, and each is at most the 84 days the routes read.
type Windows struct {
	Before Window `json:"before"`
	After  Window `json:"after"`
}

// WindowsOf is the two windows of a Legion history whose project's first slot and first claim
// observations are firstSlot and firstClaim (firstObservations). It refuses a history with no
// observation, no slot or claim observation or no finished issue, and one whose first finished
// issue's day starts at or after its first observation, since no window then starts on a day with
// input before that observation.
func WindowsOf(history legionseed.Fixture, firstSlot, firstClaim time.Time) (Windows, error) {
	if history.FirstObserved.IsZero() || history.FirstFinished.IsZero() {
		return Windows{}, errors.New("the history holds no observation or no finished issue, so no window falls on either side of the first observation")
	}
	if firstSlot.IsZero() || firstClaim.IsZero() {
		return Windows{}, errors.New("the project holds no slot or no claim observation, so slot wait or idle time reads no data in every window")
	}
	boundary := nextMidnight(slices.MaxFunc([]time.Time{history.FirstObserved, firstSlot, firstClaim}, time.Time.Compare))
	before := Window{From: measures.Midnight(history.FirstFinished), To: boundary}
	if !before.From.Before(history.FirstObserved) {
		return Windows{}, fmt.Errorf("the first issue finished on %s, a day that starts at or after the first observation (%s), so no window starts on a day with input before it",
			before.From.Format(time.DateOnly), history.FirstObserved.Format(time.RFC3339))
	}
	if before.To.Sub(before.From) > maxWindow {
		before.From = before.To.Add(-maxWindow)
	}
	after := Window{From: boundary, To: measures.Midnight(history.To).AddDate(0, 0, 1)}
	if after.To.Sub(after.From) > maxWindow {
		after.To = after.From.Add(maxWindow)
	}
	return Windows{Before: before, After: after}, nil
}

// firstObservations is the moments of project's first slot and first claim observation, from which
// slot wait and idle time have input (R6), each zero where the project has none.
func firstObservations(ctx context.Context, pool *store.Pool, project string) (time.Time, time.Time, error) {
	var slot, claim *time.Time
	if err := pool.QueryRow(ctx, `
		select min(observed_at) filter (where field = $2), min(observed_at) filter (where starts_with(field, $3))
		from legion_observations
		where project = $1`, project, legion.FieldSlot, legion.ClaimFieldPrefix).Scan(&slot, &claim); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("read the first slot and claim observations of %s: %w", project, err)
	}
	var firstSlot, firstClaim time.Time
	if slot != nil {
		firstSlot = slot.UTC()
	}
	if claim != nil {
		firstClaim = claim.UTC()
	}
	return firstSlot, firstClaim, nil
}

// LegionResult is what the Legion and Base modes wrote: the history, how many of its issues
// finished, when the project's observations begin, and the two windows.
type LegionResult struct {
	Project       string    `json:"project"`
	Issues        int       `json:"issues"`
	Finished      int       `json:"finished_issues"`
	Trees         int       `json:"trees"`
	Unobserved    int       `json:"unobserved_trees"`
	FirstObserved time.Time `json:"first_observation"`
	FirstSlot     time.Time `json:"first_slot_observation"`
	FirstClaim    time.Time `json:"first_claim_observation"`
	FirstFinished time.Time `json:"first_finished"`
	From          time.Time `json:"history_from"`
	To            time.Time `json:"history_to"`
	Windows       Windows   `json:"windows"`
	// Tables is the tables the mode wrote, and DerivedByThisHead this head's tables derived from
	// them (DerivedTables), which no mode writes.
	Tables            []string `json:"tables"`
	DerivedByThisHead []string `json:"derived_by_this_head"`
	// History is the history as legionseed answered it, for a caller that asserts against its
	// keys.
	History legionseed.Fixture `json:"-"`
}

// WriteLegion is the Legion mode: LegionSpec's history of project into the database pool is on,
// which must hold this head's schema, and the project's legion_settings row.
func WriteLegion(ctx context.Context, pool *store.Pool, project string, issues int, now time.Time) (LegionResult, error) {
	if err := requireTables(ctx, pool, append(slices.Clone(LegionTables), DerivedTables...)); err != nil {
		return LegionResult{}, fmt.Errorf("-legion writes into this head's schema: %w (a database of the base takes -base)", err)
	}
	return writeLegion(ctx, pool, project, issues, now)
}

// WriteBase is the Base mode: the Legion mode's history and settings row into a database of the
// base's schema, main's at the commit this head merges, refused for one that holds this head's
// day rows (HeadTable).
func WriteBase(ctx context.Context, pool *store.Pool, project string, issues int, now time.Time) (LegionResult, error) {
	if err := requireTables(ctx, pool, LegionTables); err != nil {
		return LegionResult{}, fmt.Errorf("-base writes into the base's schema: %w", err)
	}
	held, err := ExistingTables(ctx, pool, []string{HeadTable})
	if err != nil {
		return LegionResult{}, err
	}
	if len(held) > 0 {
		return LegionResult{}, fmt.Errorf("-base writes into the base's schema, and this database holds %s, which is this head's: load it with -legion", HeadTable)
	}
	return writeLegion(ctx, pool, project, issues, now)
}

func writeLegion(ctx context.Context, pool *store.Pool, project string, issues int, now time.Time) (LegionResult, error) {
	offset, err := issueOffset(ctx, pool, project)
	if err != nil {
		return LegionResult{}, err
	}
	spec := LegionSpec(project, issues, now, offset)
	history, err := legionseed.Write(ctx, pool, spec)
	if err != nil {
		return LegionResult{}, err
	}
	if err := observedCases(ctx, pool, history); err != nil {
		return LegionResult{}, err
	}
	if _, err := legion.PutSettings(ctx, pool, project, StateURL, actor); err != nil {
		return LegionResult{}, err
	}
	firstSlot, firstClaim, err := firstObservations(ctx, pool, project)
	if err != nil {
		return LegionResult{}, err
	}
	windows, err := WindowsOf(history, firstSlot, firstClaim)
	if err != nil {
		return LegionResult{}, err
	}
	return LegionResult{
		Project: project, Issues: len(history.Keys), Finished: spec.Trees * legionseed.FinishedPerTree,
		Trees: spec.Trees, Unobserved: spec.Unobserved,
		FirstObserved: history.FirstObserved, FirstSlot: firstSlot, FirstClaim: firstClaim, FirstFinished: history.FirstFinished,
		From: history.From, To: history.To, Windows: windows,
		Tables: LegionTables, DerivedByThisHead: DerivedTables, History: history,
	}, nil
}

// DeliveryResult is what the Delivery mode wrote: deliveryseed's summary line, its two ends, and
// the deploy repository's newest pull request, one a MergePR can take.
type DeliveryResult struct {
	Summary string    `json:"summary"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Newest  string    `json:"newest_deploy_repository_pull_request"`
	// Fixture is the history as deliveryseed answered it.
	Fixture deliveryseed.Fixture `json:"-"`
}

// WriteDelivery is the Delivery mode: days days of deliveryseed's delivery history ending at now
// (deliveryseed.DefaultDays where days is 0 or less), every pull request counted for project
// (deliveryseed.Spec.OneProject), its issues numbered after the ones the project has already, and
// seed its random draws.
func WriteDelivery(ctx context.Context, pool *store.Pool, project string, days int, seed uint64, now time.Time) (DeliveryResult, error) {
	offset, err := issueOffset(ctx, pool, project)
	if err != nil {
		return DeliveryResult{}, err
	}
	written, err := deliveryseed.Write(ctx, pool, deliveryseed.Spec{
		Project: project, Now: now.UTC(), Days: days, Seed: seed, OneProject: true, IssueOffset: offset,
	})
	if err != nil {
		return DeliveryResult{}, err
	}
	result := DeliveryResult{Summary: written.Summary(), From: written.From, To: written.To, Fixture: written}
	for _, pr := range written.PullRequests {
		if pr.Repo == written.DeployRepo {
			result.Newest = fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
		}
	}
	return result, nil
}

// MergeResult is one pull request MergePR merged: when it is merged now, and when it was merged
// before, with the UTC day of each; the start day of the deploy run that ships it now and of the
// one that shipped it before (ShipDay and WasShipDay, nil for none); and DaysAffected, each of
// those days once, which are the days whose rows the merge moves.
type MergeResult struct {
	PullRequest  string     `json:"pull_request"`
	MergedAt     time.Time  `json:"merged_at"`
	MergeDay     string     `json:"merge_day"`
	WasMergedAt  *time.Time `json:"was_merged_at"`
	WasMergeDay  *string    `json:"was_merge_day"`
	ShipDay      *string    `json:"ship_day"`
	WasShipDay   *string    `json:"was_ship_day"`
	DaysAffected []string   `json:"days_affected"`
}

// MergePR is the MergePR mode: the pull request ref names (`<owner>/<repo>#<number>`) merged at
// at, refused where no row holds it or where at is before it was opened. A pull request the
// history merged already is merged again at at, so both its old and its new merge day move, and
// so does the day of each deploy run that ships it, before and after (shipDays): a deploy's row is
// on the day the deploy run started, and it counts the pull requests the deploy ships.
func MergePR(ctx context.Context, pool *store.Pool, ref string, at time.Time) (MergeResult, error) {
	repo, number, err := parsePullRequest(ref)
	if err != nil {
		return MergeResult{}, err
	}
	at = at.UTC()
	var created *time.Time
	var was *time.Time
	if err := pool.QueryRow(ctx, `select created_at, merged_at from delivery_pull_requests where repo = $1 and number = $2`,
		repo, number).Scan(&created, &was); err != nil {
		return MergeResult{}, fmt.Errorf("read the pull request %s: %w", ref, err)
	}
	if created != nil && at.Before(*created) {
		return MergeResult{}, fmt.Errorf("%s was opened at %s, after %s, so it cannot merge then", ref, created.Format(time.RFC3339), at.Format(time.RFC3339))
	}
	result := MergeResult{PullRequest: ref, MergedAt: at, MergeDay: at.Format(time.DateOnly)}
	if was != nil {
		result.WasMergedAt, result.WasMergeDay = new(was.UTC()), new(was.UTC().Format(time.DateOnly))
	}
	if result.ShipDay, result.WasShipDay, err = shipDays(ctx, pool, repo, at, was); err != nil {
		return MergeResult{}, err
	}
	if _, err := pool.Exec(ctx, `update delivery_pull_requests set merged_at = $3, updated_at = now() where repo = $1 and number = $2`,
		repo, number, at); err != nil {
		return MergeResult{}, fmt.Errorf("merge %s: %w", ref, err)
	}
	for _, day := range []*string{&result.MergeDay, result.WasMergeDay, result.ShipDay, result.WasShipDay} {
		if day != nil {
			result.DaysAffected = append(result.DaysAffected, *day)
		}
	}
	slices.Sort(result.DaysAffected)
	result.DaysAffected = slices.Compact(result.DaysAffected)
	return result, nil
}

// shipDays is the UTC start day of the deploy run that ships a pull request of repo merged at at,
// and of the one that ships it merged at was (nil for a pull request that had not merged): the
// run of its first shipping apply (delivery.FirstShippingApply over the deploy repository's
// production applies, as delivery.ContainingRun ships a pull request). Each is nil where no run
// ships it, and both are nil for a repository that is not the deploy repository the delivery
// settings name, or where no settings are written.
func shipDays(ctx context.Context, pool *store.Pool, repo string, at time.Time, was *time.Time) (*string, *string, error) {
	settings, err := delivery.GetSettings(ctx, pool)
	if errors.Is(err, delivery.ErrNoSettings) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the delivery settings: %w", err)
	}
	if repo != settings.DeployRepo {
		return nil, nil, nil
	}
	from := at
	if was != nil && was.Before(from) {
		from = *was
	}
	read, err := delivery.ReadApplyRows(ctx, pool, settings, from)
	if err != nil {
		return nil, nil, fmt.Errorf("read the deploy runs of %s created from %s on: %w", repo, from.Format(time.RFC3339), err)
	}
	applies := delivery.ProductionApplies(read.Runs, read.Jobs, settings.Production()).Applies
	started := make(map[int64]time.Time, len(read.Runs))
	for _, run := range read.Runs {
		started[run.RunID] = run.StartedAt
	}
	shipDay := func(merged *time.Time) *string {
		if merged == nil {
			return nil
		}
		apply := delivery.FirstShippingApply(*merged, applies)
		if apply == nil {
			return nil
		}
		return new(started[apply.RunID].UTC().Format(time.DateOnly))
	}
	return shipDay(&at), shipDay(was), nil
}

// parsePullRequest reads `<owner>/<repo>#<number>`.
func parsePullRequest(ref string) (string, int, error) {
	repo, rest, found := strings.Cut(ref, "#")
	var number int
	if !found || !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("name the pull request as <owner>/<repo>#<number>, not %q", ref)
	}
	if _, err := fmt.Sscanf(rest, "%d", &number); err != nil || number <= 0 || fmt.Sprint(number) != rest {
		return "", 0, fmt.Errorf("name the pull request as <owner>/<repo>#<number>, not %q", ref)
	}
	return repo, number, nil
}

// issueOffset is the highest issue number project holds already, 0 for a project with none: the
// number a history written beside those issues numbers its own after.
func issueOffset(ctx context.Context, pool *store.Pool, project string) (int, error) {
	var offset int
	if err := pool.QueryRow(ctx, `select coalesce(max(number), 0) from issues where project_key = $1`, project).Scan(&offset); err != nil {
		return 0, fmt.Errorf("read the issue numbers of %s: %w", project, err)
	}
	return offset, nil
}

// SettleNumbers moves each project's next issue number past every issue it holds, after the modes
// wrote issues beside one another (each writer keeps a project row it finds), so the next issue
// the server creates in it takes a number no fixture issue holds.
func SettleNumbers(ctx context.Context, pool *store.Pool, projects []string) error {
	if _, err := pool.Exec(ctx, `
		update projects p
		set next_number = greatest(p.next_number, (select coalesce(max(i.number), 0) + 1 from issues i where i.project_key = p.key))
		where p.key = any($1)
	`, projects); err != nil {
		return fmt.Errorf("settle the next issue numbers of %s: %w", strings.Join(projects, ", "), err)
	}
	return nil
}

// requireTables refuses a database that lacks any of tables, naming each it lacks.
func requireTables(ctx context.Context, pool *store.Pool, tables []string) error {
	held, err := ExistingTables(ctx, pool, tables)
	if err != nil {
		return err
	}
	var missing []string
	for _, table := range tables {
		if !slices.Contains(held, table) {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the database has no table %s", strings.Join(missing, ", "))
	}
	return nil
}

// ExistingTables is those of tables the database holds in its search path: what this package's
// modes and evalbench's schema checks read a database's head by.
func ExistingTables(ctx context.Context, pool *store.Pool, tables []string) ([]string, error) {
	rows, err := pool.Query(ctx, `select name from unnest($1::text[]) as name where to_regclass(name) is not null`, tables)
	if err != nil {
		return nil, fmt.Errorf("read which tables the database holds: %w", err)
	}
	defer rows.Close()
	var held []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read which tables the database holds: %w", err)
		}
		held = append(held, name)
	}
	return held, rows.Err()
}

// nextMidnight is the first UTC midnight at or after at.
func nextMidnight(at time.Time) time.Time {
	day := measures.Midnight(at)
	if day.Before(at) {
		day = day.AddDate(0, 0, 1)
	}
	return day
}

// deliverySettingsOf is the delivery_settings row the Attribution mode writes, naming deployRepo,
// its production job and its PR-checks workflow, and author as the one population author.
func deliverySettingsOf(deployRepo string) delivery.DeliverySettings {
	return delivery.DeliverySettings{
		DeployRepo: deployRepo, DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{productionJob}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{author}, ExcludedRepos: []string{},
	}
}
