package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

// revisionByMap is a map entry's revision as the rule states it: the entry decoded into a map of
// its members, the excluded ones deleted, encoded again (encoding/json writes a map's keys in
// order) and hashed. deliveryLiveMapEntries reads the same bytes without building the map.
func revisionByMap(t *testing.T, entry json.RawMessage, excluded []string) string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(entry, &object); err != nil {
		t.Fatalf("decode %s: %v", entry, err)
	}
	for _, name := range excluded {
		delete(object, name)
	}
	kept, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encode %s: %v", entry, err)
	}
	return deliveryLiveRevision(kept)
}

// TestAMapEntrysRevisionIsTheRevisionOfItsDecodedMembersWithoutTheExcluded: for every entry of a
// Legion board's issues map, open and closed, with growth and without, and for entries written
// with their members out of name order, with escapes in a name and with whitespace,
// deliveryLiveMapEntries answers the revision the rule states (revisionByMap) and each entry's
// bytes as the field holds them; with nothing excluded, the hash of those bytes. A null field
// holds no entry. As a control, an entry whose excluded member moved keeps its revision, and one
// whose kept member moved does not.
func TestAMapEntrysRevisionIsTheRevisionOfItsDecodedMembersWithoutTheExcluded(t *testing.T) {
	t.Parallel()
	excluded := deliveryLiveKinds[legionBoardView].maps["issues"]
	if len(excluded) == 0 {
		t.Fatal("the Legion board's issues map excludes nothing, so this test examines no exclusion")
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	open, closed := now.Add(-48*time.Hour), now.Add(-time.Hour)
	growth := &legionBoardGrowth{WorkHours: 1, SlotHours: 1, HoursByPhase: map[string]float64{"implementing": 1}}
	issues := map[string]legionBoardFacts{
		"INFRA-1": {OpenedAt: open, HoursByPhase: map[string]float64{"implementing": 2.5, "planning": 0.25}, WorkHours: 2.75, RootKey: "INFRA-1", SlotHours: 2, UpdatedAt: now, AsOf: &now, Growth: growth},
		"INFRA-2": {OpenedAt: open, ClosedAt: &closed, HoursByPhase: map[string]float64{}, WorkHours: 5, ReviewRounds: 2, RootKey: "INFRA-1", TreeDepth: 1, UpdatedAt: closed},
	}
	field, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	// Written by hand: members out of name order, an escape in a name and in a value, a character
	// json.Marshal escapes for HTML, whitespace between tokens.
	field = append(field[:len(field)-1], []byte(`, "INFRA-3" : { "work_hours" : 1 , "as_of" : "2026-11-24T12:00:00Z", "a\u0062c": [1, {"b": 2}], "growth": {"work_hours": 1} }, "INFRA-4": {"x":"a\u0062","y":"<&>","as_of":null} }`)...)

	values, revs, err := deliveryLiveMapEntries(field, excluded)
	if err != nil {
		t.Fatalf("read the issues map: %v", err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(field, &entries); err != nil {
		t.Fatal(err)
	}
	if got, want := slices.Sorted(maps.Keys(values)), slices.Sorted(maps.Keys(entries)); !slices.Equal(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
	for key, entry := range entries {
		if want := revisionByMap(t, entry, excluded); revs[key] != want || values[key].rev != want {
			t.Errorf("%s: revision %s (value %s), want the rule's %s", key, revs[key], values[key].rev, want)
		}
		if string(values[key].raw) != string(entry) {
			t.Errorf("%s: bytes %s, want the field's %s", key, values[key].raw, entry)
		}
	}
	_, whole, err := deliveryLiveMapEntries(field, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, entry := range entries {
		if whole[key] != deliveryLiveRevision(entry) {
			t.Errorf("%s with nothing excluded: revision %s, want the hash of its bytes", key, whole[key])
		}
	}
	if values, _, err := deliveryLiveMapEntries(json.RawMessage(`null`), excluded); err != nil || len(values) != 0 {
		t.Errorf("a null field: %d entries, %v; want none", len(values), err)
	}

	// The control: the clock alone moves an open entry's excluded values; a stored value moving
	// moves its revision.
	later := now.Add(30 * time.Second)
	grown := issues["INFRA-1"]
	grown.WorkHours, grown.SlotHours, grown.AsOf = 2.75+30.0/3600, 2+30.0/3600, &later
	grown.HoursByPhase = map[string]float64{"implementing": 2.5 + 30.0/3600, "planning": 0.25}
	moved := issues["INFRA-1"]
	moved.ReviewRounds = 1
	for _, change := range []struct {
		name  string
		entry legionBoardFacts
		same  bool
	}{{"the clock alone", grown, true}, {"a review round", moved, false}} {
		raw, err := json.Marshal(map[string]legionBoardFacts{"INFRA-1": change.entry})
		if err != nil {
			t.Fatal(err)
		}
		_, next, err := deliveryLiveMapEntries(raw, excluded)
		if err != nil {
			t.Fatal(err)
		}
		if same := next["INFRA-1"] == revs["INFRA-1"]; same != change.same {
			t.Errorf("%s moved INFRA-1: revision kept %t, want %t", change.name, same, change.same)
		}
	}
}

// TestWhatTheIssuesMapsRevisionsCostAtInfrasSize logs what reading 4,500 open entries' revisions
// costs, the one-pass reading against the decode-and-encode it replaced, over the same bytes. It
// asserts only that the two agree; the farm's R8 is the measure of the recompute.
func TestWhatTheIssuesMapsRevisionsCostAtInfrasSize(t *testing.T) {
	t.Parallel()
	excluded := deliveryLiveKinds[legionBoardView].maps["issues"]
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	issues := make(map[string]legionBoardFacts, 4500)
	for i := range 4500 {
		growth := &legionBoardGrowth{WorkHours: 1, SlotHours: 1, HoursByPhase: map[string]float64{"implementing": 1}}
		issues[fmt.Sprintf("INFRA-%d", i)] = legionBoardFacts{
			OpenedAt: now.Add(-time.Duration(i) * time.Hour), HoursByPhase: map[string]float64{"implementing": float64(i) / 7, "planning": 1, "reviewing": 0.5},
			WorkHours: float64(i) / 3, RootKey: fmt.Sprintf("INFRA-%d", i/6), TreeDepth: i % 3, SlotHours: 1.5,
			WaitingReason: "slot", UpdatedAt: now, AsOf: &now, Growth: growth,
		}
	}
	field, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, revs, err := deliveryLiveMapEntries(field, excluded)
	onePass := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(field, &entries); err != nil {
		t.Fatal(err)
	}
	byMap := make(map[string]string, len(entries))
	for key, entry := range entries {
		byMap[key] = revisionByMap(t, entry, excluded)
	}
	decoded := time.Since(started)
	if !maps.Equal(revs, byMap) {
		t.Fatal("the one-pass revisions differ from the decode-and-encode revisions")
	}
	t.Logf("%d open entries, %d bytes: one pass %s, decode and encode %s", len(revs), len(field), onePass, decoded)
}
