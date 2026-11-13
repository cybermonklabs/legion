package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The lock table (item_row_owner_lock_test.go) holds every writer of an item row behind the owner
// lock of the row it writes. This is its sibling for the routes of the two-phase order that write
// no item row - a review, a document's rename or removal, a delivery's receipt - and whose read-
// phase owner lock no other test held: with that lock deleted, every test of the route stayed
// green. Each row is held the way a row of the lock table is (holdWriterBehindOwnerLock): the test
// takes the owner's lock in a transaction of its own, and the route must wait for it having
// written nothing.
//
// Waiting alone would not tell the read-phase lock from the one the event's append takes: every
// route here appends its owner's event, and appending takes that owner's lock (events.Broker's
// lockEventOwner), so a route whose own lock was deleted still stops at the holder. It stops
// there with its first write already run, though, holding a transaction id, and that is what each
// row's second question reads.

// routeOwnerLockCase is one row of the route table: a route of the two-phase order and how to
// ready it.
type routeOwnerLockCase struct {
	// name is the subtest's name.
	name string
	// route is the route the row drives and the function that serves it, as a failure names it.
	route string
	// ready stands up a server of this row's own, seeds what the route needs and readies the
	// call on a handler of one connection, so the backend the wait watches is the route's.
	ready func(t *testing.T) lockedItemWrite
}

// routeOwnerLockTable is the route table.
var routeOwnerLockTable = []routeOwnerLockCase{
	{name: "the review route", route: "POST /api/v1/artifacts/{id}/reviews (createArtifactReview)", ready: readyArtifactReview},
	{name: "the plan review route", route: "POST /api/v1/artifacts/{id}/plan-reviews (createArtifactPlanReview)", ready: readyPlanReview},
	{name: "the title correction route", route: "PATCH /api/v1/artifacts/{id} (renameArtifact)", ready: readyArtifactRename},
	{name: "the document removal route", route: "DELETE /api/v1/artifacts/{id} (removeArtifact)", ready: readyArtifactRemoval},
	{name: "the named version route", route: "POST /api/v1/artifacts/{id}/versions (createNamedVersion)", ready: readyNamedVersion},
	{name: "the document edit route", route: "POST /api/v1/artifacts/{id}/edits (editArtifact)", ready: readyDocumentEdit},
	{name: "the mention retry route", route: "POST /api/v1/comments/{id}/deliveries (claimCommentDeliveryAttempt)", ready: readyMentionRetry},
	{name: "the message retry route", route: "POST /api/v1/messages/{id}/deliveries (recordPendingMessageDelivery)", ready: readyMessageRetry},
	{name: "the result record route held on the recording issue", route: "POST /api/v1/issues/{key}/results (recordResultUnderOwnerLocks)", ready: readyResultHeldOnTheRecordingIssue},
	{name: "the result record route held on the check's issue", route: "POST /api/v1/issues/{key}/results (recordResultUnderOwnerLocks)", ready: readyResultHeldOnTheChecksIssue},
	{name: "the design gates route", route: "PUT /api/v1/issues/{key}/design-gates (putIssueDesignGates)", ready: readyDesignGates},
	{name: "the project default assignee route", route: "PUT /api/v1/projects/{key}/default-assignee (putProjectDefaultAssignee)", ready: readyProjectDefaultAssignee},
	{name: "the project file upload route", route: "POST /api/v1/projects/{key}/artifacts on a file it has (storeArtifact)", ready: readyProjectFileUpload},
	{name: "the ask follow route", route: "PUT /api/v1/asks/{id}/followers/{session_id} (changeAskFollower)", ready: readyAskFollow},
}

// TestEveryRouteOnTheRouteTableWaitsForItsOwnerLockHoldingNoTransactionID holds the owner lock
// of each route of routeOwnerLockTable and requires the route to wait for it having written
// nothing, then to finish once the lock is given back (holdWriterBehindOwnerLock).
func TestEveryRouteOnTheRouteTableWaitsForItsOwnerLockHoldingNoTransactionID(t *testing.T) {
	for _, row := range routeOwnerLockTable {
		t.Run(row.name, func(t *testing.T) { holdWriterBehindOwnerLock(t, row.route, row.ready(t)) })
	}
}

