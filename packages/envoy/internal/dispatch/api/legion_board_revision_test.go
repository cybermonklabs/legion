package api

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// revisedEntry is the issues entry the board builds of row under key, closed at at as the board's
// reads of the facts close it (legionfacts.Row.At), with the revision the board gives it
// (legionBoardResponse's mapRevisions), which it checks is the one the live hub's encoding holds
// the entry at.
func revisedEntry(t *testing.T, key string, row legionfacts.Row, at time.Time) (legionBoardFacts, string) {
	t.Helper()
	body := legionBoardCompute(&legionBoardPopulation{project: "INFRA", facts: map[string]legionfacts.Row{key: row.At(at)}}, at)
	revisions, revised := body.mapRevisions("issues")
	if !revised {
		t.Fatal("the board gives no revision of its issues entries")
	}
	encoded, _, err := encodeDeliveryBody(legionBoardView, body)
	if err != nil {
		t.Fatalf("encode the board: %v", err)
	}
	if held := encoded.revs.Maps["issues"][key]; held != revisions[key] {
		t.Fatalf("the hub holds %s's entry at the revision %q, want the board's %q", key, held, revisions[key])
	}
	return body.Issues[key], revisions[key]
}

// TestAnIssuesEntrysRevisionMovesWithEachKeptValueAndNotWithTheClock: the revision the live hub
// sends an issues entry by is taken from the entry's typed values, and is that of every value the
// entry keeps: each value it carries but one whose rate its growth carries, and whether its as_of
// is null but not the moment it names. So it moves whenever the stored row behind the entry
// moves and never when only the clock does (live_frames.go's legionBoardView).
//
// Each row of the table moves one kept value of a stored row: of a row whose work time, slot
// hours, implementing hours and last moment of work grow while its other columns stand, every
// other value the entry carries, each rate of its growth, a rate for a column that stands, and
// the growth that leaves, which takes the entry's as_of and growth to null; and of the same row
// with no rate of a value the entry carries, each value that then stands, a column that comes to
// hold hours and a rate that joins. Closing either row a day later moves no revision, though it
// moves the growing row's values and its as_of, and eight builds of one row answer one revision,
// whatever order its hours are read in.
func TestAnIssuesEntrysRevisionMovesWithEachKeptValueAndNotWithTheClock(t *testing.T) {
	t.Parallel()
	const key = "INFRA-4"
	asOf := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	at, later := asOf.Add(time.Hour), asOf.Add(25*time.Hour)
	growing := func() legionfacts.Row {
		first, last := asOf.Add(-6*time.Hour), asOf.Add(-time.Hour)
		return legionfacts.Row{
			Project: "INFRA", Issue: key, WaitingReason: "waiting for a slot", UpdatedAt: asOf.Add(2 * time.Minute),
			ClosedFacts: metrics.ClosedFacts{
				IssueFacts: metrics.IssueFacts{
					OpenedAt: asOf.Add(-9 * time.Hour),
					PhaseHours: map[string]float64{
						legion.ColumnQueue: 0.5, legion.ColumnDesign: 1.5, legion.ColumnImplementing: 3.5, legion.ColumnReviewing: 1.25,
					},
					WorkHours: 4.75, ReviewRounds: 2, Tree: "INFRA-3", TreeDepth: 1, SlotHours: 4,
					WorkFirstAt: &first, WorkLastAt: &last,
				},
				AsOf: asOf,
				Growth: metrics.Growth{
					"work_hours": 1, "slot_hours": 1, "phase_hours.implementing": 1, "work_last_at": 1,
					"backlog_or_icebox_hours": 1,
				},
			},
		}
	}
	// The same row growing only its hours in backlog or icebox, which the entry does not carry, so
	// every value of its entry stands and it carries null for its as_of and its growth.
	standing := func() legionfacts.Row {
		row := growing()
		row.Growth = metrics.Growth{"backlog_or_icebox_hours": 1}
		return row
	}

	for _, moved := range []struct {
		name string
		from func() legionfacts.Row
		move func(row *legionfacts.Row)
	}{
		{"opened_at", growing, func(row *legionfacts.Row) { row.OpenedAt = row.OpenedAt.Add(-time.Hour) }},
		{"closed_at", growing, func(row *legionfacts.Row) { closed := asOf; row.ClosedAt = &closed }},
		{"the hours of a column with no rate", growing, func(row *legionfacts.Row) { row.PhaseHours[legion.ColumnReviewing] += 0.5 }},
		{"review_rounds", growing, func(row *legionfacts.Row) { row.ReviewRounds++ }},
		{"root_key", growing, func(row *legionfacts.Row) { row.Tree = "INFRA-1" }},
		{"tree_depth", growing, func(row *legionfacts.Row) { row.TreeDepth++ }},
		{"waiting_reason", growing, func(row *legionfacts.Row) { row.WaitingReason = "blocked" }},
		{"work_first_at", growing, func(row *legionfacts.Row) { first := row.WorkFirstAt.Add(-time.Hour); row.WorkFirstAt = &first }},
		{"updated_at", growing, func(row *legionfacts.Row) { row.UpdatedAt = row.UpdatedAt.Add(time.Minute) }},
		{"the implementing hours' rate", growing, func(row *legionfacts.Row) { row.Growth["phase_hours.implementing"] = 2 }},
		{"the work time's rate", growing, func(row *legionfacts.Row) { row.Growth["work_hours"] = 2 }},
		{"the slot hours' rate", growing, func(row *legionfacts.Row) { row.Growth["slot_hours"] = 2 }},
		{"the last moment of work's rate", growing, func(row *legionfacts.Row) { delete(row.Growth, "work_last_at") }},
		{"a rate for the review hours", growing, func(row *legionfacts.Row) { row.Growth["phase_hours.reviewing"] = 1 }},
		{"as_of and growth to null", growing, func(row *legionfacts.Row) { row.Growth = metrics.Growth{"backlog_or_icebox_hours": 1} }},
		{"the work time, which stands", standing, func(row *legionfacts.Row) { row.WorkHours++ }},
		{"the slot hours, which stand", standing, func(row *legionfacts.Row) { row.SlotHours++ }},
		{"the implementing hours, which stand", standing, func(row *legionfacts.Row) { row.PhaseHours[legion.ColumnImplementing]++ }},
		{"a column that comes to hold hours", standing, func(row *legionfacts.Row) { row.PhaseHours[legion.ColumnAccepting] = 0.25 }},
		{"the last moment of work, which stands", standing, func(row *legionfacts.Row) {
			last := row.WorkLastAt.Add(-30 * time.Minute)
			row.WorkLastAt = &last
		}},
		{"as_of and growth to set", standing, func(row *legionfacts.Row) { row.Growth["work_hours"] = 1 }},
	} {
		base, baseRevision := revisedEntry(t, key, moved.from(), at)
		row := moved.from()
		moved.move(&row)
		entry, revision := revisedEntry(t, key, row, at)
		before, err := json.Marshal(base)
		if err != nil {
			t.Fatalf("%s: encode the entry: %v", moved.name, err)
		}
		after, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("%s: encode the moved entry: %v", moved.name, err)
		}
		if string(before) == string(after) {
			t.Errorf("%s: the move left the entry %s as it was, so the row proves nothing", moved.name, after)
			continue
		}
		if revision == baseRevision {
			t.Errorf("%s moved and the entry's revision stayed %s: the entry %s is sent nothing of it", moved.name, revision, after)
		}
	}

	for _, closed := range []struct {
		name string
		row  func() legionfacts.Row
	}{{"a growing row", growing}, {"a standing row", standing}} {
		entry, revision := revisedEntry(t, key, closed.row(), at)
		grown, grownRevision := revisedEntry(t, key, closed.row(), later)
		if grownRevision != revision {
			t.Errorf("%s closed a day later moved its revision from %s to %s: an entry is sent only when its row moves",
				closed.name, revision, grownRevision)
		}
		if closed.row().Growth["work_hours"] == 0 {
			continue
		}
		if grown.WorkHours == entry.WorkHours || grown.SlotHours == entry.SlotHours ||
			grown.HoursByPhase[legion.ColumnImplementing] == entry.HoursByPhase[legion.ColumnImplementing] ||
			grown.WorkLastAt.Equal(*entry.WorkLastAt) || grown.AsOf.Equal(*entry.AsOf) {
			t.Errorf("%s closed a day later is %+v, the same values and moment as at %s, %+v, so the clock moved nothing",
				closed.name, grown, at, entry)
		}
	}

	for _, built := range []struct {
		name string
		row  func() legionfacts.Row
	}{{"a growing row", growing}, {"a standing row", standing}} {
		_, first := revisedEntry(t, key, built.row(), at)
		for range 7 {
			if _, again := revisedEntry(t, key, built.row(), at); again != first {
				t.Fatalf("two builds of %s answer the revisions %s and %s", built.name, first, again)
			}
		}
	}
}

