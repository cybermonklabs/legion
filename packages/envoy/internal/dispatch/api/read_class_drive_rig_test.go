package api

// read_class_drive_rig_test.go holds what the drives of read_class_drive_test.go run on: a tracer
// on the whole pool the server and the broker-token verifier share, the window each drive reads
// the pool's statements in, the writes a read may make, the comparison of a read-only answer with
// its writer's, and the stand-ins of GitHub and the Envoy listener the seeded store is read
// through.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/githubtest"
)

// drivenStatement is one statement the traced pool ran: its SQL, the writes it makes as
// driveWritesOf reads them when it is recorded (writes, and whether every write word in it was
// read, readable), whether it ran on a context the test marked (countedRead), which is a
// request's own context and the work that request detached from it, and never the server's own
// context, and the backend it ran on (conn, its process id), which places it in a transaction
// (unrolled).
type drivenStatement struct {
	sql      string
	writes   []string
	readable bool
	marked   bool
	conn     uint32
	oracle   bool
}

// writingOracle marks only the writing-token comparison request and work deriving its context.
// That request may persist results metadata; it is not a read-only request under test.
type writingOracle struct{}

// poolStatements records every statement run on the pool it traces, whatever context it runs on:
// the request's own, the work a request detaches from it, and the server's own context, where a
// read can start a statistics refresh or a roster fetch, and where every duty of the rig runs.
// Each statement of a batch is recorded on its own, and a copy as the insert it is. waitWork joins
// every worker the world's reads can leave running before a window closes (window).
type poolStatements struct {
	mu         sync.Mutex
	statements []drivenStatement
	waitWork   func(context.Context) error
	// checked is the next unaudited statement in a read-only world. It is touched only by the
	// world's serial request driver; record uses mu for the statements it appends.
	checked   int
	checkGaps bool
}

func (p *poolStatements) record(ctx context.Context, conn *pgx.Conn, sql string) {
	writes, readable := driveWritesOf(sql)
	var backend uint32
	if conn != nil {
		backend = conn.PgConn().PID()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statements = append(p.statements, drivenStatement{
		sql: sql, writes: writes, readable: readable, marked: ctx.Value(countedRead{}) != nil, conn: backend,
		oracle: ctx.Value(writingOracle{}) != nil,
	})
}

func (p *poolStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p.record(ctx, conn, data.SQL)
	return ctx
}

func (p *poolStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *poolStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (p *poolStatements) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	p.record(ctx, conn, data.SQL)
}

func (p *poolStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (p *poolStatements) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	p.record(ctx, conn, "insert into "+data.TableName.Sanitize()+" (copy from)")
	return ctx
}

func (p *poolStatements) TraceCopyFromEnd(context.Context, *pgx.Conn, pgx.TraceCopyFromEndData) {}

// count is how many statements the pool has run.
func (p *poolStatements) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.statements)
}

// since is every statement the pool ran after the first from.
func (p *poolStatements) since(from int) []drivenStatement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.statements[from:])
}

// auditGap checks every statement since the preceding window's snapshot. Advancing by exactly
// that snapshot's length leaves a concurrently appended statement for the next check.
func (p *poolStatements) auditGap(t *testing.T, what string) {
	t.Helper()
	if !p.checkGaps {
		return
	}
	statements := p.since(p.checked)
	p.checked += len(statements)
	// Only the writer comparison runs between windows (driveRead), and its statements are dropped
	// as the oracle's (writingOracle, nonOracleStatements), so no rolled-back write left is a
	// route's (readMayRollBack).
	requireReadsWrites(t, "between windows "+what, "", nonOracleStatements(statements))
}

// nonOracleStatements filters a snapshot in place, never the tracer's recorded statements.
// Only the writer comparison's own context and contexts derived from it are exempt.
func nonOracleStatements(statements []drivenStatement) []drivenStatement {
	checked := statements[:0]
	for _, statement := range statements {
		if !statement.oracle {
			checked = append(checked, statement)
		}
	}
	return checked
}

