package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// mergeMarkIssue records an issue the marks can hang on: a jump mark references the issue record,
// so a test that writes one seeds the issue first. The project column is the key's own prefix,
// which is what the project-scoped reads of issue-keyed tables split on.
func mergeMarkIssue(t *testing.T, records *Postgres, tx pgx.Tx, key string) {
	t.Helper()
	issue := issueFixture(key)
	issue.Project, _, _ = strings.Cut(key, "-")
	issue.Tree = key
	issue.Parent = nil
	must(t, records.PutIssue(context.Background(), tx, issue))
}

// setMark writes one jump mark and reads the answer the conditional write gives: true, since
// every issue these cases seed is one a daemon still runs (Postgres.SetMergeJump). A case that
// wants the other answer reads it for itself.
func setMark(t *testing.T, records *Postgres, tx pgx.Tx, jump MergeJump) {
	t.Helper()
	wrote, err := records.SetMergeJump(context.Background(), tx, jump)
	must(t, err)
	if !wrote {
		t.Fatalf("the jump mark of %s was not written, want it on an issue still in the workflow", jump.Issue)
	}
}

// A jump mark is written per issue and read back two ways: by the issue, which is how the
// submission asks whether this merge jumps, and by the project, which is how the controller and
// the state read the marks that stand. A second set replaces the first, since an issue carries at
// most one. Clearing it answers whether a row went — the answer the spending transaction reads —
// and an issue that carries none clears nothing. Another project's marks are never listed.
func TestAJumpMarkIsReadBackByIssueAndByProjectAndSpentOnce(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// A time in another location, to read the mark's own time back in UTC.
	setAt := time.Date(2026, 11, 10, 9, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))

	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-208", "LEGION-209", "INFRA-7"} {
			mergeMarkIssue(t, records, tx, key)
		}
		setMark(t, records, tx, MergeJump{Issue: "LEGION-208", Reason: "the release waits on it", Session: "controller-1", SetAt: setAt})
		setMark(t, records, tx, MergeJump{Issue: "INFRA-7", Reason: "another project's mark", Session: "controller-2", SetAt: setAt})
	})

	inTx(t, st, func(tx pgx.Tx) {
		jump, err := records.MergeJumpOf(ctx, tx, "LEGION-208")
		must(t, err)
		if jump == nil {
			t.Fatal("the mark of LEGION-208 reads as none, want the one just set")
		}
		if jump.Reason != "the release waits on it" || jump.Session != "controller-1" {
			t.Fatalf("the mark of LEGION-208 = %+v, want the reason and session it was set with", *jump)
		}
		if !jump.SetAt.Equal(setAt) || jump.SetAt.Location() != time.UTC {
			t.Fatalf("the mark's time = %s in %s, want %s in UTC", jump.SetAt, jump.SetAt.Location(), setAt)
		}
		none, err := records.MergeJumpOf(ctx, tx, "LEGION-209")
		must(t, err)
		if none != nil {
			t.Fatalf("the mark of an issue nobody marked = %+v, want none", *none)
		}
		marks, err := records.MergeJumps(ctx, tx, "LEGION")
		must(t, err)
		if len(marks) != 1 || marks[0].Issue != "LEGION-208" {
			t.Fatalf("the marks of LEGION = %+v, want the one of LEGION-208 and no mark of another project", marks)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		setMark(t, records, tx, MergeJump{Issue: "LEGION-208", Reason: "a later reason", Session: "controller-3", SetAt: setAt.Add(time.Hour)})
		marks, err := records.MergeJumps(ctx, tx, "LEGION")
		must(t, err)
		if len(marks) != 1 || marks[0].Reason != "a later reason" || marks[0].Session != "controller-3" {
			t.Fatalf("the marks of LEGION after a second set = %+v, want the one mark replaced", marks)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		cleared, err := records.ClearMergeJump(ctx, tx, "LEGION-208")
		must(t, err)
		if !cleared {
			t.Fatal("clearing the mark of LEGION-208 answered false, want the row it deleted")
		}
		cleared, err = records.ClearMergeJump(ctx, tx, "LEGION-208")
		must(t, err)
		if cleared {
			t.Fatal("clearing a mark already spent answered true, want false: the spending transaction reads that answer")
		}
		cleared, err = records.ClearMergeJump(ctx, tx, "LEGION-209")
		must(t, err)
		if cleared {
			t.Fatal("clearing the mark of an issue that carries none answered true, want false")
		}
		jump, err := records.MergeJumpOf(ctx, tx, "LEGION-208")
		must(t, err)
		if jump != nil {
			t.Fatalf("the spent mark reads as %+v, want none", *jump)
		}
	})
}

// The write of a jump mark is conditional on the issue still being one the daemon runs, and it
// answers whether it wrote. The route checks the same rule before it writes, in a transaction of
// its own (api's runningMarkIssue), so there is a window between that read and this write; an
// issue whose departure commits inside it would otherwise take a mark that outlives the
// departure, to be spent weeks later by whatever merge a re-admission of the issue reached. The
// window is driven here as the request meets it: the check reads a running issue, the departure
// commits, and the write then runs in a transaction of its own and must write nothing.
//
// The three departures are Stopped's own, the one rule the refusal and the workflow's clear
// share. The fourth case is an issue the record no longer holds at all, as a tree's cleanup
// leaves it: there is no issue for a mark to govern, and the insert's own foreign key would
// refuse it with nothing a caller could act on.
func TestAJumpMarkIsNotWrittenForAnIssueWhoseDepartureCommitsFirst(t *testing.T) {
	ctx := context.Background()
	setAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	lingerUntil := setAt.Add(24 * time.Hour)
	for _, tc := range []struct {
		name   string
		depart func(*Issue)
	}{
		{name: "closed as done", depart: func(issue *Issue) { issue.Status, issue.Phase = "done", phase.Done }},
		{name: "parked in backlog", depart: func(issue *Issue) { issue.Status = "backlog" }},
		{name: "lingering after its close", depart: func(issue *Issue) { issue.LingerUntil = &lingerUntil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			records := NewStore()
			inTx(t, st, func(tx pgx.Tx) { mergeMarkIssue(t, records, tx, "LEGION-208") })

			// The route's own check, in its own transaction: the issue is running, so the
			// request goes on to the write.
			inTx(t, st, func(tx pgx.Tx) {
				issue, err := records.Issue(ctx, tx, "LEGION-208")
				must(t, err)
				if Stopped(*issue) {
					t.Fatal("the seeded issue reads as stopped, want one the daemon still runs")
				}
			})
			// The departure commits in the window that check leaves open.
			inTx(t, st, func(tx pgx.Tx) {
				issue, err := records.Issue(ctx, tx, "LEGION-208")
				must(t, err)
				tc.depart(issue)
				must(t, records.PutIssue(ctx, tx, *issue))
			})

			inTx(t, st, func(tx pgx.Tx) {
				wrote, err := records.SetMergeJump(ctx, tx, MergeJump{Issue: "LEGION-208",
					Reason: "a merge this issue will never submit", Session: "controller-1", SetAt: setAt})
				must(t, err)
				if wrote {
					t.Fatal("the write answered that it wrote the mark of an issue that had left the workflow, want false")
				}
				mark, err := records.MergeJumpOf(ctx, tx, "LEGION-208")
				must(t, err)
				if mark != nil {
					t.Fatalf("the record holds the mark %+v of an issue that had left the workflow, want none", *mark)
				}
			})
		})
	}

	t.Run("an issue the record no longer holds", func(t *testing.T) {
		st := migratedStore(t)
		records := NewStore()
		inTx(t, st, func(tx pgx.Tx) {
			wrote, err := records.SetMergeJump(ctx, tx, MergeJump{Issue: "LEGION-404",
				Reason: "a merge of an issue that is gone", Session: "controller-1", SetAt: setAt})
			must(t, err)
			if wrote {
				t.Fatal("the write answered that it wrote the mark of an issue the record does not hold, want false")
			}
			mark, err := records.MergeJumpOf(ctx, tx, "LEGION-404")
			must(t, err)
			if mark != nil {
				t.Fatalf("the record holds the mark %+v of an issue it does not record, want none", *mark)
			}
		})
	})
}

// The hold list is replaced whole: the second replacement is the list, so a hold the first one
// carried and the second does not is gone. An ended hold is deleted by MergeHolds, the read the
// daemon and the controller make, and left where it is by LiveMergeHolds, the read the state
// projection makes inside its read-only snapshot, which lists the live holds alone. A hold's
// times come back in UTC.
func TestTheHoldListIsReplacedWholeAndOnlyMergeHoldsDeletesAnEndedHold(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// A time in another location, to read the hold's own times back in UTC.
	zone := time.FixedZone("UTC-5", -5*60*60)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, zone)
	setAt := now.Add(-time.Hour)
	rowsOf := func(tx pgx.Tx, project string) int {
		t.Helper()
		var rows int
		must(t, tx.QueryRow(ctx, "select count(*) from merge_holds where project = $1", project).Scan(&rows))
		return rows
	}

	first := []MergeHold{
		{Paths: []string{"packages/daemon/", "charts/"}, Repository: "acme/widgets", Reason: "the release is going out", EndsAt: now.Add(time.Hour), Session: "controller-1", SetAt: setAt},
		{Paths: []string{"docs/"}, Reason: "the handbook is being rewritten", EndsAt: now.Add(2 * time.Hour), Session: "controller-1", SetAt: setAt},
		{Paths: []string{"infra/"}, Reason: "a migration lands by hand", EndsAt: now.Add(3 * time.Hour), Session: "controller-1", SetAt: setAt},
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ReplaceMergeHolds(ctx, tx, "LEGION", first))
		must(t, records.ReplaceMergeHolds(ctx, tx, "INFRA", []MergeHold{
			{Paths: []string{"infra/"}, Reason: "another project's list", EndsAt: now.Add(time.Hour), Session: "controller-2", SetAt: setAt},
		}))
		holds, err := records.MergeHolds(ctx, tx, "LEGION", now)
		must(t, err)
		if len(holds) != 3 {
			t.Fatalf("the holds of LEGION = %+v, want the three the list was replaced with", holds)
		}
		for i, hold := range holds {
			if !reflect.DeepEqual(hold.Paths, first[i].Paths) || hold.Repository != first[i].Repository || hold.Reason != first[i].Reason {
				t.Fatalf("hold %d = %+v, want %+v: the list comes back in the order it was given", i, hold, first[i])
			}
			if !hold.EndsAt.Equal(first[i].EndsAt) || hold.EndsAt.Location() != time.UTC {
				t.Fatalf("hold %d ends at %s in %s, want %s in UTC", i, hold.EndsAt, hold.EndsAt.Location(), first[i].EndsAt)
			}
			if !hold.SetAt.Equal(setAt) || hold.SetAt.Location() != time.UTC {
				t.Fatalf("hold %d was set at %s in %s, want %s in UTC", i, hold.SetAt, hold.SetAt.Location(), setAt)
			}
		}
	})

	// The second replacement is the whole list: the one hold it carries is what stands, and the
	// other two are gone. Another project's list is untouched by either.
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ReplaceMergeHolds(ctx, tx, "LEGION", []MergeHold{
			{Paths: []string{"packages/daemon/internal/record/"}, Reason: "the record is being changed", EndsAt: now.Add(4 * time.Hour), Session: "controller-4", SetAt: now},
		}))
		holds, err := records.MergeHolds(ctx, tx, "LEGION", now)
		must(t, err)
		if len(holds) != 1 || holds[0].Reason != "the record is being changed" {
			t.Fatalf("the holds of LEGION after the second replacement = %+v, want only the hold the second list carried", holds)
		}
		if holds[0].Repository != "" {
			t.Fatalf("a hold that names no repository came back as %q, want \"\": every repository the project serves", holds[0].Repository)
		}
		if rows := rowsOf(tx, "INFRA"); rows != 1 {
			t.Fatalf("another project holds %d rows after LEGION's list was replaced twice, want its own one", rows)
		}
	})

	// An ended hold: the live read lists it by neither name, and only the writing read deletes it.
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ReplaceMergeHolds(ctx, tx, "LEGION", []MergeHold{
			{Paths: []string{"packages/cli/"}, Reason: "the hold that has ended", EndsAt: now.Add(-time.Minute), Session: "controller-5", SetAt: setAt},
			{Paths: []string{"packages/daemon/"}, Reason: "the hold still in force", EndsAt: now.Add(time.Hour), Session: "controller-5", SetAt: setAt},
		}))
		live, err := records.LiveMergeHolds(ctx, tx, "LEGION", now)
		must(t, err)
		if len(live) != 1 || live[0].Reason != "the hold still in force" {
			t.Fatalf("the live holds of LEGION = %+v, want the one still in force", live)
		}
		if rows := rowsOf(tx, "LEGION"); rows != 2 {
			t.Fatalf("LEGION holds %d rows after the live read, want both: that read writes nothing", rows)
		}
		holds, err := records.MergeHolds(ctx, tx, "LEGION", now)
		must(t, err)
		if len(holds) != 1 || holds[0].Reason != "the hold still in force" {
			t.Fatalf("the holds of LEGION = %+v, want the one still in force", holds)
		}
		if rows := rowsOf(tx, "LEGION"); rows != 1 {
			t.Fatalf("LEGION holds %d rows after the writing read, want the ended hold deleted", rows)
		}
	})
}

