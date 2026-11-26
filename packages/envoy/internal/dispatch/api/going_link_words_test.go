package api

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// TestAnAnswerIsStoredWithTheWordsOfItsLinks: the bar draws each link of an answer to an item as
// the item's chip in place of the link's words, and the eval's answers lost what they wrote as
// those words (c13 at 9ae5605b): a session's name written as the words of a link to its issue, and
// a comment's words past the head the bar cuts its text to. The server stores the reply frame of
// an answer with such words written beside the link, which then reads as the item's short form,
// and every other field of the frame as the agent sent it. Words the chip draws - the issue's key,
// its title, a run of the comment's head - stay a link, as do a time and a web address; and a
// reply frame of tool calls, whose text the bar writes as plain words over its calls, is stored
// as it came. Each item is read as the run's person may see it: a comment reserved for another
// author is one the server reads nothing of, so the words of a link to it stay beside it, though
// they are the comment's own.
func TestAnAnswerIsStoredWithTheWordsOfItsLinks(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	issue := rig.issue(t, "Rotate the signing keys")
	commentRef := "dispatch://" + issue + "/comment/" + createThreadComment(t, rig.mux, issue, map[string]any{
		"body": "p95 has stayed under 200 ms since the cache change landed on Monday, and the error rate is flat.",
	}, "alice").ID
	messageRef := "dispatch://" + issue + "/message/" + createIssueMessage(t, rig.mux, issue, map[string]any{
		"body": "Pushed the rotation script; the staging run is green.",
	}, "alice").ID

	t.Run("an answer's links keep the words their chips do not draw", func(t *testing.T) {
		run, token := rig.working(t, "alice", "who works on "+issue)
		header := map[string]any{"step": 1, "text": "Reading [Implementer of " + issue + "](dispatch://" + issue + ")…",
			"tools": []string{"dispatch_read"}, "labels": []string{"Reading " + issue}, "waits_for_person": false, "model_ms": 40}
		answer := map[string]any{"step": 3, "tools": []string{}, "labels": []string{}, "waits_for_person": false, "model_ms": 120,
			"text": "[Implementer of " + issue + "](dispatch://" + issue + ") is unresponsive; bob@example.com wrote " +
				"[p95 has stayed under 200 ms since the cache change landed on Monday](" + commentRef + "), and " +
				"[Implementer of " + issue + " says the staging run is green](" + messageRef + "). " +
				"[" + issue + "](dispatch://" + issue + "), [Rotate the signing keys](/issues/" + issue + "), " +
				"[p95 has stayed under 200 ms](" + commentRef + "), [5 days ago](dispatch-time:2026-11-18T11:35:00Z) and " +
				"[the runbook](https://dispatch.example/issues/" + issue + ") stay as written."}
		storeReply(t, rig, run, token, 1, header)
		storeReply(t, rig, run, token, 3, answer)
		want := map[string]any{}
		for field, value := range answer {
			want[field] = value
		}
		want["text"] = "Implementer of " + issue + " ([" + issue + "](dispatch://" + issue + ")) is unresponsive; bob@example.com wrote " +
			"p95 has stayed under 200 ms since the cache change landed on Monday ([" + issue + " comment](" + commentRef + ")), and " +
			"Implementer of " + issue + " says the staging run is green ([" + issue + " message](" + messageRef + ")). " +
			"[" + issue + "](dispatch://" + issue + "), [Rotate the signing keys](/issues/" + issue + "), " +
			"[p95 has stayed under 200 ms](" + commentRef + "), [5 days ago](dispatch-time:2026-11-18T11:35:00Z) and " +
			"[the runbook](https://dispatch.example/issues/" + issue + ") stay as written."
		requireStoredReplies(t, rig, run, header, want)
	})

	t.Run("a link to a comment the person may not see keeps its words", func(t *testing.T) {
		private := createThreadComment(t, rig.mux, issue, map[string]any{"body": "A private remark about the vault."}, "bob").ID
		if reserved := rig.count(t, `
			with reserved as (update graph_nodes set visibility = 'author' where kind = 'comment' and id = $1 returning 1)
			select count(*) from reserved
		`, private); reserved != 1 {
			t.Fatalf("reserved %d graph nodes of comment %s for its author, want its one", reserved, private)
		}
		privateRef := "dispatch://" + issue + "/comment/" + private
		run, token := rig.working(t, "alice", "what did bob say on "+issue)
		answer := map[string]any{"step": 1, "tools": []string{}, "labels": []string{}, "waits_for_person": false, "model_ms": 90,
			"text": "bob wrote [A private remark about the vault.](" + privateRef + ") on [" + issue + "](dispatch://" + issue + ")."}
		storeReply(t, rig, run, token, 1, answer)
		want := map[string]any{}
		for field, value := range answer {
			want[field] = value
		}
		want["text"] = "bob wrote A private remark about the vault. ([" + issue + " comment](" + privateRef + ")) on [" + issue + "](dispatch://" + issue + ")."
		requireStoredReplies(t, rig, run, want)
	})
}

