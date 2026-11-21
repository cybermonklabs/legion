package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// acceptPhase moves the root of the status harness's tree into at, the phase that makes it an
// ancestor the daemon has already accepted.
func acceptPhase(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	if _, err := h.store.Pool().Exec(context.Background(), "update issues set phase = $2 where key = $1", key, string(at)); err != nil {
		t.Fatalf("move %s into %s: %v", key, at, err)
	}
}

// holdFrom stands key at phase.Held, held from at: where a root whose worker died stands. The
// head its task's last step passed is wherever the hold left it, so a root held from retro or
// merging is accepted still (record.Accepted).
func holdFrom(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	if _, err := h.store.Pool().Exec(context.Background(), "update issues set phase = $2, held_from = $3 where key = $1",
		key, string(phase.Held), string(at)); err != nil {
		t.Fatalf("hold %s from %s: %v", key, at, err)
	}
}

// A6. Both doors refuse a child under a root held from an accepted phase, as they refuse one
// under a root standing in that phase itself: a worker that dies leaves its issue at held with
// the phase it left recorded, and the head the last step of its task passed is accepted still.
// Read by the phase alone the hold let work in beneath such a head — rerun_child and
// release_children both took it — and the waiting line offered that work a slot of its own.
func TestTheDoorsRefuseAChildUnderARootHeldFromAnAcceptedPhase(t *testing.T) {
	for _, from := range []phase.Phase{phase.Retro, phase.Merging} {
		t.Run(string(from), func(t *testing.T) {
			h, _, statuses := statusHarness(t)
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("park = %d %s", recorder.Code, recorder.Body)
			}
			holdFrom(t, h, statusRoot, from)

			recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
			if sentence := refused(t, recorder, http.StatusConflict, "CHILD_UNDER_ACCEPTED_PARENT"); !strings.Contains(sentence, statusRoot) {
				t.Fatalf("rerun refusal under a root held from %s says %q, want it to name %s", from, sentence, statusRoot)
			}

			// The same for a child Dispatch holds in backlog that the daemon has never recorded,
			// which release_children reads the chain of from the Dispatch parent it read.
			const grandchild = "LEGION-300"
			parent := statusChild
			statuses.issues[grandchild] = dispatch.Issue{Key: grandchild, ResolvedRepo: "acme/widgets", Title: "grandchild",
				Status: "backlog", Parent: &parent, Rank: "C", LastSeq: 7}
			writes := len(statuses.writes)

			recorder = h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{grandchild}}, nil)
			if sentence := refused(t, recorder, http.StatusConflict, "CHILD_UNDER_ACCEPTED_PARENT"); !strings.Contains(sentence, statusRoot) {
				t.Fatalf("release refusal under a root held from %s says %q, want it to name %s", from, sentence, statusRoot)
			}
			if len(statuses.writes) != writes {
				t.Fatalf("Dispatch writes of the refused release = %#v, want none", statuses.writes[writes:])
			}
		})
	}
}

// A6. rerun_child is refused while an ancestor of the child has already been accepted: an issue
// entering the workflow beneath a head on its way to the merge queue would let that head merge
// without carrying it. The refusal names the ancestor, and is lifted once that ancestor closes.
func TestRerunChildIsRefusedUnderAnAcceptedAncestor(t *testing.T) {
	for _, at := range []phase.Phase{phase.Retro, phase.Merging} {
		t.Run(string(at), func(t *testing.T) {
			h, _, _ := statusHarness(t)
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("park = %d %s", recorder.Code, recorder.Body)
			}
			acceptPhase(t, h, statusRoot, at)

			recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
			if sentence := refused(t, recorder, http.StatusConflict, "CHILD_UNDER_ACCEPTED_PARENT"); !strings.Contains(sentence, statusRoot) {
				t.Fatalf("rerun refusal under a root in %s says %q, want it to name %s", at, sentence, statusRoot)
			}

			// Once the ancestor has closed there is no order left to keep, and the rerun runs.
			acceptPhase(t, h, statusRoot, phase.Done)
			if recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("rerun once the ancestor closed = %d %s, want 200", recorder.Code, recorder.Body)
			}
		})
	}
}

