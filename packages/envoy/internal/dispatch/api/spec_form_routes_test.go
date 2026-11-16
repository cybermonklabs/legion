package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Dispatch refuses an approval request, and a person's approval, of a covered document whose text
// breaks the form of The Spec (R49), on every route that opens an approval request or answers one,
// and a reopen and the repair pass leave such a request resolved rather than open it again. The
// server's setting names the roots of The Spec (DISPATCH_SPEC_ROOTS); these tests name ACME-1.

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

// uploadFormDocument uploads markdown to issue as the document name, as the asking agent.
func uploadFormDocument(t *testing.T, handler http.Handler, issue, name, markdown string) model.Artifact {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{
		"name": name, "content": markdown, "actor": sessionActor(),
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

// formFault is one of the seven fixture texts of v-form, with a clean text of the same node that
// an edit can break, and the rule and detail its finding names. A fault of a definition (v-terms)
// holds whole term lines: its clean line defines another term, or the same term in one sentence,
// and excerpt is the definition its broken line gives, which the finding quotes.
type formFault struct {
	name, node    string
	kind          specform.Kind
	clean, broken string
	excerpt       string
	rule, detail  string
}

var formFaults = []formFault{
	{name: "three sentences", node: "store-comment", kind: specform.KindStatement,
		clean:  "Dispatch stores each comment.",
		broken: "Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment.",
		rule:   "sentences", detail: "it has 3 sentences, and a statement has one or two"},
	{name: "a sentence of 26 words", node: "refuse-write", kind: specform.KindStatement,
		clean:  "Dispatch refuses a write that breaks the document schema.",
		broken: "Dispatch refuses a write of the whole text of a document when that text breaks the document schema, and it answers with one named error code.",
		rule:   "sentence-words", detail: `a sentence has 26 words, and a sentence has at most 25: "Dispatch refuses a write of the whole text"`},
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

// document is a spec that holds the fault's node with body: a requirement block for a statement,
// a check block for a claim, and the line body under a heading Terms for a definition or a line.
func (f formFault) document(body string) string {
	if f.kind == specform.KindDefinition || f.kind == specform.KindLine {
		return specWithTerms(body)
	}
	block := "requirement"
	if f.kind == specform.KindClaim {
		block = "check"
	}
	return fmt.Sprintf("# Spec\n\n:::%s{#%s}\n%s\n:::\n", block, f.node, body)
}

// line is the refusal's line for the broken text at version of spec.md.
func (f formFault) line(version int) string {
	return fmt.Sprintf("- %s %s in spec.md version %d: %s.", f.kind, f.node, version, f.detail)
}

// finding is the finding the broken text at version of the issue's spec gives.
func (f formFault) finding(issue formIssue, version int) specFormFinding {
	return specFormFinding{
		Document: "spec.md", ArtifactID: issue.PrimaryArtifactID, Version: version,
		Finding: specform.Finding{
			Node: f.node, Kind: f.kind, Where: fmt.Sprintf("spec.md version %d", version),
			Rule: specform.Rule(f.rule), Detail: f.detail, Excerpt: cmp.Or(f.excerpt, f.broken),
		},
	}
}

// requireFormRefusal fails t unless response is the 409 SPEC_FORM refusal of act, naming exactly
// want, each by its node and rule, in that order. Its head counts the texts that want names, and
// findings_total counts want.
func requireFormRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, act specform.Act, lines []string, want ...specFormFinding) specFormError {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("%s: status=%d body=%s, want 409 SPEC_FORM", what, response.Code, response.Body.String())
	}
	body := decodeBody[specFormError](t, response)
	if body.Code != "SPEC_FORM" || body.FindingsTotal != len(want) || !reflect.DeepEqual(body.Findings, want) {
		t.Fatalf("%s: refusal = code %q, total %d, findings %#v; want SPEC_FORM, %d, %#v", what, body.Code, body.FindingsTotal, body.Findings, len(want), want)
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

// Each of the seven fixture texts of v-form, on each of the six paths to a person's approval, is
// refused with its node and rule, and leaves nothing written (requireEveryApprovalRouteRefuses).
func TestEveryApprovalRouteRefusesABreakOfTheForm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	for _, fault := range formFaults {
		t.Run(fault.name, func(t *testing.T) { requireEveryApprovalRouteRefuses(t, tree, fault) })
	}
	// The controls of path 6: a reopen opens the broken request again where the form does not
	// cover it, so a build that left every approval request resolved fails here.
	fault := formFaults[0]
	t.Run("path 6 control: the issue stays outside the root", func(t *testing.T) {
		outside := createFormIssue(t, tree.handler, tree.outside, "Reopened outside the root", fault.document(fault.broken))
		request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, outside.PrimaryArtifactID, nil)).Ask.ID
		seq := reopenFormIssue(t, tree, outside.Key)
		requireRestoredOpen(t, tree, outside.Key, seq, request)
	})
	t.Run("path 6 control: the server names no root", func(t *testing.T) {
		unnamed := newSpecFormTree(t, testServerOptions{})
		issue := createFormIssue(t, unnamed.handler, unnamed.outside, "Reopened with no root named", fault.document(fault.broken))
		request := requireFormRequestOpened(t, requestFormApproval(t, unnamed.handler, issue.PrimaryArtifactID, nil)).Ask.ID
		reparentFormIssue(t, unnamed.handler, issue.Key, unnamed.root)
		seq := reopenFormIssue(t, unnamed, issue.Key)
		requireRestoredOpen(t, unnamed, issue.Key, seq, request)
	})
}

// requireEveryApprovalRouteRefuses fails t unless the broken text of fault, under the root of
// tree, is refused with its node and rule on each of the six paths to a person's approval, and
// leaves nothing written: (1) open a request; (2) hand a moved request back; (3) approve from the
// header and (4) approve a named version, each with no request open and on a request an edit moved
// to the broken version; (5) approve from the Inbox, on that moved request and on one opened
// outside the root and then moved under it; and (6) reopen the issue, which leaves the request
// resolved with the findings and opens every other ask.
func requireEveryApprovalRouteRefuses(t *testing.T, tree specFormTree, fault formFault) {
	t.Helper()
	// The broken text in the spec's first version, with no request open: paths 1, 3 and 4.
	first := createFormIssue(t, tree.handler, tree.root, "Broken at first: "+fault.name, fault.document(fault.broken))
	awaitSettled(t, tree.database, first.PrimaryArtifactID)
	before := readFormState(t, tree.database, first.Key)
	lines := []string{fault.line(1)}
	requireFormRefusal(t, "path 1, open a request", requestFormApproval(t, tree.handler, first.PrimaryArtifactID, nil),
		specform.ActRequest, lines, fault.finding(first, 1))
	requireFormRefusal(t, "path 3 with no request open", reviewFormDocument(t, tree.handler, first.PrimaryArtifactID, "approved", ""),
		specform.ActApproval, lines, fault.finding(first, 1))
	requireFormRefusal(t, "path 4 with no request open", reviewFormVersion(t, tree.handler, first.PrimaryArtifactID, 1, "approved"),
		specform.ActApproval, lines, fault.finding(first, 1))
	if after := readFormState(t, tree.database, first.Key); after != before {
		t.Fatalf("the refused writes on the first version changed %+v to %+v", before, after)
	}

	// A request on a clean first version, which an edit moves to the broken second: paths 2, 3, 4
	// and 5.
	moved := createFormIssue(t, tree.handler, tree.root, "Broken by an edit: "+fault.name, fault.document(fault.clean))
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, moved.PrimaryArtifactID, nil)).Ask.ID
	reviseDocument(t, tree.handler, moved.PrimaryArtifactID, fault.clean, fault.broken, 2)
	awaitSettled(t, tree.database, moved.PrimaryArtifactID)
	before = readFormState(t, tree.database, moved.Key)
	lines = []string{fault.line(2)}
	requireFormRefusal(t, "path 2, hand the moved request back", requestFormApproval(t, tree.handler, moved.PrimaryArtifactID, nil),
		specform.ActRequest, lines, fault.finding(moved, 2))
	requireFormRefusal(t, "path 3 on the moved request", reviewFormDocument(t, tree.handler, moved.PrimaryArtifactID, "approved", ""),
		specform.ActApproval, lines, fault.finding(moved, 2))
	requireFormRefusal(t, "path 4 on the moved request", reviewFormVersion(t, tree.handler, moved.PrimaryArtifactID, 2, "approved"),
		specform.ActApproval, lines, fault.finding(moved, 2))
	requireFormRefusal(t, "path 5 on the moved request", answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""),
		specform.ActApproval, lines, fault.finding(moved, 2))
	if after := readFormState(t, tree.database, moved.Key); after != before {
		t.Fatalf("the refused writes on the moved request changed %+v to %+v", before, after)
	}
	if got := readBlockAsk(t, tree.handler, ask); got.State != "open" {
		t.Fatalf("the moved request = %s, want it still open", got.State)
	}

	// A request opened while the issue stood outside the root, then moved under it: path 5.
	reparented := createFormIssue(t, tree.handler, tree.outside, "Moved under the root: "+fault.name, fault.document(fault.broken))
	opened := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, reparented.PrimaryArtifactID, nil))
	if opened.SpecForm != nil {
		t.Fatalf("a request outside the root answered spec_form %#v, want none", opened.SpecForm)
	}
	reparentFormIssue(t, tree.handler, reparented.Key, tree.root)
	awaitSettled(t, tree.database, reparented.PrimaryArtifactID)
	before = readFormState(t, tree.database, reparented.Key)
	requireFormRefusal(t, "path 5 on a request moved under the root", answerFormApproval(t, tree.handler, opened.Ask.ID, docs.ApprovalOptionApprove, ""),
		specform.ActApproval, []string{fault.line(1)}, fault.finding(reparented, 1))
	if after := readFormState(t, tree.database, reparented.Key); after != before {
		t.Fatalf("the refused answer changed %+v to %+v", before, after)
	}

	// The same on a copy that also holds a question, closed and reopened: path 6.
	reopened := createFormIssue(t, tree.handler, tree.outside, "Reopened under the root: "+fault.name, fault.document(fault.broken))
	request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, reopened.PrimaryArtifactID, nil)).Ask.ID
	question := createSessionAsk(t, tree.handler, reopened.Key, "Which region takes the first wave?")
	reparentFormIssue(t, tree.handler, reopened.Key, tree.root)
	seq := reopenFormIssue(t, tree, reopened.Key)
	requireLeftResolved(t, tree.handler, tree.database, reopened.Key, seq, request, fault.line(1), model.Actor{Kind: "user", ID: "alice"})
	requireRestoredOpen(t, tree, reopened.Key, seq, question)
	reviews := readFormState(t, tree.database, reopened.Key).reviews
	answered := answerFormApproval(t, tree.handler, request, docs.ApprovalOptionApprove, "")
	if answered.Code != http.StatusConflict || strings.Contains(answered.Body.String(), "SPEC_FORM") {
		t.Fatalf("approve the request the reopen left resolved: status=%d body=%s, want 409 for a request that is not open",
			answered.Code, answered.Body.String())
	}
	if after := readFormState(t, tree.database, reopened.Key).reviews; after != reviews {
		t.Fatalf("the refused answer wrote %d reviews", after-reviews)
	}
}

