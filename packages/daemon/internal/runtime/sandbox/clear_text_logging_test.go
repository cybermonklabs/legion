package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// plantedToken is the made-up credential the mint rows plant in the request whose answer their
// sink's error is built from, and that the cacheStreams row's stand-in quotes in its own words
// (oauth2calltest.Reason holds it): one of the secrets oauth2calltest.RequireNone looks for
// (redacttest's Token, one of its Secrets).
const plantedToken = "not-a-secret-token"

// TestTheSandboxRuntimesWarningsOfARefusedAnswerHoldNoCredentialTheRequestCarried: each warning of
// the Sandbox runtime whose error is built from another party's answer is driven through the
// runtime's own function that logs it, with a stand-in in the far side's place whose answer
// carries a credential outside any URL of the request. The line the daemon's own handler writes
// (redact.HandlerOptions, as cmd/legion builds the log) and the text of the error the sink logged,
// which the row keeps as the call passed it (logged), hold none of it. Each row checks that the
// stand-in was asked and that each text it wrote holding a credential held plantedToken, that the
// line's error is logged's text as the handler writes it, and that neither the line nor logged's
// text holds a credential (oauth2calltest.RequireNone), and prints logged, and for a refused mint
// the GitHub answer it keeps on appauth.AnswerError, under every fmt verb and a bad verb, at every
// holding, beside the first text the stand-in wrote as a plain string
// (oauth2calltest.KeptPrintTest). Captures sink/sandbox/<file>/<function>, and
// kept/sandbox/<file>/<function>/{error,answer}/<holding>/<verb>.
//
//   - refreshGitHubCredentials: the walk's mint of an issue pod's provisioning lease, through the
//     daemon's token source over a real token manager in token mode (mintedTokens, as
//     internal/daemon's implementTokens): the command releases plantedToken, and GitHub's stand-in
//     refuses the identity read every release is checked by (appauth's viewerLogin) with a rate
//     limit whose body and every rate-limit header written as text echo its Authorization. The
//     echo sits in the kept field of the answer's words (RemoteText.carried: githubrest's
//     limitHeaders reads each header through oauth2call.Header into Answer.Limit, and the body
//     through Words into Answer.Body), carried on appauth.AnswerError. The sink's error prints
//     none of the answer, since an AnswerError prints its Err alone, and every printer of the
//     answer's words subtracts what the request carried (RemoteText.String and Format,
//     redact.Tokens).
//   - cacheStreams: the cache's /healthz read before a drifted cache pod is replaced
//     (EnsureRepositoryCache, reconcileCachePod). No input a test can set puts a credential in that
//     request but its address: cacheHealthOf builds GET <address>/healthz and sets no header, and
//     its client (cacheHealthClient) adds none, uses no proxy, follows no redirect and is no
//     RefusalClient, so the answer's record is that one request. The address is
//     http://<pod IP>:8418 of the pod as the store holds it, which every error of the read prints
//     by design, so a credential planted there (a pod IP written with user information, which
//     net/http sends as a Basic pair) would sit in a field printed by design and not in the box.
//     So the row plants nothing, and its stand-in fails the row where it is asked with a
//     credential; nothing is subtracted, and the row proves the box's other half instead. The
//     cache's stand-in names the repository with words that quote a URL carrying made-up
//     credentials (oauth2calltest.Reason), which cacheHealthOf prints with %q through
//     oauth2call.Header. The row checks that the error is the sink's sentence with the rule's
//     reading of those words in it and nothing else, and that the header as it came (the twin)
//     and the sentence printed with it hold each credential. The words quote a credential in a
//     URL alone: one outside a URL prints as it came here, since the request carried nothing for
//     the box to subtract. The daemon's handler removes a URL's user information and query as well
//     (redact.URLCredentials), so the line would hold none of Reason's credentials even were the
//     header printed as it came: the error's own text is the half of the check that can fail here.
//   - refreshCacheTokens: writeCacheSecret's mint of a stored cache's token, refused as the first.
//   - sweepCaches: EnsureRepositoryCache's error through writeCacheSecret's mint, refused as the
//     first, from ReconcileOrphans as the boot's reconciliation runs it.
func TestTheSandboxRuntimesWarningsOfARefusedAnswerHoldNoCredentialTheRequestCarried(t *testing.T) {
	for _, row := range []struct {
		name, capture string
		// message is the sink's warning, and key the attribute it logs its error under.
		message, key string
		// drive runs the sink's function, the runtime logging to log, and answers the repository
		// the warning names and what the stand-in wrote that holds a credential.
		drive func(t *testing.T, log *slog.Logger) (ghrepo.Repository, *echoes)
		// check, when set, is the row's own checks of the error its sink logged.
		check func(t *testing.T, logged error)
		// mint is whether the sink's error is a refused mint's, which keeps GitHub's answer on
		// appauth.AnswerError.
		mint bool
	}{
		{
			name:    "ghcredential.go:refreshGitHubCredentials provisioning token mint failed",
			capture: "sink/sandbox/ghcredential.go/refreshGitHubCredentials",
			message: "sandbox runtime: provisioning token mint failed", key: "err", mint: true,
			drive: func(t *testing.T, log *slog.Logger) (ghrepo.Repository, *echoes) {
				repo := rootSpec(t).Repository
				tokens, echoed := plantedApps(t, repo.Owner())
				g := newRig(t, nil, withOptions(func(o *Options) { o.Tokens, o.Log = tokens, log }))
				g.spawn(rootSpec(t))
				g.eventually("the issue pod running in the store", func() bool {
					pod := g.r.storedPod(SandboxName(rootToken))
					return pod != nil && pod.Status.Phase == corev1.PodRunning
				})
				tokens.minting.Store(true)
				g.r.refreshGitHubCredentials(g.ctx)
				return repo, echoed
			},
		},
		{
			name:    "repocache.go:cacheStreams the repository cache's streams could not be read",
			capture: "sink/sandbox/repocache.go/cacheStreams",
			message: "sandbox runtime: the repository cache's streams could not be read before its replacement", key: "error",
			drive: func(t *testing.T, log *slog.Logger) (ghrepo.Repository, *echoes) {
				echoed := &echoes{}
				var credentialed atomic.Int64
				health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
						if r.Header.Get(name) != "" {
							credentialed.Add(1)
						}
					}
					echoed.add(oauth2calltest.Reason)
					w.Header().Set(workspace.CacheRepositoryHeader, oauth2calltest.Reason)
					_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: cacheRepo.String()})
				}))
				t.Cleanup(health.Close)
				live := cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodRunning)
				live.Status.PodIP = "10.0.0.5"
				g := newRig(t, []k8sruntime.Object{reserveCacheCPU(t, cacheObject(t, cacheRepo, "uid-cache", modeRunning, ""), "7"), live},
					withOptions(func(o *Options) { o.Log = log }))
				g.r.cacheDial = (&podNetwork{routes: map[string]string{"10.0.0.5:8418": health.Listener.Addr().String()}}).dial
				if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
					t.Fatalf("EnsureRepositoryCache: %v", err)
				}
				if n := credentialed.Load(); n != 0 {
					t.Errorf("the cache's stand-in was asked with %d credential headers, want none: the read carries no credential", n)
				}
				return cacheRepo, echoed
			},
			check: func(t *testing.T, logged error) {
				// The header box under the sink's %q: the rule's reading of the stand-in's words,
				// with nothing the request carried to subtract first, as
				// TestACacheHealthNamesItsStatusAndRepositoryAsTheRuleReadsThem reads them too.
				shown := fmt.Sprintf("%q", oauth2calltest.Said(oauth2calltest.Reason))
				if want := `"see https://issuer.example/code?access_token="`; shown != want {
					t.Errorf("the rule reads the stand-in's words as %s, want %s", shown, want)
				}
				if want := fmt.Sprintf("GET http://10.0.0.5:8418/healthz answered %s naming the repository %s, not %s",
					oauth2call.StatusLine(http.StatusOK), shown, cacheRepo); logged.Error() != want {
					t.Errorf("the sink's error is %q, want the sink's sentence with the header as the rule reads it, %q", logged, want)
				}
				// The twin: the header as the stand-in wrote it, a plain string, and the sentence
				// printed with it as cacheHealthOf would print the header as it came.
				twin := oauth2calltest.Reason
				asCame := fmt.Sprintf("GET http://10.0.0.5:8418/healthz answered %s naming the repository %q, not %s",
					oauth2call.StatusLine(http.StatusOK), twin, cacheRepo)
				t.Logf("the sentence printed with the twin: %s", asCame)
				t.Logf("that sentence as the daemon's handler writes it: %s", redact.URLCredentials(asCame))
				for _, text := range []string{twin, asCame} {
					for _, secret := range []string{"not-a-secret-user", "not-a-secret-password", plantedToken} {
						if !strings.Contains(text, secret) {
							t.Errorf("%q holds no %q: the check would not see the header printed as it came", text, secret)
						}
					}
				}
			},
		},
		{
			name:    "repocache.go:refreshCacheTokens repository cache token refresh failed",
			capture: "sink/sandbox/repocache.go/refreshCacheTokens",
			message: "sandbox runtime: repository cache token refresh failed", key: "err", mint: true,
			drive: func(t *testing.T, log *slog.Logger) (ghrepo.Repository, *echoes) {
				tokens, echoed := plantedApps(t, cacheRepo.Owner())
				tokens.minting.Store(true)
				g := newRig(t, []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, "")},
					withOptions(func(o *Options) { o.Tokens, o.Log = tokens, log }))
				g.eventually("the cache Sandbox in the store", func() bool { return len(g.r.storedCaches()) == 1 })
				g.r.refreshCacheTokens(g.ctx)
				return cacheRepo, echoed
			},
		},
		{
			name:    "repocache.go:sweepCaches repository cache sweep failed",
			capture: "sink/sandbox/repocache.go/sweepCaches",
			message: "sandbox runtime: repository cache sweep failed; tried at the next sweep", key: "err", mint: true,
			drive: func(t *testing.T, log *slog.Logger) (ghrepo.Repository, *echoes) {
				tokens, echoed := plantedApps(t, cacheRepo.Owner())
				tokens.minting.Store(true)
				g := newRig(t, nil, withOptions(func(o *Options) {
					o.Tokens, o.Log = tokens, log
					o.CacheRepositories = func() []ghrepo.Repository { return []ghrepo.Repository{cacheRepo} }
				}))
				if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
					t.Fatalf("ReconcileOrphans: %v", err)
				}
				return cacheRepo, echoed
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			buffer := &logBuffer{}
			records := &keptRecords{}
			log := slog.New(keptLog{Handler: slog.NewJSONHandler(buffer, redact.HandlerOptions(nil)), records: records})

			repo, echoed := row.drive(t, log)

			sent := echoed.all()
			if len(sent) == 0 {
				t.Fatal("the stand-in was asked nothing")
			}
			for _, echo := range sent {
				if !strings.Contains(echo, plantedToken) {
					t.Errorf("the stand-in wrote %q, want the planted credential in it", echo)
				}
			}
			lines := sinkLines(t, buffer.String(), row.message, repo)
			if len(lines) == 0 {
				t.Fatalf("the runtime logged no %q line naming %s", row.message, repo)
			}
			value, found := records.value(row.message, repo, row.key)
			logged, isError := value.(error)
			if !found || !isError {
				t.Fatalf("the %q line's %s is %v (kept %t), want the error its sink logged", row.message, row.key, value, found)
			}
			var texts []string
			for _, line := range lines {
				if line.fields["level"] != "WARN" {
					t.Errorf("the %q line is at %v, want WARN", row.message, line.fields["level"])
				}
				if got, want := line.fields[row.key], redact.URLCredentials(logged.Error()); got != want {
					t.Errorf("the line's %s is %q, want the text of the error its sink logged as the handler writes it, %q", row.key, got, want)
				}
				texts = append(texts, line.text)
			}
			if row.check != nil {
				row.check(t, logged)
			}
			oauth2calltest.RequireNone(t, row.capture, strings.Join(append(texts, logged.Error()), "\n"))
			name := strings.TrimPrefix(row.capture, "sink/")
			oauth2calltest.KeptPrintTest(t, name+"/error", logged, sent[0])
			if row.mint {
				// The error prints none of GitHub's answer (an AnswerError prints its Err alone), so
				// the answer it keeps is printed too: there the request's credential sits.
				var answered *appauth.AnswerError
				if !errors.As(logged, &answered) || answered.Answer == nil {
					t.Fatalf("the %q line's error %q keeps no GitHub answer of appauth's", row.message, logged)
				}
				oauth2calltest.KeptPrintTest(t, name+"/answer", answered.Answer, sent[0])
			}
		})
	}
}

