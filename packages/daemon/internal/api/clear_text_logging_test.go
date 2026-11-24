package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// plantedToken is the made-up credential every row's GitHub call carries: the installation token
// the App's token_command releases for acme (releasingManager), so the Authorization of the
// identity reads appauth makes of it and of the route's own read carries it. It is redacttest's
// Token, one of the secrets oauth2calltest.RequireNone looks for.
const plantedToken = "not-a-secret-token"

// sinkAppID is the App id every row's App is configured with and the stand-in answers for the
// implement App (echoingGitHub).
const sinkAppID = "12345"

// completingIssue is the issue every completion row completes: the package's fixture root
// (statusRoot), recorded on acme/widgets.
const completingIssue = statusRoot

// unreadPullRequestLine is the message of the line an implementer's completion logs when its read
// of GitHub fails (implementationPullRequest's logUnread).
const unreadPullRequestLine = "api: read the open pull request on a completing implementer's branch; the completion is applied unread, and the per-issue read records the pull request it finds"

// controllerMintLine is the message the controller's credential route logs a failed mint under
// (controllerGitHubCredential).
const controllerMintLine = "api: mint the controller's GitHub App tokens"

// echoingGitHub is GitHub as every row's calls reach it, its REST root and its GraphQL root both:
// the identity reads appauth's token mode makes of a released token (the viewer, the App, its bot
// user), answered as GitHub answers them for the implement App, and nothing else. The call to the
// path refuse is refused instead (refuseEchoing), and the Authorization each such call carried is
// kept in refused.
type echoingGitHub struct {
	*httptest.Server
	refuse  string
	mu      sync.Mutex
	refused []string
}

func newEchoingGitHub(t *testing.T, refuse string) *echoingGitHub {
	t.Helper()
	g := &echoingGitHub{refuse: refuse}
	g.Server = githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == g.refuse:
			g.mu.Lock()
			g.refused = append(g.refused, r.Header.Get("Authorization"))
			g.mu.Unlock()
			refuseEchoing(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			fmt.Fprint(w, `{"data":{"viewer":{"login":"legion-implementer[bot]"}}}`)
		case r.URL.Path == "/apps/legion-implementer":
			fmt.Fprintf(w, `{"id":%s}`, sinkAppID)
		case r.URL.Path == "/users/legion-implementer[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	return g
}

// refusedCalls is the Authorization of each call the stand-in refused, in order.
func (g *echoingGitHub) refusedCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.refused)
}

// refuseEchoing answers r with GitHub's rate limit, a 403 with X-RateLimit-Remaining 0, as
// something in GitHub's place may write it: the Authorization r carried, as it came and outside
// any URL, is the limit's resource, its Retry-After and the words of the body, so it reaches the
// answer's every kept field that is not a number.
func refuseEchoing(w http.ResponseWriter, r *http.Request) {
	echoed := r.Header.Get("Authorization")
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", "1795000000")
	w.Header().Set("X-RateLimit-Resource", echoed)
	w.Header().Set("Retry-After", echoed)
	w.WriteHeader(http.StatusForbidden)
	body, _ := json.Marshal(map[string]string{"message": "API rate limit exceeded for " + echoed})
	_, _ = w.Write(body)
}

// releasingManager is the daemon's token manager (appauth.New) with role's App in token mode,
// reading GitHub at github: its token_command releases plantedToken as acme's installation token,
// so every lease of acme is minted, and checked, through the stand-in.
func releasingManager(t *testing.T, github *echoingGitHub, role appauth.AppRole) *appauth.Manager {
	t.Helper()
	release := filepath.Join(t.TempDir(), "release.json")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(release, fmt.Appendf(nil, `{"installations":{"acme":{"installation_id":7,"token":%q,"expires_at":%q}}}`,
		plantedToken, expires), 0o600); err != nil {
		t.Fatal(err)
	}
	app := config.GitHubApp{AppID: sinkAppID, TokenCommand: "cat '" + release + "'"}
	var apps config.GitHubApps
	if role == appauth.Review {
		apps.Review = app
	} else {
		apps.Implement = app
	}
	tokens := appauth.New(apps, appauth.Options{BaseURL: github.URL, HTTPClient: github.Client(), Stderr: io.Discard,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(tokens.Close)
	return tokens
}

// endingTokens is the token manager whose read lease, refused, ends the request it is minted for:
// the client hangs up (hangUp, the request's own cancel) as GitHub's refusal arrives, and the mint
// fails with the refusal and the request's ending, joined.
type endingTokens struct {
	*appauth.Manager
	hangUp context.CancelFunc
}

func (t endingTokens) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.Manager.ReadToken(ctx, role, owner)
	if err == nil {
		return lease, nil
	}
	t.hangUp()
	return appauth.Lease{}, fmt.Errorf("%w: %w", err, ctx.Err())
}

// keptLog is the daemon's log as cmd/legion's start builds it, slog's JSON handler under
// redact.HandlerOptions, writing into kept, which also keeps each record as the call made it, so
// a row holds the value a sink logged beside the line the sink wrote.
type keptLog struct {
	slog.Handler
	kept *keptRecords
}

// keptRecords is what a keptLog wrote and kept.
type keptRecords struct {
	mu      sync.Mutex
	out     bytes.Buffer
	records []slog.Record
}

// newKeptLog is a logger built as the daemon builds its own, and what it writes and keeps.
func newKeptLog() (*slog.Logger, *keptRecords) {
	kept := &keptRecords{}
	return slog.New(keptLog{Handler: slog.NewJSONHandler(kept, redact.HandlerOptions(nil)), kept: kept}), kept
}

func (k *keptRecords) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.out.Write(p)
}

