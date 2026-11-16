package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A claim the daemon suspended by rule that stands behind the entry its phases row names is
// started by the boot on the work of that entry, as the ready of such a claim is
// (TestTheReadyOfAWorkerBehindItsEntrySendsThatEntrysWork, readyWorkerTask): the phase's resume
// task says to carry the existing phase work on, and the work of an entry this claim was never
// given is work it does not have. Sent the resume, the worker spends its first turn after the boot
// on the wrong prompt, and the completion it answers with is refused as behind that entry, so the
// entry's own task reaches it only on the next admission pass or entry sweep. A claim level with
// its row is resumed as before, at no entry, since it is on the entry its row names already.
func TestTheBootsResumeOfAWorkerBehindItsEntrySendsThatEntrysWork(t *testing.T) {
	issue := record.Issue{Key: entryIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: entryIssue,
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true}
	for _, tc := range []struct {
		name string
		// given is the entry the suspended claim was last queued a task of
		// (record.RoleClaim.GivenEntry).
		given int64
		// want is the task the boot's start must carry, and entry the entry that start must be
		// of: the row's own for the work it names, and none for the resume, which hands the claim
		// no work it was not already on.
		want  string
		entry int64
	}{
		{name: "behind the entry its row names", given: seededEntry - 1, want: seededEntryTask, entry: seededEntry},
		{name: "level with the entry its row names", given: seededEntry, want: ResumePhaseTask(issue), entry: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := t.Context()
			project, err := claim.ProjectToken("LEGION")
			if err != nil {
				t.Fatal(err)
			}
			seedIssue(t, pool, issue)
			seedSlot(t, pool, record.Slot{Issue: entryIssue, Index: 0})
			token, err := claim.NewToken(project, entryIssue, claim.RoleImplementer)
			if err != nil {
				t.Fatal(err)
			}
			seedEntryRow(t, pool, claim.RoleImplementer, token)
			if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
				values ($1, $2, 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
				string(token), project); err != nil {
				t.Fatalf("seed the implementer the daemon suspended: %v", err)
			}
			seedGivenEntry(t, pool, token, tc.given)

			if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the boot: %v", err)
			}

			starts := superviseRowsOf(t, pool, entryIssue, "start")
			if len(starts) != 1 {
				t.Fatalf("the boot queued %+v, want the one start it writes for the suspended claim", starts)
			}
			start := starts[0]
			if start.Task != tc.want || start.Entry != tc.entry {
				t.Fatalf("the boot's start of a claim given entry %d under a row at entry %d sent %q at entry %d; want %q at entry %d",
					tc.given, seededEntry, start.Task, start.Entry, tc.want, tc.entry)
			}
			assertEntryUntouched(t, pool, claim.RoleImplementer, "the boot")
		})
	}
}
