package record

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const maxInt64 = uint64(^uint64(0) >> 1)

// Postgres implements Store exclusively through caller-owned pgx transactions. It has no pool:
// opening, committing, and retrying a transaction belong to the layer that owns its boundary.
type Postgres struct {
	// clock is the time every statement of the store writes and compares with in place of
	// Postgres's own now() (NewStoreWithClock), nil for the daemon's store (NewStore), which keeps
	// the database's clock.
	clock func() time.Time
}

var _ Store = (*Postgres)(nil)

// NewStore returns the stateless Postgres record implementation.
func NewStore() *Postgres { return &Postgres{} }

// NewStoreWithClock returns the Postgres record implementation on clock rather than on the
// database's: every statement that writes or compares with Postgres's now(), a column default
// among them — the outbox's created_at (Enqueue, EnqueueStart), a lease's lapse (OwedStarts,
// RoleRuns), a row's age (RoleRuns), an issue's moved_at (PutIssue), processed_events'
// processed_at (MarkProcessed), the refill mark (TakeDispatchRefill) and a tree lifecycle's
// updated_at (OpenTreeLifecycle) — takes clock's time instead. A replay of a recorded window
// builds its store with one, on the simulated clock its admission pass and its engine read, so
// the times the pass writes are the window's and not the moment the replay ran, and a release
// mark the pass writes from its own clock (Store.ReleaseSlotForWait) and an owed start's
// created_at sort in the line as the pass wrote them. The daemon keeps NewStore and Postgres's
// clock, for the reason QueuedSupervise's Age gives: a row's created_at and the time it is read
// against come from one clock.
func NewStoreWithClock(clock func() time.Time) *Postgres { return &Postgres{clock: clock} }

// stamp is the store's clock as a statement's argument: the time a statement that reads
// Postgres's clock writes or compares with in its place, as coalesce($n::timestamptz, now()).
// It is nil for the daemon's own store (NewStore), so each such statement reads the database's
// clock exactly as it did before the store took one.
func (s *Postgres) stamp() *time.Time {
	if s.clock == nil {
		return nil
	}
	at := s.clock()
	return &at
}

// stamped is the column list and the value list an insert adds after its own to name column, a
// time column whose default is Postgres's now(), and args with that value bound after the rest.
// The daemon's own store names nothing, so the column takes its default and the statement sends
// what it always sent; a store built with a clock names the column at the clock's time, since a
// default reads no clock but the database's. PutIssue alone takes it: the daemon's own write of an
// issue names no moved_at, so it still writes on a schema from before that column
// (20261111010031_issue_moved_at), which admit's TestTheRecordKeepsWhenAnIssueLastMoved relies on.
func (s *Postgres) stamped(column string, args []any) (names, values string, bound []any) {
	if s.clock == nil {
		return "", "", args
	}
	return ", " + column, ", $" + strconv.Itoa(len(args)+1), append(args, s.clock())
}

// MarkProcessed records the fact as applied, at the store's clock (stamp): processed_at is
// coalesce($3::timestamptz, now()), so the daemon's own store writes the column's default, now().
func (s *Postgres) MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error) {
	tag, err := tx.Exec(ctx, `insert into processed_events (source, event_id, processed_at) values ($1, $2, coalesce($3::timestamptz, now()))
		on conflict (source, event_id) do nothing`, source, eventID, s.stamp())
	if err != nil {
		return false, fmt.Errorf("mark %s event %s processed: %w", source, eventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetProcessed removes the processed-events row of one fact, so the same fact id applies again
// when it is sent anew. ApplyFact uses it for a refused production check report — the report of
// a look is one fact id, and a refusal of it must not stand as the answer to the implementer's
// next report of that same look (dispatch://INFRA-714/spec R12) — and for a fact a handler left
// unspent (intake.Result.Unspent).
func (s *Postgres) ForgetProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) error {
	if _, err := tx.Exec(ctx, "delete from processed_events where source = $1 and event_id = $2", source, eventID); err != nil {
		return fmt.Errorf("forget processed event %s/%s: %w", source, eventID, err)
	}
	return nil
}

func (s *Postgres) Processed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error) {
	var marked bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from processed_events where source = $1 and event_id = $2)",
		source, eventID).Scan(&marked); err != nil {
		return false, fmt.Errorf("read whether %s event %s is processed: %w", source, eventID, err)
	}
	return marked, nil
}

// issueColumns reads an empty ancestry as NULL, so a record with none scans as a nil Ancestry, as
// it was written, and so an empty list of the children a wait was on, and a NULL view_of as "", an
// issue that is a view on no ancestor; and it reads the priority column as it stands, Dispatch's
// own level (0 for P0 to 3 for P3, null for none), which scanIssue reads through
// DispatchPriority. The four issues.memory_* columns are no part of it: what a unit was drawn is
// an assignment row of its own (AssignmentsOf, AssignmentsFor), and nothing this release's daemon
// runs reads the columns but the back-fill of 20261111045636_experiment_assignments. The last
// column is the time the issue's hold began (Hold.Since), read off issue_holds, which only the
// engine's move into held writes (MarkHoldBegan); scanIssue keeps it for a held issue alone, so
// the lookup runs for a held issue alone too.
const issueColumns = `key, tree, project, title, parent, phase, generation, status, rank, handed_over, taken_back, linger_until, held_from, last_dispatch_seq, coalesce(hold_reason, ''), dispatch_status, repo, default_branch, github_repo, awaiting_ask, awaiting_gate_version, coalesce(awaiting_artifact, ''), awaiting_answered_at, coalesce(awaiting_answered_by, ''), coalesce(awaiting_answered_detail, ''), awaiting_since_seq, awaiting_since, handout_seq, coalesce(declared_seq, 0), declared_at, nullif(ancestry, '{}'), handed_over_from, coalesce(told_parent, ''), moved_at, tree_stalled_told_at, released_for_wait_at, coalesce(view_of, ''), children_wait_start, children_wait_task, children_wait_reason, children_wait_since, nullif(children_wait_children, '{}'), children_waited_at, members_owed, priority, resolved_known, resolved_priority, priority_from, case when held_from is not null then (select held_at from issue_holds where issue_holds.issue = issues.key) end`

func (s *Postgres) Issue(ctx context.Context, tx pgx.Tx, key string) (*Issue, error) {
	issue, err := scanIssue(tx.QueryRow(ctx, "select "+issueColumns+" from issues where key = $1", key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read issue %s: %w", key, err)
	}
	return issue, nil
}

func (s *Postgres) Issues(ctx context.Context, tx pgx.Tx) ([]Issue, error) {
	issues, err := queryIssues(ctx, tx, "select "+issueColumns+" from issues order by key")
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	return issues, nil
}

// LiveIssues is every record of project that is not finished, by key: each not in phase done, each
// set back to todo (a done root a person re-admits, a parked child of a live tree), and each done
// one that still holds a slot, which the next release of slots frees. The first two are the
// issues_live index's predicate (0062), so the read grows with the project's unfinished records,
// which admission's cap bounds, and never with the done records a project keeps for good; the
// last comes through the slots, which the cap bounds too.
func (s *Postgres) LiveIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error) {
	issues, err := queryIssues(ctx, tx, "select "+issueColumns+` from issues
		where project = $1 and (phase <> 'done' or status = 'todo')
		union all select `+issueColumns+` from issues
		where project = $1 and phase = 'done' and status <> 'todo' and key in (select issue from slots)
		order by key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the live issues of project %s: %w", project, err)
	}
	return issues, nil
}

// IssuesOf is the record of each of keys that is an issue of project, by key; a key with no record
// is left out. No keys read nothing.
func (s *Postgres) IssuesOf(ctx context.Context, tx pgx.Tx, project string, keys []string) ([]Issue, error) {
	if len(keys) == 0 {
		return []Issue{}, nil
	}
	issues, err := queryIssues(ctx, tx, "select "+issueColumns+" from issues where project = $1 and key = any($2) order by key", project, keys)
	if err != nil {
		return nil, fmt.Errorf("read issues %v of project %s: %w", keys, project, err)
	}
	return issues, nil
}

// IssuesByKey is the record of each of keys, whatever its project, by key; a key with no record is
// left out. No keys read nothing.
func (s *Postgres) IssuesByKey(ctx context.Context, tx pgx.Tx, keys []string) ([]Issue, error) {
	if len(keys) == 0 {
		return []Issue{}, nil
	}
	issues, err := queryIssues(ctx, tx, "select "+issueColumns+" from issues where key = any($1) order by key", keys)
	if err != nil {
		return nil, fmt.Errorf("read issues %v: %w", keys, err)
	}
	return issues, nil
}

// AncestorsOf is the record of each of keys and of every issue above it on the chain of recorded
// parents, by key order, for the issues of project; a key with no record contributes nothing. No
// keys read nothing.
//
// The chain is walked in the database, in one query whatever its depth, because its callers walk
// it under the one fact lock every fact takes (intake.ApplyFact): a read per step would cost a
// round trip per level on every pass (the pass rule on admit's Admission.Apply). The walk carries
// only the key and the parent, and the records are read once at the end; `union` is what ends it,
// since a chain that loops back on itself derives nothing new on its second turn.
func (s *Postgres) AncestorsOf(ctx context.Context, tx pgx.Tx, project string, keys []string) ([]Issue, error) {
	if len(keys) == 0 {
		return []Issue{}, nil
	}
	issues, err := queryIssues(ctx, tx, `with recursive chain as (
			select key, parent from issues where project = $1 and key = any($2)
			union
			select above.key, above.parent from issues above
				join chain on above.key = chain.parent and above.project = $1
		)
		select `+issueColumns+` from issues where project = $1 and key in (select key from chain) order by key`, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read the issues above %v of project %s: %w", keys, project, err)
	}
	return issues, nil
}

func (s *Postgres) QueuedRootsUnder(ctx context.Context, tx pgx.Tx, key string) ([]Issue, error) {
	issues, err := queryIssues(ctx, tx, "select "+issueColumns+` from issues
		where status = 'todo' and handed_over and key = tree and $1 = any(ancestry)
		order by key`, key)
	if err != nil {
		return nil, fmt.Errorf("list the queued roots whose chain holds %s: %w", key, err)
	}
	return issues, nil
}

// queryIssues is every issue record query selects with args, in its order.
func queryIssues(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]Issue, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, *issue)
	}
	return issues, rows.Err()
}

// SlottedIssues is the record of each issue of project that holds an admission slot, in slot order
// (index, then issue), in one read: what admission judges when it releases the slots a fact has
// finished with (admit's releaseSlots), on every pass (the pass rule on admit's Admission.Apply)
// and under the one fact lock. The slots table is shared by every project's daemon, so another
// project's slot is left out here rather than by the caller, and no issue of another project, and
// no issue holding no slot, is read at all. It selects issueColumns itself, so it reads every
// column scanIssue scans: the slots join is a derived table whose columns are renamed, so no
// column of issueColumns is ambiguous.
func (s *Postgres) SlottedIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error) {
	rows, err := tx.Query(ctx, `select `+issueColumns+` from issues
		join (select index, issue from slots) slot (slot_index, slot_issue) on slot.slot_issue = issues.key
		where issues.project = $1 order by slot.slot_index, slot.slot_issue`, project)
	if err != nil {
		return nil, fmt.Errorf("list the slotted issues of project %s: %w", project, err)
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("read a slotted issue of project %s: %w", project, err)
		}
		issues = append(issues, *issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the slotted issues of project %s: %w", project, err)
	}
	return issues, nil
}

func (s *Postgres) PutIssue(ctx context.Context, tx pgx.Tx, issue Issue) error {
	switch {
	case issue.Repo == "":
		return fmt.Errorf("record issue %s: repository is required", issue.Key)
	case issue.DefaultBranch == "":
		return fmt.Errorf("record issue %s: default branch is required", issue.Key)
	case issue.GitHubRepo == "":
		return fmt.Errorf("record issue %s: GitHub's spelling of its repository is required", issue.Key)
	case !strings.EqualFold(issue.GitHubRepo, issue.Repo):
		return fmt.Errorf("record issue %s: GitHub's spelling %s names another repository than %s", issue.Key, issue.GitHubRepo, issue.Repo)
	case issue.Generation > maxInt64:
		return fmt.Errorf("put issue %s: generation %d does not fit a bigint", issue.Key, issue.Generation)
	}
	noteIssueWrite(ctx, issue.Key)
	var heldFrom any
	var holdReason HoldReason
	if issue.Hold != nil {
		heldFrom, holdReason = string(issue.Hold.From), issue.Hold.Reason
	}
	var awaitingAsk, awaitingGate, awaitingArtifact, answeredAt, sinceSeq, since any
	var answeredBy Answer
	var answeredDetail string
	if issue.Awaiting != nil {
		if issue.Awaiting.Ask != "" {
			awaitingAsk = issue.Awaiting.Ask
		}
		if issue.Awaiting.GateVersion != 0 {
			awaitingGate = issue.Awaiting.GateVersion
		}
		if issue.Awaiting.Artifact != "" {
			awaitingArtifact = issue.Awaiting.Artifact
		}
		if issue.Awaiting.AnsweredAt != nil {
			answeredAt = *issue.Awaiting.AnsweredAt
		}
		answeredBy, answeredDetail = issue.Awaiting.AnsweredBy, issue.Awaiting.AnsweredDetail
		sinceSeq, since = markValues(issue.Awaiting.Since)
	}
	var childrenWaitStart, childrenWaitTask, childrenWaitReason, childrenWaitSince any
	var childrenWaitChildren []string
	if issue.ChildrenWait != nil {
		childrenWaitStart = nullableText(string(issue.ChildrenWait.Start))
		childrenWaitTask = nullableText(issue.ChildrenWait.Task)
		childrenWaitReason = nullableText(issue.ChildrenWait.Reason)
		if !issue.ChildrenWait.Since.IsZero() {
			childrenWaitSince = issue.ChildrenWait.Since
		}
		childrenWaitChildren = issue.ChildrenWait.Children
	}
	// The update's condition keeps a recorded issue on the repository, default branch and GitHub
	// spelling it was recorded with: a write naming others changes no row, and is refused below.
	// handout_seq, declared_seq and declared_at are StartHandout's, TakeInThrough's, Declare's and
	// ClearDeclared's alone, moved_at is MarkIssueMoved's, released_for_wait_at is
	// ReleaseSlotForWait's, ClearReleasedForWait's and ReleaseTreeSlots's, and members_owed is
	// OweMembers's and ClearMembersOwed's, so a copy of the issue read before any of
	// them never writes them back. That is what keeps the movement safe, since every write site
	// that marks one holds a copy of the issue read before its own fact and records the issue
	// through this statement in that same transaction. An insert names moved_at nowhere either,
	// but on a store built with a clock (stamped), so a newly recorded issue takes the moment of its
	// own write, which is the column's default on the daemon's store and the clock's time on such a
	// store, and tree_stalled_told_at, which is MarkTreeStalledTold's alone, is named in neither half,
	// so a root recorded again carries the stall it was already told about with it. The four
	// memory_* columns are named here no more than they are on Issue, which no longer carries
	// them: nothing writes those columns from this release on, since what a unit was drawn is an
	// assignment row of its own now (PutAssignments), and nothing this release's daemon runs reads
	// them but the back-fill of 20261111045636_experiment_assignments, and the copy by hand an
	// operator runs on a roll forward (docs/kubernetes.md), so an update this statement makes
	// leaves them at whatever the draw before this release left, and a row it inserts takes the
	// columns' defaults.
	// ready_pending_version and ready_pending_step are neither written nor read: READY is gone,
	// and the columns are left as the rows hold them so a rollback to a daemon that still reads
	// them finds its data. awaiting_slot_notice_at is named nowhere because the column is gone
	// with the notice it marked (20261109060737_drop_awaiting_slot_notice): a root keeps its row
	// through its wait, so no architect is ever told a slot has freed under it. view_of is the
	// record's like every column above it, written whole by both halves, so a registration that
	// finds no ancestor to be a view on writes the view away. The priority is written as
	// Dispatch's own level, the record's Priority.Level, null for NoPriority, as issueColumns
	// reads it back, and the resolved priority beside it the same way, with resolved_known and
	// priority_from: its level is null wherever Dispatch sent none, as the column's check holds it.
	var resolvedLevel *int
	if issue.Resolved.Known {
		resolvedLevel = issue.Resolved.Priority.Level()
	}
	names, values, args := s.stamped("moved_at", []any{
		issue.Key, issue.Tree, issue.Project, issue.Title, issue.Parent, string(issue.Phase), int64(issue.Generation), issue.Status,
		issue.Rank, issue.HandedOver, issue.TakenBack, issue.LingerUntil, heldFrom, issue.LastDispatchSeq, string(holdReason), issue.DispatchStatus,
		issue.Repo, issue.DefaultBranch, issue.GitHubRepo, awaitingAsk, awaitingGate, awaitingArtifact, answeredAt, string(answeredBy), answeredDetail,
		sinceSeq, since, issue.Ancestry, issue.HandedOverFrom, issue.ToldParent, issue.ViewOf,
		childrenWaitStart, childrenWaitTask, childrenWaitReason, childrenWaitSince, childrenWaitChildren, issue.ChildrenWaitedAt,
		issue.Priority.Level(), issue.Resolved.Known, resolvedLevel, issue.Resolved.From,
	})
	tag, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, parent, phase, generation, status, rank, handed_over, taken_back, linger_until, held_from, last_dispatch_seq, hold_reason, dispatch_status, repo, default_branch, github_repo,
		awaiting_ask, awaiting_gate_version, awaiting_artifact, awaiting_answered_at, awaiting_answered_by, awaiting_answered_detail, awaiting_since_seq, awaiting_since,
		ancestry, handed_over_from, told_parent, view_of,
		children_wait_start, children_wait_task, children_wait_reason, children_wait_since, children_wait_children, children_waited_at, priority,
		resolved_known, resolved_priority, priority_from`+names+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15::text, ''), $16, $17, $18, $19, $20, $21, $22, $23, nullif($24::text, ''), nullif($25::text, ''), $26, $27,
		coalesce($28::text[], '{}'), $29, nullif($30::text, ''), nullif($31::text, ''),
		$32, $33, $34, $35, nullif($36::text[], '{}'), $37, $38, $39, $40, $41`+values+`)
		on conflict (key) do update set tree = excluded.tree, project = excluded.project, title = excluded.title,
		parent = excluded.parent, phase = excluded.phase, generation = excluded.generation,
		status = excluded.status, rank = excluded.rank, handed_over = excluded.handed_over, taken_back = excluded.taken_back,
		linger_until = excluded.linger_until,
		held_from = excluded.held_from, last_dispatch_seq = excluded.last_dispatch_seq,
		hold_reason = excluded.hold_reason, dispatch_status = excluded.dispatch_status,
		awaiting_ask = excluded.awaiting_ask, awaiting_gate_version = excluded.awaiting_gate_version,
		awaiting_artifact = excluded.awaiting_artifact,
		awaiting_answered_at = excluded.awaiting_answered_at, awaiting_answered_by = excluded.awaiting_answered_by,
		awaiting_answered_detail = excluded.awaiting_answered_detail,
		awaiting_since_seq = excluded.awaiting_since_seq, awaiting_since = excluded.awaiting_since,
		ancestry = excluded.ancestry, handed_over_from = excluded.handed_over_from, told_parent = excluded.told_parent,
		view_of = excluded.view_of,
		children_wait_start = excluded.children_wait_start, children_wait_task = excluded.children_wait_task,
		children_wait_reason = excluded.children_wait_reason, children_wait_since = excluded.children_wait_since,
		children_wait_children = excluded.children_wait_children, children_waited_at = excluded.children_waited_at,
		priority = excluded.priority, resolved_known = excluded.resolved_known, resolved_priority = excluded.resolved_priority,
		priority_from = excluded.priority_from
		where issues.repo = excluded.repo and issues.default_branch = excluded.default_branch and issues.github_repo = excluded.github_repo`,
		args...)
	if err != nil {
		return fmt.Errorf("put issue %s: %w", issue.Key, err)
	}
	if tag.RowsAffected() == 0 {
		var repo, branch, github string
		if err := tx.QueryRow(ctx, "select repo, default_branch, github_repo from issues where key = $1", issue.Key).Scan(&repo, &branch, &github); err != nil {
			return fmt.Errorf("put issue %s: read the repository it is recorded on: %w", issue.Key, err)
		}
		return fmt.Errorf("record issue %s: it is recorded on %s@%s (GitHub's %s), and a record keeps its repository and default branch: only the one-time rename pass writes another repository name on it", issue.Key, repo, branch, github)
	}
	return nil
}

// MarkIssueMoved records at as when the daemon last saw key move. It is the one writer of
// moved_at on a recorded issue: PutIssue names the column, if at all, on an insert alone, so the
// write sites can hold a copy of the issue read before their own fact and still record that
// fact's movement.
//
// The statement is monotone — `moved_at <= $2` — so a time at or before the recorded one changes
// no row. The facts that move an issue do not arrive in the order they were made: a GitHub
// webhook can be delivered behind a later one, a boot applies a listing beside live events, and
// a redelivery replays a fact the record already took. Without the condition any of those would
// take the record back to an older moment, and a tree would read as quieter than it is, which is
// the one error this measure must not make.
//
// A key with no record matches no row and writes nothing, which is right: a fact of an issue the
// daemon does not run moves nothing, and the caller has no record to tell apart from a stale
// one. The write is the caller's own transaction, so the movement commits with the fact that
// made it or with neither.
func (s *Postgres) MarkIssueMoved(ctx context.Context, tx pgx.Tx, key string, at time.Time) error {
	tag, err := tx.Exec(ctx, "update issues set moved_at = $2 where key = $1 and moved_at <= $2", key, at)
	if err != nil {
		return fmt.Errorf("mark issue %s moved at %s: %w", key, at, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// MarkHoldBegan records at as when key's hold began (Hold.Since), the time the slots reading
// dates a held holder by. It is the one writer of issue_holds, and its one caller is the engine's
// move into held, in the transaction that writes the hold: a later hold of the same issue
// replaces the time, so the row always dates the hold the issue stands in, since no other path
// moves an issue into held. The row goes with its issue (issue_holds' foreign key), and a key with
// no record has no row to hold and fails the write.
func (s *Postgres) MarkHoldBegan(ctx context.Context, tx pgx.Tx, key string, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into issue_holds (issue, held_at) values ($1, $2)
		on conflict (issue) do update set held_at = excluded.held_at`, key, at); err != nil {
		return fmt.Errorf("mark the hold of %s begun at %s: %w", key, at, err)
	}
	return nil
}

// MarkTreeStalledTold records at as the movement the daemon told root's tree stopped for. It is
// the one writer of tree_stalled_told_at: PutIssue names the column nowhere, so the pass can
// hold a copy of the root read before the telling and still mark it.
//
// The write is not monotone, unlike MarkIssueMoved's, because the column is not a movement of
// the tree but the movement a notice was written for. The rule writes it only where it has
// just told the controller of a tree whose newest movement stands after the mark (admit's
// tellStalledTrees), so every write carries the row forwards; a statement that compared the
// two would say the same thing in SQL and hide the rule's own condition.
//
// A key with no record matches no row and writes nothing. The write is the caller's own
// transaction, so the mark commits with the notice it was written for or with neither: a stall
// told to nobody is a stall the next pass tells.
func (s *Postgres) MarkTreeStalledTold(ctx context.Context, tx pgx.Tx, root string, at time.Time) error {
	tag, err := tx.Exec(ctx, "update issues set tree_stalled_told_at = $2 where key = $1", root, at)
	if err != nil {
		return fmt.Errorf("mark the tree of %s told stalled at %s: %w", root, at, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) UnfilledIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error) {
	return s.issuesWhere(ctx, tx, "project = $1 and (repo = '' or default_branch = '')", project, "with no repository")
}

func (s *Postgres) UnnamedIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error) {
	return s.issuesWhere(ctx, tx, "project = $1 and repo <> '' and github_repo = ''", project, "with no GitHub spelling of their repository")
}

func (s *Postgres) HandedOffWithoutOpenOrMergedPullRequest(ctx context.Context, tx pgx.Tx, project string) ([]HandedOff, error) {
	const what = "in implementing with a recorded handoff and no open or merged pull request"
	rows, err := tx.Query(ctx, "select "+issueColumns+", implementer.handoff_commit from issues"+
		" join (select issue, handoff_commit from phases where role = '"+string(claim.RoleImplementer)+"' and handoff_commit <> '')"+
		" implementer (implementer_issue, handoff_commit) on implementer.implementer_issue = issues.key"+
		" where project = $1 and github_repo <> '' and phase = '"+string(phase.Implementing)+"'"+
		" and key not in (select issue from pull_requests where state = any($2))"+
		" order by key", project, implementationMoveSQLStates)
	if err != nil {
		return nil, fmt.Errorf("list the issues of %s %s: %w", project, what, err)
	}
	defer rows.Close()
	var handedOff []HandedOff
	for rows.Next() {
		var commit string
		issue, err := scanIssue(scanThen{row: rows, after: []any{&commit}})
		if err != nil {
			return nil, fmt.Errorf("list the issues of %s %s: %w", project, what, err)
		}
		handedOff = append(handedOff, HandedOff{Issue: *issue, Commit: commit})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the issues of %s %s: %w", project, what, err)
	}
	return handedOff, nil
}

// scanThen is row with after scanned from the columns that follow the ones a Scan names.
type scanThen struct {
	row   scanner
	after []any
}

func (s scanThen) Scan(dest ...any) error {
	return s.row.Scan(append(dest, s.after...)...)
}

// issuesWhere is every record of project the condition selects, by key; what names them in an
// error.
func (s *Postgres) issuesWhere(ctx context.Context, tx pgx.Tx, condition, project, what string) ([]Issue, error) {
	rows, err := tx.Query(ctx, "select "+issueColumns+" from issues where "+condition+" order by key", project)
	if err != nil {
		return nil, fmt.Errorf("list the issues of %s %s: %w", project, what, err)
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("list the issues of %s %s: %w", project, what, err)
		}
		issues = append(issues, *issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the issues of %s %s: %w", project, what, err)
	}
	return issues, nil
}

