package legionfacts

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectWrite is one project's computed facts and the set the pass that computed them stated:
// what WriteProject writes in one call. The project stands beside the rows rather than being
// read out of them, because a project whose rows are all stale answers no row at all and still
// owes its deletion.
//
// Stated is the set of keys the pass computed over, inside which it may delete a stale row, and
// Whole says the pass read the project whole (ComputeProject), whose stated set is every issue
// the project has and whose Stated is therefore empty.
type ProjectWrite struct {
	Project string
	Rows    []Row
	Stated  []string
	Whole   bool
}

// WriteProject writes one project's computed facts: it upserts the rows the computation answered
// (upsert) and deletes the stored rows inside the stated set the computation answered none for
// (deleteStaleRows, or deleteStaleProjectRows for a pass that read the project whole). It answers
// every key whose stored row moved, the keys it deleted after the keys it wrote, because a row
// that went is a change of the stored facts as much as a row that moved.
//
// A stored row means Legion worked the issue (dispatch://INFRA-2915), and an issue loses its
// phase work as well as gains it: a session's newest title decides which issue that session's
// events work (legion.PhaseWork), so a role session that writes next on another issue under
// that issue's title takes the work with it, and the issue it left carries none. The deletion
// is what takes that issue's row away; an upsert alone leaves it standing for ever.
//
// It is the one write of the facts: the consumer's pass makes it for each project of the pass,
// inside the transaction that moves the cursors (consumer.go's writePass), and a caller that
// holds one project's rows makes it alone. It writes no filing (WriteFilings), so a caller that
// holds rows alone cannot take a filing away.
func WriteProject(ctx context.Context, q Querier, write ProjectWrite) ([]string, error) {
	written, err := upsert(ctx, q, write.Project, write.Rows)
	if err != nil {
		return nil, err
	}
	kept := computedKeys(write.Rows)
	deleted := []string{}
	if write.Whole {
		deleted, err = deleteStaleProjectRows(ctx, q, write.Project, kept)
	} else {
		deleted, err = deleteStaleRows(ctx, q, write.Project, write.Stated, kept)
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
// of the project, computeWhole), whose Stated is therefore empty.
type FilingsWrite struct {
	Project string
	Filings []Filing
	Stated  []string
	Whole   bool
}

// WriteFilings writes one project's computed filings, as WriteProject writes its rows: it upserts
// the filings the computation answered (upsertFilings) and deletes the project's stored filings
// inside the stated set the computation answered none for (deleteStaleFilings). A stored filing
// goes when the session that filed it is no Legion session of the project any more, as its
// newest title reads, and its row moves when its class, its filer's phase or the filed issue's
// tree does. The consumer's pass makes it in the transaction that writes the same pass's rows.
func WriteFilings(ctx context.Context, q Querier, write FilingsWrite) error {
	if err := upsertFilings(ctx, q, write.Project, write.Filings); err != nil {
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
// (WriteProject). A row whose stored values already match is left alone: a sweep that finds
// nothing new rewrites nothing, and a check can count what moved.
//
// The project is the caller's and not the first row's: a write of no row still belongs to a
// project, and the loop below refuses a row of any other project, so the rows and the project
// this writes into cannot disagree.
//
// One statement, so a sweep of hundreds of issues is one round trip. The rows go in as arrays
// unnested into a row each, as the sampler's own write does (legion/store.go's
// RecordObservations), and the conflict's `do update` carries the whole row's comparison in its
// where clause, so Postgres decides what changed and `returning` answers exactly the rows it
// touched. updated_at moves only with a row that changed, because an unchanged row is never
// updated at all. The comparison holds every column but updated_at, the ones migration
// 20261113111231 added among them, so a row an earlier build wrote without them is rewritten by
// the first pass that computes it. The hours, the bars and the idle hours are compared as jsonb,
// so a Go encoding that ordered an object's keys differently from jsonb cannot read as a change.
//
// updated_at is the row's own UpdatedAt, the moment its facts were computed as of; a row that
// carries none takes the moment this write started. A waiting reason of "" is stored as null, the
// column's "waits on nothing": the rule never answers an empty reason, so the two cannot be
// confused. A repository of "" is stored as null the same way, the column's "none resolves".
func upsert(ctx context.Context, q Querier, project string, rows []Row) ([]string, error) {
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
	for index, row := range rows {
		if row.Project != project {
			return nil, fmt.Errorf("write the Legion facts: %s and %s are two projects in one write", project, row.Project)
		}
		hours, err := json.Marshal(storedMap(row.HoursByPhase))
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
		openedAt[index] = row.OpenedAt
		closedAt[index] = row.ClosedAt
		hoursByPhase[index] = string(hours)
		workHours[index] = row.WorkHours
		reviewRounds[index] = int32(row.ReviewRounds)
		rootKeys[index] = row.RootKey
		treeDepth[index] = int32(row.TreeDepth)
		slotHours[index] = row.SlotHours
		waitingReason[index] = row.WaitingReason
		workFirstAt[index] = row.WorkFirstAt
		workLastAt[index] = row.WorkLastAt
		updatedAt[index] = row.UpdatedAt
		if row.UpdatedAt.IsZero() {
			updatedAt[index] = started
		}
		parkedHours[index] = row.ParkedHours
		queuedHours[index] = row.QueuedHours
		startedAt[index] = row.StartedAt
		mergedAt[index] = row.MergedAt
		bars[index] = string(barParts)
		idleByRole[index] = string(idle)
		slotWaitHours[index] = row.SlotWaitHours
		sentBack[index] = row.SentBack
		implementingAfter[index] = row.ImplementingAfterFirstReview
		testingAfter[index] = row.TestingAfterFirstReview
		children[index] = int32(row.Children)
		repos[index] = row.Repo
	}
	result, err := q.Query(ctx, `
		insert into issue_facts (
			project_key, issue_key, opened_at, closed_at, hours_by_phase, work_hours,
			review_rounds, root_key, tree_depth, slot_hours, waiting_reason,
			work_first_at, work_last_at, updated_at,
			parked_hours, queued_hours, started_at, merged_at, bars, idle_by_role,
			slot_wait_hours, sent_back, implementing_after_first_review,
			testing_after_first_review, children, repo)
		select $1, given.issue_key, given.opened_at, given.closed_at, given.hours_by_phase::jsonb,
		       given.work_hours, given.review_rounds, given.root_key, given.tree_depth,
		       given.slot_hours, nullif(given.waiting_reason, ''), given.work_first_at,
		       given.work_last_at, given.updated_at,
		       given.parked_hours, given.queued_hours, given.started_at, given.merged_at,
		       given.bars::jsonb, given.idle_by_role::jsonb, given.slot_wait_hours,
		       given.sent_back, given.implementing_after_first_review,
		       given.testing_after_first_review, given.children, nullif(given.repo, '')
		from unnest($2::text[], $3::timestamptz[], $4::timestamptz[], $5::text[],
		            $6::double precision[], $7::integer[], $8::text[], $9::integer[],
		            $10::double precision[], $11::text[], $12::timestamptz[], $13::timestamptz[],
		            $14::timestamptz[],
		            $15::double precision[], $16::double precision[], $17::timestamptz[],
		            $18::timestamptz[], $19::text[], $20::text[], $21::double precision[],
		            $22::boolean[], $23::double precision[], $24::double precision[],
		            $25::integer[], $26::text[])
			as given(issue_key, opened_at, closed_at, hours_by_phase, work_hours, review_rounds,
			         root_key, tree_depth, slot_hours, waiting_reason, work_first_at, work_last_at,
			         updated_at,
			         parked_hours, queued_hours, started_at, merged_at, bars, idle_by_role,
			         slot_wait_hours, sent_back, implementing_after_first_review,
			         testing_after_first_review, children, repo)
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
			updated_at = excluded.updated_at
		where (issue_facts.opened_at, issue_facts.closed_at, issue_facts.hours_by_phase,
		       issue_facts.work_hours, issue_facts.review_rounds, issue_facts.root_key,
		       issue_facts.tree_depth, issue_facts.slot_hours, issue_facts.waiting_reason,
		       issue_facts.work_first_at, issue_facts.work_last_at,
		       issue_facts.parked_hours, issue_facts.queued_hours, issue_facts.started_at,
		       issue_facts.merged_at, issue_facts.bars, issue_facts.idle_by_role,
		       issue_facts.slot_wait_hours, issue_facts.sent_back,
		       issue_facts.implementing_after_first_review, issue_facts.testing_after_first_review,
		       issue_facts.children, issue_facts.repo)
		  is distinct from
		      (excluded.opened_at, excluded.closed_at, excluded.hours_by_phase,
		       excluded.work_hours, excluded.review_rounds, excluded.root_key,
		       excluded.tree_depth, excluded.slot_hours, excluded.waiting_reason,
		       excluded.work_first_at, excluded.work_last_at,
		       excluded.parked_hours, excluded.queued_hours, excluded.started_at,
		       excluded.merged_at, excluded.bars, excluded.idle_by_role,
		       excluded.slot_wait_hours, excluded.sent_back,
		       excluded.implementing_after_first_review, excluded.testing_after_first_review,
		       excluded.children, excluded.repo)
		returning issue_key
	`, project, keys, openedAt, closedAt, hoursByPhase, workHours, reviewRounds, rootKeys,
		treeDepth, slotHours, waitingReason, workFirstAt, workLastAt, updatedAt,
		parkedHours, queuedHours, startedAt, mergedAt, bars, idleByRole, slotWaitHours, sentBack,
		implementingAfter, testingAfter, children, repos)
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

// deleteStaleRows deletes the stored rows of the stated keys that computed holds no row for, and
// answers the keys it deleted. It is the deletion of a pass that computed a set of keys: stated
// is the set the pass considered, computed is the keys its computation answered rows for, and no
// row outside stated is read at all.
func deleteStaleRows(ctx context.Context, q Querier, project string, stated, computed []string) ([]string, error) {
	if len(stated) == 0 {
		return nil, nil
	}
	return deleteRows(ctx, q, project, `
		delete from issue_facts
		where project_key = $1 and issue_key = any($2) and not (issue_key = any($3))
		returning issue_key
	`, project, stated, computed)
}

// deleteStaleProjectRows deletes the stored rows of the project that computed holds no row for,
// and answers the keys it deleted. It is the deletion of a pass that computed the project whole
// (ComputeProject), whose stated set is every issue the project has, because that is the
// population such a computation reads (metrics.ReadProject). A stored row whose key names no
// issue of the project was therefore computed by nothing, and this deletion leaves it where it
// stands.
func deleteStaleProjectRows(ctx context.Context, q Querier, project string, computed []string) ([]string, error) {
	return deleteRows(ctx, q, project, `
		delete from issue_facts
		where project_key = $1 and not (issue_key = any($2))
		  and exists (select 1 from issues
		              where issues.project_key = $1 and issues.key = issue_facts.issue_key)
		returning issue_key
	`, project, computed)
}

// deleteRows runs one of the two deletions and answers the keys it deleted, in the order
// Postgres deleted them. Each row it deletes logs a delivery change of kind issue_facts carrying
// that row's day span, which the table's own trigger writes on a delete as it does on a write
// (migration 20261111093100), so every reader of the facts is told the row went.
func deleteRows(ctx context.Context, q Querier, project, statement string, args ...any) ([]string, error) {
	result, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("delete the stale Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	deleted := []string{}
	for result.Next() {
		var key string
		if err := result.Scan(&key); err != nil {
			return nil, fmt.Errorf("delete the stale Legion facts of %s: %w", project, err)
		}
		deleted = append(deleted, key)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("delete the stale Legion facts of %s: %w", project, err)
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

// Read answers the stored rows of a project, by issue key. It is the board's read of the facts:
// rows, and no computation. The evals read the rows of a window's closed issues instead
// (ReadClosedIn).
func Read(ctx context.Context, q Querier, project string) (map[string]Row, error) {
	read, err := readRows(ctx, q, project, `where project_key = $1`, project)
	if err != nil {
		return nil, err
	}
	rows := make(map[string]Row, len(read))
	for _, row := range read {
		rows[row.Issue] = row
	}
	return rows, nil
}

// ReadClosedIn answers the stored rows of the project's issues that closed from from up to to,
// newest close first, through issue_facts_closed (migration 20261113082157): the issues an eval
// of the Legion measures counts in that window, every one with Legion phase work since every
// stored row is one. The Legion evals' day rows are built from one day's (evals.ReadLegionDay).
func ReadClosedIn(ctx context.Context, q Querier, project string, from, to time.Time) ([]Row, error) {
	return readRows(ctx, q, project, closedInWhere, project, from, to)
}

// closedInWhere is ReadClosedIn's clause: a range of issue_facts_closed, (project_key, closed_at
// desc), read in its order.
const closedInWhere = `
	where project_key = $1 and closed_at >= $2 and closed_at < $3
	order by closed_at desc, issue_key`

// rowsSelect is the read of every column of issue_facts scanRow reads, in its order, which a
// where clause completes.
const rowsSelect = `
	select issue_key, opened_at, closed_at, hours_by_phase, work_hours, review_rounds,
	       root_key, tree_depth, slot_hours, coalesce(waiting_reason, ''),
	       work_first_at, work_last_at, updated_at,
	       parked_hours, queued_hours, started_at, merged_at, bars, idle_by_role,
	       slot_wait_hours, sent_back, implementing_after_first_review,
	       testing_after_first_review, children, coalesce(repo, '')
	from issue_facts `

// readRows is the stored rows of project that where (a where clause, with its order) selects,
// whose arguments args are.
func readRows(ctx context.Context, q Querier, project, where string, args ...any) ([]Row, error) {
	result, err := q.Query(ctx, rowsSelect+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	defer result.Close()
	var rows []Row
	for result.Next() {
		row, err := scanRow(result, project)
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

// scanRow reads one row of readRows' columns.
func scanRow(result pgx.Rows, project string) (Row, error) {
	row := Row{Project: project}
	// opened_at is a nullable column, which a row this package wrote always fills.
	var opened *time.Time
	var hours, bars, idle []byte
	if err := result.Scan(&row.Issue, &opened, &row.ClosedAt, &hours, &row.WorkHours,
		&row.ReviewRounds, &row.RootKey, &row.TreeDepth, &row.SlotHours, &row.WaitingReason,
		&row.WorkFirstAt, &row.WorkLastAt, &row.UpdatedAt,
		&row.ParkedHours, &row.QueuedHours, &row.StartedAt, &row.MergedAt, &bars, &idle,
		&row.SlotWaitHours, &row.SentBack, &row.ImplementingAfterFirstReview,
		&row.TestingAfterFirstReview, &row.Children, &row.Repo); err != nil {
		return Row{}, fmt.Errorf("read the Legion facts of %s: %w", project, err)
	}
	if opened != nil {
		row.OpenedAt = *opened
	}
	for _, column := range []struct {
		stored []byte
		into   any
	}{{hours, &row.HoursByPhase}, {bars, &row.Bars}, {idle, &row.IdleByRole}} {
		if err := json.Unmarshal(column.stored, column.into); err != nil {
			return Row{}, fmt.Errorf("read the Legion facts of %s: %w", row.Issue, err)
		}
	}
	return row, nil
}

// upsertFilings writes the filings of the project, in one statement as upsert writes the rows. A
// filing whose stored values already match is left alone, and a filing a session of another
// project made of the same issue before is moved to this one, since issue_key is the table's
// identity and the newest title of the filing session is what says whose filing it is.
func upsertFilings(ctx context.Context, q Querier, project string, filings []Filing) error {
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
		keys[index] = filing.Issue
		filedAt[index] = filing.FiledAt
		roles[index] = filing.FilerRole
		filers[index] = filing.FilerIssue
		phases[index] = filing.FilerPhase
		classes[index] = filing.Class
		roots[index] = filing.RootKey
	}
	if err := exec(ctx, q, `
		insert into issue_filings (issue_key, project_key, filed_at, filer_role, filer_issue,
			filer_phase, class, root_key)
		select given.issue_key, $1, given.filed_at, given.filer_role, given.filer_issue,
		       given.filer_phase, given.class, given.root_key
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
			root_key = excluded.root_key
		where (issue_filings.project_key, issue_filings.filed_at, issue_filings.filer_role,
		       issue_filings.filer_issue, issue_filings.filer_phase, issue_filings.class,
		       issue_filings.root_key)
		  is distinct from
		      (excluded.project_key, excluded.filed_at, excluded.filer_role,
		       excluded.filer_issue, excluded.filer_phase, excluded.class, excluded.root_key)
	`, project, keys, filedAt, roles, filers, phases, classes, roots); err != nil {
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
// filing another project's session made is that project's row, which this never reads.
func deleteStaleFilings(ctx context.Context, q Querier, write FilingsWrite) error {
	kept := make([]string, len(write.Filings))
	for index, filing := range write.Filings {
		kept[index] = filing.Issue
	}
	var err error
	switch {
	case write.Whole:
		err = exec(ctx, q, `
			delete from issue_filings where project_key = $1 and not (issue_key = any($2))
		`, write.Project, kept)
	case len(write.Stated) > 0:
		err = exec(ctx, q, `
			delete from issue_filings
			where project_key = $1 and issue_key = any($2) and not (issue_key = any($3))
		`, write.Project, write.Stated, kept)
	}
	if err != nil {
		return fmt.Errorf("delete the stale Legion filings of %s: %w", write.Project, err)
	}
	return nil
}

// ReadFilings answers the stored filings of a project, by filed issue: the filings a Legion
// session of the project made, wherever the filed issue lives.
func ReadFilings(ctx context.Context, q Querier, project string) (map[string]Filing, error) {
	read, err := readFilings(ctx, q, project, `where project_key = $1`, project)
	if err != nil {
		return nil, err
	}
	filings := make(map[string]Filing, len(read))
	for _, filing := range read {
		filings[filing.Issue] = filing
	}
	return filings, nil
}

// ReadFilingsIn answers the stored filings a Legion session of the project made from from up to
// to, oldest first, through issue_filings_day (migration 20261113082155): the filings the
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
		if err := result.Scan(&filing.Issue, &filing.FiledAt, &filing.FilerRole, &filing.FilerIssue,
			&filing.FilerPhase, &filing.Class, &filing.RootKey); err != nil {
			return nil, fmt.Errorf("read the Legion filings of %s: %w", project, err)
		}
		filings = append(filings, filing)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read the Legion filings of %s: %w", project, err)
	}
	return filings, nil
}
