package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The daemon's ActsAs is the owner's step: LEGION-1 stands at step 1 of `implementer:owner
// reviewer`, which its owner, LEGION-1's own architect, does. Its architect's slot acts as the
// implementer; LEGION-2's architect, the owner of an issue that is not LEGION-1, and LEGION-1's
// implementer act as nothing, and neither does a token of an issue the daemon does not record.
// The owner's retro is done as the role of the task's last step of the type implementation where
// that step is the owner's, so the slot keeps the App that opened the pull request, the one App
// that can resolve its review threads: LEGION-4 at the retro of `implementer:owner reviewer` acts
// as the implementer. LEGION-5 at the retro of `acceptance:owner`, a task with no such step, whose
// retro the owner does as itself, acts as nothing; so does LEGION-6's architect at the retro of
// `implementer reviewer`, whose implementer does the retro, and so does that implementer.
// runtime.SlotRole, given that ActsAs, answers the role acted as for each architect that acts, and
// the role it is given for the others, each claim's stored role. Every writer of a slot's gh files
// of both runtimes judges the slot's App through that one function (daemon.go, kubernetes.go).
func TestTheDaemonsActsAsIsTheOwnersStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	issue := func(key string, at phase.Phase) record.Issue {
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: "LEGION",
			Title: key, Phase: at, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", LastDispatchSeq: 1}
	}
	retro := record.TaskPlace{Kind: record.PlaceRetro}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		for _, put := range []error{
			putPlannedIssue(t, ctx, tx, records, issue("LEGION-1", phase.Implementing), record.TaskPlace{Kind: record.PlaceStep, Position: 1}, now,
				"implementer:owner", "reviewer"),
			records.PutIssue(ctx, tx, issue("LEGION-2", phase.Planning)),
			putPlannedIssue(t, ctx, tx, records, issue("LEGION-4", phase.Retro), retro, now, "implementer:owner", "reviewer"),
			putPlannedIssue(t, ctx, tx, records, issue("LEGION-5", phase.Retro), retro, now, "acceptance:owner"),
			putPlannedIssue(t, ctx, tx, records, issue("LEGION-6", phase.Retro), retro, now, "implementer", "reviewer"),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed LEGION-1 at its owner's implementer step, LEGION-2, and LEGION-4 to LEGION-6 at their retros: %v", err)
	}

	actsAs := ownerActsAs(pool, records)
	for _, tc := range []struct {
		name   string
		token  claim.Token
		stored claim.Role
		want   claim.Role
		acting bool
	}{
		{"LEGION-1's architect, its owner", mustClaimToken(t, "LEGION-1", claim.RoleArchitect), claim.RoleArchitect, claim.RoleImplementer, true},
		{"LEGION-2's architect, the owner of another issue", mustClaimToken(t, "LEGION-2", claim.RoleArchitect), claim.RoleArchitect, "", false},
		{"LEGION-1's implementer", mustClaimToken(t, "LEGION-1", claim.RoleImplementer), claim.RoleImplementer, "", false},
		{"the architect of an issue the daemon does not record", mustClaimToken(t, "LEGION-3", claim.RoleArchitect), claim.RoleArchitect, "", false},
		{"LEGION-4's architect at the retro of its own implementer step", mustClaimToken(t, "LEGION-4", claim.RoleArchitect), claim.RoleArchitect, claim.RoleImplementer, true},
		{"LEGION-5's architect at the retro of acceptance:owner alone", mustClaimToken(t, "LEGION-5", claim.RoleArchitect), claim.RoleArchitect, "", false},
		{"LEGION-6's architect at its implementer's retro", mustClaimToken(t, "LEGION-6", claim.RoleArchitect), claim.RoleArchitect, "", false},
		{"LEGION-6's implementer at its own retro", mustClaimToken(t, "LEGION-6", claim.RoleImplementer), claim.RoleImplementer, "", false},
	} {
		role, acting, err := actsAs(ctx, tc.token)
		if err != nil || role != tc.want || acting != tc.acting {
			t.Errorf("ActsAs(%s) = (%q, %v, %v), want (%q, %v, nil)", tc.name, role, acting, err, tc.want, tc.acting)
		}
		slot, err := runtime.SlotRole(ctx, actsAs, tc.token, tc.stored)
		want := tc.stored
		if tc.acting {
			want = tc.want
		}
		if err != nil || slot != want {
			t.Errorf("SlotRole(%s) = (%q, %v), want (%q, nil)", tc.name, slot, err, want)
		}
	}
}

