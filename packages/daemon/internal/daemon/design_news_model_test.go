package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
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

// The news record against a plain model of what each holder was really told. One tree, its root in
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
// Two steps reach what one claim alone cannot. In a hand-over, the child's next run after a re-entry
// stops in testing while a person approves a version that changes the work, its tester takes the
// phase task that tells it and completes in that turn, a person approves another version, and the
// reviewer's phase start tells that one from the version the tester's credit gave, which is not
// final while the tester's turn runs. A seed makes two: the first tester's turn ends in a provider
// error once the reviewer has taken that news to its result, which gives the tester's credit back
// under a credit counted from it; the second tester's turn runs on to the end of the drive, under
// whatever clear comes, so a credit of a run that ended stands in the next. And the tree root's
// architect is handed the task of a new entry that names two decisions a person made while nobody
// could hear them and carries the architect's news, and a newer decision replaces one of the two
// before the task is sent, so its send narrows it to the decision that stays (supervise's
// currentRootTask, the daemon's rootTask) and renders its news again under the narrowed text; the
// architect takes that task to its result.
//
// The test injects every failure itself, so the model knows what each step did. It follows two told
// rows — the child's holder row, credited by whichever of the child's reviewer and tester holds its
// phase, and the root's architect row, credited by the root's architect — and, for each claim, what
// its agent was given, read from each prompt's own text, and which of those prompts it read; and what
// the claim store credits each row with, read from the writes the store let through
// (newsModelStore): a confirmed copy whose turn's result landed is final, the copy a confirmed
// delivery holds stands until then, and the reconcile raises past a pair that changed nothing of the
// work exactly where the record says it does (newsModel.settled). After every step each told row is
// held to it:
//
//   - exactly: the version the row says its holder took is the one the run's baseline, the raises
//     and the credits the claim store holds reach (the SQL function design_news_known), counting
//     only news rendered for the run the issue stands in (design_news_current), and the row owes the
//     opening that stands exactly when no role message told it, no final credit settled it and no
//     standing credit carried it (design_news_opening_told, design_news_told_in);
//   - never ahead of what the holder was told: the record never says it took a version no prompt
//     its holders read told it, beyond versions that changed only decision blocks, and never says an
//     opening told that no message and no prompt they read told (told twice, never lost);
//   - level with what its holders read: everything they read is credited, but a prompt a newer task
//     took the place of in its turn (a supersede) and one whose final credit a refused write lost,
//     and the record owes an opening exactly when no message and no prompt whose credit stands told
//     it;
//   - the news a delivery records is the news its prompt carried (invariant 1): the news the delivery
//     holds when its prompt goes, and the copy its acknowledgement writes, which its confirmation
//     credits, name the start's own text the prompt carries, exactly the version pairs the prompt's
//     text names, and the opening it says opened, rendered for the run the prompt was sent in;
//   - a clear empties what the issue's holders took: the next run starts from its own baseline —
//     the version approved when its first phase started, or, after a re-admission of the tree, the
//     version the review history names once the reconcile has read it — and counts only what a
//     prompt sent in that run told;
//   - and, once the run quiesces, the reviewer has taken the approved version and owes no opening.
//
// A refused write leaves the claim store behind the machine until the machine's next write of the
// same delivery finishes it or undoes it (lagging). The model reads the credit the store holds, so
// the exact rule and the never-ahead rule hold through it; but whether the agents' own view is level
// with the record has two answers until the store has caught up, so each step after which a claim's
// store lags is counted as ambiguous, with how many steps each such stretch lasts until the step
// that settles it (newsModelAmbiguity), and the level rule is held again from that step on. Each seed
// logs its counts, and the run logs their totals.
//
// With no variable set it runs newsModelCISeed for 40 steps, a few seconds: a seed whose run
// reaches eighteen of the twenty kinds of step and that each of the record's rules, put back wrong
// one at a time, fails (newsModelCISeed says how that was read). LEGION_NEWS_MODEL_SEED runs one seed,
// LEGION_NEWS_MODEL_RUNS runs seeds 1 to that many (1 runs seed 1 alone), LEGION_NEWS_MODEL_STEPS
// sets the steps each drives, and LEGION_NEWS_MODEL_TRACE prints each seed's steps and the state each
// left. A seed's steps are its own: every step waits for the machines before the next, and the
// machines' timers fire only when a step fires them.
func TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold(t *testing.T) {
	steps := keptPropertyEnv(t, "LEGION_NEWS_MODEL_STEPS", 40)
	seeds := []int64{newsModelCISeed}
	if named := os.Getenv("LEGION_NEWS_MODEL_SEED"); named != "" {
		seed, err := strconv.ParseInt(named, 10, 64)
		if err != nil {
			t.Fatalf("LEGION_NEWS_MODEL_SEED=%q is no seed: %v", named, err)
		}
		seeds = []int64{seed}
	} else if runs := keptPropertyEnv(t, "LEGION_NEWS_MODEL_RUNS", 0); runs > 0 {
		seeds = seeds[:0]
		for seed := int64(1); seed <= int64(runs); seed++ {
			seeds = append(seeds, seed)
		}
	}
	var mu sync.Mutex
	counts := map[string]int{}
	var ambiguous newsModelAmbiguity
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			m := newNewsModel(t, seed)
			m.drive(steps)
			m.quiesce()
			t.Logf("seed %d ran %d kinds of step: %v; %s", seed, len(m.kinds), m.ran, m.ambiguity)
			mu.Lock()
			defer mu.Unlock()
			for op, n := range m.ran {
				counts[op] += n
			}
			ambiguous.add(m.ambiguity)
		})
	}
	t.Logf("%d seeds of %d steps; steps run: %v; %s", len(seeds), steps, counts, ambiguous)
}

// newsModelCISeed is the seed the model test runs where no variable names one. Its 40 steps reach
// eighteen of the twenty kinds of step — all but the provider error's ladder and the restart — the
// two hand-overs, a re-entry, a re-admission and the root architect's narrowed task among them, and
// the run fails under each of the seven rules the record keeps put back wrong one at a time (MA to MG
// of the round-12 mutations: a credit counted in any run, a credit reaching from any version, a fold
// that keeps no unreached credit, a read that counts no standing credit, a fold before the turn's
// result, a carried opening that clears any opening, and a narrowed root task that keeps the news
// it held). Read from runs of seeds 1 to 100 at 40 steps: unmutated with -v, which prints the kinds
// each seed's drive chose (newsModel.kinds), and under each mutation. 91 of those seeds failed under
// all seven, no seed reached all twenty kinds, and 35 is one of the five that fail under all seven
// and reach eighteen.
const newsModelCISeed = 35

// errModelWrite is a write the model's claim store refused (newsModelStore).
var errModelWrite = errors.New("the claim store refused this write")

// newsModelStore is the daemon's claim store with a switch that refuses the next writes of the
// reviewer's delivery — a write and the repair of it that settle makes before the next decision —
// and a log of every write of a delivery it let through, which is what the model reads the claim
// store's credits from (newsModel.drain): the record reads a credit from the delivery rows these
// writes leave, and the model reads it from the same writes by its own rule.
type newsModelStore struct {
	*store.Store
	// refusing is the claim whose writes an armed refusal refuses: the reviewer's.
	refusing claim.Token
	mu       sync.Mutex
	fails    int
	// spent is how many refusals have refused a write so far.
	spent  int
	landed []newsModelWrite
}

// newsModelWrite is one write of a delivery the claim store took: the delivery a put wrote, or nil
// for the retirement of one.
type newsModelWrite struct {
	token    claim.Token
	delivery *supervise.Delivery
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

// spentCount is how many refusals have refused a write so far.
func (s *newsModelStore) spentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent
}

// take spends one armed refusal on a write of token's delivery, if any is armed and token is the
// claim refusals are armed for.
func (s *newsModelStore) take(token claim.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 && token == s.refusing {
		s.fails--
		s.spent++
		return errModelWrite
	}
	return nil
}

// land logs a write the claim store took.
func (s *newsModelStore) land(token claim.Token, d *supervise.Delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.landed = append(s.landed, newsModelWrite{token: token, delivery: d})
}

// drain answers every write logged since the last drain, in the order the store took them.
func (s *newsModelStore) drain() []newsModelWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	landed := s.landed
	s.landed = nil
	return landed
}

func (s *newsModelStore) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	if err := s.take(token); err != nil {
		return err
	}
	if err := s.Store.PutDelivery(ctx, token, d); err != nil {
		return err
	}
	s.land(token, &d)
	return nil
}

func (s *newsModelStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	if err := s.take(c.Token); err != nil {
		return err
	}
	if err := s.Store.PutClaimAndDelivery(ctx, c, d); err != nil {
		return err
	}
	s.land(c.Token, &d)
	return nil
}

func (s *newsModelStore) RetireDelivery(ctx context.Context, c supervise.Claim, id string) error {
	if err := s.take(c.Token); err != nil {
		return err
	}
	if err := s.Store.RetireDelivery(ctx, c, id); err != nil {
		return err
	}
	s.land(c.Token, nil)
	return nil
}

