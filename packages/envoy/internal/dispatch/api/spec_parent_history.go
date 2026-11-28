package api

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
)

// The parents that the issues' events record, which order the arrival of a definition in the list
// of terms of a root (definitionJoins in spec_terms.go).

// parentChange is one write of an issue's parent that the issue's events hold: the id of the
// event, and the parent the write left ("" for none).
type parentChange struct {
	event  int64
	parent string
}

// parentHistory is the parents of the issues it holds as their events wrote them, each issue's
// changes in the order of its events: the parent it was made with, then each parent a write set.
type parentHistory map[string][]parentChange

// parentHistoryQuery reads each change of the parent of each issue that $1 names from its
// issue.created, issue.updated and issue.closed events. Each of them holds the issue as the write
// left it, its parent among the fields, in every release that writes them, so the events are the
// one record of a move whichever build made it. Every write of parent_key appends one of them
// (issue_create.go, issue_patch.go), and the parent a write left alone is no change.
const parentHistoryQuery = `
	select issue_key, id, coalesce(parent, '') from (
		select issue_key, id, payload->>'parent' as parent,
			lag(payload->>'parent') over (partition by issue_key order by id) as previous,
			row_number() over (partition by issue_key order by id) as n
		from events
		where issue_key = any($1::text[]) and type in ('issue.created', 'issue.updated', 'issue.closed')
	) written
	where n = 1 or parent is distinct from previous
	order by issue_key, id
`

// readParentHistory reads the parent history of each issue keys names, and of each issue that was
// ever a parent of one of them, and so on up, so that the parents of every ancestor an issue had at
// any event are known: one query for each step up. An issue with no such event, which only a
// direct write to the database can make, has no parent in the history, so no path to a root
// passes it, and a definition under it joins the list at its approval.
func readParentHistory(ctx context.Context, q queryer, keys []string) (parentHistory, error) {
	history := parentHistory{}
	for len(keys) > 0 {
		rows, err := q.Query(ctx, parentHistoryQuery, keys)
		if err != nil {
			return nil, fmt.Errorf("read the parents of %d issues from their events: %w", len(keys), err)
		}
		var (
			issue  string
			change parentChange
		)
		if _, err := pgx.ForEachRow(rows, []any{&issue, &change.event, &change.parent}, func() error {
			history[issue] = append(history[issue], change)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("read the parents of %d issues from their events: %w", len(keys), err)
		}
		for _, key := range keys {
			if _, read := history[key]; !read {
				history[key] = nil // no event: read, so no later step asks again
			}
		}
		keys = history.above(keys)
	}
	return history, nil
}

// above is each parent that the changes of the issues of keys name and that h does not hold yet,
// once, in the order of keys and of their changes: the issues that the next step up reads.
func (h parentHistory) above(keys []string) []string {
	var next []string
	queued := make(map[string]bool, len(keys))
	for _, key := range keys {
		for _, change := range h[key] {
			if _, read := h[change.parent]; change.parent != "" && !read && !queued[change.parent] {
				queued[change.parent] = true
				next = append(next, change.parent)
			}
		}
	}
	return next
}

// upTo is the changes of issue written at or before event. Each issue's changes are in the order
// of their events, so a binary search finds them, and a walk back over the moves of an issue
// (arrival) does work in proportion to its moves times their logarithm, not their square.
func (h parentHistory) upTo(issue string, event int64) []parentChange {
	changes := h[issue]
	return changes[:sort.Search(len(changes), func(at int) bool { return changes[at].event > event })]
}

// parentAt is the parent of issue once the events up to event were written: the parent of its
// last change at or before event, and before its first change the parent it was made with.
func (h parentHistory) parentAt(issue string, event int64) string {
	changes := h[issue]
	if len(changes) == 0 {
		return ""
	}
	if written := h.upTo(issue, event); len(written) > 0 {
		return written[len(written)-1].parent
	}
	return changes[0].parent
}

// lastChange is the id of the last event at or before event that changed the parent of issue, 0
// for none.
func (h parentHistory) lastChange(issue string, event int64) int64 {
	if written := h.upTo(issue, event); len(written) > 0 {
		return written[len(written)-1].event
	}
	return 0
}

// pathTo is issue and each issue above it, up to the first named root of roots, once the events
// up to event were written, and whether that root is root. It climbs at most parentDepthCap
// links, as every walk of the issue tree does.
func (h parentHistory) pathTo(issue, root string, event int64, roots []string) ([]string, bool) {
	path := []string{issue}
	for parent := h.parentAt(issue, event); parent != ""; parent = h.parentAt(parent, event) {
		if slices.Contains(roots, parent) {
			return path, parent == root
		}
		if len(path) > parentDepthCap {
			break
		}
		path = append(path, parent)
	}
	return path, false
}

// arrival is the event at which issue last came under root, by the parents as the events left
// them at each move: the last event after which the nearest named root of roots above issue was
// root and before which it was not. It walks back from the present: the last change of a parent
// on the path from issue to root is an arrival when the path before it led to no root or to
// another one, and is a move inside the root when it led to root too, which it then walks past.
// A move of root itself, or of an issue above it, changes no path to it. An issue that has been
// under root since it was made, and one whose events put it under no root now, has no arrival.
func (h parentHistory) arrival(issue, root string, roots []string) (int64, bool) {
	for event := int64(math.MaxInt64); ; {
		path, under := h.pathTo(issue, root, event, roots)
		if !under {
			return 0, false
		}
		var last int64
		for _, key := range path {
			last = max(last, h.lastChange(key, event))
		}
		if last == 0 {
			return 0, false
		}
		if _, under := h.pathTo(issue, root, last-1, roots); !under {
			return last, true
		}
		event = last - 1
	}
}
