package cireconcile

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/internal/webhook"
)

// The repository every test below reconciles. The row is what a project's default repository or
// an issue's own holds, lower-cased as both are stored; gitHubOwner and gitHubName are what GitHub
// itself answers with, in its own casing, and every record and every settlement is keyed by those
// rather than the row's.
const (
	rowRepository    = "acme/widgets"
	gitHubOwner      = "Acme"
	gitHubName       = "Widgets"
	gitHubRepository = gitHubOwner + "/" + gitHubName
	repositoryID     = 4242

	pullNumber = "7"
	headSHA    = "abcdef1234567890abcdef1234567890abcdef12"

	// The review App's id, and another App's, as GitHub's own JSON carries them: a number.
	reviewerApp = 15368
	otherApp    = 98765

	webhookSecret = "cireconcile-secret"
)

// checksSubject is where the listener publishes the head's settlement: the owner and name GitHub
// gave, which is why a pass keyed from the lower-cased row would settle somewhere else.
const checksSubject = "notifications.github." + gitHubOwner + "." + gitHubName + ".pr." + pullNumber + ".checks"

// checkRunsPath and checkSuitesPath are the paths of the head's two listings, which a pass reads
// page by page and a test counts the reads of.
const (
	checkRunsPath   = "/repos/" + gitHubRepository + "/commits/" + headSHA + "/check-runs"
	checkSuitesPath = "/repos/" + gitHubRepository + "/commits/" + headSHA + "/check-suites"
)

// reviewerAppID is the review App's id as a Reconciler and the listener's webhook take it: the
// text of the number GitHub's own objects carry.
var reviewerAppID = strconv.Itoa(reviewerApp)

// harnessStart is the moment every clock of a harness starts at: the test's own, never the wall's.
var harnessStart = time.Date(2026, time.November, 5, 3, 0, 0, 0, time.UTC)

// harness is one test's whole stack: the GitHub stand-in, a Postgres of this test's own with no
// delivery_settings row, the package's shared NATS under a JetStream account of this test's own
// (testnats.URL) carrying the listener's real CI store and its real summary loop, both on a clock
// the test moves (ciClock), the listener's real webhook handler over them, and the reconcile pass.
type harness struct {
	t          *testing.T
	ctx        context.Context
	github     *fakeGitHub
	pool       *store.Pool
	bus        *bus.Client
	listener   *cistore.Store
	webhook    http.HandlerFunc
	logs       *syncBuffer
	reconciler *Reconciler
	// reviewer is the review App's id this test's listener and pass were built with, which
	// startListener needs for the webhook handler it mounts.
	reviewer string
	// verdict is the verdict App's id this test's listener was built with, "" for none:
	// startListener gives it to the webhook handler, as cmd/listener does.
	verdict string
	// issues counts the issues issueInRepository has recorded, so each one it records has a key
	// and a number of its own without its caller passing them.
	issues int
	// verify says startListener opens the listener's store with ENVOY_CI_VERIFY on
	// (cistore.WithVerify), so its summary loop publishes only after a verify pass answers.
	verify bool
	// loopLogs is what the listener's summary loop logged.
	loopLogs *syncBuffer
	// defaults counts the projects seedProjectDefault has made, so each one it makes has a key of
	// its own.
	defaults int
	// clock is the time every pass of this harness runs its bound and its waits on, and reads its
	// held answers, the verify loop's moments and the end of a limit by (Reconciler.Clock): each
	// replica's too, and the stand-in's, which stamps a limit's reset on it. It moves only when a
	// test moves it, so a pass's bound falls where the test moves it to (passCutByItsBound,
	// endBound) and never because the machine running the test is slow.
	clock *clocktest.Fake
	// ciClock is the time the listener's CI store runs its summary loop on (cistore.WithClock):
	// the loop's grid, its debounce, and the backoff of a claim that lost a compare-and-swap. The
	// reconciler's CI writer stamps its records on it too (openWriter), so a record it writes falls
	// due a debounce after a moment on this clock and on no other. It moves only while a test
	// waits for what the loop writes or publishes (driveLoop), so two events a test sends with no
	// such wait between them fall in one debounce. It is the reconciler's clock's neighbour rather
	// than the same one, so a move of a pass's clock to its bound or past a hold moves no tick of
	// the loop.
	ciClock *clocktest.Fake
	summary *summaryWitness
	// ci is the CI bucket the listener makes, opened once as startListener makes it: the test's
	// own reads and writes of it (record, recordAt, subjectRecords, whileWrittenAgain) go through
	// this one handle, where opening the bucket again for each would cost one or two JetStream
	// requests more than the read or write itself.
	ci bus.KeyValue
	// giveUp is the shared wait's context (testwait.Context), made once for the harness: the
	// listener's opens (startListener), the witness's listings (subjectRecords), the read of a
	// settlement and the end of drivenUntil's moves run within it.
	giveUp context.Context
	// natsGiveUp is the context driveLoop's flush takes (testnats.Context), made once for the
	// harness: nats.go refuses a flush whose context names no deadline, as giveUp's does not under
	// -timeout 0.
	natsGiveUp context.Context
}

