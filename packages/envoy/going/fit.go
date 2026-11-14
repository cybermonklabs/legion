package going

// fit.go fits the two thresholds of R5 on a model's per-case answers. "At or above" holds
// throughout: a case at a threshold is a case the threshold admits.

// fitShareNumerator and fitShareDenominator are the least share of the cases a threshold must
// leave at or above it, 60%, held as a fraction so the comparison is exact in integers.
const (
	fitShareNumerator   = 3
	fitShareDenominator = 5
)

// ActionOutcome is one case's answer to the action question. Answered is false where the call
// gave no answer, a case that counts in the share's denominator and is never at or above any
// value; Right is whether the chosen action is the case's label; Confidence is the answer's own
// confidence in that choice.
type ActionOutcome struct {
	Answered   bool
	Right      bool
	Confidence float64
}

// DisplayOutcome is one case's answer to the display question: whether a chart is the right
// display for the case, whether the model answered chart, and its confidence in that answer. A
// case with no answer is one not answered chart.
type DisplayOutcome struct {
	ChartWorthy bool
	Chart       bool
	Confidence  float64
}

// Fit is a fitted threshold. Exists is whether any value satisfies the threshold's rule at all,
// Threshold that value, Share the share of the cases the rule counts that are at or above it,
// and Holds whether a value exists and Share is at least 60%. A fit that does not hold is no
// threshold: a deployment acts on none.
type Fit struct {
	Threshold float64
	Exists    bool
	Share     float64
	Holds     bool
}

// FitActionThreshold is R5's action threshold: the lowest confidence of a case with no wrong
// choice at or above it, holding only where at least 60% of all the cases are at or above it.
// Candidate values are the cases' own confidences, so the threshold is the lowest confidence
// above every wrong choice's; a right case tied with a wrong one has a wrong choice at its value
// and is no candidate.
func FitActionThreshold(outcomes []ActionOutcome) Fit {
	var wrongTop float64
	wrong := false
	for _, o := range outcomes {
		if o.Answered && !o.Right && (!wrong || o.Confidence > wrongTop) {
			wrongTop, wrong = o.Confidence, true
		}
	}
	var fit Fit
	for _, o := range outcomes {
		if o.Answered && (!wrong || o.Confidence > wrongTop) && (!fit.Exists || o.Confidence < fit.Threshold) {
			fit.Threshold, fit.Exists = o.Confidence, true
		}
	}
	if !fit.Exists {
		return fit
	}
	admitted := 0
	for _, o := range outcomes {
		if o.Answered && o.Confidence >= fit.Threshold {
			admitted++
		}
	}
	return fit.counted(admitted, len(outcomes))
}

// FitChartThreshold is R5's chart-display threshold: the lowest display confidence of a
// chart-worthy case answered chart that is above the display confidence of every case answered
// chart that is not chart-worthy, holding only where at least 60% of the chart-worthy cases are
// at or above it. A chart-worthy case answered prose counts against that 60% and is no wrong
// display; a case that is not chart-worthy and was answered prose is no wrong display either.
func FitChartThreshold(outcomes []DisplayOutcome) Fit {
	var wrongTop float64
	wrong := false
	worthy := 0
	for _, o := range outcomes {
		if o.ChartWorthy {
			worthy++
		}
		if o.Chart && !o.ChartWorthy && (!wrong || o.Confidence > wrongTop) {
			wrongTop, wrong = o.Confidence, true
		}
	}
	var fit Fit
	for _, o := range outcomes {
		if o.Chart && o.ChartWorthy && (!wrong || o.Confidence > wrongTop) && (!fit.Exists || o.Confidence < fit.Threshold) {
			fit.Threshold, fit.Exists = o.Confidence, true
		}
	}
	if !fit.Exists {
		return fit
	}
	admitted := 0
	for _, o := range outcomes {
		if o.Chart && o.ChartWorthy && o.Confidence >= fit.Threshold {
			admitted++
		}
	}
	return fit.counted(admitted, worthy)
}

// counted is the fit with its share of admitted cases out of total, and whether that share
// reaches 60%.
func (fit Fit) counted(admitted, total int) Fit {
	fit.Share = float64(admitted) / float64(total)
	fit.Holds = admitted*fitShareDenominator >= total*fitShareNumerator
	return fit
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
