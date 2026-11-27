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

	"github.com/jackc/pgx/v5"
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

// The blocks the ask drives write, each named so a drive's check can find it.
const (
	choiceBlock   = "sized-choice"
	longBlock     = "sized-long-choice"
	invalidBlock  = "sized-invalid"
	removedBlock  = "sized-removed"
	restoredBlock = "sized-restored"
)

// askDrives are the drives of the sites of the register of the store's writers that write an ask's
// answer state, a Keep of an agent's decision, the decisions step's restore and conversion, its
// catch-up, and the settlement's writes beyond the ask it opens, each the real site reached as its
// own lock-step driver reaches it, over the rig the drives before them wrote (importedDrives). They
// run in this order: alice's answer of the ask a session opened; the close of an issue of its own
// holding an open ask, and its reopen; alice's Keep of a decision a session uploaded; an upload of a
// decision in a document of more blocks than a write indexes in its own request, which writes the
// decision through the typed-node pass on its own; the settlement of a copy of the notes' ask block
// on a document of its own; the settlement of an ask block whose question a person's edit emptied;
// the settlement that carries a person's approval onto an approval request a session opened; a
// person's edit that takes the block of an answered ask out of its document; an upload that changes
// nothing, whose version no index pass plans; the settlement of a person's upload that writes the
// answered ask's block back, which retracts that person's removal; the decisions step's conversion
// of an issue whose decisions document holds one section a session wrote and a person kept; and
// the step's catch-up of an unfollow a build before this one recorded.
func askDrives(t *testing.T, r *writersRig) []graphcensus.StoreDrive {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	// What each drive's Ready writes for the drive and its check: the issue the close and the
	// reopen take, the document the Keep keeps a decision of and the decision's sum, the copy, the
	// document whose ask block is emptied and that ask, the document the carry settles and its
	// approval request, the document whose answered ask a person removes and that ask, the document
	// an upload leaves as it was, the project the decisions step converts, and the answered ask an
	// earlier release rewords.
	var closing, choices, choiceSum, copied, invalid, invalidAsk, carried, request, removal, removedAsk, unchanged, restoredAsk string
	// The token of the version same.md's replacement replaces, read before its drive.
	var unchangedBase map[string]string
	const decided = "DECIDE"
	// The text of removal.md as its session wrote it, which the retraction's drive writes back.
	const removalText = "# Removal\n\nContext.\n\n:::ask{#" + removedBlock + "}\nWhich block does a person remove?\n:::\n"
	return []graphcensus.StoreDrive{{
		// alice answers the ask ses_asker opened (closeAskTx), through the one writer of an ask's
		// answer state (asks.Settled.Queue), at the edited_at the asker's edit of it left
		// (api/asks.go#editAsk), which she reviewed.
		Name: "api/ask_transitions.go#closeAskTx, answering", Database: r.database,
		Sites: []string{"api/ask_transitions.go#closeAskTx", "asks/settle.go#Queue"},
		Drive: func(t *testing.T) {
			var edited time.Time
			if err := r.database.Pool.QueryRow(ctx, `select edited_at from asks where id = $1::uuid`, r.ask).Scan(&edited); err != nil {
				t.Fatalf("read the edited_at of ask %s: %v", r.ask, err)
			}
			r.request(t, http.MethodPost, "/api/v1/asks/"+r.ask+"/answer", map[string]any{
				"text": "The node store.", "expected_edited_at": edited.UTC().Format(time.RFC3339Nano),
			}, "alice", http.StatusOK)
		},
		// The answer's text, selection, time, answerer as answered_by and as decided_by, and the
		// ask's answer_state. alice's node stands from her first comment. The answer's body
		// block, and alice's part in the ask's thread with the read mark the answer moves; and her
		// own answer, written as herself, takes the ask's thread out of her Inbox, her dismissed,
		// which the term dismissal counts (the own-answer rule, R1).
		Counts: merged(answerCounted, bodyBlockCounted, ownThreadCounted, map[graphcensus.StoredKind][]string{
			{graphstore.KindPerson, graphstore.TupleDismissed}: {"dismissal"},
		}),
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
		Sites: []string{"api/issue_ask_lifecycle.go#recordRestoredAsksTx", "asks/settle.go#Queue", "inboxwrite/waiting.go#QueueMoveWaiting"},
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
		// alice's Keep of a decision a session uploaded (keepDecisionNode), at the text_sha256 the
		// issue's decisions read serves.
		Name: "api/decisions.go#keepDecisionNode", Database: r.database,
		Sites: []string{"api/decisions.go#keepDecisionNode"},
		Ready: func(t *testing.T) {
			choices = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "choices.md", "content": "::::decision{#" + choiceBlock + "}\nThe writers take the node store.\n::::\n",
				"actor": sessionActor("ses_decider"),
			}, "", http.StatusCreated)).Artifact.ID
			read := decoded[struct {
				Entries []struct {
					TextSHA256 string `json:"text_sha256"`
					Document   *struct {
						ArtifactID string `json:"artifact_id"`
						BlockID    string `json:"block_id"`
					} `json:"document"`
				} `json:"entries"`
			}](t, r.request(t, http.MethodGet, "/api/v1/issues/"+r.issue+"/decisions", nil, "alice", http.StatusOK))
			for _, entry := range read.Entries {
				if entry.Document != nil && entry.Document.ArtifactID == choices && entry.Document.BlockID == choiceBlock {
					choiceSum = entry.TextSHA256
				}
			}
			if choiceSum == "" {
				t.Fatalf("the decisions of %s list no decision %s of choices.md", r.issue, choiceBlock)
			}
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+choices+"/blocks/"+choiceBlock+"/keep",
				map[string]any{"text_sha256": choiceSum}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{refs.BlockNodeKind, refs.TupleKeptBy}:      {"keep"},
			{refs.BlockNodeKind, refs.ValueKeptTextSum}: {"keep"},
		},
	}, {
		// A session's upload of a decision at the end of a document of more blocks than a write
		// indexes in its own request: the write leaves the blocks to the block-index tick and
		// writes the decision through the typed-node pass on its own (refs.DecisionIndexing.Queue).
		Name: "refs/decision_nodes.go#Queue, above the deferral bound", Database: r.database,
		Sites: []string{"refs/decision_nodes.go#Queue", "api/artifacts.go#storeArtifact", "api/artifacts.go#queueNewArtifact"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "long.md", "actor": sessionActor("ses_long"),
				"content": strings.Repeat("A paragraph past the bound.\n\n", 2001) +
					"::::decision{#" + longBlock + "}\nThe long document takes the node store.\n::::\n",
			}, "", http.StatusCreated)
		},
		// The new document's node, and the decision's node, placed in it, with its decided_by and
		// the node of the session that wrote it; its in_document waits for the block-index tick.
		Counts: merged(objectNodeCounted(graphstore.KindArtifact), sessionNodeCounted, map[graphcensus.StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:      {"block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:    {"block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"block"},
			{refs.BlockNodeKind, graphstore.TupleDecidedBy}:      {"decision"},
		}),
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
		// states the reason on the ask's node (asks.PlanInvalidReasons, from docs'
		// stateCopiesAndReadability). No server write takes such a block (the edits route refuses
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
		// The carry's answer of the request, and the node of pat, the person who approved; the
		// answer's body block, the request reaching the person it now waits on, and pat's part in
		// its thread with the read mark the answer moves.
		Counts: merged(answerCounted, bodyBlockCounted, askDeliveredCounted, ownThreadCounted, map[graphcensus.StoredKind][]string{
			{graphstore.KindPerson, graphstore.AttributeVisibility}: {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:      {graphstore.KindPerson},
		}),
	}, {
		// alice's edit that takes the block of an answered ask out of its document: the version's
		// index pass plans her removal into its own write (refs.PlanBlocks), and the edit appends
		// it with the index's facts under the version's event.
		Name: "refs/blocks.go#Queue, a person's removal of an answered ask", Database: r.database,
		Sites: []string{"refs/blocks.go#Queue", "api/artifacts.go#commitArtifactVersionEvent"},
		Ready: func(t *testing.T) {
			removal = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "removal.md", "actor": sessionActor("ses_removed"), "content": removalText,
			}, "", http.StatusCreated)).Artifact.ID
			removedAsk = r.settledAsk(t, removal, removedBlock)
			r.request(t, http.MethodPost, "/api/v1/asks/"+removedAsk+"/answer",
				map[string]any{"text": "This one.", "expected_edited_at": nil}, "alice", http.StatusOK)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+removal+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "delete", "block": removedBlock}},
			}, "alice", http.StatusOK)
		},
		// The removed block's node and its paragraph's stated gone, and alice's removed_by on the
		// answered ask's node.
		Counts: map[graphcensus.StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeState}: {"block_gone"},
			{graphstore.KindAsk, graphstore.TupleRemovedBy}: {"ask_removal"},
		},
	}, {
		// An upload that writes a document's text over with the text it holds: the replacement
		// changes nothing and writes no tree, so the version's index pass runs on no rendering and
		// the removals it carries are queued on their own (queueRemovals) - none, since the write
		// took no block out, and so it appends nothing. The removal it would append is the one
		// the drive before appends through the planned path, the same person's node and the same
		// removed_by.
		Name: "docs/removed_by.go#queueRemovals", Database: r.database,
		Sites: []string{"docs/removed_by.go#queueRemovals", "api/artifacts.go#storeArtifact"},
		Ready: func(t *testing.T) {
			unchanged = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "same.md", "content": "# Same\n\nA text the upload writes again.\n",
			}, "alice", http.StatusCreated)).Artifact.ID
			unchangedBase = r.uploadBase(t, unchanged)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "same.md", "content": "# Same\n\nA text the upload writes again.\n", "precondition": unchangedBase,
			}, "alice", http.StatusCreated)
			r.awaitTrue(t, "the upload writing version 2 of same.md", `
				select exists (select 1 from artifact_versions where artifact_id = $1::uuid and number = 2)
			`, unchanged)
		},
		Counts: map[graphcensus.StoredKind][]string{},
	}, {
		// The settlement of removal.md once alice's upload has written the answered ask's block
		// back: the block stands in the document its ask's row names again, so the settlement
		// retracts the removed_by alice's edit stated (the drive of a person's removal of an
		// answered ask, above) through planRemovalRetractions, from docs'
		// stateCopiesAndReadability, and a later removal is judged afresh. The upload is the
		// Ready's, so the drive reads what the settlement alone appends.
		Name: "docs/ask_blocks.go#planRemovalRetractions", Database: r.database,
		Sites: []string{"docs/ask_blocks.go#planRemovalRetractions"},
		Ready: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "removal.md", "content": removalText, "precondition": r.uploadBase(t, removal),
			}, "alice", http.StatusCreated)
		},
		Drive: func(t *testing.T) {
			r.documents.SettleNow(removal)
			r.awaitTrue(t, "the settlement retracting alice's removal of the answered ask", `
				select not t.asserted from graph_tuples t
				where t.from_kind = $1 and t.from_id = $2 and t.kind = $3
				order by t.seq desc limit 1
			`, graphstore.KindAsk, removedAsk, graphstore.TupleRemovedBy)
		},
		// The retraction of alice's removed_by on the answered ask's node.
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindAsk, graphstore.TupleRemovedBy}: {"ask_removal"}},
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
	}, {
		// The decisions step's conversion of an issue of a project of its own, whose decisions
		// document holds one section an agent's session wrote and a person kept
		// (docs.DecisionStep.RestoreAndConvert): the decisions document's version without the
		// section, the spec's version holding it as a decision, the section's move and the Keep
		// carried onto the decision's node, and the decisions document's retirement. This release's
		// upload refuses a decisions document (DECISIONS_ARE_NODES), so the document is written as a
		// release before wrote one: its row, its first text and version, and that version's block
		// index.
		Name: "docs/decision_restore.go#writeFacts, converting a decisions document", Database: r.database,
		Sites: []string{"docs/decision_restore.go#writeFacts", "docs/decision_restore.go#writeVersion",
			"docs/retire.go#RetireDocument", "docs/retire.go#writeRetiredNode", "refs/blocks.go#Queue"},
		Ready: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": decided, "name": "Decided"}, "alice", http.StatusCreated)
			issue := decoded[model.Issue](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": decided, "title": "What the decisions step converts",
				"spec": "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n",
			}, "alice", http.StatusCreated)).Key
			// The section's author is ses_decider, whose node the Keep drive's upload wrote, so the
			// conversion's decided_by names a node the store holds and states none of its own.
			decisions := r.earlierDecisionsDocument(t, issue, decided,
				"# Decisions\n\n## Files stay out of the corpus\n\nWe write no file beside the database.\n",
				model.Actor{Kind: "session", ID: "ses_decider"})
			if err := r.documents.SettleIssueDocuments(ctx, issue); err != nil {
				t.Fatalf("settle the documents of %s: %v", issue, err)
			}
			rows, err := r.database.Pool.Query(ctx, `select block_id from document_blocks where artifact_id = $1::uuid and type = 'heading'`, decisions)
			if err != nil {
				t.Fatalf("read the headings of decisions.md: %v", err)
			}
			headings, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read the headings of decisions.md: %v", err)
			}
			kept := false
			for _, heading := range headings {
				section, found, err := r.documents.DecisionHeading(ctx, r.database.Pool, decisions, heading)
				if err != nil {
					t.Fatalf("read the section heading %s opens: %v", heading, err)
				}
				if !found {
					continue
				}
				if _, err := r.database.Pool.Exec(ctx, `
					insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
					values ($1::uuid, $2, $3, 1, 'lee')
				`, decisions, heading, section.TextSHA256); err != nil {
					t.Fatalf("write lee's Keep of %s: %v", heading, err)
				}
				kept = true
			}
			if !kept {
				t.Fatalf("decisions.md of %s holds no section among its headings %v", issue, headings)
			}
		},
		Drive: func(t *testing.T) {
			step := docs.NewDecisionStep(r.database, "", nil)
			step.UseDocuments(r.documents)
			done, failed, err := step.RestoreAndConvert(ctx, decided, true, false)
			if err != nil || len(failed) > 0 {
				t.Fatalf("the decisions step's conversion answered %v with failures %+v", err, failed)
			}
			if len(done) != 1 || done[0].Decisions != 1 || !done[0].Retired {
				t.Fatalf("the decisions step did %+v, want one section a decision and its decisions document retired", done)
			}
		},
		// The versions' blocks - the spec's new decision, and the decisions document's section
		// stated gone - the decision's decided_by, which names the session that wrote its section
		// (docs' creditSectionAuthors), the section's move, the Keep it carries with the keeper's
		// node, and the retired document's node stated gone.
		Counts: merged(newBlockCounted, map[graphcensus.StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeState}:         {"block", "block_gone"},
			{refs.BlockNodeKind, graphstore.TupleDecidedBy}:         {"decision"},
			{refs.BlockNodeKind, graphstore.TupleMovedTo}:           {"block_move"},
			{refs.BlockNodeKind, refs.TupleKeptBy}:                  {"keep"},
			{refs.BlockNodeKind, refs.ValueKeptTextSum}:             {"keep"},
			{graphstore.KindPerson, graphstore.AttributeVisibility}: {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:      {graphstore.KindPerson},
			{graphstore.KindArtifact, graphstore.AttributeState}:    {"artifact_removed"},
		}),
	}, {
		// The boot step that writes back the wording an answered ask was decided on, which an
		// earlier release's edit of its row changed after the answer (docs.RestoreDecidedWordings).
		Name: "docs/decided_wording.go#restoreWordings", Database: r.database,
		Sites: []string{"docs/decided_wording.go#restoreWordings"},
		Ready: func(t *testing.T) {
			restoredDocument := decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "restored.md", "actor": sessionActor("ses_restored"),
				"content": "# Restored\n\nContext.\n\n:::ask{#" + restoredBlock + "}\nWhich wording was decided?\n:::\n",
			}, "", http.StatusCreated)).Artifact.ID
			restoredAsk = r.settledAsk(t, restoredDocument, restoredBlock)
			r.request(t, http.MethodPost, "/api/v1/asks/"+restoredAsk+"/answer",
				map[string]any{"text": "This one.", "expected_edited_at": nil}, "alice", http.StatusOK)
			// An earlier release's edit of the answered ask: its row reworded, and the ask.edited
			// recording the wording the answer was given on as the edit's previous, which the
			// restoration reads back.
			answered := decoded[struct {
				Ask model.Ask `json:"ask"`
			}](t, r.request(t, http.MethodGet, "/api/v1/asks/"+restoredAsk, nil, "alice", http.StatusOK)).Ask
			previous := model.NewAskEditPrevious(answered)
			answered.Question = "Which wording did an earlier release write?"
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the earlier release's edit: %v", err)
			}
			defer tx.Rollback(ctx)
			if err := store.LockOwner(ctx, tx, store.IssueOwner(r.issue)); err != nil {
				t.Fatalf("take %s's owner lock: %v", r.issue, err)
			}
			if _, err := tx.Exec(ctx, `update asks set question = $2, edited_at = now() where id = $1::uuid`,
				restoredAsk, answered.Question); err != nil {
				t.Fatalf("reword the answered ask as an earlier release's edit does: %v", err)
			}
			if _, err := events.NewBroker().Append(ctx, tx, model.Event{
				IssueKey: &r.issue, Type: "ask.edited", Actor: docs.SettlementActor,
				Payload: model.NewAskEditEventPayload(answered, previous, docs.SettlementActor, model.ReferenceChanges{}),
			}); err != nil {
				t.Fatalf("append the earlier release's ask.edited: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit the earlier release's edit: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			restored, _, err := r.documents.RestoreDecidedWordings(ctx)
			if err != nil || restored != 1 {
				t.Fatalf("restore the decided wording (%d asks): %v", restored, err)
			}
		},
		Counts: map[graphcensus.StoredKind][]string{},
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

// earlierDecisionsDocument writes issue's decisions document holding markdown as a release before
// this one wrote one, which this release's upload refuses: its artifacts row, of the decisions
// role, then in one transaction of the document service its first text, its version 1 written by
// writer and that version's block index, caused at the sentinel as a version no event recorded is.
// Its node is then written as the decisions step's first phase writes the node of a row an
// earlier release wrote with none (graphcensus.BackfillAsks), which a server of this release runs
// at boot before the conversion can. It answers the document's id.
func (r *writersRig) earlierDecisionsDocument(t *testing.T, issue, project, markdown string, writer model.Actor) string {
	t.Helper()
	ctx := context.Background()
	var document string
	if err := r.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		values ($1, $2, 'decisions-md', 'decisions.md', 'doc', false, 'decisions', '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue, project).Scan(&document); err != nil {
		t.Fatalf("write the decisions document of %s: %v", issue, err)
	}
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the decisions document's first version: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := r.documents.Join(ctx, tx)
	defer ledger.Discard()
	written, err := r.documents.SeedText(joined, document, markdown, writer)
	if err != nil {
		t.Fatalf("write the decisions document's first text: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, jsonb_build_array(jsonb_build_object('kind', $3::text, 'id', $4::text)))
	`, document, written, writer.Kind, writer.ID); err != nil {
		t.Fatalf("write the decisions document's version 1: %v", err)
	}
	_, indexed, err := r.documents.IndexDocumentReferences(joined, document, 1, written)
	if err != nil {
		t.Fatalf("index the decisions document's version 1: %v", err)
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, indexed, graphstore.NoEvent)
		return nil
	}); err != nil {
		t.Fatalf("queue the cause of the decisions document's index: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the decisions document's first version: %v", err)
	}
	if _, err := graphcensus.BackfillAsks(ctx, r.database); err != nil {
		t.Fatalf("write the node of the decisions document as the first phase does: %v", err)
	}
	return document
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
