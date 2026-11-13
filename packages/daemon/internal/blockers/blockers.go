// Package blockers mirrors what Dispatch says every issue of the daemon's project waits on, and
// judges from that mirror which of an issue's blockers are still unfinished.
//
// Admission decides whether to admit a root, and the workflow whether to start a child's first
// phase, inside a transaction that performs no I/O: neither can ask Dispatch what an issue is
// blocked by at the moment it decides. So the daemon keeps its own table of Dispatch's answer
// (record.DispatchIssueRow), written by Mirror as each issue event arrives and by Reconcile from
// the boot listing, and read back for the issues a judgement names, with what they wait on.
package blockers

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// doneStatus is the one Dispatch status that finishes an issue. Nothing else does: an issue moved
// to backlog or icebox has stopped, not finished, and an issue waiting on it still waits.
const doneStatus = "done"

// Row is the mirror as the judgement reads it: one issue's Dispatch status and the keys it waits
// on, in the order Dispatch lists them.
type Row struct {
	Status   string
	Blockers []string
}

// Rows indexes mirrored rows by issue key, as Unfinished takes them: every caller reads the rows
// of the issues it judges in one query (Load) and then judges in memory.
func Rows(mirrored []record.DispatchIssueRow) map[string]Row {
	rows := make(map[string]Row, len(mirrored))
	for _, row := range mirrored {
		rows[row.Key] = Row{Status: row.Status, Blockers: row.Blockers}
	}
	return rows
}

// Load is the mirror of keys, the issues a judgement reads, and of every issue their rows wait
// on, read in one query (record.Store.DispatchIssuesOf) and indexed as every judgement here takes
// it. No keys read nothing.
//
// Admission decides whether to admit a root, and the workflow whether to start a child's first
// phase, inside a transaction that performs no I/O: neither can ask Dispatch what an issue is
// blocked by at the moment it decides. Both read the rows Mirror wrote instead — the deciding
// fact's own row included, written ahead of them in the same transaction — and judge them in
// memory. Each call that judges loads once, whatever the number of issues it judges, and reads the
// rows of those issues alone, never the project's whole mirror, which keeps a row of every issue
// the project ever had.
func Load(ctx context.Context, tx pgx.Tx, store record.Store, project string, keys []string) (map[string]Row, error) {
	if len(keys) == 0 {
		return map[string]Row{}, nil
	}
	mirrored, err := store.DispatchIssuesOf(ctx, tx, keys)
	if err != nil {
		return nil, fmt.Errorf("read what the issues of %s wait on: %w", project, err)
	}
	return Rows(mirrored), nil
}

// Judged is the keys whose rows a judgement of issues and waiting reads (Blocked, and the
// deciders it stands for): each root of waiting the rule does not exempt (WaitsOn), and each issue
// a blocker can hold where it stands (Holdable: Holds, and the waiting line's own hold through
// HeldIssues). Load reads these and what they wait on.
func Judged(issues, waiting []record.Issue) []string {
	keys := make([]string, 0, len(waiting))
	for _, issue := range waiting {
		if !Exempt(issue) {
			keys = append(keys, issue.Key)
		}
	}
	for _, issue := range issues {
		if Holdable(issue) {
			keys = append(keys, issue.Key)
		}
	}
	return keys
}

// Unfinished is the keys key waits on that are not done, in the order its row lists them, and is
// empty when key may proceed.
//
// A blocker with no row of its own counts as unfinished: a child can name a blocker whose
// issue.created the daemon has not consumed yet, and holding is the safe reading — the row
// arrives with the next event, the periodic turn or the next boot, and the wait ends then. A key
// with no row of its own, on the other hand, names no blockers at all and so waits on nothing.
func Unfinished(rows map[string]Row, key string) []string {
	return UnfinishedOf(rows, rows[key].Blockers)
}

// UnfinishedOf is the keys of blockedBy that are not done, in its order, judged as Unfinished
// judges a mirrored issue's own list. It takes the list from the caller for an issue whose own
// row may not exist yet: release_children answers for a child it has only just written todo,
// whose row arrives with that write's event, from the blockers Dispatch just showed it.
func UnfinishedOf(rows map[string]Row, blockedBy []string) []string {
	unfinished := []string{}
	for _, blocker := range blockedBy {
		if row, ok := rows[blocker]; !ok || row.Status != doneStatus {
			unfinished = append(unfinished, blocker)
		}
	}
	return unfinished
}

