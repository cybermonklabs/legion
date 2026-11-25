package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheWritersOwnAnswerOrReplyDismissesTheThreadForThem is c-answer-dismisses-store
// (the own-answer rule, R1). A person's own answer or reply takes its thread out of their Inbox
// in the write's own transaction, as a Dismiss press does: one dismissed tuple from the person to
// the thread's root, at the write's moment and under its event, and the projection's mark - the
// person's user_threads row, or their user_agent_thread_dismissed row at an Agent thread's root.
// Nothing else dismisses. The next write by somebody else brings the thread back, and touches
// that thread alone. The census reads zero differences after it all.
//
// The test writes through the handlers and reads the store. Its first subtest is inventory I1:
// every write into a thread the module's source makes, each with the route that reaches it and
// whether it dismisses.
func TestTheWritersOwnAnswerOrReplyDismissesTheThreadForThem(t *testing.T) {
	f := newOwnDismissFixture(t)

	t.Run("I1: every write into a thread is listed with the route that reaches it, and only a person's own answer or reply dismisses", func(t *testing.T) {
		rows := ownDismissInventory(f)
		requireEveryWriteIntoAThreadIsListed(t, rows)
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				if row.drive == nil {
					t.Logf("%s (%s) is reached by no route of this server: %s", row.site, row.route, row.undriven)
					return
				}
				written := row.drive(t)
				if row.dismisses {
					requireOwnDismissal(t, f.database, written)
				} else {
					requireNoOwnDismissal(t, f.database, written)
				}
			})
		}
	})

	t.Run("own reply after another raised the version", func(t *testing.T) {
		// alice wrote the thread and has read nothing since; bob's reply raised her row's version
		// past the one she last read. Her own reply dismisses the thread all the same.
		root := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Which day do we cut the branch?"}, "alice")
		createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Thursday.", "reply_to": root.ID}, "bob")
		before := threadRowWrites(t, f.database, "alice", root.ID)
		if before.version != 1 {
			t.Fatalf("alice's row is at version %d after bob's reply, want 1", before.version)
		}
		reply := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Thursday it is.", "reply_to": root.ID}, "alice")
		requireOwnDismissal(t, f.database, ownWrite{
			person: "alice", rootKind: graphstore.KindComment, rootID: root.ID,
			event: writeEventOf(t, f.database, "comment.created", reply.ID), at: reply.CreatedAt,
		})
		if after := threadRowWrites(t, f.database, "alice", root.ID); after.version != before.version {
			t.Errorf("alice's own reply moved her row's version from %d to %d, want it left", before.version, after.version)
		}
	})

	t.Run("own reply under an Agent thread reads its replies read, as a Dismiss press does", func(t *testing.T) {
		requireTheOwnAgentReplyReadsTheReplies(t, f)
	})

	t.Run("the next write by somebody else brings each thread back, and touches that thread alone", func(t *testing.T) {
		requireTheReturnTouchesOneThread(t, f)
	})

	t.Run("the census reads zero differences", func(t *testing.T) {
		requireStoreCensusZero(t, f.database, "after the own dismissals and their return")
	})
}

// ownDismissFixture is the server the test writes through: s1 is a live session, so a direct
// message reaches it and it can answer, and the store records each statement with its arguments.
type ownDismissFixture struct {
	handler  http.Handler
	database *store.Store
	recorder *storetest.BatchStatementRecorder
	issue    string
	document string
	token    string
}

func newOwnDismissFixture(t *testing.T) *ownDismissFixture {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	recorder := &storetest.BatchStatementRecorder{}
	handler, database, _ := newTestServer(t, testServerOptions{
		envoyURL: listener.URL, store: storetest.TracedStore(t, storetest.Open(t), recorder),
	})
	issue := createInteractionIssue(t, handler, "OWND", "Own answers and replies", "A spec")
	document := createProjectDocument(t, handler, "OWND", "Runbook", "# Runbook\n\nStep one.\n")
	return &ownDismissFixture{
		handler: handler, database: database, recorder: recorder, issue: issue.Key, document: document.ID,
		token: mintPersonalToken(t, handler, "alice", actingTokenName, nil).Token,
	}
}

// ownWrite is one write a row of I1 made, as the store is read for it: the person whose Inbox the
// row is about, the root of the thread the write is in, the event that recorded the write, its
// moment, and whether the thread is an Agent thread, whose mark is a row of its own.
type ownWrite struct {
	person           string
	rootKind, rootID string
	event            int64
	at               time.Time
	agent            bool
}

// ownDismissRow is one row of I1: a call of an inboxwrite constructor, or an inboxwrite.Write
// literal, in the module's non-test source (site, "<file>#<function> <constructor>"), a route
// that reaches it, and whether that route's write dismisses. drive makes the write through this
// server; a site no route of this server reaches says why instead (undriven).
type ownDismissRow struct {
	name      string
	site      string
	route     string
	dismisses bool
	drive     func(t *testing.T) ownWrite
	undriven  string
}

// The sites of I1, named as requireEveryWriteIntoAThreadIsListed reads them off the source.
const (
	answeredSite        = "api/ask_transitions.go#closeAskTx Answered"
	declinedSite        = "api/ask_transitions.go#closeAskTx Declined"
	askCreateSite       = "api/asks.go#createAskFor Opened"
	commentSite         = "api/comment_create.go#createCommentFor Comment"
	sessionCommentSite  = "api/comment_delivery.go#planCommentReply Comment"
	messageSite         = "api/messages.go#inbox Message"
	ownMessageSite      = "api/messages.go#ownInbox Message"
	approvalRequestSite = "api/reviews.go#openApprovalAsk Opened"
	carriedSite         = "docs/approval_carry.go#answerApprovalAsk Answered"
	askBlockSite        = "docs/ask_blocks.go#createAskBlock Opened"
	restoredSite        = "docs/ask_blocks.go#restoreRetractedAsk Answered"
)

