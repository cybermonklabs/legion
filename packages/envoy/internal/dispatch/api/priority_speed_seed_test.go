package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// How the corpus the three reading tests of api/priority_speed_test.go and its timing lane
// (api/priority_speed_timing_test.go) read is built, and what is in it. It is a file of its own
// because none of it is a reading: a reader asking what each number is held to reads those two
// files, and a reader asking what was in the database when it was taken reads this one.
//
// The whole corpus is five COPY statements into the database storetest.Open cloned from this
// package's migrated template (priority_fixture_test.go): 2,500 open issues over 18 projects,
// two events on every one of them, 20 priorities and 400 `advances` relations, one chain as deep
// as the parent-depth cap, and the cap of 20 relations on one issue. Written through the routes
// it would be some 8,000 requests and the minutes they cost. One VACUUM (ANALYZE) follows the
// five (settleSpeedCorpus), so every read is planned from the corpus's own statistics. Once every
// other reading is taken, the Go side's growth is read over speedCorpusMoreRelations relations
// more (growSpeedPriorities), which a sixth COPY writes.
//
// Twenty priorities rather than the sixteen the work is spread over: one issue carries the most
// relations a write allows (contracts.MaxIssueAdvances, 20), and an issue cannot hold twenty
// distinct priorities where the store holds sixteen. So the store holds twenty, the 380 spread
// relations name the first sixteen of them, and the heaviest issue names all twenty.
const (
	speedCorpusProjects   = 18
	speedCorpusIssues     = 2500
	speedCorpusPriorities = 20
	speedCorpusSpread     = 16
	speedCorpusRelations  = 400
	speedCorpusEvents     = 2 * speedCorpusIssues
	// speedCorpusChain is the chain as deep as the parent-depth cap: its root and the
	// refs.IssueDepthCap issues below it, which is the deepest tree a write allows
	// (refuseParentTooDeep) and the deepest the inheritance walk climbs.
	speedCorpusChain = refs.IssueDepthCap + 1
	// speedCorpusFanout is how many children an issue of the trees below takes, so the corpus
	// is trees an inheritance walk climbs rather than 2,500 roots.
	speedCorpusFanout = 4
	// speedCorpusPage is the page the listing is read over, the most issues one page holds
	// (contracts.MaxIssuePageLimit).
	speedCorpusPage = 250
	// speedCorpusMoreRelations is how many `advances` relations the Go side's growth is read
	// over (growSpeedPriorities): written after every other reading, they double the store's
	// relations, each on a leaf issue in `triage` that held none (seedSpeedCorpus).
	speedCorpusMoreRelations = speedCorpusRelations
)

// speedCorpusStatuses are the statuses the corpus's issues stand in: every status of the
// lifecycle work is open in. None of them is `done`, because every issue of the corpus is open.
var speedCorpusStatuses = []string{"triage", "icebox", "backlog", "todo", "in_progress", "testing", "needs_review", "retro"}

// speedCorpusShape is the corpus as the readings address it: the server over it, the issue
// carrying the most relations, the chain as deep as the cap, the priorities' ids, and the parent
// of every issue that has one, as the corpus was written; the database under the server, which
// the counts explain each statement on and settle the statistics of, and the trace of what the
// server sends for a counted read. The server reads through a traced pool of that database, and
// the caller's identity is resolved on the untraced one, so a count is the read's own statements
// and not the sign-in lookup every request makes.
type speedCorpusShape struct {
	handler    http.Handler
	server     *server
	heaviest   string
	chain      []string
	priorities []string
	parents    map[string]string
	database   *store.Store
	trace      *speedTrace
	// moreRelations are the speedCorpusMoreRelations tuples growSpeedPriorities writes, once
	// every other reading is taken.
	moreRelations [][]any
}

// walkRows is the rows the relation read's inheritance walk climbs for page, the keys a page of
// the listing holds: one for each of them and one for each issue above it, which is what
// issueAdvancesTerms' advances_chain answers, counted here from the parents the corpus was
// written with rather than read back through SQL of the same shape. The walk stops at the
// parent-depth cap, and the deepest chain of the corpus is exactly as deep as that cap, so the
// whole of every chain counts.
func (corpus speedCorpusShape) walkRows(page []string) int {
	rows := 0
	for _, key := range page {
		for at, above := key, true; above; at, above = corpus.parents[at] {
			rows++
		}
	}
	return rows
}

