package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testport"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// leakCheckRedirectToken is the token the stand-in GitHub's redirect carries in its query, of no
// real token's shape.
const leakCheckRedirectToken = "plantedRedirectToken4k"

// GitHub answers the daemon's read of the shared public leak check with a redirect to its download
// host, whose URL carries a token in its query. Here that host is a port the test holds bound and
// never listening (testport.Refused), so the read's dial is refused, no other program can take the
// port and answer it, and Go's client names the URL, query and all, in its error. The daemon's own
// reader (leakCheckSource) refuses with a cause sentence, the start is refused with it as the
// Sandbox runtime refuses one (runtime.LeakCheckRefusal, launchers.serveLeakCheck), and the
// claim's launch budget runs out on it. The token is in none of: the stored claim (its
// leak_check_cause, last_start_failure and failure among its columns), the claim's view as the
// state serves it (api.ClaimViewOf), and the lines the daemon's logger wrote through a JSON handler
// built as `legion start` builds it (redact.HandlerOptions).
func TestALeakCheckRedirectTokenReachesNoClaimStoreStateOrLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	github := provisioningStandIn(t, provisioningInstallation{id: "78", owner: leakCheckSetting.Repository.Owner(), granted: leakCheckGrant,
		answer: func(w http.ResponseWriter, permissions map[string]string) {
			mintedToken(w, leakCheckLease, permissions)
		}})
	var download string
	repo := githubtest.Server(t, githubtest.Spec{Token: leakCheckLease, Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != leakCheckTarballRoute+leakCheckTag {
			githubtest.Unexpected(t, w, r)
			return
		}
		http.Redirect(w, r, download+"?token="+leakCheckRedirectToken, http.StatusFound)
	}})
	dead := testport.Refused(t)
	download = "http://" + dead + "/" + leakCheckSetting.Repository.String() + "/legacy.tar.gz/refs/tags/" + leakCheckTag
	_, readErr := leakCheckSource(github.tokens, repo, leakCheckSetting).Get(ctx, leakCheckTag)
	if readErr == nil {
		t.Fatalf("the read redirected to %s was served, want it refused", dead)
	}
	cause := leakguard.RefusalPrefix + "the daemon could not read " + leakCheckSetting.Repository.String() + " at " + leakCheckTag + `: Get "` + download + `": `
	if !strings.HasPrefix(readErr.Error(), cause) {
		t.Fatalf("the read is refused with %q, want a cause beginning %q", readErr, cause)
	}
	refusal := fmt.Errorf("start through its authenticated launcher: %w",
		runtime.LeakCheckRefusal{Cause: leakguard.Cause(readErr.Error(), nil)})

	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	const issue = "LEGION-209"
	if _, err := st.OpenTreeLifecycle(ctx, "legion", issue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", issue, err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, redact.HandlerOptions(nil)))
	rt := fake.NewRuntime()
	sup := newSupervisor(ctx, st, "legion", t.TempDir(), logger)
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: st, Specs: outboxSpecs{}, Clock: stillClock{}, Log: logger,
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: 24,
		RefreshGate:    supervise.NewRefreshGate(0),
	}
	t.Cleanup(sup.stop)
	token := mustClaimToken(t, issue, claim.RoleImplementer)
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue, Issue: issue,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	refused := fake.SpawnResult{Err: refusal}
	rt.ScriptSpawn(refused, refused)

	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err == nil {
		t.Fatalf("the starts the leak check refused answered no error")
	}
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("%s is %s, want it failed: its launch budget ran out on the refusals", token, got)
	}

	var leakCheckCause, lastStartFailure, failure, row string
	if err := pool.QueryRow(ctx, `select leak_check_cause, last_start_failure, failure, row_to_json(c)::text from claims c where token = $1`,
		string(token)).Scan(&leakCheckCause, &lastStartFailure, &failure, &row); err != nil {
		t.Fatalf("read the stored claim: %v", err)
	}
	if !strings.HasPrefix(leakCheckCause, cause) {
		t.Errorf("the stored leak_check_cause is %q, want the read's cause, beginning %q", leakCheckCause, cause)
	}
	for column, text := range map[string]string{"last_start_failure": lastStartFailure, "failure": failure} {
		if !strings.Contains(text, cause) {
			t.Errorf("the stored %s is %q, want it to carry the read's cause %q", column, text, cause)
		}
	}
	if strings.Contains(row, leakCheckRedirectToken) {
		t.Errorf("the stored claim holds the planted token: %s", row)
	}

	claims, err := st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for _, c := range append(claims, machine.Claim()) {
		if c.Token != token {
			continue
		}
		view := api.ClaimViewOf(c)
		if view.LeakCheckCause != leakCheckCause {
			t.Errorf("the claim's view carries the leak-check cause %q, want the stored %q", view.LeakCheckCause, leakCheckCause)
		}
		served, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("encode the claim's view: %v", err)
		}
		if strings.Contains(string(served), leakCheckRedirectToken) {
			t.Errorf("the claim's view holds the planted token: %s", served)
		}
	}

	logged := logs.String()
	for _, line := range []string{`"msg":"supervise: launch failed"`, `"msg":"supervise: claim failed"`, "the daemon could not read " + leakCheckSetting.Repository.String()} {
		if !strings.Contains(logged, line) {
			t.Errorf("the log holds no %s; want the failure's lines with the read's cause:\n%s", line, logged)
		}
	}
	if strings.Contains(logged, leakCheckRedirectToken) {
		t.Errorf("the log holds the planted token:\n%s", logged)
	}
}
