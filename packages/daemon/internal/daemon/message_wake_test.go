package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// priorityWakeRig runs real outbox start rows, the listener's reports and the runtime's
// observations against machines backed by Postgres, under the shipped pace with no jitter, so each
// turn is one interval after the turn before it. Its clock moves one turn at a time (run), and
// restart reads the claims the store holds into new machines in front of a fresh pacer, as a
// daemon's boot does. The tests on it are the ones dispatch://INFRA-5682 R1 and R2 name.
type priorityWakeRig struct {
	t       *testing.T
	pool    *store.Pool
	st      *store.Store
	records record.Store
	clock   *waveClock
	rt      *waveRuntime
	sup     *supervisor
	runner  *outbox
	logged  *syncBuffer
	issue   int
}

// newPriorityWakeRig is a rig over a database of its own, with the pacer's burst already spent
// (open), so every paced wake a test raises waits its turn.
func newPriorityWakeRig(t *testing.T) *priorityWakeRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	r := &priorityWakeRig{t: t, pool: pool, st: outboxTreeStore(t, pool), records: record.NewStore()}
	r.open()
	return r
}

// open builds the supervisor, its machines' dependencies and the outbox runner over the rig's
// store, with a fresh clock, runtime and pacer: what a daemon's start builds, and what restart
// builds again. It takes the fresh bucket's one burst, so the first wake a test raises waits.
func (r *priorityWakeRig) open() {
	r.clock = &waveClock{now: wakeOrigin}
	r.rt, r.logged = newWaveRuntime(r.clock), &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(r.logged, nil))
	sup := newSupervisor(r.t.Context(), r.st, "legion", r.t.TempDir(), log)
	sup.deps = supervise.Deps{
		Runtime: r.rt, Conns: fake.NewConns(), Store: r.st, Specs: outboxSpecs{}, Clock: r.clock, Log: log,
		Limits:           supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 3},
		PluginContract:   24,
		TaskTakeContract: api.TaskTakeContract(),
		RefreshGate:      supervise.NewRefreshGate(0),
		Timeouts:         supervise.Timeouts{Boot: time.Hour, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Hour, Stop: time.Second, LaunchBackoff: time.Millisecond},
		WakePace:         wakePaceOf(wakepace.New(testWakeRate, testWakeBurst, nil), r.clock),
	}
	r.sup = sup
	r.t.Cleanup(sup.stop)
	r.runner = &outbox{log: log, pool: r.pool, records: r.records, supervisor: sup, trees: r.st,
		project: "legion", dispatchProject: "LEGION", now: r.clock.Now}
	if _, out := sup.deps.WakePace.Pacer.Join(wakepace.Second).Let(); !out {
		r.t.Fatal("the fresh bucket did not let its burst out")
	}
}

// seed records the next issue of the rig, a root of its own tree in implementing, and creates its
// implementer's claim in state: released for suspended, with no process for queued and launching,
// and with a live one for every other.
func (r *priorityWakeRig) seed(state supervise.ClaimState) supervise.Claim {
	r.t.Helper()
	r.issue++
	issue := fmt.Sprintf("LEGION-%d", r.issue)
	token := mustClaimToken(r.t, issue, claim.RoleImplementer)
	putOutboxIssue(r.t, r.pool, r.records, record.Issue{Key: issue, Tree: issue, Project: "LEGION", Title: issue,
		Repo: "acme/widgets", GitHubRepo: "acme/widgets", DefaultBranch: "main", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "U"})
	if _, err := r.st.OpenTreeLifecycle(r.t.Context(), "legion", issue, "workflow"); err != nil {
		r.t.Fatal(err)
	}
	c := liveClaim(r.issue, token, r.sup.deps.PluginContract)
	c.Token, c.Project, c.Tree, c.Issue, c.Role = token, "legion", issue, issue, claim.RoleImplementer
	c.State, c.ServingGeneration = state, 1
	if state == supervise.StateSuspended {
		c.Locator, c.SuspendedBy = nil, supervise.SuspendedByReleased
	} else if state == supervise.StateQueued || state == supervise.StateLaunching {
		c.Locator = nil
	}
	if _, _, err := r.sup.Create(r.t.Context(), c, ""); err != nil {
		r.t.Fatal(err)
	}
	return c
}

// start seeds a released implementer and runs the start row a wake of kind queues for it
// (runStart), which resumes it by rule: a paced launch that waits in the queue its class names.
func (r *priorityWakeRig) start(kind string) supervise.Claim {
	r.t.Helper()
	c := r.seed(supervise.StateSuspended)
	r.runStart(c, kind)
	return c
}

