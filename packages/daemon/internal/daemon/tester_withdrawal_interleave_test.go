package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// inFlightBoard is closeWaitBoard whose next upload, where during is set, waits for during to run
// to its end on a goroutine of its own before Dispatch takes the version: a Dispatch slow to
// answer, with a change of the record made by another caller while the row's upload is in flight.
type inFlightBoard struct {
	*closeWaitBoard
	during func()
}

func (d *inFlightBoard) UploadArtifactOver(ctx context.Context, key, name, content, token string) (string, error) {
	if during := d.during; during != nil {
		d.during = nil
		done := make(chan struct{})
		go func() {
			defer close(done)
			during()
		}()
		<-done
	}
	return d.closeWaitBoard.UploadArtifactOver(ctx, key, name, content, token)
}

// The tester's third proof of #5825's withdrawal fix (INFRA-5580 Task 9), through the real
// workflow and the real outbox: each report and withdrawal is a fact the engine applies
// (intake.ApplyFact), and each close_wait_publish row is claimed, leased, run and settled by the
// outbox's own runRow. A withdrawal applied while a row's upload is in flight, under that row's
// live lease, queues a row of its own, stays in the stored wait after the first row clears the
// withdrawal it showed, survives a refused upload, and reaches the next version of close-wait.md.
func TestTesterAWithdrawalAppliedWhileTheUploadRunsReachesTheNextVersion(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		issue record.Issue
	}{
		{name: "retro", issue: record.Issue{Phase: phase.Retro, Status: "retro"}},
		{name: "verifying", issue: record.Issue{Phase: phase.Verifying, Status: "testing", CloseWaitSince: &since}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			const key = "LEGION-208"
			const head = "4d2f6a0c8e1b3d5f7a9c0e2b4d6f8a1c3e5b7d90"
			const reasonA = "tester-reason-A: the same defect as C"
			const reasonB = "tester-reason-B: fixed in the retro"
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
			seeded := tc.issue
			seeded.Key, seeded.Repo, seeded.DefaultBranch, seeded.GitHubRepo = key, "acme/widgets", "main", "acme/widgets"
			seeded.Project, seeded.Tree, seeded.Title, seeded.Generation, seeded.Rank = "LEGION", key, "Workflow", 3, "U"
			putOutboxIssue(t, pool, records, seeded)
			project, err := claim.ProjectToken("LEGION")
			if err != nil {
				t.Fatal(err)
			}
			architect, err := claim.NewToken(project, key, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			// applyFact applies fact and says why it was not taken, so a goroutine other than the
			// test's can apply one and leave the failing to the test's own goroutine.
			applyFact := func(id string, fact intake.Fact) (intake.Result, error) {
				result, err := intake.ApplyFact(ctx, pool, "api", "tester:"+tc.name+":"+id, fact, engine)
				if err == nil && (result.Refusal != nil || result.Defect == nil) {
					err = fmt.Errorf("refused %+v, answered %+v", result.Refusal, result.Defect)
				}
				return result, err
			}
			apply := func(id string, fact intake.Fact) intake.Result {
				t.Helper()
				result, err := applyFact(id, fact)
				if err != nil {
					t.Fatalf("apply %s: %v; want it taken", id, err)
				}
				return result
			}
			report := func(id, text string) int64 {
				t.Helper()
				return apply("report:"+id, intake.DefectReport{Issue: key, Role: claim.RoleArchitect, By: architect, Text: text, Head: head}).Defect.Defect
			}
			withdrawal := func(defect int64, reason string) intake.Fact {
				return intake.DefectWithdrawal{Issue: key, Defect: defect, Reason: reason, By: architect}
			}
			withdraw := func(defect int64, reason string) {
				t.Helper()
				if got := apply("withdraw:"+reason, withdrawal(defect, reason)); got.Defect.State != intake.DefectWithdrawn {
					t.Fatalf("withdraw %d = %+v, want withdrawn", defect, got.Defect)
				}
			}
			board := &inFlightBoard{closeWaitBoard: &closeWaitBoard{outboxDispatch: &outboxDispatch{}}}
			runner := &outbox{log: quietLogger(), pool: pool, records: records, dispatch: board, dispatchProject: "LEGION", now: time.Now}
			drain := func() int {
				t.Helper()
				rows, err := runner.listDue(ctx, key, []record.OutboxKind{record.OutboxKindCloseWaitPublish}, time.Now().UTC())
				if err != nil {
					t.Fatalf("list the due close_wait_publish rows: %v", err)
				}
				for _, row := range rows {
					if err := runner.runRow(ctx, row); err != nil {
						t.Fatalf("run row %d: %v", row.ID, err)
					}
				}
				return len(rows)
			}
			// countPublishRows counts the issue's close_wait_publish rows, those under a live lease
			// alone where leasedOnly; it fails nothing, so any goroutine may call it.
			countPublishRows := func(leasedOnly bool) (int, error) {
				var count int
				err := pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'close_wait_publish'
					and (not $2 or coalesce(lease_token <> '' and lease_until > now(), false))`, key, leasedOnly).Scan(&count)
				return count, err
			}
			publishRows := func(leasedOnly bool) int {
				t.Helper()
				count, err := countPublishRows(leasedOnly)
				if err != nil {
					t.Fatalf("count the close_wait_publish rows: %v", err)
				}
				return count
			}
			withdrawnNow := func() []int64 {
				t.Helper()
				var ids []int64
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					issue, err := records.Issue(ctx, tx, key)
					if err != nil || issue == nil || len(issue.CloseWait) == 0 {
						return err
					}
					var stored record.StoredWait
					if err := json.Unmarshal(issue.CloseWait, &stored); err != nil {
						return err
					}
					for _, withdrawn := range stored.Withdrawn {
						ids = append(ids, withdrawn.ID)
					}
					return nil
				}); err != nil {
					t.Fatalf("read the stored wait: %v", err)
				}
				return ids
			}
			lastVersion := func() string {
				t.Helper()
				if len(board.versions) == 0 {
					t.Fatal("close-wait.md has no version")
				}
				return board.versions[len(board.versions)-1]
			}

			a := report("a", "Defect A: internal/retry/loop.go:42 loops after the budget ends.")
			b := report("b", "Defect B: internal/log/line.go:7 names no issue.")
			if n := drain(); n != 1 || len(board.versions) != 1 {
				t.Fatalf("the first drain ran %d rows and wrote %d versions, want 1 and 1", n, len(board.versions))
			}

			// A is withdrawn; the row that writes it is claimed, and while its upload is in flight,
			// under its live lease, B is withdrawn by another caller.
			withdraw(a, reasonA)
			leasedDuring, waitingDuring := -1, -1
			var duringErr error
			board.during = func() {
				if leasedDuring, duringErr = countPublishRows(true); duringErr != nil {
					return
				}
				if _, duringErr = applyFact("withdraw:"+reasonB, withdrawal(b, reasonB)); duringErr != nil {
					return
				}
				all, err := countPublishRows(false)
				if err != nil {
					duringErr = err
					return
				}
				leased, err := countPublishRows(true)
				waitingDuring, duringErr = all-leased, err
			}
			if n := drain(); n != 1 {
				t.Fatalf("the second drain ran %d rows, want 1", n)
			}
			if duringErr != nil {
				t.Fatalf("the change made while the upload ran: %v", duringErr)
			}
			if leasedDuring != 1 {
				t.Fatalf("leased close_wait_publish rows during the upload = %d, want 1: the interleaving did not happen under a live lease", leasedDuring)
			}
			if waitingDuring != 1 {
				t.Fatalf("waiting close_wait_publish rows after B's withdrawal during the upload = %d, want 1: the running row counts as no waiting row", waitingDuring)
			}
			if v := lastVersion(); !strings.Contains(v, reasonA) || strings.Contains(v, reasonB) {
				t.Fatalf("the version uploaded while B was withdrawn = %q, want A's reason alone", v)
			}
			if got := withdrawnNow(); !slices.Equal(got, []int64{b}) {
				t.Fatalf("the stored wait's withdrawals after that upload = %v, want [%d]: only what the version showed is cleared", got, b)
			}
			if got := publishRows(false); got != 1 {
				t.Fatalf("close_wait_publish rows after that upload = %d, want 1 waiting to write B", got)
			}

			// That row's upload is refused for good: the row finishes, and B stays.
			versions := len(board.versions)
			board.uploadErr = &dispatch.Error{Status: http.StatusRequestEntityTooLarge, Code: "CAP_EXCEEDED", Message: "a markdown document is at most 1 MiB"}
			if n := drain(); n != 1 {
				t.Fatalf("the refused drain ran %d rows, want 1", n)
			}
			board.uploadErr = nil
			if len(board.versions) != versions {
				t.Fatalf("a refused upload wrote a version: %d, want %d", len(board.versions), versions)
			}
			if got := withdrawnNow(); !slices.Equal(got, []int64{b}) {
				t.Fatalf("the stored wait's withdrawals after a refused upload = %v, want [%d] kept", got, b)
			}
			if got := publishRows(false); got != 0 {
				t.Fatalf("close_wait_publish rows after a refused upload = %d, want 0: the refusal finishes the row", got)
			}

			// The next write of the wait, a report, carries B into the next version, which clears it.
			report("c", "Defect C: internal/retry/loop.go:42 loops after the budget ends, and so does :44.")
			if got := withdrawnNow(); !slices.Equal(got, []int64{b}) {
				t.Fatalf("the stored wait's withdrawals after the next report = %v, want [%d] kept through the write", got, b)
			}
			if n := drain(); n != 1 {
				t.Fatalf("the last drain ran %d rows, want 1", n)
			}
			if v := lastVersion(); !strings.Contains(v, reasonB) || strings.Contains(v, reasonA) {
				t.Fatalf("the next version = %q, want B's reason alone", v)
			}
			if got := withdrawnNow(); len(got) != 0 {
				t.Fatalf("the stored wait's withdrawals after B's version = %v, want none", got)
			}
		})
	}
}
