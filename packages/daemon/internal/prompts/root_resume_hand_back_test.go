package prompts

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// rootResumeArchitectLines is every line of the three files the root architect's prompt is
// composed of (Compose(claim.RoleArchitect, true)), each prefixed with its file, read from disk.
func rootResumeArchitectLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, path := range []string{"roles/architect-root.md", "go/architect-root.md", "go/architect-common.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			lines = append(lines, path+": "+line)
		}
	}
	return lines
}

// Main's resumeTask (workflow/awaiting_reconcile.go at d46ec4e0) gives the hand-back step only
// while the gate is not open:
//
//	case len(still) > 0 && !gateOpen: " Still with the person: %s. Hand each one back … Handing one back starts your next wait; then end your turn."
//	case len(still) > 0:              " Still with the person: %s. Continue."
//
// and its comment says why: "Handing back is advice only while the gate is not open, since an
// open gate moves the root to planning, where a hand-back starts no wait". INFRA-3686's R2: "a
// step that a text gives today goes into the reader's prompt where the prompt does not give it".
// So every passage of the root architect's prompt that tells it to hand an item that is still
// with the person back, or that a hand-back starts its next wait, holds main's condition.
func TestTheRootResumesHandBackStepsHoldMainsGateNotOpenCondition(t *testing.T) {
	handBack := regexp.MustCompile(`(?i)hand (it|an item|each one|the request) back|handing (an item|one|it) back`)
	condition := regexp.MustCompile("(?i)not open|gate_open: false|closed")
	for _, line := range rootResumeArchitectLines(t) {
		if !strings.Contains(line, "with_person") && !strings.Contains(line, "Handing an item back") {
			continue
		}
		if handBack.MatchString(line) && !condition.MatchString(line) {
			t.Errorf("a hand-back step of the root-resume task drops main's condition that the gate is not open "+
				"(main: with the gate open, \" Still with the person: %%s. Continue.\"):\n%s", line)
		}
	}
}