// readyArtifactReview drives POST /api/v1/artifacts/{id}/reviews on an issue's spec with no
// approval request open, which inserts the review under the issue's owner lock the route takes
// before it reads the settled version (createArtifactReview, requireOpenOwner).
func readyArtifactReview(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Review", "A spec.\n")
	return fixture.routeWrite(t, "the review it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost,
				"/api/v1/artifacts/"+fixture.artifactID+"/reviews", map[string]any{"state": "approved"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("review the spec once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if reviews := fixture.reviewCount(t, fixture.artifactID); reviews != 1 {
				t.Fatalf("the spec holds %d reviews, want the one the route wrote", reviews)
			}
		})
}

// readyPlanReview drives POST /api/v1/artifacts/{id}/plan-reviews on an issue's plan, which
// inserts the plan review under the issue's owner lock the route takes before it reads the plan's
// settled versions (createArtifactPlanReview, requireOpenOwner).
func readyPlanReview(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Plan review", "A spec.\n")
	plan := uploadDocument(t, fixture.handler, "/api/v1/issues/"+fixture.issueKey, "plan.md", "# Plan\n\nOne task.\n")
	return fixture.routeWrite(t, "the plan review it inserts",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews",
				map[string]any{"actor": sessionActor(), "version": plan.Version.Number})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("record the plan review once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if reviews := fixture.reviewCount(t, plan.Artifact.ID); reviews != 1 {
				t.Fatalf("the plan holds %d reviews, want the one the route wrote", reviews)
			}
		})
}

// readyArtifactRename drives PATCH /api/v1/artifacts/{id} on a document of the issue, which
// corrects its title under the issue's owner lock beginArtifactWrite takes before the owner's
// names are read (renameArtifact, requireOpenOwnerStatus).
func readyArtifactRename(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Rename", "A spec.\n")
	document := emptyDocument(t, fixture.handler, fixture.issueKey, "Notes")
	const corrected = "Field notes"
	return fixture.routeWrite(t, "the document's corrected title",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return correctTitle(t, handler, document.ID, corrected)
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("correct the title once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			var name string
			if err := fixture.database.Pool.QueryRow(context.Background(),
				`select name from artifacts where id = $1`, document.ID).Scan(&name); err != nil {
				t.Fatalf("read the document's title: %v", err)
			}
			if name != corrected {
				t.Fatalf("the document's title = %q, want %q", name, corrected)
			}
		})
}

// readyArtifactRemoval drives DELETE /api/v1/artifacts/{id} on an untouched document of the
// issue, which removes it under the issue's owner lock beginArtifactWrite takes before the
// document's room (removeArtifact, requireOpenOwnerStatus).
func readyArtifactRemoval(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Removal", "A spec.\n")
	document := emptyDocument(t, fixture.handler, fixture.issueKey, "Scratch")
	return fixture.routeWrite(t, "the document it removes",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return removeDocument(t, handler, document.ID)
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusNoContent {
				t.Fatalf("remove the document once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			var left bool
			if err := fixture.database.Pool.QueryRow(context.Background(),
				`select exists (select 1 from artifacts where id = $1)`, document.ID).Scan(&left); err != nil {
				t.Fatalf("read whether the document is left: %v", err)
			}
			if left {
				t.Fatal("the document is still there after its removal answered 204")
			}
		})
}

// readyNamedVersion drives POST /api/v1/artifacts/{id}/versions on the issue's spec, which names
// its current version under the issue's owner lock (createNamedVersion, requireOpenOwner, and
// docs.NamedVersion's own lockArtifactOwner).
func readyNamedVersion(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Named version", "A spec.\n")
	return fixture.routeWrite(t, "the named version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/versions",
				map[string]string{"summary": "The version the held lock delays"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated || !decodeBody[model.Version](t, answer).Named {
				t.Fatalf("name the version once the lock was released: status=%d body=%s, want 201 and a named version",
					answer.Code, answer.Body.String())
			}
		})
}

// readyDocumentEdit drives POST /api/v1/artifacts/{id}/edits on the issue's spec, which applies
// its ops under the issue's owner lock (editArtifact, requireOpenOwnerStatus, and the live
// write's own lockArtifactOwner).
func readyDocumentEdit(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Document edit", "before\n")
	return fixture.routeWrite(t, "the document's edit",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+fixture.artifactID+"/edits", map[string]any{
				"ops":   []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
				"actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK || decodeBody[struct {
				Applied int `json:"applied"`
			}](t, answer).Applied != 1 {
				t.Fatalf("edit the document once the lock was released: status=%d body=%s, want 200 and one op applied",
					answer.Code, answer.Body.String())
			}
		})
}