func (s *Postgres) FillIssueRepository(ctx context.Context, tx pgx.Tx, key, repo, defaultBranch, githubRepo string) error {
	if repo == "" || defaultBranch == "" || githubRepo == "" {
		return fmt.Errorf("fill the repository of %s: the repository, its default branch and GitHub's spelling of it are required", key)
	}
	if !strings.EqualFold(repo, githubRepo) {
		return fmt.Errorf("fill the repository of %s: GitHub's spelling %s names another repository than %s", key, githubRepo, repo)
	}
	tag, err := tx.Exec(ctx, "update issues set repo = $2, default_branch = $3, github_repo = $4 where key = $1 and repo = '' and default_branch = ''",
		key, repo, defaultBranch, githubRepo)
	if err != nil {
		return fmt.Errorf("fill the repository of %s: %w", key, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fill the repository of %s: it has no unfilled record", key)
	}
	return nil
}

func (s *Postgres) NameIssueRepository(ctx context.Context, tx pgx.Tx, key, repo, githubRepo string) error {
	if !strings.EqualFold(repo, githubRepo) {
		return fmt.Errorf("name the repository of %s: GitHub's spelling %s names another repository than %s", key, githubRepo, repo)
	}
	tag, err := tx.Exec(ctx, "update issues set github_repo = $3 where key = $1 and repo = $2 and github_repo = ''", key, repo, githubRepo)
	if err != nil {
		return fmt.Errorf("name the repository of %s: %w", key, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("name the repository of %s: it has no record on %s without GitHub's spelling", key, repo)
	}
	return nil
}

func (s *Postgres) RepoNote(ctx context.Context, tx pgx.Tx, issue string) (*RepoNote, error) {
	var note RepoNote
	err := tx.QueryRow(ctx, "select issue, resolved_repo, note, posted_at from repo_notes where issue = $1", issue).
		Scan(&note.Issue, &note.ResolvedRepo, &note.Note, &note.PostedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the repository note of %s: %w", issue, err)
	}
	return &note, nil
}

func (s *Postgres) PutRepoNote(ctx context.Context, tx pgx.Tx, note RepoNote) error {
	if _, err := tx.Exec(ctx, `insert into repo_notes (issue, resolved_repo, note, posted_at) values ($1, $2, $3, $4)
		on conflict (issue) do update set resolved_repo = excluded.resolved_repo, note = excluded.note, posted_at = excluded.posted_at`,
		note.Issue, note.ResolvedRepo, note.Note, note.PostedAt); err != nil {
		return fmt.Errorf("record the repository note of %s: %w", note.Issue, err)
	}
	return nil
}

// OpenTreeLifecycle is the admission fact's transactional entry point into the shared lifecycle
// barrier. A lifetime it starts is stamped at the store's clock (treelifecycle.OpenAt), and at
// Postgres's now() for the daemon's own store.
func (s *Postgres) OpenTreeLifecycle(ctx context.Context, tx pgx.Tx, project, tree string, authority treelifecycle.Authority) (treelifecycle.Lifecycle, error) {
	if s.clock == nil {
		return treelifecycle.Open(ctx, tx, project, tree, authority)
	}
	return treelifecycle.OpenAt(ctx, tx, project, tree, authority, s.clock())
}

// TreeLifecycle reads the tree's lifecycle without opening it.
func (s *Postgres) TreeLifecycle(ctx context.Context, tx pgx.Tx, project, tree string) (treelifecycle.Lifecycle, bool, error) {
	return treelifecycle.Read(ctx, tx, project, tree)
}

// TreeLifecycles reads the lifecycle of each of trees in one query, opening nothing; a tree with
// no row has no entry. project is the claim project token the tree_lifecycles table is keyed by,
// the value OpenTreeLifecycle takes, not the Dispatch project key.
func (s *Postgres) TreeLifecycles(ctx context.Context, tx pgx.Tx, project string, trees []string) (map[string]treelifecycle.Lifecycle, error) {
	return treelifecycle.ReadMany(ctx, tx, project, trees)
}

func scanIssue(row scanner) (*Issue, error) {
	var issue Issue
	var phaseValue string
	var generation int64
	var heldFrom *string
	var holdReason string
	var awaitingAsk *string
	var awaitingGate *int
	var awaitingArtifact string
	var answeredAt *time.Time
	var answeredBy, answeredDetail string
	var sinceSeq *int64
	var since, declaredAt, treeStalledToldAt, releasedForWaitAt, childrenWaitSince, childrenWaitedAt, heldSince *time.Time
	var childrenWaitStart, childrenWaitTask, childrenWaitReason *string
	var childrenWaitChildren []string
	var priority, resolvedPriority *int
	if err := row.Scan(&issue.Key, &issue.Tree, &issue.Project, &issue.Title, &issue.Parent, &phaseValue, &generation, &issue.Status,
		&issue.Rank, &issue.HandedOver, &issue.TakenBack, &issue.LingerUntil, &heldFrom, &issue.LastDispatchSeq, &holdReason, &issue.DispatchStatus,
		&issue.Repo, &issue.DefaultBranch, &issue.GitHubRepo, &awaitingAsk, &awaitingGate, &awaitingArtifact, &answeredAt, &answeredBy, &answeredDetail,
		&sinceSeq, &since, &issue.HandoutSeq, &issue.DeclaredSeq, &declaredAt, &issue.Ancestry, &issue.HandedOverFrom,
		&issue.ToldParent, &issue.MovedAt, &treeStalledToldAt, &releasedForWaitAt,
		&issue.ViewOf, &childrenWaitStart, &childrenWaitTask, &childrenWaitReason, &childrenWaitSince, &childrenWaitChildren,
		&childrenWaitedAt, &issue.MembersOwed, &priority, &issue.Resolved.Known, &resolvedPriority, &issue.Resolved.From, &heldSince); err != nil {
		return nil, err
	}
	// The columns' checks hold both to Dispatch's 0 to 3 or null, each of which DispatchPriority
	// reads, and the resolved one to null wherever Dispatch sent none.
	issue.Priority, _ = DispatchPriority(priority)
	issue.Resolved.Priority, _ = DispatchPriority(resolvedPriority)
	if declaredAt != nil {
		issue.DeclaredAt = declaredAt.UTC()
	}
	// The column is not null, so the movement is scanned straight into the record; it is read in
	// UTC as every other time the record answers is.
	issue.MovedAt = issue.MovedAt.UTC()
	if treeStalledToldAt != nil {
		utc := treeStalledToldAt.UTC()
		issue.TreeStalledToldAt = &utc
	}
	if releasedForWaitAt != nil {
		utc := releasedForWaitAt.UTC()
		issue.ReleasedForWaitAt = &utc
	}
	if generation < 0 {
		return nil, fmt.Errorf("issue %s has negative generation %d", issue.Key, generation)
	}
	issue.Phase = phase.Phase(phaseValue)
	if heldFrom != nil {
		issue.Hold = &Hold{From: phase.Phase(*heldFrom), Reason: HoldReason(holdReason)}
		if heldSince != nil {
			utc := heldSince.UTC()
			issue.Hold.Since = &utc
		}
	}
	if awaitingAsk != nil || awaitingGate != nil {
		if answeredAt != nil {
			utc := answeredAt.UTC()
			answeredAt = &utc
		}
		issue.Awaiting = &Awaiting{Artifact: awaitingArtifact, AnsweredAt: answeredAt, AnsweredBy: Answer(answeredBy),
			AnsweredDetail: answeredDetail, Since: scannedMark(sinceSeq, since)}
		if awaitingAsk != nil {
			issue.Awaiting.Ask = *awaitingAsk
		}
		if awaitingGate != nil {
			issue.Awaiting.GateVersion = *awaitingGate
		}
	}
	if childrenWaitStart != nil {
		wait := ChildrenWait{Start: phase.Phase(*childrenWaitStart), Children: childrenWaitChildren}
		if childrenWaitTask != nil {
			wait.Task = *childrenWaitTask
		}
		if childrenWaitReason != nil {
			wait.Reason = *childrenWaitReason
		}
		if childrenWaitSince != nil {
			wait.Since = childrenWaitSince.UTC()
		}
		issue.ChildrenWait = &wait
	}
	if childrenWaitedAt != nil {
		utc := childrenWaitedAt.UTC()
		issue.ChildrenWaitedAt = &utc
	}
	issue.Generation = uint64(generation)
	return &issue, nil
}

// MergersReady is the interface's: one statement over the merger's phases rows of issues, whose
// key (issue, role) holds one row per issue.
func (s *Postgres) MergersReady(ctx context.Context, tx pgx.Tx, issues []string) ([]string, error) {
	rows, err := tx.Query(ctx, `select issue from phases where role = $1 and handoff_commit <> '' and issue = any($2)`,
		string(claim.RoleMerger), issues)
	if err != nil {
		return nil, fmt.Errorf("read the merger's ready marks of %v: %w", issues, err)
	}
	ready, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the merger's ready marks of %v: %w", issues, err)
	}
	return ready, nil
}

func (s *Postgres) Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error) {
	rows, err := tx.Query(ctx, `select issue, role, claim, handoff_commit, pass, handoff_record, handoff_version, rounds, conflict_rounds, backward_moves, cap_told, verdict, summary, last_handoff, decision, completed_at, entry, entry_task, entry_verbatim, entry_raised_at, entry_moved_back, coalesce(entry_from, ''), entry_given_task, entry_given_entry, entry_given_generation, reviewed_head, step_task, step_place from phases
		where issue = $1 order by role`, issue)
	if err != nil {
		return nil, fmt.Errorf("list phases for %s: %w", issue, err)
	}
	defer rows.Close()
	phases := []PhaseRow{}
	for rows.Next() {
		phase, err := scanPhase(rows)
		if err != nil {
			return nil, fmt.Errorf("list phases for %s: %w", issue, err)
		}
		phases = append(phases, phase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list phases for %s: %w", issue, err)
	}
	return phases, nil
}

// PhaseEntries reads the entry of every phases row of issues, the moment it was raised and whether
// its task is one to send as written, in one statement, keyed by issue and role. The rows of an
// issue the record holds none of are simply absent, which every reader takes as the zero entry.
// entry_raised_at is the row's one nullable column (PutPhase), null on a row no entry has been
// raised on, and reads back as the zero moment.
func (s *Postgres) PhaseEntries(ctx context.Context, tx pgx.Tx, issues []string) (map[PhaseRole]PhaseEntry, error) {
	entries := make(map[PhaseRole]PhaseEntry, len(issues))
	if len(issues) == 0 {
		return entries, nil
	}
	rows, err := tx.Query(ctx, `select issue, role, entry, entry_raised_at, entry_verbatim from phases where issue = any($1)`, issues)
	if err != nil {
		return nil, fmt.Errorf("list the phase entries of %d issues: %w", len(issues), err)
	}
	defer rows.Close()
	for rows.Next() {
		var key PhaseRole
		var role string
		var entry PhaseEntry
		var raisedAt *time.Time
		if err := rows.Scan(&key.Issue, &role, &entry.Entry, &raisedAt, &entry.Verbatim); err != nil {
			return nil, fmt.Errorf("read a phase entry: %w", err)
		}
		if raisedAt != nil {
			entry.RaisedAt = raisedAt.UTC()
		}
		key.Role = claim.Role(role)
		entries[key] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the phase entries of %d issues: %w", len(issues), err)
	}
	return entries, nil
}

func (s *Postgres) PutPhase(ctx context.Context, tx pgx.Tx, phase PhaseRow) error {
	if phase.Given.Generation > maxInt64 {
		return fmt.Errorf("put %s phase on %s: the generation %d of its given task does not fit a bigint", phase.Role, phase.Issue, phase.Given.Generation)
	}
	var decision []byte
	if phase.Decision != nil {
		var err error
		if decision, err = json.Marshal(phase.Decision); err != nil {
			return fmt.Errorf("put %s phase on %s: encode the review decision: %w", phase.Role, phase.Issue, err)
		}
	}
	// backward_moves is not null and defaults to the empty object, so a row with no move counted
	// yet holds '{}' rather than null: a nil or empty map is written as that same empty object,
	// which is what scanPhase reads back as nil.
	backwardMoves := []byte("{}")
	if len(phase.BackwardMoves) > 0 {
		var err error
		if backwardMoves, err = json.Marshal(phase.BackwardMoves); err != nil {
			return fmt.Errorf("put %s phase on %s: encode the backward move counts: %w", phase.Role, phase.Issue, err)
		}
	}
	// entry_raised_at and entry_from are the nullable columns of the row: a row no entry has been
	// raised on has no moment to record, and the zero time would read as one; an entry no move
	// raised has no phase it moved from.
	var raisedAt, from any
	if !phase.EntryRaisedAt.IsZero() {
		raisedAt = phase.EntryRaisedAt
	}
	if phase.EntryFrom != "" {
		from = string(phase.EntryFrom)
	}
	tag, err := tx.Exec(ctx, `insert into phases (issue, role, claim, handoff_commit, pass, handoff_record, handoff_version, rounds, conflict_rounds, backward_moves, cap_told, verdict, summary, last_handoff,
		decision, completed_at, entry, entry_task, entry_verbatim, entry_raised_at, entry_moved_back, entry_from, entry_given_task, entry_given_entry, entry_given_generation, reviewed_head, step_task, step_place)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28)
		on conflict (issue, role) do update set claim = excluded.claim,
		handoff_commit = excluded.handoff_commit, pass = excluded.pass,
		handoff_record = excluded.handoff_record, handoff_version = excluded.handoff_version,
		rounds = excluded.rounds, conflict_rounds = excluded.conflict_rounds, backward_moves = excluded.backward_moves,
		cap_told = excluded.cap_told, verdict = excluded.verdict,
		summary = excluded.summary, last_handoff = excluded.last_handoff, decision = excluded.decision,
		completed_at = excluded.completed_at,
		entry = excluded.entry, entry_task = excluded.entry_task, entry_verbatim = excluded.entry_verbatim,
		entry_raised_at = excluded.entry_raised_at, entry_moved_back = excluded.entry_moved_back, entry_from = excluded.entry_from,
		entry_given_task = excluded.entry_given_task, entry_given_entry = excluded.entry_given_entry, entry_given_generation = excluded.entry_given_generation,
		reviewed_head = excluded.reviewed_head, step_task = excluded.step_task, step_place = excluded.step_place`,
		phase.Issue, string(phase.Role), string(phase.Claim), phase.HandoffCommit, phase.Pass, phase.HandoffRecord, phase.HandoffVersion, phase.Rounds, phase.ConflictRounds,
		backwardMoves, phase.CapTold, phase.Verdict, phase.Summary, phase.LastHandoff, decision, phase.CompletedAt,
		phase.Entry, phase.EntryTask, phase.EntryVerbatim, raisedAt, phase.EntryMovedBack, from, phase.Given.Task, phase.Given.Entry, int64(phase.Given.Generation),
		phase.ReviewedHead, phase.StepTask, phase.StepPlace,
	)
	if err != nil {
		return fmt.Errorf("put %s phase on %s: %w", phase.Role, phase.Issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func scanPhase(row scanner) (PhaseRow, error) {
	var phase PhaseRow
	var role, token string
	var decision, backwardMoves []byte
	var raisedAt *time.Time
	var givenGeneration int64
	if err := row.Scan(&phase.Issue, &role, &token, &phase.HandoffCommit, &phase.Pass, &phase.HandoffRecord, &phase.HandoffVersion, &phase.Rounds, &phase.ConflictRounds,
		&backwardMoves, &phase.CapTold, &phase.Verdict, &phase.Summary, &phase.LastHandoff, &decision, &phase.CompletedAt,
		&phase.Entry, &phase.EntryTask, &phase.EntryVerbatim, &raisedAt, &phase.EntryMovedBack, &phase.EntryFrom, &phase.Given.Task, &phase.Given.Entry,
		&givenGeneration, &phase.ReviewedHead, &phase.StepTask, &phase.StepPlace); err != nil {
		return PhaseRow{}, err
	}
	phase.Given.Generation = uint64(givenGeneration)
	if raisedAt != nil {
		phase.EntryRaisedAt = raisedAt.UTC()
	}
	phase.CompletedAt = phase.CompletedAt.UTC()
	if decision != nil {
		phase.Decision = &ReviewDecision{}
		if err := json.Unmarshal(decision, phase.Decision); err != nil {
			return PhaseRow{}, fmt.Errorf("decode the review decision: %w", err)
		}
	}
	// A row that counted no backward move reads as a nil map and never as an empty one: the
	// column holds the empty object for it, and a scanned row of a phase nothing went back from
	// is then equal to the PhaseRow a caller builds for it. The empty object is what nearly every
	// row holds, and this scan runs on every phase row of every state read, so those bytes answer
	// the nil map without a decode of their own, as the decision column above answers nil without
	// one. Any other bytes, a null among them, still go through json.Unmarshal and still answer
	// its error.
	if !bytes.Equal(backwardMoves, []byte("{}")) {
		moves := map[string]int{}
		if err := json.Unmarshal(backwardMoves, &moves); err != nil {
			return PhaseRow{}, fmt.Errorf("decode the backward move counts: %w", err)
		}
		if len(moves) > 0 {
			phase.BackwardMoves = moves
		}
	}
	phase.Role, phase.Claim = claim.Role(role), claim.Token(token)
	return phase, nil
}

const pullRequestColumns = `issue, repo, number, branch, base, base_moved, head_sha, head_updated_at,
	failing, cancelled, check_runs,
	generation, snapshot, statuses, pushes, head_moves, head_run, review_seen, review_seen_at, state,
	checked_head, mergeability, merge_state, reviews_requested, head_checks, null_mergeable_reads, null_mergeable_since,
	evidence_read_at, state_read_at,
	conflicting_head, merge_sha, conflict_read, merge_forward`

// PullRequest, PullRequestByBranch and PullRequestByNumber each read one pull request whole, with
// what it remembers of its settled heads (loadSettledHeads), so each record they answer is one a
// write may keep (PutPullRequest).
func (s *Postgres) PullRequest(ctx context.Context, tx pgx.Tx, issue string) (*PullRequest, error) {
	pr, err := onePullRequest(ctx, tx, pullRequestByIssue, issue)
	if err != nil {
		return nil, fmt.Errorf("read pull request for %s: %w", issue, err)
	}
	return pr, nil
}

// pullRequestByIssue is the query of the two reads of one pull request by its issue: PullRequest
// reads it whole, PullRequestRow its row alone.
const pullRequestByIssue = "select " + pullRequestColumns + " from pull_requests where issue = $1"

// onePullRequest is the row query selects with what it remembers of its settled heads, nil for no
// row.
func onePullRequest(ctx context.Context, tx pgx.Tx, query string, args ...any) (*PullRequest, error) {
	pr, err := queryPullRequestRow(ctx, tx, "", query, args...)
	if err != nil || pr == nil {
		return nil, err
	}
	if err := loadSettledHeads(ctx, tx, pr); err != nil {
		return nil, err
	}
	return pr, nil
}

// queryPullRequestRow is the row query selects alone, with none of what it remembers of its
// settled heads, nil for no row, marked with readBy, the read that made it (PullRequest.readBy):
// "" for onePullRequest, which loads that memory next.
func queryPullRequestRow(ctx context.Context, tx pgx.Tx, readBy, query string, args ...any) (*PullRequest, error) {
	pr, err := scanPullRequest(tx.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pr.readBy = readBy
	return pr, nil
}

// The three reads below key the pull_requests table on its repository — PullRequestByBranch,
// PullRequestByNumber and PullRequestRecorded — and each compares that name without case, as
// every other comparison of two repository names in the daemon does (PutIssue's strings.EqualFold
// above, ghrepo.Renames.Same): GitHub spells a repository as its owner does, the row carries the
// spelling of the fact that opened the pull request, and a fact that names the same repository in
// another spelling names the same row. The exact match was the outlier, and it lost exactly the
// row a configured rename exists to find — a pull request recorded under GitHub's spelling of the
// name the repository moved from, read under the pair's spelling of it — leaving a recorded pull
// request read as one the daemon does not record. PullRequestRowByBranch and
// PullRequestRowByNumber run the first two's queries (pullRequestByBranch, pullRequestByNumber).
//
// Each read is bounded by an index on the expression it writes: PullRequestByNumber and
// PullRequestRecorded by pull_requests_lower_repo_number, PullRequestByBranch by
// pull_requests_lower_repo_branch, both from migration 20261102095707, which replaced 0062's
// pull_requests_repo_number. An index on (repo, number) or (repo, branch) bounds none of these
// reads, because lower(repo) is not the indexed expression: the planner walks the whole index and
// filters every row it reads, a cost that grows with the records of finished issues, which are
// never deleted. 0004's unique pull_requests_repo_branch stays the table's uniqueness rule. The
// one other read of the table that keys on the repository is this release's one-time rename pass
// (rename.go: RenameCandidates, RenameIssueRepository), which compares the name
// without case in the same way.
const (
	pullRequestByBranch = "select " + pullRequestColumns + " from pull_requests where lower(repo) = lower($1) and branch = $2"
	pullRequestByNumber = "select " + pullRequestColumns + " from pull_requests where lower(repo) = lower($1) and number = $2"
)

func (s *Postgres) PullRequestByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*PullRequest, error) {
	pr, err := onePullRequest(ctx, tx, pullRequestByBranch, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("read pull request for %s %s: %w", repo, branch, err)
	}
	return pr, nil
}

// PullRequestByNumber reads the pull request a GitHub event names. The engine reached it by
// listing every issue and reading each one's pull request — a read per issue for every check,
// review and push event — and the row is one query away.
func (s *Postgres) PullRequestByNumber(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequest, error) {
	pr, err := onePullRequest(ctx, tx, pullRequestByNumber, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read pull request %s#%d: %w", repo, number, err)
	}
	return pr, nil
}

// PullRequestRow, PullRequestRowByBranch and PullRequestRowByNumber read the pull request
// PullRequest, PullRequestByBranch and PullRequestByNumber read, by the same query, as its row
// alone: with none of what it remembers of its settled heads, and marked with the read that made
// it (PullRequest.readBy), so PutPullRequest refuses it. They are a reader's that never writes the
// record back and reads neither its SettledHeads nor its head run's Settled, which the whole read
// would load for nothing.
func (s *Postgres) PullRequestRow(ctx context.Context, tx pgx.Tx, issue string) (*PullRequest, error) {
	pr, err := queryPullRequestRow(ctx, tx, "PullRequestRow", pullRequestByIssue, issue)
	if err != nil {
		return nil, fmt.Errorf("read the pull request of %s: %w", issue, err)
	}
	return pr, nil
}

func (s *Postgres) PullRequestRowByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*PullRequest, error) {
	pr, err := queryPullRequestRow(ctx, tx, "PullRequestRowByBranch", pullRequestByBranch, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("read the row of the pull request for %s %s: %w", repo, branch, err)
	}
	return pr, nil
}

func (s *Postgres) PullRequestRowByNumber(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequest, error) {
	pr, err := queryPullRequestRow(ctx, tx, "PullRequestRowByNumber", pullRequestByNumber, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read the row of pull request %s#%d: %w", repo, number, err)
	}
	return pr, nil
}

func (s *Postgres) ClearedPullRequestClock(ctx context.Context, tx pgx.Tx, repo string, number int) (time.Time, error) {
	var clock *time.Time
	if err := tx.QueryRow(ctx, `select max(head_updated_at) from cleared_pull_requests where lower(repo) = lower($1) and number = $2`,
		repo, number).Scan(&clock); err != nil {
		return time.Time{}, fmt.Errorf("read the clock of cleared pull request %s#%d: %w", repo, number, err)
	}
	if clock == nil {
		return time.Time{}, nil
	}
	return *clock, nil
}

// PullRequestRecorded says whether a pull request of repo is recorded by number, as
// PullRequestByNumber looks it up.
func PullRequestRecorded(ctx context.Context, tx pgx.Tx, repo string, number int) (bool, error) {
	var recorded bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from pull_requests where lower(repo) = lower($1) and number = $2)`, repo, number).Scan(&recorded); err != nil {
		return false, fmt.Errorf("read whether %s#%d is recorded: %w", repo, number, err)
	}
	return recorded, nil
}

// OpenPullRequests is every open pull request of the Dispatch project's issues, by issue key, with
// none of what each remembers of its settled heads (PullRequest.SettledHeads).
func (s *Postgres) OpenPullRequests(ctx context.Context, tx pgx.Tx, project string) ([]PullRequest, error) {
	describe := fmt.Sprintf("list the open pull requests of %s", project)
	rows, err := tx.Query(ctx, "select "+pullRequestColumns+" from pull_requests where state = $1 and issue in (select key from issues where project = $2) order by issue",
		PullRequestOpen, project)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return collectPullRequests(rows, describe, "OpenPullRequests")
}

// OpenPullRequestsOnBase is every open pull request of repo whose recorded base branch is base, by
// issue key. It is one indexed lookup (pull_requests_open_on_base, a partial index of the open rows
// by repository and base), so a push onto a branch no open pull request sits on - the common case
// for a repository other automation pushes to many times a minute - reads one index and no row. A
// base recorded as "" is every pull request written before the daemon kept the base at all, so it
// matches none: the caller never asks for one, and the next GitHub read fills those rows. It reads
// none of what each pull request remembers of its settled heads (PullRequest.SettledHeads).
func (s *Postgres) OpenPullRequestsOnBase(ctx context.Context, tx pgx.Tx, repo, base string) ([]PullRequest, error) {
	if base == "" {
		return nil, nil
	}
	describe := fmt.Sprintf("list the open pull requests of %s on %s", repo, base)
	rows, err := tx.Query(ctx, "select "+pullRequestColumns+" from pull_requests where repo = $1 and base = $2 and state = $3 order by issue",
		repo, base, PullRequestOpen)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return collectPullRequests(rows, describe, "OpenPullRequestsOnBase")
}

// collectPullRequests reads every row of a pull request listing, naming the listing in any failure
// as describe, and marks each record with readBy, the read that made it (PullRequest.readBy).
func collectPullRequests(rows pgx.Rows, describe, readBy string) ([]PullRequest, error) {
	defer rows.Close()
	open := []PullRequest{}
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		pr.readBy = readBy
		open = append(open, *pr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return open, nil
}

// PullRequestsOf is the pull request of each of issues, by issue key, in one query whatever the
// number of issues: a caller that must name the pull request of every issue of a tree makes no
// read per issue (admit's tellStalledTrees), and the state's build reads every issue's in one
// (projection's Project). An issue the record holds no pull request for has no entry, and no
// issues at all reads nothing — the query is not made.
//
// It reads the state the record holds, whichever it is. OpenPullRequests, the other read of
// several of this table, answers the open ones alone and could not serve this caller: a tree
// that has stopped with its work already merged is exactly the tree that caller reports, and a
// merged pull request is the fact its controller most needs to read. It reads none of what each
// pull request remembers of its settled heads (PullRequest.SettledHeads). An error names how many
// issues the read was of and the first of them, not every key: the state's read asks for each
// issue of the project.
func (s *Postgres) PullRequestsOf(ctx context.Context, tx pgx.Tx, issues []string) (map[string]PullRequest, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	describe := "read the pull request of " + issues[0]
	if len(issues) > 1 {
		describe = fmt.Sprintf("read the pull requests of %d issues from %s", len(issues), issues[0])
	}
	rows, err := tx.Query(ctx, "select "+pullRequestColumns+" from pull_requests where issue = any($1)", issues)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	defer rows.Close()
	requests := make(map[string]PullRequest, len(issues))
	for rows.Next() {
		pr, err := scanPullRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		pr.readBy = "PullRequestsOf"
		requests[pr.Issue] = *pr
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return requests, nil
}

func (s *Postgres) PutPullRequest(ctx context.Context, tx pgx.Tx, pr PullRequest) error {
	if pr.readBy != "" {
		return fmt.Errorf("put pull request for %s: the record was read by %s, which loads none of what the pull request remembers of its settled heads; "+
			"only PullRequest, PullRequestByNumber and PullRequestByBranch load the fence memory a write keeps", pr.Issue, pr.readBy)
	}
	failing, err := json.Marshal(pr.Failing)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode failing checks: %w", pr.Issue, err)
	}
	cancelled, err := json.Marshal(append([]string{}, pr.Cancelled...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode cancelled checks: %w", pr.Issue, err)
	}
	checkRuns, err := json.Marshal(pr.CheckRuns)
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode check runs: %w", pr.Issue, err)
	}
	noteIssueWrite(ctx, pr.Issue)
	statuses, err := json.Marshal(append([]CommitStatus{}, pr.Statuses...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode commit statuses: %w", pr.Issue, err)
	}
	pushes, err := json.Marshal(append([]ClassifiedPush{}, pr.Pushes...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode pushes: %w", pr.Issue, err)
	}
	moves, err := json.Marshal(append([]Move{}, pr.Moves...))
	if err != nil {
		return fmt.Errorf("put pull request for %s: encode moves: %w", pr.Issue, err)
	}
	// No head run is null.
	var run []byte
	if pr.Run != nil {
		if run, err = json.Marshal(pr.Run); err != nil {
			return fmt.Errorf("put pull request for %s: encode the head run: %w", pr.Issue, err)
		}
	}
	// No first conflicting read and no ask to merge forward are null, as no head run is.
	var conflictRead, mergeForward []byte
	if pr.ConflictRead != nil {
		if conflictRead, err = json.Marshal(pr.ConflictRead); err != nil {
			return fmt.Errorf("put pull request for %s: encode the first conflicting read: %w", pr.Issue, err)
		}
	}
	if pr.MergeForward != nil {
		if mergeForward, err = json.Marshal(pr.MergeForward); err != nil {
			return fmt.Errorf("put pull request for %s: encode the ask to merge forward: %w", pr.Issue, err)
		}
	}
	// fix_attempts, blocked_attempts and head_counted are stopped columns: nothing counts a fix
	// attempt since dispatch://INFRA-956 R4, and nothing reads them at all since a CI result
	// stopped moving an issue (dispatch://INFRA-714/spec R1), so they are kept rather than
	// dropped so a rollback to the daemon before either still runs. Each is `not null` with no
	// default (migration 0004), so a new row is given its zero here and no write ever touches it
	// again; planned_red, required, required_workflows and required_workflows_head are stopped too
	// and need not be named at all, being nullable or defaulted (migrations 0013, 0028, 0029).
	_, err = tx.Exec(ctx, `insert into pull_requests (issue, repo, number, branch, base, base_moved, head_sha, head_updated_at,
		failing, cancelled, check_runs, generation, snapshot, statuses, pushes, head_moves,
		head_run, review_seen, review_seen_at, state, checked_head, mergeability, merge_state, reviews_requested, head_checks,
		null_mergeable_reads, null_mergeable_since, evidence_read_at, state_read_at, conflicting_head, merge_sha, conflict_read,
		merge_forward, fix_attempts, blocked_attempts, head_counted)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31, $32, $33, 0, 0, '')
		on conflict (issue) do update set repo = excluded.repo, number = excluded.number, branch = excluded.branch,
		base = excluded.base, base_moved = excluded.base_moved,
		head_sha = excluded.head_sha, head_updated_at = excluded.head_updated_at,
		failing = excluded.failing, cancelled = excluded.cancelled, check_runs = excluded.check_runs,
		generation = excluded.generation, snapshot = excluded.snapshot, statuses = excluded.statuses,
		pushes = excluded.pushes, head_moves = excluded.head_moves, head_run = excluded.head_run,
		review_seen = excluded.review_seen,
		review_seen_at = excluded.review_seen_at, state = excluded.state, checked_head = excluded.checked_head,
		mergeability = excluded.mergeability, merge_state = excluded.merge_state, reviews_requested = excluded.reviews_requested,
		head_checks = excluded.head_checks, null_mergeable_reads = excluded.null_mergeable_reads,
		null_mergeable_since = excluded.null_mergeable_since, evidence_read_at = excluded.evidence_read_at,
		state_read_at = excluded.state_read_at,
		conflicting_head = excluded.conflicting_head, merge_sha = excluded.merge_sha,
		conflict_read = excluded.conflict_read, merge_forward = excluded.merge_forward`,
		pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.Base, pr.BaseMoved, pr.HeadSHA, pr.HeadUpdatedAt,
		failing, cancelled, checkRuns,
		pr.Generation, pr.Snapshot, statuses, pushes, moves, run,
		pr.ReviewSeen.ID, pr.ReviewSeen.SubmittedAt, pr.State, pr.CheckedHead,
		pr.Mergeability, pr.MergeState, pr.ReviewsRequested, pr.HeadChecks, pr.NullMergeableReads, pr.NullMergeableSince, pr.EvidenceReadAt,
		pr.StateReadAt,
		pr.ConflictingHead, pr.MergeSHA, conflictRead, mergeForward,
	)
	if err != nil {
		return fmt.Errorf("put pull request for %s: %w", pr.Issue, err)
	}
	if err := putSettledHeads(ctx, tx, pr); err != nil {
		return fmt.Errorf("put pull request for %s: %w", pr.Issue, err)
	}
	return nil
}

func (s *Postgres) SetPullRequestBaseMoved(ctx context.Context, tx pgx.Tx, issue, after string) error {
	noteIssueWrite(ctx, issue)
	if _, err := tx.Exec(ctx, `update pull_requests set base_moved = $2 where issue = $1`, issue, after); err != nil {
		return fmt.Errorf("record that the base of %s's pull request moved to %s: %w", issue, after, err)
	}
	return nil
}

func (s *Postgres) SessionClaimsTree(ctx context.Context, tx pgx.Tx, tree, session string) (bool, error) {
	var claims bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from claims where tree = $1 and session = $2 and session <> '')", tree, session).Scan(&claims); err != nil {
		return false, fmt.Errorf("read the claims of session %s in %s: %w", session, tree, err)
	}
	return claims, nil
}

func (s *Postgres) ArchitectIssueOfSession(ctx context.Context, tx pgx.Tx, tree, session string) (string, bool, error) {
	var issue string
	err := tx.QueryRow(ctx, "select issue from claims where tree = $1 and session = $2 and role = $3 and session <> ''", tree, session, string(claim.RoleArchitect)).Scan(&issue)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the architect claim of session %s in %s: %w", session, tree, err)
	}
	return issue, true, nil
}

func (s *Postgres) SessionClaimsAnywhere(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	var claims bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from claims where session = $1 and session <> '')", session).Scan(&claims); err != nil {
		return false, fmt.Errorf("read the claims of session %s: %w", session, err)
	}
	return claims, nil
}

// ProjectClaims reads the claims of one project as the quiet measure reads them, in one
// statement and no order: the five columns TreeClaim holds, for every claim of the project that
// stands on an issue. The two step columns are nullable — a claim whose agent has made no call
// has no step_at, one whose agent has begun no turn has no turn_started_at — and each reads back
// as the zero time, which moves no tree (QuietTrees).
//
// project is the claim project token, the spelling `claims.project` holds, which the caller
// converts to (the interface's own doc).
func (s *Postgres) ProjectClaims(ctx context.Context, tx pgx.Tx, project string) ([]TreeClaim, error) {
	rows, err := tx.Query(ctx, "select token, issue, state, step_at, turn_started_at from claims where project = $1 and issue <> ''", project)
	if err != nil {
		return nil, fmt.Errorf("read the claims of project %s: %w", project, err)
	}
	defer rows.Close()
	var claims []TreeClaim
	for rows.Next() {
		var (
			held                TreeClaim
			token, state        string
			stepAt, turnStarted *time.Time
		)
		if err := rows.Scan(&token, &held.Issue, &state, &stepAt, &turnStarted); err != nil {
			return nil, fmt.Errorf("read the claims of project %s: %w", project, err)
		}
		held.Token = claim.Token(token)
		held.State = supervise.ClaimState(state)
		if stepAt != nil {
			held.StepAt = *stepAt
		}
		if turnStarted != nil {
			held.TurnStartedAt = *turnStarted
		}
		claims = append(claims, held)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims of project %s: %w", project, err)
	}
	return claims, nil
}

// HolderClaims reads the claims of one project's slot holders as a slot holder's state reads
// them, in one statement and no order: for every claim of the project that stands on an issue
// holding a slot, its identity, its state and who suspended it, its two turn times, the queued
// time of the task it holds or else of its handshake's first queue, whether it holds a task, and
// when it stopped (HolderClaim): failed_at for a failed claim, stopped_at, which the claim store
// writes on the move into suspended or retired, for the other two. Every time column is nullable
// and reads back as the zero time, a time the record does not hold. The slots are read in the
// statement itself, so they are the ones the caller's transaction holds: both callers read their
// slots in that same transaction, with no write of a slot between (admit's tickRead, and the
// state route's read-only repeatable-read snapshot). The answer is never nil, a project no slot
// holder of which has a claim included, so a projection given it never falls back to the claims
// it holds whole (projection.Inputs.HolderClaims).
//
// project is the claim project token, the spelling `claims.project` holds, which the caller
// converts to (the interface's own doc).
func (s *Postgres) HolderClaims(ctx context.Context, tx pgx.Tx, project string) ([]HolderClaim, error) {
	rows, err := tx.Query(ctx, `select c.token, c.issue, c.role, c.tree, c.state, c.suspended_by, c.last_turn_start,
			c.last_turn_end, coalesce(d.queued_at, c.handshake_queued_at), d.claim_token is not null,
			case when c.state = 'failed' then c.failed_at else c.stopped_at end
		from claims c left join pending_task_deliveries d on d.claim_token = c.token
		where c.project = $1 and c.issue in (select issue from slots)`, project)
	if err != nil {
		return nil, fmt.Errorf("read the claims of project %s for its slot holders: %w", project, err)
	}
	defer rows.Close()
	claims := []HolderClaim{}
	for rows.Next() {
		var (
			held                                  HolderClaim
			token, role, state, by                string
			turnStart, turnEnd, queuedAt, stopped *time.Time
		)
		if err := rows.Scan(&token, &held.Issue, &role, &held.Tree, &state, &by, &turnStart, &turnEnd, &queuedAt, &held.Pending,
			&stopped); err != nil {
			return nil, fmt.Errorf("read the claims of project %s for its slot holders: %w", project, err)
		}
		held.Token, held.Role = claim.Token(token), claim.Role(role)
		held.State, held.SuspendedBy = supervise.ClaimState(state), supervise.SuspendSource(by)
		for _, at := range []struct {
			read *time.Time
			into *time.Time
		}{{turnStart, &held.LastTurnStart}, {turnEnd, &held.LastTurnEnd}, {queuedAt, &held.QueuedAt}, {stopped, &held.StoppedAt}} {
			if at.read != nil {
				*at.into = at.read.UTC()
			}
		}
		claims = append(claims, held)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims of project %s for its slot holders: %w", project, err)
	}
	return claims, nil
}

// keptCounts is the validity rule as SQL: the claim and the hold a kept row carries count only
// while they stand in the same tree as the row's own issue. row is the kept row's alias and owner
// the alias of the issues row of its issue, which is where the tree it is owed to is read. A void
// claim or hold is ignored, so a hand-over, an orphaning, an adoption or a grandchild's
// hand-over leaves nothing to move: the tree the row's issue stands in now names it.
func keptCounts(row, owner string) string {
	return `(` + row + `.claimed_by is null or not exists (
		select 1 from outbox claimer join issues claimed on claimed.key = claimer.issue
		where claimer.id = ` + row + `.claimed_by and claimed.tree = ` + owner + `.tree))
		and (` + row + `.held_by_claim is null or not exists (
		select 1 from claims holder join pending_task_deliveries held on held.claim_token = holder.token
		where holder.token = ` + row + `.held_by_claim and held.delivery_id = ` + row + `.held_by_delivery
		and held.confirmed_at is null and holder.tree = ` + owner + `.tree))`
}

// keptNameable and approvalNameable are keptCounts for a kept decision and for a readmitted
// approval, each read through the issues row of its own issue.
var (
	keptNameable     = keptCounts("kept", "owner")
	approvalNameable = keptCounts("readmitted_approvals", "owner")
)

func (s *Postgres) AddParkedDecision(ctx context.Context, tx pgx.Tx, decision ParkedDecision) error {
	if _, err := tx.Exec(ctx, `insert into parked_decisions (issue, ref, decision, actor, at) values ($1, $2, $3, $4, $5)
		on conflict (issue, ref) do update set decision = excluded.decision, actor = excluded.actor,
		at = excluded.at, claimed_by = null, held_by_claim = null, held_by_delivery = null
		where parked_decisions.at <= excluded.at`,
		decision.Issue, decision.Ref, string(decision.Decision), decision.Actor, decision.At); err != nil {
		return fmt.Errorf("keep the decision %s on %s: %w", decision.Ref, decision.Issue, err)
	}
	return nil
}

func (s *Postgres) ParkedDecisions(ctx context.Context, tx pgx.Tx, tree string) ([]ParkedDecision, error) {
	return parkedDecisions(ctx, tx, tree, false)
}

// UnclaimedKept locks every kept row of tree, and tree's readmitted approval, before it reads which
// of them nothing claims: the composer that reads them claims what it names in the same
// transaction (ClaimKeptDecisions), so a second composer of the tree waits here until the first
// commits and then reads the first's claims. The lock is its own statement, so the read after it
// judges each claim against a snapshot that holds every claimer committed before the lock was
// granted. Without it the second composer would read a claim the first has not committed as no
// claim, name the decision too, and two tellings would name it.
func (s *Postgres) UnclaimedKept(ctx context.Context, tx pgx.Tx, tree string) ([]ParkedDecision, KeptApproval, error) {
	if _, err := tx.Exec(ctx, `select 1 from parked_decisions kept join issues owner on owner.key = kept.issue
		where owner.tree = $1 order by kept.issue, kept.ref for update of kept`, tree); err != nil {
		return nil, KeptApproval{}, fmt.Errorf("lock the decisions owed to %s: %w", tree, err)
	}
	if _, err := tx.Exec(ctx, `select 1 from readmitted_approvals where issue = $1 for update`, tree); err != nil {
		return nil, KeptApproval{}, fmt.Errorf("lock the approval kept for %s: %w", tree, err)
	}
	decisions, err := parkedDecisions(ctx, tx, tree, true)
	if err != nil {
		return nil, KeptApproval{}, err
	}
	var approval KeptApproval
	err = tx.QueryRow(ctx, `select step, approved_version, latest_version from readmitted_approvals
		join issues owner on owner.key = readmitted_approvals.issue
		where readmitted_approvals.issue = $1 and `+approvalNameable, tree).Scan(&approval.Step, &approval.Approved, &approval.Latest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, KeptApproval{}, fmt.Errorf("read the approval kept for %s: %w", tree, err)
	}
	return decisions, approval, nil
}

// parkedDecisions is every decision owed to tree — every row whose issue stands in tree now — or
// only those a start or catch-up of tree may name (keptCounts), newest first, ties by ref.
func parkedDecisions(ctx context.Context, tx pgx.Tx, tree string, nameable bool) ([]ParkedDecision, error) {
	rows, err := tx.Query(ctx, `select kept.issue, kept.ref, kept.decision, kept.actor, kept.at, coalesce(kept.claimed_by, 0),
		coalesce(kept.held_by_claim, ''), coalesce(kept.held_by_delivery, '')
		from parked_decisions kept join issues owner on owner.key = kept.issue
		where owner.tree = $1 and (not $2 or (`+keptNameable+`)) order by kept.at desc, kept.ref`, tree, nameable)
	if err != nil {
		return nil, fmt.Errorf("read the decisions owed to %s: %w", tree, err)
	}
	defer rows.Close()
	var decisions []ParkedDecision
	for rows.Next() {
		var decision ParkedDecision
		if err := rows.Scan(&decision.Issue, &decision.Ref, &decision.Decision, &decision.Actor, &decision.At,
			&decision.ClaimedBy, &decision.HeldBy.Claim, &decision.HeldBy.Delivery); err != nil {
			return nil, fmt.Errorf("scan a decision owed to %s: %w", tree, err)
		}
		decisions = append(decisions, decision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the decisions owed to %s: %w", tree, err)
	}
	return decisions, nil
}

func (s *Postgres) KeepReadmittedApproval(ctx context.Context, tx pgx.Tx, root string, approval KeptApproval) error {
	if _, err := tx.Exec(ctx, `insert into readmitted_approvals (issue, step, approved_version, latest_version) values ($1, $2, $3, $4)
		on conflict (issue) do update set step = excluded.step, approved_version = excluded.approved_version,
		latest_version = excluded.latest_version, claimed_by = null, held_by_claim = null, held_by_delivery = null`,
		root, string(approval.Step), approval.Approved, approval.Latest); err != nil {
		return fmt.Errorf("keep the approval of %s's %s step at version %d below version %d: %w",
			root, approval.Step, approval.Approved, approval.Latest, err)
	}
	return nil
}

func (s *Postgres) DropReadmittedApproval(ctx context.Context, tx pgx.Tx, root string) error {
	if _, err := tx.Exec(ctx, "delete from readmitted_approvals where issue = $1", root); err != nil {
		return fmt.Errorf("drop the approval kept for %s: %w", root, err)
	}
	return nil
}

// TellChildrenFirst records that the architect that owns issue has been told, for the run
// generation of tree, that the issue waits for its own Dispatch children, and reports whether
// this call is the telling: a second call for the same tree and generation changes nothing and
// answers false, so the one row decides who tells. That architect is the root's own for a root.
// For a child it is the architect above it, since the child's own starts only with that first
// phase, unless a person set the child back to todo while it ran or was held: that re-entry starts
// and stops nothing (workflow's ReenterChild), so a child's own architect that held its role still
// holds it and is the one told. The tree is part of the key because the generation is the root's
// and only names a run within one tree: an issue handed over as a root of its own waits in a tree
// of its own, at a generation of its own numbering (admit's readmit), and is told there again. at
// is the moment the pass ran, kept for a reader of the table alone.
func (s *Postgres) TellChildrenFirst(ctx context.Context, tx pgx.Tx, issue, tree string, generation uint64, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `insert into children_first_notices (issue, tree, generation, told_at) values ($1, $2, $3, $4)
		on conflict (issue, tree, generation) do nothing`, issue, tree, int64(generation), at)
	if err != nil {
		return false, fmt.Errorf("mark %s told it waits for its children: %w", issue, err)
	}
	return tag.RowsAffected() == 1, nil
}

// TellArchitectsStalled records, in one statement, that the controller has been told each of
// stalls, and answers at each index whether this call is the telling of that one: a mark that
// already stands, or one an earlier index of this call wrote, answers false, so the one row
// decides who tells. The arm is part of the key because each arm is a fact of its own and a root
// that two of them read must wake the controller twice. The tree is part of it for the reason
// TellChildrenFirst states, and each stall's At is the moment its pass read it, kept for a reader
// of the table alone.
func (s *Postgres) TellArchitectsStalled(ctx context.Context, tx pgx.Tx, stalls []ArchitectStall) ([]bool, error) {
	told := make([]bool, len(stalls))
	if len(stalls) == 0 {
		return told, nil
	}
	issues, trees, generations, arms := architectStallKeys(stalls)
	at := make([]time.Time, len(stalls))
	for i, stall := range stalls {
		at[i] = stall.At
	}
	rows, err := tx.Query(ctx, `insert into architect_stalled_notices (issue, tree, generation, arm, told_at)
		select * from unnest($1::text[], $2::text[], $3::bigint[], $4::text[], $5::timestamptz[])
		on conflict (issue, tree, generation, arm) do nothing
		returning issue, tree, generation, arm`, issues, trees, generations, arms, at)
	if err != nil {
		return nil, fmt.Errorf("mark the controller told of %d architect stalls: %w", len(stalls), err)
	}
	defer rows.Close()
	written := make(map[ArchitectStall]bool, len(stalls))
	for rows.Next() {
		var mark ArchitectStall
		var generation int64
		var arm string
		if err := rows.Scan(&mark.Issue, &mark.Tree, &generation, &arm); err != nil {
			return nil, fmt.Errorf("scan an architect stall marked told: %w", err)
		}
		mark.Generation, mark.Arm = uint64(generation), ArchitectStallArm(arm)
		written[mark] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mark the controller told of %d architect stalls: %w", len(stalls), err)
	}
	for i, stall := range stalls {
		key := ArchitectStall{Issue: stall.Issue, Tree: stall.Tree, Generation: stall.Generation, Arm: stall.Arm}
		told[i] = written[key]
		delete(written, key)
	}
	return told, nil
}

// ArchitectStallsUntold says, in one statement, whether any of stalls holds no mark yet.
func (s *Postgres) ArchitectStallsUntold(ctx context.Context, tx pgx.Tx, stalls []ArchitectStall) (bool, error) {
	if len(stalls) == 0 {
		return false, nil
	}
	issues, trees, generations, arms := architectStallKeys(stalls)
	var untold bool
	if err := tx.QueryRow(ctx, `select exists (
		select 1 from unnest($1::text[], $2::text[], $3::bigint[], $4::text[]) as w (issue, tree, generation, arm)
		where not exists (select 1 from architect_stalled_notices n
			where n.issue = w.issue and n.tree = w.tree and n.generation = w.generation and n.arm = w.arm))`,
		issues, trees, generations, arms).Scan(&untold); err != nil {
		return false, fmt.Errorf("read whether any of %d architect stalls is untold: %w", len(stalls), err)
	}
	return untold, nil
}

// architectStallKeys is the key of each of stalls' marks as four parallel arrays, for unnest.
func architectStallKeys(stalls []ArchitectStall) (issues, trees []string, generations []int64, arms []string) {
	issues = make([]string, len(stalls))
	trees = make([]string, len(stalls))
	generations = make([]int64, len(stalls))
	arms = make([]string, len(stalls))
	for i, stall := range stalls {
		issues[i], trees[i], generations[i], arms[i] = stall.Issue, stall.Tree, int64(stall.Generation), string(stall.Arm)
	}
	return issues, trees, generations, arms
}

func (s *Postgres) ReadmittedApproval(ctx context.Context, tx pgx.Tx, root string) (approved, latest int, err error) {
	err = tx.QueryRow(ctx, "select approved_version, latest_version from readmitted_approvals where issue = $1", root).Scan(&approved, &latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read the approval kept for %s: %w", root, err)
	}
	return approved, latest, nil
}

// keptMarks is kept's decisions as two parallel arrays, the reference and the time of each, which
// is what identifies the rows it names within one tree, for unnest. The reference spells the
// issue it was decided on and the document of that decision (workflow's stepRef, itemRef), so no
// two rows of one tree answer to one reference.
func keptMarks(kept KeptDecisions) ([]string, []time.Time) {
	refs := make([]string, 0, len(kept.Decisions))
	at := make([]time.Time, 0, len(kept.Decisions))
	for _, named := range kept.Decisions {
		refs = append(refs, named.Ref)
		at = append(at, named.At)
	}
	return refs, at
}

// ClaimKeptDecisions claims for outbox row what it names of what tree is owed and nothing else
// claims or holds (keptNameable). A row is claimed or held, never both: a hold the claim overrides
// is one keptNameable found void (its holder serves another tree than the row's issue now), and
// the claim clears it, so no later release of that hold can touch this claim.
//
// What stops a second composer from naming a decision another telling claims is UnclaimedKept's
// lock, which the composer takes before it reads what it names: each row it names is still there
// to claim when this runs. The count check below is defensive and does not catch that race on its
// own: without the lock, a second composer's UPDATE waits on the first's row lock and then
// re-checks the row, but keptNameable's subquery reads the statement's old snapshot, which does
// not see the first composer's outbox row, so it claims anyway and the count matches. The check
// catches a caller that names what it did not read under the lock, and rolls it back.
func (s *Postgres) ClaimKeptDecisions(ctx context.Context, tx pgx.Tx, tree string, kept KeptDecisions, row int64) error {
	if len(kept.Decisions) > 0 {
		refs, at := keptMarks(kept)
		claimed, err := tx.Exec(ctx, `update parked_decisions kept set claimed_by = $4, held_by_claim = null, held_by_delivery = null
			from unnest($2::text[], $3::timestamptz[]) as named (ref, at), issues owner
			where owner.key = kept.issue and owner.tree = $1 and kept.ref = named.ref
			and kept.at = named.at and `+keptNameable, tree, refs, at, row)
		if err != nil {
			return fmt.Errorf("claim the decisions named to %s for outbox row %d: %w", tree, row, err)
		}
		if claimed.RowsAffected() != int64(len(kept.Decisions)) {
			return fmt.Errorf("outbox row %d names %d decisions owed to %s and could claim %d: a row names only what it claims",
				row, len(kept.Decisions), tree, claimed.RowsAffected())
		}
	}
	if kept.Approved > 0 {
		claimed, err := tx.Exec(ctx, `update readmitted_approvals set claimed_by = $4, held_by_claim = null, held_by_delivery = null
			from issues owner
			where owner.key = readmitted_approvals.issue and readmitted_approvals.issue = $1
			and approved_version = $2 and latest_version = $3 and `+approvalNameable,
			tree, kept.Approved, kept.Latest, row)
		if err != nil {
			return fmt.Errorf("claim the approval named to %s for outbox row %d: %w", tree, row, err)
		}
		if claimed.RowsAffected() != 1 {
			return fmt.Errorf("outbox row %d names the approval kept for %s and could not claim it: a row names only what it claims", row, tree)
		}
	}
	return nil
}

func (s *Postgres) MoveKeptClaims(ctx context.Context, tx pgx.Tx, from, to int64) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := tx.Exec(ctx, "update "+table+" set claimed_by = $2 where claimed_by = $1", from, to); err != nil {
			return fmt.Errorf("move the claims of outbox row %d to %d: %w", from, to, err)
		}
	}
	return nil
}

func (s *Postgres) ReleaseKeptClaims(ctx context.Context, tx pgx.Tx, row int64) ([]string, error) {
	trees, err := releasedTrees(ctx, tx, `with decisions as (
		update parked_decisions set claimed_by = null where claimed_by = $1 returning issue
	), approvals as (
		update readmitted_approvals set claimed_by = null where claimed_by = $1 returning issue
	), released as (
		select issue from decisions union all select issue from approvals
	)`, row)
	if err != nil {
		return nil, fmt.Errorf("release the claims of outbox row %d: %w", row, err)
	}
	return trees, nil
}

func (s *Postgres) ReleaseHeldDecisions(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string,
	marks []DecisionMark) ([]string, error) {
	if len(marks) == 0 {
		return nil, nil
	}
	refs := make([]string, 0, len(marks))
	for _, mark := range marks {
		refs = append(refs, mark.Ref)
	}
	trees, err := releasedTrees(ctx, tx, `with released as (
		update parked_decisions set held_by_claim = null, held_by_delivery = null
		where held_by_claim = $1 and held_by_delivery = $3 and ref = any($2::text[]) returning issue
	)`, string(token), refs, delivery)
	if err != nil {
		return nil, fmt.Errorf("release what the task %s held for %s names of another tree: %w", delivery, token, err)
	}
	return trees, nil
}

func (s *Postgres) HeldKeptRefs(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string) ([]string, error) {
	rows, err := tx.Query(ctx, `select ref from parked_decisions where held_by_claim = $1 and held_by_delivery = $2`,
		string(token), delivery)
	if err != nil {
		return nil, fmt.Errorf("read what the delivery %s on %s holds: %w", delivery, token, err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan what the delivery %s on %s holds: %w", delivery, token, err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read what the delivery %s on %s holds: %w", delivery, token, err)
	}
	return refs, nil
}

// releasedTrees runs a release whose common table expressions return, under the name released, the
// issue of every row it released, and reports the tree each of those issues stands in now, by key:
// the trees whose architects the release must write a fresh catch-up to (workflow.HandOverKept).
func releasedTrees(ctx context.Context, tx pgx.Tx, release string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, release+`
		select distinct owner.tree from issues owner
		where owner.key in (select issue from released) order by owner.tree`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var trees []string
	for rows.Next() {
		var tree string
		if err := rows.Scan(&tree); err != nil {
			return nil, err
		}
		trees = append(trees, tree)
	}
	return trees, rows.Err()
}

func (s *Postgres) ArchitectLaunches(ctx context.Context, tx pgx.Tx, trees []string) (map[string]uint64, error) {
	if len(trees) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select tree, generation from claims
		where role = $1 and issue = tree and tree = any($2::text[])`, string(claim.RoleArchitect), trees)
	if err != nil {
		return nil, fmt.Errorf("read the launches of the architects of %v: %w", trees, err)
	}
	defer rows.Close()
	launches := map[string]uint64{}
	for rows.Next() {
		var tree string
		var generation int64
		if err := rows.Scan(&tree, &generation); err != nil {
			return nil, fmt.Errorf("scan the launch of an architect of %v: %w", trees, err)
		}
		launches[tree] = uint64(generation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the launches of the architects of %v: %w", trees, err)
	}
	return launches, nil
}

func (s *Postgres) ArchitectServed(ctx context.Context, tx pgx.Tx, tree string) (uint64, int64, error) {
	var served, entry int64
	err := tx.QueryRow(ctx, `select serving_generation, served_entry from claims
		where role = $1 and issue = $2 and tree = $2`, string(claim.RoleArchitect), tree).Scan(&served, &entry)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read what the architect of %s last served: %w", tree, err)
	}
	return uint64(served), entry, nil
}

