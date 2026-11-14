package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The instrument: pg_stat_statements for each statement's database time, and Postgres's statement
// log (log_min_duration_statement 0, in jsonlog, whose query_id is pg_stat_statements' queryid)
// for the parameters each execution was bound with, so each statement is explained as the server
// ran it.

// statement is one row of pg_stat_statements: a statement and what its executions cost since the
// last reset.
type statement struct {
	queryID int64
	query   string
	calls   int64
	execMs  float64
	rows    int64
	hit     int64
	read    int64
}

// resetStatements empties pg_stat_statements.
func resetStatements(ctx context.Context, pool *store.Pool) error {
	if _, err := pool.Exec(ctx, `select pg_stat_statements_reset()`); err != nil {
		return fmt.Errorf("reset pg_stat_statements (is it in shared_preload_libraries, and is the role a superuser?): %w", err)
	}
	return nil
}

// snapshotStatements is every statement this database ran since the last reset, the instrument's
// own reads of pg_stat_statements left out.
func snapshotStatements(ctx context.Context, pool *store.Pool) ([]statement, error) {
	rows, err := pool.Query(ctx, `
		select queryid, query, calls, total_exec_time, rows, shared_blks_hit, shared_blks_read
		from pg_stat_statements
		where dbid = (select oid from pg_database where datname = current_database())
		  and calls > 0 and query not like '%pg_stat_statements%'
		order by total_exec_time desc`)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	var all []statement
	var one statement
	if _, err := pgx.ForEachRow(rows, []any{&one.queryID, &one.query, &one.calls, &one.execMs, &one.rows, &one.hit, &one.read}, func() error {
		all = append(all, one)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	return all, nil
}

// setStatementLog turns Postgres's log of every statement, with its parameters, on or off.
func setStatementLog(ctx context.Context, pool *store.Pool, on bool) error {
	change := `alter system reset log_min_duration_statement`
	if on {
		change = `alter system set log_min_duration_statement = 0`
	}
	if _, err := pool.Exec(ctx, change); err != nil {
		return fmt.Errorf("set the statement log (the role must be a superuser): %w", err)
	}
	if _, err := pool.Exec(ctx, `select pg_reload_conf()`); err != nil {
		return fmt.Errorf("reload Postgres's configuration: %w", err)
	}
	return nil
}

// logged is one execution the statement log holds: when it ended, its query id, its text and
// the literals of its parameters, in order.
type logged struct {
	at      time.Time
	queryID int64
	text    string
	params  []string
}

// executed reads a duration line's statement: an extended-protocol execute or a simple query.
var executed = regexp.MustCompile(`(?s)^duration: [0-9.]+ ms  (?:execute [^:]*|statement): (.*)$`)

// fileSize is how long path is now, where a later read of what was appended starts.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// readStatementLog reads every execution the jsonlog file at path holds after offset.
func readStatementLog(path string, offset int64) ([]logged, error) {
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
	var all []logged
	for scanner.Scan() {
		var line struct {
			Timestamp string `json:"timestamp"`
			Message   string `json:"message"`
			Detail    string `json:"detail"`
			QueryID   int64  `json:"query_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil || line.QueryID == 0 {
			continue
		}
		match := executed.FindStringSubmatch(line.Message)
		if match == nil {
			continue
		}
		at, err := time.Parse("2006-01-02 15:04:05.000 MST", line.Timestamp)
		if err != nil {
			continue
		}
		params, err := parseParameters(line.Detail)
		if err != nil {
			return nil, fmt.Errorf("read the parameters of query %d at %s: %w", line.QueryID, line.Timestamp, err)
		}
		all = append(all, logged{at: at.UTC(), queryID: line.QueryID, text: match[1], params: params})
	}
	return all, scanner.Err()
}

// parseParameters reads a statement log's detail, `Parameters: $1 = '…', $2 = NULL`, into the
// parameters' literals in order, each as SQL writes it: quoted, or NULL.
func parseParameters(detail string) ([]string, error) {
	rest, found := strings.CutPrefix(detail, "Parameters: ")
	if !found {
		return nil, nil
	}
	var params []string
	for index := 1; rest != ""; index++ {
		head := "$" + strconv.Itoa(index) + " = "
		if !strings.HasPrefix(rest, head) {
			return nil, fmt.Errorf("parameter %d does not start %q", index, head)
		}
		rest = rest[len(head):]
		var literal string
		switch {
		case strings.HasPrefix(rest, "NULL"):
			literal, rest = "NULL", rest[len("NULL"):]
		case strings.HasPrefix(rest, "'"):
			end := 1
			for {
				next := strings.IndexByte(rest[end:], '\'')
				if next < 0 {
					return nil, fmt.Errorf("parameter %d has no closing quote", index)
				}
				end += next + 1
				if end < len(rest) && rest[end] == '\'' {
					end++
					continue
				}
				break
			}
			literal, rest = rest[:end], rest[end:]
		default:
			return nil, fmt.Errorf("parameter %d is neither quoted nor NULL", index)
		}
		params = append(params, literal)
		rest = strings.TrimPrefix(rest, ", ")
	}
	return params, nil
}

// quote is s as an SQL literal.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// explainer runs `explain (analyze, buffers)` on a connection of its own, in the simple protocol,
// so its prepared statements are its own and nothing else on the connection caches one.
type explainer struct {
	conn *pgx.Conn
}

func newExplainer(ctx context.Context, url string) (*explainer, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect the explainer: %w", err)
	}
	return &explainer{conn: conn}, nil
}

// utility is a statement explain cannot run.
var utility = regexp.MustCompile(`(?i)^\s*(begin|commit|rollback|set|show|reset|savepoint|release|deallocate|discard|listen|unlisten|notify|prepare|execute|declare|fetch|close|lock|start|end)\b`)

// explain is the plan of text bound with params, as `explain (analyze, buffers)` answers it, and
// its execution time. It runs in a transaction it rolls back, so a write the statement makes is
// undone; text with parameters is prepared first and executed with their literals, which plans it
// as the server's own bound execution was planned.
func (e *explainer) explain(ctx context.Context, text string, params []string) (string, float64, error) {
	return e.explainAfter(ctx, nil, text, params)
}

// explainAfter is explain with first, where it is not nil, run before the statement in the same
// transaction: a change the rollback undoes, such as a dropped index.
func (e *explainer) explainAfter(ctx context.Context, first func(ctx context.Context, tx pgx.Tx) error, text string, params []string) (string, float64, error) {
	if utility.MatchString(text) {
		return "", 0, errUtility
	}
	tx, err := e.conn.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = tx.Rollback(ctx)
		_, _ = e.conn.Exec(ctx, `deallocate all`)
	}()
	if first != nil {
		if err := first(ctx, tx); err != nil {
			return "", 0, err
		}
	}
	query := "explain (analyze, buffers) " + text
	if len(params) > 0 {
		if _, err := tx.Exec(ctx, "prepare evalbench_explained as "+text); err != nil {
			return "", 0, fmt.Errorf("prepare: %w", err)
		}
		query = "explain (analyze, buffers) execute evalbench_explained(" + strings.Join(params, ", ") + ")"
	}
	return e.planOf(ctx, tx, query)
}

// planOf runs an explain and reads its plan and execution time.
func (e *explainer) planOf(ctx context.Context, tx pgx.Tx, query string) (string, float64, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return "", 0, err
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", 0, err
	}
	plan := strings.Join(lines, "\n")
	return plan, executionTime(plan), nil
}

// errUtility is explain's answer for a statement it cannot explain.
var errUtility = fmt.Errorf("a utility statement, which explain does not run")

// executionTimeLine matches a plan's `Execution Time` line, with its milliseconds.
var executionTimeLine = regexp.MustCompile(`Execution Time: ([0-9.]+) ms`)

// executionTime is a plan's `Execution Time`, in milliseconds.
func executionTime(plan string) float64 {
	match := executionTimeLine.FindStringSubmatch(plan)
	if match == nil {
		return -1
	}
	ms, _ := strconv.ParseFloat(match[1], 64)
	return ms
}

// scannedRelation matches each scan of a relation in a plan: the scan, the index it uses if any,
// and the relation's name.
var scannedRelation = regexp.MustCompile(`((?:Parallel )?(?:Seq Scan|Index Scan|Index Only Scan|Bitmap Heap Scan|Tid Scan|Tid Range Scan|Sample Scan))(?: Backward)?(?: using (\S+))? on (\S+)`)

// namedJobCondition matches a plan whose index condition names the jobs it reads (`name =
// ANY`): the run-jobs read that names the judged jobs in SQL.
var namedJobCondition = regexp.MustCompile(`Index Cond: .*\bname = ANY`)

// runJobsScanNode matches a text plan's line that is a scan of delivery_run_jobs, with the index
// it reads through, if any.
var runJobsScanNode = regexp.MustCompile(`^(?:->  )?((?:Parallel )?(?:Seq Scan|Index Scan|Index Only Scan|Bitmap Heap Scan|Tid Scan|Tid Range Scan|Sample Scan))(?: Backward)?(?: using (\S+))? on delivery_run_jobs\b`)

// bitmapIndexScanNode matches a text plan's line that is a bitmap index scan, with its index.
var bitmapIndexScanNode = regexp.MustCompile(`^->  Bitmap Index Scan on (\S+)`)

// jobNameCondition matches an index condition that names the jobs it reads: `name = ANY (…)` or
// one `name = '…'`.
var jobNameCondition = regexp.MustCompile(`\bname = (?:ANY \(|')`)

// failedJobsCondition matches an index condition that keeps a run's failed jobs alone.
var failedJobsCondition = regexp.MustCompile(`\bconclusion = 'failure'`)

// failedJobsPredicate is the predicate, as pg_get_expr writes it, of an index of
// delivery_run_jobs that holds the failed jobs alone (migration 20261114154233's
// delivery_run_jobs_failed). Postgres plans no condition its index's predicate implies, so a scan
// through such an index states no failure in its index condition, and the index says it.
const failedJobsPredicate = `(conclusion = 'failure'::text)`

// failureIndexes is the indexes of delivery_run_jobs whose predicate is failedJobsPredicate, from
// the catalog.
func failureIndexes(ctx context.Context, pool *store.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		select c.relname from pg_index i join pg_class c on c.oid = i.indexrelid
		where i.indrelid = 'delivery_run_jobs'::regclass and pg_get_expr(i.indpred, i.indrelid) = $1
	`, failedJobsPredicate)
	if err != nil {
		return nil, fmt.Errorf("read the indexes of delivery_run_jobs' failed jobs: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the indexes of delivery_run_jobs' failed jobs: %w", err)
	}
	indexes := make(map[string]bool, len(names))
	for _, name := range names {
		indexes[name] = true
	}
	return indexes, nil
}

// jobScan is one scan of delivery_run_jobs in a plan, and how each index it reads through keys it:
// "name" where the index condition names the jobs, "failure" where the condition or the index's
// own predicate keeps the failed jobs alone, "none" otherwise. A scan through no index is "none".
type jobScan struct {
	node string
	keys []string
}

// runJobsScans is every scan of delivery_run_jobs in a text plan: an index scan judged by its own
// index condition and its index, a bitmap heap scan by each bitmap index scan under it. failed is
// failureIndexes.
func runJobsScans(plan string, failed map[string]bool) []jobScan {
	lines := strings.Split(plan, "\n")
	var scans []jobScan
	for i, line := range lines {
		match := runJobsScanNode.FindStringSubmatch(strings.TrimLeft(line, " "))
		if match == nil {
			continue
		}
		scan := jobScan{node: strings.TrimPrefix(match[0], "->  ")}
		switch {
		case strings.HasPrefix(match[1], "Bitmap Heap Scan"):
			for j := i + 1; j < nodeEnd(lines, i); j++ {
				if index := bitmapIndexScanNode.FindStringSubmatch(strings.TrimLeft(lines[j], " ")); index != nil {
					scan.keys = append(scan.keys, indexKey(ownIndexCond(lines, j), index[1], failed))
				}
			}
		case match[2] != "":
			scan.keys = []string{indexKey(ownIndexCond(lines, i), match[2], failed)}
		}
		if len(scan.keys) == 0 {
			scan.keys = []string{"none"}
		}
		scans = append(scans, scan)
	}
	return scans
}

// everyScanKeyed reports whether every index each of scans reads through keys it by the job's
// name or by its failure.
func everyScanKeyed(scans []jobScan) bool {
	for _, scan := range scans {
		if slices.Contains(scan.keys, "none") {
			return false
		}
	}
	return true
}

// indexKey is how one read of delivery_run_jobs through index under cond is keyed (jobScan).
func indexKey(cond, index string, failed map[string]bool) string {
	switch {
	case jobNameCondition.MatchString(cond):
		return "name"
	case failedJobsCondition.MatchString(cond) || failed[index]:
		return "failure"
	}
	return "none"
}

// nodeColumn is the column a text plan's node line's own text starts at, past its arrow: the
// node's properties and children are indented two columns past it.
func nodeColumn(line string) int {
	text := strings.TrimLeft(line, " ")
	column := len(line) - len(text)
	if strings.HasPrefix(text, "->  ") {
		column += len("->  ")
	}
	return column
}

// nodeEnd is the index of the first line past the node at line i's properties and children.
func nodeEnd(lines []string, i int) int {
	inner := nodeColumn(lines[i]) + 2
	end := i + 1
	for ; end < len(lines); end++ {
		text := strings.TrimLeft(lines[end], " ")
		if text != "" && len(lines[end])-len(text) < inner {
			break
		}
	}
	return end
}

// ownIndexCond is the index condition of the node at line i, a property line at the node's own
// indent rather than a child's, or empty where it has none.
func ownIndexCond(lines []string, i int) string {
	inner := nodeColumn(lines[i]) + 2
	for j := i + 1; j < nodeEnd(lines, i); j++ {
		text := strings.TrimLeft(lines[j], " ")
		if len(lines[j])-len(text) == inner && strings.HasPrefix(text, "Index Cond: ") {
			return text
		}
	}
	return ""
}

// relations is each relation a plan scans, with the scans of it.
func relations(plan string) map[string][]string {
	found := map[string][]string{}
	for _, match := range scannedRelation.FindAllStringSubmatch(plan, -1) {
		found[match[3]] = append(found[match[3]], strings.TrimSpace(match[1]+" "+match[2]))
	}
	return found
}

// seqScanned reports whether a plan reads table by a sequential scan.
func seqScanned(plan, table string) bool {
	for _, scan := range relations(plan)[table] {
		if strings.Contains(scan, "Seq Scan") {
			return true
		}
	}
	return false
}

// usesIndex reports whether a plan reads through index.
func usesIndex(plan, index string) bool {
	return strings.Contains(plan, "using "+index+" ") || strings.Contains(plan, "Bitmap Index Scan on "+index)
}

// measuredNumbers matches a plan node's measured numbers: its cost, its actual time and rows, or
// its "never executed".
var measuredNumbers = regexp.MustCompile(`\s*\((?:cost|actual|never)[^)]*\)`)

// planShape is a plan's nodes and conditions with every measured number taken out, which two runs
// of one plan share: what "the same plan" compares.
func planShape(plan string) string {
	var shape []string
	for _, line := range strings.Split(plan, "\n") {
		trimmed := strings.TrimSpace(line)
		keep := strings.HasPrefix(trimmed, "->") || len(shape) == 0
		for _, condition := range []string{"Index Cond:", "Recheck Cond:", "Filter:", "Sort Key:", "Join Filter:", "Hash Cond:"} {
			keep = keep || strings.HasPrefix(trimmed, condition)
		}
		if keep && !strings.HasPrefix(trimmed, "Planning") && !strings.HasPrefix(trimmed, "Execution") {
			shape = append(shape, measuredNumbers.ReplaceAllString(line, ""))
		}
	}
	return strings.Join(shape, "\n")
}

// oneLine is a statement's text on one line, for a RESULT line.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
