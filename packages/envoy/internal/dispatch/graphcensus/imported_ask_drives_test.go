package graphcensus_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The block the ask drives write, named so a drive's check can find it.
const invalidBlock = "sized-invalid"

// askDrives are the drives of the sites of the register of the store's writers that write an ask's
// answer state, the decisions step's catch-up, and the settlement's writes beyond the ask it
// opens, each the real site reached as its own lock-step driver reaches it, over the rig the
// drives before them wrote (importedDrives). They run in this order: alice's answer of the ask a
// session opened; the close of an issue of its own holding an open ask, and its reopen; the
// settlement of a copy of the notes' ask block on a document of its own; the settlement of an ask
// block whose question a person's edit emptied; the settlement that carries a person's approval
// onto an approval request a session opened; and the step's catch-up of an unfollow a build
// before this one recorded.
func askDrives(t *testing.T, r *writersRig) []graphcensus.StoreDrive {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	// What each drive's Ready writes for the drive and its check: the issue the close and the
	// reopen take, the copy, the document whose ask block is emptied and that ask, and the document
	// the carry settles and its approval request.
	var closing, copied, invalid, invalidAsk, carried, request string
	return []graphcensus.StoreDrive{{
		// alice answers the ask ses_asker opened (closeAskTx), through the one writer of an ask's
		// answer state (asks.Settled.Queue).
		Name: "api/ask_transitions.go#closeAskTx, answering", Database: r.database,
		Sites: []string{"api/ask_transitions.go#closeAskTx", "asks/settle.go#Queue"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/asks/"+r.ask+"/answer",
				map[string]any{"text": "The node store.", "expected_edited_at": nil}, "alice", http.StatusOK)
		},
		// The answer's text, selection, time, answerer as answered_by and as decided_by, and the
		// ask's answer_state. alice's node stands from her first comment.
		Counts: answerCounted,
	}, {
		// The close of an issue of its own holding an open ask, which withdraws the ask.
		Name: "api/issue_ask_lifecycle.go#recordWithdrawnAsksTx", Database: r.database,
		Sites: []string{"api/issue_ask_lifecycle.go#recordWithdrawnAsksTx", "asks/settle.go#Queue"},
		Ready: func(t *testing.T) {
			closing = decoded[model.Issue](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What a close withdraws", "force": true,
			}, "alice", http.StatusCreated)).Key
			r.request(t, http.MethodPost, "/api/v1/issues/"+closing+"/asks",
				map[string]any{"question": "Does the close withdraw this?"}, "alice", http.StatusCreated)
			// A close settles its issue's documents before its transaction, so they are settled
			// here, and the close's drive reads what the close alone appends.
			if err := r.documents.SettleIssueDocuments(ctx, closing); err != nil {
				t.Fatalf("settle the documents of %s: %v", closing, err)
			}
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPatch, "/api/v1/issues/"+closing, map[string]any{"status": "done"}, "alice", http.StatusOK)
		},
		Counts: resolutionCounted,
	}, {
		// The reopen of that issue, which restores the ask its close withdrew.
		Name: "api/issue_ask_lifecycle.go#recordRestoredAsksTx", Database: r.database,
		Sites: []string{"api/issue_ask_lifecycle.go#recordRestoredAsksTx", "asks/settle.go#Queue"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPatch, "/api/v1/issues/"+closing, map[string]any{"status": "todo"}, "alice", http.StatusOK)
		},
		// The restore appends the ask's ask.opened, under which its answer_state is open again, and
		// the retraction of the resolution its close stated.
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindAsk, graphstore.TupleAnswerState}: {"ask_opened"},
			{graphstore.KindAsk, graphstore.TupleResolution}:  {"ask_resolved"},
		},
	}, {
		// The settlement of a document of the issue holding a copy of the notes' ask block, whose
		// ask is open: the copy opens none, and the settlement claims the copy's copied_from. The
		// version it writes keeps the copy's source on the block's opener (docs' keptAskState), whose
		// copied_from_document is a dispatch:// address the block index reads as the block's mention
		// of the notes, as it read the same opener the release before stored.
		Name: "docs/ask_blocks.go#claimCopies", Database: r.database,
		Sites: []string{"docs/ask_blocks.go#claimCopies"},
		Ready: func(t *testing.T) {
			copied = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "copy.md", "content": ":::ask{#sizing-ask}\nWhich store do the writers take?\n:::\n",
			}, "alice", http.StatusCreated)).Artifact.ID
		},
		Drive: func(t *testing.T) {
			r.documents.SettleNow(copied)
			r.awaitTrue(t, "the settlement claiming the copy's copied_from", `
				select exists (select 1 from document_blocks d
					join graph_tuples t on t.from_kind = d.node_kind and t.from_id = d.node_address
					where d.artifact_id = $1::uuid and d.block_id = 'sizing-ask' and t.kind = $2)
			`, copied, graphstore.TupleCopiedFrom)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{refs.BlockNodeKind, graphstore.TupleCopiedFrom}: {"ask_copy"},
			{refs.BlockNodeKind, "mentions"}:                 {"block_ref"},
		},
	}, {
		// The settlement of a document whose ask block, opened by an earlier settlement, a person's
		// own editor left with no question: the block fails the strict read, and the settlement
		// states the reason on the ask's node (docs' stateInvalidReasons, through
		// asks.PlanInvalidReasons). No server write takes such a block (the edits route refuses
		// it, INVALID_ASK_BLOCK), so alice's browser writes it.
		Name: "asks/invalid.go#PlanInvalidReasons", Database: r.database,
		Sites: []string{"asks/invalid.go#PlanInvalidReasons"},
		Ready: func(t *testing.T) {
			invalid = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "invalid.md", "actor": sessionActor("ses_invalid"),
				"content": ":::ask{#" + invalidBlock + "}\nWhich way does the sizing go?\n:::\n",
			}, "", http.StatusCreated)).Artifact.ID
			invalidAsk = r.settledAsk(t, invalid, invalidBlock)
			r.emptyAskQuestion(t, invalid, invalidBlock)
		},
		Drive: func(t *testing.T) {
			r.documents.SettleNow(invalid)
			r.awaitTrue(t, "the settlement stating the emptied ask block's reason", `
				select exists (select 1 from graph_tuples where from_kind = $1 and from_id = $2 and kind = 'invalid' and asserted)
			`, graphstore.KindAsk, invalidAsk)
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindAsk, "invalid"}: {"ask_invalid"}},
	}, {
		// The settlement of a document whose live text holds again, byte for byte, the version a
		// person approved, while an approval request a session opened over the version that changed
		// it is open: the settlement versions it and carries the approval, whose answer of the
		// request goes through the one writer of an ask's answer state (answerApprovalAsk).
		Name: "docs/approval_carry.go#answerApprovalAsk", Database: r.database,
		Sites: []string{"docs/approval_carry.go#answerApprovalAsk", "asks/settle.go#Queue", "docs/settlement.go#appendEvents", "refs/blocks.go#Queue"},
		Ready: func(t *testing.T) {
			carried = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "carry.md", "content": "# Carry\n\nContext.\n",
			}, "alice", http.StatusCreated)).Artifact.ID
			if _, err := r.database.Pool.Exec(ctx, `
				insert into artifact_reviews (artifact_id, version, state, actor)
				values ($1, 1, 'approved', '{"kind":"user","id":"pat"}')
			`, carried); err != nil {
				t.Fatalf("approve version 1 of carry.md as pat: %v", err)
			}
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+carried+"/edits", map[string]any{
				"actor": sessionActor("ses_carrier"),
				"ops":   []map[string]any{{"op": "replace", "find": "Context.", "with": "Context, edited."}},
			}, "", http.StatusOK)
			request = decoded[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, r.request(t, http.MethodPost, "/api/v1/artifacts/"+carried+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_carrier")}, "", http.StatusCreated)).Ask.ID
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin alice's live edit: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := r.documents.Join(ctx, tx)
			defer ledger.Discard()
			if edit, err := r.documents.ApplyOps(joined, carried, []model.EditOp{{
				Op: "replace", Find: "Context, edited.", With: "Context.",
			}}, alice, nil); err != nil || !edit.Changed {
				t.Fatalf("undo the session's edit of carry.md live (changed %v): %v", edit.Changed, err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit alice's live edit: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			r.documents.SettleNow(carried)
			r.awaitTrue(t, "the settlement carrying the approval onto the request", `
				select exists (select 1 from asks where id = $1::uuid and state = 'answered')
			`, request)
		},
		// The carry's answer of the request, and the node of pat, the person who approved.
		Counts: merged(answerCounted, map[graphcensus.StoredKind][]string{
			{graphstore.KindPerson, graphstore.AttributeVisibility}: {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:      {graphstore.KindPerson},
		}),
	}, {
		// The decisions step's catch-up (a tick of docs.DecisionStep) of an unfollow a build before
		// this one recorded: that build deleted the follower row of the follower route's follow and
		// appended its ask.follower_removed, and stated nothing on the store, so the catch-up
		// retracts the followed_by the ask's node still asserts (asks.CatchUpFollower).
		Name: "asks/followers.go#CatchUpFollower, an unfollow", Database: r.database,
		Sites: []string{"asks/followers.go#CatchUpFollower"},
		Ready: func(t *testing.T) {
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the unfollow of the build before: %v", err)
			}
			defer tx.Rollback(ctx)
			if err := store.LockOwner(ctx, tx, store.IssueOwner(r.issue)); err != nil {
				t.Fatalf("take %s's owner lock: %v", r.issue, err)
			}
			if _, err := tx.Exec(ctx, `delete from ask_followers where ask_id = $1::uuid and session_id = 'ses_follower'`, r.ask); err != nil {
				t.Fatalf("delete the follower row as the build before: %v", err)
			}
			by := model.Actor{Kind: "session", ID: "ses_follower"}
			if _, err := events.NewBroker().Append(ctx, tx, model.Event{
				IssueKey: &r.issue, Type: "ask.follower_removed", Actor: by,
				Payload: model.AskFollowerEventPayload{AskID: r.ask, SessionID: "ses_follower", By: by},
			}); err != nil {
				t.Fatalf("append the unfollow's ask.follower_removed: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit the unfollow of the build before: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			if err := docs.NewDecisionStep(r.database, "", nil).Tick(ctx); err != nil {
				t.Fatalf("tick the decisions step: %v", err)
			}
			r.awaitTrue(t, "the catch-up retracting ses_follower's followed_by", `
				select not t.asserted from graph_tuples t
				where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_id = 'ses_follower'
				order by t.seq desc limit 1
			`, graphstore.KindAsk, r.ask, graphstore.TupleFollowedBy)
		},
		Counts: followCounted,
	}}
}

// settledAsk settles document, whose ask block block a session wrote with its upload, and answers
// the id of the ask the settlement opened for it.
func (r *writersRig) settledAsk(t *testing.T, document, block string) string {
	t.Helper()
	r.documents.SettleNow(document)
	r.awaitTrue(t, "the settlement opening ask block "+block, `
		select exists (select 1 from asks where block_artifact_id = $1::uuid and block_id = $2)
	`, document, block)
	var ask string
	if err := r.database.Pool.QueryRow(context.Background(),
		`select id::text from asks where block_artifact_id = $1::uuid and block_id = $2`, document, block).Scan(&ask); err != nil {
		t.Fatalf("read the ask of block %s: %v", block, err)
	}
	return ask
}

// emptyAskQuestion connects alice's browser to document's room, as the SPA's provider connects
// one, and empties the question of the document's ask block block in one local transaction, as a
// person's own editor may, and returns once the room holds the update. The browser stays connected
// until the test ends.
func (r *writersRig) emptyAskQuestion(t *testing.T, document, block string) {
	t.Helper()
	server := httptest.NewServer(r.documents)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + document + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	peer := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{"alice"}}, document, crdt.New())
	// roomHolds reports whether state, the content of the room's sync step 2, holds everything
	// the browser's document does: applying the browser's document over it changes nothing.
	roomHolds := func(state []byte) bool {
		room := crdt.New()
		if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
			t.Fatalf("decode the room's sync step 2: %v", err)
		}
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		if err := crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(peer.Doc, nil), nil); err != nil {
			t.Fatalf("apply the browser's document to the room's state: %v", err)
		}
		return bytes.Equal(before, crdt.EncodeStateAsUpdateV1(room, nil))
	}
	select {
	case <-peer.Answers:
	case <-time.After(30 * time.Second):
		t.Fatalf("the room of %s sent alice's browser nothing within 30s", document)
	}
	var emptied error
	// The fragment is taken before the transaction, which holds the document's lock.
	fragment := peer.Doc.GetXmlFragment("prosemirror")
	if _, err := peer.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			emptied = err
			return
		}
		for _, child := range tree.Children {
			if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); child.Type == "ask" && id == block && len(child.Children) > 0 {
				child.Children[0].Children = nil
				emptied = pmdoc.Update(txn, fragment, tree)
				return
			}
		}
		emptied = fmt.Errorf("the browser's document holds no ask block %s opening with a question", block)
	}); err != nil || emptied != nil {
		t.Fatalf("empty the question of %s in alice's browser: send %v, edit %v", block, err, emptied)
	}
	deadline := time.After(30 * time.Second)
	for {
		if err := peer.AskForDocument(); err != nil {
			t.Fatalf("ask the room of %s for its document: %v", document, err)
		}
		select {
		case state := <-peer.Answers:
			if roomHolds(state) {
				return
			}
		case <-peer.Ended:
			t.Fatalf("alice's browser was let go before the room of %s held her edit", document)
		case <-deadline:
			t.Fatalf("the room of %s never held alice's edit", document)
		}
	}
}
