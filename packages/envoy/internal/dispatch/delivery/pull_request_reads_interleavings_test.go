package delivery

// The invariant of the owed reads, over random interleavings
// of the writers of one pull request: no final state holds an older head or state than the one
// GitHub answered last. The writers are the real code: the intake's handling of an event, whose
// read of GitHub succeeds or fails; the reconcile's pass; the release before's write of an
// envelope; GitHub itself, whose changes come in the same second or a later one; and a step back
// of the database's clock. Each writer stops at the points where another writer can come between
// two of its steps, and a seeded scheduler lets exactly one of them go on at a time, so a run is
// one interleaving of the writers and the seed chooses which.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// ghPull is one pull request as GitHub holds it in the interleavings, and one event GitHub sent
// for it: the pull request as GitHub held it right after the change the event reports.
type ghPull struct {
	head      string
	state     string // open, closed or merged
	updatedAt time.Time
}

// String names the head by what tells it from the other heads of its pull request.
func (p ghPull) String() string {
	return fmt.Sprintf("%s %s at %s", strings.TrimRight(p.head, "0"), p.state, p.updatedAt.Format(time.TimeOnly))
}

// interleavingWriter is one writer of the interleavings: it runs on a goroutine of its own and
// waits at each stop until the scheduler lets it go on.
type interleavingWriter struct {
	id   int
	kind string
	// late is whether its reads of GitHub answer what GitHub holds when the read is let go, and
	// not what it held when the read arrived (an answer held back in transit).
	late bool
	// fails is whether its reads of GitHub answer 500.
	fails  bool
	resume chan struct{}
}

// interleavingStop is what a writer tells the scheduler: it stopped at point, or it ended.
type interleavingStop struct {
	writer *interleavingWriter
	point  string
	ended  bool
	err    error
}

type interleavingWriterKey struct{}

// interleavings is the scheduler's side of the writers, and the stand-in for GitHub it drives.
type interleavings struct {
	t     *testing.T
	stops chan interleavingStop
	mu    sync.Mutex
	// github is each pull request as GitHub holds it now.
	github map[int]ghPull
	// running is the one writer the scheduler let go on, nil where none is: the writer a read of
	// GitHub that arrives now belongs to.
	running *interleavingWriter
	// over is closed when the test ends, which lets every stopped writer go, so a failed test
	// leaves no handler of the stand-in for GitHub waiting.
	over chan struct{}
	// behind is the pull request whose owed reads a database clock 3 s behind the reader's
	// dated, 0 where none: before each delete of an owed read, its owed reads move 3 s back, on
	// pool, which no writer's tracer stops.
	behind int
	pool   *store.Pool
	// fired counts how often each stop point stopped a writer, so a stop whose statement no
	// writer runs any more fails the test (stopPoints).
	fired map[string]int
}

// stopPoints is every point where a writer stops. Each must stop a writer in the run, or the
// interleavings test none of the orders that point stands for.
var stopPoints = []string{"github", "write", "owe", "settle"}

// stop waits at point until the scheduler lets w go on, or the test ends.
func (s *interleavings) stop(w *interleavingWriter, point string) {
	s.mu.Lock()
	s.fired[point]++
	s.mu.Unlock()
	select {
	case s.stops <- interleavingStop{writer: w, point: point}:
	case <-s.over:
		return
	}
	select {
	case <-w.resume:
	case <-s.over:
	}
}

// TraceQueryStart stops a writer where another writer can come between two of its steps: the
// write of the head row, which is the first statement of the transaction that writes the head
// row and its texts (upsertHead), and each owe and each delete of an owed read. No stop holds a
// lock: that first statement is the one that takes the row's.
func (s *interleavings) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	w, _ := ctx.Value(interleavingWriterKey{}).(*interleavingWriter)
	if w == nil {
		return ctx
	}
	switch {
	case strings.Contains(data.SQL, "insert into pull_request_heads"):
		s.stop(w, "write")
	case strings.Contains(data.SQL, "delete from pull_request_reads_owed"):
		s.mu.Lock()
		behind := s.behind
		s.mu.Unlock()
		if behind != 0 {
			if _, err := s.pool.Exec(ctx, `
				update pull_request_reads_owed set owed_at = owed_at - interval '3 seconds'
				where repo = 'acme/widgets' and number = $1
			`, behind); err != nil {
				s.t.Errorf("date the owed reads by a clock behind: %v", err)
			}
		}
		s.stop(w, "settle")
	case strings.Contains(data.SQL, "insert into pull_request_reads_owed"):
		s.stop(w, "owe")
	}
	return ctx
}