// runStart builds the start row a wake of kind queues for c (workflow's wakeRow) and runs it, and
// returns the row for a test that runs it again. The row is created at the rig's clock, as the
// wake's own transaction creates it (record.OutboxRow.CreatedAt), which is the moment the executor
// marks a message wake at, and its id is the number of c's issue.
func (r *priorityWakeRig) runStart(c supervise.Claim, kind string) record.OutboxRow {
	r.t.Helper()
	var id int64
	if _, err := fmt.Sscanf(c.Issue, "LEGION-%d", &id); err != nil {
		r.t.Fatalf("read the number of issue %s: %v", c.Issue, err)
	}
	row := mustOutboxRow(r.t, c.Issue, record.SuperviseRequest{Op: "start", Tree: c.Tree, Role: c.Role,
		Generation: 1, WakeKind: kind}, r.clock.Now())
	row.ID, row.CreatedAt = id, r.clock.Now()
	if err := r.runner.execute(r.t.Context(), row); err != nil {
		r.t.Fatalf("execute %s start of %s: %v", kind, c.Token, err)
	}
	return row
}

// wave is n such starts, in order, and the claims they resumed.
func (r *priorityWakeRig) wave(n int, kind string) []claim.Token {
	r.t.Helper()
	tokens := make([]claim.Token, 0, n)
	for range n {
		tokens = append(tokens, r.start(kind).Token)
	}
	return tokens
}

// report applies the Envoy listener's wake report of kind for c through the workflow engine: a
// role_message report names c's role token, an agent_message report the session c records. It
// returns once c's machine has taken every event the report posted.
func (r *priorityWakeRig) report(c supervise.Claim, kind string) {
	r.t.Helper()
	pods, err := r.st.PodRecordsOf("LEGION")
	if err != nil {
		r.t.Fatal(err)
	}
	w := &workflowRuntime{}
	w.outboxWake.Store(r.runner)
	engine := workflow.New(r.records, workflow.Config{Project: "LEGION", Pods: pods, MessageMarked: w.messageMarked, Clock: r.clock.Now}, quietLogger())
	session, role := "", string(c.Token)
	if kind == intake.ListenerWakeAgent {
		session, role = c.Session, ""
	}
	if _, err := intake.ApplyFact(r.t.Context(), r.pool, "listener", "report-wake", intake.ListenerWake{
		Kind: kind, Session: session, Role: role, At: r.clock.Now(), Report: "report-wake",
	}, engine); err != nil {
		r.t.Fatal(err)
	}
	// A later event in the same machine inbox gives us an ordering fence, even if the report
	// was ignored. Do not wait on a mark itself: the FIFO control must fail on launch order.
	// The wait ends at the test binary's deadline, not at a span of its own, so the load of the
	// tests beside it cannot fail it (docs/testing.md, testwait.Receive).
	fence := make(chan struct{})
	r.sup.drained(c.Token, fence)
	testwait.Receive(r.t, "supervision to drain the listener report", fence)
}

// observe tells c's machine that the runtime found its process gone: displaced by a pod that was
// replaced under it, which charges no death, or dead of its own, here for memory, which charges
// one.
func (r *priorityWakeRig) observe(c supervise.Claim, displaced bool) {
	r.t.Helper()
	m, _ := r.sup.Machine(c.Token)
	if err := m.Handle(r.t.Context(), supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *c.Locator, Kind: runtime.Gone, At: r.clock.Now(), Displaced: displaced, OutOfMemory: !displaced,
	}}); err != nil {
		r.t.Fatal(err)
	}
}

// run moves the clock turn by turn until the runtime has made n launches, and returns them in the
// order they were made, each with the moment it was made at.
func (r *priorityWakeRig) run(n int) []waveLaunch {
	r.t.Helper()
	for i := 0; len(r.rt.launches()) < n && i < n+1; i++ {
		if !r.clock.fireNext() {
			break
		}
	}
	got := r.rt.launches()
	if len(got) != n {
		r.t.Fatalf("got %d launches, want %d: %v", len(got), n, got)
	}
	return got
}

// wantWakeOrder is launches made for the claims of want, in that order.
func wantWakeOrder(t *testing.T, launches []waveLaunch, want []claim.Token) {
	t.Helper()
	got := make([]claim.Token, 0, len(launches))
	for _, launch := range launches {
		got = append(got, launch.claim)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("launch order = %v; want %v", got, want)
	}
}

// R1: with 20 rule resumes paced, a start row whose wake carried a role message runs at the
// earliest turn still held, and the 20 run after it in their order. With one queue the message
// wake runs last, 21 turns out, which is how legion-infra-infra-4857-architect's wake waited an
// hour on 2026-11-23 while Envoy dropped the messages it was woken for.
func TestAMessageWakeGoesAheadOfTheRuleResumesItFindsQueued(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	other := r.wave(20, pods.WakeChecks)
	message := r.start(pods.WakeRoleMessage).Token
	launches := r.run(21)
	wantWakeOrder(t, launches, append([]claim.Token{message}, other...))
	if at := launches[0].at; !at.Equal(wakeOrigin.Add(testWakeInterval)) {
		t.Fatalf("message ran at %s; want the earliest held turn", at)
	}
}