// TestAnAnswerIsStoredWithTheBareKeysItsReadsReturnedLinked: the preview at e2b341f7 answered
// "DEMO-21 is a P3 backlog feature with no assignee" with DEMO-21 as text, not a link, so the bar
// drew no chip of it and no one read its title. The server stores the reply frame of an answer
// with each issue key it names bare that a tool result of the run holds written as the issue's
// link, and every other field as the agent sent it. A key no read of the run returned stays text,
// though its issue is real, so the answer links only items its run read; and a word of a key's
// shape that a read returned but that names no issue, SHA-256 or UTF-8, stays text, since a link
// to it would draw a chip of no issue.
func TestAnAnswerIsStoredWithTheBareKeysItsReadsReturnedLinked(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	read := rig.issue(t, "Export: CSV and PDF")
	unread := rig.issue(t, "Rotate the signing keys")
	run, token := rig.working(t, "alice", "what is "+read)
	body, err := json.Marshal(map[string]any{
		"call":  map[string]any{"id": "call_1", "tool": "read", "arguments": map[string]any{"ref": "dispatch://" + read}},
		"error": false,
		"result": `{"key": "` + read + `", "title": "Export: CSV and PDF", "status": "backlog", "priority_shown": "P3", "assignee": null, ` +
			`"spec": "Verify the SHA-256 digest and write the file as UTF-8."}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client.Step(context.Background(), agentwire.StepRequest{Run: run, LeaseToken: token, Step: 1,
		Kind: agentwire.StepTool, Body: body, Note: "read an issue"}); err != nil {
		t.Fatalf("save the tool step of run %s: %v", run, err)
	}
	answer := map[string]any{"step": 2, "tools": []string{}, "labels": []string{}, "waits_for_person": false, "model_ms": 80,
		"text": read + " is a P3 backlog feature with no assignee, and " + unread + " waits on it. It asks for a SHA-256 digest and UTF-8."}
	storeReply(t, rig, run, token, 2, answer)
	want := map[string]any{}
	for field, value := range answer {
		want[field] = value
	}
	want["text"] = "[" + read + "](dispatch://" + read + ") is a P3 backlog feature with no assignee, and " + unread +
		" waits on it. It asks for a SHA-256 digest and UTF-8."
	requireStoredReplies(t, rig, run, want)
}

// TestAnAnswerIsStoredWithoutTheBlocksItsRunDrew: in 1 of 10 tries of "Chart the DEMO project's
// issues by status." at 9fc43c69 the answer wrote the block draw_block drew, its JSON in a code
// block whose info string was draw, and the bar drew it as a box of code above the chart itself.
// The server stores the answer of a run without each fenced code block that holds a block the run
// drew, its spacing aside, or whose info string is draw; every other code block, JSON or not,
// stays as it came, byte for byte. The words call counts the answer as it is stored, both blocks
// left out, which the answer's frame says.
func TestAnAnswerIsStoredWithoutTheBlocksItsRunDrew(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	loop := newGoingLoop(t, rig)
	const words = "chart how many issues DEMO holds, and show the query"
	const kept = "```json\n{\"status\": \"todo\"}\n```\n\nThe query:\n\n```sql\nselect count(*) from issues;\n```\nThat is all."
	loop.script.set(words, func(n int) agenttest.Reply {
		switch n {
		case 1:
			return toolCall("call_tool", map[string]any{"tool": "dispatch_issues", "arguments": map[string]any{"project": "DEMO"}})
		case 2:
			return toolCall("call_tool", map[string]any{"tool": "draw_block", "arguments": map[string]any{
				"kind": "metric", "title": "The issues of DEMO", "steps": []map[string]any{{"step": 2, "call": 1}},
				"chain": []map[string]any{{"op": "count"}}, "field": "count",
			}})
		}
		// The answer writes the block step 4 drew again, indented, and a block for the bar to
		// draw, before a JSON block and a block of code of its own.
		var body []byte
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select s.body from going_run_steps s join going_runs r on r.id = s.run_id where r.words = $1 and s.step = 4
		`, words).Scan(&body); err != nil {
			t.Errorf("read the block step: %v", err)
		}
		block, drew := agent.DrawnBlock(body)
		var indented bytes.Buffer
		if !drew || json.Indent(&indented, block, "", "  ") != nil {
			t.Errorf("step 4 holds no block the run drew: %s", body)
		}
		return answered("DEMO's issues:\n\n```json\n" + indented.String() + "\n```\n\n```draw\n{\"kind\":\"metric\",\"title\":\"The issues of DEMO\"}\n```\n\n" + kept)
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	if record := rig.ended(t, run, time.Minute); record.state != "done" {
		t.Fatalf("the run ended %s with %q, want done", record.state, record.reason)
	}
	var data []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select data from going_run_events where run_id = $1::uuid and name = $2 order by sequence desc limit 1
	`, run, goingReplyFrame).Scan(&data); err != nil {
		t.Fatalf("read the answer's frame: %v", err)
	}
	var answer struct {
		Text  string `json:"text"`
		Drawn *int   `json:"drawn_words"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatal(err)
	}
	if want := "DEMO's issues:\n\n\n\n" + kept; answer.Text != want {
		t.Errorf("the answer is stored as\n%q\nwant\n%q", answer.Text, want)
	}
	// DEMO's issues: ```json {"status": "todo"} The query: ```sql select count(*) from issues; That
	// is all. The fences alone, ``` and ```, are no words.
	if answer.Drawn == nil || *answer.Drawn != 15 {
		t.Errorf("the answer's frame says %v drawn words, want the 15 of the answer as stored", answer.Drawn)
	}
}

// storeReply saves step of run, a model step, with its one reply frame, reply.
func storeReply(t *testing.T, rig goingAgentRig, run, token string, step int, reply map[string]any) {
	t.Helper()
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client.Step(context.Background(), agentwire.StepRequest{Run: run, LeaseToken: token, Step: step,
		Kind: agentwire.StepModel, Body: json.RawMessage(`{}`), Note: "a reply",
		Frames: []agentwire.Frame{{Name: goingReplyFrame, Data: data}}}); err != nil {
		t.Fatalf("save step %d of run %s: %v", step, run, err)
	}
}

// requireStoredReplies fails t unless the reply frames of run's events are want, in order, each
// read as JSON.
func requireStoredReplies(t *testing.T, rig goingAgentRig, run string, want ...map[string]any) {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `select data from going_run_events where run_id = $1::uuid and name = 'reply' order by sequence`, run)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) {
		var data []byte
		var frame map[string]any
		if err := row.Scan(&data); err != nil {
			return nil, err
		}
		return frame, json.Unmarshal(data, &frame)
	})
	if err != nil {
		t.Fatalf("read the reply frames of run %s: %v", run, err)
	}
	// Read through JSON, so each wanted number and list is what a stored one decodes to.
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var wanted []map[string]any
	if err := json.Unmarshal(encoded, &wanted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, wanted) {
		t.Errorf("run %s stored the reply frames\n%v\nwant\n%v", run, stored, wanted)
	}
}
