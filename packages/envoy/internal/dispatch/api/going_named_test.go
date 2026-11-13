// going_named_test.go holds which invocations of a proposal run at once to what the person's own
// typed words name: an item is named only by a reference to it as Dispatch reads references in
// text, a document only under an owner so named, and a hand-over of the work - its route, its
// assignee - only where the words name it or the issue already holds it.
package api

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// goingAutomaticOf is whether the one invocation the answer wrote for action, as the proposal of a
// run typed as words marks it: about the page's DEMO-12 where onPage, about the items the words
// name otherwise.
func goingAutomaticOf(t *testing.T, words string, onPage bool, action string, arguments map[string]any, held goingHeld) bool {
	t.Helper()
	written, err := json.Marshal(map[string][]map[string]any{action: {arguments}})
	if err != nil {
		t.Fatal(err)
	}
	start := goingBrowserStart(t)
	start.words = words
	start.serverURL = testServerURL
	start.chip = &goingContext{Path: "/issues/DEMO-12", Project: "DEMO", Issue: "DEMO-12"}
	context := going.ContextNamed
	if onPage {
		context = going.ContextPage
	}
	proposal := goingPropose(goingDecision{Context: context, offered: []string{action}},
		&going.Final{Answer: "Done.", Arguments: string(written)}, start, held)
	if len(proposal.Offers) != 1 || len(proposal.Offers[0].Invocations) != 1 {
		t.Fatalf("%q proposed %+v, want the one invocation of %s the answer wrote", words, proposal, action)
	}
	return proposal.Offers[0].Invocations[0].Automatic
}

// TestGoingAutomaticNamesAnItemOnlyByAReferenceTheWordsWrite: an ordinary English word that is
// also a project key names no project, and a document's slug names nothing under an owner the
// words do not name; an issue key, a project key typed in upper case, a reference and a whole id
// each name their item, and a longer key or a key in lower case names nothing.
func TestGoingAutomaticNamesAnItemOnlyByAReferenceTheWordsWrite(t *testing.T) {
	const ask = "0199e2a0-2222-7000-8000-000000000002"
	edit := []any{map[string]any{"op": "replace", "find": "first line", "with": "opening line"}}
	for _, tc := range []struct {
		name, words, action string
		onPage              bool
		arguments           map[string]any
		automatic           bool
	}{
		{"a project key the words spell as an English word", "write the stock count down as a document", "dispatch_artifact", false,
			map[string]any{"project": "AS", "name": "stock.md", "content": "# Stock\n"}, false},
		{"a document under a project the words spell as an English word", "update the notes", "dispatch_doc_edit", false,
			map[string]any{"project": "THE", "artifact": "notes", "ops": edit}, false},
		{"a project key typed in upper case as a token of its own", "write the stock count down as a document in AS", "dispatch_artifact", false,
			map[string]any{"project": "AS", "name": "stock.md", "content": "# Stock\n"}, true},
		{"a document under the page's project", "update the notes", "dispatch_doc_edit", true,
			map[string]any{"project": "DEMO", "artifact": "notes", "ops": edit}, true},
		{"a document under an issue the words name", "call it the opening line in the notes of DEMO-7", "dispatch_doc_edit", false,
			map[string]any{"issue": "DEMO-7", "artifact": "notes", "ops": edit}, true},
		{"a document a reference names", "fix dispatch://DEMO-7/artifact/notes", "dispatch_doc_edit", false,
			map[string]any{"issue": "DEMO-7", "artifact": "notes", "ops": edit}, true},
		{"a document whose name the words never write", "fix DEMO-7", "dispatch_doc_edit", false,
			map[string]any{"issue": "DEMO-7", "artifact": "notes", "ops": edit}, false},
		{"an issue key the words write", "mark DEMO-12 done", "dispatch_issue_update", false,
			map[string]any{"issue": "DEMO-12", "status": "done", "reason": "Shipped."}, true},
		{"a key the words write a longer key of", "mark DEMO-123 done", "dispatch_issue_update", false,
			map[string]any{"issue": "DEMO-12", "status": "done", "reason": "Shipped."}, false},
		{"a key the words write in lower case", "mark demo-12 done", "dispatch_issue_update", false,
			map[string]any{"issue": "DEMO-12", "status": "done", "reason": "Shipped."}, false},
		{"an ask's id the words write whole", "move the ship ask " + ask + " to Monday", "dispatch_edit_ask", false,
			map[string]any{"ask": ask, "question": "Ship it on Monday?"}, true},
		{"an ask's id the words do not write", "move the ship ask to Monday", "dispatch_edit_ask", false,
			map[string]any{"ask": ask, "question": "Ship it on Monday?"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goingAutomaticOf(t, tc.words, tc.onPage, tc.action, tc.arguments, nil); got != tc.automatic {
				t.Fatalf("%q with %v is automatic=%v, want %v", tc.words, tc.arguments, got, tc.automatic)
			}
		})
	}
}

// TestGoingAutomaticHoldsAHandOverAndALinkToTheWordsToo: on the page's own issue, an update that
// also routes the issue to a session, assigns it to a person, makes it wait on another issue or
// ranks it beside one, each of which only stored text named, is an offer; a route or an assignee
// the words name, or one the issue already holds, runs at once.
func TestGoingAutomaticHoldsAHandOverAndALinkToTheWordsToo(t *testing.T) {
	update := func(extra map[string]any) map[string]any {
		arguments := map[string]any{"issue": "DEMO-12", "status": "done", "reason": "Shipped."}
		for name, value := range extra {
			arguments[name] = value
		}
		return arguments
	}
	holds := func(route, assignee string) goingHeld {
		return func(issue string) (map[string]string, bool) {
			return map[string]string{"route": route, "assignee": assignee}, issue == "DEMO-12"
		}
	}
	for _, tc := range []struct {
		name, words string
		extra       map[string]any
		held        goingHeld
		automatic   bool
	}{
		{"the update alone", "mark this done", nil, nil, true},
		{"a route only stored text named", "mark this done", map[string]any{"route": "session:0199e2a0-9999-7000-8000-000000000009"}, holds("", ""), false},
		{"an assignee only stored text named", "mark this done", map[string]any{"assignee": "mallory@example.com"}, holds("", "alice@example.com"), false},
		{"an unassign the words did not ask for", "mark this done", map[string]any{"assignee": nil}, holds("", "alice@example.com"), false},
		{"a blocker only stored text named", "mark this done", map[string]any{"blocked_by": []any{"DEMO-99"}}, nil, false},
		{"a rank beside an issue only stored text named", "mark this done", map[string]any{"before": "DEMO-99"}, nil, false},
		{"a route the issue already holds", "mark this done", map[string]any{"route": "role:reviewer"}, holds("role:reviewer", ""), true},
		{"an assignee the issue already holds", "mark this done", map[string]any{"assignee": "alice@example.com"}, holds("", "alice@example.com"), true},
		{"a route the words name", "mark this done and route it to role:reviewer.", map[string]any{"route": "role:reviewer"}, nil, true},
		{"an assignee the words name", "mark this done for alice@example.com", map[string]any{"assignee": "alice@example.com"}, nil, true},
		{"a blocker the words name", "mark this done, blocked by DEMO-99", map[string]any{"blocked_by": []any{"DEMO-99"}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goingAutomaticOf(t, tc.words, true, "dispatch_issue_update", update(tc.extra), tc.held); got != tc.automatic {
				t.Fatalf("%q with %v is automatic=%v, want %v", tc.words, tc.extra, got, tc.automatic)
			}
		})
	}
}
