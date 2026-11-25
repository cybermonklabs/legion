//go:build basebuild

package api

// The database time of the reads c-read-cost and c-moved-read-cost measure: what Postgres spent
// executing the statements one reading of a read sends. It is read from pg_stat_statements as the
// sum of total_exec_time over the statements of a batch of readings, from a snapshot before the
// batch and one after it, divided by the batch's readings. It is printed beside each read's
// percentiles and in the tables. c-read-cost binds nothing on it; c-moved-read-cost holds each
// read's on the new side and on the third side to twice its old side's.
//
// pg_stat_statements holds every statement of the server, so what is not the read's is kept out
// of the sum, or printed beside it, as follows.
//
//   - Every other database's statements, those of every other suite on a shared server among them:
//     a snapshot reads the run's database alone (dbid), and the run's role alone (userid), the role
//     both builds connect as.
//   - The test's own statements: the snapshots are read over a connection to a database of their
//     own, created beside the run's database and dropped with it, so each snapshot is recorded
//     under that database and never under the run's. The test's other connection to the run's
//     database (readCostConnect) sends nothing while a batch runs.
//   - A statement counted twice: under pg_stat_statements.track=all a statement that a function
//     or a trigger runs is recorded beside the statement that ran it, whose time already holds it,
//     so only top-level statements are summed (toplevel).
//   - The server's background work that a setting stops is stopped on every build that has the
//     setting (startReadCostServer): the keyword fill (DISPATCH_SEARCH_FILL=off), the embedding
//     queue (DISPATCH_TEST_EMBEDDER_OFF=1) and the maintenance sweep, whose first tick then falls
//     a day after the boot (DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL). The CI reconcile, the
//     webhook redelivery and the delivery reconcile do not run at all, since they need NATS or
//     the GitHub App and neither build is given either.
//   - The background work no setting stops: the process heartbeat, the leased loops' ticks, the
//     block index's tick and its embedding windows, the search term statistics rebuilt at boot
//     and every ten minutes, the display-tree fill and the other reconciliations. They send their
//     statements on schedules of their own, so the read's time is the time of the statements that
//     ran at least once per reading of the batch, as every statement the read sends does; the
//     statements that ran fewer times are printed beside it, with their time. After the batch, an
//     idle interval of the batch's length is read in the same way on the same server while
//     nothing reads, and printed beside both: its whole time per reading of the batch, and the
//     part of it in statements that ran at least as often as the batch has readings, which is how
//     much background work the first figure would count as the read's over that length.
//
// The noise that remains: background work that runs a statement at least once per reading during
// a batch (the window arm, while windows are owed) is counted as the read's, which the idle
// interval's second figure estimates; the background calls of a statement the read sends as well
// (COMMIT, which every transaction shares) are counted as the read's; a statement the read sends
// less than once per reading (the refresh of a cached answer) is printed beside the read's time
// and not in it; planning time is not total_exec_time and is not counted; and an entry that
// pg_stat_statements evicts during a batch takes its time with it, which the read's line says
// where pg_stat_statements_info counts an eviction.
//
// Beside each read's figure the run prints the statements behind it, the readCostTopStatements
// slowest per reading, each with its queryid, its calls, its time, its shared blocks hit and its
// shared blocks read per reading (shared_blks_hit, shared_blks_read, taken in the same snapshots
// as the time) and the head of its text (logReadCostStatements), so a reader can see which
// statements one side adds or slows, and whether a slower one read more blocks or only took longer
// over the same ones. Nothing is bound on the blocks either. The texts are read once, after the
// idle interval, and only for those statements.
//
// pg_stat_statements needs the server to preload it (shared_preload_libraries) and a role that
// may create the extension. Where either is missing, c-read-cost says in one line that database
// time was not measured and why, and measures everything else, failing on none of it
// (newReadCostStatements); c-moved-read-cost fails before it builds anything, naming why and each
// read whose bound it cannot read (openReadCostStatements).

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// readCostStatements reads pg_stat_statements for the top-level statements of the run's database
// and role, over a connection to a database of its own. A nil one measures nothing.
type readCostStatements struct {
	check    string
	conn     *pgx.Conn
	database uint32 // the oid of the run's database, every snapshot's dbid
	role     uint32 // the oid of the role the run and both builds connect as, every snapshot's userid
}

