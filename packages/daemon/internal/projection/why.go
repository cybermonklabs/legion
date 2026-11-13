// Package projection: this file holds the one rendered answer an issue that is not working now
// carries on its own view (api.Issue.Why) — the four states, the sentence of each fill they can
// be read from, and the list render they share. Project calls whyStates once the line and
// `blocked` are both in hand (project.go), and why_state_test.go beside it reads the answers off
// a projected state. The waiting line's own sentence is the file next to it
// (waiting_line_reason.go), which a member of the line carries here as well.
package projection

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// whyStates fills the one rendered answer an issue that is not working now carries on its own
// view (api.Issue.Why): the state word, and the sentence that says why, from whichever fact
// holds the issue. state is the projection so far, whose `admission.active`,
// `admission.waiting` and `admission.blocked` are all filled and whose members of the line
// carry the sentence already rendered for them (api.WaitingLineView.Reason); own are this
// project's own records; stranded are the claims the daemon judges nothing will wake, handed to
// Project because this package may not read internal/workflow; rows is the Dispatch mirror this
// pass judged the links from and childWaits the children-first judgement it made
// (record.ChildWaitsOf).
//
// The four states are read in one order, so no issue carries two answers: claim_stopped, then
// waiting, then blocked, then unlisted. claim_stopped is first because its fact is about the
// issue's worker rather than its place in a queue, and an issue at work whose worker stopped is
// the commonest question a reader brings. An issue that matches none of the four carries no
// answer at all: an issue of `admission.active` whose worker is running, and an issue at
// phase.Held, which holds a slot and so stands in that same list.
//
// A blocked issue and an unlisted one may read the sentence of the held word the record carries
// for it; heldSentences states the rule, which covers the order of the blocked arm's three
// sentences, the words each arm serves and what every such sentence may say.
//
// The scope is this project's own records that the record holds in the workflow
// (record.InWorkflow). An issue the record does not hold there — done, backlog, icebox,
// triage — carries no answer, an issue of another project carries none, since admission is
// judged over this project's records alone, and an issue the workflow does not record at all,
// an operator's claim, is in no record of own and so carries none either.
//
// The three blocked fills read the links and the children-first waits from the two judgements
// this pass holds apart, and never from state.Admission.Blocked: that list merges both into one
// record.BlockedIssue apiece (blockers.Blocked), so a reader of it cannot tell a Dispatch link
// from an open child, and the fills need them apart. The published list is read for the two
// things only it says: which issues something holds, which is the `blocked` state's own
// membership, and what a member of the line a link holds waits on where the links half names
// nothing for it, the one fallback in this file (the waiting arm below).
func whyStates(state *api.State, own []record.Issue, stranded []supervise.Claim, rows map[string]blockers.Row, childWaits map[string]record.ChildWait) {
	// The stranded claim each issue's answer names. The set reports two kinds of claim and no
	// others (workflow.Stranded) — the worker of the issue's phase, and the tree's architect —
	// so where one issue has both, the worker is the one named: its stop is the one that holds
	// this issue's own phase, and the architect's is the whole tree's.
	stoppedAt := make(map[string]supervise.Claim, len(stranded))
	for _, stopped := range stranded {
		if named, known := stoppedAt[stopped.Issue]; known && named.Role != claim.RoleArchitect {
			continue
		}
		stoppedAt[stopped.Issue] = stopped
	}
	// The three lists that place an issue: the line and the blocked list each answer a state of
	// their own, read from their own set below, and `active` is read for `unlisted` alone, which
	// is the issue in none of the three. `admission.trees` is not among them, because it names
	// the roots of live trees rather than placing an issue, so a root stands here on the same
	// terms as a child.
	inLine := make(map[string]struct{}, len(state.Admission.Waiting))
	for _, key := range state.Admission.Waiting {
		inLine[key] = struct{}{}
	}
	// Which issues something holds and what the published list says each of them waits on: the
	// membership the `blocked` state is read by, and the fallback of the clause a member of the
	// line a link holds carries, for an issue the links half names nothing for — a root of the
	// line whose wait blockers.LineWaitsOn judged.
	blocked := make(map[string][]string, len(state.Admission.Blocked))
	for _, row := range state.Admission.Blocked {
		blocked[row.Issue] = row.BlockedBy
	}
	// The issues holding a slot, which `unlisted` is the complement of together with the two
	// sets above. Only those two sets' own arms read them, so this holds the slots alone: the
	// arm that reads it is reached only by an issue neither of them names.
	active := make(map[string]struct{}, len(state.Admission.Active))
	for _, key := range state.Admission.Active {
		active[key] = struct{}{}
	}
	// Each cycle the children-first judgement named, by the child whose own blocked_by link
	// holds it under an ancestor (record.OpenChild.Cycle): the one fact of the three blocked
	// fills a reader can get nowhere else.
	cycles := make(map[string]string, len(childWaits))
	for _, wait := range childWaits {
		for _, child := range wait.Open {
			if child.Cycle != "" {
				cycles[child.Key] = child.Cycle
			}
		}
	}
	// The one word the record carries for each issue it holds in the workflow that stands in
	// neither the slots nor the line, with the issues that hold it (record.HeldList, read into
	// state.Admission.Held before this pass runs). Every issue the `unlisted` arm reaches has a
	// row here, because that list is built over the same two lists this arm is the complement
	// of, so an issue in none of them is an issue of the held list. A key the map does not hold
	// reads the zero row all the same, whose word is no word of the ladder and whose sentence
	// is the fallback: an answer a reader can act on beats none where a later pass publishes
	// the two lists differently. The `blocked` arm reads it too, by the rule heldSentences states.
	causes := make(map[string]record.HeldIssue, len(state.Admission.Held))
	for _, row := range state.Admission.Held {
		causes[row.Issue] = row
	}
	for _, issue := range own {
		view, projected := state.Issues[issue.Key]
		if !projected || !record.InWorkflow(issue) {
			continue
		}
		stopped, stoppedClaim := stoppedAt[issue.Key]
		_, waiting := inLine[issue.Key]
		_, held := blocked[issue.Key]
		var why *api.WhyView
		switch {
		case stoppedClaim:
			why = &api.WhyView{State: api.WhyClaimStopped, Reason: claimStoppedReason(issue, stopped)}
		case waiting:
			// The member's own sentence, and one more clause where a link holds it as well as
			// the line. The clause is added here and never inside the line's own `reason`: the
			// two are separate fields, and only `why` carries it. Every member of
			// `admission.waiting` carries a view of the line with a sentence in it, since the
			// fill that listed it rendered one (Project, over record.LineEntry), so both are
			// read without a guard.
			reason := view.WaitingLine.Reason
			if held {
				// What this member waits on: the links this pass judged for it
				// (blockers.Holds), and the published row where that half names nothing — a
				// root of the line whose wait blockers.LineWaitsOn judged. The links are
				// judged for a member something holds and for no other, since a member
				// nothing holds reads no clause whatever they say.
				waitsOn := blockers.Holds(rows, issue)
				if len(waitsOn) == 0 {
					waitsOn = blocked[issue.Key]
				}
				if clause := linkReason(waitsOn); clause != "" {
					reason += " " + clause
				}
			}
			why = &api.WhyView{State: api.WhyWaiting, Reason: reason}
		case held:
			// The cycle first, then the held word's sentence where heldSentences serves the word
			// under `blocked`, then the links and children: heldSentences states the rule.
			cycle := cycles[issue.Key]
			if cycle == "" {
				if reason, serves := heldWordReason(api.WhyBlocked, causes[issue.Key]); serves {
					why = &api.WhyView{State: api.WhyBlocked, Reason: reason}
					break
				}
			}
			// The links this pass judged, read once for the two fills that name them: the
			// children fill names them after the children it waits for, and the link fill is
			// the whole of them.
			links := blockers.Holds(rows, issue)
			why = &api.WhyView{State: api.WhyBlocked, Reason: blockedReason(cycle, childWaits[issue.Key], links)}
		default:
			if _, placed := active[issue.Key]; placed {
				continue
			}
			reason, _ := heldWordReason(api.WhyUnlisted, causes[issue.Key])
			why = &api.WhyView{State: api.WhyUnlisted, Reason: reason}
		}
		// An issue whose sentence renders nothing is served no `why` at all, rather than a state
		// word with no reason beside it: it is the rule the line's own sentence is served by
		// (waitingLineReason), and the two answers a reader reads together stay together.
		if why.Reason == "" {
			continue
		}
		view.Why = why
		state.Issues[issue.Key] = view
	}
}

