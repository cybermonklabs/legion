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
	"github.com/sjawhar/envoy/internal/explaintest"
)

// prioritiesReadPlanRatio is what R3 of dispatch://INFRA-5791 holds each statement of the read
// to with a project named: at most twice the shared buffers on the generic plan that it reads on
// the plan made for its values, the rule dispatch://INFRA-5362 set for this read, which
// TestPrioritiesReadKeepsItsPlans holds with no project named (planBuffersRatio).
const prioritiesReadPlanRatio float64 = planBuffersRatio

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
// (seedPrioritiesReadCorpus), with no project named and with project PRDW named (the corpus's
// readCorpusAdvancesProject), under the generic plan a prepared statement settles on and under the
// plan made for its values, each run at the planner's default settings:
//
//   - one read of issues in the walk, which runs once (requireWalkReadsIssuesOnce);
//   - no more shared buffers in the walk, the read of the relations it counts aside
//     (newestEventWalkRelationsBuffers), than main's walk reads under the same plan over the same
//     scope (mainPriorityNewestEventQuery, over the same relations as two arrays, which its walk
//     reads by their subscripts and so reads no relation);
//   - one read of issues in member_issue, the issues row of each member issue that every part of
//     the statement takes, in one scan that runs once (requireMemberIssueReadsIssuesOnce);
//   - in steady state, every member's newest-event pair naming its event of last_seq, as the
//     corpus's fill leaves it: no read of events in the newest event (requireNewestEventProbes).
//
// These holds read the corpus of production's counts because the planner sizes the statement's
// walk from the relations it reads: over a corpus of a few relations it scans issues at each step
// of the walk, where over a deployment's relations it hashes issues once. Each must fail a control
// under each plan over each scope, and the test fails where a control passes
// (explaintest.RequireControlFails): the walk keyed through issues_parent_key
// (priorityNewestEventKeyedWalkQuery) fails both walk holds; a copy of the statement whose
// member_issue probes issues_pkey for each member issue (priorityMemberIssueProbedQuery) fails the
// read of issues in member_issue; and the statement itself, whose walk reads issues in a Seq Scan,
// fails the index condition on every scan (requireEveryScanKeyed) that
// TestPriorityNewestEventReadsOneEventOfEachMember holds the walk keyed to.
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
	// Not parallel: it holds shared-buffer counts, as the two other corpus tests that hold them
	// (TestThePrioritiesReadAnswersWithinItsBoundOnEveryPlan and
	// TestThePrioritiesReadKeepsItsPlansWithAProjectNamed) are not; the merge base's comparison
	// holds none and runs in parallel.
	if !strings.Contains(priorityReadQuery, mainNewestEventWalk) {
		t.Fatalf("the priorities read's statement does not carry main's walk as mainNewestEventWalk freezes it, so the walk holds hold no text the read runs")
	}
	corpus := seedPrioritiesReadCorpus(t)
	t.Log(corpus.census)
	corpus.census.hold(t)
	ctx := context.Background()
	ids := livePriorityIDs(t, corpus.database)
	seedKeys, seedIDs := priorityCorpusSeed(t, corpus.database, ids)
	keys := explaintest.NewIndexKeys(corpus.database.Pool)
	connection, err := corpus.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()

	probedQuery := priorityMemberIssueProbedQuery(t)
	const mains, statement, keyed, probed = 0, 1, 2, 3
	for scopeNumber, scope := range []struct {
		name     string
		projects []string
	}{
		{"no project named", []string{}},
		{"project " + readCorpusAdvancesProject + " named", []string{readCorpusAdvancesProject}},
	} {
		seeded := []any{seedKeys, seedIDs, scope.projects}
		texts := []struct {
			name, statement string
			arguments       []any
		}{
			mains:     {"main's newest event", mainPriorityNewestEventQuery, seeded},
			statement: {"the priorities read's statement", priorityReadQuery, []any{ids, scope.projects}},
			keyed:     {"the newest event with its walk keyed", priorityNewestEventKeyedWalkQuery, seeded},
			probed:    {"the statement with member_issue probed", probedQuery, []any{ids, scope.projects}},
		}
		plans := []struct {
			name  string
			plans []json.RawMessage
		}{{name: "the generic plan"}, {name: "the plan for its values"}}
		for textNumber, text := range texts {
			parameters, literals := benchtime.PreparedArguments(t, text.name+" with "+scope.name, text.arguments...)
			plans[0].plans = append(plans[0].plans, explaintest.AnalyzedGenericPlan(t, ctx, connection,
				fmt.Sprintf("corpus_walk_%d_%d", scopeNumber, textNumber), text.statement, parameters, literals))
			plans[1].plans = append(plans[1].plans, explaintest.AnalyzedPlan(t, ctx, connection,
				text.statement, nil, text.arguments...))
		}
		for _, plan := range plans {
			subjects := make([]string, len(texts))
			for textNumber, text := range texts {
				subjects[textNumber] = fmt.Sprintf("%s under %s on the corpus with %s", text.name, plan.name, scope.name)
			}
			mainsWalk := newestEventWalkBuffers(t, plan.plans[mains], subjects[mains])
			if mainsWalk == 0 {
				t.Fatalf("%s reads no shared buffer in its walk over the members, so the bound on the walk holds nothing:\n%s",
					subjects[mains], plan.plans[mains])
			}
			requireWalkReadsIssuesOnce(t, plan.plans[statement], subjects[statement])
			requireWalkWithinMains(t, plan.plans[statement], subjects[statement], mainsWalk)
			requireMemberIssueReadsIssuesOnce(t, plan.plans[statement], subjects[statement])
			requireNewestEventProbes(t, keys, plan.plans[statement], subjects[statement], 0)
			for _, control := range []struct {
				text       int
				cannotTell string
				check      func(t testing.TB, plan json.RawMessage, subject string)
			}{
				{keyed, "a walk that reads issues for each member row from one that reads it once", requireWalkReadsIssuesOnce},
				{keyed, "a walk that reads more shared buffers than main's from one that reads no more", func(t testing.TB, plan json.RawMessage, subject string) {
					requireWalkWithinMains(t, plan, subject, mainsWalk)
				}},
				{probed, "a member_issue that probes issues_pkey once for each member issue from one that reads issues in one scan", requireMemberIssueReadsIssuesOnce},
				{statement, "a walk that reads the whole of issues from one keyed through an index", requireEveryScanKeyed},
			} {
				subject, controlPlan := subjects[control.text], plan.plans[control.text]
				explaintest.RequireControlFails(t, subject, "a control", control.cannotTell, controlPlan,
					func(t testing.TB) { control.check(t, controlPlan, subject) })
			}
		}
	}
}

