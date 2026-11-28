package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// R2 rests on three numbers that live in different places: a forward starts only on a resolution
// younger than roles.ForwardFenceAge, the keep window less one sweep interval, so it is stored
// inside the hour a takeover's copy reads; a takeover record's passes run for takeoverRecordLife,
// that hour, so the pass that ends a record has read every such forward; and the record's last
// write, at the latest one sweep interval past its life, comes before the stream's age limit takes
// its first, so no record a pass needs ages out. (What the cache of finished records rests on is
// stated once, on bus.Client.TakeoverRecords.) A handover record lives the stream's retention
// instead (handoverRecordLife), since nothing fences a direct send to a session, so its passes copy
// each message sent to the replaced session within that time after the claim; what keeps such a
// record on the stream is its refresh write, which comes well inside the age limit at every sweep
// that runs it. A change to one that leaves the others fails here.
func TestTheTakeoverCopysBoundsHoldTogether(t *testing.T) {
	t.Parallel()
	if want := roleKeepWindow - roleWakeInterval; roles.ForwardFenceAge != want {
		t.Errorf("roles.ForwardFenceAge = %s, want roleKeepWindow less roleWakeInterval, %s", roles.ForwardFenceAge, want)
	}
	if takeoverRecordLife != roleKeepWindow {
		t.Errorf("takeoverRecordLife = %s, want the keep window, %s", takeoverRecordLife, roleKeepWindow)
	}
	if last := takeoverRecordLife + roleWakeInterval; last >= contracts.DeliveryDuplicateWindow {
		t.Errorf("a record's last pass, %s after its claim, is at or past the stream's age limit, %s", last, contracts.DeliveryDuplicateWindow)
	}
	if handoverRecordLife != contracts.DeliveryDuplicateWindow {
		t.Errorf("handoverRecordLife = %s, want the stream's retention, %s", handoverRecordLife, contracts.DeliveryDuplicateWindow)
	}
	if every := takeoverRecordRefresh + roleWakeInterval; every >= contracts.DeliveryDuplicateWindow {
		t.Errorf("a record nothing moves is written every %s at the latest, at or past the stream's age limit, %s", every, contracts.DeliveryDuplicateWindow)
	}
	// A forward the role lane reports in time (roles.ForwardStoredTooLate, roleForwardTimeout) is
	// one the record's passes read only while the fence, that forward's wait and the claim route's
	// budget (sendRequestBudget), inside which the claim lands, fit inside the record's life. Each
	// is read here from its own constant, so a change to one that breaks the sum fails here.
	if sum := roles.ForwardFenceAge + roleForwardTimeout + sendRequestBudget; sum >= takeoverRecordLife {
		t.Errorf("the fence %s, the role lane's forward wait %s and the claim's budget %s come to %s, at or past a takeover record's life, %s",
			roles.ForwardFenceAge, roleForwardTimeout, sendRequestBudget, sum, takeoverRecordLife)
	}
	if now := time.Unix(1_800_000_000, 0); roles.ResolutionTooOld(time.Time{}, now) ||
		roles.ResolutionTooOld(now.Add(-roles.ForwardFenceAge+time.Second), now) ||
		!roles.ResolutionTooOld(now.Add(-roles.ForwardFenceAge), now) {
		t.Error("roles.ResolutionTooOld: want false for a zero resolution and one younger than the fence, true at the fence")
	}
}
