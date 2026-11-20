package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The steps of TestTheNewsRecordHoldsToWhatTheHolderWasReallyTold (design_news_model_test.go):
// which steps the model allows at each point of a seed's run (enabled), and what each step does
// to the daemon's parts and to the model's view of them (do).

// newsModelLatest is the newest version a seed approves: past it, no new version is saved.
const newsModelLatest = 40

// enabled is every step the model allows now, in a fixed order, so a seed always picks the same.
// Each step is listed as many times as it is likely, and the likeliest is the one that moves the
// run on from where it stands: the reconcile while the outbox holds a row of it, the reviewer's
// starts while it holds one of those, the agent's turn while it holds a prompt, the turn's end while
// it is in one, the provider error's ladder while it waits, and the shim's hello while the
// reviewer's claim store lags its machine, which is the decision that repairs it. A row nothing
// holds is not run, so a run spends its steps between turns and inside them rather than on rows that
// are not there; the sweep, a close and reopening of the gate and the hello are listed once always.
//
// A person's new version, of each kind, is listed once up to version newsModelLatest, and three
// times while the reviewer holds, or is in the turn of, a prompt of its run that told news (but not
// while nothing says yet what its run stood on), while the root architect is in the turn of a prompt
// that carried an opening, or while the reviewer holds no prompt and the outbox holds no reconcile
// and no start of the reviewer's: so a version lands behind what a prompt told and moves the opening
// past the one it carried, and a run that has told everything goes on. The reviewer's turn of a
// prompt that told news of its run ends as often in a provider error as in its result, but three
// times as often in its result once the gate has moved the row's opening past the one that prompt
// carried and no role message told the one that stands, so the fold of a passed opening comes; the
// turn of a prompt that told nothing of its run mostly ends in its result, so the run gets on to the
// next news. A newer task can take the place of a confirmed task whose turn runs.
//
// A seed makes two hand-overs, each listed six times while the reviewer is in no turn, no tester's
// turn runs and two versions are left to approve, so both come early enough. The first is held to
// the traces of a credit counted from an unfinal one. While its tester's turn runs no clear is
// listed at all. Until the reviewer has taken to its result a prompt of its run that told it a
// version while that credit stands (newsModel.reviewed), no refused write is listed, the reviewer's
// turn and its result are listed twelve times and nothing else ends that turn, and a person's new
// version once of each kind, so the reviewer's credit counted from the tester's is made final while
// the tester's stands; from then on the tester's provider error is listed six times, which gives the
// tester's credit back and ends the turn. The second is held to a clear under a credit of the run it
// ends: its tester's turn runs to the end of the drive, a re-entry and a re-admission are each listed
// four times while it does and its run stands, until one of them is made (handoverClearDue, whether
// or not the reviewer holds a prompt and outside newsModelClears), and a person's new version six
// times of each kind once a clear has ended that run, which owes the next run an opening under the
// number the tester's prompt carried for the run that ended.
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
// is listed eight times once the gate has moved the architect's opening past the one its prompt
// carried and no role message told the one that stands, once while its prompt carried an opening
// otherwise, so a person's new version can come first, and twice for a prompt that carried none.
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
	_, stored := m.storedClaims()
	lagging := m.lagging(rev, stored)
	if lagging {
		add("hello", 2)
	}
	held := m.r.observe()
	reconciles, starts := slices.ContainsFunc(held, ofKind(record.OutboxKindDesignChanges)), slices.ContainsFunc(held, reviewerStarts)
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
		pair := record.VersionPair{ArtifactID: pathDoc, From: took, To: m.latest}
		var recorded map[record.VersionPair]record.DesignChanges
		m.r.inTx(func(tx pgx.Tx) error {
			var err error
			recorded, err = m.r.records.DesignChangesOf(m.r.ctx, tx, []record.VersionPair{pair})
			return err
		})
		if _, held := recorded[pair]; !held {
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
	if slices.Contains(newsModelEntryStarts, name) {
		m.entryStarts = true
	}
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
		// whose turn runs; check marks the give-back where the claim holds this task, or none.
		m.superseding = fmt.Sprintf("model:%d", m.step)
		m.handle(rev, supervise.RequestDeliver{Claim: rev.token, ID: m.superseding, Task: "Review the change again.",
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
// which the model waits for (ranAt): a prompt whose claim holds another task by then, which the
// check after the step that replaced it marked (superseded), is another delivery's, and makes
// none, and a prompt that told nothing has no credit to make final.
func (m *newsModel) result(a *newsModelAgent) {
	m.t.Helper()
	spent := m.claims.spentCount()
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

// newsModelEntryStarts is every step that can run the start of a newer entry, which takes the place
// of a task whose turn runs (check): a re-entry's, a re-admission's and a hand-over's starts of the
// reviewer, the sweep's admission pass, which sends a claim the entry its phases row is owed, and
// the starts step, which runs such a start that a refused write left on its backoff.
var newsModelEntryStarts = []string{"reenter", "readmit", "handover", "sweep", "starts"}

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
	_, stored := m.storedClaims()
	for _, row := range m.rows() {
		m.settled(row, stored)
	}
}

// settled is the model's side of what the reconcile settles for row: a row that knows no version
// is filled from the review history, which names the version below the approved one
// (newNewsModel), and a row owed only versions that changed decision blocks alone, and no opening,
// is raised past them, by a credit counted from the version it took (record.Store.RaiseTold), so a
// raise read off a version an unfinal credit gave reaches nothing once that credit is given back.
func (m *newsModel) settled(row *newsModelRow, stored map[claim.Token]*supervise.Delivery) {
	m.t.Helper()
	if row.base == newsModelUnknown {
		row.base = m.latest - 1
	}
	links, opens := m.standing(row, stored)
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
// and the outbox runs the reviewer's start of the phase where the claim can take it
// (startReviewer). The run began under an open gate: no opening is owed to it.
func (m *newsModel) reenter() {
	m.t.Helper()
	m.reenterChild(phase.Reviewing)
	m.startReviewer()
	m.child.openSeq = m.r.toldOf(pathChild, record.AudienceHolder).Opening
	m.child.told[m.child.openSeq] = true
}

// startReviewer runs the row that creates the child's branch and every start of its reviewer the
// outbox holds, and waits for what the reviewer's machine started.
func (m *newsModel) startReviewer() {
	m.t.Helper()
	m.r.run(ofKind(record.OutboxKindIssueBranch))
	m.r.run(reviewerStarts)
	m.reviewer.machine.Wait()
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
		sent := "nothing"
		if p != nil {
			sent = strconv.Quote(p.text)
		}
		m.fail("the tester was sent %s, want its phase task telling version %d: the hand-over's own premise", sent, m.latest)
	}
	m.handle(tester, supervise.StreamTurnStart{Claim: tester.token})
	tester.turn, tester.next = tester.next, ""
	m.r.complete(pathChild)
	if child := readIssue(m.t, m.r.pool, pathChild); child.Phase != phase.Reviewing {
		m.fail("%s stands in %s after its tester completed, want reviewing", pathChild, child.Phase)
	}
	m.approve(false)
	m.reconcile()
	m.startReviewer()
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
	m.startReviewer()
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
	m.entry = max(m.entry, rolePhaseRow(m.t, m.r.pool, pathRoot, claim.RoleArchitect).Entry) + 1
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC).Add(time.Duration(m.entry) * time.Hour)
	stays := record.DecisionMark{Issue: pathRoot, Ref: fmt.Sprintf("dispatch://%s/ask/entry-%d-stays", pathRoot, m.entry),
		Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at}
	replaced := stays
	replaced.Ref, replaced.At = fmt.Sprintf("dispatch://%s/ask/entry-%d-replaced", pathRoot, m.entry), at.Add(time.Minute)
	keep := func(marks ...record.DecisionMark) {
		m.r.inTx(func(tx pgx.Tx) error {
			for _, mark := range marks {
				if err := m.r.records.AddParkedDecision(m.r.ctx, tx, record.ParkedDecision{Issue: mark.Issue, Ref: mark.Ref,
					Decision: mark.Decision, Actor: mark.Actor, At: mark.At}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	keep(stays, replaced)
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
	// A newer decision on the same reference replaces it, which leaves this task no longer holding
	// it (record.Store.AddParkedDecision).
	newer := replaced
	newer.At = replaced.At.Add(time.Hour)
	keep(newer)
	if a.machine == nil {
		a.machine, a.conn = m.r.connect(pathRoot, claim.RoleArchitect)
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
	told := m.toldRows()
	for _, row := range m.rows() {
		row.openSeq = told[toldKey{row.issue, row.audience}].Opening
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
	claims, stored := m.storedClaims()
	for _, a := range m.agents() {
		if m.lagging(a, stored) {
			at := storedFrom(stored[a.token])
			a.behind = &at
		}
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
	if _, err := sup.restore(m.r.ctx, claims); err != nil {
		m.fail("restore the stored claims: %v", err)
	}
	m.r.sup, m.r.conns, m.r.runner.supervisor = sup, deps.Conns.(*fake.Conns), sup
	for _, a := range m.agents() {
		if a.machine == nil {
			continue
		}
		a.machine, a.conn = m.r.reconnect(a.row.issue, a.role, a.turn != "")
		a.seen, a.next = 0, ""
		m.handle(a, supervise.StreamHello{Claim: a.token, Generation: a.machine.Claim().Generation})
	}
}
