package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Dispatch refuses an approval request, and a person's approval, of a covered document whose text
// breaks the form of The Spec (R49), on every route that opens an approval request or answers one,
// and a reopen and the repair pass leave such a request resolved rather than open it again. The
// server's setting names the roots of The Spec (DISPATCH_SPEC_ROOTS); these tests name ACME-1.

// Each of the seven fixture texts of v-form, and each of the two texts of a procedure, on each of
// the six paths to a person's approval, is refused with its node and rule, and leaves nothing
// written: (1) open a request; (2) hand a moved request back; (3) approve from the header and (4)
// approve a named version, each with no request open and on a request a new version moved to the
// broken text; (5) approve from the Inbox, on that moved request and on one opened outside the
// root and then moved under it; and (6) reopen the issue, which leaves the request resolved with
// the findings and opens every other ask.
func TestEveryApprovalRouteRefusesABreakOfTheForm(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	for _, fault := range append(slices.Clone(formFaults), procedureFaults...) {
		t.Run(fault.name, func(t *testing.T) {
			// The broken text in the spec's first version, with no request open: paths 1, 3 and 4.
			first := createFormIssue(t, tree.handler, tree.root, "Broken at first: "+fault.name, fault.document(fault.broken))
			awaitSettled(t, tree.database, first.PrimaryArtifactID)
			before := readFormState(t, tree.database, first.Key)
			lines := fault.lines(1)
			requireFormRefusal(t, "path 1, open a request", requestFormApproval(t, tree.handler, first.PrimaryArtifactID, nil),
				specform.ActRequest, lines, fault.findings(first, 1)...)
			requireFormRefusal(t, "path 3 with no request open", reviewFormDocument(t, tree.handler, first.PrimaryArtifactID, "approved", ""),
				specform.ActApproval, lines, fault.findings(first, 1)...)
			requireFormRefusal(t, "path 4 with no request open", reviewFormVersion(t, tree.handler, first.PrimaryArtifactID, 1, "approved"),
				specform.ActApproval, lines, fault.findings(first, 1)...)
			if after := readFormState(t, tree.database, first.Key); after != before {
				t.Fatalf("the refused writes on the first version changed %+v to %+v", before, after)
			}

			// A request on a clean first version, which a new version moves to the broken second:
			// paths 2, 3, 4 and 5.
			moved := createFormIssue(t, tree.handler, tree.root, "Broken by an edit: "+fault.name, fault.document(fault.clean))
			ask := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, moved.PrimaryArtifactID, nil)).Ask.ID
			fault.revise(t, tree.handler, moved)
			awaitSettled(t, tree.database, moved.PrimaryArtifactID)
			before = readFormState(t, tree.database, moved.Key)
			lines = fault.lines(2)
			requireFormRefusal(t, "path 2, hand the moved request back", requestFormApproval(t, tree.handler, moved.PrimaryArtifactID, nil),
				specform.ActRequest, lines, fault.findings(moved, 2)...)
			requireFormRefusal(t, "path 3 on the moved request", reviewFormDocument(t, tree.handler, moved.PrimaryArtifactID, "approved", ""),
				specform.ActApproval, lines, fault.findings(moved, 2)...)
			requireFormRefusal(t, "path 4 on the moved request", reviewFormVersion(t, tree.handler, moved.PrimaryArtifactID, 2, "approved"),
				specform.ActApproval, lines, fault.findings(moved, 2)...)
			requireFormRefusal(t, "path 5 on the moved request", answerFormApproval(t, tree.handler, ask, docs.ApprovalOptionApprove, ""),
				specform.ActApproval, lines, fault.findings(moved, 2)...)
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
				specform.ActApproval, fault.lines(1), fault.findings(reparented, 1)...)
			if after := readFormState(t, tree.database, reparented.Key); after != before {
				t.Fatalf("the refused answer changed %+v to %+v", before, after)
			}

			// The same on a copy that also holds a question, closed and reopened: path 6.
			reopened := createFormIssue(t, tree.handler, tree.outside, "Reopened under the root: "+fault.name, fault.document(fault.broken))
			request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, reopened.PrimaryArtifactID, nil)).Ask.ID
			question := createSessionAsk(t, tree.handler, reopened.Key, "Which region takes the first wave?")
			reparentFormIssue(t, tree.handler, reopened.Key, tree.root)
			seq := reopenFormIssue(t, tree, reopened.Key)
			requireLeftResolved(t, tree.handler, tree.database, reopened.Key, seq, request, strings.Join(fault.lines(1), "\n"), model.Actor{Kind: "user", ID: "alice"})
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
		})
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

