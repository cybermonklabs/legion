package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// TestZZGapBProbe is a measurement, not a suite test: it sends the phase-backward bodies of the
// deployment-roles change (a `step` target, a `returnTo` place) to this daemon, which predates
// both, and logs what the route answers and where the issue stands afterwards. The route runs
// against the daemon's own workflow over a real Postgres (newWorkflowHarness), so a move that
// applies moves the stored phase. LEGION-208 stands in testing with an open pull request, and its
// tester, the role running testing, holds the grant. Each body gets its own harness, so every
// call starts from the same state.
//
// The "newer plugin" rows repeat the four bodies from a tester whose plugin registered at a
// contract above this daemon's, on a daemon that relaunches no stale plugin (the Kubernetes
// degraded mode), which is the case where such a plugin can hold a grant here at all: a daemon
// that can relaunch refuses the registration itself with 409. Each request states its plugin's
// contract in `Legion-Plugin-Contract`, as every plugin from shapedAnswersContract on does.
//
// Control: the first body of each group (a move back to implementing) must be taken and must move
// the stored phase to implementing, else the test fails, so a harness that never reaches the
// handler cannot read as a refusal.
func TestZZGapBProbe(t *testing.T) {
	const root = "LEGION-208"
	t.Logf("daemon contract DaemonAPIVersion() = %d", DaemonAPIVersion())
	bodies := []struct {
		name    string
		extra   map[string]any
		control bool
	}{
		{name: "a-control", extra: map[string]any{"to": "implementing"}, control: true},
		{name: "b-step", extra: map[string]any{"to": "step"}},
		{name: "c-implementing-returnTo2", extra: map[string]any{"to": "implementing", "returnTo": 2}},
		{name: "d-step-returnTo1", extra: map[string]any{"to": "step", "returnTo": 1}},
	}
	for _, group := range []struct {
		name        string
		newerPlugin bool
	}{{name: "current-plugin"}, {name: "newer-plugin", newerPlugin: true}} {
		for _, b := range bodies {
			t.Run(group.name+"/"+b.name, func(t *testing.T) {
				h, facts, tester := gapbProbeHarness(t, root, group.newerPlugin)
				body := map[string]any{"grantId": tester.grant(t), "reason": "the tests found a gap"}
				for k, v := range b.extra {
					body[k] = v
				}
				before := recordedChild(t, h, root).Phase
				factsBefore := len(facts.recorded())
				stated := DaemonAPIVersion()
				if group.newerPlugin {
					stated++
				}
				got := h.request(http.MethodPost, "/legion/v1/phase/backward", body, http.Header{pluginContractHeader: {strconv.Itoa(stated)}})
				after := recordedChild(t, h, root).Phase
				t.Logf("PROBE %s/%s: recorded plugin contract %d, stated %d; request %v", group.name, b.name, h.stored(tester.token).PluginContract, stated, b.extra)
				t.Logf("PROBE %s/%s: status %d %s; Legion-Daemon-Contract %q", group.name, b.name, got.Code, http.StatusText(got.Code), got.Header().Get("Legion-Daemon-Contract"))
				t.Logf("PROBE %s/%s: body %s", group.name, b.name, got.Body.String())
				t.Logf("PROBE %s/%s: phase before %s, after %s; facts applied by the call %d", group.name, b.name, before, after, len(facts.recorded())-factsBefore)
				if b.control && (got.Code != http.StatusOK || after != phase.Implementing) {
					t.Fatalf("CONTROL FAILED: the move back to implementing answered %d %s and left the issue in %s; the probe cannot tell a refusal from a blind harness",
						got.Code, got.Body, after)
				}
			})
		}
	}
}

// gapbProbeHarness is newWorkflowHarness with LEGION-208 recorded in testing behind an open pull
// request, and the live claim of its tester. newerPlugin builds the same server over supervise
// deps that relaunch nothing and registers the tester at contract DaemonAPIVersion()+1.
func gapbProbeHarness(t *testing.T, root string, newerPlugin bool) (*harness, *factRecorder, liveClaim) {
	t.Helper()
	cfg := workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}
	var h *harness
	var facts *factRecorder
	if newerPlugin {
		h = newHarnessUnrelaunchable(t)
		h.handoffs = &statusRecorder{}
		github := newHandoffGitHub(t)
		facts = &factRecorder{}
		engine := workflow.New(record.NewStore(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		h.handler = NewServer("127.0.0.1", 8437, Options{
			Steps:      cfg.Steps,
			Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
			Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
			Handlers: []intake.Handler{engine, facts}, Dispatch: h.handoffs,
			Tokens: handoffTokens{}, GitHubAPI: github.url,
		}).Handler
	} else {
		h, _, facts = newWorkflowHarness(t, cfg)
	}
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root,
		Project: testProject, Title: root, Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true,
		LastDispatchSeq: 3})
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: root, Repo: "acme/widgets",
			Number: 42, Branch: "legion/" + root, HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}
	if !newerPlugin {
		return h, facts, newLiveClaim(t, h, root, claim.RoleTester)
	}
	token, boot := h.launch(root, claim.RoleTester)
	session := "ses_tester_" + root
	registration := h.registerContract(boot, session, DaemonAPIVersion()+1, false)
	tester := liveClaim{h: h, token: token, session: session, secret: registration.Secret, tree: root, issue: root}
	working(t, h, token, root)
	return h, facts, tester
}
