package replay

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// What a rule's grace keeps over the waits on another issue that gave a slot back in a window's
// record (waitsGaveBack, graceKeeps), at each grace the repository keeps, five minutes (wait-grace)
// and an hour (wait-grace-1h), read from four fixture windows' records as a comparison reads them
// (newWindowRecord, its record.json), and the rows the comparison's document gives it
// (writeGraces). Each window's waits are its README's, and the values are worked by hand from them:
//
//   - one-minute-wait: ACME-1's slot goes back at 10:00 for its link wait on ACME-2, 10:00 to
//     10:01, one minute. Five minutes and an hour each keep the slot through it, 1 of 1, and hold
//     it for the minute: 1/60 slot-hour, 0.0167.
//   - link-added-and-removed: ACME-1's slot goes back at 10:00 for its link wait on ACME-2, 10:00
//     to 11:00, an hour. Five minutes keeps the slot through none, 0 of 1, and holds it five
//     minutes: 5/60 slot-hour, 0.0833. An hour keeps it through the wait, which ends at the grace's
//     end, where the pass finds it over: 1 of 1, 1 slot-hour.
//   - two-waiters-one-blocker: ACME-1's slot goes back at 09:40 for its link wait on ACME-3, 09:40
//     to 12:10 (150 minutes), and ACME-2's at 09:45, 09:45 to 12:10 (145 minutes). Neither grace
//     keeps a slot through either wait, 0 of 2. Five minutes holds each slot five minutes, 10/60
//     slot-hour, 0.1667; an hour holds each an hour, 2 slot-hours.
//   - child-filed-after-parent-start: ACME-2's slot goes back at 13:30 for its children wait on
//     ACME-3, still open at the window's end, 15:00, so it counts at its 90 minutes to the end.
//     Neither grace keeps the slot through it, 0 of 1: five minutes holds it 5/60 slot-hour,
//     0.0833, and an hour 1. A grace of two hours, which no kept rule names, keeps it to the
//     window's end, 1 of 1, for its 90 minutes: 1.5 slot-hours.
//
// No other slot of the four windows goes back while its issue waits on another issue: each other
// release is a move into accepting or to done. A comparison of no rule with a grace writes nothing
// of the graces.
func TestAGraceKeepsTheWaitsNoLongerThanItInTheRecord(t *testing.T) {
	t.Parallel()
	short, long := keptGraceRule(t, "wait-grace", 5*time.Minute), keptGraceRule(t, "wait-grace-1h", time.Hour)
	type kept struct {
		grace time.Duration
		want  graceKept
	}
	windows := []struct {
		name  string
		waits []waitGaveBack
		kept  []kept
		rows  []string
	}{
		{
			name:  "one-minute-wait",
			waits: []waitGaveBack{{Issue: "ACME-1", Start: clock(10, 0), End: clock(10, 1)}},
			kept:  []kept{{short.WaitGrace, graceKept{Kept: 1, Waits: 1, SlotHours: 1.0 / 60}}, {long.WaitGrace, graceKept{Kept: 1, Waits: 1, SlotHours: 1.0 / 60}}},
			rows: []string{
				"| one-minute-wait | `wait-grace` | 5m0s | 1 of 1 | 0.0167 |",
				"| one-minute-wait | `wait-grace-1h` | 1h0m0s | 1 of 1 | 0.0167 |",
			},
		},
		{
			name:  "link-added-and-removed",
			waits: []waitGaveBack{{Issue: "ACME-1", Start: clock(10, 0), End: clock(11, 0)}},
			kept:  []kept{{short.WaitGrace, graceKept{Kept: 0, Waits: 1, SlotHours: 5.0 / 60}}, {long.WaitGrace, graceKept{Kept: 1, Waits: 1, SlotHours: 1}}},
			rows: []string{
				"| link-added-and-removed | `wait-grace` | 5m0s | 0 of 1 | 0.0833 |",
				"| link-added-and-removed | `wait-grace-1h` | 1h0m0s | 1 of 1 | 1 |",
			},
		},
		{
			name: "two-waiters-one-blocker",
			waits: []waitGaveBack{
				{Issue: "ACME-1", Start: clock(9, 40), End: clock(12, 10)},
				{Issue: "ACME-2", Start: clock(9, 45), End: clock(12, 10)},
			},
			kept: []kept{{short.WaitGrace, graceKept{Kept: 0, Waits: 2, SlotHours: 10.0 / 60}}, {long.WaitGrace, graceKept{Kept: 0, Waits: 2, SlotHours: 2}}},
			rows: []string{
				"| two-waiters-one-blocker | `wait-grace` | 5m0s | 0 of 2 | 0.1667 |",
				"| two-waiters-one-blocker | `wait-grace-1h` | 1h0m0s | 0 of 2 | 2 |",
			},
		},
		{
			name:  "child-filed-after-parent-start",
			waits: []waitGaveBack{{Issue: "ACME-2", Start: clock(13, 30), End: clock(15, 0), Open: true}},
			kept: []kept{{short.WaitGrace, graceKept{Kept: 0, Waits: 1, SlotHours: 5.0 / 60}}, {long.WaitGrace, graceKept{Kept: 0, Waits: 1, SlotHours: 1}},
				{2 * time.Hour, graceKept{Kept: 1, Waits: 1, SlotHours: 1.5}}},
			rows: []string{
				"| child-filed-after-parent-start | `wait-grace` | 5m0s | 0 of 1 | 0.0833 |",
				"| child-filed-after-parent-start | `wait-grace-1h` | 1h0m0s | 0 of 1 | 1 |",
			},
		},
	}
	var records []windowRecord
	var rows []string
	for i, window := range windows {
		rec, err := newWindowRecord(fixtureWindow(t, window.name), window.name, i)
		if err != nil {
			t.Fatalf("read the record of %s: %v", window.name, err)
		}
		if !slices.EqualFunc(rec.WaitsGaveBack, window.waits, sameWait) {
			t.Errorf("the waits that gave a slot back in the record of %s are %+v, want %+v", window.name, rec.WaitsGaveBack, window.waits)
		}
		for _, k := range window.kept {
			got := graceKeeps(k.grace, rec.WaitsGaveBack)
			if got.Kept != k.want.Kept || got.Waits != k.want.Waits || formatNumber(got.SlotHours) != formatNumber(k.want.SlotHours) {
				t.Errorf("a grace of %s over the record of %s keeps %+v, want %+v", k.grace, window.name, got, k.want)
			}
		}
		path := filepath.Join(t.TempDir(), recordFile)
		var read windowRecord
		if err := writeJSON(path, rec); err != nil {
			t.Fatal(err)
		}
		if err := readJSON(path, &read); err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(read.WaitsGaveBack, window.waits, sameWait) {
			t.Errorf("the record.json of %s holds the waits that gave a slot back as %+v, want %+v", window.name, read.WaitsGaveBack, window.waits)
		}
		records = append(records, rec)
		rows = append(rows, window.rows...)
	}

	var doc strings.Builder
	p := &printer{w: &doc}
	comparison{windows: records, rules: []string{record.MainRule.Name, "cover", short.Name, long.Name}}.writeGraces(p)
	if p.err != nil {
		t.Fatal(p.err)
	}
	if head := "## What each grace keeps, read from each window's record\n"; !strings.HasPrefix(doc.String(), head) {
		t.Errorf("the document's part of the graces does not begin %q:\n%s", head, doc.String())
	}
	table := "| window | rule | grace | waits it keeps the slot through | slot-hours it keeps |\n| --- | --- | --- | --- | --- |\n" +
		strings.Join(rows, "\n") + "\n"
	if !strings.Contains(doc.String(), table) {
		t.Errorf("the document's part of the graces holds no table\n%s\nit reads:\n%s", table, doc.String())
	}

	doc.Reset()
	comparison{windows: records, rules: []string{record.MainRule.Name, "cover"}}.writeGraces(p)
	if doc.Len() > 0 || p.err != nil {
		t.Errorf("a comparison of no rule with a grace writes of the graces %q (%v), want nothing", doc.String(), p.err)
	}
}

// keptGraceRule is the rule the repository keeps under name, which must name a grace of grace,
// the value TestAGraceKeepsTheWaitsNoLongerThanItInTheRecord is worked for.
func keptGraceRule(t *testing.T, name string, grace time.Duration) record.SlotRule {
	t.Helper()
	rule, kept := record.SlotRuleNamed(name)
	if !kept || rule.WaitGrace != grace {
		t.Fatalf("the repository keeps the rule %q (%t) with a grace of %s, and the test is worked for %s", name, kept, rule.WaitGrace, grace)
	}
	return rule
}

// sameWait says whether a and b are one wait: the same issue, moments and openness.
func sameWait(a, b waitGaveBack) bool {
	return a.Issue == b.Issue && a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Open == b.Open
}
