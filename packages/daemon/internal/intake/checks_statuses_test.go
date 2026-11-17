package intake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// statusesSubject is the subject of every settlement below: sjawhar/legion#42's checks.
const statusesSubject = "notifications.github.sjawhar.legion.pr.42.checks"

// statusesHead is the head every settlement below is of.
const statusesHead = "abcdef1234567890abcdef1234567890abcdef12"

// settlementPayload is a settlement of statusesHead as Envoy's listener writes one, with the check
// runs, failed and cancelled names given and no statuses field.
func settlementPayload(runs []record.AttemptRun, failed, cancelled []string) map[string]any {
	listed := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		listed = append(listed, map[string]any{"name": run.Name, "id": run.ID})
	}
	group := func(names []string) map[string]any {
		return map[string]any{"count": len(names), "checks": append([]string{}, names...)}
	}
	return map[string]any{"kind": "checks", "repo": "sjawhar/legion", "number": "42", "sha": statusesHead,
		"check_runs": listed, "generation": 3, "snapshot": "snapshot-3", "settled_at": 1790124840596,
		"failed": group(failed), "cancelled": group(cancelled), "passed": group(nil), "running": group(nil),
		"queued": group(nil), "skipped": group(nil)}
}

// decodeSettlement decodes payload as the consumer does, through its Envoy envelope.
func decodeSettlement(t *testing.T, payload map[string]any) decodedMessage {
	t.Helper()
	text, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMessage(statusesSubject, "CAPTURE", capturedRepositories, envelopeJSON(t, "checks-statuses", "github", string(text)))
	if err != nil {
		t.Fatalf("decode the settlement: %v", err)
	}
	return decoded
}

// settledChecks is the PullRequestChecks a decoded settlement holds, failing the test on any other.
func settledChecks(t *testing.T, decoded decodedMessage) PullRequestChecks {
	t.Helper()
	checks, ok := decoded.Fact.(PullRequestChecks)
	if !ok {
		t.Fatalf("the settlement decoded to %#v, want its checks", decoded.Fact)
	}
	return checks
}

// A settlement names each commit status of its head in a field of its own, `statuses`: each
// context with its state and GitHub's created_at, which the daemon reads in context order whatever
// order it arrives in, with the time in UTC whatever offset it is written at, and keeps apart from
// the check runs, so a status that shares its name with a failing check run changes nothing of the
// runs, the failing names or the cancelled ones. A key of an entry the daemon does not read is the
// listener's to add. A field left out, null or empty names no status, and none of them is a list
// the daemon could not read.
func TestDecodeChecksReadsStatuses(t *testing.T) {
	runs := []record.AttemptRun{{Name: "build", ID: 11}, {Name: "unit", ID: 12}}
	payload := settlementPayload(runs, []string{"unit"}, []string{})
	payload["statuses"] = []map[string]any{
		{"context": "unit", "state": "success", "created_at": "2026-11-17T05:01:00Z", "target_url": "https://example.test/statuses/1"},
		{"context": "ci/untested-draft", "state": "failure", "created_at": "2026-11-17T05:02:00Z"},
		{"context": "deploy", "state": "error", "created_at": "2026-11-17T07:03:00+02:00"},
	}
	decoded := decodeSettlement(t, payload)
	checks := settledChecks(t, decoded)
	want := []record.CommitStatus{{Context: "ci/untested-draft", State: record.CommitStatusFailure, CreatedAt: time.Date(2026, 11, 17, 5, 2, 0, 0, time.UTC)},
		{Context: "deploy", State: record.CommitStatusError, CreatedAt: time.Date(2026, 11, 17, 5, 3, 0, 0, time.UTC)},
		{Context: "unit", State: record.CommitStatusSuccess, CreatedAt: time.Date(2026, 11, 17, 5, 1, 0, 0, time.UTC)}}
	if !reflect.DeepEqual(checks.Statuses, want) || checks.StatusesUnread || len(decoded.Unread) != 0 {
		t.Fatalf("statuses = %+v, unread %t, reported %q; want %+v read whole", checks.Statuses, checks.StatusesUnread, decoded.Unread, want)
	}
	if !reflect.DeepEqual(checks.CheckRuns, runs) || !reflect.DeepEqual(checks.Failing, []string{"unit"}) || len(checks.Cancelled) != 0 {
		t.Fatalf("runs %+v, failing %v, cancelled %v; want the two runs and unit failing: a status named unit is no check run",
			checks.CheckRuns, checks.Failing, checks.Cancelled)
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
			payload := settlementPayload(runs, []string{"unit"}, []string{})
			if tc.field != nil {
				payload["statuses"] = tc.field
			}
			decoded := decodeSettlement(t, payload)
			if checks := settledChecks(t, decoded); len(checks.Statuses) != 0 || checks.StatusesUnread || len(decoded.Unread) != 0 {
				t.Fatalf("statuses %s = %+v, unread %t, reported %q; want no status, read", tc.name, checks.Statuses, checks.StatusesUnread, decoded.Unread)
			}
		})
	}
}

