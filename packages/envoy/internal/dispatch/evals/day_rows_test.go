package evals

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The checks of the day rows' write (writeDayRows): it sends its rows in statements whose
// parameters take at most statementBytes as pgx encodes them, in the byte order of the rows' keys
// across its statements, and the statements together store and log what one would.

// dayRowStatementOIDs is the type of each parameter of writeDayRows' statement, as the server
// describes it (TestAWriteOfDayRowsPastTheBoundIsCutIntoStatementsInKeyOrder holds it to the
// description): what pgx encodes each argument as.
var dayRowStatementOIDs = []uint32{
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.DateArrayOID, pgtype.BoolArrayOID,
	pgtype.Int4ArrayOID, pgtype.Float8ArrayOID, pgtype.Float8ArrayOID, pgtype.Float8ArrayOID, pgtype.Float8ArrayOID, pgtype.TextArrayOID,
	pgtype.Float8ArrayOID, pgtype.TimestamptzArrayOID, pgtype.TextArrayOID, pgtype.Int4ArrayOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TimestamptzArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
}

// dayKeyLine is a day row's key as one string whose bytes order the keys as their fields' bytes
// do, field by field (project, eval, deploy repository, series, repository and day): the fields
// joined by a NUL, which no field holds, so a field that is a prefix of another orders first.
func dayKeyLine(project, evalID, deployRepo, series, repo string, day time.Time) string {
	return strings.Join([]string{project, evalID, deployRepo, series, repo, day.UTC().Format(time.DateOnly)}, "\x00")
}

// dayKeyColumnsIn is the key columns of the rows one of writeDayRows' statements takes, args its
// arguments, each in the order of its array: project, eval, deploy repository, series, repository
// and day.
func dayKeyColumnsIn(args []any) (projects, evalIDs, deployRepos, series, repos []string, days []time.Time) {
	return args[0].([]string), args[1].([]string), args[20].([]string), args[2].([]string), args[3].([]string), args[4].([]time.Time)
}

// dayKeysIn is the keys of the rows one of writeDayRows' statements takes, args its arguments, in
// the order of its arrays.
func dayKeysIn(args []any) []string {
	projects, evalIDs, deployRepos, series, repos, days := dayKeyColumnsIn(args)
	keys := make([]string, len(projects))
	for index := range keys {
		keys[index] = dayKeyLine(projects[index], evalIDs[index], deployRepos[index], series[index], repos[index], days[index])
	}
	return keys
}

// dayRowKeyOrder answers the positions, from 0, of the day rows' keys its arrays hold (project,
// eval, deploy repository, series, repository and day, each array a column of the keys), in the
// byte order of the keys, every text `collate "C"`: the order of eval_days' primary key that every
// statement that inserts or locks day rows takes them in (compareDayKeys), and so the order
// writeDayRows' statement must be sent its rows in, since it takes them as sent (`with
// ordinality`).
const dayRowKeyOrder = `
	select (k.sent - 1)::int from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::date[]) with ordinality
		as k(project_key, eval_id, deploy_repo, series, repo, day, sent)
	order by k.project_key collate "C", k.eval_id collate "C", k.deploy_repo collate "C", k.series collate "C",
		k.repo collate "C", k.day`

// dayRowKeyArraysIn is the keys of the rows writeDayRows' statements take, statements the
// arguments of each, in the order they take them across the statements, as dayRowKeyOrder's
// arrays.
func dayRowKeyArraysIn(statements [][]any) []any {
	var projects, evalIDs, deployRepos, series, repos []string
	var days []time.Time
	for _, args := range statements {
		ofProjects, ofEvalIDs, ofDeployRepos, ofSeries, ofRepos, ofDays := dayKeyColumnsIn(args)
		projects, evalIDs, deployRepos = append(projects, ofProjects...), append(evalIDs, ofEvalIDs...), append(deployRepos, ofDeployRepos...)
		series, repos, days = append(series, ofSeries...), append(repos, ofRepos...), append(days, ofDays...)
	}
	return []any{projects, evalIDs, deployRepos, series, repos, days}
}

