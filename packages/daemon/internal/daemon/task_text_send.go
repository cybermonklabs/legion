package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// taskText answers supervise's Deps.TaskText: a phase worker's held task with the text of its
// task's block as the plan in force gives it now, at the moment the task is about to be sent. A
// later version of the plan can change only the text of a running task (plan.RunningChange), and
// the claim of the step that runs then holds the start task it was given: waiting for its send, or
// taken back from a turn its process could not finish and sent again to the relaunched agent, a
// fresh session among them. That send carries the text the plan holds now, by the one rewrite of a
// start's task for a later text (workflow.WithTaskText). It is the only place a start task takes a
// later text: the version writes no stored start task, and the send of a stored one (an owed
// entry's, record.PhaseRow.EntryTask) is a start the claim's machine sends through here as well.
//
// A task that carries news of the approved design is sent as its start's own text
// (record.DeliveryNews.Task) followed by the news, rendered again at each send (designNews), so the
// rewrite is made in that own text, and the news it carries is answered with it; the text the
// send renders then holds the new text. A task whose own text holds no owner's part is answered as
// it is, with no read of the record: the task of an issue with no plan rows, of the merge, and an
// operator's own. So is a task of a run the issue has left, or of an issue the record does not
// hold.
//
// It is the machine that asks, because the machine owns the pending delivery, in a short read-only
// transaction of its own, as rootHears and designNews are asked. The issue and its plan are
// separate reads, so the transaction reads one snapshot (repeatable read): a version taken between
// them is read whole or not at all.
func (w *workflowRuntime) taskText(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, error) {
	own := d.Task
	var carried record.DeliveryNews
	if len(d.News) > 0 {
		if err := json.Unmarshal(d.News, &carried); err != nil {
			return "", nil, fmt.Errorf("decode the news the task held for %s carries: %w", c.Token, err)
		}
		own = carried.Task
	}
	if !workflow.HoldsTaskText(own) {
		return d.Task, d.News, nil
	}
	var text string
	var found bool
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := w.records.Issue(ctx, tx, c.Issue)
		if err != nil || issue == nil || issue.Generation != d.Generation {
			return err
		}
		stored, err := w.records.Plan(ctx, tx, issue.Key, issue.Generation)
		if err != nil || stored == nil {
			return err
		}
		task, err := stored.Task()
		if err != nil {
			return err
		}
		text, found = task.BlockText, true
		return nil
	}); err != nil {
		return "", nil, fmt.Errorf("read the text of the task of %s's held task: %w", c.Token, err)
	}
	if !found {
		return d.Task, d.News, nil
	}
	rewritten := workflow.WithTaskText(own, text)
	if len(d.News) == 0 {
		return rewritten, d.News, nil
	}
	if rewritten == carried.Task {
		return d.Task, d.News, nil
	}
	carried.Task = rewritten
	news, err := json.Marshal(carried)
	if err != nil {
		return "", nil, fmt.Errorf("encode the news the task held for %s carries: %w", c.Token, err)
	}
	return d.Task, news, nil
}