// fixtureName is a check name as packages/contracts/fixtures/checks-settlement writes it: a string,
// or {repeat, times, then}, repeat written times over and then then.
type fixtureName string

func (n *fixtureName) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*n = fixtureName(name)
		return nil
	}
	var built struct {
		Repeat string `json:"repeat"`
		Times  int    `json:"times"`
		Then   string `json:"then"`
	}
	if err := json.Unmarshal(data, &built); err != nil {
		return err
	}
	*n = fixtureName(strings.Repeat(built.Repeat, built.Times) + built.Then)
	return nil
}

// The settlement of the shared fixture (packages/contracts/fixtures/checks-settlement), which
// Envoy's listener test publishes from the fixture's check runs and statuses, names three statuses,
// one of them `test` failing beside the passed check run `test`, each with its created_at. The
// daemon reads its check runs, its failing names and its cancelled names as it reads the same
// settlement without the field, which is the settlement an earlier listener publishes, and as the
// fixture gives them: the field adds the statuses, each with its created_at, as the fixture writes
// them, and takes nothing from the rest, and the failing status named test is in no list of check
// runs.
func TestDecodeChecksReadsTheNewSettlementsCheckRunsAsToday(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "checks-settlement", "listener-rules.json"))
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fixture struct {
		Settlement struct {
			CheckRuns []struct {
				Name fixtureName `json:"name"`
				ID   int64       `json:"id"`
			} `json:"check_runs"`
			Failed    []fixtureName   `json:"failed"`
			Cancelled []fixtureName   `json:"cancelled"`
			Statuses  json.RawMessage `json:"statuses"`
		} `json:"settlement"`
	}
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode the shared fixture: %v", err)
	}
	names := func(list []fixtureName) []string {
		out := make([]string, 0, len(list))
		for _, name := range list {
			out = append(out, string(name))
		}
		return out
	}
	runs := make([]record.AttemptRun, 0, len(fixture.Settlement.CheckRuns))
	for _, run := range fixture.Settlement.CheckRuns {
		runs = append(runs, record.AttemptRun{Name: string(run.Name), ID: run.ID})
	}
	failed, cancelled := names(fixture.Settlement.Failed), names(fixture.Settlement.Cancelled)
	var statuses []record.CommitStatus
	if err := json.Unmarshal(fixture.Settlement.Statuses, &statuses); err != nil {
		t.Fatalf("decode the shared fixture's statuses: %v", err)
	}
	if len(runs) == 0 || len(failed) == 0 || len(cancelled) == 0 || len(statuses) != 3 ||
		slices.ContainsFunc(statuses, func(status record.CommitStatus) bool { return status.CreatedAt.IsZero() }) {
		t.Fatalf("the fixture's settlement holds %d runs, %d failed, %d cancelled and statuses %+v; want some of each and its three statuses, each with its created_at",
			len(runs), len(failed), len(cancelled), statuses)
	}

	without := settlementPayload(runs, failed, cancelled)
	with := settlementPayload(runs, failed, cancelled)
	// The fixture's statuses, byte for byte as the fixture writes them, as the listener does.
	with["statuses"] = fixture.Settlement.Statuses
	today, withStatuses := decodeSettlement(t, without), decodeSettlement(t, with)
	before, after := settledChecks(t, today), settledChecks(t, withStatuses)

	wantRuns := slices.Clone(runs)
	slices.SortFunc(wantRuns, func(a, b record.AttemptRun) int { return strings.Compare(a.Name, b.Name) })
	for _, got := range []PullRequestChecks{before, after} {
		if !reflect.DeepEqual(got.CheckRuns, wantRuns) || !reflect.DeepEqual(got.Failing, failed) || !reflect.DeepEqual(got.Cancelled, cancelled) {
			t.Fatalf("decoded runs %+v, failing %v, cancelled %v; want the fixture's %+v, %v and %v", got.CheckRuns, got.Failing,
				got.Cancelled, wantRuns, failed, cancelled)
		}
	}
	if slices.Contains(after.Failing, "test") {
		t.Fatalf("failing %v names test, want the failing status named test kept out of the check runs", after.Failing)
	}
	if !reflect.DeepEqual(after.Statuses, statuses) || after.StatusesUnread || len(withStatuses.Unread) != 0 {
		t.Fatalf("the fixture's statuses decoded to %+v, unread %t, reported %q; want %+v", after.Statuses, after.StatusesUnread,
			withStatuses.Unread, statuses)
	}
	if len(before.Statuses) != 0 || before.StatusesUnread || len(today.Unread) != 0 {
		t.Fatalf("the settlement without the field decoded statuses %+v, unread %t, reported %q; want none, read",
			before.Statuses, before.StatusesUnread, today.Unread)
	}
}