// listenerDebounce and listenerTick are the summary loop's settings cmd/listener runs it with
// (ENVOY_CI_DEBOUNCE's default and the one-second tick, cmd/listener/main.go), which this harness
// runs it with too: the loop's grid and its debounce read the CI store's clock, which the test
// moves (driveLoop), so no test shortens them.
const (
	listenerDebounce = 5 * time.Second
	listenerTick     = time.Second
)

// newHarness is the whole stack with the listener already running, which every test wants but
// the one about a pass that runs before the listener has made the CI bucket.
func newHarness(t *testing.T, reviewer string) *harness {
	t.Helper()
	h := newHarnessBeforeTheListener(t, reviewer)
	h.startListener()
	return h
}

// newVerifyHarness is newHarness with the listener's ENVOY_CI_VERIFY on. GitHub answers a
// repository's installation and identity under any casing of its name, and a verify request names
// the repository as GitHub spells it, where a pass reads it under its lower-cased row;
// the stand-in answers only the names it was given, so it is given GitHub's spelling too.
func newVerifyHarness(t *testing.T, reviewer string) *harness {
	t.Helper()
	h := newHarnessBeforeTheListener(t, reviewer)
	h.verify = true
	h.github.setRepository(gitHubRepository, gitHubRepository, repositoryID)
	h.startListener()
	return h
}

// verifyPass runs one pass of the verify loop of the harness's own Reconciler (verifyPassOf).
func (h *harness) verifyPass() {
	h.t.Helper()
	h.verifyPassOf(h.reconciler)
}

// verifyPassOf runs one pass of r's verify loop (Reconciler.Verify), r the harness's own
// Reconciler or another process's (replica), and waits through the shared wait for it to return,
// as passOf waits for a reconcile pass and for the same reason.
func (h *harness) verifyPassOf(r *Reconciler) {
	h.t.Helper()
	testwait.Returns(h.t, "the verify pass to return", func() { r.Verify(h.ctx) })
}

// newHarnessBeforeTheListener is the stack with no CI bucket: NATS is up, so a pass runs, but
// nothing has made envoy_ci_state yet, which is where a Dispatch process booted ahead of the
// listener stands. startListener then makes the bucket, as the listener itself does.
func newHarnessBeforeTheListener(t *testing.T, reviewer string) *harness {
	t.Helper()
	ctx := t.Context()
	database := storetest.Open(t)
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })

	logs := &syncBuffer{}
	limits := ratelimit.New(database.Pool)
	fake := clocktest.New(harnessStart)
	github := newFakeGitHub(t, fake)
	ciClock := clocktest.New(harnessStart)
	return &harness{
		t:        t,
		ctx:      ctx,
		github:   github,
		pool:     database.Pool,
		bus:      client,
		logs:     logs,
		reviewer: reviewer,
		clock:    fake,
		ciClock:  ciClock,
		reconciler: &Reconciler{
			Pool:   database.Pool,
			GitHub: github.client(limits),
			Limits: limits,
			// The writer is bound by the first pass that finds NATS connected and the bucket
			// made, as the running pass binds it, rather than here.
			Open:          openWriter(client, ciClock),
			ReviewerAppID: reviewer,
			Logger:        slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			// A test waits for no pace.
			Pace: 0,
			// The bound, the waits and the held answers run on a clock that moves only when a
			// test moves it.
			Clock: fake,
		},
		giveUp:     testwait.Context(t),
		natsGiveUp: testnats.Context(t),
	}
}