// The repair pass, which restores the asks of an issue an older release reopened, leaves a broken
// approval request resolved with the findings, as a reopen does, and a second pass finds nothing
// left. The control: a clean request is restored open.
func TestTheRepairPassDoesNotOpenABrokenRequestAgain(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		broken bool
	}{
		{name: "a broken request", body: formFaults[0].broken, broken: true},
		{name: "control: a clean request", body: formFaults[0].clean},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRepairFixtureFrom(t, 20*time.Millisecond, "ACME-1")
			tree := seedSpecFormTree(t, f.handler, f.database, f.deps)
			withdrawn := withdrawnByAnOlderRelease(t, f, tree, formFaults[0].document(test.body))

			repair := repairNow(t, f)
			if !test.broken {
				if repair.Restored != 1 || repair.LeftResolved != 0 {
					t.Fatalf("repair = %+v, want 1 restored and none left resolved", repair)
				}
				requireRestoredOpen(t, tree, withdrawn.issue, withdrawn.seq, withdrawn.request)
				return
			}
			if repair.Restored != 0 || repair.LeftResolved != 1 {
				t.Fatalf("repair = %+v, want none restored and 1 left resolved", repair)
			}
			requireLeftResolved(t, f.handler, f.database, withdrawn.issue, withdrawn.seq, withdrawn.request, formFaults[0].line(1), withdrawn.reopener)
			if again := repairNow(t, f); again != (IssueAskRepair{}) {
				t.Fatalf("second repair = %+v, want nothing found", again)
			}
		})
	}
}

// withdrawnRequest is an approval request that a close withdrew and that a reopen of an older
// release did not restore: its issue, the ask, the sequence of the issue's newest event before
// the repair, and the actor of the reopen.
type withdrawnRequest struct {
	issue, request string
	seq            int
	reopener       model.Actor
}

// withdrawnByAnOlderRelease opens an approval request of a new issue whose spec is document,
// while the issue stands outside the root of tree, moves the issue under the root, closes it and
// reopens it as an older release did, without restoring its asks.
func withdrawnByAnOlderRelease(t *testing.T, f repairFixture, tree specFormTree, document string) withdrawnRequest {
	t.Helper()
	issue := createFormIssue(t, f.handler, tree.outside, "Reopened by an older release", document)
	request := requireFormRequestOpened(t, requestFormApproval(t, f.handler, issue.PrimaryArtifactID, nil)).Ask.ID
	reparentFormIssue(t, f.handler, issue.Key, tree.root)
	closeIssue(t, f.handler, issue.Key)
	reopener := repairActor("session-reopener")
	reopenWithoutRestoring(t, f, issue.Key, "todo", reopener)
	return withdrawnRequest{issue: issue.Key, request: request, seq: lastIssueSeq(t, f.database, issue.Key), reopener: reopener}
}

// A request for changes is never gated: the person sends the refusal's changes_reason to the
// author, who reads it as the answer to the request, or, with no request open, in the review's
// reason, on the document's reviews and in the issue's artifact.changes_requested event.
func TestARequestForChangesIsNotGated(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	fault := formFaults[0]
	for _, path := range []string{"path 3", "path 5"} {
		t.Run(path, func(t *testing.T) {
			issue := createFormIssue(t, tree.handler, tree.root, "Changes asked on "+path, fault.document(fault.clean))
			ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)).Ask.ID
			reviseDocument(t, tree.handler, issue.PrimaryArtifactID, fault.clean, fault.broken, 2)
			var refused, sent *httptest.ResponseRecorder
			if path == "path 3" {
				refused = reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", "")
			} else {
				refused = answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, "")
			}
			reason := requireFormRefusal(t, path+" approve", refused, specform.ActApproval, []string{fault.line(2)}, fault.finding(issue, 2)).ChangesReason
			if path == "path 3" {
				if sent = reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "changes_requested", reason); sent.Code != http.StatusCreated {
					t.Fatalf("request changes: status=%d body=%s, want 201", sent.Code, sent.Body.String())
				}
			} else if sent = answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionRequestChanges, reason); sent.Code != http.StatusOK {
				t.Fatalf("answer Request changes: status=%d body=%s, want 200", sent.Code, sent.Body.String())
			}
			// The author reads the request's answer.
			read := readAuthorAsk(t, tree.handler, ask)
			if read.Answer == nil || !reflect.DeepEqual(read.Answer.Selected, []string{docs.ApprovalOptionRequestChanges}) ||
				read.Answer.Text == nil || *read.Answer.Text != reason {
				t.Fatalf("the author reads the answer %#v, want Request changes with the reason %q", read.Answer, reason)
			}
		})
	}
	t.Run("path 3 with no request open", func(t *testing.T) {
		issue := createFormIssue(t, tree.handler, tree.root, "Changes asked with no request", fault.document(fault.broken))
		reason := requireFormRefusal(t, "approve", reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", ""),
			specform.ActApproval, []string{fault.line(1)}, fault.finding(issue, 1)).ChangesReason
		sent := reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "changes_requested", reason)
		if sent.Code != http.StatusCreated {
			t.Fatalf("request changes: status=%d body=%s, want 201", sent.Code, sent.Body.String())
		}
		review := decodeBody[model.ArtifactReview](t, sent)
		if review.AskID != nil || review.Reason == nil || *review.Reason != reason {
			t.Fatalf("review = %#v, want no ask and the reason %q", review, reason)
		}
		listed := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", nil, "alice")
		if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), jsonString(t, reason)) {
			t.Fatalf("the document's reviews: status=%d body=%s, want the reason", listed.Code, listed.Body.String())
		}
		var payload string
		if err := tree.database.Pool.QueryRow(context.Background(), `
			select payload::text from events where issue_key = $1 and type = 'artifact.changes_requested' order by seq desc limit 1
		`, issue.Key).Scan(&payload); err != nil {
			t.Fatalf("read the artifact.changes_requested event: %v", err)
		}
		if !strings.Contains(payload, jsonString(t, reason)) {
			t.Fatalf("artifact.changes_requested payload = %s, want the reason", payload)
		}
	})
}

// readAuthorAsk is the ask as its author, the asking agent, reads it.
func readAuthorAsk(t *testing.T, handler http.Handler, ask string) model.Ask {
	t.Helper()
	read := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("the author reads ask %s: status=%d body=%s", ask, read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask
}

// jsonString is value as a JSON string literal, without its quotes, as a body carries it.
func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := encodeJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(string(encoded), `"`)
}

// twentyFiveWords is the control of v-form: two sentences of 25 words each, on what Dispatch shows
// the person, which no rule refuses.
const twentyFiveWords = "Dispatch shows the person each question of each issue that the person follows, with the time of the question and the name of its project. " +
	"Dispatch shows the person each approval request that waits on the person, with the document, its version and the short summary that its agent wrote."

// Every route takes the control of v-form: a statement of two sentences of 25 words each.
func TestAnApprovalRouteTakesAStatementOfTwoSentencesOfTwentyFiveWords(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	statement := formFault{node: "show-person", kind: specform.KindStatement, clean: "Dispatch shows the person each question.", broken: twentyFiveWords}

	first := createFormIssue(t, tree.handler, tree.root, "Path 1 control", statement.document(twentyFiveWords))
	read := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, first.PrimaryArtifactID, nil))
	if read.SpecForm == nil || read.SpecForm.Root != "ACME-1" || read.SpecForm.Texts[specform.KindStatement] != 1 {
		t.Fatalf("spec_form = %#v, want root ACME-1 and one statement read", read.SpecForm)
	}

	moved := createFormIssue(t, tree.handler, tree.root, "Path 2 control", statement.document(statement.clean))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, moved.PrimaryArtifactID, nil))
	reviseDocument(t, tree.handler, moved.PrimaryArtifactID, statement.clean, twentyFiveWords, 2)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, moved.PrimaryArtifactID, nil))

	header := createFormIssue(t, tree.handler, tree.root, "Path 3 control", statement.document(twentyFiveWords))
	if response := reviewFormDocument(t, tree.handler, header.PrimaryArtifactID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("path 3: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	versioned := createFormIssue(t, tree.handler, tree.root, "Path 4 control", statement.document(twentyFiveWords))
	if response := reviewFormVersion(t, tree.handler, versioned.PrimaryArtifactID, 1, "approved"); response.Code != http.StatusCreated {
		t.Fatalf("path 4: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	inbox := createFormIssue(t, tree.handler, tree.root, "Path 5 control", statement.document(statement.clean))
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, inbox.PrimaryArtifactID, nil)).Ask.ID
	reviseDocument(t, tree.handler, inbox.PrimaryArtifactID, statement.clean, twentyFiveWords, 2)
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("path 5: status=%d body=%s, want 200", response.Code, response.Body.String())
	}

	reopened := createFormIssue(t, tree.handler, tree.outside, "Path 6 control", statement.document(twentyFiveWords))
	request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, reopened.PrimaryArtifactID, nil)).Ask.ID
	reparentFormIssue(t, tree.handler, reopened.Key, tree.root)
	seq := reopenFormIssue(t, tree, reopened.Key)
	requireRestoredOpen(t, tree, reopened.Key, seq, request)
	if response := answerFormApproval(t, tree.handler, request, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("path 6: approve the restored request: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// specFormReadOf is GET /api/v1/artifacts/{id}/spec-form as alice reads it.
func specFormReadOf(t *testing.T, handler http.Handler, artifactID string) specFormRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/spec-form", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the form of %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	return decodeBody[specFormRead](t, response)
}

// requireScope fails t unless read answers scope and root (nil for none).
func requireScope(t *testing.T, read specFormRead, scope string, root *string) {
	t.Helper()
	if read.Scope != scope || !reflect.DeepEqual(read.Root, root) {
		t.Fatalf("read = scope %q, root %v; want %q, %v", read.Scope, ptrText(read.Root), scope, ptrText(root))
	}
}

func ptrText(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// A server whose setting names no root covers nothing: a three-sentence spec is approved, and the
// read says no root is named.
func TestNoRootNamedCoversNothing(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{})
	issue := createFormIssue(t, tree.handler, tree.root, "Under an unnamed root", formFaults[0].document(formFaults[0].broken))
	if read := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)); read.SpecForm != nil {
		t.Fatalf("spec_form = %#v, want none", read.SpecForm)
	}
	read := specFormReadOf(t, tree.handler, issue.PrimaryArtifactID)
	requireScope(t, read, "no_root", nil)
	if read.InScope || read.RefusesApproval || read.FindingsTotal != 1 {
		t.Fatalf("read = %#v, want out of scope, refusing nothing, with its one finding", read)
	}
}

