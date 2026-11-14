package workflow

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A silent phase worker whose claim stands behind the phase entry its row names is sent the
// work of that entry at that entry (repairSilence, OwedEntry, sendOwedEntry), as the ready of
// such a claim is (TestTheReadyOfAWorkerBehindItsEntrySendsThatEntrysWork, readyWorkerTask) and
// the boot's resume of one is (TestTheBootsResumeOfAWorkerBehindItsEntrySendsThatEntrysWork):
// the phase's resume task says to carry the existing phase work on, and the work of an entry
// this claim was never given is work it does not have, so the resume would spend the turn on the
// wrong prompt and the completion it answered with would be refused as behind that entry. A
// silent claim level with its row is sent the phase's resume task at no entry, marked as the
// resume, since it is on the entry its row names already. Neither start moves the row.
func TestASilentWorkerIsSentTheWorkOfTheEntryItIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name string
		// given is the entry the silent claim was last given work of (record.RoleClaim.GivenEntry).
		given int64
		// want is the task the repair's start must carry, entry the entry that start must be of
		// and resume whether it is marked the phase's resume task: the row's own entry and work
		// for a claim behind it, and no entry with the resume for a claim level with it.
		want   func(record.Issue) string
		entry  int64
		resume bool
		// did is what the architect's notice must say the daemon wrote.
		did string
	}{
		{name: "behind the entry its row names", given: silenceEntry - 1,
			want: func(record.Issue) string { return silenceEntryTask }, entry: silenceEntry,
			did: fmt.Sprintf("sends the work of phase entry %d its phase row names, which its claim, last given entry %d, never had",
				silenceEntry, silenceEntry-1)},
		{name: "level with the entry its row names", given: silenceEntry,
			want: ResumePhaseTask, entry: 0, resume: true,
			did: fmt.Sprintf("sends the phase's task again, on phase entry %d its phase row names", silenceEntry)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			rec := aSilentTester()
			rec.given = tc.given
			issue := seedSilence(t, pool, rec)

			if err := applySilence(t, pool, testEngine(config.DesignGateRootIssues, nil), aSilenceOf(claim.RoleTester, 1)); err != nil {
				t.Fatalf("apply the silence: %v", err)
			}

			starts := superviseRowsOf(t, pool, silenceIssue, "start")
			if len(starts) != 1 {
				t.Fatalf("the silence queued %+v, want the one start it writes for the silent claim", starts)
			}
			start := starts[0]
			if start.Task != tc.want(issue) || start.Entry != tc.entry || start.ResumeTask != tc.resume ||
				start.ResendEntry || start.RuleResume || !start.NeedsSlot {
				t.Fatalf("the silence of a claim given entry %d under a row at entry %d sent %q at entry %d, resume task %t, resend %t, rule resume %t, needs slot %t; want %q at entry %d, resume task %t, no resend, no rule resume, waiting for the slot",
					tc.given, silenceEntry, start.Task, start.Entry, start.ResumeTask, start.ResendEntry, start.RuleResume, start.NeedsSlot,
					tc.want(issue), tc.entry, tc.resume)
			}
			notices := noticesOfKind(t, pool, silenceIssue, workerSilentNotice)
			if len(notices) != 1 {
				t.Fatalf("the %s notices of %s = %d, want the one its architect is owed", workerSilentNotice, silenceIssue, len(notices))
			}
			namesTheSilence(t, notices[0].Reason, tc.did, "")
			var row record.PhaseRow
			seedRecord(t, pool, func(tx pgx.Tx) error {
				rows, err := record.NewStore().Phases(t.Context(), tx, silenceIssue)
				row = record.PhaseRowOf(rows, claim.RoleTester)
				return err
			})
			if row.Entry != silenceEntry || row.EntryTask != silenceEntryTask {
				t.Fatalf("after the silence the tester's row is at entry %d carrying %q, want the entry %d and work %q it stood at: the repair hands no new entry",
					row.Entry, row.EntryTask, silenceEntry, silenceEntryTask)
			}
		})
	}
}
