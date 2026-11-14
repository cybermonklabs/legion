package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The route window table holds the write routes INFRA-636 changed that had no xid window test
// of their own (xid_window_test.go) to the rule those tests hold theirs to (A1): a route holds
// its transaction id for its database statements alone, with no document work and no
// statement-free gap inside its window. Each row is a route, readied the way the route table
// readies it (route_owner_lock_test.go, routeOwnerLockCase.ready) - the rows it shares with that
// table borrow its ready function, the rest have one of their own here - on a server whose pool
// carries the xid tracer and whose issue's spec is the load harness's large one (xidSpec), so
// a document read or render a route ran inside its window would show as the gap it is. The
// request goes through the fixture's own server under a marked context (storetest.MarkXid), so
// only the transactions the request itself ran are judged, and once the route answered the row's
// own check holds it to having made the write it means, so a window measured over a refusal is a
// rig failure and never a pass.
//
// Every row declares its window (storetest.XidWindowOf): the most statements one transaction of
// the request may run while it holds its transaction id, every one of them named. A route whose
// window holds only a write and its COMMIT passes trivially, and that is what the row is for: it
// shows the window rather than asserting it. A route added to the table is a row added here.

// routeXidWindowCase is one row of the window table.
type routeXidWindowCase struct {
	// name is the subtest's name.
	name string
	// route is the route and the function that serves it, as a failure names it.
	route string
	// ready stands the row's server up on base and readies its request (routeOwnerLockCase.ready).
	ready func(t *testing.T, base routeFixture) lockedItemWrite
	// window is what each transaction of the request may hold while it holds its transaction id.
	window storetest.XidShape
	// closedIssue says the request is made on a closed issue, whose documents owe their
	// settlements until it reopens, so the measurement waits for the database to fall quiet
	// rather than for every settlement (awaitQuietDatabase).
	closedIssue bool
}