func (l keptLog) Handle(ctx context.Context, r slog.Record) error {
	l.kept.mu.Lock()
	l.kept.records = append(l.kept.records, r.Clone())
	l.kept.mu.Unlock()
	return l.Handler.Handle(ctx, r)
}

func (l keptLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return keptLog{Handler: l.Handler.WithAttrs(attrs), kept: l.kept}
}

func (l keptLog) WithGroup(name string) slog.Handler {
	return keptLog{Handler: l.Handler.WithGroup(name), kept: l.kept}
}

// line is the one line written whose message is msg, which must be at level: the line as written,
// its fields, and the value the call handed slog under key.
func (k *keptRecords) line(t *testing.T, level slog.Level, msg, key string) (string, map[string]any, any) {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	var lines []string
	var fields []map[string]any
	for _, line := range strings.Split(k.out.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the log holds %q, which is not one JSON record: %v", line, err)
		}
		if record["msg"] == msg {
			lines, fields = append(lines, line), append(fields, record)
		}
	}
	var values []any
	for _, r := range k.records {
		if r.Message != msg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				values = append(values, a.Value.Any())
			}
			return true
		})
	}
	if len(lines) != 1 || len(values) != 1 {
		t.Fatalf("the log holds %d lines of %q and %d values under %s, want one of each; log:\n%s", len(lines), msg, len(values), key, k.out.String())
	}
	if fields[0]["level"] != level.String() {
		t.Errorf("the line is at %v, want %s: %s", fields[0]["level"], level, lines[0])
	}
	return lines[0], fields[0], values[0]
}

// loggedError is v, the value a sink logged under "error", as the error it must be.
func loggedError(t *testing.T, v any) error {
	t.Helper()
	err, ok := v.(error)
	if !ok || err == nil {
		t.Fatalf("the line's error was handed slog as %T %v, want an error", v, v)
	}
	return err
}

// appauthAnswer is GitHub's answer an error of appauth carries (appauth.AnswerError).
func appauthAnswer(t *testing.T, logged error) *githubrest.Answer {
	t.Helper()
	var answered *appauth.AnswerError
	if !errors.As(logged, &answered) || answered.Answer == nil {
		t.Fatalf("the logged error %q carries no GitHub answer of appauth's", logged)
	}
	return answered.Answer
}

// githubrestAnswer is GitHub's answer a read of githubrest's client failed with.
func githubrestAnswer(t *testing.T, logged error) *githubrest.Answer {
	t.Helper()
	var answer *githubrest.Answer
	if !errors.As(logged, &answer) {
		t.Fatalf("the logged error %q is no GitHub answer of githubrest's", logged)
	}
	return answer
}

// requireEchoKept fails t unless answer is the stand-in's refusal with the echo in its kept
// fields as the rule prints them: the Authorization the call carried, redacted, where the stand-in
// wrote it. A row whose stand-in echoed nothing would hold none of the credential however its
// printers read, so this is what says each row's check could see one.
func requireEchoKept(t *testing.T, answer *githubrest.Answer) {
	t.Helper()
	if answer.Status != http.StatusForbidden || !answer.RateLimited {
		t.Errorf("the answer is %d, rate limited %t; want the stand-in's 403 rate limit", answer.Status, answer.RateLimited)
	}
	for _, kept := range []struct{ field, text string }{
		{"Limit.Resource", answer.Limit.Resource}, {"Limit.RetryAfter", answer.Limit.RetryAfter}, {"Body", answer.Body},
	} {
		if !strings.Contains(kept.text, "[redacted]") {
			t.Errorf("the answer's %s is %q, want the echoed Authorization in it, redacted", kept.field, kept.text)
		}
	}
}

