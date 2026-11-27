package record

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// EndHoldsOf reads what holds each issue of issues that stands held before its retro for its fix
// or for its CI where it keeps no slot (HoldForFix, HoldForCI), by key, for the held list to name
// beside the word the ladder gives such an issue (LineFacts.EndHolds; HeldFixRunning,
// HeldCIAtHead). The record of a hold keeps its reason alone, so the two reads the workflow's hold
// of a task's end makes (workflow's retroHold) are made again here:
//
//   - for a fix, the check and the head of the fix of the issue's task that reads its job or runs
//     (Store.RunningFix), the task being the one task of the plan at the issue's generation
//     (Store.Plan, StoredPlan.Task), as the workflow's walk names it;
//   - for CI, each check a CI job runs that has not passed at the head of the issue's open pull
//     request (CIWaits over Store.HeadStates), the head the workflow reads a task's checks at
//     (workflow's taskHead).
//
// An issue whose hold has nothing to name — no plan rows, no fix of its task that still runs, no
// open pull request, no set of checks held at the head, or no check left to pass there — has no
// entry, and its row names nothing. The map is never nil, so the caller can tell facts already
// read from facts never read (blockers.JudgeLine). Each issue held so costs two or three reads,
// and every other issue none.
func EndHoldsOf(ctx context.Context, tx pgx.Tx, store Store, issues []Issue) (map[string][]string, error) {
	holds := map[string][]string{}
	for _, issue := range issues {
		if !InWorkflow(issue) || KeepsSlot(issue) {
			continue
		}
		var held []string
		var err error
		switch {
		case HeldFor(issue, HoldForFix):
			held, err = fixHolding(ctx, tx, store, issue)
		case HeldFor(issue, HoldForCI):
			held, err = ciHolding(ctx, tx, store, issue.Key)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(held) > 0 {
			holds[issue.Key] = held
		}
	}
	return holds, nil
}

// fixHolding is the check and the head of the fix of issue's task that reads its job or runs,
// and nothing where the issue's generation has no plan rows, which run no fix, or no fix of its
// task runs any longer.
func fixHolding(ctx context.Context, tx pgx.Tx, store Store, issue Issue) ([]string, error) {
	stored, err := store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return nil, err
	}
	task, err := stored.Task()
	if err != nil {
		return nil, err
	}
	fix, err := store.RunningFix(ctx, tx, issue.Key, issue.Generation, task.Task)
	if err != nil || fix == nil {
		return nil, err
	}
	return []string{fix.Check, fix.Head}, nil
}

// ciHolding is each check a CI job runs that has not passed at the head of key's open pull
// request (CIWaits), and nothing where key has no open pull request. It writes no record back, so
// it reads the pull request's row alone (Store.PullRequestRow), with none of what it remembers of
// its settled heads.
func ciHolding(ctx context.Context, tx pgx.Tx, store Store, key string) ([]string, error) {
	pr, err := store.PullRequestRow(ctx, tx, key)
	if err != nil || pr == nil || pr.State != PullRequestOpen {
		return nil, err
	}
	head, err := store.HeadStates(ctx, tx, key, pr.Repo, pr.HeadSHA)
	if err != nil {
		return nil, err
	}
	return CIWaits(head), nil
}