// A tree with a terms.md of its own is not The Spec: only the setting names a root.
func TestAnUnrelatedTreeWithTermsMdIsNotRefused(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	terms := uploadFormDocument(t, tree.handler, tree.outside, "terms.md", "# Terms\n\n- **work unit**: A piece of work that one agent does.\n")
	if response := reviewFormDocument(t, tree.handler, terms.ID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("approve the other tree's terms.md: status=%d body=%s", response.Code, response.Body.String())
	}
	child := createFormIssue(t, tree.handler, tree.outside, "Under the other tree", formFaults[0].document(formFaults[0].broken))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, child.PrimaryArtifactID, nil))
	requireScope(t, specFormReadOf(t, tree.handler, child.PrimaryArtifactID), "outside", nil)
}

// A covered issue's notes.md is not its spec or its Verification document, so it is not judged.
func TestACoveredLevelsNotesAreNotRefused(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	level := createFormIssue(t, tree.handler, tree.root, "A level with notes", "# Level\n\nThe level.\n")
	notes := uploadFormDocument(t, tree.handler, level.Key, "notes.md", formFaults[0].document(formFaults[0].broken))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, notes.ID, nil))
	read := specFormReadOf(t, tree.handler, notes.ID)
	requireScope(t, read, "not_spec", new("ACME-1"))
	if read.Plays != "" {
		t.Fatalf("notes.md plays %q, want nothing", read.Plays)
	}
}

// A root's own spec holds the design, which is not written in the form, so it is not judged.
func TestTheRootsOwnDocumentsAreNotRefused(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{specRoots: "ACME-1"})
	seedPeople(t, database, "ada.lovelace@example.com")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACME", "name": "Acme"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project ACME: status=%d body=%s", response.Code, response.Body.String())
	}
	root := createFormIssue(t, handler, "", "A root that breaks the form", formFaults[0].document(formFaults[0].broken))
	requireFormRequestOpened(t, requestFormApproval(t, handler, root.PrimaryArtifactID, nil))
	requireScope(t, specFormReadOf(t, handler, root.PrimaryArtifactID), "root", new("ACME-1"))
}

// The nearest named root decides: a named root inside another covers its own subtree, and its own
// spec is not covered.
func TestTheNearestNamedRootDecides(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1,ACME-5"})
	for _, filler := range []string{"Third issue", "Fourth issue"} {
		createFormIssue(t, tree.handler, "", filler, "# Filler\n\n"+filler+".\n")
	}
	inner := createFormIssue(t, tree.handler, tree.root, "The inner root", formFaults[0].document(formFaults[0].broken))
	leaf := createFormIssue(t, tree.handler, inner.Key, "Under the inner root", formFaults[0].document(formFaults[0].clean))
	if inner.Key != "ACME-5" || leaf.Key != "ACME-6" {
		t.Fatalf("the inner root and its child are %s and %s, want ACME-5 and ACME-6", inner.Key, leaf.Key)
	}
	read := specFormReadOf(t, tree.handler, leaf.PrimaryArtifactID)
	requireScope(t, read, "covered", new("ACME-5"))
	requireScope(t, specFormReadOf(t, tree.handler, inner.PrimaryArtifactID), "root", new("ACME-5"))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, inner.PrimaryArtifactID, nil))
}

// A reparent out of the root takes the issue's documents out of scope, which the read shows.
func TestAReparentOutOfTheRootShowsOnTheRead(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "Moved out of the root", formFaults[0].document(formFaults[0].broken))
	requireScope(t, specFormReadOf(t, tree.handler, issue.PrimaryArtifactID), "covered", new("ACME-1"))
	reparentFormIssue(t, tree.handler, issue.Key, tree.outside)
	requireScope(t, specFormReadOf(t, tree.handler, issue.PrimaryArtifactID), "outside", nil)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
}

// A version write moves an open request to the new version, and the person's Approve of it judges
// that version: refused, with no review written and the request still open.
func TestAMovedRequestIsRefusedAtTheApproval(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	fault := formFaults[0]
	issue := createFormIssue(t, tree.handler, tree.root, "Moved before the approval", fault.document(fault.clean))
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)).Ask.ID
	reviseDocument(t, tree.handler, issue.PrimaryArtifactID, fault.clean, fault.broken, 2)
	before := readFormState(t, tree.database, issue.Key)
	requireFormRefusal(t, "approve", answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""),
		specform.ActApproval, []string{fault.line(2)}, fault.finding(issue, 2))
	if after := readFormState(t, tree.database, issue.Key); after != before || after.reviews != 0 {
		t.Fatalf("the refused approval changed %+v to %+v, want no review", before, after)
	}
	if got := readBlockAsk(t, tree.handler, ask); got.State != "open" {
		t.Fatalf("the request = %s, want it still open", got.State)
	}
}

// A renewal hands back every document the open request names, at the version an edit moved it to,
// so the gate judges them all, not only the document the renewal names.
func TestARenewJudgesTheDocumentsTheHandBackGives(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "A spec and its checks", formFaults[0].document(formFaults[0].clean))
	const cleanClaim = "Dispatch refuses a write that breaks the document schema."
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", "# Verification\n\n:::check{#refuse-write}\n"+cleanClaim+"\n:::\n")
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true}))
	longClaim := formFaults[1].broken
	reviseDocument(t, tree.handler, verification.ID, cleanClaim, longClaim, 2)
	requireFormRefusal(t, "renew naming the spec alone", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil),
		specform.ActRequest, []string{"- claim refuse-write in verification.md version 2: " + formFaults[1].detail + "."},
		specFormFinding{Document: "verification.md", ArtifactID: verification.ID, Version: 2, Finding: specform.Finding{
			Node: "refuse-write", Kind: specform.KindClaim, Where: "verification.md version 2",
			Rule: "sentence-words", Detail: formFaults[1].detail, Excerpt: longClaim,
		}})
}

// The read route answers the findings of any document with no side effect, and says whether the
// form refuses its approval, which root covers it and the part it plays.
func TestSpecFormReadNamesEachFinding(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	fault := formFaults[0]
	issue := createFormIssue(t, tree.handler, tree.root, "Read before a request", fault.document(fault.broken))
	before := readFormState(t, tree.database, issue.Key)
	read := specFormReadOf(t, tree.handler, issue.PrimaryArtifactID)
	want := specFormRead{
		ArtifactID: issue.PrimaryArtifactID, Name: "spec.md", Version: 1,
		InScope: true, Scope: "covered", Plays: "spec", Root: new("ACME-1"),
		Texts: map[specform.Kind]int{
			specform.KindStatement: 1, specform.KindClaim: 0, specform.KindProcedure: 0, specform.KindFailingExample: 0,
			specform.KindName: 0, specform.KindPurpose: 0, specform.KindDefinition: 0,
		},
		Findings: []specFormFinding{fault.finding(issue, 1)}, FindingsTotal: 1,
		Terms: []specFormTerm{}, RefusesApproval: true,
	}
	if !reflect.DeepEqual(read, want) {
		t.Fatalf("read =\n%#v\nwant\n%#v", read, want)
	}
	if after := readFormState(t, tree.database, issue.Key); after != before {
		t.Fatalf("the read changed %+v to %+v", before, after)
	}
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", "# Verification\n\n:::check{#store-comment}\nDispatch stores each comment.\n:::\n")
	if read := specFormReadOf(t, tree.handler, verification.ID); read.Plays != "verification" || read.Scope != "covered" || read.FindingsTotal != 0 || read.RefusesApproval {
		t.Fatalf("the Verification document's read = %#v, want plays verification, covered, no finding", read)
	}
	notes := uploadFormDocument(t, tree.handler, issue.Key, "notes.md", "# Notes\n\nA note.\n")
	if read := specFormReadOf(t, tree.handler, notes.ID); read.Plays != "" || read.Scope != "not_spec" {
		t.Fatalf("notes.md's read = %#v, want plays nothing, not_spec", read)
	}
	uncovered := createFormIssue(t, tree.handler, tree.outside, "Read outside the root", fault.document(fault.broken))
	read = specFormReadOf(t, tree.handler, uncovered.PrimaryArtifactID)
	if read.InScope || read.Scope != "outside" || read.RefusesApproval || !reflect.DeepEqual(read.Findings, []specFormFinding{fault.finding(uncovered, 1)}) {
		t.Fatalf("an uncovered document's read = %#v, want out of scope with its finding listed", read)
	}
	if response := sessionRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil); response.Code != http.StatusOK {
		t.Fatalf("an agent's read: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("a read with no credential: status=%d body=%s, want 401", response.Code, response.Body.String())
	}
}