// newsModelUnknown is the version a run's holder started from where nothing has said it yet
// (newsModelRow.base), and the version a closure reaches from it while no credit reaches it.
const newsModelUnknown = -1

// newsModelAny is the earlier version of a credit told with none (record.DeliveryNewsItem.From
// nil), which reaches any row.
const newsModelAny = -2

// newsModelNoOpening is a prompt that told no opening (newsModelPrompt.opening).
const newsModelNoOpening = -1

// newsModelLink is one credit as the record reads it (the SQL function design_news_known): it
// reaches a version at or above from, or any version where from is newsModelAny, and takes it to to.
type newsModelLink struct{ from, to int }

func (l newsModelLink) String() string {
	if l.from == newsModelAny {
		return fmt.Sprintf("any→%d", l.to)
	}
	return fmt.Sprintf("%d→%d", l.from, l.to)
}

// newsModelClosure is the version base and links reach together (design_news_known): every link
// that reaches is followed until none reaches further, whatever their order. A base the record
// does not know is reached only through a link told with no earlier version.
func newsModelClosure(base int, links []newsModelLink) (int, bool) {
	took, known := base, base != newsModelUnknown
	for moved := true; moved; {
		moved = false
		for _, link := range links {
			if (link.from == newsModelAny || (known && link.from <= took)) && (!known || link.to > took) {
				took, known, moved = link.to, true, true
			}
		}
	}
	return took, known
}

// newsModelPrompt is one prompt a holder was given, as its own text says it: the text, the run it
// was sent in (its row's run, and the generations of the row's issue and its tree's root then), each
// version pair it names, and the opening it said opened, by the number the row held for the opening
// that stood when it was sent (newsModelNoOpening for none).
type newsModelPrompt struct {
	text         string
	run          int
	gen, treeGen uint64
	pairs        []newsModelLink
	opening      int
}

// tells says whether p told any news: a version pair or the opening.
func (p *newsModelPrompt) tells() bool { return len(p.pairs) > 0 || p.opening != newsModelNoOpening }

// newsModelRow is one told row the model follows, the claims whose deliveries credit it, and what
// the model says the record holds of it.
type newsModelRow struct {
	name     string
	issue    string
	audience record.DesignAudience
	agents   []*newsModelAgent
	// run counts the clears of a generation the row has been through, and base is the version the
	// run's holder started from: the baseline its first phase took, the version the architect took
	// at a registration, or, after a re-admission of the tree, newsModelUnknown until the reconcile
	// has read it from the review history.
	run  int
	base int
	// final is every credit of this run the record folded into the row: each confirmed copy whose
	// turn's result landed in the claim store, and each raise of the reconcile. cleared is each
	// opening, by its number, such a fold settled.
	final   []newsModelLink
	cleared map[int]bool
	// told is every opening number of this run a role message told, and openSeq the number of the
	// opening that stands now.
	told    map[int]bool
	openSeq int
	// prompts is every prompt the row's claims were given, by delivery id.
	prompts map[string]*newsModelPrompt
}

// newsModelAgent is one claim whose agent the model follows: what it was given and did, and what
// the claim store took of its deliveries.
type newsModelAgent struct {
	name  string
	row   *newsModelRow
	issue string
	role  claim.Role
	token claim.Token
	// machine and conn are the claim and the connection its prompts reach, nil until a start
	// creates the claim.
	machine *supervise.Machine
	conn    *fake.Conn
	// seen is how many of conn's prompts the model has read, next the prompt the agent holds and
	// has not started a turn on, turn the prompt whose turn runs, and messages how many role
	// messages its topic had carried when the model last looked.
	seen     int
	next     string
	turn     string
	messages int
	// took is every prompt whose turn ran and was read, in order; errored each prompt whose turn
	// ended in a provider error, which the agent did not take; superseded each prompt whose turn ran
	// while its claim held another task, or none (a newer task took its place), whose credit that
	// gave back.
	took       []string
	errored    map[string]bool
	superseded map[string]bool
	// ranAt is each prompt whose turn's result came and whose final credit the claim store has not
	// landed yet, with how many refusals had been spent before that result; finalized each delivery
	// id the claim store wrote final; lost each prompt whose final credit a refused write lost: its
	// result came, a write was refused since, and the store caught up without making it final.
	ranAt     map[string]int
	finalized map[string]bool
	lost      map[string]bool
	// carried is the copy the last write of the claim's delivery the store took held, and behind the
	// delivery as the store held it at a restart that found the store lagging the stopped machine,
	// nil otherwise (lagging).
	carried []byte
	behind  *newsModelStored
}

// newNewsModelRow is issue's audience row starting from version 4 of the spec, its opening told.
func newNewsModelRow(name, issue string, audience record.DesignAudience) *newsModelRow {
	return &newsModelRow{name: name, issue: issue, audience: audience, base: 4, cleared: map[int]bool{}, told: map[int]bool{},
		prompts: map[string]*newsModelPrompt{}}
}

// agent adds role's claim on issue to the claims crediting r.
func (r *newsModelRow) agent(t *testing.T, name, issue string, role claim.Role) *newsModelAgent {
	a := &newsModelAgent{name: name, row: r, issue: issue, role: role, token: mustClaimToken(t, issue, role), errored: map[string]bool{},
		superseded: map[string]bool{}, ranAt: map[string]int{}, finalized: map[string]bool{}, lost: map[string]bool{}}
	r.agents = append(r.agents, a)
	return a
}

// readPrompts is every prompt of the row's run its agents read, or read in the turn that runs, that
// keep keeps: a prompt sent before a clear told the run that clear ended, whenever its turn ran.
func (r *newsModelRow) readPrompts(keep func(a *newsModelAgent, id string) bool) []*newsModelPrompt {
	var read []*newsModelPrompt
	for _, a := range r.agents {
		ids := slices.Clone(a.took)
		if a.turn != "" {
			ids = append(ids, a.turn)
		}
		for _, id := range ids {
			if p := r.prompts[id]; p != nil && p.run == r.run && keep(a, id) {
				read = append(read, p)
			}
		}
	}
	return read
}

// toldPrompts is every prompt the record may count as told to the row's holders: each they read,
// and, while the claim store lags a machine, each whose turn ended in a provider error, whose
// take-back is a write the store still owes.
func (r *newsModelRow) toldPrompts(lagging bool) []*newsModelPrompt {
	told := r.readPrompts(func(*newsModelAgent, string) bool { return true })
	if lagging {
		for _, a := range r.agents {
			for id := range a.errored {
				if p := r.prompts[id]; p != nil && p.run == r.run {
					told = append(told, p)
				}
			}
		}
	}
	return told
}

// creditedPrompts is every prompt the row's holders read whose credit the steps leave standing:
// each they read but one a supersede gave back and one a refused write lost.
func (r *newsModelRow) creditedPrompts() []*newsModelPrompt {
	return r.readPrompts(func(a *newsModelAgent, id string) bool { return !a.superseded[id] && !a.lost[id] })
}

// newsModelHolderPair and newsModelHolderUnchanged are how a holder's news names a version pair
// (workflow.RenderNews): one that changed the work, and one that changed decision blocks alone.
// newsModelArchitectPair is how the root architect's summary names a pair of its own.
var (
	newsModelHolderPair      = regexp.MustCompile(`version (\d+), since version (\d+), which your phase last took`)
	newsModelHolderUnchanged = regexp.MustCompile(`Nothing your issue holds changed since version (\d+), which your phase took\. Version (\d+) of its`)
	newsModelArchitectPair   = regexp.MustCompile(`version (\d+), since version (\d+), which you last took`)
)

// named is what text tells r's holder of news: each version pair it names as the holder's own, and
// whether it says the gate opened.
func (r *newsModelRow) named(text string) ([]newsModelLink, bool) {
	var pairs []newsModelLink
	add := func(from, to string) {
		f, _ := strconv.Atoi(from)
		t, _ := strconv.Atoi(to)
		pairs = append(pairs, newsModelLink{from: f, to: t})
	}
	if r.audience == record.AudienceArchitect {
		for _, found := range newsModelArchitectPair.FindAllStringSubmatch(text, -1) {
			add(found[2], found[1])
		}
	} else {
		for _, found := range newsModelHolderPair.FindAllStringSubmatch(text, -1) {
			add(found[2], found[1])
		}
		for _, found := range newsModelHolderUnchanged.FindAllStringSubmatch(text, -1) {
			add(found[1], found[2])
		}
	}
	return pairs, strings.Contains(text, "The design gate of "+pathRoot+" opened:")
}

// newsModelAmbiguity counts the steps after which the record could stand in either of two states
// the agents' view allows, because a refused write left a claim store behind its machine: how many
// such steps, how many stretches of them, and how long the longest stretch lasted, in steps until
// the step that settled it. open is the length of the stretch still running.
type newsModelAmbiguity struct {
	steps, stretches, longest, open int
}

// step counts one step, ambiguous or not.
func (a *newsModelAmbiguity) step(ambiguous bool) {
	if !ambiguous {
		a.open = 0
		return
	}
	a.steps++
	a.open++
	if a.open == 1 {
		a.stretches++
	}
	a.longest = max(a.longest, a.open)
}

