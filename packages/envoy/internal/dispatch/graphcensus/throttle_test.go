package graphcensus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fakeClock is a paceClock whose time moves only when a test says so: work moves it by the time a
// batch takes, and Sleep records each pause and moves it by the pause, so a test reads every
// pause the pacer takes and the exact time each batch began and ended. onSleep, where set, runs
// at each pause before the clock moves, while the run is between two batches.
type fakeClock struct {
	now     time.Time
	sleeps  []time.Duration
	onSleep func()
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 24, 13, 39, 30, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.onSleep != nil {
		c.onSleep()
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) work(d time.Duration) { c.now = c.now.Add(d) }

// paceCycle is one batch of a paced pass as the fake clock saw it: its rows, and the time from
// the batch's start to the next batch's start, which is the batch's read and write and the pause
// after it.
type paceCycle struct {
	rows  int
	taken time.Duration
}

// requirePacedAt answers why cycles read faster than rate rows a second, or nil where none does:
// each cycle's rows over its own time, and the whole pass's rows over its whole time, compared in
// whole nanoseconds. A pass of one batch after another with no pause reads at the rate its
// batches take, which is the reading this exists to refuse.
func requirePacedAt(cycles []paceCycle, rate int) error {
	if len(cycles) == 0 {
		return errors.New("no batch was read, so nothing shows the pace")
	}
	faster := func(rows int, taken time.Duration) bool {
		return time.Duration(rows)*time.Second > time.Duration(rate)*taken
	}
	var rows int
	var taken time.Duration
	for index, cycle := range cycles {
		rows += cycle.rows
		taken += cycle.taken
		if faster(cycle.rows, cycle.taken) {
			return fmt.Errorf("batch %d read %d rows in %v, faster than %d rows a second", index+1, cycle.rows, cycle.taken, rate)
		}
	}
	if faster(rows, taken) {
		return fmt.Errorf("the pass read %d rows in %v, faster than %d rows a second", rows, taken, rate)
	}
	return nil
}

// pacedPass runs one pass of batches through a pacer of throttle on clock: each batch of rows
// takes work, then the pacer takes it. It answers each batch's cycle.
func pacedPass(t *testing.T, throttle Throttle, clock *fakeClock, out io.Writer, batches []struct {
	rows int
	work time.Duration
}) []paceCycle {
	t.Helper()
	p := newPacer(throttle, clock, out)
	p.begin(refs.BlockNodeKind)
	cycles := make([]paceCycle, 0, len(batches))
	for _, batch := range batches {
		began := clock.Now()
		clock.work(batch.work)
		if err := p.after(context.Background(), batch.rows); err != nil {
			t.Fatalf("take a batch of %d rows: %v", batch.rows, err)
		}
		cycles = append(cycles, paceCycle{rows: batch.rows, taken: clock.Now().Sub(began)})
	}
	p.end()
	return cycles
}

// The pacer pauses after each batch until that batch has taken its rows over the rate since it
// began, and never less: a batch that took 70 ms of its 200 ms waits 130 ms, one that took longer
// than its 200 ms waits nothing, and the batch after a slow one still waits all its own time, so a
// slow batch buys no burst; the short last batch waits its own 80 ms less its work. Each batch
// then reads no faster than the rate, nor the pass as a whole. The control is the same pass with
// no rate, the throttle disabled: it pauses nowhere, and the same check refuses it as reading
// faster than the rate, so the check sees an unpaced pass.
func TestThePacerPausesEachBatchUntilItHasTakenItsRowsOverTheRate(t *testing.T) {
	batches := []struct {
		rows int
		work time.Duration
	}{
		{100, 70 * time.Millisecond},
		{100, 300 * time.Millisecond},
		{100, 10 * time.Millisecond},
		{40, 5 * time.Millisecond},
	}
	clock := newFakeClock()
	var out bytes.Buffer
	cycles := pacedPass(t, Throttle{Batch: 100, Rate: 500}, clock, &out, batches)
	want := []time.Duration{130 * time.Millisecond, 190 * time.Millisecond, 75 * time.Millisecond}
	if fmt.Sprint(clock.sleeps) != fmt.Sprint(want) {
		t.Errorf("the pacer paused %v after the batches, want %v: each batch's 200 ms (80 ms for the last) less its work, and none after the slow one", clock.sleeps, want)
	}
	if err := requirePacedAt(cycles, 500); err != nil {
		t.Errorf("the paced pass: %v", err)
	}
	// 340 rows over 70+130+300+10+190+5+75 = 780 ms.
	if line := "backfill-graph-store: pace kind=block rows=340 batches=4 seconds=0.8 rate=435.9 paused=0.4 done=true\n"; out.String() != line {
		t.Errorf("the pass printed %q, want %q", out.String(), line)
	}

	control := newFakeClock()
	unthrottled := pacedPass(t, Throttle{Batch: 100}, control, io.Discard, batches)
	if len(control.sleeps) != 0 {
		t.Errorf("the pass with no rate paused %v, want no pause", control.sleeps)
	}
	if err := requirePacedAt(unthrottled, 500); err == nil {
		t.Error("control: the pass with no rate read as paced at 500 rows a second, so this check cannot see a pass that is not")
	} else {
		t.Logf("control: the pass with no rate is refused: %v", err)
	}
}

// A pass under way prints its pace every paceLogEvery and once when it ends, so a person watching
// a pass of hours sees the rate and the pause move: 160 batches of 100 rows, each 50 ms of work
// and 150 ms of pause at 500 rows a second, are 32 s, so the pass prints one line at its 30th
// second and one at its end.
func TestAPassPrintsItsRateAndPauseWhileItRunsAndWhenItEnds(t *testing.T) {
	batches := make([]struct {
		rows int
		work time.Duration
	}, 160)
	for index := range batches {
		batches[index].rows, batches[index].work = 100, 50*time.Millisecond
	}
	var out bytes.Buffer
	pacedPass(t, Throttle{Batch: 100, Rate: 500}, newFakeClock(), &out, batches)
	want := "backfill-graph-store: pace kind=block rows=15000 batches=150 seconds=30.0 rate=500.0 paused=22.5 done=false\n" +
		"backfill-graph-store: pace kind=block rows=16000 batches=160 seconds=32.0 rate=500.0 paused=24.0 done=true\n"
	if out.String() != want {
		t.Errorf("the pass printed\n%s\nwant\n%s", out.String(), want)
	}
}

// A pause ends with the run's context, as a deadline or a stop ends the run: the pacer answers the
// context's error, naming the pass, and the wall clock's own pause of an hour returns at once on a
// context already ended.
func TestAPauseEndsWithTheRunsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := newPacer(Throttle{Batch: 100, Rate: 500}, newFakeClock(), io.Discard)
	p.begin(graphstore.KindComment)
	if err := p.after(ctx, 100); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the comment pass") {
		t.Errorf("a pause under an ended context answered %v, want context.Canceled naming the comment pass", err)
	}
	began := time.Now()
	if err := (wallClock{}).Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("the wall clock's pause under an ended context answered %v, want context.Canceled", err)
	}
	if waited := time.Since(began); waited > time.Minute {
		t.Errorf("the wall clock's pause of an hour under an ended context returned after %v", waited)
	}
}