// driveWindowLimit bounds each request's work and each extra background observation.
const driveWindowLimit = 20 * time.Second

// driveCalibrationWindow is the two-second observation used before and after a world's rows to
// catch background writers outside the known workers' completion barriers.
const driveCalibrationWindow = 2 * time.Second

// window records the request and all its background work, including a worker that runs no SQL
// until after the response. waitWork joins those workers. An optional observation then keeps the
// window open for unknown writers before or after the rows. The same driveWindowLimit bounds both.
func (p *poolStatements) window(t *testing.T, what string, observation time.Duration, send func()) []drivenStatement {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), driveWindowLimit)
	defer cancel()
	p.auditGap(t, "before "+what)
	from := p.count()
	if p.checkGaps {
		from = p.checked
	}
	send()
	p.finish(t, ctx, what, from)
	if observation > 0 {
		timer := time.NewTimer(observation)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("%s: observation did not finish within %s: %v; window writes: %v", what, driveWindowLimit, err, writesIn(p.since(from)))
		}
	}
	statements := p.since(from)
	if p.checkGaps {
		p.checked = from + len(statements)
	}
	return statements
}

// settle joins the preceding request's work, so the next window contains none of it.
func (p *poolStatements) settle(t *testing.T, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), driveWindowLimit)
	defer cancel()
	p.finish(t, ctx, what, p.count())
	p.auditGap(t, what)
}

func (p *poolStatements) finish(t *testing.T, ctx context.Context, what string, from int) {
	t.Helper()
	if p.waitWork == nil {
		t.Fatal("the traced pool has no completion barrier")
	}
	if err := p.waitWork(ctx); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("%s: request work did not finish within %s: %v; window writes: %v", what, driveWindowLimit, err, writesIn(p.since(from)))
		}
		t.Fatalf("%s: request work failed: %v; window writes: %v", what, err, writesIn(p.since(from)))
	}
}

var (
	// driveUpdateColumn is doc_updates' column named update where a compaction rewrites it (set
	// update =, docs/compaction.go's fold), the one place updateNoWrite reads the column as the
	// verb of a write. A document load's read of the column (select update, d.update) is
	// updateNoWrite's to place, after writesOf has set comments aside, so that a comment cannot
	// end in a word that hides the write after it.
	driveUpdateColumn = regexp.MustCompile(`(?i)\bset\s+update\s*=`)
	// lastUsedUpdate is the one write of a personal token a read may make: its last use.
	lastUsedUpdate = regexp.MustCompile(`(?is)^\s*update\s+agent_(read_)?tokens\s+set\s+last_used_at\s*=`)
)

// driveWritesOf is writesOf (read_only_token_test.go) over statement with a compaction's set
// update = set aside (driveUpdateColumn), the one column of doc_updates named update that
// updateNoWrite would read as a write.
func driveWritesOf(statement string) ([]string, bool) {
	read := driveUpdateColumn.ReplaceAllStringFunc(statement, func(column string) string {
		return updateWord.ReplaceAllString(column, "update_column")
	})
	return writesOf(read)
}