// add counts b's steps in a.
func (a *newsModelAmbiguity) add(b newsModelAmbiguity) {
	a.steps += b.steps
	a.stretches += b.stretches
	a.longest = max(a.longest, b.longest)
}

func (a newsModelAmbiguity) String() string {
	mean := 0.0
	if a.stretches > 0 {
		mean = float64(a.steps) / float64(a.stretches)
	}
	return fmt.Sprintf("ambiguous steps %d in %d stretches (mean %.1f, longest %d steps until settled)", a.steps, a.stretches, mean, a.longest)
}

// newsModel is one seed's run: the rig, the machines' clock, the two rows and their claims, and
// what the model says of the gate and of each run.
type newsModel struct {
	t      *testing.T
	seed   int64
	rng    *rand.Rand
	r      *newsPathRig
	st     *store.Store
	claims *newsModelStore
	clock  *heldClock
	step   int
	trace  []string
	// ran counts each step the run made and each check, and kinds is the kinds of step the drive
	// chose.
	ran   map[string]int
	kinds map[string]bool

	// latest is the newest version of the spec, every one of them approved when it is saved, and
	// decisionOnly says which versions changed only decision blocks of the one before.
	latest       int
	decisionOnly map[int]bool
	// childGen and rootGen are the generations the child and the tree's root stand at: a re-entry
	// raises the child's, a re-admission the root's. clears counts the re-entries and re-admissions,
	// and handovers the hand-overs.
	childGen, rootGen uint64
	clears, handovers int
	// handoverClear counts the clear made under the second hand-over's tester credit, which no
	// other clear uses up (handoverClearDue).
	handoverClear int
	// child is the child's holder row and root the root's architect row; reviewer, tester and
	// architect the claims that credit them.
	child, root                 *newsModelRow
	reviewer, tester, architect *newsModelAgent
	// reviewed says the reviewer has taken to its result a prompt of the run that told it a version
	// since the first hand-over, while that hand-over's tester credit stood: a credit counted from
	// the version the tester's gave (newsModel.result).
	reviewed bool
	// ticks is how many admission passes the sweep has made, and entry the newest entry the root
	// architect was handed.
	ticks     int
	entry     int64
	ambiguity newsModelAmbiguity
}

// newsModelStored is a delivery as the claim store holds it, as far as its credit goes: whether it
// holds one, under which id, confirmed or not, with which copy, final or not. The copy is its JSON
// with its keys in order: the store reads it back from a jsonb column, which writes it its own way.
type newsModelStored struct {
	held      bool
	id        string
	confirmed bool
	carried   string
	ran       bool
}

// storedFrom is the delivery d as far as its credit goes (newsModelStored), the zero value for nil.
func storedFrom(d *supervise.Delivery) newsModelStored {
	if d == nil {
		return newsModelStored{}
	}
	carried := string(d.Carried)
	var copied any
	if json.Unmarshal(d.Carried, &copied) == nil {
		if ordered, err := json.Marshal(copied); err == nil {
			carried = string(ordered)
		}
	}
	return newsModelStored{held: true, id: d.ID, confirmed: !d.ConfirmedAt.IsZero(), carried: carried, ran: d.Ran}
}

// storedDelivery is token's delivery as the claim store holds it now, nil for none.
func (m *newsModel) storedDelivery(token claim.Token) *supervise.Delivery {
	m.t.Helper()
	claims, err := m.st.Claims(m.r.ctx)
	if err != nil {
		m.fail("read the stored claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c.Pending
		}
	}
	return nil
}

// newNewsModel builds seed's run: the root in implementing and its child in testing at version 4
// of the root's spec, every audience having taken 4, the child's reviewer brought into reviewing
// and between turns, holding nothing. The child's tester and the root's architect have no claim
// until a step starts one. The review history names, for each version, the one below it: every
// version of a run is approved when it is saved.
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
	child := newNewsModelRow("child's holder", pathChild, record.AudienceHolder)
	root := newNewsModelRow("root's architect", pathRoot, record.AudienceArchitect)
	m := &newsModel{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), r: r, st: st, clock: &heldClock{},
		latest: 4, decisionOnly: map[int]bool{}, childGen: 1, rootGen: 1, child: child, root: root, ran: map[string]int{}, kinds: map[string]bool{}}
	m.reviewer = child.agent(t, "reviewer", pathChild, claim.RoleReviewer)
	m.tester = child.agent(t, "tester", pathChild, claim.RoleTester)
	m.architect = root.agent(t, "root's architect", pathRoot, claim.RoleArchitect)
	m.claims = &newsModelStore{Store: st, refusing: m.reviewer.token}
	r.sup.deps.Store, r.sup.deps.Clock = m.claims, m.clock
	r.client.answer = m.answer
	r.client.before = func(_ string, version int) (int, error) { return version - 1, nil }
	// GitHub already holds each issue's branch, so the row that creates one finishes.
	r.runner.githubAPI = newBranchGitHub(t, r.rt, branchExists).url
	m.reviewer.machine, m.reviewer.conn, _ = r.reviewerBetweenTurns(pathChild)
	m.reviewer.seen = len(m.reviewer.conn.Prompts())
	m.claims.drain()
	for _, row := range m.rows() {
		row.openSeq = r.toldOf(row.issue, row.audience).Opening
		row.told[row.openSeq] = true
		for _, a := range row.agents {
			a.messages = len(r.publishedTo(a.token))
		}
	}
	return m
}

// rows is the two told rows the model follows, and agents every claim that credits one.
func (m *newsModel) rows() []*newsModelRow { return []*newsModelRow{m.child, m.root} }

func (m *newsModel) agents() []*newsModelAgent {
	return []*newsModelAgent{m.reviewer, m.tester, m.architect}
}

// agentOf is the agent whose claim token is, nil for any other claim.
func (m *newsModel) agentOf(token claim.Token) *newsModelAgent {
	for _, a := range m.agents() {
		if a.token == token {
			return a
		}
	}
	return nil
}

// gens is the run row's news is rendered for now: the generation of its issue and of its tree's root.
func (m *newsModel) gens(row *newsModelRow) (uint64, uint64) {
	if row.issue == pathChild {
		return m.childGen, m.rootGen
	}
	return m.rootGen, m.rootGen
}

// answer is Dispatch's change set of pair, by the model's versions: decision-only where every
// version after From up to To changed only decision blocks, a changed section otherwise.
func (m *newsModel) answer(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
	changes := dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}
	if !m.workUnchanged(pair.From, pair.To) {
		changes.DecisionOnly = false
		changes.Sections = []dispatch.SectionChange{{Heading: "Design", Change: dispatch.ChangeChanged}}
	}
	return changes, nil
}

// workUnchanged says whether every version after from up to to changed only decision blocks.
func (m *newsModel) workUnchanged(from, to int) bool {
	for v := from + 1; v <= to; v++ {
		if !m.decisionOnly[v] {
			return false
		}
	}
	return true
}

// fail fails the run, naming its seed and every step it took.
func (m *newsModel) fail(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf("seed %d, step %d: %s\nsteps:\n  %s", m.seed, m.step, fmt.Sprintf(format, args...), strings.Join(m.trace, "\n  "))
}

// drive runs steps random steps, each chosen among the ones the model enables, and holds the told
// rows to the model after each.
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

// state is what a step left, for the trace: of each row, what the record and the model say its
// holder took, and of each claim, what it holds and what its agent holds.
func (m *newsModel) state() string {
	parts := []string{fmt.Sprintf("approved %d", m.latest)}
	for _, row := range m.rows() {
		told := m.r.toldOf(row.issue, row.audience)
		took, known := told.Took(record.DesignStepSpec)
		links, _ := m.standing(row)
		parts = append(parts, fmt.Sprintf("%s took %s owed %t (opening %d); run %d base %d final %v standing %v",
			row.name, versionWords(took, known), told.OpeningOwed, told.Opening, row.run, row.base, row.final, links))
	}
	for _, a := range m.agents() {
		if a.machine == nil {
			continue
		}
		pending := "none"
		if p := a.machine.Claim().Pending; p != nil {
			pending = fmt.Sprintf("%s confirmed=%t carried=%t ran=%t", p.ID, !p.ConfirmedAt.IsZero(), len(p.Carried) > 0, p.Ran)
		}
		parts = append(parts, fmt.Sprintf("%s holds %s (the claim store %+v), its agent next %q turn %q",
			a.name, pending, storedFrom(m.storedDelivery(a.token)), a.next, a.turn))
	}
	return strings.Join(parts, "; ")
}

// versionWords is a version a told row holds as a sentence says it.
func versionWords(took int, known bool) string {
	if !known {
		return "no version"
	}
	return fmt.Sprintf("version %d", took)
}

// newsModelLatest is the newest version a seed approves: past it, no new version is saved.
const newsModelLatest = 40

