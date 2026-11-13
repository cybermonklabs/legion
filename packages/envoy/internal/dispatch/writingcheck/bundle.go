package writingcheck

import "github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"

// The bundle: what the check enforces, frozen with the measurement that chose it.

// EnabledRules is the structure rules the bundle enforces, by id. For now it is every rule
// package rules carries; the measurement narrows it.
var EnabledRules = []string{"semicolon", "phrasal-verb", "nominalization", "long-sentence", "dangling-conjunction"}

// DetectorRule is the rule id of the detector's findings.
const DetectorRule = "vacuous-verb-phrase"

// CandidateCap is the most candidates one reader call reads.
const CandidateCap = reader.CandidateCap

// BundleModelID is the model id the bundle was measured with, set when the bundle is frozen. A
// reader built with it treats a reply from any other model as a mismatch; while it is empty no
// model is pinned.
const BundleModelID = ""
