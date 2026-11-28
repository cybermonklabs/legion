package intake_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// An old event of a child the daemon has never recorded, given up and replayed long after,
// records the child as Dispatch shows it now, never as the event said. The workflow records such
// a child on the path before its sequence fence (recordChildUnderLiveTree), and what keeps that
// path from writing an old state is the live handling the replay runs: the issue's preparation
// replaces the event with Dispatch's present view of the issue (the daemon's prepareIssue, played
// here by a stand-in that answers that view), and ApplyFact applies the view while the issue is
// unrecorded (settleObserved). A replay that skipped the preparation would record the event's old
// title at its old sequence, which this test refuses.
func TestAReplayOfAnUnrecordedChildsOldEventRecordsItAsDispatchShowsItNow(t *testing.T) {
	const project, root, child, repo = "LEGION", "LEGION-7", "LEGION-8", "acme/widgets"
	ctx := context.Background()
	pool := testpg.ClonePool(t, "legion_intake_replay_test")
	records := record.NewStore()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	// The root's tree runs: its record, the slot its work holds and its open lifecycle, as
	// promotion writes them, so a todo child under it is recorded into it.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: root, Tree: root, Project: project, Title: "the root", Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "M", HandedOver: true, LastDispatchSeq: 1,
			Repo: repo, DefaultBranch: "main", GitHubRepo: repo}); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`, token, root)
		return err
	}); err != nil {
		t.Fatalf("seed the running tree of %s: %v", root, err)
	}

	// The event Dispatch wrote at sequence 3, given up and kept whole; Dispatch has run to 9 since,
	// and the child's title changed.
	event, err := json.Marshal(map[string]any{"id": 30, "issue_key": child, "seq": 3, "type": "issue.updated", "notify": true,
		"actor": map[string]string{"kind": "user", "id": "a-person"},
		"payload": map[string]any{"key": child, "status": "todo", "title": "the title of the old event", "rank": "A", "parent": root,
			"labels": []string{}, "resolved_repo": repo}})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": "dispatch-30", "source": "dispatch", "source_event_id": "30", "topic": "issue",
		"dedupe_key": "dispatch-30", "issued_at": 1, "payload_summary": child + " issue updated", "payload": string(event), "trace_id": "dispatch-30"})
	if err != nil {
		t.Fatal(err)
	}
	message := record.IntakeMessage{Project: project, Consumer: "legion-go-" + project + "-dispatch", Subject: "notifications.dispatch.issue." + child + ".issue.updated",
		StreamSeq: 40, StoredAt: time.Now().Add(-100 * time.Hour), Payload: envelope, EventSource: "dispatch", EventID: "dispatch-30"}
	var given record.IntakeFailure
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		failure, err := records.NoteFailure(ctx, tx, message, 21, "the handler failed", time.Now().Add(-99*time.Hour))
		if err != nil {
			return err
		}
		given, _, err = records.GiveUpIntakeMessage(ctx, tx, project, failure.ID, record.GiveUpBound, "the handler failed", time.Now().Add(-98*time.Hour))
		return err
	}); err != nil {
		t.Fatalf("give up %s's old event: %v", child, err)
	}

	var prepared atomic.Int32
	rootKey := root
	spec := intake.ConsumerSpec{Project: project, Repositories: []ghrepo.Repository{ghrepo.MustParse(repo)}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PrepareRepository: func(_ context.Context, event intake.DispatchIssue) (intake.DispatchIssue, error) {
			prepared.Add(1)
			view := intake.ObservedIssue(dispatch.Issue{Key: event.Key, Status: "todo", Title: "the title Dispatch shows now", Rank: "B",
				Parent: &rootKey, ResolvedRepo: repo, LastSeq: 9})
			view.Seq, view.Delivered = max(event.Seq, view.Seq), &event
			return view, nil
		}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: project, Repositories: redeliveredRepositories{}, Steps: config.DesignGateRootIssues.Steps()}, log)
	giveUps, err := intake.NewGiveUps(intake.Records{Pool: pool, Spec: spec, Repositories: func() []ghrepo.Repository { return spec.Repositories }},
		blockers.NewMirror(records, project), engine, admit.New(records, engine, 10, project, log))
	if err != nil {
		t.Fatalf("NewGiveUps: %v", err)
	}
	answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: given.ID}, "operator")
	if err != nil {
		t.Fatalf("replay give-up %d: %v", given.ID, err)
	}
	if len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != intake.ReplayApplied {
		t.Fatalf("the replay answered %+v, want the event applied", answer)
	}
	if calls := prepared.Load(); calls != 1 {
		t.Fatalf("the replay prepared the event %d times, want once, as a live delivery prepares it", calls)
	}

	var recorded *record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		recorded, err = records.Issue(ctx, tx, child)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", child, err)
	}
	if recorded == nil {
		t.Fatalf("%s is not recorded after the replay, want it recorded under %s's running tree", child, root)
	}
	if recorded.Title != "the title Dispatch shows now" || recorded.LastDispatchSeq != 9 || recorded.Tree != root {
		t.Fatalf("%s is recorded with title %q at sequence %d in tree %s, want Dispatch's title now at sequence 9 in %s: the old event's state must not reach the record",
			child, recorded.Title, recorded.LastDispatchSeq, recorded.Tree, root)
	}
}
