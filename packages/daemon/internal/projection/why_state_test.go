package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Every issue that is not working now carries one rendered answer on its own view (api.WhyView),
// and the fixture below holds an issue of every state and of every fill the daemon can render
// from this record:
//
//   - claim_stopped, read first and whatever slot the issue holds: LEGION-100 is at work in a
//     slot of the cap and its tree's architect is stranded, LEGION-101's own phase worker is;
//   - waiting, the sentence the line already rendered for the member (LEGION-200), with one more
//     clause for a member a link holds as well (LEGION-201);
//   - blocked, the three judgements the published list has merged by then: the cycle of
//     LEGION-301, whose own blocked_by link holds it under its parent, the open children below
//     LEGION-300, and the Dispatch links LEGION-110 waits on — and LEGION-601, which both of
//     the last two hold at once and which reads both sentences, the children and then the link;
//   - unlisted, the issue this build records no cause for (LEGION-401). The second fill this
//     test had went with the phase it read: on this contract no issue stands in
//     `awaiting_merge`, since the merger merges the head it reads and the daemon closes the
//     issue when it reads that merge (dispatch://INFRA-714 R2).
//
// And four issues carry no answer at all: LEGION-500, at work in a slot with its implementer
// running; LEGION-501, at phase.Held, which holds a slot and so stands among the issues at work;
// LEGION-502, which is done; and LEGION-503, which a person left on the backlog. The three states
// that read a list are proved against the lists the same projection published, so a fixture that
// stops placing an issue where this test says it stands fails here rather than drifting.
//
// Every record is built by lineIssue (project_test), this package's one builder of a record
// with the fields each judgement of the pass reads.
func TestWhyStateAnswersEveryIssueThatIsNotWorkingNow(t *testing.T) {
	opened := projectedAt.Add(-2 * time.Hour)
	approved := 1
	store := projectionStore{
		issues: []record.Issue{
			// The tree whose work has stopped: its architect's claim is stranded, and its child
			// LEGION-101's implementer is. LEGION-110 is held at admitted by two Dispatch links.
			lineIssue("LEGION-100", "LEGION-100", "in_progress", phase.Accepting, "A"),
			lineIssue("LEGION-101", "LEGION-100", "in_progress", phase.Implementing, "B"),
			lineIssue("LEGION-110", "LEGION-100", "todo", phase.Admitted, "C"),
			// The two queued roots of the line, the second of them held by a link as well.
			lineIssue("LEGION-200", "LEGION-200", "todo", phase.Admitted, "D"),
			lineIssue("LEGION-201", "LEGION-201", "todo", phase.Admitted, "E"),
			// The tree whose gate has opened and whose own first phase waits for the Dispatch
			// children under it, one of which waits for the parent above it in turn.
			lineIssue("LEGION-300", "LEGION-300", "in_progress", phase.Admitted, "F"),
			lineIssue("LEGION-301", "LEGION-300", "todo", phase.Admitted, "G"),
			// The second such tree, whose child LEGION-601 both halves hold at once: the open
			// Dispatch children below it, and a link of its own that is not done.
			lineIssue("LEGION-600", "LEGION-600", "in_progress", phase.Admitted, "N"),
			lineIssue("LEGION-601", "LEGION-600", "todo", phase.Admitted, "O"),
			// The issue the lists do not place: the record holds it in the workflow with no cause
			// this build can name.
			lineIssue("LEGION-401", "LEGION-401", "in_progress", phase.Implementing, "I"),
			// The four that carry no answer: at work, held in a slot, done, and on the backlog.
			lineIssue("LEGION-500", "LEGION-500", "in_progress", phase.Implementing, "J"),
			lineIssue("LEGION-501", "LEGION-500", "in_progress", phase.Held, "K"),
			lineIssue("LEGION-502", "LEGION-502", "done", phase.Done, "L"),
			lineIssue("LEGION-503", "LEGION-503", "backlog", phase.Admitted, "M"),
		},
		slots: []record.Slot{
			{Issue: "LEGION-100", Index: 0, AdmittedAt: opened},
			{Issue: "LEGION-500", Index: 1, AdmittedAt: opened},
			{Issue: "LEGION-501", Index: 2, AdmittedAt: opened},
		},
		// Each of the two trees runs with its design gate approved, which is what makes a first
		// phase of theirs a member of the line the children-first wait then holds back.
		lifecycles: []string{"LEGION-300", "LEGION-600"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-300": {record.DesignStepSpec: {Issue: "LEGION-300", Step: record.DesignStepSpec,
				LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true}},
			"LEGION-600": {record.DesignStepSpec: {Issue: "LEGION-600", Step: record.DesignStepSpec,
				LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 8, At: opened}, EverOpened: true}},
		},
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-110", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900", "LEGION-901"}},
			{Key: "LEGION-201", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-900"}},
			{Key: "LEGION-300", Project: "LEGION", Status: "in_progress"},
			// The three open children of LEGION-300, in the order Dispatch lists them; the first
			// is blocked_by the parent above it, which is the cycle the record names.
			{Key: "LEGION-301", Project: "LEGION", Status: "todo", Parent: "LEGION-300", Blockers: []string{"LEGION-300"}},
			{Key: "LEGION-302", Project: "LEGION", Status: "todo", Parent: "LEGION-300"},
			{Key: "LEGION-303", Project: "LEGION", Status: "todo", Parent: "LEGION-300"},
			{Key: "LEGION-900", Project: "LEGION", Status: "in_progress"},
			{Key: "LEGION-901", Project: "LEGION", Status: "todo"},
			// The tree of LEGION-601, whose own two open children stand below it while
			// LEGION-900 holds it by a link: the two halves of `blocked` on one issue.
			{Key: "LEGION-600", Project: "LEGION", Status: "in_progress"},
			{Key: "LEGION-601", Project: "LEGION", Status: "todo", Parent: "LEGION-600", Blockers: []string{"LEGION-900"}},
			{Key: "LEGION-602", Project: "LEGION", Status: "todo", Parent: "LEGION-601"},
			{Key: "LEGION-603", Project: "LEGION", Status: "todo", Parent: "LEGION-601"},
		},
	}
	// The stranded set is the daemon's own judgement, handed to the projection: the tree
	// architect of LEGION-100, suspended, and the implementer of LEGION-101, failed. The
	// implementer of LEGION-500 is a live claim and no part of it, so that issue is at work.
	architect, err := claim.NewToken("legion", "LEGION-100", claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	implementer, err := claim.NewToken("legion", "LEGION-101", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	running, err := claim.NewToken("legion", "LEGION-500", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	stranded := []supervise.Claim{
		{Token: architect, Project: "legion", Tree: "LEGION-100", Issue: "LEGION-100", Role: claim.RoleArchitect, State: supervise.StateSuspended},
		{Token: implementer, Project: "legion", Tree: "LEGION-100", Issue: "LEGION-101", Role: claim.RoleImplementer, State: supervise.StateFailed},
	}
	claims := append([]supervise.Claim{
		{Token: running, Project: "legion", Tree: "LEGION-500", Issue: "LEGION-500", Role: claim.RoleImplementer,
			State: supervise.StateWorking, Session: "ses_implementer_500"},
	}, stranded...)

	got, err := Project(context.Background(), nil, store, "LEGION", nil, claims, stranded, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	// The three lists this pass reads its states off, proved before the sentences are: a fixture
	// that no longer places an issue where this test reads it would otherwise prove nothing.
	if want := []string{"LEGION-100", "LEGION-500", "LEGION-501"}; !reflect.DeepEqual(got.Admission.Active, want) {
		t.Fatalf("admission.active = %#v, want %#v", got.Admission.Active, want)
	}
	if want := []string{"LEGION-200", "LEGION-201"}; !reflect.DeepEqual(got.Admission.Waiting, want) {
		t.Fatalf("admission.waiting = %#v, want %#v", got.Admission.Waiting, want)
	}
	want := []api.BlockedIssue{
		{Issue: "LEGION-110", BlockedBy: []string{"LEGION-900", "LEGION-901"}},
		{Issue: "LEGION-201", BlockedBy: []string{"LEGION-900"}},
		{Issue: "LEGION-300", BlockedBy: []string{"LEGION-301", "LEGION-302", "LEGION-303"}},
		{Issue: "LEGION-301", BlockedBy: []string{"LEGION-300"}},
		{Issue: "LEGION-600", BlockedBy: []string{"LEGION-601"}},
		{Issue: "LEGION-601", BlockedBy: []string{"LEGION-900", "LEGION-602", "LEGION-603"}},
	}
	if !reflect.DeepEqual(got.Admission.Blocked, want) {
		t.Fatalf("admission.blocked = %#v, want %#v", got.Admission.Blocked, want)
	}

	line := got.Issues["LEGION-201"].WaitingLine
	if line == nil {
		t.Fatalf("LEGION-201 stands in the line and carries no waitingLine: %#v", got.Issues["LEGION-201"])
	}
	answers := map[string]api.WhyView{
		"LEGION-100": {State: api.WhyClaimStopped,
			Reason: "Its phase is accepting and its tree's architect claim is stranded at suspended, so nothing will wake it."},
		"LEGION-101": {State: api.WhyClaimStopped,
			Reason: "Its phase is implementing and the implementer's claim is stranded at failed, so nothing will wake it."},
		"LEGION-200": {State: api.WhyWaiting,
			Reason: "It has not started. It is 1st of 2 issues that have not started, by its own Dispatch rank."},
		// The member's own sentence and one clause more, because the line holds it and a link
		// holds it at once; the line's own `reason` carries no such clause.
		"LEGION-201": {State: api.WhyWaiting,
			Reason: "It has not started. It is 2nd of 2 issues that have not started, by its own Dispatch rank. " +
				"It waits on LEGION-900, which is not done."},
		"LEGION-110": {State: api.WhyBlocked,
			Reason: "It waits on LEGION-900 and LEGION-901, which are not done."},
		"LEGION-300": {State: api.WhyBlocked,
			Reason: "Its own first phase waits for the open issues below it: LEGION-301, LEGION-302 and LEGION-303."},
		"LEGION-301": {State: api.WhyBlocked,
			Reason: "A blocked_by link holds it under LEGION-300, which stands above it in its own tree."},
		"LEGION-600": {State: api.WhyBlocked,
			Reason: "Its own first phase waits for the open issues below it: LEGION-601."},
		// Both halves hold this one, and it reads both sentences in the order `blocked` merges
		// them: closing the children alone would not free it, which the children sentence on
		// its own would tell a reader it would.
		"LEGION-601": {State: api.WhyBlocked,
			Reason: "Its own first phase waits for the open issues below it: LEGION-602 and LEGION-603. " +
				"It waits on LEGION-900, which is not done."},
		"LEGION-401": {State: api.WhyUnlisted,
			Reason: "The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, " +
				"so no slot has been offered to it. This build records no cause."},
	}
	for key, view := range got.Issues {
		answer, answered := answers[key]
		switch {
		case !answered && view.Why != nil:
			t.Errorf("%s carries why %+v, want none: it is at work, done or out of the workflow", key, view.Why)
		case !answered:
		case view.Why == nil:
			t.Errorf("%s carries no why, want %+v", key, answer)
		case *view.Why != answer:
			t.Errorf("%s's why = %+v, want %+v", key, *view.Why, answer)
		}
	}
	for key := range answers {
		if _, projected := got.Issues[key]; !projected {
			t.Errorf("%s is absent from the projected state, so its why proves nothing", key)
		}
	}
	// The clause the blocked member takes is the issue's own and never the line's: the two are
	// separate fields, and a reader of `waitingLine.reason` reads the place alone.
	if line.Reason != "It has not started. It is 2nd of 2 issues that have not started, by its own Dispatch rank." {
		t.Errorf("LEGION-201's waitingLine.reason = %q, want the place alone", line.Reason)
	}
}
