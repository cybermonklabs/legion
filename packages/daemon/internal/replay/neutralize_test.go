package replay

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The private-looking names of the window the neutralize tests write: a project and another
// project, a person's email and login, a repository, a machine, a home directory, a session's
// title, a label, a component, a priority, free text, a wiki link, a uuid of a document and of a
// session, a document's digest, the claim token of a worker, and a word no rule knows to be a name
// (privateWord, which the tests deny). The tailnet host is built at run time, so no file of the
// repository holds a host name of that form.
const (
	privateEmail    = "ops.lead@hushcorp.example"
	privateLogin    = "opslead-hush"
	privateRepo     = "hushcorp/vault-service"
	privateArtifact = "1b4e28ba-2fa1-41d2-883f-0016d3cca427"
	privateSession  = "6fa459ea-ee8a-4ca4-894e-db77e160355e"
	privateAsk      = "c0ffee00-1234-4abc-8def-0123456789ab"
	privateToken    = "legion-nimbus-nimbus-412-implementer"
	privateWord     = "megahush"
)

var (
	privateHost   = "vault-runner." + "tail" + "5eed" + ".ts.net"
	privateDigest = strings.Repeat("ab", 32)
	// privateNames is every private name the window holds, each of which no file of the neutral
	// window may hold in any case.
	privateNames = []string{"nimbus", "orion", "hush", "vault", "tail5eed", "ts.net", privateArtifact, privateSession, privateAsk,
		privateWord, "outage", "wiki", privateDigest}
)