// openWriter is the Open a Reconciler of this harness binds through: cistore's own writer, which
// creates no bucket and so fails until the listener has made one, stamping its records on
// ciClock, the listener's clock.
//
// It opens the writer once, at the first call that finds the bucket made, and answers every call
// after that one with the same writer: each Reconciler it is given to (the harness's own, and each
// replica, as each process binds its own) keeps one. The product binds afresh at every pass for a
// connection nats.go gave up on and the bus replaced (Pass), and no test here replaces the bus's
// connection, so a bind at each pass, each verify pass and each listing of the requests (requests)
// would be two JetStream requests that no test is about. A call before the bucket is made fails
// as the product's bind does and keeps nothing, so a later call opens the writer; the calls are
// the product's to make, and a test that counts them (TestNoBucketYetNoPass) counts the same.
func openWriter(client *bus.Client, ciClock *clocktest.Fake) func() (Writer, error) {
	// A pass on a goroutine of its own and a listing on the test's own can bind at once.
	var mu sync.Mutex
	var opened *cistore.Writer
	return func() (Writer, error) {
		mu.Lock()
		defer mu.Unlock()
		if opened != nil {
			return opened, nil
		}
		// The writer's clock: the moments it stamps and the retry budget it reads are ciClock's,
		// the listener's, so its records stand on the clock the summary loop judges them by, and
		// its waits - the backoff after a lost compare-and-swap, a record's RecordWait - run on
		// the real clock, as each is a few milliseconds' or seconds' pause of the pass that makes
		// it. On ciClock they would end only when driveLoop moved it, and a pass a test runs on
		// its own goroutine, with nothing driving the loop, would wait for ever on a race it lost.
		stamps := clocktest.WithNow(clock.Real(), ciClock.Now)
		writer, err := cistore.OpenWriter(client.Connection(), client.JS(), logging.New("writer"), cistore.WithClock(stamps))
		if err != nil {
			return nil, err
		}
		opened = writer
		return writer, nil
	}
}

// boundByTheTest checks the bound each pass of the harness's Reconciler puts on itself on the
// harness's clock: the test ends that bound at a held write (endBound), and checkedPassClock
// checks that the pass bound itself at its PassTimeout.
func (h *harness) boundByTheTest() {
	h.reconciler.Clock = checkedPassClock{Clock: h.clock, h: h}
}

// checkedPassClock is the Reconciler's clock with every bound a pass arms on it checked against
// the PassTimeout the test set.
type checkedPassClock struct {
	clock.Clock
	h *harness
}

func (c checkedPassClock) WithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if want := c.h.reconciler.PassTimeout; timeout != want {
		c.h.t.Errorf("the pass bound itself at %s, want its PassTimeout, %s", timeout, want)
	}
	return c.Clock.WithTimeout(ctx, timeout)
}

// endBound ends the bound of the pass running now: it moves the harness's clock to the one
// deadline pending on it, which that pass armed as it began. It fails the test where no deadline
// is pending, which is a pass that put no bound on itself, and where more than one is.
func (h *harness) endBound() {
	armed, moved := h.clock.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
		if len(armed) != 1 {
			return now, false
		}
		return armed[0], true
	})
	if !moved {
		h.t.Errorf("the test ended the pass's bound with %d deadlines pending, want the one its pass armed: %v", len(armed), armed)
	}
}

// endBoundFrom ends the bound of the pass that began at began, which waits on held, what the test
// made GitHub hold: it checks that the one deadline pending on the harness's clock is that pass's
// bound, PassTimeout from began -- the clock moves only when the test moves it, so the pass armed
// its bound there as it began -- and moves the clock to it. It fails the test where the pass armed
// its bound anywhere else, armed none, or armed another deadline beside it.
func (h *harness) endBoundFrom(began time.Time, held string) {
	h.t.Helper()
	bound := began.Add(h.reconciler.PassTimeout)
	armed, moved := h.clock.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
		if len(armed) != 1 || !armed[0].Equal(bound) {
			return now, false
		}
		return bound, true
	})
	if !moved {
		h.t.Fatalf("the pass held at %s armed %v on its clock, want its bound alone at %s, its PassTimeout (%s) from its start at %s",
			held, armed, bound.Format(time.RFC3339Nano), h.reconciler.PassTimeout, began.Format(time.RFC3339Nano))
	}
}

// holdWriteUntilTheBoundFalls makes the nth check-run write of this harness's passes, each the
// runs of one head, end the pass's own bound (boundByTheTest, endBound) and wait until the pass
// making it has run out of it. It is a CI bucket slower to answer than the pass has left, which
// is the one thing that can take a pass past its bound in the middle of a page its query
// answered whole: those heads cost no request of their own, so nothing else about them can be
// slow.
//
// The count is the harness's rather than one pass's, so the pass after the one that was held
// writes every head of its own without waiting.
func (h *harness) holdWriteUntilTheBoundFalls(nth int) {
	h.boundByTheTest()
	open := h.reconciler.Open
	writes := 0
	h.reconciler.Open = func() (Writer, error) {
		writer, err := open()
		if err != nil {
			return nil, err
		}
		return &holdingWriter{Writer: writer, hold: func(ctx context.Context) {
			if writes++; writes == nth {
				h.endBound()
				testwait.Receive(h.t, "the end of the pass's context at its bound", ctx.Done())
			}
		}}, nil
	}
}

