// going_tool_rules_test.go holds the bar's check of a call to what the call's tool spec states:
// each bound of its schema, and the cross-field rule its spec states beside its arguments, ported
// to Go once per write tool the generator marks (going_tool_rules.go).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestEveryWriteToolsRuleIsPortedOnce: every write tool whose spec states a cross-field rule, as
// the generator marks it (contracts.DispatchTool.Validation), has a Go port of that rule, and no
// other tool has one, so a rule a spec gains or loses fails here until the Go follows. A read
// tool's rule is not ported: the bar holds no read to its tool's schema. Each port keeps its own
// spec's example.
func TestEveryWriteToolsRuleIsPortedOnce(t *testing.T) {
	t.Parallel()
	stated := map[string]contracts.DispatchTool{}
	for _, tool := range contracts.DispatchTools {
		stated[tool.Name] = tool
		_, writes := goingWriteTools[tool.Name]
		_, ported := goingToolRules[tool.Name]
		switch {
		case writes && tool.Validation != "" && !ported:
			t.Errorf("%s states the rule %q, and goingToolRules holds no port of it", tool.Name, tool.Validation)
		case ported && tool.Validation == "":
			t.Errorf("goingToolRules ports a rule of %s, whose spec states none", tool.Name)
		case ported && !writes:
			t.Errorf("goingToolRules ports a rule of %s, which is no write tool of the bar", tool.Name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(goingToolRules)) {
		tool, held := stated[name]
		if !held {
			t.Errorf("goingToolRules ports a rule of %s, which no tool spec names", name)
			continue
		}
		var example map[string]any
		if err := json.Unmarshal(tool.Example, &example); err != nil {
			t.Fatalf("%s's example %s: %v", name, tool.Example, err)
		}
		if refusal := goingToolRules[name](example, tool.Validation); refusal != "" {
			t.Errorf("%s's port refuses its spec's own example %s: %s", name, tool.Example, refusal)
		}
	}
}

// goingRuleBreaks is, for each write tool whose rule is ported, a call its schema takes and the bar
// can send that breaks the rule, with the refusal its spec answers it with: the rule's own words
// where the spec words no refusal of its own ("" here), and otherwise the spec's refusal of the
// part the call broke.
var goingRuleBreaks = map[string]struct {
	call    map[string]any
	refusal string
}{
	"dispatch_issue_update": {map[string]any{"issue": "DSP-1"}, goingIssueUpdateFieldsRule},
	"dispatch_ask":          {map[string]any{"question": "Ship it on Friday?"}, ""},
	"dispatch_edit_ask":     {map[string]any{"ask": "01234567-0000-4000-8000-000000000001"}, ""},
	"dispatch_comment": {map[string]any{"issue": "DSP-1", "project": "DSP", "body": "Reads well.", "turn": "human", "mentions": []any{"planner"}},
		"Exactly one of issue and project is required; with project, artifact names the document. " +
			"turn requires reply_to_ask: turn says who holds an open ask's thread after the reply. Pass reply_to_ask with the open ask's id, or drop turn. " +
			"mention target must be role:<name> or session:<id>"},
	"dispatch_suggest": {map[string]any{"issue": "DSP-1", "quote": "old wording", "replace_with": "new wording"}, ""},
	"dispatch_message": {map[string]any{"body": "Where are we?"}, ""},
	"dispatch_doc_edit": {map[string]any{"issue": "DSP-1", "ops": []any{map[string]any{"op": "replace", "find": "old", "markdown": "new"}}, "precondition": map[string]any{}},
		"A precondition selects exactly one of document or blocks. " +
			"ops[0] is a replace carrying markdown, which replace does not take: it takes find, with and occurrence, and its new text goes in with. " +
			`ops[0] is a replace with no with, which would delete the text find matches: write the new text in with, or with "" to delete it on purpose.`},
	"dispatch_request_approval": {map[string]any{"artifact": "01234567-0000-4000-8000-000000000001", "summary": "Ready for review."}, ""},
	"dispatch_review":           {map[string]any{"issue": "DSP-1", "version": 2, "verdict": "changes_requested"}, ""},
	"dispatch_result": {map[string]any{"retract": "01234567-0000-4000-8000-000000000001", "reason": "Recorded at the wrong commit.", "issue": "DSP-1"},
		"retract takes reason alone, and this call also carries issue. Retract the result, then record a new one with a call of its own."},
	"dispatch_deploy": {map[string]any{"repo": "widgets", "program": "worker", "commit": strings.Repeat("a", 40)}, "repo must be owner/repo"},
	"dispatch_artifact": {map[string]any{"issue": "DSP-1", "name": "rows.jsonl", "content": "{}"},
		`a document holds Markdown, which rewrites other text (a JSONL file's lines come back joined into one paragraph), and "rows.jsonl" is not a Markdown name. ` +
			"Upload the file under the same name with path (--path <file>), which stores a file whose path does not end in .md or .markdown as its bytes, unchanged; " +
			"a document takes a name ending in .md or .markdown or with no extension."},
	"dispatch_architecture_sync": {map[string]any{"project": "CORE", "repo": "acme/.."}, ""},
	"dispatch_handoff_write": {map[string]any{"issue": "DSP-1", "phase": "implement", "body": "[1]"},
		`body is not one JSON object: a handoff body is a JSON object such as {"schemaVersion":1}`},
	"dispatch_create_priority": {map[string]any{"title": strings.Repeat("t", priorityIDMax+1)}, ""},
	"dispatch_edit_priority":   {map[string]any{"priority": "ship-the-command-bar"}, "name at least one of title, description, owner and rank."},
	"dispatch_mark_inbox_read": {map[string]any{"read_through": "2026-11-19T00:00:00Z", "issue": "DSP-1", "artifact": "notes"}, ""},
	"dispatch_dismiss_inbox": {map[string]any{"threads": []any{"reply:0190c7d2-1111-7222-8333-444444444444"}, "restore": true,
		"seen_replies": map[string]any{"reply:0190c7d2-1111-7222-8333-444444444444": map[string]any{"version": 3}}}, "a restore takes no seen_replies."},
	"dispatch_answer_ask": {map[string]any{"ask": "01234567-0000-4000-8000-000000000001", "expected_edited_at": nil}, ""},
	"dispatch_reply_comment_delivery": {map[string]any{"comment": "0190c7d2-1111-7222-8333-444444444444", "attempt": 1, "body": "On it.", "error": "Busy."},
		"name exactly one of body and error."},
	"dispatch_review_document": {map[string]any{"document": "0190c7d2-1111-7222-8333-444444444444", "state": "changes_requested", "reason": "  "}, ""},
	"dispatch_set_agent_state": {map[string]any{"session": "0190c7d2-1111-7222-8333-444444444444", "read_replies": []any{}}, ""},
}

// TestAPlanRowBreakingItsToolsRuleIsRefusedBeforeAnyRequest: for each write tool whose rule is
// ported, one call its schema takes that breaks the rule is refused by the check a plan's row is
// held to when it is proposed, approved and run (GoingCaller.prepare), in the words its spec
// refuses it with, and nothing is sent, read or write. Before, the bar held a call to its schema
// alone, so such a row was approved and its route answered 400 when it ran: a replace with no
// with is 400 REPLACE_FIELDS, a restore that carries seen_replies 400 at the dismissal.
func TestAPlanRowBreakingItsToolsRuleIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	if broken, ported := slices.Sorted(maps.Keys(goingRuleBreaks)), slices.Sorted(maps.Keys(goingToolRules)); !slices.Equal(broken, ported) {
		t.Fatalf("goingRuleBreaks breaks the rules of %v, want one call of each ported rule %v", broken, ported)
	}
	for _, name := range slices.Sorted(maps.Keys(goingRuleBreaks)) {
		t.Run(name, func(t *testing.T) {
			breaks := goingRuleBreaks[name]
			action, held := catalogue.Lookup(name)
			if !held {
				t.Fatalf("the catalogue holds no %s", name)
			}
			if refusal := checkGoingArguments(action.Request, breaks.call); refusal != "" {
				t.Fatalf("the schema refuses the call %v (%s), want it taken so its rule is what refuses it", breaks.call, refusal)
			}
			want := breaks.refusal
			if want == "" {
				want = action.rule
			}
			doer := &countingDoer{}
			_, err := GoingCaller{Catalogue: catalogue, Session: goingPlanner, Doer: doer}.Do(context.Background(), name, breaks.call)
			var refused GoingArgumentError
			if !errors.As(err, &refused) || refused.Reason != want {
				t.Fatalf("the call %v answered %v, want its rule's refusal %q", breaks.call, err, want)
			}
			if len(doer.requests) != 0 {
				t.Fatalf("the refused call sent %d request(s), want none", len(doer.requests))
			}
		})
	}
}

// TestEachPortedRuleRefusesThePartACallBreaks: the parts of the ported rules the table above does
// not break, each refused in its spec's words and the call that keeps it taken; and the bar's two
// departures: a close with no reason, which the bar sends with none (barTool), and an upload
// naming no owner, a file of the calling session's own conversation, which takes any name.
func TestEachPortedRuleRefusesThePartACallBreaks(t *testing.T) {
	t.Parallel()
	ruleOf := map[string]string{}
	for _, tool := range contracts.DispatchTools {
		ruleOf[tool.Name] = tool.Validation
	}
	for _, tc := range []struct {
		name, tool string
		call       map[string]any
		refusal    string
	}{
		{"a close with no reason", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "status": "done"}, ""},
		{"a close with a blank reason", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "status": "done", "reason": " "},
			goingIssueUpdateDoneRule + "."},
		{"a reason beside no close", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "status": "todo", "reason": "Why."},
			"reason goes only with status done: it is the note posted on the issue as it closes, and status todo closes nothing. Drop reason, or set status done to close the issue."},
		{"an update that clears the priority", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "priority": nil}, ""},
		{"labels beside labels_add", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "labels": []any{"board"}, "labels_add": []any{"api"}},
			goingIssueUpdateLabelsRule},
		{"an update that adds a label alone", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "labels_add": []any{"api"}}, ""},
		{"an option set to false asks for nothing", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "include_children": false, "title": "A title"}, ""},
		{"an option beside a status alone", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "status": "todo", "include_children": true}, ""},
		{"an option beside a close and its reason", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "done", "reason": "Why.", "include_parent": true}, ""},
		{"unlink_children beside a close and its reason", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "done", "reason": "Why.", "unlink_children": true}, ""},
		{"a close of a blank reason beside an option", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "done", "reason": " ", "unlink_children": true}, goingIssueUpdateDoneRule + "."},
		{"include_children beside unlink_children", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "todo", "include_children": true, "unlink_children": true},
			"include_children does not go with unlink_children. The open children move with the issue or leave it, so name one of the two."},
		{"include_children beside a close", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "done", "reason": "Why.", "include_children": true},
			"include_children does not go with status done. Only a person who is signed in at a browser can close the open issues under an issue together with it, " +
				"and this call presents a token. That person does it on the issue's page in Dispatch: Close issue, then Move them with it."},
		{"an option with no status", "dispatch_issue_update", map[string]any{"issue": "DSP-1", "include_parent": true},
			"status is required with include_parent. Add status, or drop include_parent."},
		{"an option beside fields of its own", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "todo", "unlink_children": true, "title": "A title", "priority": nil},
			"With unlink_children, give status alone, and reason when the status is done. This call also names title, priority. Send those fields in a call of their own."},
		{"two options named together", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "todo", "include_children": true, "include_parent": true, "title": "A title"},
			"With include_children and include_parent, give status alone, and reason when the status is done. This call also names title. Send those fields in a call of their own."},
		{"three options named together", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "todo", "include_children": true, "unlink_children": true, "include_parent": true, "title": "A title"},
			"include_children does not go with unlink_children. The open children move with the issue or leave it, so name one of the two. " +
				"With include_children, unlink_children and include_parent, give status alone, and reason when the status is done. This call also names title. Send those fields in a call of their own."},
		{"every part a call breaks, in the rule's order", "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "todo", "reason": "Why.", "labels": []any{"board"}, "labels_add": []any{"api"}, "include_parent": true, "title": "A title"},
			"reason goes only with status done: it is the note posted on the issue as it closes, and status todo closes nothing. Drop reason, or set status done to close the issue. " +
				goingIssueUpdateLabelsRule + " " +
				"With include_parent, give status alone, and reason when the status is done. This call also names title, labels, labels_add. Send those fields in a call of their own."},
		{"mentions past their count", "dispatch_comment", map[string]any{"issue": "DSP-1", "body": "Hi.", "mentions": []any{
			"role:a", "role:b", "role:c", "role:d", "role:e", "role:f", "role:g", "role:h", "role:i", "role:j", "role:k"}}, goingCommentMentionLimitRule},
		{"a mention named twice", "dispatch_comment", map[string]any{"issue": "DSP-1", "body": "Hi.", "mentions": []any{"session:s-1", "session:s-1"}},
			goingCommentMentionRepeatRule},
		{"a turn beside the ask it answers", "dispatch_comment", map[string]any{"issue": "DSP-1", "body": "Hi.", "turn": "human", "reply_to_ask": "a-1"}, ""},
		{"an edit naming no owner", "dispatch_doc_edit", map[string]any{"artifact": "01234567-0000-4000-8000-000000000001", "ops": []any{
			map[string]any{"op": "replace", "find": "old", "with": "new"}}}, goingOwnerRule},
		{"an edit with a precondition of both tokens", "dispatch_doc_edit", map[string]any{"issue": "DSP-1", "precondition": map[string]any{
			"document": "sha256:1", "blocks": []any{map[string]any{"id": "b", "token": "t"}}}, "ops": []any{}}, goingPreconditionRule},
		{"an edit whose replace deletes on purpose", "dispatch_doc_edit", map[string]any{"issue": "DSP-1", "ops": []any{
			map[string]any{"op": "replace", "find": "old", "with": ""}}}, ""},
		{"a retraction with no reason", "dispatch_result", map[string]any{"retract": "r-1"}, "retract needs reason, the words that say why the result is wrong."},
		{"a reason with no retraction", "dispatch_result", map[string]any{"issue": "DSP-1", "reason": "Why."},
			"reason goes with retract, and this call carries no retract. Name the result to retract, or drop reason."},
		{"a record missing its fields", "dispatch_result", map[string]any{"issue": "DSP-1", "check": "k1"},
			"recording a result needs role, verdict, commit. " + goingResultRetractRule},
		{"an upload of path and content both", "dispatch_artifact", map[string]any{"issue": "DSP-1", "name": "notes.md", "path": "", "content": "# Notes\n"},
			goingArtifactSourceRule},
		{"an upload naming no extension", "dispatch_artifact", map[string]any{"project": "DSP", "name": "README", "content": "Words."}, ""},
		{"an upload to the calling session's conversation", "dispatch_artifact", map[string]any{"name": "notes.txt", "content": "Plain words."}, ""},
		{"a handoff of no phase", "dispatch_handoff_write", map[string]any{"issue": "DSP-1", "phase": "Implement", "body": "{}"}, goingHandoffPhaseRule},
		{"a sync naming a whole repository", "dispatch_architecture_sync", map[string]any{"project": "CORE", "repo": "mona-cat_octo/infra.dev"}, ""},
		{"a long title naming its id", "dispatch_create_priority", map[string]any{"title": strings.Repeat("t", priorityIDMax+1), "id": "long"}, ""},
		{"a rank naming neither neighbour", "dispatch_edit_priority", map[string]any{"priority": "p", "rank": map[string]any{"before": " "}},
			"rank names before, after, or both."},
		{"a dismissal of named rows with nothing seen", "dispatch_dismiss_inbox", map[string]any{"threads": []any{"reply:r"}},
			"taking rows named by threads out carries seen_replies, what was seen of each."},
		{"a dismissal of named rows narrowed too", "dispatch_dismiss_inbox", map[string]any{"threads": []any{"reply:r"}, "kind": []any{"reply"}},
			"threads names the rows itself, so it takes neither dismiss_through nor the narrowing."},
		{"seen replies with no rows", "dispatch_dismiss_inbox", map[string]any{"seen_replies": map[string]any{}}, "seen_replies goes with threads alone."},
		{"a narrowing with no time", "dispatch_dismiss_inbox", map[string]any{"kind": []any{"reply"}}, "dismiss_through is required when threads names no row."},
		{"a narrowing of both owners", "dispatch_dismiss_inbox", map[string]any{"dismiss_through": "2026-11-19T00:00:00Z", "issue": "DSP-1", "artifact": "a"},
			goingInboxOwnerRule},
		{"a blank reply", "dispatch_reply_comment_delivery", map[string]any{"comment": "c", "attempt": 1, "body": " "}, "body is blank."},
		{"an error answering a block", "dispatch_reply_comment_delivery", map[string]any{"comment": "c", "attempt": 1, "error": "Busy.", "reply_to_block": "b"},
			"reply_to_block names the block a body answers, and an error has none."},
		{"a deploy of a program in capitals", "dispatch_deploy", map[string]any{"repo": "acme/widgets", "program": "Worker", "commit": strings.Repeat("a", 40)},
			"program must be 1 to 64 lowercase letters, digits or hyphens"},
		{"a deploy of a short commit", "dispatch_deploy", map[string]any{"repo": "acme/widgets", "program": "worker", "commit": "abc1234"},
			"commit must be 40 lowercase hexadecimal characters"},
		{"a deploy at a time with no zone", "dispatch_deploy",
			map[string]any{"repo": "acme/widgets", "program": "worker", "commit": strings.Repeat("a", 40), "deployed_at": "2026-11-20T10:00:00"},
			"deployed_at must be RFC3339"},
		{"a deploy in a month no year has", "dispatch_deploy",
			map[string]any{"repo": "acme/widgets", "program": "worker", "commit": strings.Repeat("a", 40), "deployed_at": "2026-13-01T10:00:00Z"},
			"deployed_at must be RFC3339"},
		{"a deploy on a day its month has not", "dispatch_deploy",
			map[string]any{"repo": "acme/widgets", "program": "worker", "commit": strings.Repeat("a", 40), "deployed_at": "2026-02-30T10:00:00Z"},
			"deployed_at must be a valid calendar date"},
		{"a deploy an hour ahead", "dispatch_deploy",
			map[string]any{"repo": "acme/widgets", "program": "worker", "commit": strings.Repeat("a", 40), "deployed_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
			"deployed_at must not be more than five minutes in the future"},
		{"a deploy at a past time in an offset, with its URL", "dispatch_deploy",
			map[string]any{"repo": "acme/widgets", "program": "worker", "commit": strings.Repeat("a", 40), "deployed_at": "2026-02-28T23:30:00.25+01:00",
				"url": "https://widgets.example.com"},
			""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goingToolRules[tc.tool](tc.call, ruleOf[tc.tool]); got != tc.refusal {
				t.Fatalf("%s's rule answered %q for %v, want %q", tc.tool, got, tc.call, tc.refusal)
			}
		})
	}
}

