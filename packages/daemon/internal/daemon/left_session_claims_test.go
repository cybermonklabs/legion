package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The tests of this file are the release of the Dispatch claim a session holds once the daemon has
// left that session behind (dispatch://INFRA-3144): the claim write that moves a claim off a
// session it recorded queues a claim release naming that session (store.ReleaseLeftSessionClaims),
// a move of an issue out of the workflow releases the claim of any session its own tree recorded,
// current or left behind, and each release that leaves a claim on purpose says why in the issue's
// record (`claimLeft`). They run the daemon's own parts over the real store: the supervisor and its
// machines, the workflow engine and admission, the outbox, the operator's and the controller's API
// routes, and a Dispatch stand-in that keeps the claims of the board as Dispatch's own handler keeps
// them.

// leftRoot is the root every tree of these tests runs under, and leftChild a child of its tree.
const (
	leftRoot  = "LEGION-310"
	leftChild = "LEGION-311"
	// leftOperatorToken is the bearer `legion claims` sends, which the operator routes compare
	// against, and the one the controller's grant is minted with.
	leftOperatorToken = "operator-token"
	// leftTask is the phase task a phase worker of the implementing root is given.
	leftTask = "Continue Workflow. Issue: LEGION-310. Phase: implementing."
)

// leftClock is the time the rig's outbox and workflow run on: the wall clock, moved forward by
// what the test has advanced it and never behind it. Every row the store or the workflow writes is
// due at once, and a release's backoff, or the hour it is given (claimReleaseWait), lapses only
// when the test moves the clock.
type leftClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *leftClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *leftClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// leftRelease is one claim Dispatch released: the issue, the holder the release took the claim
// from (dispatch.Claim.Holder), and the claim release row that sent the DELETE, by the session its
// payload names ("" for the row of a move out of the workflow or of the boot).
type leftRelease struct {
	issue, holder, row string
}

// leftDispatch is Dispatch's issue claims as its own handler keeps and judges them
// (packages/envoy/internal/dispatch/api/issue_claim.go), for the issues these tests drive. Each
// issue has one holder, a session or a person. Each session is live or gone, as Dispatch's read
// of the session's registry record judges it (judgeClaim): a session is live until the test says
// its process has ended. A session's claim, as `dispatch claim` makes it (claimIssue), takes the
// issue from nobody and from a session that is gone, and is refused by a live session's claim and
// by a person's (claimBlockedBy). The daemon's release (releaseIssueClaim, under the daemon's own
// session, which holds no person's scope) clears no claim at all and takes a gone session's claim,
// and answers 409 ISSUE_CLAIMED (issueClaimedError) for a live session's claim and for a person's.
// answers are what DELETEs get back before any of that is judged — CLAIM_CONTENDED
// (answerContendedClaim), or a refusal no retry repairs — and refusedReads what a read of an issue
// gets back in place of the issue.
//
// Every release is recorded with the holder it took and the row that sent it (running), and
// forbidden fails the test at any DELETE, where it is sent rather than at an assertion after it.
type leftDispatch struct {
	*outboxDispatch
	t            *testing.T
	mu           sync.Mutex
	holders      map[string]dispatch.Claim
	unread       map[string]bool
	gone         map[string]bool
	statuses     map[string]string
	refusedReads map[string]error
	answers      []error
	forbidden    bool
	released     []leftRelease
	deletes      int
	// running is the session the claim release row running now names for an issue, which the
	// rig reads from the outbox's lease (leftRig.runningRelease).
	running func(issue string) string
}

func newLeftDispatch(t *testing.T) *leftDispatch {
	return &leftDispatch{outboxDispatch: &outboxDispatch{}, t: t, holders: map[string]dispatch.Claim{}, unread: map[string]bool{},
		gone: map[string]bool{}, statuses: map[string]string{}, refusedReads: map[string]error{}}
}

func (d *leftDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.refusedReads[key]; err != nil {
		return dispatch.Issue{}, err
	}
	issue := dispatch.Issue{Key: key, Status: d.statuses[key], ClaimUnread: d.unread[key]}
	if holder, held := d.holders[key]; held {
		issue.Claim = &holder
	}
	return issue, nil
}

func (d *leftDispatch) ReleaseClaim(_ context.Context, key string) error {
	row := ""
	if d.running != nil {
		row = d.running(key)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes++
	if d.forbidden {
		d.t.Errorf("Dispatch was sent the claim release of %s, held by %s; this case must send none", key, d.holderLocked(key))
	}
	if len(d.answers) > 0 {
		answer := d.answers[0]
		d.answers = d.answers[1:]
		return answer
	}
	holder, held := d.holders[key]
	switch {
	case !held:
		return nil
	case holder.Kind != "session":
		return claimConflict(dispatch.IssueClaimed)
	case holder.ID != dispatch.DaemonSession(key) && !d.gone[holder.ID]:
		return claimConflict(dispatch.IssueClaimed)
	}
	delete(d.holders, key)
	d.released = append(d.released, leftRelease{issue: key, holder: holder.Holder(), row: row})
	return nil
}

func (d *leftDispatch) SetStatus(_ context.Context, key, status string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statuses[key] = status
	return dispatch.Issue{Key: key, Status: status}, nil
}

func (d *leftDispatch) SetStatusAt(ctx context.Context, issue dispatch.Issue, status string) (dispatch.Issue, error) {
	return d.SetStatus(ctx, issue.Key, status)
}

// takes is session claiming issue, as `dispatch claim` run as that session does; a claim Dispatch
// refuses fails the test.
func (d *leftDispatch) takes(issue, session string) {
	d.t.Helper()
	if !d.claim(issue, dispatch.Claim{Kind: "session", ID: session}) {
		d.t.Fatalf("Dispatch refused %s's claim of %s, held by %s", session, issue, d.holder(issue))
	}
}

// claim is actor claiming issue, and whether Dispatch let it (claimIssue, claimBlockedBy).
func (d *leftDispatch) claim(issue string, actor dispatch.Claim) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	holder, held := d.holders[issue]
	switch {
	case !held, holder == actor:
	case holder.Kind != "session", !d.gone[holder.ID]:
		return false
	}
	d.holders[issue] = actor
	delete(d.unread, issue)
	return true
}

// forces is a person taking issue's claim, whoever holds it: a person may force any claim.
func (d *leftDispatch) forces(issue, user string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.holders[issue] = dispatch.Claim{Kind: "user", ID: user}
	delete(d.unread, issue)
}

// shows sets what Dispatch shows of issue's claim directly: holder, or a claim naming no actor
// where unread.
func (d *leftDispatch) shows(issue string, holder *dispatch.Claim, unread bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.holders, issue)
	if holder != nil {
		d.holders[issue] = *holder
	}
	d.unread[issue] = unread
}

// ends is session's process ending: Dispatch's registry reads it gone from now on.
func (d *leftDispatch) ends(session string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gone[session] = true
}

// lives is session registered and heard from: Dispatch's registry reads it live.
func (d *leftDispatch) lives(session string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.gone, session)
}

// answer queues what the next DELETEs get back before their holder is judged.
func (d *leftDispatch) answer(answers ...error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answers = append(d.answers, answers...)
}

// refuseReads has every read of issue answered with err.
func (d *leftDispatch) refuseReads(issue string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refusedReads[issue] = err
}

// forbid fails the test at every DELETE from now on.
func (d *leftDispatch) forbid() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forbidden = true
}

// holder is issue's holder as Dispatch shows it, "" for none.
func (d *leftDispatch) holder(issue string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.holderLocked(issue)
}

func (d *leftDispatch) holderLocked(issue string) string {
	holder, held := d.holders[issue]
	if !held {
		return ""
	}
	return holder.Holder()
}

// releases is every claim Dispatch released, in order, and deleted how many DELETEs it was sent.
func (d *leftDispatch) releases() []leftRelease {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.released)
}

func (d *leftDispatch) deleted() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deletes
}

// leftConn is an agent's connection that runs a hook just before the next prompt it is sent, so a
// test can read the board at the moment a task reaches the agent.
type leftConn struct {
	*fake.Conn
	mu       sync.Mutex
	onPrompt func()
}

func (c *leftConn) Prompt(ctx context.Context, deliveryID, message string) error {
	c.mu.Lock()
	hook := c.onPrompt
	c.onPrompt = nil
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return c.Conn.Prompt(ctx, deliveryID, message)
}

// beforeNextPrompt arms hook for the next prompt alone.
func (c *leftConn) beforeNextPrompt(hook func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onPrompt = hook
}

// leftSpecs is the stub spec builder with one difference: a claim it is told runs Claude Code is
// built on that backend, which is what a launch reads to resume a transcript file alone
// (supervise's start, SessionLossClaudeNoFile).
type leftSpecs struct {
	mu     sync.Mutex
	claude map[claim.Token]bool
}

func (s *leftSpecs) SpawnSpec(_ context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claude[c.Token] {
		return runtime.SpawnSpec{Backend: runtime.BackendClaude}, nil
	}
	return runtime.SpawnSpec{}, nil
}