// The board BenchmarkEncodeALegionBoardOfInfrasSize encodes: INFRA's open issues, each of which
// the board carries, and the closed issues of the trees its cards stand in, at the size R8's
// load gives the project.
const (
	infraOpenEntries   = 2600
	infraClosedEntries = 10000
)

// infraSizedLegionRows is the stored rows of a board of INFRA's size as the board's reads answer
// them at now: every open issue closed at now and growing its work time, its slot hours, its
// implementing hours and the end of its open bar, and every closed issue standing still at its
// close. The hours carry four decimals, as the computation rounds them, so each encodes as long
// as a stored row's does.
func infraSizedLegionRows(now time.Time) map[string]legionfacts.Row {
	rows := make(map[string]legionfacts.Row, infraOpenEntries+infraClosedEntries)
	for i := range infraOpenEntries + infraClosedEntries {
		hours := func(base float64) float64 { return math.Round((base+float64(i%37)*0.7311)*1e4) / 1e4 }
		opened := now.Add(-time.Duration(48+i%500) * time.Hour)
		first, last := opened.Add(30*time.Minute), now
		root, depth := i-i%5, min(i%5, 2)
		facts := metrics.IssueFacts{
			OpenedAt: opened,
			PhaseHours: map[string]float64{
				legion.ColumnQueue: hours(1.5), legion.ColumnDesign: hours(2.25),
				legion.ColumnImplementing: hours(3.5), legion.ColumnReviewing: hours(1.25),
			},
			WorkHours: hours(8.5), ReviewRounds: i % 3, Root: depth == 0, Tree: "INFRA-" + strconv.Itoa(1000+root),
			TreeDepth: depth, SlotHours: hours(6), WorkFirstAt: &first, WorkLastAt: &last,
		}
		row := legionfacts.Row{
			Project: "INFRA", Issue: "INFRA-" + strconv.Itoa(1000+i), UpdatedAt: now.Add(-time.Duration(i) * time.Second),
			ClosedFacts: metrics.ClosedFacts{IssueFacts: facts, AsOf: now},
		}
		if i < infraOpenEntries {
			row.Growth = metrics.Growth{"work_hours": 1, "slot_hours": 1, "phase_hours.implementing": 1, "work_last_at": 1}
			if i%4 == 0 {
				row.WaitingReason = "waiting for a slot"
			}
		} else {
			closed := opened.Add(time.Duration(12+i%30) * time.Hour)
			row.ClosedAt, row.WorkLastAt, row.AsOf = &closed, &closed, closed
			row.PhaseHours[legion.ColumnTesting], row.PhaseHours[legion.ColumnMerging] = hours(0.75), hours(0.5)
		}
		rows[row.Issue] = row
	}
	return rows
}

