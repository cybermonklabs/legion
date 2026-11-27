package record

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A clear of a generation is handed the forget it runs over the decisions kept on the issues it
// clears (KeptForget), because it deletes the gates that forget's rule reads: a decision that is no
// longer the person's word, asked after its gate is gone, would read as their word again. So the
// forget is required, and a clear handed none is refused before it writes anything, whichever of
// the two clears it is. It answers no emptied issue, so its caller queues no publication of the
// empty set of gates, and a caller that commits its transaction after the refusal finds the issue
// as it was: its handoff, its rounds and its entry's task, and its gate.
func TestAClearHandedNoForgetForTheKeptDecisionsIsRefusedAndClearsNothing(t *testing.T) {
	ctx := context.Background()
	records := NewStore()
	const task = "Continue the change. Issue: LEGION-288. Phase: implementing."
	for _, tc := range []struct {
		name  string
		clear func(tx pgx.Tx, issue Issue) ([]string, error)
	}{
		{name: "the clear of one issue", clear: func(tx pgx.Tx, issue Issue) ([]string, error) {
			return records.ClearGeneration(ctx, tx, issue, nil)
		}},
		{name: "the clear of a tree", clear: func(tx pgx.Tx, issue Issue) ([]string, error) {
			return records.ClearTreeGeneration(ctx, tx, issue.Tree, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			issue := issueFixture("LEGION-288")
			approved := 2
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, issue))
				must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "implementer",
					HandoffCommit: "head", Rounds: 2, Entry: 3, EntryTask: task}))
				must(t, records.PutGate(ctx, tx, DesignGate{Issue: issue.Key, ArtifactID: "artifact-288", LatestVersion: 2, ApprovedVersion: &approved}))
			})
			inTx(t, st, func(tx pgx.Tx) {
				emptied, err := tc.clear(tx, issue)
				if err == nil {
					t.Fatalf("%s handed no forget for the kept decisions answered nil, want it refused: the clear deletes the gates "+
						"the forget's rule reads, so a clear that skips it leaves a superseded decision to read as the person's word", tc.name)
				}
				if len(emptied) != 0 {
					t.Fatalf("%s was refused and answers %v emptied, want none: its caller would publish an empty set of gates the issue still holds",
						tc.name, emptied)
				}
			})
			inTx(t, st, func(tx pgx.Tx) {
				rows, err := records.Phases(ctx, tx, issue.Key)
				must(t, err)
				if len(rows) != 1 || rows[0].HandoffCommit != "head" || rows[0].Rounds != 2 || rows[0].EntryTask != task {
					t.Fatalf("phases after %s was refused = %#v, want the implementer's row as it was put: handoff head, two rounds, its entry's task",
						tc.name, rows)
				}
				gates, err := records.GatesOf(ctx, tx, issue.Key)
				must(t, err)
				if len(gates) != 1 {
					t.Fatalf("gates after %s was refused = %#v, want the one gate put", tc.name, gates)
				}
			})
		})
	}
}