// R1: a rule resume that already waits in the pacing goes into the message queue when the
// listener reports an agent message for its claim. The claim is not released, since its launch
// is under way, so the report wakes nothing: the mark alone moves the wake.
func TestAListenerReportMarksAWaitingWake(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	other := r.wave(3, pods.WakeChecks)
	waiting := r.start(pods.WakeChecks)
	r.report(waiting, intake.ListenerWakeAgent)
	wantWakeOrder(t, r.run(4), append([]claim.Token{waiting.Token}, other...))
}

// R1: the message wakes run first, in their order, and the turn after each three of them runs a
// rule resume, so the other queue keeps one turn in four while both hold wakes (share). Nothing
// is owed to the other queue for the turns it was empty: when it fills after ten message turns it
// gets one turn, and then three message turns run (refill). No two wakes run in one turn, so each
// of the 28 runs at least one interval after the run before it (interval).
func TestMessageWakesKeepTheirOrderTheShareAndTheRate(t *testing.T) {
	t.Parallel()
	t.Run("share", func(t *testing.T) {
		r := newPriorityWakeRig(t)
		other := r.wave(20, pods.WakeChecks)
		message := r.wave(8, pods.WakeRoleMessage)
		want := slices.Concat(message[:3], other[:1], message[3:6], other[1:2], message[6:], other[2:])
		wantWakeOrder(t, r.run(28), want)
	})
	t.Run("refill", func(t *testing.T) {
		r := newPriorityWakeRig(t)
		message := r.wave(13, pods.WakeAgentMessage)
		wantWakeOrder(t, r.run(10), message[:10])
		other := r.wave(3, pods.WakeChecks)
		want := slices.Concat(message[:10], other[:1], message[10:], other[1:])
		wantWakeOrder(t, r.run(16), want)
	})
	t.Run("interval", func(t *testing.T) {
		r := newPriorityWakeRig(t)
		r.wave(20, pods.WakeChecks)
		r.wave(8, pods.WakeRoleMessage)
		launches := r.run(28)
		for i := 1; i < len(launches); i++ {
			if gap := launches[i].at.Sub(launches[i-1].at); gap < testWakeInterval {
				t.Fatalf("launches %s and %s were %s apart; want at least %s", launches[i-1].claim, launches[i].claim, gap, testWakeInterval)
			}
		}
	})
}

