package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// endedWorkVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const endedWorkVersion = 20261113082415

// What a relaunch ended is recorded on the claim's own row (supervise.Claim.EndedWork), because the
// prompt that tells the next process can be a relaunch, a wake and a daemon restart away. The round
// trip is the column through putClaim and scanClaim; the read the machine records the background
// work from is the activity report of the launch it lets go, and of no other (ClaimActivity); and
// the migration is read back under both: every row the table already holds takes null.
func TestARelaunchRecordsItsEndedWorkOnTheClaimRow(t *testing.T) {
	t.Run("the record round-trips with the claim, and its absence is null", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-209-implementer")
		want.EndedWork = supervise.EndedWork{
			At: at(5), Cause: "the process died: gone: pod legion-legion-209 (uid 8a1f) ended: OOMKilled",
			Generation: 3, Turn: true, TaskTurn: true, Background: true,
			Kinds: []string{"subagent", "async-job"}, Reported: true,
		}
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with its record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// A record whose process left no report says so, and one that has been told says that.
		want.EndedWork.Background, want.EndedWork.Kinds, want.EndedWork.Reported = false, nil, false
		want.EndedWork.Told = true
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with its told record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// A claim that records none writes null, not an empty record.
		want.EndedWork = supervise.EndedWork{}
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with no record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
		var null bool
		if err := store.Pool().QueryRow(ctx, "select ended_work is null from claims where token = $1",
			string(want.Token)).Scan(&null); err != nil {
			t.Fatalf("read the column: %v", err)
		}
		if !null {
			t.Error("a claim that records no ended work wrote a record, want null")
		}
	})

	t.Run("the read answers the report of the launch it names and of no other", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		c := tmuxClaim("legion-LEGION-210-implementer")
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put claim: %v", err)
		}
		if report, held, err := store.ClaimActivity(ctx, c.Token, c.Generation); err != nil || held {
			t.Fatalf("the launch that has reported nothing reads %+v, %t, %v; want no report", report, held, err)
		}

		reported := claim.SessionActivity{At: at(10),
			BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"subagent", "async-job"}}}
		if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, reported); err != nil || !known {
			t.Fatalf("PutClaimActivity = %t, %v", known, err)
		}
		report, held, err := store.ClaimActivity(ctx, c.Token, c.Generation)
		if err != nil || !held || !report.At.Equal(at(10)) || !report.Background ||
			!slices.Equal(report.Kinds, []string{"subagent", "async-job"}) {
			t.Fatalf("the launch's own report reads %+v, %t, %v; want the subagent and the job it named at %s",
				report, held, err, at(10))
		}
		if report, held, err := store.ClaimActivity(ctx, c.Token, c.Generation-1); err != nil || held {
			t.Fatalf("an earlier launch reads %+v, %t, %v; want no report: the row's report is its own launch's", report, held, err)
		}

		// The launch that replaces the process moves the generation and clears the report, so
		// neither launch reads one any more.
		c.Generation++
		c.State = supervise.StateLaunching
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("record the launch: %v", err)
		}
		for _, generation := range []uint64{c.Generation - 1, c.Generation} {
			if report, held, err := store.ClaimActivity(ctx, c.Token, generation); err != nil || held {
				t.Errorf("launch %d after the relaunch reads %+v, %t, %v; want no report", generation, report, held, err)
			}
		}
		if report, held, err := store.ClaimActivity(ctx, "legion-LEGION-999-implementer", 1); err != nil || held {
			t.Errorf("a claim the store does not hold reads %+v, %t, %v; want no report", report, held, err)
		}
	})

	t.Run("the migration gives every stored claim no record", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, endedWorkVersion-1)

		const token = "legion-LEGION-211-implementer"
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', 'idle', '', 0, 0, 0, 0)`, token); err != nil {
			t.Fatalf("record %s before the migration: %v", token, err)
		}
		_, err := store.Pool().Exec(ctx, `update claims set ended_work = '{}'::jsonb`)
		if err == nil || !strings.Contains(err.Error(), "ended_work") {
			t.Fatalf("writing ended_work before the migration = %v, want the column to be unknown", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the ended-work record: %v", err)
		}

		read := onlyClaim(t, store)
		if read.EndedWork.Cause != "" || read.EndedWork.Tells() {
			t.Errorf("the stored claim reads the record %+v after the migration, want none", read.EndedWork)
		}
	})
}
