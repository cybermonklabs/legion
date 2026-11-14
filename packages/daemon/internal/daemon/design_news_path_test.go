package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The cases here run news of the approved design the whole way, from the fact that moves the gate
// or the issue to the confirmation that records what a holder was told, through the daemon's own
// parts: the workflow engine and admission apply each fact as the daemon applies it, the outbox
// runs each row those facts write through the lanes' own claim, execute and settle (runRow), the
// supervise machines take the deliveries and send them, and the daemon's store records every claim
// and delivery and raises the told versions in the write that records a confirmation. Dispatch, the
// runtime, the worker's connection and Envoy are the doubles; nothing between them is.

const (
	pathRoot       = "LEGION-800"
	pathChild      = "LEGION-801"
	pathGrandchild = "LEGION-802"
	pathDoc        = "artifact-800"
)

// pathMember is one issue of a rig's tree: its key, its parent's key ("" for the tree's root,
// which is the first member a rig is given), and the phase it stands in.
type pathMember struct {
	key, parent string
	at          phase.Phase
}

// pathRow is one row the outbox held when the rig read it, with its payload decoded.
type pathRow struct {
	id      int64
	kind    record.OutboxKind
	issue   string
	payload record.OutboxPayload
}

// newsPathRig is one tree on a database of its own, gated on its spec, with the daemon's path from
// a fact to a confirmation wired as production wires it (openSupervision, newOutbox): the engine and
// admission that apply each fact, the outbox that runs each row, and a supervisor whose machines
// write to the daemon's store and ask the workflow runtime whether a task's phase holds, whether a
// root hears, and what a task's news says at its send (PhaseHolds, RootHears, RootTask,
// DesignNews). Each member holds an open pull request at a head of its own, the phases row of the
// role its phase names, and the admission slot its work runs under.
//
// seen is every row the rig has read from the outbox, by id: a row the lanes run is finished and
// gone, so the rig reads the outbox before each run and at each count, and what a case counts is
// every row the daemon wrote, run or not.
type newsPathRig struct {
	t         *testing.T
	ctx       context.Context
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	runner    *outbox
	sup       *supervisor
	rt        *fake.Runtime
	conns     *fake.Conns
	client    *changeSetDispatch
	notices   *outboxPublisher
	root      string
	seen      map[int64]pathRow
	// marks is the place of the newest Dispatch event of the root's spec in its event log, the
	// gate's approval among them, so each event the rig applies is placed after the last.
	marks int64
	// skew is how far the outbox's clock runs ahead of the wall's, so a case can let a row's
	// backoff run out (later).
	skew atomic.Int64
}

// newNewsPathRig records members, the first being the tree's root, with the root's spec gate
// approved at version approved and open.
func newNewsPathRig(t *testing.T, approved int, members ...pathMember) *newsPathRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	steps := config.DesignGateRootIssues.Steps()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: steps}, quietLogger())
	r := &newsPathRig{t: t, ctx: context.Background(), pool: pool, records: records, engine: engine,
		admission: admit.New(records, engine, len(members), "LEGION", quietLogger()), client: &changeSetDispatch{},
		notices: &outboxPublisher{}, root: members[0].key, seen: map[int64]pathRow{}, marks: 10}
	keys := make([]string, len(members))
	for i, member := range members {
		r.member(member, i)
		keys[i] = member.key
	}
	r.inTx(func(tx pgx.Tx) error {
		return records.PutGate(r.ctx, tx, record.DesignGate{Issue: r.root, Step: record.DesignStepSpec, ArtifactID: pathDoc,
			LatestVersion: approved, ApprovedVersion: new(approved), ApprovedMark: record.EventMark{Seq: r.marks}, EverOpened: true})
	})
	holdSlot(t, pool, keys...)
	// The daemon's store, with the tree's lifecycle open under the workflow as admission opens it.
	st := outboxTreeStore(t, pool, r.root)
	r.sup, r.rt = newOutboxSupervisor(t, "legion", t.TempDir())
	answers := &workflowRuntime{pool: pool, records: records, gatedSteps: steps, dispatchProject: "LEGION", log: quietLogger()}
	r.sup.deps.Store = st
	r.sup.deps.PhaseHolds, r.sup.deps.RootHears = answers.phaseHolds, answers.rootHears
	r.sup.deps.RootTask, r.sup.deps.DesignNews = answers.rootTask, answers.designNews
	r.conns = r.sup.deps.Conns.(*fake.Conns)
	r.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: r.sup, trees: st,
		dispatch: r.client, notices: r.notices, handlers: []intake.Handler{engine, r.admission}, tokens: outboxTokens{},
		project: "legion", stateDir: t.TempDir(), gatedSteps: steps,
		now: func() time.Time { return time.Now().Add(time.Duration(r.skew.Load())) },
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + r.root}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}
	return r
}

func (r *newsPathRig) inTx(write func(pgx.Tx) error) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, write); err != nil {
		r.t.Fatalf("write the record: %v", err)
	}
}

