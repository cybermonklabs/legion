//go:build !race

package api

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What R1 of dispatch://INFRA-5791 reads the priorities read at: 200 reads of each form on one
// connection, after the reads that warm it (prioritiesReadWarmReads), and the 90th percentile of
// them, which the bound of 200 ms is on. The bound is a time, which the race detector stretches,
// so this file is out of the race build and envoy-go's plain step runs it (ci_job_reads.py derives
// that step's list from this tag). The test holds no time: c-both-forms' step holds the bound, on
// the median of three build Jobs' readings.
const (
	prioritiesReadReads    = 200
	prioritiesReadQuantile = 0.9
)

// TestThePrioritiesReadAnswersWithinItsBoundOnEveryPlan reads GET /api/v1/priorities and GET
// /api/v1/priorities?summary=true, with no project as the pages send them, on the corpus of
// production's counts (seedPrioritiesReadCorpus), under each of the two plans a connection runs a
// statement with: the generic plan and the plan made for its values, each forced on the one
// connection of the server's pool (newPrioritiesReadServer), which runs with JIT off as
// production's Cloud SQL does.
//
// For each form it first holds the answer to the counts the corpus was built to hold
// (readCorpusAnswerFaults), because a time of a wrong answer shows nothing. Then it times 200
// reads through the server, sign-in and encoding included, and reads the database's time of each
// statement over them from pg_stat_statements. It holds that the connection ran every statement
// of every read under the plan named (requirePlansRun), and then reads each statement's plan on
// that connection under EXPLAIN (ANALYZE, BUFFERS). For each form and plan it prints, once and
// on the test binary's standard output so the line starts with it, as c-both-forms' step reads it,
//
//	c-both-forms: reading: form=<full|summary> plan=<generic|custom> p50=<ms> p90=<ms> db=<ms>
//
// where db is the database's time a read, executing and planning, summed over the read's
// statements, and beside it each statement's database time, its EXPLAIN time and shared buffers,
// the walk's share of both (prioritiesStatementPlan.walk), each part's time and shared buffers
// (prioritiesStatementPlan.partsRead), and its plan. It fails on a wrong answer, on JIT not off,
// on a database time it cannot read and on a read under another plan.
//
// It also holds the shape of each read, and prints it for each form and plan, as c-both-forms'
// step reads it:
//
//	c-both-forms: shape: form=<full|summary> plan=<generic|custom> statements=<n> round_trips=<n> event_probes=<n>
//
// statements and round_trips are what the server sent for the counted read, as the trace of
// TestPrioritiesReadSpeed counts them (speedTrace), and event_probes the runs of every scan of
// events_issue_key_seq_key in the read's plans (prioritiesStatementPlan.eventProbes). The seed ran
// the fill, so this is the steady state: the test fails where a form sends other than 2
// statements in 2 round trips, and where it probes an event.
func TestThePrioritiesReadAnswersWithinItsBoundOnEveryPlan(t *testing.T) {
	// Not parallel: it times reads with a wall clock and reads shared-buffer counts, and the
	// clock, the cores and Postgres's shared buffers are the whole process's and server's, which
	// the package's parallel tests use at the same time; it runs before them, alone.
	corpus := seedPrioritiesReadCorpus(t)
	t.Log(corpus.census)
	corpus.census.hold(t)
	for _, plan := range prioritiesReadPlans {
		t.Run(plan.called, func(t *testing.T) {
			served := newPrioritiesReadServer(t, corpus.database, plan)
			for _, form := range prioritiesReadForms {
				measurePrioritiesRead(t, corpus, served, form)
			}
		})
	}
}

