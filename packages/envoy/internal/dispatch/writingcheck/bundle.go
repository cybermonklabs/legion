package writingcheck

import "github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"

// The bundle: what the check enforces, frozen with the measurement that chose it. Settings can
// turn more off (DISPATCH_WRITING_CHECK_RULES_OFF, DISPATCH_WRITING_CHECK_DETECTOR) and never turn
// on what the bundle leaves off.
//
// The measurement: the rule-cost run of `writing-check rule-cost` over 600 drawn agent writes
// (seed 3231, manifest sha256 a8a58b9cb8bbc5cd0228c83ead18cba27dd16035757b086f480c9795fb61f7a0).
// A rule ships only where the share of those writes it refuses is 5% or below.

// EnabledRules is the structure rules the bundle enforces, by id: the rules the measurement kept,
// which together refused 0.0% of the drawn writes.
var EnabledRules = []string{"phrasal-verb", "nominalization", "dangling-conjunction"}

// RuleMeasurement is one rule the measurement turned off: its id, the share of the drawn writes
// it refused, and why it is off.
type RuleMeasurement struct {
	ID          string
	PooledShare float64
	Reason      string
}

// MeasuredOff is every rule package rules carries that the measurement turned off.
var MeasuredOff = []RuleMeasurement{
	{ID: "semicolon", PooledShare: 0.622, Reason: "refuses 62.2% of a drawn sample of 600 agent writes; the ceiling for one rule is 5%"},
	{ID: "long-sentence", PooledShare: 0.791, Reason: "refuses 79.1% of a drawn sample of 600 agent writes; the ceiling for one rule is 5%"},
}

// DetectorRule is the rule id of the detector's findings.
const DetectorRule = "vacuous-verb-phrase"

// DetectorEnabled is whether the bundle runs the detector. The detector ships on only once its
// final test bounds 1 - precision at 5% or below; until then it is off in the code, and
// DISPATCH_WRITING_CHECK_DETECTOR=on does not turn it on.
const DetectorEnabled = false

// ReaderNoEffort is whether the reader sends no reasoning_effort at all: a Foundry chat
// deployment refuses the value `none` (measured on grok-4.7), so the reader sends none.
const ReaderNoEffort = true

// CandidateCap is the most candidates one reader call reads.
const CandidateCap = reader.CandidateCap

// BundleModelID is the model id the bundle was measured with, set when the bundle is frozen. A
// reader built with it treats a reply from any other model as a mismatch; while it is empty no
// model is pinned.
const BundleModelID = ""
