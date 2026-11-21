package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// withdrawnDecision is a decision block whose ask the close withdraws and the reopen writes open
// again, which leaves its document owed at the reopen.
const withdrawnDecision = "\n:::ask{#close-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// A reopen and the repair pass each open an approval request again and, in the same transaction,
// move it to the latest version of each document that a settlement wrote while the issue was
// closed (docs.Service.FollowClosedIssueVersions). Each judges the versions that the request
// names once it has moved. A request opened at a clean version 1, whose spec a browser edit
// breaks while the issue is closed, at version 2, is left resolved with the findings of version 2
// as its reason, and the read route refuses version 2 too: each of the seven fixture texts of
// v-form, one at a time, on each of the two paths. The control, the control of v-form: an edit to
// a statement of two sentences of 25 words each leaves the request open at version 2.
//
// The other order: a document that the reopen leaves owed, because it holds a block ask that the
// close withdrew or a browser edit of the closed period that no settlement saved yet, is judged
// at the version that the request named, and the request opens there. The settlement that the
// reopen leaves owed then moves the open request to its version with no judgment, as each version
// write moves an open request: the request names that version and waits on its agent, and a
// person's approval of a version that breaks the form is refused. The control: a clean edit is
// approved.
func TestAReopenJudgesTheVersionsThatTheRequestNamesOnceItMoves(t *testing.T) {
	// edited sets the text of the first paragraph of the first requirement or check block to edit.
	edited := func(edit string) func(document *pmdoc.Node) error {
		return func(document *pmdoc.Node) error {
			pmdoc.Walk(document, func(node *pmdoc.Node) bool {
				if node.Type != "requirement" && node.Type != "check" {
					return true
				}
				node.Children[0].Children[0].Text = edit
				return false
			})
			return nil
		}
	}
	control := formFault{name: "the control of v-form, two sentences of 25 words each", node: "show-person", kind: specform.KindStatement,
		clean: "Dispatch shows the person each question.", broken: twentyFiveWords}
	for _, path := range []string{"the reopen", "the repair pass"} {
		for _, fault := range append(slices.Clone(formFaults), control) {
			broken := fault.node != control.node
			name := path + ", " + fault.name + ", in an edit while the issue is closed"
			t.Run(name, func(t *testing.T) {
				tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", documentSocket: true, settle: 20 * time.Millisecond})
				documents := tree.deps.Docs.(*docs.Service)
				issue := createFormIssue(t, tree.handler, tree.root, name, fault.document(fault.clean))
				request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)).Ask.ID
				peer := connectBrowserPeer(t, documents, issue.PrimaryArtifactID)
				closeIssueThroughPatch(t, tree.handler, tree.database, issue.Key)
				closed := closedEditFixture{handler: tree.handler, database: tree.database, documents: documents, spec: issue.PrimaryArtifactID, peer: peer}
				version := settleClosedEdit(t, closed, edited(fault.broken))
				if named := readCarriedAsk(t, tree.handler, request).Approval.Version; version != 2 || named != 1 {
					t.Fatalf("the closed issue's settlement wrote version %d, and the request names version %d; want version 2 and a request at version 1", version, named)
				}
				if read := specFormReadAtVersion(t, tree, issue.PrimaryArtifactID, 2); read.RefusesApproval != broken {
					t.Fatalf("the read route says version 2 refuses its approval: %t, want %t", read.RefusesApproval, broken)
				}

				seq := lastIssueSeq(t, tree.database, issue.Key)
				reviews := readFormState(t, tree.database, issue.Key).reviews
				actor := model.Actor{Kind: "user", ID: "alice"}
				if path == "the reopen" {
					if response := dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
						t.Fatalf("reopen %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
					}
				} else {
					// A release older than the restore reopens the issue and leaves its asks withdrawn
					// (reopenWithoutRestoring, less its wait for a room sync that this server runs none of).
					repair := repairFixture{handler: tree.handler, database: tree.database, documents: documents, deps: tree.deps}
					actor = repairActor("session-reopener")
					if _, err := tree.database.Pool.Exec(context.Background(), `update issues set status = 'todo', closed_at = null, updated_at = now() where key = $1`, issue.Key); err != nil {
						t.Fatalf("reopen %s: %v", issue.Key, err)
					}
					appendIssueEvent(t, repair, issue.Key, "issue.updated", "todo", actor)
					seq = lastIssueSeq(t, tree.database, issue.Key)
					repairNow(t, repair)
				}
				if broken {
					requireLeftResolved(t, tree.handler, tree.database, issue.Key, seq, request, strings.Join(fault.lines(2), "\n"), actor)
					if after := readFormState(t, tree.database, issue.Key).reviews; after != reviews {
						t.Fatalf("the restore that left the request resolved wrote %d reviews", after-reviews)
					}
					return
				}
				requireRestoredOpen(t, tree, issue.Key, seq, request)
				if named := readCarriedAsk(t, tree.handler, request).Approval.Version; named != 2 {
					t.Fatalf("the restored request names version %d, want version 2", named)
				}
			})
		}
	}
	fault := formFaults[0]
	for _, owed := range []string{"a withdrawn block ask", "an edit not yet settled"} {
		for _, broken := range []bool{false, true} {
			name, edit := "a reopen that leaves "+owed+", the control: a clean edit while the issue is closed", "Dispatch indexes each comment."
			if broken {
				name, edit = "a reopen that leaves "+owed+", a broken edit while the issue is closed", fault.broken
			}
			t.Run(name, func(t *testing.T) {
				unsettled := owed == "an edit not yet settled"
				settle := 20 * time.Millisecond
				if unsettled {
					settle = time.Hour
				}
				tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", documentSocket: true, settle: settle})
				documents := tree.deps.Docs.(*docs.Service)
				body := fault.document(fault.clean)
				if !unsettled {
					body += withdrawnDecision
				}
				issue := createFormIssue(t, tree.handler, tree.root, name, body)
				if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
					t.Fatal(err)
				}
				if !unsettled {
					awaitIndexedAskBlock(t, tree.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
					requireBlockAsk(t, tree.handler, issue.Key, issue.PrimaryArtifactID, "close-decision")
				}
				awaitSettled(t, tree.database, issue.PrimaryArtifactID)
				request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)).Ask.ID
				peer := connectBrowserPeer(t, documents, issue.PrimaryArtifactID)
				closeIssueThroughPatch(t, tree.handler, tree.database, issue.Key)
				change := edited(edit)
				if unsettled {
					stored := storedUpdateCount(t, tree.database, issue.PrimaryArtifactID)
					peer.edit(t, change)
					peer.barrier(t)
					awaitStoredUpdates(t, tree.database, issue.PrimaryArtifactID, stored+1)
				} else {
					if err := documents.ResumeOwedSettlementsForTest(context.Background(), 0); err != nil {
						t.Fatal(err)
					}
					awaitSettled(t, tree.database, issue.PrimaryArtifactID)
					settleClosedEdit(t, closedEditFixture{handler: tree.handler, database: tree.database, documents: documents, spec: issue.PrimaryArtifactID, peer: peer}, change)
				}
				if response := dispatchRequest(t, tree.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("reopen %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
				}
				if state, named := readBlockAsk(t, tree.handler, request).State, readCarriedAsk(t, tree.handler, request).Approval.Version; state != "open" || named != 1 {
					t.Fatalf("after the reopen the request is %s at version %d, want open at version 1, the clean version that the reopen judged", state, named)
				}
				var err error
				if unsettled {
					err = documents.SettleIssueDocuments(context.Background(), issue.Key)
				} else {
					err = documents.ResumeOwedSettlementsForTest(context.Background(), 0)
				}
				if err != nil {
					t.Fatal(err)
				}
				awaitSettled(t, tree.database, issue.PrimaryArtifactID)
				moved := readBlockAsk(t, tree.handler, request)
				named := readCarriedAsk(t, tree.handler, request).Approval.Version
				latest := readApproval(t, tree.handler, issue.PrimaryArtifactID).Approval.LatestVersion
				if moved.State != "open" || moved.WaitingOn != "agent" || latest <= 1 || named != latest {
					t.Fatalf("after the owed settlement the request is %s, waiting on %s, at version %d of %d; want open, waiting on its agent, at the latest version",
						moved.State, moved.WaitingOn, named, latest)
				}
				if read := specFormReadAtVersion(t, tree, issue.PrimaryArtifactID, latest); read.RefusesApproval != broken {
					t.Fatalf("the read route says version %d refuses its approval: %t, want %t", latest, read.RefusesApproval, broken)
				}
				want := map[bool]int{true: http.StatusConflict, false: http.StatusCreated}[broken]
				if review := reviewFormDocument(t, tree.handler, issue.PrimaryArtifactID, "approved", ""); review.Code != want {
					t.Fatalf("alice approves version %d: status=%d body=%s, want %d", latest, review.Code, review.Body.String(), want)
				}
			})
		}
	}
}
