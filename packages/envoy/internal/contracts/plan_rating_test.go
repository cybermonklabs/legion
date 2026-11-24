package contracts

//ci:reads packages/contracts/fixtures/daemon-api/plan-rating-cases.json TestCheckPlanRatingTakesExactlyTheRatingsTheDaemonTakes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CheckPlanRating is the one rule a rating of the plan is held to, by the daemon's completion
// route and by Dispatch's store, so it takes exactly the ratings the daemon took in
// plan-rating-cases.json, the verdicts the daemon's own route wrote (internal/api's
// TestPlanRatingCasesGolden): each case that carries both a rating and a reason, the daemon's
// verdict `accepted` where CheckPlanRating finds no fault and PLAN_RATING_INPUT where it finds one.
// A case that leaves either out is the daemon's own rule of the pair, which no write to Dispatch
// can break: its rating carries both. The cases hold an accepted rating and a refused one, and a
// reason blank to Go's strings.TrimSpace alone, so a check that took every rating, or none, or
// read blank another way, fails here.
func TestCheckPlanRatingTakesExactlyTheRatingsTheDaemonTakes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", "plan-rating-cases.json"))
	if err != nil {
		t.Fatalf("read the daemon's verdicts on the ratings: %v", err)
	}
	var golden struct {
		Cases []struct {
			Name       string  `json:"name"`
			PlanRating *int    `json:"planRating"`
			PlanReason *string `json:"planReason"`
			Verdict    string  `json:"verdict"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode the daemon's verdicts on the ratings: %v", err)
	}
	read := map[string]int{}
	for _, c := range golden.Cases {
		if c.PlanRating == nil || c.PlanReason == nil {
			continue
		}
		read[c.Verdict]++
		fault := CheckPlanRating(*c.PlanRating, *c.PlanReason)
		switch c.Verdict {
		case "accepted":
			if fault != PlanRatingHolds {
				t.Errorf("%s: CheckPlanRating = %d, want PlanRatingHolds: the daemon took it", c.Name, fault)
			}
		case "PLAN_RATING_INPUT":
			if fault == PlanRatingHolds {
				t.Errorf("%s: CheckPlanRating = PlanRatingHolds, want a fault: the daemon refused it PLAN_RATING_INPUT", c.Name)
			}
		default:
			t.Errorf("%s: the daemon's verdict %q is neither accepted nor PLAN_RATING_INPUT, for a rating that carries both members", c.Name, c.Verdict)
		}
	}
	if read["accepted"] == 0 || read["PLAN_RATING_INPUT"] == 0 {
		t.Fatalf("the cases that carry both members hold %v, want both an accepted one and a refused one, or this test cannot fail", read)
	}
}

// CheckPlanRating answers the first rule a rating breaks, in the order its callers' refusals name
// them: the scale, then a blank reason, then a reason past PlanReasonMax code points, a character
// outside the Basic Multilingual Plane counting once.
func TestCheckPlanRatingAnswersTheFirstRuleARatingBreaks(t *testing.T) {
	for _, tc := range []struct {
		rating int
		reason string
		want   PlanRatingFault
	}{
		{PlanRatingLeast, "fit", PlanRatingHolds},
		{PlanRatingMost, strings.Repeat("\U0001F600", PlanReasonMax), PlanRatingHolds},
		{PlanRatingLeast - 1, "fit", PlanRatingOffScale},
		{PlanRatingMost + 1, "", PlanRatingOffScale},
		{PlanRatingLeast, "", PlanReasonBlank},
		{PlanRatingLeast, " \t\u0085", PlanReasonBlank},
		{PlanRatingLeast, strings.Repeat("\U0001F600", PlanReasonMax+1), PlanReasonLong},
	} {
		if got := CheckPlanRating(tc.rating, tc.reason); got != tc.want {
			t.Errorf("CheckPlanRating(%d, a reason of %d bytes) = %d, want %d", tc.rating, len(tc.reason), got, tc.want)
		}
	}
}