// The record keeps the newest jump of each repository: a later jump of one repository replaces
// its record, and a second repository keeps a record of its own. The queue snapshot, the counts
// and a refusal come back as they were written, and the jump's time in UTC.
func TestAJumpRecordIsReplacedPerRepository(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// A time in another location, to read the jump's own time back in UTC.
	at := time.Date(2026, 11, 10, 8, 0, 0, 0, time.FixedZone("UTC+1", 60*60))
	entries := []MergeQueueEntryRecord{
		{Number: 7, Position: 0, State: "QUEUED", EnqueuedAt: at.Add(-time.Hour)},
		{Number: 9, Position: 1, Jump: true, State: "MERGEABLE", EnqueuedAt: at.Add(-30 * time.Minute)},
	}
	// The snapshot comes back with each entry's time in UTC, as every time this package answers
	// is, so what is written in another location is compared against its UTC spelling.
	inUTC := func(written []MergeQueueEntryRecord) []MergeQueueEntryRecord {
		read := make([]MergeQueueEntryRecord, len(written))
		for i, entry := range written {
			entry.EnqueuedAt = entry.EnqueuedAt.UTC()
			read[i] = entry
		}
		return read
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutMergeJumpRecord(ctx, tx, "LEGION", MergeJumpRecord{
			Repository: "acme/widgets", Issue: "LEGION-208", Number: 42, Head: "c0de", At: at,
			Reason: "the release waits on it", Entries: entries, EntriesTotal: len(entries),
		}))
		must(t, records.PutMergeJumpRecord(ctx, tx, "LEGION", MergeJumpRecord{
			Repository: "acme/other", Issue: "LEGION-209", Number: 43, Head: "beef", At: at,
			Reason: "another repository of the project", EntriesUnknown: true, Refused: true,
			Refusal: "GitHub refused the jump",
		}))
		must(t, records.PutMergeJumpRecord(ctx, tx, "INFRA", MergeJumpRecord{
			Repository: "acme/widgets", Issue: "INFRA-7", Number: 44, Head: "f00d", At: at,
			Reason: "another project's record",
		}))
	})

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutMergeJumpRecord(ctx, tx, "LEGION", MergeJumpRecord{
			Repository: "acme/widgets", Issue: "LEGION-210", Number: 45, Head: "cafe", At: at.Add(time.Hour),
			Reason: "the later jump of the same repository", Entries: entries[:1], EntriesTotal: 1,
		}))
		kept, err := records.MergeJumpRecords(ctx, tx, "LEGION")
		must(t, err)
		if len(kept) != 2 {
			t.Fatalf("the jump records of LEGION = %+v, want one per repository", kept)
		}
		other, widgets := kept[0], kept[1]
		if other.Repository != "acme/other" || widgets.Repository != "acme/widgets" {
			t.Fatalf("the jump records came as %s then %s, want them by repository", other.Repository, widgets.Repository)
		}
		if widgets.Issue != "LEGION-210" || widgets.Number != 45 || widgets.Head != "cafe" {
			t.Fatalf("the record of acme/widgets = %+v, want the later jump of that repository", widgets)
		}
		if !reflect.DeepEqual(widgets.Entries, inUTC(entries[:1])) || widgets.EntriesTotal != 1 || widgets.EntriesUnknown {
			t.Fatalf("the queue snapshot of acme/widgets = %+v (%d, unknown %v), want the one entry written",
				widgets.Entries, widgets.EntriesTotal, widgets.EntriesUnknown)
		}
		if !widgets.At.Equal(at.Add(time.Hour)) || widgets.At.Location() != time.UTC {
			t.Fatalf("the record's time = %s in %s, want %s in UTC", widgets.At, widgets.At.Location(), at.Add(time.Hour))
		}
		if len(other.Entries) != 0 || !other.EntriesUnknown || !other.Refused || other.Refusal != "GitHub refused the jump" {
			t.Fatalf("the record of acme/other = %+v, want an unread queue and GitHub's refusal", other)
		}
	})
}

