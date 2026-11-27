package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// k14 reads the transactions a write starts from Postgres's statement log, the jsonlog file the
// instrument turns log_min_duration_statement 0 on for (capture.go): every statement each backend
// ran, as one line when it ended, with its duration, its backend's pid and the application name its
// connection gave. A transaction is a backend's statements from its begin to its commit or its
// rollback, from the begin's start to the commit's end; a statement run outside one is a
// transaction of its own. So the time of the write's own commit is the end of the commit of the
// transaction that ran the write's statement, read from the log the database wrote, not from a
// poll, and the time of each transaction the write starts is its own span. The instrument's own
// connections name themselves measureApp and are left out.

// measureApp is the application name of k14's own connections to the database.
const measureApp = "evalbench-measure"

// withApplication is the database URL url with measureApp as its application name.
func withApplication(url string) string {
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	return url + separator + "application_name=" + measureApp
}

// logLine is one line of the statement log: when it was written, which backend wrote it, the
// connection's application name, the kind of what ran (statement, execute, parse or bind), its
// text, its duration and its parameters' literals.
type logLine struct {
	at     time.Time
	pid    int64
	app    string
	kind   string
	text   string
	ms     float64
	params []string
}

// durationLine reads a duration line of the statement log: its milliseconds, the kind of what ran
// (a simple query's statement, or an extended-protocol parse, bind or execute, with the prepared
// statement's name) and its text.
var durationLine = regexp.MustCompile(`(?s)^duration: ([0-9.]+) ms  (statement|execute|parse|bind)(?: [^:]*)?: (.*)$`)

// parseLogLine reads one jsonlog line, false for one that is no duration line.
func parseLogLine(raw []byte) (logLine, bool) {
	var line struct {
		Timestamp string `json:"timestamp"`
		PID       int64  `json:"pid"`
		App       string `json:"application_name"`
		Message   string `json:"message"`
		Detail    string `json:"detail"`
	}
	if json.Unmarshal(raw, &line) != nil {
		return logLine{}, false
	}
	match := durationLine.FindStringSubmatch(line.Message)
	if match == nil {
		return logLine{}, false
	}
	at, err := time.Parse("2006-01-02 15:04:05.000 MST", line.Timestamp)
	if err != nil {
		return logLine{}, false
	}
	ms, _ := strconv.ParseFloat(match[1], 64)
	params, _ := parseParameters(line.Detail)
	return logLine{at: at.UTC(), pid: line.PID, app: line.App, kind: match[2], text: match[3], ms: ms, params: params}, true
}

// started is when the line's statement began: its end less its duration.
func (l logLine) started() time.Time {
	return l.at.Add(-time.Duration(l.ms * float64(time.Millisecond)))
}

// transaction is one transaction of the statement log: its backend and application name, when it
// began and ended, how it ended (commit, rollback, implicit for a statement outside a transaction,
// or open where the log ends first), the statements it ran (an execute or a simple query's, not its
// begin or its end), the tables they wrote, and the mark of the first of them a reader marked
// (transactionReader), "" for none.
type transaction struct {
	pid        int64
	app        string
	began, end time.Time
	ended      string
	statements int
	writes     []string
	mark       string
}

// ms is how long the transaction was open.
func (t transaction) ms() float64 {
	return float64(t.end.Sub(t.began).Microseconds()) / 1000
}

// writtenTable reads each table a statement writes: an insert, update, delete or merge, at its head
// or in a data-modifying with; the keywords a conflict clause or a row lock puts after `update` are
// no table.
var writtenTable = regexp.MustCompile(`(?i)\b(?:insert\s+into|update|delete\s+from|merge\s+into)\s+(?:only\s+)?("?[a-z_][a-z0-9_.]*"?)`)

// notTables are the words writtenTable can read after `update` that name no table.
var notTables = []string{"set", "skip", "of", "nowait"}

// tablesWritten is the tables text writes, each once, in the order it names them.
func tablesWritten(text string) []string {
	var tables []string
	for _, match := range writtenTable.FindAllStringSubmatch(text, -1) {
		table := strings.ToLower(strings.Trim(match[1], `"`))
		if slices.Contains(notTables, table) || slices.Contains(tables, table) {
			continue
		}
		tables = append(tables, table)
	}
	return tables
}

// transactionReader groups the statement log's lines, given in the order the log holds them, into
// each backend's transactions, keeping of each line only what its transaction needs, so a log of
// many statements, each with its parameters, is read in the memory of its transactions alone. mark
// names a statement of interest, "" for any other.
type transactionReader struct {
	mark func(logLine) string
	open map[int64]*transaction
	done []transaction
}

func newTransactionReader(mark func(logLine) string) *transactionReader {
	return &transactionReader{mark: mark, open: map[int64]*transaction{}}
}