// enabled is every step the model allows now, in a fixed order, so a seed always picks the same.
// Each step is listed as many times as it is likely, and the likeliest is the one that moves the
// run on from where it stands: the reconcile while the outbox holds a row of it, the reviewer's
// starts while it holds one of those, the agent's turn while it holds a prompt, the turn's end while
// it is in one, the provider error's ladder while it waits, and the shim's hello while the claim
// store lags the machine, which is the decision that repairs it. A row nothing holds is not run, so
// a run spends its steps between turns and inside them rather than on rows that are not there.
//
// A person's new version is listed at any time: three times as often while the reviewer holds a
// prompt that told news or is in its turn (but while nothing says yet what its run stood on), or
// the root architect is in the turn of a prompt that carried its opening, so a version lands behind
// what that prompt told and moves the opening past the one it carried, and three times as often
// while nothing is in flight, so a run that has told everything goes on. The turn of a prompt that
// told news ends as often in a provider error as in its result, but three times as often in its
// result once the gate has moved the row's opening past the one that prompt carried, so the fold of
// a passed opening comes; the turn of a prompt that told nothing mostly ends in its result, so the
// run gets on to the next news. A newer task can take the place of a confirmed task whose turn
// runs.
//
// A seed makes two hand-overs, each listed six times while the reviewer is in no turn and no
// tester's turn runs, so both come early enough. The first is held to the traces of a credit
// counted from an unfinal one: until the reviewer has taken to its result a prompt of the run that
// told it a version while that hand-over's tester credit stands (newsModel.reviewed), no clear and no
// refused write is listed, the reviewer's turn and its result are listed twelve times and nothing
// else ends that turn, and a person's new version once, so the reviewer's credit counted from the
// tester's is made final while the tester's stands; then the tester's provider error is listed six
// times, which gives the tester's credit back. The second is held to a clear under a credit of the
// run it ends: its tester's turn runs to the end of the drive, a re-entry and a re-admission are each
// listed four times while it does and its run stands (handoverClearDue), and a person's new version
// six times once a clear has ended that run, which owes the next run an opening under the number the
// tester's prompt carried for the run that ended.
//
// Otherwise a clear of a generation is listed while the reviewer holds a prompt or is in a turn:
// the claim then holds what the clear must leave counting for nothing, and a re-admission is listed
// three times more where that prompt told the approved version and that version changed the work,
// which the clear must leave counting for nothing though no later version has come. The sweep, which
// queues the reconcile of a tree owed news, is listed twice more while the child's run after a
// re-admission knows no version and no reconcile is queued. A seed makes at most newsModelClears
// re-entries and re-admissions beside the second hand-over's, so the steps left are spent telling
// the run the last one began. The restart waits until the reviewer holds nothing and the root
// architect is in no turn (restart). The root architect's task is listed three times while it has
// news to carry (rootTaskEnabled) and has not been handed one yet, and once after; its turn's result
// is listed once while the opening its prompt carried still stands, so a person's new version can
// come first, eight times once the gate has moved the opening past it, and twice for a prompt that
// carried no opening.
func (m *newsModel) enabled() []string {
	rev, tester := m.reviewer, m.tester
	var ops []string
	add := func(name string, times int) {
		for range times {
			ops = append(ops, name)
		}
	}
	add("sweep", 1)
	add("close-and-reopen", 1)
	add("hello", 1)
	lagging := m.lagging(rev)
	if lagging {
		add("hello", 2)
	}
	reconciles, starts := len(m.r.held(ofKind(record.OutboxKindDesignChanges))) > 0, len(m.r.held(reviewerStarts)) > 0
	if reconciles {
		add("reconcile", 6)
	}
	if starts {
		add("starts", 6)
	}
	if !reconciles && m.child.base == newsModelUnknown {
		add("sweep", 2)
	}
	prompt := rev.turn
	if prompt == "" {
		prompt = rev.next
	}
	p := m.child.prompts[prompt]
	tells := p != nil && p.run == m.child.run && p.tells()
	// window is the first hand-over's: its tester's turn runs, and the reviewer has not yet taken to
	// its result a prompt the hand-over left it. stale is the second hand-over's tester credit once a
	// clear has ended its run: it carries the first opening of that run, which a person's next
	// approval owes the next run under the same number.
	window := tester.turn != "" && m.handovers == 1 && !m.reviewed
	stale := false
	if tp := m.child.prompts[tester.turn]; tester.turn != "" && m.handovers == newsModelHandovers && tp != nil {
		stale = tp.run != m.child.run
	}
	// passed says a prompt whose turn runs carried an opening of its row's run that a later move of
	// the gate has put behind the one the row owes now: its result folds an opening the row no longer
	// holds (design_news_opening_told).
	passed := func(row *newsModelRow, id string) bool {
		q := row.prompts[id]
		return id != "" && q != nil && q.run == row.run && q.opening != newsModelNoOpening && q.opening != row.openSeq && !row.told[row.openSeq]
	}
	architectOpening := false
	if ap := m.root.prompts[m.architect.turn]; m.architect.turn != "" && ap != nil {
		architectOpening = ap.opening != newsModelNoOpening
	}
	if m.latest < newsModelLatest {
		switch {
		case window:
			add("approve-changed", 1)
			add("approve-decision-only", 1)
		case stale:
			add("approve-changed", 6)
			add("approve-decision-only", 6)
		case architectOpening, prompt != "" && tells && m.child.base != newsModelUnknown, prompt == "" && !reconciles && !starts:
			add("approve-changed", 3)
			add("approve-decision-only", 3)
		default:
			add("approve-changed", 1)
			add("approve-decision-only", 1)
		}
	}
	if rev.next != "" && rev.turn == "" {
		add("turn-start", 6)
		if window {
			add("turn-start", 6)
		}
	}
	if rev.turn != "" {
		switch {
		case window:
			add("result", 12)
		case passed(m.child, rev.turn):
			add("result", 9)
			add("provider-error", 3)
		case tells:
			add("result", 3)
			add("provider-error", 3)
		default:
			add("result", 6)
			add("provider-error", 1)
		}
		if pending := rev.machine.Claim().Pending; pending != nil && !pending.ConfirmedAt.IsZero() && !window {
			add("supersede", 1)
		}
	}
	if prompt == "" && m.architect.turn == "" {
		add("restart", 1)
	}
	if m.clock.armedLadder() {
		add("ladder", 4)
	}
	if m.claims.armed() == 0 && !lagging && !window {
		add("fail-writes", 1)
	}
	switch {
	case tester.turn != "" && m.handovers == 1:
	case m.handoverClearDue():
		add("reenter", 4)
		add("readmit", 4)
	case m.clears >= newsModelClears || prompt == "":
	default:
		add("reenter", 1)
		add("readmit", 1)
		if p != nil && p.run == m.child.run && !m.decisionOnly[m.latest] &&
			slices.ContainsFunc(p.pairs, func(l newsModelLink) bool { return l.to == m.latest }) {
			add("readmit", 3)
		}
	}
	if m.handovers < newsModelHandovers && rev.turn == "" && tester.turn == "" && m.latest+2 <= newsModelLatest {
		add("handover", 6)
	}
	if tester.turn != "" && m.handovers == 1 && m.reviewed {
		add("tester-provider-error", 6)
	}
	if m.rootTaskEnabled() {
		add("root-task", 1)
		if m.entry == 0 {
			add("root-task", 2)
		}
	}
	if m.architect.turn != "" {
		switch {
		case passed(m.root, m.architect.turn):
			add("root-task-result", 8)
		case architectOpening:
			add("root-task-result", 1)
		default:
			add("root-task-result", 2)
		}
	}
	return ops
}

// rootTaskEnabled says whether the root architect's task has news to carry and would be sent: its
// row knows a version below the approved one and either owes the opening or the change set of what
// it is owed is recorded (an item whose change set is not recorded is not told), its root hears,
// and its agent holds nothing.
func (m *newsModel) rootTaskEnabled() bool {
	m.t.Helper()
	a := m.architect
	if a.next != "" || a.turn != "" {
		return false
	}
	row := m.r.toldOf(pathRoot, record.AudienceArchitect)
	took, known := row.Took(record.DesignStepSpec)
	if !known || took >= m.latest {
		return false
	}
	if !row.OpeningOwed {
		var recorded bool
		if err := m.r.pool.QueryRow(m.r.ctx, `select exists (select 1 from design_changes where artifact_id = $1 and from_version = $2 and to_version = $3)`,
			pathDoc, took, m.latest).Scan(&recorded); err != nil {
			m.fail("read whether the change set of %d to %d is recorded: %v", took, m.latest, err)
		}
		if !recorded {
			return false
		}
	}
	hears, err := m.r.sup.deps.RootHears(m.r.ctx, supervise.Claim{Token: a.token, Tree: pathRoot, Issue: pathRoot}, supervise.Delivery{})
	if err != nil {
		m.fail("read whether %s hears its architect: %v", pathRoot, err)
	}
	return hears
}

