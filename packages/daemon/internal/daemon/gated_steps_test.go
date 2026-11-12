package daemon

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A boot publishes the design-gate steps it is configured with onto the project's Dispatch row,
// through the outbox: the boot queues one row addressed to the project key and the lane writes
// it. The daemon keeps no record of what it last published, so a boot that finds its own row
// still unpublished queues no second; and because that row can have been queued by a boot on a
// list the operator has changed since, the boot writes its own list into it. A boot after the
// row ran publishes again — which is what lets a deployment that changed its list have the new
// one written without anyone asking.
func TestGatedStepsPublishAtBoot(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	boot := &workflowRuntime{pool: pool, records: records, dispatchProject: "LEGION",
		gatedSteps: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}}

	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the first boot's publish: %v", err)
	}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("a second boot's publish: %v", err)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows = %d, want the one publish the second boot found pending", rows)
	}

	client := &outboxDispatch{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client,
		now: func() time.Time { return time.Now().UTC() }, log: quietLogger()}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the publish: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v, the configured list in the configured order", client.gatedSteps, want)
	}
	if want := []string{"LEGION"}; !reflect.DeepEqual(client.gatedStepsOf, want) {
		t.Fatalf("the publish went to %v, want %v, the one project this daemon serves", client.gatedStepsOf, want)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("outbox rows = %d after the publish ran, want none", rows)
	}

	// An operator changes the list while the row of the boot before is still waiting for the
	// lane. The row waits for as long as Dispatch is down or has yet to take the route, so this
	// boot writes its own list into it rather than leaving the stale one to be published.
	boot.gatedSteps = []record.DesignStep{record.DesignStepSpec}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the boot whose publish waits: %v", err)
	}
	boot.gatedSteps = []record.DesignStep{record.DesignStepCriteria}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the boot on the changed list: %v", err)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows = %d, want the one publish carrying the list of the boot that ran last", rows)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the publish of the changed list: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}, {"criteria"}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v: the boot on a changed list publishes its own", client.gatedSteps, want)
	}

	// The next boot gates nothing, and publishes that rather than leaving the stale list standing.
	boot.gatedSteps = []record.DesignStep{}
	if err := boot.queueGatedStepsPublish(ctx); err != nil {
		t.Fatalf("the boot that gates nothing: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the third publish: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}, {"criteria"}, {}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v: a deployment that gates nothing publishes the empty list", client.gatedSteps, want)
	}
}

// The publish executor refuses a runner with no Dispatch client rather than dropping the row, as
// every other Dispatch write of the outbox does: the row is retried once a client is there.
func TestGatedStepsPublishWithoutADispatchClientIsRefused(t *testing.T) {
	row := mustOutboxRow(t, "LEGION", record.GatedStepsPublish{Project: "LEGION", Steps: []record.DesignStep{record.DesignStepSpec}}, time.Now())
	err := (&outbox{}).execute(context.Background(), row)
	if err == nil || err.Error() != "gated steps executor has no Dispatch client" {
		t.Fatalf("execute without a Dispatch client = %v, want the refusal naming the missing client", err)
	}
}

