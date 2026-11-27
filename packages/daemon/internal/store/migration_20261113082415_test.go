package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
// A death's kind round-trips with the record (EndedWork.Death): the out-of-memory note reads it.
func TestARelaunchRecordsItsEndedWorkOnTheClaimRow(t *testing.T) {
	t.Parallel()
	t.Run("the record round-trips with the claim, and its absence is null", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-209-implementer")
		want.EndedWork = supervise.EndedWork{
			At: at(5), Cause: "the pod's roles container, which runs every role of this issue, was killed because it used " +
				`more than its 8 GiB memory limit. The runtime's detail: "gone: pod legion-legion-209 (uid 8a1f) ended: OOMKilled"`,
			Death: runtime.DeathMemoryLimit, Generation: 3, Turn: true, TaskTurn: true, Background: true,
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

	// Each judgement of a launch's end types its record (supervise.EndedWork's FromOutside, Died,
	// Displaced and Outlived, with SentAgain, the task a running read sends again, TookBack, the
	// id the take-back of a death whose record joined an earlier launch's gave the task, and
	// Earlier, that earlier launch's record, still untold, which a running read puts back), and
	// the judgement a later report or a restarted daemon routes by is that type: so the cause the
	// role is told and the type the launch was judged under go into the column with the rest of
	// the record and come back from it as they went in.
	const memory = "the pod's roles container, which runs every role of this issue, was killed because it used " +
		`more than its 8 GiB memory limit. The runtime's detail: "gone: OOMKilled"`
	for _, row := range []struct {
		name  string
		ended supervise.EndedWork
	}{
		{"an end from outside the pod", supervise.EndedWork{At: at(6), Cause: "its pod was ended from outside it",
			FromOutside: true, Generation: 3, Turn: true, TaskTurn: true, Reported: true}},
		{"a death died judged", supervise.EndedWork{At: at(7), Cause: memory,
			Death: runtime.DeathMemoryLimit, Died: true, Generation: 3, TaskTurn: true, Reported: true}},
		{"a displacement, told already by no delivery", supervise.EndedWork{At: at(8), Cause: "its pod was replaced",
			Displaced: true, Generation: 3, Told: true}},
		{"a death a running read ended, its task sent again", supervise.EndedWork{At: at(9), Cause: `the process died: "not_recorded_process"`,
			Outlived: true, SentAgain: "delivery-2", Generation: 3, Told: true}},
		{"a death joined over an earlier launch's untold record", supervise.EndedWork{At: at(10), Cause: `the process died: "gone"`,
			Died: true, TookBack: "delivery-3", TookBackEarlierTask: true, TookBackEarlierOutOfMemory: true,
			Generation: 3, Turn: true, TaskTurn: true, Background: true, Kinds: []string{"subagent"},
			Earlier: &supervise.EndedWork{At: at(4), Cause: memory, Death: runtime.DeathMemoryLimit, Died: true,
				Generation: 2, TaskTurn: true, Background: true, Kinds: []string{"subagent"}, Reported: true}}},
		{"an earlier launch's untold record a running read put back, its task sent again", supervise.EndedWork{At: at(4), Cause: memory,
			Death: runtime.DeathMemoryLimit, Died: true, SentAgain: "delivery-3", Generation: 2, TaskTurn: true, Reported: true}},
	} {
		t.Run("the record of "+row.name+" round-trips with its judgement's type", func(t *testing.T) {
			ctx := context.Background()
			store := migratedStore(t)
			want := tmuxClaim("legion-LEGION-209-implementer")
			want.EndedWork = row.ended
			if err := store.PutClaim(ctx, want); err != nil {
				t.Fatalf("put the claim with its record: %v", err)
			}
			got := onlyClaim(t, store)
			sameClaim(t, got, want)
			if e := got.EndedWork; e.Cause != row.ended.Cause || e.FromOutside != row.ended.FromOutside || e.Died != row.ended.Died ||
				e.Displaced != row.ended.Displaced || e.Outlived != row.ended.Outlived || e.SentAgain != row.ended.SentAgain ||
				e.TookBack != row.ended.TookBack || e.Told != row.ended.Told || e.Generation != row.ended.Generation ||
				(e.Earlier == nil) != (row.ended.Earlier == nil) ||
				e.Earlier != nil && (e.Earlier.Cause != row.ended.Earlier.Cause || e.Earlier.Death != row.ended.Earlier.Death ||
					e.Earlier.Generation != row.ended.Earlier.Generation || !e.Earlier.At.Equal(row.ended.Earlier.At)) {
				t.Fatalf("the claim read back records %+v, want %+v: its cause and its judgement's type as written", e, row.ended)
			}
		})
	}

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

		var null bool
		if err := store.Pool().QueryRow(ctx, "select ended_work is null from claims where token = $1", token).Scan(&null); err != nil {
			t.Fatalf("read the column after the migration: %v", err)
		}
		if !null {
			t.Error("the stored claim holds a record after the migration, want null")
		}
		if read := onlyClaim(t, store); read.EndedWork.Cause != "" {
			t.Errorf("the stored claim reads the record %+v after the migration, want none", read.EndedWork)
		}
	})
}