// seedSpeedCorpus writes the corpus and answers the server over it. Every issue of the first
// page the readings take stands in `triage` with the lowest board keys, so that page is the
// chain as deep as the cap, the issue carrying twenty relations and the issues below them —
// the heaviest page the corpus holds, which is the one worth reading.
func seedSpeedCorpus(t *testing.T) speedCorpusShape {
	t.Helper()
	database := storetest.Open(t)
	at := time.Now().UTC().Add(-time.Hour)

	projectKeys := make([]string, speedCorpusProjects)
	projectCounts := make([]int, speedCorpusProjects)
	projectTrees := make([][]string, speedCorpusProjects)
	for index := range projectKeys {
		projectKeys[index] = fmt.Sprintf("SPD%02d", index+1)
	}

	issues := make([][]any, 0, speedCorpusIssues)
	events := make([][]any, 0, speedCorpusEvents)
	keys := make([]string, 0, speedCorpusIssues)
	statuses := make([]string, 0, speedCorpusIssues)
	parented := make(map[string]bool, speedCorpusIssues)
	chain := make([]string, 0, speedCorpusChain)
	parents := make(map[string]string, speedCorpusIssues)
	for index := range speedCorpusIssues {
		project := 0
		if index >= speedCorpusChain {
			project = index % speedCorpusProjects
		}
		projectCounts[project]++
		key := seededIssueKey(projectKeys[project], projectCounts[project])
		var parent *string
		switch {
		case index > 0 && index < speedCorpusChain:
			// The chain: each issue the child of the one before it, so the last stands
			// refs.IssueDepthCap links below its root.
			above := keys[index-1]
			parent = &above
		case index >= speedCorpusChain:
			// The trees: each project's work under roots of its own, one issue the parent of
			// the next speedCorpusFanout.
			if ordinal := len(projectTrees[project]); ordinal > 0 {
				above := projectTrees[project][(ordinal-1)/speedCorpusFanout]
				parent = &above
			}
			projectTrees[project] = append(projectTrees[project], key)
		}
		status := "triage"
		if index >= speedCorpusPage {
			status = speedCorpusStatuses[index%len(speedCorpusStatuses)]
		}
		written := at.Add(time.Duration(index) * time.Second)
		issues = append(issues, seededIssueRow{
			key: key, project: projectKeys[project], number: projectCounts[project],
			title: fmt.Sprintf("Corpus work %04d", index), status: status, parent: parent,
			rank: seededRank(index), at: written, lastSeq: 2,
		}.values())
		events = append(events,
			seededEvent(key, 1, "issue.created", `{"seeded":true}`, written),
			seededEvent(key, 2, "comment.created", `{"seeded":true}`, written.Add(time.Second)))
		keys = append(keys, key)
		statuses = append(statuses, status)
		if parent != nil {
			parents[key] = *parent
			parented[*parent] = true
		}
		if index < speedCorpusChain {
			chain = append(chain, key)
		}
	}

	priorities := make([][]any, 0, speedCorpusPriorities)
	priorityIDs := make([]string, 0, speedCorpusPriorities)
	for index := range speedCorpusPriorities {
		id := fmt.Sprintf("goal-%02d", index)
		priorityIDs = append(priorityIDs, id)
		priorities = append(priorities, seededPriorityNode(t, id, fmt.Sprintf("Goal %02d", index),
			"What this goal is for.", "alice", fmt.Sprintf("s%02d", index), at))
	}

	// The heaviest issue is the chain's root: it states every priority the store holds, so the
	// 32 issues below it each inherit twenty.
	heaviest := chain[0]
	tuples := make([][]any, 0, speedCorpusRelations)
	for _, id := range priorityIDs {
		tuples = append(tuples, seededAdvancesTuple(heaviest, id, at))
	}
	// The rest, spread over the corpus by a stride that reaches its far end, each naming one of
	// the sixteen priorities the work is spread over.
	stride := (speedCorpusIssues - speedCorpusChain) / (speedCorpusRelations - len(tuples))
	for spread := 0; len(tuples) < speedCorpusRelations; spread++ {
		tuples = append(tuples, seededAdvancesTuple(
			keys[speedCorpusChain+spread*stride], priorityIDs[spread%speedCorpusSpread], at))
	}

	// The relations the Go side's growth is read over (growSpeedPriorities), on issues off the
	// first page that stand in `triage`, which neither fault reads, hold no relation yet and have
	// no issue below them to inherit one: so a second read answers the same faults, and every
	// priority already counts work in `triage` (the chain's), so the counts answer the same rows.
	// Each leaf names priorities one after another, a different one on each pass over the
	// leaves, so no pair is written twice.
	holding := make(map[string]bool, speedCorpusRelations)
	for _, tuple := range tuples {
		holding[tuple[1].(string)] = true
	}
	leaves := []string{}
	for index, key := range keys {
		if index >= speedCorpusPage && statuses[index] == "triage" && !parented[key] && !holding[key] {
			leaves = append(leaves, key)
		}
	}
	if len(leaves)*speedCorpusPriorities < speedCorpusMoreRelations {
		t.Fatalf("the corpus has %d leaf issues in triage without a relation, too few for %d relations more over %d priorities",
			len(leaves), speedCorpusMoreRelations, speedCorpusPriorities)
	}
	more := make([][]any, 0, speedCorpusMoreRelations)
	for index := range speedCorpusMoreRelations {
		leaf, pass := index%len(leaves), index/len(leaves)
		more = append(more, seededAdvancesTuple(leaves[leaf], priorityIDs[(leaf+pass)%speedCorpusPriorities], at))
	}

	projects := make([][]any, 0, speedCorpusProjects)
	for index, key := range projectKeys {
		projects = append(projects, seededProject(key, projectCounts[index], at))
	}
	copySeededRows(t, database, "projects", seededProjectColumns, projects)
	copySeededRows(t, database, "issues", seededIssueColumns, issues)
	copySeededRows(t, database, "events", seededEventColumns, events)
	copySeededRows(t, database, "priority_nodes", seededNodeColumns, priorities)
	copySeededRows(t, database, "priority_tuples", seededTupleColumns, tuples)
	assertSpeedCorpus(t, database)
	settleSpeedCorpus(t, database)

	trace := &speedTrace{}
	handler, _, _, srv := newRegisteredTestServer(t, testServerOptions{store: database, tracer: trace, identity: headerIdentity(database)})
	return speedCorpusShape{
		handler: handler, server: srv, heaviest: heaviest, chain: chain, priorities: priorityIDs, parents: parents,
		database: database, trace: trace, moreRelations: more,
	}
}

