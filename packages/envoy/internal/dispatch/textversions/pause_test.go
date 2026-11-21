package textversions

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// TestThePauseFollowsTheAnswersReadsThisProcessServes holds the fill's pause to each state the
// fill is in, filling or paused, with the answers reads that move it or leave it, one row each. A
// row writes two events and runs a fill of one event a batch, its pause the one a serving process
// sets (pauser.waitUntilFast) on answers reads a sessionanswers.Latency records, on a fake clock
// that moves only where the row moves it and as the paused fill waits. The row's reads before the
// fill, while its first batch runs and while it is paused bring the state and the event the row
// names. The row reads each pause: how many batches had begun when it paused, every one of them
// committed and none begun while it lasted; how long it lasted; and the lines it logged among the
// batches'.
func TestThePauseFollowsTheAnswersReadsThisProcessServes(t *testing.T) {
	for _, row := range []struct {
		name string
		// before brings the reads to the row's state as the fill asks before its first batch;
		// duringFirstBatch runs once the first batch's transaction has begun; whilePaused runs
		// each step once the fill has been paused its at.
		before, duringFirstBatch []readStep
		whilePaused              []timedStep
		want                     []observedPause
	}{
		{
			name: "filling, no read over 800 ms in 10 s and none running over 800 ms: the next batch runs",
			before: []readStep{
				readBegins("slow"), timePasses(900 * time.Millisecond), readEnds("slow"),
				timePasses(9200 * time.Millisecond),
				readBegins("800 ms"), readBegins("running"), timePasses(800 * time.Millisecond), readEnds("800 ms"),
			},
		},
		{
			name: "filling, a read ended over 800 ms: no next batch",
			before: []readStep{
				readBegins("slow"), timePasses(801 * time.Millisecond), readEnds("slow"),
				timePasses(4 * time.Second),
			},
			want: []observedPause{{after: 0, lasted: 6 * time.Second}},
		},
		{
			name:        "filling, a read has run over 800 ms and not ended: no next batch",
			before:      []readStep{readBegins("slow"), timePasses(801 * time.Millisecond)},
			whilePaused: []timedStep{{5 * time.Second, readEnds("slow")}},
			want:        []observedPause{{after: 0, lasted: 15 * time.Second}},
		},
		{
			name:             "filling, a read turns slow while a batch runs: that batch commits; no next batch",
			before:           []readStep{readBegins("slow"), timePasses(500 * time.Millisecond)},
			duringFirstBatch: []readStep{timePasses(400 * time.Millisecond)},
			whilePaused:      []timedStep{{2 * time.Second, readEnds("slow")}},
			want:             []observedPause{{after: 1, lasted: 12 * time.Second}},
		},
		{
			name:   "paused, 10 s pass with no slow read: continues",
			before: []readStep{readBegins("slow"), timePasses(900 * time.Millisecond), readEnds("slow")},
			whilePaused: []timedStep{
				{2 * time.Second, readBegins("800 ms")}, {2800 * time.Millisecond, readEnds("800 ms")},
				{9500 * time.Millisecond, readBegins("running")},
			},
			want: []observedPause{{after: 0, lasted: 10 * time.Second}},
		},
		{
			name:        "paused, a slow read inside the 10 s: the 10 s start again",
			before:      []readStep{readBegins("slow"), timePasses(900 * time.Millisecond), readEnds("slow")},
			whilePaused: []timedStep{{5 * time.Second, readBegins("1 s")}, {6 * time.Second, readEnds("1 s")}},
			want:        []observedPause{{after: 0, lasted: 16 * time.Second}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			database := storetest.Open(t)
			textversionstest.SeedIssue(t, database)
			broker := events.NewBroker()
			written := []int64{commitComment(t, database, broker, textversionstest.Issue).id, commitComment(t, database, broker, textversionstest.Issue).id}

			rig := &pauseRig{now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), reads: map[string]sessionanswers.Read{}}
			rig.latency = sessionanswers.NewLatency(rig.clock)
			rig.run(row.before)

			var logs bytes.Buffer
			fill := testFiller(database, events.NewHorizon(database.Pool), &logs)
			fill.maxEvents = 1
			pause := newPauser(rig.latency, fill.logger)
			pause.now = rig.clock
			pending := row.whilePaused
			pause.wait = func(ctx context.Context, d time.Duration) error {
				if d != pauseAskEvery {
					t.Errorf("the paused fill waited %v to ask again; want pauseAskEvery, %v", d, pauseAskEvery)
				}
				if !rig.paused {
					rig.paused, rig.pausedAt, rig.pausedAfter = true, rig.now, rig.begun
					if cursor, want := textversionstest.Cursor(t, database), cursorAfter(written, rig.begun); cursor != want {
						t.Errorf("the fill paused with %d batches begun and its cursor at event %d; want each of them committed, the cursor at event %d",
							rig.begun, cursor, want)
					}
				}
				until := rig.now.Add(d)
				for len(pending) > 0 && !rig.pausedAt.Add(pending[0].at).After(until) {
					rig.now = rig.pausedAt.Add(pending[0].at)
					pending[0].step(rig)
					pending = pending[1:]
				}
				rig.now = until
				if rig.now.Sub(rig.pausedAt) > time.Minute {
					return errors.New("the fill stayed paused for a minute")
				}
				return ctx.Err()
			}
			fill.pause = pause.waitUntilFast
			fill.pool = batchStarts{txBeginner: database.Pool, begins: func() {
				if rig.paused {
					rig.pauses = append(rig.pauses, observedPause{after: rig.pausedAfter, lasted: rig.now.Sub(rig.pausedAt)})
					rig.paused = false
				}
				if rig.begun++; rig.begun == 1 {
					rig.run(row.duringFirstBatch)
				}
			}}

			if err := fill.toHorizon(t.Context()); err != nil {
				t.Fatalf("the fill answered %v", err)
			}
			if !slices.Equal(rig.pauses, row.want) {
				t.Errorf("the fill paused %+v; want %+v (after: the batches begun when it paused; lasted: on the fake clock)", rig.pauses, row.want)
			}
			if len(pending) > 0 {
				t.Errorf("the fill continued before the reads of its pause at %v ran", pending[0].at)
			}
			if got := readCursorRow(t, database).eventID; got != written[1] {
				t.Errorf("the fill ended with its cursor at event %d; want event %d, the last written", got, written[1])
			}
			var lines []string
			for _, line := range logLines(t, &logs) {
				if line.Msg == batchMessage || line.Msg == pausedMessage || line.Msg == continuesMessage {
					lines = append(lines, line.Msg)
				}
			}
			if want := pauseLines(row.want, len(written)); !slices.Equal(lines, want) {
				t.Errorf("the fill logged\n%q\nwant\n%q", lines, want)
			}
		})
	}
}

