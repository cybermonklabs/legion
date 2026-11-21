package launcher

import (
	"cmp"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google/externalaccount"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
)

// The pod-local metadata endpoint, which an issue pod's launcher serves when the pod runs in a GKE
// Sandbox microVM and the deployment sets `runtime.kubernetes.sandbox.microvm.metadata`
// (Config.Metadata). Under Workload Identity a pod gets its Google credential from the node's
// metadata server, which a microVM's pod does not reach, so the launcher answers the paths Google's
// clients read in that server's place and in its form (testdata/gke-metadata.json holds GKE's own
// answers), and gets the access token it hands out the way GKE's server does: it exchanges the
// pod's projected ServiceAccount token at Google's security token service.
//
// MetadataAddress is where a pod's endpoint listens (MetadataConfig.listenAddress): on the loopback
// every role shares, since a role has no network namespace of its own (role-init), and at a port
// above 1024, since the pod runs as no root. MetadataHostVariable and MetadataIPVariable are where
// Google's clients look for it: Go's compute/metadata and Python's google-auth read the first,
// Node's gcp-metadata the second before the first. The launcher sets both, to the address it
// listens at, in its own environment and on every generation it starts (metadataEnv).
// WorkloadIdentityDirName is the directory the daemon mounts the projected token in, under the
// roles directory, which every role's namespace covers (role-init), so no role reads the token
// that the endpoint exchanges; WorkloadIdentityTokenFile is the token's file in it.
const (
	MetadataAddress           = "127.0.0.1:8169"
	MetadataHostVariable      = "GCE_METADATA_HOST"
	MetadataIPVariable        = "GCE_METADATA_IP"
	WorkloadIdentityDirName   = "workload-identity"
	WorkloadIdentityTokenFile = "token"
)

// MetadataConfig is what the endpoint serves: WorkloadPool is the cluster's Workload Identity pool,
// `<project>.svc.id.goog`, which GKE's server names as the account of a pod whose Kubernetes
// ServiceAccount is bound to no Google service account, and whose project is the project it
// answers; IdentityProvider is the cluster's identity provider,
// `https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>`,
// which with the pool makes the token exchange's audience. address, tokenFile, tokenURL,
// readTimeout and idleTimeout are a test's, which only this package's tests set: the zero value of
// each is MetadataAddress, which a pod always listens on, the token the daemon mounts
// (WorkloadIdentityDirName under the roles directory), Google's security token service,
// metadataReadTimeout and metadataIdleTimeout. A test listens on a port of its own
// (`127.0.0.1:0`), so no test holds the pod's, and names bounds short enough to wait out.
type MetadataConfig struct {
	WorkloadPool, IdentityProvider string
	address, tokenFile, tokenURL   string
	readTimeout, idleTimeout       time.Duration
}

// listenAddress is where the endpoint listens.
func (c MetadataConfig) listenAddress() string { return cmp.Or(c.address, MetadataAddress) }

// httpServer is the endpoint's HTTP server over handler, with a bound on each way a client could
// hold a connection of the launcher's open: metadataReadTimeout to send a request, its line and
// headers (ReadHeaderTimeout) and any body it names (ReadTimeout), and metadataIdleTimeout
// between two requests of a connection it keeps. ReadTimeout stops counting once the request is
// read, so a request with no body, which is every client's, is answered however long its token's
// exchange takes; the endpoint reads no body, so one a request names and never sends ends at the
// bound, answered, with its connection closed.
func (c MetadataConfig) httpServer(handler http.Handler) *http.Server {
	readTimeout := cmp.Or(c.readTimeout, metadataReadTimeout)
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       cmp.Or(c.idleTimeout, metadataIdleTimeout),
	}
}

