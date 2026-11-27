package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// CensusIssue is one issue whose merge the census reads candidates of: its key, the repository its
// record names, and what its rows show of its merge (record.MergeHistoryOf). The census passes
// only PARTIAL and NONE issues: an ANSWERED one contacts no candidate source.
type CensusIssue struct {
	Key      string
	Repo     string
	Class    record.MergeHistoryClass
	Evidence record.MergeEvidence
}

// CensusProgress is what a census's candidate reads saved, which its next run resumes from: each
// issue's merge read progress, by key, the passes its runs started, and the GET attempts, retries
// and token requests they made.
type CensusProgress struct {
	Passes        int                                 `json:"passes"`
	Gets          int                                 `json:"gets"`
	Retries       int                                 `json:"retries"`
	TokenRequests int                                 `json:"tokenRequests"`
	Issues        map[string]record.MergeReadProgress `json:"issues"`
}

// CensusRead is what a census's candidate reads read with: Dispatch's client, the GitHub App tokens
// and the GitHub API they are minted for (empty is api.github.com), the database whose merge read
// slot every request takes, its clock, and how many GETs this run may send (MaxGets, at most one
// pass's eight, and the pass's eight where it is 0). Save is called with the progress after each
// change, before a request is sent and after its answer: what makes a run resumable.
type CensusRead struct {
	Dispatch  dispatch.Client
	Tokens    appauth.Tokens
	GitHubAPI string
	Pool      *store.Pool
	Now       func() time.Time
	MaxGets   int
	Save      func(CensusProgress) error
}

// CensusOutcome is where one issue's candidate read stands after a run: its merge answered
// (Answering), its search completed with no answering candidate (Unread), or unfinished, where its
// automatic retries stopped (Stopped) or not.
type CensusOutcome struct {
	Answering, Unread, Stopped bool
}

// ReadMergeCandidates runs one pass of the census's candidate reads over issues, in their order,
// with the merge read's own stages (nextMergeRequest, mergeReader.send, applyMergeAnswer,
// mergeReadFailed) and its bounds: one database-wide slot, at most one GET at a time, and at most
// eight GETs a pass. It applies no fact and writes nothing to the record: progress is the census's
// own, saved by read.Save. A stage whose retry is not due yet is passed over and stays unfinished.
// An attempt reserved by a run that stopped before it saved the answer is counted uncertain. It
// answers the progress and each issue's outcome, by key.
func ReadMergeCandidates(ctx context.Context, read CensusRead, issues []CensusIssue, progress CensusProgress) (CensusProgress, map[string]CensusOutcome, error) {
	if progress.Issues == nil {
		progress.Issues = map[string]record.MergeReadProgress{}
	}
	limit := read.MaxGets
	if limit <= 0 || limit > mergeReadPassGETs {
		limit = mergeReadPassGETs
	}
	progress.Passes++
	if err := read.Save(progress); err != nil {
		return progress, nil, err
	}
	reader := mergeReader{dispatch: read.Dispatch, github: func(ctx context.Context, repository ghrepo.Repository) (githubrest.Client, error) {
		progress.TokenRequests++
		return implementClient(ctx, read.Tokens, read.GitHubAPI, repository)
	}}
	sent := 0
	var runErr error
	for _, issue := range issues {
		if runErr != nil {
			break
		}
		recorded, err := ghrepo.Parse("the issue's repository", issue.Repo)
		if err != nil {
			return progress, nil, fmt.Errorf("read the repository of %s: %w", issue.Key, err)
		}
		state := progress.Issues[issue.Key]
		if state.Reserved {
			state = mergeReadFailed(state, errors.New("a request whose answer was never saved: the census stopped after it was reserved"), read.Now().UTC(), true)
			progress.Issues[issue.Key] = state
			if err := read.Save(progress); err != nil {
				return progress, nil, err
			}
		}
		for {
			request, _, done := nextMergeRequest(issue.Key, recorded, issue.Evidence, &state)
			progress.Issues[issue.Key] = state
			if done || !state.RetryAt.IsZero() && state.RetryAt.After(read.Now().UTC()) {
				break
			}
			if sent == limit || ctx.Err() != nil {
				runErr = ctx.Err()
				break
			}
			slot, err := takeCensusSlot(ctx, read.Pool)
			if err != nil {
				runErr = err
				break
			}
			if state.Failures > 0 {
				progress.Retries++
			}
			state.Attempts++
			state.Reserved = true
			progress.Gets++
			progress.Issues[issue.Key] = state
			if err := read.Save(progress); err != nil {
				slot.Release(ctx)
				return progress, nil, err
			}
			sent++
			sending, cancel := context.WithTimeout(ctx, mergeReadStageBound)
			answer, sendErr := reader.send(sending, issue.Key, state, request)
			cancel()
			slot.Release(ctx)
			state.Reserved = false
			if sendErr != nil {
				state = mergeReadFailed(state, sendErr, read.Now().UTC(), false)
			} else {
				state = applyMergeAnswer(issue.Key, recorded, state, request, answer)
			}
			progress.Issues[issue.Key] = state
			if err := read.Save(progress); err != nil {
				return progress, nil, err
			}
		}
	}
	if err := read.Save(progress); err != nil {
		return progress, nil, err
	}
	outcomes := make(map[string]CensusOutcome, len(issues))
	for _, issue := range issues {
		state := progress.Issues[issue.Key]
		recorded, _ := ghrepo.Parse("the issue's repository", issue.Repo)
		_, merge, done := nextMergeRequest(issue.Key, recorded, issue.Evidence, &state)
		outcomes[issue.Key] = CensusOutcome{Answering: done && merge.Merged(), Unread: state.State == mergeStateUnread,
			Stopped: state.State == mergeStateStopped}
	}
	return progress, outcomes, runErr
}

// takeCensusSlot takes the database's one merge read slot, trying again each second while a
// daemon's read holds it, until ctx ends: the census's requests stand one at a time with the
// daemon's.
func takeCensusSlot(ctx context.Context, pool *store.Pool) (*store.MergeReadSlot, error) {
	for {
		slot, taken, err := pool.TakeMergeReadSlot(ctx)
		if err != nil || taken {
			return slot, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(mergeReadSlotWait):
		}
	}
}
