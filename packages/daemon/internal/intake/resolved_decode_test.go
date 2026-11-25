package intake

//ci:reads packages/daemon/internal/dispatch/testdata/resolved/* TestEveryDispatchReadDecodesEachPriorityField reads the listing and detail bodies through ../dispatch/testdata/resolved

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// testdata/dispatch-resolved/acme-1549-after-0.json is real Dispatch events: the answer of one GET
// of an issue's event log, read on 2026-11-25 with the deployment's Dispatch token from a Dispatch
// release that sends `priority` and no `resolved_priority`, <KEY> being the issue's key in that
// deployment:
//
//	curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$DISPATCH_URL/api/v1/issues/<KEY>/events?after=0&limit=4"
//
// This repository is public, so the answer went through the filter of status_change_test.go's
// header before it was kept, with `resolved_priority` and `priority_from` added to the keys it
// keeps of each payload (it keeps `changed` already), which this release does not send, and with
// each actor cut to its kind and id: these events' actors also name the email of the session's
// owner and the machine and directory the session ran in.
//
//	jq 'map(.actor |= {kind, id} | .payload |= with_entries(select(.key | IN("key", "project",
//	  "number", "parent", "ancestors", "status", "changed", "closed_at", "last_seq", "labels",
//	  "blocked_by", "rank", "priority", "claim", "created_at", "updated_at", "id", "issue_key",
//	  "artifact_id", "name", "resolved_priority", "priority_from"))))'
//
// Then, in every string, the project's key became the neutral ACME, as AGENTS.md asks, so each
// issue keeps its number, and the two sessions that wrote the events became
// 00000000-0000-7000-8000-000000000001 and 00000000-0000-7000-8000-000000000002; every other value
// is as the filter left it, and the file was then formatted with `bunx biome format --write`. The
// other files there are written by hand in the shape dispatch://INFRA-5793/spec#b-j-inherit and
// #b-j-events give, which testdata/dispatch-resolved/README quotes, and so are the HTTP bodies of
// internal/dispatch/testdata/resolved named resolved-<v> (its README), whose two captures, of the
// same issue's listing row and detail read, have their commands in internal/dispatch's
// resolved_test.go.
const dispatchResolvedDir = "testdata/dispatch-resolved"

// resolvedHTTPDir holds the HTTP bodies of the listing and the detail read (internal/dispatch's
// testdata/resolved).
const resolvedHTTPDir = "../dispatch/testdata/resolved"

// fromAbove is the issue an inherited priority of the hand-written files comes from: an issue above
// the child in another project.
const fromAbove = "OTHER-5"

// resolvedLogs is each hand-written issue log of testdata/dispatch-resolved but the resolved-only
// updates (resolved-only.json), with the priority every event of it carries: the issue's own and
// the one Dispatch resolves it to.
var resolvedLogs = []struct {
	file     string
	own      record.Priority
	resolved record.Resolved
}{
	{file: "resolved-0.json", resolved: record.Resolved{Priority: record.P0, From: fromAbove, Known: true}},
	{file: "resolved-1.json", resolved: record.Resolved{Priority: record.P1, From: fromAbove, Known: true}},
	{file: "resolved-2.json", resolved: record.Resolved{Priority: record.P2, From: fromAbove, Known: true}},
	{file: "resolved-3.json", resolved: record.Resolved{Priority: record.P3, From: fromAbove, Known: true}},
	{file: "resolved-null.json", resolved: record.Resolved{Known: true}},
	{file: "resolved-own.json", own: record.P2, resolved: record.Resolved{Priority: record.P2, Known: true}},
}

// resolvedEventLog is the event log testdata/dispatch-resolved/<name> holds, one raw event for each
// element, as dispatch.Client.EventsAfter hands the events to its caller.
func resolvedEventLog(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dispatchResolvedDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return events
}

// eventType is the type of raw, an event of a log.
func eventType(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("read the type of %s: %v", raw, err)
	}
	return event.Type
}

