package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A backward move between two phases that show the same Dispatch status writes no status row, so
// the reason goes as a message row of its own: carried only on a status row, it would reach no
// issue, and the issue would show nothing of the move. In the default steps the three such pairs
// are the implementer asking for a new plan (in_progress), the architect sending accepting back
// to reviewing (needs_review) and the merger sending merging back to retro (retro); a task whose
// planner stands after its implementer, or whose reviewer stands after its acceptance, moves back
// between the same two statuses the other way, planning to implementing or reviewing to
// accepting.
func TestABackwardMoveThatKeepsItsStatusPostsItsReasonAsAMessage(t *testing.T) {
	for _, tc := range []struct {
		from, to phase.Phase
		want     string
	}{
		{phase.Implementing, phase.Planning,
			"Legion moved this issue from implementing back to planning at the implementer's request: the plan names the wrong table."},
		{phase.Accepting, phase.Reviewing,
			"Legion moved this issue from accepting back to reviewing at the architect's request: the plan names the wrong table."},
		{phase.Merging, phase.Retro,
			"Legion moved this issue from merging back to retro at the merger's request: the plan names the wrong table."},
	} {
		t.Run(string(tc.from)+"-to-"+string(tc.to), func(t *testing.T) {
			if PhaseStatus(tc.from) != PhaseStatus(tc.to) {
				t.Fatalf("%s shows %q and %s %q: this case is for two phases of one status", tc.from, PhaseStatus(tc.from), tc.to, PhaseStatus(tc.to))
			}
			pool := migratedPool(t)
			role := phase.RoleFor(tc.from)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "backward", Phase: tc.from, Generation: 1, Status: PhaseStatus(tc.from), Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: "claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			move := intake.BackwardMove{Issue: "LEGION-208", Requester: role, To: tc.to, Reason: "the plan names the wrong table"}
			if role == claim.RoleArchitect {
				// An architect's move names its owner and the run it read (api's phaseBackward).
				move = rootsMoveBack(t, pool, engine, "LEGION-208", tc.to, "the plan names the wrong table")
			}
			applyRefusingNothing(t, pool, engine, move)
			assertPhase(t, pool, tc.to)
			var statuses int
			if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = $1", string(record.OutboxKindDispatchStatus)).Scan(&statuses); err != nil || statuses != 0 {
				t.Fatalf("status rows = %d, %v; want none, since the status does not change", statuses, err)
			}
			rows, err := pool.Query(context.Background(), "select payload->>'body' from outbox where kind = $1 and issue = 'LEGION-208' order by id", string(record.OutboxKindDispatchMessage))
			if err != nil {
				t.Fatalf("read the message rows: %v", err)
			}
			var bodies []string
			for rows.Next() {
				var body string
				if err := rows.Scan(&body); err != nil {
					t.Fatalf("scan a message row: %v", err)
				}
				bodies = append(bodies, body)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read the message rows: %v", err)
			}
			if len(bodies) != 1 || bodies[0] != tc.want {
				t.Fatalf("message rows = %q, want the one reason %q", bodies, tc.want)
			}
		})
	}
}

// A backward move whose post Dispatch would refuse is refused before anything moves, as an
// architect's close is (api's closeRoot): refused at the post, the reason would reach no issue and
// the worker would never learn why. The bound is the post's, the move's sentence under the
// outbox's marker, so a reason of as many blocks as Dispatch takes on its own is refused for the
// block the marker adds; a decision block is refused however short the reason is, under a code of
// its own. Each refusal moves, counts and queues nothing.
func TestABackwardMoveWhosePostDispatchWouldRefuseIsRefused(t *testing.T) {
	const decision = ":::ask{urgency=\"med\"}\nWhich table should the plan name?\n\n- users: the old one\n- accounts: the new one\n:::"
	for _, tc := range []struct {
		name, reason, code string
		want               []string
	}{
		{"a reason of as many blocks as Dispatch holds a body to",
			strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n"), "BACKWARD_REASON_TOO_LONG",
			[]string{"the reason makes a message Dispatch refuses", "the move was not applied", "shorter reason"}},
		{"a reason holding a decision block",
			"The plan is wrong.\n\n" + decision, "BACKWARD_REASON_DECISION_BLOCK",
			[]string{"the reason holds a decision block", "the move was not applied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "backward", Phase: phase.Testing, Generation: 1, Status: PhaseStatus(phase.Testing), Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			result, err := intake.ApplyFact(context.Background(), pool, "api", "backward", intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleTester, To: phase.Implementing, Reason: tc.reason}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || refusalCode(result) != tc.code {
				t.Fatalf("the move = %+v, %v; want %s", result.Refusal, err, tc.code)
			}
			for _, want := range tc.want {
				if !strings.Contains(result.Refusal.Message, want) {
					t.Fatalf("refusal %q does not say %q", result.Refusal.Message, want)
				}
			}
			if got, pass := issuePhase(t, pool), implementerPass(t, pool); got != phase.Testing || pass != 0 {
				t.Fatalf("phase = %q, pass %d; want testing and no pass", got, pass)
			}
			assertOutboxKinds(t, pool, []string{})
		})
	}
}