// sortedDayKeys is the keys of rows, sorted by their bytes.
func sortedDayKeys(rows []DayRow) []string {
	keys := make([]string, len(rows))
	for index, row := range rows {
		keys[index] = dayKeyLine(row.ProjectKey, row.EvalID, row.DeployRepo, row.Series, row.Repo, row.Day)
	}
	slices.Sort(keys)
	return keys
}

// dayRowsIn is how many rows a statement of day rows takes, args its arguments.
func dayRowsIn(args []any) int {
	return len(args[0].([]string))
}

// shuffledDays is rows in an order of a fixed seed's, the given slice left as it is.
func shuffledDays(rows []DayRow) []DayRow {
	shuffled := slices.Clone(rows)
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	return shuffled
}

// TestAStatementOfDayRowsHoldsAtMostItsBoundAsPgxEncodesIt: rows whose every field takes the most
// bytes its encoding can (every number, date and time set, to the largest numbers and the farthest
// dates, whose binary encodings are as long as any, and long strings, sample lists and counts,
// which the estimate counts byte for byte), and short rows, of which a statement holds more than a
// thousand, given out of order, are cut into statements (dayRowStatements) whose parameters, as
// pgx encodes them, take no more than the estimate (paramEstimate, dayRowBytes') and no more than
// statementBytes, holding every row once and in the byte order of their keys: each statement but
// the last as many rows as that estimate lets under the bound, and no fewer. The rows given keep
// their order. A row larger than the bound alone is sent in a statement of its own, between the
// statements of the rows around it.
func TestAStatementOfDayRowsHoldsAtMostItsBoundAsPgxEncodesIt(t *testing.T) {
	t.Parallel()
	long := func(index, size int) string { return fmt.Sprintf("%06d", index) + strings.Repeat("é", size/2) }
	far := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	lastAt := far.Add(24*time.Hour - time.Microsecond)
	build := long(0, 300)
	// -math.MaxFloat64's shortest decimal, -1.7976931348623157e+308, is as long as any float's.
	widest := func(count int) []float64 {
		samples := make([]float64, count)
		for index := range samples {
			samples[index] = -math.MaxFloat64
		}
		return samples
	}
	counts := map[string]float64{}
	for index := range 16 {
		counts[long(index, 40)] = -math.MaxFloat64
	}
	most := func(index int) DayRow {
		return DayRow{ProjectKey: "EVAL", EvalID: EvalID("EVAL", long(index, 200)), DeployRepo: long(index, 200), Repo: long(index, 200), Listed: true,
			DayPart: measures.DayPart{
				Series: long(index, 200), Day: far, N: math.MinInt32, Sum: new(-math.MaxFloat64), Num: new(-math.MaxFloat64),
				Den: new(-math.MaxFloat64), Max: new(-math.MaxFloat64), Samples: widest(64), Counts: counts, LastValue: new(-math.MaxFloat64),
				LastAt: &lastAt, LastRecordedAt: &lastAt, LastReading: "ffffffff-ffff-ffff-ffff-ffffffffffff", ViaRepository: math.MinInt32,
			}}
	}
	short := func(index int) DayRow {
		return DayRow{ProjectKey: "EVAL", EvalID: EvalID("EVAL", fmt.Sprintf("%06d", index)),
			DayPart: measures.DayPart{Series: "s", Day: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}}
	}
	statementsOf := func(rows []DayRow) [][]any {
		statements, err := dayRowStatements(rows, build, true)
		if err != nil {
			t.Fatalf("encode %d day rows: %v", len(rows), err)
		}
		return slices.Collect(statements)
	}

	for _, shape := range []struct {
		name  string
		count int
		row   func(int) DayRow
	}{{"rows whose fields take the most bytes", 400, most}, {"short rows", 6000, short}} {
		rows := make([]DayRow, shape.count)
		for index := range rows {
			rows[index] = shape.row(index)
		}
		given := shuffledDays(rows)
		order := sortedDayKeys(given)
		// Every row of the shape estimates alike, so the rows the bound lets into one statement are
		// what is left of it past a statement's own parameters, over a row's.
		estimateOf := func(n int) int { return paramEstimate(t, statementsOf(given[:n])[0]) }
		row := estimateOf(2) - estimateOf(1)
		fits := min((statementBytes-(estimateOf(1)-row))/row, statementRows)
		var held []string
		var sizes []int
		largest := 0
		for _, args := range statementsOf(given) {
			encoded, estimated := encodedBytes(t, dayRowStatementOIDs, args), paramEstimate(t, args)
			if encoded > estimated {
				t.Errorf("%s: a statement of %d rows encodes to %d bytes of parameters, past its estimate of %d", shape.name, dayRowsIn(args), encoded, estimated)
			}
			if estimated > statementBytes {
				t.Errorf("%s: a statement of %d rows takes an estimated %d bytes of parameters, past %d", shape.name, dayRowsIn(args), estimated, statementBytes)
			}
			held, sizes, largest = append(held, dayKeysIn(args)...), append(sizes, dayRowsIn(args)), max(largest, encoded)
		}
		if !slices.Equal(held, order) {
			t.Errorf("%s: the statements take %d rows, other rows or in another order than the %d given in the byte order of their keys", shape.name, len(held), len(order))
		}
		if !reflect.DeepEqual(given, shuffledDays(rows)) {
			t.Errorf("%s: the cut reordered the rows it was given", shape.name)
		}
		for index, size := range sizes {
			if size != fits && (index < len(sizes)-1 || size > fits) {
				t.Errorf("%s: the statements hold %v rows, want %d in each but the last, as many as the estimate lets under the bound", shape.name, sizes, fits)
				break
			}
		}
		if len(sizes) < 2 {
			t.Errorf("%s: %d rows went in %d statement, and the check needs them cut", shape.name, shape.count, len(sizes))
		}
		t.Logf("%s: %d rows in %d statements of %d rows, the largest %d bytes of parameters as pgx encodes them, of %d",
			shape.name, shape.count, len(sizes), fits, largest, statementBytes)
	}

	huge := most(1)
	huge.Samples = widest(20000)
	var sizes []int
	var alone *string
	for _, args := range statementsOf([]DayRow{most(3), huge, most(0), most(2)}) {
		if sizes = append(sizes, dayRowsIn(args)); len(sizes) == 2 {
			alone = args[11].([]*string)[0]
		}
	}
	if want := floatArray(huge.Samples); !slices.Equal(sizes, []int{1, 1, 2}) || alone == nil || *alone != *want {
		t.Errorf("a row of %d samples among three others went in statements of %v rows, the second's samples not the row's alone; want 1, 1 and 2, the row alone in the second",
			len(huge.Samples), sizes)
	}
}

