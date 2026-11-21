package going

// fit.go scores a model's per-case answers to the action question, as the bake-off
// (cmd/going-bakeoff) records them.

// ActionOutcome is one case's answer to the action question. Answered is false where the call
// gave no answer, a case that counts as a choice that was not right; Right is whether the chosen
// action is the case's label.
type ActionOutcome struct {
	Answered bool
	Right    bool
}

// Accuracy is the share of the outcomes whose choice was right; a case with no answer is not.
// It is zero for no outcomes.
func Accuracy(outcomes []ActionOutcome) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	right := 0
	for _, o := range outcomes {
		if o.Answered && o.Right {
			right++
		}
	}
	return float64(right) / float64(len(outcomes))
}