// leftRig is the daemon of one project over a database of its own: the real store, with the claim
// writes of a daemon that has a workflow (PublishRoleHolders and ReleaseLeftSessionClaims), the
// supervisor writing every claim through it, the workflow engine and admission the facts are
// applied through, the outbox that runs what they queue, the operator's and the controller's API
// routes beside the record route a role reads, the fake runtime, and the Dispatch stand-in. The
// outbox is drained by hand, and its log captured.
type leftRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	records   record.Store
	st        *store.Store
	engine    *workflow.Engine
	admission *admit.Admission
	sup       *supervisor
	rt        *fake.Runtime
	specs     *leftSpecs
	runner    *outbox
	clock     *leftClock
	dispatch  *leftDispatch
	api       http.Handler
	logs      *syncBuffer
	seq       int64
	tick      int
	// ran is the newest start row run of each issue and role, and suspended every suspend row
	// run.
	ran       map[string]int64
	suspended map[int64]bool
	conns     map[claim.Token]*leftConn
}

func newLeftRig(t *testing.T) *leftRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := &leftClock{}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Clock: clock.Now}, quietLogger())
	admission := admit.New(records, engine, 4, "LEGION", quietLogger())
	handlers := []intake.Handler{engine, admission}
	// The daemon's store as a daemon with a workflow runs it (daemon.go): a claim write that
	// changes a role holder queues its publication, and one that leaves a session behind queues
	// that session's claim release. The test drains the outbox itself, so the wake that would
	// start the claim lane at the commit has nothing to wake.
	st := outboxTreeStore(t, pool, leftRoot)
	st.PublishRoleHolders(nil)
	st.ReleaseLeftSessionClaims(func(string) {})
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	specs := &leftSpecs{claude: map[claim.Token]bool{}}
	sup.store = st
	sup.deps.Store = keptClaimStore{Store: st}
	sup.deps.Specs = specs
	w := &workflowRuntime{pool: pool, records: records, handlers: handlers, log: quietLogger()}
	sup.deps.PhaseHolds = w.phaseHolds
	sup.deps.RootHears = w.rootHears
	sup.deps.RootTask = w.rootTask
	sup.OnDurableState(w.terminal)
	logs := &syncBuffer{}
	client := newLeftDispatch(t)
	r := &leftRig{t: t, ctx: ctx, pool: pool, records: records, st: st, engine: engine, admission: admission, sup: sup, rt: rt,
		specs: specs, clock: clock, dispatch: client, logs: logs, seq: 10,
		ran: map[string]int64{}, suspended: map[int64]bool{}, conns: map[claim.Token]*leftConn{}}
	client.running = r.runningRelease
	workspaceDir := t.TempDir()
	// GitHub as the issue branch row of an admission meets it: the branch exists already, so the
	// starts that wait behind that row come due once it has run.
	r.runner = &outbox{log: slog.New(slog.NewJSONHandler(logs, nil)), pool: pool, dispatchProject: "LEGION", project: "legion",
		records: records, supervisor: sup, dispatch: client, notices: &outboxPublisher{}, handlers: handlers,
		tokens: outboxTokens{}, stateDir: t.TempDir(), now: clock.Now, trees: st, githubAPI: newBranchGitHub(t, rt, branchExists).url,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: workspaceDir, Bookmark: "legion/" + leftRoot}, nil
		}}
	// The routes as the daemon wires them: the operator's `legion claims`, the controller's agent
	// door, and the record route a role's read_record reads, over the daemon's own state source.
	r.api = api.NewServer("127.0.0.1", 8437, api.Options{
		Supervisor: sup, BootTokens: api.NewBootTokens(st), Project: "legion", DispatchProject: "LEGION",
		OperatorToken: leftOperatorToken, Controller: st, Trees: st, Releaser: rt, Pool: pool, Record: records,
		Handlers: handlers, RoleFor: workflow.RoleFor, Log: quietLogger(),
		State: &source{store: st, records: records, project: "LEGION", projectToken: "legion", runtime: "tmux", admissionCap: 4,
			startedAt:  time.Now(),
			deployment: func([]sandbox.CacheStatus) capabilities.Deployment { return capabilities.Deployment{} },
			reports:    func() map[claim.Token]capabilities.Report { return nil }},
		StateTransactions: pool,
	}).Handler
	return r
}

// apply is one fact as the daemon applies it: through the engine and admission, in one transaction
// keyed by its id.
func (r *leftRig) apply(id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.engine, r.admission)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply %s = %#v, %v", id, result, err)
	}
}

// admitPass is one admission pass, the controller's tick.
func (r *leftRig) admitPass() {
	r.t.Helper()
	r.tick++
	r.apply(fmt.Sprintf("tick-%d", r.tick), intake.ControllerTick{})
}

func (r *leftRig) nextSeq() int64 {
	r.seq++
	return r.seq
}

func (r *leftRig) put(issue record.Issue) {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, r.records, issue)
}

func (r *leftRig) issue(key string) record.Issue {
	r.t.Helper()
	got := readIssue(r.t, r.pool, key)
	if got == nil {
		r.t.Fatalf("%s is not recorded", key)
	}
	return *got
}

// move is a person setting key to status on Dispatch's board, as Dispatch's event of it reaches the
// engine and admission.
func (r *leftRig) move(key, status string) {
	r.t.Helper()
	recorded := r.issue(key)
	fact := intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: r.nextSeq(), Type: "issue.updated", Status: status,
		Title: recorded.Title, Rank: recorded.Rank, HandedOver: recorded.HandedOver}
	if recorded.Parent != nil {
		fact.Parent = *recorded.Parent
	}
	r.apply(fmt.Sprintf("move:%s:%s:%d", key, status, fact.Seq), fact)
}

// token is the claim token of issue's role, with a connection registered for the agent the claim
// will run.
func (r *leftRig) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token := mustClaimToken(r.t, issue, role)
	if _, known := r.conns[token]; !known {
		conn := &leftConn{Conn: fake.NewConn()}
		r.conns[token] = conn
		r.sup.deps.Conns.(*fake.Conns).Register(token, conn)
	}
	return token
}

func (r *leftRig) machine(token claim.Token) *supervise.Machine {
	r.t.Helper()
	machine, found := r.sup.Machine(token)
	if !found {
		r.t.Fatalf("the supervisor holds no claim %s", token)
	}
	return machine
}

// must hands token's machine ev and waits for what it set going.
func (r *leftRig) must(token claim.Token, ev supervise.Event) {
	r.t.Helper()
	machine := r.machine(token)
	if err := machine.Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("handle %T for %s: %v", ev, token, err)
	}
	machine.Wait()
}

// settle waits for every send a machine of the rig started to have had its outcome handled.
func (r *leftRig) settle() {
	for token := range r.conns {
		if machine, found := r.sup.Machine(token); found {
			machine.Wait()
		}
	}
}

// launch creates the claim of issue's role in tree, queued, and spawns its agent, as an operator's
// or a workflow's first start does.
func (r *leftRig) launch(issue, tree string, role claim.Role) claim.Token {
	r.t.Helper()
	token := r.token(issue, role)
	if _, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role,
		State: supervise.StateQueued}, ""); err != nil {
		r.t.Fatalf("create the claim %s: %v", token, err)
	}
	r.must(token, supervise.RequestSpawn{Claim: token})
	return token
}

// bringUp is the agent of token's claim coming up on the launch the claim stands at: the shim's
// hello, the agent's registration on session and its ready. Registered, the session is live in
// Dispatch's registry.
func (r *leftRig) bringUp(token claim.Token, session string) {
	r.t.Helper()
	machine := r.machine(token)
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: session, SessionFile: leftSessionFile(session)},
		supervise.RequestReady{Claim: token, Generation: launch, Session: session},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for %s on %s: %v", ev, token, session, err)
		}
	}
	machine.Wait()
	if got := machine.Claim(); !supervise.SaidReady(got.State) || got.Session != session {
		r.t.Fatalf("%s after its ready is %s on %q, want a claim that can be prompted on %s", token, got.State, got.Session, session)
	}
	r.dispatch.lives(session)
}

// leftSessionFile is the transcript an agent of these tests records for session.
func leftSessionFile(session string) string { return "/tmp/" + session + ".jsonl" }

// takeTask is the agent of token's claim working the task it has been given through its turn and
// its prompt's result; it does nothing for a claim that holds no task.
func (r *leftRig) takeTask(token claim.Token) {
	r.t.Helper()
	machine := r.machine(token)
	pending := machine.Claim().Pending
	if pending == nil {
		return
	}
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID},
		supervise.StreamTurnEnd{Claim: token},
		supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
	machine.Wait()
}