// routeXidWindowTable is the window table.
var routeXidWindowTable = []routeXidWindowCase{
	// The review's one write phase is the ledger's batch - the review row and artifact.approved
	// - with everything it is decided on read before it: the document under its owner's
	// advisory lock, its settled version and whether an approval request is open.
	{
		name: "the review route", route: "POST /api/v1/artifacts/{id}/reviews (createArtifactReview)",
		ready: readyArtifactReview, window: storetest.XidBatchedWindow,
	},
	// Answering the open request adds the ask's answer, its block state, its parts' index and
	// ask.answered to that batch (closeAskTx queues them on the ledger), and nothing to the
	// window.
	{
		name: "the review route answering an approval request", route: "POST /api/v1/artifacts/{id}/reviews (createArtifactReview, closeAskTx)",
		ready: readyReviewAnsweringApprovalRequest, window: storetest.XidBatchedWindow,
	},
	// The reopen's document work - the withdrawn asks read back, their blocks and copies written
	// open into the spec (prepareRestoration, docs.WriteIssueAskStates) - runs before the issue's
	// update and queues its writes. The restored asks' parts are planned after that update
	// (recordRestoredAsksTx, queueAskParts), and their plan's reads are database reads, three per
	// ask, so this window grows with the asks the reopen restores; this fixture restores two.
	{
		name: "the issue reopen", route: "PATCH /api/v1/issues/{key} reopening a closed issue (patchIssue, prepareRestoration)",
		ready: readyIssueReopen, closedIssue: true, window: storetest.XidWindowOf(11,
			"the issue row's own update, then for each of its two restored asks the three reads that "+
				"plan its parts' index - its block state, its blocks and their references - then the two "+
				"reads that load the issue it answers with, the batch restoring its asks and appending "+
				"ask.opened for each, and the COMMIT ending them"),
	},
	{
		name: "the title correction route", route: "PATCH /api/v1/artifacts/{id} (renameArtifact)",
		ready: readyArtifactRename, window: storetest.XidWindowOf(4,
			"the title's update, the one batch reading the versions and the approval the renamed row "+
				"answers with (loadArtifactDetails), the batch appending artifact.renamed and the COMMIT ending them"),
	},
	{
		name: "the document removal route", route: "DELETE /api/v1/artifacts/{id} (removeArtifact)",
		ready: readyArtifactRemoval, window: storetest.XidWindowOf(16,
			"the twelve deletes of the document's rows in other tables - its versions, updates, snapshots, "+
				"checkpoints, search row, search backfill, compaction debt, search-term queue, threads, "+
				"references, grammar debt and excerpt backoff - the delete of its own row, the "+
				"dispatch_doc_removed notify, the batch appending artifact.removed and the COMMIT ending them"),
	},
	// The delete of the gates a publish no longer names writes nothing on this fixture's first
	// publish, so the window opens at the upsert; a publish that drops a gate opens it at the
	// delete, and the shape counts it.
	{
		name: "the design gates route", route: "PUT /api/v1/issues/{key}/design-gates (putIssueDesignGates)",
		ready: readyDesignGates, window: storetest.XidWindowOf(3,
			"the delete of the gates the publish no longer names, the upsert of the ones it names and the COMMIT ending them"),
	},
	{
		name: "the project default assignee route", route: "PUT /api/v1/projects/{key}/default-assignee (putProjectDefaultAssignee)",
		ready: readyProjectDefaultAssignee, window: storetest.XidWindowOf(3,
			"the project row's update, the batch appending settings.project_default_assignee.updated and the COMMIT ending them"),
	},
	{
		name: "the project gated steps route", route: "PUT /api/v1/projects/{key}/gated-steps (putProjectGatedSteps)",
		ready: readyProjectGatedSteps, window: storetest.XidWindowOf(3,
			"the project row's update, the batch appending project.updated and the COMMIT ending them"),
	},
	{
		name: "the ask follow route", route: "PUT /api/v1/asks/{id}/followers/{session_id} (changeAskFollower)",
		ready: readyAskFollow, window: storetest.XidWindowOf(3,
			"the follower row's insert, the batch appending ask.follower_added and the COMMIT ending them"),
	},
	{
		name: "the ask unfollow route", route: "DELETE /api/v1/asks/{id}/followers/{session_id} (changeAskFollower)",
		ready: readyAskUnfollow, window: storetest.XidWindowOf(3,
			"the follower row's delete, the batch appending ask.follower_removed and the COMMIT ending them"),
	},
	// The Retry runs three transactions, the listener call between the last two holding none:
	// the claim, the resolution's record on the attempt it claimed (recordCommentDeliveryResolution)
	// - that update and its COMMIT - and the settle - the attempt's update, the batch appending
	// the delivery's receipt and the COMMIT. One shape judges all three, so its bound is the
	// claim's.
	{
		name: "the mention retry route", route: "POST /api/v1/comments/{id}/deliveries (claimCommentDeliveryAttempt)",
		ready: readyMentionRetry, window: storetest.XidWindowOf(4,
			"the claim's lock of the mention row, its locked read of the pending attempt, the new "+
				"attempt's insert and the COMMIT ending them"),
	},
	{
		name: "the project file upload route", route: "POST /api/v1/projects/{key}/artifacts on a file it has (storeArtifact)",
		ready: readyProjectFileUpload, window: storetest.XidBatchedWindow,
	},
	{
		name: "the project document upload route", route: "POST /api/v1/projects/{key}/artifacts on a document it has (storeArtifact)",
		ready: readyProjectDocumentUpload, window: storetest.XidBatchedWindow,
	},
	{
		name: "the agent conversation upload route", route: "POST /api/v1/agents/{session_id}/artifacts (uploadAgentArtifact, storeArtifact)",
		ready: readyAgentArtifactUpload, window: storetest.XidBatchedWindow,
	},
	// A broadcast writes inline, five statements per recipient, so its window grows with them;
	// this one goes to two.
	{
		name: "the broadcast route", route: "POST /api/v1/broadcasts (writeBroadcast)",
		ready: readyBroadcast, window: storetest.XidWindowOf(14,
			"the broadcast's insert, its keyed request's recovery point, then for each of its two "+
				"recipients the message's insert, the read of its block state, the batch writing its "+
				"block index, the batch appending message.created and its first attempt's insert, and "+
				"last the keyed request's commit mark and the COMMIT ending them"),
	},
	// The direct message runs three transactions, the listener call holding none: the create -
	// its write phase's one batch, the keyed request's commit mark and its COMMIT - the
	// delivery's claim, and its settle - the attempt's update, the batch appending
	// message.delivery, the commit mark and the COMMIT. One shape judges all three, so its
	// bound is the claim's.
	{
		name: "the direct message route", route: "POST /api/v1/agents/{session_id}/messages (createAgentMessage, createStoredMessage)",
		ready: readyDirectMessage, window: storetest.XidWindowOf(5,
			"the claim's lock of the message row, its read of the pending attempt, the attempt's "+
				"claim, the keyed request's commit mark and the COMMIT ending them"),
	},
	{
		name: "the handoff route", route: "POST /api/v1/issues/{key}/handoffs/{phase} (recordHandoff)",
		ready: readyHandoff, window: storetest.XidWindowOf(3,
			"the handoff's insert, the batch appending handoff.recorded and the COMMIT ending them"),
	},
	// A decline of a block ask writes the block resolved into the spec, and that document write
	// is queued on the ledger with the ask's own writes: the window is the one batch.
	{
		name: "the ask decline route", route: "POST /api/v1/asks/{id}/decline on a block ask (declineAsk)",
		ready: readyBlockAskDecline, window: storetest.XidBatchedWindow,
	},
	{
		name: "the claim release route", route: "DELETE /api/v1/issues/{key}/claim (releaseIssueClaim, claimWrite)",
		ready: readyClaimRelease, window: storetest.XidBatchedWindow,
	},
	{
		name: "the document ask route", route: "POST /api/v1/artifacts/{id}/asks (createArtifactAsk, createAskFor)",
		ready: readyDocumentAsk, window: storetest.XidBatchedWindow,
	},
	{
		name: "the document comment route", route: "POST /api/v1/artifacts/{id}/comments (createArtifactComment, createCommentFor)",
		ready: readyDocumentComment, window: storetest.XidBatchedWindow,
	},
	{
		name: "the inbox mark-all route", route: "POST /api/v1/me/inbox/read (markInboxRead)",
		ready: readyInboxMarkAll, window: storetest.XidWindowOf(5,
			"the lock of the caller's thread rows, the marks' update, the lock of the project its "+
				"event names, the batch appending user_thread_state.updated and the COMMIT ending them"),
	},
	{
		name: "the inbox dismiss route", route: "POST /api/v1/me/inbox/dismiss (dismissInbox)",
		ready: readyInboxDismiss, window: storetest.XidWindowOf(5,
			"the lock of the caller's thread rows, the dismissals' update, the lock of the project its "+
				"event names, the batch appending user_thread_state.updated and the COMMIT ending them"),
	},
}

