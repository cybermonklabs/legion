package api

import (
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// PlanRatingContract is the daemon contract that added the rating of the plan to the completion
// that ends the retro (version.go's 168): planRating, a whole number from 1 (the plan cost the
// work) to 5 (the plan fit the work), and planReason, why, the two together. The agent that does
// the retro gives it. The route holds the two to the rule of a rating (planRatingOf), and the
// workflow judges which completion ends the retro where it ends it, on the issue as the fact's
// own transaction reads it (workflow's planRatingRefusal): that completion takes the two, every
// other refuses them (PLAN_RATING_UNEXPECTED), and so does a production check's report (the route's
// checkReport). A claim whose plugin declared this contract or a later one must rate
// (intake.HandoffComplete.MustRate, PLAN_RATING_REQUIRED); a plugin below it never sends the two,
// and its retro, where the daemon serves its claim (staleClaim), completes with no rating.
const PlanRatingContract = 168

// planRatingOf holds a completion's rating of the plan to the rule of a rating and returns it,
// nil for a completion that names neither member, or the refusal PLAN_RATING_INPUT: one of the two
// without the other, or a rating and a reason that break the rule Dispatch's store holds the
// rating to as well (steprecord.CheckRating), a rating outside steprecord.RatingLeast to
// RatingMost, or a reason that is blank or holds more than RatingReasonMax characters, counted as
// code points. A member written with a value is named, an empty reason included. It reads nothing
// of the issue, since which completion carries a rating is judged where the retro ends
// (PlanRatingContract). The rating is built only where each rule has held, so no caller reads
// either member of the request itself. The refusal is a 400 its caller writes before anything is
// recorded, so the same completion sent again, corrected, is taken.
func planRatingOf(req HandoffCompleteRequest) (*record.PlanRating, *intake.Refusal) {
	if req.PlanRating == nil && req.PlanReason == nil {
		return nil, nil
	}
	// The pair, and then the rule of a rating. Each member is read only past the case that finds
	// it named.
	var wrong string
	switch {
	case req.PlanRating == nil:
		wrong = "planReason comes with no planRating"
	case req.PlanReason == nil:
		wrong = fmt.Sprintf("planRating %d comes with no planReason", *req.PlanRating)
	default:
		switch steprecord.CheckRating(*req.PlanRating, *req.PlanReason) {
		case steprecord.RatingHolds:
			return &record.PlanRating{Rating: *req.PlanRating, Reason: *req.PlanReason}, nil
		case steprecord.RatingOffScale:
			wrong = fmt.Sprintf("planRating %d is outside %d to %d", *req.PlanRating, steprecord.RatingLeast, steprecord.RatingMost)
		case steprecord.RatingReasonBlank:
			wrong = "planReason is blank"
		case steprecord.RatingReasonLong:
			wrong = fmt.Sprintf("planReason is %d characters, and it holds at most %d", utf8.RuneCountInString(*req.PlanReason), steprecord.RatingReasonMax)
		}
	}
	return nil, &intake.Refusal{Status: http.StatusBadRequest, Code: "PLAN_RATING_INPUT", Message: fmt.Sprintf(
		"%s: a rating of the plan is a whole number from %d to %d in planRating with why in planReason, one to %d characters, the two together; nothing was recorded", wrong, steprecord.RatingLeast, steprecord.RatingMost, steprecord.RatingReasonMax)}
}