// privateEvent is one Dispatch event as the issue event log serves it.
func privateEvent(t *testing.T, id, seq int64, key, project, kind string, at time.Time, actor, payload map[string]any) json.RawMessage {
	t.Helper()
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": key, "artifact_id": nil, "project": project, "seq": seq, "type": kind,
		"actor": actor, "notify": false, "created_at": at, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// privateWindow writes privateRecord into a directory of the test's own.
func privateWindow(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := privateRecord(t).Write(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// privateRecord is a window of the project NIMBUS whose every input holds private-looking names:
// NIMBUS-412, a child of NIMBUS-400, waits on ORION-77 of another project; its implementer's claim
// failed with the daemon's words naming a tailnet host and privateWord; a person requested
// changes on the root's document, which the daemon's gate line names by the same uuid; the root's
// architect asked a question from its workspace; and the window's merges are of the private
// repository, one merging pull request 88, and of this one.
func privateRecord(t *testing.T) Window {
	t.Helper()
	at := func(minute int) time.Time { return time.Date(2026, time.March, 2, 10, minute, 0, 0, time.UTC) }
	token := privateToken
	log := []json.RawMessage{
		jsonLine(t, at(1), slog.LevelInfo, "legion daemon started", "project", "NIMBUS", "admissionCap", 4, "boot", 7),
		jsonLine(t, at(2), slog.LevelInfo, gateOpenedLine, "issue", "NIMBUS-400", "artifact", privateArtifact,
			"event", "approved", "version", 2),
		jsonLine(t, at(3), slog.LevelInfo, phaseChangedLine, "issue", "NIMBUS-412", "tree", "NIMBUS-400", "from", "implementing",
			"to", "testing", "trigger", "implementation_ready", "status", "testing"),
		jsonLine(t, at(4), slog.LevelInfo, "admission waits for what this issue waits on", "issue", "NIMBUS-412", "blockers", []string{"ORION-77"}),
		jsonLine(t, at(5), slog.LevelInfo, "supervise: a turn started", "claim", token, "generation", 3, "delivery", "outbox:9"),
		jsonLine(t, at(6), slog.LevelError, "supervise: claim failed", "claim", token, "cause", "launch_refused",
			"why", "launch "+token+": dial tcp: lookup "+privateHost+": no such host ("+privateWord+" network)"),
		jsonLine(t, at(7), slog.LevelInfo, "supervise: suspended", "claim", "legion-nimbus-controller", "by", "released",
			"reason", "the issue's pod has been idle for the release period"),
	}
	person := map[string]any{"kind": "user", "id": privateEmail}
	daemon := func(project string) map[string]any {
		return map[string]any{"kind": "session", "id": "legion-daemon:" + project}
	}
	laptop := map[string]any{"kind": "session", "id": privateSession, "origin": map[string]any{"host": "omp", "machine": "hushcorp-laptop-7",
		"cwd": "/home/hushuser/src/vault-service", "session_title": "fixing the vault outage"}}
	architect := map[string]any{"kind": "session", "id": privateSession, "origin": map[string]any{"host": "omp", "machine": "legion-nimbus-daemon",
		"cwd": "/legion/workspaces/hushcorp/vault-service/nimbus-412", "session_title": "Legion architect · NIMBUS-412"}}
	issue := func(key string, number int, project, status, parent string, blockedBy, labels []string, extra map[string]any) map[string]any {
		payload := map[string]any{"key": key, "number": number, "project": project, "status": status, "parent": nil, "blocked_by": blockedBy,
			"labels": labels, "rank": "m" + strconv.Itoa(number), "title": "Rotate the vault keys for hushcorp", "changed": []string{"status"}}
		if parent != "" {
			payload["parent"] = parent
		}
		maps.Copy(payload, extra)
		return payload
	}
	w := Window{
		Project: "NIMBUS", From: at(0), To: at(30), Before: Duration(time.Hour), Caps: []CapAt{{At: at(1), Cap: 4}},
		Parts:        []Part{{From: at(0), To: at(30), Release: "legion-v19.41.3", Commit: record.MainRule.Commit, Rule: record.MainRule.Name}},
		TickInterval: Duration(time.Hour), GateSteps: []record.DesignStep{record.DesignStepSpec}, Linger: Duration(72 * time.Hour), ReviewRoundCap: 3,
		StalledTreeNotice: Duration(6 * time.Hour), WorkerSilence: Duration(30 * time.Minute), WorkerBackground: Duration(4 * time.Hour),
		Collector: record.MainRule.Commit, Log: log,
		Events: map[string][]json.RawMessage{
			"NIMBUS-400": {
				privateEvent(t, 11, 1, "NIMBUS-400", "NIMBUS", "issue.created", at(0), daemon("NIMBUS"),
					issue("NIMBUS-400", 400, "NIMBUS", "todo", "", []string{}, []string{"legion"},
						map[string]any{"repo": privateRepo, "resolved_repo": privateRepo})),
				privateEvent(t, 12, 2, "NIMBUS-400", "NIMBUS", "child.added", at(1), person, map[string]any{"child_key": "NIMBUS-412"}),
			},
			"NIMBUS-412": {
				privateEvent(t, 13, 4, "NIMBUS-412", "NIMBUS", "issue.updated", at(2), laptop,
					issue("NIMBUS-412", 412, "NIMBUS", "in_progress", "NIMBUS-400", []string{"ORION-77"}, []string{"legion", "vault-secret"},
						map[string]any{
							"assignee": privateEmail, "repo": privateRepo, "resolved_repo": privateRepo, "ancestors": []string{"NIMBUS-400"},
							"thread_logins": []string{privateLogin},
							"external_links": []map[string]any{{"url": "https://github.com/" + privateRepo + "/pull/88", "kind": "url"},
								{"url": "https://wiki.hushcorp.example/vault", "kind": "url"}},
							"components": map[string]any{"ids": []string{"vault-core"}, "mode": "inherit", "reason": "kept for hushcorp",
								"unknown": []string{}, "inherited_from": nil},
							"advances": []map[string]any{{"id": "hush-launch", "inherited": true}},
						})),
				privateEvent(t, 14, 5, "NIMBUS-412", "NIMBUS", "artifact.changes_requested", at(3), person, map[string]any{
					"artifact_id": privateArtifact, "name": "spec.md", "version": 2, "reason": "The hushcorp numbers are wrong",
					"content_sha256": privateDigest, "actor": map[string]any{"id": privateEmail, "kind": "user"}}),
				privateEvent(t, 15, 6, "NIMBUS-412", "NIMBUS", "ask.opened", at(4), architect, map[string]any{
					"id": privateAsk, "kind": "question", "state": "open", "issue_key": "NIMBUS-412", "author": architect,
					"question": "Should hushcorp keys rotate weekly?", "references_changed": []map[string]any{{"id": "ORION-77", "kind": "issue"}},
					"options": []map[string]any{{"id": "b2", "label": "Yes, for hushcorp"}}}),
			},
			"ORION-77": {
				privateEvent(t, 16, 9, "ORION-77", "ORION", "issue.closed", at(5), daemon("ORION"),
					issue("ORION-77", 77, "ORION", "done", "", []string{}, []string{}, nil)),
			},
		},
		Merges: map[string][]Merge{
			privateRepo:    {{SHA: strings.Repeat("1", 40), At: at(8), Branch: "main", PR: 88}},
			keptRepository: {{SHA: strings.Repeat("2", 40), At: at(9), Branch: "main"}},
		},
	}
	kinds := make(map[string]bool)
	for _, line := range log {
		parsed, err := ParseLogLine(line)
		if err != nil || parsed == nil {
			t.Fatalf("the private window's line %s does not read: %v", line, err)
		}
		kinds[parsed.Head().Msg] = true
	}
	w.Lines = slices.Sorted(maps.Keys(kinds))
	return w
}

// shapeOf is the shape of the JSON value raw: every member's name in order, every list's length and
// every value's JSON type, each value itself left out.
func shapeOf(t *testing.T, raw []byte) any {
	t.Helper()
	value, err := decodeOrdered(raw)
	if err != nil {
		t.Fatal(err)
	}
	var shape func(v any) any
	shape = func(v any) any {
		switch value := v.(type) {
		case jsonObject:
			out := make([]any, 0, 2*len(value))
			for _, member := range value {
				out = append(out, member.key, shape(member.value))
			}
			return map[string]any{"object": out}
		case []any:
			out := make([]any, len(value))
			for i, element := range value {
				out[i] = shape(element)
			}
			return out
		case string:
			return "string"
		case json.Number:
			return "number"
		case bool:
			return "bool"
		}
		return "null"
	}
	return shape(value)
}

// pick is the value at path in raw, each step a member's name or a list's index.
func pick(t *testing.T, raw json.RawMessage, path ...any) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	for _, step := range path {
		switch step := step.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%s has no member %q on the path %v", raw, step, path)
			}
			value = object[step]
		case int:
			list, ok := value.([]any)
			if !ok || step >= len(list) {
				t.Fatalf("%s has no element %d on the path %v", raw, step, path)
			}
			value = list[step]
		}
	}
	return value
}