// mintedTokens is the daemon's provisioning token source over apps, as internal/daemon's
// implementTokens is: the implement App's read lease for the owner of the repository asked for,
// its error returned as it came. Until minting is set it answers staticTokens' lease, so a launch
// provisions before a row turns the mint on.
type mintedTokens struct {
	apps    *appauth.Manager
	minting atomic.Bool
}

func (m *mintedTokens) Token(ctx context.Context, repo ghrepo.Repository) (string, error) {
	if !m.minting.Load() {
		return staticTokens{}.Token(ctx, repo)
	}
	lease, err := m.apps.ReadToken(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return "", err
	}
	return lease.Token, nil
}

// plantedApps is a real token manager in token mode, as mintedTokens of the daemon holds it, whose
// token_command releases plantedToken as owner's installation token, and whose GitHub is a
// stand-in that refuses the identity read every release is checked by (appauth's viewerLogin, POST
// /graphql under "token <token>") with a rate limit: 403 with X-RateLimit-Remaining 0, its
// Authorization echoed verbatim in the body and in every other rate-limit header, each of which
// GitHub would write as a number or a date. Each echo is kept in the answer's echoes.
func plantedApps(t *testing.T, owner string) (*mintedTokens, *echoes) {
	t.Helper()
	echoed := &echoes{}
	github := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			githubtest.Unexpected(t, w, r)
			return
		}
		authorization := r.Header.Get("Authorization")
		echoed.add(authorization)
		for _, name := range []string{"Retry-After", "X-RateLimit-Limit", "X-RateLimit-Reset", "X-RateLimit-Resource"} {
			w.Header().Set(name, authorization)
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"message":"API rate limit exceeded for %s"}`, authorization)
	}})
	release := fmt.Sprintf(`{"installations":{%q:{"installation_id":7,"token":%q,"expires_at":%q}}}`,
		owner, plantedToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: "printf '%s' '" + release + "'"}},
		appauth.Options{BaseURL: github.URL, HTTPClient: github.Client(), Stderr: io.Discard})
	t.Cleanup(apps.Close)
	return &mintedTokens{apps: apps}, echoed
}

// echoes is each text a stand-in wrote in its answers that holds a credential, in order: the
// request's own, echoed, or words of the stand-in's that quote one.
type echoes struct {
	mu   sync.Mutex
	sent []string
}

func (e *echoes) add(echo string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, echo)
}

func (e *echoes) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.sent)
}

// keptLog hands every record to the daemon's own handler, which writes the line, and keeps the
// record as the call made it in records, so a row reads the value its sink logged, the error
// itself, beside the line written of it.
type keptLog struct {
	slog.Handler
	records *keptRecords
}

func (h keptLog) Handle(ctx context.Context, record slog.Record) error {
	h.records.keep(record)
	return h.Handler.Handle(ctx, record)
}

func (h keptLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return keptLog{Handler: h.Handler.WithAttrs(attrs), records: h.records}
}

func (h keptLog) WithGroup(name string) slog.Handler {
	return keptLog{Handler: h.Handler.WithGroup(name), records: h.records}
}

// keptRecords are the records a keptLog kept, in order.
type keptRecords struct {
	mu   sync.Mutex
	kept []slog.Record
}

func (k *keptRecords) keep(record slog.Record) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kept = append(k.kept, record.Clone())
}

// value is what the first record of message naming repo carries under key, as the call passed it.
func (k *keptRecords) value(message string, repo ghrepo.Repository, key string) (any, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, record := range k.kept {
		if record.Message != message {
			continue
		}
		var named bool
		var value any
		var held bool
		record.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "repo":
				named = a.Value.String() == repo.String()
			case key:
				value, held = a.Value.Any(), true
			}
			return true
		})
		if named && held {
			return value, true
		}
	}
	return nil, false
}

// sinkLine is one line of the daemon's JSON log, as written and decoded.
type sinkLine struct {
	text   string
	fields map[string]any
}

// sinkLines are the lines of logged, the daemon's JSON log, whose message is message and which
// name repo.
func sinkLines(t *testing.T, logged, message string, repo ghrepo.Repository) []sinkLine {
	t.Helper()
	var lines []sinkLine
	for _, text := range strings.Split(logged, "\n") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(text), &fields); err != nil {
			t.Fatalf("a line of the daemon's log is not JSON: %v", err)
		}
		if fields[slog.MessageKey] == message && fields["repo"] == repo.String() {
			lines = append(lines, sinkLine{text: text, fields: fields})
		}
	}
	return lines
}