// sentExecs is a transaction that records the arguments of each statement executed on it and runs
// none, answering each with no rows: what a write sends, as far as it goes with no stored row.
type sentExecs struct {
	pgx.Tx
	sent [][]any
}

func (s *sentExecs) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	s.sent = append(s.sent, args)
	return pgconn.CommandTag{}, nil
}

// TestTwoDayRowsOfOneKeyAreRefusedByAnUpsertBeforeAnyStatement: two day rows of one key (project,
// eval, deploy repository, series and repository, on one UTC day at two instants), given apart
// among rows of other keys, are refused by upsertDays before it sends any statement, the refusal
// naming the key, as the one statement that took every row refused them (ON CONFLICT DO UPDATE
// cannot affect a row a second time); cut into two statements, the second would rewrite the
// first.
func TestTwoDayRowsOfOneKeyAreRefusedByAnUpsertBeforeAnyStatement(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	rowOf := func(series, repo string, at time.Time, n int) DayRow {
		return DayRow{ProjectKey: "EVAL", EvalID: EvalID("EVAL", "lead-time"), DeployRepo: "Acme/deploy", Repo: repo, Listed: true,
			DayPart: measures.DayPart{Series: series, Day: at, N: n, Sum: new(float64(n))}}
	}
	rows := []DayRow{
		rowOf("Queue", "acme/web", day.Add(9*time.Hour), 1), rowOf("latency", "", day, 2),
		rowOf("latency", "acme/web", day, 3), rowOf("Queue", "acme/web", day.Add(17*time.Hour), 4),
	}

	upsert := &sentExecs{}
	err := upsertDays(t.Context(), upsert, rows, testBuild)
	named := err != nil && strings.Contains(err.Error(), "EVAL/lead-time") && strings.Contains(err.Error(), `"Acme/deploy"`) &&
		strings.Contains(err.Error(), `"Queue"`) && strings.Contains(err.Error(), `"acme/web"`) && strings.Contains(err.Error(), "2026-11-03")
	if !named || len(upsert.sent) != 0 {
		t.Errorf("an upsert of two rows of one key sent %d statements and answered %v, want no statement and a refusal naming the key", len(upsert.sent), err)
	}
	t.Logf("the upsert answered: %v", err)
}

