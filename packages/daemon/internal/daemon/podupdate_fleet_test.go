package daemon

// The fleet-wide update round (INFRA-419, folded into INFRA-484): one change to the pod template —
// the deployment's session store here — drifts the pod of every issue of the project at once. Each
// issue's pod is a round of its own: it is replaced only once its own workers have answered, and
// an answer in one issue's pod replaces no other issue's. Nothing of the round suspends a root
// architect, parks an issue or sends a root to await a person, and the daemon goes on serving every
// claim in an open round — a worker tasked to push its work completes its phase through the same
// route it would on any other day (api's handoff/complete).
//
// The rig is podupdate_test.go's over two issues rather than one: real machines over a real
// Postgres with the runtime faked, ticked by hand one sweep at a time. The completion is driven
// through the daemon's own HTTP handler over that same store, with the workflow engine behind it,
// so what accepts it is the route the `legion` tool posts to and not a test's stand-in.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// The two issues the one template change drifts at once: each a root of its own, each with its
	// own architect, its own phase workers and its own pod.
	fleetIssueA = "LEGION-419"
	fleetIssueB = "LEGION-420"
	fleetPodA   = "6b1d0c4e-7a92-4c1f-8d35-2f6b9c0ad417"
	fleetPodB   = "c8f52a71-04b3-4de6-9a7c-15d8e3b6027f"
	// fleetDrift is what the runtime answers for every pod of the fleet after the one change: the
	// deployment moved the session store its pods are created under, which no running pod can be
	// changed to carry (the Sandbox runtime's movedSessionStore).
	fleetDrift = "the session store volume, now postgres key=session-dsn"
)

// fleetStart is where a fleet rig's clock stands when a test begins, and what a round seeded on
// its claims records as its own clock.
var fleetStart = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

// fleetIssue is one issue the fleet round covers: its key, the phase its record stands in, the pod
// every role of it runs in, and the roles that run there. Every role is a registered worker on
// this daemon's contract, which is what a fleet-wide drift finds, but for the roles the two
// lists below name.
//
// unregistered is each role seeded launching in the issue's pod with no session of its own, as a
// role whose process has started and whose agent has not registered yet. Such a claim blocks no
// round (blockingClaims), so the sweep's ask never reaches it, and it records the issue's pod
// all the same. failed is each role seeded failed — its launches or prompts ran out — with the
// session its agent registered and no locator, as supervise's fail leaves one: such a claim
// blocks the round (blockingClaims) and the round asks it nothing (askedClaims), because its
// machine ignores the round's post (supervise's table).
type fleetIssue struct {
	key          string
	phase        phase.Phase
	pod          string
	roles        []claim.Role
	unregistered []claim.Role
	failed       []claim.Role
}

// fleetClaim names one claim of the fleet: the issue it is of and the role it runs.
type fleetClaim struct {
	issue string
	role  claim.Role
}

type fleetRig struct {
	t       *testing.T
	ctx     context.Context
	s       *supervision
	sup     *supervisor
	rt      *fake.Runtime
	pool    *store.Pool
	records record.Store
	clock   *heldClock
	grants  *credential.Grants
	// api is the daemon's own HTTP handler over the same store: the completion route under test.
	api http.Handler
	// handoffs is the Dispatch that route reads a completion's handoff record back from.
	handoffs *fleetHandoffs
	tokens   map[fleetClaim]claim.Token
	// workers is the connection each claim's agent is reached over, which records every prompt
	// the daemon sends it.
	workers map[claim.Token]*fake.Conn
	pods    map[string]string
	// noPod is every issue whose claims a sweep reads as recording no pod (podless).
	noPod map[string]bool
	// wire is what the rig set on its supervisor's Deps before any machine was made
	// (newWiredFleetRig), nil for none; a restart sets it again on the supervisor it builds.
	wire fleetWiring
}

// fleetWiring sets a dependency of the daemon's on a fleet rig's supervisor before the rig
// restores its claims. A machine takes its Deps when it is made (supervise.NewMachine), so a
// dependency set after the rig is built reaches none of its machines.
type fleetWiring func(sup *supervisor)

// adoptingAsTheDaemonDoes wires the two dependencies the daemon gives its supervisor for the
// working copy an issue's roles share (daemon.go): an App identity per role, under which a phase
// task's send adopts that working copy first (supervise's adopts), and the daemon's own answer to
// which claim of an issue is in a turn (claimInTurn), which holds such a send while another role
// works there. The identity stands in for the one adoptIdentity mints, named for the role's App,
// as newIssueRig's is.
func adoptingAsTheDaemonDoes(sup *supervisor) {
	sup.deps.Identity = func(_ context.Context, role claim.Role, _ string) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{Name: "legion-" + string(role) + "[bot]", Email: string(role) + "@example.invalid"}, nil
	}
	sup.deps.IssueInTurn = sup.claimInTurn
}