// The repair pass, which restores the asks of an issue an older release reopened, leaves a broken
// approval request resolved with the findings, as a reopen does, and a second pass finds nothing
// left. Each of the seven fixture texts of v-form, and each of the two texts of a procedure, is
// sent through it one at a time, with the assertions of the six other paths: the reason names the
// node and the rule, the request is not opened again, no review is written, and the person's
// Approve of it is refused. The control of v-form, a statement of two sentences of 25 words each,
// beside a procedure of one instruction in 20 words, is restored open, and the person's Approve of
// it is taken.
func TestTheRepairPassDoesNotOpenABrokenRequestAgain(t *testing.T) {
	f := newRepairFixtureFrom(t, 20*time.Millisecond, "ACME-1")
	tree := seedSpecFormTree(t, f.handler, f.database, f.deps)
	reopener := repairActor("session-reopener")
	// repaired opens a request of document under the root, closes the issue, reopens it as a
	// release older than the restore does, and runs the repair pass once.
	repaired := func(t *testing.T, name, document string) (formIssue, string, int, IssueAskRepair) {
		t.Helper()
		issue := createFormIssue(t, f.handler, tree.outside, "Reopened by an older release: "+name, document)
		request := requireFormRequestOpened(t, requestFormApproval(t, f.handler, issue.PrimaryArtifactID, nil)).Ask.ID
		reparentFormIssue(t, f.handler, issue.Key, tree.root)
		closeIssue(t, f.handler, issue.Key)
		reopenWithoutRestoring(t, f, issue.Key, "todo", reopener)
		seq := lastIssueSeq(t, f.database, issue.Key)
		return issue, request, seq, repairNow(t, f)
	}
	for _, fault := range append(slices.Clone(formFaults), procedureFaults...) {
		t.Run(fault.name, func(t *testing.T) {
			issue, request, seq, repair := repaired(t, fault.name, fault.document(fault.broken))
			if repair.Restored != 0 || repair.LeftResolved != 1 {
				t.Fatalf("repair = %+v, want none restored and 1 left resolved", repair)
			}
			requireLeftResolved(t, f.handler, f.database, issue.Key, seq, request, strings.Join(fault.lines(1), "\n"), reopener)
			if again := repairNow(t, f); again != (IssueAskRepair{}) {
				t.Fatalf("second repair = %+v, want nothing found", again)
			}
			reviews := readFormState(t, f.database, issue.Key).reviews
			answered := answerFormApproval(t, f.handler, request, docs.ApprovalOptionApprove, "")
			if answered.Code != http.StatusConflict || strings.Contains(answered.Body.String(), "SPEC_FORM") {
				t.Fatalf("approve the request the repair left resolved: status=%d body=%s, want 409 for a request that is not open",
					answered.Code, answered.Body.String())
			}
			if after := readFormState(t, f.database, issue.Key).reviews; after != reviews {
				t.Fatalf("the refused answer wrote %d reviews", after-reviews)
			}
		})
	}
	t.Run("control: a statement of two sentences of 25 words each", func(t *testing.T) {
		statement := formFault{node: "show-person", kind: specform.KindStatement}
		procedure := formFault{node: "run-merge", kind: specform.KindProcedure}
		document := statement.document(twentyFiveWords) + strings.TrimPrefix(procedure.document(twentyWords), "# Spec\n")
		issue, request, seq, repair := repaired(t, "the control", document)
		if repair.Restored != 1 || repair.LeftResolved != 0 {
			t.Fatalf("repair = %+v, want 1 restored and none left resolved", repair)
		}
		requireRestoredOpen(t, specFormTree{handler: f.handler, database: f.database}, issue.Key, seq, request)
		if response := answerFormApproval(t, f.handler, request, docs.ApprovalOptionApprove, ""); response.Code != http.StatusOK {
			t.Fatalf("approve the restored request: status=%d body=%s, want 200", response.Code, response.Body.String())
		}
	})
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
			reason := requireFormRefusal(t, path+" approve", refused, specform.ActApproval, fault.lines(2), fault.findings(issue, 2)...).ChangesReason
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
			specform.ActApproval, fault.lines(1), fault.findings(issue, 1)...).ChangesReason
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
		specform.ActApproval, fault.lines(2), fault.findings(issue, 2)...)
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
	claimFinding := func(rule specform.Rule, detail string) specFormFinding {
		return specFormFinding{Document: "verification.md", ArtifactID: verification.ID, Version: 2, Finding: specform.Finding{
			Node: "refuse-write", Kind: specform.KindClaim, Where: "verification.md version 2",
			Rule: rule, Detail: detail, Excerpt: longClaim,
		}}
	}
	requireFormRefusal(t, "renew naming the spec alone", requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil),
		specform.ActRequest, []string{
			"- claim refuse-write in verification.md version 2: " + formFaults[1].detail + ".",
			"- claim refuse-write in verification.md version 2: " + formFaults[1].steLong + ".",
		},
		claimFinding("sentence-words", formFaults[1].detail), claimFinding("ste100-long-sentence", formFaults[1].steLong))
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
		Findings: fault.findings(issue, 1), FindingsTotal: 1,
		Terms: []specFormTerm{}, Unread: []specFormUnread{}, RefusesApproval: true,
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
	if read.InScope || read.Scope != "outside" || read.RefusesApproval || !reflect.DeepEqual(read.Findings, fault.findings(uncovered, 1)) {
		t.Fatalf("an uncovered document's read = %#v, want out of scope with its finding listed", read)
	}
	if response := sessionRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil); response.Code != http.StatusOK {
		t.Fatalf("an agent's read: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, tree.handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/spec-form", nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("a read with no credential: status=%d body=%s, want 401", response.Code, response.Body.String())
	}
}

