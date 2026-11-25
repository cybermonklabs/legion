//go:build !race

package api

import (
	"math"
	"testing"
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
			_, read[index] = served.warm(t, target)
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