// A clear runs the forget it is handed once, over the decisions kept on the issues it clears and
// the changes requests the outbox holds on them, while those issues' gates still stand
// (Postgres.forgetKept): the clear of one issue hands over that issue's, and the clear of a tree
// those of every issue of it. Neither hands over a decision kept on, or a request queued on,
// another tree, nor the tree's kept approval, which the forget deletes nothing of, nor a queued
// approval, whose own notice is the person's later word. A request is handed over as the row
// carries it, a design-changes-requested notice for the architect or its relay to the controller
// read as the Notice it is, with the version, the mark and the document the forget asks of. The
// forget asks each of the gates of the issue it was made on, so a set handed over after the gates
// went would ask a gate that is gone.
func TestAClearHandsItsForgetTheDecisionsKeptOnTheIssuesItClears(t *testing.T) {
	ctx := context.Background()
	records := NewStore()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	const root, child, other = "LEGION-300", "LEGION-301", "LEGION-302"
	parent := root
	issues := []Issue{
		{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION",
			Title: "Root", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "U"},
		{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION",
			Title: "Child", Parent: &parent, Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "V"},
		{Key: other, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: other, Project: "LEGION",
			Title: "Another tree", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "W"},
	}
	kept := []ParkedDecision{
		{Issue: root, Ref: "dispatch://LEGION-300/spec", Decision: DecisionDesignChangesRequested, Actor: "user:alice", At: at, Seq: 8},
		{Issue: child, Ref: "dispatch://LEGION-301/spec", Decision: DecisionDesignChangesRequested, Actor: "user:bob", At: at.Add(time.Minute), Seq: 9},
		{Issue: other, Ref: "dispatch://LEGION-302/spec", Decision: DecisionDesignChangesRequested, Actor: "user:carol", At: at.Add(2 * time.Minute), Seq: 10},
	}
	// changes is the changes request each issue's notice carries, and relay the parked-decision
	// controller notice that relays it, read as the Notice it is.
	changes := func(issue Issue) Notice {
		return Notice{Kind: "design-changes-requested", Version: 3, Reason: "name the rollback path",
			Actor: "user:alice", Ref: "dispatch://" + issue.Key + "/spec",
			Review: EventMark{Seq: 11, At: at.Add(time.Hour)}, Artifact: "artifact-" + issue.Key}
	}
	relay := func(issue Issue, decision DecisionKind) Notice {
		notice := changes(issue)
		if decision != DecisionDesignChangesRequested {
			notice.Reason = ""
		}
		notice.Kind, notice.Decision, notice.Root = ParkedDecisionNotice, decision, issue.Tree
		return notice
	}
	queuedOn := func(keys ...string) map[string]Notice {
		want := map[string]Notice{}
		for _, issue := range issues {
			if slices.Contains(keys, issue.Key) {
				want[string(OutboxKindNotice)+" "+issue.Key] = changes(issue)
				want[string(OutboxKindControllerNotice)+" "+issue.Key] = relay(issue, DecisionDesignChangesRequested)
			}
		}
		return want
	}
	for _, tc := range []struct {
		name   string
		clear  func(tx pgx.Tx, forget KeptForget) ([]string, error)
		want   []string
		queued map[string]Notice
	}{
		{name: "the clear of the child", clear: func(tx pgx.Tx, forget KeptForget) ([]string, error) {
			return records.ClearGeneration(ctx, tx, issues[1], forget)
		}, want: []string{kept[1].Ref}, queued: queuedOn(child)},
		{name: "the clear of the tree", clear: func(tx pgx.Tx, forget KeptForget) ([]string, error) {
			return records.ClearTreeGeneration(ctx, tx, root, forget)
		}, want: []string{kept[0].Ref, kept[1].Ref}, queued: queuedOn(root, child)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			approved := 2
			inTx(t, st, func(tx pgx.Tx) {
				for i, issue := range issues {
					must(t, records.PutIssue(ctx, tx, issue))
					must(t, records.PutGate(ctx, tx, DesignGate{Issue: issue.Key, ArtifactID: "artifact-" + issue.Key, LatestVersion: 3,
						ApprovedVersion: &approved}))
					must(t, records.AddParkedDecision(ctx, tx, kept[i]))
					request := changes(issue)
					approval := request
					approval.Kind, approval.Reason = "design-approved", ""
					for _, payload := range []OutboxPayload{request, ControllerNotice(relay(issue, DecisionDesignChangesRequested)),
						approval, ControllerNotice(relay(issue, DecisionDesignApproved))} {
						row, err := NewOutboxRow(issue.Key, payload, at)
						must(t, err)
						must(t, records.Enqueue(ctx, tx, row))
					}
				}
				must(t, records.KeepReadmittedApproval(ctx, tx, root, KeptApproval{Step: DesignStepSpec, Approved: 2, Latest: 3}))
			})
			var runs int
			var tree string
			var handed KeptDecisions
			var requests []QueuedChanges
			var ungated []string
			forget := func(ctx context.Context, tx pgx.Tx, owedTo string, owed KeptDecisions, queued []QueuedChanges) error {
				runs++
				tree, handed, requests = owedTo, owed, queued
				var asked []string
				for _, d := range owed.Decisions {
					asked = append(asked, d.Issue)
				}
				for _, request := range queued {
					asked = append(asked, request.Issue)
				}
				for _, issue := range asked {
					gates, err := records.GatesOf(ctx, tx, issue)
					if err != nil {
						return err
					}
					if len(gates) == 0 {
						ungated = append(ungated, issue)
					}
				}
				return nil
			}
			inTx(t, st, func(tx pgx.Tx) {
				_, err := tc.clear(tx, forget)
				must(t, err)
			})
			if runs != 1 || tree != root {
				t.Fatalf("%s ran its forget %d times, last for tree %q, want once for %s, the tree the cleared issues stand in",
					tc.name, runs, tree, root)
			}
			var refs []string
			for _, d := range handed.Decisions {
				refs = append(refs, d.Ref)
			}
			slices.Sort(refs)
			if !slices.Equal(refs, tc.want) {
				t.Fatalf("%s handed its forget the decisions %v, want %v: the ones kept on the issues it clears, and no other",
					tc.name, refs, tc.want)
			}
			if handed.KeptApproval != (KeptApproval{}) {
				t.Fatalf("%s handed its forget the kept approval %#v, want none: the forget deletes no approval",
					tc.name, handed.KeptApproval)
			}
			got := map[string]Notice{}
			for _, request := range requests {
				got[string(request.Kind)+" "+request.Issue] = request.Notice
			}
			if len(requests) != len(tc.queued) || !reflect.DeepEqual(got, tc.queued) {
				t.Fatalf("%s handed its forget the queued requests %#v, want %#v: the changes requests queued on the issues it clears, "+
					"and no other", tc.name, requests, tc.queued)
			}
			if len(ungated) != 0 {
				t.Fatalf("%s ran its forget after the gates of %v went, want it run while they stand: the forget's rule reads them",
					tc.name, ungated)
			}
		})
	}
}