func newFleetRig(t *testing.T, issues ...fleetIssue) *fleetRig {
	t.Helper()
	return newWiredFleetRig(t, nil, issues...)
}

// newWiredFleetRig is newFleetRig with wire set on the supervisor's Deps before the seeded claims
// are restored, nil for nothing.
func newWiredFleetRig(t *testing.T, wire fleetWiring, issues ...fleetIssue) *fleetRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	tokens := map[fleetClaim]claim.Token{}
	pods := map[string]string{}
	for _, issue := range issues {
		pods[issue.key] = issue.pod
		claims := make([]supervise.Claim, 0, len(issue.roles))
		for _, role := range issue.roles {
			token := mustClaimToken(t, issue.key, role)
			tokens[fleetClaim{issue: issue.key, role: role}] = token
			seeded := supervise.Claim{
				Token: token, Project: podUpdateProject, Tree: issue.key, Issue: issue.key, Role: role,
				Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion,
				Session: "ses_" + issue.key + "_" + string(role), SessionFile: "/sessions/" + issue.key + "-" + string(role) + ".jsonl",
				CapabilityHash: []byte(issue.key + ":" + string(role)), ServingGeneration: 1,
				Locator: podUpdateLocator(token, issue.key, issue.pod),
			}
			switch {
			case slices.Contains(issue.unregistered, role):
				seeded.State, seeded.Session, seeded.SessionFile = supervise.StateLaunching, "", ""
				seeded.PluginContract, seeded.CapabilityHash, seeded.ServingGeneration = 0, nil, 0
			case slices.Contains(issue.failed, role):
				seeded.State, seeded.Locator = supervise.StateFailed, nil
			}
			claims = append(claims, seeded)
		}
		seedWorkflowTree(t, dsn, podUpdateProject, record.Issue{
			Key: issue.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: issue.key, Title: "The fleet round", Phase: issue.phase,
			Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
		}, claims...)
	}

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	clock := &heldClock{at: fleetStart}
	rt := fake.NewRuntime()
	rt.Now = clock.Now
	conns := fake.NewConns()
	sup := newSupervisor(ctx, st, podUpdateProject, t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: st, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:          supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:        supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract:  api.DaemonAPIVersion,
		PodUpdateRetask: podUpdateRetask,
		RefreshGate:     supervise.NewRefreshGate(0),
	}
	if wire != nil {
		wire(sup)
	}
	t.Cleanup(sup.stop)
	stored, err := sup.Claims(ctx)
	if err != nil {
		t.Fatalf("read the seeded claims: %v", err)
	}
	if _, err := sup.restore(ctx, stored); err != nil {
		t.Fatalf("restore the seeded claims: %v", err)
	}
	workers := map[claim.Token]*fake.Conn{}
	for _, token := range tokens {
		workers[token] = fake.NewConn()
		conns.Register(token, workers[token])
	}
	records := record.NewStore()
	rig := &fleetRig{t: t, ctx: ctx, sup: sup, rt: rt, pool: pool, records: records, clock: clock,
		grants: credential.New(nil), handoffs: &fleetHandoffs{handoffs: map[string]dispatch.Handoff{}},
		tokens: tokens, workers: workers, pods: pods, noPod: map[string]bool{}, wire: wire}
	rig.s = &supervision{
		cfg: config.Config{Project: "LEGION", PodUpdateRetask: podUpdateRetask, PodUpdateNotice: podUpdateNotice},
		log: quietLogger(), plan: plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
		runtime: rt, supervisor: sup,
		enqueue:     (&workflowRuntime{pool: pool, records: records, log: quietLogger()}).enqueueOutbox,
		refreshGate: supervise.NewRefreshGate(0),
	}
	engine := workflow.New(records, workflow.Config{Project: podUpdateProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	rig.api = api.NewServer("127.0.0.1", 0, api.Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: sup, Project: podUpdateProject, DispatchProject: "LEGION", PluginRelaunchable: true,
		Pool: pool, Handlers: []intake.Handler{engine}, Record: records, Grants: rig.grants, Log: quietLogger(),
		Dispatch: rig.handoffs,
	}).Handler
	return rig
}

// drifts scripts the runtime to answer reasons for every claim of issue, which is what one
// template change leaves on every pod built before it.
func (r *fleetRig) drifts(issue string, reasons ...string) {
	r.t.Helper()
	for key, token := range r.tokens {
		if key.issue == issue {
			r.rt.ScriptPodDrift(token, reasons...)
		}
	}
}

// tick is one orphan sweep over every claim the store holds, which is what the daemon's own tick
// puts to the round. The claims of an issue podless says records no pod are read with none, as
// every sweep between an operator's suspension and the next launch reads them (supervise's
// letGo).
func (r *fleetRig) tick() {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	for i := range claims {
		if r.noPod[claims[i].Issue] {
			claims[i].Locator = nil
		}
	}
	r.s.reconcilePodUpdates(r.ctx, claims)
}

