package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A release_children that stalls answers the architect that called it, so its child-release-stalled
// notice is queued on that architect's own issue, where the owner walk takes it to that architect
// while it holds its role, and on up the tree once it does not: a sub-architect's release is told
// to the sub-architect, not to the root's architect. That holds for the release whose unrecorded
// parent ran out of attempts (CHILD_RELEASE_STALLED) and for the one the recovery of an unread
// repository will not record, since its chain stops at a parent in backlog.
func TestASubArchitectsStalledReleaseIsToldOnItsOwnIssue(t *testing.T) {
	const root, own = "LEGION-208", "LEGION-209"
	stalledOn := func(t *testing.T, h *harness, issue string) []string {
		t.Helper()
		var facts []string
		if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `select `+stalledFacts+` from outbox
				where issue = $1 and kind = 'notice' and payload->>'kind' = 'child-release-stalled' order by id`, issue)
			if err != nil {
				return err
			}
			facts, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatalf("read the stalled notices on %s: %v", issue, err)
		}
		return facts
	}
	ownIssue := record.Issue{Key: own, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: new(root), Project: testProject, Title: own,
		Phase: phase.Planning, Generation: 1, Status: "in_progress"}

	t.Run("a parent that never reaches the daemon", func(t *testing.T) {
		const parent, child = "LEGION-210", "LEGION-211"
		h := newHarness(t)
		records := record.NewStore()
		engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		statuses := &statusRecorder{}
		h.handler = NewServer("127.0.0.1", 8437, Options{
			Steps:      config.DesignGateRootIssues.Steps(),
			Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
			Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
			Handlers: []intake.Handler{engine, admit.New(records, engine, 8, testProject, slog.New(slog.NewTextHandler(io.Discard, nil)))}, Dispatch: statuses,
		}).Handler
		h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
		h.recordIssue(ownIssue)
		if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
			if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now()}); err != nil {
				return err
			}
			return records.PutReleaseRetry(context.Background(), tx, record.ReleaseRetry{Issue: child, Sequence: 5, Attempts: maxChildReleaseAttempts - 1, NextAt: time.Now().Add(-time.Second)})
		}); err != nil {
			t.Fatalf("seed the tree and the retry: %v", err)
		}
		statuses.issues = map[string]dispatch.Issue{
			root:   {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
			own:    {Key: own, ResolvedRepo: "acme/widgets", Title: own, Status: "in_progress", Parent: new(root), LastSeq: 2},
			parent: {Key: parent, ResolvedRepo: "acme/widgets", Title: parent, Status: "todo", Parent: new(own), LastSeq: 3},
			child:  {Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: new(parent), LastSeq: 5},
		}
		sub := newLiveClaimIn(t, h, root, own, claim.RoleArchitect)
		refused(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": sub.grant(t), "issues": []string{child}}, nil),
			http.StatusConflict, "CHILD_RELEASE_STALLED")
		if got, want := stalledOn(t, h, own), stalledFact(texts.StallAttemptsSpent, child, parent, "", "", "", "", maxChildReleaseAttempts); len(got) != 1 || got[0] != want {
			t.Fatalf("stalled notices on %s = %q, want [%s]", own, got, want)
		}
		if got := stalledOn(t, h, root); len(got) != 0 {
			t.Fatalf("stalled notices on the root %s = %q, want none: the release was %s's architect's", root, got, own)
		}
	})

	t.Run("a parent in backlog on an unread repository", func(t *testing.T) {
		const parent, child = "LEGION-220", "LEGION-221"
		u := newUnreadRelease(t,
			dispatch.Issue{Key: own, ResolvedRepo: "acme/widgets", Title: own, Status: "in_progress", Parent: new(root), LastSeq: 2},
			on(parent, "zeta/tools", "backlog", own, 3), on(child, "zeta/tools", "todo", parent, 4))
		u.h.recordIssue(ownIssue)
		sub := newLiveClaimIn(t, u.h, root, own, claim.RoleArchitect)
		if recorder := u.h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": sub.grant(t), "issues": []string{child}}, nil); recorder.Code != http.StatusOK {
			t.Fatalf("the sub-architect's release of %s = %d: %s, want it released", child, recorder.Code, recorder.Body)
		}
		if got, want := stalledOn(t, u.h, own), stalledFact(texts.StallUnrecordedBlocker, child, parent, "", parent, "backlog", "zeta/tools", 0); len(got) != 1 || got[0] != want {
			t.Fatalf("stalled notices on %s = %q, want [%s]: release %s first", own, got, want, parent)
		}
		if got := stalledOn(t, u.h, root); len(got) != 0 {
			t.Fatalf("stalled notices on the root %s = %q, want none: the release was %s's architect's", root, got, own)
		}
	})
}
