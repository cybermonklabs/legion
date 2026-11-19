package graphcensus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/grapherase"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// externalDrives are the drives of the sites of api, docs and grapherase the register of the
// store's writers lists and no drive before them runs, each the real site reached as its own
// lock-step driver reaches it, over the rig the drives before them wrote (importedDrives). They
// run in this order: alice's run of the command bar and its one frame; her writes made for that
// run - a PATCH of the issue's labels, an edit of an ask's words and one of a comment's; a PATCH
// of the issue's needs and of its filed_from at a comment the release before wrote; a lens on
// the spec with a finding, a second lens's triggered_by of it, and the first lens stated
// overtaken; a result of a check of the issue's Verification document and its retraction; an
// accept that finds a suggestion's mark gone; an upload that takes a suggestion's quote out of
// its document; the anchor block backfill of a comment written with no block; a settlement of an
// issue's room; and the erase of a block a version held.
func externalDrives(t *testing.T, r *writersRig) []graphcensus.StoreDrive {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	// What a drive or its Ready writes for itself and the drives after it: alice's run, the ask and
	// the comment her run edits, the issue the PATCH waits on and the comment it is filed from, the
	// first lens and the second, the result, the suggestion the accept finds orphaned and the one
	// the refresh orphans, the comment the backfill pins, the issue whose room a settlement settles
	// with its spec, and the document of the block the erase takes, with that block.
	var run, ask, edited, target, filedFrom, lens, chained, result, orphaned, refreshed, pinned string
	var roomIssue, roomSpec, erasedDocument, erased string
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
			{graphstore.KindInteraction, graphstore.TupleAuthoredBy}:     {graphstore.KindInteraction},
			{graphstore.KindInteraction, graphstore.AttributeFrame}:      {"frame"},
		},
	}, {
		// A PATCH of the issue's labels made for alice's run (patchIssue), which restates the
		// issue's node marked changed by the run: its changed_by_run, which the term interaction
		// counts into the run.
		Name: "api/issue_patch.go#patchIssue, under a run", Database: r.database,
		Sites: []string{"api/issue_patch.go#patchIssue"},
		Ready: func(t *testing.T) { r.runWindowAhead(t, run) },
		Drive: func(t *testing.T) {
			r.requestForRun(t, http.MethodPatch, "/api/v1/issues/"+r.issue, map[string]any{"labels": []string{"sized"}}, "alice", run, http.StatusOK)
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
			r.requestForRun(t, http.MethodPatch, "/api/v1/asks/"+ask,
				map[string]any{"question": "Which words did the run change?"}, "alice", run, http.StatusOK)
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
			r.requestForRun(t, http.MethodPatch, "/api/v1/comments/"+edited,
				map[string]any{"body": "A remark the run reworded."}, "alice", run, http.StatusOK)
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
			"api/issue_patch.go#patchIssue"},
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
		// with its base attributes, its part_of the spec, its has_kind, its verdict and her
		// authored_by, which the term lens counts, and the finding's block node with its base
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
		// person's result names no run_session, and her node stands from the issue's spec.
		Name: "api/results.go#writeRecordedResult", Database: r.database,
		Sites: []string{"api/results.go#writeRecordedResult"},
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
		Counts: resultCounted(),
	}, {
		// alice's retraction of that result (writeCheckResultRetraction): its retracted value,
		// which the term result_retraction counts.
		Name: "api/results_retract.go#writeCheckResultRetraction", Database: r.database,
		Sites: []string{"api/results_retract.go#writeCheckResultRetraction"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/results/"+result+"/retract",
				map[string]any{"reason": "The run measured the wrong build."}, "alice", http.StatusCreated)
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindResult, graphstore.AttributeRetracted}: {"result_retraction"}},
	}, {
		// An accept of a suggestion whose mark is gone from a row the refresh did not reach
		// (orphanCommentAnchor): the suggestion is anchored on a paragraph of a document of its
		// own, the paragraph is deleted, and the suggestion's anchor and its anchored_in are stood
		// up again as such a row stands, so the accept is what orphans it and retracts its
		// anchored_in, which the term anchor_block counts.
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
		// retracts its anchored_in under its comment.anchor_refreshed (stateRefreshedAnchoredIn),
		// beside the version's own node, the new paragraph's block with its values, and the
		// suggested paragraph's block stated gone.
		Name: "docs/anchor_refresh.go#stateRefreshedAnchoredIn, an upload that takes a suggestion's quote out", Database: r.database,
		Sites: []string{"docs/anchor_refresh.go#stateRefreshedAnchoredIn", "api/artifacts.go#storeArtifact",
			"refs/blocks.go#Queue", "docs/mutation.go#WriteVersionNode"},
		Ready: func(t *testing.T) {
			document := r.uploaded(t, "refreshed.md", "# Refreshed\n\nThe moon is up.\n")
			refreshed = r.suggestion(t, document, "moon is up").ID
		},
		Drive: func(t *testing.T) {
			r.uploaded(t, "refreshed.md", "# Refreshed\n\nStars come out.\n")
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
		// and states the anchored_in (pinCommentAnchorBlock), which the term anchor_block counts of
		// the comments the window holds.
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
		// room records its authors, and the version's node.
		Name: "docs/block_index.go#causeRoomFacts, a settlement of an issue's room", Database: r.database,
		Sites: []string{"docs/block_index.go#causeRoomFacts", "refs/blocks.go#Queue", "docs/mutation.go#WriteVersionNode"},
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
		Counts: merged(newBlockCounted, blockValuesCounted, roomBlockCounted, versionCounted),
	}, {
		// The erase of a paragraph inside a callout of a document of its own (grapherase.Erase):
		// the one version that held it goes, its node stated gone (endErasedVersions), which the
		// term erased_version counts, and the callout's text value, which quotes it, is rewritten in
		// place (redactBlocks), which appends nothing.
		Name: "grapherase/erase.go#endErasedVersions, the erase of a block a version held", Database: r.database,
		Sites: []string{"grapherase/erase.go#endErasedVersions", "grapherase/redaction.go#redactBlocks", "grapherase/erase.go#apply"},
		Ready: func(t *testing.T) {
			erasedDocument = r.uploaded(t, "erased.md", ":::callout{#holder}\nWords the erase takes.\n:::\n")
			if err := r.database.Pool.QueryRow(ctx, `
				select node_id from document_blocks where artifact_id = $1::uuid and type = 'paragraph'
			`, erasedDocument).Scan(&erased); err != nil {
				t.Fatalf("read the node of the paragraph of erased.md: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			url := r.database.Pool.Config().ConnString()
			listing, err := grapherase.Erase(ctx, url, "block", erased, "", 10*time.Millisecond)
			if err != nil {
				t.Fatalf("list the erase of block %s: %v", erased, err)
			}
			if len(listing.Blockers) > 0 {
				t.Fatalf("the erase of block %s is blocked by %v", erased, listing.Blockers)
			}
			erasure, err := grapherase.Erase(ctx, url, "block", erased, listing.Digest, 10*time.Millisecond)
			if err != nil || !erasure.Applied || len(erasure.Versions) != 1 {
				t.Fatalf("erase block %s (applied %v, versions %+v): %v", erased, erasure != nil && erasure.Applied, erasure, err)
			}
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindVersion, graphstore.AttributeState}: {"erased_version"}},
	}}
}

