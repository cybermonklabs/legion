package record

import (
	"testing"
	"time"
)

// A row or a fact that carries no check identity — one queued, or made, before the rows carried
// one — names only a check of generation 0, which a release before the generation opened and which
// binds none. So it changes nothing of a check bound to a generation, while a check of the release
// before still takes it as it took it. An identity names its own check and no other.
func TestAZeroIdentityNamesOnlyAnUnboundCheck(t *testing.T) {
	opened := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	unbound := ProductionCheck{Issue: "ACME-1", OpenedAt: opened}
	bound := ProductionCheck{Issue: "ACME-1", OpenedAt: opened, Generation: 3}
	for _, tc := range []struct {
		name     string
		check    ProductionCheck
		openedAt time.Time
		want     bool
	}{
		{"no identity, a check of generation 0", unbound, time.Time{}, true},
		{"no identity, a check of generation 3", bound, time.Time{}, false},
		{"its own identity, a check of generation 3", bound, opened, true},
		{"another identity, a check of generation 3", bound, opened.Add(time.Second), false},
		{"another identity, a check of generation 0", unbound, opened.Add(time.Second), false},
	} {
		if got := tc.check.NamedBy(tc.openedAt); got != tc.want {
			t.Errorf("%s: NamedBy = %t, want %t", tc.name, got, tc.want)
		}
	}
}