// TreeMembersRun takes the workflow's statuses and the two phases from their one statement
// (OutOfWorkflowStatuses, InWorkflow's phase.Done, phase.Admitted) as parameters rather than
// spelling them into the select.
func (s *Postgres) TreeMembersRun(ctx context.Context, tx pgx.Tx, tree string) (bool, error) {
	var run bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issues
		where tree = $1 and key <> $1 and phase <> $2 and phase <> $3 and status <> all($4::text[]))`,
		tree, string(phase.Admitted), string(phase.Done), OutOfWorkflowStatuses).Scan(&run); err != nil {
		return false, fmt.Errorf("read whether a member of %s stands past admitted in the workflow: %w", tree, err)
	}
	return run, nil
}

func (s *Postgres) ForgetKeptDecisions(ctx context.Context, tx pgx.Tx, tree string, kept KeptDecisions, row int64) error {
	if len(kept.Decisions) > 0 {
		refs, at := keptMarks(kept)
		if _, err := tx.Exec(ctx, `delete from parked_decisions kept
			using unnest($2::text[], $3::timestamptz[]) as named (ref, at), issues owner
			where owner.key = kept.issue and owner.tree = $1 and kept.ref = named.ref
			and kept.at = named.at and (kept.claimed_by = $4 or kept.claimed_by is null)`,
			tree, refs, at, row); err != nil {
			return fmt.Errorf("tell the decisions named to %s: %w", tree, err)
		}
		// What the teller named of an issue that has left its tree since the row was composed is
		// not this architect's to be told: the row's claim goes instead, so the tree the issue
		// stands in now names it. The old architect hearing it as well is the lesser fault, and the
		// telling that reached it is one this row has already made. Only this row's own claim is
		// cleared: a row is claimed or held, never both, and no other claim or hold is this row's.
		if _, err := tx.Exec(ctx, `update parked_decisions kept set claimed_by = null
			from unnest($2::text[], $3::timestamptz[]) as named (ref, at), issues owner
			where owner.key = kept.issue and owner.tree <> $1 and kept.ref = named.ref
			and kept.at = named.at and kept.claimed_by = $4`, tree, refs, at, row); err != nil {
			return fmt.Errorf("leave the decisions named to %s that its tree no longer owes: %w", tree, err)
		}
	}
	if kept.Approved > 0 {
		if _, err := tx.Exec(ctx, `delete from readmitted_approvals using issues owner
			where owner.key = readmitted_approvals.issue and owner.tree = $1 and readmitted_approvals.issue = $1
			and approved_version = $2 and latest_version = $3 and (claimed_by = $4 or claimed_by is null)`,
			tree, kept.Approved, kept.Latest, row); err != nil {
			return fmt.Errorf("tell the approval named to %s: %w", tree, err)
		}
	}
	return nil
}

func (s *Postgres) ClaimSession(ctx context.Context, tx pgx.Tx, token claim.Token) (string, error) {
	var session string
	if err := tx.QueryRow(ctx, "select session from claims where token = $1", string(token)).Scan(&session); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read the session of claim %s: %w", token, err)
	}
	return session, nil
}

// ClearGeneration empties one issue's generation-scoped facts: a merged or closed pull request,
// the counters of one still open, its design gate, what its holders took of the approved design
// and its recorded handoffs. now is when the publication a cleared gate queues is due.
func (s *Postgres) ClearGeneration(ctx context.Context, tx pgx.Tx, issue string, now time.Time) error {
	return s.clearGeneration(ctx, tx, "issue = $1", "key = $1", issue, fmt.Sprintf("clear the generation of %s", issue), now)
}

// ClearTreeGeneration empties them for every issue of the tree. A root set back to todo starts a
// new generation of the whole tree, and its children carried the old one's pull request, gate and
// handoffs into it — a child re-entered at the new generation read a handoff of the last.
func (s *Postgres) ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string, now time.Time) error {
	return s.clearGeneration(ctx, tx, "issue in (select key from issues where tree = $1)", "tree = $1", tree,
		fmt.Sprintf("clear the generation of tree %s", tree), now)
}

// clearGeneration runs each statement with key as $1: where selects the cleared issues' rows by
// their issue column, and issues selects the same issues in the issues table. The design gates go
// with them, in a statement of their own (clearGates), and the rows of what each issue's holders
// took of those gates go beside them.
//
// A pull request that is not open goes, and its clock — the updated_at of the newest lifecycle
// event it applied — is kept in cleared_pull_requests at its repository and number, the latest of
// every clear's, with the issue whose row the latest clear deleted: GitHub can deliver an opened
// or a reopen of it after the clear, late or replayed from a message the intake gave up on,
// however late, and the workflow records such an event only when it is newer than that clock
// (workflow's pullRequestOpened, ClearedPullRequestClock). The row is never deleted: a pull
// request recorded again is fenced by its own row from then on. The one-time rename pass moves it
// to a configured rename's new name with the issue's other rows, and where it merges the moved row
// into one the new name already holds, that row names the moved row's issue
// (RenameIssueRepository).
func (s *Postgres) clearGeneration(ctx context.Context, tx pgx.Tx, where, issues, key, describe string, now time.Time) error {
	tag, err := tx.Exec(ctx, `with cleared as (
			delete from pull_requests where `+where+` and state <> 'open' returning issue, repo, number, head_updated_at)
		insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
		select distinct on (repo, number) repo, number, issue, head_updated_at, $2::timestamptz from cleared order by repo, number, head_updated_at desc
		on conflict (repo, number) do update set issue = excluded.issue,
			head_updated_at = greatest(cleared_pull_requests.head_updated_at, excluded.head_updated_at), cleared_at = excluded.cleared_at`,
		key, now)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	noteRowsWritten(ctx, tag)
	for _, statement := range []string{
		// An open pull request is kept across a new generation, but the last one's reading of it is
		// not: its checks settlement is of a head nobody has judged since, and the
		// review round's decision goes with the phases below, and so does its head run, whose
		// replay would put the old generation's judgement back. Its pushes stay, each a fact about
		// two commits, and so does its newest review, which orders every review it will have, and
		// the checks its base branch requires, which belong to the base, not the generation. So
		// does conflicting_head, the head the daemon last judged conflicting with its base: like
		// mergeability beside it, it is GitHub's answer about that head and that base, which a new
		// generation of the issue's work does not make false. A head that still does not merge
		// still does not merge, and the count it excuses starts again with the rounds below, so
		// nothing of the old generation's counting survives it. conflict_read, a first conflicting
		// read still waiting for its second (PullRequest.ConflictRead), stays for the same reason:
		// it is GitHub's answer, and the second read that confirms or releases it is still owed.
		// merge_forward goes (PullRequest.MergeForward): it is the old run's ask that a role
		// merge the base forward, and the new generation decides afresh, on its own confirmed
		// conflict, whether to ask again. What the pull request remembers of its settled heads
		// stays (PullRequest.SettledHeads, pull_request_settled_heads). Its head run's rows stay
		// too, until the next write of the pull request deletes them: a record with no run open
		// reads none of them (loadSettledHeads).
		"update pull_requests set " +
			"head_run = null, planned_red = false, checked_head = '', failing = '[]'::jsonb, cancelled = '[]'::jsonb, " +
			"check_runs = '[]'::jsonb, statuses = '[]'::jsonb, merge_forward = null where " + where,
		// The entry's task goes with the run, and the entry itself stays where it stands: the
		// number is never reset, so a worker still holding the old entry's task is behind the row
		// rather than level with it, and the first run of the re-admitted tree — its promotion, or
		// the admission pass that finds it running first — raises the entry again and writes the
		// task of that entry beside it (admit's startMidPhaseMembers). The task the role was last
		// given for an entry goes with it, with the entry and the generation it was given for
		// (PhaseRow.Given, GivenTask): it was the old run's. The generation alone would not retire
		// it on a member of a re-admitted tree, which keeps its own generation while its root takes
		// a new one. That the number is never reset is also what keeps a review of the old entry
		// from moving the reviewer's commit after that run begins: its footer names a number the
		// row has left (PhaseRow.ReviewedHead), and until then the row holds no entry's task. The
		// mark that the task is sent as written goes with the task it was about, and so does the
		// step the row ran (step_task, step_place), a step of the last generation's plan, whose
		// task ids the new generation's plan need not share. The time the entry was raised stays
		// with the number it belongs to, and so does the move that raised it (entry_moved_back),
		// which that same promotion writes again. The counts go with the rounds: the new generation
		// counts its moves back from nothing, and the cap mark goes with them, so the architect is
		// told again at the new generation's own first crossing of the cap. The commit the daemon
		// recorded for the reviewer's own review goes with the decision: the new generation's first
		// review reads the whole change.
		"update phases set handoff_commit = '', handoff_record = '', handoff_version = 0, rounds = 0, conflict_rounds = 0, " +
			"backward_moves = '{}'::jsonb, cap_told = 0, verdict = '', summary = '', decision = null, reviewed_head = '', " +
			"entry_task = '', entry_verbatim = false, entry_given_task = '', entry_given_entry = 0, entry_given_generation = 0, step_task = '', step_place = 0 where " + where,
		// What each holder took of the approved design goes with the gate it was taken from: the
		// next generation registers its gate again, and its work starts from what that gate
		// approves, which its first working phase takes as its baseline (workflow's Engine.enter),
		// and the root's architect row from its tree's first opening. A row kept across the clear
		// would hold the new generation's work to versions of a design its architect has written
		// again, and owe it news of changes it never stood on. A delivery's copy of what a prompt
		// carried (the claim store's pending_task_deliveries.carried_news) is left as it stands:
		// each item and opening of it is marked with the run it was rendered in, the generation of
		// its issue and of its tree's root (DeliveryNewsItem.Generation and TreeGeneration), and
		// the write beside this clear raises one of the two for every issue it covers (its callers:
		// workflow's ReenterChild and moveIntoTree raise the issue's own, admit's readmit the
		// root's). So that copy counts as a credit nowhere after the clear, neither in the read of
		// what an audience took nor in the fold of its turn's result (the SQL function
		// design_news_current), and a prompt of it whose turn starts after the clear confirms
		// nothing for the next run: the news stays owed to that run, which its own start tells.
		"delete from design_news_told where " + where,
	} {
		tag, err := tx.Exec(ctx, statement, key)
		if err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
		noteRowsWritten(ctx, tag)
	}
	return s.clearGates(ctx, tx, where, key, describe, now)
}

// clearGates deletes the design gates of every issue the clear covers and queues one publication
// of the empty set for each issue it emptied: those gates recorded a document until this
// statement, and Dispatch holds the set they recorded until something tells it otherwise. The
// delete names the issues it emptied rather than the clear naming every issue it covers, so an
// issue that held no gate row — the ordinary child of a re-entry, and every member of a
// re-admitted tree whose architect registered nothing — costs no Dispatch write. A row comes back
// for each step, so an issue gated on several steps is published once. The empty set carries no
// entry, so nothing in it says whether a row holds a tree (DesignGateDocument.Holds), and the
// clear needs none of the deployment's steps to build it.
//
// The publication is queued through this store, which knows nothing of the lanes beside it, so no
// lane is woken for it: the row waits on its issue's board lane for the next sweep, among the
// status writes the clearing pass queues with it.
func (s *Postgres) clearGates(ctx context.Context, tx pgx.Tx, where, key, describe string, now time.Time) error {
	rows, err := tx.Query(ctx, "delete from design_gates where "+where+" returning issue", key)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	emptied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if len(emptied) > 0 {
		noteRecordWrite(ctx)
	}
	slices.Sort(emptied)
	for _, issue := range slices.Compact(emptied) {
		if err := s.EnqueueDesignGates(ctx, tx, DesignGatesPublish{Issue: issue, Gates: GateDocuments(nil, nil)}, now); err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
	}
	return nil
}

func scanPullRequest(row scanner) (*PullRequest, error) {
	var pr PullRequest
	var failing, cancelled, checkRuns, statuses, pushes, moves, run, conflictRead, mergeForward []byte
	if err := row.Scan(&pr.Issue, &pr.Repo, &pr.Number, &pr.Branch, &pr.Base, &pr.BaseMoved, &pr.HeadSHA, &pr.HeadUpdatedAt,
		&failing, &cancelled, &checkRuns, &pr.Generation, &pr.Snapshot, &statuses,
		&pushes, &moves, &run, &pr.ReviewSeen.ID, &pr.ReviewSeen.SubmittedAt, &pr.State,
		&pr.CheckedHead, &pr.Mergeability, &pr.MergeState, &pr.ReviewsRequested, &pr.HeadChecks, &pr.NullMergeableReads,
		&pr.NullMergeableSince, &pr.EvidenceReadAt,
		&pr.StateReadAt, &pr.ConflictingHead, &pr.MergeSHA,
		&conflictRead, &mergeForward); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(failing, &pr.Failing); err != nil {
		return nil, fmt.Errorf("decode failing checks: %w", err)
	}
	if err := json.Unmarshal(cancelled, &pr.Cancelled); err != nil {
		return nil, fmt.Errorf("decode cancelled checks: %w", err)
	}
	if err := json.Unmarshal(checkRuns, &pr.CheckRuns); err != nil {
		return nil, fmt.Errorf("decode check runs: %w", err)
	}
	if err := json.Unmarshal(statuses, &pr.Statuses); err != nil {
		return nil, fmt.Errorf("decode commit statuses: %w", err)
	}
	if err := json.Unmarshal(pushes, &pr.Pushes); err != nil {
		return nil, fmt.Errorf("decode pushes: %w", err)
	}
	if err := json.Unmarshal(moves, &pr.Moves); err != nil {
		return nil, fmt.Errorf("decode moves: %w", err)
	}
	if run != nil {
		if err := json.Unmarshal(run, &pr.Run); err != nil {
			return nil, fmt.Errorf("decode the head run: %w", err)
		}
	}
	if conflictRead != nil {
		if err := json.Unmarshal(conflictRead, &pr.ConflictRead); err != nil {
			return nil, fmt.Errorf("decode the first conflicting read: %w", err)
		}
	}
	if mergeForward != nil {
		if err := json.Unmarshal(mergeForward, &pr.MergeForward); err != nil {
			return nil, fmt.Errorf("decode the ask to merge forward: %w", err)
		}
	}
	return &pr, nil
}

const productionCheckColumns = `issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at, role, looks, look_open,
	look_wake_at, last_wake_kind, last_wake_detail, last_wake_at, wake_at, last, closed_at, outcome, failed_looks`

func (s *Postgres) ProductionCheck(ctx context.Context, tx pgx.Tx, issue string) (*ProductionCheck, error) {
	check, err := scanProductionCheck(tx.QueryRow(ctx, "select "+productionCheckColumns+" from production_checks where issue = $1", issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read production check of %s: %w", issue, err)
	}
	return check, nil
}

func (s *Postgres) PutProductionCheck(ctx context.Context, tx pgx.Tx, check ProductionCheck) error {
	_, err := tx.Exec(ctx, `insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at,
		role, looks, look_open, look_wake_at, last_wake_kind, last_wake_detail, last_wake_at, wake_at,
		last, closed_at, outcome, failed_looks)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, nullif($19::text, ''), $20)
		on conflict (issue) do update set project = excluded.project, tree = excluded.tree, repo = excluded.repo,
		pr_number = excluded.pr_number, merge_sha = excluded.merge_sha, opened_at = excluded.opened_at, bound_at = excluded.bound_at,
		role = excluded.role, looks = excluded.looks, look_open = excluded.look_open, look_wake_at = excluded.look_wake_at,
		last_wake_kind = excluded.last_wake_kind, last_wake_detail = excluded.last_wake_detail, last_wake_at = excluded.last_wake_at,
		wake_at = excluded.wake_at, last = excluded.last, closed_at = excluded.closed_at, outcome = excluded.outcome,
		failed_looks = excluded.failed_looks`,
		check.Issue, check.Project, check.Tree, check.Repo, check.Number, check.MergeSHA, check.OpenedAt, check.BoundAt,
		string(check.Role), check.Looks, check.LookOpen, check.LookWakeAt, check.LastWakeKind, check.LastWakeDetail, check.LastWakeAt,
		check.WakeAt, check.Last, check.ClosedAt, string(check.Outcome), check.FailedLooks,
	)
	if err != nil {
		return fmt.Errorf("put production check of %s: %w", check.Issue, err)
	}
	return nil
}

func (s *Postgres) OpenProductionChecks(ctx context.Context, tx pgx.Tx) ([]ProductionCheck, error) {
	return openProductionChecks(ctx, tx, "", "list the open production checks")
}

func (s *Postgres) OpenProductionChecksOfTree(ctx context.Context, tx pgx.Tx, tree string) ([]ProductionCheck, error) {
	return openProductionChecks(ctx, tx, tree, fmt.Sprintf("list the open production checks of tree %s", tree))
}

// openProductionChecks is every open check, of tree when tree is not "", oldest opened first.
func openProductionChecks(ctx context.Context, tx pgx.Tx, tree, describe string) ([]ProductionCheck, error) {
	rows, err := tx.Query(ctx, "select "+productionCheckColumns+` from production_checks
		where closed_at is null and ($1::text = '' or tree = $1::text) order by opened_at, issue`, tree)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	defer rows.Close()
	open := []ProductionCheck{}
	for rows.Next() {
		check, err := scanProductionCheck(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		open = append(open, *check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return open, nil
}

func scanProductionCheck(row scanner) (*ProductionCheck, error) {
	var check ProductionCheck
	var outcome *string
	var role string
	if err := row.Scan(&check.Issue, &check.Project, &check.Tree, &check.Repo, &check.Number, &check.MergeSHA, &check.OpenedAt, &check.BoundAt,
		&role, &check.Looks, &check.LookOpen, &check.LookWakeAt, &check.LastWakeKind, &check.LastWakeDetail, &check.LastWakeAt,
		&check.WakeAt, &check.Last, &check.ClosedAt, &outcome, &check.FailedLooks); err != nil {
		return nil, err
	}
	check.Role = claim.Role(role)
	if outcome != nil {
		check.Outcome = CheckOutcome(*outcome)
	}
	return &check, nil
}

// markValues is mark as its two nullable columns, a sequence and a time, each null when unknown.
func markValues(mark EventMark) (seq, at any) {
	if mark.Seq > 0 {
		seq = mark.Seq
	}
	if !mark.At.IsZero() {
		at = mark.At
	}
	return seq, at
}

// scannedMark is the EventMark markValues stored.
func scannedMark(seq *int64, at *time.Time) EventMark {
	var mark EventMark
	if seq != nil {
		mark.Seq = *seq
	}
	if at != nil {
		mark.At = at.UTC()
	}
	return mark
}

// gateColumns reads the basis's text columns as "" where no basis stands, its served references
// as nil where it names none, and rests_on as nil for a step that rests on nothing.
const gateColumns = `issue, step, artifact_id, latest_version, approved_version, approved_seq, approved_at, told_changes_version, told_changes_reason, told_changes_seq, told_changes_at, ever_opened,
	coalesce(basis_issue, ''), coalesce(basis_artifact_id, ''), coalesce(basis_version, 0), basis_approved_seq, basis_approved_at, coalesce(basis_approver, ''),
	nullif(basis_serves, '{}'), coalesce(basis_reason, ''), coalesce(basis_session, ''), basis_registered_at, coalesce(basis_pending_ask, ''),
	kept_person_version, kept_person_seq, kept_person_at, nullif(rests_on, '{}'), basis_moves`

// scanGate reads a gate row in the shape PutGate writes: an approval's mark only with the approval,
// a told changes request's reason and mark only with its version, a basis only with its document,
// its approval only while it is not pending, and a kept person's approval only while a basis
// stands. An older daemon, which knows none of these columns, may clear a version and leave the
// rest, so the shape is the reader's and the writer's to keep rather than a constraint that would
// refuse that daemon's write: a basis such a daemon's write moved is read as ended
// (basisAsWritten).
func scanGate(row scanner) (DesignGate, error) {
	var gate DesignGate
	var approvedSeq, toldSeq, basisSeq, keptSeq *int64
	var approvedAt, toldAt, basisAt, registeredAt, keptAt *time.Time
	var keptVersion *int
	var step, pendingAsk string
	var basis GateBasis
	if err := row.Scan(&gate.Issue, &step, &gate.ArtifactID, &gate.LatestVersion, &gate.ApprovedVersion, &approvedSeq, &approvedAt,
		&gate.ToldChangesVersion, &gate.ToldChangesReason, &toldSeq, &toldAt, &gate.EverOpened,
		&basis.Issue, &basis.ArtifactID, &basis.Version, &basisSeq, &basisAt, &basis.Approver,
		&basis.Serves, &basis.Reason, &basis.Session, &registeredAt, &pendingAsk,
		&keptVersion, &keptSeq, &keptAt, &gate.RestsOn, &gate.BasisMoves); err != nil {
		return DesignGate{}, err
	}
	gate.Step = DesignStep(step)
	if gate.ApprovedVersion != nil {
		gate.ApprovedMark = scannedMark(approvedSeq, approvedAt)
	}
	if gate.ToldChangesVersion != nil {
		gate.ToldChangesMark = scannedMark(toldSeq, toldAt)
	} else {
		gate.ToldChangesReason = ""
	}
	if basis.ArtifactID != "" {
		if registeredAt != nil {
			basis.RegisteredAt = registeredAt.UTC()
		}
		if pendingAsk != "" {
			basis.Approver, basis.Pending = "", &PendingBasis{Ask: pendingAsk}
		} else {
			basis.Approval = scannedMark(basisSeq, basisAt)
		}
		gate.Basis = &basis
	}
	if gate.OnBasis() && keptVersion != nil {
		gate.KeptPerson = &PersonApproval{Version: *keptVersion, Mark: scannedMark(keptSeq, keptAt)}
	}
	return basisAsWritten(gate), nil
}

// GatesOf is every gate issue holds, by step. An issue with no gate gives an empty map.
func (s *Postgres) GatesOf(ctx context.Context, tx pgx.Tx, issue string) (map[DesignStep]DesignGate, error) {
	rows, err := tx.Query(ctx, `select `+gateColumns+` from design_gates where issue = $1 order by step`, issue)
	if err != nil {
		return nil, fmt.Errorf("read design gates for %s: %w", issue, err)
	}
	defer rows.Close()
	gates := map[DesignStep]DesignGate{}
	for rows.Next() {
		gate, err := scanGate(rows)
		if err != nil {
			return nil, fmt.Errorf("read design gates for %s: %w", issue, err)
		}
		gates[gate.Step] = gate
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read design gates for %s: %w", issue, err)
	}
	return gates, nil
}

// Gates is the design gates of each of issues, by issue and then step, in one read; an issue with
// no gate has no entry.
func (s *Postgres) Gates(ctx context.Context, tx pgx.Tx, issues []string) (map[string]map[DesignStep]DesignGate, error) {
	gates := make(map[string]map[DesignStep]DesignGate, len(issues))
	if len(issues) == 0 {
		return gates, nil
	}
	rows, err := tx.Query(ctx, `select `+gateColumns+` from design_gates where issue = any($1) order by issue, step`, issues)
	if err != nil {
		return nil, fmt.Errorf("read the design gates of %d issues: %w", len(issues), err)
	}
	defer rows.Close()
	for rows.Next() {
		gate, err := scanGate(rows)
		if err != nil {
			return nil, fmt.Errorf("read the design gates of %d issues: %w", len(issues), err)
		}
		if gates[gate.Issue] == nil {
			gates[gate.Issue] = map[DesignStep]DesignGate{}
		}
		gates[gate.Issue][gate.Step] = gate
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the design gates of %d issues: %w", len(issues), err)
	}
	return gates, nil
}

// GatedIssues is the key of each issue of project that holds at least one design gate and that
// Dispatch does not show as done, in key order. It is the boot's look over every recorded gate
// (the daemon's queueRecordedGatesPublish), which publishes the gates of these issues (Gates):
// two queries whatever the number of issues the project has recorded, rather than a read per
// record. The done ones are left out here rather than by the caller, since the rows nobody
// publishes are rows nobody need read, and the key is the whole of what the caller carries on
// with. The status read is dispatch_status, what Dispatch last showed (DoneStatus), rather than
// status, which is what the daemon means by the issue.
func (s *Postgres) GatedIssues(ctx context.Context, tx pgx.Tx, project string) ([]string, error) {
	rows, err := tx.Query(ctx, `select key from issues
		where project = $1 and dispatch_status <> 'done'
		and exists (select 1 from design_gates where design_gates.issue = issues.key)
		order by key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the gated issues of project %s: %w", project, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the gated issues of project %s: %w", project, err)
	}
	return keys, nil
}

