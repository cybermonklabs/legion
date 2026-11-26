// Package evalfixture loads the histories the eval registry's checks read (dispatch://INFRA-2916,
// k6, k12 and k15, the base whose first paint k8's and k13's negative controls read, and
// dispatch://INFRA-978's checks of the record of deploys and failed changes) into a database that
// already holds Dispatch's schema: the local stack's, which migrates its database when it starts,
// or the base's, main at the commit this head merges. cmd is the command a checker types.
//
// The modes:
//
//   - Legion (-legion): legiontest's Legion history, at least the asked number of finished issues,
//     with phase, slot and claim observations that start partway through it (LegionSpec), and the
//     project's legion_settings row, which names the project's daemon.
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
//   - Delivery (-delivery): deliverytest's delivery history of the project over the days its
//     caller names (the command's -days, a week by default), or deliverytest's own default of
//     120 days (deliverytest.DefaultDays) for a caller that names none.
//   - MergePR (-merge-pr): one pull request of those rows merged at a given moment, k12's one
//     change.
//   - Failures (-failures): INFRA-978's two projects with their deploy sources, deploys and
//     failed changes, each written through the writer production uses (WriteFailures).
//
// Legion adds to one project and writes that project's row only where it has none, so it combines
// with Attribution, Delivery, Failures and MergePR; Base writes the same history alone, into a
// database of the base. Delivery adds to one project too, beside deliverytest's second project,
// and gives the project the deploy repository as its default repository where it has none;
// Attribution writes its three projects with their default repositories; and each of the two, as
// Failures does for its two projects, saves the project's delivery settings, so none of the three
// is combined with another. MergePR writes no project's rows.
//
// Every time is relative to the moment the caller names (-now), so two loads with one -now hold
// the same history at the same times. The rows bypass the server's own writers (legiontest's
// events bypass events.Broker.Append), so a page open while they land may need a reload; the
// refresher reads them on its next pass, as it reads every table. The Legion history's events are
// dated in the past, so the statement that writes them also nulls the keyword fill's stored
// reach (legiontest's write), and a serving fill writes their rows with no breach of a reach it
// stored before them.
package evalfixture

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// actor is the actor every row this package writes that records one names.
var actor = model.Actor{Kind: "system", ID: "evalfixture"}

// StateURL is the daemon address a fixture project's legion_settings row names. It is under
// `.invalid`, which no name server answers (RFC 2606), so the sampler's every read of it fails and
// records the failure, and no read adds an observation to the history the fixture wrote.
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
// merged pull requests of legiontest.Spec.Merged).
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
	"eval_days_cursor", "eval_days_walks", "eval_days_serving", "eval_days_settings", "eval_days_rules",
	"eval_rebuilds", "eval_rebuild_walks", "eval_rebuild_firsts", "eval_rebuild_days", "eval_rebuild_members",
	"eval_rebuild_range_figures", "eval_rebuild_day_figures", "eval_rebuild_reverify",
	"eval_range_figures", "eval_day_figures", "eval_figures_built", "eval_figure_cuts_stale",
}

// HeadTable is the table that tells a database of this head from one of the base: the registry's
// day rows, which only this head's migrations create.
const HeadTable = "eval_days"

