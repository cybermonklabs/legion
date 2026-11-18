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
//   - unlisted, the issue whose cause the record carries (LEGION-401).
//
// And eight issues carry no answer at all. Two are at work on a slot of the cap: LEGION-500,
// with its implementer running, and LEGION-501, at phase.Held with no earlier phase on the
// record, which keeps a slot and so stands among the issues at work. Three have left the
// workflow: LEGION-502, which is done, LEGION-503, which a person left on the backlog, and
// LEGION-710, the parked root itself, whose park took its own record out of the workflow while
// the status of the child below it stands. The other three stand past their own review, where
// an issue holds no slot at all since dispatch://INFRA-2560 and so is named by none of the
// three lists, only by `admission.held` under at_work: LEGION-700, whose tree's architect is
// reading the head its worker built, LEGION-701, whose retro implementer is at work, and
// LEGION-702, whose merger rests between its turns. Each of the three is at work in a tree that
// runs, the resting claim no less than the other two: a claim nothing will wake is the daemon's
// own judgement, which the fixture hands over as the stranded set and claim_stopped answers
// first, so what is left for the last arm of the pass is a worker between its turns.
//
// Two more stand past their review and are working on nothing, and each is served `unlisted`
// with the sentence of its held word: LEGION-703, which the daemon holds from its retro, where
// no worker of it runs, reads unclassified, since the ladder gives no held issue at_work
// (record.HeldReasonOf), whose sentence says the workers run; and LEGION-711, a child of a root
// a person parked, whose linger suspends every claim of the tree while its children keep the
// Dispatch status that holds them in the workflow, reads tree_closed, since that linger is still
// running. That tree's lifecycle stands open, so the linger is the whole of what says it runs
// nothing (record.TreeRuns), and it is the one fact that tells LEGION-711 from LEGION-701 beside
// it.
// `unlisted` is the answer that child had before the phases past the review stopped holding a
// slot as well, by another road: the park takes back the slot of every member of a tree it
// stops (record.NeedsSlot), so there too it stood in none of the three lists.
//
// The three states that read a list are proved against the lists the same projection published,
// so a fixture that stops placing an issue where this test says it stands fails here rather
// than drifting.
//
// Every record is built by lineIssue (project_test), this package's one builder of a record
// with the fields each judgement of the pass reads.
func TestWhyStateAnswersEveryIssueThatIsNotWorkingNow(t *testing.T) {
	opened := projectedAt.Add(-2 * time.Hour)
	approved := 1
	// The member its architect held from its retro, and the root a person parked. Neither
	// field is lineIssue's to write: a hold names the phase the work stands at
	// (record.StandsAt), and a linger is the deadline a root carries while the tree it closed
	// may still be taken back (record.Issue.Lingers).
	held := lineIssue("LEGION-703", "LEGION-700", "in_progress", phase.Held, "S")
	held.Hold = &record.Hold{From: phase.Retro, Reason: record.HoldEscalated}
	parked := lineIssue("LEGION-710", "LEGION-710", "backlog", phase.Implementing, "U")
	lingerUntil := projectedAt.Add(time.Hour)
	parked.LingerUntil = &lingerUntil
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
			// The issue the lists do not place: the record holds it in the workflow under the
			// word design_gate — it is the root of its own tree, and this fixture records no gate
			// row for that root, so record.TreeGate.Open() is false of it.
			lineIssue("LEGION-401", "LEGION-401", "in_progress", phase.Implementing, "I"),
			// The four that carry no answer: at work, held in a slot, done, and on the backlog.
			lineIssue("LEGION-500", "LEGION-500", "in_progress", phase.Implementing, "J"),
			lineIssue("LEGION-501", "LEGION-500", "in_progress", phase.Held, "K"),
			lineIssue("LEGION-502", "LEGION-502", "done", phase.Done, "L"),
			lineIssue("LEGION-503", "LEGION-503", "backlog", phase.Admitted, "M"),
			// The tree that runs, every member of which stands past its own review and so
			// holds none of the cap's slots, which the assertion over `admission.active`
			// below is what proves: each of them reaches the last arm of the pass placed by
			// no list, and what tells them apart there is the work rather than the phase.
			// Three are at work — the architect reading the head LEGION-700's worker built,
			// the retro of LEGION-701 and the merge of LEGION-702 — and are served no answer,
			// because `unlisted` would serve a reader of `why` a sentence of the held list about
			// issues whose work is running. The fourth, LEGION-703, the daemon holds from its
			// retro: a hold is what stops the work, so no worker of it runs and it carries the
			// answer the other three do not.
			lineIssue("LEGION-700", "LEGION-700", "in_progress", phase.Accepting, "P"),
			lineIssue("LEGION-701", "LEGION-700", "in_progress", phase.Retro, "Q"),
			lineIssue("LEGION-702", "LEGION-700", "in_progress", phase.Merging, "R"),
			held,
			// The tree a person parked, which is the shape the phase alone cannot tell from
			// the three above: its root's linger suspends every claim of it while its child
			// keeps the Dispatch status that holds it in the workflow, so the record still
			// shows a child at work in its retro and nothing of it will run again until the
			// root is admitted afresh. The root's own lifecycle stands open below, so the
			// linger is the whole of what says the tree runs nothing (record.TreeRuns).
			parked,
			lineIssue("LEGION-711", "LEGION-710", "in_progress", phase.Retro, "T"),
		},
		slots: []record.Slot{
			{Issue: "LEGION-100", Index: 0, AdmittedAt: opened},
			{Issue: "LEGION-500", Index: 1, AdmittedAt: opened},
			{Issue: "LEGION-501", Index: 2, AdmittedAt: opened},
		},
		// Each of the first two trees runs with its design gate approved, which is what makes a
		// first phase of theirs a member of the line the children-first wait then holds back.
		// LEGION-700's tree runs on its own record alone, since every member of it carries a
		// run already; LEGION-710's lifecycle stands open beside it and its tree runs nothing,
		// because its root lingers.
		lifecycles: []string{"LEGION-300", "LEGION-600", "LEGION-700", "LEGION-710"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			// A root at accepting has opened its gate; without this row LEGION-110 would read
			// design_gate's sentence.
			"LEGION-100": {record.DesignStepSpec: {Issue: "LEGION-100", Step: record.DesignStepSpec,
				LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 6, At: opened}, EverOpened: true}},
			"LEGION-300": {record.DesignStepSpec: {Issue: "LEGION-300", Step: record.DesignStepSpec,
				LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true}},
			"LEGION-600": {record.DesignStepSpec: {Issue: "LEGION-600", Step: record.DesignStepSpec,
				LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 8, At: opened}, EverOpened: true}},
			// LEGION-700's root at accepting has opened its gate too; without this row every
			// member of its tree would read design_gate in the held list, and LEGION-703 that
			// word's sentence rather than at_work's.
			"LEGION-700": {record.DesignStepSpec: {Issue: "LEGION-700", Step: record.DesignStepSpec,
				LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 9, At: opened}, EverOpened: true}},
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
	// The claim of one role of one issue, which this fixture builds seven of.
	token := func(issue string, role claim.Role) claim.Token {
		t.Helper()
		token, err := claim.NewToken("legion", issue, role)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	// The stranded set is the daemon's own judgement, handed to the projection: the tree
	// architect of LEGION-100, suspended, and the implementer of LEGION-101, failed. The
	// implementer of LEGION-500 is a live claim and no part of it, so that issue is at work.
	stranded := []supervise.Claim{
		{Token: token("LEGION-100", claim.RoleArchitect), Project: "legion", Tree: "LEGION-100", Issue: "LEGION-100",
			Role: claim.RoleArchitect, State: supervise.StateSuspended},
		{Token: token("LEGION-101", claim.RoleImplementer), Project: "legion", Tree: "LEGION-100", Issue: "LEGION-101",
			Role: claim.RoleImplementer, State: supervise.StateFailed},
	}
	// The workers of the issues past their review, and the one the park suspended. The three
	// of the running tree are a claim at work, a second at work and a third resting between
	// its turns (supervise.StateIdle, one of the four states record's measure counts as no
	// work under way), which is the pair of shapes the last arm must answer alike: the daemon
	// will prompt an idle worker, and the set above is where a claim nothing will wake is
	// named. The suspended worker of LEGION-711 is in no stranded set either, and for the
	// reason the judgement itself gives: it refuses a tree that does not run
	// (workflow.Stranded, over record.TreeRuns), so a park strands nothing and every answer
	// about that tree is the last arm's.
	claims := append([]supervise.Claim{
		{Token: token("LEGION-500", claim.RoleImplementer), Project: "legion", Tree: "LEGION-500", Issue: "LEGION-500",
			Role: claim.RoleImplementer, State: supervise.StateWorking, Session: "ses_implementer_500"},
		{Token: token("LEGION-700", claim.RoleArchitect), Project: "legion", Tree: "LEGION-700", Issue: "LEGION-700",
			Role: claim.RoleArchitect, State: supervise.StateWorking, Session: "ses_architect_700"},
		{Token: token("LEGION-701", claim.RoleImplementer), Project: "legion", Tree: "LEGION-700", Issue: "LEGION-701",
			Role: claim.RoleImplementer, State: supervise.StateWorking, Session: "ses_implementer_701"},
		{Token: token("LEGION-702", claim.RoleMerger), Project: "legion", Tree: "LEGION-700", Issue: "LEGION-702",
			Role: claim.RoleMerger, State: supervise.StateIdle, Session: "ses_merger_702"},
		{Token: token("LEGION-711", claim.RoleImplementer), Project: "legion", Tree: "LEGION-710", Issue: "LEGION-711",
			Role: claim.RoleImplementer, State: supervise.StateSuspended, Session: "ses_implementer_711"},
	}, stranded...)

	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, claims, stranded, nil, projectedAt)
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
			Reason: "The design gate of its tree stands shut: LEGION-401 waits for a person's approval of each gated " +
				"step's document at its current version, and the daemon starts no issue of that tree until it opens."},
		// The two past their review that nothing is working on, each served its held word's
		// sentence: the one its architect held from its retro reads unclassified, since at_work's
		// sentence says the workers run and the ladder gives that word to no held issue, and the
		// child of the parked root, whose linger still runs, reads tree_closed.
		"LEGION-703": {State: api.WhyUnlisted,
			Reason: "The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, " +
				"so no slot has been offered to it. No rule of the held list says what it waits for."},
		"LEGION-711": {State: api.WhyUnlisted,
			Reason: "Its tree is closed: LEGION-710 has finished, or lingers after leaving the workflow, " +
				"and the daemon starts no issue of that tree."},
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