// TestXidWindowRouteTable measures every row of routeXidWindowTable.
func TestXidWindowRouteTable(t *testing.T) {
	for _, row := range routeXidWindowTable {
		t.Run(row.name, func(t *testing.T) { measureRouteXidWindow(t, row) })
	}
}

// measureRouteXidWindow readies row on a traced server with the harness's large spec, waits for
// every settlement the seeding owed to finish (for a closed issue, for the database to fall
// quiet), so the request is measured alone, and judges the transactions the request ran against
// the row's window.
func measureRouteXidWindow(t *testing.T, row routeXidWindowCase) {
	t.Helper()
	tracer := storetest.NewXidTracer()
	spec, _ := xidSpec(1)
	write := row.ready(t, routeFixture{options: testServerOptions{tracer: tracer}, spec: spec})
	awaitQuietDatabase(t, write.database, !row.closedIssue)
	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write.server.ServeHTTP(w, r.WithContext(storetest.MarkXid(r.Context())))
	})
	var answer *httptest.ResponseRecorder
	report := tracer.Measure(t, write.database.Pool, func() { answer = write.send(marked) })
	write.answered(t, answer)
	t.Logf("route %s", row.route)
	storetest.AssertXidWindows(t, report, row.window)
}

// readyReviewAnsweringApprovalRequest drives POST /api/v1/artifacts/{id}/reviews on a spec with
// an approval request open, so the person's review answers that request (closeAskTx) and
// reviews the document at the version the request named.
func readyReviewAnsweringApprovalRequest(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Review answering a request")
	waitForArtifactVersion(t, fixture.handler, fixture.artifactID, 1)
	requested := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/approval-requests", map[string]any{
		"summary": "The migration section is rewritten; the rest of the spec stands.", "actor": sessionActor(),
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("open the approval request: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[approvalRequestAsk](t, requested)
	if request.Ask == nil {
		t.Fatalf("the approval request opened no ask: %s", requested.Body.String())
	}
	return fixture.routeWrite(t, "the review answering the open request",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/artifacts/"+fixture.artifactID+"/reviews", map[string]any{"state": "approved"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("review the spec: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if review := decodeBody[model.ArtifactReview](t, answer); review.AskID == nil || *review.AskID != request.Ask.ID {
				t.Fatalf("the review = %#v, want the one answering approval request %s", review, request.Ask.ID)
			}
		})
}

