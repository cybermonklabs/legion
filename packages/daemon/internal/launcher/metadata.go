package launcher

import (
	"cmp"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google/externalaccount"
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
// which with the pool makes the token exchange's audience. address, tokenFile and tokenURL are a
// test's, which only this package's tests set: "" is MetadataAddress, which a pod always listens
// on, the token the daemon mounts (WorkloadIdentityDirName under the roles directory) and Google's
// security token service. A test listens on a port of its own (`127.0.0.1:0`), so no test holds
// the pod's.
type MetadataConfig struct {
	WorkloadPool, IdentityProvider string
	address, tokenFile, tokenURL   string
}

// listenAddress is where the endpoint listens.
func (c MetadataConfig) listenAddress() string { return cmp.Or(c.address, MetadataAddress) }

const (
	// metadataPrefix is the version of the metadata API Google's clients read. Every path under it
	// needs the flavor header, as GKE's server has it; the two directories above it do not.
	metadataPrefix = "/computeMetadata/v1/"
	// stsTokenURL is the exchange GKE's own server makes.
	stsTokenURL = "https://sts.googleapis.com/v1/token"
	// workloadPoolSuffix ends every Workload Identity pool; the project is what comes before it.
	workloadPoolSuffix = ".svc.id.goog"
	// subjectTokenType is the projected ServiceAccount token's type, a JWT.
	subjectTokenType = "urn:ietf:params:oauth:token-type:jwt"
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
	server := &http.Server{Handler: newMetadataServer(ctx, metadata, logf)}
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
	// accounts is the listing of instance/service-accounts/ and accountJSON the account's answer
	// to `?recursive=true`, each made once (newMetadataServer).
	accounts    []string
	accountJSON []byte
	logf        func(format string, args ...any)
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

func newMetadataServer(ctx context.Context, cfg MetadataConfig, logf func(format string, args ...any)) *metadataServer {
	accounts := []string{"default/", cfg.WorkloadPool + "/"}
	slices.Sort(accounts)
	// Strings alone, which json.Marshal always encodes.
	accountJSON, _ := json.Marshal(struct {
		Aliases []string `json:"aliases"`
		Email   string   `json:"email"`
		Scopes  []string `json:"scopes"`
	}{[]string{"default"}, cfg.WorkloadPool, accountScopes})
	return &metadataServer{
		ctx:       context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: exchangeTimeout}),
		pool:      cfg.WorkloadPool,
		audience:  "identitynamespace:" + cfg.WorkloadPool + ":" + cfg.IdentityProvider,
		tokenFile: cfg.tokenFile, tokenURL: cmp.Or(cfg.tokenURL, stsTokenURL),
		accounts: accounts, accountJSON: accountJSON, logf: logf, now: time.Now,
		sources: map[string]oauth2.TokenSource{}, notServed: map[string]bool{},
	}
}

// ServeHTTP answers each path the way GKE's server answers it for a pod under Workload Identity:
// status, Metadata-Flavor, Content-Type, Location and body. The two directories above the API
// answer anyone; every path under it is refused without `Metadata-Flavor: Google` or with
// X-Forwarded-For, before anything else is read, so a request a proxy forwards on a role's behalf
// gets no token. Anything the endpoint does not serve is answered as GKE answers an unknown path.
func (s *metadataServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		answerText(w, "computeMetadata/\n")
		return
	case "/computeMetadata/":
		answerText(w, "v1/\n")
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
		s.listing(w, r, "instance/", "project/", "universe/")
	case "project/":
		s.listing(w, r, "project-id")
	case "project/project-id":
		answerText(w, strings.TrimSuffix(s.pool, workloadPoolSuffix))
	case "instance", "instance/service-accounts":
		redirectToDirectory(w, r)
	case "instance/":
		s.listing(w, r, "service-accounts/")
	case "instance/service-accounts/":
		s.listing(w, r, s.accounts...)
	case "universe/":
		s.listing(w, r, "universe-domain", "universe_domain")
	case "universe/universe-domain", "universe/universe_domain":
		answerText(w, "googleapis.com")
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
		if r.URL.Query().Get("recursive") != "true" {
			answerText(w, "aliases\nemail\nscopes\ntoken\n")
			return
		}
		answer(w, "application/json", s.accountJSON)
	case "email":
		answerText(w, s.pool)
	case "aliases":
		answerText(w, "default\n")
	case "scopes":
		answerText(w, strings.Join(accountScopes, "\n")+"\n")
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
		SubjectTokenType: subjectTokenType,
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
// maxFailureReason bytes at a character's start; it is quoted, as are the scopes a client named, so
// neither can break the line.
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
	reason := err.Error()
	if len(reason) > maxFailureReason {
		cut := maxFailureReason
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	s.logf("launcher: metadata: token exchange failed scopes=%q reason=%q", strings.Join(scopes, ","), reason)
}

// listing answers a directory: its entries, one a line, each one the endpoint serves. GKE's server
// answers `?recursive=true` on a directory with all of it as JSON, which the endpoint does only for
// an account (account), so on any other directory such a request is a path it does not serve.
func (s *metadataServer) listing(w http.ResponseWriter, r *http.Request, entries ...string) {
	if r.URL.Query().Get("recursive") == "true" {
		s.notFound(w, r.URL.EscapedPath()+"?recursive=true")
		return
	}
	answerText(w, strings.Join(entries, "\n")+"\n")
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

// answerText answers a value or a listing as GKE's server does, under its own content type.
func answerText(w http.ResponseWriter, body string) {
	answer(w, "application/text", []byte(body))
}

func answer(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}
