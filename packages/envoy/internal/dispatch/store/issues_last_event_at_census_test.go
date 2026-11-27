package store

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// The two migrations of the newest-event pair: the columns, then the fill
// (newestEventFillMigration).
const (
	issuesLastEventAtVersion     = 20261125143901
	issuesLastEventAtFillVersion = 20261125144002
)

// TestTheDeploysCensusAdmitsBothMigrationsOverIssuesWithEvents holds the census a deploy takes
// before it applies the newest-event pair's two migrations (c-fill-census of
// dispatch://INFRA-5791) to admitting both over a store whose issues have events. A census that
// counts a row refuses its migration ("its census counts N"), and the fill writes every issue
// whose last_seq names an event, so a census of the fill that counted those rows would refuse
// every deploy to a store that has events, production's among them.
//
// The database is migrated through the migration before the pair's column migration, the store
// of the release before, and holds issues as that release writes them: each with last_seq > 0 and
// its events up to last_seq. The census is the one a deploy takes (census with the options
// store.Census gives, pgmigrate.CensusOptions{}, so its own limits apply) over the embedded
// migrations through the fill, so its only pending migrations are the pair's two. The report must
// hold exactly those two as pending, each with no refusal and a census count of 0, and end
// `census: ok`; a refusal fails the test with each refusal and the whole report. Then the store's
// runner applies the two over the same set, and each seeded issue must hold last_event_at_seq =
// last_seq and the created_at of its event of last_seq as last_event_at.
func TestTheDeploysCensusAdmitsBothMigrationsOverIssuesWithEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, migrationBefore(t, issuesLastEventAtVersion))
	const project = "CENS"
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx,
		`insert into projects (key, name, default_assignee, next_number) values ($1, 'The census', 'alice', 6)`, project); err != nil {
		t.Fatalf("write the project: %v", err)
	}
	var issues, events [][]any
	for number := 1; number <= 5; number++ {
		key := project + "-" + strconv.Itoa(number)
		issues = append(issues, []any{key, project, number, "Counted " + key, "todo", `{"kind":"user","id":"alice"}`, key, number})
		for seq := 1; seq <= number; seq++ {
			events = append(events, []any{key, seq, "issue.updated", `{"kind":"user","id":"alice"}`, `{}`, false,
				at.Add(time.Duration(number)*time.Hour + time.Duration(seq)*time.Minute)})
		}
	}
	for _, write := range []struct {
		table   string
		columns []string
		rows    [][]any
	}{
		{"issues", []string{"key", "project_key", "number", "title", "status", "created_by", "rank", "last_seq"}, issues},
		{"events", []string{"issue_key", "seq", "type", "actor", "payload", "notify", "created_at"}, events},
	} {
		if _, err := store.Pool.CopyFrom(ctx, pgx.Identifier{write.table}, write.columns, pgx.CopyFromRows(write.rows)); err != nil {
			t.Fatalf("copy the %s rows: %v", write.table, err)
		}
	}

	migrations := migrationsThrough(t, issuesLastEventAtFillVersion)
	report, err := census(ctx, store.Pool.Config().ConnString(), migrations, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	text := reportText(report)
	if refused := refusals(report); len(refused) > 0 {
		t.Fatalf("the deploy's census refuses %d of the pending migrations over %d issues with events:\n  %s\nreport:\n%s",
			len(refused), len(issues), strings.Join(refused, "\n  "), text)
	}
	var pending []int
	for _, entry := range report.Pending {
		pending = append(pending, entry.Migration.Version)
		if entry.Count == nil || *entry.Count != 0 {
			counted := "no count"
			if entry.Count != nil {
				counted = fmt.Sprintf("a count of %d", *entry.Count)
			}
			t.Errorf("%s's census answers %s, want 0; report:\n%s", entry.Migration.Name, counted, text)
		}
	}
	if want := []int{issuesLastEventAtVersion, issuesLastEventAtFillVersion}; !slices.Equal(pending, want) {
		t.Fatalf("the census holds %v pending, want exactly %v; report:\n%s", pending, want, text)
	}
	if !strings.HasSuffix(text, "census: ok\n") {
		t.Fatalf("the census report does not end census: ok:\n%s", text)
	}
	t.Logf("the deploy's census over %d issues with events admits both migrations, each counting 0:\n%s", len(issues), text)

	if err := store.migrate(ctx, migrations); err != nil {
		t.Fatalf("apply the two migrations: %v", err)
	}
	rows, err := store.Pool.Query(ctx, `
		select i.key, i.last_seq, i.last_event_at_seq, i.last_event_at, e.created_at
		from issues i left join events e on e.issue_key = i.key and e.seq = i.last_seq
		where i.project_key = $1
		order by i.key
	`, project)
	if err != nil {
		t.Fatalf("read the pairs: %v", err)
	}
	filled := 0
	for rows.Next() {
		var key string
		var lastSeq int64
		var pairSeq *int64
		var pairAt, eventAt *time.Time
		if err := rows.Scan(&key, &lastSeq, &pairSeq, &pairAt, &eventAt); err != nil {
			rows.Close()
			t.Fatalf("read a pair: %v", err)
		}
		if pairSeq == nil || *pairSeq != lastSeq || pairAt == nil || eventAt == nil || !pairAt.Equal(*eventAt) {
			t.Errorf("after the fill, %s (last_seq %d) holds the pair (last_event_at_seq %s, last_event_at %s), want %d and its event of last_seq's created_at %s",
				key, lastSeq, pointed(pairSeq), pointed(pairAt), lastSeq, pointed(eventAt))
			continue
		}
		filled++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the pairs: %v", err)
	}
	if filled != len(issues) {
		t.Fatalf("after the fill, %d of the %d seeded issues hold the pair of their event of last_seq, want each", filled, len(issues))
	}
	t.Logf("after the two migrations, each of the %d seeded issues holds the pair of its event of last_seq", filled)
}

// migrationBefore is the version of the embedded migration that sorts right before version: the
// store a release that does not carry version left.
func migrationBefore(t *testing.T, version int) int {
	t.Helper()
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, entry := range entries {
		digits, _, _ := strings.Cut(entry.Name(), "_")
		number, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if number < version && number > before {
			before = number
		}
	}
	if before == 0 {
		t.Fatalf("no embedded migration sorts before %d", version)
	}
	return before
}

// pointed is what p points to as a failure prints it, and "null" for nil.
func pointed[T any](p *T) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}
