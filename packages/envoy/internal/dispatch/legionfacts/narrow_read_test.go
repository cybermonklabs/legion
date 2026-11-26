package legionfacts

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAFilingOutsideTheScopeHoldsTheRootTheProjectComputedWhole: a pass that computes some issues
// reads Legion phase work only for the issues its computation can place (metrics.ReadIssues'
// placedKeys), not over the whole log. A filing is the one place it asks the tree rule about an
// issue outside its scope: a filer of the scope files an issue into another tree, and the filing
// stores that issue's root as it stands now (metrics.ProjectFilings). The root is placed by the
// placement walk (legion.TreeChain), which climbs to a parent only while the parent is a Legion
// issue, so a filing under a parent the daemon reports nothing of, whose only Legion standing is
// phase work, holds that parent as its root only if the pass read the parent's phase work.
//
// The history is the rig's with three rows added: a tracking issue no tree holds and the daemon
// reports nothing of, a planner's event on it, which is its one Legion standing, and an issue an
// implementer of the first tree filed under it. The filing is computed as a pass that read the
// implementer's session computes it, its filer's issue the key and the filed issue named, and
// compared field by field with the filing the project computed whole.
func TestAFilingOutsideTheScopeHoldsTheRootTheProjectComputedWhole(t *testing.T) {
	run := newConsumer(t, 3)
	project := run.fixture.Project
	pool := run.database.Pool
	filer := run.fixture.Keys[1]
	at := run.at.Add(-2 * time.Hour)

	parent := run.fileIssue(t, project, 901, "", `{"kind":"user","id":"alice"}`, at)
	run.writeEventAs(t, parent, roleSession("planner", parent), "comment.created", `{}`, at.Add(time.Minute))
	filed := run.fileIssue(t, project, 902, parent, roleSession("implementer", filer), at.Add(2*time.Minute))

	_, whole, err := ComputeProject(t.Context(), pool, project, run.at)
	if err != nil {
		t.Fatalf("compute the project whole: %v", err)
	}
	want, held := Filing{}, false
	for _, filing := range whole {
		if filing.Key == filed {
			want, held = filing, true
		}
	}
	if !held {
		t.Fatalf("the project computed whole holds no filing of %s, so this history states nothing", filed)
	}
	// The parent's phase work has to have placed the filing under it, or the history asks the
	// placement walk nothing a pass could answer otherwise.
	if want.Root != parent {
		t.Fatalf("the project computed whole roots %s at %q, want %s, the parent whose phase work makes it a Legion issue",
			filed, want.Root, parent)
	}

	_, alone, err := computeKeys(t.Context(), pool, project, []string{filer}, []string{filed}, run.at)
	if err != nil {
		t.Fatalf("compute %s with the filing of %s: %v", filer, filed, err)
	}
	got, held := Filing{}, false
	for _, filing := range alone {
		if filing.Key == filed {
			got, held = filing, true
		}
	}
	switch {
	case !held:
		t.Errorf("computing %s with the filing of %s answered the filings %v and none of %s", filer, filed, alone, filed)
	case !sameFiling(got, want):
		t.Errorf("the filing of %s computed with %s is %s, and computed with its project %s", filed, filer, showFiling(got), showFiling(want))
	}
	t.Logf("%s, filed by %s's implementer under %s, which the daemon reports nothing of and whose one Legion standing is a planner's event: computed with %s alone %s, and with its project %s",
		filed, filer, parent, filer, showFiling(got), showFiling(want))
}

// eventLogStatement matches a statement that reads the event log.
var eventLogStatement = regexp.MustCompile(`\bevents\b`)

// eventLogBound is one thing that bounds a statement of the event log a narrow pass may run, a
// part of the statement's own text.
type eventLogBound struct {
	what  string
	bound *regexp.Regexp
}

// eventLogBounds is every such bound: the issues the computation reads (metrics.ReadIssues'
// reads, and legion.PhaseWorkOf), and the two reads of the page itself (feeds.go), its event ids
// and the sessions its events name.
var eventLogBounds = []eventLogBound{
	{"the issues", regexp.MustCompile(`issue_key\s*=\s*any\(`)},
	{"the page's event ids", regexp.MustCompile(`event\.id\s*>\s*\$1\s+and\s+event\.id\s*<=\s*\$2`)},
	{"the page's sessions", regexp.MustCompile(`event\.actor\s*->>\s*'id'\s*=\s*any\(`)},
}

// TestANarrowPassReadsTheEventLogOnlyByIssueOrByItsPage: a pass after an event reads the event
// log only through what bounds it, never whole. The computation reads it by the issues it computes
// (`issue_key = any(`), the phase work among them: the whole log's read of phase work
// (legion.PhaseWorkKeys) cost every pass about 730 ms a call at ten times INFRA's rate, and a
// narrow pass reads it for the issues its computation can place alone (legion.PhaseWorkOf). The
// page reads the events after its cursor by id and the events of the sessions it names.
//
// The pass's statements are recorded on a traced pool, and each that reads the event log is held
// to one of those bounds and printed.
func TestANarrowPassReadsTheEventLogOnlyByIssueOrByItsPage(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	open := run.fixture.Open[0]
	run.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-time.Minute))

	recorder := &storetest.StatementRecorder{}
	run.deps.Pool = storetest.Traced(t, run.database.Pool, recorder, nil)
	changed := run.pass(t)
	if !slices.Contains(changed, open) {
		t.Fatalf("the pass after an event on %s changed %v, which does not hold it, so it computed nothing this test can judge", open, changed)
	}

	read, bounded := 0, map[string]int{}
	for _, statement := range recorder.Statements() {
		if !eventLogStatement.MatchString(statement) {
			continue
		}
		read++
		index := slices.IndexFunc(eventLogBounds, func(one eventLogBound) bool { return one.bound.MatchString(statement) })
		if index < 0 {
			t.Errorf("the narrow pass read the event log bounded by no issue and no page:\n%s", strings.TrimSpace(statement))
			continue
		}
		bounded[eventLogBounds[index].what]++
		t.Logf("bounded by %s:\n%s", eventLogBounds[index].what, strings.TrimSpace(statement))
	}
	if bounded["the issues"] == 0 {
		t.Fatalf("the narrow pass read the event log %d times and never by its issues, so it computed nothing this test can judge", read)
	}
	t.Logf("the pass after an event on %s read the event log %d times, by bound %v", open, read, bounded)
}