// readCostSnapshot is pg_stat_statements at one moment: each top-level statement of the run's
// database and role by its queryid, and how many entries the extension had evicted until then
// (pg_stat_statements_info.dealloc).
type readCostSnapshot struct {
	at         time.Time
	statements map[int64]readCostStatementTotal
	evicted    int64
}

// readCostStatementTotal is one statement's calls, its total_exec_time in milliseconds and its
// shared blocks hit and read (shared_blks_hit, shared_blks_read).
type readCostStatementTotal struct {
	calls     int64
	ms        float64
	hit, read int64
}

// readCostDatabaseTime is the database time of one interval per reading of the batch it is read
// against: the statements that ran at least once per reading, and beside them those that ran
// fewer times.
type readCostDatabaseTime struct {
	readings int
	length   time.Duration
	// ms and calls are the time and the calls per reading of the statements that ran at least
	// once per reading, hit and read their shared blocks hit and read per reading, and statements
	// how many statements those are.
	ms, calls, hit, read float64
	statements           int
	// top are the readCostTopStatements of those statements that took the most time, the slowest
	// first.
	top []readCostStatementTime
	// otherMS is the time per reading of the statements that ran fewer times, and others how many
	// statements those are.
	otherMS float64
	others  int
	evicted int64
}

// readCostTopStatements is how many of the statements behind a read's database time are printed,
// and readCostStatementText how many characters of each one's text.
const (
	readCostTopStatements = 12
	readCostStatementText = 160
)

// readCostStatementTime is one statement behind a read's database time: its queryid, its calls,
// its time and its shared blocks hit and read per reading, and the first readCostStatementText
// characters of its text with its whitespace collapsed, "" until it is read.
type readCostStatementTime struct {
	query                int64
	calls, ms, hit, read float64
	text                 string
}

// openReadCostStatements opens what database time is read with, for the run's database at
// databaseURL: a database of its own beside it, dropped when the test ends, holding the
// extension. Where pg_stat_statements cannot be read it answers why, and no reader.
func openReadCostStatements(t *testing.T, check, databaseURL string) (*readCostStatements, error) {
	t.Helper()
	run, err := url.Parse(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("read the run's database URL: %w", err)
	}
	database := strings.TrimPrefix(run.Path, "/")
	own := database + "_statements"
	admin, statements := *run, *run
	admin.Path, statements.Path = "/postgres", "/"+own
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return nil, fmt.Errorf("connect to the test server: %w", err)
	}
	_, err = conn.Exec(ctx, "create database "+own)
	_ = conn.Close(ctx)
	if err != nil {
		return nil, fmt.Errorf("create %s, the database the snapshots are read from: %w", own, err)
	}
	t.Cleanup(func() { dropReadCostDatabase(t, admin.String(), own) })
	s := &readCostStatements{check: check}
	if s.conn, err = pgx.Connect(ctx, statements.String()); err != nil {
		return nil, fmt.Errorf("connect to %s: %w", own, err)
	}
	t.Cleanup(func() { _ = s.conn.Close(context.Background()) })
	if _, err := s.conn.Exec(ctx, `create extension if not exists pg_stat_statements`); err != nil {
		return nil, fmt.Errorf("create extension pg_stat_statements in %s: %w", own, err)
	}
	var track string
	if err := s.conn.QueryRow(ctx, `select (select oid from pg_database where datname = $1),
		(select oid from pg_roles where rolname = current_user), coalesce(current_setting('pg_stat_statements.track', true), '')`,
		database).Scan(&s.database, &s.role, &track); err != nil {
		return nil, fmt.Errorf("read the oids of the run's database %s and of its role: %w", database, err)
	}
	if _, err := s.snapshot(ctx); err != nil {
		return nil, err
	}
	if track != "top" && track != "all" {
		return nil, fmt.Errorf("pg_stat_statements.track is %q, so it records no statement", track)
	}
	t.Logf("%s: database time is read from pg_stat_statements (track=%s) for the top-level statements of the run's database %s "+
		"(oid %d) and its role (oid %d), over a connection to %s", check, track, database, s.database, s.role, own)
	return s, nil
}

// newReadCostStatements is openReadCostStatements for a run that binds nothing on database time:
// where pg_stat_statements cannot be read it says so in one line, naming why, and answers nil, and
// the run goes on without database time.
func newReadCostStatements(t *testing.T, check, databaseURL string) *readCostStatements {
	t.Helper()
	s, err := openReadCostStatements(t, check, databaseURL)
	if err != nil {
		t.Logf("%s: database time was not measured: %v; it needs pg_stat_statements in the server's shared_preload_libraries and "+
			"a role that may create the extension, and the run measures everything else and fails on none of it", check, err)
	}
	return s
}

