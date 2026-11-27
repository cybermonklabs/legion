package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The boot's lines this test orders: the daemon's line of the repair of the open production
// checks (repairOpenChecks), and the engine's line of the close of the check of a run that left
// with no merge (workflow's closeCheckOfARunThatLeft).
const (
	bootRepairLine  = "gave open production check looks a look id and a new task, and open checks the generation they opened at"
	runLeftLineText = "workflow: production check of a run that left with no merge closes as not run"
)

// listingBoard is a Dispatch whose project listing answers listing, with every other call
// unavailable, as a Dispatch whose dependencies are down answers (checkWakeDispatch): what the
// boot's other calls of Dispatch get is not what this test is about.
func listingBoard(t *testing.T, listing []map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues" {
			if err := json.NewEncoder(w).Encode(listing); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// listedIssue is one summary of a Dispatch listing: key at status and seq under parent ("" for
// none) on acme/widgets, carrying the legion label where labelled says so.
func listedIssue(key, status, parent string, seq int64, labelled bool) map[string]any {
	labels := []string{}
	if labelled {
		labels = []string{dispatch.LegionLabel}
	}
	summary := map[string]any{"key": key, "title": key, "status": status, "rank": "a0", "labels": labels, "last_seq": seq,
		"resolved_repo": "acme/widgets", "blocked_by": []string{}}
	if parent != "" {
		summary["parent"] = parent
	}
	return summary
}

// TestTheBootGivesTheChecksTheirGenerationsBeforeTheListingsLeaves holds that the boot gives each
// open production check an older binary opened, which carries no generation, the one its issue's
// record gives it (repairOpenChecks) before the listing's reconcile applies a leave from it
// (dispatch://INFRA-3518). Two issues moved while the daemon was down, and the boot's listing,
// with the Dispatch consumer caught up, parks both in the backlog:
//
//   - a done child whose check an older binary opened on its merge. The boot gives the check the
//     child's generation, so the park is no run that left with no merge, and the check stays open.
//   - a root that older binary re-admitted after its check's merge, in the workflow at a later
//     generation with that check still open. The boot gives the check the root's generation less
//     one, so the park is the leave of a run with no merge, and the check closes as not run. Were
//     the generation read after the reconcile, it would be the parked root's own, and the check
//     would stay open.
//
// The repair's line comes before the close's.
func TestTheBootGivesTheChecksTheirGenerationsBeforeTheListingsLeaves(t *testing.T) {
	t.Parallel()
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	project := cfg.Project
	root, child, readmitted := project+"-1", project+"-2", project+"-3"
	cfg.DispatchURL = listingBoard(t, []map[string]any{
		listedIssue(root, "done", "", 3, true),
		listedIssue(child, "backlog", root, 3, false),
		listedIssue(readmitted, "backlog", "", 3, true),
	})
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatal(err)
	}
	records := record.NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	opened, lingers, wake := now.Add(-72*time.Hour), now.Add(24*time.Hour), now.Add(24*time.Hour)
	issue := func(key, tree string, parent *string, at phase.Phase, generation uint64, status, rank string) record.Issue {
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: project, Tree: tree, Parent: parent,
			Title: key, Phase: at, Generation: generation, Status: status, DispatchStatus: status, HandedOver: parent == nil, Rank: rank, LastDispatchSeq: 2}
	}
	// The open check of an older binary: no generation, no look id, its look 1 ended pending and its
	// next look named a day out.
	olderCheck := func(key, tree string, number int) record.ProductionCheck {
		return record.ProductionCheck{Issue: key, Project: project, Tree: tree, Repo: "acme/widgets", Number: number, MergeSHA: "merge-" + key,
			Role: claim.RoleTester, OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 1, LookWakeAt: &opened, WakeAt: &wake,
			LastWakeKind: "merge", LastWakeDetail: "merge-" + key, LastWakeAt: &opened}
	}
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		closed := issue(root, root, nil, phase.Done, 1, "done", "U")
		closed.LingerUntil = &lingers
		parent := root
		for _, write := range []record.Issue{closed, issue(child, root, &parent, phase.Done, 3, "done", "V"), issue(readmitted, readmitted, nil, phase.Admitted, 8, "todo", "W")} {
			if err := records.PutIssue(ctx, tx, write); err != nil {
				return err
			}
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: readmitted, Index: 0, AdmittedAt: now}); err != nil {
			return err
		}
		for _, check := range []record.ProductionCheck{olderCheck(child, root, 42), olderCheck(readmitted, readmitted, 43)} {
			if err := records.PutProductionCheck(ctx, tx, check); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the issues and the checks as an older binary left them: %v", err)
	}
	for _, tree := range []string{root, readmitted} {
		if _, err := st.OpenTreeLifecycle(ctx, token, tree, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", tree, err)
		}
	}
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	logs := &syncBuffer{}
	startDaemonLogging(t, cfg, o, jsonLogger(logs))

	check := func(key string) record.ProductionCheck { return readProductionCheck(t, st.Pool(), records, key) }
	testwait.Eventually(t, "the listing's parks of "+child+" and "+readmitted+" applied", func() bool {
		return issueStatus(t, st.Pool(), child) == "backlog" && !check(readmitted).Open()
	})
	if got := check(readmitted); got.Outcome != record.CheckNotRun || got.OpenedGeneration != 7 {
		t.Fatalf("the re-admitted root's check after the boot = %+v, want it given generation 7 and closed as not run", got)
	}
	if got := check(child); !got.Open() || got.OpenedGeneration != 3 {
		t.Fatalf("the done child's check after the boot = %+v, want it given generation 3 and open", got)
	}
	repaired, left := -1, -1
	for i, line := range strings.Split(logs.String(), "\n") {
		switch {
		case repaired < 0 && strings.Contains(line, `"msg":"`+bootRepairLine+`"`):
			repaired = i
			if !strings.Contains(line, `"generations":2`) || !strings.Contains(line, `"looks":0`) {
				t.Fatalf("the repair's line = %s, want two checks given a generation and no look an id", line)
			}
		case left < 0 && strings.Contains(line, `"msg":"`+runLeftLineText+`"`) && strings.Contains(line, `"issue":"`+readmitted+`"`):
			left = i
		}
	}
	if repaired < 0 || left < 0 || repaired > left {
		t.Fatalf("the repair's line is line %d and the close's line %d, want the repair first\nlog:\n%s", repaired, left, logs.String())
	}
}

// issueStatus is the status the record holds for key.
func issueStatus(t *testing.T, pool *store.Pool, key string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), "select status from issues where key = $1", key).Scan(&status); err != nil {
		t.Fatalf("read the status of %s: %v", key, err)
	}
	return status
}