// The drive judges each statement it records by driveWritesOf and readMayWrite
// (requireReadsWrites): a compaction's rewrite of doc_updates is a write a read may make, a
// document load's read of the column named update is no write, and a write after a comment that
// ends in select is still the write it is.
func TestTheDriveReadsOnlyACompactionsUpdateColumnAsNoVerb(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, sql string
		write     string // "" for none
		allowed   bool
	}{
		{"a compaction's fold (docs/compaction.go)", "update doc_updates set update = $3, content_changed = $4 where artifact_id = $1 and version = $2", "update doc_updates", true},
		{"a document load's read of the column", "select update, d.update from doc_updates d where d.artifact_id = $1", "", true},
		{"a write after a comment that ends in select", "-- the reader's note, then select\nupdate projects set name = name", "update projects", false},
	} {
		writes, readable := driveWritesOf(row.sql)
		if !readable {
			t.Errorf("%s: driveWritesOf cannot read a table for every write word of %q", row.name, row.sql)
			continue
		}
		if row.write == "" {
			if len(writes) != 0 {
				t.Errorf("%s: driveWritesOf reads %v, want no write", row.name, writes)
			}
			continue
		}
		if !slices.Equal(writes, []string{row.write}) {
			t.Errorf("%s: driveWritesOf reads %v, want [%s]", row.name, writes, row.write)
			continue
		}
		verb, table, _ := strings.Cut(row.write, " ")
		if allowed := readMayWrite(verb, table, row.sql); allowed != row.allowed {
			t.Errorf("%s: readMayWrite(%s, %s) = %v, want %v", row.name, verb, table, allowed, row.allowed)
		}
	}
}

// The drive judges a write by whether a rollback undid it (unrolled), each backend's
// transactions apart: a write a rollback undid stores nothing, one a commit kept is the read's,
// and one in a transaction the window holds no end of is the read's too, since nothing showed it
// undone. The rows with a commit are the red: a walk that called every write in a transaction
// undone would pass a read that commits a write.
func TestTheDriveJudgesOnlyTheWritesNoRollbackUndid(t *testing.T) {
	t.Parallel()
	statement := func(conn uint32, sql string) drivenStatement {
		writes, _ := driveWritesOf(sql)
		return drivenStatement{sql: sql, writes: writes, readable: true, conn: conn}
	}
	const (
		heads = "insert into pull_request_heads (repo, number) values ($1, $2)"
		texts = "insert into pull_request_texts (repo, number) values ($1, $2)"
	)
	for _, row := range []struct {
		name         string
		statements   []drivenStatement
		kept, undone []string
	}{
		{"a write a rollback undid", []drivenStatement{
			statement(1, "begin"), statement(1, heads), statement(1, "rollback"),
		}, nil, []string{heads}},
		{"a write a commit kept", []drivenStatement{
			statement(1, "begin"), statement(1, heads), statement(1, "commit"),
		}, []string{heads}, nil},
		{"a savepoint rolled back to inside a commit", []drivenStatement{
			statement(1, "begin"), statement(1, "savepoint sp_1"), statement(1, heads),
			statement(1, "rollback to savepoint sp_1"), statement(1, texts), statement(1, "commit"),
		}, []string{texts}, []string{heads}},
		{"a released savepoint inside a rollback", []drivenStatement{
			statement(1, "begin"), statement(1, "savepoint sp_1"), statement(1, heads),
			statement(1, "release savepoint sp_1"), statement(1, "rollback"),
		}, nil, []string{heads}},
		{"another backend's write beside a rollback", []drivenStatement{
			statement(1, "begin"), statement(1, heads), statement(2, texts), statement(1, "rollback"),
		}, []string{texts}, []string{heads}},
		{"a transaction the window holds no end of", []drivenStatement{
			statement(1, "begin"), statement(1, heads),
		}, []string{heads}, nil},
	} {
		kept, undone := unrolled(row.statements)
		sqlOf := func(statements []drivenStatement) []string {
			var sql []string
			for _, statement := range statements {
				sql = append(sql, statement.sql)
			}
			return sql
		}
		if got := sqlOf(kept); !slices.Equal(got, row.kept) {
			t.Errorf("%s: unrolled kept %q, want %q", row.name, got, row.kept)
		}
		if got := sqlOf(undone); !slices.Equal(got, row.undone) {
			t.Errorf("%s: unrolled undid %q, want %q", row.name, got, row.undone)
		}
	}
}

