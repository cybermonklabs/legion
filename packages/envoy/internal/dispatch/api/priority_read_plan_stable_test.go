//go:build !race

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// prioritiesReadPlanRatio is what R3 of dispatch://INFRA-5791 holds each statement of the read
// to with a project named: at most twice the shared buffers on the generic plan that it reads on
// the plan made for its values, the rule dispatch://INFRA-5362 set for this read.
const prioritiesReadPlanRatio = 2.0

// TestThePrioritiesReadKeepsItsPlansWithAProjectNamed holds the full read and the summary with
// the largest project named, `?project=<p>` and `?project=<p>&summary=true`, to R3: on the
// corpus of production's counts (seedPrioritiesReadCorpus), each statement either request sends
// reads at most twice the shared buffers on the generic plan that it reads on the plan made for
// its values. A named project makes the second arm of priorityProjectFilter select the rows,
// which TestPrioritiesReadKeepsItsPlans, reading no project, does not reach.
//
// Each request goes through a server whose pool holds one connection under the plan named
// (newPrioritiesReadServer), with JIT off, ten times (prioritiesReadWarmReads); the connection is
// held to having run every statement under that plan, and then each statement the first request
// sent runs on that connection, with the arguments it was sent with, under EXPLAIN (ANALYZE,
// BUFFERS). The test prints the shared buffers of each statement's top node, hit and read, and
// its time on both plans, their ratio, and both plans. Buffers are what a plan reads, not how
// fast the machine reads them, so the bound holds on any machine.
func TestThePrioritiesReadKeepsItsPlansWithAProjectNamed(t *testing.T) {
	// Not parallel: it holds shared-buffer counts of its own statements to a ratio, and the
	// shared buffers and their caching are the whole Postgres server's, which the package's
	// parallel tests use at the same time, as TestPrioritiesReadKeepsItsPlans says.
	corpus := seedPrioritiesReadCorpus(t)
	t.Log(corpus.census)
	corpus.census.hold(t)
	project := readCorpusLargestProjects(t, corpus.database, 1)[0]
	servers := make([]prioritiesReadServer, len(prioritiesReadPlans))
	for index, plan := range prioritiesReadPlans {
		servers[index] = newPrioritiesReadServer(t, corpus.database, plan)
	}
	for _, form := range prioritiesReadForms {
		target := form.target([]string{project})
		read := make([][]prioritiesReadStatement, len(servers))
		plans := make([][]prioritiesStatementPlan, len(servers))
		for index, served := range servers {
			_, read[index], _ = served.warm(t, target)
			served.requirePlansRun(t, read[index], prioritiesReadWarmReads)
			for _, statement := range read[index] {
				plans[index] = append(plans[index], served.explain(t, statement))
			}
		}
		generic, custom := read[0], read[1]
		if len(generic) != len(custom) {
			t.Fatalf("%s with project %s sent %d statements under the generic plan and %d under the plan made for its values, want the same statements",
				form.called, project, len(generic), len(custom))
		}
		for index, statement := range generic {
			if custom[index].sql != statement.sql {
				t.Fatalf("%s with project %s sent %s as statement %d under the generic plan and %s under the plan made for its values, want the same statements",
					form.called, project, statement.name, index+1, custom[index].name)
			}
			onGeneric, onCustom := plans[0][index], plans[1][index]
			ratio := 1.0
			switch {
			case onCustom.buffers() > 0:
				ratio = onGeneric.buffers() / onCustom.buffers()
			case onGeneric.buffers() > 0:
				ratio = math.Inf(1)
			}
			t.Logf("c-plan-stable: %s of %s with project %s: %.0f shared buffers (hit %.0f, read %.0f) in %.1f ms on the generic plan, %.0f (hit %.0f, read %.0f) in %.1f ms on the plan made for its values: %.2f times, at most %.0f; on the generic plan %s, on the plan made for its values %s\n  the generic plan:\n%s\n  the plan made for its values:\n%s",
				statement.name, form.called, project,
				onGeneric.buffers(), onGeneric.hit, onGeneric.read, onGeneric.milliseconds,
				onCustom.buffers(), onCustom.hit, onCustom.read, onCustom.milliseconds,
				ratio, prioritiesReadPlanRatio, onGeneric.walk(), onCustom.walk(),
				indentPlan(onGeneric.text), indentPlan(onCustom.text))
			t.Logf("c-plan-stable: parts: %s of %s with project %s: on the generic plan %s; on the plan made for its values %s",
				statement.name, form.called, project, onGeneric.partsRead(), onCustom.partsRead())
			if ratio > prioritiesReadPlanRatio {
				t.Errorf("%s of %s with project %s: %.0f shared buffers on the generic plan, %.2f times the %.0f on the plan made for its values, want at most %.0f times",
					statement.name, form.called, project, onGeneric.buffers(), ratio, onCustom.buffers(), prioritiesReadPlanRatio)
			}
		}
	}
}

