package daemon

// What a database that has stopped answering costs one claim, and what it must not cost the rest
// of the daemon (INFRA-1132). The parts only a real daemon over a real store can show are here:
// the claim-write deadline measured on a decision of a supervised machine, the other claims and
// the readers that go on while one claim's write waits it out, the mark's loss with the process
// that held it, and the owed report arriving at the daemon's own durable-state path rather than
// at the machine's callback. supervise's own tests hold the rule itself.
//
// Each rig is a supervisor of this package's own kind — a real store over a database of the
// test's own (isolatedOutboxPool), claims seeded and restored as a boot restores them — whose
// deps carry a store a test can take away and put back (switchStore). The store is switchable
// rather than rebuilt because supervise.NewMachine copies the Deps it is given: a machine reads
// the store it was built with for the whole of its life, so taking the database away under a
// live claim means handing that one store another target.
//
// That same copying is why Timeouts.ClaimWrite is one value for every machine of a supervision,
// and why what a row names there bounds the faults alone. The real store is handed out through
// detachedWrites, which takes that deadline off every write that reaches the database, so a
// figure a row names is measured against the store that withholds its answer and against nothing
// else: a healthy write to a real database is not that store, and is never cut short by a bound
// chosen to be short enough for a test to wait out.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	owedProject = "legion"
	owedIssue   = "LEGION-1132"
	// owedPod is the pod every role of the issue runs in, as a sandbox locator records it. Each
	// seeded claim records a process: a live claim restored with none is taken up as
	// launch_uncertain (supervisor.restore), which is not the claim these tests are about.
	owedPod = "0f2d4a6b-8c1e-4f33-9a57-1d2b3c4e5f60"
	// owedWriteFailedLine and owedWriteMadeLine are the two lines supervise writes about a claim
	// write the database did not take: the Warn where the write failed or a repair of it failed
	// again, and the Info where the repair made it. They are read here as an operator reads them,
	// because the mark itself is memory internal/daemon cannot see.
	owedWriteFailedLine = "supervise: a claim write the database did not take"
	owedWriteMadeLine   = "supervise: a claim write the database did not take was made"
)

// unmeasuredClaimWrite is Timeouts.ClaimWrite for the rows that put no withholding store in the
// way of the write they drive. Such a row measures nothing against it, because every write that
// reaches the database runs with that deadline off it (detachedWrites), so nothing in those
// rows waits this out. It is named rather than written as a figure at each row so that no reader
// takes it for a bound something there is waiting out.
const unmeasuredClaimWrite = 250 * time.Millisecond

// loggedMessage is how one of those lines is found in a machine's log: slog's message field,
// whole. The made line begins with the failed one, so a search for the failed line's text alone
// finds the made line too.
func loggedMessage(line string) string { return `"msg":"` + line + `"` }

// owedRole is one claim a rig seeds: the role, and the state the store holds it in.
type owedRole struct {
	role  claim.Role
	state supervise.ClaimState
}

// switchStore is the store a rig puts in a supervisor's deps: every call goes to whatever target
// is in place at the moment of the call, so a test takes the database away under a live claim and
// puts it back without rebuilding a machine.
type switchStore struct {
	mu     sync.Mutex
	target supervise.Store
}

func (s *switchStore) use(target supervise.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = target
}

func (s *switchStore) now() supervise.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}

func (s *switchStore) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	return s.now().AdmitClaim(ctx, c)
}

func (s *switchStore) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	return s.now().CheckLaunch(ctx, c)
}

func (s *switchStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	return s.now().PutClaim(ctx, c)
}

func (s *switchStore) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	return s.now().PutDelivery(ctx, token, d)
}

func (s *switchStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	return s.now().PutClaimAndDelivery(ctx, c, d)
}

func (s *switchStore) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	return s.now().RetireDelivery(ctx, c, deliveryID)
}

func (s *switchStore) PutClaimStep(ctx context.Context, token claim.Token, step supervise.StepRecord) (bool, error) {
	return s.now().PutClaimStep(ctx, token, step)
}

