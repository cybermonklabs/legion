package delivery

import (
	"strings"
	"testing"
	"time"
)

// TestOverlappingDaysAreRefusedByEveryReadOfSeveralDays checks the clause of R2 of
// dispatch://INFRA-6225 that each read of several days reads days whose windows do not overlap.
func TestOverlappingDaysAreRefusedByEveryReadOfSeveralDays(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	settings := DeliverySettings{DeployRepo: "acme/widgets"}
	midnight := time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC)
	noon := midnight.Add(12 * time.Hour)
	next := midnight.AddDate(0, 0, 1)
	cases := []struct {
		name       string
		days       []time.Time
		refuse     bool
		wantStarts []string
	}{
		{"12_hours_apart", []time.Time{midnight, noon}, true, []string{
			"2024-06-01T00:00:00Z", "2024-06-01T12:00:00Z",
		}},
		{"reverse_order", []time.Time{next, midnight}, true, nil},
		{"repeated_day", []time.Time{midnight, midnight}, true, nil},
		{"consecutive_midnights", []time.Time{midnight, next}, false, nil},
	}

	for _, read := range []struct {
		name string
		run  func([]time.Time) error
	}{
		{"ListPullRequestsOnDays", func(days []time.Time) error {
			_, _, err := ListPullRequestsOnDays(ctx, pool, days)
			return err
		}},
		{"ReadDaysDeploys_without_applies", func(days []time.Time) error {
			_, err := ReadDaysDeploys(ctx, pool, settings, days, false)
			return err
		}},
		{"ReadDaysDeploys_with_applies", func(days []time.Time) error {
			_, err := ReadDaysDeploys(ctx, pool, settings, days, true)
			return err
		}},
		{"ReadCIDays", func(days []time.Time) error {
			_, err := ReadCIDays(ctx, pool, settings.DeployRepo, days, nil)
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					err := read.run(tc.days)
					if !tc.refuse {
						if err != nil {
							t.Fatalf("read refused consecutive UTC midnights: %v", err)
						}
						return
					}
					if err == nil {
						t.Fatalf("read accepted overlapping or out-of-order days %v", tc.days)
					}
					for _, start := range tc.wantStarts {
						if !strings.Contains(err.Error(), start) {
							t.Errorf("error %q does not name day start %s", err, start)
						}
					}
				})
			}
		})
	}
}
