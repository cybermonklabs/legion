package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The hand-over is a door into the workflow with no Dispatch event and no PATCH of its own: the
// outbox re-admits a child as a root of its own from the record and the durable fence, so nothing
// of the issue's Dispatch state reaches admission here but what the outbox's own read answered.
// That read's repository is what the fact carries (intake.Handover.ResolvedRepo), and these tests
// read what admission makes of it — the same following of Dispatch's resolution a live event and
// the boot listing make (the engine's ApplyRepositoryChange). dispatch://INFRA-2865 measured what
// its absence costs: a new generation opened on the repository the issue had left.

// fencedRoot is the done root lingering after its sign-off, and fencedChild the member it
// hands to Legion as a root of its own.
const fencedRoot, fencedChild = "LEGION-208", "LEGION-209"

// seedHandOverFence records that root and that child, both on repo at branch, the child in a run
// of its own, with the durable fence the linger's hand-over wrote for it.
func seedHandOverFence(t *testing.T, pool *pgxpool.Pool, repo, branch string) {
	t.Helper()
	until := fixedNow.Add(time.Hour)
	parent := fencedRoot
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: fencedRoot, Repo: repo, DefaultBranch: branch, GitHubRepo: repo, Tree: fencedRoot,
				Project: testProject, Title: fencedRoot, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A",
				HandedOver: handed, LingerUntil: &until, LastDispatchSeq: 3}),
			records.PutIssue(ctx, tx, record.Issue{Key: fencedChild, Repo: repo, DefaultBranch: branch, GitHubRepo: repo, Tree: fencedRoot,
				Project: testProject, Title: fencedChild, Parent: &parent, Phase: phase.Planning, Generation: 1, Status: "in_progress",
				DispatchStatus: "todo", Rank: "B", HandedOver: handed, LastDispatchSeq: 4}),
			records.PutHandover(ctx, tx, record.Handover{Issue: fencedChild, Tree: fencedRoot, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed the hand-over: %v", put)
			}
		}
	})
}

// consumeHandOver applies the fact the outbox applies to complete the hand-over, carrying resolved
// as the repository its Dispatch read answered with — "" for a read that answered none.
func consumeHandOver(t *testing.T, pool *pgxpool.Pool, admission *Admission, resolved string) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "handover:"+fencedChild+":1",
		intake.Handover{Issue: fencedChild, Tree: fencedRoot, Generation: 1, ResolvedRepo: resolved},
		admission.engine, admission); err != nil {
		t.Fatalf("consume the hand-over: %v", err)
	}
}

// handOverMessages is every Dispatch message the hand-over queued, in the order they were queued.
func handOverMessages(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var bodies []string
	for _, effect := range effects(t, pool) {
		if post, ok := effect.payload.(record.MessagePost); ok {
			bodies = append(bodies, post.Body)
		}
	}
	return bodies
}

// assertReadmitted fails unless the child came back as a root of its own at its next generation:
// the hand-over's own work, which no repository question moves.
func assertReadmitted(t *testing.T, got record.Issue) {
	t.Helper()
	if got.Tree != fencedChild || got.Generation != 2 || got.LingerUntil != nil {
		t.Fatalf("the handed-over child = %#v, want it re-admitted as a tree of its own at generation 2", got)
	}
}