// settleSpeedCorpus leaves the corpus as a deployment's database stands between writes: every
// table vacuumed and analyzed, which autovacuum does for a deployment some time after its writes.
// Left to autovacuum here, each read runs the plan Postgres makes from no statistics until
// autovacuum reaches this database, and when that is depends on how busy the server is: a reading
// taken before it and one taken after are readings of two different plans. With the corpus's few
// thousand rows, ANALYZE reads every row rather than a sample, so the statistics, and the plans
// made from them, are the same on every run.
func settleSpeedCorpus(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), "vacuum (analyze)"); err != nil {
		t.Fatalf("vacuum and analyze the corpus: %v", err)
	}
}

// assertSpeedCorpus holds the corpus to its own counts before anything is measured: a measurement
// over a corpus that landed short is a measurement of a corpus nobody meant to build.
func assertSpeedCorpus(t *testing.T, database *store.Store) {
	t.Helper()
	for _, counted := range []struct {
		what  string
		query string
		want  int
	}{
		{"projects", `select count(*) from projects`, speedCorpusProjects},
		{"open issues", `select count(*) from issues where closed_at is null`, speedCorpusIssues},
		{"events", `select count(*) from events`, speedCorpusEvents},
		{"priorities", `select count(*) from priority_nodes`, speedCorpusPriorities},
		{"advances relations", `select count(*) from priority_tuples`, speedCorpusRelations},
	} {
		got := 0
		if err := database.Pool.QueryRow(context.Background(), counted.query).Scan(&got); err != nil {
			t.Fatalf("count the corpus's %s: %v", counted.what, err)
		}
		if got != counted.want {
			t.Fatalf("the corpus holds %d %s, want %d", got, counted.what, counted.want)
		}
	}
}
