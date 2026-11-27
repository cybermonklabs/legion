// Package cicase holds the cases of a check a CI job runs, as Dispatch publishes them in a
// check.state_changed event's ci_case, for a caller outside this module: the cases where the job
// ended at the head (a conclusion GitHub wrote, or job_recorded), and the cases where it has not
// ended there, or Dispatch could not read its end. Each is the one generated from
// packages/contracts (internal/contracts), which Dispatch writes the case from and the event's
// schema closes ci_case to, so the Legion daemon reads the same list Dispatch writes.
//
// It exists because the Legion daemon (packages/daemon, module github.com/sjawhar/legion/daemon)
// is a Go module of its own, and Go's internal rule bars it from
// github.com/sjawhar/envoy/internal/contracts, where the lists are generated. No package of the
// envoy module imports this one: an envoy package that needs a case reads internal/contracts
// directly, as Dispatch's results read does.
package cicase

import (
	"slices"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Ended says whether ciCase is a case of a job that ended at the head: a conclusion GitHub wrote
// (success, the three that prove nothing, and the fails), or job_recorded.
func Ended(ciCase string) bool {
	return slices.Contains(contracts.CheckCICasesEnded, ciCase)
}

// NotEnded says whether ciCase is a case of a job that has not ended at the head, or whose end
// Dispatch could not read: its run has not concluded, no run of its name is on the head, Dispatch
// holds no head to read at, GitHub did not answer, a result of the job named no repository Dispatch
// could resolve, or the read had not started when its batch's bound ended.
func NotEnded(ciCase string) bool {
	return slices.Contains(contracts.CheckCICasesNotEnded, ciCase)
}

// Known says whether ciCase is a case Dispatch publishes: "" for a check no CI job runs, or a case
// of either list. Any other is a case of no Dispatch this module knows.
func Known(ciCase string) bool {
	return ciCase == "" || Ended(ciCase) || NotEnded(ciCase)
}
