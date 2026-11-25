package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main's wokenBy (workflow/check.go at d46ec4e0) told the look a pending report's wake_at started
// that it was "woken by the wake named at the last look — <summary>", the summary of that report.
// INFRA-3686's R1: "A daemon text … that carries words another author wrote (a person, another
// session, a worker's summary) puts them in a block of their own after its facts, which names the
// author and the time where the text's own fields do not already name them". The branch carries
// that summary in a block that names its author and no time, and no field of the task gives the
// time of that report.
// The check's ReportedAt is when the daemon recorded that report: the block's time must be it.
func TestTheNamedLooksSummaryBlockNamesTheTimeOfTheReport(t *testing.T) {
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
		reported := checkOpened.Add(26*time.Hour + 17*time.Minute)
		check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
			OpenedAt: checkOpened, BoundAt: checkOpened.Add(14 * 24 * time.Hour), Looks: 2, Role: role, ReportedAt: &reported}
		task := checkTask(issue, check, wakeNamed, "production runs yesterday's build; the release train runs at 09:00", false)
		opener := ""
		for _, line := range strings.Split(task, "\n") {
			if strings.HasPrefix(line, "<summary") {
				opener = line
			}
		}
		if !strings.Contains(opener, " at=\"") {
			t.Errorf("the %s's look woken by its own wake carries the report's summary in %q, which names no time, and no field of the task gives it:\n%s", role, opener, task)
		} else if want := " at=\"" + reported.UTC().Format(time.RFC3339Nano) + "\""; !strings.Contains(opener, want) {
			t.Errorf("the %s's look woken by its own wake carries the report's summary in %q, want the block to name%s, the check's ReportedAt:\n%s", role, opener, want, task)
		}
	}
}
