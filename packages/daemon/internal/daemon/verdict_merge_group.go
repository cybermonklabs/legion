package daemon

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// mergeGroupHandler is the intake handler of a merge queue's group (intake.MergeGroup), and of
// nothing else: on checks_requested it asks, on the fact's commit, for a write of legion-verdict
// on the group's commit (queueMergeGroupWrite), which the writer makes outside the fact's
// transaction; on destroyed, and on any other action, it asks for nothing (spec R6).
type mergeGroupHandler struct{ w *workflowRuntime }

func (h mergeGroupHandler) Apply(ctx context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	group, ok := fact.(intake.MergeGroup)
	if !ok || !group.ChecksRequested() {
		return intake.Result{}, nil
	}
	repository, err := ghrepo.Parse("the repository of a merge group", group.Repo)
	if err != nil {
		return intake.Result{}, err
	}
	store.OnCommit(ctx, func() { h.w.queueMergeGroupWrite(repository, group.Number, group.HeadSHA, false) })
	return intake.Result{}, nil
}

// queueMergeGroupWrite queues, for the writer (writeVerdicts), a write of legion-verdict on group,
// the commit of a merge queue's group for pull request number of repository (spec R6): a
// merge_group event's checks_requested asks for it, and so does the writers' sweep for each group
// commit it finds with no run of this daemon's. overForeign is writeDecided's. A repository no
// verdict entry names asks nothing, as it asks nothing of queueVerdictWrite, and neither does a
// runtime with no writer. No merge-state read waits on it: a group is no head an issue's reads are
// of.
func (w *workflowRuntime) queueMergeGroupWrite(repository ghrepo.Repository, number int, group string, overForeign bool) {
	if w.verdicts == nil {
		return
	}
	if _, ok := w.verdictEntry(repository); !ok {
		return
	}
	w.verdicts.add(verdictTarget{repository: repository, number: number, group: group, overForeign: overForeign})
}

// writeMergeGroup writes legion-verdict on target's group commit from the verdict of its pull
// request at the head that pull request stands at now (decideVerdict at currentHead): success
// where the verdict stands there, and failure where it does not (spec R6). A success, and a head
// with no decision yet, is decided over every open pull request at that head (atHead), so a refusal
// of another pull request there fails the group too (R4); the write reads the open pull requests at
// that head once (headPulls). A group with no verdict is failure, not nothing, so the queue removes
// the entry at once rather than at its check's timeout; that holds for a pull request on another
// project's issue branch too, where no verdict stands here. A head that is undecided, GitHub having
// given no answer to a read its decision needs (verdict.Undecided), gets the group no run of the
// daemon's own yet, as it gets the head none (writeDecided); once the writers' sweeps found that head
// so noAnswerSweeps times in a row, the head is failure, and so is the group (unreadAfterNoAnswer).
// A group's read of the head's change that GitHub answers starts the head's count again, as any
// answered read of it does (countUnanswered), and adds none.
// The group commit holds the pull requests ahead of it in the queue too, each with a group of its
// own that the queue also waits for, so only the pull request the group's ref names is read.
func (w *workflowRuntime) writeMergeGroup(ctx context.Context, target verdictTarget) error {
	entry, ok := w.verdictEntry(target.repository)
	if !ok {
		return nil
	}
	pulls := &headPulls{}
	decided, err := w.decideVerdict(ctx, entry, target.repository, target.number, currentHead, pulls)
	if err != nil {
		return err
	}
	if decided.conclusion == verdict.Success || decided.conclusion == verdict.Undecided {
		combined, err := w.atHead(ctx, entry, target.repository, target.number, decided, pulls)
		switch {
		case err == nil:
			decided = combined
		case !gitHubReadFailed(err) || decided.conclusion == verdict.Success:
			return err
		}
	}
	sweeps, _ := w.verdicts.countUnanswered(target.repository.String(), decided.head, decided, false)
	switch decided.conclusion {
	case verdict.Success:
	case verdict.Undecided:
		if sweeps >= noAnswerSweeps {
			decided = unreadAfterNoAnswer(decided)
		}
	default:
		decided.conclusion = verdict.Failure
	}
	return w.writeDecided(ctx, verdictTarget{repository: target.repository, number: target.number, head: target.group}, decided, target.overForeign)
}