// podless makes every later sweep read the claims of issue as recording no pod, which is what a
// suspension leaves behind and what a claim minutes into a relaunch records.
func (r *fleetRig) podless(issue string) {
	r.t.Helper()
	r.noPod[issue] = true
}

// restart is a second daemon over the same Postgres: new machines, a new runtime, and nothing of
// the first daemon's memory, which is where the sweep keeps the openings it has made
// (reconcilePodUpdates). The new runtime has been scripted no drift, as a daemon that boots
// knows nothing of what the one before it read, so a caller that wants an issue still drifted
// scripts it again.
func (r *fleetRig) restart() *fleetRig {
	r.t.Helper()
	r.sup.stop()
	st, err := store.Open(r.ctx, r.pool.Config().ConnString())
	if err != nil {
		r.t.Fatalf("open the store again: %v", err)
	}
	r.t.Cleanup(st.Close)
	rt := fake.NewRuntime()
	rt.Now = r.clock.Now
	conns := fake.NewConns()
	sup := newSupervisor(r.ctx, st, podUpdateProject, r.t.TempDir(), quietLogger())
	sup.deps = r.sup.deps
	sup.deps.Runtime, sup.deps.Conns, sup.deps.Store = rt, conns, st
	// The copy carries the first supervisor's wiring, whose answers read that supervisor's
	// machines, so it is set again over this one's.
	if r.wire != nil {
		r.wire(sup)
	}
	r.t.Cleanup(sup.stop)
	stored, err := sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	workers := map[claim.Token]*fake.Conn{}
	for _, token := range r.tokens {
		workers[token] = fake.NewConn()
		conns.Register(token, workers[token])
	}
	next := &fleetRig{t: r.t, ctx: r.ctx, sup: sup, rt: rt, pool: r.pool, records: r.records, clock: r.clock,
		grants: r.grants, handoffs: r.handoffs, tokens: r.tokens, workers: workers, pods: r.pods,
		noPod: maps.Clone(r.noPod), wire: r.wire}
	next.s = &supervision{
		cfg: r.s.cfg, log: quietLogger(), plan: r.s.plan, runtime: rt, supervisor: sup, enqueue: r.s.enqueue,
		refreshGate: supervise.NewRefreshGate(0),
	}
	engine := workflow.New(r.records, workflow.Config{Project: podUpdateProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	next.api = api.NewServer("127.0.0.1", 0, api.Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: sup, Project: podUpdateProject, DispatchProject: "LEGION", PluginRelaunchable: true,
		Pool: r.pool, Handlers: []intake.Handler{engine}, Record: r.records, Grants: next.grants, Log: quietLogger(),
		Dispatch: next.handoffs,
	}).Handler
	return next
}

func (r *fleetRig) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, ok := r.tokens[fleetClaim{issue: issue, role: role}]
	if !ok {
		r.t.Fatalf("%s has no %s claim in this rig", issue, role)
	}
	return token
}

// claimOf is the claim as its machine holds it, which is what the next tick reads from the store.
func (r *fleetRig) claimOf(issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	return r.machineOf(issue, role).Claim()
}

// round is the pod round the sweep opens over issue.
func (r *fleetRig) round(issue string) string {
	r.t.Helper()
	return supervise.PodRound(r.pods[issue])
}

// awaitRound waits for the round of issue to be recorded on role's claim with the task it sends,
// which the sweep's posts reach through each machine's own queue.
func (r *fleetRig) awaitRound(issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	want := r.round(issue)
	testwait.Eventually(r.t, fmt.Sprintf("%s's %s to hold the update round %s", issue, role, want), func() bool {
		c := r.claimOf(issue, role)
		return c.UpdateRound == want && c.UpdateTaskedAt != nil
	})
	return r.claimOf(issue, role)
}

// signal is the worker answering ready_for_update, as api's claims/ready-for-update posts it.
func (r *fleetRig) signal(issue string, role claim.Role, head string) {
	r.t.Helper()
	m := r.machineOf(issue, role)
	c := m.Claim()
	if err := m.Handle(r.ctx, supervise.PodUpdateSignalled{
		Claim: c.Token, Generation: c.Generation, Session: c.Session, Round: c.UpdateRound, Head: head,
	}); err != nil {
		r.t.Fatalf("%s's %s answered the update round: %v", issue, role, err)
	}
}

// handle drives one event into the machine of issue's role and waits for its decision, as a
// source that hands a machine its event directly does.
func (r *fleetRig) handle(issue string, role claim.Role, ev supervise.Event) {
	r.t.Helper()
	if err := r.machineOf(issue, role).Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("%s's %s handled %T: %v", issue, role, ev, err)
	}
}

// sent waits for every send the machine of issue's role has in flight to be over, the prompt's
// acknowledgement or refusal decided with it. A decision that sends runs the prompt on a
// goroutine of its own (supervise's startSend), so what the agent was sent is read only once this
// has returned.
func (r *fleetRig) sent(issue string, role claim.Role) {
	r.t.Helper()
	r.machineOf(issue, role).Wait()
}