const (
	// metadataPrefix is the version of the metadata API Google's clients read. Every path under it
	// needs the flavor header, as GKE's server has it; the two directories above it do not.
	metadataPrefix = "/computeMetadata/v1/"
	// workloadPoolSuffix ends every Workload Identity pool; the project is what comes before it.
	workloadPoolSuffix = ".svc.id.goog"
	// cloudPlatformScope is the scope of a token request that names none, as GKE's server has it.
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	// tokenEarlyRefresh is how long before its expiry a cached token is exchanged again, so no
	// client is handed one that runs out while it uses it.
	tokenEarlyRefresh = 5 * time.Minute
	// maxTokenSources bounds the token sources kept, one per scope set, so a client asking for
	// ever new scope sets cannot grow the launcher's memory; one goes when another comes.
	maxTokenSources = 16
	// maxNotServedPaths bounds the paths remembered as logged (notFound) for the same reason; a
	// path beyond it is answered and not logged.
	maxNotServedPaths = 256
	// exchangeFailureLogInterval is the least time between two lines of a failing exchange, which
	// fails for every request while the service refuses the pod's token.
	exchangeFailureLogInterval = time.Minute
	// exchangeTimeout bounds one exchange. A token source holds its lock through an exchange, so
	// one that never answered would hold every later request for that scope set with it.
	exchangeTimeout = 30 * time.Second
	// maxFailureReason bounds the reason a failure line quotes, in bytes, so one long answer of the
	// service cannot flood the log.
	maxFailureReason = 256
	// metadataReadTimeout bounds how long a client may take to send a request, so a process in the
	// pod that opens a connection and sends nothing, or part of a request, does not hold it open.
	metadataReadTimeout = 10 * time.Second
	// metadataIdleTimeout bounds how long a connection a client keeps may wait for its next
	// request, for the same reason.
	metadataIdleTimeout = time.Minute
)

// accountScopes are the scopes GKE's server lists for a Workload Identity pod's account.
var accountScopes = []string{cloudPlatformScope, "https://www.googleapis.com/auth/userinfo.email"}

// serveMetadata starts the pod-local metadata endpoint, before the launcher serves any role, and
// answers a channel closed once the server has stopped, which it does when ctx ends, and the
// address it serves at, "" when it serves none. Listening is the one step that can fail: an
// address another process of the container holds turns the endpoint off with one line in the log,
// and the roles then start told nothing (metadataEnv), rather than the pod failing its container
// and every role it serves. Once it listens it names the address the listener bound in the
// launcher's own environment, so the launcher's own Google clients (the session database's proxy,
// which signs in as the pod) read the endpoint too.
func serveMetadata(ctx context.Context, cfg Config, logf func(format string, args ...any)) (<-chan struct{}, string) {
	metadata := *cfg.Metadata
	listener, err := net.Listen("tcp", metadata.listenAddress())
	if err != nil {
		logf("launcher: metadata: off reason=%v", err)
		return nil, ""
	}
	address := listener.Addr().String()
	for _, name := range []string{MetadataHostVariable, MetadataIPVariable} {
		// A listener's address: Setenv refuses only a name or value holding '=' or NUL.
		_ = os.Setenv(name, address)
	}
	metadata.tokenFile = cmp.Or(metadata.tokenFile, filepath.Join(cfg.RolesDir, WorkloadIdentityDirName, WorkloadIdentityTokenFile))
	server := metadata.httpServer(newMetadataServer(ctx, metadata, logf))
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		_ = server.Close()
		<-served
	}()
	return done, address
}

// metadataEnv is what a generation of this launcher is told about the pod-local metadata endpoint:
// when this launcher serves it (address, which serveMetadata answers), MetadataHostVariable and
// MetadataIPVariable are that address, over whatever value the start command carried, so a role
// is told only the address its own launcher serves; when it serves none (address "") the
// environment is left exactly as it is, neither added to nor taken from, so a gVisor pod's roles
// see what they saw before the endpoint existed.
func metadataEnv(env []string, address string) []string {
	if address == "" {
		return env
	}
	return mergeEnv(env, []string{MetadataHostVariable + "=" + address, MetadataIPVariable + "=" + address})
}

// metadataServer answers the endpoint's requests. Its token sources, one per scope set, each
// exchange the pod's token again once the token they hold has less than tokenEarlyRefresh left,
// reading the token file at each exchange, so the token the kubelet rotates is the one sent.
type metadataServer struct {
	// ctx is the launcher's, carrying the exchanges' bounded HTTP client.
	ctx                 context.Context
	pool, audience      string
	tokenFile, tokenURL string
	answers             metadataAnswers
	logf                func(format string, args ...any)
	// now is the clock a token's remaining seconds and the failure lines' interval are read
	// against; a test replaces it.
	now func() time.Time
	// mu guards the token sources by scope set, the paths logged as not served and when a failed
	// exchange was last logged.
	mu            sync.Mutex
	sources       map[string]oauth2.TokenSource
	notServed     map[string]bool
	failureLogged time.Time
}

