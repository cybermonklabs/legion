package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The tests of new work on a closed issue whose work merged (D17 of the done-by-checks plan, Task
// 13) that the engine decides alone: a child of a tree that runs, the request that waits on its
// merge read, the read's fact, and how a request ends. Each drives the waitRig of
// close_wait_test.go, whose root works in implementing, so the child's tree is live.

// mnwMerge is the child's merge as GitHub answers the merge read: pull request #43 into main.
var mnwMerge = record.Merge{Number: waitChildNumber, Head: headTwo, SHA: waitChildMerge, Base: "main", Repo: waitRepo, MergedAt: waitMergedAt}

// closedChild is a rig whose root works in implementing and whose child closed at generation 3 with
// its design gate approved. With merged set the record holds the child's merge snapshot
// (ANSWERED). Without it the record holds no row that bears on the merge (NONE).
func closedChild(t *testing.T, rig *waitRig, merged bool) {
	t.Helper()
	rig.seedRoot(phase.Implementing, "in_progress")
	rig.seedChild(waitChild, phase.Done, "done")
	approvedGate(t, rig, waitChild)
	if merged {
		seedRecord(t, rig.pool, func(tx pgx.Tx) error {
			_, err := record.NewStore().PutMergeSnapshot(t.Context(), tx, waitChild, mnwMerge)
			return err
		})
	}
}

// todoBy is the child's move to todo at seq, as who wrote it: "controller", "architect" and
// "operator" are a route's own write (intake.OwnStatusWrite), "person" a Dispatch user's event,
// and "session" an event a session wrote.
func todoBy(who string, seq int64) intake.DispatchIssue {
	fact := intake.DispatchIssue{Key: waitChild, Seq: seq, Type: "issue.updated", Status: "todo", Title: waitChild, Parent: waitRoot,
		Rank: "V", ResolvedRepo: waitRepo, ClaimKnown: true}
	switch who {
	case "controller":
		fact.DaemonActor, fact.StatusMove, fact.Requester = true, true, claim.RoleController
	case "architect":
		fact.DaemonActor, fact.StatusMove, fact.Requester = true, true, claim.RoleArchitect
	case "operator":
		fact.DaemonActor, fact.StatusMove, fact.Requester, fact.PersonActor = true, true, record.RequesterPerson, "operator"
	case "person":
		fact.PersonActor = "a-person"
	case "session":
		fact.ActorSession = "ses-elsewhere"
	}
	return fact
}

// pendingRead is the issue's standing merge_read row, and whether one stands. It fails the test
// where more than one stands: one row stands for an issue at a time.
func (r *waitRig) pendingRead(issue string) (record.MergeRead, bool) {
	r.t.Helper()
	rows := issueOutboxOf(r.t, r.pool, issue, string(record.OutboxKindMergeRead))
	switch len(rows) {
	case 0:
		return record.MergeRead{}, false
	case 1:
	default:
		r.t.Fatalf("%s holds %d merge_read rows, want at most one", issue, len(rows))
	}
	var read record.MergeRead
	if err := json.Unmarshal(rows[0].payload, &read); err != nil {
		r.t.Fatalf("decode the merge_read row of %s: %v", issue, err)
	}
	return read, true
}

// waitsOn fails the test unless the request on issue waits for its merge read: the record stands
// at generation 3 at done with the status todo, no change started, and one merge_read row of that
// generation stands, asked by requester. It answers the row.
func (r *waitRig) waitsOn(issue string, requester claim.Role) record.MergeRead {
	r.t.Helper()
	got := r.issue(issue)
	if got.Generation != waitGeneration || got.Phase != phase.Done || got.Status != "todo" || got.NextChange != 0 {
		r.t.Fatalf("%s stands at generation %d in %s with status %s and change %d, want generation %d at done with todo and no change: the request waits for its merge read",
			issue, got.Generation, got.Phase, got.Status, got.NextChange, waitGeneration)
	}
	read, standing := r.pendingRead(issue)
	if !standing || read.Generation != waitGeneration || read.Requester != requester || read.Request == "" {
		r.t.Fatalf("%s's merge read = %+v (standing %t), want one of generation %d asked by %s with its text", issue, read, standing, waitGeneration, requester)
	}
	return read
}

// requestSeen is one request row (kind request) as these tests read it.
type requestSeen struct {
	id        int64
	role      claim.Role
	text      string
	carriedBy uint64
	withdrawn string
}

// requests is every request row of issue, oldest first.
func (r *waitRig) requests(issue string) []requestSeen {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), `select id, role, text, coalesce(carried_by, 0), coalesce(withdraw_reason, '')
		from defects where issue = $1 and kind = 'request' order by id`, issue)
	if err != nil {
		r.t.Fatalf("read the requests of %s: %v", issue, err)
	}
	defer rows.Close()
	var seen []requestSeen
	for rows.Next() {
		var row requestSeen
		var role string
		var carried int64
		if err := rows.Scan(&row.id, &role, &row.text, &carried, &row.withdrawn); err != nil {
			r.t.Fatalf("scan a request of %s: %v", issue, err)
		}
		row.role, row.carriedBy = claim.Role(role), uint64(carried)
		seen = append(seen, row)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the requests of %s: %v", issue, err)
	}
	return seen
}

