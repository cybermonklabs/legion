package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A design-changes-requested notice is of the request its document's gate records as told where
// it names the told version and the told reason as the notice carries it: cut to texts.OneAct,
// with the count of the units the cut left off in its words object (ChangesRequestedNotice), or
// whole, as a row an older daemon queued carries it with no words object. A whole reason is only
// ever the whole told reason, never its head, and a cut is the told reason's own only where it
// left off what the bound leaves off the told reason. Any other reason or version is of a request
// the person has since replaced, and a gate that records no request, or no gate of the notice's
// document, says nothing against the notice.
func TestChangesSupersededReadsTheNoticesReasonAsItsCutOfTheToldReason(t *testing.T) {
	const issue = "LEGION-208"
	reason := strings.Repeat("r", texts.OneAct+1)
	cut, err := record.NoticeOf(ChangesRequestedNotice(issue, record.DesignStepSpec, 3, reason, "user:alice", time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("build the changes-requested notice: %v", err)
	}
	if cut.Reason != reason[:texts.OneAct] {
		t.Fatalf("the notice carries a reason of %d units, want the first %d of the told reason's %d", len(cut.Reason), texts.OneAct, len(reason))
	}
	told := func(version int, reason string) map[record.DesignStep]record.DesignGate {
		return map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: issue, Step: record.DesignStepSpec, ToldChangesVersion: &version, ToldChangesReason: reason}}
	}
	changes := func(version int, reason string) record.Notice {
		return record.Notice{Kind: texts.DesignChangesRequested, Issue: issue, Version: version, Reason: reason, Ref: stepRef(issue, record.DesignStepSpec)}
	}
	longer, err := record.NoticeOf(ChangesRequestedNotice(issue, record.DesignStepSpec, 3, reason+"s", "user:alice", time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("build the changes-requested notice of the longer reason: %v", err)
	}
	if longer.Reason != cut.Reason || longer.Words["reason"].Rest != 2 {
		t.Fatalf("premise: the longer reason's notice carries %d units with rest %d, want the told reason's cut head and rest 2", len(longer.Reason), longer.Words["reason"].Rest)
	}
	for _, tc := range []struct {
		name     string
		gates    map[record.DesignStep]record.DesignGate
		notice   record.Notice
		replaced bool
	}{
		{"a reason over the bound, as the notice cuts it", told(3, reason), cut, false},
		{"a reason over the bound, whole, as an older daemon queued it", told(3, reason), changes(3, reason), false},
		{"a reason inside the bound", told(3, "name the rollback path"), changes(3, "name the rollback path"), false},
		{"another reason at the told version", told(3, "name the rollback path"), changes(3, "cut the migration"), true},
		{"a cut of another reason over the bound", told(3, reason), changes(3, strings.Repeat("s", texts.OneAct)), true},
		{"a shorter head of the told reason", told(3, reason), changes(3, reason[:texts.OneAct-1]), true},
		{"the told reason's head, whole, as an older daemon queued an earlier request whose whole reason it was", told(3, reason), changes(3, reason[:texts.OneAct]), true},
		{"a cut of a longer reason that shares the told reason's head", told(3, reason), longer, true},
		{"an older version with the told reason cut", told(3, reason), changes(2, reason[:texts.OneAct]), true},
		{"a gate that records no request", map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: issue, Step: record.DesignStepSpec}},
			changes(2, "cut the migration"), false},
		{"no gate of the notice's document", map[record.DesignStep]record.DesignGate{}, changes(2, "cut the migration"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChangesSuperseded(tc.gates, issue, tc.notice); got != tc.replaced {
				t.Errorf("ChangesSuperseded = %t, want %t", got, tc.replaced)
			}
		})
	}
}