// readyMentionRetry drives POST /api/v1/comments/{id}/deliveries, a person's Retry of a mention
// already delivered, which claims an attempt of its own under the comment's owner lock
// (claimCommentDeliveryAttempt, lockedCommentForDelivery) before it sends. The mention needs a
// listener to resolve against, so this row stands one up and lets the create's delivery settle.
//
// The claim is a transaction of its own that appends no event: it commits the attempt it opens
// and the send's later transactions take the owner's lock again. A claim that took no lock would
// therefore commit its attempt past the holder and then wait in the next transaction, with no
// transaction id, which the wait alone cannot tell from a claim still queued. The row's meanwhile
// tells them apart: while the route waits, the mention must still hold its one attempt.
func readyMentionRetry(t *testing.T) lockedItemWrite {
	t.Helper()
	listener := itemRowSessionListener(t, "steer")
	fixture := newItemRowFixture(t, testServerOptions{envoyURL: listener.URL}, "Mention retry", "A spec.\n")
	comment := decodeMentionedComment(t, postMentionedComment(t, fixture.handler, fixture.issueKey, map[string]any{
		"body":     "The session this mention names is sent it twice.",
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, fixture.database, comment.ID)
	write := fixture.routeWrite(t, "the delivery attempt it claims",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries",
				map[string]any{"delivery": "steer"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated || decodeBody[commentDeliveryRead](t, answer).Attempt != 2 {
				t.Fatalf("retry the mention once the lock was released: status=%d body=%s, want 201 and attempt 2",
					answer.Code, answer.Body.String())
			}
		})
	write.meanwhile = func(t *testing.T, _ pgx.Tx) {
		if attempts := fixture.rowCount(t, `select count(*) from comment_deliveries where comment_id = $1`, comment.ID); attempts != 1 {
			t.Fatalf("the mention holds %d attempts while this test holds its owner's lock, want its one "+
				"first attempt: the Retry committed its claim under a writer holding that owner", attempts)
		}
	}
	return write
}

// readyMessageRetry drives POST /api/v1/messages/{id}/deliveries, a person's Retry of a message
// already delivered to a session, which claims an attempt of its own under the issue's owner lock
// (recordPendingMessageDelivery, lockIssue) before it sends. The message needs a listener to take
// it, so this row stands one up and sends the message to that session before the lock is held.
// Its claim commits on its own, as the mention's does, so the row asks the same meanwhile
// (readyMentionRetry): while the route waits, the message must still hold its one attempt.
func readyMessageRetry(t *testing.T) lockedItemWrite {
	t.Helper()
	listener := itemRowSessionListener(t, "btw")
	fixture := newItemRowFixture(t, testServerOptions{envoyURL: listener.URL}, "Message retry", "A spec.\n")
	message := createIssueMessage(t, fixture.handler, fixture.issueKey, map[string]any{
		"body": "Did this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the message this row retries = %#v, want one attempt sent to the listener's session",
			message.Deliveries)
	}
	write := fixture.routeWrite(t, "the delivery attempt it claims",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
				map[string]any{"delivery": "btw"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated || decodeBody[model.MessageDelivery](t, answer).Attempt != 2 {
				t.Fatalf("retry the message once the lock was released: status=%d body=%s, want 201 and attempt 2",
					answer.Code, answer.Body.String())
			}
		})
	write.meanwhile = func(t *testing.T, _ pgx.Tx) {
		if attempts := fixture.rowCount(t, `select count(*) from message_deliveries where message_id = $1`, message.ID); attempts != 1 {
			t.Fatalf("the message holds %d attempts while this test holds its issue's lock, want its one "+
				"first attempt: the Retry committed its claim under a writer holding that owner", attempts)
		}
	}
	return write
}

// readyResultRecord drives POST /api/v1/issues/{key}/results from a child recording a result
// against its parent's check, which writes the result under the owner locks of both issues, the
// recording one and the check's, taken in one key order (recordResultUnderOwnerLocks,
// lockIssueOwners), and reads under them the version the check's words last changed in.
//
// Held on the child's lock, the row asks the plain questions: the result's event goes on the
// check's issue, so the recording issue's lock is one the write takes no other way.
//
// Held on the parent's, waiting proves nothing: the event's append takes that lock as the first
// statement of the write phase, before any write, so a record that lost its read-phase lock still
// waits there holding no transaction id. What the lock is for is the read after it, so the holder
// is a version that rewords the check: while the record waits it moves the check's row of the
// block index as a version's indexing does (refs.BlockIndexing.Queue, under the issue's owner
// lock) and commits. A record that read the check under the lock finds the version moved and
// judges the result by it; one that read the check before the lock records it against the words
// the version replaced.
func readyResultRecord(t *testing.T, heldOnTheChecksIssue bool) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Result record", "A spec.\n")
	verification := uploadDocument(t, fixture.handler, "/api/v1/issues/"+fixture.issueKey, "verification.md", twoChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "LOCK", "title": "Child", "parent": fixture.issueKey, "force": true}, "alice"))
	var reworded int
	write := fixture.routeWrite(t, "the result it records",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return recordResult(t, handler, child.Key, resultBody("k1", "fail", headCommit, map[string]any{"failed": 2}))
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("record the result once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if results := fixture.rowCount(t, `select count(*) from check_results where issue_key = $1`, child.Key); results != 1 {
				t.Fatalf("the child holds %d results, want the one the route recorded", results)
			}
			if recorded := decodeBody[recordedResult](t, answer).Result.CheckVersion; heldOnTheChecksIssue && recorded != reworded {
				t.Fatalf("the result was judged by check version %d, want %d, the version committed while it "+
					"waited on the check's issue: it read the check before that issue's owner lock", recorded, reworded)
			}
		})
	if !heldOnTheChecksIssue {
		write.owner = store.IssueOwner(child.Key)
		return write
	}
	write.meanwhile = func(t *testing.T, holder pgx.Tx) {
		if err := holder.QueryRow(context.Background(), `
			update document_blocks set changed_version = changed_version + 1
			where artifact_id = $1::uuid and block_id = 'k1' and type = 'check'
			returning changed_version
		`, verification.Artifact.ID).Scan(&reworded); err != nil {
			t.Fatalf("move the check's indexed version: %v", err)
		}
	}
	return write
}