// A child handed to Legion as a root of its own, whose repository Dispatch changed while it was
// out of the workflow, comes back on the repository Dispatch resolves it to now: the outbox's read
// answered acme/tools where the record held acme/widgets, so the new generation is on acme/tools
// at that repository's own default branch, keeping acme/widgets as the repository the issue left
// and the moment it moved. Nothing is said to the issue: its record followed Dispatch.
func TestRepositoryCorrectionFollowsAHandOver(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedHandOverFence(t, pool, "acme/widgets", "main")

	consumeHandOver(t, pool, admission, "acme/tools")

	got := issue(t, pool, fencedChild)
	assertReadmitted(t, got)
	if got.Repo != "acme/tools" || got.DefaultBranch != "release/v7.1" || got.GitHubRepo != "acme/tools" {
		t.Fatalf("the re-admitted child is on %s at %s, spelled %s; want acme/tools at release/v7.1, spelled acme/tools",
			got.Repo, got.DefaultBranch, got.GitHubRepo)
	}
	if got.RepoBefore != "acme/widgets" || got.RepoCorrectedAt == nil || !got.RepoCorrectedAt.Equal(fixedNow) {
		t.Fatalf("the correction's marks = %q at %v, want acme/widgets at %v", got.RepoBefore, got.RepoCorrectedAt, fixedNow)
	}
	if got.RepoChangePending != "" || got.RepoChangeReason != "" {
		t.Fatalf("the waiting change after the correction = %q because %q, want none: the correction applied it",
			got.RepoChangePending, got.RepoChangeReason)
	}
	if messages := handOverMessages(t, pool); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing: its record followed Dispatch onto a repository Legion serves", messages)
	}
}

// A hand-over whose Dispatch read answered no repository carries none, and an absent resolution is
// no statement by Dispatch that the issue is on no repository: the child comes back on the
// repository its own record holds, with no correction written and nothing said to it.
func TestRepositoryCorrectionIsNotMadeByAHandOverWhoseDispatchReadAnsweredNoRepository(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedHandOverFence(t, pool, "acme/widgets", "main")

	consumeHandOver(t, pool, admission, "")

	got := issue(t, pool, fencedChild)
	assertReadmitted(t, got)
	if got.Repo != "acme/widgets" || got.DefaultBranch != "main" || got.GitHubRepo != "acme/widgets" {
		t.Fatalf("the re-admitted child is on %s at %s, spelled %s; want the record's own acme/widgets at main",
			got.Repo, got.DefaultBranch, got.GitHubRepo)
	}
	if got.RepoBefore != "" || got.RepoCorrectedAt != nil {
		t.Fatalf("the correction's marks = %q at %v, want none written: nothing read a repository to follow",
			got.RepoBefore, got.RepoCorrectedAt)
	}
	if got.RepoChangePending != "" || got.RepoChangeReason != "" {
		t.Fatalf("the waiting change = %q because %q, want none: an absent resolution says nothing about the repository",
			got.RepoChangePending, got.RepoChangeReason)
	}
	if messages := handOverMessages(t, pool); len(messages) != 0 {
		t.Fatalf("the issue was told %q, want nothing", messages)
	}
}

// A hand-over onto a repository the project serves no installation of leaves the record where it
// is and the change waiting on it, as the live event and the boot listing do: the child still
// comes back as a root of its own, on the repository it holds, and is told once why its repository
// has not moved. The waiting pair is what the first pass that finds the repository served reads.
func TestRepositoryCorrectionOfAHandOverOntoAnUnservedRepositoryWaitsOnTheRecord(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedHandOverFence(t, pool, "acme/widgets", "main")

	consumeHandOver(t, pool, admission, "acme/gadgets")

	got := issue(t, pool, fencedChild)
	assertReadmitted(t, got)
	if got.Repo != "acme/widgets" || got.DefaultBranch != "main" || got.RepoBefore != "" || got.RepoCorrectedAt != nil {
		t.Fatalf("the re-admitted child is on %s at %s, having left %q at %v; want it still on acme/widgets at main, corrected in nothing",
			got.Repo, got.DefaultBranch, got.RepoBefore, got.RepoCorrectedAt)
	}
	if got.RepoChangePending != "acme/gadgets" || got.RepoChangeReason != "the review App is not installed on acme/gadgets" {
		t.Fatalf("the waiting change = %q because %q, want acme/gadgets and why it waits", got.RepoChangePending, got.RepoChangeReason)
	}
	want := "Legion serves no installation of acme/gadgets, so this issue keeps acme/widgets as its repository. Legion moves the issue to acme/gadgets itself once it serves that repository, with nothing for anyone to do."
	if messages := handOverMessages(t, pool); len(messages) != 1 || messages[0] != want {
		t.Fatalf("the issue was told %q, want it told once, saying %q", messages, want)
	}
}
