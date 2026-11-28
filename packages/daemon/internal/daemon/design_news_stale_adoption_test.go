package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A phase task that carries news of the approved design is rendered anew at every send from the
// start's own text its news holds (designNews, renderNews), not from the text the delivery held.
// The commit in which the adoption's update of a stale working copy left the edits that no jj
// command had recorded is recorded on the claim after that adoption (supervise's LeftEdits), and
// the sentence that names it is composed into every prompt from that record, after the render
// (supervise's Delivery.message): the reviewer's phase task, carrying the news of version 5, is
// sent naming the commit, its prompt is refused, and the next send of the same task, whose
// adoption names no commit and whose text the render writes again, still names it, with the
// news, and the two sentences of the reviewer's round still last.
func TestAStaleAdoptionsCommitStaysInEverySendOfATaskThatCarriesNews(t *testing.T) {
	t.Parallel()
	const commit = "c45ff7e33d4899a2b99db849241fe128115021b0"
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.sup.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{Name: "legion-reviewer[bot]", Email: "reviewer@example.com"}, nil
	}
	r.told(pathRoot, record.AudienceHolder, 4)
	r.told(pathRoot, record.AudienceArchitect, 4)

	// The tester completes, and a person approves version 5 before the reviewer's start runs: the
	// reconcile leaves the news to that start, whose phase task carries it.
	r.complete(pathRoot)
	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	r.run(ofKind(record.OutboxKindDesignChanges))
	starts := r.run(startOf(pathRoot, claim.RoleReviewer, false))
	if len(starts) != 1 {
		t.Fatalf("the reviewer's phase starts = %d, want one", len(starts))
	}

	// The reviewer comes up with its working copy stale: the adoption answers the commit that holds
	// the edits, and the agent refuses the prompt.
	r.rt.AnswerAdoptWorkingCopy(runtime.Adoption{EditsCommits: []string{commit}})
	token := mustClaimToken(t, pathRoot, claim.RoleReviewer)
	reviewer, found := r.sup.Machine(token)
	if !found {
		t.Fatal("the reviewer's start created no claim")
	}
	conn := fake.NewConn()
	conn.RefusePrompt("the model provider refused the request")
	r.conns.Register(token, conn)
	reviewer.Wait()
	generation := reviewer.Claim().Generation
	session := "ses-legion-800-reviewer"
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: session, SessionFile: "/tmp/" + session + ".jsonl",
			CapabilityHash: []byte("capability-of-" + session)},
		supervise.RequestReady{Claim: token, Generation: generation, Session: session},
	} {
		if err := reviewer.Handle(r.ctx, ev); err != nil {
			t.Fatalf("handle %T of %s: %v", ev, token, err)
		}
	}
	reviewer.Wait()

	_, round, named := reviewround.Split(starts[0].payload.(record.SuperviseRequest).Task)
	if !named {
		t.Fatalf("the reviewer's start names no round at its end: %q", starts[0].payload.(record.SuperviseRequest).Task)
	}
	says := func(when string, message string) {
		t.Helper()
		for _, want := range []string{
			"Before this task, the working copy was stale.",
			"left the edits that no jj command had recorded in commit " + commit + ", and they are no longer on disk",
			"The approved design of LEGION-800 changed for LEGION-800, which you hold in reviewing:",
			"version 5, since version 4, which your phase last took",
		} {
			if !strings.Contains(message, want) {
				t.Fatalf("%s, the reviewer was prompted %q, want it to say %q", when, message, want)
			}
		}
		if !strings.HasSuffix(message, round) {
			t.Fatalf("%s, the reviewer was prompted %q, want it to end with the two sentences of its round", when, message)
		}
	}
	first := conn.Prompts()
	if len(first) != 1 {
		t.Fatalf("the reviewer's prompts = %+v, want the phase task, once", first)
	}
	says("at the first send", first[0].Message)

	// The next send of the same task: its adoption names no commit, and the render writes its text
	// again from the news it carries.
	r.rt.AnswerAdoptWorkingCopy(runtime.Adoption{})
	conn.FailPrompt(nil)
	r.hello(reviewer)
	again := promptsAfter(conn, 1)
	if len(again) != 1 {
		t.Fatalf("the reviewer's prompts after the refused one = %+v, want the task sent again, once", again)
	}
	says("at the send after the refused one", again[0].Message)
	if left := reviewer.Claim().LeftEdits; !left.Tells() || len(left.Commits) != 1 || left.Commits[0] != commit {
		t.Fatalf("the reviewer's claim records %+v, want %s untold: no turn has taken a prompt that named it", left, commit)
	}
}