// A refusal lands only on the record its own jump wrote. Two issues of one repository can jump
// close together and the record keeps one row per repository, so an older jump whose refusal
// comes back after the newer jump has written that row would, written unconditionally, show the
// wrong newest jump and mark it refused. The older jump's refusal answers false and writes
// nothing; the refusal of the row's own jump — by its issue and its head — is written, and
// nothing else of the record moves.
func TestAJumpRefusalWritesOnlyOnTheRecordItsOwnJumpWrote(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	older := MergeJumpRecord{Repository: "acme/widgets", Issue: "LEGION-299", Number: 41, Head: "beef", At: at,
		Reason: "the older jump of this repository"}
	newer := MergeJumpRecord{Repository: "acme/widgets", Issue: "LEGION-300", Number: 42, Head: "c0de",
		At: at.Add(time.Minute), Reason: "the newer jump of this repository"}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutMergeJumpRecord(ctx, tx, "LEGION", older))
		must(t, records.PutMergeJumpRecord(ctx, tx, "LEGION", newer))
	})

	inTx(t, st, func(tx pgx.Tx) {
		refused := older
		refused.Refused, refused.Refusal = true, "GitHub refused the older jump"
		wrote, err := records.RefuseMergeJumpRecord(ctx, tx, "LEGION", refused)
		must(t, err)
		if wrote {
			t.Fatal("the older jump's refusal was written, want the newer jump's record left as it stands")
		}
		kept, err := records.MergeJumpRecords(ctx, tx, "LEGION")
		must(t, err)
		if len(kept) != 1 || kept[0].Issue != "LEGION-300" || kept[0].Refused || kept[0].Refusal != "" {
			t.Fatalf("the record of acme/widgets = %+v, want the newer jump of LEGION-300 with no refusal", kept)
		}
	})

	// A refusal of the row's own jump is written, which is every refusal that comes back before
	// another issue of the repository jumps, and it moves nothing but refused and refusal.
	inTx(t, st, func(tx pgx.Tx) {
		refused := newer
		refused.Refused, refused.Refusal = true, "GitHub refused the newer jump"
		wrote, err := records.RefuseMergeJumpRecord(ctx, tx, "LEGION", refused)
		must(t, err)
		if !wrote {
			t.Fatal("the refusal of the record's own jump was not written, want it on the row that jump wrote")
		}
		kept, err := records.MergeJumpRecords(ctx, tx, "LEGION")
		must(t, err)
		if len(kept) != 1 || !kept[0].Refused || kept[0].Refusal != "GitHub refused the newer jump" {
			t.Fatalf("the record of acme/widgets = %+v, want its own jump's refusal", kept)
		}
		if kept[0].Issue != "LEGION-300" || kept[0].Number != 42 || kept[0].Head != "c0de" || kept[0].Reason != newer.Reason {
			t.Fatalf("the refused record = %+v, want everything else as the jump itself wrote it", kept[0])
		}
	})

	// The same issue at another head is another jump, and its refusal lands no more than another
	// issue's does: a head is half of what tells one jump of a repository from the next.
	inTx(t, st, func(tx pgx.Tx) {
		refused := newer
		refused.Head = "cafe"
		refused.Refused, refused.Refusal = true, "GitHub refused the jump at another head"
		wrote, err := records.RefuseMergeJumpRecord(ctx, tx, "LEGION", refused)
		must(t, err)
		if wrote {
			t.Fatal("a refusal naming another head was written, want the record of the head it holds left alone")
		}
		kept, err := records.MergeJumpRecords(ctx, tx, "LEGION")
		must(t, err)
		if len(kept) != 1 || kept[0].Head != "c0de" || kept[0].Refusal != "GitHub refused the newer jump" {
			t.Fatalf("the record of acme/widgets = %+v, want the record of c0de with its own refusal", kept)
		}
	})
}

