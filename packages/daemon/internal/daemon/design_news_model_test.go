package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The news record against a plain model of what the holder was really told. One tree, its root in
// implementing and a child in reviewing whose reviewer holds the phase, runs through the daemon's
// own parts (the newsPathRig: the engine, admission, the outbox, the supervisor's machine over the
// claim store, the one render), and a seed's random sequence of steps drives it: a person approves a
// new version that changes the work or only its decision blocks, a person requests changes and
// approves again (the gate closes and opens), the reconcile runs (and raises past a pair that
// changed nothing of the work), the sweep queues it, the reviewer's starts run (a send, and its
// acknowledgement), the agent starts the turn of the prompt it holds (a confirmation), the turn's
// result comes (the credit made final) or ends in a provider error (a give-back), the provider
// error's ladder sends the task again, a newer task takes the place of the task whose turn runs (a
// supersede, a give-back of what the agent read), the daemon restarts, the claim store fails a
// write and its repair, and a generation is cleared under the reviewer's claim, which is kept with
// whatever it holds: a person sets the child back to todo (a re-entry, its next run brought back to
// reviewing), or closes the root and sets it back to todo inside the linger (a re-admission of the
// tree, whose architect registers its approved spec again).
//
// The model holds what the agent was given and took, as the agent sees it: each prompt's news and
// the run it was sent in, the prompt whose turn runs, the prompts whose turns ran, and each opening
// a role message told. After every step the told row is held to it:
//
//   - never ahead of it: the record never says the holder took a version it was not told, and never
//     says an opening told that was not (told twice, never lost);
//   - level with it while nothing has made the record give back what the agent did read (a
//     supersede of a task whose turn ran, a failed write): everything the agent read is credited, and
//     the record owes an opening exactly when the holder was not told it;
//   - a clear empties what the issue's holders took: the next run starts from its own baseline —
//     the version approved when its first phase started, or, after a re-admission of the tree, the
//     version the review history names once the reconcile has read it — and counts only what a
//     prompt sent in that run told, so nothing the agent read of the run that ended is level with
//     it or ahead of it;
//   - and, once the run quiesces, the holder has taken the approved version and owes no opening.
//
// With no variable set it runs newsModelCISeed for 40 steps, a few seconds: a seed whose run
// reaches every kind of step at least once (newsModelCISeed says how that was read).
// LEGION_NEWS_MODEL_SEED runs one seed, LEGION_NEWS_MODEL_RUNS runs seeds 1 to that many,
// LEGION_NEWS_MODEL_STEPS sets the steps each drives, and LEGION_NEWS_MODEL_TRACE prints each
// seed's steps and the state each left. A seed's steps are its own: every step waits for the
// machine before the next, and the machine's timers fire only when a step fires them.
func TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold(t *testing.T) {
	steps := newsModelCount(t, "LEGION_NEWS_MODEL_STEPS", 40)
	seeds := []int64{newsModelCISeed}
	if named := os.Getenv("LEGION_NEWS_MODEL_SEED"); named != "" {
		seed, err := strconv.ParseInt(named, 10, 64)
		if err != nil {
			t.Fatalf("LEGION_NEWS_MODEL_SEED=%q is no seed: %v", named, err)
		}
		seeds = []int64{seed}
	} else if runs := newsModelCount(t, "LEGION_NEWS_MODEL_RUNS", 1); runs > 1 {
		seeds = seeds[:0]
		for seed := int64(1); seed <= int64(runs); seed++ {
			seeds = append(seeds, seed)
		}
	}
	var mu sync.Mutex
	counts := map[string]int{}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			m := newNewsModel(t, seed)
			m.drive(steps)
			m.quiesce()
			t.Logf("seed %d ran %d kinds of step: %v", seed, len(m.kinds), m.ran)
			mu.Lock()
			defer mu.Unlock()
			for op, n := range m.ran {
				counts[op] += n
			}
		})
	}
	t.Logf("%d seeds of %d steps; steps run: %v", len(seeds), steps, counts)
}

// newsModelCISeed is the seed the model test runs where no variable names one. Its 40 steps reach
// every kind of step, the two clears among them: read from a run of seeds 1 to 200 at 40 steps
// with -v, which prints how many of the sixteen kinds each seed's drive chose (newsModel.kinds);
// seeds 25 and 126 were the two that reached all sixteen.
const newsModelCISeed = 25

