package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
)

// labelCases are calls of the run's tools, each by its tool and its arguments, with the words the
// bar shows while it runs and once it ended with no list to count: every form dispatch_read's spec
// takes (packages/contracts/src/dispatch-tools.ts, its ref's forms and its arguments' forms), the
// calls of a real-model preview's status question (search, read, find_tools, call_tool), a read of
// each kind call_tool runs (a tool of Dispatch, a read of going's, GitHub, the web, draw_block),
// propose_plan, a test hook, a tool no label names, and the changes a plan's rows make.
var labelCases = []struct {
	tool, input, present, past string
}{
	// read, by its arguments' forms.
	{toolRead, `{"issue":"DEMO-15"}`, "Reading DEMO-15", "Read DEMO-15"},
	{toolRead, `{"issue":"DEMO-1","artifact":"plan.md"}`, "Reading the document plan.md of DEMO-1", "Read the document plan.md of DEMO-1"},
	{toolRead, `{"project":"DEMO","artifact":"roadmap-md"}`, "Reading the document roadmap-md of DEMO", "Read the document roadmap-md of DEMO"},
	{toolRead, `{"project":"DEMO"}`, "Reading the documents of DEMO", "Read the documents of DEMO"},
	{toolRead, `{"message":"0190c7d2-1111-7222-8333-444444444444"}`, "Reading the message 0190c7d2-1111-7222-8333-444444444444", "Read the message 0190c7d2-1111-7222-8333-444444444444"},
	{toolRead, `{"issue":"DEMO-2","message":"0190c7d2-1111-7222-8333-444444444444"}`, "Reading the message 0190c7d2-1111-7222-8333-444444444444 on DEMO-2", "Read the message 0190c7d2-1111-7222-8333-444444444444 on DEMO-2"},
	{toolRead, `{"issue":"DEMO-1","events":20}`, "Reading the events of DEMO-1", "Read the events of DEMO-1"},
	{toolRead, `{"issue":"DEMO-1","all_events":true}`, "Reading the events of DEMO-1", "Read the events of DEMO-1"},
	{toolRead, `{}`, "Reading an item", "Read an item"},
	// read, by its ref's forms.
	{toolRead, `{"ref":"dispatch://DEMO-1"}`, "Reading DEMO-1", "Read DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/spec"}`, "Reading the spec of DEMO-1", "Read the spec of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-7/comments"}`, "Reading the comments of DEMO-7", "Read the comments of DEMO-7"},
	{toolRead, `{"ref":"dispatch://DEMO-1/log"}`, "Reading the events of DEMO-1", "Read the events of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/activity"}`, "Reading the events of DEMO-1", "Read the events of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/children"}`, "Reading the children of DEMO-1", "Read the children of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/artifact/plan-md"}`, "Reading the document plan-md of DEMO-1", "Read the document plan-md of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/artifact/plan-md@v3"}`, "Reading version 3 of the document plan-md of DEMO-1", "Read version 3 of the document plan-md of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/ask/9fa9e0d0"}`, "Reading the ask 9fa9e0d0 on DEMO-1", "Read the ask 9fa9e0d0 on DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/comment/c0ffee01"}`, "Reading the comment c0ffee01 on DEMO-1", "Read the comment c0ffee01 on DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/message/m0ffee01"}`, "Reading the message m0ffee01 on DEMO-1", "Read the message m0ffee01 on DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO/artifact/roadmap-md"}`, "Reading the document roadmap-md of DEMO", "Read the document roadmap-md of DEMO"},
	{toolRead, `{"ref":"dispatch://DEMO/artifact/roadmap-md@v2"}`, "Reading version 2 of the document roadmap-md of DEMO", "Read version 2 of the document roadmap-md of DEMO"},
	{toolRead, `{"ref":"dispatch://DEMO/artifact/roadmap-md/ask/a5k"}`, "Reading the ask a5k on the document roadmap-md of DEMO", "Read the ask a5k on the document roadmap-md of DEMO"},
	{toolRead, `{"ref":"dispatch://DEMO/artifact/roadmap-md/comment/c0m"}`, "Reading the comment c0m on the document roadmap-md of DEMO", "Read the comment c0m on the document roadmap-md of DEMO"},
	{toolRead, `{"ref":"dispatch://DEMO-1/spec#b-intro"}`, "Reading a block of the spec of DEMO-1", "Read a block of the spec of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO-1/artifact/plan-md#b-t3@v2"}`, "Reading a block of the document plan-md of DEMO-1", "Read a block of the document plan-md of DEMO-1"},
	{toolRead, `{"ref":"dispatch://DEMO/artifact/roadmap-md#b-goals"}`, "Reading a block of the document roadmap-md of DEMO", "Read a block of the document roadmap-md of DEMO"},
	{toolRead, `{"ref":"dispatch://DEMO-1/comment/c0ffee01#b-2"}`, "Reading a block of the comment c0ffee01 on DEMO-1", "Read a block of the comment c0ffee01 on DEMO-1"},
	{toolRead, `{"ref":"https://dispatch.example/issues/DEMO-4/artifacts/plan-md"}`, "Reading the document plan-md of DEMO-4", "Read the document plan-md of DEMO-4"},
	{toolRead, `{"ref":"https://dispatch.example/issues/DEMO-4"}`, "Reading DEMO-4", "Read DEMO-4"},
	{toolRead, `{"ref":"acme/infra#12"}`, "Reading acme/infra#12", "Read acme/infra#12"},
	// The status question a real-model preview asked, in its order.
	{toolRead, `{"issue":"DEMO-1"}`, "Reading DEMO-1", "Read DEMO-1"},
	{toolFindTools, `{"need":"list issue messages and handoffs for DEMO-1"}`, "Looking for a way to list issue messages and handoffs for DEMO-1", "Looked for a way to list issue messages and handoffs for DEMO-1"},
	// A need the model wrote as a tool's name reads as that tool's work in words.
	{toolFindTools, `{"need":"dispatch_issues"}`, "Looking for a way to list a project's issues for a roadmap or backlog pass", "Looked for a way to list a project's issues for a roadmap or backlog pass"},
	{toolFindTools, `{"need":"draw_block"}`, "Looking for a way to draw a table, a chart or one number", "Looked for a way to draw a table, a chart or one number"},
	{toolFindTools, `{"need":"dispatch_handoff_read for DEMO-7"}`, "Looking for a way to read the newest handoff recorded for one phase of an issue…", "Looked for a way to read the newest handoff recorded for one phase of an issue…"},
	{toolFindTools, `{"need":"Dispatch_Issue_Update labels"}`, "Looking for a way to update an existing issue", "Looked for a way to update an existing issue"},
	{toolCallTool, `{"tool":"dispatch_list_issue_messages","arguments":{"issue":"DEMO-1"}}`, "Reading the messages of DEMO-1", "Read the messages of DEMO-1"},
	{toolSearch, `{"query":"three things","project":"DEMO"}`, `Searching DEMO for "three things"`, `Searched DEMO for "three things"`},
	{toolSearch, `{"query":"three things"}`, `Searching Dispatch for "three things"`, `Searched Dispatch for "three things"`},
	// call_tool's reads.
	{toolCallTool, `{"tool":"dispatch_issues","arguments":{"project":"DEMO"}}`, "Listing the issues of DEMO", "Listed the issues of DEMO"},
	{toolCallTool, `{"tool":"dispatch_read","arguments":{"ref":"dispatch://DEMO-7/comments"}}`, "Reading the comments of DEMO-7", "Read the comments of DEMO-7"},
	{toolCallTool, `{"tool":"dispatch_doc_read","arguments":{"issue":"DEMO-1"}}`, "Reading the text of DEMO-1", "Read the text of DEMO-1"},
	{toolCallTool, `{"tool":"dispatch_doc_read","arguments":{"ref":"dispatch://DEMO-1/spec"}}`, "Reading the text of the spec of DEMO-1", "Read the text of the spec of DEMO-1"},
	{toolCallTool, `{"tool":"dispatch_priorities","arguments":{}}`, "Listing the priorities of every project", "Listed the priorities of every project"},
	{toolCallTool, `{"tool":"dispatch_priorities","arguments":{"project":["DEMO","CORE"]}}`, "Listing the priorities of DEMO, CORE", "Listed the priorities of DEMO, CORE"},
	{toolCallTool, `{"tool":"dispatch_resolve_issue","arguments":{"ref":"acme/infra#12"}}`, "Finding the issue acme/infra#12 names", "Found the issue acme/infra#12 names"},
	{toolCallTool, `{"tool":"read_roster","arguments":{}}`, "Reading the agent sessions", "Read the agent sessions"},
	{toolCallTool, `{"tool":"read_progress","arguments":{"issue":"DEMO-12"}}`, "Reading the progress of DEMO-12", "Read the progress of DEMO-12"},
	{toolCallTool, `{"tool":"read_progress","arguments":{"project":"DEMO"}}`, "Reading the progress of DEMO", "Read the progress of DEMO"},
	{toolCallTool, `{"tool":"read_progress","arguments":{}}`, "Reading the progress of the page's issue", "Read the progress of the page's issue"},
	{toolCallTool, `{"tool":"read_architecture","arguments":{"project":"DEMO"}}`, "Reading the components of DEMO", "Read the components of DEMO"},
	{toolCallTool, `{"tool":"read_project_activity","arguments":{}}`, "Reading the activity of the page's project", "Read the activity of the page's project"},
	{toolCallTool, `{"tool":"read_delivery","arguments":{"view":"measures"}}`, "Reading the delivery measures", "Read the delivery measures"},
	{toolCallTool, `{"tool":"github_read","arguments":{"repository":"cybermonklabs/legion","path":"pulls/12"}}`, "Reading pulls/12 in cybermonklabs/legion on GitHub", "Read pulls/12 in cybermonklabs/legion on GitHub"},
	{toolCallTool, `{"tool":"web_read","arguments":{"url":"https://example.com/docs/page?token=x#top"}}`, "Reading example.com/docs/page", "Read example.com/docs/page"},
	{toolCallTool, `{"tool":"draw_block","arguments":{"kind":"chart","title":"Issues by status"}}`, "Drawing a chart of Issues by status", "Drew a chart of Issues by status"},
	{toolCallTool, `{"tool":"draw_block","arguments":{"kind":"table","title":"Open asks"}}`, "Drawing a table of Open asks", "Drew a table of Open asks"},
	{toolCallTool, `{"tool":"dispatch_frobnicate","arguments":{}}`, "Trying a step Dispatch does not have", "Tried a step Dispatch does not have"},
	{toolCallTool, `{}`, "Trying a step Dispatch does not have", "Tried a step Dispatch does not have"},
	// propose_plan names how many changes its plan holds; a test hook; a tool the model made up.
	{toolProposePlan, `{"summary":"Close it","changes":[{"tool":"dispatch_issue_update","arguments":{"issue":"DEMO-3","status":"done"}}]}`, "Proposing a plan of 1 change", "Proposed a plan of 1 change"},
	{toolProposePlan, `{"summary":"Split it","changes":[{"tool":"dispatch_issue"},{"tool":"dispatch_issue"},{"tool":"dispatch_issue"}]}`, "Proposing a plan of 3 changes", "Proposed a plan of 3 changes"},
	{hookHang, `{}`, "Running a test hook", "Ran a test hook"},
	{"made_up_tool", `{"x":1}`, "Trying a step Dispatch does not have", "Tried a step Dispatch does not have"},
	// The changes a plan's rows make.
	{"dispatch_comment", `{"issue":"DEMO-7","body":"Done."}`, "Commenting on DEMO-7", "Commented on DEMO-7"},
	{"dispatch_comment", `{"ref":"dispatch://DEMO-7/spec","quote":"x","body":"Why?"}`, "Commenting on the spec of DEMO-7", "Commented on the spec of DEMO-7"},
	{"dispatch_issue_update", `{"issue":"DEMO-3","status":"done"}`, "Updating DEMO-3", "Updated DEMO-3"},
	{"dispatch_issue", `{"project":"DEMO","title":"Fix the login"}`, `Creating the issue "Fix the login" in DEMO`, `Created the issue "Fix the login" in DEMO`},
	{"dispatch_issue", `{"project":"DEMO","title":"` + strings.Repeat("word ", 30) + `"}`,
		`Creating the issue "word word word word word word word word word word word word…" in DEMO`,
		`Created the issue "word word word word word word word word word word word word…" in DEMO`},
	{"dispatch_message", `{"issue":"DEMO-1","body":"Hi"}`, "Sending a message to DEMO-1", "Sent a message to DEMO-1"},
	{"dispatch_message", `{"in_reply_to":"m1","body":"Hi"}`, "Sending a message to a conversation", "Sent a message to a conversation"},
	{"dispatch_artifact", `{"project":"DEMO","name":"notes.md","content":"x"}`, "Uploading notes.md to DEMO", "Uploaded notes.md to DEMO"},
	{"dispatch_request_approval", `{"issue":"DEMO-1","summary":"Ready"}`, "Requesting approval of the spec of DEMO-1", "Requested approval of the spec of DEMO-1"},
	{"dispatch_claim", `{"issue":"DEMO-1"}`, "Changing the claim on DEMO-1", "Changed the claim on DEMO-1"},
}

// TestEachStepReadsAsWords: each call's label, filled from its arguments, is the line the bar shows
// for it: plain words that name what the call reads, searches, draws, proposes or changes, at
// most labelLimit runes, never a tool's name, JSON or the tool-data marker.
func TestEachStepReadsAsWords(t *testing.T) {
	for _, row := range labelCases {
		label, arguments := stepLabel(row.tool, json.RawMessage(row.input))
		present, past := presentLabel(label, arguments), ended(label, arguments, "", false).label
		if present != row.present || past != row.past {
			t.Errorf("%s %s reads %q while it runs and %q once it ended, want %q and %q", row.tool, row.input, present, past, row.present, row.past)
		}
		for _, words := range []string{present, past} {
			if utf8.RuneCountInString(words) > labelLimit || strings.ContainsAny(words, "{}") || strings.Contains(words, "dispatch_") ||
				strings.Contains(words, storedDataNote) {
				t.Errorf("%s %s reads %q, which holds a tool's name, JSON or the data marker, or passes %d runes", row.tool, row.input, words, labelLimit)
			}
		}
	}
}

// TestAStepThatEndedNamesItsCountAndItems: a call that answered a list reads with how many rows it
// holds, and, where its answer states a total larger than that, how many there are in all (50 of
// 73 sessions), never more than it read; its items are the keys and references its arguments
// name, then the keys of its rows; a call that failed reads as one sentence that says, in words,
// why it could not.
func TestAStepThatEndedNamesItsCountAndItems(t *testing.T) {
	answered := func(status int, body string) string {
		return resultText(agentwire.CallRequest{Step: 2, Call: 1}, agentwire.CallAnswer{Status: status, Body: []byte(body)})
	}
	for _, row := range []struct {
		tool, input, result string
		refused             bool
		label               string
		count               int
		items               []string
	}{
		{toolSearch, `{"query":"three things","project":"DEMO"}`,
			answered(http.StatusOK, `{"results":[{"kind":"issue","owner":{"kind":"issue","key":"DEMO-15"}},{"kind":"comment","issue":{"key":"DEMO-3"}},{"kind":"document"}],"total":3}`),
			false, `Searched DEMO for "three things" · 3 hits`, 3, []string{"DEMO-15", "DEMO-3"}},
		{toolSearch, `{"query":"nothing like it"}`, answered(http.StatusOK, `{"results":[],"total":0}`),
			false, `Searched Dispatch for "nothing like it" · no hits`, 0, []string{}},
		{toolCallTool, `{"tool":"dispatch_issues","arguments":{"project":"DEMO"}}`, answered(http.StatusOK, `{"issues":[{"key":"DEMO-1"},{"key":"DEMO-2"}]}`),
			false, "Listed the issues of DEMO · 2 issues", 2, []string{"DEMO-1", "DEMO-2"}},
		{toolCallTool, `{"tool":"read_roster","arguments":{}}`, answered(http.StatusOK, `{"facts":{"source":"live"},"tables":{"sessions":[{"id":"s1"}]}}`),
			false, "Read the agent sessions · 1 session", 1, []string{}},
		{toolCallTool, `{"tool":"read_roster","arguments":{}}`, answered(http.StatusOK, `{"total":73,"sessions":[{"session_id":"s1"},{"session_id":"s2"}]}`),
			false, "Read the agent sessions · 2 of 73 sessions", 2, []string{}},
		{toolCallTool, `{"tool":"read_roster","arguments":{}}`, answered(http.StatusOK, `{"total":2,"sessions":[{"session_id":"s1"},{"session_id":"s2"}]}`),
			false, "Read the agent sessions · 2 sessions", 2, []string{}},
		{toolCallTool, `{"tool":"dispatch_issues","arguments":{"project":"DEMO"}}`,
			answered(http.StatusOK, `{"issues":[{"key":"DEMO-1"},{"key":"DEMO-2"}],"total":500,"limit":2,"offset":0}`),
			false, "Listed the issues of DEMO · 2 of 500 issues", 2, []string{"DEMO-1", "DEMO-2"}},
		{toolCallTool, `{"tool":"dispatch_list_issue_messages","arguments":{"issue":"DEMO-7"}}`, answered(http.StatusOK, `[{"id":"m1"},{"id":"m2"}]`),
			false, "Read the messages of DEMO-7 · 2 messages", 2, []string{"DEMO-7"}},
		{toolRead, `{"issue":"DEMO-15"}`, answered(http.StatusOK, `{"key":"DEMO-15"}`), false, "Read DEMO-15", 0, []string{"DEMO-15"}},
		{toolRead, `{"ref":"dispatch://DEMO-7/comments"}`, answered(http.StatusOK, `[]`), false, "Read the comments of DEMO-7", 0, []string{"DEMO-7"}},
		{toolRead, `{"ref":"dispatch://DEMO-1/spec"}`, answered(http.StatusOK, `{}`), false, "Read the spec of DEMO-1", 0, []string{"dispatch://DEMO-1/spec"}},
		{toolRead, `{"ref":"dispatch://DEMO-1/ask/9fa9e0d0"}`, answered(http.StatusOK, `{}`), false, "Read the ask 9fa9e0d0 on DEMO-1", 0, []string{"dispatch://DEMO-1/ask/9fa9e0d0"}},
		{"dispatch_issue_update", `{"issue":"DEMO-3","parent":"DEMO-1","blocked_by":["DEMO-9"]}`, answered(http.StatusOK, `{}`),
			false, "Updated DEMO-3", 0, []string{"DEMO-3", "DEMO-9", "DEMO-1"}},
		{toolRead, `{"issue":"DEMO-12"}`, answered(http.StatusForbidden, `{"code":"FORBIDDEN","error":"no"}`),
			true, "Could not read DEMO-12: you have no access", 0, []string{"DEMO-12"}},
		{toolRead, `{"issue":"DEMO-99"}`, answered(http.StatusNotFound, `{"code":"NOT_FOUND","error":"no"}`),
			true, "Read DEMO-99 · none found", 0, []string{"DEMO-99"}},
		{toolCallTool, `{"tool":"dispatch_handoff_read","arguments":{"issue":"DEMO-7","phase":"plan"}}`, answered(http.StatusNotFound, `{"code":"NOT_FOUND","error":"no"}`),
			true, "Read the plan handoff of DEMO-7 · none found", 0, []string{"DEMO-7"}},
		{toolSearch, `{"query":"x y"}`, answered(http.StatusBadRequest, `{"code":"INVALID","error":"no"}`),
			true, `Could not search Dispatch for "x y": Dispatch refused the request`, 0, []string{}},
		{toolCallTool, `{"tool":"read_roster","arguments":{}}`, answered(http.StatusServiceUnavailable, `{}`),
			true, "Could not read the agent sessions: Dispatch failed to answer", 0, []string{}},
		{toolCallTool, `{"tool":"dispatch_comment","arguments":{"issue":"DEMO-7","body":"x"}}`,
			fmt.Sprintf("dispatch_comment %s: propose it in propose_plan for the person to approve", writeNotRunHere),
			true, "Could not comment on DEMO-7: it changes something, so it waits for a plan you approve", 0, []string{"DEMO-7"}},
		{toolRead, `{"issue":"DEMO-1"}`, callCutOff, true, "Could not read DEMO-1: the run stopped", 0, []string{"DEMO-1"}},
		{toolFindTools, `{}`, "find_tools takes need, what you need a tool for", true, "Could not look for a way to the need: the request was not valid", 0, []string{}},
		{hookPanic, `{}`, fmt.Sprintf("the tool %s failed: %s (boom)", hookPanic, toolPanicked), true, "Could not run a test hook: the tool failed", 0, []string{}},
	} {
		label, arguments := stepLabel(row.tool, json.RawMessage(row.input))
		end := ended(label, arguments, row.result, row.refused)
		if end.label != row.label || end.count != row.count || !slices.Equal(end.items, row.items) {
			t.Errorf("%s %s ended reading %q, count %d, items %q; want %q, %d, %q", row.tool, row.input, end.label, end.count, end.items, row.label, row.count, row.items)
		}
	}
}

// TestAReadThatFoundNothingIsNoFailure: a read Dispatch answered 404 - the item or record it reads
// does not exist - found nothing, which the person cannot act on as a failure: it reads as its past
// form with " · none found" and carries no error. A 403, a 5xx, a 404 of a step that is no read and
// a call that got no answer stay failures, in one sentence of why.
func TestAReadThatFoundNothingIsNoFailure(t *testing.T) {
	answered := func(status int) string {
		return resultText(agentwire.CallRequest{Step: 2, Call: 1}, agentwire.CallAnswer{Status: status, Body: []byte(`{"code":"X","error":"no"}`)})
	}
	handoff := `{"tool":"dispatch_handoff_read","arguments":{"issue":"DEMO-7","phase":"plan"}}`
	for _, row := range []struct {
		name, tool, input, result string
		noneFound                 bool
		label                     string
	}{
		{"a read answered 404", toolCallTool, handoff, answered(http.StatusNotFound), true, "Read the plan handoff of DEMO-7 · none found"},
		{"a read answered 403", toolCallTool, handoff, answered(http.StatusForbidden), false, "Could not read the plan handoff of DEMO-7: you have no access"},
		{"a read answered 500", toolCallTool, handoff, answered(http.StatusInternalServerError), false, "Could not read the plan handoff of DEMO-7: Dispatch failed to answer"},
		{"a search answered 404", toolSearch, `{"query":"x y"}`, answered(http.StatusNotFound), false, `Could not search Dispatch for "x y": it was not found`},
		{"a change answered 404", "dispatch_comment", `{"issue":"DEMO-7","body":"Done."}`, answered(http.StatusNotFound), false, "Could not comment on DEMO-7: it was not found"},
		{"a read the run stopped", toolRead, `{"issue":"DEMO-1"}`, callCutOff, false, "Could not read DEMO-1: the run stopped"},
	} {
		label, arguments := stepLabel(row.tool, json.RawMessage(row.input))
		if end := ended(label, arguments, row.result, true); end.noneFound != row.noneFound || end.label != row.label {
			t.Errorf("%s ended reading %q, none found %t; want %q, %t", row.name, end.label, end.noneFound, row.label, row.noneFound)
		}
	}
}

// TestALookForAToolByItsNameNamesNoTool: whatever tool find_tools can return the model names as
// its need, the step's line reads that tool's work in words and holds no tool's name (rule 1 of
// the bar's design), while the call itself runs with the model's own input.
func TestALookForAToolByItsNameNamesNoTool(t *testing.T) {
	for _, tool := range findableTools() {
		input := json.RawMessage(fmt.Sprintf(`{"need":%q}`, tool.name))
		label, arguments := stepLabel(toolFindTools, input)
		present, past := presentLabel(label, arguments), ended(label, arguments, "", false).label
		for _, words := range []string{present, past} {
			if strings.Contains(words, "_") || strings.TrimPrefix(strings.TrimPrefix(words, "Looking for a way to "), "Looked for a way to ") == "" {
				t.Errorf("a look for %s reads %q, which names a tool or no work", tool.name, words)
			}
		}
		if need, _ := labelArguments(input)["need"].(string); need != tool.name {
			t.Errorf("the look for %s runs with the need %q, want the model's own", tool.name, need)
		}
	}
}

// labelProblems is what is wrong with label, the label of the tool named name whose arguments'
// schema is schema, by the rules dispatch-tools.ts's dispatchToolLabelProblems holds a spec's
// label to: a kind, a lowercase verb, a present form that starts with the verb's -ing form, a past
// form that starts with a capital, a lowercase count noun, and each {name} an argument of the
// tool, with words for its absence where a call may leave it out.
func labelProblems(name string, label contracts.ToolLabel, schema json.RawMessage) []string {
	if label.Present == "" || label.Past == "" {
		return []string{name + " has no label"}
	}
	var shape struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schema, &shape); err != nil {
		return []string{fmt.Sprintf("%s's schema does not read: %v", name, err)}
	}
	var problems []string
	if !slices.Contains(labelKinds, label.Kind) {
		problems = append(problems, fmt.Sprintf("%s's label kind %q is none of %v", name, label.Kind, labelKinds))
	}
	if !regexp.MustCompile(`^[a-z]+$`).MatchString(label.Verb) {
		problems = append(problems, fmt.Sprintf("%s's label verb %q is not one lowercase word", name, label.Verb))
	}
	if label.Count != "" && !regexp.MustCompile(`^[a-z]+( [a-z]+)*$`).MatchString(label.Count) {
		problems = append(problems, fmt.Sprintf("%s's label count %q is not lowercase words", name, label.Count))
	}
	first, _, _ := strings.Cut(label.Present, " ")
	if !regexp.MustCompile(`^[A-Z][a-z]*ing$`).MatchString(first) || !strings.HasPrefix(strings.ToLower(first), strings.TrimSuffix(label.Verb, "e")) {
		problems = append(problems, fmt.Sprintf("%s's present label %q does not start with the -ing form of %q", name, label.Present, label.Verb))
	}
	if !regexp.MustCompile(`^[A-Z]`).MatchString(label.Past) {
		problems = append(problems, fmt.Sprintf("%s's past label %q does not start with a capital", name, label.Past))
	}
	var named func(form, at string)
	named = func(form, at string) {
		for len(form) > 0 {
			open := strings.IndexByte(form, '{')
			if strings.ContainsAny(form[:max(open, 0)], "}|") || (open < 0 && strings.ContainsAny(form, "}|")) {
				problems = append(problems, at+" holds a } or | that writes no {name}")
			}
			if open < 0 {
				return
			}
			end := closingBrace(form, open)
			if end < 0 {
				problems = append(problems, at+" holds a { that closes nowhere")
				return
			}
			argument, absent, hasAbsent := strings.Cut(form[open+1:end], "|")
			_, takes := shape.Properties[argument]
			switch {
			case !takes:
				problems = append(problems, fmt.Sprintf("%s names {%s}, which is no argument of the tool", at, argument))
			case hasAbsent && strings.TrimSpace(absent) == "":
				problems = append(problems, fmt.Sprintf("%s names {%s|} with no words for its absence", at, argument))
			case !hasAbsent && !slices.Contains(shape.Required, argument):
				problems = append(problems, fmt.Sprintf("%s names {%s}, an argument a call may leave out, with no words for its absence", at, argument))
			}
			if hasAbsent {
				named(absent, at)
			}
			form = form[end+1:]
		}
	}
	named(label.Present, fmt.Sprintf("%s's present label %q", name, label.Present))
	named(label.Past, fmt.Sprintf("%s's past label %q", name, label.Past))
	return problems
}

// TestEveryToolARunCallsHasALabel: every tool find_tools can answer - each tool of Dispatch the bar
// offers (all but going.UnofferedTools), the comments read of dispatch_read, the reads the server
// makes by going's names, the reads outside Dispatch and draw_block - and the run's outer tools and
// review_plan carry a label the bar can fill: one whose {name}s are their tool's arguments, each
// with words for its absence where a call may leave it out.
func TestEveryToolARunCallsHasALabel(t *testing.T) {
	findable := findableTools()
	if len(findable) < len(contracts.DispatchTools)-len(going.UnofferedTools)+len(going.ReadSpecs())+len(outsideReads)+1 {
		t.Fatalf("find_tools answers %d tools, fewer than it is given", len(findable))
	}
	for _, tool := range findable {
		label := namedLabel(tool.name)
		if label == unknownToolLabel {
			t.Errorf("%s, which find_tools answers, has no label", tool.name)
			continue
		}
		for _, problem := range labelProblems(tool.name, label, tool.schema) {
			t.Error(problem)
		}
	}
	for _, problem := range labelProblems("issueCommentsRead", issueCommentsRead.Label, schemaOf(issueCommentsRead.Arguments)) {
		t.Error(problem)
	}
	w := &worker{agent: &agent{}}
	for _, tool := range w.tools(0) {
		if tool.Name == toolCallTool {
			// call_tool shows as the tool it names, whose label is checked above.
			continue
		}
		label, _ := stepLabel(tool.Name, nil)
		for _, problem := range labelProblems(tool.Name, label, tool.Schema) {
			t.Error(problem)
		}
	}
	for _, problem := range labelProblems(toolReviewPlan, reviewPlanLabel, reviewPlanSchema) {
		t.Error(problem)
	}
	for _, problem := range labelProblems(hookHang, hookLabel, json.RawMessage(`{"type":"object","properties":{}}`)) {
		t.Error(problem)
	}
}

// TestTheLabelCheckRefusesWhatTheBarCannotFill: the check every tool's label passes refuses a tool
// with no label, a {name} that is no argument of its tool, and an argument a call may leave out
// named with no words for its absence.
func TestTheLabelCheckRefusesWhatTheBarCannotFill(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"issue":{"type":"string"},"project":{"type":"string"}},"required":["issue"]}`)
	read := contracts.ToolLabel{Kind: kindRead, Verb: "read", Present: "Reading {issue}", Past: "Read {issue}"}
	for _, row := range []struct {
		name    string
		label   contracts.ToolLabel
		problem string
	}{
		{"no label", contracts.ToolLabel{}, "t has no label"},
		{"a {name} that is no argument", contracts.ToolLabel{Kind: kindRead, Verb: "read", Present: "Reading {ref}", Past: "Read {issue}"},
			`t's present label "Reading {ref}" names {ref}, which is no argument of the tool`},
		{"an optional argument with no words for its absence", contracts.ToolLabel{Kind: kindRead, Verb: "read", Present: "Reading {issue} of {project}", Past: "Read {issue}"},
			`t's present label "Reading {issue} of {project}" names {project}, an argument a call may leave out, with no words for its absence`},
		{"empty words for an absence", contracts.ToolLabel{Kind: kindRead, Verb: "read", Present: "Reading {issue}", Past: "Read {issue} of {project|}"},
			`t's past label "Read {issue} of {project|}" names {project|} with no words for its absence`},
		{"a kind none of the seven", contracts.ToolLabel{Kind: "list", Verb: "read", Present: "Reading {issue}", Past: "Read {issue}"},
			`t's label kind "list" is none of [read search look chart plan review write]`},
		{"a present form of another verb", contracts.ToolLabel{Kind: kindRead, Verb: "list", Present: "Reading {issue}", Past: "Listed {issue}"},
			`t's present label "Reading {issue}" does not start with the -ing form of "list"`},
	} {
		if problems := labelProblems("t", row.label, schema); !slices.Contains(problems, row.problem) {
			t.Errorf("%s: the check found %q, want %q among them", row.name, problems, row.problem)
		}
	}
	if problems := labelProblems("t", read, schema); len(problems) > 0 {
		t.Errorf("the check refused a label it can fill: %q", problems)
	}
}