// LineWaitsOn is what one candidate of the waiting line still waits on among the Dispatch
// issues its mirror row names, under the rule its kind is judged by. A root whose tree is not
// open yet is judged by WaitsOn, which exempts a tree Legion takes back. An issue whose first
// phase would start is judged by Holds, which exempts nothing and answers nothing at all for a
// root: a root at admitted inside a running tree is not held by its links here. Its links are
// read by the rule for a started issue instead (waitsOf), on every pass, and a blocker not done
// gives the root's slot back. That holds its first phase too: the root's entry is then
// record.LineWaitingOnIssue, and this answers its Waits. An issue the daemon started that waits
// on another issue carries that judgement with it (record.LineEntry.Waits, startedWaits), which
// is read back here rather than made again: it was made over the rows and the children-first
// facts of the pass that judged it, and the line's entry is where the pass that promotes it reads
// it. An issue whose work is already under way — a return, an answered root's resume — waits on
// nothing here, for that same reason.
//
// It is the one statement of the rule, and both the promotion that skips a candidate (admit's
// promoteHolds) and the state's own report of what Legion waits for (Blocked) read it, so no
// issue is reported blocked by a link the promotion starts it in spite of.
func LineWaitsOn(rows map[string]Row, entry record.LineEntry) []string {
	switch entry.Kind {
	case record.LineQueuedRoot:
		return WaitsOn(rows, entry.Issue)
	case record.LineFirstPhase:
		return Holds(rows, entry.Issue)
	case record.LineWaitingOnIssue:
		return entry.Waits
	default:
		return nil
	}
}

// Blocked is every issue of the project that waits on another issue, by issue key: the ones
// Legion is waiting to start, and the ones it has already started that wait on an issue and
// stand in the line for a slot again (record.LineWaitingOnIssue). It is what the daemon's state
// shows under `blocked` and what a tick notice tells the controller, from the one judgement both
// readings and both deciders share.
//
// It is the decisions themselves, read rather than made: a candidate of the line — which the
// caller passes as it computes it — that promotion skips (LineWaitsOn, the rule promotion
// itself reads, per kind), and a recorded child the workflow holds at `admitted` (Holds;
// admission starts it once Holdable meets nothing Unfinished). A candidate the rule exempts is
// never judged, as the decider that would admit it does not judge it either (Exempt): what
// promotion starts whatever its links say is never reported as waiting on them.
//
// more is what a decider outside this package has already judged and rendered, which today is
// the children-first wait (record.ChildWaitRows): that rule reads the mirror's parent column
// against the daemon's records rather than an issue's blocked-by links, so it cannot be made
// here, and it must still reach the one list a reader reads. A key both deciders name keeps one
// row, with each one's issues in it — the deciders are independent, and an issue can wait on a
// link and on a child at once.
func Blocked(rows map[string]Row, issues []record.Issue, line []record.LineEntry, more []record.BlockedIssue) []record.BlockedIssue {
	blocked := make([]record.BlockedIssue, 0, len(line)+len(more))
	at := make(map[string]int, len(line)+len(more))
	add := func(key string, waits []string) {
		if index, named := at[key]; named {
			for _, wait := range waits {
				if !slices.Contains(blocked[index].BlockedBy, wait) {
					blocked[index].BlockedBy = append(blocked[index].BlockedBy, wait)
				}
			}
			return
		}
		at[key] = len(blocked)
		blocked = append(blocked, record.BlockedIssue{Issue: key, BlockedBy: waits})
	}
	for _, entry := range line {
		if waits := LineWaitsOn(rows, entry); len(waits) > 0 {
			add(entry.Issue.Key, waits)
		}
	}
	for _, issue := range issues {
		if waits := Holds(rows, issue); len(waits) > 0 {
			add(issue.Key, waits)
		}
	}
	for _, row := range more {
		add(row.Issue, row.BlockedBy)
	}
	slices.SortFunc(blocked, func(a, b record.BlockedIssue) int { return cmp.Compare(a.Issue, b.Issue) })
	return blocked
}

