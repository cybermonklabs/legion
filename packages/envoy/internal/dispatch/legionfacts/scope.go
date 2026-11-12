package legionfacts

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// scope.go holds the scope of one page: the projects this loop computes and which of them are
// to be read whole, the stored rows the sweep adds to a page's work, and the rows one project's
// changes reach. The set a page states for a project is worked out here and nowhere else, and
// the page writes or deletes no row outside it.
//
// It stands apart from the loop (consumer.go) because the loop asks it one question - what does
// this page owe this project - and the answer is read from the issues' links, from the stored
// rows and from the phase-work rule rather than from the feeds.

// legionProjects is every project this loop computes, by key, each with whether its facts are to
// be computed whole on this pass. A project is one of them once a Legion daemon has been read of
// it (its sampler's observations) or once it holds a stored row. An event of a project neither
// answers for is no work of this loop: its first observation brings the project in, and the
// backfill then computes it.
//
// A project is a backfill - a pass of this same loop that runs ComputeProject over the project
// rather than Compute over a key set - where its mark says it was never computed whole, where
// the rows that mark counted have gone, or where lost says the change cursor fell below the
// log, which loses the changes of every project at once. The first run on an empty table is
// the first of those cases and needs no second code path.
//
// The mark is a row of issue_facts_computed, which the pass that read the project whole writes
// (markComputedWhole). "Never computed" is read from that record and not from "this project
// holds no stored row", because a project whose issues carry no Legion phase work is computed
// whole and answers no row: read from the absence of a row, such a project would be computed
// whole on every pass for as long as it held no worked issue, which is the whole-project read
// this loop exists to make rare.
//
// The count the mark carries is what still answers for a project whose stored rows went
// missing under the consumer, an emptied table among them: a project whose last whole read
// answered rows and which now holds none has lost them to something this loop did not do, and
// no narrow pass can name rows that are not there. It costs one whole read, because that read
// writes the count it answered: a project that truly holds no row any more is marked with none
// and is left alone from then on.
func legionProjects(ctx context.Context, q Querier, lost bool) (map[string]bool, error) {
	rows, err := q.Query(ctx, `
		select project.key,
		       mark.project_key is null
		       or (mark.computed_rows > 0
		           and not exists (select 1 from issue_facts where project_key = project.key))
		from projects project
		left join issue_facts_computed mark on mark.project_key = project.key
		where exists (select 1 from legion_observations where project = project.key)
		   or exists (select 1 from issue_facts where project_key = project.key)
	`)
	if err != nil {
		return nil, fmt.Errorf("read the projects Legion works: %w", err)
	}
	defer rows.Close()
	projects := map[string]bool{}
	for rows.Next() {
		var key string
		var backfill bool
		if err := rows.Scan(&key, &backfill); err != nil {
			return nil, fmt.Errorf("read the projects Legion works: %w", err)
		}
		projects[key] = backfill || lost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the projects Legion works: %w", err)
	}
	return projects, nil
}

