package daemon

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// gating is the daemon's settings holder with `gates.design` naming steps, which is where the
// boot reads the list it publishes (workflowRuntime.gatedSteps). No step named is the
// deployment that gates nothing, `gates.design: []`, and not the unset key: the list is passed
// non-nil, since a nil one is the unset key and resolves to the shipped single step.
func gating(steps ...record.DesignStep) *config.Live {
	if steps == nil {
		steps = []record.DesignStep{}
	}
	return config.Settled(config.Config{Gates: config.Gates{Design: steps}})
}

// bootSteps is the list a boot on live would publish: what the boot reads from the settings
// holder and hands the queue (openWorkflow).
func bootSteps(w *workflowRuntime) []record.DesignStep { return w.gatedSteps() }

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
		live: gating(record.DesignStepCriteria, record.DesignStepSpec)}

	if err := boot.queueGatedStepsPublish(ctx, bootSteps(boot)); err != nil {
		t.Fatalf("the first boot's publish: %v", err)
	}
	if err := boot.queueGatedStepsPublish(ctx, bootSteps(boot)); err != nil {
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
	boot.live = gating(record.DesignStepSpec)
	if err := boot.queueGatedStepsPublish(ctx, bootSteps(boot)); err != nil {
		t.Fatalf("the boot whose publish waits: %v", err)
	}
	boot.live = gating(record.DesignStepCriteria)
	if err := boot.queueGatedStepsPublish(ctx, bootSteps(boot)); err != nil {
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
	boot.live = gating()
	if err := boot.queueGatedStepsPublish(ctx, bootSteps(boot)); err != nil {
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

// A `gates.design` change made while the daemon runs reaches the project's Dispatch row. That
// row is what tells a person what the deployment holds work for (`GET /api/v1/projects`,
// record.GatedStepsPublish, docs/running-legion.md), and the gate itself is read per decision,
// so until this the daemon gated the new steps from the moment the change settled while the
// row still showed the list the daemon booted on — until someone restarted it.
//
// The change goes through the daemon's own path: the settings holder applies it, its observers
// run (config.Live.Apply), and the publish the change queues is run by the outbox as the boot's
// own is — on the wake the commit that wrote the row sends the row's lane
// (projectRecords.EnqueueGatedSteps), which is what has the row published rather than waiting
// on the lanes' next sweep (queueGatedStepsPublish). The lanes run on a clock this test holds
// still and fires nothing, so the sweep the boot armed is the only one there is and it never
// runs: a publish that lands here landed on the wake alone.
func TestALiveDesignGateChangeIsPublishedToTheProjectRow(t *testing.T) {
	live := liveSettings(t, "gates:", "  design: [spec]")
	project := live.Snapshot().Project
	pool := isolatedOutboxPool(t)
	clock := microClock()
	// The holder writes its row due at the wall clock (queueGatedStepsPublish), so the lanes'
	// held clock stands a minute past it and the row is due the moment its lane looks. What
	// this test turns on is what wakes that lane, not when a row falls due.
	clock.advance(time.Minute)
	client := &outboxDispatch{}
	var logged syncBuffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	runner := &outbox{pool: pool, dispatchProject: project, dispatch: client, log: log}
	// The store the daemon's workflow writes through, which wakes the row's lane as the write
	// commits; the holder writes its publish through this one.
	runner.records = projectRecords{Store: record.NewStore(), project: project, wake: runner.wake, log: log}
	w := &workflowRuntime{pool: pool, records: runner.records, dispatchProject: project, live: live, log: log}
	rebuild := newRebuilder(log, live)
	live.OnChange(rebuild.observe)
	rebuild.register(newGatedStepsHolder(w))
	stop := runLanes(t, runner, clock)

	apply(t, live, map[string]any{"gates.design": []any{"criteria", "spec"}})

	testwait.Eventually(t, "the publish the change queued ran on its lane", func() bool {
		return outboxRows(t, pool) == 0
	})
	// Joining the lanes is what makes what the lane recorded this goroutine's to read.
	stop()
	if want := [][]string{{"criteria", "spec"}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v: the list the change settled, published without a restart", client.gatedSteps, want)
	}
	if want := []string{project}; !reflect.DeepEqual(client.gatedStepsOf, want) {
		t.Fatalf("the publish went to %v, want %v, the one project this daemon serves", client.gatedStepsOf, want)
	}
	if _, sweeps := clock.armed(outboxLease); sweeps != 1 {
		t.Fatalf("sweep timers armed = %d, want the one Run armed and never fired: the publish ran on the wake of its own commit", sweeps)
	}
	if strings.Contains(logged.String(), "written outside a commit scope") {
		t.Fatalf("the change's publish was written outside a commit scope, so its lane was woken before the row was there:\n%s", logged.String())
	}
}

// blockingGatedSteps is the outbox's Dispatch client with its gated-steps publish held open:
// it reports the list the lane is sending and returns only once the test releases it, which is
// how a change made while a publish is in flight is driven.
type blockingGatedSteps struct {
	*outboxDispatch
	sending chan []string
	release chan struct{}
}

func (d *blockingGatedSteps) PutGatedSteps(ctx context.Context, project string, steps []string) error {
	d.sending <- append([]string(nil), steps...)
	<-d.release
	return d.outboxDispatch.PutGatedSteps(ctx, project, steps)
}

// A `gates.design` change made while the lane is publishing the list before it is not lost.
// The lane read the payload when it claimed the row and is sending that list; the change
// replaces the row's payload under it (record.Store.EnqueueGatedSteps), and the lane's finish
// would then delete by id alone the row that now carries the new list — published to nobody,
// with Dispatch left showing the list the publish in flight carried and no row behind it.
//
// The enqueue therefore takes the row from the lane holding it, rotating the lease token the
// finish, the retry and the renewal each match on, so the row waits to be claimed again with
// the list the change settled. Before the live holder there was no such window: the only
// enqueue was the boot's, which ran before the outbox did.
func TestALiveDesignGateChangeDuringAPublishIsNotLost(t *testing.T) {
	ctx := context.Background()
	live := liveSettings(t, "gates:", "  design: [criteria, spec]")
	project := live.Snapshot().Project
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	w := &workflowRuntime{pool: pool, records: records, dispatchProject: project, live: live, log: quietLogger()}
	rebuild := newRebuilder(w.log, live)
	live.OnChange(rebuild.observe)
	rebuild.register(newGatedStepsHolder(w))
	if err := w.queueGatedStepsPublish(ctx, bootSteps(w)); err != nil {
		t.Fatalf("the boot's publish: %v", err)
	}

	client := &blockingGatedSteps{outboxDispatch: &outboxDispatch{},
		sending: make(chan []string, 1), release: make(chan struct{})}
	started := time.Now().UTC()
	running := &outbox{pool: pool, dispatchProject: project, records: records, dispatch: client,
		now: func() time.Time { return started }, log: quietLogger()}
	ran := make(chan error, 1)
	go func() { ran <- running.RunOnce(ctx) }()
	if sending := <-client.sending; !reflect.DeepEqual(sending, []string{"criteria", "spec"}) {
		t.Fatalf("the lane is sending %v, want the boot's own list", sending)
	}

	// The operator narrows the gate while that publish is in flight.
	apply(t, live, map[string]any{"gates.design": []any{"criteria"}})
	close(client.release)
	if err := <-ran; err != nil {
		t.Fatalf("run the publish the lane was holding: %v", err)
	}

	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows = %d once the publish in flight finished, want the one row carrying the list the change settled", rows)
	}
	// The lane claims it again once the lease the enqueue took from it has lapsed.
	later := &outbox{pool: pool, dispatchProject: project, records: records, dispatch: client.outboxDispatch,
		now: func() time.Time { return started.Add(outboxLease + time.Second) }, log: quietLogger()}
	if err := later.RunOnce(ctx); err != nil {
		t.Fatalf("run the publish of the list the change settled: %v", err)
	}
	if want := [][]string{{"criteria", "spec"}, {"criteria"}}; !reflect.DeepEqual(client.gatedSteps, want) {
		t.Fatalf("Dispatch was sent %v, want %v: the list the change settled is published after the one it replaced", client.gatedSteps, want)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("outbox rows = %d once the new list was published, want none", rows)
	}
}
