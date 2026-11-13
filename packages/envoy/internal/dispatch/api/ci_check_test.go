package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The check run every ci_check of these tests names, and the negative control a check CI proves
// names.
const (
	ciCheckRun             = "go-test"
	ciCheckNegativeControl = "Break the build and watch the run fail."
)

// ciCheckKept is the attributes of a check CI proves that keeps every rule while no result is
// recorded against it, and ciCheckOrdinary those of an ordinary check with the same negative
// control.
const (
	ciCheckKept     = `negative_control="` + ciCheckNegativeControl + `" ci_check="` + ciCheckRun + `"`
	ciCheckOrdinary = `negative_control="` + ciCheckNegativeControl + `"`
)

// ciCheckRecordedRule is the message of the rule a block holding a recorded result breaks.
const ciCheckRecordedRule = "this check already holds a recorded result, so it does not become CI's; write a new check block under a new id and delete this one"

// ciCheckBlock is check block id carrying attributes as markdown writes them.
func ciCheckBlock(id, attributes string) string {
	return ":::check{#" + id + " " + attributes + "}\nThe server builds at the head.\n:::\n"
}

// ciCheckRefusal is the message a write is refused with when check id breaks rule.
func ciCheckRefusal(id, rule string) string {
	return "check " + id + ` (ci_check "` + ciCheckRun + `"): ` + rule
}

// ciCheckBreaks is each rule a check carrying ci_check can break while no result is recorded
// against it: its attributes as markdown writes them and as a retype sets them, and the rule's
// message.
var ciCheckBreaks = []struct {
	name       string
	markdown   string
	attributes map[string]any
	rule       string
}{
	{
		name:       "a run",
		markdown:   `run="go test ./..." ` + ciCheckKept,
		attributes: map[string]any{"run": "go test ./...", "negative_control": ciCheckNegativeControl, "ci_check": ciCheckRun},
		rule:       "a check proved by CI carries no run",
	},
	{
		name:       "a when of after_merge",
		markdown:   `when="after_merge" ` + ciCheckKept,
		attributes: map[string]any{"when": "after_merge", "negative_control": ciCheckNegativeControl, "ci_check": ciCheckRun},
		rule:       "ci_check takes when of before_merge",
	},
	{
		name:       "no negative control",
		markdown:   `ci_check="` + ciCheckRun + `"`,
		attributes: map[string]any{"ci_check": ciCheckRun},
		rule:       "a check proved by CI names its negative control",
	},
}

// uploadVerification uploads content as the issue's verification.md.
func uploadVerification(t *testing.T, handler http.Handler, issueKey, content string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
		"name": "verification.md", "content": content,
	}, "alice")
}

// applyOp edits the document with one operation.
func applyOp(t *testing.T, handler http.Handler, artifactID string, op map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return editDocument(t, handler, artifactID, map[string]any{"ops": []map[string]any{op}})
}