func (*interleavings) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// serve answers GET /repos/acme/widgets/pulls/{number} from what GitHub holds. A read of the
// running writer stops there, with the answer taken as it arrived; it then answers that, or what
// GitHub holds when it is let go where the writer reads late, or 500 where its reads fail. Each
// pull request's head branch names its issue, so no commit messages are read.
func (s *interleavings) serve(fake *fakeGitHub) {
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			s.t.Errorf("a pull request read of %s: %v", r.URL.Path, err)
		}
		s.mu.Lock()
		pull, writer := s.github[number], s.running
		s.mu.Unlock()
		if writer != nil {
			s.stop(writer, "github")
			if writer.late {
				s.mu.Lock()
				pull = s.github[number]
				s.mu.Unlock()
			}
			if writer.fails {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message":"Server Error"}`)
				return
			}
		}
		state := "closed"
		if pull.state == "open" {
			state = "open"
		}
		mustEncode(s.t, w, map[string]any{
			"number": number, "title": fmt.Sprintf("Change %d", number), "body": "", "state": state,
			"merged": pull.state == "merged", "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			"updated_at": pull.updatedAt.UTC().Format(time.RFC3339),
			"head": map[string]any{"ref": fmt.Sprintf("legion/HEAD-%d", number), "sha": pull.head,
				"repo": map[string]any{"full_name": "acme/widgets"}},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(s.t, w, []any{})
	})
}

