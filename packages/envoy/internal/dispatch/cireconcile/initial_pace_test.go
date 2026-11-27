package cireconcile

import (
	"testing"
	"time"
)

func TestTheFirstGapHonorsTheConfiguredPace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pace time.Duration
		want time.Duration
	}{
		{name: "50ms ceiling", pace: 50 * time.Millisecond, want: 50 * time.Millisecond},
		{name: "500ms ceiling", pace: 500 * time.Millisecond, want: 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, known := range []bool{false, true} {
				r := &Reconciler{Pace: tc.pace, PassTimeout: 5 * time.Second}
				if known {
					r.listed = map[int64]int{1: 1}
				}
				if got := r.gap(0); got != tc.want {
					t.Errorf("the first gap with Pace=%s and counts known=%v is %s, want %s", tc.pace, known, got, tc.want)
				}
			}
		})
	}
}