// dayInserts is a tracer (storetest.Traced) that records, in the order they start, the statements
// on the pool it traces that insert day rows (writeDayRows'): each one's SQL and arguments.
type dayInserts struct {
	mu   sync.Mutex
	sent []pgx.TraceQueryStartData
}

func (d *dayInserts) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "insert into eval_days") {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.sent = append(d.sent, data)
	}
	return ctx
}

func (d *dayInserts) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take answers the statements recorded since the last take, and forgets them.
func (d *dayInserts) take() []pgx.TraceQueryStartData {
	d.mu.Lock()
	defer d.mu.Unlock()
	taken := d.sent
	d.sent = nil
	return taken
}

// dayChange is one eval_days change a write logged: its project, its eval and its span's first and
// last day.
type dayChange struct {
	project, evalID string
	first, last     time.Time
}

// String is c as a failure names it: its project, its eval and its span's days.
func (c dayChange) String() string {
	return fmt.Sprintf("%s %s %s..%s", c.project, c.evalID, c.first.UTC().Format(time.DateOnly), c.last.UTC().Format(time.DateOnly))
}

// TestAWriteOfDayRowsPastTheBoundIsCutIntoStatementsInKeyOrder: a write of 1,440 day rows of three
// evals, two deploy repositories, two series and three repositories over 40 days, most with a
// sample list of 40 values, given out of order (upsertDays, its statements recorded on a traced
// pool), sends at least three statements, none whose parameters pass statementBytes as
// dayRowStatements estimates them, nor that estimate as pgx encodes them as the types the server
// describes; they take the rows in the byte order of their keys across the statements, each once,
// the order Postgres's `collate "C"` gives the keys they send (dayRowKeyOrder), which the
// statement takes them in as sent; store every row as it was given, and log one eval_days change
// per eval spanning the days written, as one statement does (migration 20261117100014). A second
// write, which rewrites the later 20 days with other values and adds 20 days after them, so each
// statement holds rows the upsert updates and rows it inserts, does the same and leaves the
// earlier 20 days as they were.
func TestAWriteOfDayRowsPastTheBoundIsCutIntoStatementsInKeyOrder(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	inserts := &dayInserts{}
	traced := storetest.Traced(t, r.database.Pool, inserts, nil)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// Of each pair here, a collation that folds case orders the two the other way from their bytes.
	evalIDs := []string{EvalID(r.project, "lead-time"), EvalID(r.project, "Rework"), EvalID(r.project, "deploys")}
	deployRepos, seriesNames, repos := []string{"", "Acme/deploy"}, []string{"latency", "Queue"}, []string{"", "acme/web", "Acme/api"}
	// daysOf is a row of each eval, deploy repository, series and repository on each of n days
	// from start's day from on, its values moved by shift.
	daysOf := func(from, n int, shift float64) []DayRow {
		var rows []DayRow
		for _, evalID := range evalIDs {
			for _, deployRepo := range deployRepos {
				for _, series := range seriesNames {
					for _, repo := range repos {
						for offset := from; offset < from+n; offset++ {
							day := start.AddDate(0, 0, offset)
							value := shift + float64(len(rows))/7
							lastAt, recordedAt := day.Add(17*time.Hour), day.Add(18*time.Hour)
							row := DayRow{ProjectKey: r.project, EvalID: evalID, DeployRepo: deployRepo, Repo: repo, Listed: true, DayPart: measures.DayPart{
								Series: series, Day: day, N: 40, Sum: new(value * 40), Num: new(value), Den: new(3.0), Max: new(value + 13),
								Counts: map[string]float64{"success": value, "failure": 1}, LastValue: new(value), LastAt: &lastAt, LastRecordedAt: &recordedAt,
								LastReading: fmt.Sprintf("0190c9a4-5c1e-7a00-8000-%012d", len(rows)), ViaRepository: 1,
							}}
							row.Samples = make([]float64, 40)
							for k := range row.Samples {
								row.Samples[k] = value + float64(k)/3
							}
							// Some rows hold nulls, whose elements take no bytes past their length.
							if len(rows)%7 == 0 {
								row.Num, row.Den, row.Samples, row.LastAt, row.LastRecordedAt, row.LastReading = nil, nil, nil, nil, nil, ""
							}
							rows = append(rows, row)
						}
					}
				}
			}
		}
		return rows
	}
	var oids []uint32
	write := func(what string, rows []DayRow, first, last time.Time) {
		t.Helper()
		pendingHead, changesHead := r.count(t, `select coalesce(max(id), 0) from delivery_changes_pending`), r.count(t, `select coalesce(max(seq), 0) from delivery_changes`)
		tx, err := traced.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", what, err)
		}
		if err := upsertDays(ctx, tx, shuffledDays(rows), testBuild); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: write %d day rows: %v", what, len(rows), err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("%s: commit: %v", what, err)
		}
		sent := inserts.take()
		if len(sent) == 0 {
			t.Fatalf("%s: %d day rows were written by no statement the tracer saw", what, len(rows))
		}
		if oids == nil {
			conn, err := r.database.Pool.Acquire(ctx)
			if err != nil {
				t.Fatalf("acquire a connection to describe the statement: %v", err)
			}
			described, err := conn.Conn().PgConn().Prepare(ctx, "", sent[0].SQL, nil)
			conn.Release()
			if err != nil {
				t.Fatalf("describe the statement of day rows: %v", err)
			}
			if oids = described.ParamOIDs; !slices.Equal(oids, dayRowStatementOIDs) {
				t.Errorf("the server describes the parameters of the statement of day rows as %v, where the test encodes them as %v", oids, dayRowStatementOIDs)
			}
		}
		var held []string
		var sizes []int
		var statements [][]any
		largest := 0
		for index, statement := range sent {
			encoded, estimated := encodedBytes(t, oids, statement.Args), paramEstimate(t, statement.Args)
			if estimated > statementBytes || encoded > estimated {
				t.Errorf("%s: statement %d of %d rows takes %d bytes of parameters as pgx encodes them and an estimated %d, where the bound is %d",
					what, index, dayRowsIn(statement.Args), encoded, estimated, statementBytes)
			}
			held, sizes, largest = append(held, dayKeysIn(statement.Args)...), append(sizes, dayRowsIn(statement.Args)), max(largest, encoded)
			statements = append(statements, statement.Args)
		}
		if len(sent) < 3 {
			t.Errorf("%s: %d day rows went in %d statements of %v rows, and the check needs at least three", what, len(rows), len(sent), sizes)
		}
		if !slices.Equal(held, sortedDayKeys(rows)) {
			t.Errorf("%s: the statements take %d rows, other rows or in another order than the %d given in the byte order of their keys", what, len(held), len(rows))
		}
		if at := firstOutOfOrder(keyOrderOf(t, r.database.Pool, len(held), dayRowKeyOrder, dayRowKeyArraysIn(statements)...)); at >= 0 {
			t.Errorf("%s: the statements take the row %q at %d, out of the byte order Postgres's collate \"C\" gives their keys", what, held[at], at)
		}

		changed, err := r.database.Pool.Query(ctx, `
			select repo, ref, t_old, t_new from (
				select repo, ref, t_old, t_new from delivery_changes_pending where kind = 'eval_days' and id > $1
				union all
				select repo, ref, t_old, t_new from delivery_changes where kind = 'eval_days' and seq > $2
			) as logged
			order by ref collate "C"`, pendingHead, changesHead)
		if err != nil {
			t.Fatalf("%s: read the changes logged: %v", what, err)
		}
		changes, err := pgx.CollectRows(changed, func(row pgx.CollectableRow) (dayChange, error) {
			var change dayChange
			err := row.Scan(&change.project, &change.evalID, &change.first, &change.last)
			return change, err
		})
		if err != nil {
			t.Fatalf("%s: read the changes logged: %v", what, err)
		}
		var want []dayChange
		for _, evalID := range slices.Sorted(slices.Values(evalIDs)) {
			want = append(want, dayChange{project: r.project, evalID: evalID, first: first, last: last})
		}
		if !slices.EqualFunc(changes, want, func(a, b dayChange) bool {
			return a.project == b.project && a.evalID == b.evalID && a.first.Equal(b.first) && a.last.Equal(b.last)
		}) {
			t.Errorf("%s: the write logged the eval_days changes %v, want one of each eval spanning the days written, %v", what, changes, want)
		}
		t.Logf("%s: %d rows in %d statements of %v rows, the largest %d bytes of parameters as pgx encodes them, of %d; %d changes logged",
			what, len(rows), len(sent), sizes, largest, statementBytes, len(changes))
	}
	// stored checks that the stored day rows are exactly want, each as it was given, built by
	// testBuild.
	stored := func(what string, want []DayRow) {
		t.Helper()
		rows := r.stored(t, r.project)
		byKey := map[string]DayRow{}
		for _, row := range rows {
			row.UpdatedAt = time.Time{}
			byKey[dayKeyLine(row.ProjectKey, row.EvalID, row.DeployRepo, row.Series, row.Repo, row.Day)] = row
		}
		wrong := 0
		for _, row := range want {
			row.BuiltBy = testBuild
			if found, ok := byKey[dayKeyLine(row.ProjectKey, row.EvalID, row.DeployRepo, row.Series, row.Repo, row.Day)]; !ok || !reflect.DeepEqual(found, row) {
				if wrong++; wrong == 1 {
					t.Errorf("%s: the stored row of %s %q %s %q on %s reads back as\n%+v\nwant\n%+v", what, row.EvalID, row.DeployRepo, row.Series, row.Repo,
						row.Day.Format(time.DateOnly), found, row)
				}
			}
		}
		if wrong > 0 || len(rows) != len(want) {
			t.Errorf("%s: %d rows stored, %d of the %d written missing or not as written", what, len(rows), wrong, len(want))
		}
	}

	written := daysOf(0, 40, 0)
	write("a write of 40 new days", written, start, start.AddDate(0, 0, 39))
	stored("a write of 40 new days", written)

	rewritten := daysOf(20, 40, 1000)
	write("a write rewriting the later 20 days and adding 20", rewritten, start.AddDate(0, 0, 20), start.AddDate(0, 0, 59))
	var kept []DayRow
	for _, row := range written {
		if row.Day.Before(start.AddDate(0, 0, 20)) {
			kept = append(kept, row)
		}
	}
	stored("a write rewriting the later 20 days and adding 20", slices.Concat(kept, rewritten))
}

