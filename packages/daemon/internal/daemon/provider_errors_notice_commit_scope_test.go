package daemon

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A run of provider errors tells the controller through an outbox row, and the row's lane is woken
// only once that row has committed. The notice is written on a goroutine of its own, outside any
// fact (providerErrorRunReached), so its transaction has to open the commit scope itself. Without
// that scope the records' wake runs at the write, inside the transaction
// (projectRecords.wakeOnCommit): the lane looks before the row is there, finds nothing, and the
// notice waits for the outbox's sweep, which runs every outboxLease.
func TestAProviderErrorsNoticeWakesItsLaneOnlyOnceItsRowCommits(t *testing.T) {
	pool := isolatedOutboxPool(t)
	ctx := context.Background()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	// A controller that is registered, since an unregistered one is told nothing (admit's
	// wakeController).
	token, err := claim.ProjectToken("CAPTURE")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.MintController(ctx, token, []byte("capability"))
	if err != nil {
		t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := st.RegisterController(ctx, token, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
		t.Fatalf("register the controller = %t, %v", ok, err)
	}

	notices := func() int {
		var rows int
		if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = $1`, string(record.OutboxKindControllerNotice)).Scan(&rows); err != nil {
			t.Errorf("count the controller notices: %v", err)
		}
		return rows
	}
	var mu sync.Mutex
	var seenAtWake []int
	log := slog.New(slog.DiscardHandler)
	records := projectRecords{Store: record.NewStore(), project: "CAPTURE", log: log, wake: func(string, record.OutboxKind) {
		// What the woken lane would find: the notices committed by the moment of the wake.
		visible := notices()
		mu.Lock()
		seenAtWake = append(seenAtWake, visible)
		mu.Unlock()
	}}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 1, "CAPTURE", log)
	w := &workflowRuntime{pool: pool, records: records, admission: admission, engine: engine,
		handlers: []intake.Handler{engine, admission}, dispatchProject: "CAPTURE", log: log}

	implementer, err := claim.NewToken(token, "CAPTURE-7", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	w.providerErrorRunReached(supervise.Claim{Token: implementer, Project: token, Tree: "CAPTURE-7", Issue: "CAPTURE-7",
		Role: claim.RoleImplementer, Generation: 1}, "three provider errors in a row")

	testwait.Eventually(t, "the provider-errors notice to commit and wake its lane", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seenAtWake) > 0 && notices() == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seenAtWake) != 1 || seenAtWake[0] != 1 {
		t.Fatalf("the lane was woken %d times, seeing %v committed notices, want once with the notice committed", len(seenAtWake), seenAtWake)
	}
}
