package record

import (
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// Unplaced is every issue the pass's records call admitted that the pass has placed nowhere — it
// holds none of the cap's slots and the waiting line does not carry it — with the issue that must
// move before it can run. It is the half of the daemon's account of its own admitted issues that
// was missing: `active`, `waiting` and `blocked` are read as a partition of them, and without this
// they are not one.
//
// The line decides this already, and decides it silently. Waiting drops a candidate at admitted
// where its tree is not live, where its tree's design gate is not open, where its tree's root
// waits on a person, where its Dispatch chain reaches another live root (the ancestry hold) and
// where an ancestor the daemon has accepted holds it; of those five, only the last is named
// anywhere, and only when a Dispatch link or an open child names it too (blockers.Blocked, which
// reads the mirror's links and the children-first wait and nothing else). So an issue any of them
// stops stands in the daemon's own records at admitted, in no list a reader can see, and no
// later pass looks at it again — the state dispatch://INFRA-2964 reported, of nineteen issues at
// once. This is read over the same records, slots, line and facts the line itself was read over,
// in the same transaction, so what it names and what the line carries cannot disagree.
//
// An issue another decider has already named is left to it: one a Dispatch blocker holds where
// it stands (LineFacts.Held, rendered by blockers.Holds) and one whose own children go first
// (LineFacts.ChildWaits, rendered by ChildWaitRows). This answers only what nothing else does.
//
// What holds the issue is named as one issue key, the first of these that answers:
//
//   - the nearest ancestor the daemon has already accepted (AcceptedAncestor), which holds every
//     issue beneath it at admitted until it closes (HeldUnderAccepted);
//   - the live root its Dispatch chain reaches, for a root the ancestry hold keeps out of the
//     line: it is that tree's unreleased work, and that tree's architect releases it;
//   - its own tree's root, for a member of a tree the line will not start: the root's promotion,
//     the approval of its design gate, a person's answer to it, the hand-over its close owes this
//     member, or its own re-admission is the one move that places this issue, and each of those
//     is that root's.
//
// A tree's own root that none of those names is left out, and that is the one case: a root a
// person has taken the legion label off. Legion runs nothing for it and starts nothing for it,
// which is what taking the label off asks for — the pause the label ends, as the net for a root
// nothing would ever start reads it too (admit's unslotted). Naming it here would report a pause
// as a wait on an issue, and there is no issue to name.
func Unplaced(issues []Issue, slots []Slot, line []LineEntry, facts LineFacts) []BlockedIssue {
	placed := make(map[string]struct{}, len(slots)+len(line))
	for _, slot := range slots {
		placed[slot.Issue] = struct{}{}
	}
	for _, entry := range line {
		placed[entry.Issue.Key] = struct{}{}
	}
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	// The same two reads the line makes of these records: the parent chain walked over the
	// records above the pass's own, so a parent already done is a gap in the chain rather than
	// the end of it (chainOf), and the set of live trees the ancestry hold is read against.
	chain := chainOf(byKey, facts.Parents)
	live := LiveRoots(issues, facts)

	// Nothing is reserved: a pass that places every issue it reads answers nothing at all, which
	// is the ordinary case, and a reservation the size of the project's records would be made on
	// every read of the state.
	var rows []BlockedIssue
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || !InWorkflow(issue) {
			continue
		}
		if _, held := placed[issue.Key]; held {
			continue
		}
		// An issue a Dispatch blocker holds where it stands is named already, by the decider
		// that holds it (blockers.Holds, the judgement LineFacts.Held carries), and so is a
		// parent whose own children go first (ChildWaitRows). Naming this issue's tree root
		// beside either would put a second holder on an issue whose wait the state answers.
		if _, blocked := facts.Held[issue.Key]; blocked {
			continue
		}
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			continue
		}
		root := claim.IsTreeRoot(issue.Key, issue.Tree)
		if root && !issue.HandedOver {
			continue
		}
		by := AcceptedAncestor(issue, chain)
		if by == "" {
			by = liveAncestor(issue, live)
		}
		if by == "" && !root {
			if _, recorded := byKey[issue.Tree]; recorded {
				by = issue.Tree
			}
		}
		if by == "" {
			continue
		}
		rows = append(rows, BlockedIssue{Issue: issue.Key, BlockedBy: []string{by}})
	}
	slices.SortFunc(rows, func(a, b BlockedIssue) int { return strings.Compare(a.Issue, b.Issue) })
	return rows
}

// liveAncestor is the live root issue's recorded Dispatch chain reaches, "" for a chain that
// reaches none: the one the ancestry hold keeps a root out of the line for (Waiting), read here
// over the same set of live trees so the hold and its name are one judgement. A root records a
// chain only while it has never run, so this answers nothing for any other record.
func liveAncestor(issue Issue, live map[string]struct{}) string {
	for _, key := range issue.Ancestry {
		if key == issue.Key {
			continue
		}
		if _, under := live[key]; under {
			return key
		}
	}
	return ""
}
