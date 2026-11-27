package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// A decimal production_check_hours reaches the engine as the duration it names
// (engineConfig): the bound of a production check's look is the file's, not a default.
func TestProductionCheckHoursReachesTheEngineAsItsDuration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	body := "project: DEMO\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nproduction_check_hours: 0.1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	if engine := engineConfig(cfg, "legion-reviewer[bot]", nil); engine.ProductionCheckBound != 6*time.Minute {
		t.Errorf("the engine's production check bound = %s, want 6m0s", engine.ProductionCheckBound)
	}
}

// The engine runs the default steps of its own project (`projects.<KEY>.default_steps`, read
// through config.Project.RunsByDefault): the steps an issue whose owner gives no plan stores at its
// first step. A project that names none runs today's line up to the owner's acceptance.
func TestTheProjectsDefaultStepsReachTheEngine(t *testing.T) {
	t.Parallel()
	todays := (config.Project{}).RunsByDefault()
	steps := append(todays[1:2:2], todays[3]) // implementer reviewer
	cfg := config.Config{Project: "DEMO", Projects: map[string]config.Project{"DEMO": {DefaultSteps: steps}, "ACME": {}}}
	if got := engineConfig(cfg, "legion-reviewer[bot]", nil).DefaultSteps; !reflect.DeepEqual(got, steps) {
		t.Fatalf("the engine's default steps = %+v, want DEMO's %+v", got, steps)
	}
	cfg.Project = "ACME"
	if got, want := engineConfig(cfg, "legion-reviewer[bot]", nil).DefaultSteps, (config.Project{}).RunsByDefault(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the engine's default steps for ACME, which names none = %+v, want today's line %+v", got, want)
	}
}

// loginTokens answers each App role's lease, and its login, with the bot login named for it.
type loginTokens map[appauth.AppRole]string

func (t loginTokens) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	return appauth.Lease{Token: "token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: t[role]}}, nil
}

func (t loginTokens) AppLogin(_ context.Context, role appauth.AppRole) (string, error) {
	return t[role], nil
}

func (loginTokens) InstalledOn(context.Context, appauth.AppRole, ghrepo.Repository) (bool, error) {
	return true, nil
}

func (loginTokens) InstalledUnder(context.Context, appauth.AppRole, ghrepo.Repository) (bool, error) {
	return true, nil
}

// InstalledOwners answers each App installed on no account, which the boot's proof of each App's
// key takes (proveAppKeys): opening the workflow builds no gh bundle, and a test that builds one
// names its owners itself.
func (loginTokens) InstalledOwners(context.Context, appauth.AppRole) ([]string, error) {
	return nil, nil
}

// Forget drops nothing: every lease is minted afresh.
func (loginTokens) Forget(appauth.AppRole, string) {}

func (t loginTokens) ReadToken(ctx context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	lease, err := t.Token(ctx, role, "")
	lease.Token += "-read"
	return lease, err
}

// The engine judges a push by its pusher against the review App's login, so a configuration whose
// two roles resolve to one App would read every red the implementer leaves as a planned one: boot
// refuses it, and a review App with no login, before anything else opens.
func TestOpenWorkflowRefusesAReviewLoginItCannotTellFromTheImplementers(t *testing.T) {
	t.Parallel()
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
			_, err := openWorkflow(context.Background(), cfg, nil, "project-id", dispatch.BearerTransport("dispatch-token", nil), nil, newRepositories(cfg.Project, cfg.RepositoryRenames), appauth.NewInstalled(), log, tokens, github, nil, nil, time.Now, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "the workflow needs two different Apps") {
				t.Fatalf("openWorkflow = %v, want the two-Apps refusal", err)
			}
		})
	}
}

// The boot reads the review App's bot login, the login the engine judges a push's pusher against,
// under the App's own JWT, with no owner to mint for.
func TestAppLoginsAtBootReturnsTheReviewAppLogin(t *testing.T) {
	t.Parallel()
	tokens := loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}
	login, err := appLoginsAtBoot(context.Background(), tokens, newStartWaits(productionAppCalls()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("appLoginsAtBoot: %v", err)
	}
	if login != "legion-reviewer[bot]" {
		t.Fatalf("appLoginsAtBoot = %q, want the review App's login %q", login, "legion-reviewer[bot]")
	}
}

// countedLogins answers each App's login with the bot login named for it and counts each App's
// reads; the review App's first read fails as GitHub's own trouble, as a viewer read GitHub
// answered 502 does.
type countedLogins map[appauth.AppRole]int

func (c countedLogins) AppLogin(_ context.Context, role appauth.AppRole) (string, error) {
	c[role]++
	if role == appauth.Review && c[role] == 1 {
		return "", &appauth.TransientError{Err: errors.New("read the review App's bot login (GraphQL viewer) failed (502)")}
	}
	return "legion-" + string(role) + "[bot]", nil
}

