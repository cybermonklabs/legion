package ompsessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// verdictNames is how the state table's test names what the helper does next.
var verdictNames = map[verdict]string{
	goOn: "go on", reopen: "open a new connection", refuse: "refuse, closing nothing", rollBack: "roll back and return",
	tryAgain: "close and try again", giveUp: "close and return the bound", closeAndReturn: "close and return",
}

// The state table of one transaction of the client (inTransaction), one row for each stage of a try
// and what it met, read from decide, the pure function the helper asks after each stage: a Conn its
// holder closed and a live connection already in a transaction are refused, nothing closed; a
// connection pgx closed is opened again from its configuration, and a refused open ends the call; a
// refused opening, any SQLSTATE, a 25006 or a 42501 of the read-only check or of the work, and a
// connection that ended before COMMIT (no server error on a connection pgx closed, or a FATAL or
// PANIC one) are closed with no ROLLBACK and tried again, and on the fourth try closed and returned
// as the bound; the read-only default, any other SQLSTATE and fn's own error roll back and return; a
// context that ended, and any error once COMMIT was sent, ErrTxCommitRollback included, close and
// return. Each row's comment names the database test that drives it on a real server, where one
// does. Last, the errors of the helper's own steps: one carrying 42P01 is no "no table".
func TestEachFailureOfATransactionIsClosedRetriedOrReturned(t *testing.T) {
	server := func(severity, code, message string) *pgconn.PgError {
		return &pgconn.PgError{Severity: severity, SeverityUnlocalized: severity, Code: code, Message: message}
	}
	refusedRole := server("ERROR", insufficientPrivilege, "permission denied for table omp_session_files")
	readOnly := server("ERROR", readOnlySQLTransaction, "cannot execute INSERT in a read-only transaction")
	ended := server("FATAL", "57P01", "terminating connection due to administrator command")
	panicked := server("PANIC", "XX000", "could not write to file")
	lost := fmt.Errorf("write failed: %w", io.ErrUnexpectedEOF)
	for _, row := range []struct {
		row   int
		what  string
		steps []step
		want  verdict
	}{
		// planted_test.go's dead-connection subtest closes by its holder after; the bound test's last call.
		{1, "start: the holder closed the Conn", []step{
			{at: atStart, try: 1, held: true, closed: true},
			{at: atStart, try: 2, held: true, closed: true, ended: context.Canceled},
		}, refuse},
		// planted_test.go's nested-call subtest.
		{2, "start: a live connection already in a transaction", []step{
			{at: atStart, try: 1, busy: true},
			{at: atStart, try: 1, busy: true, ended: context.Canceled},
		}, refuse},
		// planted_test.go's dead-connection subtest; every retry of the bound test.
		{3, "start: pgx closed the connection", []step{{at: atStart, try: 1, closed: true}, {at: atStart, try: 4, closed: true}}, reopen},
		// TestALostConnectionIsTriedAgainBeforeCommitAndNotAfter, its login made NOLOGIN.
		{4, "reopen: the open fails", []step{
			{at: atReopen, try: 2, err: server("FATAL", "28000", `role "legion" is not permitted to log in`)},
			{at: atReopen, try: 4, err: lost},
		}, refuse},
		// No test server refuses an opening; root c-cloudsql sends it under Cloud SQL's locked read-only session.
		{5, "opening: the server refuses it, any SQLSTATE", []step{
			{at: atOpening, try: 1, err: server("ERROR", "42704", `unrecognized configuration parameter "restrict_nonsystem_relation_kind"`)},
			{at: atOpening, try: 2, err: readOnly},
			{at: atOpening, try: 3, err: server("ERROR", "0A000", "permission denied to set parameter")},
		}, tryAgain},
		// TestALostConnectionIsTriedAgainBeforeCommitAndNotAfter, ended at the opening.
		{6, "opening: the connection ends before the answer", []step{
			{at: atOpening, try: 1, err: lost, closed: true},
			{at: atOpening, try: 3, err: ended, closed: true},
		}, tryAgain},
		// TestAWriteUnderAReadOnlyDefaultRefusesAndKeepsItsConnection.
		{7, "read-only check: reset_val reads on", []step{{at: atCheck, try: 1, readOnlyDefault: true}}, rollBack},
		// TestTheClientClosesEachConnectionItCannotRepairAndTriesATransactionAtMostFourTimes (42501).
		{8, "check or work: 25006 or 42501", []step{
			{at: atWork, try: 1, err: refusedRole},
			{at: atWork, try: 3, err: fmt.Errorf("look up session: %w", readOnly)},
			{at: atCheck, try: 2, err: readOnly},
			{at: atCheck, try: 1, err: refusedRole},
		}, tryAgain},
		// TestTheClientClosesEachConnectionItCannotRepairAndTriesATransactionAtMostFourTimes and
		// TestALostConnectionIsTriedAgainBeforeCommitAndNotAfter, ended before a work statement.
		{9, "check or work: the connection ends", []step{
			{at: atWork, try: 1, err: lost, closed: true},
			{at: atWork, try: 2, err: ended, closed: true},
			{at: atWork, try: 2, err: ended},
			{at: atWork, try: 3, err: panicked},
			{at: atCheck, try: 1, err: lost, closed: true},
		}, tryAgain},
		// ompsessions_test.go's TestImportRefusesContentNoTextValueHolds and TestWrittenFindsOnly….
		{10, "work: another SQLSTATE", []step{
			{at: atWork, try: 1, err: server("ERROR", undefinedTable, `relation "public.omp_session_files" does not exist`)},
			{at: atWork, try: 4, err: server("ERROR", "22021", `invalid byte sequence for encoding "UTF8": 0x00`)},
			{at: atWork, try: 2, err: server("ERROR", "23505", "duplicate key value violates unique constraint")},
			{at: atWork, try: 1, err: server("ERROR", "55000", `access to non-system view "omp_session_files" is restricted`)},
			{at: atCheck, try: 1, err: server("ERROR", "57014", "canceling statement due to statement timeout")},
		}, rollBack},
		// ompsessions_test.go's TestAClaudeSaveRefusesAStaleWriter; Written of a session the table lacks.
		{11, "work: fn's own error", []step{
			{at: atWork, try: 1, err: &MovedError{Path: claudeTranscript, Saved: Absent, Held: 3}},
			{at: atWork, try: 4, err: pgx.ErrNoRows},
			{at: atWork, try: 1, err: errors.New("public.omp_session_files is a view, not a table")},
		}, rollBack},
		// TestALostConnectionIsTriedAgainBeforeCommitAndNotAfter, its context cancelled inside a try.
		{12, "any stage: the context ended", []step{
			{at: atStart, try: 1, ended: context.Canceled},
			{at: atStart, try: 2, closed: true, ended: context.DeadlineExceeded},
			{at: atReopen, try: 2, err: context.Canceled, ended: context.Canceled},
			{at: atOpening, try: 1, err: lost, closed: true, ended: context.DeadlineExceeded},
			{at: atCheck, try: 1, err: context.Canceled, ended: context.Canceled},
			{at: atWork, try: 4, err: refusedRole, ended: context.Canceled},
			{at: atWork, try: 1, err: &MovedError{Path: claudeTranscript, Saved: 3, Held: 5}, ended: context.Canceled},
			{at: atCommit, try: 1, err: lost, closed: true, ended: context.Canceled},
		}, closeAndReturn},
		// TestALostConnectionIsTriedAgainBeforeCommitAndNotAfter, ended at COMMIT.
		{13, "commit: the connection ends, or any error after the mark", []step{
			{at: atCommit, try: 1, err: lost, closed: true},
			{at: atCommit, try: 2, err: ended, closed: true},
			{at: atCommit, try: 1, err: refusedRole},
			{at: atCommit, try: 1, err: server("ERROR", "40001", "could not serialize access")},
		}, closeAndReturn},
		{14, "commit: ErrTxCommitRollback", []step{{at: atCommit, try: 1, err: pgx.ErrTxCommitRollback}}, closeAndReturn},
		// TestTheClientClosesEachConnectionItCannotRepairAndTriesATransactionAtMostFourTimes, its login without rights.
		{15, "try 4: any repairless outcome", []step{
			{at: atOpening, try: tries, err: readOnly},
			{at: atOpening, try: tries, err: lost, closed: true},
			{at: atCheck, try: tries, err: readOnly},
			{at: atWork, try: tries, err: refusedRole},
			{at: atWork, try: tries, err: ended, closed: true},
		}, giveUp},
		// Every passing test.
		{16, "commit: success", []step{{at: atCommit, try: 1}, {at: atCommit, try: tries}}, goOn},
	} {
		for i, s := range row.steps {
			if got := decide(s); got != row.want {
				t.Errorf("row %d, %s, case %d (%+v): the helper would %s, want %s", row.row, row.what, i+1, s, verdictNames[got], verdictNames[row.want])
			}
		}
	}

	// A start on a live idle connection, and each stage that met no error, go on to the next.
	for _, s := range []step{{at: atStart, try: 1}, {at: atReopen, try: 2}, {at: atOpening, try: 1}, {at: atCheck, try: 1}, {at: atWork, try: 3}} {
		if got := decide(s); got != goOn {
			t.Errorf("%+v: the helper would %s, want it to go on", s, verdictNames[got])
		}
	}

	// Written, FindByID and Read read 42P01 as "no table" only from an error their own fn returned:
	// an error of the helper's own steps names the SQLSTATE and does not unwrap to the server's
	// error, so isUndefinedTable cannot match it. The control is the server's error itself, which it
	// matches.
	t.Run("an error of the helper's own steps carrying 42P01 is no table missing", func(t *testing.T) {
		missing := server("ERROR", undefinedTable, `relation "public.omp_session_files" does not exist`)
		if !isUndefinedTable(missing) || !isUndefinedTable(fmt.Errorf("look up session: %w", missing)) {
			t.Fatal("control: isUndefinedTable does not match the server's 42P01 itself, so the rows below show nothing")
		}
		for name, own := range map[string]error{
			"a step's error": failed("commit the transaction", missing, nil),
			"the bound":      bounded(missing),
		} {
			var pgErr *pgconn.PgError
			switch {
			case isUndefinedTable(own), isUndefinedTable(fmt.Errorf("look up session: %w", own)):
				t.Errorf("%s, %q, reads as a missing table", name, own)
			case errors.As(own, &pgErr):
				t.Errorf("%s, %q, unwraps to the server's error", name, own)
			case !strings.Contains(own.Error(), "SQLSTATE "+undefinedTable):
				t.Errorf("%s, %q, does not name the SQLSTATE", name, own)
			}
		}
		if own := failed("the call's context ended", missing, context.Canceled); !errors.Is(own, context.Canceled) || isUndefinedTable(own) {
			t.Errorf("an error of the helper's own steps under a context that ended, %q: errors.Is(context.Canceled) = %t, "+
				"isUndefinedTable = %t; want true and false", own, errors.Is(own, context.Canceled), isUndefinedTable(own))
		}
	})
}

