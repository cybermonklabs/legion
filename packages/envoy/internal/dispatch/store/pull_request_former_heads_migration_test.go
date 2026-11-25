package store

import (
	"context"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// formerHeadsInstall is the version of the migration that makes pull_request_former_heads: the
// install of the record of the heads each open pull request left.
func formerHeadsInstall(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		digits, name, _ := strings.Cut(entry.Name(), "_")
		if name != "pull_request_former_heads.up.sql" {
			continue
		}
		version, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		return version
	}
	t.Fatal("no migration makes pull_request_former_heads")
	return 0
}

// closesRecordedSince is the version of the migration that has Dispatch record each write that
// opens or closes a row of a pull request it stores (pull_request_heads_lifecycle), and each pull
// request closed when it runs, undated: a close and a reopen before it are in no record.
func closesRecordedSince(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if digits, name, _ := strings.Cut(entry.Name(), "_"); name == "pull_request_heads_lifecycle.up.sql" {
			version, err := strconv.Atoi(digits)
			if err != nil {
				t.Fatalf("%s: %v", entry.Name(), err)
			}
			return version
		}
	}
	t.Fatal("no migration has Dispatch record each close of a pull request it stores")
	return 0
}

// TestTheFormerHeadsInstallSeedsTheHeadsAResultWasRecordedAt: before the install, CORE-1's open
// pull request #70 of acme/widgets left 1111… and 2222… and is at 3333…; a tester recorded a result
// of CORE-1 at 1111… and at 3333…, and a CI job one at 2222…. CORE-2's open #71 is at 5555…, and
// CORE-1's closed #72 at 6666…. No trigger saw #70 leave a head, so the install seeds 1111… and
// 2222… as #70's candidates, unverified, and nothing for #71, #72 or the head 3333…, and marks
// CORE-1 owed, so the settler confirms them against #70's commits and publishes their states.
func TestTheFormerHeadsInstallSeedsTheHeadsAResultWasRecordedAt(t *testing.T) {
	const h1, h2, h3, h5, h6 = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333", "5555555555555555555555555555555555555555", "6666666666666666666666666666666666666666"
	ctx := context.Background()
	store := openEmptyTestStore(t)
	install := formerHeadsInstall(t)
	if err := store.migrate(ctx, migrationsThrough(t, install-1)); err != nil {
		t.Fatalf("migrate through %d: %v", install-1, err)
	}
	var document string
	if err := store.Pool.QueryRow(ctx, `
		with project as (
			insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice@d.example') returning key
		), issues as (
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'CORE-' || n, key, n, 'Issue ' || n, '{"kind":"user","id":"alice"}', 'U' || n
			from project, generate_series(1, 2) n returning key
		)
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		select 'CORE-1', 'CORE', 'verification-md', 'verification.md', 'doc', false, 'verification', '{"kind":"user","id":"alice"}'
		from (select count(*) from issues) made
		returning id::text
	`).Scan(&document); err != nil {
		t.Fatalf("seed CORE-1, CORE-2 and CORE-1's Verification document: %v", err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into pull_request_heads (repo, number, head_sha, state, issue_key) values
			('acme/widgets', 70, $1, 'open', 'CORE-1'), ('acme/widgets', 71, $2, 'open', 'CORE-2'),
			('acme/widgets', 72, $3, 'closed', 'CORE-1')`, []any{h3, h5, h6}},
		{`insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, actor)
			select 'CORE-1', 'CORE-1', $1::uuid, 'k1', 1, 'sha', 'tester', commit, 'pass', '{"kind":"user","id":"alice"}'
			from unnest(array[$2, $3]::text[]) commit`, []any{document, h1, h3}},
		{`insert into ci_check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, run, commit_sha, branch, verdict, attestation, session, actor)
			values ('CORE-1', 'CORE-1', $1::uuid, 'k3', 1, 'sha', 'run-1', $2, 'legion/core-1', 'pass', '{}', 'job',
				'{"kind":"system","id":"ci"}')`, []any{document, h2}},
		// The marks the triggers made for these writes; the install's own are what is read below.
		{`delete from check_states_owed`, nil},
	} {
		if _, err := store.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed the database before the install: %s: %v", statement.sql, err)
		}
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate over the seeded database: %v", err)
	}
	rows, err := store.Pool.Query(ctx, `select `+formerHeadName+` || '@' || left(head_sha, 4) from pull_request_former_heads former order by 1`)
	if err != nil {
		t.Fatalf("read the former heads: %v", err)
	}
	recorded, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the former heads: %v", err)
	}
	if want := []string{"acme/widgets#70@1111", "acme/widgets#70@2222"}; !slices.Equal(recorded, want) {
		t.Fatalf("the install recorded the former heads %v, want %v: the heads #70 left before it, at which a result of its issue was recorded", recorded, want)
	}
	var unverified int
	if err := store.Pool.QueryRow(ctx, `select count(*) from pull_request_former_heads where not verified`).Scan(&unverified); err != nil {
		t.Fatalf("count the unverified former heads: %v", err)
	}
	if unverified != 2 {
		t.Fatalf("%d of the seeded former heads are unverified, want both: a result on the issue does not show the commit was #70's", unverified)
	}
	rows, err = store.Pool.Query(ctx, `select distinct issue_key from check_states_owed order by 1`)
	if err != nil {
		t.Fatalf("read the owed issues: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the owed issues: %v", err)
	}
	if want := []string{"CORE-1"}; !slices.Equal(owed, want) {
		t.Fatalf("the install marked %v owed, want %v: the issue whose pull request holds a candidate", owed, want)
	}
}

// Commits of the tests below: heads of acme/widgets#70 and #71, and commits recorded at.
const (
	installH1 = "1111111111111111111111111111111111111111"
	installH2 = "2222222222222222222222222222222222222222"
	installH3 = "3333333333333333333333333333333333333333"
	installH5 = "5555555555555555555555555555555555555555"
)

// installFixture migrates store through the version before the install, and writes project CORE,
// its issues CORE-1, CORE-2 and CORE-3, the Verification documents of CORE-1 and CORE-2, and the
// open pull requests of acme/widgets #70 at 3333…, CORE-1's, and #71 at 5555…, CORE-2's. It
// answers each document's id by its issue, and leaves no mark owed.
func installFixture(t *testing.T, store *Store) map[string]string {
	t.Helper()
	return installFixtureThrough(t, store, formerHeadsInstall(t)-1)
}

// installFixtureThrough is installFixture with store migrated through version alone.
func installFixtureThrough(t *testing.T, store *Store, version int) map[string]string {
	t.Helper()
	ctx := context.Background()
	if err := store.migrate(ctx, migrationsThrough(t, version)); err != nil {
		t.Fatalf("migrate through %d: %v", version, err)
	}
	rows, err := store.Pool.Query(ctx, `
		with project as (
			insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice@d.example') returning key
		), issues as (
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'CORE-' || n, key, n, 'Issue ' || n, '{"kind":"user","id":"alice"}', 'U' || n
			from project, generate_series(1, 3) n returning key
		)
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		select issue, 'CORE', 'verification-md', 'verification.md', 'doc', false, 'verification', '{"kind":"user","id":"alice"}'
		from unnest(array['CORE-1', 'CORE-2']) issue, (select count(*) from issues) made
		returning issue_key, id::text`)
	if err != nil {
		t.Fatalf("seed CORE-1, CORE-2, CORE-3 and the Verification documents of the first two: %v", err)
	}
	documents := map[string]string{}
	for rows.Next() {
		var issue, id string
		if err := rows.Scan(&issue, &id); err != nil {
			t.Fatalf("read the Verification documents: %v", err)
		}
		documents[issue] = id
	}
	if err := rows.Err(); err != nil || len(documents) != 2 {
		t.Fatalf("seed the Verification documents: %v (%d made)", err, len(documents))
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, state, issue_key) values
			('acme/widgets', 70, $1, 'open', 'CORE-1'), ('acme/widgets', 71, $2, 'open', 'CORE-2')`, installH3, installH5); err != nil {
		t.Fatalf("seed the open pull requests: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `delete from check_states_owed`); err != nil {
		t.Fatalf("clear the marks the seed's writes made: %v", err)
	}
	return documents
}