// pauseRig is one row's answers reads, the fake clock they and the pause read, and what the fill
// did with them: the batches it began, the pause it is in, and the pauses it ended.
type pauseRig struct {
	now     time.Time
	latency *sessionanswers.Latency
	// reads holds each read the row began and has not ended, by its name.
	reads map[string]sessionanswers.Read
	// begun counts the batches the fill began. While paused, the fill has been paused since
	// pausedAt, with pausedAfter batches begun; pauses are the pauses it ended.
	begun       int
	paused      bool
	pausedAt    time.Time
	pausedAfter int
	pauses      []observedPause
}

// clock is the rig's fake clock.
func (r *pauseRig) clock() time.Time { return r.now }

// run runs steps in order.
func (r *pauseRig) run(steps []readStep) {
	for _, step := range steps {
		step(r)
	}
}

// readStep is one move of a row: a read begins or ends, or the clock moves.
type readStep func(r *pauseRig)

// readBegins begins the read name.
func readBegins(name string) readStep {
	return func(r *pauseRig) { r.reads[name] = r.latency.Begin() }
}

// readEnds ends the read name.
func readEnds(name string) readStep {
	return func(r *pauseRig) {
		r.reads[name].End()
		delete(r.reads, name)
	}
}

// timePasses moves the clock by d.
func timePasses(d time.Duration) readStep {
	return func(r *pauseRig) { r.now = r.now.Add(d) }
}

// timedStep is a step a row runs once the fill has been paused at.
type timedStep struct {
	at   time.Duration
	step readStep
}

// observedPause is one pause of the fill: how many batches had begun when it paused, and how
// long it lasted on the fake clock, until the next batch began.
type observedPause struct {
	after  int
	lasted time.Duration
}

// pauseLines is the lines a fill of batches batches that each write one event logs among its
// batches' lines when it pauses as pauses say: each pause's two lines after the batch lines of
// the batches begun before it.
func pauseLines(pauses []observedPause, batches int) []string {
	var lines []string
	for begun := 0; begun <= batches; begun++ {
		for _, pause := range pauses {
			if pause.after == begun {
				lines = append(lines, pausedMessage, continuesMessage)
			}
		}
		if begun < batches {
			lines = append(lines, batchMessage)
		}
	}
	return lines
}

// batchStarts is a pool that calls begins before each transaction it begins: as each batch
// starts.
type batchStarts struct {
	txBeginner
	begins func()
}

func (b batchStarts) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	b.begins()
	return b.txBeginner.BeginTx(ctx, options)
}

// cursorAfter is the cursor a fill of one event a batch holds once begun batches committed over
// written: the last event they wrote, 0 before any.
func cursorAfter(written []int64, begun int) int64 {
	if begun == 0 {
		return 0
	}
	return written[begun-1]
}
