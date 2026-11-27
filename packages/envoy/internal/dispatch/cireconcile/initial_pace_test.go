package cireconcile

import (
	"testing"
	"time"
)

func TestTheInitialAndNoWorkGapsHonorTheConfiguredPace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pace time.Duration
		want time.Duration
	}{
		{name: "50ms ceiling", pace: 50 * time.Millisecond, want: 50 * time.Millisecond},
		{name: "300ms ceiling", pace: 300 * time.Millisecond, want: 100 * time.Millisecond},
		{name: "500ms ceiling", pace: 500 * time.Millisecond, want: 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []struct {
				name  string
				known bool
			}{
				{name: "nothing known"},
				{name: "heads known, no REST work", known: true},
			} {
				t.Run(state.name, func(t *testing.T) {
					r := &Reconciler{Pace: tc.pace, PassTimeout: 5 * time.Second}
					if state.known {
						r.listed = map[int64]int{1: 1}
					}
					if got := r.gap(0); got != tc.want {
						t.Errorf("the %s gap with Pace=%s is %s, want %s", state.name, tc.pace, got, tc.want)
					}
				})
			}
		})
	}
}
