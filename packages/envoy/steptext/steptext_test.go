package steptext

import (
	"strings"
	"testing"
)

// Holds takes the agent text a step record names as the daemon reads it - a claim's agent name, a
// session id, a `<provider>/<model>` id - and refuses any text with a space, a control character or
// a letter outside printable ASCII, the empty text, and text over 128 characters.
func TestHoldsTakesAgentTextAndRefusesTextOutsideTheForm(t *testing.T) {
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
		if got := Holds(text); got != want {
			t.Errorf("Holds(%q) = %t, want %t", text, got, want)
		}
	}
}
