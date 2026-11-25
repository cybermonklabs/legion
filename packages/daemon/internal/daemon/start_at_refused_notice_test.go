package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The owning architect's notice of a start the shim refused (supervise's Deps.StartAtRefused) is a
// row the outbox takes, of the kind an architect reads for a next change's workspace, and carries
// the shim's words.
func TestAStartAtBranchRefusalIsANoticeTheOutboxTakes(t *testing.T) {
	t.Parallel()
	c := supervise.Claim{Token: "legion-LEGION-208-implementer", Issue: "LEGION-208", Role: claim.RoleImplementer}
	d := supervise.Delivery{ID: "outbox:7", Phase: phase.Implementing,
		StartAt: supervise.StartAt{StartAt: runtime.StartAt{Bookmark: "legion/LEGION-208", Base: "main"}}}
	const refusal = "Bookmark legion/LEGION-208 is conflicted. The working copy did not move."
	notice := startAtRefusedNotice(c, d, refusal)
	if notice.Kind != "next-change-workspace" || notice.Role != claim.RoleImplementer || !strings.Contains(notice.Reason, refusal) {
		t.Fatalf("the notice = %+v, want a next-change-workspace notice of the implementer carrying the shim's words", notice)
	}
	if _, err := record.NewOutboxRow("LEGION-208", notice, time.Now()); err != nil {
		t.Fatalf("the outbox refuses the notice: %v", err)
	}
}