// newsModelCount is the positive count the environment variable name sets, or fallback.
func newsModelCount(t *testing.T, name string, fallback int) int {
	t.Helper()
	set := os.Getenv(name)
	if set == "" {
		return fallback
	}
	n, err := strconv.Atoi(set)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q is no count: %v", name, set, err)
	}
	return n
}

// errModelWrite is a write the model's claim store refused (newsModelStore).
var errModelWrite = errors.New("the claim store refused this write")

// newsModelStore is the daemon's claim store with a switch that refuses the next writes of a
// delivery: a write and the repair of it that settle makes before the next decision.
type newsModelStore struct {
	*store.Store
	mu    sync.Mutex
	fails int
}

// refuse arms n refusals.
func (s *newsModelStore) refuse(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = n
}

// armed is how many refusals are still armed.
func (s *newsModelStore) armed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fails
}

// take spends one armed refusal, if any.
func (s *newsModelStore) take() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return errModelWrite
	}
	return nil
}

func (s *newsModelStore) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	if err := s.take(); err != nil {
		return err
	}
	return s.Store.PutDelivery(ctx, token, d)
}

func (s *newsModelStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	if err := s.take(); err != nil {
		return err
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s *newsModelStore) RetireDelivery(ctx context.Context, c supervise.Claim, id string) error {
	if err := s.take(); err != nil {
		return err
	}
	return s.Store.RetireDelivery(ctx, c, id)
}

// newsModelPrompt is one prompt the agent was given: its delivery id and the news it carried.
type newsModelPrompt struct {
	id   string
	news record.DeliveryNews
}

// newsModelUnknown is the version a run's holder started from where nothing has said it yet
// (newsModel.base).
const newsModelUnknown = -1

// newsModel is one seed's run: the rig, the reviewer's machine and connection, and the model of
// what the reviewer was given and took.
type newsModel struct {
	t       *testing.T
	seed    int64
	rng     *rand.Rand
	r       *newsPathRig
	st      *store.Store
	claims  *newsModelStore
	clock   *heldClock
	token   claim.Token
	machine *supervise.Machine
	conn    *fake.Conn
	step    int
	trace   []string
	// ran counts each step the run made and each check, and kinds is the kinds of step the drive
	// chose.
	ran   map[string]int
	kinds map[string]bool

	// latest is the newest version of the spec, every one of them approved when it is saved, and
	// decisionOnly says which versions changed only decision blocks of the one before.
	latest       int
	decisionOnly map[int]bool
	// run counts the clears of a generation the child has been through, and runOf is the run each
	// prompt was sent in: a prompt tells only the run it was rendered for. base is the version
	// the run's holder started from: the baseline its first phase took, or, after a re-admission
	// of the tree, newsModelUnknown until the reconcile has read it from the review history.
	run   int
	runOf map[string]int
	base  int
	// seen is how many of conn's prompts the model has read, next the prompt the agent holds and
	// has not started a turn on, turn the prompt whose turn runs, and newsOf each prompt's news.
	seen   int
	next   string
	turn   string
	newsOf map[string]record.DeliveryNews
	// took is every prompt whose turn ran and was read, in order; told every opening number of
	// this run that a role message told; openSeq the number of the opening that stands now.
	took    []newsModelPrompt
	told    map[int]bool
	openSeq int
	// messages is how many role messages the reviewer had been sent when the model last looked,
	// and ticks how many admission passes the sweep has made.
	messages int
	ticks    int
	// ambiguous says the record has been made to give back what the agent did read, or a write was
	// refused, so it may lag the model from here on: only the never-ahead rule still holds.
	ambiguous bool
	// behind is the reviewer's delivery as the store held it at a restart that found the store
	// lagging the stopped machine, nil otherwise (lagging).
	behind *newsModelStored
}

// newsModelStored is the reviewer's delivery as the store holds it, as far as its credit goes:
// whether it holds one, under which id, confirmed or not, with which copy, final or not.
type newsModelStored struct {
	held      bool
	id        string
	confirmed bool
	carried   string
	ran       bool
}

// stored is the reviewer's delivery as the store holds it now.
func (m *newsModel) stored() newsModelStored {
	m.t.Helper()
	claims, err := m.st.Claims(m.r.ctx)
	if err != nil {
		m.fail("read the stored claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == m.token && c.Pending != nil {
			return newsModelStored{held: true, id: c.Pending.ID, confirmed: !c.Pending.ConfirmedAt.IsZero(),
				carried: string(c.Pending.Carried), ran: c.Pending.Ran}
		}
	}
	return newsModelStored{}
}

// newNewsModel builds seed's run: the root in implementing and its child in testing at version 4
// of the root's spec, every audience having taken 4, the child's reviewer brought into reviewing
// and between turns, holding nothing. The review history names, for each version, the one below
// it: every version of a run is approved when it is saved.
func newNewsModel(t *testing.T, seed int64) *newsModel {
	t.Helper()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing},
		pathMember{key: pathChild, parent: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	r.told(pathChild, record.AudienceHolder, 4)
	st, ok := r.sup.deps.Store.(*store.Store)
	if !ok {
		t.Fatalf("the rig's claim store is %T, want the daemon's", r.sup.deps.Store)
	}
	m := &newsModel{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), r: r, st: st,
		claims: &newsModelStore{Store: st}, clock: &heldClock{}, latest: 4, base: 4, decisionOnly: map[int]bool{},
		newsOf: map[string]record.DeliveryNews{}, runOf: map[string]int{}, told: map[int]bool{}, ran: map[string]int{},
		kinds: map[string]bool{}}
	r.sup.deps.Store, r.sup.deps.Clock = m.claims, m.clock
	r.client.answer = m.answer
	r.client.before = func(_ string, version int) (int, error) { return version - 1, nil }
	// GitHub already holds each issue's branch, so the row that creates one finishes.
	r.runner.githubAPI = newBranchGitHub(t, r.rt, branchExists).url
	m.machine, m.conn, m.token = r.reviewerBetweenTurns(pathChild)
	m.seen = len(m.conn.Prompts())
	m.openSeq = r.toldOf(pathChild, record.AudienceHolder).Opening
	m.told[m.openSeq] = true
	m.messages = len(r.publishedTo(m.token))
	return m
}