// metadataAnswer is a value, a list value or a directory's listing in the two forms GKE's server
// answers it in: the text form, and the JSON a request asks for with `?alt=json`.
type metadataAnswer struct{ text, json []byte }

// metadataAnswers is every value, list value and listing the endpoint serves, each made once in
// both forms (newMetadataServer), and each listing naming only the entries the endpoint serves.
type metadataAnswers struct {
	// The two directories above the API, the API's own (api is /computeMetadata/v1/ itself and
	// accounts is instance/service-accounts/) and its two values.
	root, computeMetadata, api, project, instance, accounts, universe metadataAnswer
	projectID, universeDomain                                         metadataAnswer
	// The pod's one account's, the same under both of its names (account): its directory, its
	// value, its two list values and its answer to `?recursive=true`, which has the JSON form
	// alone.
	account, email, aliases, scopes metadataAnswer
	recursive                       []byte
}

// metadataValue is a value in both forms: itself, and itself as a JSON string.
func metadataValue(value string) metadataAnswer {
	return metadataAnswer{text: []byte(value), json: metadataJSON(value)}
}

// metadataList is a list value or a directory's listing in both forms: its entries one a line, and
// a JSON array of them.
func metadataList(entries ...string) metadataAnswer {
	return metadataAnswer{text: []byte(strings.Join(entries, "\n") + "\n"), json: metadataJSON(entries)}
}

// metadataJSON is v as GKE's server writes JSON: on one line, with no line break after it. v
// holds strings alone, which json.Marshal always encodes.
func metadataJSON(v any) []byte {
	encoded, _ := json.Marshal(v)
	return encoded
}

func newMetadataServer(ctx context.Context, cfg MetadataConfig, logf func(format string, args ...any)) *metadataServer {
	accounts := []string{"default/", cfg.WorkloadPool + "/"}
	slices.Sort(accounts)
	return &metadataServer{
		ctx:       context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: exchangeTimeout}),
		pool:      cfg.WorkloadPool,
		audience:  "identitynamespace:" + cfg.WorkloadPool + ":" + cfg.IdentityProvider,
		tokenFile: cfg.tokenFile, tokenURL: cmp.Or(cfg.tokenURL, sessionproxy.STSTokenURL),
		answers: metadataAnswers{
			root:            metadataList("computeMetadata/"),
			computeMetadata: metadataList("v1/"),
			api:             metadataList("instance/", "project/", "universe/"),
			project:         metadataList("project-id"),
			projectID:       metadataValue(strings.TrimSuffix(cfg.WorkloadPool, workloadPoolSuffix)),
			instance:        metadataList("service-accounts/"),
			accounts:        metadataList(accounts...),
			universe:        metadataList("universe-domain", "universe_domain"),
			universeDomain:  metadataValue("googleapis.com"),
			account:         metadataList("aliases", "email", "scopes", "token"),
			email:           metadataValue(cfg.WorkloadPool),
			aliases:         metadataList("default"),
			scopes:          metadataList(accountScopes...),
			recursive: metadataJSON(struct {
				Aliases []string `json:"aliases"`
				Email   string   `json:"email"`
				Scopes  []string `json:"scopes"`
			}{[]string{"default"}, cfg.WorkloadPool, accountScopes}),
		},
		logf: logf, now: time.Now,
		sources: map[string]oauth2.TokenSource{}, notServed: map[string]bool{},
	}
}

