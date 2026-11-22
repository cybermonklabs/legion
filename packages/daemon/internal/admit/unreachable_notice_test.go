package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The daemon's reach keeper marks a claim reported only once the notice that lists it is in the
// outbox (R5), so it must know whether the wake queued a row: a project's unreachable-claims list
// is one wake per project, and a second list while the first still waits unsent would be dropped,
// with its claims marked told and never sent. Each per-claim kind stands per claim, as the
// provider-errors notice does.
func TestTellControllerQueuedSaysWhetherTheNoticeWasQueued(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	tell := func(notice record.ControllerNotice, issue string) bool {
		t.Helper()
		var queued bool
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			var err error
			queued, err = admission.TellControllerQueued(context.Background(), tx, notice, issue)
			return err
		}); err != nil {
			t.Fatalf("tell the controller %s: %v", notice.Kind, err)
		}
		return queued
	}
	list := func(token string) record.ControllerNotice {
		return record.ControllerNotice{Kind: record.UnreachableClaimsNotice,
			Unreachable: []record.UnreachableClaim{{Claim: token, Issue: "LEGION-209", Role: claim.RoleImplementer}}}
	}

	if tell(list("legion-legion-legion-209-implementer"), testProject) {
		t.Fatal("a notice with no controller registered was queued; nobody would read it")
	}
	registerController(t, pool)
	if !tell(list("legion-legion-legion-209-implementer"), testProject) {
		t.Fatal("the first unreachable-claims notice was not queued")
	}
	if tell(list("legion-legion-legion-210-implementer"), testProject) {
		t.Fatal("a second unreachable-claims notice was queued while the first waits; the keeper must list its claim again later")
	}
	ready := func(issue string) record.ControllerNotice {
		return record.ControllerNotice{Kind: record.RelaunchReadyNotice, Role: claim.RoleImplementer,
			Claim: "legion-legion-" + issue + "-implementer", Reason: "the guard would now let it through"}
	}
	if !tell(ready("legion-209"), "LEGION-209") || !tell(ready("legion-210"), "LEGION-210") {
		t.Fatal("a per-claim notice of one claim stopped another claim's")
	}
	if tell(ready("legion-209"), "LEGION-209") {
		t.Fatal("a second relaunch-ready notice of one claim was queued while its first waits")
	}
}
