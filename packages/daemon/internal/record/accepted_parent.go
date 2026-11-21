package record

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
)

// OpenBeneath is the sorted keys of the issues in Legion's work whose Dispatch chains pass
// through key (Chains.Under), and nothing at all when none is. It is the one set the two
// acceptance rules read: an architect's sign-off of a reviewed head (signOff) and its close of an
// issue that never started a phase (closeRoot) are both refused while it is not empty, and an
// issue whose pull request merged waits in merging until it empties (closeMerged). A parent is
// accepted once its checks pass on a base commit that carries its children's merges, so a parent
// that is accepted or closed first would merge with no check of it run on its children's work.
//
// One kind of issue beneath is left out: one the daemon is itself holding at phase.Admitted,
// because an issue above it has already been accepted (HeldUnderAccepted). Without that exclusion
// the three rules deadlock. An issue held at admitted under an issue in merging is in Legion's
// work, so it keeps that issue open through this set for ever, while the held issue itself is
// pinned at admitted until the issue above it closes; an issue held under an ancestor above an
// issue in accepting deadlocks that issue's sign-off the same way. Excluding exactly the issues
// the daemon's own hold has stopped costs nothing else: every other issue beneath still refuses
// the sign-off and still holds the merged issue open, and the held issue still holds every
// ancestor that has not been accepted, because the hold is what the exclusion is read from. This
// is the one recorded departure from the spec of dispatch://INFRA-980, decided by that tree's
// architect (dispatch://INFRA-980/comment/d642164a-31d2-4d6c-ae5e-4da5dd0b9104).
func OpenBeneath(chains *Chains, key string) []string {
	open := []string{}
	for _, under := range chains.Under(key) {
		if issue, _ := chains.Record(under); HeldUnderAccepted(chains, issue) {
			continue
		}
		open = append(open, under)
	}
	slices.Sort(open)
	return open
}

// AcceptedAncestorOf is the issue above key in Legion's work that the daemon has already accepted
// (AcceptedAncestor), and "" when none has been. It reads the chains of project with key and
// parent named (ReadChains), three queries, and it is what the workflow asks when an issue it
// records enters the workflow beneath such an issue (engine's enterChild), which is the one caller
// that holds no read of the chains already. The two doors into the workflow — release_children
// and rerun_child, which refuse CHILD_UNDER_ACCEPTED_PARENT — read the chains once for the whole
// request and ask AcceptedAncestorIn of them instead.
//
// parent is the Dispatch parent the caller read for an issue the copy may hold no row of yet,
// which is every child release_children releases before the reading of it reaches the copy: the
// chain starts there instead, so the rule reaches such a child exactly as it reaches one the copy
// holds.
func AcceptedAncestorOf(ctx context.Context, store Store, tx pgx.Tx, project, key, parent string) (string, error) {
	named := []string{key}
	if parent != "" {
		named = append(named, parent)
	}
	chains, err := ReadChains(ctx, tx, store, project, named)
	if err != nil {
		return "", err
	}
	return AcceptedAncestorIn(chains, key, parent), nil
}

// AcceptedAncestorIn is AcceptedAncestorOf over chains the caller has already read, with key and
// parent named: a handler that reads the chains for a request of its own asks this of each child
// it answers for, rather than reading them again per child (api's release_children, which answers
// a whole wave, and its rerun_child). Where the copy holds no row of key and the caller read its
// parent, the walk starts at that parent: the parent itself where it is in Legion's work and
// accepted, else the issue above it that is.
func AcceptedAncestorIn(chains *Chains, key, parent string) string {
	if chain := chains.Chain(key); parent == "" || chain.Cause != ChainMissingRow || chain.At != key {
		return AcceptedAncestor(chains, key)
	}
	if issue, recorded := chains.Record(parent); recorded && InLegionsWork(issue) && Accepted(issue) {
		return parent
	}
	return AcceptedAncestor(chains, parent)
}