// designGatesPublications is every design-gates publication the outbox holds, by the issue it is
// addressed to, with the row's id: the kind's unique partial index allows one row per issue, so a
// second pass that queued beside a pending row would be read here as a changed id, and a second
// row of one issue could not be written at all.
func designGatesPublications(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	queued := map[string]int64{}
	rows, err := pool.Query(context.Background(), "select issue, id from outbox where kind = $1", string(record.OutboxKindDesignGatesPublish))
	if err != nil {
		t.Fatalf("read the queued design-gates publications: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue string
		var id int64
		if err := rows.Scan(&issue, &id); err != nil {
			t.Fatalf("read the queued design-gates publications: %v", err)
		}
		queued[issue] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queued design-gates publications: %v", err)
	}
	return queued
}

// Every gate of a deployment that upgrades into the design-gates route was written before the
// kind existed, so nothing has published any of them: without the boot pass Dispatch would learn
// an issue's documents only once something moved that set again, which for a tree already waiting
// on its approval is never. The boot queues one publication for each recorded issue that holds a
// gate (daemon.queueRecordedGatesPublish), carrying the documents its own gate rows record.
//
// A done issue is left out: a gate is live for Dispatch's advice only while the issue holding it
// is not done there, and a closed tree's close deletes no gate row, so publishing its set would
// have Dispatch print a clause for a gate that holds nothing back. The status read is what
// Dispatch last showed, not what the daemon means by the issue. An issue with no gate is left out
// for the plainer reason that it has no set.
//
// It queues at every boot, because the daemon keeps no record of what it last published, so a
// restart loop must cost neither a second row nor a Dispatch write: the enqueue writes nothing
// for a row whose payload is already what it would write (record.Postgres.EnqueueDesignGates).
func TestBootQueuesEveryRecordedGatePublication(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	boot := &workflowRuntime{pool: pool, records: records, dispatchProject: gatePublishProject}
	const gated, done, ungated = "LEGION-301", "LEGION-302", "LEGION-303"
	criteria := record.DesignGateDocument{Step: record.DesignStepCriteria, ArtifactID: "artifact-criteria"}
	spec := record.DesignGateDocument{Step: record.DesignStepSpec, ArtifactID: "artifact-spec"}
	approved := 1
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, seed := range []struct {
			key   string
			shown string
			pairs []record.DesignGateDocument
		}{
			{key: gated, shown: "in_progress", pairs: []record.DesignGateDocument{criteria, spec}},
			{key: done, shown: record.DoneStatus, pairs: []record.DesignGateDocument{spec}},
			{key: ungated, shown: "in_progress"},
		} {
			if err := records.PutIssue(ctx, tx, record.Issue{Key: seed.key, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: gatePublishProject, Title: seed.key, Tree: seed.key,
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: seed.shown,
				Rank: seed.key, HandedOver: true, LastDispatchSeq: 1}); err != nil {
				return err
			}
			for _, pair := range seed.pairs {
				if err := records.PutGate(ctx, tx, record.DesignGate{Issue: seed.key, Step: pair.Step,
					ArtifactID: pair.ArtifactID, LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the recorded gates: %v", err)
	}

	if err := boot.queueRecordedGatesPublish(ctx); err != nil {
		t.Fatalf("the first boot's pass: %v", err)
	}
	queued := designGatesPublications(t, pool)
	if len(queued) != 1 {
		t.Fatalf("the boot queued publications for %v, want the one gated issue %s Dispatch does not show done", queued, gated)
	}
	row, ok := queued[gated]
	if !ok {
		t.Fatalf("the boot queued publications for %v, want one for %s", queued, gated)
	}

	// A restart loop: the same record read again queues no second row, and writes nothing into
	// the one still pending, which is the row the lane goes on to run.
	if err := boot.queueRecordedGatesPublish(ctx); err != nil {
		t.Fatalf("a second boot's pass: %v", err)
	}
	if second := designGatesPublications(t, pool); !reflect.DeepEqual(second, map[string]int64{gated: row}) {
		t.Fatalf("a second boot left publications %v, want the one row %d of %s the first boot queued", second, row, gated)
	}

	client := &gatePublishDispatch{}
	runner := &outbox{pool: pool, records: records, dispatch: client, dispatchProject: gatePublishProject,
		now: func() time.Time { return time.Now().UTC() }, log: quietLogger()}
	if drained, _ := runner.drain(ctx, "", []record.OutboxKind{record.OutboxKindDesignGatesPublish}); !drained {
		t.Fatal("the design-gates lane did not drain")
	}
	want := []gatePublished{{Issue: gated, Gates: wire(criteria, spec)}}
	if got := client.taken(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the boot's publications reached Dispatch as %+v, want %+v: each issue's own documents, and nothing for a done or an ungated issue", got, want)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("the outbox holds %d rows after the lane ran, want none", rows)
	}
}