// do runs the step name.
func (m *newsModel) do(name string) {
	m.t.Helper()
	rev := m.reviewer
	switch name {
	case "approve-changed", "approve-decision-only":
		m.approve(name == "approve-decision-only")
	case "close-and-reopen":
		m.r.dispatchEvent(intake.DispatchArtifactChangesRequested, m.latest)
		m.r.dispatchEvent(intake.DispatchArtifactApproved, m.latest)
		m.opened()
	case "reconcile":
		m.reconcile()
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
		m.handle(rev, supervise.StreamTurnStart{Claim: rev.token})
		rev.turn, rev.next = rev.next, ""
	case "result":
		m.result(rev)
	case "provider-error":
		m.providerError(rev)
	case "tester-provider-error":
		m.providerError(m.tester)
	case "ladder":
		m.clock.fireLadder()
		for _, a := range m.agents() {
			if a.machine != nil {
				a.machine.Wait()
			}
		}
	case "supersede":
		// A newer task of the phase, as an operator's or a repair's: it carries no entry, so the
		// claim's given entry stays the one its phases row names, and a news-only start still
		// reaches it. It takes the place of the confirmed task whose turn runs (queue), and gives
		// back that task's credit: the turn's result, when it comes, is another delivery's. A
		// refused write can refuse the newer task whole, which leaves the claim holding the task
		// whose turn runs; check marks the give-back where the claim holds another task.
		m.handle(rev, supervise.RequestDeliver{Claim: rev.token, ID: fmt.Sprintf("model:%d", m.step), Task: "Review the change again.",
			Phase: phase.Reviewing, Generation: readIssue(m.t, m.r.pool, pathChild).Generation})
	case "reenter", "readmit":
		if m.handoverClearDue() {
			m.handoverClear++
		} else {
			m.clears++
		}
		if name == "reenter" {
			m.reenter()
		} else {
			m.readmit()
		}
	case "handover":
		m.handover()
	case "restart":
		m.restart()
	case "hello":
		m.handle(rev, supervise.StreamHello{Claim: rev.token, Generation: rev.machine.Claim().Generation})
	case "fail-writes":
		m.claims.refuse(2)
	case "root-task":
		m.rootTask()
	case "root-task-result":
		m.result(m.architect)
	default:
		m.fail("no step %q", name)
	}
}

// approve is a person saving a new version of the spec and approving it: the gate closes at the
// save and opens at the approval, and the role messages either queued are run (opened).
func (m *newsModel) approve(decisionOnly bool) {
	m.t.Helper()
	m.latest++
	m.decisionOnly[m.latest] = decisionOnly
	m.r.dispatchEvent(intake.DispatchArtifactVersion, m.latest)
	m.r.dispatchEvent(intake.DispatchArtifactApproved, m.latest)
	m.opened()
}

// handoverClearDue says whether the next clear is the one made under the second hand-over's tester
// credit: that tester's turn runs, its prompt is of the run the child stands in, and no clear has
// been made under it yet.
func (m *newsModel) handoverClearDue() bool {
	tester := m.tester
	p := m.child.prompts[tester.turn]
	return tester.turn != "" && m.handovers == newsModelHandovers && m.handoverClear == 0 && p != nil && p.run == m.child.run
}

// handle posts ev to a's machine and waits for what it started. A decision a refused write cut
// short is no failure of the run: the machine owes that write and makes it again.
func (m *newsModel) handle(a *newsModelAgent, ev supervise.Event) {
	m.t.Helper()
	if err := a.machine.Handle(m.r.ctx, ev); err != nil && !errors.Is(err, errModelWrite) {
		m.fail("handle %T for the %s: %v", ev, a.name, err)
	}
	a.machine.Wait()
}

// result is the end of the turn a's agent is in, and that turn's result, which says it ran. The
// credit of a prompt that told news is final once the claim store lands the write that says so,
// which the model waits for (ranAt): a prompt whose claim holds another task by then is another
// delivery's, and makes none, and a prompt that told nothing has no credit to make final.
func (m *newsModel) result(a *newsModelAgent) {
	m.t.Helper()
	spent := m.claims.spentCount()
	if p := a.machine.Claim().Pending; p == nil || p.ID != a.turn {
		a.superseded[a.turn] = true
	}
	m.handle(a, supervise.StreamTurnEnd{Claim: a.token})
	m.handle(a, supervise.StreamPromptResult{Claim: a.token, DeliveryID: a.turn, AgentInvoked: true})
	a.took = append(a.took, a.turn)
	if p := a.row.prompts[a.turn]; p != nil && !a.superseded[a.turn] && p.tells() {
		a.ranAt[a.turn] = spent
	}
	if p := a.row.prompts[a.turn]; a == m.reviewer && m.handovers == 1 && m.tester.turn != "" && p != nil && p.run == a.row.run &&
		len(p.pairs) > 0 && !a.superseded[a.turn] {
		m.reviewed = true
	}
	a.turn = ""
}

// providerError is the end of the turn a's agent is in in a provider error, which takes the task
// back: the agent did not take it, and the provider error's ladder sends it again.
func (m *newsModel) providerError(a *newsModelAgent) {
	m.t.Helper()
	m.handle(a, supervise.StreamTurnEnd{Claim: a.token})
	m.handle(a, supervise.StreamPromptResult{Claim: a.token, DeliveryID: a.turn, AgentInvoked: true,
		Error: "overloaded_error", HTTPStatus: 529})
	a.errored[a.turn] = true
	a.turn = ""
}

// newsModelClears is how many re-entries and re-admissions one seed makes at most beside the one
// made under the second hand-over's tester credit (handoverClearDue), and newsModelHandovers how
// many hand-overs (newsModel.enabled).
const (
	newsModelClears    = 3
	newsModelHandovers = 2
)

// reviewerStarts matches every start of the child's reviewer: its phase's own, which a run that
// began again raises anew and which waits for the row that creates the issue's branch, and one
// queued for news alone.
func reviewerStarts(row pathRow) bool {
	request, ok := row.payload.(record.SuperviseRequest)
	return ok && row.issue == pathChild && request.Op == "start" && request.Role == claim.RoleReviewer
}

// reconcile runs the reconcile rows the outbox holds, and, where one ran, the model's side of
// what it settles (settled).
func (m *newsModel) reconcile() {
	m.t.Helper()
	m.r.later(time.Hour)
	if len(m.r.run(ofKind(record.OutboxKindDesignChanges))) == 0 {
		return
	}
	m.drain()
	for _, row := range m.rows() {
		m.settled(row)
	}
}

// settled is the model's side of what the reconcile settles for row: a row that knows no version
// is filled from the review history, which names the version below the approved one
// (newNewsModel), and a row owed only versions that changed decision blocks alone, and no opening,
// is raised past them, by a credit counted from the version it took (record.Store.RaiseTold), so a
// raise read off a version an unfinal credit gave reaches nothing once that credit is given back.
func (m *newsModel) settled(row *newsModelRow) {
	m.t.Helper()
	if row.base == newsModelUnknown {
		row.base = m.latest - 1
	}
	links, opens := m.standing(row)
	took, known := newsModelClosure(row.base, append(slices.Clone(row.final), links...))
	if !known || took >= m.latest || !m.workUnchanged(took, m.latest) || m.owesOpening(row, opens) {
		return
	}
	row.final = append(row.final, newsModelLink{from: took, to: m.latest})
}

// clear is the model's side of a clear of a generation under row, whose rule is that it empties
// what the issue's holders took: every prompt sent so far is of the run that ended, the next run's
// holder starts from base, and no opening and no credit of the run that ended is one of its own.
func (m *newsModel) clear(row *newsModelRow, base int) {
	row.run++
	row.base = base
	row.final = nil
	row.cleared = map[int]bool{}
	row.told = map[int]bool{}
	for _, a := range row.agents {
		a.ranAt = map[string]int{}
	}
}

// reenterChild is a person setting the child back to todo while its tree runs: its run starts
// again as its next generation (workflow's ReenterChild), and the claims of its roles are kept with
// whatever they hold. The gate stands open, so the child's first phase starts at once and takes the
// approved version as its baseline. Dispatch shows the child's own status before the todo, as the
// daemon's status write makes it: a todo that only repeats what Dispatch last showed moves nothing.
// The run's phases are then completed by their roles until the child stands in until.
func (m *newsModel) reenterChild(until phase.Phase) {
	m.t.Helper()
	// A prompt the model has not read yet was sent before the clear.
	m.observe()
	if child := readIssue(m.t, m.r.pool, pathChild); child.DispatchStatus != child.Status {
		m.r.statusEvent(pathChild, child.Status)
	}
	m.r.reenter(pathChild)
	m.childGen++
	m.clear(m.child, m.latest)
	for range 3 {
		child := readIssue(m.t, m.r.pool, pathChild)
		if child.Phase == phase.Admitted {
			m.fail("%s stands at admitted after its re-entry under an open gate, want its first phase started", pathChild)
		}
		if child.Phase == until {
			break
		}
		m.r.complete(pathChild)
	}
	if child := readIssue(m.t, m.r.pool, pathChild); child.Phase != until || child.Generation != m.childGen {
		m.fail("%s stands in %s at generation %d after its next run's phases were completed, want %s at generation %d",
			pathChild, child.Phase, child.Generation, until, m.childGen)
	}
}

// reenter is a re-entry of the child (reenterChild) whose next run is brought back to reviewing,
// and the outbox runs the reviewer's start of the phase where the claim can take it. The run began
// under an open gate: no opening is owed to it.
func (m *newsModel) reenter() {
	m.t.Helper()
	m.reenterChild(phase.Reviewing)
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	m.r.run(reviewerStarts)
	m.reviewer.machine.Wait()
	m.child.openSeq = m.r.toldOf(pathChild, record.AudienceHolder).Opening
	m.child.told[m.child.openSeq] = true
}

