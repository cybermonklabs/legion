package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// RecordIssuePush records the push of issue's branch from before to after, as the daemon applied
// it at at, in the push fact's own transaction tx (workflow's push handler). It is the record the
// restore of a new pod reads to refuse every running copy of the issue's workspace older than the
// issue's newest push (TakeWorkspaceCopyForRestore, runtime.DecideWorkspaceRestore), and the
// migration that made its two tables says which record it is (20261113005308_issue_pushes).
//
// One row per issue holds the newest push, and at cannot say which push that is. It is the
// daemon's clock when it applied the fact, so a redelivered webhook, the per-issue read's rebuild
// of a push whose own webhook then arrives as well, and a webhook an hour late are each applied
// later than the push they repeat or that replaced them. The commits say it instead: the record
// keeps every head the issue's recorded pushes replaced or left (issue_push_heads), and a push
// that leaves the branch on one of them writes nothing, so neither the row's head nor its time
// moves. Such a push is one the record holds already, one behind a push it holds, or one that
// sets the branch back on a commit it held before, and none of them puts a commit on GitHub that
// the copies taken since the row last moved lack. A push that leaves the branch on a head the
// record has not seen keeps both of its heads and moves the row to that head at at, and only ever
// to a later time. The all-zero sha GitHub names as a new branch's before and a deleted branch's
// after is no commit (pushHead), so it is never kept, and a push that deletes the branch leaves
// the row where it was: it puts nothing on GitHub that a copy could be behind.
//
// What the record cannot know is the place of a push whose after it has not seen, so it takes
// that push as the newest: nothing it holds says otherwise. A late push is such a push when the
// record has seen neither of its heads — one more than a push behind the newest whose pushes
// between are unrecorded as well, or one older than every head the bound below keeps — and taking
// it as newer costs at most a refused restore, whose copy keeps its bytes for an operator, and no
// commit on GitHub, since no restore sets the branch (runtime.RestorableBookmarks).
//
// An issue keeps its newest maxIssuePushHeads heads, by when the daemon first saw each, so the
// heads of the pushes the per-issue read can rebuild stay. Every push fact is applied under the
// intake's one serializer (intake.SerializeFacts), so two pushes of one issue never interleave
// between the read of the heads and the writes.
//
// It answers whether the push moved the row. Only a push that moved it can refuse a running copy
// the restore would otherwise send, so it is the one push the handler asks a new copy of the
// workspace for (workflow's recordPush): a push that moved nothing leaves every copy taken since
// the row last moved as good as it was, and a copy asked for it would buy one more `git bundle
// create` in a workspace a role is working in and discard the copy in flight as overtaken.
func (s *Store) RecordIssuePush(ctx context.Context, tx pgx.Tx, issue, before, after string, at time.Time) (bool, error) {
	if issue == "" {
		return false, errors.New("a push record needs its issue")
	}
	if at.IsZero() {
		return false, fmt.Errorf("the push record of %s needs when the daemon applied the push", issue)
	}
	if pushHead(after) {
		var seen bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from issue_push_heads where issue = $1 and head = $2)`,
			issue, after).Scan(&seen); err != nil {
			return false, fmt.Errorf("read whether the push of %s to %s is recorded: %w", issue, after, err)
		}
		if seen {
			return false, nil
		}
	}
	var heads []string
	for _, head := range []string{before, after} {
		if pushHead(head) && !slices.Contains(heads, head) {
			heads = append(heads, head)
		}
	}
	if len(heads) == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `insert into issue_push_heads (issue, head, seen_at)
		select $1::text, heads.head, $3::timestamptz from unnest($2::text[]) as heads(head)
		on conflict (issue, head) do nothing`, issue, heads, at); err != nil {
		return false, fmt.Errorf("record the heads of the push of %s: %w", issue, err)
	}
	var moved bool
	if pushHead(after) {
		tag, err := tx.Exec(ctx, `insert into issue_pushes (issue, pushed_at, head) values ($1, $2, $3)
			on conflict (issue) do update set pushed_at = excluded.pushed_at, head = excluded.head
				where issue_pushes.pushed_at < excluded.pushed_at`, issue, at, after)
		if err != nil {
			return false, fmt.Errorf("record the push of %s: %w", issue, err)
		}
		moved = tag.RowsAffected() > 0
	}
	if _, err := tx.Exec(ctx, `delete from issue_push_heads where issue = $1 and head not in (
		select head from issue_push_heads where issue = $1 order by seen_at desc, head desc limit $2)`,
		issue, maxIssuePushHeads); err != nil {
		return false, fmt.Errorf("bound the recorded heads of %s: %w", issue, err)
	}
	return moved, nil
}

// maxIssuePushHeads is how many of the heads an issue's pushes replaced or left the record keeps
// (RecordIssuePush): more than the hundred newest entries of a branch's activity
// (githubrest.Client.BranchActivity), whose pushes the per-issue read rebuilds, and each entry
// names two heads.
const maxIssuePushHeads = 256

// noPushHead is the sha GitHub names where a push has no commit: the before of the push that
// creates a branch and the after of the push that deletes it.
const noPushHead = "0000000000000000000000000000000000000000"

// pushHead is whether sha, one side of a push, names a commit: neither the all-zero sha
// (noPushHead) nor nothing, which a listener that sends no before leaves.
func pushHead(sha string) bool {
	return sha != "" && sha != noPushHead
}

// issuePush is the newest push of issue's branch the daemon recorded (RecordIssuePush), and the
// zero value for an issue with none recorded, which the restore's decision reads as nothing newer
// known to be on GitHub.
func issuePush(ctx context.Context, tx pgx.Tx, issue string) (runtime.WorkspacePush, error) {
	var push runtime.WorkspacePush
	err := tx.QueryRow(ctx, `select pushed_at, head from issue_pushes where issue = $1`, issue).Scan(&push.At, &push.Head)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspacePush{}, nil
	}
	if err != nil {
		return runtime.WorkspacePush{}, fmt.Errorf("read the newest push of %s: %w", issue, err)
	}
	return push, nil
}
