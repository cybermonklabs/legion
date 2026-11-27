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
	"github.com/sjawhar/legion/daemon/internal/workflow"
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

// chainReads is a record store that counts the walks of the copy of Dispatch's issues its reader
// makes (Store.ChainRows and Store.ChainRowsOf, each one read of the Dispatch chains), so a test
// holds the owed route to its one read of the chain per ask.
type chainReads struct {
	record.Store
	walks atomic.Int64
}

func (c *chainReads) ChainRows(ctx context.Context, tx pgx.Tx, project string, extra []string) ([]record.ChainRow, error) {
	c.walks.Add(1)
	return c.Store.ChainRows(ctx, tx, project, extra)
}

func (c *chainReads) ChainRowsOf(ctx context.Context, tx pgx.Tx, keys []string) ([]record.ChainRow, error) {
	c.walks.Add(1)
	return c.Store.ChainRowsOf(ctx, tx, keys)
}

// serveOwedOver has h's server read the records through records over pool, with the workflow's
// table of which role works each phase (workflow.RoleFor), as the daemon wires the owed route.
func serveOwedOver(h *harness, pool *store.Pool, records record.Store) {
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: pool, Record: records, RoleFor: workflow.RoleFor,
	}).Handler
}

// owedTree is a tree of size issues whose deepest chain is links long, with the claims the owner
// walk weighs on it: the root's architect, which holds its role, an architect the daemon retired
// on every issue of that chain below the root, as a child's architect is once the child closes,
// and the planner of the issue at the chain's bottom, which asks what it owes. Every other issue
// of the tree is a child of the root.
//
// The chain is the shape on which the walk does the most (record.OwningIssue, which
// OwnerWalk.Architect ends): the walk climbs the Dispatch chain one parent at a time from the
// asking issue and stops at the first architect that holds its role, else at the gate root. From
// the bottom of a chain whose architects below the root are all retired, it asks about every link
// and passes each over, and the read of their claims (record.ArchitectRuns) answers a row for
// each. The rest of the tree stands on no link of that chain, so the walk reads none of it.
type owedTree struct {
	size, links int
	architect   liveClaim
	planner     liveClaim
}

// seedOwedTree records a tree of size issues numbered from LEGION-<first>, its root: a chain of
// links issues below the root, each the child of the one before, each with a retired architect,
// and every other issue a child of the root. Each issue has its row of the copy of Dispatch's
// issues, where the daemon reads each chain (R15 of dispatch://INFRA-3320), and all of it is
// written in one transaction. It then launches the root's architect and the planner of the
// chain's bottom issue.
func seedOwedTree(t *testing.T, h *harness, first, size, links int) owedTree {
	t.Helper()
	root, bottom := fmt.Sprintf("LEGION-%d", first), fmt.Sprintf("LEGION-%d", first+links)
	records := record.NewStore()
	err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for n := first; n < first+size; n++ {
			key, parent := fmt.Sprintf("LEGION-%d", n), ""
			switch {
			case n == first:
			case n <= first+links:
				parent = fmt.Sprintf("LEGION-%d", n-1)
			default:
				parent = root
			}
			issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
				Project: testProject, Title: key, Phase: phase.Planning, Status: "in_progress"}
			if parent != "" {
				issue.Parent = &parent
			}
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := seedCopy(tx, key, parent, "in_progress"); err != nil {
				return err
			}
			if n == first || n > first+links {
				continue
			}
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
		return nil
	})
	if err != nil {
		t.Fatalf("seed a tree of %d issues from %s with a chain of %d links: %v", size, root, links, err)
	}
	return owedTree{
		size:      size,
		links:     links,
		architect: newLiveClaim(t, h, root, claim.RoleArchitect),
		planner:   newLiveClaimIn(t, h, root, bottom, claim.RolePlanner),
	}
}

// ask is the planner's ask of what it owes, held to the answer the tree gives — the planning
// phase of its issue, owned by the root's architect — and the time the route took to answer it.
func (o owedTree) ask(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	got := o.planner.owed(t)
	elapsed := time.Since(start)
	if want := (OwedResponse{Owes: true, Work: "the planning phase of " + o.planner.issue, Architect: o.architect.token}); got != want {
		t.Fatalf("the planner at the bottom of a chain of %d links in a tree of %d issues is told %+v, want %+v", o.links, o.size, got, want)
	}
	return elapsed
}

