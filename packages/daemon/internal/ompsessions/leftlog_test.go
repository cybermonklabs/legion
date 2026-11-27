package ompsessions

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// clientName is the application_name each opening sets for its transaction with its second
// statement (applicationNameLine), so that the server's log names each statement of the client.
const clientName = "legion-session-client"

// logRecord is what the log rule reads of one line of PostgreSQL 16's jsonlog: the backend's pid;
// the application_name in effect when the line was written, a key the server leaves out where the
// name is empty; the severity and the message; the text of the statement the line was written in,
// which an ERROR line carries under log_min_error_statement = error; and the backend's type, which
// the test reads to find the fixture's client sessions (write_jsonlog in
// src/backend/utils/error/jsonlog.c at PostgreSQL's tag REL_16_16, and
// https://www.postgresql.org/docs/16/runtime-config-logging.html#RUNTIME-CONFIG-LOGGING-JSONLOG).
type logRecord struct {
	PID             *uint32 `json:"pid"`
	ApplicationName *string `json:"application_name"`
	Severity        string  `json:"error_severity"`
	Message         string  `json:"message"`
	Statement       *string `json:"statement"`
	BackendType     string  `json:"backend_type"`
}

// statementMessage is the message of the line log_min_duration_statement = 0 has the server write
// when a statement ends: "duration: <ms> ms  statement: <text>" for a query of the simple protocol,
// and "duration: <ms> ms  parse|bind|execute <name>: <text>" for each step of the extended protocol,
// where <name> is the prepared statement's name, <unnamed> for the unnamed one, with /<portal>
// after it for a named portal (exec_simple_query, exec_parse_message, exec_bind_message and
// exec_execute_message in src/backend/tcop/postgres.c at REL_16_16). Its groups are the step, empty
// for a simple query, the name and the text.
var statementMessage = regexp.MustCompile(`(?s)\Aduration: [0-9]+\.[0-9]+ ms  (?:statement|(parse|bind|execute) (\S+?)): (.*)\z`)

// pidField is the pid of a line that is not one JSON object, such as a line cut short: jsonlog
// writes it near the start of each line, as "pid":<n>.
var pidField = regexp.MustCompile(`"pid":([0-9]+)[,}]`)

// logStatement is the step one line records and the text of its statement: "statement" for a query
// of the simple protocol and "parse", "bind" or "execute" for a step of the extended protocol, each
// from a line of statementMessage's shape, and "ERROR" for an ERROR line that carries the text of
// the statement the server refused. ok is false for a line of any other shape, such as a duration
// with no text, which the server writes where log_statement already logged the text, or another
// message the server writes while a statement runs (serverNote).
func logStatement(record logRecord) (step, text string, ok bool) {
	if match := statementMessage.FindStringSubmatch(record.Message); match != nil {
		return cmp.Or(match[1], "statement"), match[3], true
	}
	if record.Severity == "ERROR" && record.Statement != nil {
		return "ERROR", *record.Statement, true
	}
	return "", "", false
}

// statementRecord is the start of a LOG message that records a statement in a shape
// statementMessage does not read: a duration with no text, which log_min_duration_statement writes
// where log_statement already logged the text, and log_statement's own records, "statement: <text>"
// and "<step> <name>: <text>" (exec_simple_query, exec_parse_message, exec_bind_message and
// exec_execute_message in src/backend/tcop/postgres.c at REL_16_16).
var statementRecord = regexp.MustCompile(`\A(?:duration: |statement: |(?:parse|bind|execute) \S+: )`)

// serverNote reports whether record is a line the server writes while a statement runs that
// records no statement: a LOG line whose message is no statement record (statementRecord). Three
// exempt entries of LeftSettings write such lines inside the client's transactions on the backend a
// case leaves them on: debug_print_parse and debug_print_rewritten the trees "parse tree:" and
// "rewritten parse tree:" of each statement the client sends (elog_node_display in
// src/backend/nodes/print.c at REL_16_16), and trace_sort a line about each sort, "begin tuple sort:
// …" and "performsort of worker …" among them (src/backend/utils/sort/tuplesort.c). Oh My Pi's own
// left-setting Job met the trees on PostgreSQL 16.16 and held them to its name.
func serverNote(record logRecord) bool {
	return record.Severity == "LOG" && !statementRecord.MatchString(record.Message)
}