// No interleaving of the writers of one pull request leaves it stored at a head or a state older
// than GitHub's last answer, once the events still waiting are handled and the reconcile has read
// every owed read (R3: "An older event does not replace a newer head"; R4: "Dispatch keeps which
// pull request changed until a read succeeds"). 150 pull requests, one sequence each of 16
// scheduled steps, after which every writer still waiting is let go in a random order. In half of
// the sequences, chosen at random, a database clock 3 s behind the reader's dates every owed read
// (each owed read moves 3 s back before each delete of one). A step changes GitHub (in the same
// second as its last change or a later one, and each change sends an event), starts an intake
// handling of the next waiting event (its reads of GitHub fail for one in four, and a failure owes
// the read and puts the event back, as Intake.deliver and NATS do), starts a reconcile pass where
// none runs, writes the next waiting event as the release before does, steps the database's
// clock back 3 s for the owed reads, or lets one waiting writer go on.
// Three controls, each one old rule put back through -overlay on a fresh database, fail it (the
// report of this change): the trigger without its case of the same second, an answer that covers
// an event of its own second where it was written 5 s after it, and a delete of every owed read
// dated before the read began less 1 s.
func TestNoInterleavingOfTheWritersLeavesAnOlderHeadThanGitHubsLastAnswer(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	fake := newFakeGitHub(t)
	s := &interleavings{t: t, stops: make(chan interleavingStop), github: map[int]ghPull{}, over: make(chan struct{}), pool: pool, fired: map[string]int{}}
	s.serve(fake)
	client := fake.newTestClient()
	// After the stand-in's own cleanup is registered, so it runs first: the server's Close waits
	// for every handler.
	t.Cleanup(func() { close(s.over) })
	traced := storetest.Traced(t, pool, s, nil)
	// Two intakes: one writes a head in a transaction with its attribution events, as the
	// server's does, and one in a single statement.
	intakes := []*Intake{newIntake(nil, traced, nil, client, events.NewBroker(), nil), newIntake(nil, traced, nil, client, nil, nil)}
	reconcile := NewReconcile(traced, client, events.NewBroker())
	rng := rand.New(rand.NewPCG(3992, 4209))
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	const sequences = 150
	ops := map[string]int{}
	violations := 0
	for seq := range sequences {
		number := 89500 + seq
		issue := seedHeadIssue(t, ctx, pool, number)
		branch := fmt.Sprintf("legion/HEAD-%d", number)
		behind := 0
		if rng.IntN(2) == 0 {
			behind = number
		}
		s.mu.Lock()
		s.behind = behind
		s.mu.Unlock()
		var trace []string
		note := func(format string, args ...any) { trace = append(trace, fmt.Sprintf(format, args...)) }
		note("owed reads dated by a clock 3 s behind: %v", behind != 0)
		pull := ghPull{head: fakeSHA("e", number*100), state: "open", updatedAt: base}
		s.mu.Lock()
		s.github[number] = pull
		s.mu.Unlock()
		queue := []ghPull{pull}
		var waiting []*interleavingWriter
		writers, pushes := 0, 0

		// take is the next waiting event; put hands one back to the queue's end.
		take := func() (ghPull, bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(queue) == 0 {
				return ghPull{}, false
			}
			ev := queue[0]
			queue = queue[1:]
			return ev, true
		}
		put := func(ev ghPull) {
			s.mu.Lock()
			defer s.mu.Unlock()
			queue = append(queue, ev)
		}
		running := func(kind string) int {
			n := 0
			for _, w := range waiting {
				if w.kind == kind {
					n++
				}
			}
			return n
		}
		// start makes a writer that waits until it is first let go, then runs body.
		start := func(kind string, fails bool, body func(context.Context) error) *interleavingWriter {
			writers++
			w := &interleavingWriter{id: writers, kind: kind, late: rng.IntN(2) == 0, fails: fails, resume: make(chan struct{})}
			go func() {
				select {
				case <-w.resume:
				case <-s.over:
					return
				}
				err := body(context.WithValue(ctx, interleavingWriterKey{}, w))
				select {
				case s.stops <- interleavingStop{writer: w, ended: true, err: err}:
				case <-s.over:
				}
			}()
			waiting = append(waiting, w)
			return w
		}
		// run lets w go on until it stops again or ends.
		run := func(w *interleavingWriter) {
			s.mu.Lock()
			s.running = w
			s.mu.Unlock()
			w.resume <- struct{}{}
			var stop interleavingStop
			select {
			case stop = <-s.stops:
			case <-time.After(30 * time.Second):
				t.Fatalf("seq %d: %s%d neither stopped nor ended within 30 s; trace:\n  %s", seq, w.kind, w.id, strings.Join(trace, "\n  "))
			}
			s.mu.Lock()
			s.running = nil
			s.mu.Unlock()
			if stop.writer != w {
				t.Fatalf("seq %d: %s%d stopped at %q (ended %v, %v) while %s%d ran; trace:\n  %s", seq, stop.writer.kind, stop.writer.id,
					stop.point, stop.ended, stop.err, w.kind, w.id, strings.Join(trace, "\n  "))
			}
			if !stop.ended {
				note("%s%d at %s", w.kind, w.id, stop.point)
				return
			}
			if stop.err != nil {
				t.Errorf("seq %d: %s%d: %v", seq, w.kind, w.id, stop.err)
			}
			note("%s%d ends", w.kind, w.id)
			for i, other := range waiting {
				if other == w {
					waiting = append(waiting[:i], waiting[i+1:]...)
					return
				}
			}
		}
		// handle is one intake handling of ev.
		handle := func(in *Intake, ev ghPull) func(context.Context) error {
			envelope := prEnvelope("acme/widgets", number, "synchronize", ev.head, branch, "", ev.updatedAt)
			return func(ctx context.Context) error {
				_, err := in.handlePullRequestEnvelope(ctx, nil, envelope)
				if !errors.Is(err, ErrHeadNotStored) {
					return err
				}
				raw, err := json.Marshal(envelope)
				if err != nil {
					return err
				}
				put(ev)
				_, _, err = in.oweEnvelopesRead(ctx, string(raw))
				return err
			}
		}

		for range 16 {
			op := rng.IntN(20)
			switch {
			case op < 3 && pull.state != "merged":
				ops["github_change"]++
				pushes++
				next := pull
				if rng.IntN(5) >= 2 {
					next.updatedAt = pull.updatedAt.Add(time.Duration(1+rng.IntN(3)) * time.Second)
				} else {
					ops["github_change_same_second"]++
				}
				switch rng.IntN(6) {
				case 0:
					next.state = map[string]string{"open": "closed", "closed": "open"}[pull.state]
				case 1:
					next.state = "merged"
				default:
					next.head, next.state = fakeSHA("e", number*100+pushes), "open"
				}
				pull = next
				s.mu.Lock()
				s.github[number] = pull
				s.mu.Unlock()
				put(pull)
				note("github %v", pull)
			case op < 8 && running("I") < 2:
				ev, ok := take()
				if !ok {
					continue
				}
				ops["intake"]++
				fails := rng.IntN(4) == 0
				in := intakes[rng.IntN(len(intakes))]
				w := start("I", fails, handle(in, ev))
				note("I%d takes %v fails=%v late=%v transaction=%v", w.id, ev, fails, w.late, in.events != nil)
			case op < 10:
				ev, ok := take()
				if !ok {
					continue
				}
				ops["release_before_write"]++
				writeHeadAsTheReleaseBefore(t, ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
					HeadSHA: ev.head, HeadRef: branch, BaseRef: "main", State: ev.state, EventAt: &ev.updatedAt},
					releaseBeforeAttribution{issue: &issue, source: string(sourceBranch), complete: true})
				note("P writes %v", ev)
			case op < 12 && running("R") == 0:
				ops["reconcile"]++
				w := start("R", false, func(ctx context.Context) error { return reconcile.reconcilePullRequestHeads(ctx, time.Now()) })
				note("R%d starts late=%v", w.id, w.late)
			case op < 13:
				ops["clock_step"]++
				if _, err := pool.Exec(ctx, `
					update pull_request_reads_owed set owed_at = owed_at - interval '3 seconds'
					where repo = 'acme/widgets' and number = $1
				`, number); err != nil {
					t.Fatal(err)
				}
				note("clock steps back 3 s")
			case len(waiting) > 0:
				ops["let_go"]++
				run(waiting[rng.IntN(len(waiting))])
			}
		}
		for len(waiting) > 0 {
			run(waiting[rng.IntN(len(waiting))])
		}

		// What the system does next: NATS delivers the events still waiting, GitHub answers, and
		// the reconcile reads every owed read until none is owed.
		for {
			ev, ok := take()
			if !ok {
				break
			}
			if _, err := intakes[0].handlePullRequestEnvelope(ctx, nil,
				prEnvelope("acme/widgets", number, "synchronize", ev.head, branch, "", ev.updatedAt)); err != nil {
				t.Fatalf("seq %d: the event still waiting: %v", seq, err)
			}
		}
		passes := 0
		for ; passes < 6 && owedCount(t, ctx, pool, "acme/widgets", number) > 0; passes++ {
			if err := reconcile.reconcilePullRequestHeads(ctx, time.Now()); err != nil {
				t.Fatalf("seq %d: a reconcile pass: %v", seq, err)
			}
		}
		got := readHead(t, ctx, pool, "acme/widgets", number)
		owed := owedCount(t, ctx, pool, "acme/widgets", number)
		if got.HeadSHA != pull.head || got.State != pull.state || owed != 0 {
			violations++
			if violations <= 3 {
				t.Logf("seq %d: stored %s %s, GitHub's last answer %v, owed %d after %d passes; trace:\n  %s",
					seq, strings.TrimRight(got.HeadSHA, "0"), got.State, pull, owed, passes, strings.Join(trace, "\n  "))
			}
		}
		// The next sequence's reconcile passes read none of this one's rows.
		for _, table := range []string{"pull_request_heads", "pull_request_reads_owed"} {
			if _, err := pool.Exec(ctx, "delete from "+table+" where repo = 'acme/widgets' and number = $1", number); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("sequences=%d ops=%v violations=%d stops=%v", sequences, ops, violations, s.fired)
	if violations > 0 {
		t.Errorf("%d of %d sequences end at a head or a state older than GitHub's last answer, or still owed a read", violations, sequences)
	}
	for _, point := range stopPoints {
		if s.fired[point] == 0 {
			t.Errorf("no writer stopped at %q in %d sequences: no statement of a writer matches that stop, so no interleaving there was tested", point, sequences)
		}
	}
}