// BenchmarkEncodeALegionBoardOfInfrasSize measures what each recompute of an open `legion` view
// of INFRA's size pays to encode its body and revise every entry of it (encodeDeliveryBody), the
// encode and revision stages of the hub's recompute line, which R8 bounds. It reads no database:
// the body is legionBoardCompute's over the stored rows infraSizedLegionRows makes. The
// revision-ns/op metric is the part of each encode its revisions took.
//
//	go test -run '^$' -bench '^BenchmarkEncodeALegionBoardOfInfrasSize$' -benchtime 20x ./internal/dispatch/api/
func BenchmarkEncodeALegionBoardOfInfrasSize(b *testing.B) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	body := legionBoardCompute(&legionBoardPopulation{project: "INFRA", facts: infraSizedLegionRows(now)}, now)
	var revising time.Duration
	for b.Loop() {
		encoded, took, err := encodeDeliveryBody(legionBoardView, body)
		if err != nil {
			b.Fatalf("encode the board: %v", err)
		}
		if got := len(encoded.revs.Maps["issues"]); got != infraOpenEntries+infraClosedEntries {
			b.Fatalf("the encoded board revises %d issues entries, want %d", got, infraOpenEntries+infraClosedEntries)
		}
		revising += took
	}
	b.ReportMetric(float64(revising.Nanoseconds())/float64(b.N), "revision-ns/op")
}