// detachedWrites is the real store as the rig hands it out: every call Machine.write bounds runs
// with the claim-write deadline off it. The bound a row names is the bound of a store that
// withholds its answer, and a healthy write to a real database is not that store. One value
// cannot do both jobs: supervise.NewMachine copies its Deps, so every machine of a supervision
// takes the same Timeouts.ClaimWrite and no row can change it for one write and not another. The
// two jobs are therefore separated by store target rather than by row — the faults keep the
// short bound, and what reaches the database runs under no deadline of the rig's.
//
// What the bound cost a healthy write, measured in this pod on 2026-11-10: 1,000 iterations of
// TestAnOwedWriteAcrossARestartIsLostAndTheStoredClaimIsTakenUp under four busy loops on two
// CPUs with -race ran the bounded real PutClaim at min 4.8 ms, p50 10.9 ms, p90 33.0 ms, p99
// 53.3 ms, p99.9 166.8 ms, max 271.9 ms. The one write that went over 250 ms failed its row with
// "put claim legion-legion-legion-1132-architect: timeout: context deadline exceeded", which is
// the incident's own message (merge-group run 42728062336, test job 129538832017).
//
// AdmitClaim and CheckLaunch are not overridden: they carry no deadline, by Machine.write's own
// decision. Neither is PutClaimStep, which Machine.putStep calls on the store directly and so
// never passes through Machine.write at all. The embedded store serves all three.
type detachedWrites struct{ supervise.Store }

func (s detachedWrites) PutClaim(ctx context.Context, c supervise.Claim) error {
	return withoutTheClaimWriteDeadline(ctx, func(ctx context.Context) error { return s.Store.PutClaim(ctx, c) })
}

func (s detachedWrites) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	return withoutTheClaimWriteDeadline(ctx, func(ctx context.Context) error { return s.Store.PutDelivery(ctx, token, d) })
}

func (s detachedWrites) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	return withoutTheClaimWriteDeadline(ctx, func(ctx context.Context) error { return s.Store.PutClaimAndDelivery(ctx, c, d) })
}

func (s detachedWrites) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	return withoutTheClaimWriteDeadline(ctx, func(ctx context.Context) error { return s.Store.RetireDelivery(ctx, c, deliveryID) })
}

// withoutTheClaimWriteDeadline runs call on a context that keeps the values of ctx and drops
// every deadline ctx carries. In these rows the only one is Timeouts.ClaimWrite, which
// Machine.write installs, and not Deps.Clock, which this file also sets; a deadline of a parent
// would go with it, and the guard below would not cancel on that one either.
// context.WithTimeout is no alternative, whatever value it were given: it takes the earlier of
// parent and child, so nothing lengthens a deadline the machine has already installed. The
// deadline has to be left behind, and the cancellation picked up again, which is what the
// AfterFunc does: a cancellation of the decision's context would still end the write under it at
// once, as it did before the detach.
//
// No row here makes that cancellation. The rig's context is context.Background() and its stop
// cancels nothing, so the only thing that ends a write's context in these rows is the machine's
// own deadline, and the AfterFunc's cancel branch is never reached. The guard on
// context.DeadlineExceeded is kept all the same: without it the AfterFunc would fire on that
// deadline too and cancel the write, which is the whole of what this function takes off, and a
// detach that dropped cancellation altogether would be a hole rather than a re-armed one.
//
// Its residue, for whoever does cancel: once a write has run past the claim-write deadline, a
// cancellation of the parent no longer reaches it, because the AfterFunc has already run.
func withoutTheClaimWriteDeadline(ctx context.Context, call func(context.Context) error) error {
	writing, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		if ctx.Err() != context.DeadlineExceeded {
			cancel()
		}
	})
	defer stop()
	return call(writing)
}

// claimWriteFault is a fault on every claim write of one claim: the writes of that token answer
// fault, and everything else goes to the store behind it. One claim's writes alone are touched,
// so what another claim's machine does meanwhile is the real store's own answer rather than a
// second fault.
type claimWriteFault struct {
	supervise.Store
	token claim.Token
	fault func(context.Context) error
}

func (s *claimWriteFault) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Token == s.token {
		return s.fault(ctx)
	}
	return s.Store.PutClaim(ctx, c)
}

