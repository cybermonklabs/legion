package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The fixtures of the form tests (spec_form_*_test.go): the tree of issues under a named root, the
// requests and answers of each approval route, the fixture texts of v-form and of a procedure with
// the refusal that each gives, and the state that a refused write must leave as it was. The route
// assertions are in spec_form_routes_test.go.

// formIssue is an issue the form tests made, with its spec.
type formIssue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// specFormTree is a server whose setting names specRoots, holding the project ACME, its first issue
// ACME-1 and a top issue outside every root.
type specFormTree struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	root     string
	outside  string
}

func newSpecFormTree(t *testing.T, options testServerOptions) specFormTree {
	t.Helper()
	handler, database, deps := newTestServer(t, options)
	return seedSpecFormTree(t, handler, database, deps)
}

// seedSpecFormTree writes the project ACME, its first issue ACME-1, a top issue beside it, and the
// person whose name the person rule looks for.
func seedSpecFormTree(t *testing.T, handler http.Handler, database *store.Store, deps Deps) specFormTree {
	t.Helper()
	seedPeople(t, database, "ada.lovelace@example.com")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACME", "name": "Acme"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project ACME: status=%d body=%s", response.Code, response.Body.String())
	}
	root := createFormIssue(t, handler, "", "The root of the written account", "# The Spec\n\nThe design of the written account.\n")
	if root.Key != "ACME-1" {
		t.Fatalf("the first issue of ACME is %s, want ACME-1", root.Key)
	}
	outside := createFormIssue(t, handler, "", "A tree of some other work", "# Outside\n\nA plan of some other work.\n")
	return specFormTree{handler: handler, database: database, deps: deps, root: root.Key, outside: outside.Key}
}

// createFormIssue creates an issue of ACME under parent ("" for none) whose spec is spec.
func createFormIssue(t *testing.T, handler http.Handler, parent, title, spec string) formIssue {
	t.Helper()
	body := map[string]any{"project": "ACME", "title": title, "spec": spec, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[formIssue](t, response)
}

// uploadFormDocument uploads markdown to issue as the document name, as the asking agent. An
// upload that replaces a document the issue holds names the token of the version it replaces, as
// the route requires (uploadBaseNamed); a first upload names none.
func uploadFormDocument(t *testing.T, handler http.Handler, issue, name, markdown string) model.Artifact {
	t.Helper()
	owner := "/api/v1/issues/" + issue + "/artifacts"
	response := sessionRequest(t, handler, http.MethodPost, owner, map[string]any{
		"name": name, "content": markdown, "actor": sessionActor(),
		"precondition": uploadBaseNamed(t, handler, owner, name),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s to %s: status=%d body=%s", name, issue, response.Code, response.Body.String())
	}
	return decodeBody[artifactUploadResponse](t, response).Artifact
}

// reparentFormIssue moves issue under parent.
func reparentFormIssue(t *testing.T, handler http.Handler, issue, parent string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"parent": parent}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("move %s under %s: status=%d body=%s", issue, parent, response.Code, response.Body.String())
	}
}

// requestFormApproval is the asking agent's approval request of the document.
func requestFormApproval(t *testing.T, handler http.Handler, artifactID string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"actor": sessionActor()}
	for key, value := range extra {
		body[key] = value
	}
	return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", body)
}

// approvalRequestRead is the part of an approval request's answer the form tests read.
type approvalRequestRead struct {
	Ask      *model.Ask       `json:"ask"`
	SpecForm *specFormSummary `json:"spec_form"`
}

// requireFormRequestOpened fails t unless response opened or handed back the request, and
// answers it.
func requireFormRequestOpened(t *testing.T, response *httptest.ResponseRecorder) approvalRequestRead {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	read := decodeBody[approvalRequestRead](t, response)
	if read.Ask == nil {
		t.Fatalf("request approval answered no ask: %s", response.Body.String())
	}
	return read
}

// reviewFormDocument is alice's review of the document from its header.
func reviewFormDocument(t *testing.T, handler http.Handler, artifactID, state, reason string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"state": state}
	if reason != "" {
		body["reason"] = reason
	}
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", body, "alice")
}

// reviewFormVersion is alice's review of version of the document.
func reviewFormVersion(t *testing.T, handler http.Handler, artifactID string, version int, state string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/reviews", artifactID, version),
		map[string]any{"state": state}, "alice")
}

// answerFormApproval is alice's answer to the approval request ask from her Inbox, at the
// question's revision she read.
func answerFormApproval(t *testing.T, handler http.Handler, ask, selected, text string) *httptest.ResponseRecorder {
	t.Helper()
	read := readBlockAsk(t, handler, ask)
	body := map[string]any{"selected": []string{selected}, "expected_edited_at": read.EditedAt}
	if text != "" {
		body["text"] = text
	}
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", body, "alice")
}