// tracer is a pgx QueryTracer and ConnectTracer on the client's connection configuration, which
// every connection the helper opens in place of one keeps (Conn.Config answers a copy that holds
// it): it records each statement the client sends with the backend pid it went to, counts the
// connections opened, and runs its hook, where one is set, before a statement is sent.
type tracer struct {
	mu       sync.Mutex
	sent     []sentTo
	connects int
	before   func(conn *pgx.Conn, sql string)
}

// sentTo is one statement the client sent, and the backend it went to.
type sentTo struct {
	sql string
	pid uint32
}

func (r *tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sent = append(r.sent, sentTo{sql: data.SQL, pid: conn.PgConn().PID()})
	before := r.before
	r.mu.Unlock()
	if before != nil {
		before(conn, data.SQL)
	}
	return ctx
}

func (r *tracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *tracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connects++
	return ctx
}

func (r *tracer) TraceConnectEnd(context.Context, pgx.TraceConnectEndData) {}

// hook sets what runs before each statement the client sends, nil for nothing.
func (r *tracer) hook(before func(conn *pgx.Conn, sql string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = before
}

// take is what the client sent since the last take, and how many connections it opened since.
func (r *tracer) take() (sent []sentTo, connects int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sent, connects = r.sent, r.connects
	r.sent, r.connects = nil, 0
	return sent, connects
}

// openings are the backends the opening of each try went to, in order.
func openings(sent []sentTo) []uint32 {
	var pids []uint32
	for _, s := range sent {
		if s.sql == OpenRead || s.sql == OpenWrite {
			pids = append(pids, s.pid)
		}
	}
	return pids
}

// sentCount is how many of sent are statement.
func sentCount(sent []sentTo, statement string) int {
	n := 0
	for _, s := range sent {
		if s.sql == statement {
			n++
		}
	}
	return n
}

// isWork reports whether sql is a statement of a transaction's work: none of the openings, the
// read-only check, COMMIT and ROLLBACK.
func isWork(sql string) bool {
	return sql != OpenRead && sql != OpenWrite && sql != readOnlyCheck && sql != "commit" && sql != "rollback"
}

// distinct reports whether no two of pids are one backend.
func distinct(pids []uint32) bool {
	sorted := slices.Sorted(slices.Values(pids))
	return len(slices.Compact(sorted)) == len(pids)
}

// endBackend ends the backend pid from killer, a connection of the same login, which may end it
// (pg_terminate_backend), and waits up to 5 s for it to exit, as a server or a pool that ended the
// connection would: the backend sends its client a FATAL 57P01 and closes the socket.
func endBackend(t *testing.T, killer *pgx.Conn, pid uint32) {
	t.Helper()
	var ended bool
	if err := killer.QueryRow(context.Background(), "SELECT pg_catalog.pg_terminate_backend($1, 5000)", int64(pid)).Scan(&ended); err != nil || !ended {
		t.Errorf("end the client's backend %d: %t, %v", pid, ended, err)
	}
}

// gone waits until pg_stat_activity, read on admin, lists none of pids: a backend exits a moment
// after its client's Terminate, which the client sent as it closed the connection.
func gone(t *testing.T, admin *pgx.Conn, pids []uint32) {
	t.Helper()
	ids := make([]int64, len(pids))
	for i, pid := range pids {
		ids[i] = int64(pid)
	}
	testwait.Eventually(t, fmt.Sprintf("the backends %v the client closed to leave pg_stat_activity", pids), func() bool {
		var left int
		if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE pid = ANY($1)", ids).Scan(&left); err != nil {
			t.Errorf("read pg_stat_activity: %v", err)
			return true
		}
		return left == 0
	})
}

