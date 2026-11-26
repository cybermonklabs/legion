package blockers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ReadFanout is how many blocker keys one controller tick reads one at a time: up to this many are
// read singly, and above it the whole project is listed once instead (ReadPlan). The listing is a
// single unpaged request and Dispatch caps an issue at 20 blockers, so the fan-out a waiting set
// can ask for is bounded on both sides of the choice.
const ReadFanout = 25

// WaitedOn is the distinct keys of everything project's daemon holds something back for, read in
// tx, and the parents whose Dispatch children a tick reads as well. The keys are the judgement the
// deciders themselves make, read rather than remade (Blocked), over every root on the waiting line
// (record.Waiting), every recorded child a blocker holds at phase.Admitted, every open Dispatch
// child holding a parent's own first phase (record.ChildWaitsOf), and every open child a parent
// waiting in awaiting_children waits on (ChildRows), judged under rule with the deployment's gated
// steps. A candidate the rule exempts is read for nothing, as promotion admits it whatever its
// links say and nothing consults the answer (Exempt), and the keys come out in that judgement's
// own order, by the issue waiting on them. Then come the open children of every other started
// parent (record.LineFacts.StartedParentWaits), which the admission pass moves into
// awaiting_children or leaves where it stands from the same judgement, and of every parent held
// before its retro (record.RetroHolds.Open, in record.LineFacts.RetroHolds), whose retro the
// admission pass starts once none of them is open (admit's childrenWaits). Both kinds of parent
// are sorted together by key, and each parent's open children are added in that order, a key
// already added keeping the place it took first.
//
// An open child is in the set because the tick's re-read is what writes its row of the copy
// again: a reading of all five values, each of which replaces the one an older reading wrote. A
// child the architect filed and has not released has no event the daemon consumes — it reaches
// the copy from a reading of Dispatch alone — so without this its row would stand until the next
// boot, and its move to icebox, to another parent or out from under its parent would not end the
// wait it holds.
//
// parents is every issue whose phases have started (record.SlotRule.StartedParents under rule):
// one in planning through accepting, held from one of them, owing its worker a start, or waiting
// in awaiting_children (the spec of #4592, R13), and every parent held before its retro (R8),
// which an open Dispatch child holds as well: the keys of record.LineFacts.RetroHolds, which holds
// an entry for each such parent (record.RetroHeldParents) whatever holds it. A child filed under
// such an issue is in no mirror row until a listing shows it, and the admission pass holds the
// issue's next start, stops the phase under way, or holds its retro, the moment the mirror does,
// so the tick reads the children of each of them whatever the number of keys (ReadPlan). Under a
// rule whose wait stops no phase the started parents are none (record.SlotRule.StartedParents),
// so only the parents held before their retro are read.
//
// It is the one selection of what a tick re-reads, which the daemon's tick (internal/daemon's
// waitedOn) and a replay of a recorded window (internal/replay, which builds the tick's
// observation from its copy of Dispatch) both call, so the two read the same keys and children.
// The judgement drops the held rows, so now decides nothing here; it is the caller's own clock.
func WaitedOn(ctx context.Context, tx pgx.Tx, store record.Store, project string, steps []record.DesignStep, rule record.SlotRule,
	now time.Time,
) (keys, parents []string, err error) {
	issues, err := store.LiveIssues(ctx, tx, project)
	if err != nil {
		return nil, nil, fmt.Errorf("list the issues of %s: %w", project, err)
	}
	slots, err := store.Slots(ctx, tx)
	if err != nil {
		return nil, nil, fmt.Errorf("list the admission slots: %w", err)
	}
	facts, err := record.ReadLineFactsUnder(ctx, tx, store, project, issues, steps, rule)
	if err != nil {
		return nil, nil, err
	}
	// Judged through the one judgement admission itself promotes from (JudgeLine): the rows of
	// every issue a blocker can hold come before the line, which leaves such an issue out and
	// cannot judge that itself (record.LineFacts.Held); the issues the line names are read after
	// it, since only the line says which they are.
	rows, facts, line, _, err := JudgeLine(ctx, tx, store, project, issues, slots, facts, nil, now)
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[string]struct{})
	add := func(key string) {
		if _, repeated := seen[key]; repeated {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, blocked := range Blocked(rows, issues, line, facts) {
		for _, blocker := range blocked.BlockedBy {
			add(blocker)
		}
	}
	parents = slices.AppendSeq(facts.Rule.OrMain().StartedParents(issues, facts.Owed), maps.Keys(facts.RetroHolds))
	slices.Sort(parents)
	parents = slices.Compact(parents)
	for _, parent := range parents {
		for _, child := range facts.StartedParentWaits[parent].Open {
			add(child.Key)
		}
		for _, child := range facts.RetroHolds[parent].Open {
			add(child)
		}
	}
	return keys, parents, nil
}

// ReadPlan is how a tick reads keys and the children of parents, as WaitedOn names them, for
// project. Above ReadFanout keys it is one whole-project listing whose every summary is taken
// (listProject and all), and the keys of another project read one at a time (single), which the
// project's listing does not hold. At or below it, where no parent stands, every key is read one
// at a time and nothing is listed. At or below it, where some parent stands, the keys of another
// project are read one at a time, and the project is listed once and the summaries taken from it
// are the ones Taken names (listProject and not all): the listing is the one read of Dispatch that
// names an issue's children (dispatch.IssueSummary.Parent), and it already holds every key of this
// project with the fields a single read gives (Observed), so no key of this project is read singly
// as well. The daemon's tick and a replay's both read by it.
func ReadPlan(project string, keys, parents []string) (listProject, all bool, single []string) {
	switch {
	case len(keys) > ReadFanout:
		return true, true, foreignKeys(project, keys)
	case len(parents) == 0:
		return false, false, keys
	default:
		return true, false, foreignKeys(project, keys)
	}
}

// Taken says whether a plan that takes the project's listing in part (ReadPlan) takes summary: the
// summary of a key of project the tick waits on (keys), or of a child of one of parents, which
// comes sorted (WaitedOn), so each summary's parent is one search.
func Taken(project string, summary dispatch.IssueSummary, keys, parents []string) bool {
	if OwnKey(project, summary.Key) && slices.Contains(keys, summary.Key) {
		return true
	}
	if summary.Parent == nil {
		return false
	}
	_, child := slices.BinarySearch(parents, *summary.Parent)
	return child
}

// OwnKey says whether key is an issue of project, by its key's prefix: the one reading of which
// keys a project's listing holds, so the keys a tick reads singly are the keys it does not.
func OwnKey(project, key string) bool {
	keyProject, _, _ := strings.Cut(key, "-")
	return keyProject == project
}

// foreignKeys is the keys of keys that are not of project (OwnKey), in their order.
func foreignKeys(project string, keys []string) []string {
	var foreign []string
	for _, key := range keys {
		if !OwnKey(project, key) {
			foreign = append(foreign, key)
		}
	}
	return foreign
}