// neutralizePrivate neutralizes the private window into a directory of the test's own, denying
// privateWord, with the map written beside the output rather than in it.
func neutralizePrivate(t *testing.T) (in, out string, names NeutralMap) {
	t.Helper()
	in = privateWindow(t)
	scratch := t.TempDir()
	out = filepath.Join(scratch, "neutral")
	names, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: filepath.Join(scratch, "map.json"), Deny: []string{privateWord}})
	if err != nil {
		t.Fatalf("Neutralize: %v", err)
	}
	return in, out, names
}

// A neutralized window holds none of the private names of the window it was made from, in any
// case and in any file, window.json included; and it keeps the record's shapes: every line and
// every event has the members, the JSON types and the list lengths it had, the trace's readers
// read every line as the kind it was and every event without a refusal, and the merges keep their
// commits and times under their renamed repositories. The map is written outside the window,
// readable by its owner alone, and holds the names it returned.
func TestNeutralizeLeavesNoPrivateNameAndKeepsTheShapes(t *testing.T) {
	in, out, names := neutralizePrivate(t)
	assertNoneHeld(t, out, privateNames, true)

	recorded, err := ReadWindow(in)
	if err != nil {
		t.Fatal(err)
	}
	neutral, err := ReadWindow(out)
	if err != nil {
		t.Fatalf("ReadWindow of the neutral window: %v", err)
	}
	if len(neutral.Log) != len(recorded.Log) {
		t.Fatalf("the neutral log holds %d lines, the record's %d", len(neutral.Log), len(recorded.Log))
	}
	for i, line := range recorded.Log {
		if !reflect.DeepEqual(shapeOf(t, neutral.Log[i]), shapeOf(t, line)) {
			t.Errorf("log line %d is\n  %s\nwhose shape differs from the record's\n  %s", i+1, neutral.Log[i], line)
		}
		was, _ := ParseLogLine(line)
		is, err := ParseLogLine(neutral.Log[i])
		if err != nil || reflect.TypeOf(is) != reflect.TypeOf(was) {
			t.Errorf("log line %d reads as %T (%v), the record's as %T", i+1, is, err, was)
		}
	}
	for key, events := range recorded.Events {
		renamed, ok := names.Keys[key]
		if !ok || len(neutral.Events[renamed]) != len(events) {
			t.Errorf("the record's %d events of %s are %d events of %q in the neutral window", len(events), key, len(neutral.Events[renamed]), renamed)
			continue
		}
		for i, event := range events {
			if !reflect.DeepEqual(shapeOf(t, neutral.Events[renamed][i]), shapeOf(t, event)) {
				t.Errorf("event %d of %s is\n  %s\nwhose shape differs from the record's\n  %s", i+1, renamed, neutral.Events[renamed][i], event)
			}
			if _, err := ParseDispatchEvent(neutral.Events[renamed][i]); err != nil {
				t.Errorf("event %d of %s does not read: %v", i+1, renamed, err)
			}
		}
	}
	if len(neutral.Events) != len(recorded.Events) {
		t.Errorf("the neutral window holds the events of %v, the record of %v", slices.Sorted(maps.Keys(neutral.Events)), slices.Sorted(maps.Keys(recorded.Events)))
	}
	wantMerges := map[string][]Merge{"acme/repo-1": recorded.Merges[privateRepo], keptRepository: recorded.Merges[keptRepository]}
	if len(neutral.Merges) != 2 || !sameReading(neutral.Merges["acme/repo-1"], wantMerges["acme/repo-1"]) ||
		!sameReading(neutral.Merges[keptRepository], wantMerges[keptRepository]) {
		t.Errorf("the neutral merges are %+v, want %+v", neutral.Merges, wantMerges)
	}
	if neutral.Project != "ACME" || !slices.Equal(neutral.Lines, recorded.Lines) || !sameReading(neutral.Parts, recorded.Parts) {
		t.Errorf("the neutral window.json names project %q, lines %q and parts %+v; want ACME and the record's lines and parts",
			neutral.Project, neutral.Lines, neutral.Parts)
	}

	scratch := filepath.Dir(out)
	info, err := os.Stat(filepath.Join(scratch, "map.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the map is %v, %v; want a file its owner alone reads", info, err)
	}
	data, err := os.ReadFile(filepath.Join(scratch, "map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written NeutralMap
	if err := json.Unmarshal(data, &written); err != nil || !reflect.DeepEqual(written, names) {
		t.Errorf("the map file reads %+v (%v), want the map Neutralize returned, %+v", written, err, names)
	}
}

// One name of the record is one neutral name in every file it stands in: the root and its child
// are ACME-1 and ACME-2 in the order of their numbers, and the other project's issue OTHERA-1, in
// the log's lines, the claim tokens and the daemon's words that spell them, the events' files,
// keys, numbers, parents, blockers, children and references, a session's workspace and title;
// the document's uuid is one in the gate's line and the change request; the person is one in the
// assignee and the request's actor; the repository is one in the issue, the link to its pull
// request and the merges; and each daemon's Dispatch session names its project's neutral key. So
// the trace of the neutral window reads the architect's session, by its neutral id, as running the
// claim of the issue its neutral title names, the child's architect claim, and the laptop's
// session, the same id under a title outside Legion, as running none.
func TestNeutralizeRenamesANameTheSameInEveryFile(t *testing.T) {
	_, out, names := neutralizePrivate(t)
	root, child, other := names.Keys["NIMBUS-400"], names.Keys["NIMBUS-412"], names.Keys["ORION-77"]
	if root != "ACME-1" || child != "ACME-2" || other != "OTHERA-1" {
		t.Fatalf("NIMBUS-400, NIMBUS-412 and ORION-77 are %q, %q and %q; want ACME-1, ACME-2 and OTHERA-1", root, child, other)
	}
	w, err := ReadWindow(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]LogLine, len(w.Log))
	for i, line := range w.Log {
		if lines[i], err = ParseLogLine(line); err != nil {
			t.Fatal(err)
		}
	}
	const token = "legion-acme-acme-2-implementer"
	want := []LogLine{
		DaemonStarted{LineHead: lines[0].Head(), Project: "ACME", AdmissionCap: 4},
		GateOpened{LineHead: lines[1].Head(), Issue: root, Artifact: names.IDs[privateArtifact], Event: "approved", Version: 2},
		PhaseChanged{LineHead: lines[2].Head(), Issue: child, Tree: root, From: "implementing", To: "testing", Trigger: "implementation_ready", Status: "testing"},
		WaitsOnIssues{LineHead: lines[3].Head(), Issue: child, Blockers: []string{other}},
		TurnStarted{LineHead: lines[4].Head(), Claim: ClaimRef{Token: token, Issue: child, Role: "implementer"}, Generation: 3, Delivery: "outbox:9"},
		ClaimFailed{LineHead: lines[5].Head(), Claim: ClaimRef{Token: token, Issue: child, Role: "implementer"}, Cause: "launch_refused",
			Why: "launch " + token + ": dial tcp: lookup host-1.example.invalid: no such host (withheld network)"},
		Suspended{LineHead: lines[6].Head(), Claim: ClaimRef{Token: "legion-acme-controller", Role: "controller"}, By: "released",
			Reason: "the issue's pod has been idle for the release period"},
	}
	for i := range want {
		if !sameReading(lines[i], want[i]) {
			t.Errorf("neutral log line %d reads as\n  %s\nwant\n  %s", i+1, describeReading(lines[i]), describeReading(want[i]))
		}
	}
	if names.IDs[privateArtifact] == "" || names.IDs[privateArtifact] == privateArtifact {
		t.Errorf("the document's uuid became %q", names.IDs[privateArtifact])
	}

	if keys := slices.Sorted(maps.Keys(w.Events)); !slices.Equal(keys, []string{root, child, other}) {
		t.Fatalf("the neutral window holds the events of %v, want of %v", keys, []string{root, child, other})
	}
	update, request, ask := w.Events[child][0], w.Events[child][1], w.Events[child][2]
	email := pick(t, update, "payload", "assignee")
	for _, check := range []struct {
		what      string
		got, want any
	}{
		{"the child's key", pick(t, update, "payload", "key"), child},
		{"the child's number", pick(t, update, "payload", "number"), 2.0},
		{"the child's parent", pick(t, update, "payload", "parent"), root},
		{"the child's ancestor", pick(t, update, "payload", "ancestors", 0), root},
		{"the child's blocker", pick(t, update, "payload", "blocked_by", 0), other},
		{"the child's labels", pick(t, update, "payload", "labels"), []any{"legion", "label-1"}},
		{"the child's repository", pick(t, update, "payload", "repo"), "acme/repo-1"},
		{"the link to its pull request", pick(t, update, "payload", "external_links", 0, "url"), "https://github.com/acme/repo-1/pull/88"},
		{"the wiki link", pick(t, update, "payload", "external_links", 1, "url"), "https://example.invalid/link-1"},
		{"the laptop's working directory", pick(t, update, "actor", "origin", "cwd"), "/work/1"},
		{"the laptop session's title", pick(t, update, "actor", "origin", "session_title"), "session 1"},
		{"the change request's actor", pick(t, request, "actor", "id"), email},
		{"the change request's document", pick(t, request, "payload", "artifact_id"), names.IDs[privateArtifact]},
		{"the ask's issue", pick(t, ask, "payload", "issue_key"), child},
		{"the ask's reference", pick(t, ask, "payload", "references_changed", 0, "id"), other},
		{"the architect's workspace", pick(t, ask, "actor", "origin", "cwd"), "/legion/workspaces/acme/repo-1/acme-2"},
		{"the architect's title", pick(t, ask, "actor", "origin", "session_title"), "Legion architect · ACME-2"},
		{"the architect's machine", pick(t, ask, "actor", "origin", "machine"), "legion-acme-daemon"},
		{"the root's child", pick(t, w.Events[root][1], "payload", "child_key"), child},
		{"the root's daemon session", pick(t, w.Events[root][0], "actor", "id"), "legion-daemon:ACME"},
		{"the other project's daemon session", pick(t, w.Events[other][0], "actor", "id"), "legion-daemon:OTHERA"},
		{"the other project's key", pick(t, w.Events[other][0], "project"), "OTHERA"},
	} {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("%s is %v, want %v", check.what, check.got, check.want)
		}
	}
	if address, _ := email.(string); !strings.HasSuffix(address, "@example.invalid") {
		t.Errorf("the person's email became %v", email)
	}
	trace, err := BuildTrace(w)
	if err != nil {
		t.Fatal(err)
	}
	if sessions := trace.Issues[child].Sessions; len(sessions) != 1 || sessions[0].ID != names.IDs[privateSession] ||
		sessions[0].Role != "architect" {
		t.Errorf("the trace of the neutral window reads the sessions %+v running %s's claims, want its architect's, %s", sessions, child,
			names.IDs[privateSession])
	}
}

