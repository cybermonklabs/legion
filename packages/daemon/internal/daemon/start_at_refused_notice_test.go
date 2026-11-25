package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// startAtRefusedClaim and startAtRefusedTask are the implementer of LEGION-208 and its first task of
// the next change, whose start the shim refused.
var (
	startAtRefusedClaim = supervise.Claim{Token: "legion-LEGION-208-implementer", Issue: "LEGION-208", Role: claim.RoleImplementer}
	startAtRefusedTask  = supervise.Delivery{ID: "outbox:7", Phase: phase.Implementing,
		StartAt: supervise.StartAt{StartAt: runtime.StartAt{Bookmark: "legion/LEGION-208", Base: "main"}}}
)

const startAtRefusal = "Bookmark legion/LEGION-208 is conflicted. The working copy did not move."

// The owning architect's notice of a start the shim refused (supervise's Deps.StartAtRefused) is a
// row the outbox takes, of the kind an architect reads for a next change's workspace, and carries
// the shim's words.
func TestAStartAtBranchRefusalIsANoticeTheOutboxTakes(t *testing.T) {
	t.Parallel()
	notice := startAtRefusedNotice(startAtRefusedClaim, startAtRefusedTask, startAtRefusal)
	if notice.Kind != "next-change-workspace" || notice.Role != claim.RoleImplementer || !strings.Contains(notice.Reason, startAtRefusal) {
		t.Fatalf("the notice = %+v, want a next-change-workspace notice of the implementer carrying the shim's words", notice)
	}
	if _, err := record.NewOutboxRow("LEGION-208", notice, time.Now()); err != nil {
		t.Fatalf("the outbox refuses the notice: %v", err)
	}
}

// The hook answers only once its notice is durable, since the machine marks the task told on that
// answer alone (supervise's tellStartRefused). An enqueue that cannot commit is answered as the
// failure it is, so the task is told again at its next refusal. An answer of nil means the row is in
// the outbox when the hook returns.
func TestAStartAtBranchRefusalHookAnswersOnlyADurableNotice(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "U"})
	w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.startAtRefused(ended, startAtRefusedClaim, startAtRefusedTask, startAtRefusal); err == nil {
		t.Fatal("startAtRefused answered nil for a notice whose enqueue could not commit, want its error")
	}
	if notices := noticesOf(t, pool, "LEGION-208"); notices != 0 {
		t.Fatalf("%d notices queued after the enqueue that could not commit, want none", notices)
	}

	if err := w.startAtRefused(context.Background(), startAtRefusedClaim, startAtRefusedTask, startAtRefusal); err != nil {
		t.Fatalf("startAtRefused: %v", err)
	}
	if notices := noticesOf(t, pool, "LEGION-208"); notices != 1 {
		t.Fatalf("%d notices queued when startAtRefused returned nil, want its one notice durable by then", notices)
	}
}
