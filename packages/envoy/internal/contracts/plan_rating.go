package contracts

import (
	"strings"
	"unicode/utf8"
)

// PlanRatingFault is the first rule of a rating of the plan that a rating and its reason break,
// or PlanRatingHolds where they break none.
type PlanRatingFault int

const (
	// PlanRatingHolds is a rating and a reason that break no rule.
	PlanRatingHolds PlanRatingFault = iota
	// PlanRatingOffScale is a rating outside PlanRatingLeast to PlanRatingMost.
	PlanRatingOffScale
	// PlanReasonBlank is a reason that is empty or blank, as Go's strings.TrimSpace reads blank.
	PlanReasonBlank
	// PlanReasonLong is a reason of more than PlanReasonMax code points.
	PlanReasonLong
)

// CheckPlanRating is the first rule of a rating of the plan that rating and reason break, in that
// order, or PlanRatingHolds. It is the one statement of the rule: Dispatch's step store refuses a
// rating write that breaks it, and the Legion daemon's completion route, through
// github.com/sjawhar/envoy/steprecord, refuses a retro's completion that does. Each caller refuses
// a fault with its own wording, naming its own fields.
func CheckPlanRating(rating int, reason string) PlanRatingFault {
	switch {
	case rating < PlanRatingLeast || rating > PlanRatingMost:
		return PlanRatingOffScale
	case strings.TrimSpace(reason) == "":
		return PlanReasonBlank
	case utf8.RuneCountInString(reason) > PlanReasonMax:
		return PlanReasonLong
	}
	return PlanRatingHolds
}
