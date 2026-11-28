package record

import (
	"testing"
	"time"
)

// A check's look id names its current look only while it was made for that look of that check
// instance. An older binary writes none of the look id's columns, so after a rollback the record
// can still hold the id of the look before, or of the check a later merge replaced, and neither
// may count as the current look's (R6 of dispatch://INFRA-3518). The id of the look before stays
// where the way back keeps the look id of every open check and the release before then starts the
// next look. An empty id names no look whatever it was made for. The opening time the id was made
// for is compared as an instant, so the same moment read back in another zone still names the
// check.
func TestCurrentLookIDNamesOnlyTheLookItWasMadeFor(t *testing.T) {
	opened := time.Date(2026, 11, 27, 9, 0, 0, 123456000, time.UTC)
	check := ProductionCheck{Issue: "LEGION-209", OpenedAt: opened, Looks: 3, LookOpen: true}
	for _, tc := range []struct {
		name     string
		id       string
		look     int
		openedAt time.Time
		want     string
	}{
		{name: "made for this look of this check", id: "LOOKIDOFLOOKTHREE", look: 3, openedAt: opened, want: "LOOKIDOFLOOKTHREE"},
		{name: "made for this look of this check, read in another zone", id: "LOOKIDOFLOOKTHREE", look: 3,
			openedAt: opened.In(time.FixedZone("UTC+2", 2*60*60)), want: "LOOKIDOFLOOKTHREE"},
		{name: "made for the previous look", id: "LOOKIDOFLOOKTWO", look: 2, openedAt: opened, want: ""},
		{name: "made for the check of another opening time", id: "LOOKIDOFTHEOLDCHECK", look: 3, openedAt: opened.Add(-time.Hour), want: ""},
		{name: "made for no check instance", id: "LOOKIDOFNOCHECK", look: 3, want: ""},
		{name: "empty", look: 3, openedAt: opened, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := check
			check.LookID, check.LookIDLook, check.LookIDOpenedAt = tc.id, tc.look, tc.openedAt
			if got := check.CurrentLookID(); got != tc.want {
				t.Fatalf("CurrentLookID of look %d opened at %s, the id %q made for look %d opened at %s = %q, want %q",
					check.Looks, check.OpenedAt, tc.id, tc.look, tc.openedAt, got, tc.want)
			}
		})
	}
}