// The drive accepts a write a rollback undid only on the results route and only to the tables its
// writer writes (readMayRollBack); a simple-protocol string that opens, writes and commits is
// judged by its write. The rows that must fail are the control: a rolled-back write on another
// route, a rolled-back write of another table on the results route, and a commit hidden after a
// write in one string.
func TestTheDriveAcceptsARolledBackWriteOnlyOnTheResultsRouteAndItsTables(t *testing.T) {
	t.Parallel()
	statement := func(sql string) drivenStatement {
		writes, readable := driveWritesOf(sql)
		return drivenStatement{sql: sql, writes: writes, readable: readable, conn: 1}
	}
	rolledBack := func(write string) []drivenStatement {
		return []drivenStatement{statement("begin"), statement(write), statement("rollback")}
	}
	const heads = "insert into pull_request_heads (repo, number) values ($1, $2)"
	for _, row := range []struct {
		name       string
		route      string
		statements []drivenStatement
		fails      bool
	}{
		{"a rolled-back write of the results writer's table on the results route", rolledBackRoute, rolledBack(heads), false},
		{"a rolled-back write of each of its tables", rolledBackRoute, slices.Concat(
			rolledBack("insert into pull_request_texts (repo, number) values ($1, $2)"),
			rolledBack("delete from pull_request_reads_owed where repo = $1"),
			rolledBack("insert into branch_commits (repo, branch, sha) values ($1, $2, $3)")), false},
		{"a rolled-back write of the same table on another route", http.MethodGet + " /api/v1/issues/{key}", rolledBack(heads), true},
		{"a rolled-back write of another table on the results route", rolledBackRoute,
			rolledBack("update issues set title = $2 where key = $1"), true},
		// The string begins with the bare word begin, so a reading of control words alone takes it
		// for the start of a transaction and hides its write; the guard at the top of
		// transactionControl judges it by that write.
		{"a write and its commit in one string on the results route", rolledBackRoute,
			[]drivenStatement{statement("begin\n" + heads + ";\ncommit")}, true},
	} {
		recorder := &errorRecorder{TB: t}
		requireReadsWrites(recorder, row.name, row.route, row.statements)
		if failed := len(recorder.errors) > 0; failed != row.fails {
			t.Errorf("%s: the drive failed=%t (%q), want failed=%t", row.name, failed, recorder.errors, row.fails)
		}
	}
}

// errorRecorder is a testing.TB whose Errorf records the failure instead of failing the test, so
// a test can hold a judgment's failure as its control.
type errorRecorder struct {
	testing.TB
	errors []string
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *errorRecorder) Helper() {}

// readMayWrite is whether a read may write table with verb, statement being the whole statement.
// A read writes nothing but the spec's closed list, wherever the write runs: the insert that
// spends a broker token's proof, a personal token's last use, the inserts of the display trees,
// the route roster and the event horizon (writesOf reads an upsert as the insert it is), a
// project's term statistics, GitHub's rate limits, and a compaction's rewrite and delete of a
// document's updates. A write a rollback undid is held to readMayRollBack.
func readMayWrite(verb, table, statement string) bool {
	switch table {
	case "dpop_proofs", "text_trees", "route_roster", "event_horizon":
		return verb == "insert"
	case "agent_tokens", "agent_read_tokens":
		return verb == "update" && lastUsedUpdate.MatchString(statement)
	case "search_term_stats", "search_term_totals", "search_term_queue", "github_rate_limits":
		return true
	case "doc_updates":
		return verb == "update" || verb == "delete"
	case "doc_compaction_owed":
		return verb == "delete"
	}
	return false
}

// writesIn is every write statements make, as "<verb> <table>", for a failure to name.
func writesIn(statements []drivenStatement) []string {
	var written []string
	for _, statement := range statements {
		if !statement.readable {
			written = append(written, "unreadable: "+oneLine(statement.sql))
		}
		written = append(written, statement.writes...)
	}
	return written
}

// rolledBackRoute is the one route whose read may make writes a rollback undoes: a read-only
// caller's results read makes the writes of its read-throughs with the writer's own code in a
// transaction it rolls back (delivery.HeadAnswers), and rolledBackTables is every table that
// writer's code writes.
const rolledBackRoute = http.MethodGet + " /api/v1/issues/{key}/results"