func (r *fleetRig) machineOf(issue string, role claim.Role) *supervise.Machine {
	r.t.Helper()
	m, ok := r.sup.Machine(r.token(issue, role))
	if !ok {
		r.t.Fatalf("%s's %s is not supervised", issue, role)
	}
	return m
}

// prompts is every prompt the daemon sent the agent of issue's role, in order.
func (r *fleetRig) prompts(issue string, role claim.Role) []fake.Prompt {
	r.t.Helper()
	return r.workers[r.token(issue, role)].Prompts()
}

// fleetHandoffs is the Dispatch the completion route reads a handoff record back from: the
// records the rig's workers wrote with `dispatch handoff-write`, by id. Nothing else of Dispatch
// is reached in a round, so every other read answers nothing.
type fleetHandoffs struct {
	mu       sync.Mutex
	handoffs map[string]dispatch.Handoff
}

// write is one worker writing version 1 of its phase's handoff under its own session, and
// answers the record's id. A second write of the same phase by the same session is the same
// record, as a worker that completes twice names the one record it wrote.
func (d *fleetHandoffs) write(issue, word, session string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := fmt.Sprintf("handoff-%s-%s-v1", issue, word)
	d.handoffs[id] = dispatch.Handoff{ID: id, IssueKey: issue, Phase: word, Version: 1,
		Author: dispatch.HandoffAuthor{Kind: "session", ID: session}}
	return id
}

func (d *fleetHandoffs) Handoff(_ context.Context, id string) (dispatch.Handoff, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	handoff, ok := d.handoffs[id]
	if !ok {
		return dispatch.Handoff{}, &dispatch.Error{Status: http.StatusNotFound, Code: "HANDOFF_NOT_FOUND"}
	}
	return handoff, nil
}

func (d *fleetHandoffs) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *fleetHandoffs) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *fleetHandoffs) SetStatus(context.Context, string, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *fleetHandoffs) SetStatusAt(context.Context, dispatch.Issue, string) (dispatch.Issue, error) {
	return dispatch.Issue{}, nil
}
func (d *fleetHandoffs) HandOver(context.Context, dispatch.Issue, string) error    { return nil }
func (d *fleetHandoffs) ReleaseClaim(context.Context, string) error                { return nil }
func (d *fleetHandoffs) PostMessage(context.Context, string, string) error         { return nil }
func (d *fleetHandoffs) WebhookAppInstalled(context.Context, string) (bool, error) { return true, nil }
func (d *fleetHandoffs) PutGatedSteps(context.Context, string, []string) error     { return nil }
func (d *fleetHandoffs) PutDesignGates(context.Context, string, []dispatch.DesignGateDocument) error {
	return nil
}
func (d *fleetHandoffs) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return nil, nil
}
func (d *fleetHandoffs) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, nil
}
func (d *fleetHandoffs) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, nil
}
func (d *fleetHandoffs) ListRepoProjects(context.Context) ([]dispatch.RepoProject, error) {
	return nil, nil
}

