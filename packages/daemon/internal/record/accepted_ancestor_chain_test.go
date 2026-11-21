package record

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// chainIssue is one member of the tree the chain tests are read over, on the repository every
// record carries.
func chainIssue(key, tree, parent string, stands phase.Phase, status string) Issue {
	issue := Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: key, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if parent != "" {
		issue.Parent = ParentOf(parent)
	}
	return issue
}

// The hold under an accepted ancestor, where that ancestor is not the root and a done parent
// stands between it and the held issue: INFRA-500's shape in R21 of dispatch://INFRA-3320. The
// chain is the copy's (Chains), which crosses the done parent the pass's live records leave out,
// so the line holds the issue and the held list names the merging issue (accepted_ancestor). The
// doors into the workflow and the merging issue's own wait read the same chains: the doors find
// the same ancestor (AcceptedAncestorIn), and OpenBeneath leaves the held issue out of that
// ancestor's own wait, so starting it would merge a head without its work and holding it
// deadlocks nothing. The control is INFRA-1238's shape: with the issue above the done parent at
// work rather than accepted, the issue is held under its parent (ancestry) instead.
//
// The whole path is read here, from the records and the copy as the store holds them, because the
// line's own pass reads the live records alone.
func TestTheLineHoldsAnIssueUnderAnAcceptedAncestorAboveADoneParent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const (
		root        = "LEGION-900"
		merging     = "LEGION-901"
		doneParent  = "LEGION-902"
		heldAtAdmit = "LEGION-903"
	)
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{
			chainIssue(root, root, "", phase.Admitted, "in_progress"),
			chainIssue(merging, root, root, phase.Merging, "needs_review"),
			chainIssue(doneParent, root, merging, phase.Done, "done"),
			chainIssue(heldAtAdmit, root, doneParent, phase.Admitted, "todo"),
		} {
			must(t, records.PutIssue(ctx, tx, issue))
			putCopy(t, ctx, tx, issue)
		}
		approved := 1
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}))
	})
	// held reads the line and the held list of the project's live records, as a pass does.
	held := func(tx pgx.Tx) ([]LineEntry, []HeldIssue, LineFacts) {
		issues, err := records.LiveIssues(ctx, tx, "LEGION")
		must(t, err)
		facts, err := ReadLineFacts(ctx, tx, records, "LEGION", issues, nil)
		must(t, err)
		line, dropped := Waiting(issues, nil, facts)
		return line, HeldList(issues, nil, line, dropped, facts), facts
	}

	inTx(t, st, func(tx pgx.Tx) {
		line, rows, facts := held(tx)
		if inLine(line, heldAtAdmit) {
			t.Fatalf("%s stands in the waiting line, though %s above it is merging: the daemon holds it until that ancestor closes", heldAtAdmit, merging)
		}
		if row := heldRow(rows, heldAtAdmit); row.Reason != HeldAcceptedAncestor || !slices.Equal(row.HeldBy, []string{merging}) {
			t.Fatalf("the held row of %s = %+v, want accepted_ancestor held by %s", heldAtAdmit, row, merging)
		}
		// The two readers answer alike, which is what keeps the hold and the merge from
		// deadlocking: the issue the line holds is one the merging ancestor does not wait for.
		if ancestor := AcceptedAncestorIn(facts.Chains, heldAtAdmit, ""); ancestor != merging {
			t.Fatalf("the doors read the accepted ancestor of %s as %q, want %s", heldAtAdmit, ancestor, merging)
		}
		if open := OpenBeneath(facts.Chains, merging); len(open) != 0 {
			t.Fatalf("the issues beneath %s in Legion's work = %v, want none: the one the daemon holds is left out", merging, open)
		}

		// The control: the issue above the done parent at work and not accepted.
		atWork := chainIssue(merging, root, root, phase.Implementing, "in_progress")
		must(t, records.PutIssue(ctx, tx, atWork))
		line, rows, _ = held(tx)
		if inLine(line, heldAtAdmit) {
			t.Fatalf("%s stands in the waiting line under a done parent: an issue whose parent is out of work waits for it", heldAtAdmit)
		}
		if row := heldRow(rows, heldAtAdmit); row.Reason != HeldAncestry || !slices.Equal(row.HeldBy, []string{merging}) {
			t.Fatalf("the held row of %s with %s at work = %+v, want ancestry held by %s, the issue above at work", heldAtAdmit, merging, row, merging)
		}
	})
}

// heldRow is the held row of key among rows, the zero row where none names it.
func heldRow(rows []HeldIssue, key string) HeldIssue {
	for _, row := range rows {
		if row.Issue == key {
			return row
		}
	}
	return HeldIssue{}
}

// inLine says whether key stands in the waiting line.
func inLine(line []LineEntry, key string) bool {
	for _, entry := range line {
		if entry.Issue.Key == key {
			return true
		}
	}
	return false
}

// A lane whose lease lapsed may be slow rather than dead, and every move it has left is matched
// on the row it holds: it renews by the token alone, and finishes the row by the id and the
// token. So a start written in place of a lapsed row takes that row out from under it. Left in
// place, that lane could renew, deliver the earlier task it had already read, and then finish
// the row now carrying the later one — deleting the later task with nothing to re-place it.
func TestAStartOverALapsedOwedRowTakesItFromItsLane(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const issue = "LEGION-910"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture(issue)))
		must(t, records.Enqueue(ctx, tx, owedStartRow(t, issue, "Implement the change.")))
		if _, err := tx.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = now() - interval '1 hour'
			where issue = $1 and kind = 'supervise'`, issue); err != nil {
			t.Fatalf("lapse the lease: %v", err)
		}
		var lapsed int64
		if err := tx.QueryRow(ctx, `select id from outbox where issue = $1 and kind = 'supervise'`, issue).Scan(&lapsed); err != nil {
			t.Fatalf("read the lapsed row: %v", err)
		}
		must(t, records.EnqueueStart(ctx, tx, owedStartRow(t, issue, "Implement the change again.")))
		var id int64
		var token string
		var until *time.Time
		if err := tx.QueryRow(ctx, `select id, lease_token, lease_until from outbox
			where issue = $1 and kind = 'supervise'`, issue).Scan(&id, &token, &until); err != nil {
			t.Fatalf("read the start the issue owes: %v", err)
		}
		if id == lapsed {
			t.Fatalf("the later task is on row %d, the very row the lapsed lane holds: that lane can renew its lease, deliver the earlier task and delete the later one", id)
		}
		if token != "" || until != nil {
			t.Fatalf("the start the issue owes carries lease %q until %v, want none: no lane holds a row nobody has claimed", token, until)
		}
	})
}