var rolledBackTables = []string{"pull_request_heads", "pull_request_texts", "pull_request_reads_owed", "branch_commits"}

// readMayRollBack is whether a read of route may make a write of table that a rollback undid:
// only the results read, only to its writer's tables (rolledBackRoute, rolledBackTables), and
// otherwise only a write the read may make anyway (readMayWrite). A rolled-back write still takes
// row locks, fires triggers and draws sequence values, so no other route is given it.
func readMayRollBack(route, verb, table, statement string) bool {
	if route == rolledBackRoute && slices.Contains(rolledBackTables, table) {
		return true
	}
	return readMayWrite(verb, table, statement)
}

// requireReadsWrites fails what, a read of route, for every write of statements a read may not
// make (readMayWrite), every write a rollback undid that the route may not make
// (readMayRollBack), and every statement holding a write this test cannot read a table for. It
// answers every write it saw, as "<verb> <table>", each undone one as
// "rolled back <verb> <table>".
func requireReadsWrites(t testing.TB, what, route string, statements []drivenStatement) []string {
	t.Helper()
	kept, undone := unrolled(statements)
	var written []string
	for _, statement := range undone {
		if !statement.readable {
			t.Errorf("%s: the window holds a write this test cannot read a table for, rolled back: %s", what, oneLine(statement.sql))
		}
		for _, write := range statement.writes {
			written = append(written, "rolled back "+write)
			verb, table, _ := strings.Cut(write, " ")
			if !readMayRollBack(route, verb, table, statement.sql) {
				t.Errorf("%s: the window holds %s %s, rolled back, which a read of %s may not make: only %s may, and only to %v: %s",
					what, verb, table, cmp.Or(route, "this route"), rolledBackRoute, rolledBackTables, oneLine(statement.sql))
			}
		}
	}
	for _, statement := range kept {
		if !statement.readable {
			t.Errorf("%s: the window holds a write this test cannot read a table for: %s", what, oneLine(statement.sql))
		}
		for _, write := range statement.writes {
			written = append(written, write)
			verb, table, _ := strings.Cut(write, " ")
			if !readMayWrite(verb, table, statement.sql) {
				t.Errorf("%s: the window holds %s %s, which a read may not write: %s", what, verb, table, oneLine(statement.sql))
			}
		}
	}
	return written
}

// transactionControl is the word a statement controls its backend's transaction with: begin,
// savepoint, release, commit, rollback, or rollback to (a savepoint), and "" for any other
// statement, and for one holding a write or one this test cannot read: a simple-protocol string
// such as `begin; insert ...; commit` is judged by its writes, and none is hidden as control.
func transactionControl(statement string) string {
	if writes, readable := driveWritesOf(statement); len(writes) > 0 || !readable {
		return ""
	}
	words := strings.Fields(strings.ToLower(statement))
	switch {
	case len(words) == 0:
		return ""
	case words[0] == "begin" || len(words) > 1 && words[0] == "start" && words[1] == "transaction":
		return "begin"
	case words[0] == "savepoint":
		return "savepoint"
	case words[0] == "release":
		return "release"
	case words[0] == "commit" || words[0] == "end":
		return "commit"
	case words[0] == "rollback" && len(words) > 1 && words[1] == "to":
		return "rollback to"
	case words[0] == "rollback" || words[0] == "abort":
		return "rollback"
	}
	return ""
}

