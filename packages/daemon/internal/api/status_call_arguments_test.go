package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// onlyStatusRow is the one status row of issue the outbox holds, failing the test where it holds
// another count.
func onlyStatusRow(t *testing.T, h *harness, issue string) record.StatusWrite {
	t.Helper()
	rows := statusRows(t, h, issue)
	if len(rows) != 1 {
		t.Fatalf("status rows of %s = %v, want one", issue, rows)
	}
	for _, row := range rows {
		return row
	}
	return record.StatusWrite{}
}

// The controller's set_status of backlog or icebox on an issue an open child stands under in work
// would leave that child in work under an issue that is not, which Dispatch's parent rule refuses
// (R24 of dispatch://INFRA-3320). The call reads the issue's children from the copy of Dispatch's
// issues, which holds the child's row from its events. Without a children argument the call is
// refused CHILDREN_ARGUMENT_REQUIRED, naming each such child with its status, and queues nothing;
// with one it is taken, and its queued write carries the option Dispatch's status-change route
// takes: include moves the children with it (include_children), unlink takes them out from under
// it (unlink_children).
func TestSetStatusTakesAChildrenArgument(t *testing.T) {
	for _, status := range []string{"backlog", "icebox"} {
		for _, children := range []string{"", intake.IncludeArgument, intake.UnlinkArgument} {
			t.Run(status+" children="+children, func(t *testing.T) {
				h, _, statuses := statusHarness(t)
				if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
					if err := seedCopy(tx, statusRoot, "", "in_progress"); err != nil {
						return err
					}
					return seedCopy(tx, statusChild, statusRoot, "in_progress")
				}); err != nil {
					t.Fatalf("seed the copy: %v", err)
				}
				grant := controllerGrant(t, h)
				recorder := h.request(http.MethodPost, "/legion/v1/issues/status",
					IssueStatusRequest{GrantID: grant.GrantID, Issue: statusRoot, Status: status, Children: children}, nil)
				if children == "" {
					message := refused(t, recorder, http.StatusConflict, "CHILDREN_ARGUMENT_REQUIRED")
					if !strings.Contains(message, statusChild+" (in_progress)") {
						t.Fatalf("the refusal = %q, want it to name %s with its status", message, statusChild)
					}
					if rows := statusRows(t, h, statusRoot); len(rows) != 0 || len(statuses.writes) != 0 {
						t.Fatalf("the refused call queued %v and wrote %v, want nothing", rows, statuses.writes)
					}
					return
				}
				if recorder.Code != http.StatusOK {
					t.Fatalf("set_status %s with children %s = %d %s, want 200", status, children, recorder.Code, recorder.Body)
				}
				row := onlyStatusRow(t, h, statusRoot)
				if row.Status != status || row.IncludeChildren != (children == intake.IncludeArgument) || row.UnlinkChildren != (children == intake.UnlinkArgument) ||
					row.IncludeParent || !row.Controller {
					t.Fatalf("the queued write = %+v, want the controller's %s with only the %s option", row, status, children)
				}
			})
		}
	}
}

// A child in work that the daemon does not record — one a person or an architect took into work
// outside Legion — is a child the backlog would leave in work under an issue that is not, as a
// recorded one is: set_status names it with the status its row of the copy holds, though no chain
// the call reads passes through it.
//
// Negative control, run when this was written: with the child's status read only from the chains
// the call walked, the unrecorded child reads no status and the refusal names the recorded child
// alone.
func TestSetStatusNamesAChildInWorkTheDaemonDoesNotRecord(t *testing.T) {
	const unrecorded = "LEGION-210"
	h, _, statuses := statusHarness(t)
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := seedCopy(tx, statusRoot, "", "in_progress"); err != nil {
			return err
		}
		if err := seedCopy(tx, statusChild, statusRoot, "in_progress"); err != nil {
			return err
		}
		return seedCopy(tx, unrecorded, statusRoot, "testing")
	}); err != nil {
		t.Fatalf("seed the copy: %v", err)
	}
	grant := controllerGrant(t, h)
	recorder := h.request(http.MethodPost, "/legion/v1/issues/status",
		IssueStatusRequest{GrantID: grant.GrantID, Issue: statusRoot, Status: "backlog"}, nil)
	message := refused(t, recorder, http.StatusConflict, "CHILDREN_ARGUMENT_REQUIRED")
	for _, child := range []string{statusChild + " (in_progress)", unrecorded + " (testing)"} {
		if !strings.Contains(message, child) {
			t.Fatalf("the refusal = %q, want it to name %s", message, child)
		}
	}
	if rows := statusRows(t, h, statusRoot); len(rows) != 0 || len(statuses.writes) != 0 {
		t.Fatalf("the refused call queued %v and wrote %v, want nothing", rows, statuses.writes)
	}
}