// holdingWriter is the CI record writer with hold run before each write of a head's check runs
// (holdWriteUntilTheBoundFalls).
type holdingWriter struct {
	Writer
	hold func(ctx context.Context)
}

func (w *holdingWriter) RecordRuns(ctx context.Context, observations []contracts.CIObservation) (bool, error) {
	w.hold(ctx)
	return w.Writer.RecordRuns(ctx, observations)
}

// startListener opens the listener's own store, which makes the bucket and watches it, and its
// real summary loop, both on ciClock at cmd/listener's debounce and tick, and mounts the
// listener's real webhook handler over them: the pass's job is to make this listener settle, and
// a record alone does not prove that.
func (h *harness) startListener() {
	h.t.Helper()
	h.summary = &summaryWitness{records: map[string]scheduledRecord{}, ended: make(chan struct{})}
	listener, err := cistore.Open(h.ctx, h.bus.Conn, logging.New("listener"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour),
		cistore.WithClock(h.ciClock), cistore.WithVerify(h.verify), cistore.WithSummaryObserver(h.summary))
	if err != nil {
		h.t.Fatalf("open the listener's CI store: %v", err)
	}
	// The bucket the listener has just made, which the test's own reads and writes go through (ci).
	ci, err := bus.OpenKeyValue(h.giveUp, h.bus.Conn, h.bus.JS(), bus.CIStateBucket)
	if err != nil {
		h.t.Fatalf("open the CI bucket: %v", err)
	}
	h.ci = ci
	if err := listener.WaitForCacheReady(h.giveUp); err != nil {
		h.t.Fatalf("the listener's cache is not ready: %v", err)
	}
	loop, endLoop := context.WithCancel(h.ctx)
	h.t.Cleanup(endLoop)
	h.loopLogs = &syncBuffer{}
	cistore.StartSummaryLoop(loop, listener, h.bus, listenerDebounce, listenerTick,
		metrics.New().NewGauge("held", ""), logging.NewWithWriter("loop", h.loopLogs))
	h.listener = listener
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.verdict, h.bus, listener, webhook.NewBodyBudget(webhook.DefaultBodyBudget, nil))
}

// driveLoop moves the CI store's clock to the earliest moment armed on it when that moment is no
// later than until, and reports whether it moved. When it moved, it returns only once the summary
// loop's pass that the move set going, or set going again, has ended (summaryWitness.passEnd) and
// the bus has delivered what that pass published (a flush on the connection the loop publishes and
// the test subscribes on): so the read after a move reads what that pass left, a settlement it
// published already waits on its subscription, and a wait reads again at once (drivenUntil)
// rather than at the shared wait's pace.
//
// Each moment it can move to ends a pass. What is armed on that clock while a test drives the
// loop is the loop's own: its next tick, armed only between two passes (cistore.StartSummaryLoop
// arms it once a pass has returned), which starts the next pass; and the backoff of a claim that
// lost a compare-and-swap (cistore.Store.casState, which only the loop's claims and marks call),
// armed only inside a running pass, which the move lets go on to its end. The channel is taken
// before the move, so the end that closes it is that pass's: the next one's, or the running one's.
// A webhook the listener records arms its RecordWait and the backoff of a write it lost on the
// same clock (cistore's update and write), but only while the record is in flight, and no test
// drives the loop while one is (serve); the reconcile passes' writer waits on the real clock
// (openWriter). And a pass a move set going loses no compare-and-swap, which would leave it in a
// backoff only a later move ends: nothing else writes a record while a test drives the loop,
// since each pass, verify pass and webhook a test runs has returned before the test waits again
// (passOf, verifyPassOf, serve), and whileWrittenAgain writes only while reads that drive nothing
// run. A pass that did lose one would hold this wait to the shared wait's give-up, which names it.
//
// The reading and the move are one SetWhen, so no tick the loop arms between them is passed over
// unread.
func (h *harness) driveLoop(until time.Time) bool {
	h.t.Helper()
	ended := h.summary.passEnd()
	_, moved := h.ciClock.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
		if len(armed) == 0 || armed[0].After(until) {
			return now, false
		}
		return armed[0], true
	})
	if !moved {
		return false
	}
	testwait.Receive(h.t, "the end of the summary loop's pass at the moment its clock was moved to", ended)
	if err := h.bus.Conn.FlushWithContext(h.natsGiveUp); err != nil {
		h.t.Fatalf("flush the bus after a pass of the summary loop: %v", err)
	}
	return true
}