// formFault is one of the seven fixture texts of v-form (formFaults), or of the two texts of a
// procedure (procedureFaults), with a clean text of the same node that a new version can break, and
// the rule and detail its finding names. A text that ste-lint reads as a sentence of more than 25
// words has a second finding, ste-lint's long-sentence, whose detail is steLong.
type formFault struct {
	name, node    string
	kind          specform.Kind
	clean, broken string
	rule, detail  string
	steLong       string
}

var formFaults = []formFault{
	{name: "three sentences", node: "store-comment", kind: specform.KindStatement,
		clean:  "Dispatch stores each comment.",
		broken: "Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment.",
		rule:   "sentences", detail: "it has 3 sentences, and a statement has one or two"},
	{name: "a sentence of 26 words", node: "refuse-write", kind: specform.KindStatement,
		clean:  "Dispatch refuses a write that breaks the document schema.",
		broken: "Dispatch refuses a write of the whole text of a document when that text breaks the document schema, and it answers with one named error code.",
		rule:   "sentence-words", detail: `a sentence has 26 words, and a sentence has at most 25: "Dispatch refuses a write of the whole text"`,
		steLong: `ASD-STE100 does not permit a sentence of more than 25 words: "26 words"`},
	{name: "a hard finding of ASD-STE100", node: "check-upload", kind: specform.KindStatement,
		clean:  "Dispatch checks each upload.",
		broken: "Dispatch performs a validation of each upload.",
		rule:   "ste100-nominalization", detail: `ASD-STE100 does not permit an action frozen into a noun: "performs a validation"`},
	{name: "a link to an issue", node: "keep-answer", kind: specform.KindStatement,
		clean:  "Dispatch keeps each answer.",
		broken: "Dispatch keeps each answer, as dispatch://ACME-12 asks.",
		rule:   "dispatch-link", detail: `it links to a record of Dispatch: "dispatch://ACME-12"`},
	{name: "an incident", node: "restart-listener", kind: specform.KindStatement,
		clean:  "Envoy restarts its listener when the listener stops.",
		broken: "Envoy restarts its listener after incident INC-2041.",
		rule:   "incident", detail: `it names an incident: "incident INC-2041". Dispatch reads these forms as the name of an incident: #<n>, number <n>, a key such as INC-<n> and a date`},
	{name: "a pull request", node: "store-message", kind: specform.KindStatement,
		clean:  "Envoy stores each message.",
		broken: "Envoy stores each message, as pull request #4031 makes it do.",
		rule:   "pull-request", detail: `it names a pull request: "pull request #4031". Dispatch reads these forms as the name of a pull request: #<n>, number <n>, <owner>/<repo>#<n> and its address`},
	{name: "a person in a claim", node: "approve-change", kind: specform.KindClaim,
		clean:  "Each change waits for its approval before it lands.",
		broken: "Ada Lovelace approves each change before it lands.",
		rule:   "person", detail: `it names a person: "Ada Lovelace"`},
}

// procedureFaults are the two texts of the procedure of a check that R49 refuses beside the seven
// of v-form, as ASD-STE100 writes an instruction: a sentence of 21 words, and a sentence of two
// instructions. Each stands in the run of a check.
var procedureFaults = []formFault{
	{name: "a procedure of 21 words", node: "run-merge", kind: specform.KindProcedure,
		clean:  "Run the merge test.",
		broken: "Run the merge test against a fixture server that holds two approved specs of the same issue and three open requests.",
		rule:   "sentence-words", detail: `a sentence has 21 words, and an instruction has at most 20: "Run the merge test against a fixture server"`},
	{name: "a procedure of two instructions", node: "read-requests", kind: specform.KindProcedure,
		clean:  "Read each open approval request.",
		broken: "Read each open approval request and record its version.",
		rule:   "instructions", detail: `a sentence gives two instructions, and a sentence of a procedure gives one: "record its version"`},
}

// twentyWords is the control of the procedure faults: a procedure of one instruction in 20 words.
const twentyWords = "Run the merge test against a fixture server that holds two approved specs of the same issue and three requests."