// GatesForArtifact is every gate registered on artifactID, in issue and then step order: one row
// for each step of the document's owner that names it, and each copy a hand-over carried to a
// child of its tree. Each row carries its own step.
func (s *Postgres) GatesForArtifact(ctx context.Context, tx pgx.Tx, artifactID string) ([]DesignGate, error) {
	return gatesWhere(ctx, tx, "artifact_id = $1", artifactID, "for artifact "+artifactID)
}

// GatesForBasisArtifact is every gate whose basis names artifactID as the ancestor's document,
// standing or pending, of every tree, in issue and then step order. PutGate writes the basis
// columns only while a basis stands or waits, so an ended basis names no document here. The
// condition is the indexed prefilter, and the row as read decides, as GatesRestingOn's does: a row
// whose basis an earlier release moved still names the document in basis_artifact_id, but reads
// back with no basis (basisAsWritten), so it is no step on that document and is left out.
func (s *Postgres) GatesForBasisArtifact(ctx context.Context, tx pgx.Tx, artifactID string) ([]DesignGate, error) {
	gates, err := gatesWhere(ctx, tx, "basis_artifact_id = $1", artifactID, "on the ancestor's document "+artifactID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(gates, func(gate DesignGate) bool { return gate.Basis == nil }), nil
}

// GatesByPendingAsk is every gate pending on ask, the ancestor's open approval request, of every
// tree, in issue and then step order.
func (s *Postgres) GatesByPendingAsk(ctx context.Context, tx pgx.Tx, ask string) ([]DesignGate, error) {
	return gatesWhere(ctx, tx, "basis_pending_ask = $1", ask, "pending on request "+ask)
}

// GatesRestingOn is every gate that rests on one of ancestors in this generation
// (DesignGate.RestsOnIssue), of every tree, in issue and then step order: rests_on names each
// ancestor a basis opened the step on, and basis_issue the ancestor of the basis the step stands
// open on or waits on now. The condition is the indexed prefilter, and the rule over each row as
// read decides: a pending row an earlier release's write of a person's approval opened still
// names its ancestor in basis_issue, but reads back with no basis (basisAsWritten), and so rests
// on nothing.
func (s *Postgres) GatesRestingOn(ctx context.Context, tx pgx.Tx, ancestors ...string) ([]DesignGate, error) {
	gates, err := gatesWhere(ctx, tx, "rests_on && $1::text[] or basis_issue = any($1)", ancestors, "resting on "+strings.Join(ancestors, ", "))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(gates, func(gate DesignGate) bool { return !slices.ContainsFunc(ancestors, gate.RestsOnIssue) }), nil
}

// gatesWhere is every gate the condition selects with arg as its one parameter, in issue and then
// step order, nil for none; what names them in an error.
func gatesWhere(ctx context.Context, tx pgx.Tx, condition string, arg any, what string) ([]DesignGate, error) {
	rows, err := tx.Query(ctx, `select `+gateColumns+` from design_gates where `+condition+` order by issue, step`, arg)
	if err != nil {
		return nil, fmt.Errorf("list design gates %s: %w", what, err)
	}
	defer rows.Close()
	var gates []DesignGate
	for rows.Next() {
		gate, err := scanGate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan design gate %s: %w", what, err)
		}
		gates = append(gates, gate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list design gates %s: %w", what, err)
	}
	return gates, nil
}

// PutGate writes the gate of one step of one issue, which is the record's key, keeping the row's
// shape itself: an approval's mark is written only with the approval, a told changes request's
// reason and mark only with its version, a basis's approval only while it is not pending, and a
// kept person's approval only while a basis stands (scanGate). A gate with no basis writes every
// basis column null, which is how a basis that ended leaves the row. It is the single writer of
// every gate change, so it is also the only place ever_opened can rise.
//
// A gate built with no step is written as DesignStepSpec (DesignStep.OrSpec, the one place that
// rule is written). Go names no missing field at build time, so a caller that forgets the step
// would otherwise be found only by the stored column's check, at run time, in whatever deployment
// reached that path first. A basis that names no ancestor, document, version or registration time,
// or a pending one that names no request, is refused for the same reason: the readers that find a
// step by its basis (GatesForBasisArtifact, GatesByPendingAsk, GatesRestingOn) and the waiting
// line's moment (TreeGate.OpenedAt) read each.
func (s *Postgres) PutGate(ctx context.Context, tx pgx.Tx, gate DesignGate) error {
	step := gate.Step.OrSpec()
	approvedSeq, approvedAt := markValues(gate.ApprovedMark)
	if gate.ApprovedVersion == nil {
		approvedSeq, approvedAt = nil, nil
	}
	toldSeq, toldAt := markValues(gate.ToldChangesMark)
	toldReason := gate.ToldChangesReason
	if gate.ToldChangesVersion == nil {
		toldSeq, toldAt, toldReason = nil, nil, ""
	}
	var basisIssue, basisArtifact, basisVersion, basisSeq, basisAt, basisApprover, basisReason, basisSession, registeredAt, pendingAsk any
	var basisServes []string
	if basis := gate.Basis; basis != nil {
		switch {
		case basis.Issue == "" || basis.ArtifactID == "" || basis.Version <= 0:
			return fmt.Errorf("put design gate for %s: a basis names its ancestor, the ancestor's document and a version of it", gate.Issue)
		case basis.RegisteredAt.IsZero():
			return fmt.Errorf("put design gate for %s: a basis names the daemon's own time of its registration", gate.Issue)
		case basis.Pending != nil && basis.Pending.Ask == "":
			return fmt.Errorf("put design gate for %s: a pending basis names the approval request it waits on", gate.Issue)
		}
		basisIssue, basisArtifact, basisVersion = basis.Issue, basis.ArtifactID, basis.Version
		basisServes, basisReason, basisSession, registeredAt = basis.Serves, basis.Reason, basis.Session, basis.RegisteredAt
		if basis.Pending != nil {
			pendingAsk = basis.Pending.Ask
		} else {
			basisSeq, basisAt = markValues(basis.Approval)
			basisApprover = basis.Approver
		}
	}
	var keptVersion, keptSeq, keptAt any
	if kept := gate.KeptPerson; kept != nil && gate.OnBasis() {
		keptVersion = kept.Version
		keptSeq, keptAt = markValues(kept.Mark)
	}
	// ever_opened only ever rises: the write ORs what the row holds with what this gate opens with,
	// because a changes request's write carries a gate whose approval it has just cleared and would
	// otherwise put out a mark a past approval lit. What counts as opened is the one statement of
	// it (GateOpen), which classify.DesignGateOpen calls too, and which reads a step a basis opened
	// as open. rests_on only ever grows, for the same reason: each ancestor a basis opened the step
	// on is rested on until a re-admission deletes the row (DesignGate.RestsOn), and a registration
	// builds the step afresh, carrying the one ancestor it opens the step on. So the write keeps
	// every ancestor the row names and adds those the gate names that it does not, in the order
	// each was first written, and a write that carries none takes nothing away.
	//
	// basis_moves is the write's own count of the basis moving (DesignGate.BasisMoves): a new
	// row starts at one where it carries a basis and at zero where it does not, and a write over a
	// row adds one where the basis it writes differs from the row's in its ancestor, document,
	// version or request, which counts a basis set on a row that had none and the end of one, and
	// adds nothing otherwise. The gate's own value is never written.
	_, err := tx.Exec(ctx, `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, approved_seq, approved_at,
		told_changes_version, told_changes_reason, told_changes_seq, told_changes_at, ever_opened,
		basis_issue, basis_artifact_id, basis_version, basis_approved_seq, basis_approved_at, basis_approver, basis_serves,
		basis_reason, basis_session, basis_registered_at, basis_pending_ask, kept_person_version, kept_person_seq, kept_person_at, rests_on,
		basis_moves)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
		$13, $14, $15, $16, $17, $18, $19::text[], $20, $21, $22, $23, $24, $25, $26, nullif($27::text[], '{}'),
		case when $13::text is null then 0 else 1 end)
		on conflict (issue, step) do update set artifact_id = excluded.artifact_id,
		latest_version = excluded.latest_version, approved_version = excluded.approved_version,
		approved_seq = excluded.approved_seq, approved_at = excluded.approved_at,
		told_changes_version = excluded.told_changes_version, told_changes_reason = excluded.told_changes_reason,
		told_changes_seq = excluded.told_changes_seq, told_changes_at = excluded.told_changes_at,
		ever_opened = design_gates.ever_opened or excluded.ever_opened,
		basis_issue = excluded.basis_issue, basis_artifact_id = excluded.basis_artifact_id, basis_version = excluded.basis_version,
		basis_approved_seq = excluded.basis_approved_seq, basis_approved_at = excluded.basis_approved_at,
		basis_approver = excluded.basis_approver, basis_serves = excluded.basis_serves, basis_reason = excluded.basis_reason,
		basis_session = excluded.basis_session, basis_registered_at = excluded.basis_registered_at,
		basis_pending_ask = excluded.basis_pending_ask, kept_person_version = excluded.kept_person_version,
		kept_person_seq = excluded.kept_person_seq, kept_person_at = excluded.kept_person_at,
		rests_on = (select array_agg(ancestor order by first) from (
			select ancestor, min(place) as first
			from unnest(coalesce(design_gates.rests_on, '{}') || coalesce(excluded.rests_on, '{}')) with ordinality as named (ancestor, place)
			group by ancestor) as kept),
		basis_moves = design_gates.basis_moves + case
			when (design_gates.basis_issue, design_gates.basis_artifact_id, design_gates.basis_version, design_gates.basis_pending_ask)
				is distinct from (excluded.basis_issue, excluded.basis_artifact_id, excluded.basis_version, excluded.basis_pending_ask) then 1
			else 0 end`,
		gate.Issue, string(step), gate.ArtifactID, gate.LatestVersion, gate.ApprovedVersion, approvedSeq, approvedAt,
		gate.ToldChangesVersion, toldReason, toldSeq, toldAt,
		gate.EverOpened || GateOpen(gate),
		basisIssue, basisArtifact, basisVersion, basisSeq, basisAt, basisApprover, basisServes,
		basisReason, basisSession, registeredAt, pendingAsk, keptVersion, keptSeq, keptAt, gate.RestsOn)
	if err != nil {
		return fmt.Errorf("put design gate for %s: %w", gate.Issue, err)
	}
	noteRecordWrite(ctx)
	return nil
}

// A row keeps a decline as three columns and Go as one optional value (RootAsk.Decline): declined
// is the column that says there is one, and the note and the words are its two parts, each null
// where Dispatch recorded none. The scan below builds the value from the three, and the write
// takes it apart again.
const rootAskColumns = `issue, ask, coalesce(kind, ''), coalesce(artifact, ''), open, seq, coalesce(waiting_on, ''), waiting_on_seq,
	coalesce(closed_by, ''), closed_seq, by_tree, person_seq, person_answer_seq, declined, coalesce(declined_note, ''), coalesce(declined_words, '')`

func scanRootAsk(row scanner) (RootAsk, error) {
	var ask RootAsk
	var declined bool
	var decline AskDecline
	err := row.Scan(&ask.Issue, &ask.Ask, &ask.Kind, &ask.Artifact, &ask.Open, &ask.Seq, &ask.WaitingOn, &ask.WaitingOnSeq, &ask.ClosedBy,
		&ask.ClosedSeq, &ask.ByTree, &ask.PersonSeq, &ask.PersonAnswerSeq, &declined, &decline.Note, &decline.Words)
	if err == nil && declined {
		ask.Decline = &decline
	}
	return ask, err
}

func (s *Postgres) RootAsk(ctx context.Context, tx pgx.Tx, issue, ask string) (*RootAsk, error) {
	row, err := scanRootAsk(tx.QueryRow(ctx, `select `+rootAskColumns+` from root_asks where issue = $1 and ask = $2`, issue, ask))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ask %s of %s: %w", ask, issue, err)
	}
	return &row, nil
}

