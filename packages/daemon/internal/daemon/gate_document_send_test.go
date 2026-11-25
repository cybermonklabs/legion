package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// documentHolders is a Dispatch whose artifact read (dispatch.Client.Approval) names the issue that
// holds each artifact of holds, and answers an error for any other, as a Dispatch that does not
// answer.
type documentHolders struct {
	dispatch.Client
	holds map[string]string
	asked []string
}

func (d *documentHolders) Approval(_ context.Context, artifact string) (dispatch.Approval, error) {
	d.asked = append(d.asked, artifact)
	issue, ok := d.holds[artifact]
	if !ok {
		return dispatch.Approval{}, errors.New("dispatch: the read of the artifact timed out")
	}
	return dispatch.Approval{IssueKey: issue, LatestVersion: 2}, nil
}

// The outbox names a gate step's document that a root architect's start task or catch-up gives by
// the bare artifact reference — a row the record does not say the document's issue of
// (workflow's stepDocuments) — from Dispatch where it sends the text, outside any transaction: the
// start at its supervise start, before the claim takes the task, and the catch-up at its publish,
// both by Dispatch's read of the artifact (dispatchDocumentIssue). Where Dispatch does not answer,
// or the daemon has no Dispatch client, both send the bare reference, never another issue's
// document (INFRA-3686 R8 and R2, the architect's ruling on test handoff v13's failures 1 and 2).
func TestTheOutboxNamesAGatesDocumentTheRecordCannotFromDispatchWhereItSendsTheText(t *testing.T) {
	const artifact, oldRoot = "5b0c9d1e-7a3f-4e26-9c8b-1f2d3e4a5b6c", "LEGION-208"
	bare := texts.ArtifactReference(artifact)
	for _, tc := range []struct {
		name string
		// client is the outbox's Dispatch client, nil for a daemon with none.
		client *documentHolders
		want   string
	}{
		{name: "Dispatch names the issue that holds the document", client: &documentHolders{holds: map[string]string{artifact: oldRoot}},
			want: "dispatch://" + oldRoot + "/spec"},
		{name: "Dispatch does not answer", client: &documentHolders{}, want: bare},
		{name: "no Dispatch client", want: bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			token := architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			publisher := &holderPublisher{}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
				project: "legion", now: time.Now}
			if tc.client != nil {
				runner.dispatch = tc.client
			}

			root := record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Project: "LEGION", Phase: phase.Admitted, Generation: 1}
			task := workflow.ArchitectStartTask(workflow.ArchitectStart{Root: root,
				Gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}, Rows: map[record.DesignStep]record.DesignGate{
					record.DesignStepSpec: {Issue: root.Key, Step: record.DesignStepSpec, ArtifactID: artifact, LatestVersion: 2, ApprovedVersion: new(1)}}},
				Documents: map[record.DesignStep]string{record.DesignStepSpec: bare}})
			if !strings.Contains(task, ","+bare+",") {
				t.Fatalf("the start task as the record builds it:\n%s\nwant its spec row to give the bare reference %s", task, bare)
			}
			start := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.SuperviseRequest{Op: "start", Tree: "LEGION-1",
				Role: claim.RoleArchitect, Generation: 1, Task: task}, time.Now()))
			if err := runner.execute(ctx, start); err != nil {
				t.Fatalf("run the architect's start: %v", err)
			}
			machine, found := sup.Machine(token)
			if !found || machine.Claim().Pending == nil {
				t.Fatalf("the architect's claim after its start holds no task")
			}
			sent := machine.Claim().Pending.Task
			if want := strings.Replace(task, ","+bare+",", ","+encodedDocument(tc.want)+",", 1); sent != want {
				t.Errorf("the architect's claim holds the start task:\n%s\nwant:\n%s", sent, want)
			}

			catchUp := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "catch-up", Role: claim.RoleArchitect,
				CatchUp: &record.CatchUp{Generation: 1, Carried: true, Issues: []record.CatchUpIssue{}, Gates: []record.CatchUpGate{
					{Step: record.DesignStepSpec, Gated: true, Document: bare, Artifact: artifact, Version: 2, Approved: 1}}}}, time.Now()))
			if err := runner.execute(ctx, catchUp); err != nil {
				t.Fatalf("run the architect's catch-up: %v", err)
			}
			_, delivered := publisher.snapshot()
			if len(delivered) != 1 {
				t.Fatalf("published %d notices, want the catch-up", len(delivered))
			}
			if got := delivered[0].payload.(record.Notice).CatchUp.Gates[0].Document; got != tc.want {
				t.Errorf("the published catch-up names the spec step's document %q, want %q, the start task's", got, tc.want)
			}
			if tc.client != nil && strings.Join(tc.client.asked, " ") != artifact+" "+artifact {
				t.Errorf("the outbox asked Dispatch of %v, want %s once at each send", tc.client.asked, artifact)
			}
		})
	}
}

// encodedDocument is document as a gates row of a task writes it: a dispatch:// link holds a colon,
// so the row quotes it, and the bare artifact reference needs no quote.
func encodedDocument(document string) string {
	if strings.Contains(document, ":") {
		return `"` + document + `"`
	}
	return document
}
