// Package evalfixture loads the histories the eval registry's checks read (dispatch://INFRA-2916,
// k6, k12, k14, k15 and k17) into a database that already holds Dispatch's schema: the local
// stack's, which migrates its database when it starts, or the base head's for k14. Each mode adds
// to one project and writes that project's row only where it has none, so modes combine; cmd is
// the command a checker types.
//
// The modes:
//
//   - Legion (-legion): legiontest's Legion history, at least the asked number of finished issues,
//     with phase, slot and claim observations that start partway through it (LegionSpec), and the
//     project's legion_settings row, which the Delivery page's Legion section draws a project by.
//     It answers the two day-aligned windows k6 reads (Windows): one that starts before the
//     project's first observation and one that starts after it.
//   - Base (-base): the same history into a database of the base head 02a7b3c6, for k14's
//     base binary. The Legion history is written into the same tables in both, since every table
//     it writes is one the base has; what this head adds (BaseLacks) this head derives from those
//     rows and the fixture never writes, so the mode differs from Legion only in the schema it
//     requires of the database.
//   - Attribution (-attribution): k15's case of D1 (Attribution).
//   - Delivery (-delivery): deliverytest's delivery history of the project, a week by default.
//   - MergePR (-merge-pr): one pull request of those rows merged at a given moment, k12's one
//     change.
//
// Every time is relative to the moment the caller names (-now), so two loads with one -now hold
// the same history at the same times. The rows bypass the server's own writers (legiontest's
// events bypass events.Broker.Append), so a page open while they land may need a reload; the
// refresher reads them on its next pass, as it reads every table.
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

// LegionTables are the tables the Legion history is written into; the base head 02a7b3c6 has
// every one of them, with the columns the history writes (delivery_pull_requests the merged pull
// requests of legiontest.Spec.Merged).
var LegionTables = []string{"projects", "issues", "events", "handoffs", "legion_observations", "delivery_pull_requests", "legion_settings"}

// BaseLacks are the tables of this head's Legion evals that the base head 02a7b3c6 does not have
// (its migrations end at 20261112193821): the filings, the session titles, the registry and its
// day rows. This head derives every one of them from LegionTables' rows (the session titles by a
// trigger on events, the rest by the server's facts consumer and refresher), so the fixture writes
// none of them in any mode, and a database holding eval_days is this head's.
var BaseLacks = []string{
	"issue_filings", "session_titles", "session_titles_backfill", "eval_versions", "eval_series",
	"eval_readings", "eval_days", "eval_day_members", "eval_days_cursor", "eval_days_walks",
}

// LegionSpec is the Legion history of project the Legion mode writes: enough trees for at least
// issues finished issues, ending at now, with the older half of the trees unobserved and every
// observed root waiting for its slot (legiontest.Spec's Unobserved and Queued), finished issues
// that sat parked in `icebox` and in `backlog`, a merged pull request for each issue the merger
// closes and filings of all four classes (Parked, Merged and Filings, what k6 and k17 read),
// numbered after the offset issue numbers the project has already.
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

// Windows is the two windows k6 reads over a Legion history: Before starts on the day of its first
// finished issue, which is the first day a Legion measure has input, and ends at the first UTC
// midnight at or after its first observation, so it starts before that observation; After starts
// at that midnight and ends at the midnight after the history's end. Each is at most the 84 days
// the routes read.
type Windows struct {
	Before Window `json:"before"`
	After  Window `json:"after"`
}

// WindowsOf is the two windows of a Legion history, refused for a history with no observation or
// none before its first finished issue's day.
func WindowsOf(history legiontest.Fixture) (Windows, error) {
	if history.FirstObserved.IsZero() || history.FirstFinished.IsZero() {
		return Windows{}, errors.New("the history holds no observation or no finished issue, so no window falls on either side of the first observation")
	}
	boundary := nextMidnight(history.FirstObserved)
	before := Window{From: dayOf(history.FirstFinished), To: boundary}
	if !before.From.Before(history.FirstObserved) {
		return Windows{}, fmt.Errorf("the first issue finished on %s, the day of the first observation (%s), so no window starts on a day with input before it",
			before.From.Format(time.DateOnly), history.FirstObserved.Format(time.RFC3339))
	}
	if before.To.Sub(before.From) > maxWindow {
		before.From = before.To.Add(-maxWindow)
	}
	after := Window{From: boundary, To: dayOf(history.To).AddDate(0, 0, 1)}
	if after.To.Sub(after.From) > maxWindow {
		after.To = after.From.Add(maxWindow)
	}
	return Windows{Before: before, After: after}, nil
}

