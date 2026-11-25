package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// R2 rests on three numbers that live in different places: a forward starts only on a resolution
// younger than roles.ForwardFenceAge, the keep window less one sweep interval, so it is stored
// inside the hour a takeover's copy reads; and a takeover record's passes run for
// takeoverRecordLife, one hour short of the stream's age limit and duplicate window, so no record a
// pass needs ages out. A change to one that leaves the others fails here.
func TestTheTakeoverCopysBoundsHoldTogether(t *testing.T) {
	t.Parallel()
	if want := roleKeepWindow - roleWakeInterval; roles.ForwardFenceAge != want {
		t.Errorf("roles.ForwardFenceAge = %s, want roleKeepWindow less roleWakeInterval, %s", roles.ForwardFenceAge, want)
	}
	if want := contracts.DeliveryDuplicateWindow - time.Hour; takeoverRecordLife != want {
		t.Errorf("takeoverRecordLife = %s, want the duplicate window less an hour, %s", takeoverRecordLife, want)
	}
	if now := time.Unix(1_800_000_000, 0); roles.ResolutionTooOld(time.Time{}, now) ||
		roles.ResolutionTooOld(now.Add(-roles.ForwardFenceAge+time.Second), now) ||
		!roles.ResolutionTooOld(now.Add(-roles.ForwardFenceAge), now) {
		t.Error("roles.ResolutionTooOld: want false for a zero resolution and one younger than the fence, true at the fence")
	}
}
