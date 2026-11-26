package legionfacts

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ProjectWrite is one project's computed facts and the set the pass that computed them stated:
// what WriteProject writes in one call. The project stands beside the rows rather than being
// read out of them, because a project whose rows are all stale answers no row at all and still
// owes its deletion.
//
// Stated is the set of keys the pass computed over, inside which it may delete a stale row, and
// Whole says the pass read the project whole (ComputeProject), whose stated set is every issue
// the project has and whose Stated is therefore empty.
//
// Build is the build that computed the rows (Deps.Build), which each row it writes records as
// the build that wrote it (written_build). The build a project's record names reads those
// stamps to find the rows another build wrote during a roll (otherBuildsRows). A caller outside
// the consumer's loop, a test or a fixture, may leave it empty: the consumer then reads such a
// row as another build's and computes it again.
//
// Yielded says the project's record names a newer build than Build, rolling in while this one
// still serves (legionProjects). Such a write deletes no row: a row Build's rule computes none for
// may be one the newer build's rule keeps, and once deleted it would leave no row of Build's for
// the newer build to find and compute again (otherBuildsRows), so it would stay missing until an
// input named its issue. The write stamps each row it would have deleted with Build instead, and
// the newer build's next pass computes it again and keeps or deletes it by its own rule.
type ProjectWrite struct {
	Project string
	Rows    []Row
	Stated  []string
	Whole   bool
	Build   string
	Yielded bool
}

// WriteProject writes one project's computed facts: it upserts the rows the computation answered
// (upsert) and deletes the stored rows inside the stated set the computation answered none for
// (deleteStaleRows, or deleteStaleProjectRows for a pass that read the project whole), or stamps
// them where the write is Yielded. It answers every key whose stored row moved, the keys it
// deleted or stamped after the keys it wrote, because a row that went is a change of the stored
// facts as much as a row that moved.
//
// A stored row means Legion worked the issue (dispatch://INFRA-2915), and an issue loses its
// phase work as well as gains it: a session's newest title decides which issue that session's
// events work (legion.PhaseWorkKeys), so a role session that writes next on another issue under
// that issue's title takes the work with it, and the issue it left carries none. The deletion
// is what takes that issue's row away; an upsert alone leaves it standing for ever.
//
// It is the one write of the facts: the consumer's pass makes it for each project of the pass,
// inside the transaction that moves the cursors (consumer.go's writePass), and a caller that
// holds one project's rows makes it alone. It writes no filing (WriteFilings), so a caller that
// holds rows alone cannot take a filing away.
func WriteProject(ctx context.Context, q Querier, write ProjectWrite) ([]string, error) {
	written, err := upsert(ctx, q, write.Project, write.Build, write.Rows)
	if err != nil {
		return nil, err
	}
	kept := computedKeys(write.Rows)
	deleted := []string{}
	if write.Whole {
		deleted, err = deleteStaleProjectRows(ctx, q, write, kept)
	} else {
		deleted, err = deleteStaleRows(ctx, q, write, kept)
	}
	if err != nil {
		return nil, err
	}
	return append(written, deleted...), nil
}

// FilingsWrite is one project's computed filings and the set the pass that computed them
// stated: what WriteFilings writes in one call, beside the same pass's ProjectWrite. Stated is
// the filed issues the pass computed filings over, inside which it may delete a stale filing,
// and Whole says the pass read every filing a Legion session of the project made (a whole read
// of the project, ComputeProject), whose Stated is therefore empty.
//
// Build and Yielded are ProjectWrite's, for the filings: each filing written records Build as the
// build that wrote it (written_build), which the build a project's record names reads to compute
// again the filings another build wrote (otherBuildsFilings), and a Yielded write deletes no
// filing but stamps each it would have deleted with Build, since the rule that decides whose
// filing an issue is lives in the build as the phase-work rule does.
type FilingsWrite struct {
	Project string
	Filings []Filing
	Stated  []string
	Whole   bool
	Build   string
	Yielded bool
}

// WriteFilings writes one project's computed filings, as WriteProject writes its rows: it upserts
// the filings the computation answered (upsertFilings) and deletes the project's stored filings
// inside the stated set the computation answered none for (deleteStaleFilings), or stamps them
// where the write is Yielded. A stored filing goes when the session that filed it is no Legion
// session of the project any more, as its newest title reads, and its row moves when its class,
// its filer's phase or the filed issue's tree does. The consumer's pass makes it in the
// transaction that writes the same pass's rows.
func WriteFilings(ctx context.Context, q Querier, write FilingsWrite) error {
	if err := upsertFilings(ctx, q, write.Project, write.Build, write.Filings); err != nil {
		return err
	}
	return deleteStaleFilings(ctx, q, write)
}

// computedKeys is the issue of each row: the keys the computation answered a row for, which the
// deletion keeps.
func computedKeys(rows []Row) []string {
	keys := make([]string, len(rows))
	for index, row := range rows {
		keys[index] = row.Issue
	}
	return keys
}

