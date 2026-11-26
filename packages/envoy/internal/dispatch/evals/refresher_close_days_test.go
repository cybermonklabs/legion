package evals

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// factsMarks is the Legion days the issue_facts changes of issue logged after seq mark, each as a
// pass's report names a day (line): the day of each change's t_old and of its t_new, where set.
// The facts' trigger records its changes in the change log's outbox, so this moves them into the
// log first, as the refresher's pass does before it reads.
func (r *refresherRig) factsMarks(t *testing.T, seq int64, project, issue string) []string {
	t.Helper()
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 1000)
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	marked := []string{}
	for _, change := range changes {
		if change.Kind != delivery.ChangeKindIssueFacts || change.Repo != project || change.Ref != issue {
			continue
		}
		for _, at := range []*time.Time{change.TOld, change.TNew} {
			if at != nil {
				marked = append(marked, line(walkLegion, project, measures.Midnight(*at)))
			}
		}
	}
	slices.Sort(marked)
	return slices.Compact(marked)
}

// closeMarkProblems is what is wrong with the days one change of an issue's facts marked, against
// the issue's close days the change moved between, each a pass's report line: a day marked that is
// none of them, which counts no change of the issue, and a close day left unmarked. An open
// issue's change wants no day at all.
func closeMarkProblems(marked, closes []string) []string {
	var problems []string
	for _, day := range marked {
		if !slices.Contains(closes, day) {
			problems = append(problems, "marks "+day+", which is no close day of the issue")
		}
	}
	for _, day := range closes {
		if !slices.Contains(marked, day) {
			problems = append(problems, "leaves the close day "+day+" unmarked")
		}
	}
	return problems
}

// TestAFactsChangeMarksAndRewritesTheIssuesCloseDaysAlone: over the hand-written Legion history of
// EVAL, once every day is walked, each change of an issue's stored facts marks the Legion days
// that count the issue, its close day before the change and after it, and the pass after it
// rewrites a row of each of those days and no row of any other (expectDays; dispatch://INFRA-2916,
// R26 and check k29, whose reads of the stamps
// TestACloseMovedThirtyDaysReadsTheStampsOfItsCloseDaysAlone bounds):
//   - EVAL-3, closed on the history's third day, gains work on its first and second days: the
//     change marks its close day alone, and that day alone is rewritten;
//   - its close moves to the fifth day: the change marks the third and the fifth, and those two
//     alone are rewritten, neither of its two work days;
//   - EVAL-4, open, gains work on two days: the change marks no day and nothing is rewritten.
//
// The negative control is a change naming EVAL-3's work days in place of its close days: the pass
// must rewrite those two days, which count no change of the issue, and the same judgements, of the
// marks and of the days the pass wrote (dayProblems), must find it wrong.
func TestAFactsChangeMarksAndRewritesTheIssuesCloseDaysAlone(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	workFirst, workLast := legionHistoryDay(1).Add(20*time.Hour), history.d1.Add(6*time.Hour)
	closedLater := legionHistoryDay(5).Add(9 * time.Hour)
	legionDay := func(day time.Time) string { return line(walkLegion, "EVAL", measures.Midnight(day)) }

	worked := history.rows["EVAL-3"]
	worked.WorkFirstAt, worked.WorkLastAt, worked.WorkHours = &workFirst, &workLast, 5
	moved := worked
	moved.ClosedAt = &closedLater
	open := history.rows["EVAL-4"]
	open.WorkFirstAt, open.WorkLastAt, open.WorkHours = &workFirst, &workLast, 5
	steps := []struct {
		what   string
		row    string
		write  func()
		closes []string
	}{
		{"EVAL-3 gains work on two days and its close stands", "EVAL-3", func() {
			history.rows["EVAL-3"] = worked
			r.writeFacts(t, history, "EVAL-3")
		}, []string{legionDay(history.d2)}},
		{"EVAL-3's close moves two days on", "EVAL-3", func() {
			history.rows["EVAL-3"] = moved
			r.writeFacts(t, history, "EVAL-3")
		}, []string{legionDay(history.d2), legionDay(closedLater)}},
		{"EVAL-4 gains work on two days and stays open", "EVAL-4", func() {
			history.rows["EVAL-4"] = open
			r.writeFacts(t, history, "EVAL-4")
		}, nil},
	}
	before := r.dayRows(t)
	marks := 0
	for _, step := range steps {
		seq := r.latestSeq(t)
		step.write()
		marked := r.factsMarks(t, seq, "EVAL", step.row)
		marks += len(marked)
		for _, problem := range closeMarkProblems(marked, step.closes) {
			t.Errorf("%s: the change %s (marked %v, the close days are %v)", step.what, problem, marked, step.closes)
		}
		t.Logf("%s: the change marked %v; the close days are %v", step.what, marked, step.closes)
		before = r.expectDays(t, step.what, before, step.closes, step.closes)
	}
	if marks == 0 {
		t.Fatal("the changes marked no day, so the judgement read nothing")
	}

	// The negative control: a change of EVAL-3 naming its work days, which no day row counts it on.
	// The pass reads it and writes the days it names, so that the judgements, which are the ones
	// the steps above passed, are shown to find a change that names other days than the close
	// days wrong.
	seq := r.latestSeq(t)
	r.exec(t, "log a change naming EVAL-3's work days", `select delivery_change_log_pending('issue_facts', null, 'EVAL', 'EVAL-3', $1, $2)`,
		workFirst, workLast)
	marked := r.factsMarks(t, seq, "EVAL", "EVAL-3")
	controlBefore := r.dayRows(t)
	report, _ := r.pass(t)
	for _, day := range []string{legionDay(workFirst), legionDay(workLast)} {
		if !slices.Contains(report.sorted(), day) {
			t.Errorf("the pass of the change naming EVAL-3's work days wrote %v, not %s, so the negative control did not reach it", report.sorted(), day)
		}
	}
	closes := []string{legionDay(closedLater)}
	markProblems := closeMarkProblems(marked, closes)
	dayFaults := dayProblems("the negative control", report, controlBefore, r.dayRows(t), closes, nil)
	if len(markProblems) == 0 || len(dayFaults) == 0 {
		t.Errorf("a change marking EVAL-3's work days %v, whose pass wrote %v, passes as one marking its close day (mark problems %v, day problems %v), so the judgements prove nothing",
			marked, report.sorted(), markProblems, dayFaults)
	}
	t.Logf("%d changes judged, %d days marked. The negative control marked %v and its pass wrote %v, found wrong: %s; %d judgements of the days",
		len(steps), marks, marked, report.sorted(), strings.Join(markProblems, "; "), len(dayFaults))
}

