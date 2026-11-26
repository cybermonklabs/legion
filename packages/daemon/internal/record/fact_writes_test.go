package record

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The end rule of a step's run reads three inputs the store writes - the issue's record, its pull
// request and its hand-over fence - and the fact's settle reads the runs of each issue its
// transaction wrote one of them for (IssuesWritten). So each writer of those inputs keeps its
// issue, once, in the order of its first write, in a ctx that keeps writes (WithIssueWrites); a
// writer that kept none would leave the run its write ends unended. A ctx that keeps no writes
// keeps nothing, which is the control.
func TestEachWriterOfAnEndInputKeepsItsIssue(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	keys := []string{"LEGION-301", "LEGION-302", "LEGION-303", "LEGION-304", "LEGION-305"}
	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range keys {
			must(t, store.PutIssue(t.Context(), tx, issueFixture(key)))
		}
	})
	write := func(ctx context.Context, tx pgx.Tx) {
		t.Helper()
		must(t, store.PutIssue(ctx, tx, issueFixture(keys[0])))
		must(t, store.PutPullRequest(ctx, tx, PullRequest{Issue: keys[1], Repo: "sjawhar/legion", Number: 1302, Branch: "legion/" + keys[1],
			HeadSHA: "b1", State: PullRequestOpen, Failing: []string{}, Cancelled: []string{}}))
		must(t, store.PutHandover(ctx, tx, Handover{Issue: keys[2], Tree: keys[2], Generation: 1}))
		must(t, store.DeleteHandover(ctx, tx, keys[3]))
		must(t, store.DeleteHandoverAt(ctx, tx, keys[4], keys[4], 1))
		must(t, store.PutIssue(ctx, tx, issueFixture(keys[0])))
	}
	inTx(t, st, func(tx pgx.Tx) {
		kept := WithIssueWrites(t.Context())
		write(kept, tx)
		if got := IssuesWritten(kept); !slices.Equal(got, keys) {
			t.Fatalf("the issues the writes kept = %v, want each writer's issue once, in order: %v", got, keys)
		}
		plain := t.Context()
		write(plain, tx)
		if got := IssuesWritten(plain); got != nil {
			t.Fatalf("a ctx that keeps no writes kept %v, want nothing", got)
		}
	})
}