// milliseconds is d in milliseconds, to a fraction of one.
func milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// The owed route reads the architect that owns the claim on every ask (owningArchitect), and the
// worker's end-of-turn reminder waits OWED_READ_BOUND_MS, 3 s, for the answer before it nudges
// with "could not read the record" (packages/pi-shared/src/phase-reminder.ts), so a read that grew
// with the tree would nudge at every turn's end on a large one. The owner walk reads the claim's
// Dispatch chain and no stored tree (R15 and R18 of dispatch://INFRA-3320): one read of the chain
// per ask and the architect claims on its links (record.ReadOwnerWalk), so an ask sends the same
// statements whatever the chain's length and the tree's size, and only their rows grow. Asked
// from the bottom of a chain (owedTree), the route names the root's architect over a chain of one
// link in a tree of 16 issues and over one of record.MaxChainLinks, the longest Dispatch lets an
// issue stand below the top of its chain, in a tree of 1024, twice INFRA's largest. The store
// counts one walk of the chains for each ask, and the pool the server reads through counts as
// many statements for the one ask as for the other.
//
// The suite holds the counts and not a time. It runs on a pod that other work loads, where a
// wall-clock bound fails on that load and not on the code, while a read per issue changes the
// count on any machine. Each ask's time is logged, and TestClaimOwedTimingOnATreeOf1024Issues
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
	walks := &chainReads{Store: record.NewStore()}
	serveOwedOver(h, store.NewPool(traced), walks)

	trees := []owedTree{seedOwedTree(t, h, 100, 16, 1), seedOwedTree(t, h, 1000, largeTree, record.MaxChainLinks)}
	sent := make([]int64, len(trees))
	for i, tree := range trees {
		counted.sent.Store(0)
		walks.walks.Store(0)
		elapsed := tree.ask(t)
		sent[i] = counted.sent.Load()
		t.Logf("owed on a chain of %d links in a tree of %d issues: %d statements, answered in %.1f ms",
			tree.links, tree.size, sent[i], milliseconds(elapsed))
		if read := walks.walks.Load(); read != 1 {
			t.Errorf("an ask on a chain of %d links in a tree of %d issues read the Dispatch chains %d times, want once",
				tree.links, tree.size, read)
		}
	}
	if sent[0] != sent[1] {
		t.Fatalf("an ask on a chain of %d links in a tree of %d issues sent %d statements and one on a chain of %d links "+
			"in a tree of %d sent %d, want as many: the owed route's reads grow with the chain or the tree",
			trees[0].links, trees[0].size, sent[0], trees[1].links, trees[1].size, sent[1])
	}
}

// The time the owed route takes to answer on a tree of 1024 issues, asked from the bottom of its
// longest chain (owedTree), read only when LEGION_OWED_TIMING=1 asks for it, since a wall-clock
// bound in the package's suite fails on the load of the pod it runs on. One warm-up ask is not
// counted; of the owedTimedAsks after it, the slowest, the median and the fastest answer are
// logged on one line. It fails only when the slowest takes over 1 s, a third of the 3 s the
// worker's reminder waits for the answer.
func TestClaimOwedTimingOnATreeOf1024Issues(t *testing.T) {
	if os.Getenv(owedTimingEnabled) != "1" {
		t.Skip("set LEGION_OWED_TIMING=1 to time the owed route on a tree of 1024 issues: a wall-clock bound in the " +
			"package's suite fails on the load of the pod it runs on, so the suite holds the route's statement count instead")
	}
	h := newHarness(t)
	serveOwedOver(h, h.store.Pool(), record.NewStore())
	tree := seedOwedTree(t, h, 1000, largeTree, record.MaxChainLinks)

	tree.ask(t)
	times := make([]time.Duration, owedTimedAsks)
	for i := range times {
		times[i] = tree.ask(t)
	}
	slices.Sort(times)
	slowest, median, fastest := times[len(times)-1], times[len(times)/2], times[0]
	t.Logf("owed on a chain of %d links in a tree of %d issues, %d asks: slowest %.1f ms, median %.1f ms, fastest %.1f ms",
		tree.links, tree.size, len(times), milliseconds(slowest), milliseconds(median), milliseconds(fastest))
	if slowest > time.Second {
		t.Fatalf("the slowest of %d asks took %.1f ms, over 1 s: a third of the 3 s the worker's reminder waits for the "+
			"answer (OWED_READ_BOUND_MS, packages/pi-shared/src/phase-reminder.ts) before it nudges with \"could not read "+
			"the record\", so on a pod under load a tree this size would nudge at every turn's end",
			len(times), milliseconds(slowest))
	}
}