// handover is a re-entry of the child (reenterChild) whose next run stops in testing, where a
// person approves a version that changes the work, the reconcile records what changed, and the
// tester's phase start hands its tester the phase task, which tells that version. The tester starts
// the turn of that task, which confirms it, and completes its phase in that turn, which moves the
// child into reviewing; its turn goes on, so its credit stands and is not final. A person then
// approves another version, the reconcile records what changed, and the reviewer's phase start tells
// the reviewer that version from the one the tester's credit gave: a credit counted from a version
// an unfinal credit gave, which reaches nothing if that credit is given back, and which waits for it
// to be final if it is folded first (the SQL functions design_news_known and design_news_folded).
func (m *newsModel) handover() {
	m.t.Helper()
	m.handovers++
	m.reenterChild(phase.Testing)
	m.child.openSeq = m.r.toldOf(pathChild, record.AudienceHolder).Opening
	m.child.told[m.child.openSeq] = true
	m.approve(false)
	m.reconcile()
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	tester := m.tester
	tester.next = ""
	if ran := m.r.run(startOf(pathChild, claim.RoleTester, false)); len(ran) == 0 {
		m.fail("no start of %s's tester ran after its run reached testing", pathChild)
	}
	if tester.machine == nil {
		tester.machine, tester.conn = m.r.connect(pathChild, claim.RoleTester)
		tester.seen = 0
	}
	tester.machine.Wait()
	m.observeAgent(tester)
	if tester.next == "" && m.clock.armedLadder() {
		// The first hand-over's tester turn ended in a provider error, whose ladder the new task
		// waits out.
		m.do("ladder")
		m.observeAgent(tester)
	}
	if p := m.child.prompts[tester.next]; p == nil || len(p.pairs) == 0 {
		m.fail("the tester was sent %q, want its phase task telling version %d: the hand-over's own premise", tester.next, m.latest)
	}
	m.handle(tester, supervise.StreamTurnStart{Claim: tester.token})
	tester.turn, tester.next = tester.next, ""
	m.r.complete(pathChild)
	if child := readIssue(m.t, m.r.pool, pathChild); child.Phase != phase.Reviewing {
		m.fail("%s stands in %s after its tester completed, want reviewing", pathChild, child.Phase)
	}
	m.approve(false)
	m.reconcile()
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	m.r.run(reviewerStarts)
	m.reviewer.machine.Wait()
	m.reviewed = false
}

// readmit is a person closing the root and setting it back to todo inside the linger, before any
// suspension the linger queued has run: a new generation of the whole tree (admit's readmit), which
// clears what every issue of it took and its gate, and raises the root's generation alone. The
// child keeps its generation and its phase, and its claims whatever they hold. The tree's
// architect then registers the spec again at the version a person approved. A tree whose gate rows
// went with the clear stands released until a row closes it (workflow's standingGateOpen), so that
// registration opens nothing and owes the run no opening. The registration writes the architect's
// row at the version it registers, which the architect took at that opening (workflow's
// putGateTold). Nothing says yet what the child's holder stood on: its row knows no version until
// the reconcile reads the review history.
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
	m.rootGen++
	m.clear(m.child, newsModelUnknown)
	m.clear(m.root, m.latest)
	m.r.marks++
	applyFact(m.t, m.r.pool, "api", fmt.Sprintf("register-%s-%d", pathDoc, m.r.marks), intake.GateRegistered{Issue: pathRoot,
		ArtifactID: pathDoc, Version: m.latest, ApprovedVersion: m.latest, ApprovedMark: record.EventMark{Seq: m.r.marks},
		ReviewedBy: dispatch.Actor{Kind: "user", ID: "alice"}}, m.r.engine, m.r.admission)
	m.opened()
	for _, row := range m.rows() {
		row.told[row.openSeq] = true
	}
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	m.r.run(reviewerStarts)
	m.reviewer.machine.Wait()
}

// rootTask is the tree root's architect handed the task of a new entry, which names two decisions
// a person made on the tree while nobody in it could hear them and carries the news the architect's
// row is owed, as the workflow's start of an entry carries them (record.SuperviseRequest.Kept, the
// start executor's newsForStart). The task waits behind a turn of the architect's own (or, the first
// time, for the architect's process to come up), and meanwhile a newer decision on the same
// reference replaces one of the two, which leaves the task no longer holding it. So the task's send
// narrows it to the decision that stays, and renders its news again under the narrowed text
// (supervise's currentRootTask, the daemon's rootTask and narrowedNews), which is the one prompt the
// architect is sent. The architect starts the turn of that prompt, which confirms it; the turn's
// result is a step of its own (root-task-result), so a person can approve a version while it works,
// which moves the architect's opening past the one its prompt carried.
func (m *newsModel) rootTask() {
	m.t.Helper()
	a := m.architect
	// The entry is a new one: past what this model handed it, and past any entry the workflow
	// raised on the architect's phases row meanwhile (a re-admitted root's promotion).
	var raised int64
	if err := m.r.pool.QueryRow(m.r.ctx, `select coalesce(max(entry), 0) from phases where issue = $1 and role = 'architect'`,
		pathRoot).Scan(&raised); err != nil {
		m.fail("read the root architect's phases row: %v", err)
	}
	m.entry = max(m.entry, raised) + 1
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC).Add(time.Duration(m.entry) * time.Hour)
	stays := record.DecisionMark{Issue: pathRoot, Ref: fmt.Sprintf("dispatch://%s/ask/entry-%d-stays", pathRoot, m.entry),
		Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at}
	replaced := stays
	replaced.Ref, replaced.At = fmt.Sprintf("dispatch://%s/ask/entry-%d-replaced", pathRoot, m.entry), at.Add(time.Minute)
	if _, err := m.r.pool.Exec(m.r.ctx, `insert into parked_decisions (issue, ref, decision, actor, at) values ($1, $2, $3, $4, $5), ($1, $6, $3, $4, $7)`,
		pathRoot, stays.Ref, string(stays.Decision), stays.Actor, stays.At, replaced.Ref, replaced.At); err != nil {
		m.fail("keep the two decisions of the root architect's entry %d: %v", m.entry, err)
	}
	task := fmt.Sprintf("A person decided on the tree of %s while nobody in it could hear them: %s and %s. Read each before you continue.",
		pathRoot, stays.Ref, replaced.Ref)
	m.r.architectRow(m.entry, task)
	if a.machine != nil {
		m.handle(a, supervise.StreamTurnStart{Claim: a.token})
	}
	request := record.SuperviseRequest{Op: "start", Tree: pathRoot, Role: claim.RoleArchitect, Generation: m.rootGen, Entry: m.entry, Task: task,
		Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{stays, replaced}}}
	mark := m.r.lastRowID()
	m.r.inTx(func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow(pathRoot, request, time.Now().UTC())
		if err != nil {
			return err
		}
		return m.r.records.Enqueue(m.r.ctx, tx, row)
	})
	if ran := m.r.run(func(row pathRow) bool {
		start, ok := row.payload.(record.SuperviseRequest)
		return ok && row.id > mark && row.issue == pathRoot && start.Op == "start" && start.Role == claim.RoleArchitect && start.Entry == m.entry
	}); len(ran) != 1 {
		m.fail("the root architect's starts of entry %d run = %d, want the one queued", m.entry, len(ran))
	}
	if _, err := m.r.pool.Exec(m.r.ctx, `update parked_decisions set at = $3, held_by_claim = null, held_by_delivery = null
		where issue = $1 and ref = $2`, pathRoot, replaced.Ref, replaced.At.Add(time.Hour)); err != nil {
		m.fail("replace the decision %s: %v", replaced.Ref, err)
	}
	if a.machine == nil {
		a.machine, a.conn = m.r.connect(pathRoot, claim.RoleArchitect)
		a.seen = 0
		a.machine.Wait()
	} else {
		m.handle(a, supervise.StreamTurnEnd{Claim: a.token})
	}
	m.observeAgent(a)
	if a.next == "" {
		m.fail("the root's architect was sent no task of entry %d: %+v", m.entry, a.machine.Claim().Pending)
	}
	if text := m.root.prompts[a.next].text; !strings.Contains(text, stays.Ref) || strings.Contains(text, replaced.Ref) {
		m.fail("the root's architect's task of entry %d is %q, want it naming %s alone: the narrowing, the step's own premise", m.entry, text, stays.Ref)
	}
	m.handle(a, supervise.StreamTurnStart{Claim: a.token})
	a.turn, a.next = a.next, ""
}

// opened runs the role messages the gate's move queued and reads, for each row, the opening that
// stands now: its told row's opening number, and whether an opened message to the topic of a claim
// that credits it told it.
func (m *newsModel) opened() {
	m.t.Helper()
	m.r.run(ofKind(record.OutboxKindRoleMessage))
	for _, row := range m.rows() {
		row.openSeq = m.r.toldOf(row.issue, row.audience).Opening
		for _, a := range row.agents {
			messages := m.r.publishedTo(a.token)
			for _, message := range messages[a.messages:] {
				if strings.HasPrefix(message, "The design gate of "+pathRoot+" opened") {
					row.told[row.openSeq] = true
				}
			}
			a.messages = len(messages)
		}
	}
}