// alive reports whether pg_stat_activity, read on admin, lists pid.
func alive(t *testing.T, admin *pgx.Conn, pid uint32) bool {
	t.Helper()
	var listed bool
	if err := admin.QueryRow(context.Background(), "SELECT EXISTS (SELECT FROM pg_catalog.pg_stat_activity WHERE pid = $1)", int64(pid)).Scan(&listed); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return listed
}

// repairDatabase is a database of the test's own holding Oh My Pi's two tables, which the test's
// login made and owns, with the session podSessionFile in them as Oh My Pi would have written it,
// its URL, and an admin connection to it as that login: the test's own reads and writes run on it,
// and it ends the client's backends, which run as the same login.
func repairDatabase(t *testing.T, prefix string) (string, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.PlainDatabase(t, prefix)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	for _, statement := range createTables {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, $2, $3, $4)",
		podSessionFile, session, podSessionWritten, len(session)); err != nil {
		t.Fatal(err)
	}
	return dsn, admin
}

// tracedConn is the package's own kind of connection, a Conn around one pgx connection as
// ConnectURL builds one, opened from dsn with trace as its tracer, and closed when the test ends.
func tracedConn(t *testing.T, dsn string, trace *tracer) *Conn {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Tracer = trace
	config.DefaultQueryExecMode = execMode
	c, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	conn := &Conn{c: c}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// testLogin is a login of the test's own, signed in with a password of its own that the test draws
// at random, made by the test's login, which holds its SET option (dropRole needs it), with grants
// run as the test's login after it is made (each a format of one %s, the role), and a URL of dsn's
// database that signs in as it, its password in it, as the URL file holds one. The role is dropped
// when the test ends, once what the tables' owner granted it is taken back.
func testLogin(t *testing.T, admin *pgx.Conn, dsn, prefix string, grants ...string) (role, roleURL string) {
	t.Helper()
	ctx := context.Background()
	name, err := testdb.RandomName(prefix)
	if err != nil {
		t.Fatal(err)
	}
	password, err := testdb.RandomName("login")
	if err != nil {
		t.Fatal(err)
	}
	role = pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropRole(t, admin, role, "REVOKE ALL ON public.omp_session_files, public.omp_session_files_parts FROM "+role)
	})
	if _, err := admin.Exec(ctx, "GRANT "+role+" TO CURRENT_USER WITH INHERIT FALSE"); err != nil {
		t.Fatal(err)
	}
	for _, grant := range grants {
		if _, err := admin.Exec(ctx, fmt.Sprintf(grant, role)); err != nil {
			t.Fatalf("%s: %v", fmt.Sprintf(grant, role), err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(name, password)
	return role, parsed.String()
}

// setDatabaseDefault makes setting, `<name> = <value>`, a default of admin's database, which each
// session that connects to it from then on starts with (ALTER DATABASE … SET); the test's login
// made the database (testdb.PlainDatabase), and so may.
func setDatabaseDefault(t *testing.T, admin *pgx.Conn, setting string) {
	t.Helper()
	ctx := context.Background()
	var database string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" SET "+setting); err != nil {
		t.Fatal(err)
	}
}

// noRollback is what is wrong with sent where a failed try rolled back, "" for nothing: R3 closes
// a connection it cannot repair while its transaction is open, so the pool closes the server
// connection rather than hand it to the next client.
func noRollback(sent []sentTo) string {
	if n := sentCount(sent, "rollback"); n > 0 {
		return fmt.Sprintf("the client sent %d ROLLBACK, want none: it closes a connection it cannot repair with the transaction open", n)
	}
	return ""
}

// claudeContent is a Claude Code transcript's content, with a multi-byte character.
var claudeContent = []byte("{\"type\":\"user\",\"message\":\"größer\"}\n")

// A transaction whose server connection the client cannot repair runs again from its start on a new
// connection, and the old one is closed with no ROLLBACK: each backend the test ends from a tracer
// hook, from another connection of the same login, just before a statement of the work, on 1, 2 and
// 3 tries, is gone, and the write succeeds after that many closes and writes its row once. A
// transaction that fails on every try, as one of a login without rights on the tables does (42501),
// fails after 4, each connection closed, and names the SQLSTATE. The caller's Conn holds the new
// connection throughout, and answers the next call on it once the login has its rights; once its
// holder closes it, a call errs and opens nothing.
func TestTheClientClosesEachConnectionItCannotRepairAndTriesATransactionAtMostFourTimes(t *testing.T) {
	ctx := context.Background()
	dsn, admin := repairDatabase(t, "legion_ompsessions_repair")
	for _, ends := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("the backend ended before a work statement on %d tries", ends), func(t *testing.T) {
			trace := &tracer{}
			conn := tracedConn(t, dsn, trace)
			trace.take()
			path := fmt.Sprintf("%s.%d", claudeTranscript, ends)
			ended := 0
			trace.hook(func(c *pgx.Conn, sql string) {
				if isWork(sql) && ended < ends {
					ended++
					endBackend(t, admin, c.PgConn().PID())
				}
			})
			err := Replace(ctx, conn, path, Absent, claudeContent, time.UnixMilli(podSessionWritten))
			trace.hook(nil)
			sent, connects := trace.take()
			if err != nil {
				t.Fatalf("Replace after %d ended backends = %v; want it written on a new connection", ends, err)
			}
			pids := openings(sent)
			if ended != ends || connects != ends || len(pids) != ends+1 || !distinct(pids) {
				t.Fatalf("the test ended %d backends; the client opened %d connections and opened its transaction on %v; "+
					"want %d ended, %d opened and %d distinct backends", ended, connects, pids, ends, ends, ends+1)
			}
			gone(t, admin, pids[:ends])
			if last := pids[ends]; conn.IsClosed() || conn.c.PgConn().PID() != last || !alive(t, admin, last) {
				t.Errorf("after the write the Conn is closed %t on backend %d, want it open on %d, the try that committed", conn.IsClosed(), conn.c.PgConn().PID(), last)
			}
			if failure := noRollback(sent); failure != "" {
				t.Error(failure)
			}
			const insertRow = "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, '', 0, 0) ON CONFLICT (path) DO NOTHING RETURNING path"
			if n := sentCount(sent, insertRow); n != ends+1 {
				t.Errorf("the write's first statement ran %d times, want once on each of %d tries: each try runs the transaction from its start", n, ends+1)
			}
			var content string
			var byteLen, mtime int64
			if err := admin.QueryRow(ctx, "SELECT content, byte_len, mtime_ms FROM public.omp_session_files WHERE path = $1", path).Scan(&content, &byteLen, &mtime); err != nil {
				t.Fatal(err)
			}
			rows, err := admin.Query(ctx, "SELECT start_offset, content FROM public.omp_session_files_parts WHERE path = $1 ORDER BY start_offset", path)
			if err != nil {
				t.Fatal(err)
			}
			parts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
				var offset int64
				var part string
				err := row.Scan(&offset, &part)
				return fmt.Sprintf("%d:%s", offset, part), err
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"0:" + string(claudeContent)}; content != "" || byteLen != int64(len(claudeContent)) || mtime != podSessionWritten || !slices.Equal(parts, want) {
				t.Errorf("the row is content %q, byte_len %d, mtime_ms %d, parts %q; want it written once: no content of its own, %d, %d, %q",
					content, byteLen, mtime, parts, len(claudeContent), podSessionWritten, want)
			}
		})
	}

	t.Run("a login without rights on the tables fails after 4 closes, and its Conn answers on a new one once it has them", func(t *testing.T) {
		role, roleURL := testLogin(t, admin, dsn, "legion_ompsessions_norights")
		trace := &tracer{}
		conn := tracedConn(t, roleURL, trace)
		trace.take()
		_, _, err := Written(ctx, conn, podSessionFile)
		sent, connects := trace.take()
		pids := openings(sent)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d tries", tries)) || !strings.Contains(err.Error(), "SQLSTATE 42501") ||
			!strings.Contains(err.Error(), "permission denied for table omp_session_files") {
			t.Errorf("Written as a login without rights = %v; want it to fail after %d tries, naming SQLSTATE 42501 and the server's message", err, tries)
		}
		if connects != tries-1 || len(pids) != tries || !distinct(pids) {
			t.Fatalf("the client opened %d connections and opened its transaction on %v; want %d opened and %d distinct backends", connects, pids, tries-1, tries)
		}
		gone(t, admin, pids)
		if !conn.IsClosed() {
			t.Error("after the fourth failure the Conn is open, want its connection closed")
		}
		if failure := noRollback(sent); failure != "" {
			t.Error(failure)
		}

		if _, err := admin.Exec(ctx, "GRANT SELECT ON public.omp_session_files TO "+role); err != nil {
			t.Fatal(err)
		}
		at, found, err := Written(ctx, conn, podSessionFile)
		sent, connects = trace.take()
		if err != nil || !found || at.UnixMilli() != podSessionWritten {
			t.Fatalf("Written on the same Conn once the login may read = %v, %t, %v; want the row's %d", at, found, err, podSessionWritten)
		}
		if next := openings(sent); connects != 1 || len(next) != 1 || slices.Contains(pids, next[0]) || conn.c.PgConn().PID() != next[0] || conn.IsClosed() {
			t.Errorf("the next call opened %d connections and opened its transaction on %v, the Conn on %d (closed %t); "+
				"want one new backend, none of %v, which the Conn holds open", connects, next, conn.c.PgConn().PID(), conn.IsClosed(), pids)
		}

		if err := conn.Close(ctx); err != nil {
			t.Fatal(err)
		}
		_, _, err = Written(ctx, conn, podSessionFile)
		sent, connects = trace.take()
		if err == nil || !strings.Contains(err.Error(), "closed by its holder") {
			t.Errorf("Written on a Conn its holder closed = %v; want it refused as closed by its holder", err)
		}
		if connects != 0 || len(sent) != 0 || !conn.IsClosed() {
			t.Errorf("a call on a Conn its holder closed opened %d connections and sent %d statements, closed %t; want none, none, closed", connects, len(sent), conn.IsClosed())
		}
	})
}

