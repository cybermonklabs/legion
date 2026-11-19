// Package steprecord holds the words and the form of a Dispatch step record, for a caller outside
// this module: the kinds of a run, the results of a run that ended, the codes of the step write's
// refusals and the plan write's refusals that no retry changes, the form of the agent, the session
// and the model a record names (printable ASCII with no space, from 1 to 128 characters), and the
// scale and the rule of the rating a retro gives the plan, which Dispatch stores on the retro's
// record. Each is the one generated from packages/contracts (internal/contracts), which Dispatch's
// store and routes use, so a word the Legion daemon writes or reads is the word Dispatch holds, and
// the daemon never sends a record Dispatch would refuse whole for a word or a text.
//
// It exists because the Legion daemon (packages/daemon, module github.com/sjawhar/legion/daemon)
// is a Go module of its own, and Go's internal rule bars it from
// github.com/sjawhar/envoy/internal/contracts, where these are generated. This package is the one
// public door onto them, so the daemon reads the generated values rather than copies of its own.
//
// No package of the envoy module imports this one: an envoy package that needs one of these reads
// internal/contracts directly, as Dispatch's store does.
package steprecord

import (
	"regexp"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The kinds of a run: a step a plan names, at its place from 1, and the retro and the merge, which
// no plan names, at the place 0.
const (
	KindStep  = contracts.StepKindStep
	KindRetro = contracts.StepKindRetro
	KindMerge = contracts.StepKindMerge
)

// The results of a run that ended.
const (
	ResultPassed  = contracts.StepResultPassed
	ResultFailed  = contracts.StepResultFailed
	ResultStopped = contracts.StepResultStopped
)

// The codes of the step write's refusals that no retry changes: a value outside its form, a writer
// that is not the session of the issue's project's daemon, and an id whose record is a run of
// another issue.
const (
	CodeInput      = contracts.StepInputCode
	CodeWriter     = contracts.StepWriterCode
	CodeOtherIssue = contracts.StepOtherIssueCode
)

// The codes of the plan write's refusals that no retry changes: a value outside its form, and an id
// whose plan is the plan of another issue. A writer that is not the daemon's session is refused
// CodeWriter, as a step write is.
const (
	CodePlanInput      = contracts.PlanInputCode
	CodePlanOtherIssue = contracts.PlanOtherIssueCode
)

var agentText = regexp.MustCompile(contracts.StepAgentTextPattern)

// AgentTextHolds reports whether text is in the form a step record's agent, session and model
// take.
func AgentTextHolds(text string) bool {
	return agentText.MatchString(text)
}

// PlaceHolds reports whether place is a place a run of kind can stand at: from 1 for a step, 0 for
// a retro or a merge (contracts.StepPlaceHolds, the rule Dispatch's store refuses a write by).
func PlaceHolds(kind string, place int) bool {
	return contracts.StepPlaceHolds(kind, place)
}

// The scale of the rating a retro gives the plan its issue ran, RatingLeast the plan cost the work
// and RatingMost the plan fit the work, and RatingReasonMax the most characters, counted as Unicode
// code points, the rating's reason holds.
const (
	RatingLeast     = contracts.PlanRatingLeast
	RatingMost      = contracts.PlanRatingMost
	RatingReasonMax = contracts.PlanReasonMax
)

// RatingFault is the first rule of a rating of the plan that a rating and its reason break, or
// RatingHolds where they break none (contracts.PlanRatingFault).
type RatingFault = contracts.PlanRatingFault

// The faults of a rating: none, a rating outside RatingLeast to RatingMost, a reason that is empty
// or blank as Go's strings.TrimSpace reads blank, and a reason of more than RatingReasonMax code
// points.
const (
	RatingHolds       = contracts.PlanRatingHolds
	RatingOffScale    = contracts.PlanRatingOffScale
	RatingReasonBlank = contracts.PlanReasonBlank
	RatingReasonLong  = contracts.PlanReasonLong
)

// CheckRating is the first rule of a rating of the plan that rating and reason break, in that
// order, or RatingHolds (contracts.CheckPlanRating, the rule Dispatch's store refuses a rating
// write by). Each caller refuses a fault with its own wording, naming its own fields.
func CheckRating(rating int, reason string) RatingFault {
	return contracts.CheckPlanRating(rating, reason)
}
