package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// plainLegionBoardFacts is the issues entry's shape with no method of its own: what encoding/json
// writes of it by reflection, field by field, its growth among them, which has no method either.
// A defined type takes its fields from the type it is defined on and none of its methods, so a
// field added to the entry or to its growth is a field of its copy as well, and the marshal
// comparisons of TestAnIssuesEntryIsWrittenAsEncodingJSONWritesItsFields fail when the entry's
// hand-written encoder leaves it out.
type plainLegionBoardFacts legionBoardFacts

// marshalPlainLegionEntry is what encoding/json writes of the entry by reflection, so the
// expected bytes owe nothing to the entry's own encoder.
func marshalPlainLegionEntry(entry legionBoardFacts) ([]byte, error) {
	return json.Marshal(plainLegionBoardFacts(entry))
}

// TestAnIssuesEntryIsWrittenAsEncodingJSONWritesItsFields: the issues entry's own encoder
// (legionBoardFacts' MarshalJSON, which writes the growth inside it) writes, byte for byte, what
// encoding/json writes by reflection for a struct of the same shape with no method, both through
// json.Marshal, which the GET route's encoder and the live hub's encodeDeliveryBody call, and as
// the method's own bytes, which encoding/json copies as they stand. The table takes every field
// to its edges: each pointer nil and set, each map nil, empty and filled (with more columns than
// the board has, and names that need escaping), floats at zero, negative zero, negative, either
// side of the bounds where encoding/json writes an exponent, the smallest and the largest, and
// strings that need escaping or hold invalid UTF-8, and times with and without nanoseconds, at
// the zero time and the ends of the four-digit years, and in a location other than UTC.
//
// The reference entry is the plain copy, so the entry's own encoder contributes nothing to its
// expected bytes. A seeded history exercises every entry alongside the hand-written cases.
//
// No stored value is NaN or infinite: the facts consumer computes every value from lengths of
// time and counts, the JSON columns cannot hold either, and a read grows a value by a finite rate
// over a finite span (legionfacts.Row.At). The table still holds them, and a time encoding/json
// refuses, to show the encoder refuses each as encoding/json does, rather than writing a body no
// reader could parse.
func TestAnIssuesEntryIsWrittenAsEncodingJSONWritesItsFields(t *testing.T) {
	t.Parallel()
	whole := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	nanos := time.Date(2026, 11, 27, 12, 0, 0, 123456789, time.UTC)
	tenths := time.Date(2026, 11, 27, 12, 0, 0, 100000000, time.UTC)
	east := time.Date(2026, 3, 1, 5, 30, 0, 500, time.FixedZone("IST", 5*3600+30*60))
	west := time.Date(1999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -8*3600))
	oddOffset := time.Date(2026, 6, 1, 0, 0, 0, 0, time.FixedZone("", 3600+45))
	firstYear := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	lastYear := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	manyColumns := map[string]float64{}
	for column := range 20 {
		manyColumns[fmt.Sprintf("column_%02d", 19-column)] = float64(column) * 0.25
	}
	open := legionBoardFacts{
		OpenedAt: whole, HoursByPhase: map[string]float64{"queue": 1.5, "design": 2.25, "implementing": 3.5, "reviewing": 1.25},
		WorkHours: 8.5, ReviewRounds: 2, RootKey: "INFRA-1000", TreeDepth: 1, SlotHours: 6, WaitingReason: "waiting for a slot",
		WorkFirstAt: new(whole.Add(-time.Hour)), WorkLastAt: new(nanos), UpdatedAt: whole, AsOf: new(nanos),
		Growth: &legionBoardGrowth{HoursByPhase: map[string]float64{"implementing": 1}, WorkHours: 1, SlotHours: 1, WorkLastAt: 1},
	}
	with := func(change func(*legionBoardFacts)) legionBoardFacts {
		entry := open
		growth := *open.Growth
		entry.Growth = &growth
		change(&entry)
		return entry
	}
	encoded := []struct {
		name  string
		entry legionBoardFacts
	}{
		{"an open issue's entry", open},
		{"a closed issue's entry, every moment set and nothing growing", with(func(f *legionBoardFacts) {
			f.ClosedAt, f.AsOf, f.Growth, f.WaitingReason = new(tenths), nil, nil, ""
		})},
		{"every pointer nil", legionBoardFacts{OpenedAt: whole, UpdatedAt: whole, HoursByPhase: map[string]float64{}}},
		{"the zero entry: the zero times and the nil maps", legionBoardFacts{}},
		{"many columns, out of order", with(func(f *legionBoardFacts) { f.HoursByPhase, f.Growth.HoursByPhase = manyColumns, manyColumns })},
		{"columns whose names need escaping, and the empty name", with(func(f *legionBoardFacts) {
			f.HoursByPhase = map[string]float64{"<b>": 1, "a&b": 2, `"quoted"`: 3, "back\\slash": 4, "": 5, "\u2028": 6}
		})},
		{"a growth with no columns, nil and empty", with(func(f *legionBoardFacts) { f.Growth.HoursByPhase = nil })},
		{"a growth with an empty map", with(func(f *legionBoardFacts) { f.Growth.HoursByPhase = map[string]float64{} })},
		{"zero and negative zero", with(func(f *legionBoardFacts) {
			f.WorkHours, f.SlotHours = 0, math.Copysign(0, -1)
			f.HoursByPhase = map[string]float64{"queue": 0, "design": math.Copysign(0, -1)}
			f.Growth.WorkHours, f.Growth.SlotHours, f.Growth.WorkLastAt = 0, math.Copysign(0, -1), 0
		})},
		{"negative values and counts", with(func(f *legionBoardFacts) {
			f.WorkHours, f.SlotHours, f.ReviewRounds, f.TreeDepth = -1.5, -0.0001, -3, -1
			f.HoursByPhase = map[string]float64{"queue": -2.75}
			f.Growth.WorkHours, f.Growth.SlotHours, f.Growth.WorkLastAt = -1, -0.5, -1
		})},
		{"the bounds of the plain form: 1e-6 and just under it", with(func(f *legionBoardFacts) {
			f.WorkHours, f.SlotHours = 1e-6, 9.99e-7
			f.HoursByPhase = map[string]float64{"queue": -1e-6, "design": -9.99e-7}
			f.Growth.WorkHours, f.Growth.SlotHours = 1e-6, 9.99e-7
		})},
		{"the bounds of the plain form: 1e21 and just under it", with(func(f *legionBoardFacts) {
			f.WorkHours, f.SlotHours = 1e21, 999999999999999900000
			f.HoursByPhase = map[string]float64{"queue": -1e21, "design": 1e20}
			f.Growth.WorkHours, f.Growth.SlotHours = 1e21, 1e20
		})},
		{"tiny and huge values, and exponents of one and of more digits", with(func(f *legionBoardFacts) {
			f.WorkHours, f.SlotHours = math.SmallestNonzeroFloat64, math.MaxFloat64
			f.HoursByPhase = map[string]float64{"queue": 1e-7, "design": 2.5e-10, "testing": -3e-300, "merging": 1e300}
			f.Growth.WorkHours, f.Growth.SlotHours, f.Growth.WorkLastAt = 5e-9, -1.25e22, math.MaxFloat64
		})},
		{"hours of four decimals, as the computation rounds them", with(func(f *legionBoardFacts) {
			f.WorkHours, f.SlotHours = 123456.7891, 0.0001
			f.HoursByPhase = map[string]float64{"queue": 0.1 + 0.2, "design": 1.0 / 3}
		})},
		{"the largest counts", with(func(f *legionBoardFacts) { f.ReviewRounds, f.TreeDepth = math.MaxInt, math.MinInt })},
		{"strings that need escaping", with(func(f *legionBoardFacts) {
			f.RootKey, f.WaitingReason = `a "quoted" \ key`, "<b>tab\tline\nfeed\rform\fback\bbell\x01 & del\x7f</b>"
		})},
		{"strings beyond ASCII and invalid UTF-8", with(func(f *legionBoardFacts) {
			f.RootKey, f.WaitingReason = "naïve — ünïcode ✓ 𝄞", "line\u2028para\u2029 bad \xff byte \xc3"
		})},
		{"times with nanoseconds and with tenths", with(func(f *legionBoardFacts) {
			f.OpenedAt, f.UpdatedAt, f.ClosedAt, f.WorkFirstAt = nanos, tenths, new(nanos), new(tenths)
		})},
		{"times in locations other than UTC", with(func(f *legionBoardFacts) {
			f.OpenedAt, f.UpdatedAt, f.ClosedAt, f.WorkFirstAt, f.WorkLastAt, f.AsOf = east, west, new(east), new(west), new(oddOffset), new(east)
		})},
		{"the first and the last four-digit years", with(func(f *legionBoardFacts) {
			f.OpenedAt, f.UpdatedAt, f.ClosedAt, f.AsOf = firstYear, lastYear, new(firstYear), new(lastYear)
		})},
	}
	for _, row := range encoded {
		want, err := marshalPlainLegionEntry(row.entry)
		if err != nil {
			t.Fatalf("%s: encoding/json refuses the copy type: %v", row.name, err)
		}
		got, err := json.Marshal(row.entry)
		if err != nil {
			t.Errorf("%s: json.Marshal of the entry: %v", row.name, err)
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s: json.Marshal of the entry writes\n%s\nwhere encoding/json writes its fields\n%s", row.name, got, want)
		}
		own, err := row.entry.MarshalJSON()
		if err != nil {
			t.Errorf("%s: the entry's MarshalJSON: %v", row.name, err)
		} else if !bytes.Equal(own, want) {
			t.Errorf("%s: the entry's MarshalJSON writes\n%s\nwhere encoding/json writes its fields\n%s", row.name, own, want)
		}
	}

	refused := []struct {
		name  string
		entry legionBoardFacts
	}{
		{"a NaN work time", with(func(f *legionBoardFacts) { f.WorkHours = math.NaN() })},
		{"an infinite slot time", with(func(f *legionBoardFacts) { f.SlotHours = math.Inf(1) })},
		{"a NaN column", with(func(f *legionBoardFacts) { f.HoursByPhase = map[string]float64{"queue": math.NaN()} })},
		{"an infinite rate of a column", with(func(f *legionBoardFacts) { f.Growth.HoursByPhase = map[string]float64{"queue": math.Inf(-1)} })},
		{"an infinite rate of the last moment", with(func(f *legionBoardFacts) { f.Growth.WorkLastAt = math.Inf(1) })},
		{"a five-digit year", with(func(f *legionBoardFacts) { f.UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) })},
		{"a year before the first", with(func(f *legionBoardFacts) { f.ClosedAt = new(time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)) })},
		{"an offset of a whole day", with(func(f *legionBoardFacts) {
			f.AsOf = new(time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", 24*3600)))
		})},
	}
	for _, row := range refused {
		if _, err := marshalPlainLegionEntry(row.entry); err == nil {
			t.Fatalf("%s: encoding/json writes the copy type, so the row refuses nothing", row.name)
		}
		if got, err := json.Marshal(row.entry); err == nil {
			t.Errorf("%s: json.Marshal of the entry writes %s, where encoding/json refuses its fields", row.name, got)
		}
		if got, err := row.entry.MarshalJSON(); err == nil {
			t.Errorf("%s: the entry's MarshalJSON writes %s, where encoding/json refuses its fields", row.name, got)
		}
	}
	t.Logf("%d entries written as encoding/json writes their fields, %d refused as it refuses them", len(encoded), len(refused))

	t.Run("seeded history", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "ACME", Trees: 2, Now: now})
		nameLegionDaemon(t, handler, fixture.Project, "http://legion-acme.internal.example:13370")
		rows, _, err := legionfacts.ComputeProject(t.Context(), database.Pool, fixture.Project, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legionfacts.WriteProject(t.Context(), database.Pool,
			legionfacts.ProjectWrite{Project: fixture.Project, Rows: rows, Whole: true}); err != nil {
			t.Fatal(err)
		}
		board := legionBoardOf(t, handler, fixture.Project)
		if len(fixture.PhaseWork) == 0 || len(board.Issues) != len(fixture.PhaseWork) {
			t.Fatalf("board carries %d entries, want all %d issues with phase work", len(board.Issues), len(fixture.PhaseWork))
		}
		for _, key := range fixture.PhaseWork {
			if _, ok := board.Issues[key]; !ok {
				t.Errorf("seeded issue %s has no board entry", key)
			}
		}
		growing := 0
		for key, entry := range board.Issues {
			if entry.Growth != nil {
				growing++
			}
			want, err := marshalPlainLegionEntry(entry)
			if err != nil {
				t.Fatalf("%s: marshal the plain entry: %v", key, err)
			}
			got, err := entry.MarshalJSON()
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s: MarshalJSON = %s (%v), want %s", key, got, err, want)
			}
		}
		if growing == 0 || growing == len(board.Issues) {
			t.Errorf("%d of %d seeded entries grow, want growing and standing entries", growing, len(board.Issues))
		}
		t.Logf("compared %d seeded entries byte for byte to encoding/json (%d growing)", len(board.Issues), growing)
	})
}

