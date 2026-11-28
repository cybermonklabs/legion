package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The state names the refusal for good of an issue's branch row that the record holds at the
// generation the issue stands in now (record.Store.BranchRefusals): the row, what it wrote, the
// refusal's kind, the row's error as the row kept it, and when. An issue the record holds no such
// refusal of carries none.
func TestTheStateNamesARefusedBranch(t *testing.T) {
	refusedAt := projectedAt.Add(-5 * time.Minute)
	store := projectionStore{
		issues: []record.Issue{
			lineIssue("LEGION-800", "LEGION-800", "in_progress", phase.Admitted, "A"),
			lineIssue("LEGION-801", "LEGION-801", "in_progress", phase.Admitted, "B"),
		},
		branchRefusals: []record.BranchRefusal{{Issue: "LEGION-800", Generation: 2, Row: 121, Write: record.BranchWriteMove, Kind: "no-node-id",
			Reason: "read the node id of acme/widgets: GitHub named none", RefusedAt: refusedAt}},
	}
	got, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	want := &api.BranchRefusedView{Row: 121, Write: record.BranchWriteMove, Kind: "no-node-id",
		Reason: "read the node id of acme/widgets: GitHub named none", At: refusedAt}
	if got := got.Issues["LEGION-800"].BranchRefused; !reflect.DeepEqual(got, want) {
		t.Errorf("LEGION-800's branchRefused = %+v, want %+v", got, want)
	}
	if got := got.Issues["LEGION-801"].BranchRefused; got != nil {
		t.Errorf("LEGION-801's branchRefused = %+v, want none", got)
	}
}