// A throttle is a batch of 1 to MaxBackfillBatch rows and a rate of 0 or more; a rate of 0 is
// named as no pause on the run's first line, and the default is a pause.
func TestAThrottleIsABatchInItsBoundsAndARateOfZeroOrMore(t *testing.T) {
	for _, throttle := range []Throttle{{Batch: 1}, {Batch: MaxBackfillBatch, Rate: 1}, DefaultThrottle} {
		if err := throttle.Check(); err != nil {
			t.Errorf("%+v refused: %v", throttle, err)
		}
	}
	for _, throttle := range []Throttle{{Batch: 0, Rate: 500}, {Batch: MaxBackfillBatch + 1, Rate: 500}, {Batch: 100, Rate: -1}} {
		if err := throttle.Check(); err == nil {
			t.Errorf("%+v taken, want a refusal", throttle)
		}
	}
	if DefaultThrottle.Rate == 0 || DefaultThrottle.Batch > MaxBackfillBatch {
		t.Errorf("DefaultThrottle is %+v: the default must pause and keep its batch in bounds", DefaultThrottle)
	}
	if got, want := (Throttle{Batch: 100}).String(), "batch=100 rows_per_second=0 (no pause between batches)"; got != want {
		t.Errorf("a throttle of no rate reads %q, want %q", got, want)
	}
	if got, want := DefaultThrottle.String(), "batch=100 rows_per_second=500"; got != want {
		t.Errorf("DefaultThrottle reads %q, want %q", got, want)
	}
}