// The map of a neutralization holds every private name, so it is never written where the neutral
// window is: a map inside the output is refused, and so is a map already there, which a second
// run would otherwise write over; neither refusal writes the output or the map.
func TestNeutralizeRefusesAMapInsideTheOutputOrAlreadyThere(t *testing.T) {
	in := privateWindow(t)
	scratch := t.TempDir()
	out := filepath.Join(scratch, "neutral")
	_, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: filepath.Join(out, "map.json")})
	if err == nil || !strings.Contains(err.Error(), "inside the output") {
		t.Errorf("Neutralize with the map inside the output = %v, want a refusal saying so", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused neutralization made its output: %v", err)
	}

	mapFile := filepath.Join(scratch, "map.json")
	if err := os.WriteFile(mapFile, []byte("an earlier map"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: mapFile})
	if err == nil || !strings.Contains(err.Error(), "there already") {
		t.Errorf("Neutralize with a map already there = %v, want a refusal saying so", err)
	}
	if data, _ := os.ReadFile(mapFile); string(data) != "an earlier map" {
		t.Errorf("the earlier map reads %q after the refused neutralization", data)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused neutralization made its output: %v", err)
	}
}

// A deny word that stands where the neutralizer keeps the record's words — here in a msg of the
// daemon's, which the trace reads lines by — fails the neutralization rather than passing it
// through, and the output and the map are not left behind.
func TestNeutralizeRefusesADenyWordItMustKeep(t *testing.T) {
	in := privateWindow(t)
	scratch := t.TempDir()
	out, mapFile := filepath.Join(scratch, "neutral"), filepath.Join(scratch, "map.json")
	_, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: mapFile, Deny: []string{"Turn"}})
	if err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Errorf("Neutralize denying a word of a msg = %v, want a refusal naming the word left", err)
	}
	for _, path := range []string{out, mapFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the refused neutralization left %s: %v", path, err)
		}
	}
}