func (s *claimWriteFault) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	if c.Token == s.token {
		return s.fault(ctx)
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s *claimWriteFault) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	if c.Token == s.token {
		return s.fault(ctx)
	}
	return s.Store.RetireDelivery(ctx, c, deliveryID)
}

// refusing is the fault of a server that answers the write with err.
func refusing(behind supervise.Store, token claim.Token, err error) *claimWriteFault {
	return &claimWriteFault{Store: behind, token: token, fault: func(context.Context) error { return err }}
}

// blocked is the fault of a database that has stopped answering: the write goes to a pool pointed
// at a listener this test opens and never accepts from, so the kernel takes the connection and no
// server ever answers the startup message. The write returns only when something ends its wait,
// which is the whole point — what ends it is Timeouts.ClaimWrite, through the context the machine
// derives for the write.
//
// pgxpool.New dials nothing, so the pool is built at once and the first statement is where the
// wait begins.
func blocked(t *testing.T, behind supervise.Store, token claim.Token) *claimWriteFault {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the database that never answers: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf("postgres://legion@%s/legion?sslmode=disable", listener.Addr().String()))
	if err != nil {
		t.Fatalf("open a pool on the database that never answers: %v", err)
	}
	t.Cleanup(pool.Close)
	return &claimWriteFault{Store: behind, token: token, fault: func(ctx context.Context) error {
		_, err := pool.Exec(ctx, "select 1")
		return err
	}}
}

// brokenConnection is the server saying the connection under the statement failed: class 08,
// which is an answer about the connection and not about the statement, and so one the rule owes
// (supervise's writeOwed). It is what a claim write meets when the database goes away under it.
func brokenConnection() *pgconn.PgError {
	return &pgconn.PgError{Code: "08006", Message: "server closed the connection unexpectedly", Severity: "FATAL"}
}

// slowClaimWrite is a database that is merely slow rather than away: one token's claim writes
// wait by in the server before the store behind takes them, and every other call goes straight
// through. The three it waits on are the three claimWriteFault answers for its token, so which
// of them a decision makes decides nothing here. The wait is a real statement (pg_sleep) on the
// write's own context rather than a timer beside it, so what the deadline meets is what it met
// in the incident: a server that has taken a statement and not answered yet. A deadline that
// fires there is pgconn's own timeout, so the row reads the incident's "timeout: context
// deadline exceeded" rather than the bare "context deadline exceeded" that a context already
// expired before the call gives. The "put claim <token>: " the incident's line carries as well
// is store.Store.PutClaim's own wrap, which this error does not pass through.
//
// It stands behind a detach, nearest the database, because what it stands for is the database
// call itself; in front of the detach it would run on the machine's own claim-write deadline and
// be cut short whether or not the detach is there, which would prove nothing about either. A
// cancellation of the write's context still ends the wait, because the statement runs on that
// context.
type slowClaimWrite struct {
	supervise.Store
	// pool is the test's own connection to the same database, which is what makes the wait a
	// statement of the server's rather than a timer of the test's.
	pool  *store.Pool
	token claim.Token
	by    time.Duration
}

// wait is the server holding this write, for the token's writes alone.
func (s slowClaimWrite) wait(ctx context.Context, token claim.Token) error {
	if token != s.token {
		return nil
	}
	_, err := s.pool.Exec(ctx, "select pg_sleep($1)", s.by.Seconds())
	return err
}

func (s slowClaimWrite) PutClaim(ctx context.Context, c supervise.Claim) error {
	if err := s.wait(ctx, c.Token); err != nil {
		return err
	}
	return s.Store.PutClaim(ctx, c)
}

func (s slowClaimWrite) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	if err := s.wait(ctx, c.Token); err != nil {
		return err
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s slowClaimWrite) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	if err := s.wait(ctx, c.Token); err != nil {
		return err
	}
	return s.Store.RetireDelivery(ctx, c, deliveryID)
}

// owedWriteRig is one supervision over a real store: the claims seeded in a database of the
// test's own, restored as a boot restores them, each machine's events driven by hand.
type owedWriteRig struct {
	t   *testing.T
	ctx context.Context
	// pool is the test's own connection to the same database, for reading what the store holds
	// without asking the supervision.
	pool  *store.Pool
	st    *store.Store
	sup   *supervisor
	store *switchStore
	// real is the real store as the rig hands it out, with the claim-write deadline off its writes.
	real   supervise.Store
	tokens map[claim.Role]claim.Token
	// logs is what the machines wrote, which is where a write the database did not take is
	// reported: the mark itself is supervise's own memory, and the log line is what internal/daemon
	// and an operator read it by.
	logs  *syncBuffer
	write time.Duration
}