// unrolled parts statements into those that stand (kept) and those a rollback undid (undone), by
// each backend's transactions in the order its statements ran: a statement in a transaction, or
// in a savepoint of one, stands once the transaction commits, and is undone where the transaction
// rolls back or the savepoint is rolled back to. A statement of a transaction the window holds no
// end of stands, since nothing showed it undone, and so does one that ran on no backend the
// tracer could name. The control statements themselves are left out of both.
func unrolled(statements []drivenStatement) (kept, undone []drivenStatement) {
	open := map[uint32][][]drivenStatement{}
	hold := func(statement drivenStatement) {
		frames := open[statement.conn]
		if statement.conn == 0 || len(frames) == 0 {
			kept = append(kept, statement)
			return
		}
		frames[len(frames)-1] = append(frames[len(frames)-1], statement)
	}
	for _, statement := range statements {
		frames := open[statement.conn]
		switch control := transactionControl(statement.sql); {
		case statement.conn == 0 || control == "":
			hold(statement)
		case control == "begin":
			open[statement.conn] = [][]drivenStatement{nil}
		case control == "savepoint":
			open[statement.conn] = append(frames, nil)
		case len(frames) == 0:
		case control == "release" && len(frames) > 1:
			top := frames[len(frames)-1]
			frames = frames[:len(frames)-1]
			frames[len(frames)-1] = append(frames[len(frames)-1], top...)
			open[statement.conn] = frames
		case control == "rollback to":
			undone = append(undone, frames[len(frames)-1]...)
			open[statement.conn] = frames[:len(frames)-1]
			if len(frames) == 1 {
				open[statement.conn] = [][]drivenStatement{nil}
			}
		case control == "commit":
			for _, frame := range frames {
				kept = append(kept, frame...)
			}
			delete(open, statement.conn)
		case control == "rollback":
			for _, frame := range frames {
				undone = append(undone, frame...)
			}
			delete(open, statement.conn)
		}
	}
	for _, frames := range open {
		for _, frame := range frames {
			kept = append(kept, frame...)
		}
	}
	return kept, undone
}

// holdsStatement reports whether one of statements matches pattern.
func holdsStatement(statements []drivenStatement, pattern *regexp.Regexp) bool {
	return slices.ContainsFunc(statements, func(statement drivenStatement) bool { return pattern.MatchString(statement.sql) })
}

// oneLine is statement on one line, its runs of space each one space, cut to a length a failure
// can carry.
func oneLine(statement string) string {
	line := strings.Join(strings.Fields(statement), " ")
	if len(line) > 400 {
		return line[:400] + "…"
	}
	return line
}

// volatileMember is one member of a row's answer that two answers on the same state differ in,
// and why: it is left out of the comparison. Its path names the member from the top of the
// answer, its steps joined by dots, a * standing for every member of an object or element of an
// array at that step.
type volatileMember struct {
	path, reason string
}

// credentialAnswer marks a row whose answer describes the credential that asked: its members
// that do are held to what the read-only credential's own answer must say (holds) rather than to
// the writer's, and every other member is compared.
type credentialAnswer struct {
	reason  string
	members []string
	// holds says how the read-only answer's own members differ from what they must say, and is
	// empty when they say it.
	holds func(answer map[string]any) string
}

