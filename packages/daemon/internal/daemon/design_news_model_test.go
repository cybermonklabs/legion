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

// The news record against a plain model of what the holder was really told. One tree, its root a
// member in reviewing whose reviewer holds the phase, runs through the daemon's own parts (the
// newsPathRig: the engine, admission, the outbox, the supervisor's machine over the claim store, the
// one render), and a seed's random sequence of steps drives it: a person approves a new version that
// changes the work or only its decision blocks, a person requests changes and approves again (the
// gate closes and opens), the reconcile runs (and raises past a pair that changed nothing of the
// work), the sweep queues it, the news-only starts run (a send, and its acknowledgement), the agent
// starts the turn of the prompt it holds (a confirmation), the turn's result comes (the credit made
// final) or ends in a provider error (a give-back), the provider error's ladder sends the task
// again, a newer task takes the place of the task whose turn runs (a supersede, a give-back of what
// the agent read), the daemon restarts, and the claim store fails a write and its repair.
//
// The model holds what the agent was given and took, as the agent sees it: each prompt's news, the
// prompt whose turn runs, the prompts whose turns ran, and each opening a role message told. After
// every step the told row is held to it:
//
//   - never ahead of it: the record never says the holder took a version it was not told, and never
//     says an opening told that was not (told twice, never lost);
//   - level with it while nothing has made the record give back what the agent did read (a
//     supersede of a task whose turn ran, a failed write): everything the agent read is credited, and
//     the record owes an opening exactly when the holder was not told it;
//   - and, once the run quiesces, the holder has taken the approved version and owes no opening.
//
// With no variable set it runs seed 19 for 40 steps, a few seconds: a seed whose run reaches every
// kind of step at least once (newsModelCISeed says how that was read). LEGION_NEWS_MODEL_SEED runs
// one seed, LEGION_NEWS_MODEL_RUNS runs seeds 1 to that many, LEGION_NEWS_MODEL_STEPS sets the
// steps each drives, and LEGION_NEWS_MODEL_TRACE prints each seed's steps and the state each left.
// A seed's steps are its own: every step waits for the machine before the next, and the machine's
// timers fire only when a step fires them.
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
// every kind of step: read from a run of seeds 1 to 60 at 40 steps with LEGION_NEWS_MODEL_TRACE,
// counting each seed's steps by kind; seeds 19 and 41 were the two that reached all fourteen.
const newsModelCISeed = 19

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
	ran     map[string]int

	// latest is the newest version of the spec, every one of them approved when it is saved, and
	// decisionOnly says which versions changed only decision blocks of the one before.
	latest       int
	decisionOnly map[int]bool
	// seen is how many of conn's prompts the model has read, next the prompt the agent holds and
	// has not started a turn on, turn the prompt whose turn runs, and newsOf each prompt's news.
	seen   int
	next   string
	turn   string
	newsOf map[string]record.DeliveryNews
	// took is every prompt whose turn ran and was read, in order; told every opening number a role
	// message or a read prompt told; openSeq the number of the opening that stands now.
	took    []newsModelPrompt
	told    map[int]bool
	openSeq int
	// messages is how many role messages the reviewer had been sent when the model last looked.
	messages int
	// ambiguous says the record has been made to give back what the agent did read, or a write was
	// refused, so it may lag the model from here on: only the never-ahead rule still holds.
	ambiguous bool
}

// newNewsModel builds seed's run: the root in testing at version 4 of its spec, every audience
// having taken 4, the reviewer brought into reviewing and between turns, holding nothing.
func newNewsModel(t *testing.T, seed int64) *newsModel {
	t.Helper()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)
	st, ok := r.sup.deps.Store.(*store.Store)
	if !ok {
		t.Fatalf("the rig's claim store is %T, want the daemon's", r.sup.deps.Store)
	}
	m := &newsModel{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), r: r, st: st,
		claims: &newsModelStore{Store: st}, clock: &heldClock{}, latest: 4, decisionOnly: map[int]bool{},
		newsOf: map[string]record.DeliveryNews{}, told: map[int]bool{}, ran: map[string]int{}}
	r.sup.deps.Store, r.sup.deps.Clock = m.claims, m.clock
	r.client.answer = m.answer
	m.machine, m.conn, m.token = r.reviewerBetweenTurns(pathRoot)
	m.seen = len(m.conn.Prompts())
	m.openSeq = r.toldOf(pathRoot, record.AudienceHolder).Opening
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
		m.do(name)
		m.observe()
		m.check()
		m.trace[len(m.trace)-1] += " → " + m.state()
	}
	if os.Getenv("LEGION_NEWS_MODEL_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "seed %d steps:\n  %s\n", m.seed, strings.Join(m.trace, "\n  "))
	}
}

// state is what a step left, for the trace: the reviewer's row, the approved version, what the
// claim holds and what the agent holds.
func (m *newsModel) state() string {
	row := m.r.toldOf(pathRoot, record.AudienceHolder)
	took, _ := row.Took(record.DesignStepSpec)
	pending := "none"
	if p := m.machine.Claim().Pending; p != nil {
		pending = fmt.Sprintf("%s confirmed=%t carried=%t ran=%t", p.ID, !p.ConfirmedAt.IsZero(), len(p.Carried) > 0, p.Ran)
	}
	return fmt.Sprintf("took %d owed %t (opening %d) of %d; model %d..%d owed %t; claim %s %s; agent next %q turn %q",
		took, row.OpeningOwed, row.Opening, m.latest, m.truth(false), m.truth(true), m.owed(), m.machine.Claim().State, pending, m.next, m.turn)
}