// openStoredRows is the stored row of every open issue, by project: what the sweep recomputes,
// because an open issue's hours grow with the clock while no input moves. A closed issue's facts
// stand still, so the sweep passes over it and WriteProject leaves its row alone.
//
// It reads the rows rather than the issues: an open issue with no stored row carries no Legion
// phase work yet, or has never been computed, and its own event or the backfill answers for it.
func openStoredRows(ctx context.Context, q Querier) (map[string][]string, error) {
	rows, err := q.Query(ctx, `select project_key, issue_key from issue_facts where closed_at is null`)
	if err != nil {
		return nil, fmt.Errorf("read the open issues' stored facts: %w", err)
	}
	defer rows.Close()
	open := map[string][]string{}
	for rows.Next() {
		var project, issue string
		if err := rows.Scan(&project, &issue); err != nil {
			return nil, fmt.Errorf("read the open issues' stored facts: %w", err)
		}
		open[project] = append(open[project], issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the open issues' stored facts: %w", err)
	}
	return open, nil
}

// staleStoredRows is the stored row of every issue that a session wrote on and that carries no
// Legion phase work any more, by project: the second set the sweep adds to its work, beside the
// open rows.
//
// A stored row means Legion worked the issue, and an issue loses its phase work as well as
// gains it: a role session that writes next on another issue under that issue's title takes the
// work with it (legion.PhaseWork). A loss of that kind is written as an event, and the page
// that reads that event states the issues the session left behind (sessionIssues), so this read
// finds nothing to add for it.
//
// What only this read catches is a loss no event carries, and a release is one. Which sessions
// are Legion roles is six role names and one title shape held in this build
// (legion.WorkerTitle, legion/session_title.go), and the daemon writes its titles from its own
// copy of them (packages/pi-legion/src/session-title.ts). A release that renames a role, drops
// one or changes the shape takes the work off every issue whose row came from a session titled
// the old way, and no session writes an event as it does so. A write made to events or to
// handoffs outside Dispatch, by an operator or by a restore, takes work away the same silent
// way. The issues are then named by nothing else a pass reads: no event of theirs is written,
// the links do not reach them, and the sweep reads them among the open rows only while they are
// open, so the row of a closed issue would stand for ever. The key is therefore read from what
// the table already holds, as the tree a moved issue left is read from the stored root_key
// (leftTrees), and it joins the set the pass states. It is one more source of keys of the same
// kind: the computation decides whether the issue still carries work, and the deletion inside
// the stated set takes the row only where the computation answered it none.
//
// A row whose issue no session ever wrote on is left where it stands. Such a row is no row this
// loop can have written, because the computation answers a row for an issue a handoff names or
// a role session worked, and a handoff is never taken back. The whole-project pass is what
// answers for a row of that kind (deleteStaleProjectRows), as it does for a row whose key names
// no issue at all.
//
// Both reads are of the whole deployment and run once a sweep, which is what their cost is
// measured against. The call on its own takes 8.7 ms over the rig's 252 issues and 210 stored
// rows, and 33.6 ms over the 1,008 issues and 840 rows of the freshness test's larger
// population, best of five against the pod's test Postgres. What it adds to a sweep pass is
// less than that, which is the figure SweepEvery carries: legion.PhaseWorkKeys is the one read
// of the phase-work rule, and the computation of every project of the pass makes it anyway
// (metrics.Population). `select project_key, issue_key from issue_facts` reads two narrow
// columns of the whole table, which Postgres answers with one sequential scan: 26 shared
// buffers and 0.3 ms over those 1,008 rows, and 519 buffers and 8 ms over 20,000 rows, under
// explain (analyze, buffers). Neither read grows with the issues the pass computes.
func staleStoredRows(ctx context.Context, pool *store.Pool) (map[string][]string, error) {
	work, err := legion.PhaseWorkKeys(ctx, pool)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `select project_key, issue_key from issue_facts`)
	if err != nil {
		return nil, fmt.Errorf("read the stored facts of the issues that lost their Legion phase work: %w", err)
	}
	defer rows.Close()
	stale := map[string][]string{}
	for rows.Next() {
		var project, issue string
		if err := rows.Scan(&project, &issue); err != nil {
			return nil, fmt.Errorf("read the stored facts of the issues that lost their Legion phase work: %w", err)
		}
		if _, works := work.Keys[issue]; works {
			continue
		}
		if _, wrote := work.SessionIssues[issue]; !wrote {
			continue
		}
		stale[project] = append(stale[project], issue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stored facts of the issues that lost their Legion phase work: %w", err)
	}
	return stale, nil
}

// rowsToCompute is the whole set one pass computes for a project, sorted: the rows the changes
// touch through the issues' links now (rowsTouchedBy) and the rows of the tree a moved issue
// left (leftTrees). It is the set the pass states before it writes, and it writes no row
// outside it.
//
// moved names the issues a row of a feed took out of one tree and into another (touch). Only
// the tree such an issue left is read from it: the links as they stand now answer for every
// other issue a change can move, the issues below a key among them (rowsTouchedBy).
func rowsToCompute(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool, above map[string]string) ([]string, error) {
	touched := rowsTouchedBy(keys, above)
	left, err := leftTrees(ctx, q, project, keys, moved)
	if err != nil {
		return nil, err
	}
	if len(left) == 0 {
		return touched, nil
	}
	whole := make(map[string]bool, len(touched)+len(left))
	for _, key := range touched {
		whole[key] = true
	}
	for _, key := range left {
		whole[key] = true
	}
	return slices.Sorted(maps.Keys(whole)), nil
}

// leftTrees is every stored row of the tree a moved issue left, sorted. An issue a move took
// out of one tree and into another takes its own hours with it, so the old tree's root goes on
// counting the time it waited while that issue worked until its row is computed again. A closed
// root is never recomputed by the sweep, which recomputes the open issues alone, so a pass that
// left the old tree out would leave that row wrong for good.
//
// Two rows move an issue that way and both are marked the same (touch): an event whose
// `changed` names `parent`, and an observation of legion.FieldTree. This reads the tree left
// for either of them, because the tree rule reads the daemon's `tree` before the parent link
// (legion.TreeChain) and an issue leaves a tree whichever of the two moved it.
//
// The old tree is the tree as the last computation recorded it: the moved issue's stored
// root_key, and every issue the table holds under that root. Nothing else can name it, because
// the links the pass reads are the links after the move. It is two queries, and only where an
// issue moved. A move is uncommon: a parent link is moved by hand, and the sampler writes a
// `tree` observation only where the tree it reports differs from the newest one it recorded
// (legion.RecordObservations), so a daemon that goes on reporting one tree writes one row.
//
// The stored root is taken whether or not the links now give another one. There is one root
// rule, legion.TreeChain, which is what wrote that root_key, and a walk of the raw parent links
// is a second rule that can answer differently: it reads neither the daemon's tree nor the stop
// at a parent with no Legion work. Taking the stored root always states a superset of the rows
// a comparison of the two would state, which the scope rule allows: the set is stated before
// anything is written, and a row inside it is written or deleted only because the computation
// of that issue said so.
func leftTrees(ctx context.Context, q Querier, project string, keys []string, moved map[string]bool) ([]string, error) {
	wasMoved := make([]string, 0, len(moved))
	for _, key := range keys {
		if moved[key] {
			wasMoved = append(wasMoved, key)
		}
	}
	if len(wasMoved) == 0 {
		return nil, nil
	}
	stored, err := q.Query(ctx, `
		select root_key from issue_facts
		where project_key = $1 and issue_key = any($2::text[]) and root_key <> ''
	`, project, wasMoved)
	if err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
	}
	roots := []string{}
	for stored.Next() {
		var root string
		if err := stored.Scan(&root); err != nil {
			stored.Close()
			return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
		}
		roots = append(roots, root)
	}
	stored.Close()
	if err := stored.Err(); err != nil {
		return nil, fmt.Errorf("read the stored roots of %d moved issues of %s: %w", len(wasMoved), project, err)
	}
	if len(roots) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select issue_key from issue_facts
		where project_key = $1 and (root_key = any($2::text[]) or issue_key = any($2::text[]))
	`, project, roots)
	if err != nil {
		return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
	}
	defer rows.Close()
	left := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
		}
		left = append(left, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the trees %v of %s: %w", roots, project, err)
	}
	slices.Sort(left)
	return left, nil
}

// rowsTouchedBy is every issue whose stored row a change to one of keys can move, sorted: the
// issues themselves, the issues above them and the issues below them. above is the issue above
// each issue of the project (metrics.TreeLinks).
//
// An issue above a key is touched because an issue's hours carry the time it waited while a
// child worked, so a child's own change moves every row above it.
//
// An issue below a key is touched because an input that names an issue can move where every
// issue under that issue stands. That is one rule for every input this pass reads, and it is
// written here, where the set is stated, rather than at the feed that read the input:
//
//  1. A parent link an event's `changed` names and a `tree` an observation names take the
//     issue itself into another tree (touch), and the chain above every issue below it moves
//     with it.
//  2. An input that names one issue and moves nothing moves the issues below it just as far.
//     The placement rule stops a chain below a parent that is not a Legion issue
//     (legion.TreeChain), so an issue below stands under its parent only while that parent is
//     a Legion issue, and which issues are Legion issues is moved by an event of a worker
//     session, by a handoff, by the daemon's first report of an issue and by a release or a
//     retitle that takes an issue's phase work away (legion.TreeNode's Legion field holds the
//     rule). Each of those names the parent alone.
//
// The second kind is what a walk that descended from the moved issues alone left behind. The
// sweep recomputes an issue of that kind while it is open, and a closed issue below such a
// parent is named by nothing again: its row then stands at a root the computation of that
// issue does not answer, which is what this descent answers.
//
// A narrower descent would start from the keys whose Legion standing moved, and the stored
// rows cannot answer which keys those are. A row is held for an issue that carries Legion
// phase work (Compute), and the parent whose standing moves commonly carries none and holds no
// row at all, so there is nothing to read this pass's standing against. A second record of the
// standing is what that descent would need, and the wider set needs none: the set is stated
// before anything is written, and a row inside it is written or deleted only because the
// computation of that issue said so.
//
// It climbs and descends the links metrics.TreeLinks gives, which are the one tree step
// (legion.TreeAbove) under the scope rule the metrics hold, because the computation this set
// is stated for reads its own scope by that same rule. The issues below an issue are therefore
// the issues the step puts below it: the one whose `tree` names it as well as the one whose
// parent link does.
//
// It reads the links as they stand now, so it cannot name the tree a moved issue left: that one
// is read from the stored rows beside it (leftTrees), and rowsToCompute is the two together.
//
// Each walk keeps the keys it has already visited, so a chain that loops stops rather than
// running for ever, which is the stop every tree walk holds (legion.TreeChain). What the
// descent costs a pass is measured over the rig's histories and recorded with the pass's other
// costs (SweepEvery).
func rowsTouchedBy(keys []string, above map[string]string) []string {
	touched := make(map[string]bool, len(keys))
	climbed := map[string]bool{}
	for _, key := range keys {
		touched[key] = true
		clear(climbed)
		climbed[key] = true
		for up := above[key]; up != "" && !climbed[up]; up = above[up] {
			climbed[up] = true
			touched[up] = true
		}
	}
	children := map[string][]string{}
	for key, up := range above {
		if up != "" {
			children[up] = append(children[up], key)
		}
	}
	descending := slices.Clone(keys)
	walked := make(map[string]bool, len(keys))
	for len(descending) > 0 {
		key := descending[len(descending)-1]
		descending = descending[:len(descending)-1]
		if walked[key] {
			continue
		}
		walked[key] = true
		for _, below := range children[key] {
			touched[below] = true
			descending = append(descending, below)
		}
	}
	return slices.Sorted(maps.Keys(touched))
}