// The signals' inputs are renamed as every other input is: a pod name's project and issue key
// in the launcher's lines and the pods' memory, as a claim token's are; a host in the launcher's
// own words; the repository of a queue file; a GitHub account in the merge queue's events, its
// login as the person's login and its numeric id, node id and links; the project, the people, the
// sessions, the repositories, the token's name and the free text of the project's settings
// events; a person's login and an issue key in a pull request's branch; and the default assignee.
// The provider's minutes hold no name, and keep their values. So no file of the neutral window
// holds a private name, every line of an input keeps its shape, and each name is the one it
// becomes in the other inputs.
func TestNeutralizeRenamesTheSignalInputs(t *testing.T) {
	w := privateRecord(t)
	at := func(minute int) time.Time { return time.Date(2026, time.March, 2, 10, minute, 0, 0, time.UTC) }
	pod := "legion-nimbus-nimbus-412"
	limit, source := int64(6000000), "gateway-log"
	w.DefaultAssignee = privateEmail
	w.Signals = map[string][]Span{SignalKills: {{From: at(0), To: at(30)}}, SignalQueue: {{From: at(0), To: at(30)}}}
	w.Launcher = []LauncherLine{
		{Time: at(3), Pod: pod, Text: `launcher: memory guard: ended pid=9 role=implementer size=1 reading=2 counter=3 shmem=0 limit=4 executable="node"`},
		{Time: at(4), Pod: pod, Text: "launcher: memory guard: report unsent sequence=1 pid=9 reason=dial tcp: lookup " + privateHost + ": no such host; the next connection carries it"},
	}
	w.Memory = []MemorySample{{Minute: at(3), Pod: pod, Bytes: 4096}}
	w.Provider = []ProviderMinute{{Minute: at(3), Uncached: 7, Limit: &limit, LimitSource: &source, Own: 1}}
	w.Queue = map[string][]json.RawMessage{privateRepo: {json.RawMessage(`{"pullRequest":88,"item":{"id":5,` +
		`"url":"https://api.github.com/repos/` + privateRepo + `/issues/events/5","event":"added_to_merge_queue",` +
		`"actor":{"login":"` + privateLogin + `","id":4242,"node_id":"U_hush4242","avatar_url":"https://avatars.githubusercontent.com/u/4242?v=4",` +
		`"url":"https://api.github.com/users/` + privateLogin + `","html_url":"https://github.com/` + privateLogin + `","type":"User"},` +
		`"commit_id":null,"created_at":"2026-03-02T10:05:00Z"}}`)}}
	person := map[string]any{"kind": "user", "id": privateEmail, "act": map[string]any{"session": privateSession, "token_id": privateAsk,
		"token_name": "hush-laptop-token", "scopes": []string{"person"}}}
	w.Settings = []json.RawMessage{
		privateSettings(t, 21, "settings.project_default_assignee.updated", at(1), person,
			map[string]any{"project": "NIMBUS", "assignee": privateEmail, "thread_logins": []string{privateEmail}}),
		privateSettings(t, 22, "settings.architecture_source.updated", at(2), person, map[string]any{"deleted": false, "source": map[string]any{
			"repo": privateRepo, "branch": privateLogin + "/nimbus-412-keys", "project": "NIMBUS", "created_by": map[string]any{"id": privateEmail, "kind": "user"},
			"last_error": "the app is not installed on " + privateRepo}}),
		privateSettings(t, 23, "settings.repo_project.updated", at(3), map[string]any{"kind": "session", "id": privateSession},
			map[string]any{"deleted": false, "mapping": map[string]any{"repo": privateRepo, "project": "NIMBUS", "renamed_to": "hushcorp/vault-next"}}),
	}
	in := filepath.Join(t.TempDir(), "private")
	if err := w.Write(in); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	out := filepath.Join(scratch, "neutral")
	names, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: filepath.Join(scratch, "map.json"), Deny: []string{privateWord}})
	if err != nil {
		t.Fatalf("Neutralize: %v", err)
	}
	assertNoneHeld(t, out, append(slices.Clone(privateNames), "/u/4242", "laptop"), true)
	neutral, err := ReadWindow(out)
	if err != nil {
		t.Fatalf("ReadWindow of the neutral window: %v", err)
	}

	child, email, login := names.Keys["NIMBUS-412"], names.People[privateEmail], names.People[privateLogin]
	if child != "ACME-2" || email == "" || login == "" {
		t.Fatalf("NIMBUS-412, the email and the login became %q, %q and %q", child, email, login)
	}
	neutralPod := "legion-acme-acme-2"
	if len(neutral.Launcher) != 2 || neutral.Launcher[0].Pod != neutralPod || neutral.Launcher[1].Pod != neutralPod ||
		neutral.Launcher[0].Text != w.Launcher[0].Text || !neutral.Launcher[0].Time.Equal(w.Launcher[0].Time) ||
		neutral.Launcher[1].Text != "launcher: memory guard: report unsent sequence=1 pid=9 reason=dial tcp: lookup "+names.Hosts[privateHost]+
			": no such host; the next connection carries it" {
		t.Errorf("the launcher's lines became %s, want each of pod %s, its text kept but the host's name, %s", describeReading(neutral.Launcher),
			neutralPod, names.Hosts[privateHost])
	}
	if want := []MemorySample{{Minute: at(3), Pod: neutralPod, Bytes: 4096}}; !sameReading(neutral.Memory, want) {
		t.Errorf("the pods' memory became %s, want %s", describeReading(neutral.Memory), describeReading(want))
	}
	if !sameReading(neutral.Provider, w.Provider) {
		t.Errorf("the provider's minutes became %s, want them as recorded, %s", describeReading(neutral.Provider), describeReading(w.Provider))
	}
	if neutral.DefaultAssignee != email || !sameSignals(neutral.Signals, w.Signals) {
		t.Errorf("the default assignee became %q and the signals %v; want %q and the record's spans", neutral.DefaultAssignee, neutral.Signals, email)
	}
	queue := neutral.Queue["acme/repo-1"]
	if len(neutral.Queue) != 1 || len(queue) != 1 {
		t.Fatalf("the neutral window holds the queues %v, want acme/repo-1's line", neutral.Queue)
	}
	if !reflect.DeepEqual(shapeOf(t, queue[0]), shapeOf(t, w.Queue[privateRepo][0])) {
		t.Errorf("the queue's line is\n  %s\nwhose shape differs from the record's\n  %s", queue[0], w.Queue[privateRepo][0])
	}
	if event, err := ParseQueueLine(queue[0]); err != nil || event.Actor != login || event.PullRequest != 88 || event.Event != "added_to_merge_queue" ||
		event.ID != 5 || event.Repo != "acme/repo-1" {
		t.Errorf("the queue's line reads as %+v (%v), want event 5 of pull request 88 of acme/repo-1 added to the queue by %s", event, err, login)
	}
	for i, event := range w.Settings {
		if !reflect.DeepEqual(shapeOf(t, neutral.Settings[i]), shapeOf(t, event)) {
			t.Errorf("settings event %d is\n  %s\nwhose shape differs from the record's\n  %s", i+1, neutral.Settings[i], event)
		}
	}
	assignee, err := ParseSettingsEvent(neutral.Settings[0])
	if err != nil {
		t.Fatalf("the neutral default assignee event does not read: %v", err)
	}
	for _, check := range []struct {
		what      string
		got, want any
	}{
		{"the default assignee event's project", assignee.Project, "ACME"},
		{"the default assignee event's assignee", assignee.DefaultAssignee, email},
		{"the default assignee event's actor", assignee.Actor.ID, email},
		{"the thread's login", pick(t, neutral.Settings[0], "payload", "thread_logins", 0), email},
		{"the token's session", pick(t, neutral.Settings[0], "actor", "act", "session"), names.IDs[privateSession]},
		{"the token's name", pick(t, neutral.Settings[0], "actor", "act", "token_name"), names.Names["hush-laptop-token"]},
		{"the architecture source's repository", pick(t, neutral.Settings[1], "payload", "source", "repo"), "acme/repo-1"},
		{"the architecture source's branch", pick(t, neutral.Settings[1], "payload", "source", "branch"), login + "/acme-2-keys"},
		{"the architecture source's error", pick(t, neutral.Settings[1], "payload", "source", "last_error"), names.Text["the app is not installed on "+privateRepo]},
		{"the mapping's session", pick(t, neutral.Settings[2], "actor", "id"), names.IDs[privateSession]},
		{"the mapping's new name", pick(t, neutral.Settings[2], "payload", "mapping", "renamed_to"), names.Repos["hushcorp/vault-next"]},
		{"the queue actor's links", pick(t, queue[0], "item", "actor", "url"), names.Links["https://api.github.com/users/"+privateLogin]},
		{"the queue actor's id", pick(t, queue[0], "item", "actor", "id"), 1.0},
		{"the queue event's url", pick(t, queue[0], "item", "url"), "https://api.github.com/repos/acme/repo-1/issues/events/5"},
	} {
		if !reflect.DeepEqual(check.got, check.want) || check.want == "" || check.want == nil {
			t.Errorf("%s is %v, want %v", check.what, check.got, check.want)
		}
	}
}