func (s *Postgres) PutRootAsk(ctx context.Context, tx pgx.Tx, ask RootAsk) error {
	declined, note, words := false, "", ""
	if ask.Decline != nil {
		declined, note, words = true, ask.Decline.Note, ask.Decline.Words
	}
	tag, err := tx.Exec(ctx, `insert into root_asks (issue, ask, kind, artifact, open, seq, waiting_on, waiting_on_seq, closed_by, closed_seq,
		by_tree, person_seq, person_answer_seq, declined, declined_note, declined_words)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		on conflict (issue, ask) do update set kind = excluded.kind, artifact = excluded.artifact, open = excluded.open, seq = excluded.seq,
		waiting_on = excluded.waiting_on, waiting_on_seq = excluded.waiting_on_seq, closed_by = excluded.closed_by,
		closed_seq = excluded.closed_seq, by_tree = excluded.by_tree,
		person_seq = excluded.person_seq, person_answer_seq = excluded.person_answer_seq,
		declined = excluded.declined, declined_note = excluded.declined_note, declined_words = excluded.declined_words
		where root_asks.seq <= excluded.seq and root_asks.waiting_on_seq <= excluded.waiting_on_seq
		and root_asks.closed_seq <= excluded.closed_seq and root_asks.person_seq <= excluded.person_seq
		and root_asks.person_answer_seq <= excluded.person_answer_seq`,
		ask.Issue, ask.Ask, nullableText(ask.Kind), nullableText(ask.Artifact), ask.Open, ask.Seq, nullableText(ask.WaitingOn), ask.WaitingOnSeq,
		nullableText(ask.ClosedBy), ask.ClosedSeq, ask.ByTree, ask.PersonSeq, ask.PersonAnswerSeq,
		declined, nullableText(note), nullableText(words))
	if err != nil {
		return fmt.Errorf("put ask %s of %s: %w", ask.Ask, ask.Issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) RootAsks(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]RootAsk, error) {
	asks := make(map[string][]RootAsk, len(issues))
	if len(issues) == 0 {
		return asks, nil
	}
	rows, err := tx.Query(ctx, `select `+rootAskColumns+` from root_asks where issue = any($1) order by issue, ask`, issues)
	if err != nil {
		return nil, fmt.Errorf("read the asks of %d issues: %w", len(issues), err)
	}
	defer rows.Close()
	for rows.Next() {
		ask, err := scanRootAsk(rows)
		if err != nil {
			return nil, fmt.Errorf("read the asks of %d issues: %w", len(issues), err)
		}
		asks[ask.Issue] = append(asks[ask.Issue], ask)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the asks of %d issues: %w", len(issues), err)
	}
	return asks, nil
}

func (s *Postgres) StartHandout(ctx context.Context, tx pgx.Tx, issue string) error {
	// A root recorded before the catch-up cursor existed gets one at its first handout from this
	// release (caught_up_seq 0, read from the handout), since this release took in everything up to it.
	tag, err := tx.Exec(ctx, `update issues i set declared_seq = null, declared_at = null, caught_up_seq = coalesce(i.caught_up_seq, 0),
		handout_seq = greatest(i.handout_seq, i.last_dispatch_seq,
		coalesce(i.awaiting_since_seq, 0),
		coalesce((select c.claim_seq from dispatch_claims c where c.key = i.key), 0),
		coalesce((select max(greatest(a.seq, a.waiting_on_seq, a.closed_seq, a.person_seq, a.person_answer_seq)) from root_asks a where a.issue = i.key), 0),
		-- max over the issue's gate rows, one for each step it gates: the handout takes in the
		-- newest review the record holds of any of them, and a scalar subquery of several rows
		-- would refuse the handout instead. A person's approval a basis holds apart
		-- (kept_person_seq) is one of them; the basis's own approval is a place in the
		-- ancestor's event log, not this issue's, and is no part of it.
		coalesce((select max(greatest(coalesce(g.approved_seq, 0), coalesce(g.told_changes_seq, 0), coalesce(g.kept_person_seq, 0))) from design_gates g where g.issue = i.key), 0))
		where i.key = $1`, issue)
	if err != nil {
		return fmt.Errorf("start a handout to %s: %w", issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) RestartCaughtUp(ctx context.Context, tx pgx.Tx, issue string) error {
	tag, err := tx.Exec(ctx, `update issues set caught_up_seq = 0 where key = $1`, issue)
	if err != nil {
		return fmt.Errorf("restart the catch-up cursor of %s: %w", issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

const assignmentColumns = `issue_key, experiment, unit, variant, mode, drawn_at, carried, plan`

// scanAssignment reads an assignment row with the issue it is keyed by, since the readers of many
// issues' rows group by that key and the reader of one issue's rows already knows it.
func scanAssignment(row scanner) (string, Assignment, error) {
	var issue string
	var assignment Assignment
	var drawnAt *time.Time
	if err := row.Scan(&issue, &assignment.Experiment, &assignment.Unit, &assignment.Variant,
		&assignment.Mode, &drawnAt, &assignment.Carried, &assignment.Plan); err != nil {
		return "", Assignment{}, err
	}
	if drawnAt != nil {
		utc := drawnAt.UTC()
		assignment.DrawnAt = &utc
	}
	return issue, assignment, nil
}

// PutAssignments records what issue's unit runs each of rows' experiments under, once: the insert
// does nothing where a row for that issue and experiment stands, so an experiment the unit has
// been running under keeps the arm, the mode, the instant and the plan it was drawn with, whatever
// the project's file says by now. Nothing here ever updates a row. It reports how many rows it
// wrote.
//
// The write is guarded by the issue's own record, which it selects from: a key the daemon holds no
// record for — an operator's spawn, whose tree's root the daemon never recorded — writes no row
// and raises nothing. Without the guard the row's foreign key would refuse such a write instead,
// and an operator's tree would fail its admission rather than quietly run under no recorded arm.
func (s *Postgres) PutAssignments(ctx context.Context, tx pgx.Tx, issue string, rows []Assignment) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	experiments := make([]string, len(rows))
	units := make([]string, len(rows))
	variants := make([]string, len(rows))
	modes := make([]string, len(rows))
	drawnAt := make([]*time.Time, len(rows))
	carried := make([]bool, len(rows))
	plans := make([]string, len(rows))
	for i, row := range rows {
		experiments[i], units[i] = row.Experiment, row.Unit
		variants[i], modes[i] = row.Variant, row.Mode
		drawnAt[i], carried[i] = row.DrawnAt, row.Carried
		plans[i] = row.Plan
	}
	tag, err := tx.Exec(ctx, `insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried, plan)
		select target.key, drawn.experiment, drawn.unit, drawn.variant, drawn.mode, drawn.drawn_at, drawn.carried, drawn.plan
		from issues target
		cross join unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::timestamptz[], $7::boolean[], $8::text[])
			as drawn(experiment, unit, variant, mode, drawn_at, carried, plan)
		where target.key = $1
		on conflict (issue_key, experiment) do nothing`,
		issue, experiments, units, variants, modes, drawnAt, carried, plans)
	if err != nil {
		return 0, fmt.Errorf("record %d experiment assignments of %s: %w", len(rows), issue, err)
	}
	return int(tag.RowsAffected()), nil
}

// CarryAssignments settles the assignments of key, a child handed over as a tree of its own, from
// the tree it left. The copy comes first: every tree-unit assignment row of from's root, onto key
// with its arm, its mode and its instant verbatim and the carried mark set, so the work that has
// been running under those arms goes on running under them. Then each experiment of settled — the
// experiments declared at the hand-over — is written at no variant, no mode and no instant, under
// the same write-once insert, so it lands only for an experiment the copy left unwritten: an
// experiment the old tree held a tree row for keeps the copy, and one it held none for is settled
// here at no variant and is never drawn for later. A row key already holds stands either way. It
// reports how many rows the two wrote.
//
// Only the tree rows are copied. A tree row is the whole tree's answer, which every issue of the
// tree is shown (internal/projection's assignmentViews) and which a launch of any of them reads
// off the root's key, so the child takes it on as the root of its own tree. An issue row of the
// old root is that one issue's own answer, drawn for it and read back under its own key alone: it
// says nothing about the child, which held no row of its own for that experiment and so ran in no
// arm of it before the hand-over. Copied, the child would run in the old root's arm afterwards
// and be marked as having carried it, where the settling below is what answers for it — at no
// variant, as for any experiment the tree it left held no row for.
//
// The driver row is the old tree's lifecycle row, under project, the normalized runtime token
// tree_lifecycles is keyed by (issues.project is the Dispatch key). A tree with no such row was
// never opened and hands nothing over, so key is drawn for at its own first open. The old tree's
// root record is not required — an operator opened a tree the daemon recorded no root for —
// since the copy reads the assignment rows themselves, of which such a tree has none, and the
// settled rows then answer for it. key's own record is required, as PutAssignments' target is: a
// child the daemon holds no record of takes no row and raises nothing.
func (s *Postgres) CarryAssignments(ctx context.Context, tx pgx.Tx, project, key, from string, settled []DeclaredExperiment) (int, error) {
	tag, err := tx.Exec(ctx, `insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
		select target.key, source.experiment, source.unit, source.variant, source.mode, source.drawn_at, true
		from tree_lifecycles life
		join issues target on target.key = $2
		join experiment_assignments source on source.issue_key = $3 and source.unit = 'tree'
		where life.project = $1 and life.tree = $3
		on conflict (issue_key, experiment) do nothing`, project, key, from)
	if err != nil {
		return 0, fmt.Errorf("carry the experiment assignments of %s to %s: %w", from, key, err)
	}
	written := int(tag.RowsAffected())
	if len(settled) == 0 {
		return written, nil
	}
	experiments := make([]string, len(settled))
	units := make([]string, len(settled))
	for i, row := range settled {
		experiments[i], units[i] = row.Experiment, row.Unit
	}
	tag, err = tx.Exec(ctx, `insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
		select target.key, declared.experiment, declared.unit, '', '', null::timestamptz, true
		from tree_lifecycles life
		join issues target on target.key = $2
		cross join unnest($4::text[], $5::text[]) as declared(experiment, unit)
		where life.project = $1 and life.tree = $3
		on conflict (issue_key, experiment) do nothing`, project, key, from, experiments, units)
	if err != nil {
		return 0, fmt.Errorf("settle %d experiments of %s at the hand-over out of %s: %w", len(settled), key, from, err)
	}
	return written + int(tag.RowsAffected()), nil
}

// AssignmentsOf is every assignment recorded for issue, in experiment order, so two reads of one
// unit agree. An issue with no assignment gives none. It is the one-unit read of AssignmentsFor,
// and no method of the Store interface: nothing the daemon runs reads one unit's rows alone — a
// launch reads a tree's and an issue's in one call, the state reads its project's rows — so the
// tests that check what one unit was recorded read the store itself.
func (s *Postgres) AssignmentsOf(ctx context.Context, tx pgx.Tx, issue string) ([]Assignment, error) {
	assignments, err := s.AssignmentsFor(ctx, tx, []string{issue})
	return assignments[issue], err
}

// AssignmentsFor is the assignments of each of issues, by issue and then experiment, in one read;
// an issue with no assignment has no entry, and an empty list reads nothing. It is the read of one
// or two keys a launch makes; a reader of every issue's answers asks Assignments.
func (s *Postgres) AssignmentsFor(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]Assignment, error) {
	if len(issues) == 0 {
		return map[string][]Assignment{}, nil
	}
	rows, err := tx.Query(ctx, `select `+assignmentColumns+` from experiment_assignments
		where issue_key = any($1) order by issue_key, experiment`, issues)
	return assignmentsByIssue(rows, err, fmt.Sprintf("%d issues", len(issues)), len(issues))
}

// Assignments is every experiment assignment the record holds for the issues of the Dispatch
// project, by issue and then experiment, in one read. The database is shared by every project's
// daemon (internal/daemon's workflow), and a row is keyed by an issue of it
// (experiment_assignments.issue_key references issues(key) on delete cascade), so the issue's own
// record is where its project is written and the join is what keeps one daemon's state out of
// another's rows. The state's projection asks this rather than AssignmentsFor over every issue it
// has in hand: the join selects the same rows and costs no parameter the size of the table.
func (s *Postgres) Assignments(ctx context.Context, tx pgx.Tx, project string) (map[string][]Assignment, error) {
	rows, err := tx.Query(ctx, `select `+assignmentColumns+` from experiment_assignments
		join issues on issues.key = experiment_assignments.issue_key
		where issues.project = $1
		order by issue_key, experiment`, project)
	return assignmentsByIssue(rows, err, "every issue of "+project, 0)
}

// assignmentsByIssue groups the rows of an assignment query under the issue each is keyed by,
// keeping the query's order within each issue. err is the query's own failure, read here so each
// reader above is one statement; what names the rows a failure is about, and size is the number
// of issues the caller expects.
func assignmentsByIssue(rows pgx.Rows, err error, what string, size int) (map[string][]Assignment, error) {
	if err != nil {
		return nil, fmt.Errorf("read the experiment assignments of %s: %w", what, err)
	}
	defer rows.Close()
	assignments := make(map[string][]Assignment, size)
	for rows.Next() {
		issue, assignment, err := scanAssignment(rows)
		if err != nil {
			return nil, fmt.Errorf("read the experiment assignments of %s: %w", what, err)
		}
		assignments[issue] = append(assignments[issue], assignment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the experiment assignments of %s: %w", what, err)
	}
	return assignments, nil
}

func (s *Postgres) Declare(ctx context.Context, tx pgx.Tx, issue string, mark EventMark) error {
	if mark.Seq <= 0 {
		return fmt.Errorf("declare a hand-back of %s: the hand-back has no sequence", issue)
	}
	var at any
	if !mark.At.IsZero() {
		at = mark.At
	}
	// The time is the newest hand-back's: an older one keeps the newer's, and the same hand-back
	// read again keeps the time it already had when this read gives none.
	tag, err := tx.Exec(ctx, `update issues set declared_seq = greatest(coalesce(declared_seq, $2), $2),
		declared_at = case when declared_seq is null or $2 > declared_seq then $3::timestamptz
			when $2 = declared_seq then coalesce($3::timestamptz, declared_at) else declared_at end
		where key = $1`, issue, mark.Seq, at)
	if err != nil {
		return fmt.Errorf("declare a hand-back of %s at %d: %w", issue, mark.Seq, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) TakeInThrough(ctx context.Context, tx pgx.Tx, issue string, seq int64) error {
	tag, err := tx.Exec(ctx, `update issues set handout_seq = greatest(handout_seq, $2),
		declared_at = case when declared_seq <= $2 then null else declared_at end,
		declared_seq = case when declared_seq <= $2 then null else declared_seq end
		where key = $1`, issue, seq)
	if err != nil {
		return fmt.Errorf("take in the hand-backs of %s through %d: %w", issue, seq, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) ClearDeclared(ctx context.Context, tx pgx.Tx, issue string) error {
	tag, err := tx.Exec(ctx, `update issues set declared_seq = null, declared_at = null where key = $1`, issue)
	if err != nil {
		return fmt.Errorf("clear the hand-back of %s: %w", issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) CaughtUpSeq(ctx context.Context, tx pgx.Tx, issue string) (int64, bool, error) {
	var seq *int64
	if err := tx.QueryRow(ctx, `select caught_up_seq from issues where key = $1`, issue).Scan(&seq); err != nil {
		return 0, false, fmt.Errorf("read the catch-up cursor of %s: %w", issue, err)
	}
	if seq == nil {
		return 0, false, nil
	}
	return *seq, true, nil
}

func (s *Postgres) SetCaughtUp(ctx context.Context, tx pgx.Tx, issue string, seq int64) error {
	if seq < 0 {
		return fmt.Errorf("set the catch-up cursor of %s: sequence %d is negative", issue, seq)
	}
	tag, err := tx.Exec(ctx, `update issues set caught_up_seq = greatest(coalesce(caught_up_seq, 0), $2) where key = $1`, issue, seq)
	if err != nil {
		return fmt.Errorf("set the catch-up cursor of %s at %d: %w", issue, seq, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// nullableText is text, or SQL null when it is empty.
func nullableText(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// Slots is every slot of the database, in slot order, each with the time its holder began to wait
// on another issue while it keeps the slot, where the pass marked one (Slot.WaitsSince, the row of
// slot_waits that goes with the slot's).
func (s *Postgres) Slots(ctx context.Context, tx pgx.Tx) ([]Slot, error) {
	rows, err := tx.Query(ctx, `select slots.issue, slots.index, slots.admitted_at, slot_waits.since from slots
		left join slot_waits on slot_waits.issue = slots.issue order by slots.index, slots.issue`)
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	defer rows.Close()
	slots := []Slot{}
	for rows.Next() {
		var slot Slot
		var waitsSince *time.Time
		if err := rows.Scan(&slot.Issue, &slot.Index, &slot.AdmittedAt, &waitsSince); err != nil {
			return nil, fmt.Errorf("list slots: %w", err)
		}
		if waitsSince != nil {
			utc := waitsSince.UTC()
			slot.WaitsSince = &utc
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	return slots, nil
}

// MarkSlotWait writes or deletes issue's row of slot_waits (Slot.WaitsSince): the insert keeps a
// row that stands, so a wait keeps the moment it began across every pass that reads it again, and
// a holder holding no slot has no row to write, since the row references the slot's and goes
// with it.
func (s *Postgres) MarkSlotWait(ctx context.Context, tx pgx.Tx, issue string, waits bool, at time.Time) error {
	if !waits {
		if _, err := tx.Exec(ctx, "delete from slot_waits where issue = $1", issue); err != nil {
			return fmt.Errorf("clear the wait of %s's slot holder: %w", issue, err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into slot_waits (issue, since) select issue, $2 from slots where issue = $1
		on conflict (issue) do nothing`, issue, at); err != nil {
		return fmt.Errorf("mark the wait of %s's slot holder begun at %s: %w", issue, at, err)
	}
	return nil
}

func (s *Postgres) PutSlot(ctx context.Context, tx pgx.Tx, slot Slot) error {
	tag, err := tx.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, $2, $3)
		on conflict (issue) do update set index = excluded.index, admitted_at = excluded.admitted_at`,
		slot.Issue, slot.Index, slot.AdmittedAt)
	if err != nil {
		return fmt.Errorf("put slot for %s: %w", slot.Issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error {
	tag, err := tx.Exec(ctx, "delete from slots where issue = $1", issue)
	if err != nil {
		return fmt.Errorf("release slot for %s: %w", issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// ReleaseSlotForWait deletes the row and writes the mark in one statement, because the two are
// one fact: the delete's returning feeds the update, so a transaction can neither take the row
// away without the mark nor mark an issue that kept its row. An issue holding no row is marked
// by neither, which is the truth about it — nothing released it for a wait.
func (s *Postgres) ReleaseSlotForWait(ctx context.Context, tx pgx.Tx, issue string, at time.Time) error {
	tag, err := tx.Exec(ctx, `with released as (delete from slots where issue = $1 returning issue)
		update issues set released_for_wait_at = $2 from released where issues.key = released.issue`, issue, at)
	if err != nil {
		return fmt.Errorf("release the slot of %s for what it waits on: %w", issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) ClearReleasedForWait(ctx context.Context, tx pgx.Tx, issue string) error {
	tag, err := tx.Exec(ctx, "update issues set released_for_wait_at = null where key = $1", issue)
	if err != nil {
		return fmt.Errorf("clear the release of %s's slot for what it waits on: %w", issue, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) ReleaseTreeSlots(ctx context.Context, tx pgx.Tx, tree string) ([]string, error) {
	tag, err := tx.Exec(ctx, "update issues set released_for_wait_at = null where tree = $1 and released_for_wait_at is not null", tree)
	if err != nil {
		return nil, fmt.Errorf("clear the releases for a wait of %s's tree: %w", tree, err)
	}
	noteRowsWritten(ctx, tag)
	rows, err := tx.Query(ctx, `with released as (delete from slots using issues
		where slots.issue = issues.key and issues.tree = $1 returning slots.issue, slots.index)
		select issue from released order by index, issue`, tree)
	if err != nil {
		return nil, fmt.Errorf("release the slots of %s's tree: %w", tree, err)
	}
	released, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("release the slots of %s's tree: %w", tree, err)
	}
	if len(released) > 0 {
		noteRecordWrite(ctx)
	}
	return released, nil
}

// OweMembers and ClearMembersOwed are the two writers of members_owed: PutIssue names the column
// nowhere, so a copy of the root read before either never writes it back. A key with no record
// matches no row and writes nothing.
func (s *Postgres) OweMembers(ctx context.Context, tx pgx.Tx, key string) error {
	tag, err := tx.Exec(ctx, "update issues set members_owed = true where key = $1", key)
	if err != nil {
		return fmt.Errorf("owe the members of %s their return: %w", key, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) ClearMembersOwed(ctx context.Context, tx pgx.Tx, key string) error {
	tag, err := tx.Exec(ctx, "update issues set members_owed = false where key = $1", key)
	if err != nil {
		return fmt.Errorf("clear the return owed to the members of %s: %w", key, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) ControllerRegistered(ctx context.Context, tx pgx.Tx, project string) (bool, error) {
	// The controllers row is keyed by the project token `legion controller start` mints under
	// (api/controller.go), and registered is controller.Record.Registered's rule: a session holds
	// the current capability.
	token, err := claim.ProjectToken(project)
	if err != nil {
		return false, err
	}
	var registered bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from controllers where project = $1 and session <> '')", token).Scan(&registered); err != nil {
		return false, fmt.Errorf("read whether %s has a registered controller: %w", project, err)
	}
	return registered, nil
}

// ControllerNoticePending reads the project's unpublished notices of this kind, narrowed to one
// issue when the caller named one and to one role of that issue when it named a role as well:
// the same read every way, with the issue and the role as two more terms that an empty string
// leaves out, so a project-wide wake's answer is what it always was.
func (s *Postgres) ControllerNoticePending(ctx context.Context, tx pgx.Tx, project string, kind NoticeKind, issue string, role claim.Role) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2 and payload->>'kind' = $3
		and ($4 = '' or issue = $4) and ($5 = '' or payload->>'role' = $5))`,
		string(OutboxKindControllerNotice), project, string(kind), issue, string(role)).Scan(&pending); err != nil {
		return false, fmt.Errorf("read whether %s has a %s controller notice pending: %w", project, kind, err)
	}
	return pending, nil
}

// ControllerNoticesPending reads that same row for many issues in one query: the statement above
// with the issue term taken as a list rather than one key, and with the project and the role
// terms exactly as they stand there, so each issue's answer is the answer the single read gives
// it and a caller asking of a hundred roots makes one round trip instead of a hundred. It names
// the issues a row was found for, which is the set a caller filtering its own roots asks for; an
// issue with no unpublished notice is simply absent, and no issues at all reads nothing — the
// query is not made.
//
// Distinct, because a kind that stands per claim can have a row per role of one issue
// (NoticePerClaim) and the caller asks about issues; a project-wide ask names no issue and is
// ControllerNoticePending's, which this does not serve.
func (s *Postgres) ControllerNoticesPending(ctx context.Context, tx pgx.Tx, project string, kind NoticeKind, issues []string, role claim.Role) (map[string]struct{}, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select distinct issue from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2 and payload->>'kind' = $3
		and issue = any($4) and ($5 = '' or payload->>'role' = $5)`,
		string(OutboxKindControllerNotice), project, string(kind), issues, string(role))
	if err != nil {
		return nil, fmt.Errorf("read which of %v have a %s controller notice pending: %w", issues, kind, err)
	}
	defer rows.Close()
	pending := make(map[string]struct{}, len(issues))
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			return nil, fmt.Errorf("read which of %v have a %s controller notice pending: %w", issues, kind, err)
		}
		pending[issue] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read which of %v have a %s controller notice pending: %w", issues, kind, err)
	}
	return pending, nil
}

func (s *Postgres) PersonHandoverPending(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	var pending bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from outbox where kind = $1 and issue = $2 and payload->>'kind' = $3)",
		string(OutboxKindDispatchStatus), issue, string(PersonHandoverStatusRow)).Scan(&pending); err != nil {
		return false, fmt.Errorf("read whether %s has a person's handover pending: %w", issue, err)
	}
	return pending, nil
}

// EnqueueGatedSteps writes the one gated_steps_publish row of publish's project, whose issue
// column is the bare project key: it replaces the payload of the row the outbox still holds for
// that project, and inserts one when it holds none. The unique partial index of
// 20261106041346_gated_steps_publish makes that rule the table's, so no path can leave a project
// with two, and the upsert is one statement, so a boot never reads a pending row and then queues
// beside it.
//
// A replacement is what keeps the queued list the booting daemon's own. The row waits on its
// lane until Dispatch takes it, and an operator can change gates.design and restart inside that
// window; a boot that stood down for the pending row would leave the list of the boot before it
// to be written. Replacing the payload keeps the row's attempts and its next_at, because the row
// is the same publish waiting on the same lane and only the list it carries is newer, and a
// payload that already says this list is left alone.
func (s *Postgres) EnqueueGatedSteps(ctx context.Context, tx pgx.Tx, publish GatedStepsPublish, nextAt time.Time) error {
	row, err := NewOutboxRow(publish.Project, publish, nextAt)
	if err != nil {
		return fmt.Errorf("enqueue the gated-steps publish of %s: %w", publish.Project, err)
	}
	if _, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, created_at)
		values ($1, $2, $3::jsonb, 0, $4, '', coalesce($5::timestamptz, now()))
		on conflict (issue) where kind = 'gated_steps_publish'
		do update set payload = excluded.payload where outbox.payload is distinct from excluded.payload`,
		string(row.Kind), row.Issue, string(row.Payload), row.NextAt, s.stamp()); err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, err)
	}
	return nil
}

// EnqueueDesignGates writes the one design_gates_publish row of publish's issue: it replaces the
// payload of the row the outbox still holds for that issue, and inserts one when it holds none.
// The unique partial index of 20261110235320_design_gates_publish makes that rule the table's, so
// no path can leave an issue with two, and the upsert is one statement, so two gate changes in
// flight at once never queue beside each other.
//
// A replacement is what keeps the queued set the latest one. The row waits on its issue's board
// lane until Dispatch takes it, and every gate change inside that window writes the whole set
// again; queueing beside the pending row would send Dispatch the older set after the newer one.
// Replacing the payload keeps the row's attempts and its next_at, because the row is the same
// publish waiting on the same lane and only the set it carries is newer, and a payload that
// already says this set is left alone.
//
// The lease rotation is what the project's gated-steps publish needs none of: that one is written
// at boot, before any lane claims a row, while an issue's gates change under a running daemon,
// whose board lane may hold this very row and be inside the PUT. That worker read the older set,
// so its finish, its retry and its payload replacement, each matched on its lease token, must
// touch nothing — a finish that deleted the row would leave Dispatch holding the older set until
// something changed a gate again. The row decides again on the newer payload once the lease
// expires. Enqueue's statement already carries readyMergeConflict's one `on conflict` clause and
// PostgreSQL takes one per statement, so this is a statement of its own.
func (s *Postgres) EnqueueDesignGates(ctx context.Context, tx pgx.Tx, publish DesignGatesPublish, nextAt time.Time) error {
	row, err := NewOutboxRow(publish.Issue, publish, nextAt)
	if err != nil {
		return fmt.Errorf("enqueue the design-gates publish of %s: %w", publish.Issue, err)
	}
	if _, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, created_at)
		values ($1, $2, $3::jsonb, 0, $4, '', coalesce($5::timestamptz, now()))
		on conflict (issue) where kind = 'design_gates_publish'
		do update set payload = excluded.payload,
			lease_token = case when outbox.lease_token = '' then '' else gen_random_uuid()::text end
		where outbox.payload is distinct from excluded.payload`,
		string(row.Kind), row.Issue, string(row.Payload), row.NextAt, s.stamp()); err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, err)
	}
	return nil
}

// roleHoldingStates and roleHoldingSources are the claim rows whose session holds its role, as
// pairs a statement reads together (claims.state, claims.suspended_by): each state and suspension
// source, the empty source of a claim that is not suspended among them, that supervise.HoldsRole
// answers true for. They are derived from that one rule over every state and source the supervise
// package declares, which are every value the claims table's checks allow, so no statement spells
// the rule again and a state or a source added there is read here with nothing added. So a claim
// the idle release or a pause's stop suspended (supervise.SuspendedByReleased) holds its role, and
// so does one launched again over the session it kept; a failed or a retired claim holds none, and
// neither does one any other source suspended.
var roleHoldingStates, roleHoldingSources = roleHoldingPairs()

func roleHoldingPairs() (states, sources []string) {
	bys := append([]supervise.SuspendSource{""}, supervise.SuspendSources()...)
	for _, state := range supervise.States() {
		for _, by := range bys {
			if supervise.HoldsRole(state, by) {
				states, sources = append(states, string(state)), append(sources, string(by))
			}
		}
	}
	return states, sources
}

// RoleHolder is one role of an issue and the session that holds it.
type RoleHolder struct {
	Role    claim.Role
	Session string
}

// RoleHolders is the session of each claim of issue that holds its role, by role: a claim whose
// state and suspension source hold it (supervise.HoldsRole, roleHoldingStates) and that records a
// session it holds. It is what a role-holder publication sends. A claim the idle release suspended
// is in it, so the release of an idle pod and the wake that brings it back leave Dispatch's set as
// it was.
//
// A claim holds its session when claim_sessions holds every name its record gives the session
// (supervise.RecordedHolds: the id, the file's name, the id an Oh My Pi file name gives, and the
// Claude Code session a `<uuid>.jsonl` file names) for it, in its own project (internal/store's
// HoldSessions, plan E's t-e-54), and none of those names is one no hold can cover
// (supervise.UnholdableNames). Every session a registration records is held first, and the boot
// holds every recorded name no row held before any machine exists. So a claim that holds its role
// is left out only where it records a session it does not hold, which the boot suspends: a
// name two claims recorded under a daemon from before the hold, or one past the bound, while that
// claim's suspension waits for the end of its turn, or after a suspension the runtime refused.
// Published, such a session would name the claim's role as the holder of another claim's session,
// or of one no claim holds. The names are the supervise package's one rule, read here rather than
// spelled again in SQL, and the holds of an issue's few claims are read in one statement.
func (s *Postgres) RoleHolders(ctx context.Context, tx pgx.Tx, issue string) ([]RoleHolder, error) {
	type candidate struct {
		token, project, file string
		holder               RoleHolder
	}
	rows, err := tx.Query(ctx, `select c.token, c.project, c.role, c.session, c.session_file from claims c
		where c.issue = $1 and c.session <> ''
			and (c.state, c.suspended_by) in (select * from unnest($2::text[], $3::text[]))
		order by c.role`, issue, roleHoldingStates, roleHoldingSources)
	if err != nil {
		return nil, fmt.Errorf("read the role holders of %s: %w", issue, err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.token, &c.project, &c.holder.Role, &c.holder.Session, &c.file)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the role holders of %s: %w", issue, err)
	}
	var claims, projects, kinds, names []string
	for _, c := range candidates {
		for _, h := range supervise.RecordedHolds(c.holder.Session, c.file) {
			claims, projects, kinds, names = append(claims, c.token), append(projects, c.project), append(kinds, string(h.Kind)), append(names, h.Name)
		}
	}
	held := map[string]map[supervise.Hold]bool{}
	if len(names) > 0 {
		rows, err := tx.Query(ctx, `select h.claim, h.kind, h.name from claim_sessions h
			join unnest($1::text[], $2::text[], $3::text[], $4::text[]) as want(claim, project, kind, name)
				on h.project = want.project and h.kind = want.kind and h.name = want.name and h.claim = want.claim`,
			claims, projects, kinds, names)
		if err != nil {
			return nil, fmt.Errorf("read the holds of %s's role holders: %w", issue, err)
		}
		for rows.Next() {
			var token, kind, name string
			if err := rows.Scan(&token, &kind, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read the holds of %s's role holders: %w", issue, err)
			}
			if held[token] == nil {
				held[token] = map[supervise.Hold]bool{}
			}
			held[token][supervise.Hold{Kind: supervise.HoldKind(kind), Name: name}] = true
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read the holds of %s's role holders: %w", issue, err)
		}
	}
	var holders []RoleHolder
	for _, c := range candidates {
		names := supervise.RecordedHolds(c.holder.Session, c.file)
		if len(supervise.UnholdableNames(c.holder.Session, c.file)) > 0 || len(names) == 0 {
			continue
		}
		holds := true
		for _, h := range names {
			holds = holds && held[c.token][h]
		}
		if holds {
			holders = append(holders, c.holder)
		}
	}
	return holders, nil
}

// SessionHeld reports whether claim_sessions holds, for token in project, every name its record
// gives session and sessionFile, by RoleHolders' rule: every name supervise.RecordedHolds
// gives, none of them one supervise.UnholdableNames refuses. It only reads, in tx, which may be a
// fact's transaction holding the fact lock (the daemon's step record): a savepoint, one select and
// the savepoint's release, all on that transaction's own connection, so it waits on no other
// connection and writes nothing. The savepoint is what makes a failure of the read leave tx usable;
// such a read answers false with the error.
func SessionHeld(ctx context.Context, tx pgx.Tx, project string, token claim.Token, session, sessionFile string) (bool, error) {
	names := supervise.RecordedHolds(session, sessionFile)
	if len(names) == 0 || len(supervise.UnholdableNames(session, sessionFile)) > 0 {
		return false, nil
	}
	var kinds, values []string
	distinct := map[supervise.Hold]bool{}
	for _, h := range names {
		if !distinct[h] {
			distinct[h] = true
			kinds, values = append(kinds, string(h.Kind)), append(values, h.Name)
		}
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("read whether %s holds session %s: %w", token, session, err)
	}
	defer sp.Rollback(ctx)
	var held int
	if err := sp.QueryRow(ctx, `select count(distinct (h.kind, h.name)) from claim_sessions h
		join unnest($3::text[], $4::text[]) as want(kind, name) on h.kind = want.kind and h.name = want.name
		where h.project = $1 and h.claim = $2`, project, string(token), kinds, values).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether %s holds session %s: %w", token, session, err)
	}
	if err := sp.Commit(ctx); err != nil {
		return false, fmt.Errorf("read whether %s holds session %s: %w", token, session, err)
	}
	return held == len(distinct), nil
}

// RecentClaimIssues is every issue of the Dispatch project whose key starts `<project>-` and
// that has a claim that holds its role (roleHoldingStates), or a claim that changed at or after
// since (claims.updated_at, which every claim write sets), in key order. The boot publishes each
// of them, for what a release that published nothing, or published another set, left: the ones
// whose role is held because a daemon that starts over their sessions has not published them yet,
// a claim the idle release suspended under a release that left it out of the set among them, and
// the recent ones because that release ended claims and published none of those ends.
func (s *Postgres) RecentClaimIssues(ctx context.Context, tx pgx.Tx, dispatchProject string, since time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `select distinct issue from claims
		where starts_with(issue, $1 || '-')
			and (updated_at >= $2 or (state, suspended_by) in (select * from unnest($3::text[], $4::text[])))
		order by issue`, dispatchProject, since, roleHoldingStates, roleHoldingSources)
	if err != nil {
		return nil, fmt.Errorf("read the issues of %s with a recent claim: %w", dispatchProject, err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the issues of %s with a recent claim: %w", dispatchProject, err)
	}
	return issues, nil
}

// QueuedMergeForwardAsk is Store.QueuedMergeForwardAsk: a delivered or ended row has left the
// outbox, so any merge-forward row of the ask's step, role and head still there is queued.
func (s *Postgres) QueuedMergeForwardAsk(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, head string) (bool, error) {
	var queued bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from outbox where kind = $1 and issue = $2
		and payload->>'step' = $3 and payload->>'role' = $4 and payload->>'head' = $5)`,
		string(OutboxKindMergeForward), issue, string(MergeForwardAsk), string(role), head).Scan(&queued); err != nil {
		return false, fmt.Errorf("read the queued merge-forward asks of %s: %w", issue, err)
	}
	return queued, nil
}

// outboxColumns is an outbox row as scanOutbox reads it. held_by_pause is read as 0 for the null
// of a row no pause held (OutboxRow.HeldByPause).
const outboxColumns = `id, kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until,
	coalesce(held_by_pause, 0) as held_by_pause`

// Enqueue writes row, created at row.CreatedAt where the caller sets it, and otherwise at the
// store's clock: a store built with one names created_at at that clock's time, and the daemon's
// own leaves it to the column's default, the database's now() (QueuedSupervise's Age says why the
// daemon keeps that clock).
func (s *Postgres) Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error {
	if !json.Valid(row.Payload) {
		return fmt.Errorf("enqueue %s for %s: payload is not JSON", row.Kind, row.Issue)
	}
	created := row.CreatedAt
	if created.IsZero() && s.clock != nil {
		created = s.clock()
	}
	var err error
	if created.IsZero() {
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			string(row.Kind), row.Issue, string(row.Payload), row.Attempts, row.NextAt, row.LastError, row.LeaseToken, row.LeaseUntil)
	} else {
		_, err = tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			string(row.Kind), row.Issue, string(row.Payload), row.Attempts, row.NextAt, row.LastError, created, row.LeaseToken, row.LeaseUntil)
	}
	if err != nil {
		return fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, err)
	}
	return nil
}

// EnqueueID writes row and answers its id. Its created_at is the store's clock (stamp), the
// column's default, now(), for the daemon's own store; row.CreatedAt is not read.
func (s *Postgres) EnqueueID(ctx context.Context, tx pgx.Tx, row OutboxRow) (int64, error) {
	if !json.Valid(row.Payload) {
		return 0, fmt.Errorf("enqueue %s for %s: payload is not JSON", row.Kind, row.Issue)
	}
	var id int64
	if err := tx.QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, coalesce($9::timestamptz, now())) returning id`, string(row.Kind), row.Issue,
		string(row.Payload), row.Attempts, row.NextAt, row.LastError, row.LeaseToken, row.LeaseUntil, s.stamp()).Scan(&id); err != nil {
		return 0, fmt.Errorf("enqueue %s for %s: %w", row.Kind, row.Issue, err)
	}
	return id, nil
}

// pendingPhaseStart is an issue's queued start of a phase worker: the supervise start row that
// names a phase and carries that phase's task, which workflow.StartOrOwe alone writes and
// refuses to write without either. $1 is the issue. It is the shape of the row an issue holds
// one of per wait, so it is what EnqueueStart takes out; the slot mark alone cannot do that work,
// since a start of accepting, retro or merging carries none (SlotRule.PhaseKeepsSlot).
//
// The issue's other supervise starts name no phase or carry no task — a root's architect's
// (admit's promotion), a child's own architect's plain start (workflow's gateOpened and
// reviveArchitect), the roles brought back beside a member's phase worker (admit's
// startMidPhaseMembers), a production check's look, a pod wake and the resume of a cancelled idle
// release — and each of them is a start of its own that must still run. None of them is ever
// taken out here.
const pendingPhaseStart = `issue = $1 and kind = 'supervise'
				and payload->>'op' = 'start' and payload ? 'task' and payload ? 'phase'`

// EnqueueStart writes a phase worker's start as a row of its own, in one statement that takes
// out the start the issue already owes its worker for the same wait — its oldest pending start
// of a phase worker (pendingPhaseStart) whose slot mark is the mark row carries and that no live
// lease holds — and writes row in its place, carrying that row's attempts, next_at, last_error
// and created_at. Where the issue owes no such row, row is written with its own values, which is
// the first start of a phase. So two transitions before the worker runs leave one start, the
// later one, and its caller (workflow.StartOrOwe) has one call to make either way.
//
// Two start rows of one issue the outbox cannot tell apart start that issue's worker twice for
// one piece of work, and that is what this keeps to one row. The outbox's fences end a row that
// serves a phase the issue has left, an older generation, or an entry a later entry of the same
// role has passed; what no fence reaches is two rows that serve the phase the issue stands in,
// at its own generation and at one entry, which is the pair the held phase's retry writes
// (workflow's restart sends the entry its row already names, marked ResendEntry) while the
// first start is still queued. While every start of a phase worker was marked for the slot, the
// mark could stand for the row's shape; a start of accepting, retro or merging carries none, so the
// shape is read from the row itself, and a retro's or a merger's retry is written over the
// queued row as an implementer's is.
//
// The mark is part of what the row must match, because a start that waits for a slot and one
// that waits for none are two different waits and neither may be taken out for the other. A
// start that waits for a slot takes its place in the waiting line from the row it replaces —
// the line is ordered by that row's created_at (OwedStart) — and a start of a phase past its
// review stands in no line at all (OwedStarts lists a marked row alone), so an issue would wait
// from the moment a row it never waited on was queued, ahead of issues that came to wait before
// it. The other way round, a start that waits for nothing is claimable at once, so taking out a
// row that is waiting would move the start ahead of anything its lane has still to run behind
// that row, a close's suspend among them (admit's promotion of a re-admitted tree queues its own
// start behind that suspend, so the worker comes back after the stop rather than before it). Two
// rows can stand together only where they were written for different phases — the mark is read
// from the phase (SlotRule.PhaseKeepsSlot) and every caller writes the phase on the record before
// it calls — so the issue stands in the newer row's phase and the outbox's phase fence finishes
// the older without acting, as admission's repair does where that row waits in the line.
//
// The row written carries the replaced row's created_at because the waiting line is ordered on
// created_at before id (OwedStart): the issue queued when its work first came back, and a second
// transition arriving while it waits is the same wait carrying a newer task, not a new turn at
// the back of the line. The earlier task is already dead whichever path wrote it — the
// transition raised the role's phase entry on its phases row in its own transaction, so a
// completion of the earlier entry is refused against the entry the record now names — so no
// information is lost with it.
//
// Which row is the oldest is read the same way, created_at before id, and not by id alone: a row
// written here carries a creation time from before its own id, so an issue owing two rows — one
// this replaced and one written behind a lease that has since lapsed — would otherwise have the
// younger of the two replaced next, and the oldest wait left carrying a task nothing would
// replace again.
//
// What the row written does not keep is its place among rows of the very same creation time,
// which the line breaks by id (Waiting): such a tie is two rows written in one transaction's
// now(), and the issue's own age, which is what the line is for, is unmoved.
//
// The id is the one thing it may not keep, and that is what this function is: one outbox row is
// one task. A claim gives its worker nothing from a row it has already queued a task from
// (supervise.Claim.DeliveredRows), so that no retry of a row prompts the same work twice, and
// that record is kept by the row alone. A row rewritten in place could therefore be one whose
// earlier payload had already reached the claim — RetryOutbox clears the lease for any failure
// that follows a delivery, and an unleased row is taken out like any other below — and the lane
// running it then recognised the row and dropped the later task, finishing clean. What that left
// was a phase with no task and nothing saying so: no error and no row behind it, a claim owing
// no task of the new work, so every completion its worker reported was refused, and no restart
// cured it, since the record a restart rebuilds the claim from carries that same row. Only a
// start under a row of its own did (dispatch://INFRA-2800).
//
// A row under a live lease is left where it is: its slot came, its lane has it, and the caller's
// row is the next start, written beside it. A lapsed lease is no lane holding anything — the
// outbox's own due predicate reads it as free (outboxUnleasedDue) and only a finished delivery
// takes the row away — so such a row is still the start this issue is owed and is taken out like
// any other. The lease is tested in the delete's own where clause and not only in the sub-select
// that picks the row: a lane claiming the row while this statement waits on its row lock would
// otherwise have the row taken from under it, and deliver the earlier task while the phases row
// already names the later entry. It is ClaimRow's own shape — one conditional write, never a
// read and then a write — because Postgres rechecks the quals against the row's new version once
// the lock is released, so the claim's lease is seen and nothing is deleted. ClaimRow's advisory
// lock is for the one rule of its own the row lock does not cover, a subquery over other rows;
// this statement's lease test is on the row it takes out, so it needs none. The moment the test
// is made against is the one the caller is acting at, which rides in on the row it hands over
// (NewOutboxRow's nextAt), so one transition reads one clock throughout.
//
// A row whose lease has lapsed is taken out with the rest, which is stronger than the rotated
// lease token a rewrite in place had to take: the lane that wrote that lease may be slow rather
// than dead, and every move it has left is matched on the row it holds — RenewOutboxLease renews
// by the id and the token, and FinishOutbox deletes by the id and the token — so with the row
// gone it renews nothing and finishes nothing. Rewritten in place, that lane could renew its
// lease, deliver the earlier task it had already read, and then finish the row now carrying the
// later one, which would delete the later task with nothing to re-place it: the issue's pending
// delivery holds the resume pass off (StillWorked) and every completion of it is refused.
//
// Every insert takes a fresh number from outbox_write_mark_seq through the column's own default.
// A row written over the one an issue owed keeps the created_at that holds its place in the
// line, so its created_at says when the issue first came to wait and nothing about when this
// payload was written; the write mark says the second, which is what the stop rule needs of it
// (admit's stopOutlivedStart, and store's putClaim for what the sequence orders). A row that
// carried the earlier row's mark would hand that rule a start written after an operator's stop
// under the number the row took before it, and the rule would end the start the retry had just
// written.
//
// Everything else of the replaced row rides over unchanged — its attempts, when it is next due
// and the error it last recorded — so a row backing off keeps its backoff and carries the later
// task into it.
//
// A partial unique index on the pending start would be the other way to keep one row, and is not
// available: Enqueue allows one `on conflict` target per statement and already spends it on
// ready_merge (readyMergeConflict).
func (s *Postgres) EnqueueStart(ctx context.Context, tx pgx.Tx, row OutboxRow) error {
	if !json.Valid(row.Payload) {
		return fmt.Errorf("enqueue the start of %s: payload is not JSON", row.Issue)
	}
	// The row taken out is one that waits for what row waits for, read from the payload the
	// caller hands over rather than asked of the caller again: a payload that names no mark
	// carries the mark's zero (SuperviseRequest.NeedsSlot is omitempty), which is a start that
	// waits for no slot. A first start is created at the store's clock (stamp), the database's
	// now() for the daemon's own store.
	if _, err := tx.Exec(ctx, `with owed as (
			delete from outbox where id = (select id from outbox
				where `+pendingPhaseStart+`
					and coalesce(payload->>'needsSlot', 'false') = coalesce(($2::jsonb)->>'needsSlot', 'false')
					and (lease_until is null or lease_until <= $3)
				order by created_at, id limit 1)
				and (lease_until is null or lease_until <= $3)
			returning attempts, next_at, last_error, created_at)
		insert into outbox (kind, issue, payload, attempts, next_at, last_error, created_at, lease_token, lease_until)
		select 'supervise', $1, $2, coalesce(owed.attempts, $4), coalesce(owed.next_at, $3), coalesce(owed.last_error, $5),
			coalesce(owed.created_at, $6::timestamptz, now()), '', null
		from (values (1)) as caller(one) left join owed on true`,
		row.Issue, string(row.Payload), row.NextAt, row.Attempts, row.LastError, s.stamp()); err != nil {
		return fmt.Errorf("enqueue the start of %s: %w", row.Issue, err)
	}
	return nil
}

// OwedStarts is every pending worker start of project's issues that waits for its issue's
// admission slot and that no live lease holds, oldest first by the pair the waiting line orders
// them on (OwedStart), with the write mark of the write that last set each row's payload, the
// role, phase and generation each row serves and the lease token it still carries. A row a lane
// holds now is left out: its slot came, so it is neither waiting in the line nor safe for
// admission's repair to finish. A row whose lease has lapsed is not that — the lane that held
// it is gone and only a finished delivery removes a row — so it is listed, exactly as the
// outbox's own due predicate lists it again (outboxUnleasedDue); left out, its issue would
// stand in no part of the line and nothing would ever deliver or repair it.
//
// The lapse is judged against the store's clock: the database's own for the daemon's store, the
// one moment a read with no caller's now can ask for, and a replay's simulated clock for a store
// built with one (NewStoreWithClock). The lane that wrote the lease and the rows' other readers
// compare against the daemon's clock, which the deployment keeps on the same time.
func (s *Postgres) OwedStarts(ctx context.Context, tx pgx.Tx, project string) ([]OwedStart, error) {
	rows, err := tx.Query(ctx, `select issue, created_at, id, write_mark, coalesce(payload->>'role', ''),
			coalesce(payload->>'phase', ''), coalesce((payload->>'generation')::bigint, 0), lease_token
		from outbox where kind = $1 and split_part(issue, '-', 1) = $2
			and payload->>'op' = 'start' and payload->>'needsSlot' = 'true'
			and (lease_until is null or lease_until <= coalesce($3::timestamptz, now()))
		order by created_at, id`, string(OutboxKindSupervise), project, s.stamp())
	if err != nil {
		return nil, fmt.Errorf("list the starts %s owes its workers: %w", project, err)
	}
	defer rows.Close()
	var owed []OwedStart
	for rows.Next() {
		var start OwedStart
		if err := rows.Scan(&start.Issue, &start.CreatedAt, &start.ID, &start.WriteMark, &start.Role, &start.Phase,
			&start.Generation, &start.LeaseToken); err != nil {
			return nil, fmt.Errorf("list the starts %s owes its workers: %w", project, err)
		}
		owed = append(owed, start)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the starts %s owes its workers: %w", project, err)
	}
	return owed, nil
}

// The outbox's runnable predicates are one SQL text, so the lanes' list of due rows (DueRows), their
// claim (ClaimRow), the state report's held rows (HeldRows, PauseHeldRows) and the resume's mark of
// the rows its pause held (MarkPauseHeldRows) can never disagree about which row may run. Each
// refers to the statement's first five parameters, which outboxDueArgs supplies — $1 is now, $2
// to $4 the kinds the ordering rules name, $5 the lifecycle lane's kinds — so a statement
// embedding one numbers its own parameters from $6; the pause rule (outboxPauseFree) refers to
// none. The outer table is `outbox`, unaliased, which the rules' subqueries name.

// outboxUnleasedDue is a row due at $1 that no live lease holds.
const outboxUnleasedDue = `outbox.next_at <= $1 and (outbox.lease_until is null or outbox.lease_until <= $1)`

// outboxInOrder is the three rules that hold a due row back behind something else of its issue. An
// issue's Dispatch status writes run one at a time in the order they were made: a status row waits
// while an older one for the same issue is unfinished, because each carries the status its
// predecessor leaves, and a newer write run first would find the board short of it and finish
// unwritten as though a human had moved it. A start of an issue's role waits while an older
// issue_branch row of the issue is unfinished, so no role starts before its issue's branch exists
// on GitHub (IssueBranch), and a create GitHub keeps refusing holds the issue's roles back while
// the row retries. And a start marked as waiting for the issue's admission slot
// (SuperviseRequest.NeedsSlot) waits while the slots table names no slot of that issue: the
// admission cap counts the issues the daemon has started, and this row is the delivery that puts one to work, so
// it is where the cap is applied. The store repeats this last rule (its slotWaitingStart, which
// TestTheIdleRuleAndTheOutboxAgreeOnAStartWaitingForItsSlot holds to this one), so the idle release
// keeps no pod for such a start; what the start means for a backstop is the store's
// PodRecords.WaitsForItsSlot's to say. Supervise rows need no other such rule: a stop names the
// run it ends through the claim's own record of the newest start run against it
// (claims.last_start_row), so ordering them here would buy nothing that survives a retry the
// runtime delayed.
//
// None of the three holds an issue's later rows back. A row that is merely unclaimable takes no
// lease, and the lane rule (outboxLaneFree) blocks only on a live lease, so an issue whose worker
// start waits for a slot still runs its close's suspend, its Dispatch status writes and its
// notices as they come due. That is what lets a transition apply at once and hold only the start.
const outboxInOrder = `not (outbox.kind = $2 and exists (select 1 from outbox older
			where older.kind = $2 and older.issue = outbox.issue and older.id < outbox.id))
		and not (outbox.kind = $3 and outbox.payload->>'op' = 'start' and exists (select 1 from outbox branch
			where branch.kind = $4 and branch.issue = outbox.issue and branch.id < outbox.id))
		and not (outbox.kind = $3 and outbox.payload->>'op' = 'start' and coalesce(outbox.payload->>'needsSlot', '') = 'true'
			and not exists (select 1 from slots where slots.issue = outbox.issue))`

// outboxLaneFree is the rule that a lifecycle row ($5 is the lifecycle lane's kinds) runs only while
// no other lifecycle row of its issue is under a live lease. Each lifecycle row reads the issue and
// its claims and then acts on what it read — a start launches the role the issue's phase names, a
// suspend stops the roles a start launched, a workspace removal takes the workspace they used — and
// they were written to run one after another, each in the state its predecessor leaves (OutboxLane).
// A lifecycle lane runs its rows that way on its own, but a lane is one daemon's: with the rule in
// SQL, two daemons of a rolling restart never run one issue's lifecycle rows side by side, since the
// second lists and claims nothing of the issue's while the first's lease is live. The state's
// held-rows read (HeldRows) is this rule negated, so what it shows held is exactly what the lanes
// hold back. Board rows are not under it: their lane runs them as they come due.
const outboxLaneFree = `not (outbox.kind = any($5::text[]) and exists (select 1 from outbox running
			where running.issue = outbox.issue and running.id <> outbox.id and running.kind = any($5::text[]) and running.lease_until > $1))`

// outboxPauseFree is the rule that a row a project's pause holds (pauseHolds) waits while an open
// pause names its project (decision 9 of dispatch://INFRA-3759). A row's project is the Dispatch
// key its issue column starts with, as DueRows scopes it, and project_pauses keys a pause by that
// same key, so a row of another project on the same database is never held. The rule is built
// once from pauseHolds with each kind and op written in as an SQL literal, so the one list is what
// holds the rows and no statement's parameters move. It reads:
//
//	not ((outbox.kind in ('notice', 'hook_prompt', …) or (outbox.kind = 'supervise' and outbox.payload->>'op' = 'start'))
//		and exists (select 1 from project_pauses pause
//			where pause.project = split_part(outbox.issue, '-', 1) and pause.ended_at is null))
//
// A held row takes no lease, so it keeps its place, spends no attempt and holds none of its
// issue's other rows back, as a start waiting for its slot does (outboxInOrder). A row a lane
// leased before the pause committed runs to its end; the pause's catch-up stops what it launches.
var outboxPauseFree = pauseFreeClause(pauseHolds)

// pauseFreeClause writes outboxPauseFree from the list of the rows a pause holds.
func pauseFreeClause(holds []pauseHold) string {
	var kinds, shapes []string
	for _, hold := range holds {
		if hold.op == "" {
			kinds = append(kinds, sqlText(string(hold.kind)))
			continue
		}
		shapes = append(shapes, `(outbox.kind = `+sqlText(string(hold.kind))+` and outbox.payload->>'op' = `+sqlText(string(hold.op))+`)`)
	}
	held := append([]string{`outbox.kind in (` + strings.Join(kinds, ", ") + `)`}, shapes...)
	return `not ((` + strings.Join(held, " or ") + `)
			and exists (select 1 from project_pauses pause
				where pause.project = split_part(outbox.issue, '-', 1) and pause.ended_at is null))`
}

// sqlText is s as an SQL string literal.
func sqlText(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// outboxClaimable is a row a lane may run now: due, under no live lease, in order, with no other
// lifecycle row of its issue running, and not held by its project's pause.
var outboxClaimable = outboxUnleasedDue + ` and ` + outboxInOrder + ` and ` + outboxLaneFree + ` and ` + outboxPauseFree

// lifecycleKindNames is the lifecycle lane's kinds as the outbox stores them, the $5 every
// statement embedding the predicates takes: the lane rule is the lifecycle lane's alone.
var lifecycleKindNames = kindNames(laneKinds[LaneLifecycle])

// outboxDueArgs is the parameters of a statement that embeds the predicates: now, the ordering
// rules' kinds and the lifecycle lane's kinds first, then the statement's own.
func outboxDueArgs(now time.Time, own ...any) []any {
	return append([]any{now, string(OutboxKindDispatchStatus), string(OutboxKindSupervise), string(OutboxKindIssueBranch), lifecycleKindNames}, own...)
}

// kindNames is kinds as the text the outbox stores them in, an empty list for none.
func kindNames(kinds []OutboxKind) []string {
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	return names
}

// DueRows lists the rows a lane may run at now (outboxClaimable), oldest first, leasing none: a
// lane reads what is due, then claims each by id (ClaimRow). Issue "" is every issue of project,
// kinds nil every kind. The database is shared by the daemons of several projects, and a row
// belongs to its issue's project: an issue key is `<project>-<n>`, and a project key has no hyphen
// (the configuration's projects keys, claim.IsIssueKey). A project-level row — the deployment's
// gated-steps publish — carries that bare project key in place of an issue, and the same
// split_part scopes it to its own daemon. The scope is in the select, and ClaimRow
// claims only an id a list gave it, because a lease taken on another project's row is what would
// take it from its own daemon.
func (s *Postgres) DueRows(ctx context.Context, tx pgx.Tx, project, issue string, kinds []OutboxKind, now time.Time) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, dueRowsSQL, outboxDueArgs(now, project, issue, kindNames(kinds))...)
	if err != nil {
		return nil, fmt.Errorf("list due outbox rows: %w", err)
	}
	defer rows.Close()
	due := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list due outbox rows: %w", err)
		}
		due = append(due, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due outbox rows: %w", err)
	}
	return due, nil
}

// dueRowsSQL is DueRows' statement, built once from the outbox's rules (outboxClaimable). Its
// project scope, `split_part(outbox.issue, '-', 1) = $6`, is the rule every write of a row by key
// takes from the other side (claim.KeyOfProject): a daemon writes no row keyed by another
// project's issue, because only that project's daemon runs it here.
var dueRowsSQL = `select ` + outboxColumns + ` from outbox
		where split_part(outbox.issue, '-', 1) = $6 and ($7::text = '' or outbox.issue = $7::text)
		and (cardinality($8::text[]) = 0 or outbox.kind = any($8::text[]))
		and ` + outboxClaimable + `
		order by next_at, id`

// ClaimRow leases row id for leaseFor from now, if a lane may still run it at now: it is in the
// outbox, due, under no live lease, in order, and no other lifecycle row of its issue is running
// (outboxClaimable). The claim is one conditional update, never a read and then a write, so two
// lanes that listed the same due row take it exactly once whichever commits first: the other's
// update waits on the row lock, then finds the lease taken, the row finished and gone, or an older
// row of its issue since queued ahead of it, and claims nothing. False is nothing claimed.
//
// The lane rule (outboxLaneFree) is the one predicate the row lock does not cover. Two daemons
// claiming different lifecycle rows of one issue at once update different rows, so neither waits
// on the other's row lock, and each update's subquery runs in its own READ COMMITTED snapshot,
// where the other's uncommitted lease is invisible: Postgres rechecks a concurrently updated row's
// new version against the where clause, but only the row being updated, never a subquery over
// other rows, so both claims would succeed and the issue would have two lifecycle rows running
// side by side. So a lifecycle claim is two statements: first a transaction-scoped advisory lock
// keyed by the row's issue, then the update. The second daemon blocks on the lock until the first
// commits, and its update then takes a fresh snapshot, in which the first's lease is live, and
// refuses. The lock statement is a select over the row, so a board row, or a row finished and gone,
// selects nothing and locks nothing: an issue's board lane never waits on its lifecycle claim. The
// lock is released at the claim transaction's commit, so it is held for the claim alone, never
// across an executor. intake.ApplyFact's advisory lock (hashtext('legion.apply_fact')) is a
// different key, and no transaction takes both — a lane claims in a transaction of its own, and no
// fact handler claims a row — so no lock ordering arises between them.
func (s *Postgres) ClaimRow(ctx context.Context, tx pgx.Tx, id int64, now time.Time, leaseFor time.Duration) (OutboxRow, bool, error) {
	if leaseFor <= 0 {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: lease duration must be positive", id)
	}
	token, err := leaseToken()
	if err != nil {
		return OutboxRow{}, false, err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('legion.outbox.lifecycle:' || issue)) from outbox
		where id = $1 and kind = any($2::text[])`, id, lifecycleKindNames); err != nil {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: %w", id, err)
	}
	// A catch-up's claim takes an advisory lock of its own, keyed by its issue, which DropCatchUps
	// takes too: that drop reads the leases of the issue's catch-ups in a subquery over other rows,
	// which the same recheck does not cover, so a claim committing beside it would be unseen and its
	// row deleted under it. Under the lock a claim either commits before the drop takes it, and the
	// drop's delete, a statement of its own, sees the lease, or it waits for the drop and finds the
	// row gone when the drop deleted it. A row of another kind, a notice that is no catch-up, and a
	// row finished and gone each select nothing and lock nothing.
	if _, err := tx.Exec(ctx, claimCatchUpLockSQL, id, string(OutboxKindNotice)); err != nil {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: %w", id, err)
	}
	row, err := scanOutbox(tx.QueryRow(ctx, claimRowSQL, outboxDueArgs(now, id, token, now.Add(leaseFor))...))
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboxRow{}, false, nil
	}
	if err != nil {
		return OutboxRow{}, false, fmt.Errorf("claim outbox row %d: %w", id, err)
	}
	return row, true, nil
}

// claimRowSQL is ClaimRow's update, built once from the outbox's rules (outboxClaimable).
var claimRowSQL = `update outbox set lease_token = $7, lease_until = $8
		where outbox.id = $6 and ` + outboxClaimable + `
		returning ` + outboxColumns

// catchUpLockKey is the key of the transaction-scoped advisory lock of an issue's catch-ups, for
// the issue the SQL expression issue names: the one lock a catch-up's claim (ClaimRow) and the
// drop of the issue's queued catch-ups (DropCatchUps, through LockCatchUps) take.
func catchUpLockKey(issue string) string {
	return "hashtext('legion.outbox.catch-up:' || " + issue + ")"
}

// claimCatchUpLockSQL is ClaimRow's take of the catch-up lock of row $1's issue, a select over the
// row, so a row that is no catch-up notice, or one finished and gone, locks nothing.
var claimCatchUpLockSQL = `select pg_advisory_xact_lock(` + catchUpLockKey("issue") + `) from outbox
		where id = $1 and kind = $2 and payload->>'kind' = 'catch-up'`

// lockCatchUpsSQL is LockCatchUps' take of the catch-up lock of the issue $1.
var lockCatchUpsSQL = `select pg_advisory_xact_lock(` + catchUpLockKey("$1::text") + `)`

// LockCatchUps takes the advisory lock of issue's catch-ups for the rest of tx, the lock a
// catch-up's claim (ClaimRow) and the drop of issue's queued catch-ups (DropCatchUps) take: while
// tx holds it, no catch-up of issue is claimed and none is dropped by another transaction.
func LockCatchUps(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, lockCatchUpsSQL, issue); err != nil {
		return fmt.Errorf("lock the claims of the catch-ups of %s: %w", issue, err)
	}
	return nil
}

// RenewOutboxLease moves the lease on row id to until while leaseToken still holds it, so a lane
// running a long row keeps it from the lapse that would let another claim it mid-run. renewed
// false is a lease no longer the caller's, and gone says whether that is because the row is
// finished rather than leased by another. gone is read by a statement of its own, after the
// update: an update that waited on the lock of a transaction finishing the row finds it deleted
// once that commits, which only a later statement's snapshot shows.
func (s *Postgres) RenewOutboxLease(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, until time.Time) (renewed, gone bool, err error) {
	tag, err := tx.Exec(ctx, "update outbox set lease_until = $3 where id = $1 and lease_token = $2", id, leaseToken, until)
	if err != nil {
		return false, false, fmt.Errorf("renew the lease of outbox row %d: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return true, false, nil
	}
	var present bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from outbox where id = $1)", id).Scan(&present); err != nil {
		return false, false, fmt.Errorf("read whether outbox row %d is finished: %w", id, err)
	}
	return false, !present, nil
}

// NextDue is the earliest moment after now one of project's rows of kinds comes due: the least
// next_at after now among the rows no live lease holds, so a lane with nothing to run sleeps until
// it rather than polls. A row due already is not it, nor one another lane is running whose lease
// outlasts its next_at. False is none; issue "" is every issue of project, as DueRows reads it,
// and kinds nil every kind. A lane asks it of its own issue; a replay of a recorded window, which
// runs every lane of its project in one stub, asks it of the whole project.
func (s *Postgres) NextDue(ctx context.Context, tx pgx.Tx, project, issue string, kinds []OutboxKind, now time.Time) (time.Time, bool, error) {
	var next *time.Time
	if err := tx.QueryRow(ctx, `select min(next_at) from outbox
		where split_part(issue, '-', 1) = $1 and ($2::text = '' or issue = $2::text)
		and (cardinality($3::text[]) = 0 or kind = any($3::text[]))
		and next_at > $4 and (lease_until is null or lease_until <= $4)`, project, issue, kindNames(kinds), now).Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("read when %s's outbox is next due: %w", cmp.Or(issue, project), err)
	}
	if next == nil {
		return time.Time{}, false, nil
	}
	return *next, true, nil
}

// BranchQueued says whether the outbox still holds an issue_branch row of issue — due, running or
// backing off — so the branch that row creates may not exist on GitHub yet. The ordering rule
// (outboxInOrder) holds a start behind an older branch row of its issue; this is the read for a
// start older than the branch row it must wait for (the outbox's first start of a child's own
// architect, which can outlast a re-admission that queues the child's branch row again).
func (s *Postgres) BranchQueued(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	var queued bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox where issue = $1 and kind = $2)`,
		issue, string(OutboxKindIssueBranch)).Scan(&queued); err != nil {
		return false, fmt.Errorf("read whether %s's branch row is still queued: %w", issue, err)
	}
	return queued, nil
}

// UnfinishedLanes is every lane of project's issues with a row still in the outbox — due, running,
// backing off or held — by issue then lane: the lanes the runner needs an executor for. The kinds
// left are read and sorted into lanes here (LaneOf), rather than mapped in SQL, so laneKinds stays
// the one place a kind's lane is written down however many lanes there are. A kind this binary has
// no lane for — a row a newer daemon of a rolling restart queued — is left out: this daemon has no
// executor for it either, and the daemon that wrote it runs it.
func (s *Postgres) UnfinishedLanes(ctx context.Context, tx pgx.Tx, project string) ([]LaneKey, error) {
	rows, err := tx.Query(ctx, `select distinct issue, kind from outbox where split_part(issue, '-', 1) = $1`, project)
	if err != nil {
		return nil, fmt.Errorf("list the unfinished outbox lanes of %s: %w", project, err)
	}
	defer rows.Close()
	lanes := []LaneKey{}
	seen := map[LaneKey]bool{}
	for rows.Next() {
		var issue, kind string
		if err := rows.Scan(&issue, &kind); err != nil {
			return nil, fmt.Errorf("list the unfinished outbox lanes of %s: %w", project, err)
		}
		key := LaneKey{Issue: issue, Lane: LaneOf(OutboxKind(kind))}
		if key.Lane == "" || seen[key] {
			continue
		}
		seen[key] = true
		lanes = append(lanes, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the unfinished outbox lanes of %s: %w", project, err)
	}
	slices.SortFunc(lanes, func(a, b LaneKey) int {
		return cmp.Or(strings.Compare(a.Issue, b.Issue), strings.Compare(string(a.Lane), string(b.Lane)))
	})
	return lanes, nil
}

// HeldRows is every due, unleased lifecycle row of project's issues a lane may not run at now
// (outboxClaimable negated) that has a lifecycle row of its issue running to wait on (outboxLaneFree
// negated), by next_at then id, each with that row: the oldest of its issue's running lifecycle
// rows. Issue "" is every issue of project. It is the lanes' own predicates read back, so what the
// state shows held is exactly what the lanes hold back. A start fenced behind an older issue_branch
// row of its issue is held while the branch row runs — issue_branch is a lifecycle kind, so the
// start waits on it — and listed by nothing while the branch row waits unleased for its retry, since
// it then waits on no running row, as it did before the lanes. Board rows are never held: their lane
// runs them as they come due. A row the lane rule holds fails the claim, so the claim's negation
// adds no row to the lane rule's; it is written out because Postgres types a parameter by its uses
// and refuses a statement with one it never uses, and the ordering rules' kinds are among the
// parameters every fragment shares.
func (s *Postgres) HeldRows(ctx context.Context, tx pgx.Tx, project, issue string, now time.Time) ([]HeldRow, error) {
	rows, err := tx.Query(ctx, heldRowsSQL, outboxDueArgs(now, project, issue)...)
	if err != nil {
		return nil, fmt.Errorf("list the held outbox rows of %s: %w", project, err)
	}
	defer rows.Close()
	held := []HeldRow{}
	for rows.Next() {
		var row HeldRow
		var err error
		if row.OutboxRow, err = scanOutbox(rows, &row.WaitsOn); err != nil {
			return nil, fmt.Errorf("list the held outbox rows of %s: %w", project, err)
		}
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the held outbox rows of %s: %w", project, err)
	}
	return held, nil
}

// heldRowsSQL is HeldRows' statement, built once from the outbox's rules (outboxClaimable,
// outboxLaneFree).
var heldRowsSQL = `select ` + outboxColumns + `, (select min(running.id) from outbox running
			where running.issue = outbox.issue and running.id <> outbox.id and running.kind = any($5::text[]) and running.lease_until > $1) as waits_on
		from outbox where split_part(outbox.issue, '-', 1) = $6 and ($7::text = '' or outbox.issue = $7::text)
		and outbox.kind = any($5::text[])
		and ` + outboxUnleasedDue + ` and not (` + outboxClaimable + `) and not ` + outboxLaneFree + `
		order by next_at, id`

// PauseHeldRows is every row of project's issues that the project's open pause holds at now
// (outboxPauseFree negated) and that is due and under no live lease, by next_at then id: what the
// state lists under the pause (decision 25 of dispatch://INFRA-3759). It is the lanes' own rule
// read back, so what it lists is exactly what the lanes hold for the pause, and it lists nothing
// while no pause is open. A row not yet due waits on its own time and is listed once it comes
// due; a row a lane leased before the pause runs to its end and is not listed. project is the
// Dispatch key.
func (s *Postgres) PauseHeldRows(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]PauseHeldRow, error) {
	return pauseHeldRows(ctx, tx, pauseHeldRowsSQL, project, now)
}

// MarkPauseHeldRows is PauseHeldRows for the resume of pause, the project's open pause, run in
// the resume's own transaction before it ends that pause (store's ResumeRequest.MarkHeld): the
// one statement that lists the rows the pause holds writes pause on each of them
// (OutboxRow.HeldByPause), so the rows it marks are the rows the resume logs as released, and
// each carries the pause when it runs after the resume, whatever its kind or due time. No lane
// runs a held row before that statement or between it and the resume's commit: the pause stands
// until that commit, and its rule holds every such row. A restart while the project is paused
// changes nothing of it, since the marks are written by the resume alone.
func (s *Postgres) MarkPauseHeldRows(ctx context.Context, tx pgx.Tx, project string, pause int64, now time.Time) ([]PauseHeldRow, error) {
	return pauseHeldRows(ctx, tx, markPauseHeldRowsSQL, project, now, pause)
}

// pauseHeldRows runs statement, PauseHeldRows' or MarkPauseHeldRows', with now as $1, project as
// $2 and more after them, and reads the rows it answers, each with the claim it is for and how
// long it has waited at now.
func pauseHeldRows(ctx context.Context, tx pgx.Tx, statement, project string, now time.Time, more ...any) ([]PauseHeldRow, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, fmt.Errorf("list the rows %s's pause holds: %w", project, err)
	}
	rows, err := tx.Query(ctx, statement, append([]any{now, project}, more...)...)
	if err != nil {
		return nil, fmt.Errorf("list the rows %s's pause holds: %w", project, err)
	}
	defer rows.Close()
	held := []PauseHeldRow{}
	for rows.Next() {
		var row PauseHeldRow
		var role string
		if row.OutboxRow, err = scanOutbox(rows, &role); err != nil {
			return nil, fmt.Errorf("list the rows %s's pause holds: %w", project, err)
		}
		row.Claim = pauseHeldClaim(token, row.OutboxRow, role)
		row.WaitedSeconds = max(0, int64(now.Sub(row.NextAt).Seconds()))
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the rows %s's pause holds: %w", project, err)
	}
	return held, nil
}

// pauseHeld is the rows of the project $2 that its open pause holds at $1 and that are due and
// under no live lease: the outbox's own rules (outboxUnleasedDue, outboxPauseFree), the pause's
// negated, which PauseHeldRows lists and MarkPauseHeldRows marks.
var pauseHeld = `split_part(outbox.issue, '-', 1) = $2 and ` + outboxUnleasedDue + ` and not ` + outboxPauseFree

// pauseHeldRowsSQL is PauseHeldRows' statement, and markPauseHeldRowsSQL MarkPauseHeldRows', which
// writes the pause $3 on each row it answers; both are built once from the rows a pause holds
// (pauseHeld).
var (
	pauseHeldRowsSQL = `select ` + outboxColumns + `, coalesce(outbox.payload->>'role', '') from outbox
		where ` + pauseHeld + `
		order by next_at, id`
	markPauseHeldRowsSQL = `with held as (update outbox set held_by_pause = $3 where ` + pauseHeld + `
			returning ` + outboxColumns + `, coalesce(outbox.payload->>'role', '') as role)
		select * from held order by next_at, id`
)

// pauseHeldClaim is the claim a held row is for, where the row names one, and "" where it names
// none (PauseHeldRow). project is the project token and role the payload's `role`. A start, a
// phase-ended message, a hook's words, an update ask and a merge forward's message name the role of
// the row's own issue, as the executors read it (the daemon's roleToken); the merge-queue message
// and the message that the project's merge holds changed go to the issue's merger, whose token
// their executors build (the daemon's mergeQueueRemoval and mergeHoldsChanged); a role message
// names the claim's token itself (record.RoleMessage.Role).
func pauseHeldClaim(project string, row OutboxRow, role string) claim.Token {
	switch row.Kind {
	case OutboxKindSupervise, OutboxKindPhaseEnded, OutboxKindHookPrompt, OutboxKindUpdateAsk, OutboxKindMergeForward:
	case OutboxKindMergeQueueRemoval, OutboxKindMergeHoldsChanged:
		role = string(claim.RoleMerger)
	case OutboxKindRoleMessage:
		if _, ok := claim.Token(role).Role(); ok {
			return claim.Token(role)
		}
		return ""
	default:
		return ""
	}
	token, err := claim.NewToken(project, row.Issue, claim.Role(role))
	if err != nil {
		return ""
	}
	return token
}

func (s *Postgres) WaitingNotices(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and next_at > $2 and split_part(issue, '-', 1) = $3 order by id`, string(OutboxKindNotice), now, project)
	if err != nil {
		return nil, fmt.Errorf("list waiting notices: %w", err)
	}
	defer rows.Close()
	waiting := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list waiting notices: %w", err)
		}
		waiting = append(waiting, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list waiting notices: %w", err)
	}
	return waiting, nil
}

func (s *Postgres) DropStatusWrites(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where kind = $1 and issue = $2", string(OutboxKindDispatchStatus), issue); err != nil {
		return fmt.Errorf("drop the queued status writes of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) DropUnfencedCheckWakes(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where kind = $1 and issue = $2 and not payload ? 'wakeAt'", string(OutboxKindCheckWake), issue); err != nil {
		return fmt.Errorf("drop the queued check wakes of %s that name no wake: %w", issue, err)
	}
	return nil
}

func (s *Postgres) DropCatchUps(ctx context.Context, tx pgx.Tx, issue string, now time.Time) ([]int64, error) {
	// The claims of the issue's catch-ups are serialized with this drop by an advisory lock keyed
	// by the issue, which a catch-up's claim takes too (ClaimRow). The lease check below is a
	// subquery over the outbox, and Postgres rechecks a concurrently updated row's new version
	// against the where clause, never a subquery over other rows: a delete whose statement began
	// before a lane's claim committed would wait on the claim's row lock and then delete the row it
	// had read unleased. Under the lock a claim either commits before the lock is taken, and the
	// delete, a statement of its own, reads the lease it wrote, or it waits for this transaction
	// and claims nothing when the delete took its row. A copy a re-hold queues while this runs
	// (reholdNotice, which carries the row's claims onto it) is covered the same way: its own claim
	// waits here too.
	//
	// The lock takes no row: a lane finishing a catch-up deletes its row and then waits on the
	// decisions it claimed, which the callers of this drop have locked first (workflow.Adopt and
	// spentKept lock them through UnclaimedKept before they queue a catch-up), so a row lock here
	// would close a deadlock (TestDroppingCatchUpsWaitsOnNoRowAFinishIsDeleting).
	if err := LockCatchUps(ctx, tx, issue); err != nil {
		return nil, err
	}
	// A catch-up that carries kept decisions and one of whose telling's rows (itself, the row it
	// copies, or a copy of either) is under a live lease is kept: that row may be publishing now,
	// and the decisions this one claims are that telling's to settle.
	rows, err := tx.Query(ctx, `delete from outbox where kind = $1 and issue = $2 and payload->>'kind' = 'catch-up'
		and not exists (select 1 from processed_events where source = $3
			and event_id = $4 || coalesce((outbox.payload->>'resend_of')::bigint, outbox.id))
		and not (payload->'catch_up'->'kept' is not null and exists (select 1 from outbox live
			where live.kind = outbox.kind and live.issue = outbox.issue
			and coalesce((live.payload->>'resend_of')::bigint, live.id) = coalesce((outbox.payload->>'resend_of')::bigint, outbox.id)
			and live.lease_token <> '' and live.lease_until > $5))
		returning id`, string(OutboxKindNotice), issue, KeptToldSource, outboxKeyPrefix, now)
	if err != nil {
		return nil, fmt.Errorf("drop the queued catch-ups of %s: %w", issue, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read the dropped catch-ups of %s: %w", issue, err)
	}
	return ids, nil
}

// ReleaseHeldCatchUp makes the held catch-up row id due at at, the moment the wake pacing let its
// place out, and records that let-out on the notice for the line its wake writes: PacedAt becomes
// at, and PacedTurns turns, the turns it waited (Notice.PacedAt). A row the pacer let out at its
// join waited no turn (turns 0), so its PacedAt becomes the moment its hold began instead
// (PacedFrom): the time from the engine's write to the join is the fact's commit, not a wait of
// the pacing's, and its line says it waited nothing, as a machine's wake let out at its join
// does. It answers whether the row was held. A row a newer catch-up dropped, or one let out
// already, is left as it is, so a let-out made twice, or made after the drop, changes nothing.
func ReleaseHeldCatchUp(ctx context.Context, tx pgx.Tx, id int64, at time.Time, turns int) (bool, error) {
	tag, err := tx.Exec(ctx, `update outbox set next_at = $3,
		payload = payload || jsonb_build_object('paced_turns', $4::int, 'paced_at',
			case when $4::int = 0 then coalesce(payload->'paced_from', to_jsonb($3::timestamptz)) else to_jsonb($3::timestamptz) end)
		where id = $1 and next_at = $2 and kind = 'notice' and payload->>'kind' = 'catch-up'`,
		id, CatchUpHeldAt, at.UTC(), turns)
	if err != nil {
		return false, fmt.Errorf("release held catch-up row %d: %w", id, err)
	}
	return tag.RowsAffected() != 0, nil
}

// CatchUpHeld is whether the catch-up row id still stands held, with no due moment
// (CatchUpHeldAt): false for a row a newer catch-up dropped and for one the wake pacing has let
// out. The daemon reads it before it gives a held row a place in the pacing's queue, so a row that
// is gone takes none.
func CatchUpHeld(ctx context.Context, tx pgx.Tx, id int64) (bool, error) {
	var held bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from outbox where id = $1 and next_at = $2
		and kind = 'notice' and payload->>'kind' = 'catch-up')`, id, CatchUpHeldAt).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether catch-up row %d is still held: %w", id, err)
	}
	return held, nil
}

func (s *Postgres) OutboxLeased(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) (bool, error) {
	var leased bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from outbox where id = $1 and lease_token = $2)", id, leaseToken).Scan(&leased); err != nil {
		return false, fmt.Errorf("read the lease of outbox row %d: %w", id, err)
	}
	return leased, nil
}

func (s *Postgres) ExpediteOutbox(ctx context.Context, tx pgx.Tx, row OutboxRow, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, "update outbox set next_at = $2 where id = $1 and next_at > $2", row.ID, now)
	if err != nil {
		return false, fmt.Errorf("expedite outbox row %d: %w", row.ID, err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Postgres) TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]Issue, error) {
	rows, err := tx.Query(ctx, "select "+issueColumns+" from issues where tree = $1 order by key", tree)
	if err != nil {
		return nil, fmt.Errorf("list the issues of %s: %w", tree, err)
	}
	defer rows.Close()
	issues := []Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("list the issues of %s: %w", tree, err)
		}
		issues = append(issues, *issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the issues of %s: %w", tree, err)
	}
	return issues, nil
}

func (s *Postgres) EarlierNotices(ctx context.Context, tx pgx.Tx, tree string, id int64) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and id < $2 and issue in (select key from issues where tree = $3)
		order by id`, string(OutboxKindNotice), id, tree)
	if err != nil {
		return nil, fmt.Errorf("read the notices of %s before row %d: %w", tree, id, err)
	}
	defer rows.Close()
	earlier := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("read the notices of %s before row %d: %w", tree, id, err)
		}
		earlier = append(earlier, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the notices of %s before row %d: %w", tree, id, err)
	}
	return earlier, nil
}

func (s *Postgres) TreeCloses(ctx context.Context, tx pgx.Tx, tree string) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and issue in (select key from issues where tree = $2)
		and payload->>'op' = 'tree_close' and payload->>'tree' = $2
		order by id`, string(OutboxKindSupervise), tree)
	if err != nil {
		return nil, fmt.Errorf("read the close rows of tree %s: %w", tree, err)
	}
	closes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OutboxRow, error) {
		return scanOutbox(row)
	})
	if err != nil {
		return nil, fmt.Errorf("read the close rows of tree %s: %w", tree, err)
	}
	return closes, nil
}

// ErrOutboxLeaseLost is FinishLeasedOutbox finding the row no longer held by the caller's lease:
// another worker reclaimed it after this lease expired, or it is already gone.
var ErrOutboxLeaseLost = errors.New("outbox row lease is no longer held")

func (s *Postgres) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if _, err := tx.Exec(ctx, "delete from outbox where id = $1 and lease_token = $2", id, leaseToken); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", id, err)
	}
	return nil
}

