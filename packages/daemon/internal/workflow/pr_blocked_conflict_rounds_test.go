package workflow

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main's cap message, which recordRound posts on the issue and puts in the pr-blocked notice's
// reason (workflow/review.go, recordRound, at d46ec4e0), names both counts on every cap it tells,
// none excused included: "Issue reached review_round_cap=%d: %d rounds counted, %d conflict rounds
// not counted." dispatch://INFRA-1442's R5 (approved at version 15, done, and main holds it) has
// it so: "The message and the `pr-blocked` notice the cap writes name how many rounds were counted
// and how many conflict rounds were not." The Design's "Approved requirements this changes" does
// not name INFRA-1442, and dispatch://INFRA-3686's R8 says that "a fact or a step that it has the
// text give stays as a field". So the notice of a cap reached with no conflict round names its
// conflict rounds, 0, as a field.
func TestAPRBlockedNoticeNamesItsConflictRoundsWhenNoneWereExcused(t *testing.T) {
	rec := PRBlockedNotice("LEGION-41", 3, 3, 0)
	notice, err := record.NoticeOf(rec)
	if err != nil {
		t.Fatalf("the pr-blocked record as a notice: %v", err)
	}
	encoded, err := json.Marshal(notice)
	if err != nil {
		t.Fatalf("encode the notice: %v", err)
	}
	var published map[string]any
	if err := json.Unmarshal(encoded, &published); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
	if _, ok := published["conflict_rounds"]; !ok {
		t.Errorf("the pr-blocked notice of a cap reached with no conflict round is %s: it names how many rounds were counted "+
			"and not how many conflict rounds were not (INFRA-1442 R5)", encoded)
	}
}