// measurePrioritiesRead reads form with no project named on served's connection and prints its
// reading.
func measurePrioritiesRead(t *testing.T, corpus readCorpus, served prioritiesReadServer, form prioritiesReadForm) {
	t.Helper()
	target := form.target(nil)
	described := form.called + " under " + served.plan.called
	answer, statements, roundTrips := served.warm(t, target)
	if faults := readCorpusAnswerFaults(decodeBody[model.Priorities](t, answer), form.summary); len(faults) > 0 {
		t.Fatalf("%s answers what the corpus was not built to answer, so its time would show nothing: %v", described, faults)
	}

	ids := prioritiesReadQueryIDs(t, corpus.database, statements)
	before := prioritiesReadStatementTimes(t, corpus.database, ids)
	reads := make([]time.Duration, 0, prioritiesReadReads)
	for range prioritiesReadReads {
		reads = append(reads, served.read(t, target))
	}
	after := prioritiesReadStatementTimes(t, corpus.database, ids)
	served.requirePlansRun(t, statements, prioritiesReadWarmReads+prioritiesReadReads)

	// The database's time of a read is the time its statements took over the reads, which
	// pg_stat_statements must count once a read each: a count of any other number means the
	// reading looked at a statement the reads did not run as one, and its time would be no time.
	database := make([]prioritiesStatementTime, len(statements))
	var executing, planned float64
	for index, statement := range statements {
		counted := prioritiesStatementTime{
			calls:     after[ids[index]].calls - before[ids[index]].calls,
			executing: after[ids[index]].executing - before[ids[index]].executing,
			planned:   after[ids[index]].planned - before[ids[index]].planned,
		}
		if counted.calls != prioritiesReadReads {
			t.Fatalf("cannot read the database's time of %s: pg_stat_statements counted %d calls of %s (query id %d) over %d reads",
				described, counted.calls, statement.name, ids[index], prioritiesReadReads)
		}
		database[index] = counted
		executing += counted.executing
		planned += counted.planned
	}
	plans := make([]prioritiesStatementPlan, len(statements))
	for index, statement := range statements {
		plans[index] = served.explain(t, statement)
	}

	sorted := slices.Clone(reads)
	slices.Sort(sorted)
	p50, p90 := nearestRank(sorted, 0.5), nearestRank(sorted, prioritiesReadQuantile)
	perRead := (executing + planned) / prioritiesReadReads
	fmt.Printf("c-both-forms: reading: form=%s plan=%s p50=%.1f p90=%.1f db=%.1f\n",
		form.name, served.plan.name, readMilliseconds(p50), readMilliseconds(p90), perRead)
	t.Logf("%s: %d reads of %s through the server on one connection after %d warm ones: p50 %.1f ms, p90 %.1f ms, fastest %.1f ms, slowest %.1f ms; the database's time %.1f ms a read, %.1f executing and %.1f planning (pg_stat_statements, %d statements a read)",
		described, len(reads), target, prioritiesReadWarmReads, readMilliseconds(p50), readMilliseconds(p90),
		readMilliseconds(sorted[0]), readMilliseconds(sorted[len(sorted)-1]), perRead,
		executing/prioritiesReadReads, planned/prioritiesReadReads, len(statements))
	for index, statement := range statements {
		plan := plans[index]
		counted := database[index]
		t.Logf("c-both-forms: statement: form=%s plan=%s %s: the database's time %.2f ms a read (%.2f executing, %.2f planning, %d calls); EXPLAIN (ANALYZE, BUFFERS) %.1f ms executing and %.1f planning, %.0f shared buffers (hit %.0f, read %.0f); %s\n%s",
			form.name, served.plan.name, statement.name,
			(counted.executing+counted.planned)/float64(counted.calls), counted.executing/float64(counted.calls), counted.planned/float64(counted.calls), counted.calls,
			plan.milliseconds, plan.planning, plan.buffers(), plan.hit, plan.read, plan.walk(), indentPlan(plan.text))
		t.Logf("c-both-forms: parts: form=%s plan=%s %s: %s", form.name, served.plan.name, statement.name, plan.partsRead())
	}

	probes := 0.0
	for _, plan := range plans {
		probes += plan.eventProbes
	}
	fmt.Printf("c-both-forms: shape: form=%s plan=%s statements=%d round_trips=%d event_probes=%.0f\n",
		form.name, served.plan.name, len(statements), roundTrips, probes)
	wantStatements := priorityBodyStatements
	if form.summary {
		wantStatements = prioritySummaryStatements
	}
	if len(statements) != wantStatements || roundTrips != priorityReadRoundTrips {
		t.Errorf("%s sends %d statements in %d round trips, want %d in %d", form.called, len(statements), roundTrips, wantStatements, priorityReadRoundTrips)
	}
	if probes != 0 {
		t.Errorf("%s under %s probes %s %.0f times, want 0 in steady state", form.called, served.plan.called, newestEventIndex, probes)
	}
}