// stampsMarker is a part of the text of movedFigureDays' read of the stamps of day rows, and of no
// other statement.
const stampsMarker = "cut.repo, d.day, d.series, d.updated_at"

// readsFiguresCursor reports whether data is the read of the stored figures' change cursor, with
// which a pass's read of the change log for the stored figures begins (figureChanges).
func readsFiguresCursor(data pgx.TraceQueryStartData) bool {
	return strings.Contains(data.SQL, "from eval_days_cursor where name = $1") && len(data.Args) == 1 && data.Args[0] == figuresCursor
}

// stampRead is one read of the stamps of day rows (movedFigureDays) a pass sent: its statement as
// sent, whether it was sent in a transaction, and whether it came after the pass's read of the
// stored figures' change cursor, and so is figureChanges' and not a page write's.
type stampRead struct {
	statement     explaintest.Statement
	inTx, changes bool
}

// stampReads is a tracer (storetest.TracedStore) that records, in the order they start, the reads
// of the stamps of day rows on the pool it traces (stampRead).
type stampReads struct {
	mu      sync.Mutex
	changes bool
	reads   []stampRead
}

func (s *stampReads) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case readsFiguresCursor(data):
		s.changes = true
	case strings.Contains(data.SQL, stampsMarker):
		s.reads = append(s.reads, stampRead{statement: explaintest.Statement{SQL: data.SQL, Args: data.Args}, inTx: conn.PgConn().TxStatus() != 'I', changes: s.changes})
	}
	return ctx
}