// Each rule a check carrying ci_check keeps is refused on every route that writes a document, 400
// INVALID_CI_CHECK naming the block and the rule, never INVALID_OP on the edit route: an issue
// whose spec holds it, a new document, a replacement, an inserted block, a retype setting its
// attributes and a rebuild from markdown. Nothing of a refused write is stored, and a check that
// keeps every rule is written and reads back with its ci_check.
func TestEveryDocumentWriteRefusesACICheckThatBreaksARule(t *testing.T) {
	handler, _, broken := newBrokenTestServer(t)
	issue := createInteractionIssue(t, handler, "CIC", "CI checks", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckOrdinary))
	id := verification.Artifact.ID
	before := documentMarkdown(t, handler, id)

	for _, test := range ciCheckBreaks {
		t.Run(test.name, func(t *testing.T) {
			added := before + "\n" + ciCheckBlock("c2", test.markdown)
			want := ciCheckRefusal("c2", test.rule)
			expectRefusal(t, "an issue whose spec holds it", createIssueRequest(t, handler, map[string]any{
				"project": "CIC", "title": "Refused", "spec": ciCheckBlock("c2", test.markdown),
			}), http.StatusBadRequest, "INVALID_CI_CHECK", want)
			expectRefusal(t, "a new document holding it", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
				"name": "other.md", "content": ciCheckBlock("c2", test.markdown),
			}, "alice"), http.StatusBadRequest, "INVALID_CI_CHECK", want)
			expectRefusal(t, "a replacement adding it", uploadVerification(t, handler, issue.Key, added),
				http.StatusBadRequest, "INVALID_CI_CHECK", want)
			expectRefusal(t, "an inserted block", applyOp(t, handler, id, map[string]any{
				"op": "insert", "after": "end", "markdown": ciCheckBlock("c2", test.markdown),
			}), http.StatusBadRequest, "INVALID_CI_CHECK", want)
			expectRefusal(t, "a retype setting its attributes", applyOp(t, handler, id, map[string]any{
				"op": "retype", "block": "c1", "type": "check", "attributes": test.attributes,
			}), http.StatusBadRequest, "INVALID_CI_CHECK", ciCheckRefusal("c1", test.rule))
			broken.failNextLoad(id)
			expectRefusal(t, "a rebuild from markdown", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+id+"/rebuild",
				map[string]any{"markdown": added}, "alice"), http.StatusBadRequest, "INVALID_CI_CHECK", want)
			if after := documentMarkdown(t, handler, id); after != before {
				t.Fatalf("verification.md after refused writes = %q, want %q unchanged", after, before)
			}
		})
	}

	if response := applyOp(t, handler, id, map[string]any{
		"op": "insert", "after": "end", "markdown": ciCheckBlock("c2", ciCheckKept),
	}); response.Code != http.StatusOK {
		t.Fatalf("insert a check that keeps every rule: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if check := checkOf(t, readResults(t, handler, issue.Key), "c2"); check.CICheck != ciCheckRun {
		t.Fatalf("the written check reads back %+v, want ci_check %q", check, ciCheckRun)
	}
}