// privateSettings is one settings event of the project NIMBUS as Dispatch's event log serves it.
func privateSettings(t *testing.T, id int64, kind string, at time.Time, actor, payload map[string]any) json.RawMessage {
	t.Helper()
	event, err := json.Marshal(map[string]any{"id": id, "issue_key": nil, "artifact_id": nil, "project": "NIMBUS", "seq": id, "type": kind,
		"actor": actor, "notify": false, "created_at": at, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// A deny word that is part of the word every deny word is replaced with (withheld), in any case,
// is refused before the window is read: its replacement would hold it again, and the check of the
// bytes written would refuse every window that holds it. Nothing is written.
func TestNeutralizeRefusesADenyWordItsReplacementHolds(t *testing.T) {
	in := privateWindow(t)
	scratch := t.TempDir()
	out, mapFile := filepath.Join(scratch, "neutral"), filepath.Join(scratch, "map.json")
	_, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: mapFile, Deny: []string{"Held"}})
	if err == nil || !strings.Contains(err.Error(), `"Held" is part of "withheld"`) {
		t.Errorf("Neutralize denying a word withheld holds = %v, want a refusal naming the word", err)
	}
	for _, path := range []string{out, mapFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the refused neutralization left %s: %v", path, err)
		}
	}
}

// A network address of the deployment is a name of its private infrastructure, which no neutral
// window may hold (AGENTS.md, "Conventions"). A window whose log names, in the detail of a
// displacement, a worker's IPv4 address with its port beside its host's name, and, in the detail
// of a death, a cluster's IPv6 address, another IPv4 address, and the loopback, unspecified and
// documentation addresses, and one of whose events names the worker's address again, is
// neutralized: each address of the deployment becomes one of a documentation range, the same
// address the same one in the log and in the event and two addresses two, none of them an address
// the record itself holds; the host's name becomes a neutral one; and the loopback, unspecified
// and documentation addresses are kept. The map holds each address renamed, and the neutral
// window none of them.
func TestNeutralizeRenamesEveryAddressToADocumentationOne(t *testing.T) {
	recorded, err := ReadWindow(privateWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	const (
		worker  = "10.20.30.40"
		another = "172.16.9.8"
		cluster = "fd00:10:96::1a"
		host    = "vault-runner.hush.internal"
	)
	at := time.Date(2026, time.March, 2, 10, 8, 0, 0, time.UTC)
	token := privateToken
	kept := "via 127.0.0.1:8080, [::1]:9000, 0.0.0.0:13371, 192.0.2.1, 198.51.100.7 and [2001:db8::42]:80"
	recorded.Log = append(recorded.Log,
		jsonLine(t, at, slog.LevelWarn, processDisplacedLine, "claim", token, "incarnation", "5e0c1a01", "process", "pod/nimbus-412",
			"observed", "gone", "detail", "dial tcp "+worker+":13371: connect: connection refused; the pod on "+host+" is gone"),
		jsonLine(t, at.Add(time.Minute), slog.LevelWarn, processDiedLine, "claim", token, "incarnation", "5e0c1a02", "observed", "gone",
			"detail", "dial tcp ["+cluster+"]:443 from "+another+" "+kept, "podEnd", "", "chargedBefore", false))
	recorded.Lines = slices.Sorted(slices.Values(append(slices.Clone(recorded.Lines), processDisplacedLine, processDiedLine)))
	recorded.Events["NIMBUS-412"] = append(slices.Clone(recorded.Events["NIMBUS-412"]), privateEvent(t, 17, 7, "NIMBUS-412", "NIMBUS",
		"issue.commented", at, map[string]any{"kind": "user", "id": privateEmail}, map[string]any{"endpoint": worker + ":13371"}))
	scratch := t.TempDir()
	in, out := filepath.Join(scratch, "addresses"), filepath.Join(scratch, "neutral")
	if err := recorded.Write(in); err != nil {
		t.Fatal(err)
	}
	names, err := Neutralize(NeutralizeConfig{In: in, Out: out, Project: "ACME", Map: filepath.Join(scratch, "map.json"), Deny: []string{privateWord}})
	if err != nil {
		t.Fatalf("Neutralize: %v", err)
	}

	// 192.0.2.1 is the record's own, so the first address renamed is the next one.
	want := map[string]string{worker: "192.0.2.2", cluster: "2001:db8::1", another: "192.0.2.3"}
	if !maps.Equal(names.Addresses, want) {
		t.Errorf("the map renames the addresses %v; want %v", names.Addresses, want)
	}
	for address, neutral := range names.Addresses {
		addr, err := netip.ParseAddr(neutral)
		if err != nil || !documentationAddress(addr) {
			t.Errorf("%s became %q, which is no address of a documentation range", address, neutral)
		}
	}
	w, err := ReadWindow(out)
	if err != nil {
		t.Fatal(err)
	}
	n := len(w.Log)
	displaced, died := w.Log[n-2], w.Log[n-1]
	for _, check := range []struct {
		what      string
		got, want any
	}{
		{"the displacement's detail", pick(t, displaced, "detail"),
			"dial tcp 192.0.2.2:13371: connect: connection refused; the pod on " + names.Hosts[host] + " is gone"},
		{"the death's detail", pick(t, died, "detail"), "dial tcp [2001:db8::1]:443 from 192.0.2.3 " + kept},
		{"the event's endpoint", pick(t, w.Events["ACME-2"][len(w.Events["ACME-2"])-1], "payload", "endpoint"), "192.0.2.2:13371"},
	} {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("%s is %v, want %v", check.what, check.got, check.want)
		}
	}
	if !strings.HasSuffix(names.Hosts[host], ".example.invalid") {
		t.Errorf("the worker's host became %q", names.Hosts[host])
	}
	assertNoneHeld(t, out, []string{worker, another, cluster, host}, false)
}

// assertNoneHeld fails for each file under dir that holds one of names, in any case where foldCase.
func assertNoneHeld(t *testing.T, dir string, names []string, foldCase bool) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if foldCase {
			data = bytes.ToLower(data)
		}
		for _, name := range names {
			held := []byte(name)
			if foldCase {
				held = bytes.ToLower(held)
			}
			if bytes.Contains(data, held) {
				t.Errorf("%s still holds %q", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