// answerDifference says how readOnly's answer differs from writer's, and is empty when the two
// have one status and one body but for the members volatile lists and those credential holds to
// the read-only answer of its own. A body that is no JSON is compared byte for byte.
func answerDifference(readOnly, writer *httptest.ResponseRecorder, volatile []volatileMember, credential *credentialAnswer) string {
	if readOnly.Code != writer.Code {
		return fmt.Sprintf("the read-only answer is %d %s and the writer's %d %s",
			readOnly.Code, cutBody(readOnly.Body.String()), writer.Code, cutBody(writer.Body.String()))
	}
	var readOnlyBody, writerBody any
	readOnlyErr := json.Unmarshal(readOnly.Body.Bytes(), &readOnlyBody)
	writerErr := json.Unmarshal(writer.Body.Bytes(), &writerBody)
	if readOnlyErr != nil || writerErr != nil {
		if readOnly.Body.String() != writer.Body.String() {
			return fmt.Sprintf("the read-only answer's body is\n%s\nand the writer's\n%s", cutBody(readOnly.Body.String()), cutBody(writer.Body.String()))
		}
		return ""
	}
	if credential != nil {
		object, _ := readOnlyBody.(map[string]any)
		if problem := credential.holds(object); problem != "" {
			return "the read-only answer does not describe its own credential: " + problem
		}
	}
	paths := make([]string, 0, len(volatile))
	// set is what is left out of the comparison and why, for a failure to name.
	set := make([]string, 0, len(volatile)+1)
	for _, member := range volatile {
		paths = append(paths, member.path)
		set = append(set, member.path+" ("+member.reason+")")
	}
	if credential != nil {
		paths = append(paths, credential.members...)
		set = append(set, strings.Join(credential.members, ", ")+" ("+credential.reason+")")
	}
	for _, path := range paths {
		dropMember(readOnlyBody, strings.Split(path, "."))
		dropMember(writerBody, strings.Split(path, "."))
	}
	if differences := memberDifferences("", readOnlyBody, writerBody, nil); len(differences) > 0 {
		return fmt.Sprintf("the read-only answer, but for [%s], differs from the writer's in %d members:\n  %s",
			strings.Join(set, "; "), len(differences), strings.Join(differences, "\n  "))
	}
	return ""
}

// memberDifferences appends to differences every member at or below path where read-only and
// writer differ, read-only's value first, and answers them; at most twenty are named. A member
// one answer holds and the other does not is a difference, even where the one that holds it holds
// null.
func memberDifferences(path string, readOnly, writer any, differences []string) []string {
	const most = 20
	if len(differences) >= most {
		return differences
	}
	at := func(step string) string {
		if path == "" {
			return step
		}
		return path + "." + step
	}
	switch got := readOnly.(type) {
	case map[string]any:
		if want, ok := writer.(map[string]any); ok {
			keys := slices.Sorted(maps.Keys(got))
			for key := range want {
				if _, held := got[key]; !held {
					keys = append(keys, key)
				}
			}
			presence := map[bool]string{true: "present", false: "absent"}
			for _, key := range keys {
				if len(differences) >= most {
					return differences
				}
				gotValue, gotHeld := got[key]
				wantValue, wantHeld := want[key]
				if gotHeld != wantHeld {
					differences = append(differences, fmt.Sprintf("%s: %s in the read-only answer, and %s in the writer's", at(key), presence[gotHeld], presence[wantHeld]))
					continue
				}
				differences = memberDifferences(at(key), gotValue, wantValue, differences)
			}
			return differences
		}
	case []any:
		if want, ok := writer.([]any); ok && len(want) == len(got) {
			for index := range got {
				differences = memberDifferences(at(fmt.Sprint(index)), got[index], want[index], differences)
			}
			return differences
		}
	}
	gotJSON, _ := json.Marshal(readOnly)
	wantJSON, _ := json.Marshal(writer)
	if string(gotJSON) != string(wantJSON) {
		differences = append(differences, fmt.Sprintf("%s: %s, and the writer's %s", cmp.Or(path, "the answer"), cutValue(gotJSON), cutValue(wantJSON)))
	}
	return differences
}

// cutValue is one member's JSON cut to a length a failure line can carry.
func cutValue(value []byte) string {
	if len(value) > 300 {
		return string(value[:300]) + "…"
	}
	return string(value)
}

// dropMember deletes the member path names from value, at every place a * step reaches.
func dropMember(value any, path []string) {
	if len(path) == 0 {
		return
	}
	step, rest := path[0], path[1:]
	switch node := value.(type) {
	case map[string]any:
		switch {
		case len(rest) == 0 && step == "*":
			clear(node)
		case len(rest) == 0:
			delete(node, step)
		case step == "*":
			for _, child := range node {
				dropMember(child, rest)
			}
		default:
			dropMember(node[step], rest)
		}
	case []any:
		if step != "*" {
			return
		}
		for _, child := range node {
			dropMember(child, rest)
		}
	}
}