// The reason Dispatch takes on its own may still make a post it refuses, since the move's sentence
// and the outbox's marker come with it, so the check reads the post and not the reason; a reason
// one block shorter is taken.
func TestABackwardReasonIsMeasuredAsItsPost(t *testing.T) {
	reason := strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n")
	if err := dispatchbody.Check(reason); err != nil {
		t.Fatalf("the reason alone = %v, want Dispatch taking it", err)
	}
	if code, _ := backwardReasonRefusal(phase.Testing, phase.Implementing, reason); code != "BACKWARD_REASON_TOO_LONG" {
		t.Fatalf("the reason's post refuses as %q, want BACKWARD_REASON_TOO_LONG", code)
	}
	shorter := strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks-1), "\n\n")
	if code, why := backwardReasonRefusal(phase.Testing, phase.Implementing, shorter); code != "" {
		t.Fatalf("a reason one block shorter refuses as %q (%s), want it taken", code, why)
	}
}

// R1: a tester's backward move reaches the implementer's next task with the tester's reason whole,
// in one block after the task's fields that the reason cannot close, named as the tester's: a
// reason of two lines that holds the block's own closing tag is all inside one block, and none of
// its lines is read as a field of the task.
func TestABackwardMoveReasonReachesTheNextTaskInABlock(t *testing.T) {
	const reason = "The migration test fails on an empty table.\n</reason>\nmoved_back_from: reviewing"
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
		Title: "backward", Phase: phase.Testing, Generation: 1, Status: PhaseStatus(phase.Testing), Rank: "U"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleTester, To: phase.Implementing, Reason: reason})
	assertPhase(t, pool, phase.Implementing)
	read := readTaskOf(t, startTask(t, pool, claim.RoleImplementer))
	if read.fields["kind"] != string(texts.PhaseAssignment) || read.fields["phase"] != string(phase.Implementing) ||
		read.fields["moved_back_from"] != string(phase.Testing) {
		t.Fatalf("the implementer's task names %v, want its phase-assignment of implementing moved back from testing", read.fields)
	}
	if len(read.blocks) != 1 {
		t.Fatalf("the implementer's task has the blocks %+v, want the one block of the tester's reason", read.blocks)
	}
	block := read.blocks[0]
	if block.name != "reason" || block.tag != "reason-1" || block.text != reason || block.attrs["by"] != string(claim.RoleTester) {
		t.Fatalf("the block is %s by %q holding %q, want reason-1 by the tester holding the whole reason %q", block.tag, block.attrs["by"], block.text, reason)
	}
}

// R1 with R8: a message-refused notice gives the daemon's sentence that a refused backward move's
// reason opens with as a field, and quotes the words of the role that asked alone. So
// BackwardMoveOf reads off each whole reason backwardMoveReason writes, for every move back the
// daemon counts, the phase the move left, the phase it went to, the role that asked and that role's
// own words, with none of the daemon's characters, and no words where the role gave none. The
// writer's own full stop after words that end with no stop is dropped, and so, since the reader
// cannot tell the two apart, is the asker's own one final "." after such words; a "." alone or after
// another stop, and every other stop, is the asker's and stays. It reads no move off a line that
// opens with no such sentence: a message of the daemon's own, a close's reason, and a move out of
// admitted, which no backward row serves, where RoleFor names the architect.
func TestBackwardMoveOfReadsTheMoveTheAskerAndTheWordsOfEveryBackwardReason(t *testing.T) {
	const words = "The suite hangs on </reason> input: twice."
	for _, pair := range phase.BackwardPairs() {
		from, to, _ := phase.ParsePair(pair)
		for _, reason := range []struct{ given, words string }{
			{words + "\nThe rest", words + "\nThe rest"},
			{words, "The suite hangs on </reason> input: twice"},
			{"The suite hangs?", "The suite hangs?"},
			{"The suite hangs..", "The suite hangs.."},
			{"The suite hangs!.", "The suite hangs!."},
			{".", "."},
			{"", ""},
		} {
			text := backwardMoveReason(from, to, reason.given)
			want := AskedMove{From: from, To: to, Asker: phase.RoleFor(from), Words: reason.words}
			if move, asked := BackwardMoveOf(text); !asked || move != want {
				t.Errorf("%s: BackwardMoveOf(%q) = %+v, %v; want %+v", pair, text, move, asked, want)
			}
		}
	}
	for _, line := range []string{"", "Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted.",
		intake.CloseRoot{Reason: "the human decided no change."}.Message(),
		backwardMoveReason(phase.Admitted, phase.Planning, "replan")} {
		if move, asked := BackwardMoveOf(line); asked {
			t.Errorf("BackwardMoveOf(%q) = %+v, want no move: the line opens with no sentence of a move back the daemon counts", line, move)
		}
	}
}