// snapshot reads pg_stat_statements for the run's database and role.
func (s *readCostStatements) snapshot(ctx context.Context) (readCostSnapshot, error) {
	snapshot := readCostSnapshot{at: time.Now(), statements: map[int64]readCostStatementTotal{}}
	if err := s.conn.QueryRow(ctx, `select dealloc from pg_stat_statements_info`).Scan(&snapshot.evicted); err != nil {
		return snapshot, fmt.Errorf("read pg_stat_statements_info: %w", err)
	}
	rows, err := s.conn.Query(ctx, `select queryid, calls, total_exec_time, shared_blks_hit, shared_blks_read
		from pg_stat_statements(false) where dbid = $1 and userid = $2 and toplevel`, s.database, s.role)
	if err != nil {
		return snapshot, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var query int64
		var total readCostStatementTotal
		if err := rows.Scan(&query, &total.calls, &total.ms, &total.hit, &total.read); err != nil {
			return snapshot, fmt.Errorf("read pg_stat_statements: %w", err)
		}
		snapshot.statements[query] = total
	}
	if err := rows.Err(); err != nil {
		return snapshot, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	return snapshot, nil
}

// measure runs batch, which takes readings readings, between two snapshots, and then waits an idle
// interval of the batch's length on the same server, with nothing reading, before a third. It
// answers the database time of the batch and of the idle interval, each per reading of the batch,
// and then reads the texts of the statements behind the batch's time (readTexts). Either is nil
// where s is nil or a snapshot failed, which it says, failing nothing; a text it cannot read it
// says it did not read, and prints none.
func (s *readCostStatements) measure(t *testing.T, readings int, batch func()) (database, idle *readCostDatabaseTime) {
	t.Helper()
	if s == nil {
		batch()
		return nil, nil
	}
	ctx := context.Background()
	before, err := s.snapshot(ctx)
	if err != nil {
		t.Logf("%s: the database time of this batch was not measured: %v", s.check, err)
		batch()
		return nil, nil
	}
	batch()
	after, err := s.snapshot(ctx)
	if err != nil {
		t.Logf("%s: the database time of this batch was not measured: %v", s.check, err)
		return nil, nil
	}
	database = readCostDatabaseTimeOf(before, after, readings)
	time.Sleep(database.length)
	if rested, err := s.snapshot(ctx); err != nil {
		t.Logf("%s: the database time of the idle interval after this batch was not measured: %v", s.check, err)
	} else {
		idle = readCostDatabaseTimeOf(after, rested, readings)
	}
	if err := s.readTexts(ctx, database.top); err != nil {
		t.Logf("%s: the texts of the statements behind this batch's database time were not read: %v", s.check, err)
	}
	return database, idle
}

// readTexts reads the text of each statement of top from pg_stat_statements, which the snapshots
// leave out (showtext false) since only these few are printed.
func (s *readCostStatements) readTexts(ctx context.Context, top []readCostStatementTime) error {
	if len(top) == 0 {
		return nil
	}
	queries := make([]int64, len(top))
	for index, statement := range top {
		queries[index] = statement.query
	}
	rows, err := s.conn.Query(ctx, `select queryid, coalesce(query, '') from pg_stat_statements(true)
		where dbid = $1 and userid = $2 and toplevel and queryid = any($3)`, s.database, s.role, queries)
	if err != nil {
		return fmt.Errorf("read pg_stat_statements: %w", err)
	}
	defer rows.Close()
	texts := make(map[int64]string, len(top))
	for rows.Next() {
		var query int64
		var text string
		if err := rows.Scan(&query, &text); err != nil {
			return fmt.Errorf("read pg_stat_statements: %w", err)
		}
		texts[query] = text
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read pg_stat_statements: %w", err)
	}
	for index := range top {
		text := strings.Join(strings.Fields(texts[top[index].query]), " ")
		if characters := []rune(text); len(characters) > readCostStatementText {
			text = string(characters[:readCostStatementText])
		}
		top[index].text = text
	}
	return nil
}

// readCostDatabaseTimeOf is the database time between two snapshots per reading of a batch of
// readings: the statements that ran at least readings times, with the readCostTopStatements of
// them that took the most time, and apart from them those that ran fewer times. A statement with
// fewer calls than before was evicted and recorded again since, so all it holds is the interval's.
func readCostDatabaseTimeOf(before, after readCostSnapshot, readings int) *readCostDatabaseTime {
	spent := &readCostDatabaseTime{readings: readings, length: after.at.Sub(before.at), evicted: after.evicted - before.evicted}
	var counted []readCostStatementTime
	for query, now := range after.statements {
		was := before.statements[query]
		if now.calls < was.calls {
			was = readCostStatementTotal{}
		}
		switch calls, ms, hit, read := now.calls-was.calls, now.ms-was.ms, now.hit-was.hit, now.read-was.read; {
		case calls >= int64(readings):
			spent.ms += ms
			spent.calls += float64(calls)
			spent.hit += float64(hit)
			spent.read += float64(read)
			counted = append(counted, readCostStatementTime{query: query, calls: float64(calls) / float64(readings), ms: ms / float64(readings),
				hit: float64(hit) / float64(readings), read: float64(read) / float64(readings)})
		case calls > 0:
			spent.otherMS += ms
			spent.others++
		}
	}
	spent.ms /= float64(readings)
	spent.calls /= float64(readings)
	spent.hit /= float64(readings)
	spent.read /= float64(readings)
	spent.otherMS /= float64(readings)
	slices.SortFunc(counted, func(one, other readCostStatementTime) int {
		return cmp.Or(cmp.Compare(other.ms, one.ms), cmp.Compare(one.query, other.query))
	})
	spent.statements = len(counted)
	spent.top = counted[:min(len(counted), readCostTopStatements)]
	return spent
}

// logReadCostStatements prints the statements behind the database time of read on side, one per
// line, the slowest first: each one's queryid, its calls, its milliseconds, its shared blocks hit
// and its shared blocks read per reading, and the head of its text. It says so where the time was
// not measured.
func logReadCostStatements(t *testing.T, check, read, side string, database *readCostDatabaseTime) {
	t.Helper()
	if database == nil {
		t.Logf("%s: the statements of %s on the %s: its database time was not measured", check, read, side)
		return
	}
	t.Logf("%s: the statements of %s on the %s: the %d that took the most time of the %d that ran at least %d times, "+
		"%.2f ms, %.1f shared blocks hit and %.1f shared blocks read per reading in all; queryid, calls per reading, ms per reading, "+
		"shared blocks hit per reading, shared blocks read per reading, text", check, read, side, len(database.top),
		database.statements, database.readings, database.ms, database.hit, database.read)
	for _, statement := range database.top {
		t.Logf("%s: %s, %s: %d\t%.1f\t%.3f\t%.1f\t%.1f\t%s", check, read, side, statement.query, statement.calls, statement.ms,
			statement.hit, statement.read, statement.text)
	}
}

// readCostDatabaseLine is the database time a read's line prints beside its percentiles, and
// nothing where it was not measured.
func readCostDatabaseLine(database, idle *readCostDatabaseTime) string {
	if database == nil {
		return ""
	}
	line := fmt.Sprintf("; database time %.2f ms per reading in %.1f statements, beside %.2f ms per reading in %d statements that ran "+
		"fewer than %d times", database.ms, database.calls, database.otherMS, database.others, database.readings)
	evicted := database.evicted
	if idle != nil {
		line += fmt.Sprintf(", and %.2f ms per reading over an idle %s after the batch, %.2f ms of it in statements that ran %d times or more",
			idle.ms+idle.otherMS, idle.length.Round(time.Millisecond), idle.ms, idle.readings)
		evicted += idle.evicted
	}
	if evicted > 0 {
		line += fmt.Sprintf("; pg_stat_statements evicted %d entries meanwhile, and the time they held is not counted", evicted)
	}
	return line
}

// readCell is a table's cell for the read's database time per reading, "-" where it was not
// measured.
func (spent *readCostDatabaseTime) readCell() string {
	if spent == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", spent.ms)
}

// idleCell is a table's cell for an idle interval's whole database time per reading, "-" where it
// was not measured.
func (spent *readCostDatabaseTime) idleCell() string {
	if spent == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", spent.ms+spent.otherMS)
}

// readCostDatabaseRatio is the ratio of now's database time per reading to old's, "-" where
// either was not measured.
func readCostDatabaseRatio(old, now *readCostDatabaseTime) string {
	if old == nil || now == nil || old.ms == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", now.ms/old.ms)
}