// completionServer is newWorkflowHandoffHarness's server with tokens as its token source, GitHub
// at github and log as its logger.
func completionServer(t *testing.T, github string, tokens appauth.Source, log *slog.Logger) *harness {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handoffs = &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: h.handoffs, Log: log,
		Tokens: tokens, GitHubAPI: github,
	}).Handler
	return h
}

// controllerCredentialServer is newControllerCredentialHarness's server, its controller left to
// the operator, with log as its logger and stopping as the daemon's stop (Options.Stopping).
func controllerCredentialServer(t *testing.T, tokens appauth.Source, log *slog.Logger, stopping context.Context) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil),
		Tokens: tokens, Served: servesOneOf(ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("acme/widgets")),
		Log: log, Stopping: stopping,
	}).Handler
	return h
}

// TestACompletionsUnreadPullRequestLineHoldsNoCredential: an implementer's completion reads its
// branch's open pull request as the implement App (implementationPullRequest), and GitHub, or
// whatever answers in its place, refuses with its rate limit, echoing the call's Authorization,
// which carries plantedToken, as the limit's resource, its Retry-After and the words of its body
// (refuseEchoing). Refused at the mint (token mode's viewer read of the released token) or at the
// read of the pull requests, the completion is applied unread and the line logUnread writes at
// WARN names the failure. The echo sits in GitHub's answer the logged error carries
// (githubrest.Answer's Body and Limit, read through oauth2call.Answer.Words and oauth2call.Header,
// whose RemoteText keeps the credentials the request carried in its carried field), and the box's
// printer subtracts them (RemoteText.String); the line holds none of the token. The mint's error
// prints appauth's own message (appauth.AnswerError.Error), its answer a field it never prints;
// the read's error prints the answer (githubrest.Answer.Error). Each row holds the logged error to
// that answer and the answer to the echo (requireEchoKept), and prints the logged error and that
// answer under every fmt verb and a bad verb, at every holding, beside the echo as a plain string
// (oauth2calltest.KeptPrintTest). Captures sink/api/handoff.go/implementationPullRequest/<call>,
// and kept/api/handoff.go/implementationPullRequest/<call>/{error,answer}/<holding>/<verb>.
func TestACompletionsUnreadPullRequestLineHoldsNoCredential(t *testing.T) {
	for _, row := range []struct {
		name, capture string
		// refuse is the path of the call GitHub refuses, and authorization the Authorization it
		// carries.
		refuse, authorization string
		// names is a part of the line's error.
		names string
		// answer is GitHub's answer the logged error carries.
		answer func(t *testing.T, logged error) *githubrest.Answer
	}{
		{
			name:    "handoff.go:implementationPullRequest api: read the open pull request, the implement App token's mint",
			capture: "sink/api/handoff.go/implementationPullRequest/mint",
			refuse:  "/graphql", authorization: "token " + plantedToken,
			names:  "mint the implement App token for acme/widgets: read the implement App's bot login (GraphQL viewer) failed (403)",
			answer: appauthAnswer,
		},
		{
			name:    "handoff.go:implementationPullRequest api: read the open pull request, the pull request read",
			capture: "sink/api/handoff.go/implementationPullRequest/read",
			refuse:  "/repos/acme/widgets/pulls", authorization: "Bearer " + plantedToken,
			names:  "GitHub answered GET /pulls with 403",
			answer: githubrestAnswer,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			github := newEchoingGitHub(t, row.refuse)
			log, kept := newKeptLog()
			h := completionServer(t, github.URL, releasingManager(t, github, appauth.Implement), log)
			seedIssueAt(t, h, completingIssue, phase.Implementing)
			implementer := newLiveClaim(t, h, completingIssue, claim.RoleImplementer)
			written := implementer.writeHandoff("implement", 1)

			completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA, HandoffID: written, HandoffVersion: 1}))

			if calls := github.refusedCalls(); !slices.Equal(calls, []string{row.authorization}) {
				t.Fatalf("GitHub refused calls carrying %q, want one carrying %q", calls, row.authorization)
			}
			line, fields, value := kept.line(t, slog.LevelWarn, unreadPullRequestLine, "error")
			oauth2calltest.RequireNone(t, row.capture, line)
			if said, _ := fields["error"].(string); !strings.Contains(said, row.names) {
				t.Errorf("the line's error is %q, want %q in it", said, row.names)
			}
			logged := loggedError(t, value)
			answer := row.answer(t, logged)
			requireEchoKept(t, answer)
			name := strings.TrimPrefix(row.capture, "sink/")
			oauth2calltest.KeptPrintTest(t, name+"/error", logged, row.authorization)
			oauth2calltest.KeptPrintTest(t, name+"/answer", answer, row.authorization)
		})
	}
}