// A merge a hold keeps back is a ready_merge row whose payload carries the moment the hold caught
// it. The read answers those rows of the project alone, each with its own next_at as the moment
// it is tried again: a ready_merge row no hold caught carries no heldSince and is not listed, and
// another project's held row is not either.
func TestMergeHeldIssuesAnswersTheReadyMergeRowsAHoldCaught(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	due := time.Date(2026, 11, 10, 15, 0, 0, 0, time.FixedZone("UTC+3", 3*60*60))
	// The payload is written here as JSON rather than through record.ReadyMerge: the held moment
	// is a field of that payload the submission path owns, and this read asks the stored key.
	queue := func(tx pgx.Tx, issue, payload string, nextAt time.Time) {
		t.Helper()
		_, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('ready_merge', $1, $2::jsonb, 0, $3, '')`, issue, payload, nextAt)
		must(t, err)
	}

	inTx(t, st, func(tx pgx.Tx) {
		queue(tx, "LEGION-208", `{"number": 42, "head": "c0de", "heldSince": "2026-11-10T11:00:00Z"}`, due)
		queue(tx, "LEGION-209", `{"number": 43, "head": "beef"}`, due)
		queue(tx, "INFRA-7", `{"number": 44, "head": "f00d", "heldSince": "2026-11-10T11:00:00Z"}`, due)
	})

	inTx(t, st, func(tx pgx.Tx) {
		held, err := records.MergeHeldIssues(ctx, tx, "LEGION")
		must(t, err)
		if len(held) != 1 || held[0].Issue != "LEGION-208" {
			t.Fatalf("the held merges of LEGION = %+v, want the one row whose payload carries a held moment", held)
		}
		if !held[0].DueAt.Equal(due) || held[0].DueAt.Location() != time.UTC {
			t.Fatalf("the held merge is due at %s in %s, want %s in UTC: the row's own next_at", held[0].DueAt, held[0].DueAt.Location(), due)
		}
	})
}