// Exempt says whether an issue starts whatever its links say, and so is neither held by the
// promotion skip (WaitsOn) nor reported here. It is the one definition the promotion skip and the
// state's reading of waiting roots share, so a root one of them exempts can never be reported
// blocked by the other.
//
// Two kinds of issue are exempt. One that carries a run into a tree of its own — a handed-over
// child, a re-admitted tree — is work already under way, which the promotion never holds by its
// links (record.Issue.CarriesRun), by whichever door it arrives. Once it holds a slot, the rule
// for a started issue (waitsOf) reads its links like any other. And a tree Legion takes back
// after it has finished and lingered starts again at its person's word, though re-admission leaves
// it carrying no run: that is both halves at once, that it ran and that the tree coming back is its
// own, which record.Issue.TakenBack holds. The mark is a fact about a run: the workflow writes it
// as a phase of that run begins, re-admission keeps it when the root's own tree comes back, and
// every other path that opens a run starts it clean — a child's re-entry into its tree, and a
// child's hand-over to a root of its own without its run. So no issue is exempt unless the run it
// is exempt for is the run it arrived with. A root waiting on a person (awaiting_person, answered or
// not) carries its run (record.Issue.CarriesRun): its links were read when it was first admitted,
// and it resumes on the slot it holds whatever they say now (waitsOf). Everything else is checked like
// any other issue: a child that has been released but never started, a waiting root that only left
// the line and came back, and a child handed over from a finished tree as a root of its own without
// its run, whether or not it ran there, a done child a person labels as an orphan root among them. A
// child held at phase.Admitted in its own tree is no candidate here at all: the workflow holds it,
// and Blocked reports it, by Holds, whatever its mark — and a re-entered child carries none.
func Exempt(issue record.Issue) bool { return issue.CarriesRun() || issue.TakenBack }

// WaitsOn is what issue still waits on, from rows: the issues Dispatch says it is blocked by
// that are not done (Unfinished, which counts a blocker the daemon has no row of as not done),
// and nothing at all for an issue the rule exempts, which starts whatever its links say — the
// one definition the promotion skip and the root half of the state's `blocked` both read, the
// exemption included (Exempt). The workflow's hold of a child reads Holds instead, which exempts
// nothing.
func WaitsOn(rows map[string]Row, issue record.Issue) []string {
	if Exempt(issue) {
		return nil
	}
	return Unfinished(rows, issue.Key)
}

// Holdable says whether a blocker can hold issue where it stands: it is a child of a tree rather
// than a root of its own, and it waits at phase.Admitted, where workflow.Engine.enterChild or
// the opening of its tree's gate left it. A root's wait is the waiting line's instead, judged
// over record.Waiting by the promotion that skips it.
func Holdable(issue record.Issue) bool {
	return !claim.IsTreeRoot(issue.Key, issue.Tree) && issue.Phase == phase.Admitted
}

// Holds is what holds issue where it stands: the issues it waits on that are not done
// (Unfinished) when a blocker can hold it there (Holdable), and nothing otherwise. It exempts
// nothing: a child held at phase.Admitted is held whatever its mark (Exempt). It is the one
// statement of the child hold rule — what a held child waits on — which the waiting line's own
// hold (record.LineFacts.Held, through HeldIssues) and the child half of Blocked both read.
func Holds(rows map[string]Row, issue record.Issue) []string {
	if !Holdable(issue) {
		return nil
	}
	return Unfinished(rows, issue.Key)
}

// HeldIssues is, for each issue among issues that a blocker holds where it stands, what holds it
// (Holds), by issue key — what the waiting line takes as record.LineFacts.Held: the line must
// not offer a slot to an issue whose first phase an unfinished Dispatch issue holds, and it
// cannot judge that itself, since internal/record may not read this package. An issue nothing
// holds is left out of the map rather than carried with an empty list, so a reader that asks
// only whether the key is there asks the question the line asks. Every caller that reads the
// line therefore loads the mirror rows first (Load over Judged with no line) and fills the field
// from here, which JudgeLine does for all of them, so the line, the promotion's own skip, the
// state's `blocked` and the word the held list gives such an issue
// (record.HeldDispatchBlocker) all answer from one judgement of one read.
func HeldIssues(rows map[string]Row, issues []record.Issue) map[string][]string {
	held := make(map[string][]string)
	for _, issue := range issues {
		if holds := Holds(rows, issue); len(holds) > 0 {
			held[issue.Key] = holds
		}
	}
	return held
}