// restart stops the rig's supervisor and starts a new one over the same database, as a daemon's
// restart does, and runs the boot's pass over the launches the first left unfinished. older's
// work is owed an hour before priority's, so the owed order alone would launch older first: the
// pass must place priority in the first queue, where it runs first and says so in its wake line.
func (r *priorityWakeRig) restart(older, priority claim.Token) {
	r.t.Helper()
	r.sup.stop()
	// Boot sorts owed work within each queue. Write the competing tasks' owed times explicitly
	// so equal times or the order the SQL read returns cannot make the priority claim win.
	if err := r.st.PutDelivery(r.t.Context(), older, supervise.Delivery{ID: "older", Task: "Older work", QueuedAt: wakeOrigin.Add(-time.Hour)}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.PutDelivery(r.t.Context(), priority, supervise.Delivery{ID: "later", Task: "Later work", QueuedAt: wakeOrigin.Add(-time.Minute)}); err != nil {
		r.t.Fatal(err)
	}
	r.st = outboxTreeStore(r.t, r.pool)
	claims, err := r.st.Claims(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	r.open()
	unfinished, err := r.sup.restore(r.t.Context(), claims)
	if err != nil {
		r.t.Fatal(err)
	}
	s := supervision{supervisor: r.sup, log: quietLogger()}
	if refused := s.launchUnfinished(r.t.Context(), waitingLaunches(unfinished), recordedDeaths(claims)...); len(refused) != 0 {
		r.t.Fatalf("boot relaunches refused: %v", refused)
	}
	wantWakeOrder(r.t, r.run(2), []claim.Token{priority, older})
	if line, ok := wakeLineOf(r.t, r.logged, priority); !ok || line["queue"] != "message" {
		r.t.Fatalf("the restart's first launch wrote %v; want the first queue", line)
	}
}

// R1: a role whose open resume's wake carried a message, and whose work is owed later than the
// other's, is placed in the message queue by the boot's pass. The mark the start row's executor
// stored on the claim is what the pass reads.
func TestARestartKeepsTheMessageMark(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	older := r.start(pods.WakeChecks)
	// The wake record and start are separate: seed a released role's open resume before the
	// start runs, without a listener report that would also write the mark under test.
	message := r.seed(supervise.StateSuspended)
	if err := store.Transact(t.Context(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := store.RecordPodWakeTx(ctx, tx, store.PodResume{Project: "legion", Issue: message.Issue,
			Role: string(message.Role), Kind: pods.WakeRoleMessage, At: r.clock.Now()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.runStart(message, pods.WakeRoleMessage)
	r.restart(older.Token, message.Token)
}

// R1: a claim that was not released, whose launch the previous daemon left unfinished, was marked
// by a listener's report before the restart. The boot's pass places its relaunch in the message
// queue, though its work is owed later than the other's. A mark kept in the machine's memory alone
// would be lost here.
func TestARestartKeepsAReportedMark(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	older := r.start(pods.WakeChecks)
	unfinished := r.seed(supervise.StateLaunching)
	r.report(unfinished, intake.ListenerWakeRole)
	r.restart(older.Token, unfinished.Token)
}

// R1: a start row whose wake carried a role message marks its claim at the row's own creation,
// the moment the wake was raised, so the same row run again after the role has taken a turn,
// with no message since, writes a mark that does not stand: that turn read the message. The
// claim's next wake, a displaced relaunch here, then waits in the other queue, behind the wakes
// queued before it. Negative control: marked at the moment the row runs (the executor's
// r.now() in place of row.CreatedAt), the retry's mark is later than the turn and stands, in
// the store and in the machine, and the relaunch takes the message queue and runs first, so
// this test fails.
func TestARetriedMessageStartAfterATurnMarksNothingThatStands(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	c := r.seed(supervise.StateIdle)
	row := r.runStart(c, pods.WakeRoleMessage)
	m, _ := r.sup.Machine(c.Token)
	if got := m.Claim(); !got.MessageMarked() {
		t.Fatalf("the start left %s marked at %s; want a mark that stands before the role's turn", c.Token, got.MessageMarkAt)
	}
	// Each step is a second, short of the interval, so the bucket earns no token and every wake
	// raised below waits its turn.
	r.clock.advance(time.Second)
	if err := m.Handle(t.Context(), supervise.StreamTurnStart{Claim: c.Token}); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(time.Second)
	if err := r.runner.execute(t.Context(), row); err != nil {
		t.Fatalf("run the start row again: %v", err)
	}
	claims, err := r.st.Claims(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]supervise.Claim{"in the machine": m.Claim()}
	for _, stored := range claims {
		if stored.Token == c.Token {
			held["in the store"] = stored
		}
	}
	if len(held) != 2 {
		t.Fatalf("the store holds no claim %s", c.Token)
	}
	for where, got := range held {
		if got.MessageMarked() {
			t.Fatalf("%s, the retried start left %s marked at %s after its turn at %s; want no mark that stands",
				where, c.Token, got.MessageMarkAt, got.LastTurnStart)
		}
	}
	other := r.wave(3, pods.WakeChecks)
	r.observe(c, true)
	wantWakeOrder(t, r.run(4), append(other, c.Token))
}

// R2: with 20 displaced relaunches paced, the relaunch of a role whose own process died runs at
// the earliest turn still held, and the 20 run after it in their order, each from the other queue:
// a displaced relaunch charges no death and takes no place in the first queue.
func TestADeathRelaunchGoesAheadOfDisplacedRelaunches(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	var displaced []claim.Token
	for range 20 {
		c := r.seed(supervise.StateWorking)
		r.observe(c, true)
		displaced = append(displaced, c.Token)
	}
	dead := r.seed(supervise.StateWorking)
	r.observe(dead, false)
	launches := r.run(21)
	wantWakeOrder(t, launches, append([]claim.Token{dead.Token}, displaced...))
	if !launches[0].at.Equal(wakeOrigin.Add(testWakeInterval)) {
		t.Fatalf("charged relaunch ran at %s; want the earliest held turn", launches[0].at)
	}
	for _, token := range displaced {
		if line, ok := wakeLineOf(t, r.logged, token); !ok || line["queue"] != "other" {
			t.Fatalf("displaced relaunch of %s wrote %v; want the other queue", token, line)
		}
	}
}

// R2: the relaunch after a charged death keeps its place in the first queue across a restart,
// where the boot finds it by the death the claim records.
func TestARestartKeepsTheDeathRelaunchInTheFirstQueue(t *testing.T) {
	t.Parallel()
	r := newPriorityWakeRig(t)
	older := r.start(pods.WakeChecks)
	dead := r.seed(supervise.StateWorking)
	r.observe(dead, false)
	r.restart(older.Token, dead.Token)
}
