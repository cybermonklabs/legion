package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// TestAConvertedRisksCheckOfAnotherIssueIsRetargetedAndRetractedThroughThePointerRoute converts the
// captured pre-mortem with risk 8 prevented by a check of a second issue, as a root's map names the
// checks of its children (premortemMap; spec requirement r-premortem-nodes), and then
// corrects that tuple through the tuple route, which R9 names as the one way a prevented_by moves
// (spec requirement r-constraint-risk). A retarget from the second issue's check to a
// check of the risk's own issue answers 200 and leaves risk 8 prevented by that check alone; a
// retract of the second issue's check answers 200 and leaves risk 8 prevented by nothing. A
// prevented_by the conversion writes and no route can move is a pointer nobody can correct.
func TestAConvertedRisksCheckOfAnotherIssueIsRetargetedAndRetractedThroughThePointerRoute(t *testing.T) {
	// converted is a rig whose pre-mortem is converted, and the store id of its risk 8, prevented
	// by the second issue's check alone.
	converted := func(t *testing.T) (premortemRig, string, string) {
		t.Helper()
		rig := newPremortemRig(t)
		report, err := rig.convert(rig.premortemMap(), false)
		if err != nil {
			t.Fatalf("convert the pre-mortem: %v", err)
		}
		if len(report.Risks) != 11 || report.Risks[8].Address == nil {
			t.Fatalf("the conversion answers %+v, want eleven risks written", report)
		}
		address := *report.Risks[8].Address
		id := strings.TrimPrefix(address, "dispatch://"+graphstore.KindRisk+"/")
		if got, want := rig.current(graphstore.KindRisk, id, graphstore.TuplePreventedBy), []string{"block/" + rig.otherIssueCheckNode}; !slices.Equal(got, want) {
			t.Fatalf("risk 8 is prevented by %v after the conversion, want %v", got, want)
		}
		return rig, address, id
	}

	t.Run("a retarget from the other issue's check", func(t *testing.T) {
		rig, address, id := converted(t)
		response := rig.post(nodePath(address, "pointers"), map[string]any{
			"kind": graphstore.TuplePreventedBy, "action": "retarget", "from": rig.otherIssueCheck, "to": rig.check,
		}, "alice")
		t.Logf("retarget %s from %s to %s: status=%d body=%s", address, rig.otherIssueCheck, rig.check, response.Code, response.Body.String())
		if response.Code != http.StatusOK {
			t.Errorf("the retarget of risk 8's prevented_by from the check %s, which the conversion wrote, answers %d %s, want 200",
				rig.otherIssueCheck, response.Code, response.Body.String())
		}
		if got, want := rig.current(graphstore.KindRisk, id, graphstore.TuplePreventedBy), []string{"block/" + rig.checkNode}; !slices.Equal(got, want) {
			t.Errorf("after the retarget risk 8 is prevented by %v, want %v", got, want)
		}
	})

	t.Run("a retract of the other issue's check", func(t *testing.T) {
		rig, address, id := converted(t)
		response := rig.post(nodePath(address, "pointers"), map[string]any{
			"kind": graphstore.TuplePreventedBy, "action": "retract", "to": rig.otherIssueCheck,
		}, "alice")
		t.Logf("retract %s to %s: status=%d body=%s", address, rig.otherIssueCheck, response.Code, response.Body.String())
		if response.Code != http.StatusOK {
			t.Errorf("the retract of risk 8's prevented_by to the check %s, which the conversion wrote, answers %d %s, want 200",
				rig.otherIssueCheck, response.Code, response.Body.String())
		}
		if got := rig.current(graphstore.KindRisk, id, graphstore.TuplePreventedBy); len(got) != 0 {
			t.Errorf("after the retract risk 8 is prevented by %v, want nothing", got)
		}
	})
}