// answer is Dispatch's change set of pair, by the model's versions: decision-only where every
// version after From up to To changed only decision blocks, a changed section otherwise.
func (m *newsModel) answer(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
	changes := dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}
	for v := pair.From + 1; v <= pair.To; v++ {
		if !m.decisionOnly[v] {
			changes.DecisionOnly = false
			changes.Sections = []dispatch.SectionChange{{Heading: "Design", Change: dispatch.ChangeChanged}}
			break
		}
	}
	return changes, nil
}

// fail fails the run, naming its seed and every step it took.
func (m *newsModel) fail(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf("seed %d, step %d: %s\nsteps:\n  %s", m.seed, m.step, fmt.Sprintf(format, args...), strings.Join(m.trace, "\n  "))
}

// drive runs steps random steps, each chosen among the ones the model enables, and holds the told
// row to the model after each.
func (m *newsModel) drive(steps int) {
	for m.step = 1; m.step <= steps; m.step++ {
		ops := m.enabled()
		name := ops[m.rng.IntN(len(ops))]
		m.trace = append(m.trace, name)
		m.ran[name]++
		m.kinds[name] = true
		m.do(name)
		m.observe()
		m.check()
		m.trace[len(m.trace)-1] += " → " + m.state()
	}
	if os.Getenv("LEGION_NEWS_MODEL_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "seed %d steps:\n  %s\n", m.seed, strings.Join(m.trace, "\n  "))
	}
}

// state is what a step left, for the trace: the reviewer's row, the approved version, the run and
// what the model says its holder took, what the claim holds and what the agent holds.
func (m *newsModel) state() string {
	row := m.r.toldOf(pathChild, record.AudienceHolder)
	took, known := row.Took(record.DesignStepSpec)
	pending := "none"
	if p := m.machine.Claim().Pending; p != nil {
		pending = fmt.Sprintf("%s confirmed=%t carried=%t ran=%t", p.ID, !p.ConfirmedAt.IsZero(), len(p.Carried) > 0, p.Ran)
	}
	return fmt.Sprintf("took %d (known %t) owed %t (opening %d) of %d; run %d model %d..%d owed %t; claim %s %s; agent next %q turn %q",
		took, known, row.OpeningOwed, row.Opening, m.latest, m.run, m.truth(false), m.truth(true), m.owed(), m.machine.Claim().State, pending, m.next, m.turn)
}