// changeFor fails the test unless issue's one next change started at generation 4 carrying one
// request of role with text, and answers that request.
func (r *waitRig) changeFor(issue string, role claim.Role, text string) requestSeen {
	r.t.Helper()
	r.started(issue, changeGeneration, 1)
	got := r.requests(issue)
	if len(got) != 1 || got[0].role != role || got[0].carriedBy != changeGeneration || got[0].text != text {
		r.t.Fatalf("the requests of %s = %+v, want one of %s carried by %d with the text %q", issue, got, role, changeGeneration, text)
	}
	return got[0]
}

// answered is the merge read's fact for issue at generation, carrying merge.
func (r *waitRig) answered(issue string, generation uint64, merge record.Merge) {
	r.t.Helper()
	r.fact(intake.MergeRead{Issue: issue, Generation: generation, Merge: merge})
}

// A request for new work on a closed child whose record shows no merge (NONE) waits for its merge
// read, and survives every later event while it waits: a rank change and a label change before the
// read lands lose nothing, and the read starts one change carrying the request as it stood. Each
// request keeps who asked and its text through the read. A person's later request makes the
// pending request the person's, before the read's first turn and after its fact was prepared. A
// later request of a session, the controller's or an architect's, changes neither the person nor
// the text, and neither does an event a session wrote.
func TestAPendingRequestSurvivesItsUnreadMergeRead(t *testing.T) {
	t.Parallel()

	t.Run("rank and label events while the read waits lose nothing", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, false)
		rig.fact(todoBy("controller", 2))
		read := rig.waitsOn(waitChild, claim.RoleController)
		rank := todoBy("person", 3)
		rank.Rank = "W"
		rig.fact(rank)
		label := todoBy("person", 4)
		label.Rank, label.Labels = "W", []string{"legion", "ops"}
		rig.fact(label)
		if again := rig.waitsOn(waitChild, claim.RoleController); again.Request != read.Request {
			t.Fatalf("the request after the rank and label events = %q, want it as it stood, %q", again.Request, read.Request)
		}
		rig.answered(waitChild, waitGeneration, mnwMerge)
		rig.changeFor(waitChild, claim.RoleController, read.Request)
	})

	for _, who := range []string{"person", "operator", "architect"} {
		t.Run("a request of "+who+" keeps its asker and its text through the read", func(t *testing.T) {
			t.Parallel()
			rig := newWaitRig(t, "")
			closedChild(t, rig, false)
			rig.fact(todoBy(who, 2))
			asker := map[string]claim.Role{"person": record.RequesterPerson, "operator": record.RequesterPerson, "architect": claim.RoleArchitect}[who]
			read := rig.waitsOn(waitChild, asker)
			rig.answered(waitChild, waitGeneration, mnwMerge)
			rig.changeFor(waitChild, asker, read.Request)
		})
	}

	// progress is where the read stands when the person's request comes: before its first turn, or
	// past its last GET with its answer saved, whose fact the executor has prepared.
	for _, tc := range []struct {
		name     string
		progress *record.MergeReadProgress
	}{
		{name: "before the read's first turn"},
		{name: "after the read's fact was prepared", progress: &record.MergeReadProgress{Stage: record.MergeReadStageCandidates, Attempts: 3,
			Candidates: []record.MergeReadCandidate{{Repo: waitRepo, Number: waitChildNumber, Source: record.MergeReadSourceLink,
				Answer: &record.MergeReadAnswer{Merged: true, Names: true, Head: headTwo, SHA: waitChildMerge, Base: "main", MergedAt: waitMergedAt}}}}},
	} {
		t.Run("a person's later request replaces the controller's "+tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWaitRig(t, "")
			closedChild(t, rig, false)
			rig.fact(todoBy("controller", 2))
			controller := rig.waitsOn(waitChild, claim.RoleController)
			if tc.progress != nil {
				encoded, err := json.Marshal(tc.progress)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := rig.pool.Exec(t.Context(), `update outbox set payload = jsonb_set(payload, '{progress}', $2::jsonb)
					where issue = $1 and kind = 'merge_read'`, waitChild, string(encoded)); err != nil {
					t.Fatalf("stand the read at its prepared fact: %v", err)
				}
			}
			rig.fact(todoBy("operator", 3))
			person := rig.waitsOn(waitChild, record.RequesterPerson)
			if person.Request == controller.Request {
				t.Fatalf("the pending request kept the controller's text %q, want the operator's", controller.Request)
			}
			if tc.progress != nil && person.Progress.Attempts != tc.progress.Attempts {
				t.Fatalf("the read's progress after the person's request = %+v, want the executor's as it stood", person.Progress)
			}
			rig.answered(waitChild, waitGeneration, mnwMerge)
			rig.changeFor(waitChild, record.RequesterPerson, person.Request)
		})
	}

	t.Run("a later request of a session or an architect, and a session's event, change neither person nor text", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, false)
		rig.fact(todoBy("person", 2))
		person := rig.waitsOn(waitChild, record.RequesterPerson)
		rig.fact(todoBy("controller", 3))
		rig.fact(todoBy("architect", 4))
		session := todoBy("session", 5)
		session.Rank = "W"
		rig.fact(session)
		if again := rig.waitsOn(waitChild, record.RequesterPerson); again.Request != person.Request {
			t.Fatalf("the pending request after later requests = %q, want the person's, %q", again.Request, person.Request)
		}
		rig.answered(waitChild, waitGeneration, mnwMerge)
		rig.changeFor(waitChild, record.RequesterPerson, person.Request)
	})
}