// failTask is the provider refusing the task token's claim was given with an error that cannot
// clear, code being the class the producer gave it ("" for a plain HTTP 400): the claim fails at
// once (supervise's classifyProviderError), keeping its session and the task.
func (r *leftRig) failTask(token claim.Token, code string) {
	r.t.Helper()
	machine := r.machine(token)
	machine.Wait()
	pending := machine.Claim().Pending
	if pending == nil {
		r.t.Fatalf("%s holds no task for its provider to refuse", token)
	}
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID},
		supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true, Error: "invalid_request_error",
			HTTPStatus: http.StatusBadRequest, Code: code},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
	machine.Wait()
	if got := machine.Claim(); got.State != supervise.StateFailed {
		r.t.Fatalf("%s after its provider's refusal is %s, want it failed", token, got.State)
	}
}

// outboxRows reads every outbox row query selects (its id, kind, issue and payload, in that order).
func (r *leftRig) outboxRows(query string, args ...any) []record.OutboxRow {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, query, args...)
	if err != nil {
		r.t.Fatalf("read the outbox: %v", err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var out record.OutboxRow
		var kind string
		err := row.Scan(&out.ID, &kind, &out.Issue, &out.Payload)
		out.Kind = record.OutboxKind(kind)
		return out, err
	})
	if err != nil {
		r.t.Fatalf("read the outbox: %v", err)
	}
	return read
}

// runLifecycle runs row as the issue's lifecycle lane runs it: leased, executed, and finished when
// it acted. A row that waits (wait.ErrWaiting) is left for its backoff, as the lane leaves it, and
// reported false.
func (r *leftRig) runLifecycle(row record.OutboxRow) bool {
	r.t.Helper()
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	switch err := r.runner.execute(r.ctx, row); {
	case errors.Is(err, wait.ErrWaiting):
		r.settle()
		return false
	case err != nil:
		r.t.Fatalf("run the %s row %d of %s: %v", row.Kind, row.ID, row.Issue, err)
	}
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		r.t.Fatalf("finish the %s row %d of %s: %v", row.Kind, row.ID, row.Issue, err)
	}
	r.settle()
	return true
}

// runStarts runs every start row of issue's role past the last one the rig ran, oldest first. A
// path that queued none fails the test.
func (r *leftRig) runStarts(issue string, role claim.Role) {
	r.t.Helper()
	key := issue + "/" + string(role)
	queued := r.outboxRows(`select id, kind, issue, payload from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $2 and id > $3 order by id`,
		issue, string(role), r.ran[key])
	if len(queued) == 0 {
		r.t.Fatalf("no %s start row of %s past row %d: the path queued none", role, issue, r.ran[key])
	}
	for _, row := range queued {
		if r.runLifecycle(row) {
			r.ran[key] = row.ID
		}
	}
}

// runSuspends runs every suspend row of issue the rig has not run yet, oldest first: what a move
// out of the workflow queued for each claim of the issue.
func (r *leftRig) runSuspends(issue string) {
	r.t.Helper()
	for _, row := range r.outboxRows(`select id, kind, issue, payload from outbox
		where issue = $1 and kind = 'supervise' and payload->>'op' = 'suspend' order by id`, issue) {
		if r.suspended[row.ID] {
			continue
		}
		if r.runLifecycle(row) {
			r.suspended[row.ID] = true
		}
	}
}

// releases runs every claim release row due on the rig's clock, as the issue's claim lane runs
// them: each leased, run and settled (outbox.runRow).
func (r *leftRig) releases() {
	r.t.Helper()
	due, err := r.runner.listDue(r.ctx, "", []record.OutboxKind{record.OutboxKindClaimRelease}, r.clock.Now().UTC())
	if err != nil {
		r.t.Fatalf("list the due claim releases: %v", err)
	}
	for _, row := range due {
		if err := r.runner.runRow(r.ctx, row); err != nil {
			r.t.Fatalf("run the claim release row %d of %s: %v", row.ID, row.Issue, err)
		}
	}
}

// later moves the rig's clock past the longest backoff a waiting release takes
// (record.RetryBackoffMost) and runs every release due then.
func (r *leftRig) later() {
	r.t.Helper()
	r.clock.advance(record.RetryBackoffMost + time.Second)
	r.releases()
}

// drain runs every row of the outbox due on the rig's clock, as the lanes run them, until none is
// due: a row that waits or fails is left for its backoff.
func (r *leftRig) drain() {
	r.t.Helper()
	for range 10 {
		due, err := r.runner.listDue(r.ctx, "", nil, r.clock.Now().UTC())
		if err != nil {
			r.t.Fatalf("list the due outbox rows: %v", err)
		}
		if len(due) == 0 {
			return
		}
		if err := r.runner.RunOnce(r.ctx); err != nil {
			r.t.Fatalf("drain the outbox: %v", err)
		}
		r.settle()
	}
}

// runningRelease is the session the claim release row of issue running now names, "" for the row
// of a move: the one claim release row of the issue under a live lease.
func (r *leftRig) runningRelease(issue string) string {
	rows, err := r.pool.Query(context.Background(), `select coalesce(payload->>'session', '') from outbox
		where kind = $1 and issue = $2 and lease_until is not null`, string(record.OutboxKindClaimRelease), issue)
	if err != nil {
		r.t.Errorf("read the claim release running for %s: %v", issue, err)
		return ""
	}
	running, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(running) != 1 {
		r.t.Errorf("the claim release rows of %s under a lease name sessions %q (%v), want the one row that sent the DELETE", issue, running, err)
		return ""
	}
	return running[0]
}

// leftQueued is one claim release row the outbox still holds: its issue, the session it names (""
// for a move's) and how often it has run.
type leftQueued struct {
	issue, session string
	attempts       int
}

