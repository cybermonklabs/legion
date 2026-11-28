package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// c-child-view's reads of the own-spec mark (issue_views.own_spec_at) and of the coverage read
// over the tuples. The mark keeps its first time and a second statement that sets it changes no
// column, so which writers set it, and in which transaction, is read off a log of the statements
// that write the row: a trigger this test's own database alone holds logs each row an insert into
// issue_views proposes - the create's row that says the issue was born converted
// (docs.QueueBornConverted) and each own-spec mark (docs.QueueOwnSpec), which conflicts with that
// row and updates it - with the transaction it ran in and that transaction's time.

// installOwnSpecMarkLog adds that log to the database of the test: the table own_spec_mark_log and
// the trigger on issue_views that fills it.
func installOwnSpecMarkLog(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		create table own_spec_mark_log (
			seq bigserial primary key,
			issue_key text not null,
			born boolean not null,
			marked boolean not null,
			xact bigint not null default txid_current(),
			at timestamptz not null default now()
		);
		create function own_spec_mark_log() returns trigger language plpgsql as $$
		begin
			insert into own_spec_mark_log (issue_key, born, marked)
			values (new.issue_key, new.converted is not distinct from 'born', new.own_spec_at is not null);
			return new;
		end $$;
		create trigger own_spec_mark_log before insert on issue_views
			for each row execute function own_spec_mark_log();`); err != nil {
		t.Fatalf("add the log of the statements that write issue_views: %v", err)
	}
}

// ownSpecMarks is what the log holds of the issue issueKey: the transaction of its create, the
// one that wrote the row saying it was born converted, and that transaction's time; the
// transaction of each own-spec mark, in the order the marks ran; and the mark the row holds now,
// nil where it holds none.
type ownSpecMarks struct {
	create   int64
	createAt time.Time
	marks    []int64
	mark     *time.Time
}

// inCreate is how many own-spec marks ran in the issue's create's own transaction.
func (m ownSpecMarks) inCreate() int {
	count := 0
	for _, xact := range m.marks {
		if xact == m.create {
			count++
		}
	}
	return count
}

// markValue is the mark as a failure line names it: its time, or null.
func (m ownSpecMarks) markValue() string {
	if m.mark == nil {
		return "null"
	}
	return m.mark.UTC().Format(time.RFC3339Nano)
}

// readOwnSpecMarks reads ownSpecMarks of issueKey, and fails unless the log holds the create's row.
func readOwnSpecMarks(t *testing.T, database *store.Store, issueKey string) ownSpecMarks {
	t.Helper()
	ctx := context.Background()
	var read ownSpecMarks
	if err := database.Pool.QueryRow(ctx, `select xact, at from own_spec_mark_log where issue_key = $1 and born order by seq limit 1`, issueKey).
		Scan(&read.create, &read.createAt); err != nil {
		t.Fatalf("read the transaction of the create of %s off the log of issue_views: %v", issueKey, err)
	}
	rows, err := database.Pool.Query(ctx, `select xact from own_spec_mark_log where issue_key = $1 and marked order by seq`, issueKey)
	if err != nil {
		t.Fatalf("read the own-spec marks of %s off the log of issue_views: %v", issueKey, err)
	}
	if read.marks, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
		t.Fatalf("read the own-spec marks of %s off the log of issue_views: %v", issueKey, err)
	}
	if err := database.Pool.QueryRow(ctx, `select own_spec_at from issue_views where issue_key = $1`, issueKey).Scan(&read.mark); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read the own-spec mark of %s: %v", issueKey, err)
	}
	return read
}

// requireMarkedByItsCreate fails unless the create of the issue issueKey, whose spec holds a
// requirement, set its own-spec mark in its own transaction: own_spec_at holds that transaction's
// time, and two marks ran in it, the create's own (docs.RefuseRequirementUnder) and the one of the
// pass of the block index that states the requirement (docs' markOwnSpecIndexed), pass naming
// which pass that is. It fails with t.Errorf, so the test goes on to the reads it fails on.
func requireMarkedByItsCreate(t *testing.T, database *store.Store, issueKey, pass string) {
	t.Helper()
	marks := readOwnSpecMarks(t, database, issueKey)
	if marks.mark == nil || !marks.mark.Equal(marks.createAt) {
		t.Errorf("the create of %s left its own-spec mark %s, want %s, the time of its own transaction: the create, whose spec holds a requirement, sets the mark in its own transaction",
			issueKey, marks.markValue(), marks.createAt.UTC().Format(time.RFC3339Nano))
	}
	if held := marks.inCreate(); held != 2 {
		t.Errorf("the create of %s ran the own-spec mark %d time(s) in its own transaction %d (the marks ran in the transactions %v), want two: the create's own mark, and %s, which marks it again",
			issueKey, held, marks.create, marks.marks, pass)
	}
}

// requireMarkedByItsSettlement fails unless the issue issueKey, whose create's spec held no
// requirement, holds an own-spec mark that no statement of its create set and the pass of the
// block index of its settlement did. It fails with t.Errorf.
func requireMarkedByItsSettlement(t *testing.T, database *store.Store, issueKey, block string) {
	t.Helper()
	marks := readOwnSpecMarks(t, database, issueKey)
	if held := marks.inCreate(); held != 0 {
		t.Errorf("the create of %s, whose spec holds no requirement, ran the own-spec mark %d time(s) in its own transaction, want none", issueKey, held)
	}
	if marks.mark == nil || !slices.ContainsFunc(marks.marks, func(xact int64) bool { return xact != marks.create }) {
		t.Errorf("after the settlement whose block index states the requirement %s of %s, its own-spec mark is %s, set in the transactions %v, want the mark the index pass sets",
			block, issueKey, marks.markValue(), marks.marks)
	}
}

// requireTwinNamesGap fails unless the coverage read over the tuples of issueKey, which the
// coverage route answers through (issueCoverageOverTuples), read on its own, names line among its
// gaps.
func requireTwinNamesGap(t *testing.T, deps Deps, database *store.Store, issueKey, line string) {
	t.Helper()
	coverage, err := directServer(deps).issueCoverageOverTuples(context.Background(), database.Pool, model.Actor{Kind: "user", ID: "alice"}, issueKey, nil)
	if err != nil {
		t.Fatalf("read the coverage of %s over the tuples: %v", issueKey, err)
	}
	if !slices.Contains(coverage.Gaps, line) {
		t.Errorf("the coverage over the tuples of %s answers %q, want the line %q", issueKey, coverage.Gaps, line)
	}
}