// A MergeRead fact of the pending row's issue and generation applies under the fact lock: it saves
// its merge through the snapshot's one writer (record.Store.PutMergeSnapshot) and starts one next
// change. Where a later merge's snapshot already stands, the fact's older merge changes no field of
// it, and one change still starts. A fact of an earlier generation, and one whose request no longer
// stands, applies nothing.
func TestAMergeReadAnswerKeepsTheNewerSnapshot(t *testing.T) {
	t.Parallel()

	t.Run("the read's merge is saved and one change starts", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, false)
		rig.fact(todoBy("controller", 2))
		read := rig.waitsOn(waitChild, claim.RoleController)
		rig.answered(waitChild, waitGeneration, mnwMerge)
		rig.changeFor(waitChild, claim.RoleController, read.Request)
		if got := rig.issue(waitChild).Merge; !got.MergedAt.Equal(mnwMerge.MergedAt) || got.Number != mnwMerge.Number || got.SHA != mnwMerge.SHA ||
			got.Head != mnwMerge.Head || got.Base != mnwMerge.Base {
			t.Fatalf("the snapshot after the read = %+v, want the read's merge %+v", got, mnwMerge)
		}
	})

	t.Run("a newer snapshot stands over the read's older merge", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, false)
		rig.fact(todoBy("controller", 2))
		read := rig.waitsOn(waitChild, claim.RoleController)
		newer := record.Merge{Number: 44, Head: headThree, SHA: waitLaterMerge, Base: "main", Repo: waitRepo, MergedAt: waitMergedAt.Add(time.Hour)}
		seedRecord(t, rig.pool, func(tx pgx.Tx) error {
			_, err := record.NewStore().PutMergeSnapshot(t.Context(), tx, waitChild, newer)
			return err
		})
		rig.answered(waitChild, waitGeneration, mnwMerge)
		rig.changeFor(waitChild, claim.RoleController, read.Request)
		if got := rig.issue(waitChild).Merge; got.Number != newer.Number || got.SHA != newer.SHA || got.Head != newer.Head || !got.MergedAt.Equal(newer.MergedAt) {
			t.Fatalf("the snapshot after the read = %+v, want the newer merge %+v kept", got, newer)
		}
	})

	t.Run("a fact of an earlier generation applies nothing", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, false)
		rig.fact(todoBy("controller", 2))
		rig.waitsOn(waitChild, claim.RoleController)
		rig.answered(waitChild, waitGeneration-1, mnwMerge)
		rig.waitsOn(waitChild, claim.RoleController)
		if got := rig.issue(waitChild).Merge; got.Merged() {
			t.Fatalf("the snapshot after a fact of generation %d = %+v, want none", waitGeneration-1, got)
		}
	})

	t.Run("a fact whose request no longer stands applies nothing", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, false)
		rig.fact(todoBy("controller", 2))
		rig.waitsOn(waitChild, claim.RoleController)
		backlog := todoBy("controller", 3)
		backlog.Status = "backlog"
		rig.fact(backlog)
		if _, standing := rig.pendingRead(waitChild); standing {
			t.Fatalf("the merge read stands after the controller's backlog, want the request cancelled")
		}
		rig.answered(waitChild, waitGeneration, mnwMerge)
		rig.noChange(waitChild, waitGeneration)
		if got := rig.issue(waitChild).Merge; got.Merged() {
			t.Fatalf("the snapshot after a fact of no standing request = %+v, want none", got)
		}
	})
}