// document is a spec that holds the fault's node with body: a requirement block for a statement,
// a check block for a claim, and a check block whose run is body for a procedure.
func (f formFault) document(body string) string {
	switch f.kind {
	case specform.KindClaim:
		return fmt.Sprintf("# Spec\n\n:::check{#%s}\n%s\n:::\n", f.node, body)
	case specform.KindProcedure:
		return fmt.Sprintf("# Spec\n\n:::check{#%s run=%q}\nDispatch keeps each record.\n:::\n", f.node, body)
	}
	return fmt.Sprintf("# Spec\n\n:::requirement{#%s}\n%s\n:::\n", f.node, body)
}

// revise writes the broken text over the clean text of the issue's spec as its second version,
// as the asking agent: an edit of the text, or for a procedure, which no edit reaches in the run
// of its check, an upload of the whole spec.
func (f formFault) revise(t *testing.T, handler http.Handler, issue formIssue) {
	t.Helper()
	if f.kind != specform.KindProcedure {
		reviseDocument(t, handler, issue.PrimaryArtifactID, f.clean, f.broken, 2)
		return
	}
	uploadFormDocument(t, handler, issue.Key, "spec.md", f.document(f.broken))
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
}

// lines is the refusal's line of each finding of the broken text at version of spec.md: its
// detail.
func (f formFault) lines(version int) []string {
	lines := []string{fmt.Sprintf("- %s %s in spec.md version %d: %s.", f.kind, f.node, version, f.detail)}
	if f.steLong != "" {
		lines = append(lines, fmt.Sprintf("- %s %s in spec.md version %d: %s.", f.kind, f.node, version, f.steLong))
	}
	return lines
}

// findings is each finding the broken text at version of the issue's spec gives.
func (f formFault) findings(issue formIssue, version int) []specFormFinding {
	finding := func(rule specform.Rule, detail string) specFormFinding {
		return specFormFinding{
			Document: "spec.md", ArtifactID: issue.PrimaryArtifactID, Version: version,
			Finding: specform.Finding{
				Node: f.node, Kind: f.kind, Where: fmt.Sprintf("spec.md version %d", version),
				Rule: rule, Detail: detail, Excerpt: f.broken,
			},
		}
	}
	findings := []specFormFinding{finding(specform.Rule(f.rule), f.detail)}
	if f.steLong != "" {
		findings = append(findings, finding("ste100-long-sentence", f.steLong))
	}
	return findings
}

// requireFormRefusal fails t unless response is the 409 SPEC_FORM refusal of act, naming exactly
// want, each by its node and rule, in that order. Its head counts the texts that want names, and
// findings_total counts want. error is refusal, then next_step, the next step of act, on a line
// of its own.
func requireFormRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, act specform.Act, lines []string, want ...specFormFinding) specFormError {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("%s: status=%d body=%s, want 409 SPEC_FORM", what, response.Code, response.Body.String())
	}
	body := decodeBody[specFormError](t, response)
	if body.Code != "SPEC_FORM" || body.FindingsTotal != len(want) || !reflect.DeepEqual(body.Findings, want) {
		t.Fatalf("%s: refusal = code %q, total %d, findings %#v; want SPEC_FORM, %d, %#v", what, body.Code, body.FindingsTotal, body.Findings, len(want), want)
	}
	members := decodeBody[map[string]any](t, response)
	refusal, _ := members["refusal"].(string)
	nextStep, _ := members["next_step"].(string)
	wantStep := map[bool]string{true: "To send these findings to the author, choose Request changes.", false: "Fix each text, then request approval again."}[act == specform.ActApproval]
	if nextStep != wantStep || refusal+"\n"+nextStep != body.Error {
		t.Fatalf("%s: refusal %q and next_step %q, want error in its two parts and the next step %q; error =\n%s", what, refusal, nextStep, wantStep, body.Error)
	}
	texts := map[string]bool{}
	for _, finding := range want {
		texts[fmt.Sprintf("%s version %d: %s %s", finding.Document, finding.Version, finding.Kind, finding.Node)] = true
	}
	count := "1 text breaks"
	if len(texts) != 1 {
		count = fmt.Sprintf("%d texts break", len(texts))
	}
	listed := strings.Join(lines, "\n")
	switch act {
	case specform.ActApproval:
		if wantError := "Dispatch refused this approval, because " + count + " the form of The Spec.\n" + listed +
			"\nTo send these findings to the author, choose Request changes."; body.Error != wantError {
			t.Fatalf("%s: error =\n%s\nwant\n%s", what, body.Error, wantError)
		}
		if wantReason := "Dispatch refused my approval, because " + count + " the form of The Spec.\n" + listed +
			"\nFix each text, then request approval again."; body.ChangesReason != wantReason {
			t.Fatalf("%s: changes_reason =\n%s\nwant\n%s", what, body.ChangesReason, wantReason)
		}
	default:
		if wantError := "Dispatch refused this approval request, because " + count + " the form of The Spec.\n" + listed +
			"\nFix each text, then request approval again."; body.Error != wantError {
			t.Fatalf("%s: error =\n%s\nwant\n%s", what, body.Error, wantError)
		}
		if strings.Contains(response.Body.String(), `"changes_reason"`) {
			t.Fatalf("%s: a request's refusal carries changes_reason: %s", what, response.Body.String())
		}
	}
	return body
}

