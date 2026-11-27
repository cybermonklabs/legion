package daemon

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A run of provider errors of a phase worker's claim tells the issue's architect and the controller
// through two outbox rows, a notice and a controller notice, and each row's lane is woken only once
// both rows have committed. The notices are written on a goroutine of their own, outside any fact
// (providerErrorRunReached), so their transaction has to open the commit scope itself. Without that
// scope the records' wake runs at the write, inside the transaction (projectRecords.wakeOnCommit):
// the lane looks before the row is there, finds nothing, and the notice waits for the outbox's
// sweep, which runs every outboxLease.
func TestAProviderErrorsNoticeWakesItsLaneOnlyOnceItsRowCommits(t *testing.T) {
	t.Parallel()
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
		if err := pool.QueryRow(ctx, `select count(*) from outbox where (kind = $1 or kind = $2) and payload->>'kind' = 'provider-errors'`,
			string(record.OutboxKindNotice), string(record.OutboxKindControllerNotice)).Scan(&rows); err != nil {
			t.Errorf("count the provider-errors notices: %v", err)
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

	testwait.Eventually(t, "the two provider-errors notices to commit and wake their lanes", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seenAtWake) == 2 && notices() == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if len(seenAtWake) != 2 || seenAtWake[0] != 2 || seenAtWake[1] != 2 {
		t.Fatalf("the lanes were woken %d times, seeing %v committed notices, want twice, each with both notices committed", len(seenAtWake), seenAtWake)
	}
}

// A run of provider errors of a phase worker reaches the issue's architect on its own role topic,
// as every notice of the issue does (deliverNotice), and the project's controller on the
// controller topic, each as `provider-errors on <issue>` and each under its own row's key. The
// architect is who narrows the work the provider keeps refusing, and the controller is who
// compacts or refreshes the claim's session, so the controller relays nothing
// (#4065). A run of the tree's architect's own claim reaches the controller
// alone: the architect copy would wake the very session the provider keeps refusing, for one more
// failing turn, and the controller holds that work. A run of a child's own architect's claim
// reaches the architect above that child, here the root's, and never the child's own architect,
// which still holds its role, so the controller relays nothing for it either.
func TestAProviderErrorsNoticeReachesTheIssuesArchitectAndTheController(t *testing.T) {
	t.Parallel()
	const reason = "6 turns in a row on claude-opus-4-8 ended in the provider's reasoning_extraction content filter"
	for _, tc := range []struct {
		name      string
		issue     string
		role      claim.Role
		architect string // whose architect topic hears it; "" for none
	}{
		{name: "a phase worker's run", issue: "LEGION-1", role: claim.RoleImplementer, architect: "LEGION-1"},
		{name: "the tree architect's own run", issue: "LEGION-1", role: claim.RoleArchitect},
		{name: "a child's own architect's run", issue: "LEGION-2", role: claim.RoleArchitect, architect: "LEGION-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			ctx := context.Background()
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			st, err := store.Open(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatalf("open the store: %v", err)
			}
			defer st.Close()
			token, err := claim.ProjectToken("LEGION")
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
			log := quietLogger()
			projected := projectRecords{Store: records, project: "LEGION", log: log}
			engine := workflow.New(projected, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, log)
			admission := admit.New(projected, engine, 1, "LEGION", log)
			w := &workflowRuntime{pool: pool, records: projected, admission: admission, engine: engine,
				handlers: []intake.Handler{engine, admission}, dispatchProject: "LEGION", log: log}

			w.providerErrorRunReached(supervise.Claim{Token: mustClaimToken(t, tc.issue, tc.role), Project: "legion", Tree: "LEGION-1",
				Issue: tc.issue, Role: tc.role, Generation: 1}, reason)
			// The rows are one transaction, so once the controller's row is there every row is.
			testwait.Eventually(t, "the provider-errors notices to commit", func() bool {
				var rows int
				if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = $1`, string(record.OutboxKindControllerNotice)).Scan(&rows); err != nil {
					t.Errorf("count the controller notices: %v", err)
				}
				return rows == 1
			})

			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			if tc.issue != "LEGION-1" {
				architectClaim(t, sup, tc.issue, supervise.StateWorking)
			}
			publisher := &holderPublisher{}
			runner := &outbox{log: log, pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("run the notices: %v", err)
			}

			_, delivered := publisher.snapshot()
			want := record.Notice{Kind: record.ProviderErrorsNotice, Role: tc.role, Reason: reason}
			byTopic := map[string]outboxPublish{}
			for _, published := range delivered {
				byTopic[published.topic] = published
			}
			controller := notify.ControllerTopic("legion")
			topics := []string{controller}
			var architect string
			if tc.architect != "" {
				architect = architectTopic(t, tc.architect)
				topics = append(topics, architect)
			}
			if len(delivered) != len(topics) || len(byTopic) != len(topics) || outboxRows(t, pool) != 0 {
				t.Fatalf("delivered %+v with %d rows left, want one publish on each of %v, none on any other topic, and no row left",
					delivered, outboxRows(t, pool), topics)
			}
			for _, topic := range topics {
				published, ok := byTopic[topic]
				if !ok {
					t.Fatalf("delivered %+v, want a publish on %s", delivered, topic)
				}
				if message := "provider-errors on " + tc.issue; published.message != message || !reflect.DeepEqual(published.payload, want) {
					t.Fatalf("the publish on %s = %q %+v, want %s carrying %+v", topic, published.message, published.payload, message, want)
				}
			}
			if architect != "" && byTopic[architect].key == byTopic[controller].key {
				t.Fatalf("the two publishes share the key %q, want each under its own row's key", byTopic[architect].key)
			}
		})
	}
}