// enabled is every step the model allows now, in a fixed order, so a seed always picks the same.
// The agent's own steps are listed twice and a person's new version only while the agent holds no
// prompt, so a run spends its steps between turns and inside them rather than on a gate that moves
// faster than any task can tell it; the gate can still close and open again while a task waits. A
// clear of a generation is listed while the agent holds a prompt or is in a turn: the claim then
// holds what the clear must leave counting for nothing. A seed makes at most newsModelClears of
// them, so the steps left are spent telling the run the last one began.
func (m *newsModel) enabled() []string {
	ops := []string{"reconcile", "reconcile", "sweep", "starts", "starts", "close-and-reopen", "hello"}
	if m.latest < 12 && m.next == "" && m.turn == "" {
		ops = append(ops, "approve-changed", "approve-decision-only")
	}
	if m.next != "" && m.turn == "" {
		ops = append(ops, "turn-start", "turn-start")
	}
	if m.turn != "" {
		ops = append(ops, "result", "result", "provider-error")
		if p := m.machine.Claim().Pending; p != nil && !p.ConfirmedAt.IsZero() {
			ops = append(ops, "supersede")
		}
	}
	if m.turn == "" && m.next == "" {
		ops = append(ops, "restart")
	}
	if m.clock.armedLadder() {
		ops = append(ops, "ladder", "ladder")
	}
	if m.claims.armed() == 0 {
		ops = append(ops, "fail-writes")
	}
	if m.run < newsModelClears && (m.next != "" || m.turn != "") {
		ops = append(ops, "reenter", "readmit")
	}
	return ops
}

// do runs the step name.
func (m *newsModel) do(name string) {
	m.t.Helper()
	switch name {
	case "approve-changed", "approve-decision-only":
		m.latest++
		m.decisionOnly[m.latest] = name == "approve-decision-only"
		m.r.dispatchEvent(intake.DispatchArtifactVersion, m.latest)
		m.r.dispatchEvent(intake.DispatchArtifactApproved, m.latest)
		m.opened()
	case "close-and-reopen":
		m.r.dispatchEvent(intake.DispatchArtifactChangesRequested, m.latest)
		m.r.dispatchEvent(intake.DispatchArtifactApproved, m.latest)
		m.opened()
	case "reconcile":
		m.r.later(time.Hour)
		m.r.run(ofKind(record.OutboxKindDesignChanges))
	case "sweep":
		// The entry sweep's tick: admission's pass, which sends each role the entry its phases
		// row is owed (a claim a clear left behind the entry its run raised), and its backstop for
		// news.
		m.ticks++
		applyFact(m.t, m.r.pool, "controller", fmt.Sprintf("tick-%d", m.ticks), intake.ControllerTick{}, m.r.engine, m.r.admission)
		m.r.sweepNews()
	case "starts":
		m.r.later(time.Hour)
		m.r.run(ofKind(record.OutboxKindIssueBranch))
		m.r.run(reviewerStarts)
	case "turn-start":
		m.handle(supervise.StreamTurnStart{Claim: m.token})
		m.turn, m.next = m.next, ""
	case "result":
		m.handle(supervise.StreamTurnEnd{Claim: m.token})
		m.handle(supervise.StreamPromptResult{Claim: m.token, DeliveryID: m.turn, AgentInvoked: true})
		m.took = append(m.took, newsModelPrompt{id: m.turn, news: m.newsOf[m.turn]})
		m.turn = ""
	case "provider-error":
		m.handle(supervise.StreamTurnEnd{Claim: m.token})
		m.handle(supervise.StreamPromptResult{Claim: m.token, DeliveryID: m.turn, AgentInvoked: true,
			Error: "overloaded_error", HTTPStatus: 529})
		m.turn = ""
	case "ladder":
		m.clock.fireLadder()
		m.machine.Wait()
	case "supersede":
		// A newer task of the phase, as an operator's or a repair's: it carries no entry, so the
		// claim's given entry stays the one its phases row names, and a news-only start still
		// reaches it. It takes the place of the confirmed task whose turn runs (queue).
		m.handle(supervise.RequestDeliver{Claim: m.token, ID: fmt.Sprintf("model:%d", m.step), Task: "Review the change again.",
			Phase: phase.Reviewing, Generation: readIssue(m.t, m.r.pool, pathChild).Generation})
		m.ambiguous = true
	case "reenter":
		m.reenter()
	case "readmit":
		m.readmit()
	case "restart":
		m.restart()
	case "hello":
		m.handle(supervise.StreamHello{Claim: m.token, Generation: m.machine.Claim().Generation})
	case "fail-writes":
		m.claims.refuse(2)
		m.ambiguous = true
	default:
		m.fail("no step %q", name)
	}
}

