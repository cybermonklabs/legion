package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The start of a child's tester step passes its fences, and while it provisions the workspace,
// before it stores its new claim, the child leaves the workflow at done and the leave commits. The
// leave read the issue's claims before the new one existed, so no close names it. The start reads
// the issue again after it stores the claim, finds it gone from the workflow, and retires the claim,
// which has run nothing: the runtime is asked no spawn, and the tree's cleanup does not wait for
// it.
func TestAStartBetweenItsReadAndItsCreateWhenTheLeaveCommitsRetiresItsClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	parent := "LEGION-208"
	root := record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: parent, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: parent, Parent: &parent, Title: "Child", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, root)
	putOutboxIssue(t, pool, records, child)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	// The claims are stored as the daemon stores them, so the record's claims are what the tree's
	// cleanup reads (store's pendingTreeClaims: every claim of the tree not retired).
	trees := outboxTreeStore(t, pool, parent)
	sup.deps.Store, sup.store = trees, trees
	provisioning, released := make(chan struct{}), make(chan struct{})
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
		trees: trees, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			close(provisioning)
			<-released
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-209"}, nil
		},
	}
	token := mustClaimToken(t, child.Key, claim.RoleTester)

	start := mustOutboxRow(t, child.Key, record.SuperviseRequest{Op: "start", Tree: parent, Role: claim.RoleTester, Phase: phase.Testing,
		Task: "Test it.", Generation: 1}, time.Now())
	start.ID = 80
	done := make(chan error, 1)
	go func() { done <- runner.execute(ctx, start) }()
	<-provisioning
	// The child's leave at done commits while the start provisions.
	if _, err := pool.Exec(ctx, `update issues set status = 'done', phase = 'done' where key = $1`, child.Key); err != nil {
		t.Fatalf("leave the child: %v", err)
	}
	close(released)
	if err := <-done; err != nil {
		t.Fatalf("the start: %v", err)
	}

	machine, found := sup.Machine(token)
	if !found {
		t.Fatalf("the start stored no claim %s: the case's premise", token)
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Errorf("the start's new claim of an issue that left is %s, want retired", got)
	}
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Errorf("the runtime was asked %d spawns, want none", len(spawns))
	}
	var pending []string
	rows, err := pool.Query(ctx, `select token from claims where project = 'legion' and tree = $1 and state <> 'retired' order by token`, parent)
	if err != nil {
		t.Fatalf("read the tree's pending claims: %v", err)
	}
	for rows.Next() {
		var pendingToken string
		if err := rows.Scan(&pendingToken); err != nil {
			t.Fatalf("read a pending claim: %v", err)
		}
		pending = append(pending, pendingToken)
	}
	rows.Close()
	var stored int
	if err := pool.QueryRow(ctx, `select count(*) from claims where token = $1`, string(token)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("the record holds %d rows of %s (%v), want the one the start stored: the case's premise", stored, token, err)
	}
	if len(pending) != 0 {
		t.Errorf("the tree's cleanup waits for %v, want nothing: the start's claim of an issue that left must not hold it", pending)
	}
}

// admittedHook is the daemon's claim store with a hook that runs after each claim it admits is
// stored (supervise.Store.AdmitClaim), the write a start's create of its claim makes.
type admittedHook struct {
	*store.Store
	admitted func(supervise.Claim)
}

func (s admittedHook) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	bound, err := s.Store.AdmitClaim(ctx, c)
	if err == nil {
		s.admitted(bound)
	}
	return bound, err
}

// The start's read of the issue after it stores its claim is what retires the claim of an issue
// that left the workflow between the start's fences and that store. Where that read itself fails,
// the try ends with the claim stored and nothing retired, and the row runs again. Its retry finds
// the claim, which no start has recorded and which has run nothing, reads the issue again for it,
// and retires it, since the issue has left: the runtime is asked no spawn, and the tree's cleanup
// does not wait for it. Here the child's leave at done commits as the first try stores the claim,
// and that try is cut short there, so its read fails. The fault: the read was made only on the try
// that made the claim, the retry finished at its fences without acting, and the claim, which no
// close names, held the tree's cleanup.
func TestAStartWhoseReadAfterItsCreateFailsRetiresItsClaimOnItsRetry(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	parent := "LEGION-208"
	root := record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: parent, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: parent, Parent: &parent, Title: "Child", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, root)
	putOutboxIssue(t, pool, records, child)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	trees := outboxTreeStore(t, pool, parent)
	token := mustClaimToken(t, child.Key, claim.RoleTester)
	first, cut := context.WithCancel(ctx)
	defer cut()
	var once sync.Once
	sup.deps.Store, sup.store = admittedHook{Store: trees, admitted: func(c supervise.Claim) {
		if c.Token != token {
			return
		}
		once.Do(func() {
			if _, err := pool.Exec(ctx, `update issues set status = 'done', phase = 'done' where key = $1`, child.Key); err != nil {
				t.Errorf("leave the child: %v", err)
			}
			cut()
		})
	}}, trees
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
		trees: trees, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-209"}, nil
		},
	}
	start := mustOutboxRow(t, child.Key, record.SuperviseRequest{Op: "start", Tree: parent, Role: claim.RoleTester, Phase: phase.Testing,
		Task: "Test it.", Generation: 1}, time.Now())
	start.ID = 80

	if err := runner.execute(first, start); err == nil {
		t.Fatal("the first try = nil, want it to fail at its read of the issue after it stored its claim")
	}
	machine, found := sup.Machine(token)
	if !found {
		t.Fatalf("the first try stored no claim %s: the case's premise", token)
	}
	if got := machine.Claim(); got.State != supervise.StateQueued {
		t.Fatalf("the first try left the claim %s, want it queued: the case's premise", got.State)
	}

	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Errorf("the retry left the claim of an issue that left %s, want it retired", got)
	}
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Errorf("the runtime was asked %d spawns, want none", len(spawns))
	}
	var pending int
	if err := pool.QueryRow(ctx, `select count(*) from claims where project = 'legion' and tree = $1 and state <> 'retired'`, parent).Scan(&pending); err != nil {
		t.Fatalf("read the tree's pending claims: %v", err)
	}
	if pending != 0 {
		t.Errorf("the tree's cleanup waits for %d claims, want none: the start's claim of an issue that left must not hold it", pending)
	}
}
