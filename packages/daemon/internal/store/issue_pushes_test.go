package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// newestPush is the newest push of issue's branch the record holds, as the restore reads it
// (issuePush), in a transaction of its own.
func newestPush(t *testing.T, s *Store, issue string) runtime.WorkspacePush {
	t.Helper()
	var push runtime.WorkspacePush
	if err := s.Tx(context.Background(), func(tx pgx.Tx) error {
		read, err := issuePush(context.Background(), tx, issue)
		push = read
		return err
	}); err != nil {
		t.Fatalf("read the newest push of %s: %v", issue, err)
	}
	return push
}

// pushHeads is how many heads the record keeps for issue, and how many of them are the all-zero
// sha.
func pushHeads(t *testing.T, s *Store, issue string) (int, int) {
	t.Helper()
	var held, zero int
	if err := s.pool.QueryRow(context.Background(), `select count(*), count(*) filter (where head = $2)
		from issue_push_heads where issue = $1`, issue, noPushHead).Scan(&held, &zero); err != nil {
		t.Fatalf("count the heads the record keeps for %s: %v", issue, err)
	}
	return held, zero
}

// The daemon applies every push at its own clock, so a push it applies again — a webhook GitHub
// delivered twice past the fact's own dedupe, or the per-issue read's rebuild of a push whose
// webhook then arrives too, under an event id of its own — comes with a later time than the first.
// It moves neither the head nor the time: a time moved forward refuses every copy taken between
// the two although each holds the push, and a refused release bundle holds the issue's pod.
func TestTheSamePushRecordedTwiceMovesNeitherTheHeadNorItsTime(t *testing.T) {
	s := migratedStore(t)
	first := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	recordPush(t, s, "INFRA-1117", "c1", "c2", first)

	recordPush(t, s, "INFRA-1117", "c1", "c2", first.Add(time.Minute))

	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c2" || !push.At.Equal(first) {
		t.Fatalf("the push applied again left the record at %+v, want c2 at %s, when it was first applied", push, first)
	}
}

// A late webhook of a push the record has already passed moves nothing back. The read rebuilds
// c1..c2 from the branch's activity, the webhook of c2..c3 follows, and then c1..c2's own webhook
// arrives an hour on: its head is the head c2..c3 replaced, so it is behind the newest push, and the
// record still says c3 at the time c2..c3 was applied.
func TestALatePushBehindTheNewestMovesNeitherTheHeadNorItsTime(t *testing.T) {
	s := migratedStore(t)
	read := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	webhook := read.Add(time.Minute)
	recordPush(t, s, "INFRA-1117", "c1", "c2", read)
	recordPush(t, s, "INFRA-1117", "c2", "c3", webhook)
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c3" || !push.At.Equal(webhook) {
		t.Fatalf("the push of c3 left the record at %+v, want c3 at %s", push, webhook)
	}

	recordPush(t, s, "INFRA-1117", "c1", "c2", webhook.Add(time.Hour))

	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c3" || !push.At.Equal(webhook) {
		t.Fatalf("the late webhook of c1..c2 moved the record to %+v, want c3 at %s", push, webhook)
	}

	// Its time only ever moves later: a push to a head the record has not seen, applied at an
	// earlier time, leaves it where it was.
	recordPush(t, s, "INFRA-1117", "c3", "c4", read)
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c3" || !push.At.Equal(webhook) {
		t.Fatalf("a push applied at an earlier time moved the record to %+v, want c3 at %s", push, webhook)
	}
}

// The push that creates the branch names the all-zero sha as its before, and the push that deletes
// it names it as its after. That sha is no commit, so the record never keeps it as a head. The
// creation is the issue's newest push and the push after it moves the record on; a deletion leaves
// the record where it was, since it puts nothing on GitHub that a copy could be behind.
func TestABranchCreatedFromTheZeroShaIsRecordedAndALaterPushMovesTheRecord(t *testing.T) {
	s := migratedStore(t)
	created := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	recordPush(t, s, "INFRA-1117", noPushHead, "c1", created)
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c1" || !push.At.Equal(created) {
		t.Fatalf("the branch's creation left the record at %+v, want c1 at %s", push, created)
	}

	pushed := created.Add(time.Minute)
	recordPush(t, s, "INFRA-1117", "c1", "c2", pushed)
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c2" || !push.At.Equal(pushed) {
		t.Fatalf("the push after the creation left the record at %+v, want c2 at %s", push, pushed)
	}

	recordPush(t, s, "INFRA-1117", "c2", noPushHead, pushed.Add(time.Minute))
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c2" || !push.At.Equal(pushed) {
		t.Fatalf("the branch's deletion moved the record to %+v, want c2 at %s", push, pushed)
	}
	if held, zero := pushHeads(t, s, "INFRA-1117"); held != 2 || zero != 0 {
		t.Fatalf("the record keeps %d heads of the issue, %d of them the all-zero sha; want c1 and c2 and never the all-zero sha", held, zero)
	}
}