// A connection that ends before the client sent COMMIT holds nothing that can have committed, so
// the transaction runs again on one more connection: ended at the opening, and just before a
// statement of the work. Once COMMIT was sent nobody knows whether it committed, so a connection
// that ends there is closed and its error returned, with no second connection and no second
// write: a retry would answer a write that landed as moved, which stops the keeper saving the
// file. A context that ends inside a try is its error, with no second connection. And a reopen the
// server refuses, the login made NOLOGIN after its first connection, ends the call with that
// refusal, which names no password.
func TestALostConnectionIsTriedAgainBeforeCommitAndNotAfter(t *testing.T) {
	ctx := context.Background()
	dsn, admin := repairDatabase(t, "legion_ompsessions_lost")

	// tried is what is wrong with a call whose first try lost its connection and whose second
	// answered: the client opened one more connection, on a backend of its own, and the first is gone.
	tried := func(t *testing.T, conn *Conn, sent []sentTo, connects int) {
		t.Helper()
		pids := openings(sent)
		if connects != 1 || len(pids) != 2 || !distinct(pids) || conn.c.PgConn().PID() != pids[1] {
			t.Fatalf("the client opened %d connections and opened its transaction on %v, the Conn on %d; "+
				"want one more connection and two distinct backends, the Conn on the second", connects, pids, conn.c.PgConn().PID())
		}
		gone(t, admin, pids[:1])
		if failure := noRollback(sent); failure != "" {
			t.Error(failure)
		}
	}

	t.Run("ended at the opening, it runs on one more connection", func(t *testing.T) {
		trace := &tracer{}
		conn := tracedConn(t, dsn, trace)
		trace.take()
		ended := false
		trace.hook(func(c *pgx.Conn, sql string) {
			if sql == OpenRead && !ended {
				ended = true
				endBackend(t, admin, c.PgConn().PID())
			}
		})
		at, found, err := Written(ctx, conn, podSessionFile)
		trace.hook(nil)
		sent, connects := trace.take()
		if !ended || err != nil || !found || at.UnixMilli() != podSessionWritten {
			t.Fatalf("Written with its first opening's backend ended (ended %t) = %v, %t, %v; want the row's %d", ended, at, found, err, podSessionWritten)
		}
		tried(t, conn, sent, connects)
	})

	t.Run("ended before a statement of the work, it runs on one more connection", func(t *testing.T) {
		trace := &tracer{}
		conn := tracedConn(t, dsn, trace)
		trace.take()
		ended := false
		trace.hook(func(c *pgx.Conn, sql string) {
			if isWork(sql) && !ended {
				ended = true
				endBackend(t, admin, c.PgConn().PID())
			}
		})
		content, found, err := Read(ctx, conn, podSessionFile)
		trace.hook(nil)
		sent, connects := trace.take()
		if !ended || err != nil || !found || string(content) != session {
			t.Fatalf("Read with its first work statement's backend ended (ended %t) = %q, %t, %v; want the session whole, "+
				"nothing kept from the failed try", ended, content, found, err)
		}
		tried(t, conn, sent, connects)
	})

	t.Run("ended at COMMIT, it is returned, with no second connection and no second write", func(t *testing.T) {
		trace := &tracer{}
		conn := tracedConn(t, dsn, trace)
		trace.take()
		ended := false
		trace.hook(func(c *pgx.Conn, sql string) {
			if sql == "commit" && !ended {
				ended = true
				endBackend(t, admin, c.PgConn().PID())
			}
		})
		err := Replace(ctx, conn, claudeTranscript, Absent, claudeContent, time.Now())
		trace.hook(nil)
		sent, connects := trace.take()
		var moved *MovedError
		if !ended || err == nil || errors.As(err, &moved) || !strings.Contains(err.Error(), "COMMIT") {
			t.Fatalf("Replace with its backend ended at COMMIT (ended %t) = %v; want the lost commit's error, which is no *MovedError", ended, err)
		}
		if pids := openings(sent); connects != 0 || len(pids) != 1 || sentCount(sent, "commit") != 1 || !conn.IsClosed() {
			t.Errorf("after a lost COMMIT the client opened %d connections, opened its transaction on %v and sent %d COMMIT, closed %t; "+
				"want no new connection, one opening, one COMMIT, the connection closed", connects, pids, sentCount(sent, "commit"), conn.IsClosed())
		}
		var held int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM public.omp_session_files WHERE path = $1", claudeTranscript).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Errorf("the table holds %d rows at %s, want none: the backend ended before it read the COMMIT", held, claudeTranscript)
		}
	})

	t.Run("a context cancelled inside a try is its error, with no second connection", func(t *testing.T) {
		trace := &tracer{}
		conn := tracedConn(t, dsn, trace)
		trace.take()
		cancelled, cancel := context.WithCancel(ctx)
		defer cancel()
		trace.hook(func(_ *pgx.Conn, sql string) {
			if isWork(sql) {
				cancel()
			}
		})
		_, _, err := Written(cancelled, conn, podSessionFile)
		trace.hook(nil)
		sent, connects := trace.take()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Written whose context was cancelled inside its try = %v; want an error that is context.Canceled", err)
		}
		if pids := openings(sent); connects != 0 || len(pids) != 1 || !conn.IsClosed() {
			t.Errorf("the cancelled call opened %d connections and opened its transaction on %v, closed %t; want no new connection, "+
				"one opening, and the connection closed", connects, pids, conn.IsClosed())
		}
		if failure := noRollback(sent); failure != "" {
			t.Error(failure)
		}
	})

	t.Run("a reopen the server refuses ends the call with its refusal, which names no password", func(t *testing.T) {
		role, roleURL := testLogin(t, admin, dsn, "legion_ompsessions_nologin", "GRANT SELECT ON public.omp_session_files TO %s")
		trace := &tracer{}
		conn := tracedConn(t, roleURL, trace)
		// The login's own second connection ends its first: a login may end a backend of its own.
		killer, err := pgx.Connect(ctx, roleURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = killer.Close(context.Background()) })
		if _, err := admin.Exec(ctx, "ALTER ROLE "+role+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		trace.take()
		ended := false
		trace.hook(func(c *pgx.Conn, sql string) {
			if isWork(sql) && !ended {
				ended = true
				endBackend(t, killer, c.PgConn().PID())
			}
		})
		_, _, err = Written(ctx, conn, podSessionFile)
		trace.hook(nil)
		sent, connects := trace.take()
		parsed, perr := url.Parse(roleURL)
		if perr != nil {
			t.Fatal(perr)
		}
		password, _ := parsed.User.Password()
		if !ended || err == nil || !strings.Contains(err.Error(), "is not permitted to log in") || strings.Contains(err.Error(), password) {
			t.Fatalf("Written whose backend ended once its login was made NOLOGIN (ended %t) = %v; want the reopen's refusal, "+
				"naming no password", ended, err)
		}
		if pids := openings(sent); connects != 1 || len(pids) != 1 {
			t.Errorf("the call tried %d connections and opened its transaction on %v; want one refused reopen and the first try alone", connects, pids)
		}
		t.Logf("the reopen's refusal: %v", err)
	})
}