// TestTheControllerCredentialsFailureLinesHoldNoCredential: the controller's GitHub credential
// route (controllerGitHubCredential) mints the review App's read lease of acme through the
// daemon's token manager in token mode, and GitHub, or whatever answers in its place, refuses the
// viewer read of the released token with its rate limit, echoing the read's Authorization, which
// carries plantedToken, as the limit's resource, its Retry-After and the words of its body
// (refuseEchoing). The route answers GITHUB_TOKEN_FAILED and logs the mint's error through
// logRequestFailure: at Error on a running daemon and at Info once its stop has begun
// (logFailure), and at Warn where the request's own context ended and the error is that ending.
// The echo sits in the kept fields of the answer appauth's error carries (appauth.AnswerError's
// githubrest.Answer, Body and Limit, read through RemoteText, which keeps the credentials the
// request carried in its carried field), and the box's printer subtracts them
// (RemoteText.String); the line prints appauth's own message (appauth.AnswerError.Error) and holds
// none of the token. Each row holds the logged error to that answer and the answer to the echo
// (requireEchoKept), and prints the logged error and that answer under every fmt verb and a bad
// verb, at every holding, beside the echo as a plain string (oauth2calltest.KeptPrintTest).
//
// No error of appauth's is a refusal and the request's ending at once: a refusal that arrives as
// the request ends is GitHub's answer alone, which logs at Error. So the Warn row's token source
// (endingTokens) hangs the request up as the manager's real refusal arrives and returns the two
// joined, the one value that reaches that branch carrying GitHub's answer. Captures
// sink/api/server.go/<function>[/<level>], and
// kept/api/server.go/<function>[/<level>]/{error,answer}/<holding>/<verb>.
func TestTheControllerCredentialsFailureLinesHoldNoCredential(t *testing.T) {
	for _, row := range []struct {
		name, capture string
		level         slog.Level
		// stopping begins the daemon's stop before the request; endRequest ends the request
		// itself as GitHub refuses (endingTokens).
		stopping, endRequest bool
	}{
		{
			name:    "server.go:logFailure api: mint the controller's GitHub App tokens, at Error",
			capture: "sink/api/server.go/logFailure/error",
			level:   slog.LevelError,
		},
		{
			name:    "server.go:logFailure api: mint the controller's GitHub App tokens, at Info once the stop has begun",
			capture: "sink/api/server.go/logFailure/info",
			level:   slog.LevelInfo, stopping: true,
		},
		{
			name:    "server.go:logRequestFailure api: mint the controller's GitHub App tokens, at Warn once the request has ended",
			capture: "sink/api/server.go/logRequestFailure",
			level:   slog.LevelWarn, endRequest: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			github := newEchoingGitHub(t, "/graphql")
			manager := releasingManager(t, github, appauth.Review)
			requestContext, hangUp := context.WithCancel(context.Background())
			defer hangUp()
			var tokens appauth.Source = manager
			if row.endRequest {
				tokens = endingTokens{Manager: manager, hangUp: hangUp}
			}
			stopping, beginStop := context.WithCancel(context.Background())
			defer beginStop()
			log, kept := newKeptLog()
			h := controllerCredentialServer(t, tokens, log, stopping)
			secret := h.mintedSecret()
			if row.stopping {
				beginStop()
			}

			body, err := json.Marshal(ControllerCredentialRequest{Secret: secret})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/legion/v1/controller/github-credential", bytes.NewReader(body)).WithContext(requestContext)
			recorder := httptest.NewRecorder()
			h.handler.ServeHTTP(recorder, request)
			refused(t, recorder, http.StatusBadGateway, "GITHUB_TOKEN_FAILED")

			if calls := github.refusedCalls(); !slices.Equal(calls, []string{"token " + plantedToken}) {
				t.Fatalf("GitHub refused calls carrying %q, want one carrying the released token", calls)
			}
			line, fields, value := kept.line(t, row.level, controllerMintLine, "error")
			oauth2calltest.RequireNone(t, row.capture, line)
			const names = "mint the read-only review App lease for controller on acme: read the review App's bot login (GraphQL viewer) failed (403)"
			if said, _ := fields["error"].(string); !strings.Contains(said, names) {
				t.Errorf("the line's error is %q, want %q in it", said, names)
			}
			logged := loggedError(t, value)
			if ended := errors.Is(logged, context.Canceled); ended != row.endRequest {
				t.Errorf("the logged error is the request's own ending: %t, want %t", ended, row.endRequest)
			}
			answer := appauthAnswer(t, logged)
			requireEchoKept(t, answer)
			name := strings.TrimPrefix(row.capture, "sink/")
			oauth2calltest.KeptPrintTest(t, name+"/error", logged, "token "+plantedToken)
			oauth2calltest.KeptPrintTest(t, name+"/answer", answer, "token "+plantedToken)
		})
	}
}
