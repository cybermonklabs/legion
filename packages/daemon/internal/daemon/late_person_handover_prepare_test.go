package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A person parks an unrecorded labelled root at 5, approves its spec at 9, moves it to todo at 10
// and parks it again at 11. The approval's preparation and the todo's both read Dispatch, so a
// failed read sends each back for a later delivery, or gives each up, while the park at 11, which
// reads nothing, applies first. Each event here goes through the daemon's own preparation
// (prepareRepository, prepareArtifact), with Dispatch showing the root as the person left it, in
// icebox at sequence 11, and is then applied through the copy of Dispatch's issues, the workflow
// and admission, as the intake applies it. Whether the late todo applies before the approval or
// after it, as the operator's replay of both in stream order does, the approval must queue no
// handover: the person parked the root after approving it.
func TestAnApprovalAppliedAfterThePersonParkedTheRootAgainQueuesNoHandover(t *testing.T) {
	t.Parallel()
	const key = "LEGION-755"
	person := dispatch.Actor{Kind: "user", ID: "sami"}
	approved := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	event := func(seq int64, status string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: key, Seq: seq, Type: "issue.updated", Status: status, Title: key, Rank: "A", HandedOver: true,
			Labels: []string{dispatch.LegionLabel}, ResolvedRepo: "acme/widgets"}
	}
	approval := intake.DispatchArtifact{Key: key, ArtifactID: "art-1", Kind: intake.DispatchArtifactApproved, Version: 1, Actor: person,
		Mark: record.EventMark{Seq: 9, At: approved}}
	// logEvent is an event of the root's Dispatch event log as EventsAfter serves it, by the
	// person, and issueEvent an issue event of it that leaves the labelled root in status under
	// title.
	logEvent := func(seq int64, typ, payload string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":%d,"type":%q,"actor":{"kind":%q,"id":%q},"payload":%s}`,
			seq, key, seq, typ, person.Kind, person.ID, payload))
	}
	issueEvent := func(seq int64, typ, status, title string) json.RawMessage {
		return logEvent(seq, typ, fmt.Sprintf(`{"key":%q,"status":%q,"title":%q,"labels":[%q]}`, key, status, title, dispatch.LegionLabel))
	}
	approvalEvent := logEvent(9, "artifact.approved", `{"artifact_id":"art-1","version":1}`)
	parkedAgain := []json.RawMessage{issueEvent(1, "issue.created", "triage", key), issueEvent(5, "issue.updated", "icebox", key), approvalEvent,
		issueEvent(10, "issue.updated", "todo", key), issueEvent(11, "issue.updated", "icebox", key)}
	for _, tc := range []struct {
		name  string
		order []intake.Fact
		log   []json.RawMessage
		want  int
	}{
		// The control: with no change of the root's status after the approval, the same
		// preparation and apply queue the person's handover once.
		{name: "no change of status after the approval queues the handover",
			order: []intake.Fact{event(5, "icebox"), approval}, want: 1,
			log: []json.RawMessage{issueEvent(1, "issue.created", "triage", key), issueEvent(5, "issue.updated", "icebox", key), approvalEvent,
				issueEvent(11, "issue.updated", "icebox", key+", renamed")}},
		{name: "the todo applies after the second park, then the approval",
			order: []intake.Fact{event(5, "icebox"), event(11, "icebox"), event(10, "todo"), approval}, log: parkedAgain},
		{name: "the replay applies the approval, then the todo",
			order: []intake.Fact{event(5, "icebox"), event(11, "icebox"), approval, event(10, "todo")}, log: parkedAgain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			repos := newRepositories("LEGION", ghrepo.Renames{})
			repos.fill(ghrepo.MustParse("acme/widgets"))
			branches := newDefaultBranches(roleTokens{}, repos, "", defaultBranchTTL, time.Now)
			branches.read["acme/widgets"] = defaultBranchRead{repository: ghbranch.Repository{Name: ghrepo.MustParse("acme/widgets"), DefaultBranch: "main"}, at: time.Now()}
			shown := shownLabelled(key, "icebox")
			shown.PrimaryArtifactID, shown.LastSeq = "art-1", 11
			client := &eventLogDispatch{}
			client.issues = map[string]dispatch.Issue{key: shown}
			client.logs = map[string][]json.RawMessage{key: tc.log}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, repositories: repos, defaultBranches: branches, log: quietLogger()}
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			admission := admit.New(records, engine, 1, "LEGION", quietLogger())
			mirror := blockers.NewMirror(records, "LEGION")

			var preparedApproval intake.DispatchArtifact
			for i, fact := range tc.order {
				var prepared intake.Fact
				var err error
				switch fact := fact.(type) {
				case intake.DispatchIssue:
					prepared, err = w.prepareRepository(ctx, fact)
				case intake.DispatchArtifact:
					preparedApproval, err = w.prepareArtifact(ctx, fact)
					prepared = preparedApproval
				}
				if err != nil {
					t.Fatalf("prepare fact %d (%T): %v", i, fact, err)
				}
				if _, err := intake.ApplyFact(ctx, pool, "dispatch", fmt.Sprintf("event-%d", i), prepared, mirror, engine, admission); err != nil {
					t.Fatalf("apply fact %d (%T): %v", i, fact, err)
				}
			}

			var handovers int
			if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'dispatch_status' and issue = $1 and payload->>'kind' = 'person-handover'`,
				key).Scan(&handovers); err != nil {
				t.Fatalf("read the status writes of %s: %v", key, err)
			}
			if handovers != tc.want {
				t.Fatalf("the approval at sequence 9 queued %d person's handovers of %s, want %d (the prepared approval's StatusChangedSeq is %d)",
					handovers, key, tc.want, preparedApproval.StatusChangedSeq)
			}
		})
	}
}