// A write under a read-only default of the login (ALTER ROLE … SET) or of the database (ALTER
// DATABASE … SET) refuses with *ReadOnlyDefaultError and writes nothing, though BEGIN READ WRITE
// would let it: it reads that default as the reset value of default_transaction_read_only right
// after its opening, rolls back, and keeps its connection, the same backend before and after, with
// no second connection. Each read answers under it. A session-level SET another client left on the
// connection is no default of either, and refuses no write.
func TestAWriteUnderAReadOnlyDefaultRefusesAndKeepsItsConnection(t *testing.T) {
	ctx := context.Background()

	// refusesEachWrite holds conn, a connection under a read-only default, to refusing each write
	// and answering each read.
	refusesEachWrite := func(t *testing.T, conn *Conn, trace *tracer, admin *pgx.Conn) {
		t.Helper()
		pid := conn.c.PgConn().PID()
		before := testdb.TableRows(t, admin)
		trace.take()
		for _, write := range []struct {
			name string
			run  func() error
		}{
			{"CreateTables", func() error { return CreateTables(ctx, conn) }},
			{"Import", func() error {
				_, err := Import(ctx, conn, sessionPath, []byte(session), time.Now())
				return err
			}},
			{"Append", func() error { return Append(ctx, conn, podSessionFile, int64(len(session)), claudeContent, time.Now()) }},
			{"Replace", func() error {
				return Replace(ctx, conn, podSessionFile, int64(len(session)), claudeContent, time.Now())
			}},
		} {
			err := write.run()
			sent, connects := trace.take()
			var refusal *ReadOnlyDefaultError
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "default_transaction_read_only = on") {
				t.Errorf("%s under a read-only default = %v; want a *ReadOnlyDefaultError naming default_transaction_read_only = on", write.name, err)
			}
			var statements []string
			for _, s := range sent {
				statements = append(statements, s.sql)
			}
			if want := []string{OpenWrite, readOnlyCheck, "rollback"}; !slices.Equal(statements, want) {
				t.Errorf("%s sent %d statements, %q; want the opening, the read-only check and ROLLBACK alone", write.name, len(statements), statements)
			}
			if connects != 0 || conn.IsClosed() || conn.c.PgConn().PID() != pid {
				t.Errorf("%s opened %d connections, and the Conn is closed %t on backend %d; want none, open, on %d", write.name, connects, conn.IsClosed(), conn.c.PgConn().PID(), pid)
			}
		}
		if changed := testdb.ChangedTables(before, testdb.TableRows(t, admin)); len(changed) > 0 {
			t.Errorf("the refused writes changed %v, want nothing written", changed)
		}
		if at, found, err := Written(ctx, conn, podSessionFile); err != nil || !found || at.UnixMilli() != podSessionWritten {
			t.Errorf("Written under a read-only default = %v, %t, %v; want the row's %d", at, found, err, podSessionWritten)
		}
		if path, found, err := FindByID(ctx, conn, podSessionID); err != nil || !found || path != podSessionFile {
			t.Errorf("FindByID under a read-only default = %q, %t, %v; want %s", path, found, err, podSessionFile)
		}
		if content, found, err := Read(ctx, conn, podSessionFile); err != nil || !found || string(content) != session {
			t.Errorf("Read under a read-only default = %q, %t, %v; want the session", content, found, err)
		}
		if _, connects := trace.take(); connects != 0 || conn.c.PgConn().PID() != pid || !alive(t, admin, pid) {
			t.Errorf("the reads opened %d connections, and the Conn is on backend %d; want none, and %d still in pg_stat_activity", connects, conn.c.PgConn().PID(), pid)
		}
	}

	t.Run("the login's default", func(t *testing.T) {
		dsn, admin := repairDatabase(t, "legion_ompsessions_rologin")
		role, roleURL := testLogin(t, admin, dsn, "legion_ompsessions_readonly",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON public.omp_session_files, public.omp_session_files_parts TO %s",
			"ALTER ROLE %s SET default_transaction_read_only = on")
		trace := &tracer{}
		conn := tracedConn(t, roleURL, trace)
		var reset string
		if err := conn.c.QueryRow(ctx, "SELECT reset_val FROM pg_catalog.pg_settings WHERE name = 'default_transaction_read_only'").Scan(&reset); err != nil || reset != "on" {
			t.Fatalf("control: as %s the reset value of default_transaction_read_only is %q, %v; want on, the login's default", role, reset, err)
		}
		refusesEachWrite(t, conn, trace, admin)
	})

	t.Run("the database's default", func(t *testing.T) {
		dsn, admin := repairDatabase(t, "legion_ompsessions_rodatabase")
		setDatabaseDefault(t, admin, "default_transaction_read_only = on")
		trace := &tracer{}
		conn := tracedConn(t, dsn, trace)
		var reset string
		if err := conn.c.QueryRow(ctx, "SELECT reset_val FROM pg_catalog.pg_settings WHERE name = 'default_transaction_read_only'").Scan(&reset); err != nil || reset != "on" {
			t.Fatalf("control: in the database the reset value of default_transaction_read_only is %q, %v; want on, the database's default", reset, err)
		}
		refusesEachWrite(t, conn, trace, admin)
	})

	t.Run("a session-level SET left on the connection refuses no write", func(t *testing.T) {
		dsn, admin := repairDatabase(t, "legion_ompsessions_roleft")
		trace := &tracer{}
		conn := tracedConn(t, dsn, trace)
		if _, err := conn.c.Exec(ctx, "SET default_transaction_read_only = on"); err != nil {
			t.Fatal(err)
		}
		var setting, reset string
		if err := conn.c.QueryRow(ctx, "SELECT setting, reset_val FROM pg_catalog.pg_settings WHERE name = 'default_transaction_read_only'").Scan(&setting, &reset); err != nil ||
			setting != "on" || reset != "off" {
			t.Fatalf("control: under the left SET default_transaction_read_only reads setting %q, reset_val %q, %v; want on and off", setting, reset, err)
		}
		if err := Replace(ctx, conn, claudeTranscript, Absent, claudeContent, time.Now()); err != nil {
			t.Fatalf("Replace under a left SET default_transaction_read_only = on: %v; want it written", err)
		}
		var byteLen int64
		if err := admin.QueryRow(ctx, "SELECT byte_len FROM public.omp_session_files WHERE path = $1", claudeTranscript).Scan(&byteLen); err != nil || byteLen != int64(len(claudeContent)) {
			t.Errorf("the row Replace wrote has byte_len %d, %v; want %d", byteLen, err, len(claudeContent))
		}
	})
}

