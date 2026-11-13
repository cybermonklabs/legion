// going_named_test.go holds which invocations of a proposal run at once to what the person's own
// typed words name: an item is named only by a reference to it as Dispatch reads references in
// text, a document only under an owner so named, a hand-over of the work - its route, its
// assignee - only where the words name it or the issue already holds it, and an upload over a
// document its owner holds only where the words write its name.
package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
)

// goingAutomaticOf is whether the one invocation the answer wrote for action, as the proposal of a
// run typed as words marks it: about the page's DEMO-12 where onPage, about the items the words
// name otherwise.
func goingAutomaticOf(t *testing.T, words string, onPage bool, action string, arguments map[string]any, holds goingHolds) bool {
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
		&going.Final{Answer: "Done.", Arguments: string(written)}, start, holds)
	if len(proposal.Offers) != 1 || len(proposal.Offers[0].Invocations) != 1 {
		t.Fatalf("%q proposed %+v, want the one invocation of %s the answer wrote", words, proposal, action)
	}
	return proposal.Offers[0].Invocations[0].Automatic
}

// goingDocumentsHeld is a store whose owners hold the documents named, each as its owner's key,
// a slash and the document's name or slug, and which answers every read.
func goingDocumentsHeld(documents ...string) goingDocumentHeld {
	return func(owner artifactTarget, name string) (bool, bool) {
		key := owner.Project
		if owner.IssueKey != nil {
			key = *owner.IssueKey
		}
		return slices.Contains(documents, key+"/"+name), true
	}
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
			if got := goingAutomaticOf(t, tc.words, tc.onPage, tc.action, tc.arguments, goingHolds{document: goingDocumentsHeld()}); got != tc.automatic {
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
			if got := goingAutomaticOf(t, tc.words, true, "dispatch_issue_update", update(tc.extra), goingHolds{issue: tc.held}); got != tc.automatic {
				t.Fatalf("%q with %v is automatic=%v, want %v", tc.words, tc.extra, got, tc.automatic)
			}
		})
	}
}

// TestGoingAutomaticHoldsAnUploadsNameToTheWordsWhereItsOwnerHoldsThatDocument (review round 2,
// finding A): an upload whose name is a document the page's issue already holds would write that
// document's next version, so it runs at once only where the words write the name; an upload
// under a name the issue holds no document of makes one and runs at once, and one whose owner
// could not be read is an offer.
func TestGoingAutomaticHoldsAnUploadsNameToTheWordsWhereItsOwnerHoldsThatDocument(t *testing.T) {
	upload := func(name string) map[string]any {
		return map[string]any{"issue": "DEMO-12", "name": name, "content": "# Notes\n\nModel-written text.\n"}
	}
	unread := func(artifactTarget, string) (bool, bool) { return false, false }
	for _, tc := range []struct {
		name, words, document string
		holds                 goingDocumentHeld
		automatic             bool
	}{
		{"a name the issue holds a document of that only stored text wrote", "save this as a doc", "spec", goingDocumentsHeld("DEMO-12/spec"), false},
		{"a name the issue holds a document of that the words write", "save this over the spec", "spec", goingDocumentsHeld("DEMO-12/spec"), true},
		{"a name the issue holds no document of", "save this as a doc", "summary", goingDocumentsHeld("DEMO-12/spec"), true},
		{"a name another owner holds a document of", "save this as a doc", "spec", goingDocumentsHeld("DEMO-7/spec"), true},
		{"a name whose owner could not be read", "save this as a doc", "summary", unread, false},
		{"a name with no read of the store", "save this as a doc", "summary", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goingAutomaticOf(t, tc.words, true, "dispatch_artifact", upload(tc.document), goingHolds{document: tc.holds}); got != tc.automatic {
				t.Fatalf("%q uploading as %q is automatic=%v, want %v", tc.words, tc.document, got, tc.automatic)
			}
		})
	}
}

// goingPathArgumentsNamingNoItem are the arguments a row of the catalogue binds into its path that
// name no item of Dispatch's: a handoff's phase, of which the write appends the next version on
// the issue the call names.
var goingPathArgumentsNamingNoItem = map[string]bool{"phase": true}

// TestGoingAutomaticClassesEveryArgumentOfTheCatalogue (review round 2, finding C): every
// argument of every action is classed exactly once - an item (goingItemArguments), a hand-over
// (goingHeldArguments) or content (goingContentArguments) - every argument a row binds into its
// path and every argument the bar prompts for as an item is an item, and the classes hold no
// argument the catalogue does not have. An argument no class holds runs nothing at once.
func TestGoingAutomaticClassesEveryArgumentOfTheCatalogue(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]bool{}
	for _, action := range catalogue.Actions() {
		for _, field := range action.Request {
			arguments[field.Name] = true
			classes := 0
			for _, classed := range []bool{goingItemArguments[field.Name] != "", goingHeldArguments[field.Name], goingContentArguments[field.Name]} {
				if classed {
					classes++
				}
			}
			if classes != 1 {
				t.Errorf("%s's argument %s is in %d classes, want exactly one", action.Action, field.Name, classes)
			}
			if _, item := goingItemFields[field.Name]; item && goingItemArguments[field.Name] == "" {
				t.Errorf("%s's argument %s is prompted for as an item and not classed as one", action.Action, field.Name)
			}
		}
		for _, route := range action.Routes {
			for placeholder, argument := range route.path {
				if argument != goingCallerSession && goingItemArguments[argument] == "" && !goingPathArgumentsNamingNoItem[argument] {
					t.Errorf("%s %s binds %s to %s, which is not classed as an item", route.Method, route.Pattern, placeholder, argument)
				}
			}
		}
		if upload, uploads := goingUploadNames[action.Action]; uploads && !slices.ContainsFunc(action.Request, func(field contracts.ToolField) bool { return field.Name == upload }) {
			t.Errorf("%s takes no argument %s, which goingUploadNames names", action.Action, upload)
		}
	}
	classed := len(goingItemArguments) + len(goingHeldArguments) + len(goingContentArguments)
	if classed != len(arguments) {
		t.Errorf("the classes hold %d arguments and the catalogue %d: a class names an argument no action takes", classed, len(arguments))
	}
	named := goingNamedBy("mark DEMO-12 done", testServerURL)
	if goingAutomatic("dispatch_issue_update", map[string]any{"issue": "DEMO-12", "status": "done", "watcher": "DEMO-99"}, nil, "", named, goingHolds{}) {
		t.Error("an invocation with an argument no class holds is automatic")
	}
}