// endsTransaction reports whether text is COMMIT or ROLLBACK, in any case: pgx sends commit and
// rollback, and psql sends COMMIT and ROLLBACK as a reader types them.
func endsTransaction(text string) bool {
	text = strings.TrimSpace(text)
	return strings.EqualFold(text, "commit") || strings.EqualFold(text, "rollback")
}

// shownName is the application_name a line shows, as a failure names it.
func shownName(record logRecord) string {
	if record.ApplicationName == nil {
		return "no application_name"
	}
	return fmt.Sprintf("application_name %q", *record.ApplicationName)
}

// excerpt is a line the rule cannot read, as a failure names it: its first 160 bytes, quoted.
func excerpt(line []byte) string {
	if len(line) > 160 {
		return fmt.Sprintf("%q…", line[:160])
	}
	return fmt.Sprintf("%q", line)
}

// clientStatementsInLog is the log rule of TestTheClientHoldsUnderEveryLeftSetting, which holds
// that the server's log shows legion-session-client on every statement of every transaction of the
// client, so that a reader of the log can tell the client's statements from any other client's on
// the same server. It reads jsonlog, the jsonlog file of a PostgreSQL 16 server started
// with log_min_duration_statement = 0 and log_min_error_statement = error, for the lines of the
// backends whose pids are in pids, the ones the client's connections ran on. It answers the number
// of statements of the client it found; each of them that showed an application_name other than
// clientName, or none (other); and each line it could not read or hold (problems). An entry of other
// or of problems names its line by its number in jsonlog, and any entry fails the caller's test.
//
// A line that begins with # is a header, and the rule skips it. Every other line must be one JSON
// object: one that is not, and whose pid the rule cannot read or is in pids, is a problem. The lines
// of every other pid (the postmaster, the checkpointer, the test's own backends) are left out. For
// each pid in pids, in file order, a transaction window starts at a statement, execute or ERROR line
// whose text is OpenRead or OpenWrite whole (the ERROR line is a refused opening's), and ends at the
// next COMMIT or ROLLBACK line, at the next opening, or at the end of the pid's lines. Inside a
// window:
//   - the COMMIT or ROLLBACK line that ends it is left out: it shows the session's own name again,
//     since the SET LOCAL of application_name ended with the transaction;
//   - a line whose text begins with testMark, a statement of the test's own, is left out;
//   - a statement or execute line, or an ERROR line, is one statement, which must show clientName,
//     the opening's line and a refused statement's included: a statement logs its duration only when
//     it ends without an error, so a refused one is counted once, by its ERROR line;
//   - a parse or bind line must show clientName, and is not counted, since its statement's execute
//     line or ERROR line counts it. A prepared statement has any name (pgx's stmtcache_<hash>,
//     PgBouncer's PGBOUNCER_<n>, psql's <unnamed>), and one prepared in an earlier transaction logs
//     bind and execute with no parse;
//   - a line the server writes while a statement runs that records no statement (serverNote), such
//     as a parse tree that debug_print_parse writes or a line of trace_sort, must show clientName,
//     and is not counted, but for one after an ERROR line of the window: the error aborted the
//     transaction, and the abort set each SET LOCAL of it back, application_name's among them
//     (AbortTransaction's AtEOXact_GUC in src/backend/access/transam/xact.c at REL_16_16), so the
//     server writes what it writes for the ROLLBACK the client sends next, such as that statement's
//     parse tree, under the session's own name, and the rule leaves it out as it leaves out the
//     ROLLBACK line; a statement or an ERROR line there still counts, and must show clientName;
//   - a line of any other shape (logStatement) is a problem, a duration with no text among them.
//
// A line outside every window is left out: PgBouncer's own statements on a server connection, such
// as the SET of a parameter it tracks before a client's first query, come between windows.
func clientStatementsInLog(jsonlog []byte, pids map[uint32]bool) (statements int, other, problems []string) {
	inWindow, aborted := map[uint32]bool{}, map[uint32]bool{}
	number := 0
	for line := range bytes.Lines(jsonlog) {
		number++
		line = bytes.TrimSuffix(line, []byte("\n"))
		if bytes.HasPrefix(line, []byte("#")) {
			continue
		}
		var record logRecord
		err := json.Unmarshal(line, &record)
		if err == nil && record.PID == nil {
			err = errors.New("it names no pid")
		}
		if err != nil {
			if match := pidField.FindSubmatch(line); match == nil {
				problems = append(problems, fmt.Sprintf("line %d is not one JSON object with a pid, so the rule cannot tell whose it is (%v): %s",
					number, err, excerpt(line)))
			} else if pid, perr := strconv.ParseUint(string(match[1]), 10, 32); perr != nil || pids[uint32(pid)] {
				problems = append(problems, fmt.Sprintf("line %d (pid %s) is not one JSON object (%v): %s", number, match[1], err, excerpt(line)))
			}
			continue
		}
		pid := *record.PID
		if !pids[pid] {
			continue
		}
		step, text, ok := logStatement(record)
		counted := ok && step != "parse" && step != "bind"
		switch {
		case counted && (text == OpenRead || text == OpenWrite):
			inWindow[pid], aborted[pid] = true, false
		case counted && endsTransaction(text):
			inWindow[pid] = false
			continue
		}
		if !inWindow[pid] {
			continue
		}
		named := record.ApplicationName != nil && *record.ApplicationName == clientName
		where := fmt.Sprintf("line %d (pid %d)", number, pid)
		switch {
		case !ok && serverNote(record):
			if !named && !aborted[pid] {
				problems = append(problems, fmt.Sprintf("%s: the server's %s line %s shows %s, want %q", where, record.Severity,
					shown(record.Message), shownName(record), clientName))
			}
		case !ok:
			problems = append(problems, fmt.Sprintf("%s is in a transaction of the client and has no shape the rule reads: error_severity %q, message %s",
				where, record.Severity, shown(record.Message)))
		case strings.HasPrefix(text, testMark):
		case !counted:
			if !named {
				problems = append(problems, fmt.Sprintf("%s: the %s line of %s shows %s, want %q", where, step, shown(text), shownName(record), clientName))
			}
		default:
			statements++
			if !named {
				other = append(other, fmt.Sprintf("%s: the %s line of %s shows %s", where, step, shown(text), shownName(record)))
			}
		}
		// An ERROR line, the test's own included, aborts the transaction, so what the server writes
		// after it for the ROLLBACK shows the session's own name.
		aborted[pid] = aborted[pid] || step == "ERROR"
	}
	return statements, other, problems
}