// startedIssues is the set of issues among issues the daemon has started, by key: one holding a
// slot row now, one whose row the daemon has already taken away because it waits
// (record.Issue.ReleasedForWaitAt), and one that owes its next worker a start
// (record.LineFacts.Owed). An owed start is written by a transition of a run, or by
// SendOwedEntry (admit, catch-up, resume, silence), and each of those acts only on an issue that
// already has a claim or a phases row. The three are one question — has this issue's work begun
// — asked of the three states a started issue can be in, and the mark is why the second is told
// at all: a released issue is told from a child at admitted that never started by nothing else.
func startedIssues(issues []record.Issue, slots []record.Slot, facts record.LineFacts) map[string]struct{} {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	started := make(map[string]struct{}, len(slots))
	for _, issue := range issues {
		_, holds := slotted[issue.Key]
		_, owes := facts.Owed[issue.Key]
		if holds || owes || issue.ReleasedForWaitAt != nil {
			started[issue.Key] = struct{}{}
		}
	}
	return started
}

// waitsOf is what an issue the daemon has started waits on. Its one caller, startedWaits, asks
// it only of started issues (startedIssues): an issue holding no admission slot, carrying no mark
// and owing no start has not begun, and what it waits on is the line's own question, judged by
// the rule of its kind (LineWaitsOn).
//
// Two waits count. The Dispatch issues it is blocked by that are not done (Unfinished, which
// counts a blocker the daemon has no row of as not done), and the open children its own first
// phase waits for (record.LineFacts.ChildWaits, in that judgement's order), each named once. The
// second is the same judgement the line holds an unstarted parent by: a started parent waits for
// its children exactly as one that never started does, and only the slot it holds differs.
//
// Three started issues wait on neither, though a rule names them. A root waiting on a person
// (awaiting_person, answered or not) is waiting on that person and nothing else, and the slot it
// holds is what its resume takes back. A tree's own root whose design gate has never opened
// is its architect writing the design, which is the whole of that tree's work (record.AtWork at
// phase.Admitted): releasing it would take the slot of the one issue of the tree that is working.
// And a root that arrived from another tree carrying the row it held there
// (record.QueuedRoot) keeps that row: it stands in the line as record.LineQueuedRoot, whose own
// rule exempts a root carrying a run (LineWaitsOn, WaitsOn, Exempt), so a release here would be
// undone by the promotion of the same pass — and under a full cap not undone at all, leaving a
// root with the row the line promises it keeps taken away for good. The release and the
// promotion must read one judgement of such a root, and this is where the release reads it.
// Exempt is not read here: that rule is the promotion's, about what a candidate may start in
// spite of its links, and this one is about what a started issue holds a slot for, so an issue
// carrying a run gives its row back like any other.
func waitsOf(rows map[string]Row, issue record.Issue, facts record.LineFacts) []string {
	if issue.AwaitingPerson() {
		return nil
	}
	if issue.Phase == phase.Admitted && record.AtWork(issue, facts.Gates[issue.Tree]) {
		return nil
	}
	if record.QueuedRoot(issue, facts.Gates[issue.Tree]) {
		return nil
	}
	waits := Unfinished(rows, issue.Key)
	for _, child := range facts.ChildWaits[issue.Key].Open {
		if !slices.Contains(waits, child.Key) {
			waits = append(waits, child.Key)
		}
	}
	return waits
}

// startedWaits is what each started issue among issues waits on (waitsOf), by key, with no entry
// for an issue that waits on nothing and none for an issue the daemon has not started: what the
// waiting line takes as record.LineFacts.Waits, filled by the caller for the reason HeldIssues
// gives. started is the caller's own judgement of which issues the daemon has started
// (startedIssues), taken as a parameter because the caller judges it for the line as well and
// the two must be one judgement of one read.
func startedWaits(rows map[string]Row, issues []record.Issue, started map[string]struct{}, facts record.LineFacts) map[string][]string {
	waits := make(map[string][]string)
	for _, issue := range issues {
		if _, begun := started[issue.Key]; !begun {
			continue
		}
		if what := waitsOf(rows, issue, facts); len(what) > 0 {
			waits[issue.Key] = what
		}
	}
	return waits
}

