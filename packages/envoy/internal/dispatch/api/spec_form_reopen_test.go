package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A reopen and the repair pass each open an approval request again and, in the same transaction,
// move it to the latest version of each document that a settlement wrote while the issue was
// closed (docs.Service.FollowClosedIssueVersions). Each judges the versions that the request
// names once it has moved. A request opened at a clean version 1, whose spec a browser edit
// breaks while the issue is closed, at version 2, is left resolved with the finding of version 2
// as its reason, and the read route refuses version 2 too. The control: an edit that keeps the
// text clean leaves the request open at version 2.
func TestAReopenJudgesTheVersionsThatTheRequestNamesOnceItMoves(t *testing.T) {
	fault := formFaults[0]
	for _, path := range []string{"the reopen", "the repair pass"} {
		for _, broken := range []bool{false, true} {
			name, edit := path+", the control: a clean edit while the issue is closed", "Dispatch indexes each comment."
			if broken {
				name, edit = path+", a broken edit while the issue is closed", fault.broken
			}
			t.Run(name, func(t *testing.T) {
				tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", documentSocket: true, settle: 20 * time.Millisecond})
				documents := tree.deps.Docs.(*docs.Service)
				issue := createFormIssue(t, tree.handler, tree.root, name, fault.document(fault.clean))
				request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, nil)).Ask.ID
				peer := connectBrowserPeer(t, documents, issue.PrimaryArtifactID)
				closeIssueThroughPatch(t, tree.handler, tree.database, issue.Key)
				closed := closedEditFixture{handler: tree.handler, database: tree.database, documents: documents, spec: issue.PrimaryArtifactID, peer: peer}
				version := settleClosedEdit(t, closed, func(document *pmdoc.Node) error {
					pmdoc.Walk(document, func(node *pmdoc.Node) bool {
						if node.Type != "requirement" {
							return true
						}
						node.Children[0].Children[0].Text = edit
						return false
					})
					return nil
				})
				if named := readCarriedAsk(t, tree.handler, request).Approval.Version; version != 2 || named != 1 {
					t.Fatalf("the closed issue's settlement wrote version %d, and the request names version %d; want version 2 and a request at version 1", version, named)
				}
				if read := specFormReadAtVersion(t, tree, issue.PrimaryArtifactID, 2); read.RefusesApproval != broken {
					t.Fatalf("the read route says version 2 refuses its approval: %t, want %t", read.RefusesApproval, broken)
				}

				seq := lastIssueSeq(t, tree.database, issue.Key)
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
					return
				}
				requireRestoredOpen(t, tree, issue.Key, seq, request)
				if named := readCarriedAsk(t, tree.handler, request).Approval.Version; named != 2 {
					t.Fatalf("the restored request names version %d, want version 2", named)
				}
			})
		}
	}
}