// decodeIssueEvent is raw, an issue event of project ACME, through the decoder the boot catch-up
// and the live consumer share (DecodeDispatchEvent), which must read it whole.
func decodeIssueEvent(t *testing.T, raw []byte) DispatchIssue {
	t.Helper()
	var event struct {
		IssueKey string `json:"issue_key"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("read the issue of %s: %v", raw, err)
	}
	fact, unread, err := DecodeDispatchEvent("ACME", event.IssueKey, raw)
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode %s: %v, unread %q", raw, err, unread)
	}
	issue, ok := fact.(DispatchIssue)
	if !ok {
		t.Fatalf("%s decoded to %#v, want a Dispatch issue", raw, fact)
	}
	return issue
}

// priorityWords is priority as a failure names it: P0 to P3, or none.
func priorityWords(priority record.Priority) string {
	if name := priority.String(); name != "" {
		return name
	}
	return "none"
}

// resolvedWords is resolved as a failure names it, an unknown one and a known none apart.
func resolvedWords(resolved record.Resolved) string {
	if !resolved.Known {
		return "unknown"
	}
	return fmt.Sprintf("%s from %q", priorityWords(resolved.Priority), resolved.From)
}

// withFields is the JSON object raw with each key of set written as its value, a nil value
// leaving the key out.
func withFields(t *testing.T, raw []byte, set map[string]json.RawMessage) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("read %s as an object: %v", raw, err)
	}
	for key, value := range set {
		if value == nil {
			delete(object, key)
			continue
		}
		object[key] = value
	}
	changed, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

// withPayloadFields is raw, an event, with set written on its payload, the issue the event carries.
func withPayloadFields(t *testing.T, raw []byte, set map[string]json.RawMessage) []byte {
	t.Helper()
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("read %s as an event: %v", raw, err)
	}
	return withFields(t, raw, map[string]json.RawMessage{"payload": withFields(t, event["payload"], set)})
}

// withRowFields is body, a listing of one row, with set written on that row.
func withRowFields(t *testing.T, body []byte, set map[string]json.RawMessage) []byte {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("the listing body holds %d rows (%v), want one", len(rows), err)
	}
	listing, err := json.Marshal([]json.RawMessage{withFields(t, rows[0], set)})
	if err != nil {
		t.Fatal(err)
	}
	return listing
}

// readHTTPBody is internal/dispatch's testdata/resolved/<name>.
func readHTTPBody(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(resolvedHTTPDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

// servedBy is a client of a stand-in Dispatch that answers every request with body.
func servedBy(t *testing.T, body []byte) *dispatch.HTTPClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return dispatch.New(server.URL, dispatch.BearerTransport("dispatch-test-token", nil))
}

// listedFact is the fact the boot's reconcile makes of body, a listing Dispatch serves: the real
// client's read (dispatch.HTTPClient.ListIssues) of its one row, made a fact (ListedIssue).
func listedFact(t *testing.T, body []byte) DispatchIssue {
	t.Helper()
	issues, err := servedBy(t, body).ListIssues(context.Background(), "ACME", nil)
	if err != nil || len(issues) != 1 {
		t.Fatalf("list the issues: %d issues, %v; want the one row", len(issues), err)
	}
	return ListedIssue(issues[0])
}

// detailFact is the fact a detail read makes of body, an issue Dispatch serves: the real client's
// read (dispatch.HTTPClient.GetIssue), made a fact (ObservedIssue).
func detailFact(t *testing.T, body []byte) DispatchIssue {
	t.Helper()
	var served struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("read the key of %s: %v", body, err)
	}
	issue, err := servedBy(t, body).GetIssue(context.Background(), served.Key)
	if err != nil {
		t.Fatalf("get %s: %v", served.Key, err)
	}
	return ObservedIssue(issue)
}

// Each read Dispatch serves decodes each priority field: an issue event (DecodeDispatchEvent), a
// boot-listing row (the real client's ListIssues through ListedIssue) and a detail read (GetIssue
// through ObservedIssue), each with every own priority of 0 to 3 and null and every resolved
// priority of 0 to 3, null and missing. A missing `resolved_priority` is unknown and a present null
// a known none, never the other way round, and a present `priority_from` is kept. A resolved level
// Dispatch never writes, 9, on the listing and the detail read is a known none. Each row is a
// hand-written file of the resolved shape with its priority fields written as the row says. The
// older capture, whose own priority is unset, makes from each read a fact whose record would not
// start outside the cap.
func TestEveryDispatchReadDecodesEachPriorityField(t *testing.T) {
	t.Parallel()
	owns := []struct {
		raw  json.RawMessage
		want record.Priority
	}{
		{json.RawMessage(`0`), record.P0}, {json.RawMessage(`1`), record.P1}, {json.RawMessage(`2`), record.P2},
		{json.RawMessage(`3`), record.P3}, {json.RawMessage(`null`), record.NoPriority},
	}
	resolveds := []struct {
		name string
		// level and from are `resolved_priority` and `priority_from` as the read carries them,
		// nil for a field it leaves out.
		level, from json.RawMessage
		want        record.Resolved
	}{
		{"P0", json.RawMessage(`0`), json.RawMessage(`"OTHER-5"`), record.Resolved{Priority: record.P0, From: fromAbove, Known: true}},
		{"P1", json.RawMessage(`1`), json.RawMessage(`"OTHER-5"`), record.Resolved{Priority: record.P1, From: fromAbove, Known: true}},
		{"P2", json.RawMessage(`2`), json.RawMessage(`"OTHER-5"`), record.Resolved{Priority: record.P2, From: fromAbove, Known: true}},
		{"P3", json.RawMessage(`3`), json.RawMessage(`"OTHER-5"`), record.Resolved{Priority: record.P3, From: fromAbove, Known: true}},
		{"null", json.RawMessage(`null`), json.RawMessage(`null`), record.Resolved{Known: true}},
		{"missing", nil, nil, record.Resolved{}},
	}
	event := resolvedEventLog(t, "resolved-0.json")[1]
	listing := readHTTPBody(t, "listing-resolved-0.json")
	detail := readHTTPBody(t, "detail-resolved-0.json")
	check := func(t *testing.T, read string, got DispatchIssue, own record.Priority, resolved record.Resolved) {
		t.Helper()
		if got.Priority != own || got.Resolved != resolved {
			t.Errorf("the %s decoded to own %s and resolved %s, want own %s and resolved %s", read, priorityWords(got.Priority), resolvedWords(got.Resolved),
				priorityWords(own), resolvedWords(resolved))
		}
	}
	for _, own := range owns {
		for _, resolved := range resolveds {
			t.Run(fmt.Sprintf("own %s, resolved %s", own.raw, resolved.name), func(t *testing.T) {
				set := map[string]json.RawMessage{"priority": own.raw, "resolved_priority": resolved.level, "priority_from": resolved.from}
				check(t, "issue event", decodeIssueEvent(t, withPayloadFields(t, event, set)), own.want, resolved.want)
				check(t, "listing row", listedFact(t, withRowFields(t, listing, set)), own.want, resolved.want)
				check(t, "detail read", detailFact(t, withFields(t, detail, set)), own.want, resolved.want)
			})
		}
	}
	t.Run("a resolved level Dispatch never writes", func(t *testing.T) {
		set := map[string]json.RawMessage{"resolved_priority": json.RawMessage(`9`)}
		none := record.Resolved{From: fromAbove, Known: true}
		check(t, "listing row", listedFact(t, withRowFields(t, listing, set)), record.NoPriority, none)
		check(t, "detail read", detailFact(t, withFields(t, detail, set)), record.NoPriority, none)
	})
	t.Run("the older capture", func(t *testing.T) {
		facts := map[string]DispatchIssue{
			"listing row": listedFact(t, readHTTPBody(t, "listing-acme-1549.json")),
			"detail read": detailFact(t, readHTTPBody(t, "detail-acme-1549.json")),
		}
		for _, raw := range resolvedEventLog(t, "acme-1549-after-0.json") {
			if typ := eventType(t, raw); isIssueEvent(typ) {
				facts[typ] = decodeIssueEvent(t, raw)
			}
		}
		for read, fact := range facts {
			check(t, read, fact, record.NoPriority, record.Resolved{})
			if (record.Issue{Priority: fact.Priority, Resolved: fact.Resolved}).StartsNow() {
				t.Errorf("the %s of an issue with no priority of its own, from a Dispatch that resolves none, makes a record that starts outside the cap", read)
			}
		}
	})
}

// Every type of issue event the decoder takes (issueEventTypes) carries the resolved shape in
// each hand-written log: each log holds an event of each type, and each such event decodes to the
// log's own and resolved priority. A type added to the list fails here until each log holds one.
// The updates Dispatch appends on an issue whose resolved priority an issue above it changed (J3),
// which it sends with notify false, decode the same way and as an update of the resolved priority
// alone.
func TestEveryIssueEventKindTheDaemonAdmitsFromCarriesTheResolvedShape(t *testing.T) {
	t.Parallel()
	for _, log := range resolvedLogs {
		events := resolvedEventLog(t, log.file)
		for _, typ := range issueEventTypes {
			rows := 0
			for _, raw := range events {
				if eventType(t, raw) != typ {
					continue
				}
				rows++
				fact := decodeIssueEvent(t, raw)
				if fact.Type != typ || fact.Priority != log.own || fact.Resolved != log.resolved {
					t.Errorf("%s's %s decoded to type %s, own %s, resolved %s; want own %s, resolved %s", log.file, typ, fact.Type,
						priorityWords(fact.Priority), resolvedWords(fact.Resolved), priorityWords(log.own), resolvedWords(log.resolved))
				}
			}
			if rows == 0 {
				t.Errorf("%s holds no %s event: the decoder takes %s as an issue event (issueEventTypes), so each log of the resolved shape holds one", log.file, typ, typ)
			}
		}
	}
	updates := resolvedEventLog(t, "resolved-only.json")
	want := []record.Resolved{{Priority: record.P0, From: fromAbove, Known: true}, {Known: true}}
	if len(updates) != len(want) {
		t.Fatalf("resolved-only.json holds %d updates, want %d", len(updates), len(want))
	}
	for i, raw := range updates {
		var sent struct {
			Notify *bool `json:"notify"`
		}
		if err := json.Unmarshal(raw, &sent); err != nil || sent.Notify == nil || *sent.Notify {
			t.Fatalf("update %d of resolved-only.json is sent with notify %v (%v), want false as J3's events are", i, sent.Notify, err)
		}
		fact := decodeIssueEvent(t, raw)
		if fact.Type != "issue.updated" || fact.Priority != record.NoPriority || fact.Resolved != want[i] || !fact.ResolvedOnly {
			t.Errorf("update %d decoded to type %s, own %s, resolved %s, resolved only %t; want issue.updated, own none, resolved %s, resolved only",
				i, fact.Type, priorityWords(fact.Priority), resolvedWords(fact.Resolved), fact.ResolvedOnly, resolvedWords(want[i]))
		}
	}
}

// An update changed only the priority Dispatch resolves the issue to, and no work of the issue's
// own, exactly when its `changed` names something and every name is `resolved_priority` or
// `priority_from`: a list absent or empty says nothing changed, and is no such update. Each row is
// the first resolved-only update with its `changed` written as the row says.
func TestTheChangedListDecidesAResolvedOnlyUpdate(t *testing.T) {
	t.Parallel()
	update := resolvedEventLog(t, "resolved-only.json")[0]
	for _, tc := range []struct {
		name    string
		changed json.RawMessage
		want    bool
	}{
		{name: "no changed list", changed: nil, want: false},
		{name: "an empty list", changed: json.RawMessage(`[]`), want: false},
		{name: "the resolved priority", changed: json.RawMessage(`["resolved_priority"]`), want: true},
		{name: "the issue it comes from", changed: json.RawMessage(`["priority_from"]`), want: true},
		{name: "both", changed: json.RawMessage(`["resolved_priority","priority_from"]`), want: true},
		{name: "both and the title", changed: json.RawMessage(`["resolved_priority","priority_from","title"]`), want: false},
		{name: "the title", changed: json.RawMessage(`["title"]`), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact := decodeIssueEvent(t, withPayloadFields(t, update, map[string]json.RawMessage{"changed": tc.changed}))
			if fact.ResolvedOnly != tc.want {
				t.Errorf("changed %s decoded as resolved only %t, want %t", tc.changed, fact.ResolvedOnly, tc.want)
			}
			if want := (record.Resolved{Priority: record.P0, From: fromAbove, Known: true}); fact.Resolved != want {
				t.Errorf("changed %s decoded the resolved priority as %s, want %s whatever the list says", tc.changed, resolvedWords(fact.Resolved), resolvedWords(want))
			}
		})
	}
}

// An issue event of a Dispatch release that resolves no priority carries no `resolved_priority`,
// and each type the decoder takes (issueEventTypes) decodes to an unknown resolved priority, never
// a present none, from the captured log; the issue, with no priority of its own, would not start
// outside the cap.
func TestAnOlderDispatchsEventHasUnknownResolvedPriority(t *testing.T) {
	t.Parallel()
	events := resolvedEventLog(t, "acme-1549-after-0.json")
	for _, typ := range issueEventTypes {
		rows := 0
		for _, raw := range events {
			if eventType(t, raw) != typ {
				continue
			}
			rows++
			fact := decodeIssueEvent(t, raw)
			if fact.Resolved != (record.Resolved{}) || fact.Priority != record.NoPriority {
				t.Errorf("the captured %s decoded to own %s and resolved %s, want own none and an unknown resolved priority", typ,
					priorityWords(fact.Priority), resolvedWords(fact.Resolved))
			}
			if (record.Issue{Priority: fact.Priority, Resolved: fact.Resolved}).StartsNow() {
				t.Errorf("the captured %s makes a record that starts outside the cap", typ)
			}
		}
		if rows == 0 {
			t.Errorf("the captured log holds no %s event: the decoder takes %s as an issue event (issueEventTypes), so the older shape needs one", typ, typ)
		}
	}
}