// TestThePrioritiesReadWalksIssuesOnceOnTheCorpus holds the walk over the members that the full
// read's statement (priorityReadQuery) takes, main's walk (mainNewestEventWalk) seeded from the
// relations the statement reads itself, on the corpus of production's counts
// (seedPrioritiesReadCorpus), with no project named, under the generic plan a prepared statement
// settles on and under the plan made for its values, each run at the planner's default settings:
//
//   - one read of issues in the walk, which runs once (requireWalkReadsIssuesOnce);
//   - no more shared buffers in the walk, the read of the relations in it aside, than main's
//     walk reads under the same plan (mainPriorityNewestEventQuery, over the same relations as
//     two arrays, which its walk reads by their subscripts and so reads no relation);
//   - in steady state, every member's newest-event pair naming its event of last_seq, as the
//     corpus's fill leaves it: no read of events in the newest event (requireNewestEventProbes).
//
// These are the walk's holds TestPriorityNewestEventReadsOneEventOfEachMember made on its corpus
// of 29 member issues, moved here: there the statement's walk is sized from that corpus's few
// relations, and the planner then scans issues at each step, where on a deployment's relations
// it hashes issues once. The walk keyed through issues_parent_key (priorityNewestEventKeyedWalkQuery)
// is the control both walk holds must fail under each plan (explaintest.RequireControlFails).
//
// Both plans of each text run on one connection, the plan for its values right after the generic
// plan of the same text, so each count is what the plan reads on a connection that has run the
// text. A node's shared buffers count whatever the backend read while the node ran, the catalog
// pages its first lookup on a connection loads among them: on its first run on a connection of the
// pool, the sort of the relations under the plan for its values read three buffers more than the
// scan of priority_tuples below it, 41 against 38, and 38 when the same plan ran again on that
// connection (PostgreSQL 18.6, 2026-11-25). Those three were neither the relations' read nor the
// walk's, and the hold counted them as the walk's.
func TestThePrioritiesReadWalksIssuesOnceOnTheCorpus(t *testing.T) {
	// Not parallel: it holds shared-buffer counts, as the other two tests over the corpus do.
	if !strings.Contains(priorityReadQuery, mainNewestEventWalk) {
		t.Fatalf("the priorities read's statement does not carry main's walk as mainNewestEventWalk freezes it, so the walk holds hold no text the read runs")
	}
	corpus := seedPrioritiesReadCorpus(t)
	t.Log(corpus.census)
	corpus.census.hold(t)
	ctx := context.Background()
	nodes, err := prioritystore.List(ctx, corpus.database.Pool)
	if err != nil {
		t.Fatalf("read the live priorities: %v", err)
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	seedKeys, seedIDs := priorityCorpusSeed(t, corpus.database, ids)
	keys := explaintest.NewIndexKeys(corpus.database.Pool)
	connection, err := corpus.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()

	const mains, statement, keyed = 0, 1, 2
	seeded := []any{seedKeys, seedIDs, []string{}}
	texts := []struct {
		name, statement string
		arguments       []any
	}{
		mains:     {"main's newest event", mainPriorityNewestEventQuery, seeded},
		statement: {"the priorities read's statement", priorityReadQuery, []any{ids, []string{}}},
		keyed:     {"the newest event with its walk keyed", priorityNewestEventKeyedWalkQuery, seeded},
	}
	plans := []struct {
		name  string
		plans []json.RawMessage
	}{{name: "the generic plan"}, {name: "the plan for its values"}}
	for textNumber, text := range texts {
		parameters, literals := benchtime.PreparedArguments(t, text.name, text.arguments...)
		plans[0].plans = append(plans[0].plans, explaintest.AnalyzedGenericPlan(t, ctx, connection,
			fmt.Sprintf("corpus_walk_%d", textNumber), text.statement, parameters, literals))
		plans[1].plans = append(plans[1].plans, explaintest.AnalyzedPlan(t, ctx, connection,
			text.statement, nil, text.arguments...))
	}
	for _, plan := range plans {
		subjects := make([]string, len(texts))
		for textNumber, text := range texts {
			subjects[textNumber] = fmt.Sprintf("%s under %s on the corpus", text.name, plan.name)
		}
		mainsWalk := newestEventWalkBuffers(t, plan.plans[mains], subjects[mains])
		if mainsWalk == 0 {
			t.Fatalf("%s reads no shared buffer in its walk over the members, so the bound on the walk holds nothing:\n%s",
				subjects[mains], plan.plans[mains])
		}
		requireWalkReadsIssuesOnce(t, plan.plans[statement], subjects[statement])
		relations := 0.0
		for _, node := range explaintest.Nodes(t, newestEventWalk(t, plan.plans[statement], subjects[statement])) {
			if node.Relation == "priority_tuples" {
				relations += node.SharedHitBlocks + node.SharedReadBlocks
			}
		}
		walk := newestEventWalkBuffers(t, plan.plans[statement], subjects[statement]) - relations
		t.Logf("%s reads %v shared buffers in its walk over the members beside %v in its read of the relations, and main's walk %v",
			subjects[statement], walk, relations, mainsWalk)
		if walk > mainsWalk {
			t.Errorf("%s reads %v shared buffers in its walk over the members beside its read of the relations, more than the %v of main's walk under the same plan, want at most main's:\n%s",
				subjects[statement], walk, mainsWalk, plan.plans[statement])
		}
		requireNewestEventProbes(t, keys, plan.plans[statement], subjects[statement], 0)
		for _, control := range []struct {
			cannotTell string
			check      func(t testing.TB, plan json.RawMessage, subject string)
		}{
			{"a walk that reads issues for each member row from one that reads it once", requireWalkReadsIssuesOnce},
			{"a walk that reads more shared buffers than main's from one that reads no more", func(t testing.TB, plan json.RawMessage, subject string) {
				requireWalkWithinMains(t, plan, subject, mainsWalk)
			}},
		} {
			explaintest.RequireControlFails(t, subjects[keyed], "a control", control.cannotTell, plan.plans[keyed],
				func(t testing.TB) { control.check(t, plan.plans[keyed], subjects[keyed]) })
		}
	}
}