// JudgeLine reads what the issues of project wait on and builds the waiting line from it, in the
// one order the two allow. First the mirror rows of every issue a blocker can hold (Judged with
// no line) and of every issue the daemon has started, because the line must leave out every
// issue whose first phase one of them holds and cannot judge that itself: internal/record may
// not read this package, so the hold is handed to the line as a fact (record.LineFacts.Held,
// HeldIssues). The children-first wait is handed to it the same way and for a like reason — the
// line is given facts rather than a transaction, and that judgement reads the Dispatch mirror
// against the daemon's own records (record.LineFacts.ChildWaits, record.ChildWaitsOf) — and is
// read again on each judgement rather than stored anywhere, so a mirror row that has gone stale
// holds a parent for one pass and no longer. The waits of the issues the daemon has started are
// judged there too (record.LineFacts.Waits, startedWaits), from those same rows and child waits,
// because the line stands a started issue that waits in its first part and must be told which
// issues those are; the set of started issues (startedIssues) is judged once here and given to
// both readings of it. Then the line. Then the rows of the issues the line itself names, which
// only the line can say.
//
// rows is whatever the caller has already read, nil on its first judgement; loadMissing reads
// only the keys it does not hold, so a pass that reads the line twice makes one mirror read in
// all. The facts come back with the held set, the child waits and the started issues' waits
// filled, so the release, the promotion, the architect's notice and the state's `blocked` all
// answer from the one judgement.
// slots is the caller's own read of the slots table, which record.Waiting reads as the set of
// issues already holding one.
//
// The held rows are what the line left behind, with the word for why (record.Waiting's second
// answer, which C2 of dispatch://INFRA-3107 asks the pass for, so that a reader cannot take a
// candidate it left behind for a placed one). The pass builds a row at each of its fourteen
// drop sites on every judgement, whoever asked for the line: the state and the controller's
// tick read them, and so does the admission pass's stalled-tree measure (admit's
// tellStalledTrees), which takes the words of this judgement rather than walking the project's
// issues again; every other caller takes the value and ignores it. now is the caller's
// own clock, and the one thing it decides is one of those words: the tree_closed word asks
// whether a root's linger is still running at it (record.Lingering), so that a parked root's
// member reads tree_parked once that root's linger has run out. It reaches the judgement as
// record.LineFacts.Now, which the line's own placement reads nowhere.
//
// It is the one place the line is judged: admission's own pass (admit's promoteHolds), the
// projected state and the controller tick's read of what the project waits on all come through
// here, so no two of them can judge the line from different reads or leave a later hold out of
// one of the three.
func JudgeLine(ctx context.Context, tx pgx.Tx, store record.Store, project string, issues []record.Issue,
	slots []record.Slot, facts record.LineFacts, rows map[string]Row, now time.Time,
) (map[string]Row, record.LineFacts, []record.LineEntry, []record.HeldIssue, error) {
	facts.Now = now
	if rows == nil {
		rows = map[string]Row{}
	}
	started := startedIssues(issues, slots, facts)
	judged := Judged(issues, nil)
	for _, issue := range issues {
		if _, begun := started[issue.Key]; begun {
			judged = append(judged, issue.Key)
		}
	}
	rows, err := loadMissing(ctx, tx, store, project, judged, rows)
	if err != nil {
		return rows, facts, nil, nil, err
	}
	facts.Held = HeldIssues(rows, issues)
	if facts.ChildWaits, err = record.ChildWaitsOf(ctx, tx, store, project, issues, facts); err != nil {
		return rows, facts, nil, nil, fmt.Errorf("read what the unstarted issues of %s wait on under them: %w", project, err)
	}
	facts.Waits = startedWaits(rows, issues, started, facts)
	line, held := record.Waiting(issues, slots, facts)
	rows, err = loadMissing(ctx, tx, store, project, Judged(issues, record.WaitingIssues(line)), rows)
	return rows, facts, line, held, err
}

// loadMissing adds to rows the mirror rows of the keys among keys whose row it does not already
// hold. The rows already read stand — no Dispatch issue finishes inside one transaction — so
// only a key read by no earlier judgement costs anything, and a call naming none costs nothing.
func loadMissing(ctx context.Context, tx pgx.Tx, store record.Store, project string, keys []string,
	rows map[string]Row,
) (map[string]Row, error) {
	var fresh []string
	for _, key := range keys {
		if _, known := rows[key]; !known && !slices.Contains(fresh, key) {
			fresh = append(fresh, key)
		}
	}
	if len(fresh) == 0 {
		return rows, nil
	}
	more, err := Load(ctx, tx, store, project, fresh)
	if err != nil {
		return rows, err
	}
	maps.Copy(rows, more)
	return rows, nil
}