// installResult is the statement that records one result at $4 on issue $1 against a check of
// the Verification document $3 of issue $2: a tester's, in check_results, or a CI job's, in
// ci_check_results.
func installResult(table string) string {
	if table == "ci_check_results" {
		return `insert into ci_check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, run, commit_sha, branch, verdict, attestation, session, actor)
			values ($1, $2, $3::uuid, 'k3', 1, 'sha', 'run-1', $4, 'legion/core', 'pass', '{}', 'job', '{"kind":"system","id":"ci"}')`
	}
	return `insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
		check_sha256, role, commit_sha, verdict, actor)
		values ($1, $2, $3::uuid, 'k1', 1, 'sha', 'tester', $4, 'pass', '{"kind":"user","id":"alice"}')`
}

// formerHeadName is the name of a row of pull_request_former_heads, aliased former, in a test's
// reading: the least name its pull request goes by, and its number.
const formerHeadName = `(select min(alias.repo) || '#' || alias.number from pull_request_former_heads_aliases alias
	where alias.pull = former.pull group by alias.number)`

// formerHeadsAndOwed is each row of pull_request_former_heads as repo#number@first four of its
// commit, its pull request named by the least name it goes by, with " unverified" where it is, and
// each issue holding a mark.
func formerHeadsAndOwed(t *testing.T, store *Store) (heads, owed []string) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.Pool.Query(ctx, `
		select `+formerHeadName+` || '@' || left(head_sha, 4) || case when verified then '' else ' unverified' end
		from pull_request_former_heads former order by 1`)
	if err != nil {
		t.Fatalf("read the former heads: %v", err)
	}
	if heads, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatalf("read the former heads: %v", err)
	}
	if rows, err = store.Pool.Query(ctx, `select distinct issue_key from check_states_owed order by 1`); err != nil {
		t.Fatalf("read the owed issues: %v", err)
	}
	if owed, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatalf("read the owed issues: %v", err)
	}
	return heads, owed
}