// TestARunsFramesCarryEachStepsWords: a reply's frame carries the present form of each call's label
// in the order of its calls, and each call's tool frame the past form, or the sentence of a call
// that failed, with its kind and its items; the note the model reads stays as it was.
func TestARunsFramesCarryEachStepsWords(t *testing.T) {
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if len(request.ToolResults()) > 0 {
			return agenttest.Reply{Text: "DEMO-15 is in review."}
		}
		return agenttest.Reply{Text: "Reading the issue.", Calls: []agenttest.Call{
			{Name: toolRead, Arguments: map[string]any{"issue": "DEMO-15"}},
			{Name: toolCallTool, Arguments: map[string]any{"tool": "dispatch_list_issue_messages", "arguments": map[string]any{"issue": "DEMO-7"}}},
			{Name: toolFindTools, Arguments: map[string]any{"need": "list an issue's messages"}},
		}}
	})
	server, client := newFakeServer(t, func(call agentwire.CallRequest) agentwire.CallAnswer {
		if call.Tool == "dispatch_list_issue_messages" {
			return agentwire.CallAnswer{Status: http.StatusForbidden, Body: []byte(`{"code":"FORBIDDEN","error":"no"}`)}
		}
		return agentwire.CallAnswer{Status: http.StatusOK, Body: []byte(`{"key":"DEMO-15"}`)}
	})
	workRun(t, fake.Model(), client, "where is DEMO-15")
	if end, _ := server.end(t); end.State != agentwire.StateDone {
		t.Fatalf("the run ended %s with %q, want done", end.State, end.Reason)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	var replies []replyFrame
	tools := map[string]toolFrame{}
	for _, step := range server.steps {
		for _, frame := range step.Frames {
			switch frame.Name {
			case frameReply:
				var reply replyFrame
				_ = json.Unmarshal(frame.Data, &reply)
				replies = append(replies, reply)
			case frameTool:
				var tool toolFrame
				_ = json.Unmarshal(frame.Data, &tool)
				tools[tool.Tool] = tool
				if tool.Note != step.Note || !strings.HasPrefix(tool.Note, tool.Tool+" ") {
					t.Errorf("the %s frame's note is %q, want the step's own note %q, which names its call", tool.Tool, tool.Note, step.Note)
				}
			}
		}
	}
	want := []string{"Reading DEMO-15", "Reading the messages of DEMO-7", "Looking for a way to list an issue's messages"}
	if len(replies) != 2 || !slices.Equal(replies[0].Labels, want) || replies[0].Text != "Reading the issue." || len(replies[1].Labels) != 0 {
		t.Fatalf("the reply frames are %+v, want the first to carry the labels %q beside its text and the answer none", replies, want)
	}
	for tool, frame := range map[string]toolFrame{
		toolRead:      {Label: "Read DEMO-15", Kind: kindRead, Items: []string{"DEMO-15"}},
		toolCallTool:  {Label: "Could not read the messages of DEMO-7: you have no access", Kind: kindRead, Items: []string{"DEMO-7"}, Error: true},
		toolFindTools: {Label: "Looked for a way to list an issue's messages", Kind: kindLook, Items: []string{}},
	} {
		got := tools[tool]
		if got.Label != frame.Label || got.Kind != frame.Kind || got.Error != frame.Error || !slices.Equal(got.Items, frame.Items) {
			t.Errorf("the %s frame carries %+v, want the label %q, the kind %q, error %t and the items %q", tool, got, frame.Label, frame.Kind, frame.Error, frame.Items)
		}
	}
}

