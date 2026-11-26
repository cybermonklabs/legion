package record

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// RestingRoot is a root whose design-gate step rests on an ancestor's approval in its generation
// (dispatch://INFRA-3320 R8) and that is still in the workflow, as the ancestor's sign-off and
// close refusals, the hold of its retro, its start task at its admission, its architect's catch-up
// and the held word of a parent held before its retro name it (workflow's acceptanceHeld,
// HeldReasonOf): its key, the phase it stands in, and its steps that rest on the ancestor.
type RestingRoot struct {
	Key   string
	Phase phase.Phase
	Steps []DesignStep
}

// RestingRoots is each root of every tree whose step rests on ancestor's approval in its
// generation (Store.GatesRestingOn: a basis on ancestor opened the step, or the step's basis names
// ancestor now, standing or pending, on the root's own row or on the row a hand-over carried to a
// child) and that is still in the workflow, by the predicate the sign-off's hold on the issues
// beneath it reads of each of them (InWorkflow), in key order. A root that is done, on the
// backlog, in icebox or triage, or lingering holds nothing. Its record is read by key, whatever
// project or tree it is in.
func RestingRoots(ctx context.Context, tx pgx.Tx, store Store, ancestor string) ([]RestingRoot, error) {
	gates, err := store.GatesRestingOn(ctx, tx, ancestor)
	if err != nil {
		return nil, err
	}
	return restingRoots(gates, func(key string) (*Issue, error) { return store.Issue(ctx, tx, key) })
}

// restingRoots is the roots of gates, the gates that rest on one ancestor in issue and then step
// order, that are still in the workflow, each with its steps that rest (RestingRoots). rootOf is
// each root's record by key, nil for a key with no record, asked once for each root's run of
// gates.
func restingRoots(gates []DesignGate, rootOf func(key string) (*Issue, error)) ([]RestingRoot, error) {
	var roots []RestingRoot
	read, live := "", false
	for _, gate := range gates {
		if gate.Issue != read {
			read = gate.Issue
			root, err := rootOf(gate.Issue)
			if err != nil {
				return nil, err
			}
			if live = root != nil && InWorkflow(*root); live {
				roots = append(roots, RestingRoot{Key: root.Key, Phase: root.Phase})
			}
		}
		if live {
			roots[len(roots)-1].Steps = append(roots[len(roots)-1].Steps, gate.Step.OrSpec())
		}
	}
	return roots, nil
}

// RestingRootsOn is RestingRoots for each of ancestors, by ancestor key, with no entry for an
// ancestor no root still in the workflow rests on, in two reads whatever their number: the gates
// resting on any of them (Store.GatesRestingOn) and the record of every root those gates name
// (Store.IssuesByKey), each root read once however many of ancestors it rests on. No ancestors
// read nothing.
func RestingRootsOn(ctx context.Context, tx pgx.Tx, store Store, ancestors []string) (map[string][]RestingRoot, error) {
	if len(ancestors) == 0 {
		return nil, nil
	}
	gates, err := store.GatesRestingOn(ctx, tx, ancestors...)
	if err != nil || len(gates) == 0 {
		return nil, err
	}
	keys := make([]string, 0, len(gates))
	for _, gate := range gates {
		keys = append(keys, gate.Issue)
	}
	// The gates come in issue order, so each root's key is a run that Compact leaves once.
	rows, err := store.IssuesByKey(ctx, tx, slices.Compact(keys))
	if err != nil {
		return nil, err
	}
	records := make(map[string]Issue, len(rows))
	for _, row := range rows {
		records[row.Key] = row
	}
	rootOf := func(key string) (*Issue, error) {
		if root, recorded := records[key]; recorded {
			return &root, nil
		}
		return nil, nil
	}
	resting := make(map[string][]RestingRoot, len(ancestors))
	for _, ancestor := range ancestors {
		on := slices.DeleteFunc(slices.Clone(gates), func(gate DesignGate) bool { return !gate.RestsOnIssue(ancestor) })
		roots, err := restingRoots(on, rootOf)
		if err != nil {
			return nil, err
		}
		if len(roots) > 0 {
			resting[ancestor] = roots
		}
	}
	return resting, nil
}