// A todo on an issue whose Dispatch parent is not in work would put it in work under a parent
// that is not, which Dispatch's parent rule refuses (R24 of dispatch://INFRA-3320). The
// controller's set_status and the architect's rerun_child each refuse it without a parent
// argument, PARENT_ARGUMENT_REQUIRED, naming each ancestor out of work up to the first in work, and
// queue nothing; with parent include each is taken and its queued todo carries include_parent,
// which moves those ancestors to in_progress with it.
func TestATodoUnderAParentNotInWorkTakesAParentArgument(t *testing.T) {
	for _, op := range []string{"set_status", "rerun_child"} {
		for _, parent := range []string{"", intake.IncludeArgument} {
			t.Run(op+" parent="+parent, func(t *testing.T) {
				h, _, statuses := statusHarness(t)
				architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
				// A person moved the root and then the child to the backlog: both are out of work,
				// their phases kept.
				for _, key := range []string{statusRoot, statusChild} {
					setIssue(t, h, key, func(issue *record.Issue) { issue.Status, issue.DispatchStatus = "backlog", "backlog" })
					shown := statuses.issues[key]
					shown.Status = "backlog"
					statuses.issues[key] = shown
				}
				var recorder *httptest.ResponseRecorder
				if op == "set_status" {
					grant := controllerGrant(t, h)
					recorder = h.request(http.MethodPost, "/legion/v1/issues/status",
						IssueStatusRequest{GrantID: grant.GrantID, Issue: statusChild, Status: "todo", Parent: parent}, nil)
				} else {
					recorder = h.request(http.MethodPost, "/legion/v1/children/rerun",
						RerunChildRequest{GrantID: architect.grant(t), Issue: statusChild, Parent: parent}, nil)
				}
				if parent == "" {
					message := refused(t, recorder, http.StatusConflict, "PARENT_ARGUMENT_REQUIRED")
					if !strings.Contains(message, statusRoot+" (backlog)") {
						t.Fatalf("the refusal = %q, want it to name %s with its status", message, statusRoot)
					}
					if rows := statusRows(t, h, statusChild); len(rows) != 0 || len(statuses.writes) != 0 {
						t.Fatalf("the refused %s queued %v and wrote %v, want nothing", op, rows, statuses.writes)
					}
					return
				}
				if recorder.Code != http.StatusOK {
					t.Fatalf("%s todo with parent include = %d %s, want 200", op, recorder.Code, recorder.Body)
				}
				var todo []record.StatusWrite
				for _, row := range statusRows(t, h, statusChild) {
					if row.Status == "todo" {
						todo = append(todo, row)
					}
				}
				if len(todo) != 1 || !todo[0].IncludeParent || todo[0].IncludeChildren || todo[0].UnlinkChildren {
					t.Fatalf("the queued todos of %s = %+v, want one carrying include_parent alone", statusChild, todo)
				}
			})
		}
	}
}

// release_children takes no parent argument: within one wave it writes a parent before its child,
// the child's row waiting for the parent's (record.StatusWrite.After), and it refuses, with
// PARENT_NOT_RELEASED naming the parent, an issue whose own Dispatch parent is not in work and is
// not released in the same wave, which then queues nothing.
func TestReleaseChildrenRefusesAnIssueWhoseParentStaysOutOfWork(t *testing.T) {
	const parent, child = "LEGION-209", "LEGION-210"
	setUp := func(t *testing.T) (*harness, liveClaim) {
		h, statuses := releaseHarness(t, true)
		statuses.issues[parent] = backlogChild(parent)
		under := backlogChild(child)
		key := parent
		under.Parent = &key
		statuses.issues[child] = under
		return h, newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)
	}

	t.Run("the child alone", func(t *testing.T) {
		h, architect := setUp(t)
		message := refused(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil),
			http.StatusConflict, "PARENT_NOT_RELEASED")
		if !strings.Contains(message, child+" is under "+parent+", which is backlog and not in work") {
			t.Fatalf("the refusal = %q, want it to name %s and its status", message, parent)
		}
		if queued := queuedTodos(t, h); len(queued) != 0 {
			t.Fatalf("the refused wave queued %v, want nothing", queued)
		}
	})
	for _, wave := range [][]string{{parent, child}, {child, parent}} {
		t.Run("the parent and the child in one wave, "+strings.Join(wave, " then "), func(t *testing.T) {
			h, architect := setUp(t)
			releaseWave(t, h, architect, wave...)
			if want := []string{parent, child}; !slices.Equal(queuedTodos(t, h), want) {
				t.Fatalf("queued todos = %v, want %v: the parent's before its child's", queuedTodos(t, h), want)
			}
			var parentRow int64
			for id, row := range statusRows(t, h, parent) {
				if row.Status == "todo" {
					parentRow = id
				}
			}
			for _, row := range statusRows(t, h, child) {
				if row.Status == "todo" && row.After != parentRow {
					t.Fatalf("the child's todo = %+v, want it waiting for the parent's row %d", row, parentRow)
				}
			}
		})
	}
}