// ServeHTTP answers each path the way GKE's server answers it for a pod under Workload Identity:
// status, Metadata-Flavor, Content-Type, Location and body. The two directories above the API
// answer anyone; every path under it is refused without `Metadata-Flavor: Google` or with
// X-Forwarded-For, before anything else is read, so a request a proxy forwards on a role's behalf
// gets no token. A value, a list value and a listing are answered as text or, to `?alt=json`, as
// JSON (metadataAnswer.serve); the token and an account's `?recursive=true` are JSON whatever
// `alt` says. Anything the endpoint does not serve is answered as GKE answers an unknown path.
func (s *metadataServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		s.answers.root.serve(w, r.URL.Query())
		return
	case "/computeMetadata/":
		s.answers.computeMetadata.serve(w, r.URL.Query())
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, metadataPrefix)
	if !ok {
		s.notFound(w, r.URL.EscapedPath())
		return
	}
	if r.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, `Missing required header "Metadata-Flavor": "Google"`, http.StatusForbidden)
		return
	}
	if _, forwarded := r.Header["X-Forwarded-For"]; forwarded {
		http.Error(w, "Request cannot contain header: X-Forwarded-For", http.StatusForbidden)
		return
	}
	switch rest {
	case "":
		s.listing(w, r, s.answers.api)
	case "project/":
		s.listing(w, r, s.answers.project)
	case "project/project-id":
		s.answers.projectID.serve(w, r.URL.Query())
	case "instance", "instance/service-accounts":
		redirectToDirectory(w, r)
	case "instance/":
		s.listing(w, r, s.answers.instance)
	case "instance/service-accounts/":
		s.listing(w, r, s.answers.accounts)
	case "universe/":
		s.listing(w, r, s.answers.universe)
	case "universe/universe-domain", "universe/universe_domain":
		s.answers.universeDomain.serve(w, r.URL.Query())
	default:
		s.account(w, r, rest)
	}
}

// account answers the paths of the pod's one account, which GKE's server lists twice: as
// `default` and as its email, the pool. `identity` is not served: GKE's server answers it 404 for
// an account bound to no Google service account.
func (s *metadataServer) account(w http.ResponseWriter, r *http.Request, rest string) {
	path, ok := strings.CutPrefix(rest, "instance/service-accounts/")
	name, item, directory := strings.Cut(path, "/")
	if !ok || (name != "default" && name != s.pool) {
		s.notFound(w, r.URL.EscapedPath())
		return
	}
	if !directory {
		redirectToDirectory(w, r)
		return
	}
	switch item {
	case "":
		query := r.URL.Query()
		if query.Get("recursive") == "true" {
			answer(w, "application/json", s.answers.recursive)
			return
		}
		s.answers.account.serve(w, query)
	case "email":
		s.answers.email.serve(w, r.URL.Query())
	case "aliases":
		s.answers.aliases.serve(w, r.URL.Query())
	case "scopes":
		s.answers.scopes.serve(w, r.URL.Query())
	case "token":
		s.token(w, r)
	default:
		s.notFound(w, r.URL.EscapedPath())
	}
}

// token answers an access token for the scopes the request names, as GKE's server does: the
// token, the whole seconds it has left and its type. A failed exchange answers a short text that
// names no token, and logs at most one line a minute (exchangeFailed).
func (s *metadataServer) token(w http.ResponseWriter, r *http.Request) {
	scopes := requestedScopes(r.URL.Query()["scopes"])
	source, err := s.tokenSource(scopes)
	var token *oauth2.Token
	if err == nil {
		token, err = source.Token()
	}
	if err != nil {
		s.exchangeFailed(scopes, err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}{token.AccessToken, int64(token.Expiry.Sub(s.now()) / time.Second), token.Type()})
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}
	answer(w, "application/json", body)
}

// requestedScopes is the scope set a token request names: `?scopes=`, a comma-separated list as
// Google's clients send it, in one order and each once, so two requests naming one set share one
// token source; cloud-platform alone when the request names none.
func requestedScopes(lists []string) []string {
	var scopes []string
	for _, list := range lists {
		for _, scope := range strings.Split(list, ",") {
			if scope = strings.TrimSpace(scope); scope != "" {
				scopes = append(scopes, scope)
			}
		}
	}
	if len(scopes) == 0 {
		return []string{cloudPlatformScope}
	}
	slices.Sort(scopes)
	return slices.Compact(scopes)
}

