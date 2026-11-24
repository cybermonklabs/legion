package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
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
// same delivery finishes it or undoes it (lagging). The store refuses only the reviewer's writes, so
// a store behind any other claim's machine lost a write it answered as made, and that fails the run,
// as does a store that falls behind the reviewer's with no write of it refused since the last check.
// The model reads the credit the store holds, so the exact rule and the never-ahead rule hold
// through a refused write; but whether the agents' own view is level with the record has two
// answers until the store has caught up, so each step after which a claim's store lags is counted as
// ambiguous, with how many steps each such stretch lasts until the step that settles it
// (newsModelAmbiguity), and the level rule is held again from that step on. Each seed logs its
// counts, and the run logs their totals.
//
// One credit the drive never makes final is a tester's: the first hand-over's tester turn ends in a
// provider error, and the second's runs until a clear has ended its run, so no tester's turn ends
// with its result while its run stands, and a tester's credit that the result never made final is
// one the model cannot see. TestATestersTurnThatRanMakesItsCreditFinalAndItIsNotToldAgain
// (design_news_credit_test.go) pins it.
//
// With no variable set it runs newsModelCISeed and newsModelReplacementSeed for 40 steps each, a few
// seconds: a seed whose run reaches eighteen of the twenty kinds of step and that each of the
// record's rules, put back wrong one at a time, fails (newsModelCISeed says how that was read), and a
// seed that fails where a running turn's task is replaced by a step that cannot replace it
// (newsModelReplacementSeed). LEGION_NEWS_MODEL_SEED runs one seed,
// LEGION_NEWS_MODEL_RUNS runs seeds 1 to that many (1 runs seed 1 alone), LEGION_NEWS_MODEL_STEPS
// sets the steps each drives, and LEGION_NEWS_MODEL_TRACE prints each seed's steps and the state each
// left. A seed's steps are its own: every step waits for the machines before the next (wait), and
// the machines' timers fire only when a step fires them.
func TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold(t *testing.T) {
	t.Parallel()
	steps := keptPropertyEnv(t, "LEGION_NEWS_MODEL_STEPS", 40)
	seeds := []int64{newsModelCISeed, newsModelReplacementSeed}
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
// the run fails under each of the seven rules the record keeps put back wrong one at a time (a credit
// counted in any run, a credit reaching from any version, a fold that keeps no unreached credit, a
// read that counts no standing credit, a fold before the turn's result, a carried opening that
// clears any opening, and a narrowed root task that keeps the news it held). Read from runs of seeds
// 1 to 100 at 40 steps: unmutated with -v, which prints the kinds each seed's drive chose
// (newsModel.kinds), and under each of the seven. 91 of those seeds failed under all seven, no seed
// reached all twenty kinds, and 35 is one of the five that fail under all seven and reach eighteen.
const newsModelCISeed = 35

// newsModelReplacementSeed is the second seed the model test runs where no variable names one: the
// seed whose run fails when supervise's refusal to let a task of news alone take a running turn's
// place (Machine.queue) is deleted, which newsModelCISeed's run does not reach. Read from runs of
// seeds 1 to 40 at 40 steps with that refusal deleted: seed 5 alone failed, at its step 7, and every
// seed of 1 to 100 passes with it in place.
const newsModelReplacementSeed = 5

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
// that stood when it was sent (newsModelNoOpening for none). entry is the entry of the task it
// carried, as its claim held that task when the model read the prompt, 0 for a task of no entry.
type newsModelPrompt struct {
	text         string
	run          int
	gen, treeGen uint64
	pairs        []newsModelLink
	opening      int
	entry        int64
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
	// claim's writes are the ones refused (newsModelStore.refusing), its result came, a write was
	// refused since, and the store caught up without making it final.
	ranAt     map[string]int
	finalized map[string]bool
	lost      map[string]bool
	// carried is the copy the last write of the claim's delivery the store took held, and behind the
	// delivery as the store held it at a restart that found the store lagging the stopped machine,
	// nil otherwise (lagging). lagged says the store lagged the claim's machine at the last check.
	carried []byte
	behind  *newsModelStored
	lagged  bool
}

// newNewsModelRow is issue's audience row starting from version 4 of the spec, its opening told.
func newNewsModelRow(name, issue string, audience record.DesignAudience) *newsModelRow {
	return &newsModelRow{name: name, issue: issue, audience: audience, base: 4, cleared: map[int]bool{}, told: map[int]bool{},
		prompts: map[string]*newsModelPrompt{}}
}

// agent adds role's claim on r's issue to the claims crediting r.
func (r *newsModelRow) agent(t *testing.T, name string, role claim.Role) *newsModelAgent {
	a := &newsModelAgent{name: name, row: r, role: role, token: mustClaimToken(t, r.issue, role), errored: map[string]bool{},
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
	// ticks is how many admission passes the sweep has made, entry the newest entry the root
	// architect was handed, and spent how many refusals the claim store had spent at the last check
	// (lagging).
	ticks     int
	entry     int64
	spent     int
	ambiguity newsModelAmbiguity
	// superseding is the id of the task the supersede step queued since the last check, and
	// entryStarts says a step since then can have run the start of a newer entry: a re-entry's, a
	// re-admission's and a hand-over's starts of the reviewer, the sweep's admission pass, which
	// sends a claim the entry its phases row is owed, and the starts step, which runs such a start
	// that a refused write left on its backoff. Each of those steps sets it in its own case (do).
	// The two are what take the place of a task whose turn runs (check).
	superseding string
	entryStarts bool
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

// storedClaims is every claim the claim store holds now, and the delivery each holds by its token,
// nil for none. The store moves only between steps, so a check, a trace line and a restart each read
// it once and hand the map to every reader of it (lagging, standing).
func (m *newsModel) storedClaims() ([]supervise.Claim, map[claim.Token]*supervise.Delivery) {
	m.t.Helper()
	claims, err := m.claims.Claims(m.r.ctx)
	if err != nil {
		m.fail("read the stored claims: %v", err)
	}
	stored := make(map[claim.Token]*supervise.Delivery, len(claims))
	for _, c := range claims {
		stored[c.Token] = c.Pending
	}
	return claims, stored
}

// toldRows is every told row of the tree as every reader reads it (record.Store.TreeTold), by its
// issue and audience: a row the record does not hold reads as the zero row, as newsPathRig.toldOf
// answers it. A check and a trace line each read it once.
func (m *newsModel) toldRows() map[record.ToldKey]record.DesignTold {
	m.t.Helper()
	rows := map[record.ToldKey]record.DesignTold{}
	m.r.inTx(func(tx pgx.Tx) error {
		told, err := m.r.records.TreeTold(m.r.ctx, tx, m.r.root)
		for _, row := range told {
			rows[record.ToldKey{Issue: row.Issue, Audience: row.Audience}] = row
		}
		return err
	})
	return rows
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
	m := &newsModel{t: t, seed: seed, rng: rand.New(rand.NewPCG(uint64(seed), uint64(seed))), r: r, clock: &heldClock{},
		latest: 4, decisionOnly: map[int]bool{}, childGen: 1, rootGen: 1, child: child, root: root, ran: map[string]int{}, kinds: map[string]bool{}}
	m.reviewer = child.agent(t, "reviewer", claim.RoleReviewer)
	m.tester = child.agent(t, "tester", claim.RoleTester)
	m.architect = root.agent(t, "root's architect", claim.RoleArchitect)
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
	_, stored := m.storedClaims()
	rows := m.toldRows()
	parts := []string{fmt.Sprintf("approved %d", m.latest)}
	for _, row := range m.rows() {
		told := rows[record.ToldKey{Issue: row.issue, Audience: row.audience}]
		took, known := told.Took(record.DesignStepSpec)
		links, _ := m.standing(row, stored)
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
			a.name, pending, storedFrom(stored[a.token]), a.next, a.turn))
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
			told.entry = p.Entry
			if why := m.namesExactly(row, told, p.News); why != "" {
				m.fail("invariant 1: the news the %s's delivery %s records is not the news the prompt it was sent carried: %s", a.name, p.ID, why)
			}
		}
	}
	a.seen = len(prompts)
}

// namesExactly is "" where encoded, the news a delivery records for the prompt p, is the news p
// carried: the start's own text it names is in the prompt (for a reviewer's task that ends with the
// daemon's two sentences of its round, the text before them is, and the prompt ends with them,
// after the news: renderNews), it carries exactly the version pairs the prompt's text names and
// the opening it said opened, each to row, and each marked with the run the prompt was sent in.
// Otherwise it says what differs.
func (m *newsModel) namesExactly(row *newsModelRow, p *newsModelPrompt, encoded []byte) string {
	var news record.DeliveryNews
	if len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &news); err != nil {
			return fmt.Sprintf("its news %s does not decode: %v", encoded, err)
		}
	}
	own, round, _ := reviewround.Split(news.Task)
	if !strings.Contains(p.text, own) || !strings.HasSuffix(p.text, round) {
		return fmt.Sprintf("it says the prompt opened with the start's own text %q, which the prompt %q does not carry, its round's sentences last",
			news.Task, p.text)
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
