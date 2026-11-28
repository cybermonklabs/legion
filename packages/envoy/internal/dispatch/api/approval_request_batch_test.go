package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// An approval request reads the document's approval twice inside its own transaction, once
// before and once after it opens or renews the ask, each a batch on that transaction
// (attachApproval, queueApprovals). Run in two concurrent waves, one on documents of their own
// and one on a single document, every request finishes, none fails, each document is left
// awaiting its one open request, and the transactions give every connection back. What one
// request answers in sequence - 201 for the first, 200 for a repeat - is reviews_test.go's
// (TestApprovalRequestOpensAnAskWhoseAnswerPinsAReviewToTheDocumentVersion).
func TestApprovalRequestsSentAtOnceReadTheirApprovalInTheirTransaction(t *testing.T) {
	t.Parallel()
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documents := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	first := createInteractionIssue(t, handler, "APPR", "Approve the first", "A spec")
	documents := []string{first.PrimaryArtifactID}
	for range 7 {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "APPR", "title": "Approve another", "spec": "A spec", "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
		}
		documents = append(documents, decodeBody[struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, response).PrimaryArtifactID)
	}
	request := func(artifactID string) int {
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests",
			map[string]any{"actor": sessionActor()}).Code
	}
	// atOnce sends each of targets' requests at the same moment and answers their statuses, or
	// fails when one never answers: its one bound is the test binary's deadline (testwait.Receive).
	atOnce := func(targets []string) []int {
		t.Helper()
		statuses := make([]int, len(targets))
		var started, finished sync.WaitGroup
		started.Add(1)
		for index, artifactID := range targets {
			finished.Add(1)
			go func() {
				defer finished.Done()
				started.Wait()
				statuses[index] = request(artifactID)
			}()
		}
		done := make(chan struct{})
		go func() { finished.Wait(); close(done) }()
		started.Done()
		testwait.Receive(t, "every approval request sent at once to answer", done)
		return statuses
	}

	// At once, one each on eight documents: every one opens its document's ask.
	for index, code := range atOnce(documents) {
		if code != http.StatusCreated {
			t.Fatalf("request %d on its own document: status=%d, want 201", index, code)
		}
	}
	// At once, sixteen on one document that already has its request open. Each is 200: a request
	// that names no summary takes the open request's own, so the hand-back switch sees
	// summary == openSummary on a request waiting on the human (reviews.go, renewApprovalAsk),
	// which is neither the 409 a different summary would be refused with nor the hand-back that
	// would answer 201. Nothing in this test replies to the ask or writes a version, so the turn
	// every one of them reads is the human's.
	same := make([]string, 16)
	for index := range same {
		same[index] = documents[1]
	}
	for index, code := range atOnce(same) {
		if code != http.StatusOK {
			t.Fatalf("request %d on one document: status=%d, want 200", index, code)
		}
	}
	for _, artifactID := range documents {
		if got := readApproval(t, handler, artifactID); got.Approval == nil || got.Approval.State != "awaiting" || got.Approval.AskID == nil {
			t.Fatalf("document %s approval = %#v, want awaiting its open request", artifactID, got.Approval)
		}
	}
	if use := database.Pool.Use(); use.InUse != 0 {
		t.Fatalf("connections still in use after every request answered: %d", use.InUse)
	}
}

