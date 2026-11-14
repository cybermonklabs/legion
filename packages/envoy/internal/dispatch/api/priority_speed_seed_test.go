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

// How the corpus the two speed tests are measured over is built, and what is in it. It is a file
// of its own because none of it is a measurement: a reader asking what each number is held to
// reads api/priority_speed_test.go, and a reader asking what was in the database when it was
// taken reads this one.
//
// The whole corpus is five COPY statements into the database storetest.Open cloned from this
// package's migrated template (priority_fixture_test.go): 2,500 open issues over 18 projects,
// two events on every one of them, 20 priorities and 400 `advances` relations, one chain as deep
// as the parent-depth cap, and the cap of 20 relations on one issue. Written through the routes
// it would be some 8,000 requests and the minutes they cost, which is time the measurement would
// then be taken against.
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
	// speedCorpusPage is the page the listing is measured over, the most issues one page holds
	// (contracts.MaxIssuePageLimit).
	speedCorpusPage = 250
)

// speedCorpusStatuses are the statuses the corpus's issues stand in: every status of the
// lifecycle work is open in. None of them is `done`, because every issue of the corpus is open.
var speedCorpusStatuses = []string{"triage", "icebox", "backlog", "todo", "in_progress", "testing", "needs_review", "retro"}

// speedCorpusShape is the corpus as the measurements address it: the server over it, the issue
// carrying the most relations, and the chain as deep as the cap.
type speedCorpusShape struct {
	handler  http.Handler
	server   *server
	heaviest string
	chain    []string
}

// seedSpeedCorpus writes the corpus and answers the server over it. Every issue of the first
// page the measurements read stands in `triage` with the lowest board keys, so that page is the
// chain as deep as the cap, the issue carrying twenty relations and the issues below them —
// the heaviest page the corpus holds, which is the one worth timing.
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
	chain := make([]string, 0, speedCorpusChain)
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

	handler, _, _, srv := newRegisteredTestServer(t, testServerOptions{store: database})
	return speedCorpusShape{handler: handler, server: srv, heaviest: heaviest, chain: chain}
}

// assertSpeedCorpus holds the corpus to its own counts before anything is timed: a measurement
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
