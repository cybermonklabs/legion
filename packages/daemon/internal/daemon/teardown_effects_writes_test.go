package daemon

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// teardownEffectsExchange is one request a test's Dispatch answers, and its answer.
type teardownEffectsExchange struct {
	method, path string
	status       int
	answer       string
}

// teardownEffectsThroughProxy sends each exchange's request, in order, through the recording proxy
// (teardownEffectsStartProxy) to a Dispatch that answers it as the exchange says, the way Dispatch
// answers the dispatch CLI: gzipped where the request takes gzip, as bun's fetch asks, and the
// answer is JSON of a kilobyte or more (compressAnswers in
// packages/envoy/internal/dispatch/api/response_compression.go). It answers what the proxy
// recorded: the number of requests it observed and each that came back, with its answer.
func teardownEffectsThroughProxy(t *testing.T, exchanges ...teardownEffectsExchange) (int, []teardownEffectsHTTP) {
	t.Helper()
	var mu sync.Mutex
	next := 0
	dispatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		x := exchanges[next]
		next++
		mu.Unlock()
		if req.Method != x.method || req.URL.Path != x.path {
			http.Error(w, "unexpected "+req.Method+" "+req.URL.Path, http.StatusTeapot)
			return
		}
		answer := []byte(x.answer)
		w.Header().Set("Content-Type", "application/json")
		if json.Valid(answer) && len(answer) >= 1024 && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
			var zipped bytes.Buffer
			writer := gzip.NewWriter(&zipped)
			_, _ = writer.Write(answer)
			_ = writer.Close()
			answer = zipped.Bytes()
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(x.status)
		_, _ = w.Write(answer)
	}))
	t.Cleanup(dispatch.Close)
	proxy := teardownEffectsStartProxy(t, dispatch.URL, func(string) bool { return true })
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	for _, x := range exchanges {
		request, err := http.NewRequest(x.method, proxy.url+x.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept-Encoding", "gzip, deflate, br")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	return proxy.observed()
}

// teardownEffectsPadded is a JSON object of a kilobyte or more, which Dispatch gzips, holding the
// members of object and a long title.
func teardownEffectsPadded(object string) string {
	return strings.TrimSuffix(object, "}") + `,"title":"` + strings.Repeat("A long title. ", 80) + `"}`
}

// Catches: a recording proxy that keeps a gzipped answer of Dispatch's as its compressed bytes in a
// JSON string, so the replay cannot read the CLI's read of an issue (H04-S bad's "cannot unmarshal
// string" at 74c7fd1f). The proxy passes the gzipped bytes to the caller, and records the JSON;
// an answer that is no JSON it records as a JSON string of its text.
func TestTeardownEffectsProxyRecordsJSON(t *testing.T) {
	t.Parallel()
	issue := teardownEffectsPadded(`{"key":"ACME-31","artifacts":[]}`)
	asked, sent := teardownEffectsThroughProxy(t,
		teardownEffectsExchange{"GET", "/api/v1/issues/ACME-31", 200, issue},
		teardownEffectsExchange{"GET", "/api/v1/issues/ACME-32", 404, "not found"},
	)
	if asked != 2 || len(sent) != 2 {
		t.Fatalf("the proxy observed %d requests and recorded %d, want 2 and 2", asked, len(sent))
	}
	if string(sent[0].Answer) != issue || sent[0].Status != 200 {
		t.Fatalf("the proxy recorded the gzipped issue as %d %.80s, want 200 and its JSON", sent[0].Status, sent[0].Answer)
	}
	if string(sent[1].Answer) != `"not found"` || sent[1].Status != 404 {
		t.Fatalf("the proxy recorded the answer that is no JSON as %d %s, want 404 and a JSON string", sent[1].Status, sent[1].Answer)
	}
}

// The trace's request-approval as the replay sends it, through the real dispatch CLI: the request
// of the document it names, or of the issue's primary document, with its summary and whether the
// Verification document goes with it.
func teardownEffectsApproval(key, reference string) teardownEffectsCommand {
	c := teardownEffectsCommand{Command: teardownEffectsApprovalCommand, Issue: key, Flags: []teardownEffectsFlag{{Name: "issue", Value: key}}}
	if reference != "" {
		c.Flags = append(c.Flags, teardownEffectsFlag{Name: "artifact", Value: reference})
	}
	c.Flags = append(c.Flags, teardownEffectsFlag{Name: "summary", Value: "Approve it.", Form: teardownEffectsText})
	return c
}

// A request-approval the replay sends again through the real dispatch CLI is a made write where
// Dispatch took it and the CLI exited 0, and passes otherwise only as one of the two refusals a
// trace's request can hold, neither of which changes anything: the CLI's own refusal of a document
// the issue does not hold, made before any approval request, with no status or code, and
// Dispatch's 409 PLAN_APPROVED_BY_REVIEW of a plan. Each other outcome is an error, which no control
// passes: an acceptance the CLI exited 1 on; another write Dispatch took; a request with no answer,
// or one answered 500 or more; another refusal; a document the issue holds; two requests; another
// exit code; a refusal the CLI printed no words of; and a read of the issue that is no JSON. Every
// answer is built through the recording proxy, as the replay records it, the issue's read gzipped
// as Dispatch gzips it.
func TestTeardownEffectsApprovalRefusal(t *testing.T) {
	t.Parallel()
	const key, approval = "ACME-12", "/api/v1/artifacts/a-plan/approval-requests"
	refusal := func(code string) string { return `{"code":"` + code + `","error":"refused"}` }
	const spec = `{"id":"a-spec","slug":"spec-md","name":"spec.md","primary":true}`
	const plan = `{"id":"a-plan","slug":"plan-md","name":"plan.md","primary":false}`
	issue := func(artifacts ...string) teardownEffectsExchange {
		return teardownEffectsExchange{"GET", "/api/v1/issues/" + key, 200, teardownEffectsPadded(`{"key":"ACME-12","primary_artifact_id":"a-spec","artifacts":[` + strings.Join(artifacts, ",") + `]}`)}
	}
	blocks := teardownEffectsExchange{"GET", "/api/v1/artifacts/a-plan/blocks", 200, `{"blocks":[]}`}
	planned := func(answer teardownEffectsExchange) []teardownEffectsExchange {
		return []teardownEffectsExchange{issue(spec, plan), blocks, answer}
	}
	missing := []teardownEffectsExchange{issue(spec), issue(spec)}
	refused := teardownEffectsExchange{"POST", approval, 409, refusal("PLAN_APPROVED_BY_REVIEW")}
	accepted := teardownEffectsExchange{"POST", approval, 201, `{"ask":{"id":"ask-1"},"version":1}`}
	const missingText = `document "plan.md" not found by slug; this issue's documents: spec-md (spec.md)`
	const planText = "PLAN_APPROVED_BY_REVIEW: a plan is approved by its plan review, not by a person"
	status, code := 409, "PLAN_APPROVED_BY_REVIEW"
	for _, row := range []struct {
		name      string
		reference string
		exit      int
		printed   string
		lost      int
		sent      []teardownEffectsExchange
		want      *teardownEffectsRefusal
		fault     string
	}{
		{name: "a plan, which Dispatch refuses 409 PLAN_APPROVED_BY_REVIEW", reference: "plan.md", exit: 1, printed: planText + "\n", sent: planned(refused),
			want: &teardownEffectsRefusal{Cause: teardownEffectsPlanApproval, Status: &status, Code: &code, Diagnostic: planText}},
		{name: "a document the issue does not hold, which the CLI refuses before any request", reference: "plan.md", exit: 1, printed: missingText + "\n", sent: missing,
			want: &teardownEffectsRefusal{Cause: teardownEffectsMissingArtifact, Diagnostic: missingText}},
		{name: "Dispatch took the request, opening it for a person", reference: "plan.md", printed: "Approval requested for plan.md", sent: planned(accepted)},
		{name: "Dispatch took the spec's request with its Verification document", printed: "Approval requested for spec.md", sent: []teardownEffectsExchange{issue(spec), {"GET", "/api/v1/artifacts/a-spec/blocks", 200, `{"blocks":[]}`}, {"POST", "/api/v1/artifacts/a-spec/approval-requests", 201, `{"ask":{"id":"ask-2"},"version":2}`}}},
		{name: "Dispatch took the request of a document approved already", reference: "plan.md", printed: "plan.md is already approved", sent: planned(teardownEffectsExchange{"POST", approval, 200, `{"ask":null,"version":1}`})},
		{name: "Dispatch took the request, and the CLI exited 1", reference: "plan.md", exit: 1, printed: "Approval requested for plan.md", sent: planned(accepted), fault: "and the CLI exited 1"},
		{name: "another refusal of the request", reference: "plan.md", exit: 1, printed: "NO_VERSION: no settled version", sent: planned(teardownEffectsExchange{"POST", approval, 409, refusal("NO_VERSION")}), fault: "409 NO_VERSION"},
		{name: "a refusal of the request that names no code", reference: "plan.md", exit: 1, printed: "HTTP_409: conflict", sent: planned(teardownEffectsExchange{"POST", approval, 409, `"conflict"`}), fault: "refused the approval request 409,"},
		{name: "a summary Dispatch refuses", reference: "plan.md", exit: 1, printed: "SUMMARY_INPUT: summary is blank", sent: planned(teardownEffectsExchange{"POST", approval, 400, refusal("SUMMARY_INPUT")}), fault: "400 SUMMARY_INPUT"},
		{name: "a server failure of the request", reference: "plan.md", exit: 1, printed: "HTTP_503: unavailable", sent: planned(teardownEffectsExchange{"POST", approval, 503, refusal("UNAVAILABLE")}), fault: "server failure"},
		{name: "a server failure of the read of the issue", reference: "plan.md", exit: 1, printed: "HTTP_500", sent: []teardownEffectsExchange{{"GET", "/api/v1/issues/" + key, 500, `"boom"`}}, fault: "server failure"},
		{name: "a request that got no answer", reference: "plan.md", exit: 1, printed: "fetch failed", lost: 1, sent: missing, fault: "transport failure"},
		{name: "no request that reached Dispatch", reference: "plan.md", exit: 1, printed: "Unable to connect", fault: "answered no read"},
		{name: "a read of the issue Dispatch refused", reference: "plan.md", exit: 1, printed: "NOT_FOUND: no issue", sent: []teardownEffectsExchange{{"GET", "/api/v1/issues/" + key, 404, refusal("NOT_FOUND")}}, fault: "404 NOT_FOUND"},
		{name: "a read of the issue that is no JSON", reference: "plan.md", exit: 1, printed: missingText, sent: []teardownEffectsExchange{{"GET", "/api/v1/issues/" + key, 200, "<html>"}}, fault: "is no JSON"},
		{name: "another write Dispatch took", reference: "plan.md", exit: 1, printed: missingText, sent: []teardownEffectsExchange{issue(spec), {"POST", "/api/v1/issues/" + key + "/comments", 201, `{"id":"c-1"}`}}, fault: "a change"},
		{name: "a document the issue holds, refused before the request (an open decision block)", reference: "plan.md", exit: 1, printed: "dispatch request-approval refuses an open block", sent: []teardownEffectsExchange{issue(spec, plan), blocks}, fault: "holds the document plan-md (plan.md)"},
		{name: "a document the issue holds, named by its slug", reference: "plan-md", exit: 1, printed: "refused", sent: []teardownEffectsExchange{issue(spec, plan)}, fault: "holds the document plan-md (plan.md)"},
		{name: "the spec, which names the issue's primary document", reference: "spec", exit: 1, printed: "refused", sent: []teardownEffectsExchange{issue(spec)}, fault: "holds the document spec-md (spec.md)"},
		{name: "no document, which names the issue's primary document", exit: 1, printed: "refused", sent: []teardownEffectsExchange{issue(spec)}, fault: "holds the document spec-md (spec.md)"},
		{name: "two approval requests", reference: "plan.md", exit: 1, printed: planText, sent: []teardownEffectsExchange{issue(spec, plan), refused, refused}, fault: "2 approval requests"},
		{name: "a plan's refusal the CLI exited 0 on", reference: "plan.md", printed: planText, sent: planned(refused), fault: "exited 0"},
		{name: "a missing document the CLI exited 2 on, a usage error", reference: "plan.md", exit: 2, printed: missingText, sent: missing, fault: "exited 2"},
		{name: "a refusal the CLI printed no words of", reference: "plan.md", exit: 1, printed: " \n", sent: planned(refused), fault: "printed nothing"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			asked, sent := teardownEffectsThroughProxy(t, row.sent...)
			got, err := teardownEffectsApprovalRefusal(teardownEffectsApproval(key, row.reference), row.exit, row.printed, asked+row.lost, sent)
			switch {
			case row.fault != "":
				if got != nil || err == nil || !strings.Contains(err.Error(), row.fault) {
					t.Fatalf("judged %+v, %v; want no refusal and an error that names %q", got, err, row.fault)
				}
			case err != nil || !reflect.DeepEqual(got, row.want):
				t.Fatalf("judged %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
}

// teardownEffectsTestIDs is the id map of a replay of a synthetic world on ACME-31: the world's
// comment c-scope and question a-window, a comment a trace names note-1, and a gate document the
// world names art-gate, each by the id the replay's Dispatch gave it.
func teardownEffectsTestIDs() map[string]string {
	return map[string]string{
		"comment/c-scope":  "0c1d2e3f-0000-4000-8000-000000000001",
		"ask/a-window":     "0c1d2e3f-0000-4000-8000-000000000002",
		"comment/note-1":   "0c1d2e3f-0000-4000-8000-000000000003",
		"artifact/art-gtw": "0c1d2e3f-0000-4000-8000-000000000004",
	}
}

// Each Dispatch write a trace can hold, but a handoff, a result and a question, is the role's own
// `dispatch` command (teardownEffectsTraceCommand): each field the command's flag, each record the
// case names by its own name the replay's id of it, each reference in a text the replay's id too,
// and each bare pointer of a document's text the reference the importer writes. An upload's and an
// edit's version is checked against Dispatch's answer and not sent; a comment's own name is kept
// for the id map and not sent. A write on another issue of the world is made there, and so is an
// edit of another issue's document named by its reference.
func TestTeardownEffectsTraceCommand(t *testing.T) {
	t.Parallel()
	const key = "ACME-31"
	held := []string{key, "ACME-30", "ACME-32", "ACME-33"}
	ids := teardownEffectsTestIDs()
	scope, window, note, gate := ids["comment/c-scope"], ids["ask/a-window"], ids["comment/note-1"], ids["artifact/art-gtw"]
	at := time.Date(2030, 2, 4, 10, 0, 0, 0, time.UTC)
	action := func(command, target string, fields map[string]any) teardownEffectsTraceAction {
		return teardownEffectsTraceAction{Kind: "dispatch", Command: command, Target: target, Seq: 3, At: at, Fields: fields}
	}
	plain := func(name, value string) teardownEffectsFlag { return teardownEffectsFlag{Name: name, Value: value} }
	text := func(name, value string) teardownEffectsFlag {
		return teardownEffectsFlag{Name: name, Value: value, Form: teardownEffectsText}
	}
	set := func(name string) teardownEffectsFlag {
		return teardownEffectsFlag{Name: name, Form: teardownEffectsSet}
	}
	on := plain("issue", key)
	for _, row := range []struct {
		name   string
		action teardownEffectsTraceAction
		want   teardownEffectsCommand
	}{
		{"a plain comment", action("comment", key, map[string]any{"body": "The scope stands."}),
			teardownEffectsCommand{Command: "comment", Issue: key, Flags: []teardownEffectsFlag{on, text("body", "The scope stands.")}}},
		{"a reply to a comment of the world", action("comment", key, map[string]any{"reply_to": "c-scope", "body": "Agreed with dispatch://ACME-31/comment/c-scope."}),
			teardownEffectsCommand{Command: "comment", Issue: key, Flags: []teardownEffectsFlag{on, text("body", "Agreed with dispatch://ACME-31/comment/"+scope+"."), plain("reply-to", scope)}}},
		{"an answer on a question of the world", action("comment", key, map[string]any{"ask_id": "a-window", "turn": "human", "body": "Tuesday."}),
			teardownEffectsCommand{Command: "comment", Issue: key, Flags: []teardownEffectsFlag{on, text("body", "Tuesday."), plain("reply-to-ask", window), plain("turn", "human")}}},
		{"a comment the trace names", action("comment", key, map[string]any{"id": "note-2", "body": "A note."}),
			teardownEffectsCommand{Command: "comment", Issue: key, Name: "note-2", Flags: []teardownEffectsFlag{on, text("body", "A note.")}}},
		{"a message that links a comment the trace named", action("message", key, map[string]any{"body": "See dispatch://ACME-31/comment/note-1 and dispatch://ACME-30/comment/note-1."}),
			teardownEffectsCommand{Command: "message", Issue: key, Flags: []teardownEffectsFlag{on, text("body", "See dispatch://ACME-31/comment/"+note+" and dispatch://ACME-30/comment/note-1.")}}},
		{"a status of the case's issue", action("issue-update", key, map[string]any{"status": "todo"}),
			teardownEffectsCommand{Command: "issue-update", Issue: key, Flags: []teardownEffectsFlag{on, plain("status", "todo")}}},
		{"a status of another issue of the world", action("issue-update", "ACME-32", map[string]any{"status": "in_progress"}),
			teardownEffectsCommand{Command: "issue-update", Issue: "ACME-32", Flags: []teardownEffectsFlag{plain("issue", "ACME-32"), plain("status", "in_progress")}}},
		{"a move on the board of another issue of the world", action("issue-update", "ACME-33", map[string]any{"before": "ACME-32"}),
			teardownEffectsCommand{Command: "issue-update", Issue: "ACME-33", Flags: []teardownEffectsFlag{plain("issue", "ACME-33"), plain("before", "ACME-32")}}},
		{"an upload of a new version, with its bare pointers written as the importer writes them",
			action("artifact", key, map[string]any{"name": "plan.md", "content": ":::plan_task{#t2 serves=\"r1\" needs=\"t1\"}\nShip it, as dispatch://ACME-31/comment/c-scope asks.\n:::\n", "summary": "Second plan", "force": true, "version": 2.0}),
			teardownEffectsCommand{Command: "artifact", Issue: key, Version: 2, Flags: []teardownEffectsFlag{on, text("name", "plan.md"),
				text("content", ":::plan_task{#t2 serves=\""+specBlock(key, "r1")+"\" needs=\"dispatch://ACME-31/artifact/plan-md#b-t1\"}\nShip it, as dispatch://ACME-31/comment/"+scope+" asks.\n:::\n"),
				text("summary", "Second plan"), set("force")}}},
		{"an upload that asks for no new version", action("artifact", key, map[string]any{"name": "notes.md", "content": "Notes.\n", "force": false}),
			teardownEffectsCommand{Command: "artifact", Issue: key, Flags: []teardownEffectsFlag{on, text("name", "notes.md"), text("content", "Notes.\n"), set("no-force")}}},
		{"an edit of a document of the case's issue by a quote", action("doc-edit", key, map[string]any{"artifact": "rollout-md", "ops": []any{map[string]any{"op": "replace", "find": "two hosts", "with": "three hosts, as dispatch://ACME-31/comment/note-1 says"}}, "version": 2.0}),
			teardownEffectsCommand{Command: "doc-edit", Issue: key, Version: 2, Flags: []teardownEffectsFlag{on, plain("artifact", "rollout-md"),
				{Name: "ops", Value: `[{"find":"two hosts","op":"replace","with":"three hosts, as dispatch://ACME-31/comment/` + note + ` says"}]`, Form: teardownEffectsJSON}}}},
		{"an edit of a document the world names by its id", action("doc-edit", key, map[string]any{"artifact": "art-gtw", "ops": []any{map[string]any{"op": "delete", "block": "t1"}}, "summary": "Drop t1"}),
			teardownEffectsCommand{Command: "doc-edit", Issue: key, Flags: []teardownEffectsFlag{on, plain("artifact", gate), {Name: "ops", Value: `[{"block":"t1","op":"delete"}]`, Form: teardownEffectsJSON}, text("summary", "Drop t1")}}},
		{"an edit of another issue's document, by its reference", action("doc-edit", "dispatch://ACME-30/spec", map[string]any{"ops": []any{map[string]any{"op": "insert", "after": "end", "markdown": ":::check{#k9 serves=\"r2\"}\nIt holds.\n:::"}}, "summary": "Name the check"}),
			teardownEffectsCommand{Command: "doc-edit", Issue: "ACME-30", Flags: []teardownEffectsFlag{plain("ref", "dispatch://ACME-30/spec"),
				{Name: "ops", Value: `[{"after":"end","markdown":":::check{#k9 serves=\"` + specBlock("ACME-30", "r2") + `\"}\nIt holds.\n:::","op":"insert"}]`, Form: teardownEffectsJSON}, text("summary", "Name the check")}}},
		{"a plan review's approval of a version", action("plan-approve", key, map[string]any{"version": 1.0}),
			teardownEffectsCommand{Command: "plan-approve", Issue: key, Flags: []teardownEffectsFlag{on, plain("version", "1")}}},
		{"a request of the spec's approval with its Verification document", action(teardownEffectsApprovalCommand, key, map[string]any{"include_verification": true, "summary": "The rollout moves to three hosts."}),
			teardownEffectsCommand{Command: teardownEffectsApprovalCommand, Issue: key, Flags: []teardownEffectsFlag{on, text("summary", "The rollout moves to three hosts."), set("include-verification")}}},
		{"a request of a plan's approval", action(teardownEffectsApprovalCommand, key, map[string]any{"artifact": "plan.md", "summary": "Approve the plan."}),
			teardownEffectsCommand{Command: teardownEffectsApprovalCommand, Issue: key, Flags: []teardownEffectsFlag{on, plain("artifact", "plan.md"), text("summary", "Approve the plan.")}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := teardownEffectsTraceCommand(key, held, row.action, ids)
			if err != nil || !reflect.DeepEqual(got, row.want) {
				t.Fatalf("made %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
}

// A trace's Dispatch write the replay cannot make as the role makes it is an error, which makes the
// replay HARNESS and names what it cannot make: a command the replay makes no write of, a target or
// a board neighbour the case's world does not hold (a document reference on such an issue among
// them), a field the command takes no flag of, a reply or an answer that names a record the
// replay's Dispatch holds none of by that name, a version that is no version, and a value of another
// kind than its flag's.
func TestTeardownEffectsTraceCommandRefusals(t *testing.T) {
	t.Parallel()
	const key = "ACME-31"
	held := []string{key, "ACME-32"}
	action := func(command, target string, fields map[string]any) teardownEffectsTraceAction {
		return teardownEffectsTraceAction{Kind: "dispatch", Command: command, Target: target, Seq: 5, Fields: fields}
	}
	for _, row := range []struct {
		name   string
		action teardownEffectsTraceAction
		fault  string
	}{
		{"a command the replay makes no write of", action("claim", key, map[string]any{}), "makes no Dispatch write of the command claim"},
		{"a comment on an issue the world does not hold", action("comment", "ACME-99", map[string]any{"body": "Hi."}), "the trace's comment at action 5 is on ACME-99, which the case's world does not hold"},
		{"an edit of a document of an issue the world does not hold", action("doc-edit", "dispatch://ACME-99/spec", map[string]any{"ops": []any{map[string]any{"op": "delete", "block": "b1"}}}), "is on dispatch://ACME-99/spec, which the case's world does not hold"},
		{"a move before an issue the world does not hold", action("issue-update", "ACME-32", map[string]any{"before": "ACME-99"}), "moves ACME-32 before ACME-99 on the board, which the case's world does not hold"},
		{"a field the command takes no flag of", action("issue-update", key, map[string]any{"labels": []any{"ux"}}), "names the field labels, of which dispatch issue-update takes no flag"},
		{"a reply to a comment the replay holds none of", action("comment", key, map[string]any{"reply_to": "c-none", "body": "Yes."}), "names the comment c-none by reply_to, and the replay's Dispatch holds no comment the case names so"},
		{"an answer on a question the replay holds none of", action("comment", key, map[string]any{"ask_id": "a-none", "body": "Yes."}), "names the ask a-none by ask_id"},
		{"a version that is no version", action("artifact", key, map[string]any{"name": "plan.md", "content": "x", "version": 1.5}), "names the version 1.5, which is no version"},
		{"a flag that is no boolean", action("artifact", key, map[string]any{"name": "plan.md", "content": "x", "force": "yes"}), "names force as no boolean"},
		{"a body that is no text", action("message", key, map[string]any{"body": 7}), "names body as no text"},
		{"an edit whose operations are no list", action("doc-edit", key, map[string]any{"ops": "replace"}), "an edit holds a list of one operation or more"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got, err := teardownEffectsTraceCommand(key, held, row.action, teardownEffectsTestIDs()); err == nil || !strings.Contains(err.Error(), row.fault) {
				t.Fatalf("made %+v, %v; want an error that names %q", got, err, row.fault)
			}
		})
	}
}

// A reference to a comment or a question of the case's issue by a name the replay maps is written
// with the replay's id, and a document id the world names is written as the replay's; a reference
// of another issue, or by a name the replay does not map, stays as it is.
func TestTeardownEffectsCaseRefs(t *testing.T) {
	t.Parallel()
	ids := teardownEffectsTestIDs()
	got := teardownEffectsCaseRefs("dispatch://ACME-31/comment/c-scope, dispatch://ACME-31/ask/a-window, dispatch://ACME-31/ask/c-scope, dispatch://ACME-32/comment/c-scope, dispatch://ACME-31/comment/c-other and art-gtw.", "ACME-31", ids)
	want := "dispatch://ACME-31/comment/" + ids["comment/c-scope"] + ", dispatch://ACME-31/ask/" + ids["ask/a-window"] + ", dispatch://ACME-31/ask/c-scope, dispatch://ACME-32/comment/c-scope, dispatch://ACME-31/comment/c-other and " + ids["artifact/art-gtw"] + "."
	if got != want {
		t.Fatalf("mapped the text as %q, want %q", got, want)
	}
}

// What the real dispatch CLI made of a trace's write is read from Dispatch's answers as the
// recording proxy keeps them: the id of a comment, and an upload's or an edit's document and
// version, which must be the version the trace states. A refusal, by Dispatch or by the CLI before
// Dispatch refused anything, is an error that names the command and Dispatch's code or the CLI's
// words; so are a version Dispatch's answer does not name as stated, a write that got no answer,
// and a command that exited 0 and wrote nothing.
func TestTeardownEffectsCommandMade(t *testing.T) {
	t.Parallel()
	comment := teardownEffectsCommand{Command: "comment", Issue: "ACME-31", Flags: []teardownEffectsFlag{{Name: "issue", Value: "ACME-31"}, {Name: "body", Value: "Hi.", Form: teardownEffectsText}}}
	named := comment
	named.Name = "note-1"
	upload := teardownEffectsCommand{Command: "artifact", Issue: "ACME-31", Version: 2, Flags: []teardownEffectsFlag{{Name: "issue", Value: "ACME-31"}, {Name: "name", Value: "plan.md", Form: teardownEffectsText}}}
	stale := upload
	stale.Version = 3
	edit := teardownEffectsCommand{Command: "doc-edit", Issue: "ACME-31", Version: 3, Flags: []teardownEffectsFlag{{Name: "issue", Value: "ACME-31"}, {Name: "artifact", Value: "plan-md"}}}
	comments := "/api/v1/issues/ACME-31/comments"
	uploaded := []teardownEffectsExchange{
		{"GET", "/api/v1/issues/ACME-31/artifacts", 200, teardownEffectsPadded(`{"artifacts":[]}`)},
		{"POST", "/api/v1/markdown/round-trip", 200, `{"markdown":"x"}`},
		{"POST", "/api/v1/issues/ACME-31/artifacts", 201, `{"artifact":{"id":"a-plan","slug":"plan-md"},"version":{"number":2}}`},
	}
	edited := func(version string) []teardownEffectsExchange {
		return []teardownEffectsExchange{
			{"GET", "/api/v1/issues/ACME-31", 200, teardownEffectsPadded(`{"key":"ACME-31"}`)},
			{"POST", "/api/v1/artifacts/a-plan/edits", 200, `{"applied":1,"version":` + version + `}`},
		}
	}
	for _, row := range []struct {
		name    string
		command teardownEffectsCommand
		exit    int
		printed string
		lost    int
		sent    []teardownEffectsExchange
		want    teardownEffectsMade
		fault   string
	}{
		{name: "a comment", command: named, sent: []teardownEffectsExchange{{"POST", comments, 201, `{"id":"c-new","issue_key":"ACME-31"}`}}, want: teardownEffectsMade{ID: "c-new"}},
		{name: "a comment after a read Dispatch refused", command: comment, sent: []teardownEffectsExchange{{"GET", "/api/v1/asks/a1", 404, `{"code":"NOT_FOUND"}`}, {"POST", comments, 201, `{"id":"c-new"}`}}, want: teardownEffectsMade{ID: "c-new"}},
		{name: "an upload of the version the trace states", command: upload, sent: uploaded, want: teardownEffectsMade{Document: "a-plan", Version: 2}},
		{name: "an edit of the version the trace states", command: edit, sent: edited(`{"number":3}`), want: teardownEffectsMade{Document: "a-plan", Version: 3}},
		{name: "an upload of another version than the trace states", command: stale, sent: uploaded, fault: "the trace states that its write makes version 3, and Dispatch's answer to POST /api/v1/issues/ACME-31/artifacts names version 2"},
		{name: "an edit that changed nothing", command: edit, sent: edited("null"), fault: "makes version 3, and Dispatch's answer to POST /api/v1/artifacts/a-plan/edits names version 0"},
		{name: "a named comment whose answer names no id", command: named, sent: []teardownEffectsExchange{{"POST", comments, 201, `{}`}}, fault: "names no id for the comment the case names note-1"},
		{name: "a refusal by Dispatch", command: comment, exit: 1, printed: "ISSUE_CLOSED", sent: []teardownEffectsExchange{{"POST", comments, 409, `{"code":"ISSUE_CLOSED"}`}},
			fault: "dispatch comment --issue ACME-31 --body-file <body> exited 1: Dispatch refused POST /api/v1/issues/ACME-31/comments 409 ISSUE_CLOSED"},
		{name: "a server failure", command: comment, exit: 1, printed: "HTTP_503", sent: []teardownEffectsExchange{{"POST", comments, 503, `{"code":"UNAVAILABLE"}`}}, fault: "Dispatch refused POST /api/v1/issues/ACME-31/comments 503 UNAVAILABLE"},
		{name: "a refusal by the CLI before Dispatch refused anything", command: upload, exit: 1, printed: "dispatch artifact: Dispatch would store this text under another spelling\n",
			sent: uploaded[:2], fault: "dispatch artifact --issue ACME-31 --name-file <name> exited 1 before Dispatch refused any request: dispatch artifact: Dispatch would store this text under another spelling"},
		{name: "a write that got no answer", command: comment, exit: 1, printed: "fetch failed", lost: 1, fault: "sent Dispatch 1 requests, and 0 came back"},
		{name: "a command that wrote nothing", command: comment, sent: []teardownEffectsExchange{{"GET", "/api/v1/issues/ACME-31", 200, `{}`}}, fault: "exited 0 and sent Dispatch no write"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			asked, sent := teardownEffectsThroughProxy(t, row.sent...)
			got, err := teardownEffectsCommandMade(row.command, row.exit, row.printed, asked+row.lost, sent)
			switch {
			case row.fault != "":
				if err == nil || !strings.Contains(err.Error(), row.fault) {
					t.Fatalf("judged %+v, %v; want an error that names %q", got, err, row.fault)
				}
			case err != nil || got != row.want:
				t.Fatalf("judged %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
}

// A command's arguments name each text and JSON value by the file its flag reads, and a boolean
// by its flag alone, in the order of the flags.
func TestTeardownEffectsCommandArgv(t *testing.T) {
	t.Parallel()
	c := teardownEffectsCommand{Command: "doc-edit", Flags: []teardownEffectsFlag{
		{Name: "ref", Value: "dispatch://ACME-30/spec"},
		{Name: "ops", Value: `[{"op":"delete","block":"b1"}]`, Form: teardownEffectsJSON},
		{Name: "summary", Value: "Drop b1", Form: teardownEffectsText},
		{Name: "force", Form: teardownEffectsSet},
	}}
	written := map[string]string{}
	got := c.argv(func(flag, value string) string { written[flag] = value; return "/tmp/" + flag })
	want := []string{"doc-edit", "--ref", "dispatch://ACME-30/spec", "--ops-json-file", "/tmp/ops", "--summary-file", "/tmp/summary", "--force"}
	if !reflect.DeepEqual(got, want) || written["ops"] != `[{"op":"delete","block":"b1"}]` || written["summary"] != "Drop b1" {
		t.Fatalf("argv %v writing %v, want %v", got, written, want)
	}
	if s := c.String(); s != "dispatch doc-edit --ref dispatch://ACME-30/spec --ops-json-file <ops> --summary-file <summary> --force" {
		t.Fatalf("named the command %q", s)
	}
}

// A run's Dispatch write of each kind the replay makes is the request of the API route its
// `dispatch` command calls, on the case's issue: a reply and an answer with the replay's ids of
// the comment and the question they name, a message, a status, a document's first and later
// versions with the text each version had, a plan review's approval and an approval request of the
// documents it names, its summary read back as Dispatch reads it. Each record it makes joins the
// id map under the run's id of it; a record it names that the replay made no record of is an
// error.
func TestTeardownEffectsEventRequest(t *testing.T) {
	t.Parallel()
	const key = "ACME-31"
	ids := map[string]string{"comment/run-c1": "rep-c1", "ask/run-a1": "rep-a1", "message/run-m1": "rep-m1", "artifact/run-plan": "rep-plan", "artifact/run-spec": "rep-spec"}
	readback := &teardownEffectsReadback{
		Documents: []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
			Text string `json:"text"`
		}{{ID: "run-plan", Name: "plan.md", Kind: "doc", Text: "The third plan.\n"}},
		Versions: []struct {
			ArtifactID string `json:"artifact_id"`
			Number     int    `json:"number"`
			Markdown   string `json:"markdown"`
		}{{ArtifactID: "run-plan", Number: 1, Markdown: "The first plan, as dispatch://ACME-31/comment/run-c1 asks.\n"}, {ArtifactID: "run-plan", Number: 2, Markdown: "The second plan.\n"}},
		Asks: []teardownEffectsAsk{
			{ID: "run-a2", Question: "Which day?", Options: json.RawMessage(`[{"label":"Monday"}]`)},
			{ID: "run-a3", Question: "Approve spec.md (version 2) and verification.md (version 1)? The rollout moves to three hosts.", Approval: &struct {
				Documents []teardownEffectsApprovalDocument `json:"documents"`
			}{Documents: []teardownEffectsApprovalDocument{{ArtifactID: "run-spec", Name: "spec.md", Version: 2}, {ArtifactID: "run-verification", Name: "verification.md", Version: 1}}}},
		},
	}
	event := func(kind string, payload map[string]any) teardownEffectsEvent {
		return teardownEffectsEvent{Seq: 9, Type: kind, Actor: teardownEffectsActor{Kind: "session", ID: "ses-run"}, Payload: payload}
	}
	prefix := "/api/v1/issues/" + key
	for _, row := range []struct {
		name  string
		event teardownEffectsEvent
		want  teardownEffectsRequest
	}{
		{"a reply in a comment's thread", event("comment.created", map[string]any{"id": "run-c2", "body": "Yes, per dispatch://ACME-31/comment/run-c1.", "reply_to": "run-c1", "ask_id": nil, "anchor": nil}),
			teardownEffectsRequest{Method: "POST", Path: prefix + "/comments", Body: map[string]any{"body": "Yes, per dispatch://ACME-31/comment/rep-c1.", "reply_to": "rep-c1"}, Kind: "comment", ID: "run-c2"}},
		{"an answer on a question", event("comment.created", map[string]any{"id": "run-c3", "body": "Monday.", "ask_id": "run-a1", "turn": "human"}),
			teardownEffectsRequest{Method: "POST", Path: prefix + "/comments", Body: map[string]any{"body": "Monday.", "ask_id": "rep-a1", "turn": "human"}, Kind: "comment", ID: "run-c3"}},
		{"a message that replies to another", event("message.created", map[string]any{"id": "run-m2", "body": "Shipped.", "in_reply_to": "run-m1"}),
			teardownEffectsRequest{Method: "POST", Path: prefix + "/messages", Body: map[string]any{"body": "Shipped.", "in_reply_to": "rep-m1"}, Kind: "message", ID: "run-m2"}},
		{"a status", event("issue.updated", map[string]any{"key": key, "status": "todo", "changed": []any{"status"}}),
			teardownEffectsRequest{Method: "PATCH", Path: prefix, Body: map[string]any{"status": "todo"}}},
		{"a document's first version", event("artifact.created", map[string]any{"artifact": map[string]any{"id": "run-plan", "name": "plan.md"}}),
			teardownEffectsRequest{Method: "POST", Path: prefix + "/artifacts", Body: map[string]any{"name": "plan.md", "content": "The first plan, as dispatch://ACME-31/comment/rep-c1 asks.\n"}, Kind: "artifact", ID: "run-plan", Version: 1}},
		{"a document's later version", event("artifact.version", map[string]any{"artifact_id": "run-plan", "name": "plan.md", "version": map[string]any{"number": 2}}),
			teardownEffectsRequest{Method: "POST", Path: prefix + "/artifacts", Body: map[string]any{"name": "plan.md", "content": "The second plan.\n"}, Kind: "artifact", ID: "run-plan", Version: 2}},
		{"a plan review's approval", event("artifact.approved", map[string]any{"artifact_id": "run-plan", "name": "plan.md", "version": float64(2), "kind": "plan"}),
			teardownEffectsRequest{Method: "POST", Path: "/api/v1/artifacts/rep-plan/plan-reviews", Body: map[string]any{"version": 2}}},
		{"a question", event("ask.opened", map[string]any{"id": "run-a2"}),
			teardownEffectsRequest{Method: "POST", Path: prefix + "/asks", Body: map[string]any{"question": "Which day?", "options": json.RawMessage(`[{"label":"Monday"}]`)}, Kind: "ask", ID: "run-a2"}},
		{"an approval request of the spec with its Verification document", event("ask.opened", map[string]any{"id": "run-a3", "artifact_id": "run-spec"}),
			teardownEffectsRequest{Method: "POST", Path: "/api/v1/artifacts/rep-spec/approval-requests", Body: map[string]any{"summary": "The rollout moves to three hosts.", "include_verification": true}, Kind: "ask", ID: "run-a3", Version: 2}},
		{"a person's answer of a question", event("ask.answered", map[string]any{"id": "run-a1", "answer": map[string]any{"selected": []any{"Monday"}, "text": "Monday"}}),
			teardownEffectsRequest{Method: "POST", Path: "/api/v1/asks/rep-a1/answer", Body: map[string]any{"selected": []any{"Monday"}, "text": "Monday", "expected_edited_at": nil}}},
		{"a resolve of a question", event("ask.resolved", map[string]any{"id": "run-a1", "resolution": map[string]any{"kind": "resolved", "reason": "Settled in the thread."}}),
			teardownEffectsRequest{Method: "POST", Path: "/api/v1/asks/rep-a1/resolve", Body: map[string]any{"kind": "resolved", "reason": "Settled in the thread."}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if fault, _ := teardownEffectsDispatchFault(readback, row.event); fault != "" {
				t.Fatalf("the replay refuses %+v: %s", row.event, fault)
			}
			got, err := teardownEffectsEventRequest(key, row.event, readback, ids)
			if err != nil || !reflect.DeepEqual(got, row.want) {
				t.Fatalf("made %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
	if _, err := teardownEffectsEventRequest(key, event("comment.created", map[string]any{"id": "run-c4", "body": "Yes.", "reply_to": "run-unknown"}), readback, ids); err == nil || !strings.Contains(err.Error(), "names the comment run-unknown by reply_to, which the replay made no record of") {
		t.Fatalf("a reply to a comment the replay made no record of: %v", err)
	}
}

// A run's Dispatch write the replay does not make is named by why, and typed: a write of a kind
// the replay never makes (another change of the issue than its status, a comment anchored on a
// document, a person's review, a question of a document) is unsupported, and a record the readback
// lacks (a version's text, a body Dispatch cut short, an approval question that does not open as
// Dispatch's does) is the harness's own.
func TestTeardownEffectsDispatchFaultKinds(t *testing.T) {
	t.Parallel()
	readback := &teardownEffectsReadback{
		Documents: []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
			Text string `json:"text"`
		}{{ID: "run-plan", Name: "plan.md", Kind: "doc", Text: "The plan.\n"}},
		Asks: []teardownEffectsAsk{{ID: "run-a3", Question: "Approve plan.md?", Approval: &struct {
			Documents []teardownEffectsApprovalDocument `json:"documents"`
		}{Documents: []teardownEffectsApprovalDocument{{ArtifactID: "run-plan", Name: "plan.md", Version: 1}}}}},
	}
	event := func(kind string, payload map[string]any) teardownEffectsEvent {
		return teardownEffectsEvent{Seq: 9, Type: kind, Actor: teardownEffectsActor{Kind: "session", ID: "ses-run"}, Payload: payload}
	}
	for _, row := range []struct {
		name        string
		event       teardownEffectsEvent
		fault       string
		unsupported bool
	}{
		{"a move on the board", event("issue.updated", map[string]any{"status": "todo", "changed": []any{"rank"}}), "a change of the issue's rank", true},
		{"an update that names no change", event("issue.updated", map[string]any{"status": "todo"}), "names no field it changed", false},
		{"a comment anchored on a document", event("comment.created", map[string]any{"body": "Here.", "anchor": map[string]any{"quote": "x"}}), "(anchor)", true},
		{"a comment whose body Dispatch cut short", event("comment.created", map[string]any{"body": "The start", "body_truncated": true}), "the start of its body alone", false},
		{"a message aimed at a party", event("message.created", map[string]any{"body": "Hi.", "target": "session:ses-other"}), "(target)", true},
		{"a person's review", event("artifact.approved", map[string]any{"artifact_id": "run-plan", "kind": "person"}), "a review of kind person", true},
		{"a version whose text the readback lacks", event("artifact.version", map[string]any{"artifact_id": "run-plan", "name": "plan.md", "version": map[string]any{"number": 2}}), "holds no text of version 2 of plan.md", false},
		{"an approval question that does not open as Dispatch's", event("ask.opened", map[string]any{"id": "run-a3"}), "does not open as Dispatch's approval question does", false},
		{"a claim", event("issue.claimed", map[string]any{}), "the replay writes no issue.claimed", true},
		{"a question withdrawn by a close", event("ask.resolved", map[string]any{"id": "run-a1", "resolution": map[string]any{"kind": "withdrawn"}}), "a resolution of kind withdrawn", true},
		{"an answer whose event carries none", event("ask.answered", map[string]any{"id": "run-a1"}), "its event carries no answer", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fault, unsupported := teardownEffectsDispatchFault(readback, row.event)
			if !strings.Contains(fault, row.fault) || unsupported != row.unsupported {
				t.Fatalf("named %q (unsupported %t), want %q (unsupported %t)", fault, unsupported, row.fault, row.unsupported)
			}
		})
	}
}

// teardownEffectsWritesFile is the public fixture of the Dispatch writes (its .command says how it
// was made): T0's example with DW1, a synthetic planner case on ACME-31 whose two traces hold every
// Dispatch write lane D makes. The example's two cases show the checks --controls must show.
const teardownEffectsWritesFile = "testdata/teardown-effects/dispatch-writes-cases.json"

// Catches: the public fixture of the Dispatch writes losing a shape of write or a name it maps,
// holding a world part lane D cannot seed or a write lane D cannot make as the role makes it, or
// stating a body its trace's writes do not leave, before the `--controls` Job replays it. Its cases
// are decoded as --controls decodes a case file, and each write is made into the role's command with
// the ids lane D's seeding gives the world's comment and question.
func TestTeardownEffectsDispatchWritesFixture(t *testing.T) {
	t.Parallel()
	cases := teardownEffectsCasesAt(t, teardownEffectsWritesFile, nil)
	if len(cases) != 3 || cases[2].ID != "DW1" {
		t.Fatalf("the fixture holds %d cases, want the example's two and DW1", len(cases))
	}
	if _, err := teardownEffectsPromised(cases); err != nil {
		t.Fatalf("--controls promises no controls of the fixture: %v", err)
	}
	dw1 := cases[2]
	world := dw1.World
	key := world.Dispatch.Issue.Key
	if len(world.unserved) != 0 || len(teardownEffectsSeedFaults(world)) != 0 {
		t.Fatalf("DW1's world: unserved %v, faults %v", world.unserved, teardownEffectsSeedFaults(world))
	}
	ids := map[string]string{}
	for i, comment := range world.Dispatch.Comments {
		ids["comment/"+comment.ID] = fmt.Sprintf("seeded-comment-%d", i)
	}
	for i, ask := range world.Dispatch.Asks {
		ids["ask/"+ask.ID] = fmt.Sprintf("seeded-ask-%d", i)
	}
	held := []string{key}
	for _, other := range world.Dispatch.Issues {
		held = append(held, other.Key)
	}
	shapes := map[string]bool{}
	made := map[string][]teardownEffectsCommand{}
	for _, name := range teardownEffectsTraces(dw1) {
		for _, a := range dw1.trace(name).Actions {
			issue := "the case's issue"
			if a.Target != key {
				issue = "another issue"
			}
			shapes[fmt.Sprintf("%s %s on %s", a.Command, strings.Join(slices.Sorted(maps.Keys(a.Fields)), ","), issue)] = true
			c, err := teardownEffectsTraceCommand(key, held, a, ids)
			if err != nil {
				t.Fatalf("DW1's %s trace, action %d: %v", name, a.Seq, err)
			}
			if c.Name != "" {
				ids["comment/"+c.Name] = "made-" + c.Name
			}
			made[name] = append(made[name], c)
		}
	}
	for _, shape := range []string{
		"comment body on the case's issue",
		"comment body,reply_to on the case's issue",
		"comment ask_id,body,turn on the case's issue",
		"comment body,id on the case's issue",
		"message body on the case's issue",
		"issue-update status on the case's issue",
		"issue-update status on another issue",
		"issue-update before on another issue",
		"artifact content,name,summary,version on the case's issue",
		"artifact content,force,name,summary,version on the case's issue",
		"doc-edit artifact,ops,summary,version on the case's issue",
		"doc-edit ops,summary on another issue",
		"plan-approve version on the case's issue",
		"request-approval include_verification,summary on the case's issue",
		"request-approval artifact,summary on the case's issue",
	} {
		if !shapes[shape] {
			t.Errorf("DW1's traces hold no write of the shape %s; they hold %v", shape, slices.Sorted(maps.Keys(shapes)))
		}
	}
	good, bad := made["good"], made["bad"]
	for _, mapped := range []struct{ what, got, want string }{
		{"the reply's comment", good[0].flag("reply-to"), ids["comment/c-scope"]},
		{"the answer's question", good[1].flag("reply-to-ask"), ids["ask/a-window"]},
		{"the reply's link to the world's comment", good[0].flag("body"), "dispatch://" + key + "/comment/" + ids["comment/c-scope"]},
		{"the message's link to the comment the trace named", good[5].flag("body"), "dispatch://" + key + "/comment/made-note-1"},
		{"the plan's bare serves", bad[2].flag("content"), `serves="` + specBlock(key, "r1") + `"`},
		{"the plan's bare needs", bad[4].flag("content"), `needs="dispatch://` + key + `/artifact/plan-md#b-t1"`},
	} {
		if !strings.Contains(mapped.got, mapped.want) {
			t.Errorf("%s is %q, want it to hold %q", mapped.what, mapped.got, mapped.want)
		}
	}
	if good[2].Version != 1 || good[3].Version != 2 || good[4].Name != "note-1" || good[6].Issue != "ACME-32" || bad[5].Issue != "ACME-32" {
		t.Fatalf("DW1's good trace makes %+v and its bad one %+v", good, bad)
	}
	// The body the good trace states is its upload's text with its edit made, at the edit's version;
	// a body otherwise is a fault that names its first line that differs, and the bad trace states
	// no document, so its replay checks none.
	upload, edit := dw1.Traces.Good.Actions[2], dw1.Traces.Good.Actions[3]
	op, _ := edit.Fields["ops"].([]any)[0].(map[string]any)
	body := strings.Replace(teardownEffectsString(upload.Fields, "content"), teardownEffectsString(op, "find"), teardownEffectsString(op, "with"), 1)
	stated := dw1.Traces.Good.Documents
	if want := []teardownEffectsDocument{{Name: teardownEffectsString(upload.Fields, "name"), Version: good[3].Version, Body: body}}; !reflect.DeepEqual(stated, want) {
		t.Fatalf("DW1's good trace states %+v, and its writes leave %+v", stated, want)
	}
	written := []string{stated[0].Name}
	if faults := teardownEffectsDocumentFaults(stated, map[string]teardownEffectsDocument{stated[0].Name: stated[0]}, written, nil); len(faults) != 0 {
		t.Fatalf("the stated document held as stated reads as faults %v", faults)
	}
	unedited := teardownEffectsDocument{Name: stated[0].Name, Version: stated[0].Version, Body: teardownEffectsString(upload.Fields, "content")}
	if faults := teardownEffectsDocumentFaults(stated, map[string]teardownEffectsDocument{stated[0].Name: unedited}, written, nil); len(faults) != 1 || !strings.Contains(faults[0], `line 3 is "The first stage runs on three hosts."`) {
		t.Fatalf("the stated document held without its edit reads as faults %v, want its third line named", faults)
	}
	if dw1.Traces.Bad.Documents != nil {
		t.Fatalf("DW1's bad trace states the documents %+v, want none", dw1.Traces.Bad.Documents)
	}
}