// TestAReadThatFoundNothingAndALookByAToolsNameCarryWords: a read Dispatch answered 404 carries its
// past form with " · none found" and no error, so the bar counts no failure, while the model is
// still told what Dispatch answered; a find_tools call whose need is a tool's name carries that
// tool's work in words, while the call runs with the model's own need, which answers that tool.
func TestAReadThatFoundNothingAndALookByAToolsNameCarryWords(t *testing.T) {
	var told []string
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if results := request.ToolResults(); len(results) > 0 {
			told = results
			return agenttest.Reply{Text: "DEMO-99 does not exist."}
		}
		return agenttest.Reply{Calls: []agenttest.Call{
			{Name: toolRead, Arguments: map[string]any{"issue": "DEMO-99"}},
			{Name: toolFindTools, Arguments: map[string]any{"need": "dispatch_issues"}},
		}}
	})
	server, client := newFakeServer(t, func(agentwire.CallRequest) agentwire.CallAnswer {
		return agentwire.CallAnswer{Status: http.StatusNotFound, Body: []byte(`{"code":"NOT_FOUND","error":"not found"}`)}
	})
	workRun(t, fake.Model(), client, "what is DEMO-99")
	if end, _ := server.end(t); end.State != agentwire.StateDone {
		t.Fatalf("the run ended %s with %q, want done", end.State, end.Reason)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	tools := map[string]toolFrame{}
	for _, step := range server.steps {
		for _, frame := range step.Frames {
			if frame.Name == frameTool {
				var tool toolFrame
				_ = json.Unmarshal(frame.Data, &tool)
				tools[tool.Tool] = tool
			}
		}
	}
	if read := tools[toolRead]; read.Label != "Read DEMO-99 · none found" || read.Error || read.Kind != kindRead {
		t.Errorf("the read's frame carries %+v, want the label %q, kind read and no error", read, "Read DEMO-99 · none found")
	}
	look := tools[toolFindTools]
	if look.Label != "Looked for a way to list a project's issues for a roadmap or backlog pass" || look.Error || !strings.Contains(look.Note, `"need":"dispatch_issues"`) {
		t.Errorf("the look's frame carries %+v, want the work of dispatch_issues in words and the model's own need in its note", look)
	}
	if len(told) != 2 || !strings.Contains(told[0], "status 404") || !strings.Contains(told[1], `"name":"dispatch_issues"`) {
		t.Errorf("the model was told %q, want the read's 404 and the look's answer of dispatch_issues", told)
	}
}
