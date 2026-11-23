package writingcheck

import "github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"

// The bundle: what the check enforces, frozen with the measurement that chose it. Its constants
// decide the structure rules and the detector the server runs, and no setting changes them.
//
// The measurement: the rule-cost run of `writing-check rule-cost` over 600 drawn agent writes
// (seed 3231, manifest sha256 a8a58b9cb8bbc5cd0228c83ead18cba27dd16035757b086f480c9795fb61f7a0).
// It kept a rule only where the share of those writes it refused was 5% or below: a ceiling for
// the refusals the check made then. The check refuses no write now: it records the findings.
//
// key-pointer came after that measurement, which did not measure it: INFRA-3231's architect ruled
// for it in place of the reviewer's model rule for keys used as pointers. It finds 356 of the
// 2169 development texts of the harness dataset (16.4%), above that 5%. The check records its
// findings and refuses no write for them. The reviewer reads them only where
// DISPATCH_REVIEWER_RULES names `structure`, which it does not by default.

// EnabledRules is the structure rules the bundle enforces, by id: the rules the measurement kept,
// which together refused 0.0% of the drawn writes, and key-pointer, which it did not measure.
var EnabledRules = []string{"phrasal-verb", "nominalization", "dangling-conjunction", "key-pointer"}

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

// ReaderNoEffort is whether the reader sends no reasoning_effort at all: a Foundry chat
// deployment refuses the value `none` (measured on grok-4.7), so the reader sends none.
const ReaderNoEffort = true

// CandidateCap is the most candidates one reader call reads.
const CandidateCap = reader.CandidateCap

// BundleModelID is the model id the bundle was measured with, set when the bundle is frozen. A
// reader built with it treats a reply from any other model as a mismatch; while it is empty no
// model is pinned.
const BundleModelID = ""