// newOwedWriteRig seeds each role's claim in a database of its own and supervises them, with
// write as every machine's claim-write bound.
func newOwedWriteRig(t *testing.T, write time.Duration, roles ...owedRole) *owedWriteRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	tokens := map[claim.Role]claim.Token{}
	claims := make([]supervise.Claim, 0, len(roles))
	for _, role := range roles {
		token := mustClaimToken(t, owedIssue, role.role)
		tokens[role.role] = token
		claims = append(claims, supervise.Claim{
			Token: token, Project: owedProject, Tree: owedIssue, Issue: owedIssue, Role: role.role,
			Generation: 1, State: role.state, PluginContract: api.DaemonAPIVersion,
			Session: "ses_" + string(role.role), SessionFile: "/sessions/" + string(role.role) + ".jsonl",
			Locator: podUpdateLocator(token, owedIssue, owedPod),
		})
	}
	seedWorkflowTree(t, dsn, owedProject, record.Issue{
		Key: owedIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: owedIssue, Title: "The claim write the database did not take",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, claims...)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	rig := &owedWriteRig{t: t, ctx: ctx, pool: pool, st: st, tokens: tokens, write: write}
	rig.real = detachedWrites{Store: st}
	rig.boot()
	return rig
}

// boot builds the supervision over the store the rig holds and takes up every claim the store
// holds now, which is what a daemon's boot does. It is called again by the restart test, so a
// second supervision is this one and not a test's own arrangement; the rig's handles read the
// newest from here on.
func (r *owedWriteRig) boot() {
	r.t.Helper()
	logs := &syncBuffer{}
	switcher := &switchStore{target: r.real}
	sup := newSupervisor(r.ctx, r.st, owedProject, r.t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: fake.NewRuntime(), Conns: fake.NewConns(), Store: switcher, Specs: outboxSpecs{},
		Clock: stillClock{}, Log: slog.New(slog.NewJSONHandler(logs, nil)),
		Limits: supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second,
			Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond, ClaimWrite: r.write},
		PluginContract: api.DaemonAPIVersion,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	r.t.Cleanup(sup.stop)
	stored, err := sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	r.sup, r.store, r.logs = sup, switcher, logs
}

func (r *owedWriteRig) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, ok := r.tokens[role]
	if !ok {
		r.t.Fatalf("no claim was seeded for the %s", role)
	}
	return token
}

func (r *owedWriteRig) machine(role claim.Role) *supervise.Machine {
	r.t.Helper()
	m, ok := r.sup.Machine(r.token(role))
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	return m
}

// handle drives one event into a role's machine and returns what the decision answered, as the
// daemon's own sources reach a machine (supervisor.add's feeding).
func (r *owedWriteRig) handle(role claim.Role, ev supervise.Event) error {
	r.t.Helper()
	return r.machine(role).Handle(r.ctx, ev)
}

// turnStart is the agent's own turn start: the decision the measured incident lost, and one whose
// state stands in memory whether or not the row takes it.
func (r *owedWriteRig) turnStart(role claim.Role) supervise.Event {
	r.t.Helper()
	return supervise.StreamTurnStart{Claim: r.token(role)}
}

// nudge is an event that drives a decision and writes nothing of its own: a timer the claim
// never armed, which the fence drops before the table. The repair at the head of Handle still
// runs, so what a test measures around it is the owed write alone.
func (r *owedWriteRig) nudge(role claim.Role) supervise.Event {
	r.t.Helper()
	return supervise.Timer{Claim: r.token(role), Kind: supervise.TimerFinish, Generation: r.machine(role).Claim().Generation, Seq: 1 << 62}
}

// held is the claim as its machine holds it.
func (r *owedWriteRig) held(role claim.Role) supervise.ClaimState {
	r.t.Helper()
	return r.machine(role).Claim().State
}

