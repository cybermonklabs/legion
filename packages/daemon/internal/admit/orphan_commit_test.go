package admit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// An orphan's line says what its fact's transaction recorded — a root of its own under the
// parent the daemon does not record — so it is a hook of that commit: a fact whose commit never
// landed records no orphan and writes no line, and the fact applied again, which records it,
// writes the one line.
func TestAnOrphanWhoseFactNeverCommitsLogsNoLineUntilItIsApplied(t *testing.T) {
	pool := migratedPool(t)
	var logs bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logs, nil)))
	proxied, fault := testdb.CommitFaults(t, pool.Config().ConnString())
	faulted, err := pgxpool.New(context.Background(), proxied)
	if err != nil {
		t.Fatalf("open a pool through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	fact := intake.DispatchIssue{Key: "LEGION-ORPHAN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: "orphan", Parent: "LEGION-MISSING", Rank: "A", HandedOver: handed}
	lines := func() int { return strings.Count(logs.String(), `msg="admission orphan"`) }

	fired := fault.Cut("legion.apply_fact", testdb.CutBeforeCommit)
	_, err = intake.ApplyFact(context.Background(), faulted, "dispatch", "orphan-todo", fact, engineStub{}, admission)
	testdb.Met(t, fired)
	if err == nil {
		t.Fatal("the orphan's fact whose commit never landed = nil, want its failure")
	}
	if got := maybeIssue(t, pool, fact.Key); got != nil {
		t.Fatalf("the orphan of a commit that never landed is recorded: %#v", got)
	}
	if got := lines(); got != 0 {
		t.Fatalf("orphan lines after a commit that never landed = %d, want 0; log:\n%s", got, logs.String())
	}

	apply(t, pool, admission, "orphan-todo", fact, engineStub{})
	if got := maybeIssue(t, pool, fact.Key); got == nil {
		t.Fatal("the orphan applied again is not recorded")
	}
	if got := lines(); got != 1 {
		t.Fatalf("orphan lines after the fact applied again = %d, want 1; log:\n%s", got, logs.String())
	}
}