// LegionSpec is the Legion history of project the Legion mode writes: enough trees for at least
// issues finished issues, ending at now, with the older half of the trees unobserved and every
// observed root waiting for its slot (legiontest.Spec's Unobserved and Queued), finished issues
// that sat parked in `icebox` and in `backlog`, a merged pull request for each issue the merger
// closes and filings of all four classes (Parked, Merged and Filings, what k6 reads), numbered
// after the offset issue numbers the project has already.
func LegionSpec(project string, issues int, now time.Time, offset int) legiontest.Spec {
	if issues <= 0 {
		issues = DefaultIssues
	}
	trees := (issues + legiontest.FinishedPerTree - 1) / legiontest.FinishedPerTree
	return legiontest.Spec{
		Project: project, Trees: trees, Now: now.UTC(),
		Spacing: treeSpacing, Unobserved: trees / 2, Queued: true, IssueOffset: offset,
		Parked: true, Merged: true, Filings: true,
	}
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
func WindowsOf(history legiontest.Fixture, firstSlot, firstClaim time.Time) (Windows, error) {
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
	// History is the history as legiontest answered it, for a caller that asserts against its
	// keys.
	History legiontest.Fixture `json:"-"`
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
	history, err := legiontest.Write(ctx, pool, spec)
	if err != nil {
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
		Project: project, Issues: len(history.Keys), Finished: spec.Trees * legiontest.FinishedPerTree,
		Trees: spec.Trees, Unobserved: spec.Unobserved,
		FirstObserved: history.FirstObserved, FirstSlot: firstSlot, FirstClaim: firstClaim, FirstFinished: history.FirstFinished,
		From: history.From, To: history.To, Windows: windows,
		Tables: LegionTables, DerivedByThisHead: DerivedTables, History: history,
	}, nil
}

// DeliveryResult is what the Delivery mode wrote: deliverytest's summary line, its two ends, and
// the deploy repository's newest pull request, one a MergePR can take.
type DeliveryResult struct {
	Summary string    `json:"summary"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Newest  string    `json:"newest_deploy_repository_pull_request"`
	// Fixture is the history as deliverytest answered it.
	Fixture deliverytest.Fixture `json:"-"`
}

// WriteDelivery is the Delivery mode: days days of deliverytest's delivery history ending at now
// (deliverytest.DefaultDays where days is 0 or less), every pull request counted for project
// (deliverytest.Spec.OneProject), its issues numbered after the ones the project has already, and
// seed its random draws.
func WriteDelivery(ctx context.Context, pool *store.Pool, project string, days int, seed uint64, now time.Time) (DeliveryResult, error) {
	offset, err := issueOffset(ctx, pool, project)
	if err != nil {
		return DeliveryResult{}, err
	}
	written, err := deliverytest.Write(ctx, pool, deliverytest.Spec{
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
// run of its first shipping apply (delivery.FirstShippingApply over the repository's production
// applies under the rule of the source that reads it, as delivery.ContainingRun ships a pull
// request). Each is nil where no run ships it, and both are nil for a repository that no
// delivery-on project's source reads, or where no project's settings are written.
func shipDays(ctx context.Context, pool *store.Pool, repo string, at time.Time, was *time.Time) (*string, *string, error) {
	rules, err := delivery.ReadSourceRules(ctx, pool)
	if err != nil {
		return nil, nil, fmt.Errorf("read the delivery settings: %w", err)
	}
	settings, source := rules.For(repo)
	if !source {
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

// deliverySettingsOf is the rule the Attribution mode's source holds, naming deployRepo, its
// production job and its PR-checks workflow, and author as the one population author.
func deliverySettingsOf(deployRepo string) delivery.DeliverySettings {
	return delivery.DeliverySettings{
		DeployRepo: deployRepo, DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{productionJob}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{author}, ExcludedRepos: []string{},
	}
}

// PutDeliverySettings saves settings as project's delivery settings (project_delivery_settings and
// project_deploy_sources, through delivery.PutProjectSettings in a transaction of its own):
// delivery on, settings' population rule, and one github_workflow source holding settings' rule,
// which names no repository where settings.DeployRepo is the project's default repository, so it
// reads that default and moves with it, and names settings.DeployRepo otherwise. The project's row
// exists first.
func PutDeliverySettings(ctx context.Context, pool *store.Pool, project string, settings delivery.DeliverySettings, by model.Actor) (delivery.ProjectSettings, error) {
	var defaultRepo *string
	if err := pool.QueryRow(ctx, `select default_repo from projects where key = $1`, project).Scan(&defaultRepo); err != nil {
		return delivery.ProjectSettings{}, fmt.Errorf("read the default repository of %s: %w", project, err)
	}
	var repo *string
	if defaultRepo == nil || !strings.EqualFold(*defaultRepo, settings.DeployRepo) {
		repo = new(settings.DeployRepo)
	}
	return saveSettings(ctx, pool, delivery.ProjectSettingsInput{
		Project: project, Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(settings, repo)},
		PopulationAuthors: settings.PopulationAuthors, ExcludedRepos: settings.ExcludedRepos,
	}, by)
}

// saveSettings saves input through delivery.PutProjectSettings in a transaction of its own, as the
// settings route does, and drains the change rows it logged after the commit.
func saveSettings(ctx context.Context, pool *store.Pool, input delivery.ProjectSettingsInput, by model.Actor) (delivery.ProjectSettings, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return delivery.ProjectSettings{}, fmt.Errorf("begin the delivery settings of %s: %w", input.Project, err)
	}
	defer tx.Rollback(ctx)
	saved, err := delivery.PutProjectSettings(ctx, tx, input, by)
	if err != nil {
		return delivery.ProjectSettings{}, fmt.Errorf("save the delivery settings of %s: %w", input.Project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return delivery.ProjectSettings{}, fmt.Errorf("commit the delivery settings of %s: %w", input.Project, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of the fixture's delivery settings")
	return saved, nil
}