// The refusal under an accepted ancestor names the ancestor's head as the head the last step of
// its task passed, which is true of every plan: an issue stands in retro or merging
// (record.Accepted) only once its task's last step has passed, whatever that step is, while a
// plan with no reviewer step (`implementer tester`) has no reviewed head at all.
func TestTheRefusalUnderAnAcceptedAncestorNamesTheHeadItsLastStepPassed(t *testing.T) {
	h, _, _ := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("park = %d %s", recorder.Code, recorder.Body)
	}
	acceptPhase(t, h, statusRoot, phase.Retro)

	message := refused(t, h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil),
		http.StatusConflict, "CHILD_UNDER_ACCEPTED_PARENT")
	want := statusChild + " is beneath " + statusRoot + ", whose head the last step of its task passed and the daemon has already accepted, and work entering the workflow beneath " +
		statusRoot + " now would let it merge a head that does not carry that work; run it again once " + statusRoot + " has closed, or take " + statusRoot + " back with a backward move first"
	if message != want {
		t.Fatalf("the refusal = %q, want %q", message, want)
	}
}

// A6. release_children is refused the same way, for a child Dispatch holds in backlog that the
// daemon has never recorded: the chain is read from the Dispatch parent the release itself read,
// and it is walked past a parent already done, so a grandchild under a done parent is refused by
// whichever issue above it is still merging.
func TestWaveReleaseIsRefusedForAGrandchildUnderADoneParent(t *testing.T) {
	h, _, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	acceptPhase(t, h, statusRoot, phase.Merging)
	acceptPhase(t, h, statusChild, phase.Done)
	const grandchild = "LEGION-300"
	parent := statusChild
	statuses.issues[grandchild] = dispatch.Issue{Key: grandchild, ResolvedRepo: "acme/widgets", Title: "grandchild",
		Status: "backlog", Parent: &parent, Rank: "C", LastSeq: 7}

	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{grandchild}}, nil)
	sentence := refused(t, recorder, http.StatusConflict, "CHILD_UNDER_ACCEPTED_PARENT")
	if !strings.Contains(sentence, statusRoot) || !strings.Contains(sentence, grandchild) {
		t.Fatalf("release refusal of a grandchild under a merged root says %q, want it to name %s and %s", sentence, grandchild, statusRoot)
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the whole wave is refused before anything is written", statuses.writes)
	}
	if recorded := recordedChild(t, h, grandchild); recorded != nil {
		t.Fatalf("the refused grandchild = %#v, want it unrecorded", recorded)
	}

	// With the root back before acceptance the same release is taken.
	acceptPhase(t, h, statusRoot, phase.Implementing)
	if recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{grandchild}}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("release once the root is back in implementing = %d %s, want 200", recorder.Code, recorder.Body)
	}
}

// A6. The refusal is the daemon's own reading of its records, so the ancestor it names is the
// one record.AcceptedAncestor finds over the tree: the nearest issue above the child in retro or
// merging, found by walking past a parent that is already done.
func TestTheAcceptedAncestorIsFoundByWalkingThePartlyDoneChain(t *testing.T) {
	h, _, _ := statusHarness(t)
	acceptPhase(t, h, statusRoot, phase.Merging)
	acceptPhase(t, h, statusChild, phase.Done)
	parent := statusChild
	h.recordIssue(record.Issue{Key: "LEGION-300", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: statusRoot, Parent: &parent, Project: testDispatchProject, Title: "grandchild", Phase: phase.Admitted,
		Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 7})

	var ancestor string
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		ancestor, err = record.AcceptedAncestorOf(context.Background(), record.NewStore(), tx, statusRoot, "LEGION-300", "")
		return err
	}); err != nil {
		t.Fatalf("read the accepted ancestor: %v", err)
	}
	if ancestor != statusRoot {
		t.Fatalf("the accepted ancestor of the grandchild = %q, want %s", ancestor, statusRoot)
	}
}