// newestEventWalk is the plan of the walk over the members in plan, an analyzed plan of a text of
// the newest event: the subplan newestEventWalkCTE (planSubplan). The walk is told by its place in
// the plan rather than by the tables it reads, so a read of issues beside the walk is never taken
// for the walk's own.
func newestEventWalk(t testing.TB, plan json.RawMessage, subject string) json.RawMessage {
	t.Helper()
	return planSubplan(t, plan, newestEventWalkCTE, subject)
}

// newestEventWalkBuffers is the shared buffers, hit plus read, that the walk over the members in
// plan reads (newestEventWalk): those of the walk's top node, which count every node under it.
func newestEventWalkBuffers(t testing.TB, plan json.RawMessage, subject string) float64 {
	t.Helper()
	top := explaintest.Nodes(t, newestEventWalk(t, plan, subject))[0]
	return top.SharedHitBlocks + top.SharedReadBlocks
}

// requireWalkReadsIssuesOnce holds the walk over the members in plan (newestEventWalk) to one
// read of issues: one scan of the table, which runs once. Main's walk hashes issues from that one
// read and joins each step's members to the hash; a walk that probes issues for each member row
// runs its scan once for each.
func requireWalkReadsIssuesOnce(t testing.TB, plan json.RawMessage, subject string) {
	t.Helper()
	var reads []explaintest.Scan
	for _, scan := range explaintest.Scans(t, newestEventWalk(t, plan, subject)) {
		if scan.Relation == "issues" {
			reads = append(reads, scan)
		}
	}
	if len(reads) != 1 {
		t.Errorf("%s scans issues %d times in its walk over the members (%s), want one scan:\n%s",
			subject, len(reads), newestEventWalkCTE, plan)
		return
	}
	if reads[0].Loops != 1 {
		t.Errorf("%s runs its scan of issues in its walk over the members (%s) %v times, want once:\n%s",
			subject, newestEventWalkCTE, reads[0].Loops, plan)
	}
}

// requireWalkWithinMains holds the walk over the members in plan (newestEventWalk) to reading no
// more shared buffers, hit plus read, than mainsWalk, those main's walk reads under the same plan
// over the same scope, beside the read of the relations the walk counts in its own
// (newestEventWalkRelationsBuffers): main's walk reads its relations by the subscripts of two
// arrays and so reads no relation.
func requireWalkWithinMains(t testing.TB, plan json.RawMessage, subject string, mainsWalk float64) {
	t.Helper()
	relations := newestEventWalkRelationsBuffers(t, plan, subject)
	walk := newestEventWalkBuffers(t, plan, subject) - relations
	t.Logf("%s reads %v shared buffers in its walk over the members beside %v in its read of the relations, and main's walk %v",
		subject, walk, relations, mainsWalk)
	if walk > mainsWalk {
		t.Errorf("%s reads %v shared buffers in its walk over the members (%s) beside its read of the relations, more than the %v of main's walk under the same plan, want at most main's:\n%s",
			subject, walk, newestEventWalkCTE, mainsWalk, plan)
	}
}

// newestEventWalkRelationsBuffers is the shared buffers, hit plus read, that the walk over the
// members in plan counts in its own (newestEventWalk) for the read of the relations it is seeded
// from. A node's buffers count every read made while it runs, so where the walk's scan of
// members_seed runs that CTE first, it counts the CTE's own buffers; a scan of the CTE that
// another node ran first counts none of them, so the walk's scan counts at most the CTE's own
// buffers.
func newestEventWalkRelationsBuffers(t testing.TB, plan json.RawMessage, subject string) float64 {
	t.Helper()
	seed := 0.0
	if seeds := planSubplans(t, plan, "CTE members_seed", subject); len(seeds) == 1 {
		top := explaintest.Nodes(t, seeds[0])[0]
		seed = top.SharedHitBlocks + top.SharedReadBlocks
	}
	relations := 0.0
	for _, node := range explaintest.Nodes(t, newestEventWalk(t, plan, subject)) {
		if node.Type == "CTE Scan" && node.CTE == "members_seed" {
			relations += min(node.SharedHitBlocks+node.SharedReadBlocks, seed)
		}
	}
	return relations
}
