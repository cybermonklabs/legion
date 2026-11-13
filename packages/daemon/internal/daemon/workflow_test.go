package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// linger_hours and production_check_hours reach the workflow engine as the durations they name, a
// decimal included: the Stage 4b proof lingers 0.3 hours so a tree closes inside one run, and an
// hour count truncated to zero would reach the engine as no linger at all, closing a finished tree
// the moment it ends, or as no check bound, which the engine's default would then stand in for.
func TestLingerHoursReachesTheEngineAsItsDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legion.yaml")
	body := "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nlinger_hours: 0.3\nproduction_check_hours: 0.1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}

	engine := engineConfig(&cfg, "legion-reviewer[bot]")
	if engine.Linger != 18*time.Minute {
		t.Errorf("the engine's linger = %s, want 18m0s", engine.Linger)
	}
	if engine.ProductionCheckBound != 6*time.Minute {
		t.Errorf("the engine's production check bound = %s, want 6m0s", engine.ProductionCheckBound)
	}
}

// loginTokens answers each App role's lease, and its login, with the bot login named for it.
type loginTokens map[appauth.AppRole]string

func (t loginTokens) Token(_ context.Context, role appauth.AppRole, _ ghrepo.Repository) (appauth.Lease, error) {
	return appauth.Lease{Token: "token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: t[role]}}, nil
}

func (t loginTokens) AppLogin(_ context.Context, role appauth.AppRole) (string, error) {
	return t[role], nil
}

func (loginTokens) InstalledOn(context.Context, appauth.AppRole, ghrepo.Repository) (bool, error) {
	return true, nil
}

// Forget drops nothing: every lease is minted afresh.
func (loginTokens) Forget(appauth.AppRole, string) {}

func (t loginTokens) ReadToken(ctx context.Context, role appauth.AppRole, _ string, _ []string) (appauth.Lease, error) {
	lease, err := t.Token(ctx, role, ghrepo.Repository{})
	lease.Token += "-read"
	return lease, err
}

// The engine judges a push by its pusher against the review App's login, so a configuration whose
// two roles resolve to one App would read every red the implementer leaves as a planned one: boot
// refuses it, and a review App with no login, before anything else opens.
func TestOpenWorkflowRefusesAReviewLoginItCannotTellFromTheImplementers(t *testing.T) {
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servedBootCall(t, w, r, "demo") {
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(dispatchServer.Close)
	cfg := config.Config{
		Project: "demo", DispatchURL: dispatchServer.URL,
		Projects: map[string]config.Project{"demo": {}},
	}
	github := repositoriesGitHub(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, tokens := range map[string]loginTokens{
		"one App for both roles": {appauth.Implement: "legion-bot[bot]", appauth.Review: "legion-bot[bot]"},
		"no review login":        {appauth.Implement: "legion-implementer[bot]"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openWorkflow(context.Background(), cfg, config.Settled(cfg), newRebuilder(log, config.Settled(cfg)), nil, "project-id", "dispatch-token", nil, newRepositories(cfg.Project, cfg.RepositoryRenames), log, tokens, github, nil)
			if err == nil || !strings.Contains(err.Error(), "the workflow needs two different Apps") {
				t.Fatalf("openWorkflow = %v, want the two-Apps refusal", err)
			}
		})
	}
}

// The boot reads the review App's bot login, the login the engine judges a push's pusher against,
// under the App's own JWT, with no owner to mint for.
func TestAppLoginsAtBootReturnsTheReviewAppLogin(t *testing.T) {
	tokens := loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}
	login, err := appLoginsAtBoot(context.Background(), tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("appLoginsAtBoot: %v", err)
	}
	if login != "legion-reviewer[bot]" {
		t.Fatalf("appLoginsAtBoot = %q, want the review App's login %q", login, "legion-reviewer[bot]")
	}
}

// The engine a boot built goes on firing the hook boot read, whatever the file says afterwards.
//
// The engine reads its configuration through the closure openWorkflow hands workflow.NewReading,
// and that closure is where the hook list is put on it: the words of a hook that names a
// `prompt_file` live in no configuration at all — boot reads them into a map of its own — so a
// list the closure composed from the settings holder alone would stand that hook in front of the
// roles it names with no words at all. The engine answers no read of the list it holds, so what
// it holds is read where it says it: a firing, whose row carries the hook's name and its words
// (record.HookPrompt).
//
// The boot here is openWorkflow itself. The operator then renames the file-backed hook and points
// it at a file nobody wrote, and the file arm settles that edit; the move back after it fires the
// hook boot read, by the name boot read it under and in the words of the file boot read.
//
// What this holds is that the closure puts boot's list on every decision's configuration: delete
// that line and the move back fires nothing. It does not tell that line from one that reads
// `engineHooks(live.Snapshot().Hooks, hookPrompts)` there, and no test of this daemon can: the
// settle pins `hooks` back to the list boot read (config's pinFixed), so the two are the same
// list. What the pin does not carry is the words — `engineHooks(live.Snapshot().Hooks, nil)`
// fires a hook saying nothing, and this test reds on it.
func TestAFileReloadLeavesTheEngineFiringTheHookBootRead(t *testing.T) {
	ctx := context.Background()
	const words = "Say what the round missed."
	pool := isolatedOutboxPool(t)
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servedBootCall(t, w, r, hookBootProject) {
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(board.Close)
	live := hookBootSettings(t, pool.Config().ConnString(), board.URL, words)
	boot := *live.Snapshot()
	// The words of every hook that names a file, read as boot reads them (readBoot): the one map
	// the hook list the engine runs is built from, which the boot below builds and this test does
	// not.
	prompts := map[string]string{}
	for _, hook := range boot.Hooks {
		read, err := config.ReadHookPrompt(hook.Name, hook.PromptFile)
		if err != nil {
			t.Fatalf("read the hook's words as boot reads them: %v", err)
		}
		prompts[hook.Name] = read
	}

	log := quietLogger()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	w, err := openWorkflow(ctx, boot, live, newRebuilder(log, live), st, "project-id", "dispatch-token", prompts,
		newRepositories(boot.Project, boot.RepositoryRenames), log,
		loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"},
		repositoriesGitHub(t), nil)
	if err != nil {
		t.Fatalf("open the workflow: %v", err)
	}

	// The operator renames the hook and points it at a file of its own, which nothing reads: a
	// hook's file is read at boot and at no later moment. The same edit raises the admission cap,
	// a decision key, so the reload is seen to have happened.
	setInConfig(t, live, "hooks", "[{name: shove, on: work_back, every: 1, roles: [implementer], prompt_file: shove.md}]")
	setInConfig(t, live, "admission_cap", "9")
	if _, _, err := live.Apply(config.SourceFile, "", map[string]any{}); err != nil {
		t.Fatalf("the file arm of the settings route: %v", err)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 9 {
		t.Fatalf("the settled admission cap = %d, want the file's 9: the reload this test is about did not happen", cap)
	}

	// The implementer takes its retro back to implementing, the move back the hook stands on, and
	// the engine decides it through the daemon's own handlers.
	key := boot.Project + "-1"
	seedRetroIssue(t, w.pool, boot.Project, key)
	result, err := intake.ApplyFact(ctx, w.pool, "api", "retro-back:"+key,
		intake.BackwardMove{Issue: key, Requester: claim.RoleImplementer, To: phase.Implementing, Reason: "rework"}, w.handlers...)
	if err != nil {
		t.Fatalf("move %s back to implementing: %v", key, err)
	}
	if result.Refusal != nil {
		t.Fatalf("the move back of %s was refused: %+v", key, result.Refusal)
	}

	fired := firedHooks(t, w.pool)
	if len(fired) != 1 {
		t.Fatalf("the move back fired %+v, want the one hook boot read", fired)
	}
	if fired[0].Hook != "nudge" || fired[0].Role != claim.RoleImplementer || fired[0].Prompt != words {
		t.Fatalf("the engine fired %+v, want the hook `nudge` boot read putting the words of its prompt_file %q in front of the implementer: the engine is handed the list boot built, never one composed from the settings holder",
			fired[0], words)
	}
}

// The engine the daemon opens decides each fact by the design gate the settings holder holds
// when that decision begins, not the one boot read: openWorkflow hands workflow.NewReading a read
// of the holder, and NewReading reads it again at every decision. A change of `gates.design`
// settled through the file arm is therefore the gate the running engine reports next.
//
// The other half is TestADecisionReadsTheSettingsHolderOnce in the workflow package: one read
// per decision. An engine that read the holder once at construction passes that test, and
// before this one it passed every row of this package as well — they build the engine on a
// configuration that never changes — so the comment above openWorkflow's closure, that each
// decision-class value "is read as the decision is made, from the settings holder", was a claim
// no test could fail. This test reds on that engine, and on a closure that snapshots the holder
// once instead of reading it.
func TestADesignGateSettledUnderTheRunningDaemonIsTheGateItsEngineDecidesBy(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servedBootCall(t, w, r, hookBootProject) {
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(board.Close)
	live := hookBootSettings(t, pool.Config().ConnString(), board.URL, "Say what the round missed.")
	boot := *live.Snapshot()
	if len(boot.Gates.Steps()) == 0 {
		t.Fatalf("the boot gates no step, want the default spec step: the change below would move nothing")
	}

	log := quietLogger()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	w, err := openWorkflow(ctx, boot, live, newRebuilder(log, live), st, "project-id", "dispatch-token", nil,
		newRepositories(boot.Project, boot.RepositoryRenames), log,
		loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"},
		repositoriesGitHub(t), nil)
	if err != nil {
		t.Fatalf("open the workflow: %v", err)
	}
	if got := w.engine.GatedSteps(); len(got) == 0 {
		t.Fatalf("the engine opened on %+v, want the boot's gated step", got)
	}

	setInConfig(t, live, "gates", "{design: []}")
	if _, _, err := live.Apply(config.SourceFile, "", map[string]any{}); err != nil {
		t.Fatalf("the file arm of the settings route: %v", err)
	}
	if steps := live.Snapshot().Gates.Steps(); len(steps) != 0 {
		t.Fatalf("the settled design gate = %+v, want the file's empty list: the change this test is about did not settle", steps)
	}

	if got := w.engine.GatedSteps(); len(got) != 0 {
		t.Fatalf("the running engine decides by %+v after the gate was settled empty, want no gated step: the engine is reading the configuration boot handed it, not the settings holder", got)
	}
}

// The admission the daemon opens decides each promotion by the admission cap the settings holder
// holds when it decides, not the one boot read: openWorkflow hands admit.NewReading a read of the
// holder. Three roots wait for a slot under a cap of 1, and the first reconcile admits one; the
// cap is settled to 3 through the file arm, and the next reconcile admits the other two.
//
// internal/admit's TestTheAdmissionCapChangedThroughTheSettingsHolderIsUsedByTheNextDecision
// holds the admission's own half — a read at every decision — on a read it builds itself, and
// TestAnAdmissionCapChangedThroughTheRouteIsReadByTheNextDecision in this package reads the cap
// from the daemon's state, which reads the holder directly. Neither runs the read openWorkflow
// hands admission, so a closure that snapshotted the cap at boot passed both, and every other
// test of this package that names admission, a cap or a slot. This test reds on that closure.
func TestAnAdmissionCapSettledUnderTheRunningDaemonIsTheCapItsAdmissionPromotesUnder(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servedBootCall(t, w, r, hookBootProject) {
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(board.Close)
	live := hookBootSettings(t, pool.Config().ConnString(), board.URL, "Say what the round missed.")
	setInConfig(t, live, "admission_cap", "1")
	if _, _, err := live.Apply(config.SourceFile, "", map[string]any{}); err != nil {
		t.Fatalf("settle the boot's cap of 1: %v", err)
	}
	boot := *live.Snapshot()
	if boot.AdmissionCap != 1 {
		t.Fatalf("the boot's admission cap = %d, want 1", boot.AdmissionCap)
	}

	log := quietLogger()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	w, err := openWorkflow(ctx, boot, live, newRebuilder(log, live), st, "project-id", "dispatch-token", nil,
		newRepositories(boot.Project, boot.RepositoryRenames), log,
		loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"},
		repositoriesGitHub(t), nil)
	if err != nil {
		t.Fatalf("open the workflow: %v", err)
	}

	var summaries []dispatch.IssueSummary
	for _, rank := range []string{"A", "B", "C"} {
		key := hookBootProject + "-" + rank
		seedTickIssue(t, w.pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: hookBootProject, Title: key, Tree: key, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: rank, HandedOver: true})
		summaries = append(summaries, dispatch.IssueSummary{Key: key, ResolvedRepo: "acme/widgets", Title: key, Status: "todo", Rank: rank, HandedOver: true})
	}
	slots := func() int {
		t.Helper()
		var n int
		if err := w.pool.QueryRow(ctx, "select count(*) from slots").Scan(&n); err != nil {
			t.Fatalf("count the slots: %v", err)
		}
		return n
	}

	reconcileAdmission(t, w.pool, w.admission, summaries, 1, intake.DispatchConsumerPosition{})
	if got := slots(); got != 1 {
		t.Fatalf("the first reconcile under a cap of 1 filled %d slots, want 1: the test's own premise does not hold", got)
	}

	setInConfig(t, live, "admission_cap", "3")
	if _, _, err := live.Apply(config.SourceFile, "", map[string]any{}); err != nil {
		t.Fatalf("the file arm of the settings route: %v", err)
	}
	if cap := live.Snapshot().AdmissionCap; cap != 3 {
		t.Fatalf("the settled admission cap = %d, want the file's 3: the change this test is about did not settle", cap)
	}

	reconcileAdmission(t, w.pool, w.admission, summaries, 1, intake.DispatchConsumerPosition{})
	if got := slots(); got != 3 {
		t.Fatalf("the reconcile after the cap was settled to 3 filled %d slots, want 3: the running admission is promoting under the cap boot handed it, not the settings holder", got)
	}
}

// hookBootProject is the project the hook boot above runs: its own database, so one name serves.
const hookBootProject = "HOOKS"

// hookBootSettings is the settings holder of a daemon configured for the workflow — a Dispatch at
// dispatchURL, the two GitHub Apps and its own project — whose one hook reads its words from a
// file beside the configuration, written here with words. The holder is file-backed, since the
// reload the test makes is the file arm reading that file again (liveSettings, which config
// alone cannot stand in for). `hooks:` is the file's last line, which is where setInConfig
// replaces it.
func hookBootSettings(t *testing.T, dsn, dispatchURL, words string) *config.Live {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"dispatch-token": "dispatch-token\n", "nudge.md": words + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	body := fmt.Sprintf(`project: %[1]s
postgres_dsn: %[2]s
state_dir: %[3]s
dispatch_url: %[4]s
dispatch_token_file: ./dispatch-token
nats_urls: [nats://127.0.0.1:4222]
admission_cap: 4
projects:
  %[1]s: {}
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
hooks: [{name: nudge, on: work_back, every: 1, roles: [implementer], prompt_file: nudge.md}]
`, hookBootProject, dsn, shortTempDir(t), dispatchURL)
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return live
}

// seedRetroIssue records key as a root of project standing in its retro, with the implementer's
// phase row the moves back are counted on (record.PhaseRow.BackwardMoves).
func seedRetroIssue(t *testing.T, pool *store.Pool, project, key string) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: key, Project: project, Title: "root",
			Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "U"}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implement-claim"})
	}); err != nil {
		t.Fatalf("seed %s in its retro: %v", key, err)
	}
}

// firedHooks is every firing the engine wrote to the outbox, oldest first, decoded as the runner
// decodes one before it publishes it (record.HookPrompt).
func firedHooks(t *testing.T, pool *store.Pool) []record.HookPrompt {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select payload from outbox where kind = 'hook_prompt' order by id")
	if err != nil {
		t.Fatalf("read the firings: %v", err)
	}
	defer rows.Close()
	fired := []record.HookPrompt{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a firing: %v", err)
		}
		prompt := record.HookPrompt{}
		if err := json.Unmarshal(payload, &prompt); err != nil {
			t.Fatalf("decode the firing %s: %v", payload, err)
		}
		fired = append(fired, prompt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the firings: %v", err)
	}
	return fired
}