// The refusal lists 20 findings and counts the rest, with the command that lists them all.
func TestTheRefusalListsTwentyAndCountsTheRest(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	var spec strings.Builder
	spec.WriteString("# Spec\n")
	for index := 1; index <= 23; index++ {
		fmt.Fprintf(&spec, "\n:::requirement{#s%d}\n%s\n:::\n", index, formFaults[0].broken)
	}
	issue := createFormIssue(t, tree.handler, tree.root, "Twenty-three broken statements", spec.String())
	response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("request: status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	body := decodeBody[specFormError](t, response)
	lines := strings.Split(body.Error, "\n")
	if body.FindingsTotal != 23 || len(body.Findings) != 23 || len(lines) != 23 ||
		lines[0] != "Dispatch refused this approval request, because 23 texts break the form of The Spec." ||
		lines[20] != "- statement s20 in spec.md version 1: it has 3 sentences, and a statement has one or two." ||
		lines[21] != "And 3 more: dispatch spec-form --issue "+issue.Key+" --artifact spec.md lists them all." {
		t.Fatalf("refusal = total %d, %d findings, error\n%s", body.FindingsTotal, len(body.Findings), body.Error)
	}
}

// The head of a refusal counts the texts that break the form, and findings_total counts the
// findings: a statement with three faults is one text with a line for each fault, in the refusal of
// the agent's request, in the refusal of the person's approval and in the reason that Request
// changes sends.
func TestTheRefusalHeadCountsTheTextsAndTheTotalCountsTheFindings(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const body = "Dispatch spins up a seamless lease; it starts. It ends."
	issue := createFormIssue(t, tree.handler, tree.root, "One statement with three faults", "# Spec\n\n:::requirement{#lease}\n"+body+"\n:::\n")
	var want []specFormFinding
	var lines []string
	for _, fault := range []struct{ rule, detail string }{
		{"ste100-phrasal-verb", `ASD-STE100 does not permit the phrasal verb: "spins up"`},
		{"ste100-marketing-adjective", `ASD-STE100 does not permit the marketing adjective: "seamless"`},
		{"ste100-semicolon", `ASD-STE100 does not permit a semicolon: ";"`},
	} {
		want = append(want, specFormFinding{
			Document: "spec.md", ArtifactID: issue.PrimaryArtifactID, Version: 1,
			Finding: specform.Finding{
				Node: "lease", Kind: specform.KindStatement, Where: "spec.md version 1",
				Rule: specform.Rule(fault.rule), Detail: fault.detail, Excerpt: body,
			},
		})
		lines = append(lines, "- statement lease in spec.md version 1: "+fault.detail+".")
	}
	request := requireFormRefusal(t, "request", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil),
		specform.ActRequest, lines, want...)
	approval := requireFormRefusal(t, "approval", reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", ""),
		specform.ActApproval, lines, want...)
	for what, text := range map[string]string{"request": request.Error, "approval": approval.Error, "changes_reason": approval.ChangesReason} {
		if !strings.Contains(strings.Split(text, "\n")[0], "because 1 text breaks the form of The Spec.") {
			t.Errorf("%s: head %q, want 1 text for the 3 findings", what, strings.Split(text, "\n")[0])
		}
	}
}

// The gate reads the version the request names, not the live text a browser holds unsettled. The
// documents settle after an hour, so the browser's text stays live while the test runs, and the
// document service knows people, so the browser's socket is let in.
func TestTheGateReadsTheNamedVersion(t *testing.T) {
	f := newRepairFixtureFrom(t, time.Hour, "ACME-1")
	tree := seedSpecFormTree(t, f.handler, f.database, f.deps)
	documentService := f.documents
	fault := formFaults[0]
	clean := createFormIssue(t, tree.handler, tree.root, "Clean version, broken live text", fault.document(fault.clean))
	writeBrowserDocument(t, documentService, clean.PrimaryArtifactID, fault.document(fault.broken))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, clean.PrimaryArtifactID, nil))
	broken := createFormIssue(t, tree.handler, tree.root, "Broken version, clean live text", fault.document(fault.broken))
	writeBrowserDocument(t, documentService, broken.PrimaryArtifactID, fault.document(fault.clean))
	requireFormRefusal(t, "request", requestFormApproval(t, tree.handler, broken.PrimaryArtifactID, nil),
		specform.ActRequest, []string{fault.line(1)}, fault.finding(broken, 1))
}

// A word that is a person's name piece and the name of a component of the approved spec under the
// root is a noun of the system, not a person. The control: before that spec is approved, the same
// word names a person.
func TestTheGateKnowsTheComponentNamesUnderItsRoot(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	seedPeople(t, tree.database, "dispatch.admin@example.com")
	component := createFormIssue(t, tree.handler, tree.root, "The components",
		"# Components\n\n## Dispatch (component `dispatch`)\n\nThe server keeps each issue and each document.\n")
	user := formFault{node: "reach-owner", kind: specform.KindStatement, broken: "Each issue reaches Dispatch before its owner starts work.",
		rule: "person", detail: `it names a person: "Dispatch"`}
	statement := createFormIssue(t, tree.handler, tree.root, "Uses a component name", user.document(user.broken))
	requireFormRefusal(t, "before the component is approved", requestFormApproval(t, tree.handler, statement.PrimaryArtifactID, nil),
		specform.ActRequest, []string{user.line(1)}, user.finding(statement, 1))
	if response := reviewFormDocument(t, tree.handler, component.PrimaryArtifactID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("approve the components: status=%d body=%s", response.Code, response.Body.String())
	}
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, statement.PrimaryArtifactID, nil))
}

// A document of the size of level two, 301 statements that all break the form, is judged in under
// one second, as is the same document with every statement clean.
func TestLevelTwoSizedDocumentIsJudgedInUnderOneSecond(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{name: "broken", body: formFaults[0].broken, status: http.StatusConflict},
		{name: "clean", body: "Dispatch shows the person each question.", status: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			var spec strings.Builder
			spec.WriteString("# Level two\n")
			for index := 1; index <= 301; index++ {
				fmt.Fprintf(&spec, "\n:::requirement{#statement-%d}\n%s\n:::\n", index, test.body)
			}
			issue := createFormIssue(t, tree.handler, tree.root, "Level two sized: "+test.name, spec.String())
			awaitSettled(t, tree.database, issue.PrimaryArtifactID)
			started := time.Now()
			response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
			elapsed := time.Since(started)
			if response.Code != test.status {
				t.Fatalf("request: status=%d, want %d", response.Code, test.status)
			}
			if elapsed >= time.Second {
				t.Fatalf("the request of 301 statements took %s, want under one second", elapsed)
			}
			t.Logf("the request of 301 statements took %s", elapsed)
		})
	}
}

// The list of terms of a root (R50) is the term lines of the person-approved version of the
// root's terms.md, and of the latest approved version of each covered document under the root.
// A covered document that defines a term of the list again is refused, and the refusal names the
// term and the document that defines it.

const (
	sessionDefinition = "One conversation of an agent, with its id, in which the agent does its work."
	otherSession      = "The record of one agent process in Envoy."
	leaseDefinition   = "The right of one session to one work unit for a time."
	otherLease        = "The time for which one session holds one work unit."
	twoSentences      = "The registration that a session sends again. It comes every 90 seconds."
)

// termLine is the text of the term line that defines term as definition, after its list marker:
// an edit replaces the text of a list item, not its marker.
func termLine(term, definition string) string {
	return "**" + term + "**: " + definition
}

// termList is lines as the items of a bullet list.
func termList(lines []string) string {
	return "- " + strings.Join(lines, "\n- ") + "\n"
}

// specWithTerms is a spec whose heading Terms holds lines.
func specWithTerms(lines ...string) string {
	return "# Spec\n\n## Terms\n\n" + termList(lines)
}

// termsMd is a terms.md that holds lines.
func termsMd(lines ...string) string {
	return "# Terms\n\n" + termList(lines)
}

// alreadyDefines is the detail of a second definition of a term that where defines.
func alreadyDefines(where string) string {
	return where + " already defines this term. Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first"
}

// approveFormDocument is alice's approval of the document from its header, which must pass.
func approveFormDocument(t *testing.T, handler http.Handler, artifactID string) {
	t.Helper()
	if response := reviewFormDocument(t, handler, artifactID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("approve %s: status=%d body=%s, want 201", artifactID, response.Code, response.Body.String())
	}
}

// approveTermsMd uploads markdown as the terms.md of issue and approves it as alice.
func approveTermsMd(t *testing.T, tree specFormTree, issue, markdown string) model.Artifact {
	t.Helper()
	terms := uploadFormDocument(t, tree.handler, issue, "terms.md", markdown)
	approveFormDocument(t, tree.handler, terms.ID)
	return terms
}

// definitionFinding is the finding of the definition of term in version of the document name
// whose id is artifactID, which breaks rule with detail.
func definitionFinding(name, artifactID string, version int, term, definition, rule, detail string) specFormFinding {
	return specFormFinding{
		Document: name, ArtifactID: artifactID, Version: version,
		Finding: specform.Finding{
			Node: term, Kind: specform.KindDefinition, Where: fmt.Sprintf("%s version %d", name, version),
			Rule: specform.Rule(rule), Detail: detail, Excerpt: definition,
		},
	}
}

// findingLine is the refusal's line for finding.
func findingLine(finding specFormFinding) string {
	return fmt.Sprintf("- %s %s in %s: %s.", finding.Kind, finding.Node, finding.Where, finding.Detail)
}

