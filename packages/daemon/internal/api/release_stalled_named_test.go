package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
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
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// CHILD_RELEASE_STALLED tells the caller which architect was told of the stall, so it names only
// an architect a child-release-stalled notice was queued for. The retry is kept per child and the
// notice is queued once, by the call that runs out of attempts, on that caller's own issue; both the
// root's architect and the architect of the child's ancestor C may release the child. When the
// root's architect's call ran out the attempts, a later release by C's architect is refused as
// stalled too, and its answer must not say that the architect of C was notified: nothing was
// queued on C.
func TestAStalledReleaseNamesOnlyAnArchitectItTold(t *testing.T) {
	const root, own, parent, child = "LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211"
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
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root,
		Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	h.recordIssue(record.Issue{Key: own, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: new(root), Project: testProject,
		Title: own, Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
	notified := regexp.MustCompile(`the architect of (\S+) was notified`)
	stalledOn := func(issue string) int {
		var count int
		if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `select count(*) from outbox
				where issue = $1 and kind = 'notice' and payload->>'kind' = 'child-release-stalled'`, issue).Scan(&count)
		}); err != nil {
			t.Fatalf("count the stalled notices on %s: %v", issue, err)
		}
		return count
	}

	// The root's architect's release runs out the attempts: its notice is queued on the root.
	rootArchitect := newLiveClaim(t, h, root, claim.RoleArchitect)
	refused(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": rootArchitect.grant(t), "issues": []string{child}}, nil),
		http.StatusConflict, "CHILD_RELEASE_STALLED")
	if got := stalledOn(root); got != 1 {
		t.Fatalf("stalled notices on the root %s after its architect's exhausting release = %d, want 1", root, got)
	}

	// C's architect's later release is refused as stalled; whatever architect its answer names
	// must be one a stalled notice was queued for.
	sub := newLiveClaimIn(t, h, root, own, claim.RoleArchitect)
	answer := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": sub.grant(t), "issues": []string{child}}, nil)
	body := answer.Body.String()
	refused(t, answer, http.StatusConflict, "CHILD_RELEASE_STALLED")
	if match := notified.FindStringSubmatch(body); match != nil && stalledOn(match[1]) == 0 {
		t.Fatalf("the refusal %s says the architect of %s was notified, but no child-release-stalled notice was queued on %s (root %s holds %d)",
			body, match[1], match[1], root, stalledOn(root))
	}
}
