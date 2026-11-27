package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// largeTree is the issue count of the large tree the owed route is asked about here: twice the 514
// issues of INFRA's largest tree, read from the INFRA daemon's GET /legion/v1/state on 2026-11-23.
const largeTree = 1024

// owedTimedAsks is how many asks the timed reading counts after its warm-up: an odd count, so the
// median is one ask's time.
const owedTimedAsks = 21

// owedTimingEnabled is the variable that turns the timed reading on; TestMain's guard keeps it
// (guardKeep).
const owedTimingEnabled = "LEGION_OWED_TIMING"

// statementCount counts the statements a pool's connections send: each Exec, Query and QueryRow
// (pgx.QueryTracer), a transaction's begin and commit among them, and each query of a batch
// (pgx.BatchTracer), so a read split into one statement per row is counted however it is sent.
type statementCount struct{ sent atomic.Int64 }

func (c *statementCount) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.sent.Add(1)
	return ctx
}

func (*statementCount) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (*statementCount) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *statementCount) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
	c.sent.Add(1)
}

func (*statementCount) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// serveOwedOver has h's server read the records through records over pool, as the daemon wires the
// owed route: the record's running steps say which role works each phase.
func serveOwedOver(h *harness, pool *store.Pool, records record.Store) {
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Record: records,
	}).Handler
}

// owedChain is a tree of size issues in one chain, each issue the parent of the next, with the
// claims the owner walk weighs on it: the root's architect, which holds its role, an architect the
// daemon retired on every issue below the root, as a child's architect is once the child closes,
// and the planner of the issue at the chain's bottom, which asks what it owes.
//
// It is the shape on which the walk does the most (record.OwningIssue, which OwnerWalk.Architect
// ends): the walk climbs one parent at a time from the asking issue and stops at the first
// architect that holds its role, else at the root. From the bottom of a chain whose architects
// below the root are all retired, it asks about every issue of the tree and passes each over, and
// the read of their claims (record.ArchitectRuns) answers a row for each. A wide tree of the same
// size ends the walk after a few parents.
type owedChain struct {
	size      int
	architect liveClaim
	planner   liveClaim
}

// seedOwedChain records a chain of size issues numbered from LEGION-<first>, its root, with a
// retired architect on each issue below the root, in one transaction, and launches the root's
// architect and the bottom issue's planner.
func seedOwedChain(t *testing.T, h *harness, first, size int) owedChain {
	t.Helper()
	root, bottom := fmt.Sprintf("LEGION-%d", first), fmt.Sprintf("LEGION-%d", first+size-1)
	records := record.NewStore()
	err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var parent *string
		for n := first; n < first+size; n++ {
			key := fmt.Sprintf("LEGION-%d", n)
			if err := records.PutIssue(ctx, tx, record.Issue{
				Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: key, Parent: parent,
				Phase: phase.Planning, Status: "in_progress",
			}); err != nil {
				return err
			}
			if parent != nil {
				token, err := claim.NewToken(testProject, key, claim.RoleArchitect)
				if err != nil {
					return err
				}
				if err := h.store.PutClaimTx(ctx, tx, supervise.Claim{
					Token: token, Project: testProject, Tree: root, Issue: key, Role: claim.RoleArchitect, State: supervise.StateRetired,
				}); err != nil {
					return err
				}
			}
			parent = &key
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed a chain of %d issues from %s: %v", size, root, err)
	}
	return owedChain{
		size:      size,
		architect: newLiveClaim(t, h, root, claim.RoleArchitect),
		planner:   newLiveClaimIn(t, h, root, bottom, claim.RolePlanner),
	}
}

// ask is the planner's ask of what it owes, held to the answer the chain gives — the planning
// phase of its issue, owned by the root's architect — and the time the route took to answer it.
func (c owedChain) ask(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	got := c.planner.owed(t)
	elapsed := time.Since(start)
	if want := (OwedResponse{Owes: true, Work: "the planning phase of " + c.planner.issue, Architect: c.architect.token}); got != want {
		t.Fatalf("the planner at the bottom of a chain of %d issues is told %+v, want %+v", c.size, got, want)
	}
	return elapsed
}

