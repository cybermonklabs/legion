package daemon

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// storeWake is one write of record.Store that queues or moves an outbox row, made through the
// workflow's store (projectRecords) in one transaction, and the lanes its commit wakes, each as
// "<issue>:<kind>": none for a write whose lane something else wakes.
type storeWake struct {
	write func(ctx context.Context, tx pgx.Tx, records record.Store) error
	wakes []string
}

// Every write of record.Store that queues an outbox row or makes one due, which its name says
// (Enqueue…, Expedite…), wakes the row's lane once its transaction commits when it is made through
// the workflow's store (projectRecords), and wakes nothing before: a lane is woken by the commit
// that writes its row, and a row whose write woke nothing waits for the next sweep, up to one
// outbox lease. projectRecords wraps record.Store by embedding it, so a write it does not wrap
// itself reaches the bare store and wakes nothing, with no error to say so: the start of a fix
// (EnqueueFixStart) and a prompt to read Dispatch (EnqueueReadPrompt) were two such writes. Each
// write of that name has its case here, and a write the store gains without one fails the test,
// naming it. A prompt joined to a pending one wakes nothing more, since the pending row's write
// woke its lane. The gated steps' publication is queued at the boot, before the outbox runs, and
// Run's first sweep runs it (queueGatedStepsPublish), so its write wakes nothing.
func TestEveryOutboxRowTheWorkflowsStoreQueuesWakesItsLane(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	base := record.NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	message := func(issue string, at time.Time) record.OutboxRow {
		return mustOutboxRow(t, issue, record.MessagePost{Body: "posted"}, at)
	}
	start := func(issue string, fix int) record.OutboxRow {
		return mustOutboxRow(t, issue, record.SuperviseRequest{Op: "start", Tree: issue, Role: claim.RoleImplementer, Generation: 1,
			Phase: phase.Implementing, Entry: 1, Task: "Implement it.", Fix: fix}, now)
	}
	prompt := func(issue string) record.CINews {
		reader := record.CIReader{Phase: phase.Reviewing, Entry: 4}
		return record.CINews{Purpose: record.CINewsState, Role: string(mustClaimToken(t, issue, claim.RoleReviewer)), Generation: 1,
			Task: "default", Repo: "acme/widgets", PullRequest: 42, Head: ciHeadX, Reader: &reader,
			States: []record.CIStateSnapshot{{Check: "k-ci", State: "no", Seq: 1}}}
	}
	cases := map[string]storeWake{
		"Enqueue": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			return records.Enqueue(ctx, tx, message("LEGION-301", now))
		}, []string{"LEGION-301:dispatch_message"}},
		"EnqueueID": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			_, err := records.EnqueueID(ctx, tx, message("LEGION-302", now))
			return err
		}, []string{"LEGION-302:dispatch_message"}},
		"EnqueueStart": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			return records.EnqueueStart(ctx, tx, start("LEGION-303", 0))
		}, []string{"LEGION-303:supervise"}},
		"EnqueueFixStart": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			return records.EnqueueFixStart(ctx, tx, start("LEGION-304", 1))
		}, []string{"LEGION-304:supervise"}},
		"EnqueueReadPrompt": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			for i, want := range []bool{false, true} {
				joined, err := records.EnqueueReadPrompt(ctx, tx, "LEGION-305", prompt("LEGION-305"), now)
				if err != nil {
					return err
				}
				if joined != want {
					t.Errorf("prompt %d joined = %t, want %t: the second joins the first", i+1, joined, want)
				}
			}
			return nil
		}, []string{"LEGION-305:ci_news"}},
		"EnqueueDesignGates": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			return records.EnqueueDesignGates(ctx, tx, record.DesignGatesPublish{Issue: "LEGION-306", Gates: record.GateDocuments(nil, nil)}, now)
		}, []string{"LEGION-306:design_gates_publish"}},
		"EnqueueGatedSteps": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			return records.EnqueueGatedSteps(ctx, tx, record.GatedStepsPublish{Project: "LEGION", Steps: []record.DesignStep{record.DesignStepSpec}}, now)
		}, nil},
		"ExpediteOutbox": {func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			later := message("LEGION-308", now.Add(time.Hour))
			id, err := base.EnqueueID(ctx, tx, later)
			if err != nil {
				return err
			}
			later.ID = id
			moved, err := records.ExpediteOutbox(ctx, tx, later, now)
			if err == nil && !moved {
				t.Errorf("the expedite of a row due in an hour moved nothing, want it moved")
			}
			return err
		}, []string{"LEGION-308:dispatch_message"}},
	}

	var named []string
	storeType := reflect.TypeFor[record.Store]()
	for i := range storeType.NumMethod() {
		if name := storeType.Method(i).Name; strings.HasPrefix(name, "Enqueue") || strings.HasPrefix(name, "Expedite") {
			named = append(named, name)
		}
	}
	if len(named) == 0 {
		t.Fatalf("UNKNOWN: record.Store holds %d methods and none is named Enqueue… or Expedite…, so nothing was held", storeType.NumMethod())
	}
	for _, name := range named {
		if _, ok := cases[name]; !ok {
			t.Errorf("record.Store.%s queues or moves an outbox row, as its name says, and no case here holds whether the workflow's store wakes its lane", name)
		}
	}
	for name := range cases {
		if !slices.Contains(named, name) {
			t.Errorf("the case %s names no write of record.Store", name)
		}
	}

	for _, name := range named {
		c, ok := cases[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			var woken []string
			records := projectRecords{Store: base, project: "LEGION", log: quietLogger(), wake: func(issue string, kind record.OutboxKind) {
				woken = append(woken, issue+":"+string(kind))
			}}
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				if err := c.write(ctx, tx, records); err != nil {
					return err
				}
				if len(woken) != 0 {
					t.Errorf("%s woke %v before its transaction committed, want nothing until the commit", name, woken)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !slices.Equal(woken, c.wakes) {
				t.Fatalf("the commit of %s through the workflow's store woke %v, want %v", name, woken, c.wakes)
			}
		})
	}
}