// pathHead is the head of key's pull request.
func pathHead(key string) string { return "head-" + strings.ToLower(key) }

// member records m, the i-th member of the tree, with its pull request open and mergeable at its
// head, and the phases row of the role its phase names: a reviewer's holding its approval of that
// head, so that its completion ends the round.
func (r *newsPathRig) member(m pathMember, i int) {
	r.t.Helper()
	issue := record.Issue{Key: m.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: r.root, Title: m.key, Phase: m.at, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: fmt.Sprintf("A%d", i), LastDispatchSeq: 1}
	if m.parent != "" {
		issue.Parent = &m.parent
	}
	putOutboxIssue(r.t, r.pool, r.records, issue)
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutPullRequest(r.ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: m.key, Repo: "acme/widgets",
			Number: 800 + i, Branch: "legion/" + m.key, HeadSHA: pathHead(m.key), CheckedHead: pathHead(m.key),
			MergeState: record.MergeStateClean, Failing: []string{}}); err != nil {
			return err
		}
		role := phase.RoleFor(m.at)
		if role == "" || role == claim.RoleArchitect {
			return nil
		}
		row := record.PhaseRow{Issue: m.key, Role: role, Claim: mustClaimToken(r.t, m.key, role)}
		if role == claim.RoleReviewer {
			row.Decision = &record.ReviewDecision{State: "approved", Head: pathHead(m.key)}
		}
		return r.records.PutPhase(r.ctx, tx, row)
	})
}

// told raises issue's audience row to version of the spec, writing the row where there is none.
func (r *newsPathRig) told(issue string, audience record.DesignAudience, version int) {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		return r.records.RaiseTold(r.ctx, tx, issue, audience, map[record.DesignStep]int{record.DesignStepSpec: version}, time.Now())
	})
}

// toldOf is issue's audience row as the record holds it, the zero row where there is none.
func (r *newsPathRig) toldOf(issue string, audience record.DesignAudience) record.DesignTold {
	r.t.Helper()
	var found record.DesignTold
	r.inTx(func(tx pgx.Tx) error {
		rows, err := r.records.TreeTold(r.ctx, tx, r.root)
		for _, row := range rows {
			if row.Issue == issue && row.Audience == audience {
				found = row
			}
		}
		return err
	})
	return found
}

// wantTold fails the test unless issue's audience took version of the spec and is owed the opening
// exactly when owed says so.
func (r *newsPathRig) wantTold(when, issue string, audience record.DesignAudience, version int, owed bool) {
	r.t.Helper()
	row := r.toldOf(issue, audience)
	if took, known := row.Took(record.DesignStepSpec); !known || took != version || row.OpeningOwed != owed {
		r.t.Fatalf("%s: %s's %s took version %d (known %t) owing the opening %t, want version %d owing it %t",
			when, issue, audience, took, known, row.OpeningOwed, version, owed)
	}
}

// dispatchEvent applies a Dispatch event of the root's spec, as the consumer decodes one, through
// the engine and then admission, as the daemon applies every Dispatch event: a new version, or a
// person's approval.
func (r *newsPathRig) dispatchEvent(kind intake.DispatchArtifactKind, version int) {
	r.t.Helper()
	r.marks++
	fact := intake.DispatchArtifact{Key: r.root, ArtifactID: pathDoc, Kind: kind, Version: version, Mark: record.EventMark{Seq: r.marks}}
	if kind == intake.DispatchArtifactApproved {
		fact.Actor = dispatch.Actor{Kind: "user", ID: "alice"}
	}
	applyFact(r.t, r.pool, "dispatch", fmt.Sprintf("artifact-%s-%d-%d", kind, version, r.marks), fact, r.engine, r.admission)
}

// complete applies the completion of key's phase by the role its phase names, as the handoff route
// applies one once it has read the handoff record back: the tester's with a pass.
func (r *newsPathRig) complete(key string) {
	r.t.Helper()
	issue := readIssue(r.t, r.pool, key)
	role := phase.RoleFor(issue.Phase)
	word, _, _ := phase.Handoff(issue.Phase)
	fact := intake.HandoffComplete{Generation: 1, Issue: key, Role: role, Claim: mustClaimToken(r.t, key, role), Summary: "done",
		Commit: pathHead(key), HandoffRecord: word + "-1", HandoffVersion: 1}
	if role == claim.RoleTester {
		fact.Verdict = "pass"
	}
	applyFact(r.t, r.pool, "api", fmt.Sprintf("complete-%s-%s", key, issue.Phase), fact, r.engine, r.admission)
}

