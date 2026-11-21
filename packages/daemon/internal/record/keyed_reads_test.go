package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The reads that join on a stored tree have successors that take the issues from their caller,
// who reads them from its Chains (R15 and R18 of dispatch://INFRA-3320). Every stored tree below
// is wrong on purpose — each record and each claim stores a tree that is none of the issues' gate
// roots — and every answer is the one the caller's issues give: a session's claim of one of them,
// each issue's own architect's launch and service, whether one of them runs in Legion's work, and
// the decisions owed to one architect, claimed, told, released and left by the rows and claims of
// that architect's own issue alone.
func TestTheReadsKeyedOnTheCallersIssuesReadThoseIssuesWhateverTreeEachStores(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const root, member, other, idle, parked = "LEGION-900", "LEGION-901", "LEGION-902", "LEGION-903", "LEGION-904"
	members := []string{root, member}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		for _, seed := range []struct {
			key, status string
			at          phase.Phase
		}{
			{root, "in_progress", phase.Implementing},
			{member, "testing", phase.Testing},
			{other, "in_progress", phase.Implementing},
			{idle, "todo", phase.Admitted},
			{parked, "backlog", phase.Implementing},
		} {
			issue := issueFixture(seed.key)
			issue.Tree, issue.Status, issue.Phase = "LEGION-800", seed.status, seed.at
			must(t, records.PutIssue(ctx, tx, issue))
		}
	})
	claims := map[string]supervise.Claim{}
	for _, c := range []supervise.Claim{
		{Token: "legion-legion-legion-900-architect", Issue: root, Role: claim.RoleArchitect, Generation: 4, ServingGeneration: 2, ServedEntry: 6, Session: "ses-root"},
		{Token: "legion-legion-legion-901-implementer", Issue: member, Role: claim.RoleImplementer, Generation: 1, Session: "ses-member"},
		{Token: "legion-legion-legion-902-architect", Issue: other, Role: claim.RoleArchitect, Generation: 7, Session: "ses-other"},
	} {
		c.Project, c.Tree, c.State = "legion", "LEGION-999", supervise.StateIdle
		must(t, st.PutClaim(ctx, c))
		claims[c.Issue] = c
	}

	inTx(t, st, func(tx pgx.Tx) {
		for _, tc := range []struct {
			issues  []string
			session string
			want    bool
		}{
			{members, "ses-member", true},
			{members, "ses-root", true},
			{members, "ses-other", false},
			{nil, "ses-root", false},
			{members, "", false},
		} {
			if got, err := records.SessionClaimsOf(ctx, tx, tc.issues, tc.session); err != nil || got != tc.want {
				t.Errorf("SessionClaimsOf(%v, %q) = %t, %v; want %t", tc.issues, tc.session, got, err, tc.want)
			}
		}
		launches, err := records.ArchitectLaunchesOf(ctx, tx, []string{root, member, other})
		must(t, err)
		if want := map[string]uint64{root: 4, other: 7}; !reflect.DeepEqual(launches, want) {
			t.Errorf("ArchitectLaunchesOf = %v, want %v: each issue's own architect claim, and none for an issue whose claim is no architect's", launches, want)
		}
		if served, entry, err := records.ArchitectServedOf(ctx, tx, root); err != nil || served != 2 || entry != 6 {
			t.Errorf("ArchitectServedOf(%s) = %d, %d, %v; want 2, 6", root, served, entry, err)
		}
		if served, entry, err := records.ArchitectServedOf(ctx, tx, member); err != nil || served != 0 || entry != 0 {
			t.Errorf("ArchitectServedOf(%s) = %d, %d, %v; want nothing: it has no architect claim", member, served, entry, err)
		}
		for _, tc := range []struct {
			members []string
			want    bool
		}{
			{[]string{member}, true},
			{[]string{idle, parked}, false},
			{nil, false},
		} {
			if got, err := records.MembersRunOf(ctx, tx, tc.members); err != nil || got != tc.want {
				t.Errorf("MembersRunOf(%v) = %t, %v; want %t: a member past admitted runs only while it is in Legion's work", tc.members, got, err, tc.want)
			}
		}
	})

	enqueue := func(tx pgx.Tx, issue string) int64 {
		row, err := NewFencedRow(ctx, tx, records, issue, Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, now)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
		var id int64
		must(t, tx.QueryRow(ctx, "select max(id) from outbox").Scan(&id))
		return id
	}
	decisions := []ParkedDecision{
		{Issue: root, Ref: "dispatch://LEGION-900/spec", Decision: DecisionDesignApproved, Actor: "user:alice", At: now},
		{Issue: member, Ref: "dispatch://LEGION-901/spec", Decision: DecisionDesignChangesRequested, Actor: "user:alice", At: now.Add(time.Second)},
		{Issue: other, Ref: "dispatch://LEGION-902/spec", Decision: DecisionDesignApproved, Actor: "user:bob", At: now.Add(2 * time.Second)},
	}
	var foreign int64
	inTx(t, st, func(tx pgx.Tx) {
		for _, decision := range decisions {
			must(t, records.AddParkedDecision(ctx, tx, decision))
		}
		must(t, records.KeepReadmittedApproval(ctx, tx, root, KeptApproval{Step: DesignStepSpec, Approved: 2, Latest: 3}))
		// A row of another architect claims the member's decision: that claim is void for root's
		// architect, so root's start may still name the decision.
		foreign = enqueue(tx, other)
		_, err := tx.Exec(ctx, "update parked_decisions set claimed_by = $1 where issue = $2", foreign, member)
		must(t, err)
	})
	marks := func(named []ParkedDecision) []DecisionMark {
		out := make([]DecisionMark, 0, len(named))
		for _, decision := range named {
			out = append(out, DecisionMark{Ref: decision.Ref, At: decision.At})
		}
		return out
	}
	var start int64
	inTx(t, st, func(tx pgx.Tx) {
		every, err := records.ParkedDecisionsOf(ctx, tx, members)
		must(t, err)
		if len(every) != 2 || every[0].Issue != member || every[1].Issue != root {
			t.Fatalf("ParkedDecisionsOf(%v) = %#v, want the member's and the root's, newest first", members, every)
		}
		named, approval, err := records.UnclaimedKeptOf(ctx, tx, root, members)
		must(t, err)
		if len(named) != 2 || approval != (KeptApproval{Step: DesignStepSpec, Approved: 2, Latest: 3}) {
			t.Fatalf("UnclaimedKeptOf(%s) = %#v, %#v; want both decisions, the foreign claim void, and root's approval", root, named, approval)
		}
		start = enqueue(tx, root)
		must(t, records.ClaimKeptDecisionsOf(ctx, tx, root, members, KeptDecisions{Decisions: marks(named), KeptApproval: approval}, start))
		again, approvalAgain, err := records.UnclaimedKeptOf(ctx, tx, root, members)
		must(t, err)
		if len(again) != 0 || approvalAgain != (KeptApproval{}) {
			t.Fatalf("UnclaimedKeptOf after root's own start claimed them = %#v, %#v; want nothing left to name", again, approvalAgain)
		}
		if err := records.ClaimKeptDecisionsOf(ctx, tx, other, []string{other}, KeptDecisions{Decisions: marks(decisions[:1])}, foreign); err == nil {
			t.Fatal("another architect's row claimed root's decision, which it never read under the lock")
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		released, err := records.ReleasedKeptIssues(ctx, tx, start)
		must(t, err)
		if !reflect.DeepEqual(released, []string{root, member}) {
			t.Fatalf("ReleasedKeptIssues = %v, want the issues of the released rows, %s and %s", released, root, member)
		}
		_, err = tx.Exec(ctx, "update parked_decisions set claimed_by = $1 where issue = any($2)", start, members)
		must(t, err)
		// The member's issue is owed to another architect by the time the row is told: root's
		// telling deletes root's own decision and leaves the member's for its architect.
		must(t, records.ForgetKeptDecisionsOf(ctx, tx, root, []string{root}, KeptDecisions{Decisions: marks(decisions[:2]),
			KeptApproval: KeptApproval{Approved: 2, Latest: 3}}, start))
		left, err := records.ParkedDecisionsOf(ctx, tx, []string{root, member, other})
		must(t, err)
		if len(left) != 2 || left[0].Issue != other || left[1].Issue != member || left[1].ClaimedBy != 0 {
			t.Fatalf("after root's telling the decisions = %#v, want the member's, its claim cleared, and the other architect's", left)
		}
		if approved, _, err := records.ReadmittedApproval(ctx, tx, root); err != nil || approved != 0 {
			t.Fatalf("root's approval after its telling = %d, %v; want it told", approved, err)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		holder := claims[root]
		_, err := tx.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, queued_at)
			values ($1, 'dlv-1', 'start', 'implementing', 1, now())`, string(holder.Token))
		must(t, err)
		_, err = tx.Exec(ctx, "update parked_decisions set held_by_claim = $1, held_by_delivery = 'dlv-1' where issue = $2", string(holder.Token), member)
		must(t, err)
		named, _, err := records.UnclaimedKeptOf(ctx, tx, root, members)
		must(t, err)
		if len(named) != 0 {
			t.Fatalf("UnclaimedKeptOf while root's own architect holds the member's decision = %#v, want nothing to name", named)
		}
		named, _, err = records.UnclaimedKeptOf(ctx, tx, other, []string{other, member})
		must(t, err)
		if len(named) != 2 {
			t.Fatalf("UnclaimedKeptOf of another architect owed the member now = %#v, want the member's and its own: root's hold is void for it", named)
		}
		released, err := records.ReleasedHeldIssues(ctx, tx, holder.Token, "dlv-1", []DecisionMark{{Ref: decisions[1].Ref, At: decisions[1].At}})
		must(t, err)
		if !reflect.DeepEqual(released, []string{member}) {
			t.Fatalf("ReleasedHeldIssues = %v, want %s", released, member)
		}
	})
}