// A fact runs the admission pass only where its transaction wrote a record of the project that
// admission reads (RecordsWritten). Every writer of those rows keeps the mark in a ctx that keeps
// writes when its statement matches a row, a row set to the value it held among them, and only
// the writers of an issue's record, pull request and hand-over fence name the issue among
// IssuesWritten, which the end of a step's run reads: the movement and the phase rows a late
// handoff writes keep the mark and name no issue. The controls are a statement that matches no
// row (a fence that refuses it, a key that names none, the branch a task holds already), the
// repository backfill, which runs outside any fact, a name among those a generation gave, which
// decides no pass (RecordBranchGiven), and a ctx that keeps no writes. Each case runs in a
// transaction of its own, rolled back, after a seed of its own made without the mark.
func TestEachWriterOfARecordThePassReadsKeepsTheMark(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const key, generation = "LEGION-310", 3
	at := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	reading := DispatchReading{Key: key, Project: "LEGION", Seq: 5, Status: Carry("todo"), ClaimedBy: "session:ses-1", ClaimSeq: 5}
	ask := RootAsk{Issue: key, Ask: "ask-q", Kind: AskQuestion, Open: true, Seq: 5, WaitingOn: "human", WaitingOnSeq: 5}
	older := ask
	older.Seq, older.WaitingOn, older.WaitingOnSeq = 4, "agent", 4
	gate := DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "doc-spec", LatestVersion: 1}
	phaseRow := PhaseRow{Issue: key, Role: claim.RoleTester, Claim: "legion-LEGION-310-tester"}
	slot := Slot{Issue: key, Index: 7, AdmittedAt: at}
	type step func(t *testing.T, ctx context.Context, tx pgx.Tx)
	seedPlan := func(t *testing.T, ctx context.Context, tx pgx.Tx) {
		v, tasks, steps := ownersPlan(key, generation, 1, "architect-310", planGivenAt)
		must(t, store.PutPlan(ctx, tx, v, tasks, steps))
	}
	seedRunning := func(t *testing.T, ctx context.Context, tx pgx.Tx) {
		seedPlan(t, ctx, tx)
		must(t, store.MoveTask(ctx, tx, key, generation, "a", TaskPlace{Kind: PlaceStep, Position: 1}, TaskRunning))
	}
	pr := PullRequest{Issue: key, Repo: "acme/widgets", Number: 1310, Branch: "legion/" + key,
		HeadSHA: "b1", State: PullRequestOpen, Failing: []string{}, Cancelled: []string{}}
	sql := func(statement string) step {
		return func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			_, err := tx.Exec(ctx, statement)
			must(t, err)
		}
	}
	for _, tc := range []struct {
		name        string
		seed, write step
		// marks is whether the write keeps the mark, and issue whether it names key among the
		// issues written.
		marks, issue bool
	}{
		{name: "an issue's record", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutIssue(ctx, tx, issueFixture(key)))
		}, marks: true, issue: true},
		{name: "its movement", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.MarkIssueMoved(ctx, tx, key, at))
		}, marks: true},
		{name: "control: a movement older than the one recorded", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.MarkIssueMoved(ctx, tx, key, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
		}},
		{name: "control: the movement of an issue with no record", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.MarkIssueMoved(ctx, tx, "LEGION-999", at))
		}},
		{name: "its stalled tree told", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.MarkTreeStalledTold(ctx, tx, key, at))
		}, marks: true},
		{name: "a phase row", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutPhase(ctx, tx, phaseRow))
		}, marks: true},
		{name: "a clear of its generation", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutPhase(ctx, tx, phaseRow))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearGeneration(ctx, tx, key, at))
		}, marks: true},
		{name: "a clear of its tree's generation", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutPhase(ctx, tx, phaseRow))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearTreeGeneration(ctx, tx, key, at))
		}, marks: true},
		{name: "a clear that deletes a gate alone", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutGate(ctx, tx, gate))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearGeneration(ctx, tx, key, at))
		}, marks: true},
		{name: "control: a clear of a generation that holds nothing", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearGeneration(ctx, tx, key, at))
		}},
		{name: "a handout", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.StartHandout(ctx, tx, key))
		}, marks: true},
		{name: "a restart of the catch-up cursor", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.RestartCaughtUp(ctx, tx, key))
		}, marks: true},
		{name: "a hand-back declared", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.Declare(ctx, tx, key, EventMark{Seq: 50, At: at}))
		}, marks: true},
		{name: "the hand-backs taken in", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.TakeInThrough(ctx, tx, key, 50))
		}, marks: true},
		{name: "a hand-back cleared", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearDeclared(ctx, tx, key))
		}, marks: true},
		{name: "the catch-up cursor raised", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.SetCaughtUp(ctx, tx, key, 50))
		}, marks: true},
		{name: "a slot", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutSlot(ctx, tx, slot))
		}, marks: true},
		{name: "a slot released", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutSlot(ctx, tx, slot))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ReleaseSlot(ctx, tx, key))
		}, marks: true},
		{name: "control: the release of a slot nobody holds", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ReleaseSlot(ctx, tx, key))
		}},
		{name: "a slot released for a wait", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutSlot(ctx, tx, slot))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ReleaseSlotForWait(ctx, tx, key, at))
		}, marks: true},
		{name: "the release for a wait cleared", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearReleasedForWait(ctx, tx, key))
		}, marks: true},
		{name: "a tree's slots released", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutSlot(ctx, tx, slot))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			_, err := store.ReleaseTreeSlots(ctx, tx, key)
			must(t, err)
		}, marks: true},
		{name: "control: the release of a tree that holds no slot and no release", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			_, err := store.ReleaseTreeSlots(ctx, tx, key)
			must(t, err)
		}},
		{name: "members owed", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.OweMembers(ctx, tx, key))
		}, marks: true},
		{name: "members owed cleared", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ClearMembersOwed(ctx, tx, key))
		}, marks: true},
		{name: "control: the repository backfill, outside any fact",
			seed: sql(`update issues set github_repo = '' where key = 'LEGION-310'`),
			write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
				must(t, store.NameIssueRepository(ctx, tx, key, "acme/widgets", "Acme/Widgets"))
			}},
		{name: "a pull request", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutPullRequest(ctx, tx, PullRequest{Issue: key, Repo: "acme/widgets", Number: 1310, Branch: "legion/" + key,
				HeadSHA: "b1", State: PullRequestOpen, Failing: []string{}, Cancelled: []string{}}))
		}, marks: true, issue: true},
		{name: "a hand-over fence", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutHandover(ctx, tx, Handover{Issue: key, Tree: key, Generation: 1}))
		}, marks: true, issue: true},
		{name: "a tree's hand-overs cancelled", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutHandover(ctx, tx, Handover{Issue: key, Tree: key, Generation: 1}))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.CancelTreeHandovers(ctx, tx, key, 1))
		}, marks: true},
		{name: "a reading of Dispatch's issue", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchIssue(ctx, tx, reading))
		}, marks: true},
		{name: "control: the same reading of Dispatch's issue again", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchIssue(ctx, tx, reading))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchIssue(ctx, tx, reading))
		}},
		{name: "a newer claim", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchClaim(ctx, tx, key, "session:ses-1", 5))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchClaim(ctx, tx, key, "session:ses-2", 6))
		}, marks: true},
		{name: "control: an older claim", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchClaim(ctx, tx, key, "session:ses-1", 5))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchClaim(ctx, tx, key, "session:ses-0", 4))
		}},
		{name: "the refill of the copy of Dispatch",
			seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
				must(t, store.PutDispatchIssue(ctx, tx, reading))
				sql(`insert into dispatch_issue_refills (project) values ('LEGION') on conflict (project) do update set refilled_at = null`)(t, ctx, tx)
			}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
				_, _, err := store.TakeDispatchRefill(ctx, tx, "LEGION")
				must(t, err)
			}, marks: true},
		{name: "a root's ask", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutRootAsk(ctx, tx, ask))
		}, marks: true},
		{name: "control: an older reading of the ask", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutRootAsk(ctx, tx, ask))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutRootAsk(ctx, tx, older))
		}},
		{name: "a design gate", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutGate(ctx, tx, gate))
		}, marks: true},
		{name: "a plan", write: seedPlan, marks: true},
		{name: "a plan carried to the next generation", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.CarryPlan(ctx, tx, key, generation, generation+1, false))
		}, marks: true},
		{name: "a plan carried with its run", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.CarryPlan(ctx, tx, key, generation, generation+1, true))
		}, marks: true},
		{name: "a plan deleted", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.DeletePlan(ctx, tx, key, generation))
		}, marks: true},
		{name: "control: the delete of a plan nobody gave", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.DeletePlan(ctx, tx, key, generation))
		}},
		{name: "a plan's task deleted", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.DeleteTasks(ctx, tx, key, generation, []string{"a"}))
		}, marks: true},
		{name: "a plan's task moved", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.MoveTask(ctx, tx, key, generation, "a", TaskPlace{Kind: PlaceStep, Position: 1}, TaskRunning))
		}, marks: true},
		{name: "a plan's task stopped", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.StopTask(ctx, tx, key, generation, "a", "its owner stopped it"))
		}, marks: true},
		{name: "a plan's task's counts", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutReturnCounts(ctx, tx, key, generation, "a", ReturnCounts{Pass: 1}))
		}, marks: true},
		{name: "a plan's task given its branch", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			_, err := store.AssignTaskBranch(ctx, tx, key, generation, "a")
			must(t, err)
		}, marks: true},
		{name: "control: a plan's task that holds its branch already", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			seedPlan(t, ctx, tx)
			_, err := store.AssignTaskBranch(ctx, tx, key, generation, "a")
			must(t, err)
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			_, err := store.AssignTaskBranch(ctx, tx, key, generation, "a")
			must(t, err)
		}},
		{name: "control: a name the generation gave, which decides no pass", write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.RecordBranchGiven(ctx, tx, key, generation, "legion/"+key+".widget"))
		}},
		{name: "a plan's task's needs whose join conflicted", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.SetJoinConflicts(ctx, tx, key, generation, "a", []string{"b"}))
		}, marks: true},
		{name: "a plan's task's join unmerged", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.SetJoinUnmerged(ctx, tx, key, generation, "a", true))
		}, marks: true},
		{name: "a plan's task's base", seed: seedPlan, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.SetTaskBase(ctx, tx, key, generation, "a", "main"))
		}, marks: true},
		{name: "why a plan's task's step started", seed: seedRunning, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.SetStartReason(ctx, tx, key, generation, "a", "the reviewer sent the work back"))
		}, marks: true},
		{name: "a plan's task held", seed: seedRunning, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.HoldTask(ctx, tx, key, generation, "a", ""))
		}, marks: true},
		{name: "a plan's task released", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			seedRunning(t, ctx, tx)
			must(t, store.HoldTask(ctx, tx, key, generation, "a", ""))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.ReleaseTask(ctx, tx, key, generation, "a"))
		}, marks: true},
		{name: "what a plan's task's step waits for", seed: seedRunning, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.SetStepWait(ctx, tx, key, generation, "a", StepWaitAgent, &at))
		}, marks: true},
		{name: "a pull request deleted", seed: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutPullRequest(ctx, tx, pr))
		}, write: func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.DeletePullRequest(ctx, tx, key, pr.Branch))
		}, marks: true, issue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inRolledBackTx(t, st, func(ctx context.Context, tx pgx.Tx) {
				must(t, store.PutIssue(ctx, tx, issueFixture(key)))
				if tc.seed != nil {
					tc.seed(t, ctx, tx)
				}
				kept := WithIssueWrites(ctx)
				tc.write(t, kept, tx)
				if got := RecordsWritten(kept); got != tc.marks {
					t.Fatalf("RecordsWritten after %s = %t, want %t", tc.name, got, tc.marks)
				}
				if got := IssuesWritten(kept); slices.Equal(got, []string{key}) != tc.issue || (!tc.issue && got != nil) {
					t.Fatalf("IssuesWritten after %s = %v, want %s named %t", tc.name, got, key, tc.issue)
				}
			})
		})
	}
	inTx(t, st, func(tx pgx.Tx) {
		plain := t.Context()
		must(t, store.PutIssue(plain, tx, issueFixture(key)))
		if RecordsWritten(plain) {
			t.Fatal("a ctx that keeps no writes kept the mark, want none")
		}
	})
}

// errRolledBack is what inRolledBackTx's transaction returns, so that it rolls back.
var errRolledBack = errors.New("the case's transaction is rolled back")

// inRolledBackTx runs fn in a transaction of st that is rolled back, so each case of a test
// starts from the same rows.
func inRolledBackTx(t *testing.T, st *store.Store, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	err := store.Transact(t.Context(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		fn(ctx, tx)
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("the case's transaction: %v", err)
	}
}