func readyResultHeldOnTheRecordingIssue(t *testing.T) lockedItemWrite {
	return readyResultRecord(t, false)
}

func readyResultHeldOnTheChecksIssue(t *testing.T) lockedItemWrite { return readyResultRecord(t, true) }

// readyDesignGates drives PUT /api/v1/issues/{key}/design-gates, which rewrites the issue's
// design_gates rows under the issue's owner lock (putIssueDesignGates, store.LockOwnerAndRead). It
// appends no event, so that lock is the only one it takes: a publish without it would not wait at
// all.
func readyDesignGates(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Design gates", "A spec.\n")
	return fixture.routeWrite(t, "the issue's design gates",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return publishDesignGatesResponse(t, handler, fixture.issueKey, map[string]any{
				"gates": []issueDesignGate{{Step: "spec", ArtifactID: fixture.artifactID}}, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("publish the gates once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if gates := fixture.rowCount(t, `select count(*) from design_gates where issue = $1`, fixture.issueKey); gates != 1 {
				t.Fatalf("the issue holds %d design gates, want the one the route published", gates)
			}
		})
}

// readyProjectDefaultAssignee drives PUT /api/v1/projects/{key}/default-assignee, which updates
// the project's row under the project's owner lock it takes, with its read of the default the
// write moves from, before the update (putProjectDefaultAssignee, store.LockOwnerAndRead).
func readyProjectDefaultAssignee(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Default assignee", "A spec.\n")
	write := fixture.routeWrite(t, "the project's default assignee",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/LOCK/default-assignee",
				map[string]any{"assignee": "alice"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("set the default assignee once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if set := fixture.rowCount(t, `select count(*) from projects where key = $1 and default_assignee is not null`, "LOCK"); set != 1 {
				t.Fatal("the project names no default assignee after the route answered 200")
			}
		})
	write.owner = store.ProjectOwner("LOCK")
	return write
}

// readyProjectFileUpload drives POST /api/v1/projects/{key}/artifacts naming a file the project
// already has, whose owner is the file itself, and which writes its next version under that
// owner's lock (storeArtifact, requireOpenOwner). A document's upload has the document service
// take the same lock before the room, but a file is written by the route alone, so the route's own
// lock is the one it waits on.
func readyProjectFileUpload(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{files: filestest.NewMemory()}, "Project file upload", "A spec.\n")
	first := multipartRequest(t, fixture.handler, "/api/v1/projects/LOCK/artifacts", map[string]string{"name": "shot.png"},
		"shot.png", "image/png", pngBytes("first"), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload the project's file: status=%d body=%s", first.Code, first.Body.String())
	}
	file := decodeBody[uploadedArtifact](t, first).Artifact
	write := fixture.routeWrite(t, "the file's next version",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return multipartRequest(t, handler, "/api/v1/projects/LOCK/artifacts", map[string]string{"name": "shot.png"},
				"shot.png", "image/png", pngBytes("second"), "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("upload the file again once the lock was released: status=%d body=%s",
					answer.Code, answer.Body.String())
			}
			if uploaded := decodeBody[uploadedArtifact](t, answer); uploaded.Artifact.ID != file.ID || uploaded.Version.Number != 2 {
				t.Fatalf("the upload wrote version %d of %s, want version 2 of %s",
					uploaded.Version.Number, uploaded.Artifact.ID, file.ID)
			}
		})
	write.owner = store.ArtifactOwner(file.ID)
	return write
}

