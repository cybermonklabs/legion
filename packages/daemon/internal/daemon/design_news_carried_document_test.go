package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A carried root whose gate rows record no document issue: rows a daemon on main wrote, which the
// migration's backfill leaves null where the store holds no registration of the document (the old
// root's re-admission cleared its rows before the upgrade), and which a carry main made during a
// rollback leaves null whether or not it does. The architect-start task and the catch-up name such
// a row's document by the round-14 rule (workflow's stepDocuments): the old root's where the store
// holds its registration, else Dispatch at the send, else the bare artifact reference, and never a
// wrong issue. The design-news record of an approved version of that tree, sent to the carried
// root's implementer through the daemon's own send (workflowRuntime.designNews), names each item's
// document and the gate's document by record.DesignGate.DocumentOf, the carried root's own
// document for such a row, a document no person approves. INFRA-3686's R2 ("a dispatch:// link for
// each Dispatch item") and R8 (the carried-gate facts), and the architect's round-14 standard.
func TestTheNewsOfACarriedRootNamesTheOldRootsDocument(t *testing.T) {
	const (
		oldRoot = "LEGION-8"
		carried = "LEGION-1"
		spec    = "artifact-spec-8"
	)
	for _, storeKnows := range []bool{true, false} {
		name := "the old root's rows cleared before the upgrade"
		if storeKnows {
			name = "the old root's registration stands (a carry during a rollback)"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: oldRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: heldTaskProject, Tree: oldRoot, Title: "old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A"})
			putOutboxIssue(t, pool, records, record.Issue{Key: carried, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: heldTaskProject, Tree: carried, Title: "carried", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B",
				HandedOver: true, LastDispatchSeq: 1})
			if _, err := pool.Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried) values ($1, 1, true)`, carried); err != nil {
				t.Fatal(err)
			}
			holders := []string{carried}
			if storeKnows {
				holders = append(holders, oldRoot)
			}
			for _, issue := range holders {
				if _, err := pool.Exec(ctx, `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, approved_seq, ever_opened)
					values ($1, 'spec', $2, 5, 5, 31, true)`, issue, spec); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				if err := records.RaiseTold(ctx, tx, carried, record.AudienceHolder, []record.NewsItem{{Step: record.DesignStepSpec, To: 3}}, time.Now()); err != nil {
					return err
				}
				if err := records.SettleOpening(ctx, tx, carried, record.AudienceHolder, true); err != nil {
					return err
				}
				return records.PutDesignChanges(ctx, tx, record.DesignChanges{
					VersionPair: record.VersionPair{ArtifactID: spec, From: 3, To: 5}, State: record.DesignChangesRead,
					Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}}, ReadAt: time.Now()})
			}); err != nil {
				t.Fatal(err)
			}
			steps := []record.DesignStep{record.DesignStepSpec}
			var startDocument string
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				root, err := records.Issue(ctx, tx, carried)
				if err != nil {
					return err
				}
				start, err := workflow.ArchitectStartOf(ctx, tx, records, steps, *root)
				startDocument = start.Documents[record.DesignStepSpec]
				return err
			}); err != nil {
				t.Fatal(err)
			}

			runtime := &workflowRuntime{pool: pool, records: records, gatedSteps: steps}
			news := record.DeliveryNews{Task: "Implement the plan.", Items: []record.DeliveryNewsItem{{Issue: carried, Audience: record.AudienceHolder,
				Step: record.DesignStepSpec, Artifact: spec, From: new(3), To: 5, Generation: 1, TreeGeneration: 1}},
				Openings: []record.DeliveryOpening{{Issue: carried, Audience: record.AudienceHolder}}}
			encoded, err := json.Marshal(news)
			if err != nil {
				t.Fatal(err)
			}
			c := supervise.Claim{Token: mustClaimToken(t, carried, claim.RoleImplementer), Project: "legion", Tree: carried, Issue: carried, Role: claim.RoleImplementer}
			task, _, err := runtime.designNews(ctx, c, supervise.Delivery{ID: "outbox:9", Task: news.Task, News: encoded})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("the start task's rule names %s; the news task:\n%s", startDocument, task)
			want := texts.ArtifactReference(spec)
			if storeKnows {
				want = "dispatch://" + oldRoot + "/spec"
			}
			if startDocument != want {
				t.Fatalf("the start task names %s, want %s", startDocument, want)
			}
			for _, line := range strings.Split(task, "\n") {
				if strings.Contains(line, "dispatch://"+carried+"/spec") {
					t.Errorf("the news names the carried root's own document, which no person approves, where the start task and the catch-up name %s (artifact %s): %q",
						want, spec, line)
				}
			}
		})
	}
}