// A person's request ends with no pull request only on that person's word: the owner's withdrawal
// of it is refused DEFECT_REQUEST_OF_A_PERSON, and the change stays open. The person's done closes
// the issue and marks the request closed_by_person. A request of the controller, withdrawn with its
// evidence, ends the change with no pull request: the issue stands in verifying again.
func TestAPersonsRequestDoesNotEndOnTheOwnersWithdrawal(t *testing.T) {
	t.Parallel()

	t.Run("a person's request", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, true)
		rig.fact(todoBy("person", 2))
		request := rig.requests(waitChild)
		rig.started(waitChild, changeGeneration, 1)
		if len(request) != 1 || request[0].role != record.RequesterPerson {
			t.Fatalf("the requests of %s = %+v, want one of a person", waitChild, request)
		}
		result := rig.fact(intake.DefectWithdrawal{Issue: waitChild, Defect: request[0].id, Reason: "the work is done: #43 holds it", By: rig.rootArchitect()})
		if result.Refusal == nil || result.Refusal.Code != "DEFECT_REQUEST_OF_A_PERSON" {
			t.Fatalf("the owner's withdrawal of a person's request = %+v, want it refused DEFECT_REQUEST_OF_A_PERSON", result.Refusal)
		}
		rig.started(waitChild, changeGeneration, 1)
		started := rig.issue(waitChild)
		done := todoBy("person", started.LastDispatchSeq+1)
		done.Status, done.Type = "done", "issue.closed"
		rig.fact(done)
		if got := rig.issue(waitChild).Phase; got != phase.Done {
			t.Fatalf("%s stands in %s after the person's done, want done", waitChild, got)
		}
		if got := rig.requests(waitChild); len(got) != 1 || got[0].withdrawn != record.DefectClosedByPerson {
			t.Fatalf("the request after the person's done = %+v, want it ended %s", got, record.DefectClosedByPerson)
		}
	})

	t.Run("a request of the controller", func(t *testing.T) {
		t.Parallel()
		rig := newWaitRig(t, "")
		closedChild(t, rig, true)
		rig.fact(todoBy("controller", 2))
		request := rig.requests(waitChild)
		rig.started(waitChild, changeGeneration, 1)
		if len(request) != 1 || request[0].role != claim.RoleController {
			t.Fatalf("the requests of %s = %+v, want one of the controller", waitChild, request)
		}
		rig.withdraw(waitChild, request[0].id, "the work is done: #43 holds it")
		// The change opened no pull request, and the withdrawal resolved its one reason: the fresh
		// read of the change's generation, which every end with no pull request waits for, ends it.
		rig.readAt(waitChild, changeGeneration, answerOf(waitChild, nil, passing("c-after", waitChildMerge)))
		got := rig.issue(waitChild)
		if got.Phase != phase.Verifying || got.NextChange != 0 || got.Generation != changeGeneration {
			t.Fatalf("%s stands in %s with change %d at generation %d after the withdrawal, want the change %d ended in verifying",
				waitChild, got.Phase, got.NextChange, got.Generation, changeGeneration)
		}
	})
}
