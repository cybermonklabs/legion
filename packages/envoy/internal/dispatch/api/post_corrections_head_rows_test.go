package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// requirePostHeadsKept holds the store after step, a write of a comment or a message on the issue
// key: the write left the issue's head mark standing (issue_head_marks), so the census below
// compares the issue's rows at all, and the census kind issue_head_node (graphcensus.CensusOfHeads,
// census_head.go) reads no difference, each difference it reports named by where it stands and
// why. A write that took the mark away would leave nothing of the issue to compare.
func requirePostHeadsKept(t *testing.T, rig correctionRunRig, step string) {
	t.Helper()
	ctx := context.Background()
	var marked bool
	if err := rig.database.Pool.QueryRow(ctx, `select exists (select 1 from issue_head_marks where issue_key = $1)`, rig.issue).Scan(&marked); err != nil {
		t.Fatalf("read the head mark of %s after %s: %v", rig.issue, step, err)
	}
	if !marked {
		t.Fatalf("%s took the head mark of %s away, so the census compares none of its rows", step, rig.issue)
	}
	var kind graphcensus.KindReport
	if err := pgx.BeginFunc(ctx, rig.database.Pool, func(tx pgx.Tx) error {
		var err error
		kind, err = graphcensus.CensusOfHeads(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("take the census kind issue_head_node after %s: %v", step, err)
	}
	t.Logf("after %s the census kind issue_head_node reads %d rows against %d nodes, %d row and %d node difference(s)",
		step, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
	for _, found := range append(kind.ReportedRows, kind.ReportedNodes...) {
		t.Errorf("after %s the census kind issue_head_node reads a difference at %s: %s", step, found.At, found.Cause)
	}
	if reported, differences := int64(len(kind.ReportedRows)+len(kind.ReportedNodes)), kind.RowDifferences+kind.NodeDifferences; differences > reported {
		t.Errorf("after %s the census kind issue_head_node reads %d difference(s), %d of them past the ones named above", step, differences, differences-reported)
	}
}

// Main's #3512 lets the author of a comment or of an issue message correct it and delete it, and
// 4db4676d has each correction or delete made for a run of the command bar name the post changed by
// that run. The head rows of an issue's tree hold what a comment, each of its current blocks and a
// message's node hold (graphstore.HeadRowsSQL: a comment's body, its times and its delete mark, a
// block's position and text, a node's changed_by_run), so every one of those writers keeps the
// issue's head rows in its own transaction. This drives each of them through its route on one issue
// whose head rows are marked - a comment and a message created, the comment edited and the message
// corrected, each again for alice's run, then a comment and a message deleted, and each again for
// the run - and after each write holds the issue to its head mark and the census kind
// issue_head_node to no difference (requirePostHeadsKept).
func TestEveryPostCorrectionKeepsTheIssuesHeadRows(t *testing.T) {
	t.Parallel()
	rig := newCorrectionRunRig(t, "HPC")
	requirePostHeadsKept(t, rig, "the issue's create")

	comment := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "A remark alice corrects.\n\nIts second paragraph."}, "alice")
	requirePostHeadsKept(t, rig, "the create of comment "+comment.ID)
	message := createIssueMessage(t, rig.handler, rig.issue, map[string]any{"body": "A message alice corrects.\n\nIts second paragraph."}, "alice")
	requirePostHeadsKept(t, rig, "the create of message "+message.ID)

	editThreadComment(t, rig.handler, comment.ID, "A remark alice corrected.\n\nIts second paragraph.\n\nA third one.", "alice")
	requirePostHeadsKept(t, rig, "the edit of comment "+comment.ID)
	editIssueMessage(t, rig.handler, message.ID, "A message alice corrected.\n\nIts second paragraph, reworded.", "alice")
	requirePostHeadsKept(t, rig, "the correction of message "+message.ID)

	if response := requestForRun(t, rig.handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"body": "A remark alice corrected for her run."}, "alice", rig.run); response.Code != http.StatusOK {
		t.Fatalf("edit comment %s for run %s: status=%d body=%s", comment.ID, rig.run, response.Code, response.Body.String())
	}
	requirePostHeadsKept(t, rig, "the edit of comment "+comment.ID+" for run "+rig.run)
	if response := requestForRun(t, rig.handler, http.MethodPatch, "/api/v1/messages/"+message.ID,
		map[string]any{"body": "A message alice corrected for her run."}, "alice", rig.run); response.Code != http.StatusOK {
		t.Fatalf("correct message %s for run %s: status=%d body=%s", message.ID, rig.run, response.Code, response.Body.String())
	}
	requirePostHeadsKept(t, rig, "the correction of message "+message.ID+" for run "+rig.run)

	if deleted := deleteThreadComment(t, rig.handler, comment.ID, "alice"); deleted.DeletedAt == nil {
		t.Fatalf("the delete of comment %s answered no delete mark: %#v", comment.ID, deleted)
	}
	requirePostHeadsKept(t, rig, "the delete of comment "+comment.ID)
	if deleted := deleteIssueMessage(t, rig.handler, message.ID, "alice"); deleted.DeletedAt == nil {
		t.Fatalf("the delete of message %s answered no delete mark: %#v", message.ID, deleted)
	}
	requirePostHeadsKept(t, rig, "the delete of message "+message.ID)

	ranComment := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "A remark alice deletes for her run.\n\nIts second paragraph."}, "alice")
	ranMessage := createIssueMessage(t, rig.handler, rig.issue, map[string]any{"body": "A message alice deletes for her run."}, "alice")
	requirePostHeadsKept(t, rig, "the creates of comment "+ranComment.ID+" and message "+ranMessage.ID)
	response := requestForRun(t, rig.handler, http.MethodDelete, "/api/v1/comments/"+ranComment.ID, nil, "alice", rig.run)
	if response.Code != http.StatusOK || decodeBody[model.Comment](t, response).DeletedAt == nil {
		t.Fatalf("delete comment %s for run %s: status=%d body=%s", ranComment.ID, rig.run, response.Code, response.Body.String())
	}
	requirePostHeadsKept(t, rig, "the delete of comment "+ranComment.ID+" for run "+rig.run)
	response = requestForRun(t, rig.handler, http.MethodDelete, "/api/v1/messages/"+ranMessage.ID, nil, "alice", rig.run)
	if response.Code != http.StatusOK || decodeBody[model.Message](t, response).DeletedAt == nil {
		t.Fatalf("delete message %s for run %s: status=%d body=%s", ranMessage.ID, rig.run, response.Code, response.Body.String())
	}
	requirePostHeadsKept(t, rig, "the delete of message "+ranMessage.ID+" for run "+rig.run)
}