// readyIssueReopen drives PATCH /api/v1/issues/{key} reopening a closed issue whose close
// withdrew an anchored ask and the ask settlement opened from the spec's `:::ask` block, so the
// reopen restores both and writes the block's state back into the spec (prepareRestoration).
func readyIssueReopen(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.spec += "\n" + transportAsk
	fixture := base.issue(t, "Reopen")
	awaitIndexedAskBlock(t, fixture.handler, fixture.artifactID, "decision", "Which transport?")
	_, quotes := xidSpec(1)
	if created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks", map[string]any{
		"question": "Who owns this requirement?",
		"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
		"anchor":   map[string]any{"artifact": "spec", "quote": quotes[0]}, "actor": sessionActor(),
	}); created.Code != http.StatusCreated {
		t.Fatalf("open the anchored ask: status=%d body=%s", created.Code, created.Body.String())
	}
	if closed := dispatchRequest(t, fixture.handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
		map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	return fixture.routeWrite(t, "the reopened issue",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
				map[string]any{"status": "todo"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("reopen the issue: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if restored := fixture.rowCount(t, `select count(*) from asks where issue_key = $1 and state = 'open'`, fixture.issueKey); restored != 2 {
				t.Fatalf("the reopened issue holds %d open asks, want the two its close withdrew", restored)
			}
		})
}

// readyAskUnfollow drives DELETE /api/v1/asks/{id}/followers/{session_id}, a person taking a
// follower off an ask, which deletes the follower row under the ask owner's lock
// (changeAskFollower).
func readyAskUnfollow(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Ask unfollow")
	created := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks",
		map[string]any{"question": "Who follows this?"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	if followed := dispatchRequest(t, fixture.handler, http.MethodPut, "/api/v1/asks/"+ask.ID+"/followers/s3", nil, "alice"); followed.Code != http.StatusNoContent {
		t.Fatalf("follow the ask: status=%d body=%s", followed.Code, followed.Body.String())
	}
	return fixture.routeWrite(t, "the ask's follower row it deletes",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/asks/"+ask.ID+"/followers/s3", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusNoContent {
				t.Fatalf("unfollow the ask: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if followers := fixture.rowCount(t, `select count(*) from ask_followers where ask_id = $1 and session_id = 's3'`, ask.ID); followers != 0 {
				t.Fatalf("the ask still has %d followers named s3", followers)
			}
		})
}

// readyProjectDocumentUpload drives POST /api/v1/projects/{key}/artifacts naming a document the
// project already has, the harness's large spec, which writes its next version through the
// document service (storeArtifact).
func readyProjectDocumentUpload(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Project document upload")
	document := createProjectDocument(t, fixture.handler, "LOCK", "Design notes", base.spec)
	waitForArtifactVersion(t, fixture.handler, document.ID, 1)
	revised := strings.Replace(base.spec, "Revision token r0", "Revision token r1", 1)
	return fixture.routeWrite(t, "the document's next version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/LOCK/artifacts", map[string]string{
				"name": "Design notes", "content": revised,
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the document again: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if uploaded := decodeBody[uploadedArtifact](t, answer); uploaded.Artifact.ID != document.ID || uploaded.Version.Number != 2 {
				t.Fatalf("the upload wrote version %d of %s, want version 2 of %s",
					uploaded.Version.Number, uploaded.Artifact.ID, document.ID)
			}
		})
}