// sizedCheck is the check of the issue's Verification document the result drive records a result
// of.
const sizedCheck = "sized-check"

// anchorBlockCounted is a comment's anchored_in its anchor's block, stated or retracted, which the
// term anchor_block counts.
var anchorBlockCounted = map[graphcensus.StoredKind][]string{
	{graphstore.KindComment, graphstore.TupleAnchoredIn}: {"anchor_block"},
}

// changedByRunCounted is what a write made for a run appends out of the node of kind it changed:
// its changed_by_run into the run, which the term interaction counts (graphstore.WithRun).
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

// lensInvoked is the lens POST /api/v1/issues/{key}/lenses answers, by the id its address names.
type lensInvoked struct {
	ID string `json:"id"`
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
// through the store's writer, as a build that wrote the comment's anchor row without it left the
// store: what a drive's Ready stands up for the writer it drives to find.
func (r *writersRig) writeAnchoredIn(t *testing.T, comment, block string, asserted bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the anchored_in's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{
		graphstore.AnchoredIn(comment, block, asserted, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent),
	}); err != nil {
		t.Fatalf("write the anchored_in of comment %s: %v", comment, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the anchored_in's write: %v", err)
	}
}

// requestForRun sends one request as the person login names made for the run of the command bar
// run (contracts.GoingRunHeader), failing the test unless the answer is want, and answers the body.
func (r *writersRig) requestForRun(t *testing.T, method, target string, body any, login, run string, want int) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode %s %s: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	request.Header.Set(contracts.GoingRunHeader, run)
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s for run %s: status=%d body=%s, want %d", method, target, run, response.Code, response.Body.String(), want)
	}
	return response.Body.Bytes()
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