// TestTheFormerHeadsInstallSeedsTheResultsTheSettleOfTheIssueReads: before the install, CORE-1's
// open #70 left 1111… for 3333…, and one result was recorded at 1111…. The settle of an issue reads
// every result recorded against the checks of its Verification document, whichever issue recorded
// it (loadCheckResults), so the install seeds the commit for the pull request of the issue whose
// check the result names: CORE-1's #70 for a result against CORE-1's check, recorded on CORE-1 or
// on its child CORE-3, by a tester or by a CI job; CORE-2's #71 for a result CORE-1 recorded
// against its parent CORE-2's check, which CORE-1's settle never reads.
func TestTheFormerHeadsInstallSeedsTheResultsTheSettleOfTheIssueReads(t *testing.T) {
	for _, row := range []struct {
		name, table, recordedOn, checkOf string
		heads, owed                      []string
	}{
		{"a tester's result recorded on the issue against its check", "check_results", "CORE-1", "CORE-1",
			[]string{"acme/widgets#70@1111 unverified"}, []string{"CORE-1"}},
		{"a tester's result recorded on a child against the issue's check", "check_results", "CORE-3", "CORE-1",
			[]string{"acme/widgets#70@1111 unverified"}, []string{"CORE-1"}},
		{"a CI job's result recorded on a child against the issue's check", "ci_check_results", "CORE-3", "CORE-1",
			[]string{"acme/widgets#70@1111 unverified"}, []string{"CORE-1"}},
		{"a result the issue recorded against its parent's check", "check_results", "CORE-1", "CORE-2",
			[]string{"acme/widgets#71@1111 unverified"}, []string{"CORE-2"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEmptyTestStore(t)
			documents := installFixture(t, store)
			if _, err := store.Pool.Exec(ctx, installResult(row.table), row.recordedOn, row.checkOf, documents[row.checkOf], installH1); err != nil {
				t.Fatalf("record the result at 1111…: %v", err)
			}
			if _, err := store.Pool.Exec(ctx, `delete from check_states_owed`); err != nil {
				t.Fatalf("clear the result's mark: %v", err)
			}
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate over the seeded database: %v", err)
			}
			heads, owed := formerHeadsAndOwed(t, store)
			if !slices.Equal(heads, row.heads) || !slices.Equal(owed, row.owed) {
				t.Fatalf("the install recorded the former heads %v and marked %v owed, want %v and %v: the candidates of the pull request of the issue whose settle reads the result", heads, owed, row.heads, row.owed)
			}
		})
	}
}

