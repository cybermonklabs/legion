package embedqueue

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// recordOnce runs the record step (recordSources) to completion, as the pass and the periodic run
// do, and answers how many sources it recorded.
func recordOnce(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var recorded int64
	if err := recordSources(context.Background(), database, &recorded); err != nil {
		t.Fatalf("record the issues' sources: %v", err)
	}
	return recorded
}

// repairOnce runs the pass's repair of stale issue rows (repairStaleIssues) to completion, as
// coverPass does, and answers how many rows it repaired.
func repairOnce(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var repaired int64
	step := coverStep{&repaired, repairStaleIssues, []any{issueKind, coverageStatementRows}}
	if err := step.run(context.Background(), Deps{Store: database}, issueKind); err != nil {
		t.Fatalf("repair the stale issue rows: %v", err)
	}
	return repaired
}

// sourceMatches reports whether the refresh of key's state row (search_coverage_issue_refresh)
// reads key's hash from the issue's row rather than building its text: whether the source
// recorded for it is its current one, in the very expressions the refresh compares
// (issueSourceFrom, issueSource).
func sourceMatches(t *testing.T, database *store.Store, key string) bool {
	t.Helper()
	var matches bool
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(r.source = (`+issueSource+`), false) from `+issueSourceFrom+` where i.key = $1`, key,
	).Scan(&matches); err != nil {
		t.Fatalf("read whether %s's recorded source is its current one: %v", key, err)
	}
	return matches
}

// recordedSource is the source recorded for key and its row's xmin, both empty when none is.
func recordedSource(t *testing.T, database *store.Store, key string) (source, xmin string) {
	t.Helper()
	err := database.Pool.QueryRow(context.Background(),
		`select source, xmin::text from embedding_issue_sources where key = $1`, key).Scan(&source, &xmin)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read %s's recorded source: %v", key, err)
	}
	return source, xmin
}

// An issue whose row holds the hash of its current text is recorded once, and the refresh then
// reads its hash from the row rather than building its text (sourceMatches). A second run records
// nothing and rewrites nothing: the source row keeps its xmin.
func TestTheRecordStepRecordsAnIssueThatHoldsItsTextOnce(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "RCRD", "RCRD-1", "An issue its trigger enqueued")
	seedSpec(t, database.Pool, "RCRD", "RCRD-1", "## Summary\n\nIts spec.\n")
	if sourceMatches(t, database, "RCRD-1") {
		t.Fatal("before any record run the refresh reads RCRD-1's hash from its row")
	}

	if recorded := recordOnce(t, database); recorded != 1 {
		t.Fatalf("the first run recorded %d sources, want RCRD-1's", recorded)
	}
	_, xmin := recordedSource(t, database, "RCRD-1")
	if !sourceMatches(t, database, "RCRD-1") {
		t.Error("once recorded, the refresh still builds RCRD-1's text rather than reading its hash from the row")
	}

	if recorded := recordOnce(t, database); recorded != 0 {
		t.Errorf("a second run recorded %d sources, want none", recorded)
	}
	if _, again := recordedSource(t, database, "RCRD-1"); again != xmin {
		t.Errorf("a second run rewrote RCRD-1's source row (xmin %s, was %s), want it left as it was", again, xmin)
	}
}

// An issue whose row is stale - its title changed with its trigger off, so the row's content_hash
// is not the hash of the issue's text - is not recorded: the refresh must build and hash its text.
// The pass's repair points the row at the current text, and the run after the repair records it.
func TestTheRecordStepLeavesAStaleIssueUntilTheRepair(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "RSTL", "RSTL-1", "A title the trigger saw")
	withTableTriggersOff(t, database, "issues", func(q querier) {
		mustExec(t, q, `update issues set title = 'A title restored with its trigger off' where key = 'RSTL-1'`)
	})

	if recorded := recordOnce(t, database); recorded != 0 {
		t.Fatalf("the record step recorded %d sources over a stale row, want none", recorded)
	}
	if source, _ := recordedSource(t, database, "RSTL-1"); source != "" {
		t.Fatalf("RSTL-1 has the source %q recorded over its stale row, want none", source)
	}
	if repaired := repairOnce(t, database); repaired != 1 {
		t.Fatalf("the repair repaired %d rows, want RSTL-1's", repaired)
	}
	if recorded := recordOnce(t, database); recorded != 1 {
		t.Fatalf("the run after the repair recorded %d sources, want RSTL-1's", recorded)
	}
	if !sourceMatches(t, database, "RSTL-1") {
		t.Error("once recorded after the repair, the refresh still builds RSTL-1's text")
	}
}

// Every edit of an input an issue's text is built from makes its recorded source stop matching
// its current one, so its hash is built from its text again until the record step's next run
// records the issue again, and the counts follow the edit: at once where it fires its triggers,
// and at the counts loop's next recount where it fires none (R7 of dispatch://INFRA-4864's spec).
// Then the report answers what main's statements answer (requireMainsAnswer). The edits are a new
// spec version and a title edit, each with its trigger on and read at once, and the latest
// version's markdown rewritten in place with the triggers off, once as it was (which changes the
// row version alone) and once to a new text (which leaves the row stale, so the repair comes
// before the record), each read after one recount (recountCoverage). Before each edit the row is
// embedded at its current text, so a report that trusted the source past the edit would count a
// vector of the old text current.
func TestTheCountsFollowEachEditOfAnIssuesInputsByTheNextRecount(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "REDT", "REDT-1", "A title")
	spec := seedSpec(t, database.Pool, "REDT", "REDT-1", "## Summary\n\nThe first version.\n")
	embedRowAs(t, database.Pool, "issue", "REDT-1", configuredIdentity)
	fillCounts(t, database)
	if recorded := recordOnce(t, database); recorded != 1 {
		t.Fatalf("the first run recorded %d sources, want REDT-1's", recorded)
	}
	requireMainsAnswer(t, database, "once recorded")

	// rewriteLatest rewrites the latest version's markdown in place with the triggers off: to
	// markdown, or as it was when markdown is empty.
	rewriteLatest := func(markdown string) func() {
		return func() {
			withTableTriggersOff(t, database, "artifact_versions", func(q querier) {
				mustExec(t, q, `update artifact_versions set markdown = coalesce(nullif($2::text, ''), markdown)
					where artifact_id = $1::uuid and number = (select max(number) from artifact_versions where artifact_id = $1::uuid)`, spec, markdown)
			})
		}
	}
	for _, edit := range []struct {
		name string
		edit func()
		// current is how many issues are current after the edit: none, but after a rewrite that
		// leaves the text as it was.
		current int64
		// stale is whether the edit leaves the row at the text before it, for the repair to point
		// at the new one before the record step records it.
		stale bool
		// triggersOff is whether the edit fires no trigger, so the counts follow it at the next
		// recount rather than at once.
		triggersOff bool
	}{
		{"a new spec version", func() { addVersion(t, database.Pool, spec, 2, "## Summary\n\nThe second version.\n") }, 0, false, false},
		{"a title edit", func() { mustExec(t, database.Pool, `update issues set title = 'A title, edited' where key = 'REDT-1'`) }, 0, false, false},
		{"the latest version's markdown rewritten in place as it was", rewriteLatest(""), 1, false, true},
		{"the latest version's markdown changed in place", rewriteLatest("## Summary\n\nChanged in place.\n"), 0, true, true},
	} {
		edit.edit()
		if sourceMatches(t, database, "REDT-1") {
			t.Errorf("after %s REDT-1's recorded source still matches, so the refresh would read the hash of the text before it", edit.name)
		}
		stage := "after " + edit.name
		if edit.triggersOff {
			if err := recountCoverage(context.Background(), database); err != nil {
				t.Fatalf("recount %s: %v", stage, err)
			}
			stage += " and one recount"
		}
		mains, heads := requireMainsAnswer(t, database, stage)
		for name, rows := range map[string]map[string]coverageRow{"main's statement": mains, "the coverage statement": heads} {
			if got := rows["issue"].current; got != edit.current {
				t.Errorf("%s %s counts %d issues current, want %d", stage, name, got, edit.current)
			}
		}
		if edit.stale {
			if recorded := recordOnce(t, database); recorded != 0 {
				t.Errorf("after %s the record step recorded %d sources over the stale row, want none", edit.name, recorded)
			}
			if repaired := repairOnce(t, database); repaired != 1 {
				t.Errorf("after %s the repair repaired %d rows, want REDT-1's", edit.name, repaired)
			}
		}
		if recorded := recordOnce(t, database); recorded != 1 {
			t.Errorf("the run after %s recorded %d sources, want REDT-1's again", edit.name, recorded)
		}
		if !sourceMatches(t, database, "REDT-1") {
			t.Errorf("once recorded after %s, the refresh still builds REDT-1's text", edit.name)
		}
		embedRowAs(t, database.Pool, "issue", "REDT-1", configuredIdentity)
		requireMainsAnswer(t, database, "once recorded and embedded after "+edit.name)
	}
}

// The record step writes embedding_issue_sources alone, so a pass over a database the triggers
// keep current still writes no embeddings row with the step in it - the premise of
// TestCoverChangesNothingTheTriggersAlreadyEnqueued, whose snapshot compares each row's xmin. Both
// of the step's writes are held to it: the insert of a first source, and the update of a source
// whose issue was edited since.
func TestTheRecordStepWritesNoEmbeddingsRow(t *testing.T) {
	database := storetest.Open(t)
	seedEveryKind(t, database.Pool, "RNOW")
	deps := Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)}
	drain(t, deps)
	requireNoRowWritten := func(what string) {
		t.Helper()
		before := snapshotRows(t, database)
		if recorded := recordOnce(t, database); recorded != 1 {
			t.Fatalf("%s recorded %d sources, want RNOW-1's", what, recorded)
		}
		after := snapshotRows(t, database)
		if len(after) != len(before) {
			t.Fatalf("%s: %d embeddings rows after, %d before", what, len(after), len(before))
		}
		for key, was := range before {
			if now := after[key]; now != was {
				t.Errorf("%s wrote %s:\n before %+v\n after  %+v", what, key, was, now)
			}
		}
	}

	requireNoRowWritten("the record step's first run")
	mustExec(t, database.Pool, `update issues set title = 'A title edited since its source was recorded' where key = 'RNOW-1'`)
	drain(t, deps)
	requireNoRowWritten("the record step's run after an edit")
}

// passRecords is every record the pass logged at level with message, in the order it logged them.
func passRecords(records *logtest.Recorder, level slog.Level, message string) []slog.Record {
	var matched []slog.Record
	for _, record := range records.Records() {
		if record.Level == level && record.Message == message {
			matched = append(matched, record)
		}
	}
	return matched
}

// attrsOf is record's attributes by key.
func attrsOf(record slog.Record) map[string]slog.Value {
	attrs := map[string]slog.Value{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value
		return true
	})
	return attrs
}

// A pass whose report Postgres ends at its bound (coverageStatementTimeout) fails nothing: every
// step before the report is done, so Cover returns nil, and the pass logs the failed report at
// WARN with its error and what the pass changed, rather than failing and being run again whole by
// runCoverage. The counts are filled after the seed, and search_coverage_counts is then locked in
// another connection, so the report's read of the base rows waits however fast the statement
// would run, and the bound is what ends it; no step before the report touches that table, since a
// write's triggers write delta rows and never the base. Once the lock is released, the same pass
// logs its ordinary line with the coverage it leaves.
func TestACoveragePassWhoseReportRunsPastItsBoundFailsNothing(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "BNDP", "BNDP-1", "An issue the pass records")
	fillCounts(t, database)
	deps := Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)}
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)
	t.Cleanup(ShortenCoverageBoundForTest(500 * time.Millisecond))
	hold, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds search_coverage_counts: %v", err)
	}
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	if _, err := hold.Exec(ctx, `lock table search_coverage_counts in access exclusive mode`); err != nil {
		t.Fatalf("lock search_coverage_counts: %v", err)
	}

	if err := Cover(ctx, deps); err != nil {
		t.Fatalf("Cover with its report past the bound = %v, want nil: the steps before the report are done", err)
	}
	failed := passRecords(records, slog.LevelWarn, "dispatch embedqueue: coverage pass report failed")
	if len(failed) != 1 {
		t.Fatalf("the pass logged %d WARN lines of a failed report, want 1: %v", len(failed), records.Records())
	}
	attrs := attrsOf(failed[0])
	if logged, _ := attrs["error"].Any().(error); !errors.Is(logged, ErrCoverageTimedOut) {
		t.Errorf("the failed report's line logs the error %v, want ErrCoverageTimedOut", logged)
	}
	if recorded := attrs["issueSourcesRecorded"].Any(); recorded != int64(1) {
		t.Errorf("the failed report's line logs issueSourcesRecorded %v, want 1: the steps before the report ran", recorded)
	}
	if ordinary := passRecords(records, slog.LevelInfo, "dispatch embedqueue: coverage pass"); len(ordinary) != 0 {
		t.Errorf("the pass whose report failed logged the ordinary line too: %v", ordinary)
	}

	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the lock on search_coverage_counts: %v", err)
	}
	if err := Cover(ctx, deps); err != nil {
		t.Fatalf("Cover once the lock is released = %v, want nil", err)
	}
	ordinary := passRecords(records, slog.LevelInfo, "dispatch embedqueue: coverage pass")
	if len(ordinary) != 1 {
		t.Fatalf("the pass once the lock is released logged %d ordinary lines, want 1: %v", len(ordinary), records.Records())
	}
	if items := attrsOf(ordinary[0])["items"].Any(); items != int64(1) {
		t.Errorf("the ordinary line logs items %v, want BNDP-1 alone", items)
	}
	if failed := passRecords(records, slog.LevelWarn, "dispatch embedqueue: coverage pass report failed"); len(failed) != 1 {
		t.Errorf("the pass once the lock is released logged a failed report: %v", failed)
	}
}