// upsert writes the rows of the project and answers which issues it changed, by key, in the
// order Postgres wrote them. It deletes nothing, so its caller takes the stale rows away
// (WriteProject). A row whose stored values already match is left alone: a pass whose inputs
// did not move rewrites nothing, and a check can count what moved.
//
// The project is the caller's and not the first row's: a write of no row still belongs to a
// project, and the loop below refuses a row of any other project, so the rows and the project
// this writes into cannot disagree.
//
// One statement, so a whole read of hundreds of issues is one round trip. The rows go in as
// arrays unnested into a row each, as the sampler's own write does (legion/store.go's
// RecordObservations), and the conflict's `do update` carries the whole row's comparison in its
// where clause, so Postgres decides what changed and `returning` answers exactly the rows it
// touched. updated_at moves only with a row that changed, because an unchanged row is never
// updated at all. The comparison holds every column but updated_at, so a row whose stored values
// differ from the computed ones in any column is rewritten by the first pass that computes it.
// The hours, the bars, the idle hours and the growth are compared as jsonb, so a Go encoding that
// ordered an object's keys differently from jsonb cannot read as a change.
//
// A row's values are closed at as_of, the newest input its facts read, and carry no clock of
// their own (metrics.ProjectFactsClosed), so a pass that computes an issue whose inputs did not
// move answers the values the table holds, whatever moment it ran at, and writes nothing. The
// build is compared with the rest: a row another build wrote is rewritten by the build a
// project's record names when it computes it, even where the two rules answer the same values,
// so the stamp says which rule the stored values were last computed by (otherBuildsRows).
//
// updated_at is the row's own UpdatedAt, the moment it was written; a row that carries none takes
// the moment this write started. A rewrite stores it or a microsecond past the stored updated_at,
// whichever is later, so every rewrite moves updated_at. Every row of one pass carries the pass's
// one moment (pass), so a pass that writes an issue on two of its pages writes it twice at one
// UpdatedAt, and a second write that moved only values the board's revision of the issue leaves
// out, the values that grow (api's legionBoardView), would otherwise leave that revision where the
// first write put it, and a page holding the first write's values would keep them. A row that
// carries no AsOf stores none, which a reader closes at the row's updated_at, so writing such a
// row again with another UpdatedAt moves nothing a comparison reads. A waiting reason of "" is
// stored as null, the column's "waits on nothing": the rule never answers an empty reason, so the
// two cannot be confused. A repository of "" is stored as null the same way, the column's "none
// resolves".
func upsert(ctx context.Context, q Querier, project, build string, rows []Row) ([]string, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	started := time.Now().UTC()
	keys := computedKeys(rows)
	openedAt := make([]time.Time, len(rows))
	closedAt := make([]*time.Time, len(rows))
	hoursByPhase := make([]string, len(rows))
	workHours := make([]float64, len(rows))
	reviewRounds := make([]int32, len(rows))
	rootKeys := make([]string, len(rows))
	treeDepth := make([]int32, len(rows))
	slotHours := make([]float64, len(rows))
	waitingReason := make([]string, len(rows))
	workFirstAt := make([]*time.Time, len(rows))
	workLastAt := make([]*time.Time, len(rows))
	updatedAt := make([]time.Time, len(rows))
	parkedHours := make([]float64, len(rows))
	queuedHours := make([]float64, len(rows))
	startedAt := make([]*time.Time, len(rows))
	mergedAt := make([]*time.Time, len(rows))
	bars := make([]string, len(rows))
	idleByRole := make([]string, len(rows))
	slotWaitHours := make([]float64, len(rows))
	sentBack := make([]bool, len(rows))
	implementingAfter := make([]float64, len(rows))
	testingAfter := make([]float64, len(rows))
	children := make([]int32, len(rows))
	repos := make([]string, len(rows))
	asOf := make([]*time.Time, len(rows))
	growth := make([]string, len(rows))
	for index, row := range rows {
		if row.Project != project {
			return nil, fmt.Errorf("write the Legion facts: %s and %s are two projects in one write", project, row.Project)
		}
		hours, err := json.Marshal(storedMap(row.PhaseHours))
		if err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", row.Issue, err)
		}
		barParts, err := json.Marshal(storedMap(row.Bars))
		if err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", row.Issue, err)
		}
		idle, err := json.Marshal(storedMap(row.IdleByRole))
		if err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", row.Issue, err)
		}
		rates, err := json.Marshal(storedMap(row.Growth))
		if err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", row.Issue, err)
		}
		openedAt[index] = row.OpenedAt
		closedAt[index] = row.ClosedAt
		hoursByPhase[index] = string(hours)
		workHours[index] = row.WorkHours
		reviewRounds[index] = int32(row.ReviewRounds)
		rootKeys[index] = row.Tree
		treeDepth[index] = int32(row.TreeDepth)
		slotHours[index] = row.SlotHours
		waitingReason[index] = row.WaitingReason
		workFirstAt[index] = row.WorkFirstAt
		workLastAt[index] = row.WorkLastAt
		updatedAt[index] = row.UpdatedAt
		if row.UpdatedAt.IsZero() {
			updatedAt[index] = started
		}
		if !row.AsOf.IsZero() {
			asOf[index] = &row.AsOf
		}
		parkedHours[index] = row.ParkedHours
		queuedHours[index] = row.QueuedHours
		startedAt[index] = row.StartedAt
		mergedAt[index] = row.MergedAt
		bars[index] = string(barParts)
		idleByRole[index] = string(idle)
		slotWaitHours[index] = row.SlotWaitHours
		sentBack[index] = row.SentBack
		implementingAfter[index] = row.ImplementingHoursAfterFirstReview
		testingAfter[index] = row.TestingHoursAfterFirstReview
		children[index] = int32(row.Children)
		repos[index] = row.Repo
		growth[index] = string(rates)
	}
	result, err := q.Query(ctx, `
		insert into issue_facts (
			project_key, issue_key, opened_at, closed_at, hours_by_phase, work_hours,
			review_rounds, root_key, tree_depth, slot_hours, waiting_reason,
			work_first_at, work_last_at, updated_at,
			parked_hours, queued_hours, started_at, merged_at, bars, idle_by_role,
			slot_wait_hours, sent_back, implementing_after_first_review,
			testing_after_first_review, children, repo, as_of, growth, written_build)
		select $1, given.issue_key, given.opened_at, given.closed_at, given.hours_by_phase::jsonb,
		       given.work_hours, given.review_rounds, given.root_key, given.tree_depth,
		       given.slot_hours, nullif(given.waiting_reason, ''), given.work_first_at,
		       given.work_last_at, given.updated_at,
		       given.parked_hours, given.queued_hours, given.started_at, given.merged_at,
		       given.bars::jsonb, given.idle_by_role::jsonb, given.slot_wait_hours,
		       given.sent_back, given.implementing_after_first_review,
		       given.testing_after_first_review, given.children, nullif(given.repo, ''),
		       given.as_of, given.growth::jsonb, $29::text
		from unnest($2::text[], $3::timestamptz[], $4::timestamptz[], $5::text[],
		            $6::double precision[], $7::integer[], $8::text[], $9::integer[],
		            $10::double precision[], $11::text[], $12::timestamptz[], $13::timestamptz[],
		            $14::timestamptz[],
		            $15::double precision[], $16::double precision[], $17::timestamptz[],
		            $18::timestamptz[], $19::text[], $20::text[], $21::double precision[],
		            $22::boolean[], $23::double precision[], $24::double precision[],
		            $25::integer[], $26::text[], $27::timestamptz[], $28::text[])
			as given(issue_key, opened_at, closed_at, hours_by_phase, work_hours, review_rounds,
			         root_key, tree_depth, slot_hours, waiting_reason, work_first_at, work_last_at,
			         updated_at,
			         parked_hours, queued_hours, started_at, merged_at, bars, idle_by_role,
			         slot_wait_hours, sent_back, implementing_after_first_review,
			         testing_after_first_review, children, repo, as_of, growth)
		on conflict (project_key, issue_key) do update set
			opened_at = excluded.opened_at,
			closed_at = excluded.closed_at,
			hours_by_phase = excluded.hours_by_phase,
			work_hours = excluded.work_hours,
			review_rounds = excluded.review_rounds,
			root_key = excluded.root_key,
			tree_depth = excluded.tree_depth,
			slot_hours = excluded.slot_hours,
			waiting_reason = excluded.waiting_reason,
			work_first_at = excluded.work_first_at,
			work_last_at = excluded.work_last_at,
			parked_hours = excluded.parked_hours,
			queued_hours = excluded.queued_hours,
			started_at = excluded.started_at,
			merged_at = excluded.merged_at,
			bars = excluded.bars,
			idle_by_role = excluded.idle_by_role,
			slot_wait_hours = excluded.slot_wait_hours,
			sent_back = excluded.sent_back,
			implementing_after_first_review = excluded.implementing_after_first_review,
			testing_after_first_review = excluded.testing_after_first_review,
			children = excluded.children,
			repo = excluded.repo,
			as_of = excluded.as_of,
			growth = excluded.growth,
			written_build = excluded.written_build,
			updated_at = greatest(excluded.updated_at, issue_facts.updated_at + interval '1 microsecond')
		where (issue_facts.opened_at, issue_facts.closed_at, issue_facts.hours_by_phase,
		       issue_facts.work_hours, issue_facts.review_rounds, issue_facts.root_key,
		       issue_facts.tree_depth, issue_facts.slot_hours, issue_facts.waiting_reason,
		       issue_facts.work_first_at, issue_facts.work_last_at,
		       issue_facts.parked_hours, issue_facts.queued_hours, issue_facts.started_at,
		       issue_facts.merged_at, issue_facts.bars, issue_facts.idle_by_role,
		       issue_facts.slot_wait_hours, issue_facts.sent_back,
		       issue_facts.implementing_after_first_review, issue_facts.testing_after_first_review,
		       issue_facts.children, issue_facts.repo, issue_facts.as_of, issue_facts.growth,
		       issue_facts.written_build)
		  is distinct from
		      (excluded.opened_at, excluded.closed_at, excluded.hours_by_phase,
		       excluded.work_hours, excluded.review_rounds, excluded.root_key,
		       excluded.tree_depth, excluded.slot_hours, excluded.waiting_reason,
		       excluded.work_first_at, excluded.work_last_at,
		       excluded.parked_hours, excluded.queued_hours, excluded.started_at,
		       excluded.merged_at, excluded.bars, excluded.idle_by_role,
		       excluded.slot_wait_hours, excluded.sent_back,
		       excluded.implementing_after_first_review, excluded.testing_after_first_review,
		       excluded.children, excluded.repo, excluded.as_of, excluded.growth,
		       excluded.written_build)
		returning issue_key
	`, project, keys, openedAt, closedAt, hoursByPhase, workHours, reviewRounds, rootKeys,
		treeDepth, slotHours, waitingReason, workFirstAt, workLastAt, updatedAt,
		parkedHours, queuedHours, startedAt, mergedAt, bars, idleByRole, slotWaitHours, sentBack,
		implementingAfter, testingAfter, children, repos, asOf, growth, build)
	if err != nil {
		return nil, fmt.Errorf("write the Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	changed := []string{}
	for result.Next() {
		var key string
		if err := result.Scan(&key); err != nil {
			return nil, fmt.Errorf("write the Legion facts of %s: %w", project, err)
		}
		changed = append(changed, key)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("write the Legion facts of %s: %w", project, err)
	}
	return changed, nil
}

// The rule both deletions below keep, and the one that matters more than the deletion itself: a
// pass deletes only inside the set of keys it computed over, and never the row of an issue it
// did not compute. A pass states that set before it writes - the keys its feeds named, the
// issues above and below them and the trees a moved parent link touched (rowsToCompute), or
// every issue of the project where it read the project whole - and a key of that set its
// computation answered no row for is a row to delete. A stored row outside the stated set is a
// row this pass knows nothing of: the pass that states it computes it, and until then it
// stands.

// Each deletion below finds its stale rows as the stored keys except the computed ones (`gone`),
// materialized, so the set operation runs once whatever plan the statement is given and reads
// each side once, and deletes or stamps each by its primary key (staleFactsStatement,
// staleFilingsStatement). Written as `not (issue_key = any($n))`, the filter compared each stored
// row with the whole computed list: a plan with the values hashes the list, and the generic plan a
// statement pgx caches can settle on after its fifth run cannot, so a whole pass of a project of
// INFRA's 13,000 issues compared each of its rows with up to 13,000 keys.
// TestEachDeletionOfStaleFactsReadsItsListOnce holds each to its plan. Each statement spells its
// table in its own text: the census of item-row writers reads a write's table there, and refuses
// one whose table a value fills (api's TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus).

// deleteStaleRows deletes the stored rows of the stated keys that computed holds no row for, and
// answers the keys it deleted. It is the deletion of a pass that computed a set of keys: the
// write's Stated is the set the pass considered, computed is the keys its computation answered
// rows for, and no row outside Stated is read at all. A Yielded write stamps those rows with its
// build instead, and answers the keys it stamped (ProjectWrite.Yielded).
func deleteStaleRows(ctx context.Context, q Querier, write ProjectWrite, computed []string) ([]string, error) {
	if len(write.Stated) == 0 {
		return nil, nil
	}
	args := []any{write.Project, write.Stated, computed}
	if write.Yielded {
		args = append(args, write.Build)
	}
	return deleteRows(ctx, q, write.Project, staleRowsStatement(write.Yielded), args...)
}

// staleRowsStatement is deleteStaleRows' statement, the stamp where yielded: the rows of project
// $1 among the stated keys $2 that the computed keys $3 hold no row for, stamped with the build $4.
func staleRowsStatement(yielded bool) string {
	return staleFactsStatement(`with gone as materialized (
		select issue_key from issue_facts where project_key = $1 and issue_key = any($2)
		except
		select * from unnest($3::text[]) as computed(issue_key)
	)`, yielded, "$4") + ` returning f.issue_key`
}

// deleteStaleProjectRows deletes the stored rows of the project that computed holds no row for,
// and answers the keys it deleted. It is the deletion of a pass that computed the project whole
// (ComputeProject), whose stated set is every issue the project has, because that is the
// population such a computation reads (metrics.ReadProject). A stored row whose key names no
// issue of the project was therefore computed by nothing, and this deletion leaves it where it
// stands. A Yielded write stamps those rows with its build instead, and answers the keys it
// stamped (ProjectWrite.Yielded).
func deleteStaleProjectRows(ctx context.Context, q Querier, write ProjectWrite, computed []string) ([]string, error) {
	args := []any{write.Project, computed}
	if write.Yielded {
		args = append(args, write.Build)
	}
	return deleteRows(ctx, q, write.Project, staleProjectRowsStatement(write.Yielded), args...)
}

// staleProjectRowsStatement is deleteStaleProjectRows' statement, the stamp where yielded: the
// rows of project $1 whose key names an issue of the project and that the computed keys $2 hold
// no row for, stamped with the build $3.
func staleProjectRowsStatement(yielded bool) string {
	return staleFactsStatement(`with gone as materialized (
		select f.issue_key from issue_facts f
		where f.project_key = $1
		  and exists (select 1 from issues where issues.project_key = $1 and issues.key = f.issue_key)
		except
		select * from unnest($2::text[]) as computed(issue_key)
	)`, yielded, "$3") + ` returning f.issue_key`
}

// staleFactsStatement is the statement, headed by gone, the CTE of the stale keys, that deletes
// each issue_facts row of project $1 that gone names, or where yielded stamps each that holds
// another build with the build the parameter build names.
func staleFactsStatement(gone string, yielded bool, build string) string {
	if yielded {
		return gone + `
	update issue_facts f set written_build = ` + build + ` from gone
	where f.project_key = $1 and f.issue_key = gone.issue_key and f.written_build <> ` + build
	}
	return gone + `
	delete from issue_facts f using gone
	where f.project_key = $1 and f.issue_key = gone.issue_key`
}

// deleteRows runs one of the two deletions, or the stamp a Yielded write makes in its place, and
// answers the keys it deleted or stamped, in the order Postgres took them. Each row it deletes or
// stamps logs a delivery change of kind issue_facts carrying that row's close day, which the
// table's own trigger writes on a delete as it does on a write (migration 20261117100002), so
// every reader of the facts is told the row went, and the build a stamp hands the row to is woken
// to compute it again.
func deleteRows(ctx context.Context, q Querier, project, statement string, args ...any) ([]string, error) {
	result, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("delete or stamp the stale Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	deleted := []string{}
	for result.Next() {
		var key string
		if err := result.Scan(&key); err != nil {
			return nil, fmt.Errorf("delete or stamp the stale Legion facts of %s: %w", project, err)
		}
		deleted = append(deleted, key)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("delete or stamp the stale Legion facts of %s: %w", project, err)
	}
	return deleted, nil
}

// storedMap is a map as its jsonb column stores it: a nil map is written as the empty object,
// which is what an issue whose work took no measured time holds in hours_by_phase, and never as
// the JSON null.
func storedMap[V any](values map[string]V) map[string]V {
	if values == nil {
		return map[string]V{}
	}
	return values
}

// Read answers the stored rows the Legion board serves, by issue key, each as of at (Row.At):
// every open row of the project, which no other route serves, since the items route serves an
// issue by its close day (dispatch://INFRA-2916, R22); of every issue whose tree root is one of
// roots, the trees the cards stand in; and the rows of keys, the issue of each card. No other row
// is read: a closed issue of no shown tree is the items route's (ReadClosedIn), so the read and the
// board's body follow the open work and what the daemon runs, not every issue the project's Legion
// work ever covered. The read is three index reads that share no row, keyed on closed_at: the open
// rows, the closed rows of the trees by root, and the closed rows of the cards outside those trees
// by key (shownWhere), never the project's rows filtered after they are read.
//
// The statement runs in a read-only transaction of its own with sequential scans off
// (readByIndex), because the planner left to itself reads the open rows by scanning the whole
// table once they are a large share of it. At INFRA's size (13,105 rows, 2,621 of them open, on
// 2,880 pages, as evalfixture loads it), the plan with the values of a board that names no card
// was a Seq Scan of issue_facts that read all 2,880 pages and dropped the 10,484 closed rows by its
// filter, priced at 3,043.81 against 3,043.72 for the open rows' index range, a tie the planner may
// break either way. With 4,272 of 13,103 rows open, on the build farm, it was a Seq Scan too, and a
// partial index of the open rows did not move that plan (dispatch://INFRA-2916, k23, 2026-11-17).
// With the setting, every arm reads through its index under the custom and the generic plan
// alike, whatever the estimate: the open arm read the 2,621 open rows from 1,663 heap pages in 4.5
// to 5.9 ms, where the scan took 6.3 to 10.0 ms.
//
// It is the board's read of the facts: rows, and no computation but the close of each open part
// at the read's own clock, and of each row the columns the board shows (boardColumns), every other
// field of the Row it answers left at its zero value (narrow). The evals read every column of the
// rows of a window's closed issues instead (ReadClosedIn).
func Read(ctx context.Context, pool *store.Pool, project string, keys, roots []string, at time.Time) (map[string]Row, error) {
	// A nil list goes to Postgres as null, which `= any` answers null for, and `not (null)` would
	// leave out every closed row of a card; an empty array answers false.
	if keys == nil {
		keys = []string{}
	}
	if roots == nil {
		roots = []string{}
	}
	var read []Row
	if err := readByIndex(ctx, pool, func(tx pgx.Tx) (err error) {
		read, err = readRows(ctx, tx, project, false, shownWhere, project, keys, roots)
		return err
	}); err != nil {
		return nil, err
	}
	rows := make(map[string]Row, len(read))
	for _, row := range read {
		rows[row.Issue] = row.At(at).narrow()
	}
	return rows, nil
}

// readByIndex runs read in a read-only transaction of its own on pool whose first statement turns
// the planner's sequential scans off and has it plan each statement with its values for that
// transaction alone (`set local`), so nothing of it is left on the pooled connection once the
// transaction ends. It is Read's transaction, and the one its plan test takes the board's plans
// in, so the plans the test judges are the ones Read runs. It takes one connection and holds no
// lock another read waits on, so it is one of the reads the board runs side by side
// (store.ReadSideBySide), as the board's snapshot read is.
//
// The plan with the values (`plan_cache_mode = force_custom_plan`) is the one a statement pgx
// caches would otherwise leave after its fifth run for a generic plan, where `= any` over a
// parameter scans the array for each row it tests rather than hashing it. shownWhere tests no
// array on the open rows, so it costs a generic plan little, and the setting keeps the arms that
// do test one (each bounded by the board's cards) on the values as well.
func readByIndex(ctx context.Context, pool *store.Pool, read func(tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin the board's read of the Legion facts: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off; set local plan_cache_mode = force_custom_plan`); err != nil {
		return fmt.Errorf("turn sequential scans and generic plans off for the board's read of the Legion facts: %w", err)
	}
	if err := read(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the board's read of the Legion facts: %w", err)
	}
	return nil
}

// shownWhere is Read's clause, which completes boardSelect, as three reads of the project's rows
// that share no row, keyed on closed_at: the open rows, the range of issue_facts_closed
// (project_key, closed_at desc) where closed_at is null, which a btree answers as an index
// condition, so the range holds the project's open rows alone and tests nothing of them; the
// closed rows of the trees given, each root a range of issue_facts_root (project_key, root_key);
// and the closed rows of the keys given whose root is none of those trees, each a probe of the
// primary key (project_key, issue_key). Both indexes are migration 20261117100000's. The three
// answer the open rows, the trees' rows and the cards' rows, each once, and only the last tests a
// list on the rows it reads, the roots on each closed card's, which the cards bound. An earlier
// shape left the rows of the cards and the trees out of the open rows, which tested both lists on
// every open row: under the generic plan a statement pgx caches can settle on, `<> all($n)` scans
// the list for each row rather than hashing it, and over 15,803 rows of one project, 4,300 of them
// open, with 200 cards in 60 trees, that read took 119 to 168 ms against 13 to 14 ms with its
// values, and 417 to 482 ms with 1,000 cards in 300 trees; this one takes 7 to 9 ms and 11 to 16
// ms under either plan (dispatch://INFRA-2916, round 15).
// TestTheBoardsFactsReadUnderAGenericPlanStaysWithinItsBudget holds it to 20 ms.
//
// The planner keeps the open arm on the index only because Read runs the statement with
// sequential scans off (readByIndex): priced by itself, a scan of the whole table ties with the
// range once the open rows are a large share of the table, and at INFRA's size the plan with the
// values of a board of no card was that scan.
const shownWhere = `where project_key = $1 and closed_at is null
	union all ` + boardSelect + `where project_key = $1 and root_key = any($3) and closed_at is not null
	union all ` + boardSelect + `where project_key = $1 and issue_key = any($2) and closed_at is not null
		and not (root_key = any($3))`

// ReadClosedIn answers the stored rows of the project's issues that closed from from up to to,
// newest close first, through issue_facts_closed (migration 20261117100000), each as of at
// (Row.At): the issues an eval of the Legion measures counts in that window, every one with
// Legion phase work since every stored row is one, each with every column. The Legion evals' day
// rows are built from one day's, as of that day's end (evals.ReadLegionDay), and the eval items
// route reads a range's as of the request.
func ReadClosedIn(ctx context.Context, q Querier, project string, from, to, at time.Time) ([]Row, error) {
	rows, err := readRows(ctx, q, project, true, closedInWhere, project, from, to)
	if err != nil {
		return nil, err
	}
	for index := range rows {
		rows[index] = rows[index].At(at)
	}
	return rows, nil
}

// closedInWhere is ReadClosedIn's clause: a range of issue_facts_closed, (project_key, closed_at
// desc), read in its order.
const closedInWhere = `
	where project_key = $1 and closed_at >= $2 and closed_at < $3
	order by closed_at desc, issue_key`

// boardColumns is the columns of issue_facts the board shows (Read), with the moment its values
// are closed at and their growth, which every read of the rows reads first, in scanRow's order:
// every reader closes a row at its own clock (Row.At). A row no build of this release wrote holds
// no as_of, and is closed at the moment it was written. wideColumns is every other column scanRow
// reads, which the evals read and the board shows none of: two of them are JSON (bars and
// idle_by_role, beside hours_by_phase and growth) that a read decodes for every row. boardSelect
// and wideSelect are the reads of the two, which a where clause completes.
const (
	boardColumns = `issue_key, opened_at, closed_at, hours_by_phase, work_hours, review_rounds,
	       root_key, tree_depth, slot_hours, coalesce(waiting_reason, ''),
	       work_first_at, work_last_at, updated_at, coalesce(as_of, updated_at), growth`
	wideColumns = `parked_hours, queued_hours, started_at, merged_at, bars, idle_by_role,
	       slot_wait_hours, sent_back, implementing_after_first_review,
	       testing_after_first_review, children, coalesce(repo, '')`
	boardSelect = `select ` + boardColumns + ` from issue_facts `
	wideSelect  = `select ` + boardColumns + `, ` + wideColumns + ` from issue_facts `
)

// readRows is the stored rows of project that where (a where clause, with its order) selects,
// whose arguments args are: the board's columns of each, and every column where wide.
func readRows(ctx context.Context, q Querier, project string, wide bool, where string, args ...any) ([]Row, error) {
	selected := boardSelect
	if wide {
		selected = wideSelect
	}
	result, err := q.Query(ctx, selected+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	var rows []Row
	for result.Next() {
		row, err := scanRow(result, project, wide)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	return rows, nil
}

// scanRow reads one row of readRows' columns: the board's, and the rest where wide. Root is no
// column: it is whether the issue is its own tree's root (root_key), which is how the computation
// answers it (metrics.IssueFacts.Root).
func scanRow(result pgx.Rows, project string, wide bool) (Row, error) {
	row := Row{Project: project}
	// opened_at is a nullable column, which a row this package wrote always fills.
	var opened *time.Time
	var hours, growth, bars, idle []byte
	into := []any{&row.Issue, &opened, &row.ClosedAt, &hours, &row.WorkHours,
		&row.ReviewRounds, &row.Tree, &row.TreeDepth, &row.SlotHours, &row.WaitingReason,
		&row.WorkFirstAt, &row.WorkLastAt, &row.UpdatedAt, &row.AsOf, &growth}
	if wide {
		into = append(into, &row.ParkedHours, &row.QueuedHours, &row.StartedAt, &row.MergedAt, &bars, &idle,
			&row.SlotWaitHours, &row.SentBack, &row.ImplementingHoursAfterFirstReview,
			&row.TestingHoursAfterFirstReview, &row.Children, &row.Repo)
	}
	if err := result.Scan(into...); err != nil {
		return Row{}, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	if opened != nil {
		row.OpenedAt = *opened
	}
	row.Root = row.Tree == row.Issue
	decoded := []struct {
		stored []byte
		into   any
	}{{hours, &row.PhaseHours}, {growth, &row.Growth}, {bars, &row.Bars}, {idle, &row.IdleByRole}}
	if !wide {
		decoded = decoded[:2]
	}
	for _, column := range decoded {
		if err := json.Unmarshal(column.stored, column.into); err != nil {
			return Row{}, fmt.Errorf("read the Legion facts of %s: %w", row.Issue, err)
		}
	}
	return row, nil
}

// narrow is row with every field of wideColumns at its zero value, which is the row the board's
// read answers (Read) once At has closed it at the read's clock. At grows every fact the row's
// growth names, and the board's read loaded none of these, so one of them that grows - the
// parked, queued or slot-wait hours of an open issue, its hours after the first review - would
// otherwise hold its rate times the hours elapsed, grown from a zero the read never loaded.
func (row Row) narrow() Row {
	row.ParkedHours, row.QueuedHours, row.StartedAt, row.MergedAt = 0, 0, nil, nil
	row.Bars, row.IdleByRole, row.SlotWaitHours, row.SentBack = nil, nil, 0, false
	row.ImplementingHoursAfterFirstReview, row.TestingHoursAfterFirstReview = 0, 0
	row.Children, row.Repo = 0, ""
	return row
}

// upsertFilings writes the filings of the project under build, in one statement as upsert writes
// the rows. A filing whose stored values already match is left alone, and a filing a session of
// another project made of the same issue before is moved to this one, since issue_key is the
// table's identity and the newest title of the filing session is what says whose filing it is.
// The build is compared with the rest, as upsert compares a row's: a filing another build wrote
// is rewritten by the build that computes it, so its stamp says which rule it was last computed
// by (otherBuildsFilings).
func upsertFilings(ctx context.Context, q Querier, project, build string, filings []Filing) error {
	if len(filings) == 0 {
		return nil
	}
	keys := make([]string, len(filings))
	filedAt := make([]time.Time, len(filings))
	roles := make([]string, len(filings))
	filers := make([]string, len(filings))
	phases := make([]string, len(filings))
	classes := make([]string, len(filings))
	roots := make([]string, len(filings))
	for index, filing := range filings {
		if filing.Project != project {
			return fmt.Errorf("write the Legion filings: %s and %s are two projects in one write", project, filing.Project)
		}
		keys[index] = filing.Key
		filedAt[index] = filing.At
		roles[index] = filing.Filer
		filers[index] = filing.FilerIssue
		phases[index] = filing.FilerPhase
		classes[index] = filing.Class
		roots[index] = filing.Root
	}
	if err := exec(ctx, q, `
		insert into issue_filings (issue_key, project_key, filed_at, filer_role, filer_issue,
			filer_phase, class, root_key, written_build)
		select given.issue_key, $1, given.filed_at, given.filer_role, given.filer_issue,
		       given.filer_phase, given.class, given.root_key, $9
		from unnest($2::text[], $3::timestamptz[], $4::text[], $5::text[], $6::text[],
		            $7::text[], $8::text[])
			as given(issue_key, filed_at, filer_role, filer_issue, filer_phase, class, root_key)
		on conflict (issue_key) do update set
			project_key = excluded.project_key,
			filed_at = excluded.filed_at,
			filer_role = excluded.filer_role,
			filer_issue = excluded.filer_issue,
			filer_phase = excluded.filer_phase,
			class = excluded.class,
			root_key = excluded.root_key,
			written_build = excluded.written_build
		where (issue_filings.project_key, issue_filings.filed_at, issue_filings.filer_role,
		       issue_filings.filer_issue, issue_filings.filer_phase, issue_filings.class,
		       issue_filings.root_key, issue_filings.written_build)
		  is distinct from
		      (excluded.project_key, excluded.filed_at, excluded.filer_role,
		       excluded.filer_issue, excluded.filer_phase, excluded.class, excluded.root_key,
		       excluded.written_build)
	`, project, keys, filedAt, roles, filers, phases, classes, roots, build); err != nil {
		return fmt.Errorf("write the Legion filings of %s: %w", project, err)
	}
	return nil
}

// exec runs a statement that answers no row. A Querier offers Query alone, and the rows it opens
// hold the connection, or the transaction a pass writes in, until they close, so exec closes them
// before it returns and the next statement finds the connection free.
func exec(ctx context.Context, q Querier, statement string, args ...any) error {
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// deleteStaleFilings deletes the project's stored filings inside the set the pass stated that
// its computation answered none for, by the rule the two deletions of the rows keep: a pass
// deletes only inside what it computed over. A pass that read every filing of the project
// stated all of them; any other stated the filed issues it was given (filingsToCompute). A
// filing another project's session made is that project's row, which this never reads. A
// Yielded write stamps those filings with its build instead (FilingsWrite.Yielded).
func deleteStaleFilings(ctx context.Context, q Querier, write FilingsWrite) error {
	if !write.Whole && len(write.Stated) == 0 {
		return nil
	}
	kept := make([]string, len(write.Filings))
	for index, filing := range write.Filings {
		kept[index] = filing.Key
	}
	args := []any{write.Project, kept}
	if !write.Whole {
		args = append(args, write.Stated)
	}
	if write.Yielded {
		args = append(args, write.Build)
	}
	if err := exec(ctx, q, staleFilingsStatement(!write.Whole, write.Yielded), args...); err != nil {
		return fmt.Errorf("delete or stamp the stale Legion filings of %s: %w", write.Project, err)
	}
	return nil
}

// staleFilingsStatement is deleteStaleFilings' statement, the stamp where yielded: the filings of
// project $1 that the computed filings $2 do not hold, of the stated filed issues $3 where stated
// and of every filed issue otherwise, stamped with the build that follows them, each deleted or
// stamped by its primary key as staleFactsStatement writes an issue_facts row.
func staleFilingsStatement(stated, yielded bool) string {
	filter, build := "", "$3"
	if stated {
		filter, build = " and issue_key = any($3)", "$4"
	}
	gone := `with gone as materialized (
		select issue_key from issue_filings where project_key = $1` + filter + `
		except
		select * from unnest($2::text[]) as kept(issue_key)
	)`
	if yielded {
		return gone + `
	update issue_filings f set written_build = ` + build + ` from gone
	where f.project_key = $1 and f.issue_key = gone.issue_key and f.written_build <> ` + build
	}
	return gone + `
	delete from issue_filings f using gone
	where f.project_key = $1 and f.issue_key = gone.issue_key`
}

// ReadFilingsIn answers the stored filings a Legion session of the project made from from up to
// to, oldest first, through issue_filings_day (migration 20261117100007): the filings the
// splitting eval counts in that window. The Legion evals' day rows are built from one day's
// (evals.ReadLegionDay).
func ReadFilingsIn(ctx context.Context, q Querier, project string, from, to time.Time) ([]Filing, error) {
	return readFilings(ctx, q, project, `
		where project_key = $1 and filed_at >= $2 and filed_at < $3
		order by filed_at, issue_key`, project, from, to)
}

// readFilings is the stored filings of project that where (a where clause, with its order)
// selects, whose arguments args are.
func readFilings(ctx context.Context, q Querier, project, where string, args ...any) ([]Filing, error) {
	result, err := q.Query(ctx, `
		select issue_key, filed_at, filer_role, filer_issue, filer_phase, class, root_key
		from issue_filings `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read the Legion filings of %s: %w", project, err)
	}
	defer result.Close()
	var filings []Filing
	for result.Next() {
		filing := Filing{Project: project}
		if err := result.Scan(&filing.Key, &filing.At, &filing.Filer, &filing.FilerIssue,
			&filing.FilerPhase, &filing.Class, &filing.Root); err != nil {
			return nil, fmt.Errorf("read the Legion filings of %s: %w", project, err)
		}
		filings = append(filings, filing)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion filings of %s: %w", project, err)
	}
	return filings, nil
}