func (s *stampReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// readSpan is one span of days of one eval: one a read of the stamps names, or one a change names.
type readSpan struct {
	eval        string
	first, last time.Time
}

func (s readSpan) String() string {
	return fmt.Sprintf("%s %s..%s", s.eval, s.first.Format(time.DateOnly), s.last.Format(time.DateOnly))
}

// spansRead is the spans read names, in its order.
func spansRead(read stampRead) []readSpan {
	args := read.statement.Args
	evalIDs, firsts, lasts := args[1].([]string), args[3].([]time.Time), args[4].([]time.Time)
	spans := make([]readSpan, len(evalIDs))
	for index := range evalIDs {
		spans[index] = readSpan{eval: evalIDs[index], first: measures.Midnight(firsts[index]), last: measures.Midnight(lasts[index])}
	}
	return spans
}

// stampRowsRead is how many rows of eval_days read read, analyzed as it was sent, and how many its
// spans hold, counted by the test's own statement: every listed row, of the span's writer, of the
// days of each span, of each key the project reads (eval_project_owners) under its eval id of the
// span's slug.
func (r *refresherRig) stampRowsRead(t *testing.T, read stampRead) (rowsRead, held float64) {
	t.Helper()
	ctx := t.Context()
	plan := explaintest.ExplainBuffered(t, ctx, r.database.Pool, read.statement, explaintest.PlanCacheModes[0], "json")
	if err := r.database.Pool.QueryRow(ctx, `
		select count(*)::float8
		from unnest($2::text[], $3::text[], $4::date[], $5::date[]) as span(eval_id, writer, first_day, last_day)
		join eval_project_owners o on o.project_key = $1
		join eval_days d on d.project_key = o.owner and d.eval_id = o.owner || substr(span.eval_id, length($1) + 1)
			and d.writer = span.writer and d.day >= span.first_day and d.day <= span.last_day and d.listed`,
		read.statement.Args...).Scan(&held); err != nil {
		t.Fatalf("count the rows the spans of a read of the stamps hold: %v", err)
	}
	return evalDaysRowsRead(t, plan), held
}

// seedEarlyCloses adds to history, before its first walk, an issue of each of closes, EVAL-<n>,
// opened on the history's day -45, started the day after and closed at 9:00 on its day, the root of
// a tree of its own in acme/web with three hours of work, and stores their facts.
func (r *refresherRig) seedEarlyCloses(t *testing.T, history *legionEval, closes map[int]time.Time) {
	t.Helper()
	opened, started := legionHistoryDay(-45), legionHistoryDay(-44)
	var keys []string
	for _, number := range slices.Sorted(maps.Keys(closes)) {
		key := fmt.Sprintf("EVAL-%d", number)
		r.exec(t, "the issue "+key, `
			insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
			values ($1, 'EVAL', $2, 'An issue', 'in_progress', $3, 'acme/web', $4::jsonb, $5)
		`, key, number, fmt.Sprintf("%08d", number), history.actor, opened)
		closed := closes[number].Add(9 * time.Hour)
		history.rows[key] = legionfacts.Row{Project: "EVAL", Issue: key, ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: opened, ClosedAt: &closed, Root: true, Tree: key, WorkHours: 3,
			StartedAt: &started, PhaseHours: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{},
		}}}
		keys = append(keys, key)
	}
	r.writeFacts(t, history, keys...)
}