// A statuses field the daemon cannot read costs the settlement nothing else: its check runs, its
// failing and cancelled names, its head and its numbering decode as they do without the field,
// the fact is marked so the pull request keeps the statuses it holds for the head
// (PullRequestChecks.StatusesUnread), and the field is named in the decoded message's unread
// list, which the consumer logs at WARN. Each of the ten shapes below is one no listener writes: a
// field that is a string, and one that is an object rather than a list; an entry with no context;
// a state GitHub has but the listener never records; a state in capitals, which GraphQL uses and
// the listener never writes; a context named twice; an entry that is no object; a context that is
// no string; an entry with no created_at; and a created_at that is no RFC 3339 time.
func TestDecodeChecksKeepsTheCheckRunsOfAnUnreadableStatusList(t *testing.T) {
	runs := []record.AttemptRun{{Name: "build", ID: 11}, {Name: "unit", ID: 12}}
	want := settledChecks(t, decodeSettlement(t, settlementPayload(runs, []string{"unit"}, []string{"build"})))
	want.StatusesUnread = true
	const at = "2026-11-17T05:01:00Z"
	for _, tc := range []struct {
		name  string
		field any
	}{
		{"a string", "ci failure"},
		{"an object", map[string]any{"context": "ci", "state": "failure", "created_at": at}},
		{"an entry with no context", []map[string]any{{"context": "", "state": "failure", "created_at": at}}},
		{"a pending status", []map[string]any{{"context": "ci", "state": "pending", "created_at": at}}},
		{"a state in capitals", []map[string]any{{"context": "ci", "state": "FAILURE", "created_at": at}}},
		{"a context twice", []map[string]any{{"context": "ci", "state": "failure", "created_at": at}, {"context": "ci", "state": "success", "created_at": at}}},
		{"an entry that is no object", []any{"ci"}},
		{"a context that is no string", []map[string]any{{"context": 7, "state": "failure", "created_at": at}}},
		{"an entry with no created_at", []map[string]any{{"context": "deploy", "state": "success", "created_at": at}, {"context": "ci", "state": "failure"}}},
		{"a created_at that is no RFC 3339 time", []map[string]any{{"context": "ci", "state": "failure", "created_at": "2026-11-17 05:01:00"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := settlementPayload(runs, []string{"unit"}, []string{"build"})
			payload["statuses"] = tc.field
			decoded := decodeSettlement(t, payload)
			if got := settledChecks(t, decoded); !reflect.DeepEqual(got, want) {
				t.Fatalf("the settlement with statuses %s decoded to %#v, want %#v: the rest as without the field, and the list unread", tc.name, got, want)
			}
			if len(decoded.Unread) != 1 || !strings.HasPrefix(decoded.Unread[0], "statuses ") || !strings.Contains(decoded.Unread[0], statusesHead) {
				t.Fatalf("reported %q, want the statuses field named once, with the head whose statuses stay", decoded.Unread)
			}
		})
	}
}