// claimStoppedReason is the sentence of an issue a stranded claim names (api.WhyClaimStopped):
// where the issue's work stands, which claim stopped and the state it stopped in, and that
// nothing will take it up again. The two fills are the two kinds of claim the stranded set
// reports — the worker of the issue's phase, named by its role, and the tree's architect, whose
// claim is the tree's rather than this issue's — and each names the state the set carries with
// the claim rather than one read of its own: the set is the daemon's judgement, and a claim at
// suspended, failed or retired that is not in it is waiting for a start the daemon has queued.
func claimStoppedReason(issue record.Issue, stopped supervise.Claim) string {
	if stopped.Role == claim.RoleArchitect {
		return fmt.Sprintf("Its phase is %s and its tree's architect claim is stranded at %s, so nothing will wake it.",
			issue.Phase, stopped.State)
	}
	return fmt.Sprintf("Its phase is %s and the %s's claim is stranded at %s, so nothing will wake it.",
		issue.Phase, stopped.Role, stopped.State)
}

// blockedReason is the sentence of an issue something holds where it stands (api.WhyBlocked),
// from the three judgements the state's `blocked` has merged by the time it is published: the
// cycle a child's own blocked_by link makes with an ancestor, the open children below an issue
// whose first phase waits for them, and the Dispatch links an issue waits on.
//
// The cycle is read first and alone, because a cycle is a link too and reading either of the
// others first would hide the one fact a reader cannot get anywhere else — that the issue it
// waits on is the issue waiting for it, and that only a person taking the link off ends it.
//
// The other two are two holds and not one, and an issue both of them hold reads both sentences,
// the children first because they are the issues below it and the links are not. Closing the
// children would free an issue a link holds as well no sooner than it frees nothing, which the
// children sentence alone tells a reader it would, and `admission.blocked` has named both all
// along (blockers.Blocked merges them into one row). links is what the links half judged for
// this issue (blockers.Holds) alone, and the published row is read nowhere here: where that
// half names nothing for an issue that list holds, the row came from the children-first
// judgement (record.ChildWaitRows), which is a parent whose open children the second arm
// answers or a cycle child the first one does, so the last line is reached with the links in
// hand.
func blockedReason(cycle string, wait record.ChildWait, links []string) string {
	if cycle != "" {
		return fmt.Sprintf("A blocked_by link holds it under %s, which stands above it in its own tree.", cycle)
	}
	if len(wait.Open) > 0 {
		open := make([]string, 0, len(wait.Open))
		for _, child := range wait.Open {
			open = append(open, child.Key)
		}
		children := fmt.Sprintf("Its own first phase waits for the open issues below it: %s.", englishIssues(open))
		if clause := linkReason(links); clause != "" {
			return children + " " + clause
		}
		return children
	}
	return linkReason(links)
}