// handle posts ev to the reviewer's machine and waits for what it started. A decision a refused
// write cut short is no failure of the run: the machine owes that write and makes it again.
func (m *newsModel) handle(ev supervise.Event) {
	m.t.Helper()
	if err := m.machine.Handle(m.r.ctx, ev); err != nil && !errors.Is(err, errModelWrite) {
		m.fail("handle %T: %v", ev, err)
	}
	m.machine.Wait()
}

// newsModelClears is how many clears of a generation one seed makes at most (newsModel.enabled).
const newsModelClears = 2

// reviewerStarts matches every start of the child's reviewer: its phase's own, which a run that
// began again raises anew and which waits for the row that creates the issue's branch, and one
// queued for news alone.
func reviewerStarts(row pathRow) bool {
	request, ok := row.payload.(record.SuperviseRequest)
	return ok && row.issue == pathChild && request.Op == "start" && request.Role == claim.RoleReviewer
}

// cleared is the model's side of a clear of a generation, whose rule is that it empties what the
// issue's holders took: every prompt sent so far is of the run that ended, the next run's holder
// starts from base, and no opening of the run that ended is one of its own.
func (m *newsModel) cleared(base int) {
	m.run++
	m.base = base
	m.told = map[int]bool{}
}

// reenter is a person setting the child back to todo while its tree runs: its run starts again as
// its next generation (workflow's ReenterChild), and the reviewer's claim is kept with whatever it
// holds. The gate stands open, so the child's first phase starts at once and takes the approved
// version as its baseline. The model then brings that run back to reviewing, each phase completed
// by its role, and the outbox runs the reviewer's start of the phase where the claim can take it.
// Dispatch shows the child's own status before the todo, as the daemon's status write makes it: a
// todo that only repeats what Dispatch last showed moves nothing.
func (m *newsModel) reenter() {
	m.t.Helper()
	// A prompt the model has not read yet was sent before the clear.
	m.observe()
	if child := readIssue(m.t, m.r.pool, pathChild); child.DispatchStatus != child.Status {
		m.r.statusEvent(pathChild, child.Status)
	}
	m.r.reenter(pathChild)
	m.cleared(m.latest)
	for range 3 {
		child := readIssue(m.t, m.r.pool, pathChild)
		if child.Phase == phase.Admitted {
			m.fail("%s stands at admitted after its re-entry under an open gate, want its first phase started", pathChild)
		}
		if child.Phase == phase.Reviewing {
			break
		}
		m.r.complete(pathChild)
	}
	if child := readIssue(m.t, m.r.pool, pathChild); child.Phase != phase.Reviewing {
		m.fail("%s stands in %s after its next run's phases were completed, want reviewing", pathChild, child.Phase)
	}
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	m.r.run(reviewerStarts)
	m.machine.Wait()
	// The run began under an open gate: no opening is owed to it.
	m.openSeq = m.r.toldOf(pathChild, record.AudienceHolder).Opening
	m.told[m.openSeq] = true
}

