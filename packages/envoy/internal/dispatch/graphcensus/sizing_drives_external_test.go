package graphcensus_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// externalDrives are the drives of the sites of api and docs the register of the store's writers
// lists and no drive before them runs, each the real site reached as its own
// lock-step driver reaches it, over the rig the drives before them wrote (importedDrives). They
// run in this order: alice's run of the command bar and its one frame; her writes made for that
// run - a PATCH of the issue's labels, an edit of an ask's words and one of a comment's; a PATCH
// of the issue's needs and of its filed_from at a comment the release before wrote; a lens on
// the spec with a finding, a second lens's triggered_by of it, and the first lens stated
// overtaken; a result of a check of the issue's Verification document and its retraction; an
// accept that finds a suggestion's mark gone; an upload that takes a suggestion's quote out of
// its document; the anchor block backfill of a comment written with no block; a settlement of an
// issue's room; R2's daemon-thread step over a paragraph a parent pointer placed in a thread the
// Legion daemon's session began; R6's conversion of an issue a build before the issue room
// created, holding an ask and a comment anchored on its spec; and R7's delete of a lens with a
// finding a comment is anchored on, which orphans that comment's node anchor (R31).
func externalDrives(t *testing.T, r *writersRig) []graphcensus.StoreDrive {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	// What a drive or its Ready writes for itself and the drives after it: alice's run, the ask and
	// the comment her run edits, the issue the PATCH waits on and the comment it is filed from, the
	// first lens and the second, the result, the suggestion the accept finds orphaned and the one
	// the refresh orphans, the comment the backfill pins, the issue whose room a settlement
	// settles with its spec, the issue whose paragraph the daemon-thread step moves, and the issue
	// the conversion converts with its spec and the ask it moves into the issue's room, and the
	// lens the node DELETE takes, with the count its dry run answers and the lens's placement.
	var run, ask, edited, target, filedFrom, lens, chained, result, orphaned, refreshed, pinned string
	var roomIssue, roomSpec, daemonIssue, converting, convertingSpec, movedAsk, deleted, deletedPlacement string
	var deletedCount int
	// The token of the version refreshed.md's second upload replaces, read before its drive.
	var refreshedBase map[string]string
	return []graphcensus.StoreDrive{{
		// alice's run of the command bar (recordGoingRequest), which records the run's node with
		// her words, its ends_at, its project and her authored_by, which the term interaction
		// counts, and, as no answerer is configured, its one frame, the run's ending
		// (appendGoingEvent through endGoingRun), which the term frame counts. alice's node stands
		// from the issue's spec.
		Name: "api/going.go#recordGoingRequest, a run of the command bar", Database: r.database,
		Sites: []string{"api/going.go#recordGoingRequest", "api/going.go#appendGoingEvent"},
		Drive: func(t *testing.T) {
			run = decoded[struct {
				ID string `json:"id"`
			}](t, r.request(t, http.MethodPost, "/api/v1/going/requests",
				map[string]any{"words": "What do the writers write?", "project": "SIZE"}, "alice", http.StatusCreated)).ID
			r.awaitTrue(t, "run "+run+"'s ending frame", `
				select exists (select 1 from going_events where request_id = $1::uuid and name in ('end', 'error'))
			`, run)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindInteraction, graphstore.AttributeVisibility}: {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeState}:      {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeProject}:    {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeWords}:      {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeEndsAt}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeSource}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.TupleAuthoredBy}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeFrame}:      {"frame"},
		},
	}, {
		// A PATCH of the issue's labels made for alice's run (writeIssue, which patchIssue runs),
		// which restates the issue's node marked changed by the run: its changed_by_run, which the
		// term interaction counts into the run.
		Name: "api/issue_patch.go#writeIssue, under a run", Database: r.database,
		Sites: []string{"api/issue_patch.go#writeIssue"},
		Ready: func(t *testing.T) { r.runWindowAhead(t, run) },
		Drive: func(t *testing.T) {
			r.requestForRun(t, run, http.MethodPatch, "/api/v1/issues/"+r.issue, map[string]any{"labels": []string{"sized"}}, "alice", http.StatusOK)
		},
		Counts: changedByRunCounted(graphstore.KindIssue),
	}, {
		// An edit of the words of an ask alice asked, made for her run (editAsk), which restates the
		// ask's node marked changed by the run: its changed_by_run.
		Name: "api/asks.go#editAsk, under a run", Database: r.database,
		Sites: []string{"api/asks.go#editAsk"},
		Ready: func(t *testing.T) {
			ask = decoded[model.Ask](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/asks",
				map[string]any{"question": "Which words does the run change?"}, "alice", http.StatusCreated)).ID
			r.runWindowAhead(t, run)
		},
		Drive: func(t *testing.T) {
			r.requestForRun(t, run, http.MethodPatch, "/api/v1/asks/"+ask,
				map[string]any{"question": "Which words did the run change?"}, "alice", http.StatusOK)
		},
		Counts: changedByRunCounted(graphstore.KindAsk),
	}, {
		// An edit of the words of a comment alice wrote, made for her run (editComment), which
		// restates the comment's node marked changed by the run: its changed_by_run.
		Name: "api/comment_actions.go#editComment, under a run", Database: r.database,
		Sites: []string{"api/comment_actions.go#editComment"},
		Ready: func(t *testing.T) {
			edited = decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments",
				map[string]any{"body": "A remark the run rewords."}, "alice", http.StatusCreated)).ID
			r.runWindowAhead(t, run)
		},
		Drive: func(t *testing.T) {
			r.requestForRun(t, run, http.MethodPatch, "/api/v1/comments/"+edited,
				map[string]any{"body": "A remark the run reworded."}, "alice", http.StatusOK)
		},
		Counts: changedByRunCounted(graphstore.KindComment),
	}, {
		// A PATCH of the issue's relations (patchIssue): it waits on an issue of its own and is
		// filed from a comment the release before wrote, which carries no node. The comment's node
		// is written first as the backfill of the comments writes it (stateFiledFromNode), with its
		// base attributes and its authored_by, which the comment terms count, then the issue's
		// node, and last the relations the issue's event causes (queueIssueRelations): the needs,
		// which the term issue_relation counts, and the filed_from, which issue_origin counts.
		Name: "api/issue_relations.go#queueIssueRelations, a PATCH of its needs and its filed_from", Database: r.database,
		Sites: []string{"api/issue_relations.go#queueIssueRelations", "api/issue_relations.go#stateFiledFromNode",
			"api/issue_patch.go#writeIssue"},
		Ready: func(t *testing.T) {
			target = decoded[model.Issue](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What the sized issue waits on", "force": true,
			}, "alice", http.StatusCreated)).Key
			if err := r.database.Pool.QueryRow(ctx, `
				insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'Words the release before wrote.')
				returning id::text
			`, r.issue).Scan(&filedFrom); err != nil {
				t.Fatalf("write the comment the issue is filed from as the release before wrote it: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPatch, "/api/v1/issues/"+r.issue, map[string]any{
				"needs": []string{target}, "filed_from": "dispatch://" + r.issue + "/comment/" + filedFrom,
			}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindIssue, graphstore.TupleNeeds}:            {"issue_relation"},
			{graphstore.KindIssue, graphstore.TupleFiledFrom}:        {"issue_origin"},
		},
	}, {
		// alice's pre-mortem on the spec, with one finding (writeReasoningNodes): the lens's node
		// with its base attributes, its part_of the spec, its has_kind, its verdict, the artifact_id
		// of the spec's row the store records beside that part_of (graphstore.AttributeArtifactID)
		// and her authored_by, which the term lens counts, and the finding's block node with its base
		// attributes, its part_of the lens, its text and her authored_by, which lens_finding
		// counts, in one write.
		Name: "api/reasoning_write.go#writeReasoningNodes, a lens with a finding", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningNodes"},
		Drive: func(t *testing.T) {
			lens = decoded[lensInvoked](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
				"on": "dispatch://" + r.issue + "/spec", "kind": graphstore.LensKindPreMortem,
				"verdict": "The writers hold if the sizing counts them.", "findings": []string{"A writer the sizing misses reads the year low."},
			}, "alice", http.StatusCreated)).ID
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindLens, graphstore.AttributeVisibility}: {graphstore.KindLens},
			{graphstore.KindLens, graphstore.AttributeState}:      {graphstore.KindLens},
			{graphstore.KindLens, graphstore.AttributeProject}:    {graphstore.KindLens},
			{graphstore.KindLens, graphstore.TuplePartOf}:         {graphstore.KindLens},
			{graphstore.KindLens, graphstore.TupleHasKind}:        {graphstore.KindLens},
			{graphstore.KindLens, graphstore.AttributeVerdict}:    {graphstore.KindLens},
			{graphstore.KindLens, graphstore.AttributeArtifactID}: {graphstore.KindLens},
			{graphstore.KindLens, graphstore.TupleAuthoredBy}:     {graphstore.KindLens},
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:  {"lens_finding"},
			{refs.BlockNodeKind, graphstore.AttributeState}:       {"lens_finding"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:     {"lens_finding"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:          {"lens_finding"},
			{refs.BlockNodeKind, graphstore.AttributeText}:        {"lens_finding"},
			{refs.BlockNodeKind, graphstore.TupleAuthoredBy}:      {"lens_finding"},
		},
	}, {
		// A second lens on the spec, made triggered by the first through the pointer route
		// (writeReasoningPointer): the one triggered_by tuple, which the term lens counts.
		Name: "api/reasoning_write.go#writeReasoningPointer, a lens's triggered_by", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningPointer"},
		Ready: func(t *testing.T) {
			chained = decoded[lensInvoked](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
				"on": "dispatch://" + r.issue + "/spec", "kind": "trade-off", "verdict": "The drives cost a minute.",
			}, "alice", http.StatusCreated)).ID
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+nodePath(graphstore.KindLens+"/"+chained, "pointers"), map[string]any{
				"kind": graphstore.TupleTriggeredBy, "action": "append", "to": "dispatch://" + graphstore.KindLens + "/" + lens,
			}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindLens, graphstore.TupleTriggeredBy}: {graphstore.KindLens}},
	}, {
		// The first lens stated overtaken through the node state route (writeNodeState): its state
		// value, which the term node_state counts.
		Name: "api/reasoning_write.go#writeNodeState, a lens overtaken", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeNodeState"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+nodePath(graphstore.KindLens+"/"+lens, "state"),
				map[string]any{"state": graphstore.StateOvertaken}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindLens, graphstore.AttributeState}: {"node_state"}},
	}, {
		// A failed result alice records of a check of the issue's Verification document
		// (writeRecordedResult): the result's node with its base attributes, its values, its
		// part_of, its result_of its check and her authored_by, which the term result counts. A
		// person's result names no run_session, and her node stands from the issue's spec. The
		// issue is born converted, so the result moves the Verification query's record: its batch
		// marks the issue's queries owed, and the drain the route runs once the result has
		// committed appends a version of that document under result.recorded (drainMark, the one
		// writer of the drain's version, whose QueueCaused states its facts): the version's node, a
		// query version with its definition_version.
		Name: "api/results.go#writeRecordedResult", Database: r.database,
		Sites: []string{"api/results.go#writeRecordedResult", "docs/query_marks.go#drainMark", "docs/mutation.go#WriteVersionNode"},
		Ready: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "verification.md", "content": ":::check{#" + sizedCheck + " surface=\"cli\"}\nThe sizing counts every writer.\n:::\n",
			}, "alice", http.StatusCreated)
		},
		Drive: func(t *testing.T) {
			result = decoded[struct {
				Result model.CheckResult `json:"result"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/results", map[string]any{
				"check": sizedCheck, "role": "implementer", "verdict": "fail", "commit": "1111111111111111111111111111111111111111",
				"negative_control": "Broke what the check proves and the run failed.", "failed": 1,
			}, "alice", http.StatusCreated)).Result.ID
		},
		Counts: merged(resultCounted(), versionCounted, queryVersionCounted),
	}, {
		// alice's retraction of that result (writeCheckResultRetraction): its retracted value,
		// which the term result_retraction counts, and the version of the Verification document
		// the retraction moves the record of, which the drain appends under result.retracted
		// (drainMark).
		Name: "api/results_retract.go#writeCheckResultRetraction", Database: r.database,
		Sites: []string{"api/results_retract.go#writeCheckResultRetraction", "docs/query_marks.go#drainMark", "docs/mutation.go#WriteVersionNode"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/results/"+result+"/retract",
				map[string]any{"reason": "The run measured the wrong build."}, "alice", http.StatusCreated)
		},
		Counts: merged(map[graphcensus.StoredKind][]string{{graphstore.KindResult, graphstore.AttributeRetracted}: {"result_retraction"}},
			versionCounted, queryVersionCounted),
	}, {
		// An accept of a suggestion whose mark is gone from a row the refresh did not reach
		// (orphanCommentAnchor): the suggestion is anchored on a paragraph of a document of its
		// own, the paragraph is deleted, and the suggestion's anchor, its anchored_in and its
		// part_of are stood up again as such a row stands, so the accept is what orphans it and
		// retracts its anchored_in and its part_of, which the term anchor_block counts.
		Name: "api/comment_actions.go#orphanCommentAnchor, an accept that finds the mark gone", Database: r.database,
		Sites: []string{"api/comment_actions.go#orphanCommentAnchor"},
		Ready: func(t *testing.T) {
			const words = "Words the suggestion orphans"
			document := r.uploaded(t, "suggested.md", "# Suggested\n\n"+words+".\n")
			comment := r.suggestion(t, document, words)
			orphaned = comment.ID
			block := r.anchoredBlock(t, document, *comment.Anchor.BlockID)
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+document+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "delete", "find": words + "."}},
			}, "alice", http.StatusOK)
			if _, err := r.database.Pool.Exec(ctx, `update comments set anchor = jsonb_set(anchor, '{orphaned}', 'false') where id = $1::uuid`, orphaned); err != nil {
				t.Fatalf("stand the suggestion's anchor up again: %v", err)
			}
			r.writeAnchoredIn(t, orphaned, block, true)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/comments/"+orphaned+"/accept", map[string]any{}, "alice", http.StatusConflict)
		},
		Counts: anchorBlockCounted,
	}, {
		// An upload of a document of the issue without the quote a suggestion is anchored on: the
		// version's write refreshes the document's anchors, which orphans the suggestion's and
		// retracts its anchored_in and its part_of under its comment.anchor_refreshed
		// (stateRefreshedAnchoredIn), beside the version's own node, the new paragraph's block with
		// its values, and the suggested paragraph's block stated gone.
		Name: "docs/anchor_refresh.go#stateRefreshedAnchoredIn, an upload that takes a suggestion's quote out", Database: r.database,
		Sites: []string{"docs/anchor_refresh.go#stateRefreshedAnchoredIn", "api/artifacts.go#storeArtifact",
			"refs/blocks.go#Queue", "docs/mutation.go#WriteVersionNode"},
		Ready: func(t *testing.T) {
			document := r.uploaded(t, "refreshed.md", "# Refreshed\n\nThe moon is up.\n")
			refreshed = r.suggestion(t, document, "moon is up").ID
			refreshedBase = r.uploadBase(t, document)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "refreshed.md", "content": "# Refreshed\n\nStars come out.\n", "precondition": refreshedBase,
			}, "alice", http.StatusCreated)
			r.awaitTrue(t, "the refresh orphaning the suggestion's anchor", `
				select coalesce((anchor->>'orphaned')::boolean, false) from comments where id = $1::uuid
			`, refreshed)
		},
		Counts: merged(newBlockCounted, blockValuesCounted, versionCounted, anchorBlockCounted, map[graphcensus.StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeState}: {"block_gone"},
		}),
	}, {
		// The anchor block backfill (docs.Service.BackfillAnchorBlocks) of a comment whose anchor a
		// build before wrote with no block and whose node states no anchored_in: it pins the block
		// and states the anchored_in and the part_of to it (pinCommentAnchorBlock), which the term
		// anchor_block counts of the comments the window holds.
		Name: "docs/anchor_blocks.go#pinCommentAnchorBlock, the anchor block backfill", Database: r.database,
		Sites: []string{"docs/anchor_blocks.go#pinCommentAnchorBlock"},
		Ready: func(t *testing.T) {
			document := r.uploaded(t, "pinned.md", "# Pinned\n\nA lazy dog sleeps.\n")
			comment := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments", map[string]any{
				"body": "A remark on the dog.", "anchor": map[string]any{"artifact": document, "quote": "lazy dog"},
			}, "alice", http.StatusCreated))
			if comment.Anchor == nil || comment.Anchor.BlockID == nil {
				t.Fatalf("the anchored comment answers anchor %+v, want one naming its block", comment.Anchor)
			}
			pinned = comment.ID
			block := r.anchoredBlock(t, document, *comment.Anchor.BlockID)
			if _, err := r.database.Pool.Exec(ctx, `update comments set anchor = anchor - 'block_id' where id = $1::uuid`, pinned); err != nil {
				t.Fatalf("write the comment's anchor as the release before wrote it, with no block: %v", err)
			}
			r.writeAnchoredIn(t, pinned, block, false)
		},
		Drive: func(t *testing.T) {
			backfilled, err := r.documents.BackfillAnchorBlocks(ctx)
			if err != nil {
				t.Fatalf("backfill the anchor blocks: %v", err)
			}
			if backfilled.Comments != 1 {
				t.Fatalf("the anchor block backfill pinned %d comments, want comment %s alone", backfilled.Comments, pinned)
			}
		},
		Counts: anchorBlockCounted,
	}, {
		// The settlement of an issue's room (docs.Service.SettleIssueDocuments), after alice's live
		// edit of the spec of an issue of its own, born converted: the room's one index pass over
		// the spec's new version, its facts caused by that version's event (causeRoomFacts) - the
		// new paragraph's block with its values and alice's authored_by, as a block of an issue's
		// room records its authors, and the version's node, a query version with its
		// definition_version. Its write phase marks the issue's queries owed, and the drain it runs
		// once it has committed (drainMark) finds the paragraph moved no record, so that
		// appends nothing.
		Name: "docs/block_index.go#causeRoomFacts, a settlement of an issue's room", Database: r.database,
		Sites: []string{"docs/block_index.go#causeRoomFacts", "refs/blocks.go#Queue", "docs/mutation.go#WriteVersionNode",
			"docs/query_marks.go#drainMark"},
		Ready: func(t *testing.T) {
			created := decoded[struct {
				Key     string `json:"key"`
				Primary string `json:"primary_artifact_id"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What the room settles", "spec": "# Room\n\nThe room holds the spec.\n", "force": true,
			}, "alice", http.StatusCreated))
			roomIssue, roomSpec = created.Key, created.Primary
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the live edit of %s's spec: %v", roomIssue, err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := r.documents.Join(ctx, tx)
			defer ledger.Discard()
			if edit, err := r.documents.ApplyOps(joined, roomSpec, []model.EditOp{{
				Op: "insert", After: "end", Markdown: "A paragraph the room settles.",
			}}, alice, nil); err != nil || !edit.Changed {
				t.Fatalf("edit the spec of %s live (changed %v): %v", roomIssue, edit.Changed, err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the live edit of %s's spec: %v", roomIssue, err)
			}
		},
		Drive: func(t *testing.T) {
			if err := r.documents.SettleIssueDocuments(ctx, roomIssue); err != nil {
				t.Fatalf("settle the room of %s: %v", roomIssue, err)
			}
			r.awaitTrue(t, "the settlement writing version 2 of "+roomIssue+"'s spec", `
				select exists (select 1 from artifact_versions where artifact_id = $1::uuid and number = 2)
			`, roomSpec)
		},
		Counts: merged(newBlockCounted, blockValuesCounted, roomBlockCounted, versionCounted, queryVersionCounted),
	}, {
		// R2's daemon-thread step (docs.DaemonThreadStep) over the paragraph of the spec of an issue
		// of its own, born converted, which a parent pointer an agent's session wrote before R2's
		// rule placed under alice's reply to a message the Legion daemon's session posted: the step
		// retracts that part_of and states one to the issue's node (queueDaemonThreadMoves), which
		// the term block counts as it counts a node move's, and marks the issue's queries owed. The
		// drain it runs once its batch has committed (drainMark) finds that the move of a
		// paragraph, which no query takes, moved no record, so that appends nothing.
		Name: "docs/daemon_thread_step.go#queueDaemonThreadMoves, a paragraph under a daemon thread", Database: r.database,
		Sites: []string{"docs/daemon_thread_step.go#queueDaemonThreadMoves", "docs/query_marks.go#drainMark"},
		Ready: func(t *testing.T) {
			daemonIssue = decoded[model.Issue](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What the daemon's thread holds", "spec": "A paragraph the daemon's thread holds.\n", "force": true,
			}, "alice", http.StatusCreated)).Key
			messages := "/api/v1/issues/" + daemonIssue + "/messages"
			root := decoded[model.Message](t, r.request(t, http.MethodPost, messages, map[string]any{
				"body": "READY: the issue is claimed.", "actor": sessionActor(sizedDaemonSession),
			}, "", http.StatusCreated)).ID
			reply := decoded[model.Message](t, r.request(t, http.MethodPost, messages, map[string]any{
				"body": "Hold it until Friday.", "in_reply_to": root,
			}, "alice", http.StatusCreated)).ID
			r.placeUnderMessage(t, r.blockNode(t, daemonIssue, "paragraph").ID, reply)
		},
		Drive: func(t *testing.T) {
			moved, err := docs.NewDaemonThreadStep(r.documents, nil).Run(ctx)
			if err != nil {
				t.Fatalf("run the daemon-thread step: %v", err)
			}
			if moved.Moved != 1 || moved.GivenUp != 0 {
				t.Fatalf("the daemon-thread step moved %d nodes and gave %d up, want it to move the paragraph of %s alone",
					moved.Moved, moved.GivenUp, daemonIssue)
			}
		},
		Counts: map[graphcensus.StoredKind][]string{{refs.BlockNodeKind, graphstore.TuplePartOf}: {"block"}},
	}, {
		// R6's conversion of an issue into its room (docs.Service.ConvertIssue, under the issue
		// room step's gate) of an issue of its own created while a process that lists no issue room
		// was live, so its spec is a room of its own and the issue holds no tree and no decisions
		// query: the spec holds a paragraph a comment of alice's is anchored on and an ask block,
		// whose ask a settlement opened. The conversion writes the tree's node and its part_of, and
		// the decisions query's node, its part_of and its query values, with the first versions of
		// the two, which the terms issue_room and query_node count; the spec made a query node, its
		// part_of stated again and its query values, which document_part_of and query_node count;
		// the spec's version in the room, a query version, which version and query_version count; a
		// moved_to from each block's old address to its node in the room, which block_move counts,
		// and the authors it carries to that node, which block_author counts; the ask's anchored_to
		// and part_of claimed at the tree, which ask_opened counts; the spec's old blocks stated
		// gone, which block_gone counts; the room's blocks with their values, which block,
		// block_values and block_text count; and the comment's anchored_in and part_of retracted at
		// the paragraph's old address and stated at its node in the room, which anchor_block counts.
		Name: "docs/issue_conversion.go#writeTree, the conversion of an issue holding an ask and an anchored comment", Database: r.database,
		Sites: []string{"docs/issue_conversion.go#writeTree", "docs/issue_conversion.go#writeQueries",
			"docs/issue_conversion.go#writeDecisionsQuery", "docs/issue_conversion.go#writeMoves", "docs/issue_conversion.go#moveAsks",
			"docs/issue_conversion.go#movePointers", "docs/issue_conversion.go#finish", "docs/mutation.go#WriteVersionNode",
			"refs/blocks.go#Queue", "graphstore/write.go#Write", "graphstore/write.go#Caused"},
		Ready: func(t *testing.T) {
			unconverted := r.holdIssuesUnconverted(t)
			created := decoded[struct {
				Key     string `json:"key"`
				Primary string `json:"primary_artifact_id"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What the conversion moves", "spec": "The paragraph the room takes.\n", "force": true,
			}, "alice", http.StatusCreated))
			converting, convertingSpec = created.Key, created.Primary
			comment := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+converting+"/comments", map[string]any{
				"body": "A remark on the paragraph the room takes.", "anchor": map[string]any{"artifact": convertingSpec, "quote": "paragraph the room takes"},
			}, "alice", http.StatusCreated))
			if comment.Anchor == nil || comment.Anchor.BlockID == nil {
				t.Fatalf("the anchored comment answers anchor %+v, want one naming its block", comment.Anchor)
			}
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+convertingSpec+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": ":::ask{#" + convertedAsk + "}\nWhich room holds the spec?\n:::"}},
			}, "alice", http.StatusOK)
			movedAsk = r.settledAsk(t, convertingSpec, convertedAsk)
			r.releaseIssuesUnconverted(t, unconverted)
			// The conversion defers an issue one of whose documents owes its block index, so a tick
			// runs through whatever the writes before it left owed first.
			if _, err := r.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
				t.Fatalf("run the block-index tick over what the writes of %s left owed: %v", converting, err)
			}
			var unconvertedIssue bool
			if err := r.database.Pool.QueryRow(ctx, `
				select not exists (select 1 from issue_trees where issue_key = $1)
				   and not exists (select 1 from artifacts a where a.issue_key = $1 and `+refs.DecisionsQuerySQL("a")+`)
			`, converting).Scan(&unconvertedIssue); err != nil || !unconvertedIssue {
				t.Fatalf("%s holds neither a tree nor a decisions query: %t (%v), want true: the conversion takes an issue a build before the issue room created",
					converting, unconvertedIssue, err)
			}
		},
		Drive: func(t *testing.T) {
			converted, err := r.documents.ConvertIssue(ctx, converting, docs.IssueRoomGate, false)
			if err != nil || converted.Outcome != docs.IssueConverted {
				t.Fatalf("the conversion of %s answered %+v, %v; want it converted", converting, converted, err)
			}
			r.awaitTrue(t, "the conversion moving ask "+movedAsk+" into the room of "+converting, `
				select exists (select 1 from asks k join issue_trees t on t.artifact_id = k.block_artifact_id
				               where k.id = $1::uuid and t.issue_key = $2)
			`, movedAsk, converting)
		},
		Counts: merged(issueRoomCounted, queryNodeCounted, documentPartOfCounted, versionCounted, queryVersionCounted,
			newBlockCounted, blockValuesCounted, roomBlockCounted, anchorBlockCounted, map[graphcensus.StoredKind][]string{
				{refs.BlockNodeKind, graphstore.AttributeState}:  {"block_gone"},
				{refs.BlockNodeKind, graphstore.TupleMovedTo}:    {"block_move"},
				{graphstore.KindAsk, graphstore.TupleAnchoredTo}: {"ask_opened"},
				{graphstore.KindAsk, graphstore.TuplePartOf}:     {"ask_opened"},
				{graphstore.KindAsk, graphstore.TupleAskedOn}:    {"ask_opened"},
			}),
	}, {
		// alice's delete of a trade-off lens of hers on the spec with one finding (DELETE
		// /api/v1/issues/{key}/nodes/{id}, queueNodeDelete), with the count its dry run answers
		// and the lens's placement_seq, a comment of hers anchored on the finding's text (R31's node
		// anchor) standing under the finding. The delete takes the lens and the finding: each
		// node's state gone and the retraction of every relation out of each, which the term
		// node_delete counts; and it orphans the comment's node anchor (docs.NodeAnchorOrphans.Queue),
		// retracting its anchored_in and, as it answers nothing, its part_of to the finding, which
		// the term anchor_block counts. The lens is on no document, so the delete writes no
		// document's version; the issue is born converted, so its batch marks the issue's queries
		// owed, and the drain the route runs once the delete has committed appends a version of the
		// query whose record the delete moved (drainMark): the version's node, a query version
		// with its definition_version.
		Name: "api/node_delete.go#queueNodeDelete, a lens with a finding a comment is anchored on", Database: r.database,
		Sites: []string{"api/node_delete.go#queueNodeDelete", "docs/anchor_refresh.go#Queue", "docs/query_marks.go#drainMark",
			"docs/mutation.go#WriteVersionNode"},
		Ready: func(t *testing.T) {
			invoked := decoded[lensInvoked](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
				"on": "dispatch://" + r.issue + "/spec", "kind": "trade-off", "findings": []string{"One delete takes the lens and its finding."},
			}, "alice", http.StatusCreated))
			if len(invoked.Findings) != 1 {
				t.Fatalf("the lens %s answers %d findings, want the one it was invoked with", invoked.ID, len(invoked.Findings))
			}
			deleted = graphstore.KindLens + "/" + invoked.ID
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments", map[string]any{
				"body": "Which delete takes it?", "anchor": map[string]any{"node": invoked.Findings[0].ID, "quote": "takes the lens"},
			}, "alice", http.StatusCreated)
			deletedCount = decoded[struct {
				Count int `json:"count"`
			}](t, r.request(t, http.MethodDelete, "/api/v1/issues/"+r.issue+"/nodes/"+url.PathEscape(deleted)+"?dry_run=true",
				nil, "alice", http.StatusOK)).Count
			held, err := graphstore.CurrentOut(ctx, r.database.Pool, graphstore.KindLens, invoked.ID, []string{graphstore.TuplePartOf})
			if err != nil || len(held) != 1 {
				t.Fatalf("the lens %s stands on %v (%v), want one parent", invoked.ID, held, err)
			}
			deletedPlacement = `"` + strconv.FormatInt(held[0].Seq, 10) + `"`
		},
		Drive: func(t *testing.T) {
			r.requestWith(t, map[string]string{"If-Match": deletedPlacement}, http.MethodDelete,
				"/api/v1/issues/"+r.issue+"/nodes/"+url.PathEscape(deleted)+"?expected_count="+strconv.Itoa(deletedCount),
				nil, "alice", http.StatusOK)
		},
		Counts: merged(anchorBlockCounted, versionCounted, queryVersionCounted, map[graphcensus.StoredKind][]string{
			{graphstore.KindLens, graphstore.AttributeState}:  {"node_delete"},
			{graphstore.KindLens, graphstore.TuplePartOf}:     {"node_delete"},
			{graphstore.KindLens, graphstore.TupleHasKind}:    {"node_delete"},
			{graphstore.KindLens, graphstore.TupleAuthoredBy}: {"node_delete"},
			{refs.BlockNodeKind, graphstore.AttributeState}:   {"node_delete"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:      {"node_delete"},
			{refs.BlockNodeKind, graphstore.TupleAuthoredBy}:  {"node_delete"},
		}),
	}}
}

// sizedCheck is the check of the issue's Verification document the result drive records a result
// of.
const sizedCheck = "sized-check"

// sizedDaemonSession is the session the Legion daemon posts the issue's status messages of
// project SIZE under (refs' daemonSessionPrefix), which begins the thread the daemon-thread
// drive's paragraph is placed in.
const sizedDaemonSession = "legion-daemon:SIZE"

// convertedAsk is the id of the ask block the conversion drive's spec holds, whose ask the
// conversion moves into the issue's room.
const convertedAsk = "converted-ask"

// anchorBlockCounted is a comment's anchored_in its anchor's block and, since it answers nothing,
// its part_of to that same block (graphstore.AnchorTuples), stated or retracted, which the term
// anchor_block counts.
var anchorBlockCounted = map[graphcensus.StoredKind][]string{
	{graphstore.KindComment, graphstore.TupleAnchoredIn}: {"anchor_block"},
	{graphstore.KindComment, graphstore.TuplePartOf}:     {"anchor_block"},
}

// changedByRunCounted is what a write made for a run appends out of the node of kind it changed:
// its changed_by_run into the run, which the term interaction counts (store.WithRun).
func changedByRunCounted(kind string) map[graphcensus.StoredKind][]string {
	return map[graphcensus.StoredKind][]string{{kind, graphstore.TupleChangedByRun}: {graphstore.KindInteraction}}
}

// resultCounted is what a person's record of a result appends of the result's node
// (graphstore.ResultFacts): its base attributes, its values, its part_of and its result_of its
// check, and its authored_by, each counted by the term result.
func resultCounted() map[graphcensus.StoredKind][]string {
	counted := map[graphcensus.StoredKind][]string{}
	for _, kind := range []string{graphstore.AttributeVisibility, graphstore.AttributeState, graphstore.AttributeProject,
		graphstore.AttributeVerdict, graphstore.AttributeSurface, graphstore.AttributeRun, graphstore.AttributeObserved,
		graphstore.AttributeCommit, graphstore.AttributeRole, graphstore.AttributeFailed, graphstore.AttributeSkipped,
		graphstore.AttributeExcluded, graphstore.AttributeNegativeControl, graphstore.AttributeCheckVersion,
		graphstore.AttributeCheckSHA256, graphstore.TuplePartOf, graphstore.TupleResultOf, graphstore.TupleAuthoredBy} {
		counted[graphcensus.StoredKind{From: graphstore.KindResult, Kind: kind}] = []string{"result"}
	}
	return counted
}

// lensInvoked is the lens POST /api/v1/issues/{key}/lenses answers, by the id its address names,
// with each finding it was written with, by its store address.
type lensInvoked struct {
	ID       string `json:"id"`
	Findings []struct {
		ID string `json:"id"`
	} `json:"findings"`
}

// nodePath is the route path of one of a node's own routes, its store address one escaped segment.
func nodePath(address, route string) string {
	return "/nodes/" + url.PathEscape(address) + "/" + route
}

// uploaded uploads, as alice, a document of the rig's issue named name holding content, and
// answers its id: a new document's create, or a new version of the one of that name.
func (r *writersRig) uploaded(t *testing.T, name, content string) string {
	t.Helper()
	return decoded[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts",
		map[string]any{"name": name, "content": content}, "alice", http.StatusCreated)).Artifact.ID
}

// suggestion writes, as alice, a suggestion on the rig's issue anchored on quote in document, and
// answers it, failing unless its anchor names the block holding the quote.
func (r *writersRig) suggestion(t *testing.T, document, quote string) model.Comment {
	t.Helper()
	comment := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments", map[string]any{
		"body": "Other words.", "anchor": map[string]any{"artifact": document, "quote": quote},
		"suggestion": map[string]string{"replace_with": "Other words"},
	}, "alice", http.StatusCreated))
	if comment.Anchor == nil || comment.Anchor.BlockID == nil {
		t.Fatalf("the suggestion on %q answers anchor %+v, want one naming its block", quote, comment.Anchor)
	}
	return comment
}

// anchoredBlock is the address of block of document as the block index addresses it: the ref_key
// of the room the document is edited in, '#' and the block id.
func (r *writersRig) anchoredBlock(t *testing.T, document, block string) string {
	t.Helper()
	var address string
	if err := r.database.Pool.QueryRow(context.Background(),
		`select `+graphstore.RoomRefKeySQL("$1::uuid")+` || '#' || $2::text`, document, block).Scan(&address); err != nil {
		t.Fatalf("read the address of block %s of %s: %v", block, document, err)
	}
	return address
}

// writeAnchoredIn states, or retracts where asserted is false, the anchored_in of comment to block
// and, as the comment answers nothing, its part_of to the same block (graphstore.AnchorTuples),
// through the store's writer, as a build that wrote the comment's anchor row without them left the
// store: what a drive's Ready stands up for the writer it drives to find.
func (r *writersRig) writeAnchoredIn(t *testing.T, comment, block string, asserted bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the anchored_in's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, graphstore.AnchorTuples(nil, comment, refs.BlockNodeKind, block, true, asserted,
		model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent)); err != nil {
		t.Fatalf("write the anchored_in and the part_of of comment %s: %v", comment, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the anchored_in's write: %v", err)
	}
}

// runWindowAhead moves the run's window an hour ahead, so a write made for it is accepted however
// long the drives before it took: a run's window is contracts.GoingRunWindow from its record.
func (r *writersRig) runWindowAhead(t *testing.T, run string) {
	t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(),
		`update going_requests set ends_at = now() + interval '1 hour' where id = $1::uuid`, run); err != nil {
		t.Fatalf("move run %s's window ahead: %v", run, err)
	}
}

// holdIssuesUnconverted makes a server process that lists no issue room live beside the rig's
// server - a server_processes row with no capabilities, as a build before the column registers
// none - so an issue created while it stands is created as before the issue room, one room per
// document, and answers its instance. It registers this build's schema and owner lock protocol,
// so it moves no other rule the fleet decides: the ask roll window stays shut and the owner locks
// stay in this build's mode.
func (r *writersRig) holdIssuesUnconverted(t *testing.T) string {
	t.Helper()
	var instance string
	if err := r.database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), '{fingerprint}', $1, $2, now() + interval '1 day', $3)
		returning instance::text`,
		pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("add a live server process that lists no issue room: %v", err)
	}
	return instance
}

// releaseIssuesUnconverted takes away the process holdIssuesUnconverted made live, so no live
// process lacks the issue room, which a conversion's gate (docs.IssueRoomGate) requires.
func (r *writersRig) releaseIssuesUnconverted(t *testing.T, instance string) {
	t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), `delete from server_processes where instance = $1::uuid`, instance); err != nil {
		t.Fatalf("take back the live server process that lists no issue room: %v", err)
	}
}

// placeUnderMessage moves the block node under the message, through the store's writer, as a
// parent pointer an agent's session wrote before R2's rule of the Legion daemon placed it: the
// retraction of its part_of and one to the message, dated by the database's clock.
func (r *writersRig) placeUnderMessage(t *testing.T, node, message string) {
	t.Helper()
	ctx := context.Background()
	held, err := graphstore.CurrentOut(ctx, r.database.Pool, refs.BlockNodeKind, node, []string{graphstore.TuplePartOf})
	if err != nil || len(held) != 1 {
		t.Fatalf("the block %s stands on %v (%v), want one parent", node, held, err)
	}
	retracted := held[0]
	if err := r.database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&retracted.At); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	retracted.Asserted, retracted.Who, retracted.SourceEvent = false, model.Actor{Kind: "session", ID: "ses_placer"}, graphstore.NoEvent
	placed := retracted
	placed.ToKind, placed.ToID, placed.Asserted = string(refs.ItemMessage), message, true
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the placement of %s under the message %s: %v", node, message, err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{retracted, placed}); err != nil {
		t.Fatalf("place %s under the message %s: %v", node, message, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the placement of %s under the message %s: %v", node, message, err)
	}
}
