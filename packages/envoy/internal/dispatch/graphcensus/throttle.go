package graphcensus

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Throttle is how hard one run of the backfill drives the database: how many rows each batch reads
// and each transaction writes, and how many rows a second each pass may read, which a pause after
// each batch holds.
type Throttle struct {
	// Batch is how many rows one batch reads and one transaction writes, 1 to MaxBackfillBatch.
	Batch int
	// Rate is the most rows a second each pass reads, 0 being no bound. It is held batch by batch:
	// after a batch of n rows the pass pauses until n/Rate has passed since that batch began, so no
	// batch, and no run of whole batches, goes faster, and a slow batch earns the next one no head
	// start.
	Rate int
}

// MaxBackfillBatch is the largest batch a Throttle takes. A batch is one transaction, which holds
// its rows' locks and its documents' revision fences until it commits; pgmigrate's bounded writes
// stop at the same 1,000 rows.
const MaxBackfillBatch = 1000

// DefaultThrottle is the throttle Backfill and `envoy-dispatch backfill-graph-store` run at
// unless the command's flags or settings name another: batches of 100 rows, at most 500 rows a
// second.
//
// Production's run of 2026-11-24 had no throttle: batches of 200 rows, each one right after the
// one before. Its block pass wrote 2,676,178 rows in 31 min 31 s, about 1,400 a second, bound by
// the database's CPU and its WAL writes. The database's CPU read 37-48% before the run and 52, 69,
// 75 and 66% during it, 23 points above the middle of the before on average and 32.5 at the
// worst, and 18.2% of search calls ended in a statement timeout, against 0.8% before it.
//
// At that run's cost a row, 1/1,400 s of the run's one connection, 500 rows a second keeps that
// connection busy 500/1,400 of the time, about 36%: a batch of 100 rows takes about 71 ms of every
// 200 ms. Scaled by the same share, the run adds about 8 points of CPU on average and 12 at the
// worst reading: about 51% and 55%, against the 65.5% and 75% of that run. A batch half the old
// one holds its locks and its documents' fences, and writes its WAL, for half as long. At 500
// rows a second a pass over that run's 2.7 million blocks takes about 90 minutes.
var DefaultThrottle = Throttle{Batch: 100, Rate: 500}

// Check refuses a throttle no run takes: a batch outside 1 to MaxBackfillBatch rows, or a rate
// below 0.
func (t Throttle) Check() error {
	if t.Batch < 1 || t.Batch > MaxBackfillBatch {
		return fmt.Errorf("a batch of %d rows: a batch is 1 to %d rows", t.Batch, MaxBackfillBatch)
	}
	if t.Rate < 0 {
		return fmt.Errorf("%d rows a second: the rate is 0, no bound, or more", t.Rate)
	}
	return nil
}

// String is the throttle as the run's first line names it.
func (t Throttle) String() string {
	if t.Rate == 0 {
		return fmt.Sprintf("batch=%d rows_per_second=0 (no pause between batches)", t.Batch)
	}
	return fmt.Sprintf("batch=%d rows_per_second=%d", t.Batch, t.Rate)
}

// paceClock is a pacer's reading of the time and its pause: wallClock, or a test's own.
type paceClock interface {
	Now() time.Time
	// Sleep pauses for d, or until ctx ends, answering ctx's error then.
	Sleep(ctx context.Context, d time.Duration) error
}

// wallClock is the process's own clock.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// paceLogEvery is how often a pass under way prints its pace line, so a person watching a pass of
// hours sees it move and sees the pause it takes.
const paceLogEvery = 30 * time.Second

// pacer holds one run of the backfill to its Throttle, one pass at a time, and prints each pass's
// pace: `backfill-graph-store: pace kind=<pass> rows=<n> batches=<n> seconds=<s> rate=<rows a
// second> paused=<s> done=<bool>`, every paceLogEvery while the pass runs and once when it ends,
// rate and paused counted from the pass's start.
type pacer struct {
	throttle Throttle
	clock    paceClock
	out      io.Writer
	// The pass under way: its kind; when it began, when its latest batch began and when it last
	// printed its pace; and the rows, the batches and the pause it has taken.
	kind                  string
	began, batch, printed time.Time
	rows, batches         int64
	paused                time.Duration
}

func newPacer(throttle Throttle, clock paceClock, out io.Writer) *pacer {
	return &pacer{throttle: throttle, clock: clock, out: out}
}

// unpaced is the pacer of the block-index tick's object pass (WriteOwedObjectNodes) and of a test
// that drives a pass, or a run of another marking rule, by itself: batches of backfillBatch rows,
// no pause, nothing printed. The tick bounds itself by its own budget of batches and holds the
// block-index lease while it runs, so a pause would hold the lease longer and spare the database
// nothing.
func unpaced() *pacer {
	return newPacer(Throttle{Batch: backfillBatch}, wallClock{}, io.Discard)
}

// begin starts the pass of kind.
func (p *pacer) begin(kind string) {
	now := p.clock.Now()
	p.kind, p.began, p.batch, p.printed = kind, now, now, now
	p.rows, p.batches, p.paused = 0, 0, 0
}

// after takes a batch of rows the pass has read and written, and pauses until the batch has taken
// rows/Rate since it began, so the pass reads no faster than Rate. It answers ctx's error where ctx
// ends during the pause.
func (p *pacer) after(ctx context.Context, rows int) error {
	p.rows += int64(rows)
	p.batches++
	if p.throttle.Rate > 0 {
		due := p.batch.Add(time.Duration(rows) * time.Second / time.Duration(p.throttle.Rate))
		if wait := due.Sub(p.clock.Now()); wait > 0 {
			if err := p.clock.Sleep(ctx, wait); err != nil {
				return fmt.Errorf("pause after a batch of the %s pass: %w", p.kind, err)
			}
			p.paused += wait
		}
	}
	now := p.clock.Now()
	p.batch = now
	if now.Sub(p.printed) >= paceLogEvery {
		p.printed = now
		p.write(false)
	}
	return nil
}

// end prints the pace of the pass once it has read its last batch.
func (p *pacer) end() {
	p.write(true)
}

func (p *pacer) write(done bool) {
	elapsed := p.clock.Now().Sub(p.began)
	rate := 0.0
	if elapsed > 0 {
		rate = float64(p.rows) / elapsed.Seconds()
	}
	fmt.Fprintf(p.out, "backfill-graph-store: pace kind=%s rows=%d batches=%d seconds=%.1f rate=%.1f paused=%.1f done=%t\n",
		p.kind, p.rows, p.batches, elapsed.Seconds(), rate, p.paused.Seconds(), done)
}
