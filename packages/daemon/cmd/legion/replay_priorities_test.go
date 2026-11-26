package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// ancestorMissingWindow is the fixture window of the replay package that lacks the history of
// ACMEAUX-1, an ancestor on which the priority Dispatch resolves for ACME-2 rests.
var ancestorMissingWindow = filepath.Join("..", "..", "internal", "replay", "testdata", "windows", "p0-inherited-ancestor-missing")

// `legion replay run` under a rule with the new P0 start, main since cybermonklabs/legion#5488,
// refuses a window that lacks the history of an ancestor a priority rests on, naming the ancestor,
// before it opens any database (spec R10: report the missing input and make no comparison that
// depends on it).
func TestReplayRunRefusesANewP0RuleOnAWindowThatLacksAnAncestor(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := legionRun(t, "replay", "run", "--window", ancestorMissingWindow, "--rule", "main", "--pg-dsn", "postgres://unused",
		"--out", t.TempDir())
	if code != 1 || stdout != "" || !strings.Contains(stderr, "ACMEAUX-1") || !strings.Contains(stderr, "ACME-2") {
		t.Errorf("legion replay run under main = %d, stdout %q, stderr %q; want 1 and a refusal naming ACMEAUX-1 above ACME-2", code, stdout, stderr)
	}
}

// `legion replay scorecard` of a window that lacks the history of an ancestor a priority rests on
// prints no priority taken in today's line's order, which rests on that priority, and names the
// missing ancestor in its place.
func TestReplayScorecardNamesTheAncestorsAWindowLacks(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := legionRun(t, "replay", "scorecard", "--window", ancestorMissingWindow)
	if code != 0 {
		t.Fatalf("legion replay scorecard = %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "\npriorityTakenBanded ") {
		t.Errorf("legion replay scorecard prints priorityTakenBanded of a window that lacks ACMEAUX-1:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ACMEAUX-1") {
		t.Errorf("legion replay scorecard does not name ACMEAUX-1, the ancestor the window lacks:\n%s", stdout)
	}
}