// loopStep is the furthest one move of driveLoop takes the loop: a debounce and a tick, which
// holds the loop's next tick and a record's falling due, and never a far later moment the loop
// arms for (a stale claim, a verify request's timeout, a handover's grace) that the settling a
// test waits for does not reach.
func (h *harness) loopStep() time.Time {
	return h.ciClock.Now().Add(listenerDebounce + listenerTick)
}

// drivenUntil waits through the shared wait until read holds, and returns what it read then, for
// a fact the summary loop brings about: each read that does not hold moves the loop's clock on to
// the next moment the loop arms within a debounce and a tick (driveLoop), and a read after a move
// that moved it is made at once, since driveLoop has waited for the pass that move set going. Only
// a read whose move found nothing armed in that span waits the shared wait's pace, for what the
// loop has not armed yet: a change the bucket's watch has still to deliver. The moves of one try
// stop where the shared wait gives up (giveUp), so a loop that arms for ever and never brings the
// fact fails the wait there.
func drivenUntil[T any](h *harness, what string, read func() (T, bool)) T {
	h.t.Helper()
	return testwait.Until(h.t, what, func() (drivenRead[T], bool) {
		for {
			value, holds := read()
			if holds {
				return drivenRead[T]{Read: value}, true
			}
			moved := h.driveLoop(h.loopStep())
			if !moved || h.giveUp.Err() != nil {
				return drivenRead[T]{Read: value, Moved: moved, Armed: h.ciClock.Armed()}, false
			}
		}
	}).Read
}

// drivenRead is one try of drivenUntil as its failure prints it: the try's last read, whether the
// move after it moved the summary loop's clock, and what was armed on that clock then.
type drivenRead[T any] struct {
	Read  T
	Moved bool
	Armed []time.Time
}