// ownDismissInventory is I1. Dismisses: an answer, an approval review (which answers the open
// approval request through closeAskTx), a comment reply on an issue and on a project document, an
// issue message reply, and a person's reply to a session's direct message. Dismisses nothing: a
// write that starts a thread, a decline, a session's write, another person's write (for this
// person), an answer or a reply an agent writes in the person's name (an Act), the carried
// approval answer, the restore of a retracted ask, and the backfill's and the seed's writes.
func ownDismissInventory(f *ownDismissFixture) []ownDismissRow {
	issueComments := "/api/v1/issues/" + f.issue + "/comments"
	documentComments := "/api/v1/artifacts/" + f.document + "/comments"
	issueMessages := "/api/v1/issues/" + f.issue + "/messages"
	backfill := "the backfill records an older build's write through inboxwrite.RecordOlder, which plans no dismissal"
	seed := "the seed records its writes itself, none of them a person's write at that moment"
	return []ownDismissRow{
		{name: "answer", site: answeredSite, route: "POST /api/v1/asks/{id}/answer", dismisses: true,
			drive: func(t *testing.T) ownWrite {
				asked := createPersonAsk(t, f.handler, f.issue, "bob", "Ship on Friday?")
				answered := answerAskForChange(t, f.handler, asked.ID, "alice", "Ship", nil)
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: asked.ID,
					event: writeEventOf(t, f.database, "ask.answered", asked.ID), at: answered.Answer.At}
			}},
		{name: "approval review", site: answeredSite, route: "POST /api/v1/artifacts/{id}/reviews", dismisses: true,
			drive: func(t *testing.T) ownWrite {
				issue := createInteractionIssue(t, f.handler, "OWNR", "A reviewed spec", "A spec")
				request := requestApproval(t, f.handler, issue.PrimaryArtifactID)
				reviewDocument(t, f.handler, issue.PrimaryArtifactID, "alice", "approved", "")
				answered := readBlockAsk(t, f.handler, request.ID)
				if answered.State != "answered" || answered.Answer == nil {
					t.Fatalf("the approval request after alice's review = state %q answer %#v, want answered", answered.State, answered.Answer)
				}
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: request.ID,
					event: writeEventOf(t, f.database, "ask.answered", request.ID), at: answered.Answer.At}
			}},
		{name: "answer through a personal token", site: answeredSite, route: "POST /api/v1/asks/{id}/answer, an agent acting for the person",
			drive: func(t *testing.T) ownWrite {
				asked := createPersonAsk(t, f.handler, f.issue, "bob", "Ship the hotfix?")
				if answered := answerAsToken(t, f.handler, asked.ID, f.token, actingSession, map[string]any{"selected": []string{"Ship"}}); answered.Code != http.StatusOK {
					t.Fatalf("answer with alice's token: status=%d body=%s", answered.Code, answered.Body.String())
				}
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: asked.ID,
					event: writeEventOf(t, f.database, "ask.answered", asked.ID)}
			}},
		{name: "carried approval", site: carriedSite, route: "POST /api/v1/artifacts/{id}/edits, a version that restores the approved text",
			drive: func(t *testing.T) ownWrite {
				issue := createInteractionIssue(t, f.handler, "OWNC", "A carried approval", "A spec")
				reviewDocument(t, f.handler, issue.PrimaryArtifactID, "alice", "approved", "")
				reviseDocument(t, f.handler, issue.PrimaryArtifactID, "A spec", "A revised spec", 2)
				request := requestApproval(t, f.handler, issue.PrimaryArtifactID)
				reviseDocument(t, f.handler, issue.PrimaryArtifactID, "A revised spec", "A spec", 3)
				carried := readBlockAsk(t, f.handler, request.ID)
				if carried.State != "answered" || carried.Answer == nil || carried.Answer.User != "alice" {
					t.Fatalf("the request after the carry = state %q answer %#v, want alice's carried approval", carried.State, carried.Answer)
				}
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: request.ID,
					event: writeEventOf(t, f.database, "ask.answered", request.ID)}
			}},
		{name: "restored ask", site: restoredSite, route: "a settlement's restore of a retracted ask",
			undriven: "it writes an answer only where a retraction of the release before kept none, which no write of this server leaves"},
		{name: "decline", site: declinedSite, route: "POST /api/v1/asks/{id}/decline",
			drive: func(t *testing.T) ownWrite {
				asked := createPersonAsk(t, f.handler, f.issue, "bob", "Hold the release?")
				if declined := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/asks/"+asked.ID+"/decline",
					map[string]any{"note": "Not mine to answer."}, "alice"); declined.Code != http.StatusOK {
					t.Fatalf("decline the ask: status=%d body=%s", declined.Code, declined.Body.String())
				}
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: asked.ID,
					event: writeEventOf(t, f.database, "ask.resolved", asked.ID)}
			}},
		{name: "ask create", site: askCreateSite, route: "POST /api/v1/issues/{key}/asks",
			drive: func(t *testing.T) ownWrite {
				asked := createPersonAsk(t, f.handler, f.issue, "alice", "Who reviews the rollout?")
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: asked.ID,
					event: writeEventOf(t, f.database, "ask.opened", asked.ID)}
			}},
		{name: "approval request", site: approvalRequestSite, route: "POST /api/v1/artifacts/{id}/approval-requests",
			drive: func(t *testing.T) ownWrite {
				issue := createInteractionIssue(t, f.handler, "OWNQ", "A requested spec", "A spec")
				request := requestApproval(t, f.handler, issue.PrimaryArtifactID)
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: request.ID,
					event: writeEventOf(t, f.database, "ask.opened", request.ID)}
			}},
		{name: "ask block", site: askBlockSite, route: "POST /api/v1/artifacts/{id}/edits, a version holding a new ask block",
			drive: func(t *testing.T) ownWrite {
				issue := createInteractionIssue(t, f.handler, "OWNB", "A decision block", "Context\n")
				asks := insertDecisionBlocks(t, f.handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2,
					map[string]string{"wave": "How many regions take the first wave?"})
				return ownWrite{person: "alice", rootKind: graphstore.KindAsk, rootID: asks["wave"],
					event: writeEventOf(t, f.database, "ask.opened", asks["wave"])}
			}},
		{name: "comment reply", site: commentSite, route: "POST /api/v1/issues/{key}/comments with reply_to", dismisses: true,
			drive: func(t *testing.T) ownWrite {
				root := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Ready to merge?"}, "bob")
				reply := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Merging now.", "reply_to": root.ID}, "alice")
				return ownWrite{person: "alice", rootKind: graphstore.KindComment, rootID: root.ID,
					event: writeEventOf(t, f.database, "comment.created", reply.ID), at: reply.CreatedAt}
			}},
		{name: "document comment reply", site: commentSite, route: "POST /api/v1/artifacts/{id}/comments with reply_to", dismisses: true,
			drive: func(t *testing.T) ownWrite {
				root := ownDismissComment(t, f.handler, documentComments, map[string]any{"body": "Does step one still hold?"}, "bob")
				reply := ownDismissComment(t, f.handler, documentComments, map[string]any{"body": "It does.", "reply_to": root.ID}, "alice")
				return ownWrite{person: "alice", rootKind: graphstore.KindComment, rootID: root.ID,
					event: writeEventOf(t, f.database, "comment.created", reply.ID), at: reply.CreatedAt}
			}},
		{name: "root comment", site: commentSite, route: "POST /api/v1/issues/{key}/comments",
			drive: func(t *testing.T) ownWrite {
				root := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Who owns the rollback?"}, "alice")
				return ownWrite{person: "alice", rootKind: graphstore.KindComment, rootID: root.ID,
					event: writeEventOf(t, f.database, "comment.created", root.ID)}
			}},
		{name: "another person's reply", site: commentSite, route: "POST /api/v1/issues/{key}/comments with reply_to, by somebody else",
			drive: func(t *testing.T) ownWrite {
				root := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Is the banner final?"}, "alice")
				reply := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "It is.", "reply_to": root.ID}, "bob")
				return ownWrite{person: "alice", rootKind: graphstore.KindComment, rootID: root.ID,
					event: writeEventOf(t, f.database, "comment.created", reply.ID)}
			}},
		{name: "comment through an agent's bearer", site: commentSite, route: "POST /api/v1/issues/{key}/comments with reply_to, a session's write",
			drive: func(t *testing.T) ownWrite {
				root := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Can a session check the build?"}, "alice")
				reply := sessionComment(t, f.handler, issueComments, map[string]any{"body": "Checked.", "reply_to": root.ID})
				return ownWrite{person: "alice", rootKind: graphstore.KindComment, rootID: root.ID,
					event: writeEventOf(t, f.database, "comment.created", reply.ID)}
			}},
		{name: "session's comment reply", site: sessionCommentSite, route: "POST /api/v1/comments/{id}/reply",
			drive: func(t *testing.T) ownWrite {
				mentioned := decodeMentionedComment(t, postMentionedComment(t, f.handler, f.issue, map[string]any{
					"body": "Can this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
				}))
				answered := bearerRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it can ship.",
				})
				if answered.Code != http.StatusCreated {
					t.Fatalf("the session answers the mention: status=%d body=%s", answered.Code, answered.Body.String())
				}
				reply := decodeBody[model.Comment](t, answered)
				return ownWrite{person: "alice", rootKind: graphstore.KindComment, rootID: mentioned.ID,
					event: writeEventOf(t, f.database, "comment.answered", reply.ID)}
			}},
		{name: "issue message reply", site: ownMessageSite, route: "POST /api/v1/issues/{key}/messages with in_reply_to", dismisses: true,
			drive: func(t *testing.T) ownWrite {
				root := ownDismissMessage(t, f.handler, issueMessages, map[string]any{"body": "Is the deploy window open?"}, "bob")
				reply := ownDismissMessage(t, f.handler, issueMessages, map[string]any{"body": "Until six.", "in_reply_to": root.ID}, "alice")
				return ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
					event: writeEventOf(t, f.database, "message.answered", reply.ID), at: reply.CreatedAt}
			}},
		{name: "root issue message", site: ownMessageSite, route: "POST /api/v1/issues/{key}/messages",
			drive: func(t *testing.T) ownWrite {
				root := ownDismissMessage(t, f.handler, issueMessages, map[string]any{"body": "Who takes the pager tonight?"}, "alice")
				return ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
					event: writeEventOf(t, f.database, "message.created", root.ID)}
			}},
		{name: "reply to a session's direct message", site: ownMessageSite, route: "POST /api/v1/agents/{session_id}/messages with in_reply_to",
			dismisses: true, drive: func(t *testing.T) ownWrite {
				root, answer := agentThread(t, f.handler, "btw", "Can you look at the build?", "Looking now.")
				reply := ownDismissAgentReply(t, f.handler, answer.ID, "Thanks, tell me when it is green.", "")
				return ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
					event: writeEventOf(t, f.database, "message.answered", reply.ID), at: reply.CreatedAt, agent: true}
			}},
		{name: "root direct message", site: ownMessageSite, route: "POST /api/v1/agents/{session_id}/messages",
			drive: func(t *testing.T) ownWrite {
				created := postAgentMessage(t, f.handler, "s1", map[string]any{"body": "Start the canary.", "delivery": "btw"}, "alice")
				if created.Code != http.StatusCreated {
					t.Fatalf("message s1: status=%d body=%s", created.Code, created.Body.String())
				}
				root := decodeBody[model.Message](t, created)
				return ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
					event: writeEventOf(t, f.database, "message.created", root.ID), agent: true}
			}},
		{name: "reply through a personal token", site: ownMessageSite, route: "POST /api/v1/agents/{session_id}/messages with in_reply_to, an agent acting for the person",
			drive: func(t *testing.T) ownWrite {
				root, answer := agentThread(t, f.handler, "btw", "Is the cache warm?", "Warming it.")
				reply := ownDismissAgentReply(t, f.handler, answer.ID, "Tell me when it is.", f.token)
				return ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
					event: writeEventOf(t, f.database, "message.answered", reply.ID), agent: true}
			}},
		{name: "session's message reply", site: messageSite, route: "POST /api/v1/messages/{id}/reply",
			drive: func(t *testing.T) ownWrite {
				root, answer := agentThread(t, f.handler, "btw", "Did the migration run?", "It ran.")
				return ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
					event: writeEventOf(t, f.database, "message.answered", answer.ID), agent: true}
			}},
		{name: "issue write's system comment", site: "api/comment_create.go#queueIssueComment Comment",
			route:    "an issue write that leaves a comment of Dispatch's system actor (status change, parent status fix)",
			undriven: "the author is the system actor parentStatusRuleActor, never a person, so the write is no person's own and calls no Own"},
		{name: "backfill of an older message", site: "graphcensus/backfill_inbox.go#writeMessages Message", route: "dispatch backfill", undriven: backfill},
		{name: "backfill of an older comment", site: "graphcensus/backfill_inbox.go#writeCommentPointers Comment", route: "dispatch backfill", undriven: backfill},
		{name: "backfill of an older ask", site: "graphcensus/backfill_inbox_threads.go#writeAsksOwed Opened", route: "dispatch backfill", undriven: backfill},
		{name: "backfill of an older answer", site: "graphcensus/backfill_inbox_threads.go#writeAsksOwed AnsweredBefore", route: "dispatch backfill", undriven: backfill},
		{name: "backfill of an older decline", site: "graphcensus/backfill_inbox_threads.go#writeDeclines Declined", route: "dispatch backfill", undriven: backfill},
		{name: "seed's ask", site: "cmd/dispatch-seed/write_items.go#writeAsks Opened", route: "dispatch-seed", undriven: seed},
		{name: "seed's answer", site: "cmd/dispatch-seed/write_items.go#writeAsks Write", route: "dispatch-seed", undriven: seed},
		{name: "seed's comment", site: "cmd/dispatch-seed/write_items.go#writeComments Comment", route: "dispatch-seed", undriven: seed},
		{name: "seed's message", site: "cmd/dispatch-seed/write_items.go#writeMessages Message", route: "dispatch-seed", undriven: seed},
	}
}