// tokenSource is the token source of one scope set: the exchange GKE's server makes, of the token
// file's contents for an access token of the pool, kept until tokenEarlyRefresh before its expiry.
// It is made once per scope set; once maxTokenSources are kept, one of them makes way for a new one.
func (s *metadataServer) tokenSource(scopes []string) (oauth2.TokenSource, error) {
	key := strings.Join(scopes, " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	if source, ok := s.sources[key]; ok {
		return source, nil
	}
	source, err := externalaccount.NewTokenSource(s.ctx, externalaccount.Config{
		Audience:         s.audience,
		SubjectTokenType: sessionproxy.JWTTokenType,
		TokenURL:         s.tokenURL,
		Scopes:           scopes,
		CredentialSource: &externalaccount.CredentialSource{File: s.tokenFile},
	})
	if err != nil {
		return nil, err
	}
	source = oauth2.ReuseTokenSourceWithExpiry(nil, source, tokenEarlyRefresh)
	if len(s.sources) >= maxTokenSources {
		for evicted := range s.sources {
			delete(s.sources, evicted)
			break
		}
	}
	s.sources[key] = source
	return source, nil
}

// exchangeFailed logs a failed exchange, at most once in exchangeFailureLogInterval: while the
// service refuses the pod's token every request fails, and one line a minute says so. The reason is
// the exchange's own error, which names the service's answer and never the token sent, cut to
// maxFailureReason bytes at a character's start by redact.Bytes, which reads it whole through the
// URL rule first, since the answer is the service's and a cut made first could end inside a URL's
// userinfo, before its "@"; it is quoted, as are the scopes a client named, so neither can break
// the line.
func (s *metadataServer) exchangeFailed(scopes []string, err error) {
	s.mu.Lock()
	now := s.now()
	quiet := !s.failureLogged.IsZero() && now.Sub(s.failureLogged) < exchangeFailureLogInterval
	if !quiet {
		s.failureLogged = now
	}
	s.mu.Unlock()
	if quiet {
		return
	}
	reason, _ := redact.Bytes(err.Error(), maxFailureReason)
	s.logf("launcher: metadata: token exchange failed scopes=%q reason=%q", strings.Join(scopes, ","), reason)
}

// listing answers a directory: its entries, each one the endpoint serves, in the form the request
// asks for (metadataAnswer.serve). GKE's server answers `?recursive=true` on a directory with all
// of it as JSON, which the endpoint does only for an account (account), so on any other directory
// such a request is a path it does not serve, whatever `alt` it names.
func (s *metadataServer) listing(w http.ResponseWriter, r *http.Request, entries metadataAnswer) {
	query := r.URL.Query()
	if query.Get("recursive") == "true" {
		s.notFound(w, r.URL.EscapedPath()+"?recursive=true")
		return
	}
	entries.serve(w, query)
}

// notFound answers a path the endpoint does not serve as GKE's server answers an unknown one, and
// logs each such path once, so the container's log names every path a tool asked for in vain. The
// path is the request's escaped one, which holds no line break. At most maxNotServedPaths are
// remembered.
func (s *metadataServer) notFound(w http.ResponseWriter, path string) {
	s.mu.Lock()
	first := !s.notServed[path] && len(s.notServed) < maxNotServedPaths
	if first {
		s.notServed[path] = true
	}
	s.mu.Unlock()
	if first {
		s.logf("launcher: metadata: not served path=%s", path)
	}
	http.Error(w, "Not Found", http.StatusNotFound)
}

// redirectToDirectory answers a directory's path without its slash as GKE's server does: a 301 to
// the path with it, keeping the query.
func redirectToDirectory(w http.ResponseWriter, r *http.Request) {
	target := r.URL.EscapedPath() + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// serve answers a in the form the request's query asks for, as GKE's server does: the JSON form
// where its first `alt` is `json` in any case of the letters, and the text form for every other
// `alt` and for none.
func (a metadataAnswer) serve(w http.ResponseWriter, query url.Values) {
	if strings.EqualFold(query.Get("alt"), "json") {
		answer(w, "application/json", a.json)
		return
	}
	answer(w, "application/text", a.text)
}

// answer answers body under contentType, as GKE's server answers what it serves.
func answer(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}
