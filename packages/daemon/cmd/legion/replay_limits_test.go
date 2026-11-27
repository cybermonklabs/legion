package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// `legion replay limits` refuses what it cannot plan from, before it reads a window: a missing
// window, models file, baseline or ceiling, a ceiling of zero or below, a token limit of zero or
// below, and a --plan with no --runs to give the arguments of its runs, each as a usage error that
// names the flag; and a window it cannot read, with its reason, as a failure, so a plan is never
// made from nothing. Its help names the readings the output rests on.
func TestReplayLimitsRefusesWhatItCannotPlanFrom(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	given := []string{"--window-one", filepath.Join(dir, "one"), "--window-two", filepath.Join(dir, "two"),
		"--models", filepath.Join(dir, "a.json"), "--models-reverse", filepath.Join(dir, "b.json"), "--baseline", dir, "--ceiling", "171"}
	without := func(flag string) []string {
		var rest []string
		for i := 0; i < len(given); i += 2 {
			if given[i] != flag {
				rest = append(rest, given[i], given[i+1])
			}
		}
		return rest
	}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no flags", nil, "--window-one is required"},
		{"no window two", without("--window-two"), "--window-two is required"},
		{"no models", without("--models"), "--models is required"},
		{"no reverse models", without("--models-reverse"), "--models-reverse is required"},
		{"no baseline", without("--baseline"), "--baseline is required"},
		{"no ceiling", without("--ceiling"), "--ceiling is required"},
		{"a ceiling of zero", append(without("--ceiling"), "--ceiling", "0"), "--ceiling 0"},
		{"a token limit of zero", append(slices.Clone(given), "--token-limit", "0"), "--token-limit 0"},
		{"a plan with no runs", append(slices.Clone(given), "--plan", filepath.Join(dir, "plan.json")), "--runs is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := legionRun(t, append([]string{"replay", "limits"}, c.args...)...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, c.want) || !strings.Contains(stderr, "legion replay limits") {
				t.Errorf("legion replay limits %v = %d, stdout %q, stderr %q; want a usage error, 2, naming %q", c.args, code, stdout, stderr, c.want)
			}
		})
	}
	code, stdout, stderr := legionRun(t, append([]string{"replay", "limits"}, given...)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "legion replay limits:") {
		t.Errorf("legion replay limits over windows that do not exist = %d, stdout %q, stderr %q; want a failure, 1, from legion replay limits", code, stdout, stderr)
	}
	code, stdout, stderr = legionRun(t, "replay", "limits", "--help")
	if code != 0 || stderr != "" {
		t.Fatalf("legion replay limits --help = %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"50th, 75th, 90th, 95th and 99th percentile", "pass the test across the two records in both outputs", "--token-limit",
		"d of 10 minutes and of 4", "extrapolation", "the larger of the two windows' scorecard maxima", "refused minutes the model gives under the token limit"} {
		if !strings.Contains(strings.Join(strings.Fields(stdout), " "), want) {
			t.Errorf("legion replay limits --help does not say %q:\n%s", want, stdout)
		}
	}
}