// add reads one line.
func (t *transactionReader) add(line logLine) {
	if line.kind == "parse" || line.kind == "bind" {
		return
	}
	word := ""
	if fields := strings.Fields(line.text); len(fields) > 0 {
		word = strings.TrimSuffix(strings.ToLower(fields[0]), ";")
	}
	closing := word == "commit" || word == "end" || word == "rollback" || word == "abort"
	current := t.open[line.pid]
	switch {
	case word == "begin" || word == "start":
		if current != nil {
			current.ended = "open"
			t.done = append(t.done, *current)
		}
		t.open[line.pid] = &transaction{pid: line.pid, app: line.app, began: line.started(), end: line.at}
	case closing && current != nil:
		current.end, current.ended = line.at, "commit"
		if word == "rollback" || word == "abort" {
			current.ended = "rollback"
		}
		t.done = append(t.done, *current)
		delete(t.open, line.pid)
	case closing:
		// The end of a transaction that began before the part of the log read.
	case current != nil:
		current.end = line.at
		current.statements++
		for _, table := range tablesWritten(line.text) {
			if !slices.Contains(current.writes, table) {
				current.writes = append(current.writes, table)
			}
		}
		if current.mark == "" && t.mark != nil {
			current.mark = t.mark(line)
		}
	default:
		one := transaction{pid: line.pid, app: line.app, began: line.started(), end: line.at, ended: "implicit", statements: 1, writes: tablesWritten(line.text)}
		if t.mark != nil {
			one.mark = t.mark(line)
		}
		t.done = append(t.done, one)
	}
}

// transactions is every transaction read, those the log ends inside of as open, in the order they
// began.
func (t *transactionReader) transactions() []transaction {
	done := slices.Clone(t.done)
	for _, pid := range slices.Sorted(maps.Keys(t.open)) {
		current := *t.open[pid]
		current.ended = "open"
		done = append(done, current)
	}
	slices.SortStableFunc(done, func(a, b transaction) int { return a.began.Compare(b.began) })
	return done
}

// transactionsOf is the transactions of lines (transactionReader).
func transactionsOf(lines []logLine, mark func(logLine) string) []transaction {
	reader := newTransactionReader(mark)
	for _, line := range lines {
		reader.add(line)
	}
	return reader.transactions()
}

// scanTransactions is the transactions the jsonlog file at path holds after offset
// (transactionReader).
func scanTransactions(path string, offset int64, mark func(logLine) string) ([]transaction, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the statement log: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	reader := newTransactionReader(mark)
	for scanner.Scan() {
		if line, ok := parseLogLine(scanner.Bytes()); ok {
			reader.add(line)
		}
	}
	return reader.transactions(), scanner.Err()
}

// markWrite marks, as "write", a statement that matches statement: the write's own.
func markWrite(statement *regexp.Regexp) func(logLine) string {
	return func(line logLine) string {
		if statement.MatchString(line.text) {
			return "write"
		}
		return ""
	}
}

// writeOf is the transaction of transactions that ran the write: the first marked as it
// (markWrite), run by a connection other than the instrument's, and which began between from and
// to.
func writeOf(transactions []transaction, from, to time.Time) (transaction, bool) {
	for _, one := range transactions {
		if one.mark == "write" && one.app != measureApp && !one.began.Before(from) && !one.began.After(to) {
			return one, true
		}
	}
	return transaction{}, false
}

// The kinds of transaction k14 tells a write's from (kindOf).
const (
	txRoute     = "route"
	txRefresher = "refresher"
	txSignals   = "signals"
	txChanges   = "change-log"
	txReads     = "reads"
	txOther     = "background"
)

// signalsTables are the tables the production signals' loop writes (prodevents/signals).
var signalsTables = []string{"production_events", "production_deploy_rules", "production_signal_cursors"}

// kindOf is which work a transaction is: the write's own (route), the day-row refresher's and the
// stored figures' (it writes an eval_ table), the production signals' loop's, the change log's
// drain (it writes the change log alone), a read (it writes nothing), or other background work of
// the server's (the Legion facts, the keyword fill, the reconcile's bookkeeping and the rest).
func kindOf(one transaction, write transaction) string {
	switch {
	case one.pid == write.pid && one.began.Equal(write.began):
		return txRoute
	case len(one.writes) == 0:
		return txReads
	case slices.ContainsFunc(one.writes, func(table string) bool { return strings.HasPrefix(table, "eval_") }):
		return txRefresher
	case slices.ContainsFunc(one.writes, func(table string) bool { return slices.Contains(signalsTables, table) }):
		return txSignals
	case !slices.ContainsFunc(one.writes, func(table string) bool { return !strings.HasPrefix(table, "delivery_changes") }):
		return txChanges
	}
	return txOther
}

// startedBy reports whether a transaction of kind is one a write starts: its own, and the
// refresher's, the signals' and the change log's that follow it.
func startedBy(kind string) bool {
	return kind == txRoute || kind == txRefresher || kind == txSignals || kind == txChanges
}
