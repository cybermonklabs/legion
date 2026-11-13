package admit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// quietTrees is the pass's measure of its trees: how long each tree of the project has been
// still and whether any claim of it is at work (record.QuietTrees), over the records the pass
// has already read and one read of the claims of the project (record.Store.ProjectClaims). It
// is one query for the whole pass, like the issues and the slots beside it: a pass that judges
// every tree of the project may make no read per tree. The claims it read are answered beside
// the measure, so that R6's reading of a claim at work (workingTrees) is taken over the very
// rows the measure was taken from and reads them no second time.
//
// issues are the pass's own live records and now the instant the pass measures from, so that
// every reading of a tree it makes — this one and the record-only one it is filtered with — is
// taken from one instant. The claim read is keyed by the claim project token, which
// `claims.project` holds, not by the Dispatch project key the issue records carry.
//
// Where it is called from: tellStalledTrees, after blockers.JudgeLine and the releases,
// where the pass holds the facts it judges from and has given back every slot it is going to. The
// measure is behind this method rather than in the pass because the rule that acts on it and
// the read it needs belong together. The state's projection answers the same question from the
// same statement over the claims it already holds (projection's quietTrees), so the pass and
// the state read one statement of the measure and neither keeps a rule of its own. They are
// handed their own records, though: this is given the project's live ones and the state every
// record it holds, a done one included, so a tree whose root is done is measured by the state
// and by no pass of this. Nothing here rests on their agreeing — the rule below tells the
// controller only of trees this daemon runs (record.RunningRoots), every one of which the live
// read holds.
func (a *Admission) quietTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue, now time.Time) (map[string]record.TreeQuiet, []record.TreeClaim, error) {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return nil, nil, err
	}
	claims, err := a.store.ProjectClaims(ctx, tx, project)
	if err != nil {
		return nil, nil, err
	}
	return record.QuietTrees(issues, claims, now), claims, nil
}