// LegionResult is what the Legion and Base modes wrote: the history, how many of its issues
// finished, and the two windows.
type LegionResult struct {
	Project       string    `json:"project"`
	Issues        int       `json:"issues"`
	Finished      int       `json:"finished_issues"`
	Trees         int       `json:"trees"`
	Unobserved    int       `json:"unobserved_trees"`
	FirstObserved time.Time `json:"first_observation"`
	FirstFinished time.Time `json:"first_finished"`
	From          time.Time `json:"history_from"`
	To            time.Time `json:"history_to"`
	Windows       Windows   `json:"windows"`
	// Tables is the tables the mode wrote, and DerivedByThisHead this head's tables derived from
	// them, which the base lacks and no mode writes.
	Tables            []string `json:"tables"`
	DerivedByThisHead []string `json:"derived_by_this_head"`
	// History is the history as legiontest answered it, for a caller that asserts against its
	// keys.
	History legiontest.Fixture `json:"-"`
}

// WriteLegion is the Legion mode: LegionSpec's history of project into the database pool is on,
// which must hold this head's schema, and the project's legion_settings row.
func WriteLegion(ctx context.Context, pool *store.Pool, project string, issues int, now time.Time) (LegionResult, error) {
	if err := requireTables(ctx, pool, append(slices.Clone(LegionTables), BaseLacks...)); err != nil {
		return LegionResult{}, fmt.Errorf("-legion writes into this head's schema: %w (a database of the base head takes -base)", err)
	}
	return writeLegion(ctx, pool, project, issues, now)
}

// WriteBase is the Base mode: the Legion mode's history and settings row into a database of the
// base head's schema, refused for one that holds this head's eval_days.
func WriteBase(ctx context.Context, pool *store.Pool, project string, issues int, now time.Time) (LegionResult, error) {
	if err := requireTables(ctx, pool, LegionTables); err != nil {
		return LegionResult{}, fmt.Errorf("-base writes into the base head's schema: %w", err)
	}
	held, err := existingTables(ctx, pool, []string{"eval_days"})
	if err != nil {
		return LegionResult{}, err
	}
	if len(held) > 0 {
		return LegionResult{}, errors.New("-base writes into the base head's schema, and this database holds eval_days, which is this head's: load it with -legion")
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
	windows, err := WindowsOf(history)
	if err != nil {
		return LegionResult{}, err
	}
	return LegionResult{
		Project: project, Issues: len(history.Keys), Finished: spec.Trees * legiontest.FinishedPerTree,
		Trees: spec.Trees, Unobserved: spec.Unobserved,
		FirstObserved: history.FirstObserved, FirstFinished: history.FirstFinished,
		From: history.From, To: history.To, Windows: windows,
		Tables: LegionTables, DerivedByThisHead: BaseLacks, History: history,
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

// WriteDelivery is the Delivery mode: days days of deliverytest's delivery history ending at now,
// every pull request counted for project (deliverytest.Spec.OneProject), its issues numbered after
// the ones the project has already, and seed its random draws.
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
// before, with the UTC day of each, which are the days whose rows the merge moves.
type MergeResult struct {
	PullRequest  string     `json:"pull_request"`
	MergedAt     time.Time  `json:"merged_at"`
	MergeDay     string     `json:"merge_day"`
	WasMergedAt  *time.Time `json:"was_merged_at"`
	WasMergeDay  *string    `json:"was_merge_day"`
	DaysAffected []string   `json:"days_affected"`
}

// MergePR is the MergePR mode: the pull request ref names (`<owner>/<repo>#<number>`) merged at
// at, refused where no row holds it or where at is before it was opened. A pull request the
// history merged already is merged again at at, so both its old and its new merge day move.
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
	if _, err := pool.Exec(ctx, `update delivery_pull_requests set merged_at = $3, updated_at = now() where repo = $1 and number = $2`,
		repo, number, at); err != nil {
		return MergeResult{}, fmt.Errorf("merge %s: %w", ref, err)
	}
	result := MergeResult{PullRequest: ref, MergedAt: at, MergeDay: at.Format(time.DateOnly), DaysAffected: []string{at.Format(time.DateOnly)}}
	if was != nil {
		day := was.UTC().Format(time.DateOnly)
		result.WasMergedAt, result.WasMergeDay = new(was.UTC()), &day
		if day != result.MergeDay {
			result.DaysAffected = append([]string{day}, result.DaysAffected...)
		}
	}
	return result, nil
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
	held, err := existingTables(ctx, pool, tables)
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

// existingTables is those of tables the database holds in its search path.
func existingTables(ctx context.Context, pool *store.Pool, tables []string) ([]string, error) {
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

// dayOf is the UTC midnight of the day at is on.
func dayOf(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}

// nextMidnight is the first UTC midnight at or after at.
func nextMidnight(at time.Time) time.Time {
	day := dayOf(at)
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