// restart is the daemon stopped and booted again over the claims the store holds, each claim's shim
// connecting to it and saying hello. The reviewer's agent holds no prompt and the root architect's
// is in no turn: the restart's question never takes a turn for a root architect's held task (its
// send resolves it instead), so that turn would end as another delivery's. A tester in its turn is
// still in it, and its shim says it is streaming, so the booted machine takes that turn for its
// task's (the restart's question, askFirst).
func (m *newsModel) restart() {
	m.t.Helper()
	// A store that lags the machine being stopped holds a delivery that machine had moved past in
	// memory alone: the booted machine reads the stored one, and the record stays where the last
	// landed write left it until a write of that delivery lands (lagging).
	for _, a := range m.agents() {
		if m.lagging(a) {
			at := storedFrom(m.storedDelivery(a.token))
			a.behind = &at
		}
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
	for _, a := range m.agents() {
		if a.machine == nil {
			continue
		}
		a.machine, a.conn = m.r.reconnect(a.issue, a.role, a.turn != "")
		a.seen, a.next = 0, ""
		if err := a.machine.Handle(m.r.ctx, supervise.StreamHello{Claim: a.token, Generation: a.machine.Claim().Generation}); err != nil &&
			!errors.Is(err, errModelWrite) {
			m.fail("hello of the %s: %v", a.name, err)
		}
		a.machine.Wait()
	}
}

// observe reads each prompt each claim was given since the model last looked (observeAgent).
func (m *newsModel) observe() {
	m.t.Helper()
	for _, a := range m.agents() {
		m.observeAgent(a)
	}
}

// observeAgent reads each prompt a was given since the model last looked: what its text names, the
// run it was sent in, and the opening it said opened, which is the one standing at its send. The
// news the delivery records for the prompt it sent is held to that text (invariant 1).
func (m *newsModel) observeAgent(a *newsModelAgent) {
	m.t.Helper()
	if a.conn == nil {
		return
	}
	row := a.row
	prompts := a.conn.Prompts()
	gen, treeGen := m.gens(row)
	for _, prompt := range prompts[a.seen:] {
		told := &newsModelPrompt{text: prompt.Message, run: row.run, gen: gen, treeGen: treeGen, opening: newsModelNoOpening}
		var opened bool
		told.pairs, opened = row.named(prompt.Message)
		if opened {
			told.opening = row.openSeq
		}
		row.prompts[prompt.DeliveryID] = told
		a.next = prompt.DeliveryID
		if told.tells() {
			m.ran["prompts that told news"]++
		}
		if p := a.machine.Claim().Pending; p != nil && p.ID == prompt.DeliveryID {
			if why := m.namesExactly(row, told, p.News); why != "" {
				m.fail("invariant 1: the news the %s's delivery %s records is not the news the prompt it was sent carried: %s", a.name, p.ID, why)
			}
		}
	}
	a.seen = len(prompts)
}

// namesExactly is "" where encoded, the news a delivery records for the prompt p, is the news p
// carried: the start's own text it names is in the prompt, it carries exactly the version pairs
// the prompt's text names and the opening it said opened, each to row, and each marked with the run
// the prompt was sent in. Otherwise it says what differs.
func (m *newsModel) namesExactly(row *newsModelRow, p *newsModelPrompt, encoded []byte) string {
	var news record.DeliveryNews
	if len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &news); err != nil {
			return fmt.Sprintf("its news %s does not decode: %v", encoded, err)
		}
	}
	if !strings.Contains(p.text, news.Task) {
		return fmt.Sprintf("it says the prompt opened with the start's own text %q, which the prompt %q does not carry", news.Task, p.text)
	}
	var pairs []newsModelLink
	for _, item := range news.Items {
		if item.Issue != row.issue || item.Audience != row.audience || item.Step != record.DesignStepSpec {
			return fmt.Sprintf("it carries an item of the %s of %s's %s row, which is not the row of the %s", item.Step, item.Issue, item.Audience, row.name)
		}
		if item.Generation != p.gen || item.TreeGeneration != p.treeGen {
			return fmt.Sprintf("it marks its item with generation %d of %s in generation %d of its tree, but the prompt was sent in %d of %d",
				item.Generation, item.Issue, item.TreeGeneration, p.gen, p.treeGen)
		}
		pairs = append(pairs, newsModelLinkOf(item))
	}
	opening := newsModelNoOpening
	for _, carried := range news.Openings {
		if carried.Issue != row.issue || carried.Audience != row.audience || carried.Closed {
			continue
		}
		if carried.Generation != p.gen || carried.TreeGeneration != p.treeGen {
			return fmt.Sprintf("it marks its opening with generation %d in generation %d of its tree, but the prompt was sent in %d of %d",
				carried.Generation, carried.TreeGeneration, p.gen, p.treeGen)
		}
		opening = carried.Seq
	}
	if !newsModelSameLinks(pairs, p.pairs) {
		return fmt.Sprintf("it carries the version pairs %v, but the prompt's text names %v", pairs, p.pairs)
	}
	if opening != p.opening {
		return fmt.Sprintf("it carries the opening %d, but the prompt told the opening %d (%d is none)", opening, p.opening, newsModelNoOpening)
	}
	return ""
}

// newsModelLinkOf is item as the credit it makes (newsModelLink).
func newsModelLinkOf(item record.DeliveryNewsItem) newsModelLink {
	if item.From == nil {
		return newsModelLink{from: newsModelAny, to: item.To}
	}
	return newsModelLink{from: *item.From, to: item.To}
}

// newsModelSameLinks says whether a and b hold the same links, each as many times.
func newsModelSameLinks(a, b []newsModelLink) bool {
	order := func(x, y newsModelLink) int {
		if x.from != y.from {
			return x.from - y.from
		}
		return x.to - y.to
	}
	return slices.Equal(slices.SortedFunc(slices.Values(a), order), slices.SortedFunc(slices.Values(b), order))
}

// credits is what the copy carried credits row with, by the model's rule: each item to row as a
// link, and each opening it says opened, by its number, counting only what was rendered for the run
// the issue stands in now (design_news_current).
func (m *newsModel) credits(row *newsModelRow, carried []byte) ([]newsModelLink, map[int]bool) {
	m.t.Helper()
	var news record.DeliveryNews
	if err := json.Unmarshal(carried, &news); err != nil {
		m.fail("decode the copy %s a delivery of the %s holds: %v", carried, row.name, err)
	}
	gen, treeGen := m.gens(row)
	var links []newsModelLink
	for _, item := range news.Items {
		if item.Issue == row.issue && item.Audience == row.audience && item.Step == record.DesignStepSpec &&
			item.Generation == gen && item.TreeGeneration == treeGen {
			links = append(links, newsModelLinkOf(item))
		}
	}
	opens := map[int]bool{}
	for _, opening := range news.Openings {
		if opening.Issue == row.issue && opening.Audience == row.audience && !opening.Closed &&
			opening.Generation == gen && opening.TreeGeneration == treeGen {
			opens[opening.Seq] = true
		}
	}
	return links, opens
}

// standing is the credit row's claims hold in the claim store now: the copy each confirmed delivery
// whose turn's result has not said it ran holds (credits).
func (m *newsModel) standing(row *newsModelRow) ([]newsModelLink, map[int]bool) {
	m.t.Helper()
	var links []newsModelLink
	opens := map[int]bool{}
	for _, a := range row.agents {
		if a.machine == nil {
			continue
		}
		d := m.storedDelivery(a.token)
		if d == nil || d.ConfirmedAt.IsZero() || d.Ran || len(d.Carried) == 0 {
			continue
		}
		credited, opened := m.credits(row, d.Carried)
		links = append(links, credited...)
		maps.Copy(opens, opened)
	}
	return links, opens
}

// owesOpening says whether row owes the opening that stands, by the model's rule: no role message
// told it, no final credit's fold settled it, and no standing credit (opens) carried it.
func (m *newsModel) owesOpening(row *newsModelRow, opens map[int]bool) bool {
	return !row.told[row.openSeq] && !row.cleared[row.openSeq] && !opens[row.openSeq]
}

// drain reads every write of a delivery the claim store took since the last drain, in order. A
// write whose copy is new is the acknowledgement of a prompt, and the copy is held to that prompt's
// text (invariant 1). A write of a confirmed delivery whose turn's result said it ran folds its copy
// into the row its claim credits (the claim store's settleDesignNews): each credit of the run the
// issue stands in is final, and an opening it says opened is settled where the row still holds that
// number (design_news_opening_told).
func (m *newsModel) drain() {
	m.t.Helper()
	for _, w := range m.claims.drain() {
		a := m.agentOf(w.token)
		if a == nil {
			continue
		}
		if w.delivery == nil {
			a.carried = nil
			continue
		}
		d, row := *w.delivery, a.row
		if len(d.Carried) > 0 && !bytes.Equal(d.Carried, a.carried) {
			if p := row.prompts[d.ID]; p != nil {
				if why := m.namesExactly(row, p, d.Carried); why != "" {
					m.fail("invariant 1: the copy the claim store holds of the %s's prompt %s, which its confirmation credits, is not the news that prompt carried: %s",
						a.name, d.ID, why)
				}
			}
		}
		a.carried = d.Carried
		if d.ConfirmedAt.IsZero() || !d.Ran || len(d.Carried) == 0 {
			continue
		}
		a.finalized[d.ID] = true
		links, opens := m.credits(row, d.Carried)
		row.final = append(row.final, links...)
		if opens[row.openSeq] {
			row.cleared[row.openSeq] = true
		}
	}
}