// TestACloseMovedThirtyDaysReadsTheStampsOfItsCloseDaysAlone: over the hand-written Legion history
// of EVAL with two more issues closed long before it (EVAL-6 on the history's day -41 and EVAL-5
// on its day -36, so no series' first input day moves with them), once every day is walked and
// the stored figures are built, EVAL-5's close moves 30 days on, to day -6. The pass that reads the
// change writes the two close days in one commit (writeProjectDays), a row of each and of no other
// day, and folds their stored figures in it (foldFiguresInTx): every read of the stamps it sends
// there (movedFigureDays) names the two days alone, each a span of its own, and reads exactly the
// rows of eval_days those spans hold, EVAL's Legion rows of the two days, where one span from the
// first day to the last read the 29 days between as well (k29 of dispatch://INFRA-2916). The same
// pass's read of the figures' change log (figureChanges) then reads, on the pool and in no
// transaction (refoldChanged), the span the log holds of each eval the commit wrote, from the first
// close day to the last (one change of each eval a transaction, migration 20261117100014), and no
// day outside the spans the changes name; it finds nothing moved, the page having folded both days,
// so it reads no stamps under a lock; and the stored figures are the fold of the rows that stand.
// It logs the rows each read read.
func TestACloseMovedThirtyDaysReadsTheStampsOfItsCloseDaysAlone(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	history := r.seedLegionEval(t)
	from, to := legionHistoryDay(-36), legionHistoryDay(-6)
	r.seedEarlyCloses(t, history, map[int]time.Time{5: from, 6: legionHistoryDay(-41)})
	r.settle(t)
	r.pass(t)
	r.exec(t, "analyze the day rows", `analyze eval_days`)
	if through, built := r.figuresFolded(t)["EVAL"]; !built || !to.Before(through) {
		t.Fatalf("EVAL's figures are folded %t through %s, and the check needs them folded past %s", built, through.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	closes := []string{line(walkLegion, "EVAL", from), line(walkLegion, "EVAL", to)}
	moved := history.rows["EVAL-5"]
	closedAt := to.Add(9 * time.Hour)
	moved.ClosedAt = &closedAt
	history.rows["EVAL-5"] = moved
	seq := r.latestSeq(t)
	r.writeFacts(t, history, "EVAL-5")
	if problems := closeMarkProblems(r.factsMarks(t, seq, "EVAL", "EVAL-5"), closes); len(problems) > 0 {
		t.Fatalf("the change of EVAL-5's close %v", problems)
	}

	before := r.dayRows(t)
	reads := &stampReads{}
	plain := r.registry
	r.registry = r.tracedStore(t, reads)
	seq = r.latestSeq(t)
	report, _ := r.refreshIn(ctx, t, nil)
	r.registry = plain
	for _, problem := range dayProblems("the pass of EVAL-5's moved close", report, before, r.dayRows(t), closes, closes) {
		t.Error(problem)
	}
	changes, err := delivery.ChangesSince(ctx, pool, seq, 1000)
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	logged := map[string][]readSpan{}
	for _, change := range changes {
		if change.Kind == delivery.ChangeKindEvalDays && change.Repo == "EVAL" && change.TOld != nil && change.TNew != nil {
			logged[change.Ref] = append(logged[change.Ref], readSpan{eval: change.Ref, first: measures.Midnight(*change.TOld), last: measures.Midnight(*change.TNew)})
		}
	}
	var pageRows, pageHeld, readRows float64
	var pageSpans, readSpans []readSpan
	locked := 0
	for _, read := range reads.reads {
		rowsRead, held := r.stampRowsRead(t, read)
		spans := spansRead(read)
		switch {
		case !read.changes:
			if !read.inTx {
				t.Errorf("the page's read of the stamps of %v was sent in no transaction, and the fold of its write is its commit's", spans)
			}
			pageRows, pageHeld, pageSpans = pageRows+rowsRead, pageHeld+held, append(pageSpans, spans...)
		case read.inTx:
			locked++
		default:
			readRows, readSpans = readRows+rowsRead, append(readSpans, spans...)
		}
	}
	if len(pageSpans) == 0 || len(readSpans) == 0 {
		t.Fatalf("the pass read the stamps of %d spans in its page's commit and of %d in figureChanges, and the check needs both", len(pageSpans), len(readSpans))
	}
	named := map[time.Time]bool{}
	for _, span := range pageSpans {
		if !span.first.Equal(span.last) || !span.first.Equal(from) && !span.first.Equal(to) {
			t.Errorf("the page's commit reads the stamps of %s, where it wrote %s and %s alone", span, from.Format(time.DateOnly), to.Format(time.DateOnly))
		}
		named[span.first] = true
	}
	if !named[from] || !named[to] {
		t.Errorf("the page's commit reads the stamps of the days %v, want %s and %s", slices.SortedFunc(maps.Keys(named), time.Time.Compare), from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	for _, span := range readSpans {
		if !slices.ContainsFunc(logged[span.eval], func(change readSpan) bool { return !span.first.Before(change.first) && !span.last.After(change.last) }) {
			t.Errorf("figureChanges reads the stamps of %s, which no change's span (%v) holds", span, logged[span.eval])
		}
	}
	if locked > 0 {
		t.Errorf("figureChanges read the stamps %d times in a transaction, under the fold's locks, where the page had folded every day it moved", locked)
	}
	legionEvals := make([]string, len(legionDaySlugs))
	for index, slug := range legionDaySlugs {
		legionEvals[index] = EvalID("EVAL", slug)
	}
	var closeRows, betweenRows float64
	for _, count := range []struct {
		into      *float64
		statement string
		days      []any
	}{
		{&closeRows, `day = any($2)`, []any{[]time.Time{from, to}}},
		{&betweenRows, `day > $2 and day < $3`, []any{from, to}},
	} {
		statement := `select count(*)::float8 from eval_days where project_key = 'EVAL' and listed and writer = 'computation' and eval_id = any($1) and ` + count.statement
		if err := pool.QueryRow(ctx, statement, append([]any{legionEvals}, count.days...)...).Scan(count.into); err != nil {
			t.Fatalf("count EVAL's Legion rows: %v", err)
		}
	}
	if pageRows != pageHeld || pageRows != closeRows {
		t.Errorf("the page's commit read %.0f rows of eval_days in its reads of the stamps, where their spans hold %.0f and EVAL's Legion rows of %s and %s are %.0f",
			pageRows, pageHeld, from.Format(time.DateOnly), to.Format(time.DateOnly), closeRows)
		for _, read := range reads.reads {
			if !read.changes {
				t.Logf("the page's stamps statement:\n%s", explaintest.ExplainBuffered(t, ctx, pool, read.statement, explaintest.PlanCacheModes[0], "text"))
			}
		}
	}
	expectTruth(t, "the pass of EVAL-5's moved close", r.storedFigures(t), r.truthFigures(t))
	t.Logf("the page's commit read %.0f rows of eval_days in its reads of the stamps of %d spans, the rows of %s and %s (%.0f of EVAL's Legion rows lie between them); figureChanges read %.0f rows on the pool in its reads of the stamps of %v, of the changes' spans %v, and read none under a lock",
		pageRows, len(pageSpans), from.Format(time.DateOnly), to.Format(time.DateOnly), betweenRows, readRows, readSpans, logged)
}