// linkReason is the clause that names the Dispatch issues an issue waits on that are not done:
// the whole sentence of the link fill of `blocked`, and the last clause of a member of the line
// a link holds as well (whyStates). The relative clause agrees with the list, so one issue reads
// `which is not done`. An empty list renders nothing: an issue whose blockers this pass cannot
// name is served no sentence naming none.
func linkReason(waitsOn []string) string {
	switch len(waitsOn) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("It waits on %s, which is not done.", waitsOn[0])
	}
	return fmt.Sprintf("It waits on %s, which are not done.", englishIssues(waitsOn))
}

// heldSentences is the sentence table of `why`'s held words: for each word of record.HeldReason
// that `why` can serve, the states it serves the word under (states) and the sentence it serves
// (sentence), rendered over heldBy, the issues of the row record.HeldList published for the
// issue, joined by englishIssues. It is the one statement of the rule below; whyStates reads it
// in both arms that serve a held word, through heldWordReason.
//
// The arms. A blocked issue (api.WhyBlocked) reads one of three sentences, in this order. First,
// a child whose own blocked_by link holds it under an ancestor in its tree (a children-first
// cycle, record.OpenChild.Cycle) reads the cycle sentence alone (blockedReason), whatever its
// held word: nothing else in the state says the issue it waits on is waiting for it, and
// `admission.held` still names the held word beside it. Else, an issue whose word this table
// serves under `blocked` reads that sentence rather than the sentence of the links or children
// that hold it: closing those would not free it while that word stands, so `why` and
// `admission.held` name one cause. The words served there are the six the ladder reads before
// dispatch_blocker (record.HeldBefore), and a test holds the two lists equal. Else — the word is
// dispatch_blocker, children_first or a later one, or the issue has no held row — it reads
// blockedReason, whose own text names the links and children.
//
// An issue the record holds in the workflow that stands in none of the three lists that place
// one (api.WhyUnlisted) reads its word's sentence under `unlisted`. Every word but
// dispatch_blocker and children_first can reach that arm, and those two cannot, because every
// issue the ladder gives either of them stands in `admission.blocked`. The state word is
// `unlisted` and not `held`, because `held` is a phase of the workflow, whose issues hold a slot
// and stand in `admission.active`. at_work has its entry, though no issue reads that word today
// (record.HeldAtWork).
//
// The fallback is unclassified's sentence, the ladder's own last word, which says that no rule
// of the held list names what the issue waits for. An unlisted issue whose word the table lacks,
// or does not serve under `unlisted`, reads it, and so does an unlisted issue whose key the held
// list carries no row for at all. A word added to the ladder (record.HeldReasonOf) is given its
// entry here; given none, it reads the fallback, which says less than the row beside it knows.
//
// The sentence rule. Every sentence here says what is true of the issue now and what does not
// start it, and never promises what will start it: the ladder names the first of the words true
// of an issue, so the end of that one hold starts nothing where a later word holds it too. The
// ladder is the authority on which issue reads which word (record.HeldReasonOf), and each
// constant's own doc comment is the authority on what the word means.
var heldSentences = map[record.HeldReason]heldSentence{
	record.HeldTreeClosed: {states: []api.WhyState{api.WhyBlocked, api.WhyUnlisted}, sentence: func(heldBy string) string {
		return "Its tree is closed: " + heldBy + " has finished, or lingers after leaving the workflow, " +
			"and nothing of that tree runs again until a re-admission opens it."
	}},
	record.HeldTreeParked: {states: []api.WhyState{api.WhyBlocked, api.WhyUnlisted}, sentence: func(heldBy string) string {
		return "A person parked its tree: " + heldBy + " stands in backlog or icebox, " +
			"and nothing of that tree runs again until a person puts that root back at todo."
	}},
	record.HeldLabelOff: {states: []api.WhyState{api.WhyBlocked, api.WhyUnlisted}, sentence: func(string) string {
		return "It is the root of its own tree and its record says the legion label is off, " +
			"so nothing hands it to the daemon."
	}},
	record.HeldAncestry: {states: []api.WhyState{api.WhyBlocked, api.WhyUnlisted}, sentence: func(heldBy string) string {
		return "Its Dispatch chain reaches live work above it: " + heldBy + ". " +
			"It stands as that tree's unreleased work until its architect releases it."
	}},
	record.HeldRootAwaitingPerson: {states: []api.WhyState{api.WhyBlocked, api.WhyUnlisted}, sentence: func(heldBy string) string {
		return "Its tree's root waits on a person: " + heldBy + " waits for an answer or for a review of its spec, " +
			"and nothing of that tree starts before that root resumes."
	}},
	record.HeldDesignGate: {states: []api.WhyState{api.WhyBlocked, api.WhyUnlisted}, sentence: func(heldBy string) string {
		return "The design gate of its tree stands shut: " + heldBy + " waits for a person's approval of each gated " +
			"step's document at its current version, and nothing of that tree starts until it opens."
	}},
	record.HeldAcceptedAncestor: {states: []api.WhyState{api.WhyUnlisted}, sentence: func(heldBy string) string {
		return "The daemon has accepted the reviewed head of " + heldBy + ", which stands above it, " +
			"and nothing of its own work starts before that issue closes."
	}},
	record.HeldAtWork: {states: []api.WhyState{api.WhyUnlisted}, sentence: func(string) string {
		return "It stands in a phase that keeps no admission slot, and the waiting line does not carry it: " +
			"its workers run as they did, and no other issue holds it."
	}},
	record.HeldUnclassified: {states: []api.WhyState{api.WhyUnlisted}, sentence: func(string) string {
		return "The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, " +
			"so no slot has been offered to it. No rule of the held list says what it waits for."
	}},
}