// TestAWriteOfSeveralProjectsDaysDeletesWhatEachNoLongerHoldsAcrossItsRuns: one write of a day of
// two projects (writeProjectDays) whose rows and items pass statementBytes cuts each into runs of
// their keys (staleRuns, memberRuns), and sends each run's statements the days written of the
// projects its range holds keys of (writtenCuts). Written again with the first and the last row
// and item of each project's day gone, laid out so a run begins at the second project's first row
// and item that stand, the write deletes every one of them and keeps the rest: the second
// project's first row and item gone lie in the range of the run before, which ends inside that
// project, and its last ones in the range of the run after.
func TestAWriteOfSeveralProjectsDaysDeletesWhatEachNoLongerHoldsAcrossItsRuns(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pad := strings.Repeat("x", 480)
	// dayOf is project's day of rows rows and items items, each a deploy-frequency row of a
	// repository of its own and a pull request, in the byte order of their keys, less the first and
	// the last of each where trim is set.
	dayOf := func(project string, rows, items int, trim bool) ProjectDay {
		from, less := 0, 0
		if trim {
			from, less = 1, 1
		}
		written := ProjectDay{Project: project, Day: day}
		for i := from; i < rows-less; i++ {
			written.Rows = append(written.Rows, DayRow{ProjectKey: project, EvalID: EvalID(project, SlugDeployFrequency), DeployRepo: "acme/deploy",
				Repo: fmt.Sprintf("acme/r%05d-%s", i, pad), Listed: true, DayPart: measures.DayPart{Series: measures.SeriesDeploys, Day: day, N: 1, Sum: new(1.0)}})
		}
		for i := from; i < items-less; i++ {
			written.Members = append(written.Members, Member{Family: KindDelivery, DeployRepo: "acme/deploy",
				RowKey: fmt.Sprintf("pull_request:acme/web#%05d-%s", i, pad), Repo: "acme/web"})
		}
		return written
	}
	const second = 20
	// The first project's rows and items are as many as make a run of the write again begin at the
	// second project's first row and first item that stand: the cuts deleteStaleDays and
	// writeDayMembersIn make of that write.
	rows, items := 0, 0
	for n := 100; n < 2000 && (rows == 0 || items == 0); n++ {
		again := []ProjectDay{dayOf("P0", n, n, true), dayOf("P1", second, second, true)}
		if rows == 0 {
			sent := &sentExecs{}
			if err := deleteStaleDays(ctx, sent, again, deliveryFamily.slugs); err != nil {
				t.Fatal(err)
			}
			for _, args := range sent.sent {
				if slices.Equal(args[11].([]string), dayRowKey(&again[1].Rows[0])) {
					rows = n
					break
				}
			}
		}
		if items == 0 {
			sent := &sentExecs{}
			if err := writeDayMembersIn(ctx, sent, again, deliveryFamily.families); err != nil {
				t.Fatal(err)
			}
			want := []string{"P1", KindDelivery, "acme/deploy", again[1].Members[0].RowKey}
			for _, args := range sent.sent {
				if len(args) == 10 && slices.Equal(args[9].([]string), want) {
					items = n
					break
				}
			}
		}
	}
	if rows == 0 || items == 0 {
		t.Fatalf("no count of the first project's rows and items begins a run at the second project's first ones (rows %d, items %d)", rows, items)
	}
	write := func(what string, days []ProjectDay) {
		t.Helper()
		if err := r.registry.writeProjectDays(ctx, deliveryFamily, days, testBuild, false); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	write("the first write", []ProjectDay{dayOf("P0", rows, items, false), dayOf("P1", second, second, false)})
	again := []ProjectDay{dayOf("P0", rows, items, true), dayOf("P1", second, second, true)}
	write("the write again", again)
	var wantRows, wantItems []string
	for _, written := range again {
		for _, row := range written.Rows {
			wantRows = append(wantRows, row.ProjectKey+" "+row.Repo)
		}
		for _, member := range written.Members {
			wantItems = append(wantItems, written.Project+" "+member.RowKey)
		}
	}
	for _, check := range []struct {
		what, query string
		want        []string
	}{
		{"rows", `select project_key || ' ' || repo from eval_days where project_key in ('P0', 'P1') order by (project_key || ' ' || repo) collate "C"`, wantRows},
		{"items", `select project_key || ' ' || row_key from eval_day_members where project_key in ('P0', 'P1') order by (project_key || ' ' || row_key) collate "C"`, wantItems},
	} {
		found, err := r.database.Pool.Query(ctx, check.query)
		if err != nil {
			t.Fatalf("read the stored %s: %v", check.what, err)
		}
		stored, err := pgx.CollectRows(found, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the stored %s: %v", check.what, err)
		}
		if !slices.Equal(stored, check.want) {
			var extra []string
			for _, line := range stored {
				if !slices.Contains(check.want, line) {
					extra = append(extra, line[:min(len(line), 40)])
				}
			}
			t.Errorf("the write again leaves %d stored %s where its days hold %d, those it no longer holds %v", len(stored), check.what, len(check.want), extra)
		}
	}
	t.Logf("%d rows and %d items of P0 and %d of each of P1, written again with the first and the last of each gone", rows, items, second)
}

func TestAChangedShippingMemberOwesTheOldRunUnderAnotherOwner(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const repo = "acme/web"
	prOwner := OwnerKey(repo, nil, nil)
	runOwner := OwnerKey(repo, []string{r.project}, nil)
	prDay, runDay := at(3, 0, 0), at(4, 0, 0)
	old := RunMember(repo, 101)
	pr := Member{Family: KindDelivery, DeployRepo: repo, RowKey: PullRequestMember(repo, 1), Repo: repo, ShippedBy: old}
	if _, err := pool.Exec(ctx, `
		insert into eval_day_owners (owner, repo) values ($1, $3), ($2, $3)`,
		prOwner, runOwner, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into eval_day_members (project_key, family, deploy_repo, row_key, day, repo, shipped_by)
		values ($1, 'delivery', $3, $4, $5, $3, $6), ($2, 'delivery', $3, $6, $7, $3, '')`,
		prOwner, runOwner, repo, pr.RowKey, prDay, old, runDay); err != nil {
		t.Fatal(err)
	}
	pr.ShippedBy = RunMember(repo, 102)
	days := []ProjectDay{{Project: prOwner, Day: prDay, Members: []Member{pr}}}
	left, err := r.registry.leftDays(ctx, days, deliveryDayFamilies)
	if err != nil {
		t.Fatal(err)
	}
	if want := []projectDay{{project: runOwner, day: runDay}}; !slices.Equal(left, want) {
		t.Errorf("days left = %v, want %v", left, want)
	}
	// Commit just the PR's write, as a pass that fails before it can write the run's day.
	if err := r.registry.writeProjectDays(ctx, deliveryFamily, days, testBuild, false); err != nil {
		t.Fatal(err)
	}
	if n := r.count(t, `select count(*) from eval_days_owed where project_key = $1 and family = 'delivery' and day = $2`, runOwner, runDay); n != 1 {
		t.Errorf("%s owes %d writes of the old run's day, want one", runOwner, n)
	}
}

func TestALargeNamedFigureReadBoundsItsDefinitionStatements(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ids := make([]string, 600)
	for i := range ids {
		slug := fmt.Sprintf("eval-%03d-", len(ids)-i) + strings.Repeat("x", 500)
		ids[i] = EvalID(r.project, slug)
		if i == 0 || i == len(ids)-1 {
			r.write(t, slug, alice, row(t, map[string]any{
				"series": "minutes", "row_kind": "run", "row_key": "one", "value": 1, "measured_at": at(3, 9, 0),
			}))
		}
	}
	trace := &sentStatements{}
	pool := storetest.TracedStore(t, r.database, trace).Pool
	entries, err := readEntries(t.Context(), pool, r.project, append(ids, ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Definition.EvalID)
		if len(entry.Series) != 1 || entry.Series[0].Series != "minutes" {
			t.Errorf("entry %.13q has %v, want the minutes series", entry.Definition.EvalID, entry.Series)
		}
	}
	if !slices.Equal(got, []string{ids[len(ids)-1], ids[0]}) {
		t.Error("the read did not return exactly the two stored entries in slug order")
	}
	largest, _ := largestStatement(r.measured(t, trace.take()))
	if largest > 256<<10 {
		t.Errorf("definition read sent %d parameter bytes, past 256 KiB", largest)
	}
}