// A merge forward is an act as the implementer, so the daemon's ActsAs answers the implementer for
// the owner while the record holds a merge forward asked of the owner that has not ended. LEGION-7
// stands at the reviewer's step of `implementer:owner reviewer`, another agent's: the ask reached
// the owner's claim (record.MergeForward.Agent), and its pull request holds the ask, not landed, so
// its owner acts as the implementer. Once the ask landed, the owner acts as nothing again. The
// control: LEGION-8, the same with `implementer reviewer`, whose ask went to the implementer's own
// claim, leaves the owner acting as nothing.
func TestTheDaemonsActsAsIsTheImplementerForAMergeForwardAskedOfTheOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	reviewer := record.TaskPlace{Kind: record.PlaceStep, Position: 2}
	seed := func(tx pgx.Tx, key string, words ...string) error {
		issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: "LEGION",
			Title: key, Phase: phase.Reviewing, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review", Rank: "A", LastDispatchSeq: 1}
		implementer := mustClaimToken(t, key, claim.RoleImplementer)
		if strings.HasSuffix(words[0], ":owner") {
			implementer = mustClaimToken(t, key, claim.RoleArchitect)
		}
		if err := putPlannedIssue(t, ctx, tx, records, issue, reviewer, now, words...); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: implementer, Entry: 1,
			StepTask: "a", StepPlace: 1}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: key, Repo: "acme/widgets", Number: 42, Branch: "legion/" + key,
			State: record.PullRequestOpen, HeadSHA: "c0de", Failing: []string{},
			MergeForward: &record.MergeForward{Head: "c0de", Base: "main", Phase: phase.Reviewing, Agent: implementer, Why: record.ConflictCheckNotGreen, AskedAt: now}})
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := seed(tx, "LEGION-7", "implementer:owner", "reviewer"); err != nil {
			return err
		}
		return seed(tx, "LEGION-8", "implementer", "reviewer")
	}); err != nil {
		t.Fatalf("seed LEGION-7 and LEGION-8 at their reviewer's step with a merge forward asked: %v", err)
	}
	actsAs := ownerActsAs(pool, records)
	acts := func(t *testing.T, key string, want claim.Role, acting bool, when string) {
		t.Helper()
		if role, got, err := actsAs(ctx, mustClaimToken(t, key, claim.RoleArchitect)); err != nil || role != want || got != acting {
			t.Fatalf("ActsAs(%s's architect) %s = (%q, %v, %v), want (%q, %v, nil)", key, when, role, got, err, want, acting)
		}
	}
	acts(t, "LEGION-7", claim.RoleImplementer, true, "while the merge forward asked of it is open")
	acts(t, "LEGION-8", "", false, "while the merge forward asked of its implementer is open")

	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, err := records.PullRequest(ctx, tx, "LEGION-7")
		if err != nil || pr == nil {
			return fmt.Errorf("read LEGION-7's pull request: %+v, %w", pr, err)
		}
		landed := *pr.MergeForward
		landed.Landed = "d00d"
		pr.MergeForward, pr.HeadSHA = &landed, "d00d"
		return records.PutPullRequest(ctx, tx, *pr)
	}); err != nil {
		t.Fatalf("land LEGION-7's merge forward: %v", err)
	}
	acts(t, "LEGION-7", "", false, "once the merge forward landed")
}
