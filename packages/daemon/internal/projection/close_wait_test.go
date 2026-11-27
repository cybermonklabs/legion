package projection

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// withStoredWait is issue with wait as its stored wait (issues.close_wait).
func withStoredWait(t *testing.T, issue record.Issue, wait record.StoredWait) record.Issue {
	t.Helper()
	encoded, err := json.Marshal(wait)
	if err != nil {
		t.Fatalf("encode the stored wait of %s: %v", issue.Key, err)
	}
	issue.CloseWait = encoded
	return issue
}

// The state names what each issue's close waits for (D11 of dispatch://INFRA-5580/artifact/plan-md):
// an issue in verifying carries `closeWait` with each item of its stored wait and since when it
// stands, since when the issue waits and how many next changes it started, and its `why` reads
// `waiting`, naming the first thing the close waits for. An issue in retro or merging that holds a
// reported defect carries `closeWait` with its defects alone, whatever else an earlier wait left
// in its record, since nothing else of the close is read before the merge. An issue in retro with
// no stored wait, and one in merging whose wait holds no defect, carry none.
func TestTheStateNamesWhatTheCloseWaitsFor(t *testing.T) {
	merged := projectedAt.Add(-3 * time.Hour)
	entered := merged.Add(time.Minute)
	read := merged.Add(time.Hour)
	reported := merged.Add(2 * time.Hour)
	nextLook := projectedAt.Add(time.Hour)
	reason := record.WaitReason{ID: 7, Kind: record.DefectKindDefect, Text: "the retry never stops", Role: claim.RoleMerger,
		Head: "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90", Since: reported}

	verifying := lineIssue("LEGION-800", "LEGION-800", "testing", phase.Verifying, "A")
	verifying.CloseWaitSince = &merged
	verifying.NextChanges = 2
	verifying = withStoredWait(t, verifying, record.StoredWait{
		Entered:      entered,
		Checks:       []record.WaitCheck{{ID: "c-after", When: "after_merge", Answer: "no", Reason: "fail", Since: read}},
		Production:   &record.WaitProduction{State: "open", Generation: 1, Merge: "c0ffee", NextLookAt: &nextLook, Since: entered},
		OpenChildren: []record.WaitIssue{{Key: "LEGION-850", Status: "backlog", Since: read}},
		Defects:      []record.WaitReason{reason},
	})
	retro := lineIssue("LEGION-801", "LEGION-801", "retro", phase.Retro, "B")
	retro.NextChanges = 1
	retro = withStoredWait(t, retro, record.StoredWait{
		Entered: entered,
		Checks:  []record.WaitCheck{{ID: "c-old", When: "after_merge", Answer: "no", Reason: "fail", Since: read}},
		Defects: []record.WaitReason{reason},
	})
	store := projectionStore{
		issues: []record.Issue{
			verifying,
			retro,
			lineIssue("LEGION-802", "LEGION-802", "retro", phase.Retro, "C"),
			withStoredWait(t, lineIssue("LEGION-803", "LEGION-803", "retro", phase.Merging, "D"), record.StoredWait{Entered: entered}),
		},
		lifecycles: []string{"LEGION-800", "LEGION-801", "LEGION-802", "LEGION-803"},
	}

	got, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	want := &api.CloseWaitView{
		Since:        &merged,
		Checks:       []api.CloseWaitCheckView{{ID: "c-after", When: "after_merge", Answer: "no", Reason: "fail", Since: read}},
		Production:   &api.CloseWaitProductionView{State: "open", Generation: 1, Merge: "c0ffee", NextLookAt: &nextLook, Since: entered},
		OpenChildren: []api.CloseWaitIssueView{{Key: "LEGION-850", Status: "backlog", Since: read}},
		Defects: []api.CloseWaitDefectView{{ID: "7", Kind: "defect", Role: "merger", Text: "the retry never stops",
			Head: "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90", Since: reported}},
		NextChanges: 2,
	}
	if got := got.Issues["LEGION-800"].CloseWait; !reflect.DeepEqual(got, want) {
		t.Errorf("LEGION-800's closeWait = %+v, want %+v", got, want)
	}
	why := got.Issues["LEGION-800"].Why
	if why == nil || why.State != api.WhyWaiting || !strings.Contains(why.Reason, "c-after") {
		t.Errorf("LEGION-800's why = %+v, want waiting, naming c-after, the first thing its close waits for", why)
	}

	wantRetro := &api.CloseWaitView{Defects: want.Defects, NextChanges: 1}
	if got := got.Issues["LEGION-801"].CloseWait; !reflect.DeepEqual(got, wantRetro) {
		t.Errorf("LEGION-801's closeWait = %+v, want its reported defect alone, %+v", got, wantRetro)
	}
	for _, key := range []string{"LEGION-802", "LEGION-803"} {
		if got := got.Issues[key].CloseWait; got != nil {
			t.Errorf("%s's closeWait = %+v, want none", key, got)
		}
	}
}
