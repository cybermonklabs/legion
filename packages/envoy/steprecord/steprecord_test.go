package steprecord

import (
	"strings"
	"testing"
)

// AgentTextHolds takes the agent text a step record names as the daemon reads it - a claim's agent
// name, a session id, a `<provider>/<model>` id - and refuses any text with a space, a control
// character or a letter outside printable ASCII, the empty text, and text over 128 characters.
func TestAgentTextHoldsTakesAgentTextAndRefusesTextOutsideTheForm(t *testing.T) {
	bound := "openrouter/vendor/model-4.1:beta_"
	bound += strings.Repeat("x", 128-len(bound))
	for text, want := range map[string]bool{
		"legion-leg-leg-1-implementer":         true,
		"ses_planner":                          true,
		"01a1edce-23dd-726e-b804-626dd5bb3957": true,
		"anthropic/claude-opus-4-1":            true,
		bound:                                  true,
		bound + "x":                            false,
		"":                                     false,
		"first/model with spaces":              false,
		"first/model\ndefault 9 step forged":   false,
		"ses\tone":                             false,
		"first/model\u00a0nbsp":                false,
		"first/modèle":                         false,
		"first/model\x7f":                      false,
	} {
		if got := AgentTextHolds(text); got != want {
			t.Errorf("AgentTextHolds(%q) = %t, want %t", text, got, want)
		}
	}
}

// A step stands at a place from 1, a retro and a merge at the place 0, and a kind that is none of
// the three at no place at all.
func TestPlaceHoldsTheStepFromOneAndTheRetroAndTheMergeAtZero(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		place int
		want  bool
	}{
		{KindStep, 1, true}, {KindStep, 5, true}, {KindStep, 0, false}, {KindStep, -1, false},
		{KindRetro, 0, true}, {KindRetro, 3, false},
		{KindMerge, 0, true}, {KindMerge, 1, false},
		{"phase", 0, false}, {"", 1, false},
	} {
		if got := PlaceHolds(tc.kind, tc.place); got != tc.want {
			t.Errorf("PlaceHolds(%q, %d) = %t, want %t", tc.kind, tc.place, got, tc.want)
		}
	}
}