// heldSentence is one entry of heldSentences: the states of `why` that serve the word's
// sentence, and the sentence over heldBy, the issues of the word's row already joined by
// englishIssues.
type heldSentence struct {
	states   []api.WhyState
	sentence func(heldBy string) string
}

// heldWordReason is the sentence heldSentences gives the word of cause under state, over the
// issues of cause's row, and whether the table serves that word under state. Where it does not,
// the sentence is the table's fallback, unclassified's, which the unlisted arm serves and the
// blocked arm passes over for blockedReason (heldSentences states the rule).
func heldWordReason(state api.WhyState, cause record.HeldIssue) (string, bool) {
	entry, listed := heldSentences[cause.Reason]
	serves := listed && slices.Contains(entry.states, state)
	if !serves {
		entry = heldSentences[record.HeldUnclassified]
	}
	return entry.sentence(englishIssues(cause.HeldBy)), serves
}

// englishIssues is a list of issue keys as a person writes it: `INFRA-123` alone,
// `INFRA-123 and INFRA-456` for two, and `INFRA-1, INFRA-2 and INFRA-3` for three or more, in
// the order the judgement that named them gave them. It is the one list render the sentences of
// `why` share (whyStates), so no two of them punctuate a list differently.
func englishIssues(keys []string) string {
	if len(keys) < 2 {
		return strings.Join(keys, "")
	}
	return strings.Join(keys[:len(keys)-1], ", ") + " and " + keys[len(keys)-1]
}
