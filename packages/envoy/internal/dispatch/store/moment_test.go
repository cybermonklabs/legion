package store

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
)

// MomentOn hands a statement no moment where it is given no clock, as production gives it none,
// so the statement's own now() stamps and judges and no process's clock moves what it writes. A
// MomentOn that read the process's clock there would pass every row that hands it a fake. Given a
// clock, it hands that clock's reading cut to the microsecond the database keeps.
func TestMomentOnHandsNoMomentWithoutAClockAndTheClocksReadingWithOne(t *testing.T) {
	if at := MomentOn(nil); at != nil {
		t.Fatalf("MomentOn(nil) = %s, want nil, so the statement's own now() stamps and judges", at)
	}
	fake := clocktest.New(time.Date(2026, time.November, 22, 3, 4, 5, 123456789, time.UTC))
	want := time.Date(2026, time.November, 22, 3, 4, 5, 123456000, time.UTC)
	if at := MomentOn(fake); at == nil || !at.Equal(want) {
		t.Fatalf("MomentOn(a fake at %s) = %v, want %s, its reading cut to the microsecond", fake.Now(), at, want)
	}
}