// countedApprovalRequest opens a document's approval request through the handler on a request a
// roundTrips counts (countedRead, issue_read_batch_test.go).
func countedApprovalRequest(t *testing.T, handler http.Handler, artifactID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"actor": sessionActor()})
	if err != nil {
		t.Fatalf("encode the approval request: %v", err)
	}
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/approval-requests", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// One approval request on a document with no request open costs at most twenty-two round trips,
// counted as the issue read's are (roundTrips): one for each statement it runs on its own and one
// for each batch, whatever that batch holds. The budget is a ceiling, and the enumeration below
// is where it goes today: the first seven read the coverage the answer reports, on the pool before
// the request's transaction opens (approvalCoverage); the thirteen after them are the transaction
// itself, and all but two of those are reads: this route's writes are queued and sent as one
// batch before the commit, which is what holds its transaction id to its database work
// (INFRA-636). The last two store the display trees of the texts it wrote, after the commit. In
// the order it sends them:
//
//  1. the artifact row the coverage is read for (loadArtifact);
//  2. a batch of 3: that artifact's versions, its latest review and its open approval request
//     (loadArtifactRow, queueArtifactDetails);
//  3. a batch of 2: the issue's spec and whether it has a plan, and the documents that owe their
//     block index with their gap lines (issueCoverageOverTuplesAndOwedGaps,
//     queueOwedDocumentGaps);
//  4. the head cut of the tuple log (docs.WalkIssueTree's refs.HeadCut);
//  5. the issue's conversation items, of which there are none (refs.ConversationNodes);
//  6. the recursive subtree walk under the issue (refs.TreeSubtrees);
//  7. the issue's ancestor chain and view mark (docs.readViewChain); this spec marks no
//     requirement, so readTreeRequirements returns without loading block nodes and the coverage
//     asks nothing further - no descendant walk and no count of what serves each requirement;
//  8. begin, the request's own transaction;
//  9. the artifact row (loadArtifact);
//  10. a batch of 3: that artifact's versions, its latest review and its open approval request
//     (loadArtifactRow, queueArtifactDetails);
//  11. a batch of 2: the owning issue's owner lock, and in the statement after it the row that
//     lock guards (requireOpenOwner, through store.LockOwnerAndRead). The lock assigns no
//     transaction id, and the read is a statement of its own because a statement's snapshot is
//     taken before it waits;
//  12. the document's settled version number (settledVersionNumber);
//  13. a batch of 4: the approval before the write - the latest review and the open request
//     (attachApproval's queueApprovals) - the transaction's now(), which dates the new ask's node
//     and is the default its row's created_at takes, and the head's cut of the tuple log, whose
//     seq the new ask's node pins as the store revision the request was taken at
//     (requestArtifactApproval, refs.QueueHeadCut);
//  14. the open approval ask naming this document, which there is none of
//     (docs.OpenApprovalAskForDocuments);
//  15. the person this request reaches, which the answer names (askWaitingOnPerson, the one rule
//     every read of that person shares), read over the owner before the ask's row is queued;
//  16. the latest approved review the request carries (docs.LatestApprovedReview); the new ask's
//     id is drawn in the process (openApprovalAsk), its time and its store revision were read in
//     13, and the ref_key that spells the version node its approves tuple names was read with the
//     document's row in 9, so none of them costs a round trip;
//  17. the question's references as they stand, which the replacement is planned against
//     (queueNewAskReferences, through refs.PlanReplace);
//  18. the ask's block state, read for its parts' plan (refs.PlanAskParts, through
//     refs.ReadItemIndex);
//  19. a batch of 24: the whole write phase, sent back to back - the ask's node, in its
//     document's project, which costs no read of its own since the document was read with its
//     project, and its author's (graphstore.AskFacts), in the four statements of the store's
//     node write (graphstore.QueueNodes: the read of the supertags the nodes are typed by, the
//     insert, the check that no node moves project, and the move), the ask row naming its node,
//     the references and the row the ask owes the grammar's re-index queue, deleted with them
//     (refs.Replacement.Queue), its block state, the lock of that row, the part rows of its
//     question and options and their texts, the row it owes the forms index and its row of the
//     Decisions page's queue, each settled in a statement of its own, the ask.opened event in four
//     (the owner's lock, the issue's row, the append with its payload and the issue's last_seq, and
//     its own id written into that payload), the reference stamps of the question and of its parts,
//     the ask's authored_by, the base attributes the node write stated - its visibility, its state
//     and its project - its store revision and its approves tuple, appended under that event
//     (graphstore.QueueCaused), the head rows of the nodes the write names (graphstore.QueueHeads),
//     the author's follow, its follower row marked for the ask's node in one statement, its
//     followed_by stated under that ask.opened in the statement after it, and the head rows of the
//     node the follow names (asks.PlanFollowNewAsk, which reads nothing for an ask this request
//     creates);
//  20. commit;
//  21. the display trees of the texts the ask.opened event carries - its question and both option
//     labels - stored in one statement by the publish path (storeEventDisplayTrees);
//  22. the display tree of the question the request wrote, stored by the route itself, which the
//     table already holds from the statement before it (`on conflict do nothing`): the route
//     stores what it wrote whether or not the event it published carried the same text.
//
// What it does not send is the document the question's `#id`s would resolve against, which no
// read of the ask row this transaction has only queued can answer (refs.PendingAskDocument).
// The question is "Approve <document> (version N)?", with the requester's summary after it when
// there is one: it holds no `#` candidate, so neither the question nor the parts that carry it
// ever ask for that document, and the read is not made. A request whose summary writes a `#`
// pays it once for the two of them.
//
// The transaction takes its transaction id at the first statement of 19 and gives it up at 20;
// the per-route window test measures that window, and this test bounds what the route sends
// around it. The one-phase route sent thirty-seven, its write statements each a round trip of
// its own between the reads.
func TestApprovalRequestsCostAtMostTwentyTwoRoundTrips(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the request's
		// own statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of the document is database work of its own on the same pool; a delay no
		// test run reaches keeps it out of the count.
		settle: time.Hour,
	})
	issue := createInteractionIssue(t, handler, "APPRC", "Count the request", "A spec")
	counter.take()

	response := countedApprovalRequest(t, handler, issue.PrimaryArtifactID)
	counted := counter.take()
	if response.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", response.Code, response.Body.String())
	}
	// A ceiling, not an equality: the round trips are counted through helpers several other
	// requests share, so an exact count reds this row for a change that sends one round trip
	// fewer and costs nothing. What the name states is the cost bound, and the enumeration above
	// says where the twenty-two go.
	const ceiling = 22
	if len(counted) > ceiling {
		t.Fatalf("the approval request made %d round trips, want at most %d:\n%s",
			len(counted), ceiling, strings.Join(roundTripLines(counted), "\n"))
	}
}

// roundTripLines is each counted round trip's first words, numbered, so a failure names what the
// request sent without printing whole statements.
func roundTripLines(counted []string) []string {
	lines := make([]string, len(counted))
	for index, statement := range counted {
		first := statement
		for _, line := range strings.Split(statement, "\n") {
			if first = strings.TrimSpace(line); first != "" {
				break
			}
		}
		lines[index] = strconv.Itoa(index+1) + ". " + first
	}
	return lines
}
