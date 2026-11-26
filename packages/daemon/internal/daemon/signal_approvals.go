package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// retryApprovalsBootRead reads Dispatch for the approvals signal (readApprovalsOnce) until a read
// succeeds or ctx ends (retryBootRead), so the asks the boot could not read are not left unread
// until the next boot: the approvals signal reads as not recorded, with the reason of the last
// failed read, until one succeeds (intake.ApprovalsBootRead).
func (w *workflowRuntime) retryApprovalsBootRead(ctx context.Context, bootID int64, first, most time.Duration) {
	retryBootRead(ctx, bootID, first, most, w.readApprovalsOnce)
}

// readApprovalsOnce is one read of what the approvals signal needs as it stands in Dispatch, which
// id names (the boot and its read), outside the fact lock, applied as one intake.ApprovalsBootRead:
//
//   - the project's issues, listed once (dispatch.ApprovalReads.ListIssues): each issue's assignee at the
//     sequence its listing stands at, so an ask opened later on an issue nobody has changed since
//     is read against the assignee it has;
//   - the project's open asks of issues and its default assignee, read in one statement of
//     Dispatch's (OpenAsks), which finds the issues with an ask open, children and issues the
//     workflow records nothing of among them;
//   - the open asks of each such issue and of each issue the store holds an ask open of
//     (IssueAsks), each with the person it was handed to and whose turn it is as Dispatch computes
//     them, at the sequence the issue's listing stood at, which is never above the sequence the
//     read of its asks stands at. An ask the store holds open that its issue no longer lists open
//     closed while the daemon was down, and is kept closed at that sequence.
//
// The events of an ask the stream delivers after, or while, the read ran are kept by their own
// sequence in the issue's log (record.Store.KeepAskEvent), and the read is kept where no event past
// its sequence was (record.Store.KeepApprovalsRead): an event that landed during the read stands,
// one older than the read changes nothing, and one newer is applied over it. The default assignee
// is kept where no settings event was kept since the read began, the sequence the store held then
// being read first; a Dispatch that answers none leaves the one kept, or none, which keeps the
// signal unrecorded.
//
// It says whether every read succeeded. A failed one applies its reason alone, which keeps the
// approvals signal unrecorded rather than reading a count from a partial read; a read that
// succeeded clears it.
func (w *workflowRuntime) readApprovalsOnce(ctx context.Context, id string) bool {
	eventID := fmt.Sprintf("boot-approvals:%s:%s", w.dispatchProject, id)
	failed := func(why string, err error) bool {
		if ctx.Err() != nil {
			return false
		}
		w.log.Warn("legion daemon could not read Dispatch's open asks; the approvals signal is not recorded until it can", "read", id, "why", why, "error", err)
		fact := intake.ApprovalsBootRead{ReadError: fmt.Sprintf("%s: %v", why, err)}
		if _, err := intake.ApplyFact(ctx, w.pool, "daemon", eventID, fact, w.handlers...); err != nil && ctx.Err() == nil {
			w.log.Warn("legion daemon could not record the outcome of its read of Dispatch's open asks", "read", id, "error", err)
		}
		return false
	}
	var kept []record.ApprovalAsk
	var defaultSeq int64
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if kept, err = w.records.OpenApprovalAsks(ctx, tx, w.dispatchProject); err != nil {
			return err
		}
		defaultSeq, err = w.records.DefaultAssigneeSeq(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		return failed("read the asks the store holds open", err)
	}
	bounded, cancel := context.WithTimeout(ctx, issueRead)
	listed, err := w.approvals.ListIssues(bounded, w.dispatchProject, nil)
	cancel()
	if err != nil {
		return failed(fmt.Sprintf("list the issues of %s", w.dispatchProject), err)
	}
	bounded, cancel = context.WithTimeout(ctx, issueRead)
	open, err := w.approvals.OpenAsks(bounded, w.dispatchProject)
	cancel()
	if err != nil {
		return failed(fmt.Sprintf("read the open asks of %s", w.dispatchProject), err)
	}

	read := record.ApprovalsRead{DefaultAssignee: open.DefaultAssignee, DefaultAt: open.AsOf, DefaultSeq: defaultSeq}
	seqs := make(map[string]int64, len(listed))
	for _, issue := range listed {
		seqs[issue.Key] = issue.LastSeq
		if issue.AssigneeKnown && issue.LastSeq >= 1 {
			read.Assignees = append(read.Assignees, record.IssueAssignee{Issue: issue.Key, Assignee: issue.Assignee, Seq: issue.LastSeq})
		}
	}
	keptOf := make(map[string][]record.ApprovalAsk)
	for _, ask := range kept {
		keptOf[ask.Issue] = append(keptOf[ask.Issue], ask)
	}
	toRead := make(map[string]bool, len(keptOf)+len(open.Asks))
	for key := range keptOf {
		toRead[key] = true
	}
	for _, ask := range open.Asks {
		if claim.KeyOfProject(w.dispatchProject, ask.Issue) {
			toRead[ask.Issue] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(toRead)) {
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		asks, err := w.readIssueAsks(bounded, key, seqs, keptOf[key], &read)
		cancel()
		if err != nil {
			return failed(fmt.Sprintf("read the open asks of %s", key), err)
		}
		read.Asks = append(read.Asks, asks...)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "daemon", eventID, intake.ApprovalsBootRead{Read: read}, w.handlers...); err != nil {
		return failed("keep what the read of Dispatch's open asks gave", err)
	}
	return ctx.Err() == nil
}

// readIssueAsks reads the open asks of issue key and answers them as the asks the approvals signal
// keeps, each at the sequence the issue's listing stood at (seqs; the issue's own read where the
// listing does not name it, an issue made since, whose sequence and assignee are added to read),
// beside each ask in kept, the asks the store holds open of the issue, that the read no longer
// lists open, kept closed. An issue Dispatch has none of (404) holds no ask open, so each kept ask
// of it closes, at the sequence it was kept at.
func (w *workflowRuntime) readIssueAsks(ctx context.Context, key string, seqs map[string]int64, kept []record.ApprovalAsk, read *record.ApprovalsRead) ([]record.ApprovalAsk, error) {
	seq, listed := seqs[key]
	if !listed {
		issue, err := w.approvals.GetIssue(ctx, key)
		switch {
		case dispatch.Missing(err):
			return closedAsks(kept, nil, 0), nil
		case err != nil:
			return nil, err
		}
		seq = issue.LastSeq
		if issue.AssigneeKnown && seq >= 1 {
			read.Assignees = append(read.Assignees, record.IssueAssignee{Issue: key, Assignee: issue.Assignee, Seq: seq})
		}
	}
	if seq < 1 {
		return nil, fmt.Errorf("Dispatch gives the issue %s no event sequence", key)
	}
	open, err := w.approvals.IssueAsks(ctx, key)
	if err != nil {
		if dispatch.Missing(err) {
			return closedAsks(kept, nil, 0), nil
		}
		return nil, err
	}
	asks := make([]record.ApprovalAsk, 0, len(open)+len(kept))
	stillOpen := make(map[string]bool, len(open))
	for _, ask := range open {
		stillOpen[ask.ID] = true
		asks = append(asks, record.ApprovalAsk{Ask: signals.Ask{ID: ask.ID, Issue: key, Open: true, Human: ask.WaitsOnPerson, AskedOf: ask.AskedOf}, Seq: seq})
	}
	return append(asks, closedAsks(kept, stillOpen, seq)...), nil
}

// closedAsks is each of kept that is not in stillOpen, closed, at seq, or at the sequence it was
// kept at where seq is 0: an ask the store holds open that Dispatch no longer lists open.
func closedAsks(kept []record.ApprovalAsk, stillOpen map[string]bool, seq int64) []record.ApprovalAsk {
	var closed []record.ApprovalAsk
	for _, ask := range kept {
		if stillOpen[ask.ID] {
			continue
		}
		ask.Open = false
		if seq > 0 {
			ask.Seq = seq
		}
		closed = append(closed, ask)
	}
	return closed
}