// requireTerms fails t unless the read of the document answers want as its list of terms.
func requireTerms(t *testing.T, handler http.Handler, artifactID string, want ...specFormTerm) {
	t.Helper()
	if want == nil {
		want = []specFormTerm{}
	}
	if got := specFormReadOf(t, handler, artifactID).Terms; !reflect.DeepEqual(got, want) {
		t.Fatalf("the list of terms of the read of %s = %#v, want %#v", artifactID, got, want)
	}
}

// secondSession is a spec's definition of session, which the root's terms.md version 1 defines.
var secondSession = formFault{
	name: "a second definition", node: "session", kind: specform.KindDefinition,
	clean: termLine("lease", leaseDefinition), broken: termLine("session", otherSession), excerpt: otherSession,
	rule: "term-second-definition", detail: alreadyDefines("terms.md version 1 of ACME-1"),
}

// v-terms, control 1: with the root's terms.md approved defining session, a spec that defines
// session again is refused on each of the six paths to a person's approval, and each finding names
// the term and the document that defines it.
func TestEveryApprovalRouteRefusesASecondDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	requireEveryApprovalRouteRefuses(t, tree, secondSession)
}

// v-terms, control 3: a definition of two sentences is refused on each of the six paths.
func TestADefinitionOfTwoSentencesIsRefusedOnEveryRoute(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireEveryApprovalRouteRefuses(t, tree, formFault{
		name: "a definition of two sentences", node: "heartbeat", kind: specform.KindDefinition,
		clean:  termLine("heartbeat", "The registration that a session sends again every 90 seconds."),
		broken: termLine("heartbeat", twoSentences), excerpt: twoSentences,
		rule: "definition-sentences", detail: "the definition has 2 sentences, and a definition has one",
	})
}

// v-terms, last control: versions of terms.md that no person approved change nothing. They give
// session a new definition and replace heartbeat with lease, and the list keeps the approved
// definitions of heartbeat and session and has no lease, so a spec that defines lease is opened.
func TestTheListCountsOnlyApprovedVersions(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	heartbeat := "The registration that a session sends again every 90 seconds."
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition), termLine("heartbeat", heartbeat)))
	reviseDocument(t, tree.handler, terms.ID, termLine("heartbeat", heartbeat), termLine("lease", leaseDefinition), 2)
	reviseDocument(t, tree.handler, terms.ID, termLine("session", sessionDefinition), termLine("session", otherSession), 3)
	awaitSettled(t, tree.database, terms.ID)
	issue := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approved := []specFormTerm{
		{Term: "heartbeat", Definition: heartbeat, Document: "terms.md", Version: 1, Issue: "ACME-1"},
		{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"},
	}
	requireTerms(t, tree.handler, issue.PrimaryArtifactID, approved...)
	requireTerms(t, tree.handler, terms.ID, approved...)
	read := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
	if read.SpecForm == nil || read.SpecForm.Terms != 2 {
		t.Fatalf("spec_form = %#v, want the two terms of the list counted", read.SpecForm)
	}
}

// The terms of an approved change request join the list with no copy: a later spec that defines
// the same term is refused, naming that request's spec.
func TestAChangeRequestsApprovedTermsJoinTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	again := createFormIssue(t, tree.handler, tree.root, "Defines lease again", specWithTerms(termLine("lease", otherLease)))
	finding := definitionFinding("spec.md", again.PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+change.Key))
	requireFormRefusal(t, "request", requestFormApproval(t, tree.handler, again.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
}

// A term that an approved change request defines is changed in that request, not in the root's
// terms.md: a terms.md that defines it is refused, naming the request's spec, and a second version
// of that spec with a changed definition is opened.
func TestATermThatAChangeRequestDefinesIsChangedInThatRequest(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	terms := uploadFormDocument(t, tree.handler, tree.root, "terms.md",
		termsMd(termLine("session", sessionDefinition), termLine("lease", otherLease)))
	finding := definitionFinding("terms.md", terms.ID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+change.Key))
	requireFormRefusal(t, "request of terms.md", requestFormApproval(t, tree.handler, terms.ID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	reviseDocument(t, tree.handler, change.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	awaitSettled(t, tree.database, change.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil))
}

// A request names the documents whose candidate versions replace their approved ones, so a spec
// whose second version keeps the term its first version defined is no second definition.
func TestADocumentReplacingItsOwnApprovedTermsIsNotRefused(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease",
		specWithTerms(termLine("lease", leaseDefinition))+"\n:::requirement{#store-comment}\nDispatch stores each comment.\n:::\n")
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	reviseDocument(t, tree.handler, change.PrimaryArtifactID, "Dispatch stores each comment.", "Dispatch keeps each comment.", 2)
	awaitSettled(t, tree.database, change.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil))
}

// The root's terms.md is where a term of it changes: a version that changes the definition of
// session is opened, and once the person approves it the list holds the new definition.
func TestTermsMdItselfMayChangeADefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	reviseDocument(t, tree.handler, terms.ID, termLine("session", sessionDefinition), termLine("session", otherSession), 2)
	awaitSettled(t, tree.database, terms.ID)
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, terms.ID, nil)).Ask.ID
	requireTerms(t, tree.handler, terms.ID, specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"})
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve terms.md version 2: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	requireTerms(t, tree.handler, terms.ID, specFormTerm{Term: "session", Definition: otherSession, Document: "terms.md", Version: 2, Issue: "ACME-1"})
}

// Only the root's terms.md holds the list: a level's own approved terms.md is no second list, so
// a spec under that level that defines its term is opened, and the read of it lists only the
// root's terms.
func TestALevelsOwnTermsMdMakesNoSecondList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	level := createFormIssue(t, tree.handler, tree.root, "A level", "# Level\n\nThe level.\n")
	approveTermsMd(t, tree, level.Key, termsMd(termLine("lease", leaseDefinition)))
	child := createFormIssue(t, tree.handler, level.Key, "Under the level", specWithTerms(termLine("lease", otherLease)))
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, child.PrimaryArtifactID, nil))
	requireTerms(t, tree.handler, child.PrimaryArtifactID,
		specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"})
}

// Before the person approves the root's terms.md, the list is the terms of the approved covered
// documents alone: a terms.md that waits for its approval adds nothing.
func TestBeforeTermsMdIsApprovedTheListIsTheApprovedRequests(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	uploadFormDocument(t, tree.handler, tree.root, "terms.md", termsMd(termLine("session", sessionDefinition)))
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	other := createFormIssue(t, tree.handler, tree.root, "Reads the list", formFaults[0].document(formFaults[0].clean))
	requireTerms(t, tree.handler, other.PrimaryArtifactID,
		specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: change.Key})
}

// The repair pass judges a withdrawn request against the list too: a request whose spec defines a
// term of the root's terms.md again is left resolved, with a reason that names the term and the
// document that defines it.
func TestTheRepairPassDoesNotOpenARequestWithASecondDefinition(t *testing.T) {
	f := newRepairFixtureFrom(t, 20*time.Millisecond, "ACME-1")
	tree := seedSpecFormTree(t, f.handler, f.database, f.deps)
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	withdrawn := withdrawnByAnOlderRelease(t, f, tree, secondSession.document(secondSession.broken))
	if repair := repairNow(t, f); repair.Restored != 0 || repair.LeftResolved != 1 {
		t.Fatalf("repair = %+v, want none restored and 1 left resolved", repair)
	}
	requireLeftResolved(t, f.handler, f.database, withdrawn.issue, withdrawn.seq, withdrawn.request, secondSession.line(1), withdrawn.reopener)
}

// A term that a closed issue defines is still in the list, and the refusal says to reopen that
// issue first: a closed issue takes no approval request, which is the control that shows the step
// is needed. Once reopened, its second version with a changed definition is opened.
func TestASecondDefinitionInAClosedIssueSaysToReopenIt(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	change := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, change.PrimaryArtifactID)
	closeIssue(t, tree.handler, change.Key)
	again := createFormIssue(t, tree.handler, tree.root, "Defines lease again", specWithTerms(termLine("lease", otherLease)))
	finding := definitionFinding("spec.md", again.PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+change.Key))
	body := requireFormRefusal(t, "request", requestFormApproval(t, tree.handler, again.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	if detail := body.Findings[0].Detail; !strings.HasSuffix(detail, "When that issue has the status done, reopen it first") {
		t.Fatalf("detail = %q, want it to end with the step to reopen the issue", detail)
	}
	if refused := requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil); refused.Code != http.StatusConflict ||
		!strings.Contains(refused.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("request of the closed issue's spec: status=%d body=%s, want 409 ISSUE_CLOSED", refused.Code, refused.Body.String())
	}
	if response := dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+change.Key, map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reopen %s: status=%d body=%s", change.Key, response.Code, response.Body.String())
	}
	reviseDocument(t, tree.handler, change.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", otherLease), 2)
	awaitSettled(t, tree.database, change.PrimaryArtifactID)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, change.PrimaryArtifactID, nil))
}

// One request holds one definition of a term: a spec and its Verification document, requested
// together, that each define lease are refused at the second. The control: the spec alone is
// opened.
func TestARequestOfTwoDocumentsRefusesASecondDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "Defines lease twice", specWithTerms(termLine("lease", leaseDefinition)))
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md",
		"# Verification\n\n## Terms\n\n"+termList([]string{termLine("lease", otherLease)}))
	finding := definitionFinding("verification.md", verification.ID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+issue.Key))
	requireFormRefusal(t, "request of both", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true}),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
}

// termLineFinding is the finding of a line in the form of a term line outside Terms, in the
// document name whose id is artifactID, that defines term again as definition with detail.
func termLineFinding(name, artifactID string, version int, term, definition, detail string) specFormFinding {
	return specFormFinding{Document: name, ArtifactID: artifactID, Version: version, Finding: specform.Finding{
		Node: term, Kind: "term_line", Where: fmt.Sprintf("%s version %d", name, version),
		Rule: "term-second-definition", Detail: detail, Excerpt: definition,
	}}
}

