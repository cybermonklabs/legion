package intake

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A settlement names the check runs the listener counted and then took out of its record in a field
// of its own, excluded_check_runs: each run by its name and id, sorted by name and then id, none of
// them in check_runs. The daemon reads each by its name and id alone, as the listener lists them,
// and a key of an entry it does not read is the listener's to add. The rest of the settlement
// decodes as it would without the field. A field left out - how the listener writes a head that
// excluded none - null or empty names no run excluded, and none of them is a list the daemon could
// not read.
func TestDecodeChecksReadsTheExcludedCheckRuns(t *testing.T) {
	runs := []record.AttemptRun{{Name: "ci-ok", ID: 10}, {Name: "unit", ID: 12}}
	without := settledChecks(t, decodeSettlement(t, settlementPayload(runs, []string{"ci-ok"}, []string{})))

	payload := settlementPayload(runs, []string{"ci-ok"}, []string{})
	payload["excluded_check_runs"] = []map[string]any{
		{"name": "ci-ok", "id": 136723497213}, {"name": "ci-ok", "id": 136723497214, "conclusion": "success"}, {"name": "lint", "id": 9},
	}
	decoded := decodeSettlement(t, payload)
	checks := settledChecks(t, decoded)
	want := []record.AttemptRun{{Name: "ci-ok", ID: 136723497213}, {Name: "ci-ok", ID: 136723497214}, {Name: "lint", ID: 9}}
	if !reflect.DeepEqual(checks.Excluded, want) || len(decoded.Unread) != 0 {
		t.Fatalf("excluded %+v, reported %q; want %+v read whole", checks.Excluded, decoded.Unread, want)
	}
	checks.Excluded = nil
	if !reflect.DeepEqual(checks, without) {
		t.Fatalf("the settlement decoded to %+v beside its excluded runs, want %+v as without the field", checks, without)
	}

	for _, tc := range []struct {
		name  string
		field any
	}{
		{"left out", nil},
		{"null", json.RawMessage("null")},
		{"empty", []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := settlementPayload(runs, []string{"ci-ok"}, []string{})
			if tc.field != nil {
				payload["excluded_check_runs"] = tc.field
			}
			decoded := decodeSettlement(t, payload)
			if checks := settledChecks(t, decoded); checks.Excluded != nil || len(decoded.Unread) != 0 || !reflect.DeepEqual(checks, without) {
				t.Fatalf("excluded %s = %+v, reported %q, settlement %+v; want no run excluded, nothing unread, %+v", tc.name,
					checks.Excluded, decoded.Unread, checks, without)
			}
		})
	}
}

// A list of excluded check runs the daemon cannot read is taken as no run excluded, and named in
// the decoded message's unread list, which the consumer logs; the settlement itself stands with
// every run, name and group as it would without the field. Each shape below is one no listener
// writes: a field that is no list, an entry with no name or with an id that is no run's, an id
// that is no whole number, and a list out of the listener's order or naming one run twice.
func TestDecodeChecksTakesAnUnreadableExcludedListAsNone(t *testing.T) {
	runs := []record.AttemptRun{{Name: "ci-ok", ID: 10}, {Name: "unit", ID: 12}}
	without := settledChecks(t, decodeSettlement(t, settlementPayload(runs, []string{"ci-ok"}, []string{})))
	for _, tc := range []struct {
		name  string
		field any
	}{
		{"a string", "ci-ok"},
		{"an object", map[string]any{"name": "ci-ok", "id": 20}},
		{"an entry with no name", []map[string]any{{"id": 20}}},
		{"an entry with an empty name", []map[string]any{{"name": "", "id": 20}}},
		{"an entry with no id", []map[string]any{{"name": "ci-ok"}}},
		{"an entry with the id 0", []map[string]any{{"name": "ci-ok", "id": 0}}},
		{"an entry with a negative id", []map[string]any{{"name": "ci-ok", "id": -20}}},
		{"an id that is a string", []map[string]any{{"name": "ci-ok", "id": "20"}}},
		{"an id that is a fraction", []map[string]any{{"name": "ci-ok", "id": 20.5}}},
		{"names out of order", []map[string]any{{"name": "lint", "id": 9}, {"name": "ci-ok", "id": 20}}},
		{"ids of one name out of order", []map[string]any{{"name": "ci-ok", "id": 21}, {"name": "ci-ok", "id": 20}}},
		{"one run twice", []map[string]any{{"name": "ci-ok", "id": 20}, {"name": "ci-ok", "id": 20}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := settlementPayload(runs, []string{"ci-ok"}, []string{})
			payload["excluded_check_runs"] = tc.field
			decoded := decodeSettlement(t, payload)
			checks := settledChecks(t, decoded)
			if checks.Excluded != nil {
				t.Fatalf("excluded %+v, want none from a list the daemon cannot read", checks.Excluded)
			}
			if len(decoded.Unread) != 1 || !strings.HasPrefix(decoded.Unread[0], "excluded_check_runs ") ||
				!strings.Contains(decoded.Unread[0], statusesHead) {
				t.Fatalf("reported %q, want the field named once with the head %s", decoded.Unread, statusesHead)
			}
			if !reflect.DeepEqual(checks, without) {
				t.Fatalf("the settlement decoded to %+v, want %+v: an unreadable list costs the rest nothing", checks, without)
			}
		})
	}
}