// truth is the version of the spec the model says row's holder took of prompts in the run its
// issue stands in: from the version that run started from (base), through every pair they told,
// and, where closed says so, through every version since that changed only decision blocks, which a
// holder knows without being told. It is what the record is held never to pass with the prompts
// the holders were told (closed), and to reach with the prompts whose credit stands (not closed).
func (m *newsModel) truth(row *newsModelRow, prompts []*newsModelPrompt, closed bool) int {
	var links []newsModelLink
	for _, p := range prompts {
		links = append(links, p.pairs...)
	}
	took := row.base
	for moved := true; moved; {
		moved = false
		if reached, _ := newsModelClosure(took, links); reached != took {
			took, moved = reached, true
		}
		for closed && took < m.latest && m.decisionOnly[took+1] {
			took, moved = took+1, true
		}
	}
	return took
}

// owes says whether row's holders have not been told the opening that stands by the role messages
// or by any of prompts.
func (m *newsModel) owes(row *newsModelRow, prompts []*newsModelPrompt) bool {
	if row.told[row.openSeq] {
		return false
	}
	for _, p := range prompts {
		if p.opening == row.openSeq {
			return false
		}
	}
	return true
}

// lagging says whether the claim store holds a's delivery otherwise than its machine does: a
// refused write the machine still owes. A restart does not end that. The booted machine reads the
// stored delivery, so the two agree, yet what the stopped machine had decided of it in memory — a
// provider error's take-back, say — was never written, and the booted machine's own first write of
// it can be refused too. So a store that lagged at a restart lags until the stored delivery is
// another than it was then (behind).
func (m *newsModel) lagging(a *newsModelAgent) bool {
	m.t.Helper()
	if a.machine == nil {
		return false
	}
	stored := storedFrom(m.storedDelivery(a.token))
	if a.behind != nil {
		if stored == *a.behind {
			return true
		}
		a.behind = nil
	}
	return stored != storedFrom(a.machine.Claim().Pending)
}

// check reads every write the claim store took since the last check and holds each told row, as
// every reader reads it, to the model (checkRow), counting the step ambiguous where a claim store
// lags its machine. A prompt whose turn runs while its claim holds another task, or none, was given
// back: a newer task took its place (the supersede step's, or a start's of a newer entry), which
// ends its credit, and its turn's result, when it comes, is another delivery's.
func (m *newsModel) check() {
	m.t.Helper()
	m.drain()
	for _, a := range m.agents() {
		if a.turn != "" && a.machine != nil {
			if p := a.machine.Claim().Pending; p == nil || p.ID != a.turn {
				a.superseded[a.turn] = true
			}
		}
	}
	ambiguous := false
	for _, row := range m.rows() {
		if m.checkRow(row) {
			ambiguous = true
		}
	}
	m.ambiguity.step(ambiguous)
	m.ran["checks"]++
	if ambiguous {
		m.ran["checks with a refused write owed"]++
	}
}

// checkRow holds row's told row to the model and answers whether the claim store lags the machine
// of a claim that credits it.
//
// After a re-admission of the tree nothing says what the child's holder stood on, and the row knows
// no version, until the reconcile reads the review history: the version approved before the one
// that stands, which is the one below it (newNewsModel). The model takes that as the run's base at
// the reconcile that reads it (settled), and holds the row to it from that step on.
func (m *newsModel) checkRow(row *newsModelRow) bool {
	m.t.Helper()
	told := m.r.toldOf(row.issue, row.audience)
	took, known := told.Took(record.DesignStepSpec)
	lagging := false
	for _, a := range row.agents {
		lagging = m.lagging(a) || lagging
	}
	m.settleFinals(row, lagging)
	links, opens := m.standing(row)
	credited := append(slices.Clone(row.final), links...)
	if row.base != newsModelUnknown && !known {
		m.fail("exactly: the %s's row knows no version of the spec, but its run started from version %d: %+v", row.name, row.base, told)
	}
	if upper := m.truth(row, row.toldPrompts(lagging), true); known && row.base != newsModelUnknown && took > upper {
		m.fail("never ahead of what it was told: the record says the %s took version %d, but it was told no further than %d", row.name, took, upper)
	}
	if want, wantKnown := newsModelClosure(row.base, credited); known != wantKnown || (known && took != want) {
		m.fail("exactly: the record says the %s took %s, but what the claim store credits gives %s: from %s through the credits %v (final %v, standing %v)",
			row.name, versionWords(took, known), versionWords(want, wantKnown), versionWords(row.base, row.base != newsModelUnknown), credited, row.final, links)
	}
	if !lagging && row.base != newsModelUnknown {
		if lower := m.truth(row, row.creditedPrompts(), false); took < lower {
			m.fail("level with what it read: the record says the %s took version %d, but it read the news up to %d", row.name, took, lower)
		}
	}
	if !told.OpeningOwed && m.owes(row, row.toldPrompts(lagging)) {
		m.fail("never ahead of what it was told: the record owes the %s no opening, but no message or read prompt told it opening %d", row.name, row.openSeq)
	}
	if want := m.owesOpening(row, opens); told.OpeningOwed != want {
		m.fail("exactly: the record owes the %s the opening %d: %t, but the role messages and what the claim store credits say %t (told by a message %t, settled by a final credit %t, carried by a standing credit %t)",
			row.name, row.openSeq, told.OpeningOwed, want, row.told[row.openSeq], row.cleared[row.openSeq], opens[row.openSeq])
	}
	if !lagging {
		if owed := m.owes(row, row.creditedPrompts()); told.OpeningOwed != owed {
			m.fail("level with what it read: the record owes the %s the opening %d: %t, but by the messages and the prompts whose credit stands it is %t",
				row.name, row.openSeq, told.OpeningOwed, owed)
		}
	}
	return lagging
}

// settleFinals settles each prompt of row's claims whose turn's result came and whose final credit
// the claim store has not landed (ranAt): once the store has landed it, or the prompt is of a run
// that ended, there is nothing to wait for; while the store lags a machine, the write that makes it
// final may still come; and once the store has caught up without it, a refused write since that
// result lost it (lost), or, where no write was refused since, the turn ran and the store never made
// its credit final, which fails the run.
func (m *newsModel) settleFinals(row *newsModelRow, lagging bool) {
	m.t.Helper()
	for _, a := range row.agents {
		for _, id := range slices.Sorted(maps.Keys(a.ranAt)) {
			spent := a.ranAt[id]
			switch p := row.prompts[id]; {
			case a.finalized[id] || p == nil || p.run != row.run:
				delete(a.ranAt, id)
			case lagging:
			case m.claims.spentCount() > spent:
				a.lost[id] = true
				delete(a.ranAt, id)
			default:
				m.fail("level with what it read: the %s's turn on %s ran and its result came, but the claim store never made its credit final, and no write was refused since",
					a.name, id)
			}
		}
	}
}

// quiesce ends the run: no write is refused, every row runs, and each agent of the child takes
// every prompt it is given to its result, until nothing is left to send. The reviewer has then taken
// the approved version and owes no opening, and the model says it was told that much.
func (m *newsModel) quiesce() {
	m.t.Helper()
	rev := m.reviewer
	m.claims.refuse(0)
	m.trace = append(m.trace, "quiesce")
	for _, a := range []*newsModelAgent{m.tester, m.architect} {
		if a.turn != "" {
			m.result(a)
		}
	}
	for range 40 {
		if rev.turn != "" {
			m.do("result")
		} else if rev.next != "" {
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
		told := m.r.toldOf(pathChild, record.AudienceHolder)
		took, _ := told.Took(record.DesignStepSpec)
		if rev.turn == "" && rev.next == "" && rev.machine.Claim().Pending == nil && took == m.latest && !told.OpeningOwed {
			if upper := m.truth(m.child, m.child.toldPrompts(false), true); upper != m.latest {
				m.fail("at quiescence the record says version %d and the model %d", took, upper)
			}
			return
		}
	}
	told := m.r.toldOf(pathChild, record.AudienceHolder)
	var owed record.TreeNews
	m.r.inTx(func(tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(m.r.ctx, m.r.records, tx, config.DesignGateRootIssues.Steps(), pathRoot)
		owed = record.OwedNews(facts)
		return err
	})
	held := rev.machine.Claim()
	m.fail("the run did not quiesce: the child's holder row is %+v, the approved version %d, the reviewer's claim holds %+v (state %s, given entry %d), its agent holds %q in a turn and %q not started; owed %+v",
		told, m.latest, held.Pending, held.State, held.GivenEntry, rev.turn, rev.next, owed)
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