// A line in the form of a term line under another heading is a definition (R50): with lease in
// the list, a request whose spec defines lease again under "## Glossary" is refused, and the
// finding says that the form of the line makes it a definition. A document that a person approved
// with such a line adds no term to the list. The control: the same line with a name that nothing
// defines opens the request.
func TestARequestRefusesATermLineUnderAnotherHeadingThatNamesAListedTerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	listed := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
	glossary := createFormIssue(t, tree.handler, tree.root, "Defines lease again under Glossary", "# Spec\n\n## Glossary\n\n- "+termLine("lease", otherLease)+"\n")
	finding := termLineFinding("spec.md", glossary.PrimaryArtifactID, 1, "lease", otherLease, "spec.md version 1 of "+listed.Key+
		` already defines this term. A line of the form "- **<term>**: <definition>" is a definition under each heading. If this line is no definition, change its form. `+
		"If it is a definition, use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first")
	requireFormRefusal(t, "request of a spec with lease under Glossary", requestFormApproval(t, tree.handler, glossary.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	plain := createFormIssue(t, tree.handler, tree.root, "Names valve under Glossary", "# Spec\n\n## Glossary\n\n- "+termLine("valve", "A part of a pump that opens and closes.")+"\n")
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, plain.PrimaryArtifactID, nil))
	approveFormDocument(t, tree.handler, plain.PrimaryArtifactID)
	if count := countTerm(t, tree.handler, listed.PrimaryArtifactID, "valve"); count != 0 {
		t.Fatalf("the list holds valve %d times after the person approved a spec that names it under Glossary, want 0", count)
	}
}

// Two lines of one name in the form of a term line in the documents of one request are two
// definitions, wherever each stands: a spec and its Verification document, requested together,
// that each name lease under another heading are refused at the second. The control: the spec
// alone is opened.
func TestARequestOfTwoDocumentsRefusesTwoTermLinesOfOneName(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.root, "Names lease in two documents", "# Spec\n\n## Glossary\n\n- "+termLine("lease", leaseDefinition)+"\n")
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", "# Verification\n\n## Notes\n\n- "+termLine("lease", otherLease)+"\n")
	finding := termLineFinding("verification.md", verification.ID, 1, "lease", otherLease, "spec.md version 1 of "+issue.Key+
		` already defines this term. A line of the form "- **<term>**: <definition>" is a definition under each heading. If this line is no definition, change its form. `+
		"If it is a definition, use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first")
	requireFormRefusal(t, "request of both", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true}),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
}

// countTerm is how many times the list of the read of artifactID holds term.
func countTerm(t *testing.T, handler http.Handler, artifactID, term string) int {
	t.Helper()
	count := 0
	for _, listed := range specFormReadOf(t, handler, artifactID).Terms {
		if strings.EqualFold(listed.Term, term) {
			count++
		}
	}
	return count
}

// Two requests that define one new term wait on the person at one time, and the person approves
// both at the same moment. At most one approval passes: the second definition is refused, as it is
// when the two approvals come one after the other (the control).
func TestTwoApprovalsAtOneTimeLeaveOneDefinitionOfANewTerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	open := func(term, definition string) formIssue {
		issue := createFormIssue(t, tree.handler, tree.root, "Defines "+term+": "+definition, specWithTerms(termLine(term, definition)))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil))
		return issue
	}
	// The control: one approval after the other refuses the second.
	first, second := open("ledger", "The record of the work that one session did."), open("ledger", "The list of the costs that one project paid.")
	approveFormDocument(t, tree.handler, first.PrimaryArtifactID)
	if response := reviewFormDocument(t, tree.handler, second.PrimaryArtifactID, "approved", ""); response.Code != http.StatusConflict {
		t.Fatalf("control, the second approval after the first: status=%d body=%s, want 409 SPEC_FORM", response.Code, response.Body.String())
	}
	const pairs = 8
	both := 0
	for pair := range pairs {
		term := fmt.Sprintf("tally%d", pair)
		a, b := open(term, "The record of the work that one session did."), open(term, "The list of the costs that one project paid.")
		var wg sync.WaitGroup
		responses := make([]*httptest.ResponseRecorder, 2)
		for index, issue := range []formIssue{a, b} {
			wg.Go(func() {
				responses[index] = reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", "")
			})
		}
		wg.Wait()
		if responses[0].Code == http.StatusCreated && responses[1].Code == http.StatusCreated {
			both++
			t.Errorf("pair %d: both approvals passed (201, 201), and the list of terms holds %s %d times", pair, term, countTerm(t, tree.handler, a.PrimaryArtifactID, term))
		}
	}
	if both > 0 {
		t.Fatalf("%d of %d pairs approved two definitions of one term at one time", both, pairs)
	}
}

// An issue outside the root whose approved spec defines a term moves under the root, where the
// root's approved terms.md defines the same term. The list then holds one definition of it:
// Dispatch refuses the move, or the list keeps one. The control: before the move, the list holds
// the term once.
func TestAMoveUnderTheRootLeavesOneDefinitionOfATerm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	outside := createFormIssue(t, tree.handler, tree.outside, "Defines session outside the root", specWithTerms(termLine("session", otherSession)))
	approveFormDocument(t, tree.handler, outside.PrimaryArtifactID)
	inside := createFormIssue(t, tree.handler, tree.root, "Reads the list", specWithTerms(termLine("lease", leaseDefinition)))
	if count := countTerm(t, tree.handler, inside.PrimaryArtifactID, "session"); count != 1 {
		t.Fatalf("control, before the move: the list holds session %d times, want 1", count)
	}
	moved := dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+outside.Key, map[string]any{"parent": tree.root}, "alice")
	if moved.Code != http.StatusOK {
		return // Dispatch refused the move: the list keeps one definition.
	}
	if count := countTerm(t, tree.handler, inside.PrimaryArtifactID, "session"); count != 1 {
		t.Fatalf("after the move of %s under %s (status 200), the list holds session %d times, want 1", outside.Key, tree.root, count)
	}
}