func (s *Postgres) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	var finished int64
	err := tx.QueryRow(ctx, "delete from outbox where id = $1 and lease_token = $2 returning id", id, leaseToken).Scan(&finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("finish outbox row %d: %w", id, ErrOutboxLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("finish outbox row %d: %w", id, err)
	}
	return nil
}

func (s *Postgres) NameUndeliveredHookPrompt(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, paragraph string, now time.Time, window time.Duration) (time.Time, bool, error) {
	// The window's lock first, held to the end of tx: a transaction that finds no window and
	// opens one commits before the next look at the same window runs, so two finishes never both
	// find none.
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, "hook-prompts-message:"+issue+":"+string(role)); err != nil {
		return time.Time{}, false, fmt.Errorf("lock the message of %s's undelivered hook prompts on %s: %w", role, issue, err)
	}
	// The lease test is read again once the row's lock is held (READ COMMITTED), so a run that
	// leased the row while this waited for it leaves it out.
	row, err := scanOutbox(tx.QueryRow(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and issue = $2 and payload->'hook_prompts'->>'role' = $3
			and attempts = 0 and created_at > $5
			and next_at > $4 and (lease_until is null or lease_until <= $4)
		order by id limit 1 for update`,
		string(OutboxKindDispatchMessage), issue, string(role), now, now.Add(-window)))
	if errors.Is(err, pgx.ErrNoRows) {
		opened, err := NewOutboxRow(issue, UndeliveredHookPrompts{Role: role, Paragraphs: []string{paragraph}}.Message(issue), now.Add(window))
		if err != nil {
			return time.Time{}, false, fmt.Errorf("open the message of %s's undelivered hook prompts on %s: %w", role, issue, err)
		}
		opened.CreatedAt = now
		if err := s.Enqueue(ctx, tx, opened); err != nil {
			return time.Time{}, false, err
		}
		return opened.NextAt, true, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the message of %s's undelivered hook prompts on %s: %w", role, issue, err)
	}
	payload, err := DecodeOutboxPayload(row)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the message of %s's undelivered hook prompts on %s: %w", role, issue, err)
	}
	// The query took a message row whose payload names role's undelivered prompts, so the payload
	// is such a message.
	prompts := *payload.(MessagePost).HookPrompts
	prompts.Paragraphs = append(prompts.Paragraphs, paragraph)
	joined, err := NewOutboxRow(issue, prompts.Message(issue), row.NextAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("name a prompt in the message of %s's undelivered hook prompts on %s, outbox row %d: %w", role, issue, row.ID, err)
	}
	if _, err := tx.Exec(ctx, `update outbox set payload = $2 where id = $1`, row.ID, string(joined.Payload)); err != nil {
		return time.Time{}, false, fmt.Errorf("name a prompt in the message of %s's undelivered hook prompts on %s, outbox row %d: %w", role, issue, row.ID, err)
	}
	return row.NextAt, false, nil
}

// RetryOutbox returns row id to the outbox, due again at nextAt, while leaseToken holds it: one
// more attempt, its lease cleared, and lastErr as its last error. lastErr is a far side's error
// text, so the row holds storetext.FarSideError of it, and the caller logs the whole text
// (storetext.LogWhole).
func (s *Postgres) RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error {
	if _, err := tx.Exec(ctx, `update outbox set attempts = attempts + 1, next_at = $3, last_error = $4,
		lease_token = '', lease_until = null where id = $1 and lease_token = $2`, id, leaseToken, nextAt, storetext.FarSideError(lastErr)); err != nil {
		return fmt.Errorf("retry outbox row %d: %w", id, err)
	}
	return nil
}

// RoleRun reads one role's run of an issue generation as the store holds it. RoleRuns owns the
// query shape, so the one-role caller and a state-report batch share it.
func (s *Postgres) RoleRun(ctx context.Context, tx pgx.Tx, token claim.Token, issue string, role claim.Role, generation uint64) (RoleRun, error) {
	runs, err := s.RoleRuns(ctx, tx, []RoleRunKey{{Token: token, Issue: issue, Role: role, Generation: generation}})
	if err != nil {
		return RoleRun{}, err
	}
	return runs[token], nil
}

// roleRunIdentity is a candidate role run as an outbox supervise row names it: its issue, and the
// payload's role and generation exactly as Postgres renders them (`payload->>'role'`,
// `payload->>'generation'`), so a row is matched before it is decoded.
type roleRunIdentity struct {
	issue      string
	role       string
	generation string
}

// RoleRuns reads the queued supervise rows and the claims of keys in two bounded queries. The
// outbox query reads the selected issues' supervise rows by the outbox_issue index with each
// payload's role and generation as text, each row's age in the store's own clock
// (QueuedSupervise.Age) and whether a live lease holds the row at that same now()
// (QueuedSupervise.Running), and decodes only the rows of keys' roles and generations. A row of
// another role is never decoded, so one that does not decode fails only a read of its own role's
// run, as RoleRun's did.
//
// The rows are read first and the claims second, and that order is what makes the two answers
// safe to judge together under a READ COMMITTED transaction, which every fact handler's is
// (intake.ApplyFact opens it with the default options): there the two statements share no
// snapshot, each takes one of its own, and a writer may commit between them. A caller that reads
// under REPEATABLE READ has one snapshot for both statements and no such gap at all, as the
// boot's report of the claims nothing will wake does (internal/daemon's reportStranded, then
// workflow.Stranded). Every writer that turns a queued start into claim state commits the claim
// before it deletes the row — the outbox runs the row through the claim's machine, which
// persists the claim's new state, and finishes the row in a transaction after that
// (internal/daemon's outbox: supervise, then settle, then finish) — so a row the first statement
// misses was deleted before the second one ran, and the second one reads the claim state that
// delete stands for. Reading the rows first is therefore what keeps a caller from ever seeing a
// queued start gone beside a claim that has not yet moved, which is the one pair a rule that
// weighs a claim against rows it has read would take for a worker nothing is starting again
// (workflow.StartFor, for admit's resumeSlottedWorkers and startMidPhaseMembers; RoleClaims
// says what the same order means for a caller that reads the claims alone).
func (s *Postgres) RoleRuns(ctx context.Context, tx pgx.Tx, keys []RoleRunKey) (map[claim.Token]RoleRun, error) {
	runs := make(map[claim.Token]RoleRun, len(keys))
	if len(keys) == 0 {
		return runs, nil
	}
	tokens := make([]string, 0, len(keys))
	issues := make([]string, 0, len(keys))
	seenIssues := make(map[string]struct{}, len(keys))
	candidates := make(map[roleRunIdentity]claim.Token, len(keys))
	for _, key := range keys {
		if _, exists := runs[key.Token]; exists {
			continue
		}
		runs[key.Token] = RoleRun{}
		tokens = append(tokens, string(key.Token))
		candidates[roleRunIdentity{issue: key.Issue, role: string(key.Role), generation: strconv.FormatUint(key.Generation, 10)}] = key.Token
		if _, seen := seenIssues[key.Issue]; !seen {
			seenIssues[key.Issue] = struct{}{}
			issues = append(issues, key.Issue)
		}
	}

	rows, err := tx.Query(ctx, `select `+outboxColumns+`, coalesce(payload->>'role', ''), coalesce(payload->>'generation', ''),
		coalesce($3::timestamptz, now()) - created_at, coalesce(lease_token <> '' and lease_until > coalesce($3::timestamptz, now()), false)
		from outbox where kind = $1 and issue = any($2) order by id`, string(OutboxKindSupervise), issues, s.stamp())
	if err != nil {
		return nil, fmt.Errorf("read the queued role runs of %d issues: %w", len(issues), err)
	}
	for rows.Next() {
		var role, generation string
		var age time.Duration
		var running bool
		row, err := scanOutbox(rows, &role, &generation, &age, &running)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the queued role runs of %d issues: %w", len(issues), err)
		}
		token, wanted := candidates[roleRunIdentity{issue: row.Issue, role: role, generation: generation}]
		if !wanted {
			continue
		}
		decoded, err := DecodeOutboxPayload(row)
		if err != nil {
			rows.Close()
			return nil, err
		}
		request, ok := decoded.(SuperviseRequest)
		if !ok {
			rows.Close()
			return nil, fmt.Errorf("outbox row %d of kind %s decodes to %T", row.ID, row.Kind, decoded)
		}
		run := runs[token]
		run.Queued = append(run.Queued, QueuedSupervise{ID: row.ID, Request: request, Attempts: row.Attempts,
			Running: running, Age: age})
		runs[token] = run
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the queued role runs of %d issues: %w", len(issues), err)
	}
	rows.Close()

	if err := readRoleClaims(ctx, tx, tokens, func(token claim.Token, held *RoleClaim) {
		run := runs[token]
		run.Claim = held
		runs[token] = run
	}); err != nil {
		return nil, err
	}
	return runs, nil
}

// RoleClaims reads the claims of tokens and nothing else, keyed by token: RoleRuns' second
// statement alone, for a caller that judges a role run by its claim and never reads the queued
// starts beside it (admit's finishFencedStarts, through stopOutlivedStart, and the outbox's
// check_look row after a project's pause, internal/daemon's lookBoundAfterPause). A token the store
// holds no claim for is absent from the answer, which is the nil claim RoleRuns reports for it.
// It saves admit's caller RoleRuns' first statement, which selects and strictly decodes every
// supervise row of every issue that owes a start, once per admission pass.
//
// Nothing of the ordering RoleRuns sets out is given up, and admit's caller leans on it: these
// claims must be read after the owed rows they are judged against, so that a stop made between
// the two reads is in the claim read here and the rule acts on this pass rather than the next.
// The mark it weighs each owed row against is RoleClaim.RetiredAtWriteMark, taken in store's
// putClaim, whose doc holds what the sequence behind it orders. That is where the call sits —
// the pass reads its owed rows into record.LineFacts first, and weighs them here after.
func (s *Postgres) RoleClaims(ctx context.Context, tx pgx.Tx, tokens []claim.Token) (map[claim.Token]*RoleClaim, error) {
	held := make(map[claim.Token]*RoleClaim, len(tokens))
	if len(tokens) == 0 {
		return held, nil
	}
	names := make([]string, 0, len(tokens))
	for _, token := range tokens {
		names = append(names, string(token))
	}
	if err := readRoleClaims(ctx, tx, names, func(token claim.Token, c *RoleClaim) {
		held[token] = c
	}); err != nil {
		return nil, err
	}
	return held, nil
}

// readRoleClaims hands each claim of tokens to found, in no order: the one statement RoleRuns and
// RoleClaims share, with the task pending on each claim read beside it (the two tables are
// written apart and read together, as claimSelect reads them). The task's id is read with it,
// since the id is what says a delivery is the daemon's own delivery of a relaunch's record, which
// a reader of a role run reads as no work (supervise.Delivery.Relaunch; workflow's owed and
// readyWorkerTask). So is whether the task is one of news alone (supervise.Delivery.NewsOnly),
// read from the delivery's own column (pending_task_deliveries.news_only), which is no task of its
// phase to any reader of a role run (supervise.Delivery.StillWorked, supervise.HoldsWork) and no
// task of an entry, so it holds no sweep's start back (workflow.ArchitectStartPending).
//
// The task is read as the entry it is of, the two moments of its handshake — queued when the
// delivery was written, confirmed when a turn of the claim's worker took it — and whether a let-go
// took it back from a turn (interrupted), with the moment the claim's agent last said it was ready
// (last_ready) and the claim's own handshake of the last entry it was queued a task of
// (handshake_*), whose queued moment is when that entry's task was first queued, however many
// times it was queued again, and whose confirmed moment is when a turn first confirmed one. Those
// are what say an architect took nothing — the task of the entry its row names, standing
// unconfirmed, counted while the claim's agent is up from the later of that entry's first
// queueing and the agent's ready — and when the bound of a root slow before its gate starts,
// which the admission pass reads of every root it weighs (admit's architectStalls).
//
// The claim's failure cause is read as its machine reads it (RoleClaim.FailureCause): the row's
// failure_cause and held_for, decoded by the claim store's own decoder (supervise.DecodeCause),
// so this read and the claim store's cannot disagree on whether one claim is held.
func readRoleClaims(ctx context.Context, tx pgx.Tx, tokens []string, found func(claim.Token, *RoleClaim)) error {
	rows, err := tx.Query(ctx, `select c.token, c.state, c.failure_cause, c.held_for,
		c.tree, c.suspended_by, c.last_start_row, c.retired_at_write_mark, c.given_entry,
		c.last_ready, c.handshake_entry, c.handshake_generation, c.handshake_queued_at, c.handshake_confirmed_at,
		c.update_round, c.step_at, c.turn_started_at,
		c.activity_at, c.activity_background, c.activity_next_reminder_at,
		d.claim_token is not null, coalesce(d.delivery_id, ''), coalesce(d.generation, 0), coalesce(d.phase, ''), coalesce(d.entry, 0), d.queued_at, d.confirmed_at,
		coalesce(d.interrupted, false), coalesce(d.news_only, false)
		from claims c left join pending_task_deliveries d on d.claim_token = c.token where c.token = any($1)`, tokens)
	if err != nil {
		return fmt.Errorf("read the claims of %d role runs: %w", len(tokens), err)
	}
	defer rows.Close()
	for rows.Next() {
		var token, cause, heldFor string
		var held RoleClaim
		var pending bool
		var task supervise.Delivery
		var queued, confirmed *time.Time
		// last_ready is nullable (migration 20261029121000_claim_death_and_start_causes): a claim
		// whose agent has never said it was ready has none, and the zero time is what says so.
		var ready *time.Time
		// Both handshake moments are nullable (migration 20261114020537_claim_entry_handshake): a
		// claim queued no task of an entry has neither, and one no turn confirmed such a task of
		// has no confirmed moment. The zero time left in each is what says so.
		var handshakeQueued, handshakeConfirmed *time.Time
		// Both step columns are nullable (migration 20261107052815_claim_step_record): a claim
		// whose agent has neither stepped nor begun a turn has neither, and the zero time left
		// in the step record is what says so to its reader (supervise.StepRecord.LastStep).
		var stepped, turnStarted *time.Time
		// Both activity moments are nullable (migration 20261101214123_claim_session_activity):
		// a session that has reported nothing has no activity_at, and one that armed no reminder
		// has no activity_next_reminder_at. The zero time left in each is what says so to the
		// rule that reads them (workflow.SilentPhaseWorker).
		var reported, reminder *time.Time
		if err := rows.Scan(&token, &held.State, &cause, &heldFor, &held.Tree, &held.SuspendedBy, &held.LastStartRow, &held.RetiredAtWriteMark,
			&held.GivenEntry, &ready, &held.Handshake.Entry, &held.Handshake.Generation, &handshakeQueued, &handshakeConfirmed,
			&held.UpdateRound, &stepped, &turnStarted, &reported, &held.ActivityBackground, &reminder,
			&pending, &task.ID, &task.Generation, &task.Phase, &task.Entry, &queued, &confirmed, &task.Interrupted, &task.NewsOnly); err != nil {
			return fmt.Errorf("read the claims of %d role runs: %w", len(tokens), err)
		}
		held.FailureCause = supervise.DecodeCause(cause, heldFor)
		if ready != nil {
			held.LastReady = ready.UTC()
		}
		if handshakeQueued != nil {
			held.Handshake.QueuedAt = handshakeQueued.UTC()
		}
		if handshakeConfirmed != nil {
			held.Handshake.ConfirmedAt = handshakeConfirmed.UTC()
		}
		if stepped != nil {
			held.Step.At = *stepped
		}
		if turnStarted != nil {
			held.Step.TurnStartedAt = *turnStarted
		}
		if reported != nil {
			held.ActivityAt = *reported
		}
		if reminder != nil {
			held.ActivityNextReminderAt = *reminder
		}
		if pending {
			if queued != nil {
				task.QueuedAt = queued.UTC()
			}
			if confirmed != nil {
				task.ConfirmedAt = *confirmed
			}
			held.Pending = &task
		}
		found(claim.Token(token), &held)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the claims of %d role runs: %w", len(tokens), err)
	}
	return nil
}

// PendingStatusWrites lists every Dispatch status write of project's issues the outbox has not
// finished: due now, in flight, backing off after a failed attempt, or waiting behind an older
// write for the same issue. Finishing deletes the row, so each one left is a write the outbox has
// not finished; a write that reached Dispatch whose finish failed stays listed until its lease
// expires and the rerun finishes it. Oldest first: the order one issue's statuses run in
// (outboxInOrder).
func (s *Postgres) PendingStatusWrites(ctx context.Context, tx pgx.Tx, project string) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2 order by id`, string(OutboxKindDispatchStatus), project)
	if err != nil {
		return nil, fmt.Errorf("list pending Dispatch status writes: %w", err)
	}
	defer rows.Close()
	pending := []OutboxRow{}
	for rows.Next() {
		row, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending Dispatch status writes: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending Dispatch status writes: %w", err)
	}
	return pending, nil
}

// scanOutbox reads one row of outboxColumns, then extra: the destinations of any columns the
// statement selects after them.
func scanOutbox(row scanner, extra ...any) (OutboxRow, error) {
	var out OutboxRow
	var kind string
	var payload []byte
	if err := row.Scan(append([]any{&out.ID, &kind, &out.Issue, &payload, &out.Attempts, &out.NextAt, &out.LastError,
		&out.CreatedAt, &out.LeaseToken, &out.LeaseUntil, &out.HeldByPause}, extra...)...); err != nil {
		return OutboxRow{}, err
	}
	out.Kind = OutboxKind(kind)
	out.Payload = append(json.RawMessage(nil), payload...)
	return out, nil
}

func leaseToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint outbox lease token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

type scanner interface {
	Scan(dest ...any) error
}
