package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A member that stands at a step of a role the deployment defines is told the news of the approved
// design as a member in a phase of Legion's own workers is: the role of the running step holds the
// member's step (record.Issue.RoleAt), where phase.RoleFor names no role for step. With the tree's
// gate open and the checker's step behind the approved version of the spec, OwedNews owes the
// member that version (record's untold), the reconcile queues a news-only start for the checker's
// claim, which holds the step (phaseHolder), and the root architect's summary says a task tells its
// checker now (memberDraft.told, workflow.NewsMember.Role). Read by the phase alone, the member is
// named held back with the gate open, no start is queued for it, and the summary names no holder
// of its step.
func TestDesignNewsReachesTheRoleOfARunningDeploymentStep(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.member(reconcileChild, reconcileRoot, phase.Step)
	r.inTx(func(tx pgx.Tx) error {
		v := record.PlanVersion{Issue: reconcileChild, Generation: 1, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, reconcileChild, claim.RoleArchitect), GivenAt: time.Now().UTC()}
		task := record.PlanTask{Issue: reconcileChild, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: reconcileChild, Generation: 1, Task: "a", Position: 1, Role: string(claim.RoleImplementer), Type: "implementation"},
			{Issue: reconcileChild, Generation: 1, Task: "a", Position: 2, Role: "checker", Type: "testing"},
		}
		if err := r.records.PutPlan(r.ctx, tx, v, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		return r.records.MoveTask(r.ctx, tx, reconcileChild, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
	})
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileChild, "checker", supervise.StateIdle, "", 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	if got, want := startsTo(r.newsStarts()), []string{reconcileChild + "/checker@step"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the news-only starts = %v, want %v: the checker holds the member's step", got, want)
	}

	// The root architect's summary of the spec from version 4, which its row now took, to 5.
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.record(record.VersionPair{ArtifactID: reconcileDoc, From: 4, To: 5}, "")
	encoded, err := json.Marshal(record.DeliveryNews{Task: "A person approved a new version of the design of " + reconcileRoot + ".",
		Items: []record.DeliveryNewsItem{newsItem(reconcileRoot, record.AudienceArchitect, record.DesignStepSpec, reconcileDoc, 4, 5)}})
	if err != nil {
		t.Fatal(err)
	}
	sender := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: config.DesignGateRootIssues.Steps(),
		dispatchProject: "LEGION", log: quietLogger()}
	architect := supervise.Claim{Token: mustClaimToken(t, reconcileRoot, claim.RoleArchitect), Project: "legion", Tree: reconcileRoot,
		Issue: reconcileRoot, Role: claim.RoleArchitect}
	summary, _, err := sender.designNews(r.ctx, architect, supervise.Delivery{ID: "outbox:9", Task: "queued text", News: encoded, NewsOnly: true})
	if err != nil {
		t.Fatalf("the root architect's summary: %v", err)
	}
	prefix := "- " + reconcileChild + ", in step: "
	for _, line := range strings.Split(summary, "\n") {
		if strings.HasPrefix(line, prefix) {
			if want := "; a task tells its checker now."; !strings.HasSuffix(line, want) {
				t.Fatalf("the summary's line of %s = %q, want it to end %q", reconcileChild, line, want)
			}
			return
		}
	}
	t.Fatalf("summary = %q, want a line for %s", summary, reconcileChild)
}
