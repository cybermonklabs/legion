package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// wroteSuccess is whether the newest check run or the newest commit status the store records the
// daemon wrote on target's head concluded success.
func (w *workflowRuntime) wroteSuccess(ctx context.Context, target verdictTarget) (bool, error) {
	var newest map[store.VerdictWriteKind]store.VerdictConclusion
	err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		writes, err := store.VerdictWritesTx(ctx, tx, target.repository, target.head)
		newest = map[store.VerdictWriteKind]store.VerdictConclusion{}
		for _, written := range writes {
			newest[written.Kind] = written.Conclusion
		}
		return err
	})
	return newest[store.VerdictWriteCheckRun] == store.VerdictConclusionSuccess || newest[store.VerdictWriteStatus] == store.VerdictConclusionSuccess, err
}

// listedCheckRun is one check run as GitHub lists a commit's (GET /commits/{ref}/check-runs): its
// id, its conclusion and the id of the App that made it, which a rename of the App leaves as it is.
type listedCheckRun struct {
	ID         int64  `json:"id"`
	Conclusion string `json:"conclusion"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// listedStatus is one commit status as GitHub lists a commit's (GET /commits/{ref}/statuses),
// newest first: its id, context, state and the id of the account that created it, which a rename
// of the App leaves as it is for its bot.
type listedStatus struct {
	ID      int64  `json:"id"`
	Context string `json:"context"`
	State   string `json:"state"`
	Creator struct {
		ID int64 `json:"id"`
	} `json:"creator"`
}

// writeDecided writes decided's conclusion as legion-verdict on target's head with the daemon's own
// verdict token (appauth.Tokens.VerdictToken), and records each write in the store
// (store.VerdictWrite) and in one legion_verdict_write line. It reads the head's legion-verdict
// check runs and commit statuses first. It tells the one App's runs by the App's id, and its bot's
// statuses by the bot user's id the verdict lease carries (appauth.Lease.BotUserID), the creator.id
// C56 reads: a rename of the App changes neither. What each run and status of the App there should
// show is the head's conclusion, or failure where the head has none (spec R19):
//   - each run of the App the record does not hold is updated to it, whatever it shows, and so
//     becomes one the record holds;
//   - each run the record holds is updated to it where GitHub shows it at another conclusion,
//     whether the daemon's own write left it there or another token of the App wrote over it;
//   - a run of the daemon's own is created only where the head has a conclusion and no run of the
//     App is there at all;
//   - the newest legion-verdict status the App's bot created gets a status of the daemon's own over
//     it, with that state, when the record does not hold it or GitHub shows it at another state:
//     GitHub reads the newest status of each context.
//
// A head with no conclusion is written on only when overForeign is set, which is the writers'
// sweep's ask to put failure over each run and status of the App there that does not show it; a
// write a fact asks for (writeVerdictOver without overForeign) writes nothing there and makes no
// GitHub call. In token mode the daemon mints no verdict token (appauth.ErrNoVerdictToken): it
// writes nothing and logs that once. So it does while it holds no App id: an id that is no number
// tells no run as the App's, its own among them, and a write would make a new run at each write;
// the one ERROR where the writer is built says why (newVerdictWriter). One goroutine makes every
// write (writeVerdicts), so no two writes of one head read its runs together and each create one.
func (w *workflowRuntime) writeDecided(ctx context.Context, target verdictTarget, decided verdictDecision, overForeign bool) error {
	_, err := w.writeDecidedChanged(ctx, target, decided, overForeign)
	return err
}

// writeDecidedChanged is writeDecided, and answers too, where it answers no error, whether it
// created or updated a check run or set a commit status: a pass that writes a success is followed
// by one more merge-state read (writeVerdictOver), and a pass that finds it written is not.
func (w *workflowRuntime) writeDecidedChanged(ctx context.Context, target verdictTarget, decided verdictDecision, overForeign bool) (bool, error) {
	if decided.conclusion == verdict.None && !overForeign || w.verdicts.appID == 0 {
		return false, nil
	}
	serving := w.repositories.serving(target.repository)
	lease, err := w.tokens.VerdictToken(ctx, serving.Owner())
	if errors.Is(err, appauth.ErrNoVerdictToken) {
		w.verdicts.tokenMode.Do(func() { w.log.Warn(verdictTokenMode, "repository", target.repository.String()) })
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mint the verdict token for %s: %w", serving, err)
	}
	client := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, serving)}
	call, cancel := context.WithTimeout(ctx, issueRead)
	defer cancel()
	ownRuns, ownStatuses, err := w.recordedVerdictWrites(ctx, target)
	if err != nil {
		return false, err
	}
	runs, err := githubrest.GetListPages[listedCheckRun](call, client, "/commits/"+target.head+"/check-runs?check_name="+verdict.CheckName+"&filter=all", "check_runs")
	if err != nil {
		return false, fmt.Errorf("list the legion-verdict runs of %s at %s: %w", serving, target.head, err)
	}
	over := decided.conclusion
	if over == verdict.None {
		over = verdict.Failure
	}
	shown, changed := false, false
	for _, run := range runs {
		if run.App.ID != w.verdicts.appID {
			continue
		}
		shown = true
		foreign := run.ID
		if ownRuns[run.ID] {
			if run.Conclusion == string(over) {
				continue
			}
			foreign = 0
		}
		if err := w.updateVerdictRun(call, client, target, decided, run.ID, over, foreign); err != nil {
			return false, err
		}
		changed = true
	}
	if decided.conclusion != verdict.None && !shown {
		if err := w.createVerdictRun(call, client, target, decided); err != nil {
			return false, err
		}
		changed = true
	}
	statuses, err := githubrest.GetPages[listedStatus](call, client, "/commits/"+target.head+"/statuses")
	if err != nil {
		return false, fmt.Errorf("list the commit statuses of %s at %s: %w", serving, target.head, err)
	}
	bot := lease.BotUserID
	for _, status := range statuses {
		if status.Context != verdict.CheckName || bot == 0 || status.Creator.ID != bot {
			continue
		}
		switch {
		case !ownStatuses[status.ID]:
			return true, w.createVerdictStatus(call, client, target, decided.kind, over, status.ID)
		case status.State != string(over):
			return true, w.createVerdictStatus(call, client, target, decided.kind, over, 0)
		}
		return changed, nil
	}
	return changed, nil
}

// recordedVerdictWrites is the GitHub id of each check run and of each commit status the store
// records the daemon wrote on target's head.
func (w *workflowRuntime) recordedVerdictWrites(ctx context.Context, target verdictTarget) (runs, statuses map[int64]bool, err error) {
	runs, statuses = map[int64]bool{}, map[int64]bool{}
	err = store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		writes, err := store.VerdictWritesTx(ctx, tx, target.repository, target.head)
		for _, written := range writes {
			if written.Kind == store.VerdictWriteCheckRun {
				runs[written.GitHubID] = true
			} else {
				statuses[written.GitHubID] = true
			}
		}
		return err
	})
	return runs, statuses, err
}

// The output of a legion-verdict run that concludes failure on a Legion head where each check reads
// yes and every testing and review step passed, because a commit of the change has no reader
// (verdict.NoReader) or the daemon could not read the change's commits (verdict.ChangeUnread): one
// title for both, and the opening of each summary, which for a commit with no reader then names
// each such commit, one to a line ("- <sha>"). The merger reads them on the head it would merge and
// tells the owner once for that head (spec R4), so their words are the merger's prompt's to quote:
// change them with prompts/roles/merger.md.
const (
	noReaderTitle   = verdict.CheckName + ": failure (a commit with no reader)"
	noReaderOpening = "Each check reads yes and every testing and review step of the task's plan passed at this head, and no agent of the task read these commits of the change. An agent reads a commit when it approved this head as a review step, or passed here as a testing step, its claim records a session, and no session of its claim made the commit, as the commit's Omp-Session lines name its makers:"
	unreadOpening   = "Each check reads yes and every testing and review step of the task's plan passed at this head, but the daemon could not read every commit of the change, so no commit has a reader."
	// summaryBound is the most bytes a run's summary carries of the commits it names: GitHub takes
	// 65,535 characters at most, and a change of more commits with no reader than fit is named up to
	// the bound and then counted.
	summaryBound = 60000
)

// verdictOutput is the title and summary a check run of legion-verdict shows when it concludes
// conclusion on a head of kind, decided being what the writer decided there: a failure for a commit
// with no reader names each such commit, a failure for commits the daemon could not read says so,
// and every other conclusion names the head's kind alone.
func verdictOutput(decided verdictDecision, conclusion verdict.Conclusion) map[string]string {
	if conclusion == decided.conclusion {
		switch decided.reason {
		case verdict.NoReader:
			return map[string]string{"title": noReaderTitle, "summary": noReaderSummary(decided.noReader)}
		case verdict.ChangeUnread:
			return map[string]string{"title": noReaderTitle, "summary": unreadOpening}
		}
	}
	return map[string]string{
		"title":   verdict.CheckName + ": " + string(conclusion),
		"summary": "The Legion daemon wrote this from the verdicts it records at this " + decided.kind + " head.",
	}
}

// noReaderSummary is the summary of a run that concludes failure for commits with no reader: the
// opening, then each commit, one to a line, while the summary stays within summaryBound, and a count
// of the commits left out after that.
func noReaderSummary(commits []string) string {
	var summary strings.Builder
	summary.WriteString(noReaderOpening + "\n")
	for i, sha := range commits {
		line := "\n- " + sha
		if summary.Len()+len(line) > summaryBound {
			fmt.Fprintf(&summary, "\n\nand %d more commits with no reader, which this summary has no room for.", len(commits)-i)
			break
		}
		summary.WriteString(line)
	}
	return summary.String()
}

// createVerdictRun creates the daemon's own legion-verdict check run on target's head, completed
// with decided's conclusion (POST /check-runs), and records it.
func (w *workflowRuntime) createVerdictRun(ctx context.Context, client githubrest.Client, target verdictTarget, decided verdictDecision) error {
	var created struct {
		ID int64 `json:"id"`
	}
	if err := client.Post(ctx, "/check-runs", map[string]any{"name": verdict.CheckName, "head_sha": target.head, "status": "completed",
		"conclusion": string(decided.conclusion), "output": verdictOutput(decided, decided.conclusion)}, &created); err != nil {
		return fmt.Errorf("create legion-verdict on %s at %s: %w", target.repository, target.head, err)
	}
	return w.recordVerdictWrite(ctx, target, decided.kind, store.VerdictWriteCheckRun, created.ID, decided.conclusion, 0)
}

// updateVerdictRun updates check run id on target's head to conclusion (PATCH /check-runs/{id}),
// with the output decided gives it (verdictOutput), and records it, foreign being the id of the
// run when another writer made it, 0 when it is the daemon's own.
func (w *workflowRuntime) updateVerdictRun(ctx context.Context, client githubrest.Client, target verdictTarget, decided verdictDecision, id int64, conclusion verdict.Conclusion, foreign int64) error {
	if err := client.Patch(ctx, "/check-runs/"+strconv.FormatInt(id, 10), map[string]any{"status": "completed",
		"conclusion": string(conclusion), "output": verdictOutput(decided, conclusion)}, nil); err != nil {
		return fmt.Errorf("update legion-verdict run %d on %s at %s: %w", id, target.repository, target.head, err)
	}
	return w.recordVerdictWrite(ctx, target, decided.kind, store.VerdictWriteCheckRun, id, conclusion, foreign)
}

// createVerdictStatus sets a legion-verdict commit status of the daemon's own on target's head with
// conclusion's state (POST /statuses/{sha}) and records it, foreign being the id of the status of
// another writer it goes over, 0 for one of the daemon's own.
func (w *workflowRuntime) createVerdictStatus(ctx context.Context, client githubrest.Client, target verdictTarget, kind string, conclusion verdict.Conclusion, foreign int64) error {
	var created struct {
		ID int64 `json:"id"`
	}
	if err := client.Post(ctx, "/statuses/"+target.head, map[string]any{"state": string(conclusion), "context": verdict.CheckName,
		"description": "Written by the Legion daemon from the verdicts it records at this " + kind + " head."}, &created); err != nil {
		return fmt.Errorf("set the legion-verdict status on %s at %s: %w", target.repository, target.head, err)
	}
	return w.recordVerdictWrite(ctx, target, kind, store.VerdictWriteStatus, created.ID, conclusion, foreign)
}

// recordVerdictWrite logs one write GitHub made in one legion_verdict_write line, and records it
// in the store: the repository, the pull request, the head and its kind, what was written and its
// GitHub id, the conclusion, and the id of another writer's run or status it went over (0 for
// none), which is what a read of the App's runs at a head matches each one against (spec R19). The
// line comes first, so a write the store fails to record is still named; the store's failure is
// the write's, and the next sweep writes over the run its record does not hold.
func (w *workflowRuntime) recordVerdictWrite(ctx context.Context, target verdictTarget, kind string, written store.VerdictWriteKind, id int64, conclusion verdict.Conclusion, foreign int64) error {
	w.log.Info("legion_verdict_write", "repository", target.repository.String(), "pull_request", target.number, "head", target.head,
		"head_kind", kind, "kind", string(written), "id", id, "conclusion", string(conclusion), "foreign_id", foreign)
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.RecordVerdictWriteTx(ctx, tx, store.VerdictWrite{Repository: target.repository, Head: target.head, Kind: written,
			GitHubID: id, Conclusion: conclusion, ForeignID: foreign, WrittenAt: w.clockNow().UTC()})
		return err
	}); err != nil {
		return fmt.Errorf("record the write of legion-verdict %s %d on %s at %s: %w", written, id, target.repository, target.head, err)
	}
	return nil
}