func (r *leftRig) queuedReleases() []leftQueued {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select issue, coalesce(payload->>'session', ''), attempts from outbox where kind = $1 order by id`,
		string(record.OutboxKindClaimRelease))
	if err != nil {
		r.t.Fatalf("read the queued claim releases: %v", err)
	}
	queued, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (leftQueued, error) {
		var q leftQueued
		return q, row.Scan(&q.issue, &q.session, &q.attempts)
	})
	if err != nil {
		r.t.Fatalf("read the queued claim releases: %v", err)
	}
	return queued
}

// queuedFor is the claim release row of the left session, nil where the outbox holds none.
func (r *leftRig) queuedFor(session string) *leftQueued {
	r.t.Helper()
	for _, q := range r.queuedReleases() {
		if q.session == session {
			return &q
		}
	}
	return nil
}

// unqueue takes away the claim release the claim write that left session behind queued, which a
// daemon of a release before this one never wrote: that session was left behind before this
// change was deployed (departure D1 of the plan), and its claim is one only a move's release can
// find. Whether the write queued its row is held where that row is the subject (the second of
// leftHolders, and TestASessionTheDaemonLeavesBehindHasItsClaimReleased), so a write that queued
// none leaves nothing to take away here; more than one is no write of this change's.
func (r *leftRig) unqueue(session string) {
	r.t.Helper()
	tag, err := r.pool.Exec(r.ctx, `delete from outbox where kind = $1 and payload->>'session' = $2`,
		string(record.OutboxKindClaimRelease), session)
	if err != nil {
		r.t.Fatalf("take away the claim release of %s: %v", session, err)
	}
	if tag.RowsAffected() > 1 {
		r.t.Fatalf("the claim write that left %s behind queued %d claim releases naming it, want at most one", session, tag.RowsAffected())
	}
}

// call is one HTTP request of the daemon's API, with bearer where it is not "".
func (r *leftRig) call(method, target string, body any, bearer string) *httptest.ResponseRecorder {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	r.api.ServeHTTP(recorder, request)
	r.settle()
	return recorder
}

// operator is one `legion claims` request on token's claim (the route below
// /legion/v1/operator/claims/<token>), which must answer want.
func (r *leftRig) operator(token claim.Token, action string, body any, want ...int) {
	r.t.Helper()
	recorder := r.call(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/"+action, body, leftOperatorToken)
	if !slices.Contains(want, recorder.Code) {
		r.t.Fatalf("legion claims %s of %s = %d, want one of %v; body %s", action, token, recorder.Code, want, recorder.Body)
	}
}

// controllerFreshSession is the project controller's fresh_session on issue's role (POST
// /legion/v1/claims/fresh-session), under a controller grant minted with the operator's bearer
// (POST /legion/v1/grants), as `legion` mints the grant its controller's calls carry.
func (r *leftRig) controllerFreshSession(issue string, role claim.Role) {
	r.t.Helper()
	minted := r.call(http.MethodPost, "/legion/v1/grants", map[string]any{}, leftOperatorToken)
	if minted.Code != http.StatusOK {
		r.t.Fatalf("the controller grant = %d; body %s", minted.Code, minted.Body)
	}
	var grant api.GrantResponse
	if err := json.Unmarshal(minted.Body.Bytes(), &grant); err != nil {
		r.t.Fatalf("decode the controller grant: %v", err)
	}
	recorder := r.call(http.MethodPost, "/legion/v1/claims/fresh-session",
		api.ClaimRequest{GrantID: grant.GrantID, Issue: issue, Role: string(role)}, "")
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("the controller's fresh_session on %s's %s = %d, want 200; body %s", issue, role, recorder.Code, recorder.Body)
	}
}

// claimLeft is issue's `claimLeft` as GET /legion/v1/issues/{key}/record serves it, decoded as a
// role's read_record reads the body: plain JSON. It is nil where the record names none.
func (r *leftRig) claimLeft(key string) map[string]any {
	r.t.Helper()
	recorder := r.call(http.MethodGet, "/legion/v1/issues/"+key+"/record", nil, "")
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("the record of %s = %d, want 200; body %s", key, recorder.Code, recorder.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		r.t.Fatalf("decode the record of %s: %v", key, err)
	}
	served, named := body["claimLeft"]
	if !named || served == nil {
		return nil
	}
	entry, ok := served.(map[string]any)
	if !ok {
		r.t.Fatalf("the record of %s serves claimLeft %#v, want an object", key, served)
	}
	return entry
}

// recordClaimLeft writes left as issue's entry with the record's own statement, the entry an
// earlier release wrote.
func (r *leftRig) recordClaimLeft(issue string, left record.ClaimLeft) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().RecordClaimLeft(ctx, tx, issue, &left)
	}); err != nil {
		r.t.Fatalf("record the claim %s left of %s: %v", left.Holder, issue, err)
	}
}

// wantReleasedBy holds that Dispatch released issue's claim, taking it from holder, by the claim
// release row that names row ("" for a move's), and that no claim of issue stands. A claim still
// standing is reported with every line of the executor's that said it could not attribute a
// holder, the arm that leaves such a claim.
func (r *leftRig) wantReleasedBy(issue, holder, row string) {
	r.t.Helper()
	if got := r.dispatch.holder(issue); got != "" {
		lines := loggedLines(r.t, r.logs.String(), claimUnattributedLine)
		r.t.Fatalf("Dispatch shows %s claimed by %s after the release rows ran, want no claim; the executor logged %q %d times: %v",
			issue, got, claimUnattributedLine, len(lines), lines)
	}
	want := leftRelease{issue: issue, holder: holder, row: row}
	if got := r.dispatch.releases(); !slices.Contains(got, want) {
		r.t.Fatalf("Dispatch released %+v, want %+v among them", got, want)
	}
}

// wantNoUnattributedLine holds that no release left a claim as one the daemon cannot attribute to
// the issue's tree, the arm the case of dispatch://INFRA-3108 took.
func (r *leftRig) wantNoUnattributedLine() {
	r.t.Helper()
	if lines := loggedLines(r.t, r.logs.String(), claimUnattributedLine); len(lines) != 0 {
		r.t.Fatalf("a claim release logged %q %d times, want never: %v", claimUnattributedLine, len(lines), lines)
	}
}

// admitRoot hands leftRoot to Legion as a person does — labelled, handed over and todo — and runs
// the admission pass that promotes it and the start that pass queues, and brings its architect up
// on session, which works the tree's first task: the generation-1 tree a test starts from.
func (r *leftRig) admitRoot(session string) claim.Token {
	r.t.Helper()
	r.put(record.Issue{Key: leftRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: leftRoot, Title: "root", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 5})
	token := r.token(leftRoot, claim.RoleArchitect)
	r.admitPass()
	if got := r.issue(leftRoot); got.Status != "in_progress" || got.Generation != 1 {
		r.t.Fatalf("the admitted root = %+v, want generation 1 running", got)
	}
	r.runStarts(leftRoot, claim.RoleArchitect)
	r.bringUp(token, session)
	r.takeTask(token)
	return token
}

// addChild records leftChild in the root's running tree, in planning at generation 1 and holding
// its slot, and launches the child's own architect (dispatch://INFRA-3915), up on session.
func (r *leftRig) addChild(session string) claim.Token {
	r.t.Helper()
	parent := leftRoot
	r.put(record.Issue{Key: leftChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: leftRoot, Parent: &parent, Title: "child", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "B",
		LastDispatchSeq: 5})
	holdSlot(r.t, r.pool, leftChild)
	token := r.launch(leftChild, leftRoot, claim.RoleArchitect)
	r.bringUp(token, session)
	return token
}

// implementing records leftRoot implementing at generation 1, holding its slot, and launches its
// implementer, up on session and idle once it has worked the task its phase's start gave it: the
// phase worker a fresh session, a retry and a forget leave a session of behind.
func (r *leftRig) implementing(session string) claim.Token {
	r.t.Helper()
	r.implementingRecord()
	token := r.launch(leftRoot, leftRoot, claim.RoleImplementer)
	r.bringUp(token, session)
	// The agent's ready queued the start that hands it its phase's task, which it works.
	r.runStarts(leftRoot, claim.RoleImplementer)
	r.takeTask(token)
	return token
}

// deliver gives token's claim the implementing phase's task, as the outbox's start does.
func (r *leftRig) deliver(token claim.Token) {
	r.t.Helper()
	r.must(token, supervise.RequestDeliver{Claim: token, Task: leftTask, ID: "outbox:7", Phase: phase.Implementing, Generation: 1})
}

// seedClaim creates c in the supervisor as it stands, its recorded session held for it in
// claim_sessions as the registration that recorded it would have held it (supervise's
// holdRecorded): the state a claim reaches that no registration in this rig walks to.
func (r *leftRig) seedClaim(c supervise.Claim) {
	r.t.Helper()
	r.token(c.Issue, c.Role)
	holdSessionName(r.t, r.pool, c.Project, c.Session, string(c.Token))
	if _, _, err := r.sup.Create(r.ctx, c, ""); err != nil {
		r.t.Fatalf("create the claim %s: %v", c.Token, err)
	}
}

// wantLeftBehind holds that token's claim left session behind: it records another session or
// none, and session as the one it abandoned.
func (r *leftRig) wantLeftBehind(token claim.Token, session string) {
	r.t.Helper()
	got := r.machine(token).Claim()
	if got.Session == session || got.Abandoned.Session != session {
		r.t.Fatalf("%s records session %q and abandoned %q, want %s left behind", token, got.Session, got.Abandoned.Session, session)
	}
}

// leftTree is the issue a move out of the workflow takes out — the tree's root, or a child of its
// running tree — with that issue's own architect, which records session now.
type leftTree struct {
	rig     *leftRig
	issue   string
	token   claim.Token
	session string
	made    int
}

// newSession is a session name of this tree no agent has registered yet.
func (tr *leftTree) newSession() string {
	tr.made++
	return fmt.Sprintf("ses-%s-%d", strings.ToLower(tr.issue), tr.made)
}

// leftTreeOf builds the tree a move takes issue out of, at generation 1, its architect up. A
// child's tree is a root whose spec a person approved, so its design gate stands open: the gate
// is what admission promotes a child set back to todo through, and the start of the child's own
// architect comes with that promotion (workflow's gateOpened).
func leftTreeOf(r *leftRig, issue string) *leftTree {
	r.t.Helper()
	tr := &leftTree{rig: r, issue: issue}
	if issue == leftRoot {
		tr.session = tr.newSession()
		tr.token = r.admitRoot(tr.session)
		return tr
	}
	r.admitRoot("ses-root-architect")
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutGate(ctx, tx, openSpecGate(leftRoot, 1))
	}); err != nil {
		r.t.Fatalf("open the design gate of %s: %v", leftRoot, err)
	}
	tr.session = tr.newSession()
	tr.token = r.addChild(tr.session)
	return tr
}

// freshSession is the controller's fresh_session on the issue's architect, and the new agent coming
// up on a session of its own: the claim leaves the session it recorded behind.
func (tr *leftTree) freshSession() (left string) {
	tr.rig.t.Helper()
	left = tr.session
	tr.rig.controllerFreshSession(tr.issue, claim.RoleArchitect)
	tr.rig.wantLeftBehind(tr.token, left)
	tr.session = tr.newSession()
	tr.rig.bringUp(tr.token, tr.session)
	return left
}

// moveOut is a person moving the issue to status, and the suspend of each of its claims that move
// queued running: the architect's process stops, and its session is gone in Dispatch's registry.
func (tr *leftTree) moveOut(status string) {
	tr.rig.t.Helper()
	tr.rig.move(tr.issue, status)
	if got := tr.rig.issue(tr.issue); !record.Stopped(got) {
		tr.rig.t.Fatalf("%s moved to %s is %+v, want it out of the workflow", tr.issue, status, got)
	}
	tr.rig.runSuspends(tr.issue)
	tr.rig.dispatch.ends(tr.session)
}

// readmit is a person setting the issue back to todo, the admission pass that re-admits it at the
// next generation, and the start of its architect that queues, which resumes the session the
// architect records.
func (tr *leftTree) readmit() {
	r := tr.rig
	r.t.Helper()
	before := r.issue(tr.issue).Generation
	r.move(tr.issue, "todo")
	r.admitPass()
	if got := r.issue(tr.issue); got.Generation != before+1 || record.Stopped(got) {
		r.t.Fatalf("%s set back to todo = %+v, want generation %d in the workflow", tr.issue, got, before+1)
	}
	r.runStarts(tr.issue, claim.RoleArchitect)
	r.bringUp(tr.token, tr.session)
	r.takeTask(tr.token)
}

// leftHolder is who holds the Dispatch claim of the issue when it moves out of the workflow: a
// session of its own tree, in one of the three ways a session can be the tree's.
type leftHolder struct {
	name string
	// moveOut takes the issue out of the workflow at status with the claim held this way, runs the
	// release rows, and answers the session that held the claim.
	moveOut func(tr *leftTree, status string) string
}

var leftHolders = []leftHolder{
	{"the claim's current session", func(tr *leftTree, status string) string {
		held := tr.session
		tr.rig.dispatch.takes(tr.issue, held)
		tr.moveOut(status)
		tr.rig.releases()
		tr.rig.wantReleasedBy(tr.issue, "session:"+held, "")
		return held
	}},
	// The left session's own row meets a holder Dispatch still reads as live, and waits on its
	// 409 until its backoff is the longest a release takes; the move's row comes due while it
	// waits, once Dispatch reads the session gone, and takes the claim through claim_sessions.
	// What the move's row did is held first, and the premise — the left session's row was queued
	// and still waiting on its holder when the move's row ran — after it, read from what was
	// recorded before the move: a claim write that queued no row leaves the move's row alone with
	// the claim, which is the arm the case is about.
	{"a session a fresh session left behind in this generation, its own release refused until after the move", func(tr *leftTree, status string) string {
		r := tr.rig
		held := tr.session
		r.dispatch.takes(tr.issue, held)
		left := tr.freshSession()
		r.releases()
		for range 7 {
			r.later()
		}
		waiting := r.queuedFor(left)
		heldBeforeTheMove := r.dispatch.holder(tr.issue)
		tr.moveOut(status)
		r.dispatch.ends(left)
		r.releases()
		r.wantReleasedBy(tr.issue, "session:"+left, "")
		if waiting == nil || waiting.attempts < 8 {
			r.t.Fatalf("the release of %s before the move was %+v, want it queued and refused on its holder until then", left, waiting)
		}
		if heldBeforeTheMove != "session:"+left {
			r.t.Fatalf("Dispatch showed %s claimed by %q while %s lived, want it kept", tr.issue, heldBeforeTheMove, left)
		}
		if r.queuedFor(left) == nil {
			r.t.Fatalf("the release of %s ran before the move's: the move's row did not meet its claim", left)
		}
		r.later()
		if waiting := r.queuedFor(left); waiting != nil {
			r.t.Fatalf("the release of %s is still queued (%+v) after the claim went, want it ended", left, waiting)
		}
		return left
	}},
	{"a session claim_sessions holds for a claim of the tree, with no release queued for it", func(tr *leftTree, status string) string {
		r := tr.rig
		held := tr.session
		r.dispatch.takes(tr.issue, held)
		left := tr.freshSession()
		r.unqueue(left)
		r.dispatch.ends(left)
		tr.moveOut(status)
		r.releases()
		r.wantReleasedBy(tr.issue, "session:"+left, "")
		return left
	}},
}

// R1 of dispatch://INFRA-3144: a move of an issue to backlog, icebox or triage takes it out of the
// workflow, and releases the Dispatch claim its own tree's session holds, in every generation of
// that tree. Its own tree's session is the issue's own architect in three ways: the claim's current
// session; a session a fresh session left behind earlier in that generation, whose own release
// Dispatch refused while it read that session live, so the move's release meets it once Dispatch
// reads it gone; and a session claim_sessions holds for a claim of the tree with no release queued
// for it, as a session left behind before this change was deployed is held (departure D1). Each
// holds the claim of a root, and of a child, where the child's own architect holds it
// (dispatch://INFRA-3915). The tree runs a second generation after the first move, through a
// person's todo and the admission pass that re-admits it, and the same move releases the claim of
// that generation's session. No release leaves a claim as one the daemon cannot attribute.
func TestAMoveOutOfTheWorkflowReleasesTheClaimOfItsOwnTreesSessionInEveryGeneration(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"backlog", "icebox", "triage"} {
		for _, issue := range []string{leftRoot, leftChild} {
			for _, holder := range leftHolders {
				kind := "a root"
				if issue == leftChild {
					kind = "a child"
				}
				t.Run(fmt.Sprintf("%s moved to %s, held by %s", kind, status, holder.name), func(t *testing.T) {
					t.Parallel()
					r := newLeftRig(t)
					tr := leftTreeOf(r, issue)
					for generation := uint64(1); generation <= 2; generation++ {
						if generation == 2 {
							tr.readmit()
						}
						held := holder.moveOut(tr, status)
						if got := r.issue(tr.issue).Generation; got != generation {
							t.Fatalf("the move out of generation %d left %s at generation %d", generation, tr.issue, got)
						}
						if got := r.dispatch.holder(tr.issue); got != "" {
							t.Fatalf("generation %d: Dispatch shows %s claimed by %s, which %s held, after the release rows ran; want no claim",
								generation, tr.issue, got, held)
						}
					}
					r.wantNoUnattributedLine()
				})
			}
		}
	}
}

// leftArm is one arm of the claim release executor that leaves a claim, set up over the record of
// the issue the release rows of outbox_claim_release_test.go answer: claimIssue, a child of
// claimTree the daemon took out of the workflow, the tree's own architect recording
// claimTreeSession, a claim of another tree recording ses-elsewhere, and the project's controller
// recording claimControllerSession, each session held for its claim in claim_sessions.
type leftArm struct {
	name string
	// set shows Dispatch's claim, records what the arm reads, and queues the release row; run runs
	// what the arm needs run, nil for one pass of the claim lane.
	set func(r *leftRig)
	run func(r *leftRig)
	// holder is the `claimLeft.holder` the record names, "" for none; logged the executor's line
	// the arm writes, "" for none.
	holder, logged string
}

// queueRelease queues a claim release of claimIssue with payload, written now on the rig's clock.
func (r *leftRig) queueRelease(payload record.ClaimRelease) {
	r.t.Helper()
	enqueueOutbox(r.t, r.pool, r.records, mustOutboxRow(r.t, claimIssue, payload, r.clock.Now()))
}

// seedReleaseTree seeds the records the arms read: claimTree and claimIssue as recorded, and the
// claims of claimTree, of another tree and of the project's controller, as the rig's supervisor
// writes them (claims.project `legion`).
func (r *leftRig) seedReleaseTree(child record.Issue) {
	r.t.Helper()
	r.put(claimReleaseRoot())
	r.put(child)
	seedSessionClaim(r.t, r.pool, "architect-claim", "legion", claimTree, claimTree, claimTreeSession)
	seedSessionClaim(r.t, r.pool, "other-tree-claim", "legion", "LEGION-9", "LEGION-9", "ses-elsewhere")
	seedControllerClaim(r.t, r.pool, "legion", claimControllerSession)
}

// leftArms is every value of record.ClaimLeftReasons, each by the arm of the executor the plan's
// decision table gives it.
var leftArms = map[record.ClaimLeftReason][]leftArm{
	record.ClaimLeftUnread: {{name: "Dispatch shows a claim and names no actor for it", set: func(r *leftRig) {
		r.dispatch.shows(claimIssue, nil, true)
		r.queueRelease(record.ClaimRelease{})
	}, logged: claimUnreadLine}},
	record.ClaimLeftPerson: {{name: "a person holds the claim", set: func(r *leftRig) {
		r.dispatch.forces(claimIssue, "alice")
		r.queueRelease(record.ClaimRelease{})
	}, holder: "user:alice"}},
	record.ClaimLeftDaemon: {{name: "the daemon's own session holds the claim", set: func(r *leftRig) {
		r.dispatch.takes(claimIssue, dispatch.DaemonSession(claimIssue))
		r.queueRelease(record.ClaimRelease{})
	}, holder: "session:" + dispatch.DaemonSession(claimIssue)}},
	record.ClaimLeftController: {{name: "a session the project controller's claim recorded holds the claim", set: func(r *leftRig) {
		r.dispatch.takes(claimIssue, claimControllerSession)
		r.queueRelease(record.ClaimRelease{})
	}, holder: "session:" + claimControllerSession}},
	record.ClaimLeftOtherTree: {{name: "the current session of another tree's claim holds the claim", set: func(r *leftRig) {
		r.dispatch.takes(claimIssue, "ses-elsewhere")
		r.queueRelease(record.ClaimRelease{})
	}, holder: "session:ses-elsewhere"}},
	record.ClaimLeftUnattributed: {{name: "a session no claim of the project recorded holds the claim", set: func(r *leftRig) {
		r.dispatch.takes(claimIssue, "ses-stranger")
		r.queueRelease(record.ClaimRelease{})
	}, holder: "session:ses-stranger", logged: claimUnattributedLine}},
	record.ClaimLeftOperatorTree: {{name: "the operator holds the issue's tree", set: func(r *leftRig) {
		openTreeUnderOperator(r.t, r.pool)
		r.dispatch.takes(claimIssue, claimTreeSession)
		r.queueRelease(record.ClaimRelease{})
	}, holder: "session:" + claimTreeSession}},
	record.ClaimLeftIssueRuns: {{name: "the move's row runs after the issue is in the workflow again", set: func(r *leftRig) {
		readmitted := outOfWorkflowIssue()
		readmitted.Status, readmitted.Phase, readmitted.Generation = "todo", phase.Admitted, 2
		r.put(readmitted)
		r.dispatch.takes(claimIssue, claimTreeSession)
		r.queueRelease(record.ClaimRelease{})
	}, holder: "session:" + claimTreeSession}},
	record.ClaimLeftWorksNow: {{name: "the left session's row: a claim of the tree records that session as its current session again", set: func(r *leftRig) {
		seedSessionClaim(r.t, r.pool, "planner-claim", "legion", claimTree, claimIssue, "ses-taken-again")
		r.dispatch.takes(claimIssue, "ses-taken-again")
		r.queueRelease(record.ClaimRelease{Session: "ses-taken-again"})
	}, holder: "session:ses-taken-again"}},
	record.ClaimLeftHeldAnHour: {{name: "Dispatch refuses the release 409 ISSUE_CLAIMED for an hour", set: func(r *leftRig) {
		r.dispatch.takes(claimIssue, claimTreeSession)
		r.queueRelease(record.ClaimRelease{})
	}, run: func(r *leftRig) {
		r.releases()
		if left := r.claimLeft(claimIssue); left != nil {
			r.t.Fatalf("a release waiting on its holder within the hour wrote claimLeft %v, want none yet", left)
		}
		r.clock.advance(time.Hour)
		r.releases()
	}, holder: "session:" + claimTreeSession, logged: claimGaveUpLine}},
	record.ClaimLeftRefused: {
		{name: "Dispatch refuses the read for good", set: func(r *leftRig) {
			r.dispatch.takes(claimIssue, claimTreeSession)
			r.dispatch.refuseReads(claimIssue, &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound})
			r.queueRelease(record.ClaimRelease{})
		}, logged: claimRefusedLine},
		{name: "Dispatch refuses the release for good", set: func(r *leftRig) {
			r.dispatch.takes(claimIssue, claimTreeSession)
			r.dispatch.ends(claimTreeSession)
			r.dispatch.answer(&dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound})
			r.queueRelease(record.ClaimRelease{})
		}, holder: "session:" + claimTreeSession, logged: claimRefusedLine},
	},
}

// R2 of dispatch://INFRA-3144: where the daemon leaves a claim on purpose, the reason is in the
// record a role reads, and not in its log alone. Each value of record.ClaimLeftReasons is reached
// by the arm of the executor the plan's decision table gives it, and read back through GET
// /legion/v1/issues/{key}/record, decoded as plain JSON as read_record hands it on: `claimLeft`
// names the reason, the holder Dispatch named (none for a claim it names no actor for, and none
// for a read it refused), and the moment on the executor's clock. An arm that logs writes its
// line as before, once. A release that takes the claim clears an earlier entry, and so does a
// release that finds no claim.
func TestEachClaimAReleaseLeavesIsReadInTheRecordWithItsReason(t *testing.T) {
	t.Parallel()
	for _, reason := range record.ClaimLeftReasons() {
		arms, named := leftArms[reason]
		if !named {
			t.Errorf("no arm here leaves a claim for the reason %q", reason)
			continue
		}
		for _, arm := range arms {
			t.Run(string(reason)+": "+arm.name, func(t *testing.T) {
				t.Parallel()
				r := newLeftRig(t)
				r.seedReleaseTree(outOfWorkflowIssue())
				arm.set(r)
				before := r.clock.Now()
				if arm.run != nil {
					arm.run(r)
				} else {
					r.releases()
				}
				after := r.clock.Now()
				left := r.claimLeft(claimIssue)
				if left == nil {
					t.Fatalf("the record of %s names no claimLeft, want the reason %q", claimIssue, reason)
				}
				if left["reason"] != string(reason) {
					t.Fatalf("claimLeft names the reason %v, want %q", left["reason"], reason)
				}
				if holder, named := left["holder"]; arm.holder == "" && named || arm.holder != "" && holder != arm.holder {
					t.Fatalf("claimLeft names the holder %v, want %q", holder, arm.holder)
				}
				wantLeftAt(t, left, before, after)
				executorLoggedOnce(t, r.logs, arm.logged)
			})
		}
	}

	earlier := record.ClaimLeft{Holder: "user:alice", Reason: record.ClaimLeftPerson, At: time.Now().Add(-time.Hour).UTC()}
	for _, tc := range []struct {
		name string
		set  func(r *leftRig)
	}{
		{"a release that takes the claim clears the entry an earlier release wrote", func(r *leftRig) {
			r.dispatch.takes(claimIssue, claimTreeSession)
			r.dispatch.ends(claimTreeSession)
		}},
		{"a release that finds no claim clears the entry an earlier release wrote", func(*leftRig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLeftRig(t)
			r.seedReleaseTree(outOfWorkflowIssue())
			r.recordClaimLeft(claimIssue, earlier)
			if left := r.claimLeft(claimIssue); left == nil || left["reason"] != string(record.ClaimLeftPerson) {
				t.Fatalf("the record of %s names claimLeft %v before the release, want the earlier entry", claimIssue, left)
			}
			tc.set(r)
			r.queueRelease(record.ClaimRelease{})
			r.releases()
			if left := r.claimLeft(claimIssue); left != nil {
				t.Fatalf("the record of %s names claimLeft %v after the release, want it cleared", claimIssue, left)
			}
			executorLoggedOnce(t, r.logs, "")
		})
	}
}

// wantLeftAt holds claimLeft's `at` to a moment of the run on the rig's clock, read at second
// precision around it.
func wantLeftAt(t *testing.T, left map[string]any, before, after time.Time) {
	t.Helper()
	served, _ := left["at"].(string)
	at, err := time.Parse(time.RFC3339Nano, served)
	if err != nil {
		t.Fatalf("claimLeft names the time %v, want an RFC 3339 time: %v", left["at"], err)
	}
	if at.Before(before.Add(-time.Second)) || at.After(after.Add(time.Second)) {
		t.Fatalf("claimLeft names the time %s, want the executor's clock during the run, %s to %s", at, before, after)
	}
}

// R3 of dispatch://INFRA-3144, in the order dispatch://INFRA-3108's Dispatch events and the daemon's
// log show it. The root's first architect session S1 claims the root. S1's claim fails, and S1's
// process with it. The controller's fresh_session starts the architect again in a new session S2,
// which does not claim the root, and S2's claim fails too. A person moves the root to backlog, and
// the claim lane runs at once, as the daemon's log shows the move's release running 41 ms after
// the move and 3 seconds before the person's todo; there that release left S1's claim as one the
// daemon could not attribute to the root's tree. The root moves to todo 4 seconds later on the
// rig's clock, which re-admits it at generation 2, and the rest of the rows run as the lanes run
// them. When the generation-2 architect's first task is sent, Dispatch holds no claim of S1, and
// the release that took it was the left session's own row, naming S1, queued by the fresh
// session's write before the move's and so run before it. No release says it could not attribute
// a holder.
func TestAMoveToBacklogReleasesTheClaimOfTheArchitectSessionAFreshSessionReplaced(t *testing.T) {
	t.Parallel()
	const s1, s2 = "ses-3108-first", "ses-3108-second"
	r := newLeftRig(t)
	r.put(record.Issue{Key: leftRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: leftRoot, Title: "root", Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 5})
	architect := r.token(leftRoot, claim.RoleArchitect)
	r.admitPass()
	r.runStarts(leftRoot, claim.RoleArchitect)
	r.bringUp(architect, s1)
	r.dispatch.takes(leftRoot, s1)

	r.failTask(architect, "")
	r.dispatch.ends(s1)
	r.controllerFreshSession(leftRoot, claim.RoleArchitect)
	r.wantLeftBehind(architect, s1)
	r.bringUp(architect, s2)
	r.failTask(architect, "")
	r.dispatch.ends(s2)

	r.move(leftRoot, "backlog")
	if got := r.issue(leftRoot); !record.Stopped(got) {
		t.Fatalf("the root moved to backlog = %+v, want it out of the workflow", got)
	}
	r.releases()
	r.clock.advance(4 * time.Second)
	r.move(leftRoot, "todo")
	r.admitPass()
	if got := r.issue(leftRoot); got.Generation != 2 || record.Stopped(got) {
		t.Fatalf("the root set back to todo = %+v, want generation 2 in the workflow", got)
	}

	var heldAtTheTask *string
	r.conns[architect].beforeNextPrompt(func() {
		held := r.dispatch.holder(leftRoot)
		heldAtTheTask = &held
	})
	r.drain()
	r.bringUp(architect, s2)
	if heldAtTheTask == nil {
		t.Fatalf("the generation-2 architect was sent no task; prompts %+v", r.conns[architect].Prompts())
	}
	if *heldAtTheTask == "session:"+s1 {
		lines := loggedLines(t, r.logs.String(), claimUnattributedLine)
		t.Fatalf("Dispatch still showed %s claimed by %s when the generation-2 architect's first task was sent; the executor logged %q %d times: %v",
			leftRoot, *heldAtTheTask, claimUnattributedLine, len(lines), lines)
	}
	r.wantReleasedBy(leftRoot, "session:"+s1, s1)
	r.wantNoUnattributedLine()
}

// R4 of dispatch://INFRA-3144: a claim held by the session that works a claim of the issue's tree
// now stays, and no claim release takes it; Dispatch is sent no DELETE at all. The move's row that
// runs after a re-admission leaves its holder's claim, since the issue is in the workflow again,
// and records why (`issue-runs`). The left session's row acts only on the session it names: a
// claim the new session took is another holder, which that row judges nothing of, so the record
// keeps the entry it had. And a left session whose name is a claim of the tree's current session
// again, which an operator's release of its names freed and another claim took, is that claim's,
// and the row leaves it (`works-now`).
func TestAClaimReleaseLeavesTheClaimOfTheSessionThatWorksTheIssueNow(t *testing.T) {
	t.Parallel()

	t.Run("the issue is re-admitted before the move's row runs, and its architect works in the session that holds the claim", func(t *testing.T) {
		t.Parallel()
		r := newLeftRig(t)
		tr := leftTreeOf(r, leftRoot)
		r.dispatch.forbid()
		r.dispatch.takes(leftRoot, tr.session)
		r.move(leftRoot, "backlog")
		r.move(leftRoot, "todo")
		r.admitPass()
		if got := r.issue(leftRoot); got.Generation != 2 || record.Stopped(got) {
			t.Fatalf("the root set back to todo = %+v, want generation 2 in the workflow", got)
		}
		r.drain()
		r.bringUp(tr.token, tr.session)
		if got := r.dispatch.holder(leftRoot); got != "session:"+tr.session {
			t.Fatalf("Dispatch shows %s claimed by %q, want the claim of %s, whose architect works the issue now", leftRoot, got, tr.session)
		}
		left := r.claimLeft(leftRoot)
		if left == nil || left["reason"] != string(record.ClaimLeftIssueRuns) || left["holder"] != "session:"+tr.session {
			t.Fatalf("the record of %s names claimLeft %v, want %s left with the reason %s", leftRoot, left, tr.session, record.ClaimLeftIssueRuns)
		}
		if n := r.dispatch.deleted(); n != 0 {
			t.Fatalf("Dispatch was sent %d claim releases, want none", n)
		}
	})

	t.Run("a fresh session leaves a session behind, and the new session claims the issue before the left session's row runs", func(t *testing.T) {
		t.Parallel()
		r := newLeftRig(t)
		tr := leftTreeOf(r, leftRoot)
		r.dispatch.forbid()
		r.dispatch.takes(leftRoot, tr.session)
		left := tr.freshSession()
		r.dispatch.ends(left)
		r.dispatch.takes(leftRoot, tr.session)
		earlier := record.ClaimLeft{Holder: "user:alice", Reason: record.ClaimLeftPerson, At: time.Now().Add(-time.Hour).UTC()}
		r.recordClaimLeft(leftRoot, earlier)
		before := r.claimLeft(leftRoot)
		if before == nil {
			t.Fatalf("the record of %s names no claimLeft after the earlier entry was written", leftRoot)
		}
		r.releases()
		if r.queuedFor(left) != nil {
			t.Fatalf("the release of %s is still queued after its run, want it ended", left)
		}
		if got := r.dispatch.holder(leftRoot); got != "session:"+tr.session {
			t.Fatalf("Dispatch shows %s claimed by %q, want the new session %s's claim kept", leftRoot, got, tr.session)
		}
		if after := r.claimLeft(leftRoot); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Fatalf("the left session's row judged the new session's claim: claimLeft %v, want the earlier entry %v kept", after, before)
		}
		if n := r.dispatch.deleted(); n != 0 {
			t.Fatalf("Dispatch was sent %d claim releases, want none", n)
		}
	})

	t.Run("the left session's name is a claim of the tree's current session again before its row runs", func(t *testing.T) {
		t.Parallel()
		r := newLeftRig(t)
		tr := leftTreeOf(r, leftRoot)
		r.dispatch.forbid()
		r.dispatch.takes(leftRoot, tr.session)
		left := tr.freshSession()
		// The operator frees the names of every session the architect's claim does not run now,
		// and the child's own architect then registers the freed session as its own.
		r.operator(tr.token, "release-names", nil, http.StatusOK)
		child := r.addChild(left)
		if got := r.machine(child).Claim().Session; got != left {
			t.Fatalf("the child's architect records %q, want %s, the freed name", got, left)
		}
		r.releases()
		if got := r.dispatch.holder(leftRoot); got != "session:"+left {
			t.Fatalf("Dispatch shows %s claimed by %q, want %s's claim kept", leftRoot, got, left)
		}
		claimLeft := r.claimLeft(leftRoot)
		if claimLeft == nil || claimLeft["reason"] != string(record.ClaimLeftWorksNow) || claimLeft["holder"] != "session:"+left {
			t.Fatalf("the record of %s names claimLeft %v, want %s left with the reason %s", leftRoot, claimLeft, left, record.ClaimLeftWorksNow)
		}
		if n := r.dispatch.deleted(); n != 0 {
			t.Fatalf("Dispatch was sent %d claim releases, want none", n)
		}
	})
}

// leftPath is one path of the plan's "Which paths leave a session behind" that has an issue: leave
// sets its claim up on a session of its own, has that session claim the claim's issue on Dispatch
// while the issue runs, takes the path, which leaves the session behind, and answers the claim and
// the session it left. The session's process is not running once the path has run.
type leftPath struct {
	name  string
	leave func(r *leftRig) (claim.Token, string)
}

var leftPaths = []leftPath{
	{"startFresh, for a session no store holds any more (SessionLossGone)", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-gone")
		r.dispatch.takes(leftRoot, "ses-gone")
		r.operator(token, "suspend", nil, http.StatusOK, http.StatusAccepted)
		r.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume ses-gone: %w", runtime.ErrSessionFileMissing)})
		r.operator(token, "resume", nil, http.StatusOK, http.StatusAccepted)
		return token, "ses-gone"
	}},
	{"startFresh, for a session the pod's launcher could not look up by its id (SessionLossNotResumedByID)", func(r *leftRig) (claim.Token, string) {
		token := r.token(leftRoot, claim.RoleImplementer)
		r.implementingRecord()
		r.seedClaim(supervise.Claim{Token: token, Project: "legion", Tree: leftRoot, Issue: leftRoot, Role: claim.RoleImplementer,
			State: supervise.StateQueued, Session: "ses-by-id", AgentResumeGeneration: 1})
		r.dispatch.takes(leftRoot, "ses-by-id")
		r.rt.RefuseLookupByID = true
		r.must(token, supervise.RequestSpawn{Claim: token})
		return token, "ses-by-id"
	}},
	{"startFresh, for a Claude Code session recorded with no transcript file (SessionLossClaudeNoFile)", func(r *leftRig) (claim.Token, string) {
		token := r.token(leftRoot, claim.RoleImplementer)
		r.implementingRecord()
		r.specs.mu.Lock()
		r.specs.claude[token] = true
		r.specs.mu.Unlock()
		r.seedClaim(supervise.Claim{Token: token, Project: "legion", Tree: leftRoot, Issue: leftRoot, Role: claim.RoleImplementer,
			State: supervise.StateQueued, Session: "ses-claude"})
		r.dispatch.takes(leftRoot, "ses-claude")
		r.must(token, supervise.RequestSpawn{Claim: token})
		return token, "ses-claude"
	}},
	{"retry's fresh session, from RequestRetry{FreshSession} of a RestartPhase fact", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-restarted")
		r.dispatch.takes(leftRoot, "ses-restarted")
		r.operator(token, "stop", nil, http.StatusOK)
		r.apply("restart-fresh", intake.RestartPhase{Issue: leftRoot, Role: claim.RoleImplementer, FreshSession: true})
		r.runStarts(leftRoot, claim.RoleImplementer)
		return token, "ses-restarted"
	}},
	{"retry's fresh session, from RequestAskedRetry{FreshSession}", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-asked")
		r.dispatch.takes(leftRoot, "ses-asked")
		r.operator(token, "stop", nil, http.StatusOK)
		r.must(token, supervise.RequestAskedRetry{Claim: token, FreshSession: true, By: supervise.SuspendedByOperator})
		return token, "ses-asked"
	}},
	{"retry's fresh session, of a claim held for OpenAI's cyber-policy refusal", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-cyber")
		r.deliver(token)
		r.dispatch.takes(leftRoot, "ses-cyber")
		r.failTask(token, claim.TurnCodeCyberPolicy)
		if got := r.machine(token).Claim(); !got.HeldForCyberPolicy() {
			r.t.Fatalf("%s failed %q (%s), want it held for the cyber-policy refusal", token, got.FailureCause, got.Failure)
		}
		if got := r.issue(leftRoot); got.Phase != phase.Held {
			r.t.Fatalf("the root after its implementer's failure is %s, want it held for its architect", got.Phase)
		}
		r.apply("architect-retry", intake.RetryOrEscalate{Issue: leftRoot, Decision: intake.RetryDecision})
		r.runStarts(leftRoot, claim.RoleImplementer)
		return token, "ses-cyber"
	}},
	{"the table's forgetSession, from the operator's legion claims forget-session", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-forgotten")
		r.dispatch.takes(leftRoot, "ses-forgotten")
		r.operator(token, "stop", nil, http.StatusOK)
		r.operator(token, "forget-session", api.ForgetSessionRequest{Session: "ses-forgotten"}, http.StatusOK)
		return token, "ses-forgotten"
	}},
	{"the table's forgetSession, from the operator's legion claims retry --fresh-session", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-retried")
		r.dispatch.takes(leftRoot, "ses-retried")
		r.operator(token, "stop", nil, http.StatusOK)
		r.operator(token, "retry", api.RetryRequest{FreshSession: true}, http.StatusOK, http.StatusAccepted)
		return token, "ses-retried"
	}},
	{"the table's forgetSession, from the controller's fresh_session", func(r *leftRig) (claim.Token, string) {
		token := r.implementing("ses-controller-fresh")
		r.dispatch.takes(leftRoot, "ses-controller-fresh")
		r.controllerFreshSession(leftRoot, claim.RoleImplementer)
		return token, "ses-controller-fresh"
	}},
}

// implementingRecord records leftRoot implementing at generation 1, holding its slot, with no
// claim launched: a path that seeds its claim as it stands.
func (r *leftRig) implementingRecord() {
	r.t.Helper()
	r.put(record.Issue{Key: leftRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: leftRoot, Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true,
		LastDispatchSeq: 5})
	holdSlot(r.t, r.pool, leftRoot)
}

// leftControllerPath is one path of the plan's list that runs for the project's controller alone,
// whose claim has no issue: take sets the controller up and takes the path, and answers the session
// it left behind.
type leftControllerPath struct {
	name string
	take func(r *leftRig) string
}

// controller creates the daemon's own controller claim c, running a process the fake runtime
// started where c is not failed, with its machine's session age maxAge.
func (r *leftRig) controller(c supervise.Claim, maxAge time.Duration) claim.Token {
	r.t.Helper()
	token := claim.ControllerToken("legion")
	r.sup.deps.ControllerSessionMaxAge = maxAge
	c.Token, c.Project, c.Role, c.Generation, c.SessionFile = token, "legion", claim.RoleController, 1, leftSessionFile(c.Session)
	if c.State != supervise.StateFailed {
		c.Locator = &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "31999:999999",
			Tmux: &runtime.TmuxLocator{Window: "@99", Pane: "%99"}}
	}
	conn := &leftConn{Conn: fake.NewConn()}
	r.conns[token] = conn
	r.sup.deps.Conns.(*fake.Conns).Register(token, conn)
	holdSessionName(r.t, r.pool, "legion", c.Session, string(token))
	if _, _, err := r.sup.Create(r.ctx, c, ""); err != nil {
		r.t.Fatalf("create the controller's claim: %v", err)
	}
	return token
}

// controllerDies is the runtime reporting the controller's process gone, which its machine
// relaunches.
func (r *leftRig) controllerDies(token claim.Token) {
	r.t.Helper()
	c := r.machine(token).Claim()
	r.must(token, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *c.Locator, Kind: runtime.Gone, At: time.Now()}})
}

var leftControllerPaths = []leftControllerPath{
	{"replaceEndedSession, the controller's second launch in a row ended before it finished a turn", func(r *leftRig) string {
		const old = "ses-controller-ended"
		token := r.controller(supervise.Claim{State: supervise.StateIdle, Session: old, LastTurnStart: time.Now().Add(-2 * time.Hour),
			LastTurnEnd: time.Now().Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true, Ends: 1,
				Endings: []string{"its process died after the controller was ready: exit 1"}}}, 0)
		r.controllerDies(token)
		r.wantLeftBehind(token, old)
		return old
	}},
	{"replaceAgedSession, the controller's session older than its age at a relaunch", func(r *leftRig) string {
		old := sessionAged(time.Now(), 73*time.Hour)
		token := r.controller(supervise.Claim{State: supervise.StateIdle, Session: old, LastTurnStart: time.Now().Add(-2 * time.Hour),
			LastTurnEnd: time.Now().Add(-time.Hour), SessionRun: supervise.SessionRun{TurnFinished: true}}, 72*time.Hour)
		r.controllerDies(token)
		r.wantLeftBehind(token, old)
		return old
	}},
	{"the controller keeper's RequestRetry{FreshSession: true}, the resume of its session held for another claim", func(r *leftRig) string {
		const taken = "ses-controller-taken"
		holdSessionName(r.t, r.pool, "legion", taken, string(mustClaimToken(r.t, "LEGION-943", claim.RoleImplementer)))
		token := r.controller(supervise.Claim{State: supervise.StateFailed, Session: taken}, 0)
		k := newControllerKeeper(r.ctx, r.sup, "legion", time.Minute, 0, quietLogger())
		clock := time.Now()
		k.now = func() time.Time { return clock }
		for range 2 {
			if err := k.keep(); err != nil {
				r.t.Fatalf("the keeper's look: %v", err)
			}
			clock = clock.Add(time.Hour)
		}
		r.settle()
		r.wantLeftBehind(token, taken)
		return taken
	}},
}

// R5 of dispatch://INFRA-3144: when the daemon leaves a session behind for a claim, by a fresh
// session or by any other start in a new session, it releases the Dispatch claim that session
// holds on the claim's issue. Every path of the plan's "Which paths leave a session behind" that
// has an issue is a row: startFresh for each of its three causes, retry's fresh session from a
// RestartPhase fact, from an asked retry and for a claim held for a cyber-policy refusal, and the
// table's forget from the operator's two commands and the controller's fresh_session. On each, the
// left session holds the claim of the claim's issue while the issue runs, its process has ended
// once the path has run, and the claim release the path's claim write queued takes the claim from
// it. A person's claim, and the new session's, are another holder, and stay. The three paths that
// run for the project's controller alone leave a session of a claim on no issue, and queue no
// claim release at all.
func TestASessionTheDaemonLeavesBehindHasItsClaimReleased(t *testing.T) {
	t.Parallel()
	for _, path := range leftPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			r := newLeftRig(t)
			token, left := path.leave(r)
			r.wantLeftBehind(token, left)
			if got := r.issue(leftRoot); record.Stopped(got) {
				t.Fatalf("the path took %s out of the workflow (%+v), want it running", leftRoot, got)
			}
			if got := r.dispatch.holder(leftRoot); got != "session:"+left {
				t.Fatalf("Dispatch shows %s claimed by %q after the path, want %s's claim", leftRoot, got, left)
			}
			if r.queuedFor(left) == nil {
				t.Fatalf("the path queued no claim release naming %s; queued %+v", left, r.queuedReleases())
			}
			r.dispatch.ends(left)
			r.releases()
			r.wantReleasedBy(leftRoot, "session:"+left, left)
			if left := r.claimLeft(leftRoot); left != nil {
				t.Fatalf("the record of %s names claimLeft %v after the release took the claim, want none", leftRoot, left)
			}
		})
	}

	for _, tc := range []struct {
		name string
		// holds has another holder take the claim from the left session once it is gone, and
		// answers that holder.
		holds func(r *leftRig, token claim.Token) string
	}{
		{"a person's claim stays", func(r *leftRig, _ claim.Token) string {
			r.dispatch.forces(leftRoot, "alice")
			return "user:alice"
		}},
		{"the new session's claim stays", func(r *leftRig, token claim.Token) string {
			r.bringUp(token, "ses-new-agent")
			r.dispatch.takes(leftRoot, "ses-new-agent")
			return "session:ses-new-agent"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLeftRig(t)
			token := r.implementing("ses-held-before")
			r.dispatch.takes(leftRoot, "ses-held-before")
			r.controllerFreshSession(leftRoot, claim.RoleImplementer)
			r.wantLeftBehind(token, "ses-held-before")
			r.dispatch.ends("ses-held-before")
			holder := tc.holds(r, token)
			r.dispatch.forbid()
			r.releases()
			if got := r.dispatch.holder(leftRoot); got != holder {
				t.Fatalf("Dispatch shows %s claimed by %q after the left session's release ran, want %s's claim kept", leftRoot, got, holder)
			}
			if r.queuedFor("ses-held-before") != nil {
				t.Fatalf("the release of ses-held-before is still queued after its run, want it ended")
			}
		})
	}

	for _, path := range leftControllerPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			r := newLeftRig(t)
			left := path.take(r)
			if queued := r.queuedReleases(); len(queued) != 0 {
				t.Fatalf("the controller's claim left %s behind and queued the claim releases %+v, want none: its claim has no issue", left, queued)
			}
		})
	}
}