// milliseconds is d in milliseconds, to a fraction of one.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// The owed route reads the claim's tree for the architect that owns it on every ask
// (owningArchitect), and the worker's end-of-turn reminder waits OWED_READ_BOUND_MS, 3 s, for the
// answer before it nudges with "could not read the record" (packages/pi-shared/src/phase-reminder.ts),
// so a read that grew with the tree would nudge at every turn's end on a large one. The owner walk
// reads the tree's issues in one statement and their architects' claims in another
// (record.ReadOwnerWalk), so an ask sends the same statements whatever the tree's size, and only
// their rows grow. Asked from the bottom of a chain (owedChain), the route names the root's
// architect on a chain of 16 issues and on one of 1024, twice INFRA's largest tree, and the pool
// the server reads through counts as many statements for the one ask as for the other.
//
// The suite holds the count and not a time. It runs on a pod that other work loads, where a
// wall-clock bound fails on that load and not on the code, while a read per issue changes the
// count on any machine. Each ask's time is logged, and TestClaimOwedTimingOnAChainOf1024Issues
// reads the time on its own when it is asked to.
func TestClaimOwedSendsAsManyStatementsOnATreeOf1024IssuesAsOn16(t *testing.T) {
	h := newHarness(t)
	counted := &statementCount{}
	config := h.store.Pool().Config()
	config.ConnConfig.Tracer = counted
	traced, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the counted pool: %v", err)
	}
	t.Cleanup(traced.Close)
	serveOwedOver(h, store.NewPool(traced), record.NewStore())

	chains := []owedChain{seedOwedChain(t, h, 100, 16), seedOwedChain(t, h, 1000, largeTree)}
	sent := make([]int64, len(chains))
	for i, chain := range chains {
		counted.sent.Store(0)
		elapsed := chain.ask(t)
		sent[i] = counted.sent.Load()
		t.Logf("owed on a chain of %d issues: %d statements, answered in %.1f ms", chain.size, sent[i], milliseconds(elapsed))
	}
	if sent[0] != sent[1] {
		t.Fatalf("an ask on a chain of %d issues sent %d statements and one on a chain of %d sent %d, want as many: "+
			"the owed route's reads grow with the tree", chains[0].size, sent[0], chains[1].size, sent[1])
	}
}

// The time the owed route takes to answer on a chain of 1024 issues (owedChain), read only when
// LEGION_OWED_TIMING=1 asks for it, since a wall-clock bound in the package's suite fails on the
// load of the pod it runs on. One warm-up ask is not counted; of the owedTimedAsks after it, the
// slowest, the median and the fastest answer are logged on one line. It fails only when the
// slowest takes over 1 s, a third of the 3 s the worker's reminder waits for the answer.
func TestClaimOwedTimingOnAChainOf1024Issues(t *testing.T) {
	if os.Getenv(owedTimingEnabled) != "1" {
		t.Skip("set LEGION_OWED_TIMING=1 to time the owed route on a chain of 1024 issues: a wall-clock bound in the " +
			"package's suite fails on the load of the pod it runs on, so the suite holds the route's statement count instead")
	}
	h := newHarness(t)
	serveOwedOver(h, h.store.Pool(), record.NewStore())
	chain := seedOwedChain(t, h, 1000, largeTree)

	chain.ask(t)
	times := make([]time.Duration, owedTimedAsks)
	for i := range times {
		times[i] = chain.ask(t)
	}
	slices.Sort(times)
	slowest, median, fastest := times[len(times)-1], times[len(times)/2], times[0]
	t.Logf("owed on a chain of %d issues, %d asks: slowest %.1f ms, median %.1f ms, fastest %.1f ms",
		chain.size, len(times), milliseconds(slowest), milliseconds(median), milliseconds(fastest))
	if slowest > time.Second {
		t.Fatalf("the slowest of %d asks took %.1f ms, over 1 s: a third of the 3 s the worker's reminder waits for the "+
			"answer (OWED_READ_BOUND_MS, packages/pi-shared/src/phase-reminder.ts) before it nudges with \"could not read "+
			"the record\", so on a pod under load a tree this size would nudge at every turn's end",
			len(times), milliseconds(slowest))
	}
}