// enabled is every step the model allows now, in a fixed order, so a seed always picks the same.
// The agent's own steps are listed twice and a person's new version only while the agent holds no
// prompt, so a run spends its steps between turns and inside them rather than on a gate that moves
// faster than any task can tell it; the gate can still close and open again while a task waits.
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
		m.r.sweepNews()
	case "starts":
		m.r.later(time.Hour)
		m.r.run(startOf(pathRoot, claim.RoleReviewer, true))
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
			Phase: phase.Reviewing, Generation: 1})
		m.ambiguous = true
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

// opened runs the role messages the gate's move queued and reads the opening that stands now: the
// told row's opening number, and whether the opened message told it to the reviewer.
func (m *newsModel) opened() {
	m.t.Helper()
	m.r.run(ofKind(record.OutboxKindRoleMessage))
	m.openSeq = m.r.toldOf(pathRoot, record.AudienceHolder).Opening
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
	m.machine, m.conn = m.r.reconnect(pathRoot, claim.RoleReviewer, false)
	m.seen, m.next = 0, ""
	if err := m.machine.Handle(m.r.ctx, supervise.StreamHello{Claim: m.token, Generation: m.machine.Claim().Generation}); err != nil &&
		!errors.Is(err, errModelWrite) {
		m.fail("hello: %v", err)
	}
	m.machine.Wait()
}

// observe reads each prompt the reviewer was given since the model last looked, with the news the
// delivery it was sent for carries: a prompt names one rendering of that news.
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
		m.newsOf[prompt.DeliveryID] = news
		m.next = prompt.DeliveryID
	}
	m.seen = len(prompts)
}

// truth is the version of the spec the model says the reviewer took: from 4, through every pair a
// read prompt told, the one whose turn runs among them, and, where closed says so, through every
// version since that changed only decision blocks, which a holder knows without being told. It is
// what the record is held to never pass (closed) and, while nothing ambiguous happened, to reach
// (not closed).
func (m *newsModel) truth(closed bool) int {
	links := m.readItems()
	took := 4
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

// readItems is every item of a prompt the reviewer read, or reads in the turn that runs.
func (m *newsModel) readItems() []record.DeliveryNewsItem {
	var items []record.DeliveryNewsItem
	for _, prompt := range m.took {
		items = append(items, prompt.news.Items...)
	}
	if m.turn != "" {
		items = append(items, m.newsOf[m.turn].Items...)
	}
	return items
}

// owed says whether the reviewer has not been told the opening that stands: no role message and
// no prompt it read told that opening's number.
func (m *newsModel) owed() bool {
	if m.told[m.openSeq] {
		return false
	}
	reading := m.took
	if m.turn != "" {
		reading = append(append([]newsModelPrompt{}, m.took...), newsModelPrompt{news: m.newsOf[m.turn]})
	}
	for _, prompt := range reading {
		for _, opening := range prompt.news.Openings {
			if opening.Seq == m.openSeq && !opening.Closed {
				return false
			}
		}
	}
	return true
}

// lagging says whether the store holds the reviewer's delivery otherwise than the machine does: a
// refused write the machine still owes.
func (m *newsModel) lagging() bool {
	m.t.Helper()
	claims, err := m.st.Claims(m.r.ctx)
	if err != nil {
		m.fail("read the stored claims: %v", err)
	}
	var stored *supervise.Delivery
	for _, c := range claims {
		if c.Token == m.token {
			stored = c.Pending
		}
	}
	held := m.machine.Claim().Pending
	switch {
	case stored == nil || held == nil:
		return (stored == nil) != (held == nil)
	default:
		return stored.ID != held.ID || stored.ConfirmedAt.IsZero() != held.ConfirmedAt.IsZero() ||
			string(stored.Carried) != string(held.Carried) || stored.Ran != held.Ran
	}
}

// check holds the reviewer's told row, as every reader reads it, to the model. While a refused
// write leaves the stored delivery behind the one the machine holds, the credit the store holds is
// the one the last landed write left, which the machine's next write finishes or undoes; the rows
// are held to the model only once the store holds what the machine does (lagging).
func (m *newsModel) check() {
	m.t.Helper()
	if m.lagging() {
		m.ran["checks skipped while a write is owed"]++
		return
	}
	m.ran["checks"]++
	row := m.r.toldOf(pathRoot, record.AudienceHolder)
	took, known := row.Took(record.DesignStepSpec)
	if !known {
		m.fail("the reviewer's row knows no version of the spec: %+v", row)
	}
	if upper := m.truth(true); took > upper {
		m.fail("the record says the reviewer took version %d, but it was told no further than %d", took, upper)
	}
	owed := m.owed()
	if owed && !row.OpeningOwed {
		m.fail("the record owes the reviewer no opening, but no message or read prompt told it opening %d", m.openSeq)
	}
	if m.ambiguous {
		return
	}
	if lower := m.truth(false); took < lower {
		m.fail("the record says the reviewer took version %d, but it read the news up to %d", took, lower)
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
			m.r.sweepNews()
			m.do("reconcile")
			m.do("starts")
		}
		m.observe()
		m.check()
		m.trace = append(m.trace, "quiesce round → "+m.state()+"; outbox "+m.outboxRows())
		row := m.r.toldOf(pathRoot, record.AudienceHolder)
		took, _ := row.Took(record.DesignStepSpec)
		if m.turn == "" && m.next == "" && m.machine.Claim().Pending == nil && took == m.latest && !row.OpeningOwed {
			if upper := m.truth(true); upper != m.latest {
				m.fail("at quiescence the record says version %d and the model %d", took, upper)
			}
			return
		}
	}
	row := m.r.toldOf(pathRoot, record.AudienceHolder)
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
