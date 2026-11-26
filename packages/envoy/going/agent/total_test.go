package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going/agentwire"
)

// issueList is a bare list of n issues, as GET /api/v1/issues answers all of a project's.
func issueList(n int) []byte {
	rows := make([]string, n)
	for at := range rows {
		rows[at] = fmt.Sprintf(`{"key":"ACME-%d","title":"Issue %d of the list","status":"todo","priority":2}`, at+1, at+1)
	}
	return []byte("[" + strings.Join(rows, ",") + "]")
}

// savedTool is the body of a saved tool step of call_tool with tool and result.
func savedTool(t *testing.T, tool, result string) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(toolBody{Call: toolCall{ID: "call_1", Tool: toolCallTool, Arguments: json.RawMessage(`{"tool":"` + tool + `","arguments":{}}`)}, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestAListResultGivesItsTotalBeforeItsBody: at 8a3e562b a chart answer said DEMO has 41 issues
// where the list it was given, whole, and the chart held 42: the model counted the rows itself.
// So a list result names its total on a line between its head and its body, in the label's noun:
// a bare list how many items it holds, and a page how many items match and how many of them it
// holds. A list past modelResultLimit, which the model is given only the first bytes of, still
// names its whole count before the cut body. An answer that is no list, an object of a tool whose
// label counts no list, and an answer the server cut, whose body does not parse, name none. Each
// is read back with its body as the server answered it, byte for byte, and the model is given the
// total line with the body's priorities in words.
func TestAListResultGivesItsTotalBeforeItsBody(t *testing.T) {
	issues := agentwire.CallRequest{Step: 2, Call: 1, Kind: agentwire.KindRoute, Tool: "dispatch_issues"}
	head := storedDataNote + "\nstep 2, call 1: status 200\n"
	page := []byte(`{"issues":` + string(issueList(2)) + `,"total":500,"limit":2,"offset":0}`)
	long := issueList(2 * modelResultLimit / len(issueList(1)))
	for _, c := range []struct {
		name  string
		call  agentwire.CallRequest
		body  []byte
		cut   bool
		total string
	}{
		{"a list that fits", issues, issueList(3), false, "[Total: 3 issues in the list below.]\n"},
		{"a list of one", issues, issueList(1), false, "[Total: 1 issue in the list below.]\n"},
		{"a list past the bound", issues, long, false, fmt.Sprintf("[Total: %d issues in the list below.]\n", strings.Count(string(long), `"key"`))},
		{"a page", issues, page, false, "[Total: 500 matching issues; the list below holds 2 of them.]\n"},
		{"a list of a tool that counts no noun", agentwire.CallRequest{Step: 2, Call: 1, Kind: agentwire.KindGitHub}, []byte(`[{"number":1},{"number":2}]`), false, "[Total: 2 items in the list below.]\n"},
		{"an answer that is no list", agentwire.CallRequest{Step: 2, Call: 1, Kind: agentwire.KindRoute, Tool: dispatchRead}, []byte(`{"key":"ACME-1","labels":["ui","api"]}`), false, ""},
		{"a refusal of a list tool", issues, []byte(`{"code":"PROJECT_NOT_FOUND","error":"no project ACME"}`), false, ""},
		{"an answer the server cut", issues, issueList(3)[:40], true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			result := resultText(c.call, agentwire.CallAnswer{Status: http.StatusOK, Body: c.body, Cut: c.cut})
			if !strings.HasPrefix(result, head+c.total) {
				t.Fatalf("the result starts %q, want %q", result[:min(len(result), len(head)+len(c.total)+20)], head+c.total)
			}
			if after := strings.TrimPrefix(result, head+c.total); c.total == "" && strings.HasPrefix(after, "[Total: ") {
				t.Errorf("the result names a total %q, want none", after[:strings.IndexByte(after, '\n')])
			}
			saved, answered := parseResult(result)
			given := c.body
			if len(given) > modelResultLimit {
				given = given[:modelResultLimit]
			}
			if !answered || string(saved.Body) != string(given) || saved.Shortened != (len(c.body) > modelResultLimit) || saved.Cut != c.cut {
				t.Errorf("the result reads back as %d bytes (shortened %t, cut %t), want the %d bytes of the body it gave", len(saved.Body), saved.Shortened, saved.Cut, len(given))
			}
			shown := shownResult(result, time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
			if !strings.HasPrefix(shown, head+c.total) {
				t.Errorf("the model is given a result that starts %q, want %q", shown[:min(len(shown), len(head)+len(c.total))], head+c.total)
			}
		})
	}
}

// TestASavedListOfEitherShapeReadsAsItsAnswer: a saved step of a run before results named a
// total holds the head line and then the body; a saved step since holds the total line between
// them. ReadSavedResult reads the body of each as the server answered it, which draw_block reads,
// and the step's label counts the list's rows from it.
func TestASavedListOfEitherShapeReadsAsItsAnswer(t *testing.T) {
	list := issueList(3)
	label, arguments := stepLabel(toolCallTool, json.RawMessage(`{"tool":"dispatch_issues","arguments":{"project":"ACME"}}`))
	for name, result := range map[string]string{
		"the shape before the total": storedDataNote + "\nstep 2, call 1: status 200\n" + string(list),
		"the shape with the total":   resultText(agentwire.CallRequest{Step: 2, Call: 1, Kind: agentwire.KindRoute, Tool: "dispatch_issues"}, agentwire.CallAnswer{Status: http.StatusOK, Body: list}),
	} {
		t.Run(name, func(t *testing.T) {
			saved, err := ReadSavedResult(savedTool(t, "dispatch_issues", result))
			if err != nil || saved.Tool != "dispatch_issues" || saved.Call != 1 || saved.Status != http.StatusOK || string(saved.Body) != string(list) || saved.Shortened || saved.Cut {
				t.Fatalf("the saved step reads as %+v (%v), want call 1 of dispatch_issues, status 200 and the list whole", saved, err)
			}
			if step := ended(label, arguments, result, false); step.count != 3 || !strings.HasSuffix(step.label, "· 3 issues") {
				t.Errorf("the step's label is %q, counting %d, want the list's 3 issues", step.label, step.count)
			}
		})
	}
}
