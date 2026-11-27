package workflowstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAStepOfAnotherIssueIsRefusedInWordsThatNameNoOtherIssue (F16): the refusal of a write whose
// step id is a run of another issue (ErrStepOtherIssue) names the step and the issue the write
// names, and never the issue the step is a run of: a daemon writer of one issue that names the
// step id of an issue of a project it does not see learns nothing of that issue. The route answers
// the text as it is (409 STEP_OTHER_ISSUE, api/steps.go).
//
// Control: PutStep's refusal naming other.issue, as HEAD c99245cbc6 does (controls/F16.patch
// reversed): the text names LEG-1, and the test fails at the row "names LEG-1".
func TestAStepOfAnotherIssueIsRefusedInWordsThatNameNoOtherIssue(t *testing.T) {
	pool := storetest.Open(t).Pool
	id := uuid.NewString()
	put(t, pool, startOf(implementer(id), minute(0)), minute(0))

	elsewhere := endOf(implementer(id), ResultPassed, minute(1))
	elsewhere.Issue = "LEG-2"
	_, err := tryPut(pool, elsewhere, minute(1))
	if !errors.Is(err, ErrStepOtherIssue) {
		t.Fatalf("a write of LEG-1's step naming LEG-2 answered %v, want %v", err, ErrStepOtherIssue)
	}
	text := err.Error()
	if strings.Contains(text, "LEG-1") {
		t.Errorf("the refusal %q names LEG-1, the issue the step is a run of, which the writer of LEG-2 may not see", text)
	}
	for _, named := range []string{id, "LEG-2"} {
		if !strings.Contains(text, named) {
			t.Errorf("the refusal %q does not name %s", text, named)
		}
	}
}