// ownDismissComment posts one comment on path as login and answers it.
func ownDismissComment(t *testing.T, handler http.Handler, path string, body map[string]any, login string) model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, path, body, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("comment on %s as %s: status=%d body=%s", path, login, response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

// ownDismissMessage posts one issue message on path as login and answers it.
func ownDismissMessage(t *testing.T, handler http.Handler, path string, body map[string]any, login string) model.Message {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, path, body, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("message on %s as %s: status=%d body=%s", path, login, response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

// ownDismissAgentReply is alice's reply to s1 under the message inReplyTo, sent as the dashboard
// sends it - under a key of its own - and, where token is given, through alice's personal token, as
// an agent acting for her sends it.
func ownDismissAgentReply(t *testing.T, handler http.Handler, inReplyTo, body, token string) model.Message {
	t.Helper()
	input := map[string]any{"body": body, "delivery": "btw", "in_reply_to": inReplyTo}
	var response *httptest.ResponseRecorder
	if token == "" {
		response = postAgentMessage(t, handler, "s1", input, "alice")
	} else {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("encode the reply: %v", err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agents/s1/messages", bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set(contracts.IdempotencyKeyHeader, uuid.NewString())
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("reply to s1 under %s: status=%d body=%s", inReplyTo, response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

// requireOwnDismissal fails unless written took its thread out of its person's Inbox: one dismissed
// tuple from them to the thread's root, under the write's event, at the write's moment, and the
// projection's mark at that moment - their user_threads row of a Reply thread, or their
// user_agent_thread_dismissed row at an Agent thread's root.
func requireOwnDismissal(t *testing.T, database *store.Store, written ownWrite) {
	t.Helper()
	var count int
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), max(at) from graph_tuples
		where kind = $1 and from_kind = $2 and from_id = $3 and to_kind = $4 and to_id = $5 and asserted and source_event = $6
	`, graphstore.TupleDismissed, graphstore.KindPerson, written.person, written.rootKind, written.rootID, written.event).Scan(&count, &at); err != nil {
		t.Fatalf("read %s's dismissed tuples to %s/%s: %v", written.person, written.rootKind, written.rootID, err)
	}
	if count != 1 || at == nil || !at.Equal(written.at) {
		t.Errorf("%s holds %d dismissed tuple(s) to %s/%s under event %d, at %v; want one at the write's moment %s",
			written.person, count, written.rootKind, written.rootID, written.event, at, written.at.Format(time.RFC3339Nano))
	}
	if mark := projectedDismissal(t, database, written); mark == nil || !mark.Equal(written.at) {
		t.Errorf("%s's %s mark on %s is %v, want the write's moment %s",
			written.person, projectionOf(written), written.rootID, mark, written.at.Format(time.RFC3339Nano))
	}
}

// requireNoOwnDismissal fails unless written dismissed nothing for its person: no dismissed tuple
// from them under the write's event, no dismissal of theirs standing on the thread's root, and no
// mark of the projection.
func requireNoOwnDismissal(t *testing.T, database *store.Store, written ownWrite) {
	t.Helper()
	var underEvent, standing int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) filter (where source_event = $5),
		       (select count(*) from (
		            select distinct on (to_kind, to_id) asserted from graph_tuples
		            where kind = $1 and from_kind = $2 and from_id = $3 and to_id = $4
		            order by to_kind, to_id, seq desc) newest
		        where newest.asserted)
		from graph_tuples
		where kind = $1 and from_kind = $2 and from_id = $3 and asserted
	`, graphstore.TupleDismissed, graphstore.KindPerson, written.person, written.rootID, written.event).Scan(&underEvent, &standing); err != nil {
		t.Fatalf("read %s's dismissed tuples: %v", written.person, err)
	}
	if underEvent != 0 || standing != 0 {
		t.Errorf("the write under event %d appended %d dismissed tuple(s) from %s, and %d dismissal of %s/%s stands; want none",
			written.event, underEvent, written.person, standing, written.rootKind, written.rootID)
	}
	if mark := projectedDismissal(t, database, written); mark != nil {
		t.Errorf("%s's %s mark on %s is %s, want none", written.person, projectionOf(written), written.rootID, mark.Format(time.RFC3339Nano))
	}
}

// projectionOf names the table that projects written's thread's dismissal.
func projectionOf(written ownWrite) string {
	if written.agent {
		return "user_agent_thread_dismissed"
	}
	return "user_threads"
}

// projectedDismissal is the person's dismissal of written's thread as its projection holds it, nil
// where it holds none, or no row at all.
func projectedDismissal(t *testing.T, database *store.Store, written ownWrite) *time.Time {
	t.Helper()
	if written.agent {
		return agentThreadDismissal(t, database, written.person, written.rootID)
	}
	var at *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select max(dismissed_at) from user_threads where login = $1 and thread_id = $2::uuid`,
		written.person, written.rootID).Scan(&at); err != nil {
		t.Fatalf("read %s's %s mark on %s: %v", written.person, projectionOf(written), written.rootID, err)
	}
	return at
}

// requireTheOwnAgentReplyReadsTheReplies proves that alice's own reply under an Agent thread leaves
// the thread as a Dismiss press of it leaves it, holding nothing she has not read. s1 answers her
// one-off message twice; her reply under the first answer marks both read by id - a
// user_agent_reply_read row each, marked at the reply's moment, and a reply_read tuple each from
// her to that answer under the reply's event - and the Agents page reads them read: the
// conversation window flags the thread read, and s1's unread_replies count falls by the two.
func requireTheOwnAgentReplyReadsTheReplies(t *testing.T, f *ownDismissFixture) {
	t.Helper()
	root, first := agentThread(t, f.handler, "btw", "Can you rotate the keys?", "Rotating them now.")
	followed := replyTo(t, f.handler, root.ID, "Rotated.")
	if followed.Code != http.StatusCreated {
		t.Fatalf("s1 follows up under %s: status=%d body=%s", root.ID, followed.Code, followed.Body.String())
	}
	second := decodeBody[model.Message](t, followed)
	unread := agentUnreadReplies(t, f.handler, "s1")
	if !agentConversationUnread(t, f.handler, "s1", root.ID) {
		t.Fatalf("the Agents page reads alice's conversation %s read before her reply, want its two answers unread", root.ID)
	}

	reply := ownDismissAgentReply(t, f.handler, first.ID, "Thanks, both keys look right.", "")
	event := writeEventOf(t, f.database, "message.answered", reply.ID)
	requireOwnDismissal(t, f.database, ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: root.ID,
		event: event, at: reply.CreatedAt, agent: true})
	for _, answer := range []model.Message{first, second} {
		var marked []time.Time
		rows, err := f.database.Pool.Query(context.Background(), `
			select tuples_stored_at from user_agent_reply_read
			where login = 'alice' and reply_id = $1::uuid and tuples_stored_at is not null
		`, answer.ID)
		if err == nil {
			marked, err = pgx.CollectRows(rows, pgx.RowTo[time.Time])
		}
		if err != nil {
			t.Fatalf("read alice's read of %s by its id: %v", answer.ID, err)
		}
		if len(marked) != 1 || !marked[0].Equal(reply.CreatedAt) {
			t.Errorf("alice's user_agent_reply_read rows of s1's answer %q are marked at %v, want one at her reply's moment %s",
				answer.Body, marked, reply.CreatedAt.Format(time.RFC3339Nano))
		}
		node := graphstore.KindMessage + ":" + answer.ID
		requireRelationTuples(t, f.database, graphstore.TupleReplyRead, node,
			fmt.Sprintf("%s:alice > %s @%d", graphstore.KindPerson, node, event))
	}
	if agentConversationUnread(t, f.handler, "s1", root.ID) {
		t.Errorf("the Agents page flags alice's conversation %s unread after her own reply dismissed it", root.ID)
	}
	if after := agentUnreadReplies(t, f.handler, "s1"); after != unread-2 {
		t.Errorf("s1's unread_replies for alice went from %d to %d with her own reply, want %d: both answers read", unread, after, unread-2)
	}
}

// agentUnreadReplies is the Agents page's count of the replies of session alice has not read
// (GET /api/v1/me/agents/state's unread_replies).
func agentUnreadReplies(t *testing.T, handler http.Handler, session string) int {
	t.Helper()
	states := decodeBody[map[string]userAgentState](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "alice"))
	return states[session].UnreadReplies
}

// agentConversationUnread is the unread flag the Agents page's conversation window of session
// carries for alice's conversation rooted at root, which it has to hold.
func agentConversationUnread(t *testing.T, handler http.Handler, session, root string) bool {
	t.Helper()
	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/"+session+"/messages", nil, "alice"))
	for _, read := range window {
		if read.Message.ID == root {
			return read.Unread
		}
	}
	t.Fatalf("the conversation window of %s holds no conversation %s", session, root)
	return false
}

// inboxwriteImportPath is the path a file names the one writer of a thread by.
const inboxwriteImportPath = "github.com/sjawhar/envoy/internal/dispatch/inboxwrite"

// threadWriteConstructors are inboxwrite's constructors of a write into a thread, the backfill's
// AnsweredBefore among them.
var threadWriteConstructors = []string{"Comment", "Message", "Opened", "Answered", "AnsweredBefore", "Declined"}

// threadWriteSite is what the walk reads of one site: the lines it writes into a thread on, and
// how many of the writes it builds there it marks its writer's own and how many it does not.
type threadWriteSite struct {
	lines      []string
	own, plain int
}

// requireEveryWriteIntoAThreadIsListed reads the module's source, every Go file but the tests and
// inboxwrite's own, with go/parser, and finds each call of an inboxwrite constructor and each
// inboxwrite.Write literal, named "<file>#<function> <constructor>" ("Write" for a literal), the
// file from internal/dispatch where it lives under it. It fails on a site no row of rows names and
// on a row whose site the source no longer holds, so a new write into a thread cannot appear
// without a row that says whether it dismisses.
//
// It also reads whether each site marks the write it builds its writer's own - Own called on the
// expression that builds it, or OwnWrite set in the literal - which is the one way a handler lets
// inboxwrite.PlanRecord dismiss. It fails where that disagrees with the rows of the site: a site
// some row of which dismisses has to mark its writes its own, and a site none of whose rows
// dismisses - the carried approval, the restore of a retracted ask, the backfill's and the seed's
// writes among them - must not. A row of a site that marks its writes its own and dismisses
// nothing is decided by PlanRecord's other conditions (a session, an Act, a write that starts its
// thread), which its drive reads. A site that marks some of its writes its own and not others is
// refused, since no row could say which of them it is about.
func requireEveryWriteIntoAThreadIsListed(t *testing.T, rows []ownDismissRow) {
	t.Helper()
	const moduleRoot = "../../.."
	found := map[string]threadWriteSite{}
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != moduleRoot && (name == "testdata" || name == "vendor" || name == "node_modules" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if path != moduleRoot {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if strings.HasPrefix(relative, "internal/dispatch/inboxwrite/") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(source, []byte(inboxwriteImportPath)) {
			return nil
		}
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", relative, err)
		}
		for site, written := range threadWritesIn(strings.TrimPrefix(relative, "internal/dispatch/"), files, file) {
			held := found[site]
			held.lines = append(held.lines, written.lines...)
			held.own += written.own
			held.plain += written.plain
			found[site] = held
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's source under %s: %v", moduleRoot, err)
	}
	if len(found) == 0 {
		t.Fatal("the walk found no write into a thread, so nothing was checked")
	}
	listed := map[string][]string{}
	dismissing := map[string]bool{}
	for _, row := range rows {
		listed[row.site] = append(listed[row.site], row.name)
		dismissing[row.site] = dismissing[row.site] || row.dismisses
	}
	for _, site := range slices.Sorted(maps.Keys(found)) {
		written := found[site]
		own := written.own > 0
		t.Logf("write into a thread %s (%s), marked its writer's own %v, listed as %s",
			site, strings.Join(written.lines, ", "), own, strings.Join(listed[site], "; "))
		if len(listed[site]) == 0 {
			t.Errorf("%s writes into a thread (%s) and no row of I1 names it: add a row with the route that reaches it and whether it dismisses",
				site, strings.Join(written.lines, ", "))
			continue
		}
		switch {
		case written.own > 0 && written.plain > 0:
			t.Errorf("%s (%s) marks %d of its writes its writer's own and %d not: split it, so each site's rows say whether its write dismisses",
				site, strings.Join(written.lines, ", "), written.own, written.plain)
		case dismissing[site] && !own:
			t.Errorf("a row of %s (%s) dismisses, and the site marks no write its writer's own (Own, OwnWrite), so inboxwrite.PlanRecord dismisses nothing there",
				site, strings.Join(listed[site], "; "))
		case own && !dismissing[site]:
			t.Errorf("%s marks its write its writer's own (Own, OwnWrite), and no row of it (%s) dismisses: a write that is no person's act at that moment leaves OwnWrite unset",
				site, strings.Join(listed[site], "; "))
		}
	}
	for _, site := range slices.Sorted(maps.Keys(listed)) {
		if _, ok := found[site]; !ok {
			t.Errorf("I1 lists %s (%s), and no file of the module writes into a thread there any more: take the row off",
				site, strings.Join(listed[site], "; "))
		}
	}
}

// threadWritesIn is every site one file writes into a thread at, each with the lines it does so
// on and whether each write it builds there is marked its writer's own, read through the name the
// file imports inboxwrite under.
func threadWritesIn(relative string, files *token.FileSet, file *ast.File) map[string]threadWriteSite {
	name := ""
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != inboxwriteImportPath {
			continue
		}
		name = "inboxwrite"
		if imported.Name != nil {
			name = imported.Name.Name
		}
	}
	found := map[string]threadWriteSite{}
	if name == "" {
		return found
	}
	named := func(expression ast.Expr) (string, bool) {
		selector, ok := expression.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		x, ok := selector.X.(*ast.Ident)
		return selector.Sel.Name, ok && x.Name == name
	}
	for _, declared := range file.Decls {
		function := "(package level)"
		if declaration, ok := declared.(*ast.FuncDecl); ok {
			function = declaration.Name.Name
		}
		// The nodes from the declaration down to the one Inspect is at, so a write's own
		// expression can be read upwards from it.
		var path []ast.Node
		ast.Inspect(declared, func(node ast.Node) bool {
			if node == nil {
				path = path[:len(path)-1]
				return true
			}
			path = append(path, node)
			constructor, own := "", false
			switch written := node.(type) {
			case *ast.CallExpr:
				if called, ok := named(written.Fun); ok && slices.Contains(threadWriteConstructors, called) {
					constructor = called
				}
			case *ast.CompositeLit:
				if literal, ok := named(written.Type); ok && literal == "Write" {
					constructor, own = "Write", setsOwnWrite(written)
				}
			}
			if constructor != "" {
				site := relative + "#" + function + " " + constructor
				held := found[site]
				held.lines = append(held.lines, relative+":"+strconv.Itoa(files.Position(node.Pos()).Line))
				if own || calledOwn(path) {
					held.own++
				} else {
					held.plain++
				}
				found[site] = held
			}
			return true
		})
	}
	return found
}

// calledOwn is whether the write built by the last node of path is marked its writer's own on its
// way out of the expression that builds it: a chain of method calls on it, such as
// `inboxwrite.Comment(...).ReachesMentioned(indexed).Own()`, one of which is Own.
func calledOwn(path []ast.Node) bool {
	current := path[len(path)-1]
	for at := len(path) - 2; at >= 1; at -= 2 {
		selector, ok := path[at].(*ast.SelectorExpr)
		if !ok || selector.X != current {
			return false
		}
		call, ok := path[at-1].(*ast.CallExpr)
		if !ok || call.Fun != selector {
			return false
		}
		if selector.Sel.Name == "Own" {
			return true
		}
		current = call
	}
	return false
}

// setsOwnWrite is whether an inboxwrite.Write literal sets OwnWrite to anything but false.
func setsOwnWrite(literal *ast.CompositeLit) bool {
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := field.Key.(*ast.Ident); ok && key.Name == "OwnWrite" {
			value, isIdent := field.Value.(*ast.Ident)
			return !isIdent || value.Name != "false"
		}
	}
	return false
}

// otherDismissedThreads is how many dismissed threads the return's bound is read over, as the
// one-Inbox spec's R4 bounds a thread's participants.
const otherDismissedThreads = 1000

// requireTheReturnTouchesOneThread proves the return and its bound. A comment thread alice and carol
// both dismissed - carol by her own reply, which alice's reply after it brought back, and then by
// Dismiss; alice by her own reply - and an Agent thread alice replied under, which she dismissed;
// alice also holds otherDismissedThreads other dismissed threads. A session replies in the comment
// thread: one retraction of the dismissed tuple for each participant but the session, each row's
// dismissed_at cleared and its version raised by one, and alice's other dismissals still standing.
// Every statement of that reply that writes a projection row or a dismissed tuple binds the
// thread's id. The session's reply under the Agent thread clears alice's mark there too.
func requireTheReturnTouchesOneThread(t *testing.T, f *ownDismissFixture) {
	t.Helper()
	root := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Do we keep the flag after launch?"}, "alice")
	carolReply := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "One release more.", "reply_to": root.ID}, "carol")
	aliceReply := createThreadComment(t, f.handler, f.issue, map[string]any{"body": "Agreed.", "reply_to": root.ID}, "alice")
	thread := graphstore.KindComment + ":" + root.ID
	carolEvent := writeEventOf(t, f.database, "comment.created", carolReply.ID)
	aliceEvent := writeEventOf(t, f.database, "comment.created", aliceReply.ID)
	if dismissed := dismissRows(t, f.handler, f.database, "carol", inboxDismissReplyPrefix+root.ID); dismissed != 1 {
		t.Fatalf("carol's Dismiss changed %d threads, want the one alice's reply brought back", dismissed)
	}
	carolDismissEvent := newestEventOf(t, f.database, "user_thread_state.updated", "carol")
	agentRoot, agentAnswer := agentThread(t, f.handler, "btw", "Is the canary healthy?", "Healthy so far.")
	agentReply := ownDismissAgentReply(t, f.handler, agentAnswer.ID, "Keep watching it.", "")
	requireOwnDismissal(t, f.database, ownWrite{person: "alice", rootKind: graphstore.KindMessage, rootID: agentRoot.ID,
		event: writeEventOf(t, f.database, "message.answered", agentReply.ID), at: agentReply.CreatedAt, agent: true})

	seedDismissedThreads(t, f, otherDismissedThreads)
	standing := standingDismissalsBesides(t, f.database, "alice", root.ID, agentRoot.ID)
	if standing < otherDismissedThreads {
		t.Fatalf("alice holds %d other dismissed threads before the return, want at least %d", standing, otherDismissedThreads)
	}
	versions := map[string]int64{}
	for _, login := range []string{"alice", "carol"} {
		if at := projectedDismissal(t, f.database, ownWrite{person: login, rootID: root.ID}); at == nil {
			t.Fatalf("%s's row of the thread carries no dismissal before the return", login)
		}
		versions[login] = threadRowWrites(t, f.database, login, root.ID).version
	}

	f.recorder.Reset()
	back := sessionComment(t, f.handler, "/api/v1/issues/"+f.issue+"/comments", map[string]any{"body": "The flag stays one release.", "reply_to": root.ID})
	statements := f.recorder.Statements()
	backEvent := writeEventOf(t, f.database, "comment.created", back.ID)

	requireRelationTuples(t, f.database, graphstore.TupleDismissed, thread,
		fmt.Sprintf("person:carol > %s @%d", thread, carolEvent),
		fmt.Sprintf("person:alice > %s @%d", thread, aliceEvent),
		fmt.Sprintf("person:carol > %s @%d retracted", thread, aliceEvent),
		fmt.Sprintf("person:carol > %s @%d", thread, carolDismissEvent),
		fmt.Sprintf("person:alice > %s @%d retracted", thread, backEvent),
		fmt.Sprintf("person:carol > %s @%d retracted", thread, backEvent))
	for _, login := range []string{"alice", "carol"} {
		if at := projectedDismissal(t, f.database, ownWrite{person: login, rootID: root.ID}); at != nil {
			t.Errorf("%s's row still carries the dismissal %s after the session's reply", login, at.Format(time.RFC3339Nano))
		}
		if version := threadRowWrites(t, f.database, login, root.ID).version; version != versions[login]+1 {
			t.Errorf("%s's row is at version %d after the session's reply, want %d", login, version, versions[login]+1)
		}
	}
	if after := standingDismissalsBesides(t, f.database, "alice", root.ID, agentRoot.ID); after != standing {
		t.Errorf("alice's other standing dismissals went from %d to %d with one reply in another thread, want them untouched", standing, after)
	}
	requireTheReturnBindsTheThread(t, statements, root.ID)

	// The session's reply under the Agent thread clears alice's own mark there.
	if replied := replyTo(t, f.handler, agentRoot.ID, "Still healthy."); replied.Code != http.StatusCreated {
		t.Fatalf("the session replies under the Agent thread: status=%d body=%s", replied.Code, replied.Body.String())
	}
	if mark := agentThreadDismissal(t, f.database, "alice", agentRoot.ID); mark != nil {
		t.Errorf("alice's Agent thread still carries the dismissal %s after the session's reply", mark.Format(time.RFC3339Nano))
	}
	if standingDismissalsBesides(t, f.database, "alice") != standing {
		t.Errorf("alice's standing dismissals are not the %d other threads after both returns", standing)
	}
}

// seedDismissedThreads gives alice n dismissed threads of her own: n comments she starts, each
// keeping her a row, dismissed in one press that names them all.
func seedDismissedThreads(t *testing.T, f *ownDismissFixture, n int) {
	t.Helper()
	rows := make([]string, 0, n)
	for index := range n {
		started := createThreadComment(t, f.handler, f.issue, map[string]any{"body": fmt.Sprintf("Dismissed note %d.", index)}, "alice")
		rows = append(rows, inboxDismissReplyPrefix+started.ID)
		if index%100 == 99 {
			f.recorder.Reset()
		}
	}
	if dismissed := dismissRows(t, f.handler, f.database, "alice", rows...); dismissed != n {
		t.Fatalf("dismissed %d of alice's %d threads, want all of them", dismissed, n)
	}
}

// standingDismissalsBesides counts the dismissals of login that stand now, each read as the
// newest dismissed tuple from them to one root, over every root but the ones left out.
func standingDismissalsBesides(t *testing.T, database *store.Store, login string, leftOut ...string) int {
	t.Helper()
	var standing int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from (
			select distinct on (to_kind, to_id) asserted from graph_tuples
			where kind = $1 and from_kind = $2 and from_id = $3 and not (to_id = any($4::text[]))
			order by to_kind, to_id, seq desc
		) newest
		where newest.asserted
	`, graphstore.TupleDismissed, graphstore.KindPerson, login, append([]string{}, leftOut...)).Scan(&standing); err != nil {
		t.Fatalf("count %s's standing dismissals: %v", login, err)
	}
	return standing
}

// projectionWriteStatement matches a statement that makes, moves or unmakes a row of a dismissal's
// projection, and tupleWriteStatement one that appends tuples.
var (
	projectionWriteStatement = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+(user_threads|user_agent_thread_dismissed)\b`)
	tupleWriteStatement      = regexp.MustCompile(`(?is)\binsert\s+into\s+graph_tuples\b`)
)

// requireTheReturnBindsTheThread holds the statements a reply ran to the thread it was written in:
// it fails unless some of them write a projection row or a dismissed tuple, and unless each of
// those binds the thread's id, so no statement of the reply reaches another thread's rows or marks.
func requireTheReturnBindsTheThread(t *testing.T, statements []storetest.RecordedStatement, thread string) {
	t.Helper()
	counted := 0
	for _, statement := range statements {
		dismissal := tupleWriteStatement.MatchString(statement.SQL) && bindsValue(statement.Args, graphstore.TupleDismissed)
		if !projectionWriteStatement.MatchString(statement.SQL) && !dismissal {
			continue
		}
		counted++
		if !bindsValue(statement.Args, thread) {
			t.Errorf("a statement of the reply writes a dismissal's projection or a dismissed tuple and binds no id of thread %s: %s %v",
				thread, strings.Join(strings.Fields(statement.SQL), " "), statement.Args)
		}
	}
	if counted == 0 {
		t.Errorf("the reply ran %d statements and none wrote a dismissal's projection or a dismissed tuple, so the return was not read", len(statements))
	}
	t.Logf("the reply ran %d statements, %d of them writing a dismissal's projection or a dismissed tuple, each binding thread %s", len(statements), counted, thread)
}

// bindsValue is whether args binds value, as an argument of its own or as one element of an array
// argument.
func bindsValue(args []any, value string) bool {
	for _, arg := range args {
		switch bound := arg.(type) {
		case string:
			if bound == value {
				return true
			}
		case *string:
			if bound != nil && *bound == value {
				return true
			}
		case []string:
			if slices.Contains(bound, value) {
				return true
			}
		}
	}
	return false
}