// readmit is a person closing the root and setting it back to todo inside the linger, before any
// suspension the linger queued has run: a new generation of the whole tree (admit's readmit), which
// clears what every issue of it took and its gate, and raises the root's generation alone. The
// child keeps its generation and its phase, and the reviewer's claim whatever it holds. The tree's
// architect then registers the spec again at the version a person approved. A tree whose gate rows
// went with the clear stands released until a row closes it (workflow's standingGateOpen), so that
// registration opens nothing and owes the run no opening. Nothing says yet what the child's holder
// stood on: its row knows no version until the reconcile reads the review history.
func (m *newsModel) readmit() {
	m.t.Helper()
	// A prompt the model has not read yet was sent before the clear.
	m.observe()
	before := readIssue(m.t, m.r.pool, pathRoot)
	m.r.statusEvent(pathRoot, record.DoneStatus)
	m.r.statusEvent(pathRoot, "todo")
	if root := readIssue(m.t, m.r.pool, pathRoot); root.Generation != before.Generation+1 || root.Lingers() {
		m.fail("%s = %+v after it was closed and set back to todo, want generation %d and no linger", pathRoot, root, before.Generation+1)
	}
	m.cleared(newsModelUnknown)
	m.r.marks++
	applyFact(m.t, m.r.pool, "api", fmt.Sprintf("register-%s-%d", pathDoc, m.r.marks), intake.GateRegistered{Issue: pathRoot,
		ArtifactID: pathDoc, Version: m.latest, ApprovedVersion: m.latest, ApprovedMark: record.EventMark{Seq: m.r.marks},
		ReviewedBy: dispatch.Actor{Kind: "user", ID: "alice"}}, m.r.engine, m.r.admission)
	m.opened()
	m.told[m.openSeq] = true
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	m.r.run(reviewerStarts)
	m.machine.Wait()
}

// opened runs the role messages the gate's move queued and reads the opening that stands now: the
// told row's opening number, and whether the opened message told it to the reviewer.
func (m *newsModel) opened() {
	m.t.Helper()
	m.r.run(ofKind(record.OutboxKindRoleMessage))
	m.openSeq = m.r.toldOf(pathChild, record.AudienceHolder).Opening
	messages := m.r.publishedTo(m.token)
	for _, message := range messages[m.messages:] {
		if strings.HasPrefix(message, "The design gate of "+pathRoot+" opened") {
			m.told[m.openSeq] = true
		}
	}
	m.messages = len(messages)
}

// restart is the daemon stopped and booted again over the claims the store holds, the reviewer's
// shim connecting to it and saying hello. The agent holds no prompt it has not started a turn on.
func (m *newsModel) restart() {
	m.t.Helper()
	// A store that lags the machine being stopped holds a delivery that machine had moved past in
	// memory alone: the booted machine reads the stored one, and the record stays where the last
	// landed write left it until a write of that delivery lands (lagging).
	if m.lagging() {
		at := m.stored()
		m.behind = &at
	}
	stored, err := m.st.Claims(m.r.ctx)
	if err != nil {
		m.fail("read the stored claims: %v", err)
	}
	deps := m.r.sup.deps
	m.r.sup.stop()
	sup := newSupervisor(context.Background(), nil, "legion", m.t.TempDir(), quietLogger())
	deps.Conns = fake.NewConns()
	// The booted daemon's timers are its own: a timer the stopped daemon armed is gone with its
	// process, so the step that fires a ladder never fires one of those into the stopped machine.
	m.clock = &heldClock{}
	deps.Clock = m.clock
	sup.deps = deps
	m.t.Cleanup(sup.stop)
	if _, err := sup.restore(m.r.ctx, stored); err != nil {
		m.fail("restore the stored claims: %v", err)
	}
	m.r.sup, m.r.conns, m.r.runner.supervisor = sup, deps.Conns.(*fake.Conns), sup
	m.machine, m.conn = m.r.reconnect(pathChild, claim.RoleReviewer, false)
	m.seen, m.next = 0, ""
	if err := m.machine.Handle(m.r.ctx, supervise.StreamHello{Claim: m.token, Generation: m.machine.Claim().Generation}); err != nil &&
		!errors.Is(err, errModelWrite) {
		m.fail("hello: %v", err)
	}
	m.machine.Wait()
}