// TestAResultCommittedWhileTheInstallRunsIsACandidate: before the install, CORE-1's open #70 left
// 1111… and 2222… for 3333…, and no result was recorded at either, so the install's seed finds
// nothing there. The install is held after its statements, before it records its version and
// commits, and meanwhile a tester records a result at 1111… and a CI job one at 2222…. Each writer
// either committed before the install read the result tables or waits for the install's commit;
// either way, once the settle of CORE-1, which they owe, drains what the triggers left it, the
// installed database holds both commits as #70's candidates, unverified, which that settle then
// confirms against #70's commits.
func TestAResultCommittedWhileTheInstallRunsIsACandidate(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	documents := installFixture(t, store)
	connect := func(what string) *pgx.Conn {
		t.Helper()
		conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
		if err != nil {
			t.Fatalf("connect %s: %v", what, err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		return conn
	}
	waiting := func(query string, args ...any) bool {
		t.Helper()
		var held bool
		if err := store.Pool.QueryRow(ctx, query, args...).Scan(&held); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		return held
	}
	holder, err := connect("the holder").Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(ctx, `lock table schema_migrations in share mode`); err != nil {
		t.Fatalf("hold schema_migrations: %v", err)
	}
	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()
	for deadline := time.Now().Add(30 * time.Second); !waiting(`
		select exists (select 1 from pg_locks l join pg_class c on c.oid = l.relation
			where c.relname = 'schema_migrations' and not l.granted)`); {
		if time.Now().After(deadline) {
			t.Fatal("the install never waited to record its version")
		}
		time.Sleep(10 * time.Millisecond)
	}

	type writer struct {
		table, sha string
		pid        uint32
		done       chan error
	}
	writers := []*writer{{table: "check_results", sha: installH1}, {table: "ci_check_results", sha: installH2}}
	for _, w := range writers {
		conn := connect("the writer of " + w.table)
		w.pid, w.done = conn.PgConn().PID(), make(chan error, 1)
		go func() {
			_, err := conn.Exec(ctx, installResult(w.table), "CORE-1", "CORE-1", documents["CORE-1"], w.sha)
			w.done <- err
		}()
	}
	// Each writer has committed, or waits for a lock: the install's, which waits behind the holder.
	for _, w := range writers {
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			select {
			case err := <-w.done:
				if err != nil {
					t.Fatalf("record the result in %s while the install runs: %v", w.table, err)
				}
				w.done <- nil
			default:
			}
			if len(w.done) > 0 {
				t.Logf("the writer of %s committed while the install waited to record its version", w.table)
				break
			}
			if waiting(`select exists (select 1 from pg_locks where pid = $1 and not granted)`, w.pid) {
				t.Logf("the writer of %s waits for a lock while the install waits to record its version", w.table)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the writer of %s neither committed nor waited for a lock", w.table)
			}
		}
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the install: %v", err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, w := range writers {
		if err := <-w.done; err != nil {
			t.Fatalf("record the result in %s while the install runs: %v", w.table, err)
		}
	}
	drainCandidates(t, store, "CORE-1")
	heads, owed := formerHeadsAndOwed(t, store)
	if want := []string{"acme/widgets#70@1111 unverified", "acme/widgets#70@2222 unverified"}; !slices.Equal(heads, want) {
		t.Fatalf("the installed database holds the former heads %v, want %v: a result committed while the install ran leaves no candidate, so no settle publishes the head #70 left", heads, want)
	}
	if !slices.Contains(owed, "CORE-1") {
		t.Fatalf("the installed database marks %v owed, want CORE-1, whose settle confirms the candidates", owed)
	}
}

// TestAResultAfterTheInstallAtAHeadLeftBeforeItIsACandidate: CORE-1's open #70 left 1111… and
// 2222… for 3333… before the install, with no result at either, and the install seeds nothing. A
// head move before the install is in no write a trigger saw, so a result recorded after it at a
// commit that is no head of #70 Dispatch knows leaves that commit as a candidate, unverified, once
// the settle of CORE-1 that it owes drains it, and that settle then confirms it: a tester's at
// 1111… recorded on CORE-1 and a CI job's at
// 2222… recorded on its child CORE-3, each against CORE-1's check. A result at #70's head
// leaves none, and one at that head against CORE-2's check leaves it to CORE-2's #71, not to #70.
func TestAResultAfterTheInstallAtAHeadLeftBeforeItIsACandidate(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	documents := installFixture(t, store)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if heads, _ := formerHeadsAndOwed(t, store); len(heads) != 0 {
		t.Fatalf("the install seeded %v with no result recorded, want nothing", heads)
	}
	for _, result := range []struct{ table, recordedOn, checkOf, sha string }{
		{"check_results", "CORE-1", "CORE-1", installH1},
		{"ci_check_results", "CORE-3", "CORE-1", installH2},
		{"check_results", "CORE-1", "CORE-1", installH3},
		{"check_results", "CORE-1", "CORE-2", installH3},
	} {
		if _, err := store.Pool.Exec(ctx, installResult(result.table), result.recordedOn, result.checkOf, documents[result.checkOf], result.sha); err != nil {
			t.Fatalf("record a result at %s…: %v", result.sha[:4], err)
		}
	}
	drainCandidates(t, store, "CORE-1", "CORE-2")
	heads, owed := formerHeadsAndOwed(t, store)
	if want := []string{"acme/widgets#70@1111 unverified", "acme/widgets#70@2222 unverified", "acme/widgets#71@3333 unverified"}; !slices.Equal(heads, want) {
		t.Fatalf("after the results the former heads are %v, want %v: each result's commit a candidate of the open pull request of the issue whose settle reads it, but where it is that pull request's head", heads, want)
	}
	if !slices.Contains(owed, "CORE-1") {
		t.Fatalf("the results marked %v owed, want CORE-1", owed)
	}
}

// TestTheFormerHeadsInstallWaitsOutABusyWriterAndGivesWayInACycle: the install locks
// pull_request_heads, check_results and ci_check_results, and holds them to its commit.
//
// A writer that holds one of them in an open transaction for longer than the half second the
// install waits at one time is waited out as any migration waits one out: the install gives back
// what its try took each half second and asks again, for as long as the runner's LockTimeout, and
// the runner then applies it again. A writer that holds check_results for eight seconds, longer
// than the three attempts of half a second the runner makes in all, is waited out, and the install
// applies once it commits.
//
// The delete of an issue cascades into the result tables and pull_request_heads in an order of its
// own: here a transaction holds check_results, as that cascade does, while the install holds
// pull_request_heads and waits for check_results, and then it asks for pull_request_heads. The
// install gives up its try within half a second, before Postgres's deadlock check runs on either
// side, so that transaction gets its lock and commits with no error, and the install applies.
func TestTheFormerHeadsInstallWaitsOutABusyWriterAndGivesWayInACycle(t *testing.T) {
	ctx := context.Background()
	begin := func(t *testing.T, store *Store, what string) pgx.Tx {
		t.Helper()
		conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
		if err != nil {
			t.Fatalf("connect %s: %v", what, err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	installed := func(t *testing.T, store *Store) {
		t.Helper()
		var made bool
		if err := store.Pool.QueryRow(ctx, `select to_regclass('pull_request_former_heads') is not null`).Scan(&made); err != nil || !made {
			t.Fatalf("the install made pull_request_former_heads: %v (%v)", made, err)
		}
	}

	t.Run("a writer busy for eight seconds", func(t *testing.T) {
		store := openEmptyTestStore(t)
		installFixture(t, store)
		writer := begin(t, store, "the writer")
		if _, err := writer.Exec(ctx, `lock table check_results in row exclusive mode`); err != nil {
			t.Fatalf("hold check_results as a writer does: %v", err)
		}
		started := time.Now()
		migrated := make(chan error, 1)
		go func() { migrated <- store.Migrate(ctx) }()
		select {
		case err := <-migrated:
			t.Fatalf("the install ended %s after it started, while a writer still held check_results: %v; want it to wait the writer out", time.Since(started).Round(time.Millisecond), err)
		case <-time.After(8 * time.Second):
		}
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("commit the writer: %v", err)
		}
		if err := <-migrated; err != nil {
			t.Fatalf("the install after the writer committed: %v", err)
		}
		installed(t, store)
		t.Logf("the install applied %s after it started", time.Since(started).Round(time.Millisecond))
	})

	t.Run("a delete's cascade that closes a cycle", func(t *testing.T) {
		store := openEmptyTestStore(t)
		installFixture(t, store)
		cascade := begin(t, store, "the cascade")
		if _, err := cascade.Exec(ctx, `lock table check_results in row exclusive mode`); err != nil {
			t.Fatalf("hold check_results as the cascade does: %v", err)
		}
		migrated := make(chan error, 1)
		go func() { migrated <- store.Migrate(ctx) }()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			var waits bool
			if err := store.Pool.QueryRow(ctx, `
				select exists (select 1 from pg_locks l join pg_class c on c.oid = l.relation
					where c.relname = 'check_results' and l.mode = 'ShareRowExclusiveLock' and not l.granted)`).Scan(&waits); err != nil {
				t.Fatalf("read the lock waits: %v", err)
			}
			if waits {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the install never waited for check_results")
			}
		}
		asked := time.Now()
		if _, err := cascade.Exec(ctx, `lock table pull_request_heads in row exclusive mode`); err != nil {
			t.Fatalf("the cascade's lock of pull_request_heads, which the install held while it waited for check_results: %v; want the install to give way first", err)
		}
		t.Logf("the cascade had its lock of pull_request_heads %s after it asked", time.Since(asked).Round(time.Millisecond))
		if err := cascade.Commit(ctx); err != nil {
			t.Fatalf("commit the cascade: %v", err)
		}
		if err := <-migrated; err != nil {
			t.Fatalf("the install after the cascade committed: %v", err)
		}
		installed(t, store)
	})
}

// TestAResultAndARowThatStartsAtItsIssueAtOnceLeaveTheCandidate: after the install, a tester's
// result at 1111… against CORE-1's check and the first write of CORE-1's #72 at 2222… run in two
// transactions at once. Neither trigger reads what the other wrote, and neither waits for the
// other: each appends what it owes CORE-1, and the second commits while the first is still open.
// The settle of CORE-1, which starts after both committed, drains both. Either way #72 ends with
// 1111… as a candidate, unverified.
func TestAResultAndARowThatStartsAtItsIssueAtOnceLeaveTheCandidate(t *testing.T) {
	ctx := context.Background()
	for _, resultFirst := range []bool{false, true} {
		name := "the row starts first"
		if resultFirst {
			name = "the result is recorded first"
		}
		t.Run(name, func(t *testing.T) {
			store := openEmptyTestStore(t)
			documents := installFixture(t, store)
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			connect := func(what string) *pgx.Conn {
				t.Helper()
				conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
				if err != nil {
					t.Fatalf("connect %s: %v", what, err)
				}
				t.Cleanup(func() { conn.Close(context.Background()) })
				return conn
			}
			row := func(db interface {
				Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
			}) error {
				_, err := db.Exec(ctx, `insert into pull_request_heads (repo, number, head_sha, state, issue_key)
					values ('acme/widgets', 72, $1, 'open', 'CORE-1')`, installH2)
				return err
			}
			result := func(db interface {
				Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
			}) error {
				_, err := db.Exec(ctx, installResult("check_results"), "CORE-1", "CORE-1", documents["CORE-1"], installH1)
				return err
			}
			first, second := row, result
			if resultFirst {
				first, second = result, row
			}
			held, err := connect("the first writer").Begin(ctx)
			if err != nil {
				t.Fatalf("begin the first writer: %v", err)
			}
			defer held.Rollback(context.Background())
			if err := first(held); err != nil {
				t.Fatalf("the first write: %v", err)
			}
			conn := connect("the second writer")
			pid, done := conn.PgConn().PID(), make(chan error, 1)
			go func() { done <- second(conn) }()
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if len(done) > 0 {
					t.Log("the second write committed while the first was open")
					break
				}
				var waits bool
				if err := store.Pool.QueryRow(ctx, `select exists (select 1 from pg_locks where pid = $1 and not granted)`, pid).Scan(&waits); err != nil {
					t.Fatalf("read the lock waits: %v", err)
				}
				if waits {
					t.Fatal("the second write waits for a lock while the first is open: a trigger makes one writer wait for the other")
				}
				if time.Now().After(deadline) {
					t.Fatal("the second write did not commit within 10 s while the first was open")
				}
			}
			if err := held.Commit(ctx); err != nil {
				t.Fatalf("commit the first write: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("the second write: %v", err)
			}
			drainCandidates(t, store, "CORE-1")
			heads, _ := formerHeadsAndOwed(t, store)
			if !slices.Contains(heads, "acme/widgets#72@1111 unverified") {
				t.Fatalf("after a result at 1111… and #72's start at CORE-1, written at once, and CORE-1's drain, the former heads are %v, want #72@1111 unverified among them", heads)
			}
		})
	}
}

// TestAResultAndAHeadWriteInTheirOwnLockOrdersBothCommit: the two writers take CORE-1's owner lock
// in opposite orders. A result write takes the owner locks first and then inserts its row
// (api/results.go, recordResultUnderOwnerLocks). The head writer writes its row first and then
// takes the owner locks of the issues the pull request left and reached
// (delivery.UpsertPullRequestHead, appendAttributionChange). The head write moves #71 from CORE-2
// to CORE-1 at the head it stands at, or, with #71 at CORE-1 before, moves it to CORE-2 at a new
// head; a result against CORE-1's check is recorded between that row write and that owner lock.
// No trigger of the former heads may make either writer wait for the other while it holds what
// the other needs: both commit with no error, and where #71 moved to CORE-1, it holds the result's
// commit as a candidate once CORE-1 settles.
func TestAResultAndAHeadWriteInTheirOwnLockOrdersBothCommit(t *testing.T) {
	ctx := context.Background()
	for _, leaves := range []bool{false, true} {
		name := "the row moves to the issue"
		if leaves {
			name = "the row leaves the issue at a new head"
		}
		t.Run(name, func(t *testing.T) {
			store := openEmptyTestStore(t)
			documents := installFixture(t, store)
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			begin := func(what string) pgx.Tx {
				t.Helper()
				conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
				if err != nil {
					t.Fatalf("connect %s: %v", what, err)
				}
				t.Cleanup(func() { conn.Close(context.Background()) })
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatalf("begin %s: %v", what, err)
				}
				t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
				return tx
			}
			move := `update pull_request_heads set issue_key = 'CORE-1' where repo = 'acme/widgets' and number = 71`
			if leaves {
				if _, err := store.Pool.Exec(ctx, move); err != nil {
					t.Fatalf("move #71 to CORE-1 before: %v", err)
				}
				move = `update pull_request_heads set issue_key = 'CORE-2', head_sha = '` + installH2 + `'
					where repo = 'acme/widgets' and number = 71`
			}
			writer := begin("the head writer")
			if _, err := writer.Exec(ctx, move); err != nil {
				t.Fatalf("the head writer's row write: %v", err)
			}
			recorder := begin("the result writer")
			if err := LockOwners(ctx, recorder, IssueOwner("CORE-1")); err != nil {
				t.Fatalf("the result writer's owner lock: %v", err)
			}
			recorded := make(chan error, 1)
			go func() {
				_, err := recorder.Exec(ctx, installResult("check_results"), "CORE-1", "CORE-1", documents["CORE-1"], installH1)
				recorded <- err
			}()
			owned := make(chan error, 1)
			go func() { owned <- LockOwners(ctx, writer, IssueOwner("CORE-1"), IssueOwner("CORE-2")) }()
			if err := <-recorded; err != nil {
				t.Fatalf("the result writer's insert, between the head writer's row write and its owner lock: %v; want no wait for the head writer", err)
			}
			if err := recorder.Commit(ctx); err != nil {
				t.Fatalf("commit the result writer: %v", err)
			}
			if err := <-owned; err != nil {
				t.Fatalf("the head writer's owner lock after the result committed: %v", err)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("commit the head writer: %v", err)
			}
			if leaves {
				return
			}
			drainCandidates(t, store, "CORE-1")
			if heads, _ := formerHeadsAndOwed(t, store); !slices.Contains(heads, "acme/widgets#71@1111 unverified") {
				t.Fatalf("after both writers committed and CORE-1 settled, the former heads are %v, want #71@1111 unverified among them", heads)
			}
		})
	}
}

// TestEveryFormerHeadsTriggerOnAWritersTableOnlyReadsAndAppends: a trigger on a writer's table
// runs in that writer's transaction, so a lock it takes is the writer's lock. A writer that holds
// such a lock while another writer, holding a lock the first needs next, waits for it closes a
// cycle with the owner locks and the rows the writers take in their own orders, and Postgres breaks
// it by failing one writer (TestAResultAndAHeadWriteInTheirOwnLockOrdersBothCommit is one such
// pair). So each function a former heads trigger, or a trigger of the record of opens and closes
// (pull_request_heads_lifecycle), on pull_request_heads, check_results or ci_check_results runs only
// reads and appends: its body names no row lock, no table lock and no call of a function whose
// name holds "lock", updates and deletes nothing, inserts with no on conflict, and calls none of
// the functions the settler folds, drains or leaves candidates with; and each table it inserts
// into has no primary, unique, exclusion or foreign key, and no unique index, so an insert there
// waits for no other transaction. The one function those bodies call that writes,
// check_states_owe, inserts a mark whose key a sequence draws, and waits for nothing either. The
// rule holds at the schema through the install, where the record of opens and closes still has its
// triggers, as a deploy stopped before the migration that drops them leaves them, and once every
// migration has applied.
func TestEveryFormerHeadsTriggerOnAWritersTableOnlyReadsAndAppends(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	for _, stage := range []struct {
		name    string
		migrate func()
		closes  bool
	}{
		{"through the install", func() { migrateThrough(t, store, formerHeadsInstall(t)) }, true},
		{"every migration", func() {
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
		}, false},
	} {
		stage.migrate()
		rows, err := store.Pool.Query(ctx, `
			select distinct watched.relname::text, run.proname::text, run.prosrc
			from pg_trigger fired
			join pg_class watched on watched.oid = fired.tgrelid
			join pg_proc run on run.oid = fired.tgfoid
			where not fired.tgisinternal and watched.relname in ('pull_request_heads', 'check_results', 'ci_check_results')
				and (run.proname like 'pull\_request\_former\_heads\_%' or run.proname like 'pull\_request\_heads\_lifecycle\_%')
			order by 1, 2`)
		if err != nil {
			t.Fatalf("%s: read the former heads triggers: %v", stage.name, err)
		}
		type triggerFunction struct{ table, name, body string }
		functions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerFunction, error) {
			var f triggerFunction
			return f, row.Scan(&f.table, &f.name, &f.body)
		})
		if err != nil {
			t.Fatalf("%s: read the former heads triggers: %v", stage.name, err)
		}
		watched := map[string]bool{}
		closes := false
		for _, f := range functions {
			watched[f.table] = true
			closes = closes || f.name == "pull_request_heads_lifecycle_keep"
		}
		if stage.closes && !closes {
			t.Errorf("%s: found no trigger of the record of opens and closes on pull_request_heads, so this test holds it to nothing", stage.name)
		}
		for _, table := range []string{"pull_request_heads", "check_results", "ci_check_results"} {
			if !watched[table] {
				t.Errorf("%s: found no former heads trigger on %s, so this test holds nothing there to the rule", stage.name, table)
			}
		}
		waits := regexp.MustCompile(`(?i)\bfor\s+(no\s+key\s+update|update|key\s+share|share)\b|\block\s+table\b|\b[a-z_]*lock[a-z_]*\s*\(|\bon\s+conflict\b|\bupdate\s+[a-z_.]+\s+set\b|\bdelete\s+from\b|\bpull_request_former_heads_(fold|drain|leave|leave_again|apply|resolve|join)\s*\(`)
		inserts := regexp.MustCompile(`(?i)\binsert\s+into\s+([a-z_][a-z0-9_]*)`)
		appended := map[string]bool{}
		for _, f := range functions {
			if found := waits.FindString(f.body); found != "" {
				t.Errorf("%s: %s, which a trigger on %s runs, holds %q: a writer that runs it can wait for a lock another writer holds", stage.name, f.name, f.table, found)
			}
			for _, insert := range inserts.FindAllStringSubmatch(f.body, -1) {
				appended[strings.ToLower(insert[1])] = true
			}
		}
		if len(appended) == 0 {
			t.Fatalf("%s: the former heads trigger functions insert into no table, so this test holds no table to the rule", stage.name)
		}
		for table := range appended {
			var keys int
			if err := store.Pool.QueryRow(ctx, `
				select (select count(*) from pg_constraint where conrelid = $1::regclass and contype in ('p', 'u', 'x', 'f'))
					+ (select count(*) from pg_index where indrelid = $1::regclass and indisunique)`, table).Scan(&keys); err != nil {
				t.Fatalf("%s: read the keys of %s: %v", stage.name, table, err)
			}
			if keys != 0 {
				t.Errorf("%s: a former heads trigger inserts into %s, which holds %d keys or unique indexes: an insert there can wait for another transaction's insert of the same key", stage.name, table, keys)
			}
		}
	}
}

// drainCandidates runs what a settle of each of issues runs first (pull_request_former_heads_drain):
// it folds the records of each open pull request of the issue, and leaves the candidates the
// writes since the last settle owe the issue's open rows.
func drainCandidates(t *testing.T, store *Store, issues ...string) {
	t.Helper()
	for _, issue := range issues {
		if _, err := store.Pool.Exec(context.Background(), `select pull_request_former_heads_drain($1)`, issue); err != nil {
			t.Fatalf("drain the candidates of %s: %v", issue, err)
		}
	}
}