// testMark begins each statement this test runs itself through the helper, so the tracer's hook
// tells it from the client's own.
const testMark = "/* left-setting-test */ "

// A transaction_timeout another client left on the connection, 300 ms, ends no transaction of the
// client that runs longer: the opening sets it to 0, which stops the timer BEGIN started with the
// left value, and then back to its reset value for the transaction, so a Read whose work the tracer
// pauses 600 ms answers, on the connection the value was left on. It holds under the server's
// default, 0, and under a database default of 10s, which a marked read inside the transaction reads
// back. Each case's control is a transaction on a connection with the same left value that does not
// stop the timer, which the server ends: under the server's default one opened as the client's were
// before the openings (pgx's begin read only), and under the database's default OpenRead without
// its stop of the timer, since a reset to 10s alone leaves the 300 ms timer running. A server with no
// transaction_timeout (PostgreSQL 16) has nothing to stop, and the test skips there.
func TestTheOpeningStopsALeftTransactionTimeout(t *testing.T) {
	ctx := context.Background()
	const left, pause = "SET transaction_timeout = '300ms'", 600 * time.Millisecond
	for _, database := range []struct {
		name, setting, want string
		control             func(t *testing.T) pgx.TxOptions
	}{
		{"under the server's default", "", "0", func(*testing.T) pgx.TxOptions { return pgx.TxOptions{AccessMode: pgx.ReadOnly} }},
		{"under a database default of 10s", "10s", "10s", func(t *testing.T) pgx.TxOptions {
			return pgx.TxOptions{BeginQuery: openingWithout(t, OpenRead, stopTransactionTimer)}
		}},
	} {
		t.Run(database.name, func(t *testing.T) {
			dsn, admin := repairDatabase(t, "legion_ompsessions_timeout")
			var has bool
			if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_catalog.pg_settings WHERE name = 'transaction_timeout')").Scan(&has); err != nil {
				t.Fatal(err)
			}
			if !has {
				t.Skip("the server has no transaction_timeout, which PostgreSQL 17 added, so no value of it another client left can end a transaction here")
			}
			if database.setting != "" {
				setDatabaseDefault(t, admin, "transaction_timeout = '"+database.setting+"'")
			}

			live := t.Run("control: a transaction that does not stop the left timer is ended", func(t *testing.T) {
				raw, err := pgx.Connect(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = raw.Close(context.Background()) })
				if _, err := raw.Exec(ctx, left); err != nil {
					t.Fatal(err)
				}
				err = pgx.BeginTxFunc(ctx, raw, database.control(t), func(tx pgx.Tx) error {
					time.Sleep(pause)
					var one int
					return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
				})
				if err == nil || !raw.IsClosed() {
					t.Errorf("a transaction that does not stop the left timer, paused %s = %v, connection closed %t; want the server to end it", pause, err, raw.IsClosed())
				}
				t.Logf("control: %v", err)
			})
			if !live {
				t.Fatal("a left transaction_timeout that ends no transaction is no left setting: the rows below would pass whatever the opening sets")
			}

			trace := &tracer{}
			conn := tracedConn(t, dsn, trace)
			if _, err := conn.c.Exec(ctx, left); err != nil {
				t.Fatal(err)
			}
			pid := conn.c.PgConn().PID()
			trace.take()
			trace.hook(func(_ *pgx.Conn, sql string) {
				if isWork(sql) && !strings.HasPrefix(sql, testMark) {
					time.Sleep(pause)
				}
			})
			content, found, err := Read(ctx, conn, podSessionFile)
			trace.hook(nil)
			_, connects := trace.take()
			if err != nil || !found || string(content) != session {
				t.Fatalf("Read paused %s under a left transaction_timeout of 300ms = %q, %t, %v; want the session", pause, content, found, err)
			}
			if connects != 0 || conn.IsClosed() || conn.c.PgConn().PID() != pid {
				t.Fatalf("the Read opened %d connections and its Conn is on backend %d, closed %t; want none, on %d: a Read the server ended and "+
					"the client ran again elsewhere would hide the timer", connects, conn.c.PgConn().PID(), conn.IsClosed(), pid)
			}
			var inside string
			if err := inTransaction(ctx, conn, true, func(p pinned) error {
				return p.queryRow(ctx, testMark+"SELECT pg_catalog.current_setting('transaction_timeout')").Scan(&inside)
			}); err != nil || inside != database.want {
				t.Errorf("inside the client's transaction transaction_timeout reads %q, %v; want %q, the reset value", inside, err, database.want)
			}
			var after string
			if err := conn.c.QueryRow(ctx, "SELECT pg_catalog.current_setting('transaction_timeout')").Scan(&after); err != nil || after != "300ms" {
				t.Errorf("after the client's transactions the session's transaction_timeout reads %q, %v; want 300ms, the left value, "+
					"which the opening set for its transaction alone", after, err)
			}
		})
	}
}
