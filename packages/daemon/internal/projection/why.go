// Package projection: this file holds the one rendered answer an issue that is not working now
// carries on its own view (api.Issue.Why) — the four states, the sentence of each fill they can
// be read from, and the list render they share. Project calls whyStates once the line and
// `blocked` are both in hand (project.go), and why_state_test.go beside it reads the answers off
// a projected state. The waiting line's own sentence is the file next to it
// (waiting_line_reason.go), which a member of the line carries here as well.
package projection

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
	// the two lists differently.
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
			// The links this pass judged, read once for the two fills that name them: the
			// children fill names them after the children it waits for, and the link fill is
			// the whole of them.
			links := blockers.Holds(rows, issue)
			why = &api.WhyView{State: api.WhyBlocked,
				Reason: blockedReason(cycles[issue.Key], childWaits[issue.Key], links)}
		default:
			if _, placed := active[issue.Key]; placed {
				continue
			}
			why = &api.WhyView{State: api.WhyUnlisted, Reason: unlistedReason(issue, causes[issue.Key])}
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
		children := childrenReason(open)
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

// childrenReason is the sentence that names the open Dispatch children an issue's own first
// phase waits for: the first sentence of the children fill of `blocked` (blockedReason), and
// the whole of the children_first fill of `unlisted` (unlistedReason), so the same children
// read one way wherever they are rendered. An empty list renders nothing, as linkReason's does.
func childrenReason(open []string) string {
	if len(open) == 0 {
		return ""
	}
	return fmt.Sprintf("Its own first phase waits for the open issues below it: %s.", englishIssues(open))
}

// unlistedReason is the sentence of an issue the record holds in the workflow that stands in
// none of the three lists that place one (api.WhyUnlisted). The word is `unlisted` and not
// `held`, because `held` is a phase of the workflow, whose issues hold a slot and stand in
// `admission.active`.
//
// Three fills, read in this order. An issue waiting for its merge waits for that and nothing
// else, which is the record's own at_work (record.HeldAtWork, the word of an issue that keeps
// no slot where it stands), so that word needs no case of its own below. It is read before the
// record's word rather than from it, because the ladder gives such an issue whatever earlier
// word is true of it as well — a root whose design gate never opened reads design_gate — and
// its merge is what it waits for whichever of them the row carries.
//
// Then the cause the record carries: one sentence per word of record.HeldReason, each true of
// what that word means, over the row record.HeldList published for the issue. The ladder that
// chooses the word is the authority on which issue reads which (record.HeldReasonOf), and each
// constant's own doc comment is the authority on what the word means, so a word added to that
// ladder is given its sentence here; given none, it reads the fallback, which says less than
// the row beside it knows. The two words whose issues this file already renders elsewhere read
// that render — the links of dispatch_blocker through linkReason, the open children of
// children_first through childrenReason — so one list of issues reads one way wherever it is
// served, and every other list of keys renders through englishIssues.
//
// And the fallback, for unclassified, the ladder's own last word, which says that no rule of
// the held list names what the issue waits for; a key the held list carries no row for at all
// reads it too (whyStates).
func unlistedReason(issue record.Issue, cause record.HeldIssue) string {
	if issue.Phase == phase.AwaitingMerge {
		return "It waits for its merge, and for nothing else."
	}
	// The two words that name issues name at least one, since the ladder reads each off a list
	// it found filled, and a row that named none would render nothing here rather than a
	// sentence naming none, which is the rule those two renders carry already.
	switch cause.Reason {
	case record.HeldTreeClosed:
		return fmt.Sprintf("Its tree is closed: %s has finished, or lingers after leaving the workflow, "+
			"and nothing of that tree runs again until a re-admission opens it.", englishIssues(cause.HeldBy))
	case record.HeldTreeParked:
		return fmt.Sprintf("A person parked its tree: %s stands in backlog or icebox, "+
			"and nothing of that tree runs again until a person puts that root back at todo.", englishIssues(cause.HeldBy))
	case record.HeldLabelOff:
		return "It is the root of its own tree and its record says the legion label is off, " +
			"so nothing hands it to the daemon and it waits for nothing."
	case record.HeldAncestry:
		return fmt.Sprintf("Its Dispatch chain reaches live work above it: %s. "+
			"It stands as that tree's unreleased work until its architect releases it.", englishIssues(cause.HeldBy))
	case record.HeldRootAwaitingPerson:
		return fmt.Sprintf("Its tree's root waits on a person: %s waits for an answer or for a review of its spec, "+
			"and the issues of the tree start when that root resumes.", englishIssues(cause.HeldBy))
	case record.HeldDesignGate:
		return fmt.Sprintf("The design gate of its tree stands shut: %s waits for a person's approval of each gated "+
			"step's document at its current version, and nothing of that tree starts until it opens.", englishIssues(cause.HeldBy))
	case record.HeldDispatchBlocker:
		return linkReason(cause.HeldBy)
	case record.HeldChildrenFirst:
		return childrenReason(cause.HeldBy)
	case record.HeldAcceptedAncestor:
		return fmt.Sprintf("The daemon has accepted the reviewed head of %s, which stands above it, "+
			"so its own work starts when that issue closes.", englishIssues(cause.HeldBy))
	}
	return "The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, " +
		"so no slot has been offered to it. No rule of the held list says what it waits for."
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