// tellStalledTrees tells the project's controller, once for each stall, of every tree of this
// daemon's own that has stopped. A tree has stopped when all of this holds: its root is one
// the daemon runs (record.RunningRoots), it has been quiet for longer than
// `stalled_tree_notice_hours` (record.TreeQuiet.Quiet, UseStalledTreeNotice), no claim of it
// is at work as R6 reads it (workingTrees: every state but idle, suspended, failed and retired,
// save a phase worker the daemon itself names silent), its newest movement stands after the
// stall its root was last told about (record.Issue.TreeStalledToldAt), and either some issue of
// it is held by nothing the daemon knows of (everyIssueWaitsByDesign over lineHolds, from what
// the caller handed over) or the project's line can no longer move (lineCanMove).
//
// The rule. An issue of the tree is held by something the daemon knows of exactly when one of
// the first three of these holds, read in this order, and the fourth is the rest:
//
//  1. it stands in awaiting_person, answered or not, wherever the line's walk placed it — among
//     the slot holders, in the line or left behind — because only a person's act moves it;
//  2. it is an entry of the waiting line holding no slot (record.LineEntry.Holds false), which
//     is the cap's own hold in both its shapes; an entry of the line holding a slot is not held;
//  3. its first phase has still to start — it stands at admitted, in the workflow, and is no
//     root waiting for its tree to be opened (record.QueuedRoot) — and the word the walk left it
//     behind with silences (silencedTree): its tree's root waits on a person, its design gate
//     is shut, a Dispatch blocker holds it, or its own children are open. Holding a slot
//     decides nothing here, because a root at admitted holds its slot through those same waits;
//  4. anything else is not held: a slot holder at work in its phase, an issue past its review
//     on no slot, a root waiting for its tree to be opened, an issue out of the workflow, and
//     an issue whose phase is already running that the line left behind. That last one is the
//     stall itself and never a wait, whatever word the ladder gives it: the words of clause 3
//     hold a start, and such an issue has had its start. A member in implementing under a root
//     that waits on a person is the case that rule decides — the root's wait is the root's
//     alone, and the tree is told.
//
// It is written once, as lineHolds.waitsByDesign, and both halves of the silencing rule ask it:
// everyIssueWaitsByDesign of every member of a quiet tree and lineCanMove of every slot holder.
//
// The holds of the daemon's own silence a tree only while the project's line can still move,
// which is the whole of why lineCanMove is read here and not per tree: a project no work of
// which can start again by itself has every tree of it named, however its members wait
// (dispatch://INFRA-2560, R5). The clause is a fact of the project and not of one tree, so it
// is answered once for the pass beside the per-tree walk rather than inside it, where every
// tree would ask the same question again.
//
// line and dropped are what the walk that built this pass's waiting line said of the project:
// the line itself and the row of every issue it left behind, with the one word for why the
// daemon runs nothing of it (record.HeldReason, record.Waiting's second answer, handed over by
// blockers.JudgeLine). The rule is given them rather than reading them because reading them is
// a walk of every issue of the project and the pass has made that walk already, to judge the
// line it promotes from: the measure and the line are then one reading of one project, which no
// second walk could promise, and the pass walks the project's issues once however many rules
// read the words. The words are the ladder's own (record.HeldReasonOf) and are written on every
// judgement whoever asked for the line, so handing them here costs the pass nothing it did not
// pay already.
//
// Why the mark is a movement and not a time of telling: the rule reads the tree's own movement
// already, so a tree told about is a tree whose movement the daemon has named. The mark is
// written only for a notice that went into the outbox, in this same transaction (the answer
// wakeController gives), so a stall found while no controller was registered, or while an
// older stalled-tree notice of the same root still waited to be published, is told at a later
// pass rather than marked told and lost. The notice is keyed per issue
// (record.NoticePerClaim), so a second tree that stops while the first tree's notice waits is
// told in the same pass.
//
// What it reads, and in what order. This runs inside promoteHolds, on every fact, every tick
// and every boot, and on a healthy project the answer is always that nothing has stopped, so
// each read is made only once the pass cannot answer without it, and every read it makes is one
// read for the whole pass and never a read per tree:
//
//  1. the records alone (record.QuietTrees with no claims), which costs no query. A claim's
//     step times only ever move a tree's measured movement forward, so the quiet time with the
//     claims is never longer than the quiet time without them: a root the records alone show
//     inside the bound is inside it whatever the claims say. A pass whose running roots are all
//     inside it ends there, which is every pass of a project whose trees are working;
//  2. the claims of the project (quietTrees, one read), which carry the step times the records
//     do not and whether an agent of the tree is at work. The told mark is compared with this
//     movement and never with the record-only one, which can stand behind it. Where a tree past
//     the bound and past its told mark still has a claim at work by its state, the trees R6
//     counts at work are built (workingTrees), which reads the runs of the project's phase
//     workers in ready once, in one batched record.Store.RoleRuns read, and only where the
//     deployment sets `worker_silence_minutes`; a pass with no such tree builds nothing here;
//  3. whether a controller is registered (record.Store.ControllerRegistered, one read). It is a
//     refusal wakeController makes below, and one that answers for every root of this pass at
//     once, so a project with no controller registered builds no membership and judges no tree
//     for a notice that could not be queued;
//  4. the membership of the candidate trees and the silencing rule over what the caller handed
//     over (lineHolds, everyIssueWaitsByDesign, lineCanMove), which cost no query and no walk
//     of the project's issues: the line and the words are the line's own walk's, made before
//     this method was reached, and they are put in maps by key here, at this step and at no
//     earlier one, so that a pass that ends above it builds nothing for them. The rule that
//     decides which trees are told is asked here, before anything is built or read for one
//     tree, so that a tree it silences costs the pass nothing further;
//  5. for the trees that are going to be told and for no other, whether each root's own
//     tree-stalled notice is still unsent (record.Store.ControllerNoticesPending, one batched
//     read) and then the pull requests of those trees' members (record.Store.PullRequestsOf,
//     one batched read). Neither is a read per root, and no reason is built for a root the
//     first of them refuses. wakeController still decides: it asks the registration and the
//     pending notice again, and the mark is written only for a row that went into the outbox.
//
// The liveness clause reads the slots this pass already holds, what the caller handed over and
// the trees R6 counts at work (lineCanMove). Those trees are the set step 2 built where step 2
// needed it, and are built here otherwise, so the pass builds the set at most once and makes
// its one read at most once, and the clause is answered only once a root has come through the
// bound, the measure and the registration above.
func (a *Admission) tellStalledTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue,
	facts record.LineFacts, slots []record.Slot, line []record.LineEntry, dropped []record.HeldIssue,
) error {
	if a.stalledTreeNotice <= 0 {
		return nil
	}
	now := a.now()
	// Step 1: the reading that costs no query. A root inside the bound here is inside it under
	// any claim of its tree, so this is the whole of a working project's pass.
	recorded := record.QuietTrees(issues, nil, now)
	running := record.RunningRoots(issues, facts)
	candidates := make([]record.Issue, 0, len(running))
	for _, issue := range issues {
		if _, runs := running[issue.Key]; !runs {
			continue
		}
		if recorded[issue.Key].Quiet > a.stalledTreeNotice {
			candidates = append(candidates, issue)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Step 2: the measure the rule decides on.
	quiet, claims, err := a.quietTrees(ctx, tx, issues, now)
	if err != nil {
		return err
	}
	stalled := make([]record.Issue, 0, len(candidates))
	claimed := false
	for _, root := range candidates {
		measure := quiet[root.Key]
		if measure.Quiet <= a.stalledTreeNotice {
			continue
		}
		if told := root.TreeStalledToldAt; told != nil && !measure.MovedAt.After(*told) {
			continue
		}
		claimed = claimed || measure.AtWork
		stalled = append(stalled, root)
	}
	// working is the trees R6 counts at work (workingTrees), built at most once per pass and
	// read by both of its readers: here, for a tree the bound and the told mark above have let
	// through with a claim at work by its state, and by the liveness clause below. It is built
	// here only where such a tree stands, and otherwise by the clause, if the pass gets that far.
	var working map[string]struct{}
	if claimed {
		if working, err = a.workingTrees(ctx, tx, issues, facts, claims, now); err != nil {
			return err
		}
		stalled = slices.DeleteFunc(stalled, func(root record.Issue) bool {
			_, atWork := working[root.Key]
			return atWork
		})
	}
	if len(stalled) == 0 {
		return nil
	}
	// Step 3: the refusal that answers for every root of this pass at once, asked before
	// anything is built for a notice it would refuse. A controller that is not registered
	// refuses the whole pass with one read, so nothing below is gathered or judged for a notice
	// nothing would read.
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return err
	}
	// Step 4: the tree is its root and the issues under it the workflow still runs
	// (record.TreeMember), which is the membership the measure counts and the membership the
	// notice names: a child the record shows as done is no member of either, since a comment on
	// its old pull request is no movement of the tree and its phase is nobody's to act on. It is
	// gathered for the candidate roots above alone, since no other tree of the project is named
	// by anything this pass writes.
	telling := make(map[string]struct{}, len(stalled))
	for _, root := range stalled {
		telling[root.Key] = struct{}{}
	}
	members := make(map[string][]record.Issue, len(stalled))
	for _, issue := range issues {
		if _, tells := telling[issue.Tree]; !tells || !record.TreeMember(issue) {
			continue
		}
		members[issue.Tree] = append(members[issue.Tree], issue)
	}
	// Why the daemon does not start each issue of the project now is what the caller hands
	// over: the walk that built the waiting line this pass judged placed each issue in the line
	// or left it behind with the one word for why (record.Waiting through blockers.JudgeLine),
	// and the slot rows that walk read say who holds the cap. The rules below ask those and
	// derive nothing of their own: the same term, read once from an owed start row, once from
	// the line's membership and once from an issue's need of a slot, disagreed with itself three
	// times inside one review of this change, each reading right about the shape in front of it
	// (dispatch://INFRA-2560, R5).
	//
	// It is handed over rather than read here because reading it is a walk of every issue of the
	// project and no pass may walk them twice: the pass has already made that walk to judge its
	// line, so this method's answer costs it nothing but the maps that put the walk's own answers
	// by key, built here (lineHoldsOf) and at no earlier step. A pass that never reaches this
	// step — a deployment that sets no bound, a project whose trees are all working — pays
	// nothing for what it is handed and never looks at it.
	//
	// The silencing rule is asked here, before anything beyond that membership is built or read
	// for one tree, because it is the rule that decides which trees are told: a tree it silences
	// costs this pass no read, no reason and no further work of any kind.
	//
	// A wait of the daemon's own silences a tree only while the project's line can still move
	// (lineCanMove, dispatch://INFRA-2560, R5). The answer is one fact of the project, so it is
	// taken once here and asked of every tree of the pass: where the line cannot move, nothing
	// of the project can start again by itself and every tree of it is named, however its
	// members wait, and the walk of the members below is not made at all. Its claim term reads
	// the trees R6 counts at work, the set step 2 built if it built one.
	holds := lineHoldsOf(facts, slots, line, dropped)
	if working == nil {
		if working, err = a.workingTrees(ctx, tx, issues, facts, claims, now); err != nil {
			return err
		}
	}
	canMove := a.lineCanMove(issues, holds, working)
	var stopped []record.Issue
	named := 0
	for _, root := range stalled {
		if canMove && a.everyIssueWaitsByDesign(members[root.Key], holds) {
			continue
		}
		stopped = append(stopped, root)
		named += len(members[root.Key])
	}
	if len(stopped) == 0 {
		return nil
	}
	// Step 5: the refusal that answers for one root, asked of the trees this pass is going to
	// tell and of no other. A root whose own tree-stalled notice still waits unsent is left for
	// a later pass; the kind is keyed by its issue and by no role at all (record.NoticePerClaim,
	// wakeController), which is the pair asked for here. Every root is asked in the one read
	// (record.Store.ControllerNoticesPending), because this runs on every fact, every tick and
	// every boot of a deployment whose projects hold hundreds of trees, and a read per root
	// would put that many round trips on the fact lock.
	roots := make([]string, 0, len(stopped))
	for _, root := range stopped {
		roots = append(roots, root.Key)
	}
	pending, err := a.store.ControllerNoticesPending(ctx, tx, a.project, record.TreeStalledNotice, roots, "")
	if err != nil {
		return err
	}
	unsent := make([]record.Issue, 0, len(stopped))
	keys := make([]string, 0, named)
	for _, root := range stopped {
		if _, waits := pending[root.Key]; waits {
			continue
		}
		unsent = append(unsent, root)
		for _, member := range members[root.Key] {
			keys = append(keys, member.Key)
		}
	}
	if len(unsent) == 0 {
		return nil
	}
	requests, err := a.store.PullRequestsOf(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, root := range unsent {
		measure := quiet[root.Key]
		notice := record.ControllerNotice{Kind: record.TreeStalledNotice,
			Reason: stalledTreeReason(root.Key, measure, members[root.Key], requests)}
		told, err := a.wakeController(ctx, tx, notice, root.Key, a.now())
		if err != nil {
			return fmt.Errorf("tell the controller that the tree of %s has stopped: %w", root.Key, err)
		}
		if !told {
			continue
		}
		if err := a.store.MarkTreeStalledTold(ctx, tx, root.Key, measure.MovedAt); err != nil {
			return err
		}
		intake.OnCommit(ctx, func() {
			a.log.Warn("admission: told the controller a tree has stopped", "issue", root.Key,
				"quiet", measure.Quiet, "movedAt", measure.MovedAt)
		})
	}
	return nil
}

// workingTrees is the trees of the project that R6 counts at work, keyed by root: every tree a
// claim of which is at work, a claim being at work in every state but idle, suspended, failed
// and retired, save a phase worker the daemon itself names silent. claims is the rows the
// measure was taken from (quietTrees), read through the measure's own membership
// (record.TreeMember), so a claim of an issue that is no member makes no tree work, exactly as
// it moves none. It always answers a map, empty where nothing is at work, so its caller can tell
// a set it built from one it has not.
//
// The rule is R6 (dispatch://INFRA-2560): "A tree is never reported as stalled while any claim
// of it is at work. A claim is at work in every state but idle, suspended, failed and retired,
// with one exception: a phase worker the daemon itself names silent - up between turns in
// ready, holding no task, its last step older than worker_silence_minutes
// (workflow.SilentPhaseWorker) - is no work under way." Such a worker is not a process on its
// way up but one stopped between turns, and it is the worker cybermonklabs/legion#3829 sends its
// phase's task again. Counted as work, a tree whose resend never brought its worker back would
// read as working for good and no controller would ever hear of it.
//
// "At work" is one term of the spec and R6 defines it, so both readers of it take this one set:
// the per-tree refusal of tellStalledTrees and the claim term of the liveness clause
// (lineCanMove), which R5 writes in the same words. A worker the daemon names silent makes no
// phase change and no status change, so it frees no slot and keeps the line moving no more than
// an idle claim does (the architect's ruling on this change).
//
// Why the daemon's own judgement and not one of this measure's: the claim is named silent by
// workflow.SilentPhaseWorker over the role's run, with the bound the silence handler and the
// tick's silent list read (UseWorkerSilence, `worker_silence_minutes`), and by no test of
// silence written here. A second reading would sooner or later name a worker the repair does
// not, or pass over one it repairs, and the daemon would then say two things of one claim.
//
// Every other state keeps its reading (record's restingStates). A claim in ready the rule does
// not name — one that stepped inside the bound, holds a task, has a start on its way, whose own
// session's report explains its quiet, or that is not the worker of its issue's phase, such as
// an architect — is work under way, and so is every claim in queued, launch_uncertain,
// launching, shim_connected, registered or working. A bound of zero or less is the setting off,
// the admission nobody registered a bound with: then no claim is judged, nothing is read, and
// the set is every tree with a claim at work by its state (record.TreeQuiet.AtWork), which is
// what both readers read before R6.
//
// Which claims are judged: only a claim in ready, and only where no other claim of its tree is
// already at work. Ready is the one state that is at work by restingStates and that the rule can
// name silent — it names a claim in ready or idle and no other (workflow's
// unpromptedPhaseWorker), and idle is no work already — so the runs of the claims in the other
// working states are never read; and a tree another claim keeps at work is at work whatever its
// worker in ready is, so that worker is not judged either.
//
// What it costs: at most one batched read of the runs of those claims' issues' phase workers
// (phaseWorkers, record.Store.RoleRuns, the read the tick's silent list makes for the same
// rule), made only where some claim is left to judge, and walks of the pass's own records and
// of the claims quietTrees already read. It makes no read per tree or per issue, and the caller
// builds it at most once per pass, at the first of its two readers the pass reaches. The run is
// read inside the pass's own transaction, as the claims were. It reads no phase row
// (workflow.PhaseWorkerOwedTurn), which the tick's list adds to leave out a worker owed no turn:
// such a worker is stopped all the more, and R6 names SilentPhaseWorker alone.
func (a *Admission) workingTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts record.LineFacts,
	claims []record.TreeClaim, now time.Time,
) (map[string]struct{}, error) {
	byKey := make(map[string]record.Issue, len(issues))
	memberOf := make(map[string]string, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
		if record.TreeMember(issue) {
			memberOf[issue.Key] = issue.Tree
		}
	}
	working := make(map[string]struct{})
	var ready []record.TreeClaim
	for _, held := range claims {
		tree, member := memberOf[held.Issue]
		if !member || !held.AtWork() {
			continue
		}
		if a.workerSilence > 0 && held.State == supervise.StateReady {
			ready = append(ready, held)
			continue
		}
		working[tree] = struct{}{}
	}
	var judged []record.Issue
	asked := make(map[string]struct{}, len(ready))
	for _, held := range ready {
		if _, atWork := working[memberOf[held.Issue]]; atWork {
			continue
		}
		if _, seen := asked[held.Issue]; seen {
			continue
		}
		asked[held.Issue] = struct{}{}
		judged = append(judged, byKey[held.Issue])
	}
	workers, runs, err := a.phaseWorkers(ctx, tx, judged, byKey, facts)
	if err != nil {
		return nil, err
	}
	silent := make(map[claim.Token]struct{}, len(workers))
	for _, worker := range workers {
		gate := facts.Gates[worker.issue.Tree]
		if workflow.SilentPhaseWorker(worker.issue, worker.root, gate, worker.role, runs[worker.token], a.workerSilence, now) {
			silent[worker.token] = struct{}{}
		}
	}
	for _, held := range ready {
		if _, named := silent[held.Token]; !named {
			working[memberOf[held.Issue]] = struct{}{}
		}
	}
	return working, nil
}

// silencedTree is the measure's answer for every word the daemon has for why it runs nothing of
// an issue (record.HeldReason, the one vocabulary of that question, read by one ladder,
// record.HeldReasonOf): whether an issue the line left behind for that word is waiting by
// design, so that a tree whose every quiet issue waits for such reasons has not stopped and is
// told to nobody. It is the one table of those answers, every word written out with no default,
// and the rule beside it (lineHolds.waitsByDesign) reads it where the word decides: nothing here
// derives a word of its own from the waiting line, because the line and the words come out of
// one walk of the record package (record.Waiting) and a second derivation is the fault
// dispatch://INFRA-2560 was filed for.
//
// Four words silence, and they are R5's waits by design as the ladder names them: a tree whose
// root waits on a person, which only a person's act moves (root_awaiting_person); a tree whose
// design gate is closed, whose architect is writing the design the work must follow
// (design_gate); an issue a Dispatch blocked-by link holds (dispatch_blocker); and an issue the
// children-first rule holds (children_first). R5's fourth wait, "an issue that needs a slot and
// holds none, whatever holds its start", is no word of the ladder's, because the ladder speaks
// only of the issues the line left behind: it is the line's own membership, an entry holding no
// slot (record.LineEntry.Holds), and the first two words are the two ways the daemon keeps such
// an issue out of the line instead. The test of that wait is the need and not the mechanism:
// each earlier reading of it named a mechanism and missed a shape that answers to the same
// cause.
//
// Two directions decide the rest, and they are different questions. The word for an issue no
// other word is true of (unclassified) does not silence, and neither does a word this table
// does not know at run time, which a lookup that misses answers false for: an issue standing
// still for no recorded reason is the thing this issue was filed about, so the default is to
// name the tree. The other is a judgement about a known state: the cap's own hold — an entry of
// the line that holds no slot — silences a member whose start the daemon never wrote at all, a
// fault rather than a wait, read from the start row, and that is accepted rather than
// overlooked, because such an issue holds no admission slot and so costs the project none of
// the capacity this measure exists to recover, while its root, which does hold a slot, is judged
// by the words and is named when nothing else of the tree moves.
//
// The words that do not silence are each a tree's own work or a person's: a tree closed or
// parked waits for a re-admission (tree_closed, tree_parked), a root whose label is off waits
// for a person to hand it over (label_off), a root a live tree has not released waits for its
// architect (ancestry), an issue under an accepted ancestor waits for that ancestor to close
// (accepted_ancestor), and an issue past its review is at work on no slot, whose stillness is
// the stall itself (at_work). None of them is the daemon holding work it means to start, which
// is what a wait by design is.
//
// A word with no row here fails admit's TestTheMeasureAnswersEveryHeldReason, which reads the
// words from the record package's own source: a word added to the vocabulary and left out of
// this table would otherwise be ignored quietly, which is the fault rather than a new place to
// hide it.
var silencedTree = map[record.HeldReason]bool{
	record.HeldTreeClosed:         false,
	record.HeldTreeParked:         false,
	record.HeldLabelOff:           false,
	record.HeldAncestry:           false,
	record.HeldRootAwaitingPerson: true,
	record.HeldDesignGate:         true,
	record.HeldDispatchBlocker:    true,
	record.HeldChildrenFirst:      true,
	record.HeldAcceptedAncestor:   false,
	record.HeldAtWork:             false,
	record.HeldUnclassified:       false,
}

// silences says whether an issue the line left behind for word is waiting by design, from the
// one table above. A word the table does not name answers false, which is the default the
// table's doc states: an issue standing still for a reason nothing has decided about is named.
func silences(word record.HeldReason) bool { return silencedTree[word] }

// lineHolds is what the walk that built this pass's waiting line said of the project's issues,
// put by key for the measure (tellStalledTrees): the line's own entries, the issues the slot
// rows the walk read name, and the word of every issue the walk left behind. None of it is
// judged here: the line and the words are record.Waiting's two answers through
// blockers.JudgeLine, and the slots are the pass's own rows.
type lineHolds struct {
	// line is the waiting line's entries, by issue key. An entry says whether its issue already
	// holds a slot (record.LineEntry.Holds), which is the one fact of the cap's own hold.
	line map[string]record.LineEntry
	// holders is the issues one of the slot rows names, by key. The rows are every project's,
	// and the measure asks of this project's own records only, so a row of another project
	// matches no issue it asks about.
	holders map[string]struct{}
	// words is the one word for each issue the walk left behind (record.HeldIssue.Reason), by
	// key. The walk can leave one issue behind at two of its sites, and both rows carry the
	// ladder's word over one set of facts, so the first row is the one kept, as record.HeldList
	// keeps it.
	words map[string]record.HeldReason
	// gates is the pass's design gates, by root key, which tell a root waiting for its tree to be
	// opened (record.QueuedRoot) from one the daemon has already started.
	gates map[string]record.TreeGate
}

// lineHoldsOf puts by key what the caller handed over. It is built once per pass, at step 4 of
// tellStalledTrees and at no earlier step, so a pass the measure ends above builds none of it;
// it costs one map write for each entry of the line, each slot row and each row the walk left
// behind, and it reads no record and asks the ladder nothing again.
func lineHoldsOf(facts record.LineFacts, slots []record.Slot, line []record.LineEntry,
	dropped []record.HeldIssue,
) lineHolds {
	holds := lineHolds{line: make(map[string]record.LineEntry, len(line)),
		holders: make(map[string]struct{}, len(slots)),
		words:   make(map[string]record.HeldReason, len(dropped)), gates: facts.Gates}
	for _, entry := range line {
		holds.line[entry.Issue.Key] = entry
	}
	for _, slot := range slots {
		holds.holders[slot.Issue] = struct{}{}
	}
	for _, row := range dropped {
		if _, seen := holds.words[row.Issue]; !seen {
			holds.words[row.Issue] = row.Reason
		}
	}
	return holds
}

// waitsByDesign says whether issue stands still for a wait of the daemon's own: the rule the
// measure asks of every member of a quiet tree (everyIssueWaitsByDesign) and of every slot
// holder (lineCanMove). Each fact is read where record keeps it, and the word is read only
// where it decides:
//
//  1. an issue waiting on a person (record.Issue.AwaitingPerson, a root in awaiting_person,
//     answered or not) waits by design wherever the walk placed it: it keeps its slot while it
//     waits (record.KeepsSlot, the ruling on dispatch://INFRA-1909), and once answered it
//     stands in the line as well, so neither membership says anything of why it stands still;
//  2. an entry of the waiting line waits by design exactly where it holds no slot
//     (record.LineEntry.Holds): that is the cap's own hold, in both its shapes — a start written
//     and marked for a row the slots table names none of, and a first phase whose start the
//     promotion that grants the row writes. An entry that already holds a slot waits for nothing
//     but its own work;
//  3. an issue whose first phase has still to start — standing at admitted, in the workflow and
//     no root waiting for its tree to be opened — that the line left behind waits by design
//     exactly where its word silences (silencedTree). Its slot decides nothing here: a tree's
//     root at admitted holds one while its design gate is closed or its children are open, and
//     it waits for that gate or those children all the same;
//  4. every other issue does not: a slot holder at work in its phase, an issue past its review
//     on no slot, a root waiting for its tree to be opened, an issue out of the workflow, and an
//     issue already started that the line left behind. The holds the ladder names for that last
//     one — its root's wait on a person, its tree's gate — hold a start, and such an issue has
//     had its start: it is in its phase and not moving, which is the stall this measure exists
//     to report and not a wait by design (dispatch://INFRA-2560, R5).
func (h lineHolds) waitsByDesign(issue record.Issue) bool {
	if issue.AwaitingPerson() {
		return true
	}
	if entry, waits := h.line[issue.Key]; waits {
		return !entry.Holds
	}
	if issue.Phase != phase.Admitted || !record.InWorkflow(issue) || record.QueuedRoot(issue, h.gates[issue.Key]) {
		return false
	}
	return silences(h.words[issue.Key])
}

// lineCanMove says whether the project's line can still move: whether any work of this project
// can start again by itself. It is the liveness clause of the silencing rule
// (dispatch://INFRA-2560, R5): the waits the daemon makes on purpose leave a tree unnamed only
// while this holds, because a wait is a wait for something, and a project where nothing is
// coming waits for nothing.
//
// Why a project that fails both terms is stopped for good: a slot is freed by a phase or by a
// status change, and both of those need a worker, and a worker needs a slot. So where no slot
// holder can finish and no agent is at work, no slot is ever given back, no issue of the line
// is ever promoted, and every tree of the project stands exactly where it is until a person
// moves one. Naming those trees is the whole point of the rule — that project is the deadlock
// it was filed to find — so the caller silences nothing there, however its members wait.
//
// The two terms, either of which is the line moving, in the order they are asked, which is
// cheapest first:
//
//  1. the claim term: some tree of the project is at work as R6 reads it, taken from the set
//     the caller built once for the pass (workingTrees, its working argument): some claim in a
//     state other than idle, suspended, failed and retired, save a phase worker the daemon
//     itself names silent. The per-tree refusal above reads the same set, so the two read one
//     statement of what being at work is, and the spec's one term. A silent worker is no part
//     of the line moving: it makes no phase change and no status change, so it gives no slot
//     back, no more than an idle claim does (dispatch://INFRA-2560, R6, as its architect
//     ruled for this term). It costs a length check of a map the caller holds; the set's own
//     read is workingTrees', made at most once per pass. It is the project's answer and not
//     one tree's: an agent working anywhere in the project will free its slot when it is
//     done, and that slot goes to the line;
//  2. the slot term: some issue of the project holds one of the cap's slots and waits for
//     nothing of the daemon's (lineHolds.waitsByDesign) — that issue's worker can finish, which
//     frees the slot. The slots are the pass's own rows, the ones it judged the line from, so
//     the fact is the slots table's and not a phase rule's reading of it: an issue holds a slot
//     when a row names it. The table carries every project's rows while the cap counts this
//     project's alone (ownSlots), and the walk below is over this project's own records, so a
//     row of another project matches no issue here and says nothing about this line. A holder is
//     asked exactly what a member is asked, by the one rule, and the cap's own hold is simply one
//     a holder never carries: it is an entry of the line that holds no slot
//     (record.LineEntry.Holds).
//
// The slot term is over every issue of the project, and never over the trees the pass is about
// to tell. An issue of a tree nobody is telling — a tree inside the bound, a tree already
// told, a tree of a root this daemon does not run — holding a slot and silenced by nothing is
// the line moving, because its worker frees that slot for the line like any other. A later
// reader narrowing this walk to the stalled trees' members would make the rule answer
// "stopped" for every project whose quiet trees all wait, which is the opposite of what it
// says.
//
// It is answered once per pass, by the caller, and never per tree: the question is the
// project's and its answer is the same for every tree of the pass.
func (a *Admission) lineCanMove(issues []record.Issue, holds lineHolds, working map[string]struct{}) bool {
	if len(working) > 0 {
		return true
	}
	for _, issue := range issues {
		if _, holdsSlot := holds.holders[issue.Key]; !holdsSlot {
			continue
		}
		if holds.waitsByDesign(issue) {
			continue
		}
		// This holder waits for nothing the daemon knows of: its worker can finish and give
		// the slot back, so the project's line can move.
		return true
	}
	return false
}

// everyIssueWaitsByDesign says whether every issue of a quiet tree is held by something the
// daemon itself knows about (lineHolds.waitsByDesign): an issue that has not stopped but is
// holding still for a reason that will end.
//
// It answers one tree and is half of the silencing rule: a tree is left unnamed only where
// this answers true and the project's line can still move (lineCanMove, which the caller asks
// once for the whole pass). The liveness half is no part of this walk, because it is a fact of
// the project and this is a walk of one tree's members.
//
// Every, and never any: "A tree whose every quiet issue is one of those is never named"
// (dispatch://INFRA-2560, R5). A tree with one issue in a wait of the daemon's and one that has
// simply stopped is named, because nothing is going to move the stopped one, and that tree —
// a root waiting on a person over a child whose worker died, say — is exactly what this rule
// was filed to find. Answered on any one member, that single wait would silence the whole tree
// for as long as it lasted.
//
// Every member of the tree is every quiet issue of it, which is what makes this the rule R5
// states. The caller reaches this only for a tree nothing of which is at work
// as R6 reads it (workingTrees: no claim at work save a phase worker the daemon names silent)
// and whose quiet time stands past the deployment's bound, and that
// quiet time is measured over the whole tree at once (record.QuietTrees: the newest movement
// of any member and of any claim of a member). So no issue of a candidate tree has moved
// inside the bound, and the walk below is the walk over the tree's quiet issues. The members
// are the measure's own (record.TreeMember), and a tree always holds at least its root, so no
// empty walk ever answers true here.
//
// What holds a member is no judgement of this rule's: the walk that reads the project's
// issues, facts and slots places it in the line or leaves it behind with a word
// (record.Waiting), and the table beside it says what the word means, so a member held by
// something this measure has never heard of is named rather than excused. A task, a notice or
// a message queued for a role is no hold of the daemon's at all: a notice queued to a suspended
// role waits for a role nothing brings back, so an issue with one in the outbox has stopped
// like any other, and nothing of the outbox reaches the walk. An issue in `held` is no wait
// either — it waits for its architect, which is the tree's own work — and it holds its slot in
// a phase already started, so it waits for nothing of the daemon's and its tree is told like
// any other stall. An issue whose merge is still to land stands in `merging`, where the merger
// works, and is read the same way: it holds no slot and stands in no line, past its review
// (at_work), silences nothing, and its tree is named once every claim of the tree is
// at rest and nothing of it has moved inside the bound. A merger at work is what keeps such a
// tree unnamed, and that is read from the claim and never from the phase
// (record.TreeQuiet.AtWork, which the caller asks before this walk).
func (a *Admission) everyIssueWaitsByDesign(members []record.Issue, holds lineHolds) bool {
	for _, issue := range members {
		if holds.waitsByDesign(issue) {
			continue
		}
		// Nothing of the daemon's holds this issue: it has stopped, and so has its tree.
		return false
	}
	return true
}

// stalledTreeReason is what a stalled-tree notice tells the project's controller: the root, how
// long the tree has been quiet, and, for each member of the tree (record.TreeMember), the phase
// its record carries — `held` for a held issue — the role whose turn that issue's standing is
// (workflow.RoleFor over record.StandsAt, which is the phase a held issue was held from) or
// that the phase has no role, and the state of its pull request. It ends by telling the
// controller to read the tree and decide.
//
// The membership is the measure's own and is read from the one predicate, so the notice names
// exactly the issues whose stillness was measured: a root, whatever its own status says, and
// every issue under it the workflow still runs. The caller gathers members by that same
// predicate (tellStalledTrees), so each member it hands over is named here and nothing is
// tested a second time.
//
// It is written in short sentences a controller agent reads without decoding anything: the
// daemon knows the tree has stopped and knows nothing of why, so what it hands over is the
// facts and the decision, never a guess. The pull request is named from the record of its own
// table (requests, keyed by issue), because the state a stopped tree is most often in is a
// merged one — the tree whose work landed and whose roles then stopped — which no list of the
// open pull requests could answer.
func stalledTreeReason(root string, measure record.TreeQuiet, members []record.Issue, requests map[string]record.PullRequest) string {
	var reason strings.Builder
	fmt.Fprintf(&reason, "%s has stopped. The tree has been quiet for %.1f hours. No agent of the tree is at work.",
		root, measure.Quiet.Hours())
	for _, member := range members {
		fmt.Fprintf(&reason, "\n%s is in %s. ", member.Key, member.Phase)
		if role := workflow.RoleFor(record.StandsAt(member)); role != "" {
			fmt.Fprintf(&reason, "The %s has the turn. ", role)
		} else {
			reason.WriteString("No role works this phase. ")
		}
		if pr, recorded := requests[member.Key]; recorded {
			fmt.Fprintf(&reason, "Pull request %d is %s.", pr.Number, pr.State)
		} else {
			reason.WriteString("The record holds no pull request.")
		}
	}
	reason.WriteString("\nRead the tree and decide what it needs. You can ask a role what it waits on." +
		" You can move an issue back a phase. You can park the tree.")
	return reason.String()
}