// logFixture is PostgreSQL 16's jsonlog of the client's statement shapes, captured as its header
// says: four psql sessions, each sending one of open.go's openings and the statements after it.
var logFixture = filepath.Join("testdata", "pg16-statement-log.jsonl")

// fixtureSessions are the four psql sessions of logFixture's header, in its order, each with the
// opening it sends first and what the log rule must count in it: its statements, and how many of
// them show an application_name other than clientName.
//
// Session 1 sends OpenWrite, the bound SELECT, the refused SELECT 1/0 and COMMIT: 3 statements, the
// opening's statement line, the SELECT's execute line (its parse and bind lines are not counted)
// and the refusal's ERROR line, which logged no duration line and shows legion-session-client, as
// the opening set it, so 0 under another name; COMMIT shows psql and is left out. Session 2 sends
// OpenRead, the bound SELECT twice and ROLLBACK: 3, the opening's line and two execute lines, 0
// under another name. Session 3 sends OpenRead, SET LOCAL application_name = 'another-client',
// SELECT 1 and ROLLBACK: 3, of which the last 2 show another-client. Session 4 sends OpenRead, a
// statement marked /* left-setting-test */ and COMMIT: 1, the opening's line, since the marked
// statement is the test's own and left out.
var fixtureSessions = []struct {
	opening           string
	statements, other int
}{
	{OpenWrite, 3, 0},
	{OpenRead, 3, 0},
	{OpenRead, 3, 2},
	{OpenRead, 1, 0},
}

