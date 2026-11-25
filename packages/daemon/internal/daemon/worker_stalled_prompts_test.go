package daemon

//ci:reads skills/legion-controller/references/wakes.md TestTheWorkerStalledPromptsSayWhatTheDaemonDidToTheTurn reads it under ../../../.. through workerStalledReadRepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workerStalledReadRepo reads a file of the repository from disk, as
// TestEveryStepTakenOutOfATextIsInItsReadersPrompt does.
func workerStalledReadRepo(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(goldenRepositoryRoot, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// workerStalledBetween is the text of body from the first start up to the first end after it.
func workerStalledBetween(t *testing.T, body, start, end, file string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("%s holds no %q", file, start)
	}
	rest := body[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("%s holds no %q after %q", file, end, start)
	}
	return rest[:j]
}

// workerStalledPassages is the worker-stalled passage of each reader's prompt: the architect's
// (go/architect-common.md, which a root and a child architect load) and the controller's
// (roles/controller-root.md, for the stall of a tree's architect, enqueueStallNotice).
func workerStalledPassages(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		goPrompts + "architect-common.md": workerStalledBetween(t, workerStalledReadRepo(t, goPrompts+"architect-common.md"),
			"A `worker-stalled` notice", "A `worker-silent` notice", "architect-common.md"),
		rolePrompts + "controller-root.md": workerStalledBetween(t, workerStalledReadRepo(t, rolePrompts+"controller-root.md"),
			"A `worker-stalled` notice", "A `provider-errors` notice", "controller-root.md"),
	}
}

// Main's worker-stalled notice opened with what the daemon did to the stalled turn
// (daemon/stallbackstop.go:stallTurnClause at d46ec4e0): "the daemon ended the {role}'s turn" (the
// stop confirmed), "the daemon took the {role}'s turn off its claim" (the stop unconfirmed), "the
// daemon took back the {role}'s task without ending its turn" (the parked relaunch, no stop made).
// The branch's record holds `end` and `stop`. INFRA-3686's R2: "a step that a text gives today goes
// into the reader's prompt where the prompt does not give it". Each reader's passage must then say
// what became of the turn, the more since skill://legion-controller/references/wakes.md says of
// every worker-stalled notice "The daemon ended a stalled turn of the tree's architect", which
// main's notice corrected for the two stops that did not end it.
func TestTheWorkerStalledPromptsSayWhatTheDaemonDidToTheTurn(t *testing.T) {
	wakes := workerStalledReadRepo(t, "skills/legion-controller/references/wakes.md")
	if !strings.Contains(wakes, "The daemon ended a stalled turn of the tree's architect") {
		t.Fatalf("wakes.md no longer says the daemon ended the stalled turn; read the skill again")
	}
	for file, passage := range workerStalledPassages(t) {
		if !strings.Contains(passage, "without ending its turn") && !strings.Contains(passage, "did not end the turn") &&
			!strings.Contains(passage, "turn is not ended") {
			t.Errorf("%s: no word of the worker-stalled passage says that `stop: not-made` (`end: relaunch parked`) took back the task without ending the turn, as main's \"the daemon took back the {role}'s task without ending its turn\" did", file)
		}
		if !strings.Contains(passage, "off its claim") && !strings.Contains(passage, "off the claim") {
			t.Errorf("%s: no word of the worker-stalled passage says that `stop: unconfirmed` took the turn off the claim, as main's \"the daemon took the {role}'s turn off its claim\" did", file)
		}
	}
}

// Main's worker-stalled notice (daemon/stallbackstop.go:stallRecordInWords at d46ec4e0) named the
// setting whose bound the agent passed: "past the %s this deployment gives the role
// (worker_stall_backstop_hours)". The branch's record carries the bound's value alone (`bound`),
// the setting's name being the same in every notice of the kind. INFRA-3686's R2: "No field holds
// a sentence that is the same in every text of its kind" and "a step that a text gives today goes
// into the reader's prompt where the prompt does not give it". So each reader's prompt must name
// the setting.
func TestTheWorkerStalledPromptsNameTheBackstopSetting(t *testing.T) {
	for file, passage := range workerStalledPassages(t) {
		if !strings.Contains(passage, "worker_stall_backstop_hours") {
			t.Errorf("%s: the worker-stalled passage names no `worker_stall_backstop_hours`; main's notice said the bound was the one \"this deployment gives the role (worker_stall_backstop_hours)\"", file)
		}
	}
}