// pauseReading is what the database held at one pause of a run: the pass and the rows it had
// taken by then, and how many of the seeded rows of that pass named their node or their project.
type pauseReading struct {
	kind     string
	taken    int64
	written  int64
	duration time.Duration
}

// throttledRun seeds a database with the rows of two passes, 23 blocks naming no node and 9
// comment nodes naming no project, runs the whole backfill on it at throttle with a fake clock,
// and answers each pause with what the database held at it, and what the run printed.
func throttledRun(t *testing.T, throttle Throttle) ([]pauseReading, string) {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	document, comments := seedThrottledPasses(t, ctx, database, 23, 9)
	clock := newFakeClock()
	var out bytes.Buffer
	p := newPacer(throttle, clock, &out)
	var readings []pauseReading
	clock.onSleep = func() {
		reading := pauseReading{kind: p.kind, taken: p.rows, written: -1}
		switch p.kind {
		case refs.BlockNodeKind:
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from document_blocks where artifact_id = $1::uuid and node_kind is not null
			`, document).Scan(&reading.written); err != nil {
				t.Errorf("count the blocks naming their node at a pause: %v", err)
			}
		case graphstore.KindComment + "_project":
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from graph_nodes where kind = 'comment' and id = any($1) and project_key is not null
			`, comments).Scan(&reading.written); err != nil {
				t.Errorf("count the comment nodes naming their project at a pause: %v", err)
			}
		}
		readings = append(readings, reading)
	}
	if _, err := backfillAt(ctx, database, &out, refs.BlockIndexMarkingRule, p); err != nil {
		t.Fatalf("run the backfill at %+v: %v\n%s", throttle, err, out.String())
	}
	for index := range readings {
		readings[index].duration = clock.sleeps[index]
	}
	return readings, out.String()
}

