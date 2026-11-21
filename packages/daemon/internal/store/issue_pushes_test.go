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

// The store records every push at its own clock, so a push it records again — a webhook GitHub
// delivered twice past the fact's own dedupe, or the per-issue read's rebuild of a push whose
// webhook then arrives too, under an event id of its own — comes with a later time than the first.
// It moves neither the head nor the time: a time moved forward refuses every copy written between
// the two although each holds the push. The time is the database's clock when the push's own
// transaction recorded it, the clock every copy's time and every pod's first ask are read by.
func TestTheSamePushRecordedTwiceMovesNeitherTheHeadNorItsTime(t *testing.T) {
	s := migratedStore(t)
	before := storeClock(t, s)
	recordPush(t, s, "INFRA-1117", "c1", "c2")
	after := storeClock(t, s)
	first := newestPush(t, s, "INFRA-1117")
	if first.Head != "c2" || first.At.Before(before) || first.At.After(after) {
		t.Fatalf("the push left the record at %+v, want c2 at the database's clock between %s and %s", first, before, after)
	}

	recordPush(t, s, "INFRA-1117", "c1", "c2")

	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c2" || !push.At.Equal(first.At) {
		t.Fatalf("the push recorded again left the record at %+v, want c2 at %s, when it was first recorded", push, first.At)
	}
}

// A late webhook of a push the record has already passed moves nothing back. The read rebuilds
// c1..c2 from the branch's activity, the webhook of c2..c3 follows, and then c1..c2's own webhook
// arrives: its head is the head c2..c3 replaced, so it is behind the newest push, and the record
// still says c3 at the time c2..c3 was recorded.
func TestALatePushBehindTheNewestMovesNeitherTheHeadNorItsTime(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	recordPush(t, s, "INFRA-1117", "c1", "c2")
	read := newestPush(t, s, "INFRA-1117")
	recordPush(t, s, "INFRA-1117", "c2", "c3")
	webhook := newestPush(t, s, "INFRA-1117")
	if webhook.Head != "c3" || !webhook.At.After(read.At) {
		t.Fatalf("the push of c3 left the record at %+v, want c3 after the push of c2 at %s", webhook, read.At)
	}

	recordPush(t, s, "INFRA-1117", "c1", "c2")

	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c3" || !push.At.Equal(webhook.At) {
		t.Fatalf("the late webhook of c1..c2 moved the record to %+v, want c3 at %s", push, webhook.At)
	}

	// Its time only ever moves later. The record of c3 stands an hour ahead of the database's
	// clock, as it does when that clock is stepped back an hour after the record was written: a
	// push to a head the record has not seen, recorded now, leaves it where it was.
	ahead := webhook.At.Add(time.Hour)
	if _, err := s.Pool().Exec(ctx, `update issue_pushes set pushed_at = $2 where issue = $1`, "INFRA-1117", ahead); err != nil {
		t.Fatal(err)
	}
	recordPush(t, s, "INFRA-1117", "c3", "c4")
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c3" || !push.At.Equal(ahead) {
		t.Fatalf("a push recorded at an earlier time than the record's moved it to %+v, want c3 at %s", push, ahead)
	}
}

// The push that creates the branch names the all-zero sha as its before, and the push that deletes
// it names it as its after. That sha is no commit, so the record never keeps it as a head. The
// creation is the issue's newest push and the push after it moves the record on; a deletion leaves
// the record where it was, since it puts nothing on GitHub that a copy could be behind.
func TestABranchCreatedFromTheZeroShaIsRecordedAndALaterPushMovesTheRecord(t *testing.T) {
	s := migratedStore(t)
	recordPush(t, s, "INFRA-1117", noPushHead, "c1")
	created := newestPush(t, s, "INFRA-1117")
	if created.Head != "c1" || !created.Recorded() {
		t.Fatalf("the branch's creation left the record at %+v, want c1", created)
	}

	recordPush(t, s, "INFRA-1117", "c1", "c2")
	pushed := newestPush(t, s, "INFRA-1117")
	if pushed.Head != "c2" || !pushed.At.After(created.At) {
		t.Fatalf("the push after the creation left the record at %+v, want c2 after %s", pushed, created.At)
	}

	recordPush(t, s, "INFRA-1117", "c2", noPushHead)
	if push := newestPush(t, s, "INFRA-1117"); push.Head != "c2" || !push.At.Equal(pushed.At) {
		t.Fatalf("the branch's deletion moved the record to %+v, want c2 at %s", push, pushed.At)
	}
	if held, zero := pushHeads(t, s, "INFRA-1117"); held != 2 || zero != 0 {
		t.Fatalf("the record keeps %d heads of the issue, %d of them the all-zero sha; want c1 and c2 and never the all-zero sha", held, zero)
	}
}

// The record answers whether a push moved it, and the push handler asks for a copy of the
// issue's workspace for that push alone (workflow's recordPush): a copy taken for a push that
// moved nothing would discard the copy in flight as overtaken and buy a bundle the restore never
// needed. The branch's creation and a push to a head the record has not seen move it and say so;
// the same push again, a late push behind the newest, the branch's deletion and a push naming no
// after move nothing and say that.
func TestAPushSaysItMovedTheRecordOnlyWhenItDid(t *testing.T) {
	s := migratedStore(t)
	for i, step := range []struct {
		name          string
		before, after string
		moved         bool
	}{
		{name: "the branch's creation", before: noPushHead, after: "c1", moved: true},
		{name: "a push to a head the record has not seen", before: "c1", after: "c2", moved: true},
		{name: "the same push recorded again", before: "c1", after: "c2"},
		{name: "a late push behind the newest", before: noPushHead, after: "c1"},
		{name: "the branch's deletion", before: "c2", after: noPushHead},
		{name: "a push naming no after", before: "c2", after: ""},
		{name: "a push past every head the record holds", before: "c2", after: "c3", moved: true},
	} {
		before := newestPush(t, s, "INFRA-1117")
		moved := recordPush(t, s, "INFRA-1117", step.before, step.after)
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
// when it is recorded again; and it is the issue's own heads it drops, never another issue's.
// Every push of one transaction is stamped when its own statement runs, so the row moves to the
// last of them.
func TestTheRecordKeepsTheNewestHeadsOfEachIssueUpToItsBound(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	recordPush(t, s, "INFRA-2760", "d0", "d1")
	head := func(i int) string { return fmt.Sprintf("c%03d", i) }
	const pushes = maxIssuePushHeads + 44
	if err := s.Tx(ctx, func(tx pgx.Tx) error {
		for i := 1; i <= pushes; i++ {
			if _, err := s.RecordIssuePush(ctx, tx, "INFRA-1117", head(i-1), head(i)); err != nil {
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
	newest := newestPush(t, s, "INFRA-1117")
	if newest.Head != head(pushes) {
		t.Fatalf("the record reads %+v, want %s", newest, head(pushes))
	}

	// The oldest push whose head the bound keeps, recorded again, moves nothing.
	oldestKept := pushes - maxIssuePushHeads + 1
	recordPush(t, s, "INFRA-1117", head(oldestKept-1), head(oldestKept))
	if push := newestPush(t, s, "INFRA-1117"); push.Head != head(pushes) || !push.At.Equal(newest.At) {
		t.Fatalf("the push of %s recorded again moved the record to %+v, want %s at %s", head(oldestKept), push, head(pushes), newest.At)
	}
}