// A check holding a recorded result does not become CI's: giving it a ci_check is refused with the
// fourth rule through a replacement, a retype and a rebuild, and so is a new block written under
// its id once it is deleted, since the results still stand against that id. A ci_check on a fresh
// block id of the same document is written.
func TestACICheckOnACheckHoldingAResultIsRefused(t *testing.T) {
	handler, _, broken := newBrokenTestServer(t)
	issue := createInteractionIssue(t, handler, "CIR", "CI checks", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", ciCheckBlock("c1", ciCheckOrdinary))
	id := verification.Artifact.ID
	if recorded := recordResult(t, handler, issue.Key, resultBody("c1", "pass", headCommit, nil)); recorded.Code != http.StatusCreated {
		t.Fatalf("record a result against c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	before := documentMarkdown(t, handler, id)
	want := ciCheckRefusal("c1", ciCheckRecordedRule)

	expectRefusal(t, "a replacement giving it a ci_check", uploadVerification(t, handler, issue.Key, ciCheckBlock("c1", ciCheckKept)),
		http.StatusBadRequest, "INVALID_CI_CHECK", want)
	expectRefusal(t, "a retype giving it a ci_check", applyOp(t, handler, id, map[string]any{
		"op": "retype", "block": "c1", "type": "check",
		"attributes": map[string]any{"negative_control": ciCheckNegativeControl, "ci_check": ciCheckRun},
	}), http.StatusBadRequest, "INVALID_CI_CHECK", want)
	broken.failNextLoad(id)
	expectRefusal(t, "a rebuild giving it a ci_check", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+id+"/rebuild",
		map[string]any{"markdown": ciCheckBlock("c1", ciCheckKept)}, "alice"), http.StatusBadRequest, "INVALID_CI_CHECK", want)
	if after := documentMarkdown(t, handler, id); after != before {
		t.Fatalf("verification.md after refused writes = %q, want %q unchanged", after, before)
	}

	if response := applyOp(t, handler, id, map[string]any{
		"op": "insert", "after": "end", "markdown": ciCheckBlock("c3", ciCheckKept),
	}); response.Code != http.StatusOK {
		t.Fatalf("a ci_check on a fresh block id: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if check := checkOf(t, readResults(t, handler, issue.Key), "c3"); check.CICheck != ciCheckRun || len(check.Results) != 0 {
		t.Fatalf("the fresh check reads %+v, want ci_check %q and no result", check, ciCheckRun)
	}

	if response := applyOp(t, handler, id, map[string]any{"op": "delete", "block": "c1"}); response.Code != http.StatusOK {
		t.Fatalf("delete c1: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	expectRefusal(t, "a new block under the old id", applyOp(t, handler, id, map[string]any{
		"op": "insert", "after": "end", "markdown": ciCheckBlock("c1", ciCheckKept),
	}), http.StatusBadRequest, "INVALID_CI_CHECK", want)
}

// A block another writer left breaking a rule - a browser's, which settlement repairs - refuses no
// write that leaves it as it is: an edit of another block and a replacement that carries it on
// unchanged are both written. A write that changes it is held to the rules.
func TestAnEditBesideABrokenCICheckIsNotRefused(t *testing.T) {
	documents, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "CIB", "CI checks", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "Intro.\n\n"+ciCheckBlock("c1", ciCheckOrdinary))
	id := verification.Artifact.ID
	writeBrowserDocument(t, documents, id, "Intro.\n\n"+ciCheckBlock("c1", ciCheckBreaks[0].markdown))

	if response := applyOp(t, handler, id, map[string]any{"op": "replace", "find": "Intro.", "with": "Introduction."}); response.Code != http.StatusOK {
		t.Fatalf("an edit beside the broken check: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	current := documentMarkdown(t, handler, id)
	if !strings.Contains(current, `ci_check="`+ciCheckRun+`"`) || !strings.Contains(current, "Introduction.") {
		t.Fatalf("verification.md after the edit = %q, want the edit beside the broken check", current)
	}
	if response := uploadVerification(t, handler, issue.Key, strings.Replace(current, "Introduction.", "Intro again.", 1)); response.Code != http.StatusCreated {
		t.Fatalf("a replacement carrying the broken check on: status=%d body=%s, want 201", response.Code, response.Body.String())
	}

	// The control: a write that changes the broken block meets the rule it breaks.
	expectRefusal(t, "a retype changing the broken check's run", applyOp(t, handler, id, map[string]any{
		"op": "retype", "block": "c1", "type": "check",
		"attributes": map[string]any{"run": "go test ./internal/...", "negative_control": ciCheckNegativeControl, "ci_check": ciCheckRun},
	}), http.StatusBadRequest, "INVALID_CI_CHECK", ciCheckRefusal("c1", ciCheckBreaks[0].rule))
}

// A check carrying neither ci_check nor a run, and one whose ci_check is blank, which names no
// check run, store and take a result as every check did before ci_check.
func TestACheckWithNeitherCICheckNorRunStoresAsToday(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CIN", "Ordinary checks", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#c1}\nThe server builds at the head.\n:::\n\n"+ciCheckBlock("c2", `run="go test ./..." ci_check=" "`))
	read := readResults(t, handler, issue.Key)
	if check := checkOf(t, read, "c1"); check.CICheck != "" || check.Run != "" || check.NegativeControl != "" || check.When != "before_merge" {
		t.Fatalf("the bare check reads %+v, want no ci_check, run or negative control, when before_merge", check)
	}
	if check := checkOf(t, read, "c2"); strings.TrimSpace(check.CICheck) != "" || check.Run != "go test ./..." {
		t.Fatalf("the check with a blank ci_check reads %+v, want its run and a blank ci_check", check)
	}
	for _, id := range []string{"c1", "c2"} {
		if recorded := recordResult(t, handler, issue.Key, resultBody(id, "pass", headCommit, nil)); recorded.Code != http.StatusCreated {
			t.Fatalf("record a result against %s: status=%d body=%s, want 201", id, recorded.Code, recorded.Body.String())
		}
	}
}
