package legionfacts

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
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
	run.writeEventAs(t, parent, legiontest.SessionActor("planner", parent), "comment.created", `{}`, at.Add(time.Minute))
	filed := run.fileIssue(t, project, 902, parent, legiontest.SessionActor("implementer", filer), at.Add(2*time.Minute))

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

// statementTracer records the SQL and the arguments of every statement a traced pool runs
// (storetest.Traced), so a test can plan each statement again with the values it ran with.
type statementTracer struct {
	mu         sync.Mutex
	statements []tracedStatement
}

// tracedStatement is one statement a traced pool ran, and its arguments.
type tracedStatement struct {
	sql  string
	args []any
}

func (r *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.statements = append(r.statements, tracedStatement{sql: data.SQL, args: slices.Clone(data.Args)})
	r.mu.Unlock()
	return ctx
}

func (r *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestANarrowPassReadsTheEventLogOnlyByIssueOrByItsPage: a pass after an event reads the event
// log only through what bounds it, never whole. The computation reads it by the issues it computes,
// the phase work among them: the whole log's read of phase work (legion.PhaseWorkKeys) cost every
// pass about 730 ms a call at ten times INFRA's rate, and a narrow pass reads it for the issues its
// computation can place alone (legion.PhaseWorkOf). The page reads the events after its cursor by
// id, the events of the sessions it names, and each such session's title before the page.
//
// The pass's statements are recorded on a traced pool with their arguments, and each that reads
// the event log is planned again with those values and sequential scans off
// (explaintest.AnalyzedPlan): a statement no index can bound still plans a scan of the table,
// which fails. A statement the planner reads through an index under no index condition is logged
// and not failed: at the rig's few hundred events the planner reads metrics' read of a page's
// filings (`issue_key = any(…)` and a session actor) through the whole of
// events_session_actor_created_at_idx, the partial index its actor matches, which is no plan of
// a log of INFRA's size.
func TestANarrowPassReadsTheEventLogOnlyByIssueOrByItsPage(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	open := run.fixture.Open[0]
	run.writeEvent(t, open, "implementer", "issue.updated", `{"status":"in_progress"}`, run.at.Add(-time.Minute))

	recorder := &statementTracer{}
	run.deps.Pool = storetest.Traced(t, run.database.Pool, recorder, nil)
	changed := run.pass(t)
	if !slices.Contains(changed, open) {
		t.Fatalf("the pass after an event on %s changed %v, which does not hold it, so it computed nothing this test can judge", open, changed)
	}

	recorder.mu.Lock()
	statements := slices.Clone(recorder.statements)
	recorder.mu.Unlock()
	read, byIndex, unkeyed := 0, map[string]int{}, []string{}
	for _, statement := range statements {
		if !eventLogStatement.MatchString(statement.sql) {
			continue
		}
		read++
		plan, text := explaintest.AnalyzedPlan(t, t.Context(), run.database.Pool, statement.sql, statement.args...)
		for _, node := range explaintest.Nodes(t, plan) {
			onEvents := node.Relation == "events" || (node.Relation == "" && strings.HasPrefix(node.Index, "events"))
			switch {
			case !onEvents:
			case node.Type == "Seq Scan":
				t.Errorf("the narrow pass reads the event log whole:\n%s\n%s", strings.TrimSpace(statement.sql), text)
			case node.Index != "" && node.IndexCond == "":
				unkeyed = append(unkeyed, node.Index+": "+strings.Join(strings.Fields(statement.sql), " "))
			case node.Index != "":
				byIndex[node.Index+" ("+node.IndexCond+")"]++
			}
		}
	}
	if read == 0 || !slices.ContainsFunc(slices.Sorted(maps.Keys(byIndex)), func(read string) bool { return strings.Contains(read, "issue_key") }) {
		t.Fatalf("the narrow pass read the event log %d times and never through an index on its issues (%v), so it computed nothing this test can judge", read, byIndex)
	}
	t.Logf("the pass after an event on %s read the event log %d times, through %v; read through an index under no condition at the rig's size: %v", open, read, byIndex, unkeyed)
}