// readyAgentArtifactUpload drives POST /api/v1/agents/{session_id}/artifacts naming a file the
// conversation already has, which writes its next version under the conversation's lock
// (uploadAgentArtifact, storeArtifact).
func readyAgentArtifactUpload(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.options.files = filestest.NewMemory()
	fixture := base.issue(t, "Agent conversation upload")
	target := "/api/v1/agents/" + agentSession + "/artifacts"
	first := multipartRequest(t, fixture.handler, target, map[string]string{"name": "shot.png"},
		"shot.png", "image/png", pngBytes("first"), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload the conversation's file: status=%d body=%s", first.Code, first.Body.String())
	}
	file := decodeBody[uploadedArtifact](t, first).Artifact
	return fixture.routeWrite(t, "the file's next version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return multipartRequest(t, handler, target, map[string]string{"name": "shot.png"},
				"shot.png", "image/png", pngBytes("second"), "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the file again: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if uploaded := decodeBody[uploadedArtifact](t, answer); uploaded.Artifact.ID != file.ID || uploaded.Version.Number != 2 {
				t.Fatalf("the upload wrote version %d of %s, want version 2 of %s",
					uploaded.Version.Number, uploaded.Artifact.ID, file.ID)
			}
		})
}

// readyBroadcast drives POST /api/v1/broadcasts to two live sessions, which writes the broadcast,
// its recovery point and each recipient's message, index, event and first attempt in one
// transaction (writeBroadcast) and then delivers to each.
func readyBroadcast(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener, _ := newBroadcastListener(t, broadcastSessions)
	base.options.envoyURL = listener.URL
	fixture := base.issue(t, "Broadcast")
	return fixture.routeWrite(t, "the broadcast and its messages",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postBroadcast(t, handler, map[string]any{
				"body": "Stand down and report status.", "delivery": "steer",
				"session_ids": []string{"planner", "tester"},
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("broadcast: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if sent := decodeBody[broadcastResponse](t, answer); len(sent.Recipients) != 2 {
				t.Fatalf("the broadcast reached %d recipients, want planner and tester", len(sent.Recipients))
			}
		})
}

// readyDirectMessage drives POST /api/v1/agents/{session_id}/messages, a person's direct message
// to a live session, which stores the message (createStoredMessage) and delivers it.
func readyDirectMessage(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	listener, _ := newBroadcastListener(t, xidDeliverySessions)
	base.options.envoyURL = listener.URL
	fixture := base.issue(t, "Direct message")
	return fixture.routeWrite(t, "the direct message",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postAgentMessage(t, handler, "s1", map[string]any{
				"body": "Read the migration section of the spec and tell me what it costs.", "delivery": "aside",
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("direct message to s1: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// readyHandoff drives POST /api/v1/issues/{key}/handoffs/{phase}, a session's handoff of one
// phase, which writes its next version under the issue's owner lock (recordHandoff).
func readyHandoff(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Handoff")
	return fixture.routeWrite(t, "the handoff",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postHandoff(t, handler, fixture.issueKey, "implement", `{"summary":"The migration section is in."}`)
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("record the handoff: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// readyBlockAskDecline drives POST /api/v1/asks/{id}/decline on the ask settlement opened from
// the spec's `:::ask` block, whose block the decline writes resolved into the spec.
func readyBlockAskDecline(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.spec += "\n" + transportAsk
	fixture := base.issue(t, "Decline")
	awaitIndexedAskBlock(t, fixture.handler, fixture.artifactID, "decision", "Which transport?")
	askID := blockAskID(t, fixture.handler, fixture.issueKey, "decision")
	note := "The team decides this one, not me."
	return fixture.routeWrite(t, "the declined ask",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/decline", declineBody(&note), "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("decline the ask: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if resolution := storedAskResolution(t, fixture.database, askID); resolution.Kind != "declined" {
				t.Fatalf("the ask's resolution = %q, want declined", resolution.Kind)
			}
		})
}

// readyClaimRelease drives DELETE /api/v1/issues/{key}/claim on an issue a person claimed, which
// clears the claim (claimWrite).
func readyClaimRelease(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Claim release")
	if claimed := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/claim", map[string]any{}, "alice"); claimed.Code != http.StatusOK {
		t.Fatalf("claim the issue: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	return fixture.routeWrite(t, "the released claim",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+fixture.issueKey+"/claim", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("release the claim: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// projectDesignNotes files the large spec as a project document no issue holds, the owner the
// document routes write under, and waits for its first version.
func projectDesignNotes(t *testing.T, fixture itemRowFixture, spec string) model.Artifact {
	t.Helper()
	document := createProjectDocument(t, fixture.handler, "LOCK", "Design notes", spec)
	waitForArtifactVersion(t, fixture.handler, document.ID, 1)
	return document
}

// readyDocumentAsk drives POST /api/v1/artifacts/{id}/asks, a session's anchored ask on a
// project document (createArtifactAsk, createAskFor).
func readyDocumentAsk(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Document ask")
	document := projectDesignNotes(t, fixture, base.spec)
	_, quotes := xidSpec(1)
	return fixture.routeWrite(t, "the document's ask",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
				"question": "Which team signs off on this requirement?",
				"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
				"anchor":   map[string]any{"artifact": document.Slug, "quote": quotes[0]}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("ask on the document: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// readyDocumentComment drives POST /api/v1/artifacts/{id}/comments, a session's anchored comment
// on a project document (createArtifactComment, createCommentFor).
func readyDocumentComment(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Document comment")
	document := projectDesignNotes(t, fixture, base.spec)
	_, quotes := xidSpec(1)
	return fixture.routeWrite(t, "the document's comment",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
				"body":   "Does this still hold once the outbox is sharded?",
				"anchor": map[string]any{"artifact": document.Slug, "quote": quotes[0]}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("comment on the document: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// unreadReplyThread gives alice a Reply thread in her Inbox: her own comment on the issue, which
// gives her the thread's user_threads row, and a session's reply to it, which leaves that row
// unread, so a mark or a dismissal of her Inbox moves it.
func unreadReplyThread(t *testing.T, fixture itemRowFixture) model.Comment {
	t.Helper()
	root := createThreadComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body": "Does the migration section still hold?",
	}, "alice")
	sessionComment(t, fixture.handler, "/api/v1/issues/"+fixture.issueKey+"/comments", map[string]any{
		"body": "It does, and the next release carries it.", "reply_to": root.ID,
	})
	return root
}

// readyInboxMarkAll drives POST /api/v1/me/inbox/read, the Inbox's Mark all read, over one
// unread Reply thread (markInboxReadThreads).
func readyInboxMarkAll(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Inbox mark all")
	unreadReplyThread(t, fixture)
	return fixture.routeWrite(t, "the thread's read mark",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/inbox/read", map[string]any{
				"read_through": time.Now().UTC().Format(time.RFC3339Nano),
			}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("mark the inbox read: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if marked := decodeBody[inboxReadResponse](t, answer).Marked; marked != 1 {
				t.Fatalf("the mark-all marked %d threads, want the one unread thread", marked)
			}
		})
}

// readyInboxDismiss drives POST /api/v1/me/inbox/dismiss under the list's filters, over one
// unread Reply thread (dismissInboxThreads).
func readyInboxDismiss(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Inbox dismiss")
	unreadReplyThread(t, fixture)
	return fixture.routeWrite(t, "the thread's dismissal",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postInboxDismiss(t, handler, "alice", map[string]any{
				"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("dismiss the inbox: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if dismissed := dismissedBy(t, answer); dismissed != 1 {
				t.Fatalf("the dismissal changed %d threads, want the one unread thread", dismissed)
			}
		})
}