// stored is the claim's state as the database holds it, read on the test's own connection
// (storedClaimState).
func (r *owedWriteRig) stored(role claim.Role) supervise.ClaimState {
	r.t.Helper()
	return storedClaimState(r.t, r.pool, r.token(role))
}

// refuse puts a store in place that answers role's claim writes with err.
func (r *owedWriteRig) refuse(role claim.Role, err error) {
	r.t.Helper()
	r.store.use(refusing(r.real, r.token(role), err))
}

// block puts a store in place whose claim writes for role never answer.
func (r *owedWriteRig) block(role claim.Role) {
	r.t.Helper()
	r.store.use(blocked(r.t, r.real, r.token(role)))
}

// answers puts the real store back: the database is reachable again.
func (r *owedWriteRig) answers() {
	r.t.Helper()
	r.store.use(r.real)
}

// slow puts a database that is merely slow in place of the real one: role's claim writes wait by
// in the server, and the real store takes them after that. The slow store is composed under a
// detach of its own, so it stands where the real store stands — behind the detach, nearest the
// database — and never in front of it.
func (r *owedWriteRig) slow(role claim.Role, by time.Duration) {
	r.t.Helper()
	r.store.use(detachedWrites{Store: slowClaimWrite{Store: r.st, pool: r.pool, token: r.token(role), by: by}})
}

// owedTurnStart is the measured incident over a real store: the agent's turn started, the claim
// write failed, and the claim stands working in memory over a row that still reads what it was.
func (r *owedWriteRig) owedTurnStart(role claim.Role) {
	r.t.Helper()
	before := r.stored(role)
	if err := r.handle(role, r.turnStart(role)); err == nil {
		r.t.Fatalf("the %s's turn start, whose claim write the store refused, returned no error", role)
	}
	if got := r.held(role); got != supervise.StateWorking {
		r.t.Fatalf("the %s stands %s after its turn started, want working: nothing is put back", role, got)
	}
	if got := r.stored(role); got != before {
		r.t.Fatalf("the %s's row reads %s, want the %s the refused write left", role, got, before)
	}
}