// readyAskFollow drives PUT /api/v1/asks/{id}/followers/{session_id}, a person adding a follower
// to an ask, which inserts the follower row under the ask owner's lock it takes before the insert
// (changeAskFollower, events.Broker.LockOwners).
func readyAskFollow(t *testing.T) lockedItemWrite {
	t.Helper()
	fixture := newItemRowFixture(t, testServerOptions{}, "Ask follow", "A spec.\n")
	created := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks",
		map[string]any{"question": "Who follows this?"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	return fixture.routeWrite(t, "the ask's follower row",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPut, "/api/v1/asks/"+ask.ID+"/followers/s3", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusNoContent {
				t.Fatalf("follow the ask once the lock was released: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if followers := fixture.rowCount(t, `select count(*) from ask_followers where ask_id = $1 and session_id = 's3'`, ask.ID); followers != 1 {
				t.Fatalf("the ask has %d followers named s3, want the one the route added", followers)
			}
		})
}

// rowCount answers a count(*) query of one argument, read from the pool.
func (f itemRowFixture) rowCount(t *testing.T, query string, arg any) int {
	t.Helper()
	var count int
	if err := f.database.Pool.QueryRow(context.Background(), query, arg).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	return count
}

// A delivery's send is settled in a transaction of its own once the listener answers, and that
// transaction takes the receipt owner's lock before it updates the attempt
// (settleDeliveryAttempt, events.Broker.LockOwners), so it waits for a writer of that owner holding
// no transaction id. The test holds the message's send at the listener, takes the issue's owner
// lock while it is held, then lets the send answer: the settle must queue behind the lock having
// updated nothing, and record the attempt sent once the lock is given back.
func TestADeliverySettleWaitsForItsReceiptOwnersLockHoldingNoTransactionID(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	sessions := `[{"session_id":"s1","title":"planner","capabilities":["btw"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			entered <- struct{}{}
			<-release
			_, _ = w.Write([]byte(`{"event_id":"settle-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	var released bool
	defer func() {
		if !released {
			close(release)
		}
	}()
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, settle: time.Hour})
	issue := createInteractionIssue(t, handler, "LOCK", "Delivery settle", "A spec.\n")
	ctx := context.Background()

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
			map[string]any{"body": "Did this ship?", "target": "session:s1", "delivery": "btw"}, "alice")
	}()
	select {
	case <-entered:
	case response := <-answered:
		t.Fatalf("the message answered %d %s before its send reached the listener", response.Code, response.Body.String())
	case <-time.After(30 * time.Second):
		t.Fatal("the message's send never reached the listener")
	}
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := store.LockOwner(ctx, holder, store.IssueOwner(issue.Key)); err != nil {
		t.Fatalf("hold the issue's owner lock: %v", err)
	}
	released = true
	close(release)
	if early, answeredEarly, holdingXid := storetest.AwaitOwnerLockWaiter(t, database, "the send's settle", answered); answeredEarly {
		t.Fatalf("the message answered %d %s while this test held the issue's owner lock: its settle took no lock",
			early.Code, early.Body.String())
	} else if holdingXid != 0 {
		t.Fatal("the send's settle waits for the issue's owner lock with a transaction id already open: it updated " +
			"the attempt before it took that lock, under a writer holding the issue's owner")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the owner lock: %v", err)
	}
	response := awaitResponse(t, answered)
	if response.Code != http.StatusCreated {
		t.Fatalf("the message once the lock was released: status=%d body=%s", response.Code, response.Body.String())
	}
	if message := decodeBody[model.Message](t, response); len(message.Deliveries) != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the message's deliveries = %#v, want its one attempt settled sent", message.Deliveries)
	}
}

// reviewCount is how many reviews the document holds, read from the pool.
func (f itemRowFixture) reviewCount(t *testing.T, artifactID string) int {
	t.Helper()
	var count int
	if err := f.database.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_reviews where artifact_id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count the document's reviews: %v", err)
	}
	return count
}