// completeThroughTheRoute is machine's worker completing its phase through the daemon's handoff
// route, as its `legion` tool does, naming the handoff record it wrote under its own session; it
// answers the route's status and body.
func (r *newsPathRig) completeThroughTheRoute(machine *supervise.Machine, verdict string) (int, string) {
	r.t.Helper()
	held := machine.Claim()
	issue := readIssue(r.t, r.pool, held.Issue)
	word, _, _ := phase.Handoff(issue.Phase)
	grants := credential.New(nil)
	handoffs := &fleetHandoffs{handoffs: map[string]dispatch.Handoff{}}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Steps: config.DesignGateRootIssues.Steps(), Supervisor: r.sup, Project: "legion", DispatchProject: "LEGION",
		Pool: r.pool, Handlers: []intake.Handler{r.engine, r.admission}, Record: r.records, Grants: grants, Dispatch: handoffs,
		Log: quietLogger(),
	}).Handler
	grant, err := grants.Mint(held)
	if err != nil {
		r.t.Fatalf("mint the grant %s completes with: %v", held.Token, err)
	}
	body, err := json.Marshal(api.HandoffCompleteRequest{GrantID: grant.ID, Summary: "the work is done", Verdict: verdict,
		HandoffID: handoffs.write(held.Issue, word, held.Session), HandoffVersion: 1})
	if err != nil {
		r.t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/legion/v1/handoff/complete", bytes.NewReader(body)))
	return recorder.Code, strings.TrimSpace(recorder.Body.String())
}