// The record answers whether a push moved it, and the push handler asks for a copy of the
// issue's workspace for that push alone (workflow's recordPush): a copy taken for a push that
// moved nothing would discard the copy in flight as overtaken and buy a bundle the restore never
// needed. The branch's creation and a push to a head the record has not seen move it and say so;
// the same push again, a late push behind the newest, a push to an unseen head applied at an
// earlier time, the branch's deletion and a push naming no after move nothing and say that.
func TestAPushSaysItMovedTheRecordOnlyWhenItDid(t *testing.T) {
	s := migratedStore(t)
	created := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	for i, step := range []struct {
		name          string
		before, after string
		at            time.Time
		moved         bool
	}{
		{name: "the branch's creation", before: noPushHead, after: "c1", at: created, moved: true},
		{name: "a push to a head the record has not seen", before: "c1", after: "c2", at: created.Add(time.Minute), moved: true},
		{name: "the same push applied again", before: "c1", after: "c2", at: created.Add(2 * time.Minute)},
		{name: "a late push behind the newest", before: noPushHead, after: "c1", at: created.Add(3 * time.Minute)},
		{name: "a push to an unseen head applied at an earlier time", before: "c2", after: "c3", at: created},
		{name: "the branch's deletion", before: "c2", after: noPushHead, at: created.Add(4 * time.Minute)},
		{name: "a push naming no after", before: "c2", after: "", at: created.Add(5 * time.Minute)},
		{name: "a push past every head the record holds", before: "c3", after: "c4", at: created.Add(6 * time.Minute), moved: true},
	} {
		before := newestPush(t, s, "INFRA-1117")
		moved := recordPush(t, s, "INFRA-1117", step.before, step.after, step.at)
		after := newestPush(t, s, "INFRA-1117")
		if moved != step.moved {
			t.Fatalf("push %d, %s, answered moved=%t, want %t; the record went from %+v to %+v", i, step.name, moved, step.moved, before, after)
		}
		if changed := after.Head != before.Head || !after.At.Equal(before.At); changed != moved {
			t.Fatalf("push %d, %s, answered moved=%t, but the record went from %+v to %+v", i, step.name, moved, before, after)
		}
	}
}

// The heads the record keeps are bounded per issue to the newest maxIssuePushHeads, so an issue
// pushed for months keeps a bounded set and not one row per push it ever made. The bound drops the
// oldest heads and keeps the newest, so a push the per-issue read can still rebuild moves nothing
// when it is applied again; and it is the issue's own heads it drops, never another issue's.
func TestTheRecordKeepsTheNewestHeadsOfEachIssueUpToItsBound(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	start := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	recordPush(t, s, "INFRA-2760", "d0", "d1", start)
	head := func(i int) string { return fmt.Sprintf("c%03d", i) }
	const pushes = maxIssuePushHeads + 44
	if err := s.Tx(ctx, func(tx pgx.Tx) error {
		for i := 1; i <= pushes; i++ {
			if _, err := s.RecordIssuePush(ctx, tx, "INFRA-1117", head(i-1), head(i), start.Add(time.Duration(i)*time.Minute)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record %d pushes: %v", pushes, err)
	}
	if held, _ := pushHeads(t, s, "INFRA-1117"); held != maxIssuePushHeads {
		t.Fatalf("the record keeps %d heads after %d pushes, want the newest %d", held, pushes, maxIssuePushHeads)
	}
	if held, _ := pushHeads(t, s, "INFRA-2760"); held != 2 {
		t.Fatalf("the bound of INFRA-1117's heads left INFRA-2760 with %d heads, want its 2", held)
	}
	newest := start.Add(pushes * time.Minute)
	if push := newestPush(t, s, "INFRA-1117"); push.Head != head(pushes) || !push.At.Equal(newest) {
		t.Fatalf("the record reads %+v, want %s at %s", push, head(pushes), newest)
	}

	// The oldest push whose head the bound keeps, applied again, moves nothing.
	oldestKept := pushes - maxIssuePushHeads + 1
	recordPush(t, s, "INFRA-1117", head(oldestKept-1), head(oldestKept), newest.Add(time.Hour))
	if push := newestPush(t, s, "INFRA-1117"); push.Head != head(pushes) || !push.At.Equal(newest) {
		t.Fatalf("the push of %s applied again moved the record to %+v, want %s at %s", head(oldestKept), push, head(pushes), newest)
	}
}
