package record

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
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

// A fact runs the admission pass only where its transaction wrote a record of the project
// (RecordsWritten): an issue's own, or a row the pass reads beside it — the copy of what Dispatch
// says of an issue and of its claim, a root's ask, a design gate, an issue's plan. Each writer of
// those rows keeps the mark in a ctx that keeps writes when it changes a row. A write the row's
// own fence refuses changes nothing and keeps no mark, and a ctx that keeps no writes keeps none,
// which are the controls.
func TestEachWriterOfARecordThePassReadsKeepsTheMark(t *testing.T) {
	st := migratedStore(t)
	store := NewStore()
	const key = "LEGION-310"
	inTx(t, st, func(tx pgx.Tx) { must(t, store.PutIssue(t.Context(), tx, issueFixture(key))) })
	reading := DispatchReading{Key: key, Project: "LEGION", Seq: 5, Status: Carry("todo"), ClaimedBy: "session:ses-1", ClaimSeq: 5}
	ask := RootAsk{Issue: key, Ask: "ask-q", Kind: AskQuestion, Open: true, Seq: 5, WaitingOn: "human", WaitingOnSeq: 5}
	older := ask
	older.Seq, older.WaitingOn, older.WaitingOnSeq = 4, "agent", 4
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, ctx context.Context, tx pgx.Tx)
		marks bool
	}{
		{"an issue's record", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutIssue(ctx, tx, issueFixture(key)))
		}, true},
		{"a reading of Dispatch's issue", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchIssue(ctx, tx, reading))
		}, true},
		{"the same reading of Dispatch's issue again", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchIssue(ctx, tx, reading))
		}, false},
		{"a newer claim", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchClaim(ctx, tx, key, "session:ses-2", 6))
		}, true},
		{"an older claim", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutDispatchClaim(ctx, tx, key, "session:ses-0", 4))
		}, false},
		{"a root's ask", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutRootAsk(ctx, tx, ask))
		}, true},
		{"an older reading of the ask", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutRootAsk(ctx, tx, older))
		}, false},
		{"a design gate", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			must(t, store.PutGate(ctx, tx, DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "doc-spec", LatestVersion: 1}))
		}, true},
		{"an issue's plan", func(t *testing.T, ctx context.Context, tx pgx.Tx) {
			v, tasks, steps := ownersPlan(key, 1, 1, "architect-310", planGivenAt)
			must(t, store.PutPlan(ctx, tx, v, tasks, steps))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, st, func(tx pgx.Tx) {
				kept := WithIssueWrites(t.Context())
				tc.write(t, kept, tx)
				if got := RecordsWritten(kept); got != tc.marks {
					t.Fatalf("RecordsWritten after %s = %t, want %t", tc.name, got, tc.marks)
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
