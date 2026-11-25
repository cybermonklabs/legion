package workflow

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main's catch-up of a kept decision (workflow/catch_up.go via workflow/parked_decision.go at
// d46ec4e0 and c03d3afe) named it with no time: "While nobody in %s's tree could hear it, a person
// decided on the tree: %s (%s by %s)". The branch adds a time, and round 13 keeps the time a person
// approved a spec document on the kept row's words (the daemon's noticeWords: "An approval and a
// carried gate's opening carry no words but the act's time"). Its kept-decisions task names that
// time in `at` (KeptDecisionsOf), and go/architect-root.md says so: "A decision's `at` is when the
// person made it, or, where the event that reported it named no time, ... when the daemon kept the
// decision". Of a catch-up it says "act on each decision it names, as on those of a
// `kept-decisions` task". But the catch-up's `at` (keptFacts) is still when the daemon kept the
// decision, and the person's time goes in `answer_at`, which no prompt line names, so the same
// approval reaches one architect at two times under one key. INFRA-3686's R2: "The fields are
// those of the kind, the issue, the act, the actor, the time, ..."; test handoff v12 item 20 (a
// parked approval's `at` is the park time while the prompt says it is when the person decided),
// which kind-audit-r13.md's catch-up row says "also reaches this reader".
func TestACatchUpNamesWhenThePersonMadeAKeptApproval(t *testing.T) {
	const root = "LEGION-208"
	approved := time.Date(2026, 11, 21, 21, 12, 53, 0, time.UTC)
	kept := time.Date(2026, 11, 22, 3, 12, 0, 0, time.UTC)
	decision := record.ParkedDecision{Ref: stepRef(root, record.DesignStepSpec), Issue: root, Decision: record.DecisionDesignApproved,
		Actor: "user:alice", At: kept, Words: &record.ActWords{By: "user:alice", At: approved}}
	owed := record.KeptDecisions{Decisions: []record.DecisionMark{decision.Mark()}}

	task := KeptDecisionsOf(root, owed)
	if len(task.Kept) != 1 || !task.Kept[0].At.Equal(approved) {
		t.Fatalf("the kept-decisions task names %+v; want the approval at %s, when the person approved", task.Kept, approved)
	}
	facts, _ := keptFacts(owed)
	if len(facts.Decisions) != 1 {
		t.Fatalf("the catch-up names the kept decisions %+v; want the one approval", facts.Decisions)
	}
	named := facts.Decisions[0]
	if named.At.Equal(approved) {
		return
	}
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("prompts: %v", err)
	}
	parts, err := composer.Compose(claim.RoleArchitect, true)
	if err != nil {
		t.Fatalf("compose the root architect's prompt: %v", err)
	}
	for _, path := range parts.RolePromptPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(body), "`answer_at`") {
			return
		}
	}
	t.Errorf("the catch-up names the kept approval at %s, when the daemon kept it, and the person's time %s only in answer_at, which no prompt line "+
		"names; the kept-decisions task of the same approval names %s in `at`, and go/architect-root.md tells the architect to act on a catch-up's "+
		"decisions as on those of a kept-decisions task, whose `at` \"is when the person made it\"", named.At.Format(time.RFC3339),
		named.AnswerAt.Format(time.RFC3339), task.Kept[0].At.Format(time.RFC3339))
}