// The claim-write deadline is what ends a decision's wait on a database that has stopped
// answering. With a claim's write already owed and its store blocked — a pool on a listener that
// never answers — the next decision's head is the repair, and the repair is the write: the
// decision returns at the bound and not at the length of the outage, which has no end of its own
// here. The write it could not make is still owed, because a write a deadline cut short is no
// server's refusal, and it lands at the first decision after the database answers again.
//
// Without the bound the machine's lock is held for as long as the connection's own patience,
// which is what every other claim's event and every read of this claim then waits for
// (TestNoDecisionWaitsOnTheClaimWhoseWriteTheDatabaseHasNotTaken).
func TestTheRepairDeadlineEndsADecisionsWaitOnADatabaseThatHasStoppedAnswering(t *testing.T) {
	// The bound this row measures, and the bound that catches a wait which is not bounded at all:
	// the blocked store never answers, so an unbounded decision returns at no time this test
	// could name. What claimWrite bounds is the write put to that blocked store and nothing
	// else — what reaches the real database runs with that deadline off it (detachedWrites) —
	// so it is free to be short enough for a test to wait out.
	const claimWrite = 250 * time.Millisecond
	const unbounded = 10 * time.Second
	rig := newOwedWriteRig(t, claimWrite, owedRole{claim.RoleArchitect, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)

	rig.block(claim.RoleArchitect)
	rig.logs.Reset()
	began := time.Now()
	err := rig.handle(claim.RoleArchitect, rig.nudge(claim.RoleArchitect))
	elapsed := time.Since(began)

	if err != nil {
		t.Fatalf("the decision under the blocked store = %v, want the dropped event's own nil: a repair that fails must not drop the event behind it", err)
	}
	if elapsed < claimWrite {
		t.Fatalf("the decision returned after %s, sooner than the %s bound: its write was never put to the store", elapsed, claimWrite)
	}
	if elapsed > unbounded {
		t.Fatalf("the decision returned after %s, past the %s bound: it waited on the database rather than on the deadline", elapsed, unbounded)
	}
	// Still owed: the repair's own failure says so, in the line an operator counts.
	if logs := rig.logs.String(); !strings.Contains(logs, loggedMessage(owedWriteFailedLine)) || !strings.Contains(logs, `"owed":true`) {
		t.Fatalf("the repair the deadline cut short logged %s, want %q with owed true: a write a deadline cut short is no refusal, and is owed", logs, owedWriteFailedLine)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the row reads %s while the database is away, want the ready row the first failure left", got)
	}

	// The database answers again, and the claim's next decision makes the write it has owed all
	// along: the row catches up with what memory has held since the turn started.
	rig.answers()
	rig.logs.Reset()
	if err := rig.handle(claim.RoleArchitect, rig.nudge(claim.RoleArchitect)); err != nil {
		t.Fatalf("the decision after the database answered again = %v, want none", err)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the row reads %s after the repair, want the working claim the machine holds", got)
	}
	if logs := rig.logs.String(); !strings.Contains(logs, loggedMessage(owedWriteMadeLine)) {
		t.Fatalf("the repair that landed logged %s, want %q", logs, owedWriteMadeLine)
	}
}

// What the bound buys the rest of the daemon. One claim's writes never answer, so its machine is
// in a decision for the whole of this test; everything else goes on inside the deadline. Another
// claim's turn start is applied and reaches its own row, a read that waits on the held machine's
// lock is answered rather than held for the outage, and the supervisor's stop returns.
//
// This is the whole worth of bounding the write: the machine's lock is what an event of this
// claim, a read of it, and the stop all wait on, and a decision that waits out the connection's
// own patience holds every one of them for the length of the outage.
func TestNoDecisionWaitsOnTheClaimWhoseWriteTheDatabaseHasNotTaken(t *testing.T) {
	// claimWrite is what the architect's blocked writes are measured by, and all that it is: the
	// implementer's writes and this row's own repair reach the real database, which the rig hands
	// out with the claim-write deadline off it (detachedWrites).
	const claimWrite = 500 * time.Millisecond
	// answered is what each wait here is measured against: several claim-write bounds, and far
	// less than the outage, which never ends. The daemon's own stop allows its whole shutdown 15
	// seconds (daemon.stop), so a stop inside this is a stop inside that.
	const answered = 10 * time.Second
	rig := newOwedWriteRig(t, claimWrite,
		owedRole{claim.RoleArchitect, supervise.StateReady},
		owedRole{claim.RoleImplementer, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)
	rig.block(claim.RoleArchitect)

	// The architect's machine decides for the rest of this test: every one of these events drives
	// a decision whose head is the repair, and every repair waits its bound out against a store
	// that never answers.
	architect := rig.token(claim.RoleArchitect)
	const decisions = 20
	for range decisions {
		rig.sup.post(architect, rig.nudge(claim.RoleArchitect))
	}
	testwait.Eventually(t, "the architect's machine to be deciding under the outage", func() bool {
		return rig.machine(claim.RoleArchitect).Deciding() != nil
	})

	// The implementer's own turn start is applied while that goes on, and its row takes it.
	began := time.Now()
	implementer := rig.token(claim.RoleImplementer)
	rig.sup.post(implementer, supervise.StreamTurnStart{Claim: implementer})
	testwait.Eventually(t, "the implementer's turn start to reach its row", func() bool {
		return rig.stored(claim.RoleImplementer) == supervise.StateWorking
	})
	if waited := time.Since(began); waited > answered {
		t.Fatalf("the implementer's turn start took %s, past %s: one claim's outage is holding another claim's decision", waited, answered)
	}

	// A read that waits on the held machine's lock — the registration route's read of the claim
	// (internal/api/claims.go), which is Machine.Claim() — is answered inside the deadline.
	read := make(chan time.Duration, 1)
	// The machine is taken here, on the test's own goroutine: a helper that fails the test must
	// not run on another.
	machine := rig.machine(claim.RoleArchitect)
	go func() {
		began := time.Now()
		_ = machine.Claim()
		read <- time.Since(began)
	}()
	select {
	case waited := <-read:
		if waited > answered {
			t.Fatalf("the read of the architect's claim waited %s, past %s: it is held for the outage rather than for the deadline", waited, answered)
		}
	case <-time.After(answered):
		t.Fatalf("the read of the architect's claim was not answered within %s: it is held for the outage", answered)
	}

	// The outage is still on: the architect's row is what the failed write left, and its claim
	// still stands working in memory alone.
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect's row reads %s, want the ready row its failed write left: its database answered after all", got)
	}
	if got := rig.held(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the architect stands %s, want working", got)
	}

	// The stop returns though the claim it waits for is in a write that never answers: what it
	// waits out is that decision's deadline.
	testwait.Eventually(t, "the architect's machine to be deciding again", func() bool {
		return rig.machine(claim.RoleArchitect).Deciding() != nil
	})
	stopped := make(chan time.Duration, 1)
	go func() {
		began := time.Now()
		rig.sup.stop()
		stopped <- time.Since(began)
	}()
	select {
	case waited := <-stopped:
		if waited > answered {
			t.Fatalf("the supervisor's stop took %s, past %s: it waits the outage out", waited, answered)
		}
	case <-time.After(answered):
		t.Fatalf("the supervisor's stop did not return within %s: it waits the outage out", answered)
	}
}

// The mark is memory and nothing else: a daemon that stops with a write owed loses it, and loses
// the reports owed with it. That is the spec's own answer — the loss is exactly what a crash
// between a decision and its write has always cost, and no part of this change makes a claim fact
// durable that was not durable before — and the point of holding it is that nothing is wedged by
// it. The next boot reads the stored row, takes the claim up from it, and the claim decides
// again.
func TestAnOwedWriteAcrossARestartIsLostAndTheStoredClaimIsTakenUp(t *testing.T) {
	rig := newOwedWriteRig(t, unmeasuredClaimWrite, owedRole{claim.RoleArchitect, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)

	// The daemon stops with the write owed, and the supervision is built again over the same
	// store, as the next boot builds it.
	rig.sup.stop()
	rig.boot()

	// The claim is taken up from the row the failed write left, not from the working claim the
	// stopped daemon held: the mark went with the process that held it.
	if got := rig.held(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the restored architect stands %s, want the ready row the failed write left", got)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect's row reads %s after the restart, want ready: nothing made the lost write", got)
	}

	// And it decides again: the claim is a claim like any other, with nothing owed and nothing
	// held over from the daemon that stopped.
	if err := rig.handle(claim.RoleArchitect, rig.turnStart(claim.RoleArchitect)); err != nil {
		t.Fatalf("the restored architect's turn start = %v, want none", err)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the restored architect's row reads %s after a turn started, want working", got)
	}
}

// No write that reaches the real database in this file is decided by a clock the rig sets, and
// nothing else in the file pins that: every other row puts a store in the way that withholds its
// answer, so the bound those rows name is measured against that store and never against the
// database. Here the database is only slow — it answers, four times later than the bound the row
// names — and the turn start whose write it takes must still be taken whole.
//
// This is the restart row with a slow database rather than a fresh rig with one, because that is
// the shape CI met the defect in: the first decision write after a boot. The refusal, the stop
// and the boot before the measured write cost a fifth of a second and leave the claim restored
// ready, which a fresh rig would leave too.
//
// The row reads the clock as well as the claim, and that reading is the row's own guard: a turn
// start that returns quickly never reached the slow database, so the write it made proves
// nothing about a bound, and the row says so rather than passing. slowClaimWrite waits on all
// three of a token's claim writes, so no decision of this shape can step past it, and the
// reading catches a fourth path if one is ever added.
//
// The 250 ms is the incident's own figure, and this is the only row where it bounds a write a
// real database takes: the 250 ms at unmeasuredClaimWrite is a figure nothing waits out, and the
// one in TestTheRepairDeadlineEndsADecisionsWaitOnADatabaseThatHasStoppedAnswering is measured
// against a store that never answers. It is what makes this row red with the detach removed,
// whatever bound the other rows name. A row taking production's own 5 s default instead would
// swallow the delay and prove nothing.
func TestARealWriteTheDatabaseIsSlowToTakeIsNotCutShortByTheRowsBound(t *testing.T) {
	const claimWrite = 250 * time.Millisecond
	const slow = 4 * claimWrite
	rig := newOwedWriteRig(t, claimWrite, owedRole{claim.RoleArchitect, supervise.StateReady})
	rig.refuse(claim.RoleArchitect, brokenConnection())
	rig.owedTurnStart(claim.RoleArchitect)

	rig.sup.stop()
	rig.boot()
	rig.slow(claim.RoleArchitect, slow)

	began := time.Now()
	err := rig.handle(claim.RoleArchitect, rig.turnStart(claim.RoleArchitect))
	took := time.Since(began)

	if err != nil {
		t.Fatalf("the restored architect's turn start, whose write the database took %s over = %v, want none: a bound of the rig's cut a healthy write short", slow, err)
	}
	if took < slow {
		t.Fatalf("the restored architect's turn start returned after %s, sooner than the %s the slow database holds a claim write: its write never reached that database, so this row measured nothing", took, slow)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateWorking {
		t.Fatalf("the restored architect's row reads %s after a turn started, want working", got)
	}
}

// The report a decision owed reaches the daemon itself, and not only the machine's callback. A
// claim whose ready the store did not take tells the daemon nothing at the time — the fact the
// daemon would act on is one the row does not hold — and once the repair has made the write, the
// report travels the daemon's own path: Machine.OnDurableState to supervisor.durableState to
// workflowRuntime.claimDurableState, which applies the claim's ready as a workflow fact
// (applyTerminal). The fact's own row is what this reads, because that row is what the daemon
// did with the report: written in the transaction that applies the fact, keyed on the claim and
// the generation the report named (terminalEventID), and never written by anything else.
func TestTheOwedReportsReachTheDaemonOnceTheRepairLands(t *testing.T) {
	rig := newOwedWriteRig(t, unmeasuredClaimWrite, owedRole{claim.RoleArchitect, supervise.StateRegistered})
	// The daemon's own durable-state path over the rig's real pool, as the workflow installs it
	// on every machine of its supervisor (workflowRuntime.attach).
	workflow := &workflowRuntime{pool: rig.pool, records: record.NewStore(), log: quietLogger()}
	rig.sup.OnDurableState(workflow.claimDurableState)
	architect := rig.machine(claim.RoleArchitect).Claim()
	readyFact := terminalEventID(architect, supervise.StateReady)

	// The agent says it is ready and the store refuses the claim write: the claim stands ready in
	// memory, its row still reads registered, and the daemon has not been told.
	rig.refuse(claim.RoleArchitect, brokenConnection())
	if err := rig.handle(claim.RoleArchitect, supervise.RequestReady{
		Claim: architect.Token, Generation: architect.Generation, Session: architect.Session,
	}); err == nil {
		t.Fatal("the ready whose claim write the store refused returned no error")
	}
	if got := rig.held(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect stands %s after it said it was ready, want ready", got)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateRegistered {
		t.Fatalf("the architect's row reads %s, want the registered row the refused write left", got)
	}
	if rig.applied(readyFact) {
		t.Fatalf("the daemon applied %s while the claim write was owed: it was told a claim fact the store does not hold", readyFact)
	}

	// The database answers again, and the claim's next decision makes the owed write and the
	// report behind it. The daemon learns what it would have learnt had the write been taken,
	// only later.
	rig.answers()
	if err := rig.handle(claim.RoleArchitect, rig.nudge(claim.RoleArchitect)); err != nil {
		t.Fatalf("the decision after the database answered again = %v, want none", err)
	}
	if got := rig.stored(claim.RoleArchitect); got != supervise.StateReady {
		t.Fatalf("the architect's row reads %s after the repair, want ready", got)
	}
	if !rig.applied(readyFact) {
		t.Fatalf("the daemon never applied %s: the report the owed write skipped never reached claimDurableState", readyFact)
	}
}

// applied is whether the daemon has applied the supervision fact of this event id, which
// applyTerminal writes in the fact's own transaction (intake.ApplyFact).
func (r *owedWriteRig) applied(eventID string) bool {
	r.t.Helper()
	var applied bool
	if err := r.pool.QueryRow(r.ctx,
		"select exists (select 1 from processed_events where source = $1 and event_id = $2)",
		terminalSource, eventID).Scan(&applied); err != nil {
		r.t.Fatalf("read whether the daemon applied %s: %v", eventID, err)
	}
	return applied
}