// TestThePlanCheckHoldsEachBoundItsSchemaStates: a value outside a bound its field states is
// refused naming the field and the bound - a text's length in UTF-16 units, as its spec counts
// it, a list's count of items and a number's value, at any depth - and a value at the bound is
// taken. Before, the check read no bound, so a session group of no session was approved and its
// route answered 400.
func TestThePlanCheckHoldsEachBoundItsSchemaStates(t *testing.T) {
	t.Parallel()
	bound := func(min, max float64) (*float64, *float64) { return &min, &max }
	textMin, textMax := bound(1, 3)
	listMin, listMax := bound(1, 2)
	numberMin, numberMax := bound(0, 3)
	fields := []contracts.ToolField{
		{Name: "title", Kind: contracts.ToolFieldString, Min: textMin, Max: textMax},
		{Name: "labels", Kind: contracts.ToolFieldArray, Min: listMin, Max: listMax,
			Item: &contracts.ToolField{Kind: contracts.ToolFieldString, Required: true, Min: textMin}},
		{Name: "priority", Kind: contracts.ToolFieldNumber, Min: numberMin, Max: numberMax},
	}
	for _, tc := range []struct {
		name      string
		arguments map[string]any
		refusal   string
	}{
		{"a text at its bounds", map[string]any{"title": "a"}, ""},
		{"a text whose emoji counts two units, at its most", map[string]any{"title": "\U0001F600a"}, ""},
		{"a text under its least", map[string]any{"title": ""}, `argument "title" is 0 characters long, and must be at least 1`},
		{"a text over its most in UTF-16 units", map[string]any{"title": "\U0001F600\U0001F600"}, `argument "title" is 1 characters over the 3-character limit (4/3)`},
		{"a list at its bounds", map[string]any{"labels": []any{"a", "b"}}, ""},
		{"a list under its least", map[string]any{"labels": []any{}}, `argument "labels" holds 0 items, and must hold at least 1`},
		{"a list over its most", map[string]any{"labels": []any{"a", "b", "c"}}, `argument "labels" holds 3 items, and may hold at most 2`},
		{"an item of a list under its least", map[string]any{"labels": []any{""}}, `argument "labels" item 0 is 0 characters long, and must be at least 1`},
		{"a number at its bounds", map[string]any{"priority": 3.0}, ""},
		{"a number under its least", map[string]any{"priority": -1}, `argument "priority" is -1, and must be at least 0`},
		{"a number over its most", map[string]any{"priority": 3.5}, `argument "priority" is 3.5, and may be at most 3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkGoingArguments(fields, tc.arguments); got != tc.refusal {
				t.Fatalf("the check answered %q for %v, want %q", got, tc.arguments, tc.refusal)
			}
		})
	}

	t.Run("a session group of no session is refused before any request", func(t *testing.T) {
		catalogue, err := BuildGoingCatalogue()
		if err != nil {
			t.Fatal(err)
		}
		doer := &countingDoer{}
		_, err = GoingCaller{Catalogue: catalogue, Doer: doer}.Do(context.Background(), "dispatch_create_session_group",
			map[string]any{"name": "Release train", "sessions": []any{}})
		const want = `argument "sessions" holds 0 items, and must hold at least 1`
		var refused GoingArgumentError
		if !errors.As(err, &refused) || refused.Reason != want || len(doer.requests) != 0 {
			t.Fatalf("the group of no session answered %v after %d request(s), want %q and none", err, len(doer.requests), want)
		}
	})
}