// complete is one worker's phase completion through the daemon's own route, with a grant minted
// for its claim as the grants route mints one. A phase that ends with a handoff record has one
// written on the Dispatch issue first, under the claim's own session, as the worker's
// `dispatch handoff-write` writes it, and the completion names that record. It returns the status
// and the body.
func (r *fleetRig) complete(issue string, role claim.Role, summary string) (int, string) {
	r.t.Helper()
	c := r.claimOf(issue, role)
	grant, err := r.grants.Mint(c)
	if err != nil {
		r.t.Fatalf("mint a grant for %s's %s: %v", issue, role, err)
	}
	stood := readIssue(r.t, r.pool, issue)
	if stood == nil {
		r.t.Fatalf("%s is not recorded", issue)
	}
	word, writer, ok := phase.Handoff(stood.Phase)
	if !ok || writer != role {
		r.t.Fatalf("%s stands at %s, whose handoff is not the %s's: this rig completes a phase its worker writes the handoff of", issue, stood.Phase, role)
	}
	body, err := json.Marshal(api.HandoffCompleteRequest{GrantID: grant.ID, Summary: summary,
		HandoffID: r.handoffs.write(issue, word, c.Session), HandoffVersion: 1})
	if err != nil {
		r.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/legion/v1/handoff/complete", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	r.api.ServeHTTP(recorder, request)
	return recorder.Code, strings.TrimSpace(recorder.Body.String())
}

// replacementsOf is every ReplacePod the sweep made over pod, in order.
func (r *fleetRig) replacementsOf(pod string) []fake.Call {
	r.t.Helper()
	var of []fake.Call
	for _, call := range r.rt.CallsOf("ReplacePod") {
		if call.Pod.Locator == nil || call.Pod.Locator.Sandbox == nil {
			r.t.Fatalf("ReplacePod was called with %+v, want a process of a pod", call.Pod)
		}
		if call.Pod.Locator.Sandbox.PodUID == pod {
			of = append(of, call)
		}
	}
	return of
}

// replacements is every ReplacePod the sweep made, over any pod.
func (r *fleetRig) replacements() []fake.Call { return r.rt.CallsOf("ReplacePod") }

// controllerNotices is every controller notice the sweep and the workflow wrote, oldest first.
func (r *fleetRig) controllerNotices() []record.ControllerNotice {
	r.t.Helper()
	var notices []record.ControllerNotice
	for _, row := range r.rows(record.OutboxKindControllerNotice) {
		var notice record.ControllerNotice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode controller notice %d: %v", row.ID, err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// statusWrites is every Dispatch status the workflow queued, oldest first, with the issue each is
// of: a park is one of them, the backlog a leaving issue is written to.
func (r *fleetRig) statusWrites() map[string]string {
	r.t.Helper()
	writes := map[string]string{}
	for _, row := range r.rows(record.OutboxKindDispatchStatus) {
		var write record.StatusWrite
		if err := json.Unmarshal(row.Payload, &write); err != nil {
			r.t.Fatalf("decode status write %d: %v", row.ID, err)
		}
		writes[row.Issue] = write.Status
	}
	return writes
}

// rows is every outbox row of kind, oldest first, whatever issue it is of.
func (r *fleetRig) rows(kind record.OutboxKind) []record.OutboxRow {
	r.t.Helper()
	query, err := r.pool.Query(r.ctx, "select id, kind, issue, payload from outbox where kind = $1 order by id", string(kind))
	if err != nil {
		r.t.Fatalf("read the %s rows: %v", kind, err)
	}
	defer query.Close()
	var rows []record.OutboxRow
	for query.Next() {
		var row record.OutboxRow
		if err := query.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("read a %s row: %v", kind, err)
		}
		rows = append(rows, row)
	}
	if err := query.Err(); err != nil {
		r.t.Fatalf("read the %s rows: %v", kind, err)
	}
	return rows
}

// assertNoRootLeftTheWorkflow is the fleet round's second promise: the round moves no root out of
// the workflow. No root waits on a person, none is parked — its phase done at backlog, or a
// Dispatch status write that would park it — and no architect of either issue is suspended or
// holds a suspension for its turn. The controller is told of no root waiting on a person either.
func (r *fleetRig) assertNoRootLeftTheWorkflow(issues ...string) {
	r.t.Helper()
	writes := r.statusWrites()
	for _, issue := range issues {
		got := readIssue(r.t, r.pool, issue)
		if got == nil {
			r.t.Fatalf("%s is not recorded", issue)
		}
		if got.Phase == phase.AwaitingPerson || got.Awaiting != nil {
			r.t.Errorf("%s is at phase %s waiting on %+v; the round sends no root to a person", issue, got.Phase, got.Awaiting)
		}
		if got.Phase == phase.Done || got.Status == "backlog" {
			r.t.Errorf("%s is parked: phase %s, status %s", issue, got.Phase, got.Status)
		}
		if status, written := writes[issue]; written && status != "in_progress" {
			r.t.Errorf("the round queued the Dispatch status %q for %s; it parks no issue", status, issue)
		}
		architect := r.claimOf(issue, claim.RoleArchitect)
		if architect.State == supervise.StateSuspended || architect.SuspendedBy != "" || architect.SuspensionHeld {
			r.t.Errorf("%s's architect is %s, suspended by %q (held %t); the round suspends no root",
				issue, architect.State, architect.SuspendedBy, architect.SuspensionHeld)
		}
	}
	for _, notice := range r.controllerNotices() {
		if notice.Kind == record.AwaitingPersonNotice {
			r.t.Errorf("the controller was told %+v; no root of the round waits on a person", notice)
		}
	}
}

// One template change drifts every pod of the fleet at once, and each issue's pod is still its own
// round: the pod of the issue whose workers have answered is replaced, and no other. The claims of
// the issue that has not finished answering keep their round, their pod and their processes, and
// their pod is replaced only once the last of them answers.
//
// The round refuses no completion while it waits, either: a worker tasked to push its work and
// answer completes its phase through the daemon's own route, the issue moves on, and the round it
// is in stays open — the worker pushes, reports and is replaced after, which is what the round is
// for. And nothing of any of it moves a root out of the workflow: no root waits on a person, none
// is parked, and no architect is suspended.
func TestPodUpdateReplacesEachIssuesPodOnlyAfterItsOwnWorkersAnswer(t *testing.T) {
	rig := newFleetRig(t,
		fleetIssue{key: fleetIssueA, phase: phase.Implementing, pod: fleetPodA,
			roles: []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester}},
		fleetIssue{key: fleetIssueB, phase: phase.Planning, pod: fleetPodB,
			roles: []claim.Role{claim.RoleArchitect, claim.RolePlanner}},
	)
	rig.drifts(fleetIssueA, fleetDrift)
	rig.drifts(fleetIssueB, fleetDrift)

	rig.tick()

	for _, c := range []fleetClaim{
		{fleetIssueA, claim.RoleArchitect}, {fleetIssueA, claim.RoleImplementer}, {fleetIssueA, claim.RoleTester},
		{fleetIssueB, claim.RoleArchitect}, {fleetIssueB, claim.RolePlanner},
	} {
		got := rig.awaitRound(c.issue, c.role)
		if got.UpdateReasons != fleetDrift {
			t.Errorf("%s's %s was tasked for %q, want the one template change %q", c.issue, c.role, got.UpdateReasons, fleetDrift)
		}
		if got.Pending == nil || !strings.HasPrefix(got.Pending.ID, "update:"+rig.round(c.issue)+":") {
			t.Errorf("%s's %s holds %+v, want its own round's task", c.issue, c.role, got.Pending)
		}
	}
	if a, b := rig.round(fleetIssueA), rig.round(fleetIssueB); a == b {
		t.Fatalf("both issues are in round %s; each issue's pod is a round of its own", a)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before any worker answered", len(calls))
	}

	// A worker tasked by the round completes its phase through the daemon's own HTTP route, with
	// the workflow engine behind it, while both rounds wait.
	status, body := rig.complete(fleetIssueB, claim.RolePlanner, "planned; pushed before the pod goes")
	if status != http.StatusOK {
		t.Fatalf("the planner's completion in an open round = %d %s, want 200: the round refuses no completion", status, body)
	}
	if moved := readIssue(t, rig.pool, fleetIssueB); moved == nil || moved.Phase != phase.Implementing {
		t.Fatalf("%s after its planner's completion = %+v, want its phase moved on to implementing", fleetIssueB, moved)
	}
	if c := rig.claimOf(fleetIssueB, claim.RolePlanner); c.UpdateRound != rig.round(fleetIssueB) {
		t.Fatalf("the planner's round = %q after its completion, want it still open: the completion ends no round", c.UpdateRound)
	}
	rig.assertNoRootLeftTheWorkflow(fleetIssueA, fleetIssueB)

	// One worker of the second issue answers. Neither pod may go: its own round still waits on its
	// architect, and the first issue's workers have answered nothing.
	rig.signal(fleetIssueB, claim.RolePlanner, "b1b1b1b")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times on one worker of %s answering", len(calls), fleetIssueB)
	}

	// The first issue's workers all answer. Its pod is replaced; the second issue's is not, and its
	// claims keep the round they are in.
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester} {
		rig.signal(fleetIssueA, role, "a1a1a1a")
	}
	rig.tick()

	if calls := rig.replacementsOf(fleetPodA); len(calls) != 1 {
		t.Fatalf("ReplacePod over %s's pod called %d times once its workers answered, want once", fleetIssueA, len(calls))
	}
	if calls := rig.replacementsOf(fleetPodB); len(calls) != 0 {
		t.Fatalf("answering in %s's pod replaced %s's pod %d times", fleetIssueA, fleetIssueB, len(calls))
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester} {
		testwait.Eventually(t, fmt.Sprintf("%s's %s to leave its round with the replacement", fleetIssueA, role), func() bool {
			c := rig.claimOf(fleetIssueA, role)
			return c.UpdateRound == "" && c.UpdateSignalledAt == nil
		})
	}
	if c := rig.claimOf(fleetIssueB, claim.RoleArchitect); c.UpdateRound != rig.round(fleetIssueB) || c.UpdateSignalledAt != nil {
		t.Fatalf("%s's architect = %+v, want it still waiting in its own round", fleetIssueB, c)
	}
	// The replacement carries the configuration a pod made now is built from, so the first issue's
	// new pod has drifted from nothing.
	rig.drifts(fleetIssueA)

	// The second issue's last worker answers: its pod is replaced too, and the first issue's is not
	// replaced a second time.
	rig.signal(fleetIssueB, claim.RoleArchitect, "b2b2b2b")
	rig.tick()

	if calls := rig.replacementsOf(fleetPodB); len(calls) != 1 {
		t.Fatalf("ReplacePod over %s's pod called %d times once its workers answered, want once", fleetIssueB, len(calls))
	}
	if calls := rig.replacementsOf(fleetPodA); len(calls) != 1 {
		t.Fatalf("answering in %s's pod replaced %s's pod %d times, want the one replacement its own round made",
			fleetIssueB, fleetIssueA, len(calls))
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner} {
		testwait.Eventually(t, fmt.Sprintf("%s's %s to leave its round with the replacement", fleetIssueB, role), func() bool {
			return rig.claimOf(fleetIssueB, role).UpdateRound == ""
		})
	}
	rig.assertNoRootLeftTheWorkflow(fleetIssueA, fleetIssueB)
}