// observe reads each prompt the reviewer was given since the model last looked, with the news the
// delivery it was sent for carries and the run it was sent in: a prompt names one rendering of
// that news, made for the run the issue stood in then.
func (m *newsModel) observe() {
	m.t.Helper()
	prompts := m.conn.Prompts()
	for _, prompt := range prompts[m.seen:] {
		var news record.DeliveryNews
		if p := m.machine.Claim().Pending; p != nil && p.ID == prompt.DeliveryID && len(p.News) > 0 {
			if err := json.Unmarshal(p.News, &news); err != nil {
				m.fail("decode the news of %s: %v", prompt.DeliveryID, err)
			}
		}
		m.newsOf[prompt.DeliveryID], m.runOf[prompt.DeliveryID] = news, m.run
		m.next = prompt.DeliveryID
	}
	m.seen = len(prompts)
}

// truth is the version of the spec the model says the reviewer took in the run its issue stands
// in: from the version that run started from (base), through every pair a prompt of that run told
// and the reviewer read, the one whose turn runs among them, and, where closed says so, through
// every version since that changed only decision blocks, which a holder knows without being told.
// It is what the record is held to never pass (closed) and, while nothing ambiguous happened, to
// reach (not closed).
func (m *newsModel) truth(closed bool) int {
	links := m.readItems()
	took := m.base
	for moved := true; moved; {
		moved = false
		for _, item := range links {
			if item.From != nil && *item.From <= took && item.To > took {
				took, moved = item.To, true
			}
		}
		for closed && took < m.latest && m.decisionOnly[took+1] {
			took, moved = took+1, true
		}
	}
	return took
}

// read is every prompt of the run the issue stands in that the reviewer read, or reads in the turn
// that runs. A prompt sent before a clear told the run that clear ended, whenever its turn ran.
func (m *newsModel) read() []newsModelPrompt {
	var read []newsModelPrompt
	for _, prompt := range m.took {
		if m.runOf[prompt.id] == m.run {
			read = append(read, prompt)
		}
	}
	if m.turn != "" && m.runOf[m.turn] == m.run {
		read = append(read, newsModelPrompt{id: m.turn, news: m.newsOf[m.turn]})
	}
	return read
}

// readItems is every item the reviewer read of the run its issue stands in (read).
func (m *newsModel) readItems() []record.DeliveryNewsItem {
	var items []record.DeliveryNewsItem
	for _, prompt := range m.read() {
		items = append(items, prompt.news.Items...)
	}
	return items
}

// owed says whether the reviewer has not been told the opening that stands: no role message and
// no prompt of this run that it read told that opening's number.
func (m *newsModel) owed() bool {
	if m.told[m.openSeq] {
		return false
	}
	for _, prompt := range m.read() {
		for _, opening := range prompt.news.Openings {
			if opening.Seq == m.openSeq && !opening.Closed {
				return false
			}
		}
	}
	return true
}

// lagging says whether the store holds the reviewer's delivery otherwise than the machine does: a
// refused write the machine still owes. A restart does not end that. The booted machine reads the
// stored delivery, so the two agree, yet what the stopped machine had decided of it in memory — a
// provider error's take-back, say — was never written, and the booted machine's own first write of
// it can be refused too. So a store that lagged at a restart lags until the stored delivery is
// another than it was then (behind).
func (m *newsModel) lagging() bool {
	m.t.Helper()
	stored := m.stored()
	if m.behind != nil {
		if stored == *m.behind {
			return true
		}
		m.behind = nil
	}
	held := m.machine.Claim().Pending
	if held == nil {
		return stored.held
	}
	return stored != newsModelStored{held: true, id: held.ID, confirmed: !held.ConfirmedAt.IsZero(), carried: string(held.Carried), ran: held.Ran}
}