// The refusal lists 20 findings and counts the rest, with the command that lists them all at the
// version that the refusal judged: the second version of the spec, whose first version is clean.
func TestTheRefusalListsTwentyAndCountsTheRest(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	var spec strings.Builder
	spec.WriteString("# Spec\n")
	for index := 1; index <= 23; index++ {
		fmt.Fprintf(&spec, "\n:::requirement{#s%d}\n%s\n:::\n", index, formFaults[0].broken)
	}
	issue := createFormIssue(t, tree.handler, tree.root, "Twenty-three broken statements", formFaults[0].document(formFaults[0].clean))
	uploadFormDocument(t, tree.handler, issue.Key, "spec.md", spec.String())
	response := requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("request: status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	body := decodeBody[specFormError](t, response)
	lines := strings.Split(body.Error, "\n")
	if body.FindingsTotal != 23 || len(body.Findings) != 23 || len(lines) != 23 ||
		lines[0] != "Dispatch refused this approval request, because 23 texts break the form of The Spec." ||
		lines[20] != "- statement s20 in spec.md version 2: it has 3 sentences, and a statement has one or two." ||
		lines[21] != "And 3 more: dispatch spec-form --issue "+issue.Key+" --artifact spec.md --version 2 lists them all." {
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
		specform.ActRequest, fault.lines(1), fault.findings(broken, 1)...)
}

// A person's name whose part is the name of a component of the approved spec under the root is a
// noun of the system and one part of a name, which is no name. The control: before that spec is
// approved, the same words name a person.
func TestTheGateKnowsTheComponentNamesUnderItsRoot(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	seedPeople(t, tree.database, "dispatch.admin@example.com")
	component := createFormIssue(t, tree.handler, tree.root, "The components",
		"# Components\n\n## Dispatch (component `dispatch`)\n\nThe server keeps each issue and each document.\n")
	user := formFault{node: "reach-owner", kind: specform.KindStatement, broken: "Each issue reaches Dispatch Admin before its owner starts work.",
		rule: "person", detail: `it names a person: "Dispatch Admin"`}
	statement := createFormIssue(t, tree.handler, tree.root, "Uses a component name", user.document(user.broken))
	requireFormRefusal(t, "before the component is approved", requestFormApproval(t, tree.handler, statement.PrimaryArtifactID, nil),
		specform.ActRequest, user.lines(1), user.findings(statement, 1)...)
	if response := reviewFormDocument(t, tree.handler, component.PrimaryArtifactID, "approved", ""); response.Code != http.StatusCreated {
		t.Fatalf("approve the components: status=%d body=%s", response.Code, response.Body.String())
	}
	requireFormRequestOpened(t, requestFormApproval(t, tree.handler, statement.PrimaryArtifactID, nil))
}

// The judgment of the form reads the issues under its root as everyReader (approvedSpecTexts), the
// reader that the rule of graph_reader.go names for a read whose answer every reader reads, and no
// acting person: an issue under the root reserved for its author gives the judgment no approved
// component name, also in the judgment of that author's own approval, so a person's name whose part
// is that component's name is still a name there. No route writes an issue reserved for its author
// yet, so the test writes the issue's node at the store, through the store's own writer and the
// visibility that a write that asked for `author` would get (graphstoretest.VisibilityFor, handed
// graphstore.ProvesAuthor), as the tests of node_visibility_test.go write theirs. The control: the
// same issue, not reserved, gives the name, and the same approval passes.
func TestTheGateReadsTheIssuesUnderItsRootAsEveryReader(t *testing.T) {
	user := formFault{node: "reach-owner", kind: specform.KindStatement, broken: "Each issue reaches Dispatch Admin before its owner starts work.",
		rule: "person", detail: `it names a person: "Dispatch Admin"`}
	for _, reserved := range []bool{true, false} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
		seedPeople(t, tree.database, "dispatch.admin@example.com")
		component := createFormIssue(t, tree.handler, tree.root, "The components",
			"# Components\n\n## Dispatch (component `dispatch`)\n\nThe server keeps each issue and each document.\n")
		if response := reviewFormDocument(t, tree.handler, component.PrimaryArtifactID, "approved", ""); response.Code != http.StatusCreated {
			t.Fatalf("approve the components: status=%d body=%s", response.Code, response.Body.String())
		}
		if reserved {
			reserveIssueForAuthor(t, tree.database, component.Key)
		}
		statement := createFormIssue(t, tree.handler, tree.root, "Uses a component name", user.document(user.broken))
		approval := reviewFormDocument(t, tree.handler, statement.PrimaryArtifactID, "approved", "")
		if reserved {
			requireFormRefusal(t, "the author's approval beside the components reserved for that author", approval,
				specform.ActApproval, user.lines(1), user.findings(statement, 1)...)
		} else if approval.Code != http.StatusCreated {
			t.Errorf("the control: the approval beside the components that every reader reads: status=%d body=%s, want 201", approval.Code, approval.Body.String())
		}
	}
}

// reserveIssueForAuthor writes the store's node of the issue key reserved for its author, alice
// (privateNodeAuthor), who approves the documents of the form tests (reviewFormDocument).
func reserveIssueForAuthor(t *testing.T, database *store.Store, key string) {
	t.Helper()
	ctx := context.Background()
	author := privateNodeAuthor()
	visibility, err := graphstoretest.VisibilityFor(graphstore.VisibilityAuthor, graphstore.ProvesAuthor(author))
	if err != nil {
		t.Fatalf("reserve issue %s for its author: %v", key, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of issue %s: %v", key, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{{
		Kind: "issue", ID: key, Supertag: "issue", Visibility: visibility, State: graphstore.StateLive,
		MadeAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), MadeBy: author, SourceEvent: graphstore.NoEvent,
	}}, nil); err != nil {
		t.Fatalf("write the reserved issue %s: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the reserved issue %s: %v", key, err)
	}
}