// cutBody is body cut to a length a failure can carry.
func cutBody(body string) string {
	body = strings.TrimSpace(body)
	if len(body) > 4000 {
		return body[:4000] + "…"
	}
	return body
}

// driveGitHub is the GitHub the seeded store is read through. The App is installed on
// acme/widgets, the repository the drive project's issues name, and legion/arch, its
// architecture source. acme/widgets's default branch is main and it holds every commit asked
// about; a commit's pull requests are what pulls answers, an empty list where it is nil, and a
// compare answers what compare does, diverged where it is nil. legion/arch's commits, refs, trees
// and blobs are architecture_source_test.go's fake repository.
type driveGitHub struct {
	mu      sync.Mutex
	pulls   func(repo, sha string) string
	compare func(base, head string) string
}

func (g *driveGitHub) answersPulls(body func(repo, sha string) string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls = body
}

func (g *driveGitHub) answersCompare(status func(base, head string) string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.compare = status
}

// serve starts the stand-in and answers its base URL.
func (g *driveGitHub) serve(t *testing.T) string {
	t.Helper()
	const architectureRepo = "legion/arch"
	architecture := (&fakeGitHubApp{
		installations: map[string]string{architectureRepo: "read"},
		repos:         map[string]*fakeArchitectureRepo{architectureRepo: {commit: shaArch, files: archFiles()}},
	}).handler(t)
	installed := map[string]bool{"acme/widgets": true, architectureRepo: true}
	repoOf := func(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !installed[repoOf(r)] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !installed[repoOf(r)] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, repoOf(r))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if repoOf(r) == architectureRepo {
			architecture.ServeHTTP(w, r)
			return
		}
		fmt.Fprintf(w, `{"sha":%q}`, r.PathValue("ref"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/", architecture.ServeHTTP)
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		answer := g.pulls
		g.mu.Unlock()
		if answer == nil {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, answer(repoOf(r), r.PathValue("sha")))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		base, head, _ := strings.Cut(r.PathValue("basehead"), "...")
		g.mu.Lock()
		status := g.compare
		g.mu.Unlock()
		answer := "diverged"
		if status != nil {
			answer = status(base, head)
		}
		fmt.Fprintf(w, `{"status":%q}`, answer)
	})
	mux.HandleFunc("POST /graphql", commitPullsThroughMux(t, mux))
	return githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
}

// driveListener is the Envoy listener the seeded store is read through: it lists the sessions of
// sessionsJSON, answers each one's registry record (writeSessionRecord), lists interests, takes
// every message sent to a session it lists, and holds no Legion agent and no role.
func driveListener(t *testing.T, sessionsJSON string, interests map[string][]string) string {
	t.Helper()
	var sent sync.Mutex
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessionsJSON))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, sessionsJSON, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/interests/":
			rows := make([]map[string]any, 0, len(interests))
			for _, session := range slices.Sorted(maps.Keys(interests)) {
				rows = append(rows, map[string]any{"session_id": session, "topics": interests[session], "updated_at": 1700000000000})
			}
			WriteJSON(w, http.StatusOK, rows)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/interests/"):
			session := strings.TrimPrefix(r.URL.Path, "/v1/interests/")
			topics, ok := interests[session]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"session_id": session, "topics": topics, "updated_at": 1700000000000})
		case r.Method == http.MethodGet && (r.URL.Path == "/v1/legion/agents" || strings.HasPrefix(r.URL.Path, "/v1/legion/agents/roles/")):
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode a send to the drive's listener: %v", err)
			}
			sent.Lock()
			sends++
			envelope := sends
			sent.Unlock()
			fmt.Fprintf(w, `{"event_id":"envelope-%d","recipient":%q,"state":"connected"}`, envelope, request.TargetSession)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"the drive's listener serves no ` + r.Method + ` ` + r.URL.Path + `"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}
