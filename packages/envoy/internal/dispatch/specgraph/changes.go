package specgraph

import (
	"maps"
	"slices"
)

// Change is one statement that differs between two versions of the files, and what differs: each
// of "added", "removed", "text", "parent", "relations" and "checks" that applies, in that order.
type Change struct {
	ID   string
	What []string
}

// Changes is each statement whose text, parent, relations or checks differ between base and head,
// and each statement that only one of them holds, sorted by id (R4). It reads the nodes and never
// the files that hold them, so a statement that moves to another file is no change.
//
// "relations" is a new kind, serves or about. "checks" is a new set of digests of the checks that
// serve the statement: a change to any part of one of its checks, or a check that comes, goes or
// moves to another statement, changes its checks. A statement whose child changed is not changed
// itself. An id that two statements of one tree hold is read at its first statement, as
// StatementByID reads it.
func Changes(base, head Tree) []Change {
	before, after := statementsByID(base), statementsByID(head)
	beforeChecks, afterChecks := checkDigests(base), checkDigests(head)
	ids := maps.Clone(before)
	maps.Copy(ids, after)
	var changes []Change
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		b, inBase := before[id]
		h, inHead := after[id]
		var what []string
		switch {
		case !inBase:
			what = []string{"added"}
		case !inHead:
			what = []string{"removed"}
		default:
			if b.Text != h.Text {
				what = append(what, "text")
			}
			if b.Parent != h.Parent {
				what = append(what, "parent")
			}
			if b.Kind != h.Kind || recordList(b.Serves) != recordList(h.Serves) || recordList(b.About) != recordList(h.About) {
				what = append(what, "relations")
			}
			if !slices.Equal(beforeChecks[id], afterChecks[id]) {
				what = append(what, "checks")
			}
		}
		if len(what) > 0 {
			changes = append(changes, Change{ID: id, What: what})
		}
	}
	return changes
}

// statementsByID is the first statement of t with each id.
func statementsByID(t Tree) map[string]Statement {
	byID := make(map[string]Statement, len(t.Statements))
	for _, s := range t.Statements {
		if _, held := byID[s.ID]; !held {
			byID[s.ID] = s
		}
	}
	return byID
}

// checkDigests is the sorted digests of the checks of t that serve each statement.
func checkDigests(t Tree) map[string][]string {
	byStatement := map[string][]string{}
	for _, c := range t.Checks {
		byStatement[c.Serves] = append(byStatement[c.Serves], c.Digest())
	}
	for _, list := range byStatement {
		slices.Sort(list)
	}
	return byStatement
}