// An attempt of the logins read after one that failed on the review App reads the review App
// alone, as the key proof skips an App it proved: the implement App's login was read, and in
// token mode each read of an App runs its token_command, whose grant may wait on a person's
// approval.
func TestTheLoginsReadReadsNoAppAgainOnceItReadItsLogin(t *testing.T) {
	t.Parallel()
	reads := countedLogins{}
	login, err := appLoginsAtBoot(context.Background(), reads, newStartWaits(*quickAppMints()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if want := "legion-" + string(appauth.Review) + "[bot]"; err != nil || login != want {
		t.Fatalf("appLoginsAtBoot = %q, %v; want %q, the review App's login, after one transient failure", login, err, want)
	}
	if reads[appauth.Implement] != 1 || reads[appauth.Review] != 2 {
		t.Errorf("the logins read read the implement App %d times and the review App %d times, want 1 and 2: an attempt read an App whose login an attempt before it had read", reads[appauth.Implement], reads[appauth.Review])
	}
}

// Each wait the start decides for GitHub's rate limit on an App call (startWaits.wait), one row for
// each App call's row of the plan's table "One rate-limit refusal of a start's call", on the
// production policy and a clock of the test's own, so no row waits: GitHub's time with at most the
// maximum, or, where GitHub names none, a minute doubling for each further such refusal of that
// call, ending at the start's end, after which the call ends the start; and the total waited, the
// sum of the waits, each cut at that end. The start's calls run one after the other
// (openWorkflow), so no two waits of a start overlap.
func TestStartWaitsDecidesEachWait(t *testing.T) {
	t.Parallel()
	if want := (rateLimitPolicy{Max: time.Hour, Bound: 70 * time.Minute, NoTime: time.Minute}); productionAppCalls().rateLimits != want {
		t.Fatalf("the start's rate-limit policy = %+v, want R4's %+v", productionAppCalls().rateLimits, want)
	}
	start := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	retryAfter := func(seconds string) *githubrest.Answer {
		return &githubrest.Answer{Status: http.StatusTooManyRequests, RateLimited: true, Limit: githubrest.LimitHeaders{RetryAfter: seconds}}
	}
	spent := func(reset time.Duration) *githubrest.Answer {
		return &githubrest.Answer{Status: http.StatusForbidden, RateLimited: true,
			Limit: githubrest.LimitHeaders{Remaining: "0", Reset: strconv.FormatInt(start.Add(reset).Unix(), 10)}}
	}
	noHeader := &githubrest.Answer{Status: http.StatusTooManyRequests, RateLimited: true}
	bodyAlone := &githubrest.Answer{Status: http.StatusForbidden, Body: `{"message":"API rate limit exceeded for installation ID 7."}`,
		Limit: githubrest.LimitHeaders{Remaining: "14671"}}
	const proof, logins = "GitHub App key proof", "GitHub App logins read"
	// step is one refusal of an App call.
	type step struct {
		at     time.Duration // the clock, from the start
		call   string
		answer *githubrest.Answer
		until  time.Duration // the wait's end, from the start: at itself for no wait
		waited time.Duration
		ended  bool
	}
	for _, row := range []struct {
		name  string
		steps []step
	}{
		{name: "an App call's Retry-After at most the maximum", steps: []step{
			{call: proof, answer: retryAfter("30"), until: 30 * time.Second, waited: 30 * time.Second},
		}},
		{name: "an App call's Retry-After above the maximum", steps: []step{
			{call: proof, answer: retryAfter("7200"), until: time.Hour, waited: time.Hour},
		}},
		{name: "an App call's spent limit with its reset ahead", steps: []step{
			{call: proof, answer: spent(20 * time.Minute), until: 20 * time.Minute, waited: 20 * time.Minute},
		}},
		{name: "an App call that names no time, the n-th such refusal of the call", steps: []step{
			{call: proof, answer: noHeader, until: time.Minute, waited: time.Minute},
			{at: time.Minute, call: proof, answer: spent(-time.Minute), until: 3 * time.Minute, waited: 3 * time.Minute},
			{at: 3 * time.Minute, call: proof, answer: bodyAlone, until: 7 * time.Minute, waited: 7 * time.Minute},
		}},
		{name: "the first refusal that names no time of another call", steps: []step{
			{call: logins, answer: noHeader, until: time.Minute, waited: time.Minute},
			{at: time.Minute, call: logins, answer: noHeader, until: 3 * time.Minute, waited: 3 * time.Minute},
			{at: 3 * time.Minute, call: proof, answer: noHeader, until: 4 * time.Minute, waited: 4 * time.Minute},
		}},
		{name: "an App call whose wait passes the end", steps: []step{
			{call: proof, answer: retryAfter("1800"), until: 30 * time.Minute, waited: 30 * time.Minute},
			{at: 50 * time.Minute, call: proof, answer: retryAfter("3600"), until: 70 * time.Minute, waited: 50 * time.Minute},
		}},
		{name: "an App call at the end", steps: []step{
			{call: logins, answer: retryAfter("30"), until: 30 * time.Second, waited: 30 * time.Second},
			{at: 70 * time.Minute, call: proof, answer: retryAfter("30"), waited: 30 * time.Second, ended: true},
		}},
		{name: "an App call past the end", steps: []step{
			{call: logins, answer: retryAfter("30"), until: 30 * time.Second, waited: 30 * time.Second},
			{at: 75 * time.Minute, call: proof, answer: noHeader, waited: 30 * time.Second, ended: true},
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := &setClock{now: start}
			calls := productionAppCalls()
			calls.clock = clock
			waits := newStartWaits(calls)
			for index, step := range row.steps {
				clock.now = start.Add(step.at)
				until, waited, ended := waits.wait(step.call, step.answer)
				if ended != step.ended || waited != step.waited || !step.ended && !until.Equal(start.Add(step.until)) {
					t.Fatalf("step %d: until %s, waited %s, ended %v; want until %s, waited %s, ended %v",
						index+1, until.Sub(start), waited, ended, step.until, step.waited, step.ended)
				}
			}
		})
	}
}

// setClock is a clock a test sets by hand (now), for a start's waits read with no sleep between
// them; its Sleep is never reached.
type setClock struct{ now time.Time }

func (c *setClock) Now() time.Time { return c.now }

func (c *setClock) Sleep(context.Context, time.Duration) error {
	panic("setClock: nothing sleeps on a clock set by hand")
}