// check holds the reviewer's told row, as every reader reads it, to the model. While a refused
// write leaves the stored delivery behind the one the machine holds, the credit the store holds is
// the one the last landed write left, which the machine's next write finishes or undoes; the rows
// are held to the model only once the store holds what the machine does (lagging).
//
// After a re-admission of the tree nothing says what the run's holder stood on, and the row knows
// no version, until the reconcile reads the review history: the version approved before the one
// that stands, which is the one below it (newNewsModel). The model takes that as the run's base
// at the step that leaves the row knowing a version, and holds the row to it from that step on.
func (m *newsModel) check() {
	m.t.Helper()
	row := m.r.toldOf(pathChild, record.AudienceHolder)
	took, known := row.Took(record.DesignStepSpec)
	if m.base == newsModelUnknown && known {
		m.base = m.latest - 1
	}
	if m.lagging() {
		m.ran["checks skipped while a write is owed"]++
		return
	}
	m.ran["checks"]++
	if m.base != newsModelUnknown {
		if !known {
			m.fail("the reviewer's row knows no version of the spec: %+v", row)
		}
		if upper := m.truth(true); took > upper {
			m.fail("the record says the reviewer took version %d, but it was told no further than %d", took, upper)
		}
	}
	owed := m.owed()
	if owed && !row.OpeningOwed {
		m.fail("the record owes the reviewer no opening, but no message or read prompt told it opening %d", m.openSeq)
	}
	if m.ambiguous {
		return
	}
	if m.base != newsModelUnknown {
		if lower := m.truth(false); took < lower {
			m.fail("the record says the reviewer took version %d, but it read the news up to %d", took, lower)
		}
	}
	if row.OpeningOwed != owed {
		m.fail("the record owes the reviewer the opening %d: %t, want %t", m.openSeq, row.OpeningOwed, owed)
	}
}

// quiesce ends the run: no write is refused, every row runs, and the agent takes every prompt it
// is given to its result, until nothing is left to send. The reviewer has then taken the approved
// version and owes no opening, and the model says it was told that much.
func (m *newsModel) quiesce() {
	m.t.Helper()
	m.claims.refuse(0)
	m.trace = append(m.trace, "quiesce")
	for range 40 {
		if m.turn != "" {
			m.do("result")
		} else if m.next != "" {
			m.do("turn-start")
		} else {
			if m.clock.armedLadder() {
				m.do("ladder")
			}
			// The shim's hello, as the sweep's observation in production: a decision of the
			// machine's, at whose head it makes every write a refused write left owed (settle,
			// owes), so the record reads the claim the machine holds.
			m.do("hello")
			m.do("sweep")
			m.do("reconcile")
			m.do("starts")
		}
		m.observe()
		m.check()
		m.trace = append(m.trace, "quiesce round → "+m.state()+"; outbox "+m.outboxRows())
		row := m.r.toldOf(pathChild, record.AudienceHolder)
		took, _ := row.Took(record.DesignStepSpec)
		if m.turn == "" && m.next == "" && m.machine.Claim().Pending == nil && took == m.latest && !row.OpeningOwed {
			if upper := m.truth(true); upper != m.latest {
				m.fail("at quiescence the record says version %d and the model %d", took, upper)
			}
			return
		}
	}
	row := m.r.toldOf(pathChild, record.AudienceHolder)
	var owed record.TreeNews
	m.r.inTx(func(tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(m.r.ctx, m.r.records, tx, config.DesignGateRootIssues.Steps(), pathRoot)
		owed = record.OwedNews(facts)
		return err
	})
	held := m.machine.Claim()
	m.fail("the run did not quiesce: the reviewer's row is %+v, the approved version %d, the claim holds %+v (state %s, given entry %d), the agent holds %q in a turn and %q not started; owed %+v",
		row, m.latest, held.Pending, held.State, held.GivenEntry, m.turn, m.next, owed)
}

// outboxRows is the kind and attempts of each row the outbox holds of the tree, for the trace.
func (m *newsModel) outboxRows() string {
	var rows []string
	for _, row := range m.r.observe() {
		_, attempts := m.r.still(row)
		rows = append(rows, fmt.Sprintf("%d:%s/%d", row.id, row.kind, attempts))
	}
	return strings.Join(rows, " ")
}

// armedLadder says whether a provider error's ladder wait is armed (fireLadder).
func (c *heldClock) armedLadder() bool {
	for _, wait := range newsModelLadder {
		if live, _ := c.armed(wait); live > 0 {
			return true
		}
	}
	return false
}

// fireLadder fires each armed wait of a provider error's ladder.
func (c *heldClock) fireLadder() {
	for _, wait := range newsModelLadder {
		c.fire(wait)
	}
}

// newsModelLadder is every wait a provider error's ladder arms (supervise's promptRetryDelay).
var newsModelLadder = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute}