// seedThrottledPasses writes, by SQL alone, a document of blocks blocks that name no node, as a
// release before the store wrote them, and comments comment nodes a build before a node named its
// project stored with none, on issue THR-1, and answers the document's id and the comments' ids.
func seedThrottledPasses(t *testing.T, ctx context.Context, database *store.Store, blocks, comments int) (string, []string) {
	t.Helper()
	alice := `{"kind":"user","id":"alice"}`
	var document, refKey string
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('THR', 'Throttle', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('THR-1', 'THR', 1, 'One', '`+alice+`', 'U');
	`); err != nil {
		t.Fatalf("seed the project and the issue: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('THR-1', 'THR', 'spec', 'spec.md', 'doc', false, $1)
		returning id::text, ref_key
	`, alice).Scan(&document, &refKey); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'Written before the store existed.', '[`+alice+`]')
	`, document); err != nil {
		t.Fatalf("write version 1 of the document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		select $1::uuid, 'b' || lpad(n::text, 3, '0'), $2 || '#b' || lpad(n::text, 3, '0'), 'paragraph', '', 'A block.', 1, now()
		from generate_series(0, $3 - 1) n
	`, document, refKey, blocks); err != nil {
		t.Fatalf("write the document's blocks: %v", err)
	}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ids := make([]string, comments)
	for index := range ids {
		ids[index] = fmt.Sprintf("33333333-3333-4333-8333-%012d", index+1)
		node, tuples := graphstore.CommentFacts(nil, ids[index], model.Actor{Kind: "user", ID: "alice"}, at,
			graphstore.NoEvent, "", "", "", nil)
		if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
			if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, tuples); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				insert into comments (id, issue_key, author, body, created_at, node_kind, node_address)
				values ($1::uuid, 'THR-1', $2, 'A remark.', $3, 'comment', $1)
			`, ids[index], alice, at)
			return err
		}); err != nil {
			t.Fatalf("seed comment %s with a node naming no project: %v", ids[index], err)
		}
	}
	return document, ids
}

// The backfill writes batches of its throttle's size against PostgreSQL and pauses between them,
// in each pass, the projects pass among them: at a batch of 7 rows and 70 rows a second, the
// 23 blocks are written 7, 7, 7 and 2 at a time and the 9 comment nodes take their project 7 and 2
// at a time. At each pause the database holds exactly the rows of the batches before it, so each
// batch committed before its pause and the next had not begun, and each pause is its batch's rows
// over the rate: 100 ms for 7 rows and 2/70 s for 2, since the fake clock gives the database's own
// work no time. The run's first line names the throttle and each pass prints its pace. The control
// is the same run with no rate: it pauses nowhere, so no reading shows the batches apart, and the
// same check refuses it.
func TestTheBackfillWritesBatchesOfItsThrottleAndPausesBetweenThem(t *testing.T) {
	throttle := Throttle{Batch: 7, Rate: 70}
	readings, out := throttledRun(t, throttle)
	if err := requireBatchesPaused(readings, throttle, map[string][]int64{
		refs.BlockNodeKind:                  {7, 14, 21, 23},
		graphstore.KindComment + "_project": {7, 9},
	}); err != nil {
		t.Errorf("%v\nthe run printed:\n%s", err, out)
	}
	for _, line := range []string{
		"backfill-graph-store: throttle batch=7 rows_per_second=70\n",
		"backfill-graph-store: pace kind=block rows=23 batches=4 seconds=0.3 rate=70.0 paused=0.3 done=true\n",
		"backfill-graph-store: pace kind=comment_project rows=9 batches=2 seconds=0.1 rate=70.0 paused=0.1 done=true\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the run printed no line %q:\n%s", line, out)
		}
	}

	control, controlOut := throttledRun(t, Throttle{Batch: 7})
	if len(control) != 0 {
		t.Errorf("control: the run with no rate paused %d times: %+v", len(control), control)
	}
	if err := requireBatchesPaused(control, throttle, map[string][]int64{refs.BlockNodeKind: {7, 14, 21, 23}}); err == nil {
		t.Errorf("control: the run with no rate passed the check of a paced run, so the check cannot see a run that is not:\n%s", controlOut)
	} else {
		t.Logf("control: the run with no rate is refused: %v", err)
	}
}

// requireBatchesPaused answers why readings are not a pause after each batch of throttle in the
// passes want names, at each of which the pass had taken, and the database held written, the rows
// want gives in order, each pause lasting its batch's rows over throttle's rate.
func requireBatchesPaused(readings []pauseReading, throttle Throttle, want map[string][]int64) error {
	byKind := map[string][]pauseReading{}
	for _, reading := range readings {
		byKind[reading.kind] = append(byKind[reading.kind], reading)
	}
	for kind, cumulative := range want {
		got := byKind[kind]
		if len(got) != len(cumulative) {
			return fmt.Errorf("the %s pass paused %d times, %+v, want once after each of its %d batches", kind, len(got), got, len(cumulative))
		}
		var before int64
		for index, rows := range cumulative {
			batch := rows - before
			if batch > int64(throttle.Batch) {
				return fmt.Errorf("the %s pass's batch %d took %d rows, more than the throttle's %d", kind, index+1, batch, throttle.Batch)
			}
			if got[index].taken != rows || got[index].written != rows {
				return fmt.Errorf("at the %s pass's pause %d it had taken %d rows and the database held %d written, want %d of each",
					kind, index+1, got[index].taken, got[index].written, rows)
			}
			if pause := time.Duration(batch) * time.Second / time.Duration(throttle.Rate); got[index].duration != pause {
				return fmt.Errorf("the %s pass's pause %d lasted %v, want %v: %d rows at %d a second", kind, index+1, got[index].duration, pause, batch, throttle.Rate)
			}
			before = rows
		}
	}
	return nil
}