// The round serves a worker on the plugin contract it registered with, whatever this daemon's own
// is: the whole point of the round is that the process pushes its work before it is replaced, and
// it cannot push without the daemon it already speaks to (api's staleClaim). With no round open
// the same claim is refused the stale-caller 409 — the negative control — and the round is what
// turns that refusal into the 200 a completion needs.
func TestPodUpdateRoundServesAWorkerOnItsPreviousContract(t *testing.T) {
	rig := newFleetRig(t,
		fleetIssue{key: fleetIssueB, phase: phase.Planning, pod: fleetPodB,
			roles: []claim.Role{claim.RoleArchitect, claim.RolePlanner}},
	)
	before := rig.claimOf(fleetIssueB, claim.RolePlanner)
	m, ok := rig.sup.Machine(before.Token)
	if !ok {
		t.Fatalf("%s's planner is not supervised", fleetIssueB)
	}
	// The planner's process registered on the contract before this daemon's, which is what a
	// deployment's own release leaves running until each pod is replaced.
	if err := m.Handle(rig.ctx, supervise.RequestRegister{
		Claim: before.Token, Generation: before.Generation, Session: before.Session, SessionFile: before.SessionFile,
		CapabilityHash: []byte("previous-contract"), PluginContract: api.DaemonAPIVersion - 1,
	}); err != nil {
		t.Fatalf("register the planner on the previous contract: %v", err)
	}

	status, body := rig.complete(fleetIssueB, claim.RolePlanner, "planned")
	if status != http.StatusConflict || !strings.Contains(body, "STALE_PLUGIN_CONTRACT") {
		t.Fatalf("the completion of a claim on the previous contract with no round open = %d %s, want 409 STALE_PLUGIN_CONTRACT", status, body)
	}
	if got := readIssue(t, rig.pool, fleetIssueB); got.Phase != phase.Planning {
		t.Fatalf("%s moved to %s on a refused completion, want nothing recorded by it", fleetIssueB, got.Phase)
	}

	rig.drifts(fleetIssueB, fleetDrift)
	rig.tick()
	rig.awaitRound(fleetIssueB, claim.RoleArchitect)
	testwait.Eventually(t, "the planner to be in the round of "+fleetIssueB, func() bool {
		return rig.claimOf(fleetIssueB, claim.RolePlanner).UpdateRound == rig.round(fleetIssueB)
	})

	status, body = rig.complete(fleetIssueB, claim.RolePlanner, "planned; pushed for the round")
	if status != http.StatusOK {
		t.Fatalf("the same completion while the round is open = %d %s, want 200: the round serves the claim on its previous contract", status, body)
	}
	if got := readIssue(t, rig.pool, fleetIssueB); got.Phase != phase.Implementing {
		t.Fatalf("%s after the completion the round let through = %s, want its phase moved on to implementing", fleetIssueB, got.Phase)
	}
	rig.assertNoRootLeftTheWorkflow(fleetIssueB)
}