// Which queued rows hold a person's changes request is one rule (ChangesRequest), which the
// daemon's delivery of a notice and its publish of a relay ask, and the clear's select of the
// changes requests it hands its forget (queuedChanges) names the same rows by the payload's own
// fields. Were they to part, a request one of them asks again would be left by the other to be
// asked of no gate once the clear deleted it. One row of every notice kind, as an architect's
// notice and as a controller's, one relay of every decision a parked-decision notice may carry,
// and a row of another outbox kind are queued on one issue; a payload the outbox refuses is not
// queued, since no row like it can stand there. The select names exactly the rows the rule
// accepts, and the rule accepts some row of each of the two notice kinds, so the comparison is
// not of two empty sets.
func TestTheClearsSelectOfChangesRequestsNamesTheRowsChangesRequestAccepts(t *testing.T) {
	ctx := context.Background()
	records := NewStore()
	st := migratedStore(t)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	const key = "LEGION-310"
	ref := "dispatch://" + key + "/spec"
	var payloads []OutboxPayload
	for _, kind := range validNoticeKinds {
		notice := Notice{Kind: kind}
		if _, spec := SpecDecision(kind); spec {
			notice.Version, notice.Actor, notice.Ref = 3, "user:alice", ref
		}
		payloads = append(payloads, notice, ControllerNotice{Kind: kind})
	}
	for _, decision := range validDecisionKinds {
		payloads = append(payloads, ControllerNotice{Kind: ParkedDecisionNotice, Decision: decision,
			Root: key, Ref: ref, Actor: "user:alice", Version: 3})
	}
	payloads = append(payloads, ControllerNotice{Kind: TickNotice}, MessagePost{Body: "a message"})
	accepted := map[int64]OutboxKind{}
	queued := 0
	var selected []QueuedChanges
	inTx(t, st, func(tx pgx.Tx) {
		for _, payload := range payloads {
			row, err := NewOutboxRow(key, payload, at)
			if err != nil {
				continue
			}
			id, err := records.EnqueueID(ctx, tx, row)
			must(t, err)
			queued++
			if _, asks := ChangesRequest(payload); asks {
				accepted[id] = row.Kind
			}
		}
		var err error
		selected, err = queuedChanges(ctx, tx, clearedIssue, key)
		must(t, err)
	})
	if kinds := slices.Collect(maps.Values(accepted)); !slices.Contains(kinds, OutboxKindNotice) ||
		!slices.Contains(kinds, OutboxKindControllerNotice) || len(accepted) == queued {
		t.Fatalf("of %d queued rows ChangesRequest accepts %v, want some notice, some controller notice and "+
			"not every row: the comparison below would prove nothing", queued, accepted)
	}
	named := map[int64]OutboxKind{}
	for _, request := range selected {
		named[request.Row] = request.Kind
	}
	if !maps.Equal(named, accepted) {
		t.Fatalf("the clear's select names the rows %v and ChangesRequest accepts %v of the %d queued, "+
			"want the same rows: one rule says which rows hold a changes request", named, accepted, queued)
	}
}
