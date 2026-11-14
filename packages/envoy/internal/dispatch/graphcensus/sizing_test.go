package graphcensus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// sizingCensus runs the store census with its growth pass over database, as
// `envoy-dispatch census --store --sizing` runs it, and returns everything it printed.
func sizingCensus(t *testing.T, database *store.Store) string {
	t.Helper()
	report, err := StoreCensus(context.Background(), database.Pool.Config().ConnString(), true)
	if err != nil {
		t.Fatalf("take the store census with its growth pass: %v", err)
	}
	var printed bytes.Buffer
	report.Write(&printed)
	return printed.String()
}

// seedOwedDocuments writes three documents owed a block index, their debts dated an hour apart,
// and one event to stand as the database's own clock. A version carrying markdown owes its
// document an index through migration 0122's trigger, which is how a document is owed one in
// production too, so nothing here marks the rows by hand.
func seedOwedDocuments(t *testing.T, database *store.Store, clock time.Time, ages []time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	for index, age := range ages {
		written := clock.Add(-age)
		slug := fmt.Sprintf("spec-%d", index)
		var artifactID string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
			values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}', $2)
			returning id::text
		`, slug, written).Scan(&artifactID); err != nil {
			t.Fatalf("create document %s: %v", slug, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
			values ($1, 1, 'Ship it.', '[{"kind":"user","id":"alice"}]', $2)
		`, artifactID, written); err != nil {
			t.Fatalf("write version 1 of %s: %v", slug, err)
		}
	}
	// The newest event is the clock the report measures the oldest debt against. A test writes
	// it directly because this package seeds no document through the server.
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('CORE-1', 1, 'artifact.version', '{"kind":"user","id":"alice"}', '{}', false, $1)
	`, clock); err != nil {
		t.Fatalf("record the event the backlog's age is measured against: %v", err)
	}
}

// The sizing reports the block-index backlog, so somebody holding the database and not the
// server's log can read it: how many documents are owed an index, when the oldest of those
// debts was incurred, and how old that debt is. The age follows the oldest owed document, not
// the newest, because a backlog dated by its newest arrival reads as fresh however long its
// oldest has waited.
func TestTheSizingReportsTheOwedDocumentsAndTheAgeOfTheOldest(t *testing.T) {
	database := storetest.Open(t)
	// Postgres holds a timestamp to the microsecond, so the test's own clock is rounded to it.
	clock := time.Now().UTC().Truncate(time.Microsecond)
	ages := []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour}
	seedOwedDocuments(t, database, clock, ages)

	printed := sizingCensus(t, database)
	count := fmt.Sprintf("census: sizing backlog owed_documents=%d dated_by=%s", len(ages), backlogDatedBy)
	if !strings.Contains(printed, count+"\n") {
		t.Errorf("the sizing prints no owed count; want the line\n\t%s\ngot\n%s", count, printed)
	}
	age := fmt.Sprintf("census: sizing backlog oldest_owed_at=%s oldest_owed_age_seconds=%.2f against=%s",
		backlogTime(clock.Add(-ages[0])), ages[0].Seconds(), backlogTime(clock))
	if !strings.Contains(printed, age+"\n") {
		t.Errorf("the sizing prints no age for the oldest owed document; want the line\n\t%s\ngot\n%s", age, printed)
	}
}

// Two runs over one unchanged database print one report, which is what the growth check holds
// the command to (dispatch://INFRA-1149's c-growth diffs the two). An age measured against
// now() would move between the two runs, so the backlog lines carry the oldest debt's own
// timestamp and an age measured against the newest write the database recorded.
func TestTwoSizingRunsOverAnUnchangedDatabaseAgree(t *testing.T) {
	database := storetest.Open(t)
	clock := time.Now().UTC().Truncate(time.Microsecond)
	seedOwedDocuments(t, database, clock, []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour})

	first := sizingCensus(t, database)
	second := sizingCensus(t, database)
	if first != second {
		t.Fatalf("two runs over one unchanged database print different reports:\nfirst\n%s\nsecond\n%s", first, second)
	}
	if !strings.Contains(first, "census: sizing backlog ") {
		t.Fatalf("neither run printed the backlog at all:\n%s", first)
	}
}

// A database owing nothing says so rather than printing a date no row holds, and the age is
// zero against the clock it would be measured from.
func TestTheSizingPrintsNoDateWhereNothingIsOwed(t *testing.T) {
	database := storetest.Open(t)
	printed := sizingCensus(t, database)
	for _, want := range []string{
		fmt.Sprintf("census: sizing backlog owed_documents=0 dated_by=%s\n", backlogDatedBy),
		"census: sizing backlog oldest_owed_at=none oldest_owed_age_seconds=0.00 against=none\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("the sizing of an empty database prints no line\n\t%s\ngot\n%s", want, printed)
		}
	}
}