// observe reads every row the outbox holds into seen and answers them, oldest first.
func (r *newsPathRig) observe() []pathRow {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind, issue, payload from outbox order by id`)
	if err != nil {
		r.t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var held []pathRow
	for rows.Next() {
		var row pathRow
		var kind string
		var payload []byte
		if err := rows.Scan(&row.id, &kind, &row.issue, &payload); err != nil {
			r.t.Fatalf("read the outbox: %v", err)
		}
		row.kind = record.OutboxKind(kind)
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: row.id, Kind: row.kind, Issue: row.issue,
			Payload: json.RawMessage(payload), NextAt: time.Now()})
		if err != nil {
			r.t.Fatalf("decode outbox row %d: %v", row.id, err)
		}
		row.payload = decoded
		r.seen[row.id] = row
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the outbox: %v", err)
	}
	return held
}

// held is the rows the outbox holds now that match.
func (r *newsPathRig) held(match func(pathRow) bool) []pathRow {
	r.t.Helper()
	var found []pathRow
	for _, row := range r.observe() {
		if match(row) {
			found = append(found, row)
		}
	}
	return found
}

// run is the lanes' own run of each row the outbox holds now that matches: claimed by id when it
// is due, executed, and settled — finished, or retried on its backoff where it waits or fails
// (runRow). It answers the rows it ran.
func (r *newsPathRig) run(match func(pathRow) bool) []pathRow {
	r.t.Helper()
	rows := r.held(match)
	for _, row := range rows {
		if err := r.runner.runRow(r.ctx, record.OutboxRow{ID: row.id, Kind: row.kind, Issue: row.issue}); err != nil {
			r.t.Fatalf("run outbox row %d (%s of %s): %v", row.id, row.kind, row.issue, err)
		}
	}
	return rows
}

// still says whether the outbox still holds row, and on how many failed or waiting attempts.
func (r *newsPathRig) still(row pathRow) (bool, int) {
	r.t.Helper()
	var attempts int
	err := r.pool.QueryRow(r.ctx, `select attempts from outbox where id = $1`, row.id).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0
	}
	if err != nil {
		r.t.Fatalf("read outbox row %d: %v", row.id, err)
	}
	return true, attempts
}

// later runs the outbox's clock ahead by d, past any backoff a row was retried on.
func (r *newsPathRig) later(d time.Duration) { r.skew.Add(int64(d)) }

// ofKind matches the rows of kind.
func ofKind(kind record.OutboxKind) func(pathRow) bool {
	return func(row pathRow) bool { return row.kind == kind }
}

// startOf matches a start of role on issue: a phase start, or one queued for news alone when
// newsOnly says so.
func startOf(issue string, role claim.Role, newsOnly bool) func(pathRow) bool {
	return func(row pathRow) bool {
		request, ok := row.payload.(record.SuperviseRequest)
		return ok && row.issue == issue && request.Op == "start" && request.Role == role && request.News == newsOnly
	}
}

// addressedTo matches every row addressed to the claim of role on issue, whose token is token, or
// to that role: a role message to its topic, a start or stop of it, a phase-ended message and a
// hook prompt to it. A notice is the architect's, whatever role it names.
func addressedTo(issue string, role claim.Role, token claim.Token) func(pathRow) bool {
	return func(row pathRow) bool {
		switch payload := row.payload.(type) {
		case record.RoleMessage:
			return payload.Role == string(token)
		case record.SuperviseRequest:
			return row.issue == issue && payload.Role == role
		case record.PhaseEnded:
			return row.issue == issue && payload.Role == role
		case record.HookPrompt:
			return row.issue == issue && payload.Role == role
		}
		return false
	}
}

// lastRowID is the newest row id the rig has seen, the outbox read first: every row the daemon
// writes after it takes a greater id, so a case counts what was written after it by this mark.
func (r *newsPathRig) lastRowID() int64 {
	r.t.Helper()
	r.observe()
	var last int64
	for id := range r.seen {
		last = max(last, id)
	}
	return last
}

// seenSince is every row the rig has seen with an id above mark that matches, the outbox read
// first, oldest first.
func (r *newsPathRig) seenSince(mark int64, match func(pathRow) bool) []pathRow {
	r.t.Helper()
	r.observe()
	var found []pathRow
	for _, id := range slices.Sorted(maps.Keys(r.seen)) {
		if row := r.seen[id]; id > mark && match(row) {
			found = append(found, row)
		}
	}
	return found
}

// connect is the worker of role on issue coming up on the process its start launched: its shim's
// connection registers, it says hello, registers its session and says it is ready, as a launched
// worker does. It answers the claim's machine and the connection the worker's prompts reach.
func (r *newsPathRig) connect(issue string, role claim.Role) (*supervise.Machine, *fake.Conn) {
	r.t.Helper()
	token := mustClaimToken(r.t, issue, role)
	machine, found := r.sup.Machine(token)
	if !found {
		r.t.Fatalf("no claim of %s's %s to connect: its start created none", issue, role)
	}
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	machine.Wait()
	generation := machine.Claim().Generation
	session := fmt.Sprintf("ses-%s-%s", strings.ToLower(issue), role)
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: session, SessionFile: "/tmp/" + session + ".jsonl",
			CapabilityHash: []byte("capability-of-" + session)},
		supervise.RequestReady{Claim: token, Generation: generation, Session: session},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T of %s: %v", ev, token, err)
		}
	}
	return machine, conn
}

// delivered waits for the task machine holds to reach the agent, and answers it.
func (r *newsPathRig) delivered(machine *supervise.Machine) supervise.Delivery {
	r.t.Helper()
	testwait.Eventually(r.t, "the task of "+string(machine.Claim().Token)+" to reach its agent", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	return *machine.Claim().Pending
}

// startTurn is the agent beginning the turn that works the task it was sent, which confirms it.
func (r *newsPathRig) startTurn(machine *supervise.Machine) {
	r.t.Helper()
	held := machine.Claim()
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: held.Token, DeliveryID: held.Pending.ID}); err != nil {
		r.t.Fatalf("start the turn of %s on %s: %v", held.Token, held.Pending.ID, err)
	}
	if confirmed := machine.Claim().Pending; confirmed == nil || confirmed.ConfirmedAt.IsZero() {
		r.t.Fatalf("the turn's start left %s holding %+v, want its task confirmed", held.Token, confirmed)
	}
}

// promptsAfter is each prompt conn carried after its first skip, in order.
func promptsAfter(conn *fake.Conn, skip int) []fake.Prompt {
	return conn.Prompts()[skip:]
}

// publishedTo is each message published to token's role topic, in order.
func (r *newsPathRig) publishedTo(token claim.Token) []string {
	var bodies []string
	for _, published := range r.notices.publishes() {
		if published.topic == notify.RoleTopicPrefix+string(token) {
			bodies = append(bodies, published.message)
		}
	}
	return bodies
}

// A5, the whole of it. A tester is in the turn its phase task started when an approved new version
// of the spec lands, and the news task the reconcile queues for it waits behind that turn: the
// machine retires the phase task the turn took and holds the news unsent, since nothing is sent
// into a running turn. The tester completes in that same turn, through the handoff route, and the
// issue moves to reviewing. The turn's end is the news task's first chance to go, and its phase is
// over, so phaseHolds drops it: it was never sent and never confirmed, and the record says so —
// the issue's holder still took version 4 and is still owed the opening. The reviewer's phase start
// carries that news, and the told version rises, and the opening is cleared, only in the write that
// confirms the reviewer's task.
func TestATesterThatCompletesInTheTurnItsNewsWaitsBehindLeavesThatNewsToTheReviewersStart(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	testerToken := mustClaimToken(t, pathRoot, claim.RoleTester)

	// The implementer's completion hands testing to the tester, whose start the outbox runs; the
	// tester takes the entry's task and begins its turn on it.
	r.complete(pathRoot)
	if got := r.run(startOf(pathRoot, claim.RoleTester, false)); len(got) != 1 {
		t.Fatalf("the tester's start rows = %d, want the one the implementer's completion wrote", len(got))
	}
	tester, testerConn := r.connect(pathRoot, claim.RoleTester)
	phaseTask := r.delivered(tester)
	r.startTurn(tester)

	// A writer saves version 5 and a person approves it while that turn runs. The approval's
	// reconcile reads what changed and queues the tester's news-only start, which the outbox runs.
	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
		t.Fatalf("design_changes rows after the approval = %d, want the one the opening queued", len(got))
	}
	if got := r.run(startOf(pathRoot, claim.RoleTester, true)); len(got) != 1 {
		t.Fatalf("the tester's news-only starts = %d, want the one the reconcile queued", len(got))
	}
	queued := tester.Claim()
	if queued.State != supervise.StateWorking || queued.Pending == nil || queued.Pending.ID == phaseTask.ID ||
		!queued.Pending.DeliveredAt.IsZero() || len(queued.Pending.News) == 0 {
		t.Fatalf("the tester after its news-only start = %s holding %+v, want it still in its turn, holding the news task unsent", queued.State, queued.Pending)
	}
	for _, want := range []string{"The design gate of LEGION-800 opened: a person approved version 5", "version 5, since version 4, which your phase last took"} {
		if !strings.Contains(queued.Pending.Task, want) {
			t.Fatalf("the tester's news task = %q, want it to say %q", queued.Pending.Task, want)
		}
	}
	if sent := testerConn.Prompts(); len(sent) != 1 || sent[0].DeliveryID != phaseTask.ID {
		t.Fatalf("the tester's prompts = %+v, want its phase task alone: no task goes into a running turn", sent)
	}

	// The tester completes in that same turn, and the issue moves to reviewing.
	if status, body := r.completeThroughTheRoute(tester, "pass"); status != http.StatusOK {
		t.Fatalf("the tester's completion = %d %s, want 200", status, body)
	}
	if issue := readIssue(t, r.pool, pathRoot); issue.Phase != phase.Reviewing {
		t.Fatalf("%s stands in %s after the tester's completion, want reviewing", pathRoot, issue.Phase)
	}

	// The turn ends, and the news task's phase is over: it is dropped, never sent, never confirmed.
	endTurn(r.ctx, t, tester, testerToken)
	if pending := tester.Claim().Pending; pending != nil {
		t.Fatalf("the tester holds %+v after its turn ended, want the news task dropped: its phase is over", pending)
	}
	if sent := testerConn.Prompts(); len(sent) != 1 || sent[0].DeliveryID != phaseTask.ID {
		t.Fatalf("the tester's prompts = %+v, want its phase task alone: the news task was never sent", sent)
	}
	r.wantTold("after the tester's news task was dropped", pathRoot, record.AudienceHolder, 4, true)

	// The reconcile the move queued finds no claim to tell yet; the reviewer's phase start carries
	// the news.
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
		t.Fatalf("design_changes rows after the move = %d, want the one the move queued", len(got))
	}
	if got := r.held(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 0 {
		t.Fatalf("the reconcile after the move queued %d news-only starts of the reviewer, want none: its phase start carries the news", len(got))
	}
	starts := r.run(startOf(pathRoot, claim.RoleReviewer, false))
	if len(starts) != 1 || starts[0].payload.(record.SuperviseRequest).Entry != 1 {
		t.Fatalf("the reviewer's start rows = %+v, want its one phase start of entry 1", starts)
	}
	reviewer, reviewerConn := r.connect(pathRoot, claim.RoleReviewer)
	sent := r.delivered(reviewer)
	prompted := reviewerConn.Prompts()
	if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
		t.Fatalf("the reviewer's prompts = %+v, want its phase task, once", prompted)
	}
	for _, want := range []string{
		starts[0].payload.(record.SuperviseRequest).Task,
		"The design gate of LEGION-800 opened: a person approved version 5 of its spec document (artifact-800)",
		"The approved design of LEGION-800 changed for LEGION-800, which you hold in reviewing:",
		"version 5, since version 4, which your phase last took",
		`"Design" changed`,
	} {
		if !strings.Contains(prompted[0].Message, want) {
			t.Fatalf("the reviewer's prompt = %q, want it to say %q", prompted[0].Message, want)
		}
	}
	r.wantTold("after the reviewer's task was sent, before its turn took it", pathRoot, record.AudienceHolder, 4, true)
	r.startTurn(reviewer)
	r.wantTold("after the reviewer's turn confirmed its task", pathRoot, record.AudienceHolder, 5, false)
}

// INFRA-3522's condition 3. A reviewer is in a turn when a person approves version 5, so the news
// task the reconcile queues for it is composed while the gate stands open, and says opened. A
// writer then saves version 6, which closes the gate again, before the turn ends. The turn's end is
// when the task is sent, and the send renders it again from the record: the sentence says the gate
// closed and names version 6, with its reviewer, and says nothing of an opening that no longer
// stands. The approval of version 5 still stands below version 6, so its item is still told, and
// the confirmation raises it.
func TestANewsTaskComposedWhileTheGateStoodOpenAndSentAfterALaterVersionClosedItSaysClosed(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	token := mustClaimToken(t, pathRoot, claim.RoleReviewer)

	r.complete(pathRoot)
	r.run(startOf(pathRoot, claim.RoleReviewer, false))
	reviewer, conn := r.connect(pathRoot, claim.RoleReviewer)
	r.delivered(reviewer)
	r.startTurn(reviewer)

	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	r.run(ofKind(record.OutboxKindDesignChanges))
	if got := r.run(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 1 {
		t.Fatalf("the reviewer's news-only starts = %d, want the one the reconcile queued", len(got))
	}
	composed := reviewer.Claim().Pending
	if composed == nil || !composed.DeliveredAt.IsZero() || !strings.Contains(composed.Task, "The design gate of LEGION-800 opened: a person approved version 5") {
		t.Fatalf("the reviewer's news task as composed = %+v, want it held behind the turn, saying the gate opened at version 5", composed)
	}

	// Version 6 closes the gate while the task waits.
	r.dispatchEvent(intake.DispatchArtifactVersion, 6)
	endTurn(r.ctx, t, reviewer, token)
	sent := r.delivered(reviewer)
	prompted := promptsAfter(conn, 1)
	if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
		t.Fatalf("the reviewer's prompts after its turn = %+v, want the news task, once", prompted)
	}
	const closed = "The design gate of LEGION-800 closed: version 6 of its spec document (artifact-800) is with its reviewer"
	if !strings.Contains(prompted[0].Message, closed) || strings.Contains(prompted[0].Message, "opened") {
		t.Fatalf("the news task sent = %q, want it to say %q and nothing of an opening", prompted[0].Message, closed)
	}
	if !strings.Contains(prompted[0].Message, "version 5, since version 4, which your phase last took") {
		t.Fatalf("the news task sent = %q, want the approval of version 5, which still stands, told", prompted[0].Message)
	}
	r.startTurn(reviewer)
	if took, _ := r.toldOf(pathRoot, record.AudienceHolder).Took(record.DesignStepSpec); took != 5 {
		t.Fatalf("the reviewer's holder took %d after the confirmation, want 5: the approval it told stands", took)
	}
}

// An issue the engine moves into accepting is held by its owning architect, and no start carries
// news there, since the engine starts no architect. So the transition queues the reconcile of the
// tree's news when the member it moves is owed some, and that reconcile sends the member's news as
// a task to the owning architect's claim, in the holder form: the change list and the accepting
// answer, sign-off or a move back. The owner is the tree's root architect for a child of the root,
// and the sub-architect running for the child's parent for a grandchild. The member's told version
// rises at that task's confirmation.
func TestAnOwedMemberMovedIntoAcceptingIsToldByATaskToItsOwningArchitect(t *testing.T) {
	for _, tc := range []struct {
		name      string
		members   []pathMember
		owner     string
		accepting string
	}{
		{name: "the root architect", owner: pathRoot, accepting: pathChild, members: []pathMember{
			{key: pathRoot, at: phase.Implementing}, {key: pathChild, parent: pathRoot, at: phase.Reviewing}}},
		{name: "a running sub-architect", owner: pathChild, accepting: pathGrandchild, members: []pathMember{
			{key: pathRoot, at: phase.Implementing}, {key: pathChild, parent: pathRoot, at: phase.Implementing},
			{key: pathGrandchild, parent: pathChild, at: phase.Reviewing}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 5, tc.members...)
			// Every member took version 5 but the one about to move, which took version 4 and was told
			// nothing since: no claim held its review when version 5 was approved.
			for _, member := range tc.members {
				if member.key != tc.accepting {
					r.told(member.key, record.AudienceHolder, 5)
				}
			}
			r.told(tc.accepting, record.AudienceHolder, 4)
			r.told(pathRoot, record.AudienceArchitect, 5)
			// The architects run: the root's, and the sub-architect's where the case has one.
			architects := map[string]*fake.Conn{}
			for _, key := range []string{pathRoot, tc.owner} {
				if _, started := architects[key]; started {
					continue
				}
				enqueueOutbox(t, r.pool, r.records, mustOutboxRow(t, key, record.SuperviseRequest{Op: "start", Tree: pathRoot,
					Role: claim.RoleArchitect, Generation: 1}, time.Now()))
				r.run(startOf(key, claim.RoleArchitect, false))
				_, architects[key] = r.connect(key, claim.RoleArchitect)
			}
			if got := r.held(ofKind(record.OutboxKindDesignChanges)); len(got) != 0 {
				t.Fatalf("design_changes rows before the move = %d, want none: the case's own premise", len(got))
			}

			r.complete(tc.accepting)
			if issue := readIssue(t, r.pool, tc.accepting); issue.Phase != phase.Accepting {
				t.Fatalf("%s stands in %s after its reviewer's completion, want accepting", tc.accepting, issue.Phase)
			}
			reconciles := r.run(ofKind(record.OutboxKindDesignChanges))
			if len(reconciles) != 1 || reconciles[0].issue != pathRoot {
				t.Fatalf("design_changes rows after the move = %+v, want the one the transition queued for the tree", reconciles)
			}
			starts := r.held(func(row pathRow) bool {
				request, ok := row.payload.(record.SuperviseRequest)
				return ok && request.News
			})
			if len(starts) != 1 || starts[0].issue != tc.owner || starts[0].payload.(record.SuperviseRequest).Role != claim.RoleArchitect {
				t.Fatalf("the news-only starts = %+v, want one to the architect of %s", starts, tc.owner)
			}
			r.run(startOf(tc.owner, claim.RoleArchitect, true))

			owner, found := r.sup.Machine(mustClaimToken(t, tc.owner, claim.RoleArchitect))
			if !found {
				t.Fatalf("no claim of %s's architect", tc.owner)
			}
			sent := r.delivered(owner)
			prompted := architects[tc.owner].Prompts()
			if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
				t.Fatalf("the owning architect's prompts = %+v, want the news task, once", prompted)
			}
			for _, want := range []string{
				fmt.Sprintf("changed for %s, which you hold in accepting:", tc.accepting),
				fmt.Sprintf("sign %s off with `sign_off`", tc.accepting),
				"version 5, since version 4, which your phase last took",
			} {
				if !strings.Contains(prompted[0].Message, want) {
					t.Fatalf("the owning architect's task = %q, want it to say %q", prompted[0].Message, want)
				}
			}
			if tc.owner != pathRoot {
				if got := architects[pathRoot].Prompts(); len(got) != 0 {
					t.Fatalf("the root architect's prompts = %+v, want none: the running sub-architect owns %s", got, tc.accepting)
				}
			}
			r.wantTold("before the architect's turn took the task", tc.accepting, record.AudienceHolder, 4, false)
			r.startTurn(owner)
			r.wantTold("after the architect's turn confirmed the task", tc.accepting, record.AudienceHolder, 5, false)
		})
	}
}

// A merger released before it reads what changed could merge it (Design 6), so its start is held
// while its member is owed news whose change set is not recorded. The retro's completion moves the
// issue into merging and queues both the merger's start and the reconcile of the tree's news; the
// start runs first and waits, launching nothing. The reconcile records the pair, read from Dispatch,
// or — under a Dispatch without the changes route, which answers 404 — unavailable with Dispatch's
// refusal as its reason. Either releases the start once its backoff runs out: the merger is launched
// with its task carrying the news, and its told version rises at that task's confirmation.
func TestTheMergersStartIsHeldUntilTheChangeSetOfItsNewsIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(record.VersionPair) (dispatch.ArtifactChanges, error)
		state  record.DesignChangesState
		says   []string
	}{
		{name: "recorded read", state: record.DesignChangesRead, says: []string{`"Design" changed`, "r2 changed"}},
		{name: "recorded unavailable by a Dispatch without the route", state: record.DesignChangesUnavailable,
			answer: func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
				return dispatch.ArtifactChanges{}, fmt.Errorf("%w: GET /api/v1/artifacts/%s/changes: 404 NOT_FOUND", dispatch.ErrNoChangesRoute, pair.ArtifactID)
			},
			says: []string{"The change list could not be read", "dispatch has no changes route"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 5, pathMember{key: pathRoot, at: phase.Retro})
			r.told(pathRoot, record.AudienceHolder, 4)
			r.told(pathRoot, record.AudienceArchitect, 5)
			r.client.answer = tc.answer
			token := mustClaimToken(t, pathRoot, claim.RoleMerger)

			r.complete(pathRoot)
			if issue := readIssue(t, r.pool, pathRoot); issue.Phase != phase.Merging {
				t.Fatalf("%s stands in %s after the retro's completion, want merging", pathRoot, issue.Phase)
			}
			starts := r.run(startOf(pathRoot, claim.RoleMerger, false))
			if len(starts) != 1 {
				t.Fatalf("the merger's start rows = %d, want the one the move wrote", len(starts))
			}
			if still, attempts := r.still(starts[0]); !still || attempts != 1 {
				t.Fatalf("the merger's start after its run is held %t on %d attempts, want it held and retried once", still, attempts)
			}
			if _, found := r.sup.Machine(token); found || len(r.rt.CallsOf("Spawn")) != 0 {
				t.Fatalf("the merger's held start made a claim (%t) and %d spawns, want neither", found, len(r.rt.CallsOf("Spawn")))
			}

			if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
				t.Fatalf("design_changes rows = %d, want the one the move queued", len(got))
			}
			pair := record.VersionPair{ArtifactID: pathDoc, From: 4, To: 5}
			var recorded map[record.VersionPair]record.DesignChanges
			r.inTx(func(tx pgx.Tx) error {
				var err error
				recorded, err = r.records.DesignChangesOf(r.ctx, tx, []record.VersionPair{pair})
				return err
			})
			if changes, held := recorded[pair]; !held || changes.State != tc.state {
				t.Fatalf("the change set of %+v = %+v (recorded %t), want it recorded %s", pair, changes, held, tc.state)
			}

			r.later(time.Minute)
			r.run(startOf(pathRoot, claim.RoleMerger, false))
			if still, _ := r.still(starts[0]); still {
				t.Fatal("the merger's start is still held once the pair is recorded, want it released")
			}
			merger, conn := r.connect(pathRoot, claim.RoleMerger)
			sent := r.delivered(merger)
			prompted := conn.Prompts()
			if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
				t.Fatalf("the merger's prompts = %+v, want its task, once", prompted)
			}
			for _, want := range append([]string{starts[0].payload.(record.SuperviseRequest).Task, "version 5, since version 4, which your phase last took"}, tc.says...) {
				if !strings.Contains(prompted[0].Message, want) {
					t.Fatalf("the merger's task = %q, want it to say %q", prompted[0].Message, want)
				}
			}
			r.wantTold("before the merger's turn took its task", pathRoot, record.AudienceHolder, 4, false)
			r.startTurn(merger)
			r.wantTold("after the merger's turn confirmed its task", pathRoot, record.AudienceHolder, 5, false)
		})
	}
}

// oneDeliveryOfTheOpening is the condition of one delivery of the opening (workflow's
// openingDelivery), for the holder of role on the rig's root, which is between turns on its
// phase's task: a decision-only version that closed the gate, so the holder was told "closed" by
// the role message, and that a person then approved. The holder is owed the opening and, of the
// work, nothing: so it gets exactly one delivery of the opening, the news task leading with the
// opening sentence and saying that nothing its issue holds changed, with no change list, and no
// role message beside it. Every outbox row and every delivery addressed to the holder's claim or
// its role from the approval to the end of the case is counted.
func oneDeliveryOfTheOpening(t *testing.T, r *newsPathRig, role claim.Role) {
	t.Helper()
	token := mustClaimToken(t, pathRoot, role)
	if got := r.run(startOf(pathRoot, role, false)); len(got) != 1 {
		t.Fatalf("the %s's start rows = %d, want the one the move wrote", role, len(got))
	}
	holder, conn := r.connect(pathRoot, role)
	r.delivered(holder)
	r.startTurn(holder)
	endTurn(r.ctx, t, holder, token)
	if got := holder.Claim(); got.Pending != nil {
		t.Fatalf("the %s holds %+v after its turn, want nothing: the case's own premise", role, got.Pending)
	}

	// A writer saves version 5, which changes decision blocks alone, and the gate closes: the holder
	// is told so by its role message.
	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	r.run(ofKind(record.OutboxKindRoleMessage))
	closed := r.publishedTo(token)
	if len(closed) != 1 || !strings.HasPrefix(closed[0], "The design gate of LEGION-800 closed: version 5 of its spec document (artifact-800)") {
		t.Fatalf("the %s's role messages after version 5 = %q, want the one closed message", role, closed)
	}

	// A person approves version 5.
	mark, promptsBefore := r.lastRowID(), len(conn.Prompts())
	r.client.answer = func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
		return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}, nil
	}
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	r.wantTold("at the opening", pathRoot, record.AudienceHolder, 4, true)
	r.run(ofKind(record.OutboxKindRoleMessage))
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) != 1 {
		t.Fatalf("design_changes rows after the approval = %d, want the one the opening queued", len(got))
	}
	r.run(startOf(pathRoot, role, true))
	sent := r.delivered(holder)
	r.startTurn(holder)
	r.run(ofKind(record.OutboxKindRoleMessage))

	addressed := r.seenSince(mark, addressedTo(pathRoot, role, token))
	if len(addressed) != 1 || addressed[0].kind != record.OutboxKindSupervise || !addressed[0].payload.(record.SuperviseRequest).News {
		t.Fatalf("outbox rows addressed to the %s after the approval = %+v, want its one news-only start and no role message", role, addressed)
	}
	if published := r.publishedTo(token); len(published) != 1 {
		t.Fatalf("the %s's role topic carried %q, want the closed message alone", role, published)
	}
	prompted := promptsAfter(conn, promptsBefore)
	if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID {
		t.Fatalf("the %s's prompts after the approval = %+v, want the news task, once", role, prompted)
	}
	for _, want := range []string{
		"The design gate of LEGION-800 opened: a person approved version 5 of its spec document (artifact-800)",
		"Nothing your issue holds changed since version 4, which your phase took.",
	} {
		if !strings.Contains(prompted[0].Message, want) {
			t.Fatalf("the %s's news task = %q, want it to say %q", role, prompted[0].Message, want)
		}
	}
	if strings.Contains(prompted[0].Message, "Sections:") || strings.Contains(prompted[0].Message, "which you hold in") {
		t.Fatalf("the %s's news task = %q, want no change list", role, prompted[0].Message)
	}
	r.wantTold("after the news task's confirmation", pathRoot, record.AudienceHolder, 5, false)
}

// The reviewer's half of oneDeliveryOfTheOpening: a reviewer between turns on its review, told the
// gate closed, gets the decision-only opening once, as the news task, and no role message.
func TestAReviewerToldTheGateClosedGetsOneDeliveryOfADecisionOnlyOpening(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	r.complete(pathRoot)
	oneDeliveryOfTheOpening(t, r, claim.RoleReviewer)
}

// The merger's half of oneDeliveryOfTheOpening: a merger waiting between turns, told the gate
// closed, gets the decision-only opening that releases its merge once, by its news-only start, and
// no role_message row is written for it.
func TestAMergerToldTheGateClosedGetsOneDeliveryOfADecisionOnlyOpening(t *testing.T) {
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Retro})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	r.complete(pathRoot)
	oneDeliveryOfTheOpening(t, r, claim.RoleMerger)
}