// The log rule of TestTheClientHoldsUnderEveryLeftSetting (clientStatementsInLog) finds each
// statement of the client in a real PostgreSQL 16 jsonlog, logFixture, with the name it showed. The
// client's pids are the fixture's client backends, the four psql sessions of its header, each of
// which opens with an opening of open.go whole. Over the fixture as captured, for each session alone
// and for the four together, the rule counts exactly fixtureSessions' statements: the openings, the
// executes and the refused statement under legion-session-client, the COMMIT, ROLLBACK and marked
// lines left out, and the two statements of session 3 under another-client as other. psql sends
// only unnamed bound statements, so a copy of session 1's bind and execute lines renamed to
// stmtcache_0123, with no parse before them, as pgx's cache sends a statement it prepared in an
// earlier transaction, adds one statement under the client's name. A parse tree line the server
// writes in a window under legion-session-client, as debug_print_parse writes one there, is held to
// the name and counts no statement, and one after the refused statement of session 1 under psql's
// name, as the server writes it for the ROLLBACK once the error set the SET LOCAL back, is left out.
// The controls are lines the rule must name as problems: a line of a client's pid that is not JSON,
// a line in a window of no shape the rule reads, a bind line under another name, and a parse tree
// line under another name before any error; a line that is not JSON of a pid the client did not use
// is left out.
func TestTheLogRuleCountsEveryStatementOfTheClientUnderItsName(t *testing.T) {
	captured, err := os.ReadFile(logFixture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(captured), "\n"), "\n")
	records := make([]logRecord, len(lines))
	type session struct {
		pid   uint32
		lines []int
	}
	var sessions []*session
	byPID := map[uint32]*session{}
	postmaster := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &records[i]); err != nil || records[i].PID == nil {
			t.Fatalf("line %d of %s is not one JSON object with a pid (%v)", i+1, logFixture, err)
		}
		switch records[i].BackendType {
		case "postmaster":
			if postmaster < 0 {
				postmaster = i
			}
		case "client backend":
			pid := *records[i].PID
			if byPID[pid] == nil {
				byPID[pid] = &session{pid: pid}
				sessions = append(sessions, byPID[pid])
			}
			byPID[pid].lines = append(byPID[pid].lines, i)
		}
	}
	if len(sessions) != len(fixtureSessions) || postmaster < 0 {
		t.Fatalf("%s holds %d client backends and a postmaster line at index %d, want the %d psql sessions of its header and a postmaster",
			logFixture, len(sessions), postmaster, len(fixtureSessions))
	}
	all := map[uint32]bool{}
	var statements, other int
	for n, s := range sessions {
		want := fixtureSessions[n]
		if step, text, ok := logStatement(records[s.lines[0]]); !ok || step != "statement" || text != want.opening {
			t.Fatalf("session %d (pid %d) of %s does not open with open.go's %s whole: the fixture was captured from another open.go, "+
				"so capture it again with this one as its header says", n+1, s.pid, logFixture, shown(want.opening))
		}
		all[s.pid] = true
		statements += want.statements
		other += want.other
	}

	// find is the index of the first line of session n whose step is step and whose text is text,
	// any text where text is empty.
	find := func(n int, step, text string) int {
		for _, i := range sessions[n].lines {
			if got, gotText, ok := logStatement(records[i]); ok && got == step && (text == "" || gotText == text) {
				return i
			}
		}
		t.Fatalf("session %d of %s has no %s line of %q", n+1, logFixture, step, text)
		return -1
	}
	// with is the fixture with inserted written after the line at index after.
	with := func(after int, inserted ...string) []byte {
		return []byte(strings.Join(slices.Insert(slices.Clone(lines), after+1, inserted...), "\n") + "\n")
	}
	// replaced is line with its first old replaced by by; line must hold old.
	replaced := func(line, old, by string) string {
		if !strings.Contains(line, old) {
			t.Fatalf("%s holds no line of %q to change: %s", logFixture, old, excerpt([]byte(line)))
		}
		return strings.Replace(line, old, by, 1)
	}

	setOther, selectOne := find(2, "statement", "SET LOCAL application_name = 'another-client'"), find(2, "statement", "SELECT 1")
	bind, execute, parse := find(0, "bind", ""), find(0, "execute", ""), find(0, "parse", "")
	// tree is session 1's parse line as a parse tree line that debug_print_parse writes, under the
	// application_name name.
	tree := func(name string) string {
		line := replaced(lines[parse], `"message":"`+records[parse].Message+`"`, `"message":"parse tree:","detail":"   {QUERY \n   :commandType 1 \n   }"`)
		return replaced(line, `"application_name":"legion-session-client"`, `"application_name":"`+name+`"`)
	}
	_, bound, _ := logStatement(records[find(1, "execute", "")])
	type row struct {
		name       string
		jsonlog    []byte
		pids       map[uint32]bool
		statements int
		other      int
		otherLines []int  // the line numbers the entries of other name, in order, where the row names them
		problem    string // the start of the one problem the rule must name, "" for none
	}
	var rows []row
	for n, s := range sessions {
		r := row{name: fmt.Sprintf("as captured, session %d alone", n+1), jsonlog: captured, pids: map[uint32]bool{s.pid: true},
			statements: fixtureSessions[n].statements, other: fixtureSessions[n].other}
		if n == 2 {
			r.otherLines = []int{setOther + 1, selectOne + 1}
		}
		rows = append(rows, r)
	}
	rows = append(rows,
		row{name: "as captured, the four sessions", jsonlog: captured, pids: all,
			statements: statements, other: other, otherLines: []int{setOther + 1, selectOne + 1}},
		row{name: "a statement pgx prepared in an earlier transaction, bound and run as stmtcache_0123 with no parse",
			jsonlog: with(execute,
				replaced(lines[bind], "bind <unnamed>:", "bind stmtcache_0123:"),
				replaced(lines[execute], "execute <unnamed>:", "execute stmtcache_0123:")),
			pids: all, statements: statements + 1, other: other},
		row{name: "control: a line of a client's pid that is not JSON",
			jsonlog: with(selectOne, lines[selectOne][:len(lines[selectOne])/2]),
			pids:    all, statements: statements, other: other,
			problem: fmt.Sprintf("line %d (pid %d) is not one JSON object", selectOne+2, sessions[2].pid)},
		row{name: "a line of the postmaster that is not JSON is left out",
			jsonlog: with(postmaster, lines[postmaster][:len(lines[postmaster])/2]),
			pids:    all, statements: statements, other: other},
		row{name: "control: a line in a window of no shape the rule reads, a duration with no statement",
			jsonlog: with(find(1, "execute", ""), replaced(lines[find(1, "execute", "")], "  execute <unnamed>: "+bound, "")),
			pids:    all, statements: statements, other: other,
			problem: fmt.Sprintf("line %d (pid %d) is in a transaction of the client and has no shape the rule reads", find(1, "execute", "")+2, sessions[1].pid)},
		row{name: "control: a bind line in a window under another name",
			jsonlog: with(find(1, "bind", ""), replaced(lines[find(1, "bind", "")], `"application_name":"legion-session-client"`, `"application_name":"psql"`)),
			pids:    all, statements: statements, other: other,
			problem: fmt.Sprintf(`line %d (pid %d): the bind line of %q shows application_name "psql"`, find(1, "bind", "")+2, sessions[1].pid, bound)},
		row{name: "a parse tree line in a window under the client's name, as debug_print_parse writes one, counts no statement",
			jsonlog: with(parse, tree(clientName)), pids: all, statements: statements, other: other},
		row{name: "control: a parse tree line in a window under another name",
			jsonlog: with(parse, tree("psql")), pids: all, statements: statements, other: other,
			problem: fmt.Sprintf(`line %d (pid %d): the server's LOG line "parse tree:" shows application_name "psql"`, parse+2, sessions[0].pid)},
		row{name: "a parse tree line after the refused statement under the session's name, the ROLLBACK's, is left out",
			jsonlog: with(find(0, "ERROR", ""), tree("psql")), pids: all, statements: statements, other: other},
	)
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			statements, other, problems := clientStatementsInLog(r.jsonlog, r.pids)
			t.Logf("statements=%d other-name=%d problems=%d", statements, len(other), len(problems))
			for _, entry := range other {
				t.Logf("other name: %s", entry)
			}
			for _, entry := range problems {
				t.Logf("problem: %s", entry)
			}
			if statements != r.statements {
				t.Errorf("the rule counted %d statements of the client, want %d", statements, r.statements)
			}
			if len(other) != r.other {
				t.Errorf("the rule found %d statements under another name, want %d", len(other), r.other)
			}
			for i, number := range r.otherLines {
				if i < len(other) && !strings.HasPrefix(other[i], fmt.Sprintf("line %d ", number)) {
					t.Errorf("statement %d under another name is %s, want line %d", i+1, other[i], number)
				}
			}
			switch {
			case r.problem == "" && len(problems) > 0:
				t.Errorf("the rule named %d problems, want none", len(problems))
			case r.problem != "" && (len(problems) != 1 || !strings.HasPrefix(problems[0], r.problem)):
				t.Errorf("the rule named %d problems, want one that starts %q", len(problems), r.problem)
			}
		})
	}
}