// TestTheHubWritesTheLegionBoardsBodyAsTheGETWritesIt: the body the live hub sends of a `legion`
// view, which it writes from each issues entry's own encoding and json.Marshal of the board with
// its issues left out (legionBoardResponse's encodedMap, composeDeliveryBody), is byte for byte
// json.Marshal of the board, and the GET's body, which WriteJSON writes through encoding/json, is
// the same bytes and a newline. Every field the hub sends whole is that body's field, and every
// entry it holds is that body's entry, at the board's own revision of it (mapRevisions).
//
// The boards are one with every top-level field set — a header whose every pointer is set, a
// column whose card sets every field, the daemon's settings (one written with spaces, which
// encoding/json compacts, and one string it escapes) and a computed_at with nanoseconds — and
// entries of every kind: an open issue's that grows, an open issue's that stands, a closed
// issue's that stands, a closed issue's whose slot hours still grow, and keys that sort as bytes
// and need escaping; and boards whose map is empty, and nil.
func TestTheHubWritesTheLegionBoardsBodyAsTheGETWritesIt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 12, 0, 0, 123456789, time.UTC)
	opened, closed := now.Add(-72*time.Hour), now.Add(-3*time.Hour)
	growing := &legionBoardGrowth{HoursByPhase: map[string]float64{"implementing": 1}, WorkHours: 1, SlotHours: 1, WorkLastAt: 1}
	issues := map[string]legionBoardFacts{
		"INFRA-9": {
			OpenedAt: opened, HoursByPhase: map[string]float64{"queue": 1.5, "implementing": 3.25}, WorkHours: 4.75,
			ReviewRounds: 1, RootKey: "INFRA-9", SlotHours: 3.25, WaitingReason: "", WorkFirstAt: new(opened.Add(time.Hour)),
			WorkLastAt: new(now), UpdatedAt: now.Add(-time.Minute), AsOf: new(now), Growth: growing,
		},
		"INFRA-10": {
			OpenedAt: opened, HoursByPhase: map[string]float64{}, RootKey: "INFRA-9", TreeDepth: 1,
			WaitingReason: "waiting for a slot", UpdatedAt: now.Add(-time.Hour),
		},
		"INFRA-11": {
			OpenedAt: opened, ClosedAt: new(closed), HoursByPhase: map[string]float64{"merging": 0.5, "testing": 0.75},
			WorkHours: 12.5, ReviewRounds: 2, RootKey: "INFRA-9", TreeDepth: 1, SlotHours: 6,
			WorkFirstAt: new(opened.Add(2 * time.Hour)), WorkLastAt: new(closed), UpdatedAt: closed,
		},
		"INFRA-12": {
			OpenedAt: opened, ClosedAt: new(closed), HoursByPhase: map[string]float64{"merging": 1}, WorkHours: 9,
			RootKey: "INFRA-12", SlotHours: 2.5, WorkLastAt: new(closed), UpdatedAt: closed, AsOf: new(now),
			Growth: &legionBoardGrowth{HoursByPhase: map[string]float64{}, SlotHours: 1},
		},
		"INFRA-<13>&": {OpenedAt: opened, HoursByPhase: map[string]float64{}, RootKey: "INFRA-<13>&", UpdatedAt: closed},
	}
	everyField := legionBoardResponse{
		Project: "INFRA",
		Header: legionBoardHeader{
			SlotsInUse: 3, SlotCap: new(4), P0Holders: new(1), Trees: 2, QueueLength: 1, ReadAt: new(now.Add(-time.Second)),
			ReadOK: false, ReadError: new("the daemon answered 503 <busy>"), FactsUpdatedAt: new(closed), FactsCursorAgeSeconds: new(7),
		},
		Columns: []legionBoardColumn{
			{ID: "queue", Label: "Queue", Cards: []legionBoardCard{{
				Key: "INFRA-10", Title: `a "quoted" & <escaped> title`, Priority: new(1), Phase: "backlog",
				PhaseSince: new(opened), Slot: new(2), WaitingPlace: new(1), Roles: []string{"architect"},
				BlockedBy: []string{"INFRA-9"}, WaitingOn: &legionBoardWait{Kind: "ask", Since: new(opened), AnsweredAt: new(now)},
				SlotUse: new("own"), Waiting: &legionBoardWaiting{Reason: "blocked", Since: new(opened), BlockedBy: []string{"INFRA-9"}},
				Tree: "INFRA-9", Children: 2,
			}}},
			{ID: "design", Label: "Design", Cards: []legionBoardCard{}},
		},
		Issues:     issues,
		Settings:   map[string]json.RawMessage{"max_slots": json.RawMessage(`4`), "phases": json.RawMessage(`{ "a" : [1, 2] }`), "name": json.RawMessage(`"<daemon>"`)},
		ComputedAt: now,
	}
	empty, none := everyField, everyField
	empty.Issues, none.Issues = map[string]legionBoardFacts{}, nil
	boards := []struct {
		name string
		body legionBoardResponse
	}{
		{"a board with every field set and entries of every kind", everyField},
		{"a board whose issues map is empty", empty},
		{"a board whose issues map is nil", none},
	}
	for _, board := range boards {
		want, err := json.Marshal(board.body)
		if err != nil {
			t.Fatalf("%s: json.Marshal: %v", board.name, err)
		}
		encoded, _, err := encodeDeliveryBody(legionBoardView, board.body)
		if err != nil {
			t.Fatalf("%s: the hub's encoding: %v", board.name, err)
		}
		if !bytes.Equal(encoded.body, want) {
			t.Errorf("%s: the hub writes the body\n%s\nwhere json.Marshal of the board writes\n%s", board.name, encoded.body, want)
		}
		answered := httptest.NewRecorder()
		WriteJSON(answered, http.StatusOK, board.body)
		if got := answered.Body.String(); got != string(want)+"\n" {
			t.Errorf("%s: the GET writes the body\n%s\nwhere the hub writes\n%s", board.name, got, encoded.body)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(want, &top); err != nil {
			t.Fatalf("%s: read json.Marshal's body back: %v", board.name, err)
		}
		for name := range deliveryLiveKinds[legionBoardView].whole {
			if held := encoded.fields[name].raw; !bytes.Equal(held, top[name]) {
				t.Errorf("%s: the hub holds the field %s as\n%s\nwhere the body holds\n%s", board.name, name, held, top[name])
			}
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(top["issues"], &entries); err != nil {
			t.Fatalf("%s: read the body's issues back: %v", board.name, err)
		}
		revisions, _ := board.body.mapRevisions("issues")
		held := encoded.maps["issues"]
		if len(held) != len(entries) || len(encoded.revs.Maps["issues"]) != len(entries) {
			t.Errorf("%s: the hub holds %d issues entries and %d revisions, where the body holds %d entries",
				board.name, len(held), len(encoded.revs.Maps["issues"]), len(entries))
		}
		for key, entry := range entries {
			if !bytes.Equal(held[key].raw, entry) {
				t.Errorf("%s: the hub holds %s's entry as\n%s\nwhere the body holds\n%s", board.name, key, held[key].raw, entry)
			}
			if held[key].rev != revisions[key] || encoded.revs.Maps["issues"][key] != revisions[key] {
				t.Errorf("%s: the hub holds %s's entry at the revision %q (%q in its revisions), want the board's %q",
					board.name, key, held[key].rev, encoded.revs.Maps["issues"][key], revisions[key])
			}
		}
		t.Logf("%s: %d bytes, %d entries, written by the hub as json.Marshal and the GET write them", board.name, len(want), len(entries))
	}
}