// replica is a second Reconciler standing for the other Dispatch process of a deployment: a
// GitHub client of its own over the one stand-in, and a log of its own, over the database, the
// NATS and the shared rate limits this harness's own Reconciler uses.
func (h *harness) replica() *Reconciler {
	h.t.Helper()
	limits := ratelimit.New(h.pool)
	return &Reconciler{
		Pool:          h.pool,
		GitHub:        h.github.client(limits),
		Limits:        limits,
		Open:          openWriter(h.bus, h.ciClock),
		ReviewerAppID: h.reviewer,
		Logger:        slog.New(slog.NewJSONHandler(&syncBuffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Pace:          0,
		Clock:         h.clock,
	}
}

// seedProjectDefault gives repository a project of its own whose default repository it is, as
// PUT /api/v1/projects/{key}/default-repository sets it. A project has one default repository,
// so each repository a test covers this way takes a project of its own.
func (h *harness) seedProjectDefault(repository string) {
	h.t.Helper()
	h.defaults++
	key := fmt.Sprintf("CIR%d", h.defaults)
	if _, err := h.pool.Exec(h.ctx, `
		insert into projects (key, name, default_assignee, default_repo) values ($1, 'CI reconcile', 'alice', $2)
	`, key, repository); err != nil {
		h.t.Fatalf("make %s the default repository of project %s: %v", repository, key, err)
	}
}

// seedRenamePair records that the repository called repository was renamed to the full
// `owner/name` renamedTo, as a repo_projects row with a renamed_to: that table is the one place
// Dispatch holds a rename, and a person declared it, since GitHub answers 404 for the
// installation of the name the repository was renamed away from. The harness's Reconciler then
// takes the pairs read after the seed, as a boot reads them (store.ReadRepoRenames). The pair
// covers no repository: a test that wants the old name read names it by a project's default
// repository or an issue's own, as a row the boot's rename pass missed does.
func (h *harness) seedRenamePair(repository, renamedTo string) {
	h.t.Helper()
	h.seedProject()
	if _, err := h.pool.Exec(h.ctx, `
		insert into repo_projects (repo, project, created_by, renamed_to)
		values ($1, 'CIR', '{"kind":"system","id":"test"}'::jsonb, $2)
	`, repository, renamedTo); err != nil {
		h.t.Fatalf("record that %s is called %s today: %v", repository, renamedTo, err)
	}
	pairs, err := store.ReadRepoRenames(h.ctx, h.pool)
	if err != nil {
		h.t.Fatalf("read the rename pairs: %v", err)
	}
	h.reconciler.Renames = refs.NewRenames(pairs)
}

// seedProject makes the project CIR, which holds the issues issueInRepository records and the
// rename pairs seedRenamePair records. It names no default repository, so no repository is
// covered through it. It does nothing where the project is made already.
func (h *harness) seedProject() {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, `
		insert into projects (key, name, default_assignee) values ('CIR', 'CI reconcile', 'alice') on conflict (key) do nothing
	`); err != nil {
		h.t.Fatalf("seed the project: %v", err)
	}
}

// issueInRepository records one issue naming repository as its own, as an issue write does: an
// issue's repository is any `owner/name`, and no project's default has to name it
// (api/issue_repo.go, migration 20261112004945_issue_repo_needs_no_mapping). The issue is of the
// project CIR, which names no default repository (seedProject).
func (h *harness) issueInRepository(repository string) {
	h.t.Helper()
	h.seedProject()
	h.issues++
	key := fmt.Sprintf("CIR-%d", h.issues)
	if _, err := h.pool.Exec(h.ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, repo)
		values ($1, 'CIR', $2, 'A head of its own', '{"kind":"system","id":"test"}'::jsonb, 'U', $3)
	`, key, h.issues, repository); err != nil {
		h.t.Fatalf("record %s in %s: %v", key, repository, err)
	}
}

// listHead makes the stand-in answer with one open pull request at one head, of the repository
// the row names and of the name GitHub gives it.
func (h *harness) listHead() {
	h.github.setRepository(rowRepository, gitHubRepository, repositoryID)
	h.github.setPullRequests(gitHubRepository, fakePull{number: 7, sha: headSHA})
}

// listHeads makes the stand-in answer with these open pull requests of the harness's repository,
// as listHead does for the one head most tests reconcile. The pass reads a repository's heads
// in pull request number order whatever order they are given in here, which is what a test of
// the cursor reads back out of the stand-in's own log of the requests it was sent.
func (h *harness) listHeads(pulls ...fakePull) {
	h.github.setRepository(rowRepository, gitHubRepository, repositoryID)
	h.github.setPullRequests(gitHubRepository, pulls...)
}

// queriesFor is how many GraphQL queries the stand-in was asked about one repository, however
// many pages those queries covered.
func (h *harness) queriesFor(repository string) int {
	asked := 0
	for _, query := range h.github.queriesAsked() {
		if query.repository == repository {
			asked++
		}
	}
	return asked
}

// pagesAsked is the pages of one repository the stand-in was queried for since the query at
// from, in the order it was asked: the slice of the one order a pass took.
func (h *harness) pagesAsked(from int, repository string) []int {
	var asked []int
	for _, query := range h.github.queriesAsked()[from:] {
		if query.repository == repository {
			asked = append(asked, query.page)
		}
	}
	return asked
}

// pass runs one reconcile pass of the harness's own Reconciler (passOf).
func (h *harness) pass() {
	h.t.Helper()
	h.passOf(h.reconciler)
}

// passOf runs one reconcile pass of r, the harness's own Reconciler or another process's
// (replica), and waits through the shared wait for it to return: a pass that waits on what
// nothing in the test brings, such as a request GitHub holds (fakeGitHub.hold) under a bound on
// a clock the test does not move, fails the wait naming the pass, where a pass run on the test's
// own goroutine would hang the test binary to its timeout.
func (h *harness) passOf(r *Reconciler) {
	h.t.Helper()
	testwait.Returns(h.t, "the reconcile pass to return", func() { r.Pass(h.ctx) })
}

// startPass runs one reconcile pass of the harness's own Reconciler within ctx on a goroutine of
// its own, as the loop runs it, and the channel it returns is closed once the pass has returned.
func (h *harness) startPass(ctx context.Context) <-chan struct{} {
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		h.reconciler.Pass(ctx)
	}()
	return ended
}

// passCutByItsBound runs one reconcile pass and ends it on its own bound at what the test made
// GitHub hold (fakeGitHub.hold, fakeGitHub.holdQuery), as a GitHub slower than the pass has left
// ends a pass: it waits for the stand-in to name the held request (fakeGitHub.held), ends the
// pass's bound there once it has checked that the pass armed it at its PassTimeout (endBoundFrom),
// and waits for the pass to return.
func (h *harness) passCutByItsBound() {
	h.t.Helper()
	began := h.clock.Now()
	ended := h.startPass(h.ctx)
	held := testwait.Next(h.t, "GitHub to hold a request of the pass past its bound", &h.github.held, func(string) bool { return true })
	h.endBoundFrom(began, held)
	testwait.Receive(h.t, "the pass to end on its bound at "+held, ended)
}

// subscribe listens for the head's settlements before anything can publish one.
func (h *harness) subscribe() *nats.Subscription {
	h.t.Helper()
	sub, err := h.bus.Conn.SubscribeSync(checksSubject)
	if err != nil {
		h.t.Fatalf("subscribe to %s: %v", checksSubject, err)
	}
	h.t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

// settlement is the next settlement published for the head, which must arrive. It waits on sub,
// taking a settlement once one waits there, and each try that finds none moves the summary loop's
// clock on to the next moment the loop arms within a debounce and a tick (drivenUntil), so the
// loop runs the passes that publish one.
func (h *harness) settlement(sub *nats.Subscription, why string) cistore.Summary {
	h.t.Helper()
	var message *nats.Msg
	drivenUntil(h, why+": a settlement of the head", func() (string, bool) {
		waiting, _, err := sub.Pending()
		if err != nil {
			h.t.Fatalf("%s: read the settlements waiting: %v", why, err)
		}
		if waiting > 0 {
			next, err := sub.NextMsgWithContext(h.giveUp)
			if err != nil {
				h.t.Fatalf("%s: reading the settlement published failed: %v", why, err)
			}
			message = next
			return "a settlement waiting", true
		}
		return "no settlement waiting", false
	})
	var envelope contracts.Envelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		h.t.Fatalf("%s: the settlement is not an envelope: %v", why, err)
	}
	var summary cistore.Summary
	if err := json.Unmarshal([]byte(envelope.Payload), &summary); err != nil {
		h.t.Fatalf("%s: the settlement's payload is not JSON: %v\n%s", why, err, envelope.Payload)
	}
	return summary
}

// noSettlement waits for completed summary decisions for this harness's default pull request.
func (h *harness) noSettlement(sub *nats.Subscription, why string) {
	h.t.Helper()
	h.noSettlementAt(sub, why, gitHubOwner, gitHubName, pullNumber)
}

// settledRecord is the head's record once the summary loop has finished with it. Publishing a
// settlement is itself two writes of the record -- the claim, and the mark that says it was
// emitted -- so a revision read the moment a settlement arrives still moves afterwards, and a
// test comparing revisions across a pass has to start from one that has stopped.
func (h *harness) settledRecord() (cistore.State, uint64) {
	h.t.Helper()
	return h.settledRecordAt(gitHubRepository, 7, headSHA)
}

// settledRecordAt is settledRecord for one head of any repository, keyed as recordAt keys it: the
// record once it is settled and unclaimed, and a pass of the summary loop that has completed filed
// it idle as the bucket holds it (summaryWitness, decidedOn). Idle is the loop's decision on a
// settled record, which it looks at again only when a write names the record, so from that pass
// on the loop writes the record no more, and its revision is one a test compares across a pass.
// Each read that does not find that moves the summary loop's clock on as settlement does
// (drivenUntil), so the loop's pass that marks the settlement emitted, a mark held in a
// compare-and-swap backoff, and the pass that files the marked record run.
func (h *harness) settledRecordAt(repository string, number int64, sha string) (cistore.State, uint64) {
	h.t.Helper()
	owner, name, _ := strings.Cut(repository, "/")
	key := cistore.Key(owner, name, strconv.FormatInt(number, 10), sha)
	read := drivenUntil(h, fmt.Sprintf("the record of %s#%d settled, unclaimed, and filed idle as the bucket holds it by a completed pass of the summary loop", repository, number),
		func() (settledRead, bool) {
			state, revision := h.recordAt(repository, number, sha)
			note, reported, completed := h.summary.record(key)
			read := settledRead{Revision: revision, Decision: note.kind}
			if state != nil {
				read.State = *state
			}
			settled := state != nil && state.SettledEmitted && state.Claim == nil && reported && note.kind == "idle"
			if settled {
				decided, err := decidedOn(note, completed, *state)
				if err != nil {
					h.t.Fatalf("judge the summary loop's decision on %s: %v", key, err)
				}
				settled = decided
			}
			return read, settled
		})
	return read.State, read.Revision
}

// settledRead is one read of a head's record, its revision and the summary loop's last decision
// on it, as settledRecordAt's wait reports them: a record the bucket does not hold reads as the
// zero State at revision zero, and a key the loop has not filed as no decision.
type settledRead struct {
	State    cistore.State
	Revision uint64
	Decision string
}

// record is the head's record as the bucket holds it, with its revision, and ok false when the
// bucket holds none.
func (h *harness) record() (state cistore.State, revision uint64, ok bool) {
	h.t.Helper()
	entry, err := h.ci.Get(cistore.Key(gitHubOwner, gitHubName, pullNumber, headSHA))
	if err != nil {
		return cistore.State{}, 0, false
	}
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		h.t.Fatalf("the record is not JSON: %v", err)
	}
	return state, entry.Revision(), true
}

// recordOf is the record of any head of the harness's repository, and nil when the bucket holds
// none: record answers for the one head most tests reconcile, and a test of the cursor reads
// heads the pass reached at other pages.
func (h *harness) recordOf(pull fakePull, number int64) *cistore.State {
	h.t.Helper()
	return h.stateAt(gitHubRepository, number, pull.sha)
}

// stateAt is the record of one head of any repository, keyed as the pass keys it: the owner and
// name GitHub itself gives the repository, the pull request number and the head commit. It is
// nil when the bucket holds no such record.
func (h *harness) stateAt(repository string, number int64, sha string) *cistore.State {
	h.t.Helper()
	state, _ := h.recordAt(repository, number, sha)
	return state
}

// recordAt is stateAt with the record's revision beside it, which is what a test comparing a
// record across a pass reads.
func (h *harness) recordAt(repository string, number int64, sha string) (*cistore.State, uint64) {
	h.t.Helper()
	owner, name, ok := strings.Cut(repository, "/")
	if !ok {
		h.t.Fatalf("%q is no owner/name", repository)
	}
	entry, err := h.ci.Get(cistore.Key(owner, name, strconv.FormatInt(number, 10), sha))
	if err != nil {
		return nil, 0
	}
	var state cistore.State
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		h.t.Fatalf("the record is not JSON: %v", err)
	}
	return &state, entry.Revision()
}

// post sends the listener's real webhook handler one GitHub event, as GitHub sends it, and fails
// the test unless the handler answers 200.
func (h *harness) post(event, body, delivery string) {
	h.t.Helper()
	if code, answer := h.serve(event, body, delivery); code != http.StatusOK {
		h.t.Fatalf("the webhook answered %d: %s", code, answer)
	}
}

// serve sends the listener's real webhook handler one GitHub event, as GitHub sends it, waits
// through the shared wait for its answer, and answers the status and body the handler answered.
// The listener records on ciClock, which only a test moves, so a record that waits there (the
// backoff after a lost compare-and-swap) fails naming the delivery, where a handler run on the
// test's own goroutine would hang the test binary.
func (h *harness) serve(event, body, delivery string) (int, string) {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", event)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	testwait.Returns(h.t, "the listener's webhook handler to answer the "+event+" delivery "+delivery, func() {
		h.webhook.ServeHTTP(recorder, request)
	})
	return recorder.Code, recorder.Body.String()
}

// lines is the fields of every line the pass logged under msg, oldest first.
func (h *harness) lines(msg string) []map[string]any {
	h.t.Helper()
	var found []map[string]any
	for _, text := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if text == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(text), &fields); err != nil {
			h.t.Fatalf("a log line is not JSON: %v\n%s", err, text)
		}
		if fields["msg"] == msg {
			found = append(found, fields)
		}
	}
	return found
}

// passLine is the fields of the last "ci reconcile pass" line the pass logged.
func (h *harness) passLine() map[string]any {
	h.t.Helper()
	lines := h.lines("ci reconcile pass")
	if len(lines) == 0 {
		h.t.Fatalf("no pass line was logged:\n%s", h.logs.String())
	}
	return lines[len(lines)-1]
}

// logged reports whether any line the pass logged carries text.
func (h *harness) logged(text string) bool {
	return strings.Contains(h.logs.String(), text)
}

// number is one field of the pass line, which JSON decodes as a float.
func number(t *testing.T, line map[string]any, field string) int {
	t.Helper()
	value, ok := line[field].(float64)
	if !ok {
		t.Fatalf("the pass line has no %s: %v", field, line)
	}
	return int(value)
}

// syncBuffer is a log buffer two goroutines may write, since a pass and the test read and write
// it from their own.
type syncBuffer = logtest.Buffer

// checkRun is one check run object as GitHub's listing answers it, and as its webhook carries it.
func checkRun(id int64, name, status, conclusion string, appID int, observedAt string) object {
	return object{
		"id":           id,
		"name":         name,
		"status":       status,
		"conclusion":   conclusion,
		"head_sha":     headSHA,
		"completed_at": observedAt,
		"started_at":   observedAt,
		"html_url":     fmt.Sprintf("https://github.com/%s/runs/%d", gitHubRepository, id),
		"app":          object{"id": appID},
		"check_suite":  object{"id": 900, "head_branch": "feature/x"},
	}
}

// checkSuite is one check suite object as GitHub's listing answers it.
func checkSuite(id int64, status, conclusion string, appID int, observedAt string) object {
	return object{
		"id":         id,
		"status":     status,
		"conclusion": conclusion,
		"head_sha":   headSHA,
		"app":        object{"id": appID},
		"updated_at": observedAt,
	}
}