// An update round's asks adopt nothing, so none of them waits on another role's turn or moves the
// working copy under one. Every role of an issue works one jj workspace, and a task that adopts it
// snapshots that workspace and then runs `jj metaedit --update-author` or `jj new` on its `@`
// under whoever is working there, so the daemon adopts only for a task that names a phase and
// holds such a task while another claim of the issue is in a turn (supervise's adopts
// and sendPending). A round's ask names no phase — it asks a worker to push what it has and
// answer, which commits nothing for a phase — at either place one is built: the sweep's ask to a
// claim that holds nothing (askForUpdate) and the ask that takes the slot a turn's end frees
// (takeSlotForUpdate). So with the implementer in the turn of its own phase task, the architect
// and the planner are asked the moment the round opens and adopt nothing, and the implementer is
// asked once that turn ends, with no adoption beyond the one its phase task made.
//
// The releases those projects' daemons ran, from before cybermonklabs/legion#2539, adopted before
// every send and held none, which is the shape OSSV-343, OSSV-388, OSSV-400, BET-846 and
// LINUX-1498 each recorded: a round opened while the implementer was mid-turn, and the other
// roles' asks adopted the working copy under that turn (dispatch://INFRA-2695). The rig is wired
// as the daemon wires its supervisor (adoptingAsTheDaemonDoes), so who is in a turn is the
// daemon's own answer (claimInTurn), read from the machines the round posts to.
func TestPodUpdateTheRoundsAsksAdoptNothingWhileTheImplementerIsInATurn(t *testing.T) {
	rig := newWiredFleetRig(t, adoptingAsTheDaemonDoes,
		fleetIssue{key: fleetIssueA, phase: phase.Implementing, pod: fleetPodA,
			roles: []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer}},
	)
	implementer := rig.token(fleetIssueA, claim.RoleImplementer)
	others := []claim.Role{claim.RoleArchitect, claim.RolePlanner}

	// The implementer is in the turn of its own phase task, as it is while it writes the plan's
	// work. That task adopts the working copy under the implementer's App, and it is the one task
	// here that may: no other claim of the issue was in a turn when it went. It is handed to the
	// machine directly as setup, since the rig runs no outbox; what the test checks, the round's
	// asks, the real sweep builds.
	const phaseTask = "outbox:1"
	rig.handle(fleetIssueA, claim.RoleImplementer, supervise.RequestDeliver{Claim: implementer,
		Task: "implement the plan", ID: phaseTask, Phase: phase.Implementing, Generation: 1})
	rig.sent(fleetIssueA, claim.RoleImplementer)
	rig.handle(fleetIssueA, claim.RoleImplementer, supervise.StreamTurnStart{Claim: implementer})
	if c := rig.claimOf(fleetIssueA, claim.RoleImplementer); c.State != supervise.StateWorking {
		t.Fatalf("the implementer is %s once its turn started, want working", c.State)
	}
	own := adoptionsOf(rig.rt, implementer)
	if len(own) != 1 || own[0] != "legion-implementer[bot]" {
		t.Fatalf("the implementer's phase task adopted the working copy as %v, want once, as the implementer's App", own)
	}

	// One template change drifts the issue's pod, and the tick opens its round.
	rig.drifts(fleetIssueA, fleetDrift)
	rig.tick()
	round := rig.round(fleetIssueA)
	ask := "update:" + round + ":"

	// The architect and the planner hold nothing of their own, so the tick asks each of them
	// (askForUpdate), and the decision that queued the ask sends it rather than hold it behind the
	// implementer's turn.
	for _, role := range others {
		c := rig.awaitRound(fleetIssueA, role)
		if c.Pending == nil || !strings.HasPrefix(c.Pending.ID, ask) {
			t.Fatalf("%s's %s holds %+v, want its round's ask", fleetIssueA, role, c.Pending)
		}
		rig.sent(fleetIssueA, role)
		if prompts := rig.prompts(fleetIssueA, role); len(prompts) != 1 || prompts[0].DeliveryID != c.Pending.ID {
			t.Errorf("%s's %s was prompted %+v while the implementer was in a turn, want its round's ask %s sent at once rather than held behind that turn",
				fleetIssueA, role, prompts, c.Pending.ID)
		}
		if made := adoptionsOf(rig.rt, rig.token(fleetIssueA, role)); len(made) != 0 {
			t.Errorf("%s's %s adopted the working copy as %v while the implementer was in a turn; the round's ask adopts nothing",
				fleetIssueA, role, made)
		}
	}

	// The implementer holds its own phase task, so the same tick asks it nothing: the round is
	// recorded on it, and its ask waits for the slot that task holds.
	testwait.Eventually(t, "the implementer to be in the round of "+fleetIssueA, func() bool {
		return rig.claimOf(fleetIssueA, claim.RoleImplementer).UpdateRound == round
	})
	if c := rig.claimOf(fleetIssueA, claim.RoleImplementer); c.UpdateTaskedAt != nil || c.Pending == nil || c.Pending.ID != phaseTask {
		t.Fatalf("the implementer in its turn was asked at %v and holds %+v, want it asked nothing yet and holding its own task",
			c.UpdateTaskedAt, c.Pending)
	}

	// The turn ends as Oh My Pi reports it, through the inbox the worker's stream feeds: the
	// agent_end, which the daemon tells the issue's other claims of (wakeIssue), and behind it the
	// result of the prompt whose turn it was, which retires the phase task. The slot that frees is
	// the round's (takeSlotForUpdate), and its ask goes with no adoption either.
	rig.sup.post(implementer, supervise.StreamTurnEnd{Claim: implementer})
	rig.sup.post(implementer, supervise.StreamPromptResult{Claim: implementer, DeliveryID: phaseTask, AgentInvoked: true})
	testwait.Eventually(t, "the implementer's round ask to take the slot its turn freed", func() bool {
		c := rig.claimOf(fleetIssueA, claim.RoleImplementer)
		return c.Pending != nil && strings.HasPrefix(c.Pending.ID, ask) && c.UpdateTaskedAt != nil
	})
	rig.sent(fleetIssueA, claim.RoleImplementer)
	if prompts := rig.prompts(fleetIssueA, claim.RoleImplementer); len(prompts) != 2 ||
		prompts[0].DeliveryID != phaseTask || !strings.HasPrefix(prompts[1].DeliveryID, ask) {
		t.Errorf("the implementer was prompted %+v by the end of its turn, want its phase task and then its round's ask", prompts)
	}
	if made := adoptionsOf(rig.rt, implementer); len(made) != 1 {
		t.Errorf("the implementer adopted the working copy as %v by the time its round's ask was sent, want only the one adoption its phase task made",
			made)
	}
	for _, role := range others {
		rig.sent(fleetIssueA, role)
		if made := adoptionsOf(rig.rt, rig.token(fleetIssueA, role)); len(made) != 0 {
			t.Errorf("%s's %s adopted the working copy as %v by the end of the implementer's turn; the round's ask adopts nothing",
				fleetIssueA, role, made)
		}
	}
}
