package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// chainRow is one issue a test records and gives a row of the copy of Dispatch's issues: its key,
// its Dispatch parent ("" for none), the status Dispatch shows of it and the phase it stands at.
type chainRow struct {
	key, parent, status string
	at                  phase.Phase
}

// seedChain records each row, in its tree under the first row's key, and writes its row of the copy
// of Dispatch's issues, so the daemon reads each chain from the copy (R15 of dispatch://INFRA-3320).
func seedChain(t *testing.T, h *harness, rows ...chainRow) {
	t.Helper()
	records := record.NewStore()
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range rows {
			issue := record.Issue{Key: row.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: rows[0].key,
				Project: testDispatchProject, Title: row.key, Phase: row.at, Generation: 1, Status: row.status, DispatchStatus: row.status, LastDispatchSeq: 1}
			if row.parent != "" {
				parent := row.parent
				issue.Parent = &parent
			}
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := seedCopy(tx, row.key, row.parent, row.status); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the chain: %v", err)
	}
}

// An architect acts on its own issue and on the issues under it on the Dispatch chain, whatever the
// status of the issues between them (R18 of dispatch://INFRA-3320), and release_children and
// rerun_child take only an issue strictly under its own. Over a chain three deep - root R, its
// children A and S in work, A's child A1 on the backlog and A1's child A11 in work under it - the
// architect of A signs off and retries A, A1 and A11, and is refused ISSUE_NOT_OWNED on R, S and
// S's child, the refusal naming the architect who acts on the issue; the architect of R acts on
// every one. release_children and rerun_child, decided by the workflow over the copy the route
// brought up to date, take from the architect of A only issues under A: A itself, R, S and S's
// children are refused ISSUE_NOT_OWNED, an issue on no chain through A too, and a refused call
// queues nothing. Each architect's catch-up naming the issues it acts on is the workflow's
// (workflow's TestARootsCatchUpNamesOnlyTheAcceptancesItsArchitectOwns).
func TestAnArchitectActsOnItsIssueAndTheIssuesUnderItOnTheChain(t *testing.T) {
	const (
		root, own, sibling = "LEGION-208", "LEGION-209", "LEGION-210"
		ownBacklog, below  = "LEGION-211", "LEGION-212"
		siblingChild       = "LEGION-213"
	)
	t.Run("sign_off and retry_or_escalate act on the architect's issue and the issues under it", func(t *testing.T) {
		h, facts, _ := newArchitectHarness(t, nil, nil)
		seedChain(t, h,
			chainRow{root, "", "in_progress", phase.Implementing},
			chainRow{own, root, "in_progress", phase.Implementing},
			chainRow{sibling, root, "in_progress", phase.Implementing},
			chainRow{ownBacklog, own, "backlog", phase.Planning},
			chainRow{below, ownBacklog, "in_progress", phase.Implementing},
			chainRow{siblingChild, sibling, "in_progress", phase.Implementing},
		)
		rootArchitect := newLiveClaim(t, h, root, claim.RoleArchitect)
		architect := newLiveClaim(t, h, own, claim.RoleArchitect)
		for _, route := range []struct {
			op, path string
			extra    map[string]any
		}{{"sign_off", "/legion/v1/signoff", nil}, {"retry_or_escalate", "/legion/v1/phase/retry", map[string]any{"decision": "retry"}}} {
			call := func(caller liveClaim, issue string) *httptest.ResponseRecorder {
				body := map[string]any{"grantId": caller.grant(t), "issue": issue}
				for key, value := range route.extra {
					body[key] = value
				}
				return h.request(http.MethodPost, route.path, body, nil)
			}
			for _, issue := range []string{own, ownBacklog, below} {
				if got := call(architect, issue); got.Code != http.StatusOK {
					t.Fatalf("%s of %s by the architect of %s = %d %s, want 200", route.op, issue, own, got.Code, got.Body)
				}
			}
			for _, issue := range []string{root, sibling, siblingChild} {
				body := refused(t, call(architect, issue), http.StatusForbidden, "ISSUE_NOT_OWNED")
				if !strings.Contains(body, route.op+" of "+issue) || !strings.Contains(body, "legion-legion-legion-208-architect") {
					t.Fatalf("%s of %s by the architect of %s: refusal %s, want it to name the operation and the architect of %s, who acts on it", route.op, issue, own, body, root)
				}
			}
			for _, issue := range []string{root, own, sibling, ownBacklog, below, siblingChild} {
				if got := call(rootArchitect, issue); got.Code != http.StatusOK {
					t.Fatalf("%s of %s by the architect of %s = %d %s, want 200", route.op, issue, root, got.Code, got.Body)
				}
			}
		}
		if got := len(facts.recorded()); got != 18 {
			t.Fatalf("facts applied = %d, want one for each of the 18 calls the rule allows", got)
		}
	})

	t.Run("release_children and rerun_child take only issues under the architect's issue", func(t *testing.T) {
		const (
			ownTriage, ownDone, ownRunning = "LEGION-214", "LEGION-215", "LEGION-216"
			siblingTriage, siblingDone     = "LEGION-217", "LEGION-218"
			foreign                        = "LEGION-300"
		)
		h, statuses := releaseHarness(t, true)
		parent := func(key string) *string { return &key }
		for _, row := range []struct {
			key, parent, status string
			at                  phase.Phase
		}{
			{own, root, "in_progress", phase.Implementing}, {sibling, root, "in_progress", phase.Implementing},
			{ownDone, own, "done", phase.Done}, {ownRunning, own, "in_progress", phase.Implementing}, {siblingDone, sibling, "done", phase.Done},
		} {
			h.recordIssue(record.Issue{Key: row.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: parent(row.parent),
				Project: testDispatchProject, Title: row.key, Phase: row.at, Generation: 1, Status: row.status, DispatchStatus: row.status, LastDispatchSeq: 7})
			statuses.issues[row.key] = dispatch.Issue{Key: row.key, ResolvedRepo: "acme/widgets", Title: row.key, Status: row.status, Parent: parent(row.parent), LastSeq: 7}
		}
		for key, under := range map[string]string{ownTriage: own, siblingTriage: sibling} {
			statuses.issues[key] = dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Title: key, Status: "triage", Parent: parent(under), LastSeq: 7}
		}
		statuses.issues[foreign] = dispatch.Issue{Key: foreign, ResolvedRepo: "acme/widgets", Title: foreign, Status: "backlog", LastSeq: 7}
		rootArchitect := newLiveClaim(t, h, root, claim.RoleArchitect)
		architect := newLiveClaim(t, h, own, claim.RoleArchitect)

		type answer struct {
			status int
			code   string
		}
		var (
			applied     = answer{http.StatusOK, ""}
			notOwned    = answer{http.StatusForbidden, "ISSUE_NOT_OWNED"}
			notRecorded = answer{http.StatusNotFound, "ISSUE_NOT_FOUND"}
			released    = answer{http.StatusConflict, "ISSUE_ALREADY_RELEASED"}
			running     = answer{http.StatusConflict, "CHILD_RUNNING"}
		)
		call := func(caller liveClaim, op, issue string, want answer) {
			t.Helper()
			before := queuedTodos(t, h)
			path, body := "/legion/v1/children/rerun", map[string]any{"grantId": caller.grant(t), "issue": issue}
			if op == "release_children" {
				path, body = "/legion/v1/waves/release", map[string]any{"grantId": caller.grant(t), "issues": []string{issue}}
			}
			recorder := h.request(http.MethodPost, path, body, nil)
			if want == applied {
				if recorder.Code != http.StatusOK {
					t.Fatalf("%s of %s by the architect of %s = %d %s, want 200", op, issue, caller.issue, recorder.Code, recorder.Body)
				}
				if got := queuedTodos(t, h); !reflect.DeepEqual(got, append(before, issue)) {
					t.Fatalf("%s of %s by the architect of %s queued %v after %v, want its todo alone", op, issue, caller.issue, got, before)
				}
				return
			}
			text := refused(t, recorder, want.status, want.code)
			if want == notOwned && !strings.Contains(text, op+" of "+issue) {
				t.Fatalf("%s of %s by the architect of %s: refusal %s does not name the operation", op, issue, caller.issue, text)
			}
			if got := queuedTodos(t, h); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s of %s by the architect of %s was refused %s but queued %v", op, issue, caller.issue, want.code, got[len(before):])
			}
		}

		// The wave is judged whole before anything is queued: the sibling refuses the child under A
		// named beside it too.
		refused(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{ownTriage, sibling}}, nil),
			http.StatusForbidden, "ISSUE_NOT_OWNED")
		if got := queuedTodos(t, h); len(got) != 0 {
			t.Fatalf("a refused wave queued %v", got)
		}
		// Each row is one issue the architect of A names, and what each call answers it: rerun_child
		// first, so an unreleased child under A, which the daemon does not record, is not found
		// until release_children has released it.
		for _, row := range []struct {
			issue          string
			rerun, release answer
		}{
			{root, notOwned, notOwned},
			{own, notOwned, notOwned},
			{sibling, notOwned, notOwned},
			{siblingTriage, notOwned, notOwned},
			{siblingDone, notOwned, notOwned},
			{foreign, notOwned, notOwned},
			{ownTriage, notRecorded, applied},
			{ownRunning, running, released},
			{ownDone, applied, released},
		} {
			call(architect, "rerun_child", row.issue, row.rerun)
			call(architect, "release_children", row.issue, row.release)
		}
		// The architect of R acts on every issue under it, S's children included.
		call(rootArchitect, "release_children", siblingTriage, applied)
		call(rootArchitect, "rerun_child", siblingDone, applied)

		if want := []string{ownTriage, ownDone, siblingTriage, siblingDone}; !reflect.DeepEqual(queuedTodos(t, h), want) {
			t.Fatalf("queued todos = %v, want only the allowed calls' %v", queuedTodos(t, h), want)
		}
	})
}