// The other two routes of a person's approval take the same turn: an approval from the Inbox and
// an approval of a named version at one time, each of a request that defines one new term, leave
// one definition of it. The second is refused, and so the list holds the term once.
func TestTwoApprovalsAtOneTimeOnTheInboxAndAVersionLeaveOneDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const pairs = 8
	both := 0
	for pair := range pairs {
		term := fmt.Sprintf("tally%d", pair)
		a := createFormIssue(t, tree.handler, tree.root, "Defines "+term, specWithTerms(termLine(term, "The record of the work that one session did.")))
		ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, a.PrimaryArtifactID, nil)).Ask.ID
		b := createFormIssue(t, tree.handler, tree.root, "Defines "+term+" again", specWithTerms(termLine(term, "The list of the costs that one project paid.")))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, b.PrimaryArtifactID, nil))
		answer := map[string]any{"selected": []string{docs.ApprovalOptionApprove}, "expected_edited_at": readBlockAsk(t, tree.handler, ask).EditedAt}
		var wg sync.WaitGroup
		var inbox, version *httptest.ResponseRecorder
		wg.Go(func() {
			inbox = dispatchRequest(t, tree.handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", answer, "alice")
		})
		wg.Go(func() { version = reviewFormVersion(t, tree.handler, b.PrimaryArtifactID, 1, "approved") })
		wg.Wait()
		if inbox.Code == http.StatusOK && version.Code == http.StatusCreated {
			both++
			t.Errorf("pair %d: both approvals passed (200, 201), and the list of terms holds %s %d times", pair, term, countTerm(t, tree.handler, a.PrimaryArtifactID, term))
			continue
		}
		if inbox.Code != http.StatusOK && version.Code != http.StatusCreated {
			t.Fatalf("pair %d: both approvals were refused: Inbox status=%d body=%s, version status=%d body=%s, want one passed",
				pair, inbox.Code, inbox.Body.String(), version.Code, version.Body.String())
		}
		for _, refused := range []*httptest.ResponseRecorder{inbox, version} {
			if refused.Code != http.StatusOK && refused.Code != http.StatusCreated && (refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"SPEC_FORM"`)) {
				t.Fatalf("pair %d: the refused approval answered status=%d body=%s, want 409 SPEC_FORM", pair, refused.Code, refused.Body.String())
			}
		}
		if count := countTerm(t, tree.handler, a.PrimaryArtifactID, term); count != 1 {
			t.Fatalf("pair %d: the list holds %s %d times, want 1", pair, term, count)
		}
	}
	if both > 0 {
		t.Fatalf("%d of %d pairs approved two definitions of one term at one time", both, pairs)
	}
}

// The list holds one definition of each term, and the root's terms.md comes first: a spec that a
// person approved outside the root before terms.md, and that moves under the root, adds no second
// definition. Its own read names the definition that the list holds.
func TestTheRootsTermsMdComesFirstInTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	moved := createFormIssue(t, tree.handler, tree.outside, "Defines session first", specWithTerms(termLine("session", otherSession)))
	approveFormDocument(t, tree.handler, moved.PrimaryArtifactID)
	terms := approveTermsMd(t, tree, tree.root, termsMd(termLine("session", sessionDefinition)))
	reparentFormIssue(t, tree.handler, moved.Key, tree.root)
	listed := specFormTerm{Term: "session", Definition: sessionDefinition, Document: "terms.md", Version: 1, Issue: "ACME-1"}
	requireTerms(t, tree.handler, terms.ID, listed)
	requireTerms(t, tree.handler, moved.PrimaryArtifactID, listed)
	want := []specFormFinding{definitionFinding("spec.md", moved.PrimaryArtifactID, 1, "session", otherSession, "term-second-definition",
		alreadyDefines("terms.md version 1 of ACME-1"))}
	if read := specFormReadOf(t, tree.handler, moved.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, want) {
		t.Fatalf("the read of the moved spec = findings %#v, want %#v", read.Findings, want)
	}
}

// Of two documents under the root that define one term, the list holds the definition of the one
// that joined the list first, here at its approval under the root, before the other moved in, and
// keeps it when that document changes its definition. The read of the other document names that
// definition, and its next approval request is refused.
func TestTheListHoldsTheDefinitionThatJoinedFirst(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	first := createFormIssue(t, tree.handler, tree.root, "Defines lease first", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, first.PrimaryArtifactID)
	later := createFormIssue(t, tree.handler, tree.outside, "Defines lease later",
		specWithTerms(termLine("lease", otherLease))+"\n:::requirement{#store-comment}\nDispatch stores each comment.\n:::\n")
	approveFormDocument(t, tree.handler, later.PrimaryArtifactID)
	reparentFormIssue(t, tree.handler, later.Key, tree.root)
	requireTerms(t, tree.handler, first.PrimaryArtifactID, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: first.Key})
	finding := definitionFinding("spec.md", later.PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+first.Key))
	if read := specFormReadOf(t, tree.handler, later.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{finding}) {
		t.Fatalf("the read of the later spec = findings %#v, want %#v", read.Findings, []specFormFinding{finding})
	}
	reviseDocument(t, tree.handler, later.PrimaryArtifactID, "Dispatch stores each comment.", "Dispatch keeps each comment.", 2)
	awaitSettled(t, tree.database, later.PrimaryArtifactID)
	finding = definitionFinding("spec.md", later.PrimaryArtifactID, 2, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+first.Key))
	requireFormRefusal(t, "request of the later spec", requestFormApproval(t, tree.handler, later.PrimaryArtifactID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
	const changedLease = "The time for which one session may hold one work unit."
	reviseDocument(t, tree.handler, first.PrimaryArtifactID, termLine("lease", leaseDefinition), termLine("lease", changedLease), 2)
	awaitSettled(t, tree.database, first.PrimaryArtifactID)
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, first.PrimaryArtifactID, nil)).Ask.ID
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve the first spec's version 2: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	requireTerms(t, tree.handler, first.PrimaryArtifactID, specFormTerm{Term: "lease", Definition: changedLease, Document: "spec.md", Version: 2, Issue: first.Key})
}

// A spec and its Verification document that a person approved in one answer, outside the root,
// each define lease. The move under the root brings both in at one event, and the list holds the
// definition of the document whose first approval came first, the spec, which the answer names
// first; the read of the Verification document names it.
func TestTwoDocumentsThatJoinAtOneMoveKeepTheOrderOfTheirApprovals(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	issue := createFormIssue(t, tree.handler, tree.outside, "Defines lease twice", specWithTerms(termLine("lease", leaseDefinition)))
	verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md",
		"# Verification\n\n## Terms\n\n"+termList([]string{termLine("lease", otherLease)}))
	ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true})).Ask.ID
	if response := answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
		t.Fatalf("approve the spec and its Verification document: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	reparentFormIssue(t, tree.handler, issue.Key, tree.root)
	requireTerms(t, tree.handler, issue.PrimaryArtifactID, specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: issue.Key})
	want := []specFormFinding{definitionFinding("verification.md", verification.ID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+issue.Key))}
	if read := specFormReadOf(t, tree.handler, verification.ID); !reflect.DeepEqual(read.Findings, want) {
		t.Fatalf("the read of the Verification document = findings %#v, want %#v", read.Findings, want)
	}
}

// driftDefinition is a definition that the line of termsLineFault holds in another form.
const driftDefinition = "The slow change of the clock of one machine."

// termsLineFault is a line under Terms in another form than a term line: the colon in the bold.
var termsLineFault = formFault{
	name: "a line under Terms that is no term line", node: `"- **drift:** ` + driftDefinition + `"`, kind: specform.KindLine,
	clean: termLine("drift", driftDefinition), broken: "**drift:** " + driftDefinition, excerpt: "- **drift:** " + driftDefinition,
	rule: "term-line", detail: `it is no term line, and a term line has the form "- **<term>**: <definition>"`,
}

// A line under Terms that is no term line breaks the form: in a spec, on each of the six paths to
// a person's approval, and in the root's terms.md. The finding names the line and shows the form
// of a term line.
func TestEveryApprovalRouteRefusesALineUnderTermsThatIsNoTermLine(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	requireEveryApprovalRouteRefuses(t, tree, termsLineFault)
	terms := uploadFormDocument(t, tree.handler, tree.root, "terms.md", termsMd(termLine("session", sessionDefinition), termsLineFault.broken))
	finding := specFormFinding{
		Document: "terms.md", ArtifactID: terms.ID, Version: 1,
		Finding: specform.Finding{
			Node: termsLineFault.node, Kind: termsLineFault.kind, Where: "terms.md version 1",
			Rule: specform.Rule(termsLineFault.rule), Detail: termsLineFault.detail, Excerpt: termsLineFault.excerpt,
		},
	}
	requireFormRefusal(t, "request of terms.md", requestFormApproval(t, tree.handler, terms.ID, nil),
		specform.ActRequest, []string{findingLine(finding)}, finding)
}

// listedOf is the one definition of term that the read of artifactID lists, and how many it lists.
func listedOf(t *testing.T, handler http.Handler, artifactID, term string) (specFormTerm, int) {
	t.Helper()
	var found specFormTerm
	count := 0
	for _, listed := range specFormReadOf(t, handler, artifactID).Terms {
		if listed.Term == term {
			found = listed
			count++
		}
	}
	return found, count
}

// moveUnder is alice's move of issue under parent; false when Dispatch refuses it.
func moveUnder(t *testing.T, tree specFormTree, issue, parent string) bool {
	t.Helper()
	return dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"parent": parent}, "alice").Code == http.StatusOK
}

// v-terms: "A change to a definition waits for the approval of the person." A move under the root
// changes no definition that the list holds. The list holds the definition of lease that the
// person approved under the root; then an issue that the person approved earlier, outside the
// root, with another definition of lease, moves under the root. Either Dispatch refuses the move,
// or the list keeps the definition it held. The control: before the move, the list holds the
// definition approved under the root.
func TestAMoveUnderTheRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	earlier := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside the root, first", specWithTerms(termLine("lease", otherLease)))
	approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
	listed := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
	want := specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: listed.Key}
	if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("control, before the move: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
	}
	if !moveUnder(t, tree, earlier.Key, tree.root) {
		return // Dispatch refused the move: the list keeps its definition.
	}
	if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the move of %s under %s, the list holds lease %d times, as %#v; want the definition it held, %#v", earlier.Key, tree.root, count, got, want)
	}
}

// The same with two moves: of two issues approved outside the root that define one new term, the
// one approved later moves in first, and the list holds its definition; then the one approved
// first moves in. The second move changes no definition that the list holds.
func TestTheSecondMoveUnderTheRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	first := createFormIssue(t, tree.handler, tree.outside, "Defines lease, approved first", specWithTerms(termLine("lease", otherLease)))
	approveFormDocument(t, tree.handler, first.PrimaryArtifactID)
	later := createFormIssue(t, tree.handler, tree.outside, "Defines lease, approved later", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, later.PrimaryArtifactID)
	if !moveUnder(t, tree, later.Key, tree.root) {
		t.Fatalf("move %s under %s: refused, want 200 (a move of a new term)", later.Key, tree.root)
	}
	want := specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: later.Key}
	if got, count := listedOf(t, tree.handler, later.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("control, after the first move: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
	}
	if !moveUnder(t, tree, first.Key, tree.root) {
		return
	}
	if got, count := listedOf(t, tree.handler, later.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the move of %s under %s, the list holds lease %d times, as %#v; want the definition it held, %#v", first.Key, tree.root, count, got, want)
	}
}

// An approval and a move at one time: an issue under the root waits on the person with a new
// term, and an issue that the person approved outside the root, with another definition of that
// term, moves in at the same moment. The list holds the term once. When the person's approval
// passed, the list holds the definition that the person approved; when it was refused, the
// definition that moved in first.
func TestAnApprovalAndAMoveAtOneTimeLeaveTheApprovedDefinition(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const rounds = 8
	displaced := 0
	for round := range rounds {
		term := fmt.Sprintf("orbit%d", round)
		outside := createFormIssue(t, tree.handler, tree.outside, "Defines "+term+" outside the root", specWithTerms(termLine(term, "The path of one moon around one planet.")))
		approveFormDocument(t, tree.handler, outside.PrimaryArtifactID)
		inside := createFormIssue(t, tree.handler, tree.root, "Defines "+term+" under the root", specWithTerms(termLine(term, "The route that one agent takes through its work.")))
		requireFormRequestOpened(t, requestFormApproval(t, tree.handler, inside.PrimaryArtifactID, nil))
		var wg sync.WaitGroup
		var approved, moved *httptest.ResponseRecorder
		wg.Go(func() { approved = reviewFormDocument(t, tree.handler, inside.PrimaryArtifactID, "approved", "") })
		wg.Go(func() {
			moved = dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+outside.Key, map[string]any{"parent": tree.root}, "alice")
		})
		wg.Wait()
		if moved.Code != http.StatusOK {
			t.Fatalf("round %d: move %s under %s: status=%d body=%s, want 200", round, outside.Key, tree.root, moved.Code, moved.Body.String())
		}
		listed, count := listedOf(t, tree.handler, inside.PrimaryArtifactID, term)
		if count != 1 {
			t.Fatalf("round %d: the list holds %s %d times, want 1", round, term, count)
		}
		switch {
		case approved.Code == http.StatusCreated && listed.Issue != inside.Key:
			displaced++
			t.Errorf("round %d: the approval of %s passed (201), and the list holds %s from %s, which moved in at the same time", round, inside.Key, term, listed.Issue)
		case approved.Code == http.StatusConflict && strings.Contains(approved.Body.String(), `"code":"SPEC_FORM"`) && listed.Issue != outside.Key:
			t.Errorf("round %d: the approval of %s was refused, and the list holds %s from %s, want %s", round, inside.Key, term, listed.Issue, outside.Key)
		case approved.Code != http.StatusCreated && approved.Code != http.StatusConflict:
			t.Fatalf("round %d: approve %s: status=%d body=%s, want 201 or 409 SPEC_FORM", round, inside.Key, approved.Code, approved.Body.String())
		}
	}
	if displaced > 0 {
		t.Fatalf("%d of %d rounds took an approved definition away with a move", displaced, rounds)
	}
}

// A move inside the root is no arrival: the issue that the list keeps a term from moves from one
// level under the root to another, and the list keeps its definition, though a definition that
// arrived after it waits under the root.
func TestAMoveInsideTheRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	first := createFormIssue(t, tree.handler, tree.root, "The first level", "# Level\n\nThe first level.\n")
	second := createFormIssue(t, tree.handler, tree.root, "The second level", "# Level\n\nThe second level.\n")
	keeper := createFormIssue(t, tree.handler, first.Key, "Defines lease under the first level", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
	arrived := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside the root", specWithTerms(termLine("lease", otherLease)))
	approveFormDocument(t, tree.handler, arrived.PrimaryArtifactID)
	reparentFormIssue(t, tree.handler, arrived.Key, tree.root)
	want := specFormTerm{Term: "lease", Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: keeper.Key}
	if got, count := listedOf(t, tree.handler, keeper.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("control, before the move inside the root: the list holds lease %d times, as %#v; want once, as %#v", count, got, want)
	}
	reparentFormIssue(t, tree.handler, keeper.Key, second.Key)
	if got, count := listedOf(t, tree.handler, keeper.PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the move of %s from %s to %s, the list holds lease %d times, as %#v; want %#v", keeper.Key, first.Key, second.Key, count, got, want)
	}
}

// A move changes the tree and no text. When the issue that the list keeps a term from moves out
// of the root, the list holds the definition that joined it next, which the person approved too:
// of two definitions that moved in after the keeper, the one that moved in first. Its own read
// then names no finding, and the read of the last one names it.
func TestWhenTheKeeperMovesOutTheListHoldsTheDefinitionThatJoinedNext(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	keeper := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
	approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
	const thirdLease = "The hold that one session keeps on one work unit."
	var arrivals []formIssue
	for _, definition := range []string{otherLease, thirdLease} {
		arrived := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside the root: "+definition, specWithTerms(termLine("lease", definition)))
		approveFormDocument(t, tree.handler, arrived.PrimaryArtifactID)
		arrivals = append(arrivals, arrived)
	}
	// The one approved last moves in first.
	reparentFormIssue(t, tree.handler, arrivals[1].Key, tree.root)
	reparentFormIssue(t, tree.handler, arrivals[0].Key, tree.root)
	if got, count := listedOf(t, tree.handler, keeper.PrimaryArtifactID, "lease"); count != 1 || got.Issue != keeper.Key {
		t.Fatalf("control, before the keeper moves out: the list holds lease %d times, as %#v; want once, from %s", count, got, keeper.Key)
	}
	reparentFormIssue(t, tree.handler, keeper.Key, tree.outside)
	want := specFormTerm{Term: "lease", Definition: thirdLease, Document: "spec.md", Version: 1, Issue: arrivals[1].Key}
	if got, count := listedOf(t, tree.handler, arrivals[0].PrimaryArtifactID, "lease"); count != 1 || got != want {
		t.Fatalf("after the keeper moved out, the list holds lease %d times, as %#v; want the definition that joined next, %#v", count, got, want)
	}
	if read := specFormReadOf(t, tree.handler, arrivals[1].PrimaryArtifactID); len(read.Findings) != 0 {
		t.Fatalf("the read of %s, which the list now keeps lease from = findings %#v, want none", arrivals[1].Key, read.Findings)
	}
	finding := definitionFinding("spec.md", arrivals[0].PrimaryArtifactID, 1, "lease", otherLease, "term-second-definition",
		alreadyDefines("spec.md version 1 of "+arrivals[1].Key))
	if read := specFormReadOf(t, tree.handler, arrivals[0].PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{finding}) {
		t.Fatalf("the read of %s = findings %#v, want %#v", arrivals[0].Key, read.Findings, []specFormFinding{finding})
	}
}

// olderBuild is a server of this release on tree's database whose setting names no root of The
// Spec. To the list of terms it does what the release before this one does with a move: it
// writes the issue and its events, takes no lock of a list and writes nothing else, since it
// knows no root.
func olderBuild(t *testing.T, tree specFormTree) http.Handler {
	t.Helper()
	handler, _, _ := newTestServer(t, testServerOptions{store: tree.database})
	return handler
}

// moveThrough is alice's move of issue under parent through handler, which must pass.
func moveThrough(t *testing.T, handler http.Handler, issue, parent string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"parent": parent}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("move %s under %s: status=%d body=%s", issue, parent, response.Code, response.Body.String())
	}
}

// A move that a build with no root of The Spec made changes no definition of the list (the
// tester's TestAMoveThatWroteNoArrivalRowChangesNoDefinitionOfTheListAccept3, from the third
// acceptance run). The tester made the older release's move as this build's move with its arrival
// row deleted, the state that release leaves; this release writes no such row, so the older
// release's move is made here through a server that names no root (olderBuild), which does what
// that release does. v-terms: "A change to a definition waits for the approval of the person."
// The control: the same move through the server that names the root.
func TestAMoveByABuildThatNamesNoRootChangesNoDefinitionOfTheList(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	older := olderBuild(t, tree)
	for _, c := range []struct {
		term  string
		mover http.Handler
		who   string
	}{
		{"lease", tree.handler, "control, the server that names the root"},
		{"sigil", older, "a server that names no root, as the release before this one"},
	} {
		earlier := createFormIssue(t, tree.handler, tree.outside, "Defines "+c.term+" outside the root, first", specWithTerms(termLine(c.term, otherLease)))
		approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
		listed := createFormIssue(t, tree.handler, tree.root, "Defines "+c.term+" under the root", specWithTerms(termLine(c.term, leaseDefinition)))
		approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
		moveThrough(t, c.mover, earlier.Key, tree.root)
		want := specFormTerm{Term: c.term, Definition: leaseDefinition, Document: "spec.md", Version: 1, Issue: listed.Key}
		if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, c.term); count != 1 || got != want {
			t.Errorf("%s: after it moved %s under %s, the list holds %s %d times, as %#v; want the definition it held, %#v",
				c.who, earlier.Key, tree.root, c.term, count, got, want)
		}
		finding := definitionFinding("spec.md", earlier.PrimaryArtifactID, 1, c.term, otherLease, "term-second-definition", alreadyDefines("spec.md version 1 of "+listed.Key))
		if read := specFormReadOf(t, tree.handler, earlier.PrimaryArtifactID); !reflect.DeepEqual(read.Findings, []specFormFinding{finding}) {
			t.Errorf("%s: the read of %s = findings %#v, want %#v", c.who, earlier.Key, read.Findings, []specFormFinding{finding})
		}
	}
}

// The order of the list never depends on which build handled a move: each case of moves gives one
// list whether the server that names the root made the moves or a server that names none
// (olderBuild), as the release before this one and a rollback to it do. Each case also names the
// document that the list keeps its term from, by the order the decision gives: a definition joins
// the list at the later of its first approval and the move that brought it under the root.
func TestTheListIsTheSameWhicheverBuildMadeTheMoves(t *testing.T) {
	type moves func(tree specFormTree, move func(issue, parent string)) (keeper string, read string)
	cases := []struct {
		name  string
		moves moves
	}{
		{"a document approved outside first moves in", func(tree specFormTree, move func(string, string)) (string, string) {
			earlier := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside, first", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
			listed := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
			move(earlier.Key, tree.root)
			return listed.Key, listed.PrimaryArtifactID
		}},
		{"the keeper moves inside the root after a later one moved in", func(tree specFormTree, move func(string, string)) (string, string) {
			first := createFormIssue(t, tree.handler, tree.root, "The first level", "# Level\n\nThe first level.\n")
			second := createFormIssue(t, tree.handler, tree.root, "The second level", "# Level\n\nThe second level.\n")
			earlier := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside, first", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, earlier.PrimaryArtifactID)
			keeper := createFormIssue(t, tree.handler, first.Key, "Defines lease under the first level", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
			move(earlier.Key, tree.root)
			move(keeper.Key, second.Key)
			return keeper.Key, keeper.PrimaryArtifactID
		}},
		{"the keeper moves out and in again", func(tree specFormTree, move func(string, string)) (string, string) {
			keeper := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root, first", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, keeper.PrimaryArtifactID)
			later := createFormIssue(t, tree.handler, tree.outside, "Defines lease outside, later", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, later.PrimaryArtifactID)
			move(later.Key, tree.root)
			move(keeper.Key, tree.outside)
			move(keeper.Key, tree.root)
			return later.Key, later.PrimaryArtifactID
		}},
		{"a parent outside the root moves in with its child", func(tree specFormTree, move func(string, string)) (string, string) {
			parent := createFormIssue(t, tree.handler, tree.outside, "A parent outside the root", "# Parent\n\nSome other work.\n")
			child := createFormIssue(t, tree.handler, parent.Key, "Defines lease under that parent, first", specWithTerms(termLine("lease", otherLease)))
			approveFormDocument(t, tree.handler, child.PrimaryArtifactID)
			listed := createFormIssue(t, tree.handler, tree.root, "Defines lease under the root", specWithTerms(termLine("lease", leaseDefinition)))
			approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
			move(parent.Key, tree.root)
			return listed.Key, listed.PrimaryArtifactID
		}},
	}
	for _, c := range cases {
		lists := map[string][]specFormTerm{}
		for _, build := range []string{"the server that names the root", "a server that names no root"} {
			tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
			mover := tree.handler
			if build == "a server that names no root" {
				mover = olderBuild(t, tree)
			}
			keeper, read := c.moves(tree, func(issue, parent string) { moveThrough(t, mover, issue, parent) })
			if got, count := listedOf(t, tree.handler, read, "lease"); count != 1 || got.Issue != keeper {
				t.Errorf("%s, moves by %s: the list holds lease %d times, as %#v; want once, from %s", c.name, build, count, got, keeper)
			}
			lists[build] = specFormReadOf(t, tree.handler, read).Terms
		}
		if !reflect.DeepEqual(lists["the server that names the root"], lists["a server that names no root"]) {
			t.Errorf("%s: the list is %#v after moves by the server that names the root, and %#v after moves by a server that names none",
				c.name, lists["the server that names the root"], lists["a server that names no root"])
		}
	}
}