// formState is what a refused write must leave as it was: every event of the issue, every review
// of its documents and every ask of it.
type formState struct {
	events, reviews, asks int
}

func readFormState(t *testing.T, database *store.Store, issue string) formState {
	t.Helper()
	var state formState
	if err := database.Pool.QueryRow(context.Background(), `
		select
			(select count(*) from events where issue_key = $1),
			(select count(*) from artifact_reviews r join artifacts d on d.id = r.artifact_id where d.issue_key = $1),
			(select count(*) from asks where issue_key = $1)
	`, issue).Scan(&state.events, &state.reviews, &state.asks); err != nil {
		t.Fatalf("read the state of %s: %v", issue, err)
	}
	return state
}

// lastIssueSeq is the sequence of issue's newest event.
func lastIssueSeq(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var seq int
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from events where issue_key = $1`, issue).Scan(&seq); err != nil {
		t.Fatalf("read the newest event of %s: %v", issue, err)
	}
	return seq
}

// askEventsAfter counts the eventType events of issue after its event seq that name ask.
func askEventsAfter(t *testing.T, database *store.Store, issue string, seq int, ask, eventType string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and seq > $2 and type = $3 and payload->>'id' = $4
	`, issue, seq, eventType, ask).Scan(&count); err != nil {
		t.Fatalf("count the %s events of %s: %v", eventType, ask, err)
	}
	return count
}

// restoreReason is the reason a reopen or the repair pass records on a request it leaves resolved,
// for one finding line.
func restoreReason(line string) string {
	return "Dispatch did not open this approval request again, because 1 text breaks the form of The Spec.\n" + line +
		"\nFix each text, then request approval again."
}

// reopenFormIssue closes issue and reopens it through the route a person uses, and answers the
// sequence of its newest event before the reopen.
func reopenFormIssue(t *testing.T, tree specFormTree, issue string) int {
	t.Helper()
	closeIssue(t, tree.handler, issue)
	seq := lastIssueSeq(t, tree.database, issue)
	if response := dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reopen %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	return seq
}

// requireRestoredOpen fails t unless ask reads open after the reopen at seq, with one ask.opened.
func requireRestoredOpen(t *testing.T, tree specFormTree, issue string, seq int, ask string) {
	t.Helper()
	if got := readBlockAsk(t, tree.handler, ask); got.State != "open" || got.Resolution != nil {
		t.Fatalf("ask %s after the reopen = %s %#v, want open", ask, got.State, got.Resolution)
	}
	if opened := askEventsAfter(t, tree.database, issue, seq, ask, "ask.opened"); opened != 1 {
		t.Fatalf("ask %s has %d ask.opened events after the reopen, want 1", ask, opened)
	}
}

// requireLeftResolved fails t unless the approval request ask reads resolved after the reopen or
// repair at seq, with the restore's reason naming line, actor, one ask.resolved and no ask.opened.
func requireLeftResolved(t *testing.T, handler http.Handler, database *store.Store, issue string, seq int, ask, line string, actor model.Actor) {
	t.Helper()
	got := readBlockAsk(t, handler, ask)
	if got.State != "resolved" || got.Resolution == nil || got.Resolution.Kind != "resolved" {
		t.Fatalf("request %s = %s %#v, want resolved with kind resolved", ask, got.State, got.Resolution)
	}
	if got.Resolution.Reason != restoreReason(line) {
		t.Fatalf("request %s reason =\n%s\nwant\n%s", ask, got.Resolution.Reason, restoreReason(line))
	}
	if got.Resolution.Actor.Kind != actor.Kind || got.Resolution.Actor.ID != actor.ID {
		t.Fatalf("request %s resolved by %#v, want %#v", ask, got.Resolution.Actor, actor)
	}
	if opened := askEventsAfter(t, database, issue, seq, ask, "ask.opened"); opened != 0 {
		t.Fatalf("request %s has %d ask.opened events after the reopen, want none", ask, opened)
	}
	if resolved := askEventsAfter(t, database, issue, seq, ask, "ask.resolved"); resolved != 1 {
		t.Fatalf("request %s has %d ask.resolved events after the reopen, want 1", ask, resolved)
	}
}
