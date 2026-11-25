package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Main's carried-gate-opened reason (workflow/gate.go:applyArtifact at d46ec4e0), which the outbox
// relayed as the parked-decision notice's reason (daemon/parked_decision.go:parkDecision): "a
// human approved %s's spec document at version %d, which opens the gate %s carried; any child
// waiting on it starts", the old root first and the carrying issue second. The branch sends
// issue: the carrying issue and ref: the old root's document (workflow.CarriedGateOpenedNotice,
// record.ParkedDecisionNoticeOf). INFRA-3686's R2: "a step that a text gives today goes into the
// reader's prompt where the prompt does not give it"; the change's own wrong fact is its defect
// (the architect's ruling). roles/controller-root.md must say that the approved spec is the
// document `ref` names, not the spec of `issue`.
func TestAParkedCarriedGateSaysWhoseSpecWasApproved(t *testing.T) {
	opened := workflow.CarriedGateOpenedNotice("LEGION-1", "LEGION-9", record.DesignStepSpec, 3, "user:alice",
		time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	parked := record.ParkedDecisionNoticeOf("LEGION-1", "LEGION-1",
		record.ParkedDecision{Ref: opened.Ref, Decision: record.DecisionCarriedGateOpened, Actor: opened.Actor, At: opened.At}, opened.Version)
	if parked.Issue != "LEGION-1" || !strings.Contains(parked.Ref, "LEGION-9") {
		t.Fatalf("the parked carried-gate-opened notice is issue %q ref %q; want issue LEGION-1 (the carrier) and ref on LEGION-9 (the old root)", parked.Issue, parked.Ref)
	}
	held, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(rolePrompts+"controller-root.md")))
	if err != nil {
		t.Fatalf("read controller-root.md: %v", err)
	}
	var line, whole string
	for _, l := range strings.Split(string(held), "\n") {
		if strings.Contains(l, "`decision: carried-gate-opened` means") {
			_, line, _ = strings.Cut(l, "`decision: carried-gate-opened` means")
			line = "`decision: carried-gate-opened` means" + line
		}
		if strings.HasPrefix(l, "A `parked-decision` notice says") {
			whole = l
		}
	}
	if line == "" || whole == "" {
		t.Fatalf("controller-root.md has no parked-decision passage or no carried-gate-opened line")
	}
	if strings.Contains(line, "the issue's spec") {
		t.Errorf("controller-root.md says %q, but the notice's `issue` is the carrier (%s) and the spec a person approved is the old root's (%s, the document `ref` names); main said \"a human approved LEGION-9's spec document at version 3, which opens the gate LEGION-1 carried\"",
			line, parked.Issue, parked.Ref)
	}
	if strings.Contains(whole, "the issue it was decided on (`issue`)") {
		t.Errorf("controller-root.md says the parked-decision's `issue` is \"the issue it was decided on\", but a carried-gate-opened decision was made on %s's document while `issue` is %s", parked.Ref, parked.Issue)
	}
}