// Each architect call is judged twice: by the route, over the chain it reads, and by the workflow
// where the call's fact commits, over the chain that transaction reads (decision 5 of
// dispatch://INFRA-4730). A change of parent that lands between the two is the one the workflow
// judges by: sign_off, request_backward_move, retry_or_escalate, rerun_child and release_children,
// each allowed by the route while its issue stood under R and committed once a person moved the
// issue under the root Q, are refused ISSUE_NOT_OWNED, and the refusal names Q's architect, who may
// act on it now (R18 of dispatch://INFRA-3320). The route told the seam of each call, so each was
// allowed before the move; the refused call writes nothing: no status row, and the issue's record
// as the move left it.
func TestAnArchitectCallCommittedAfterAReparentIsRefused(t *testing.T) {
	const (
		other           = "LEGION-300"
		closed, triaged = "LEGION-211", "LEGION-212"
	)
	for _, tc := range []struct {
		op, path, issue string
		body            map[string]any
	}{
		{"sign_off", "/legion/v1/signoff", statusChild, nil},
		{"request_backward_move", "/legion/v1/phase/backward", statusChild, map[string]any{"to": "planning", "reason": "the plan missed the migration"}},
		{"retry_or_escalate", "/legion/v1/phase/retry", statusChild, map[string]any{"decision": "retry"}},
		{"rerun_child", "/legion/v1/children/rerun", closed, nil},
		{"release_children", "/legion/v1/waves/release", triaged, nil},
	} {
		t.Run(tc.op, func(t *testing.T) {
			var handlers []intake.Handler
			var h *harness
			var told []string
			moved := func(ctx context.Context, op string, issues []string) {
				told = append(told, op)
				view := intake.DispatchIssue{Key: tc.issue, Seq: 10, Type: "issue.updated", Status: "in_progress", Title: tc.issue, Parent: other,
					ResolvedRepo: "acme/widgets", ClaimKnown: true}
				switch tc.issue {
				case closed:
					view.Status, view.Type = "done", "issue.closed"
				case triaged:
					view.Status = "triage"
				}
				if _, err := intake.ApplyFact(ctx, h.store.Pool(), "dispatch", "reparent-"+tc.issue, view, handlers...); err != nil {
					t.Errorf("move %s under %s: %v", tc.issue, other, err)
				}
			}
			var statuses *statusRecorder
			h, handlers, statuses = statusHarness(t, func(opts *Options) { opts.CallJudged = moved })
			root := statusRoot
			h.recordIssue(record.Issue{Key: other, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: other,
				Project: testDispatchProject, Title: other, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", LastDispatchSeq: 3})
			h.recordIssue(record.Issue{Key: closed, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
				Project: testDispatchProject, Title: closed, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", LastDispatchSeq: 5})
			statuses.issues[other] = dispatch.Issue{Key: other, ResolvedRepo: "acme/widgets", Title: other, Status: "in_progress", LastSeq: 3}
			statuses.issues[closed] = dispatch.Issue{Key: closed, ResolvedRepo: "acme/widgets", Title: closed, Status: "done", Parent: &root, LastSeq: 5}
			statuses.issues[triaged] = dispatch.Issue{Key: triaged, ResolvedRepo: "acme/widgets", Title: triaged, Status: "triage", Parent: &root, LastSeq: 5}
			if err := store.Transact(context.Background(), h.store.Pool(), func(_ context.Context, tx pgx.Tx) error {
				for _, row := range []struct{ key, parent, status string }{
					{statusRoot, "", "in_progress"}, {statusChild, statusRoot, "in_progress"}, {other, "", "in_progress"},
					{closed, statusRoot, "done"}, {triaged, statusRoot, "triage"},
				} {
					if err := seedCopy(tx, row.key, row.parent, row.status); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed the copy: %v", err)
			}
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			newLiveClaim(t, h, other, claim.RoleArchitect)
			before := recordedChild(t, h, tc.issue)

			body := map[string]any{"grantId": architect.grant(t), "issue": tc.issue}
			if tc.op == "release_children" {
				body = map[string]any{"grantId": architect.grant(t), "issues": []string{tc.issue}}
			}
			for key, value := range tc.body {
				body[key] = value
			}
			text := refused(t, h.request(http.MethodPost, tc.path, body, nil), http.StatusForbidden, "ISSUE_NOT_OWNED")
			if !reflect.DeepEqual(told, []string{tc.op}) {
				t.Fatalf("the seam was told %v, want the route to have allowed the %s once before the move", told, tc.op)
			}
			if !strings.Contains(text, tc.op+" of "+tc.issue) || !strings.Contains(text, "legion-legion-legion-300-architect") {
				t.Fatalf("the refusal = %s, want it to name the %s of %s and the architect of %s, who may act on it now", text, tc.op, tc.issue, other)
			}
			if rows := statusRows(t, h, tc.issue); len(rows) != 0 {
				t.Fatalf("status rows of %s after the refused %s = %v, want none", tc.issue, tc.op, rows)
			}
			after := recordedChild(t, h, tc.issue)
			if (before == nil) != (after == nil) || after != nil && (after.Phase != before.Phase || after.Generation != before.Generation || after.Parent == nil || *after.Parent != other) {
				t.Fatalf("the record of %s after the refused %s = %#v, want it as the move left it from %#v: under %s, its phase and run kept", tc.issue, tc.op, after, before, other)
			}
		})
	}
}
