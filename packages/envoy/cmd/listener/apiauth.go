package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
)

const bearerPrefix = "Bearer "

// The credential label of envoy_listener_requests_total: which credential admitted a request.
// none is the open /v1, which only a listener on loopback serves (requiresCredential), admitting a
// request that presents no credential it knows, and none_broker_token is that open /v1 admitting a
// request that presents a broker token on a listener that takes none (brokers is nil), so serves it
// unverified: the series counts it apart from a bare request, since a caller that already holds a
// broker token is served as one only once its listener takes that credential.
const (
	credentialNone            = "none"
	credentialNoneBrokerToken = "none_broker_token"
	credentialOIDC            = "oidc"
	credentialGoogle          = "google"
	credentialBroker          = "broker"
)

// The warnings an admission on one of the credentials that name no caller logs, and how often each
// logs at most (admissionWarnings).
const (
	openAdmittedMessage             = "listener: request admitted with no credential"
	unverifiedBrokerAdmittedMessage = "listener: request admitted with a broker token this listener does not verify"
	admissionWarnEvery              = time.Minute
)

// newListenerRequests registers envoy_listener_requests_total, the census of which credential
// admitted each /v1 request. Its none and none_broker_token series count the callers a loopback
// listener's open /v1 serves with no credential it verifies; every listener off loopback reads
// zero on both, since its /v1 is closed to such a caller.
func newListenerRequests(met *metrics.Registry) *metrics.Counter {
	return met.NewCounter("envoy_listener_requests_total",
		"Requests /v1 admitted, by the credential that admitted each (none, none_broker_token, oidc, google, broker) and the route it reached")
}

// listenerAPIAuth is everything apiAuth admits a /v1 caller with, and where it records each caller
// it admits.
type listenerAPIAuth struct {
	// verifier is the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair's, nil when the pair is unset.
	verifier *oidc.Verifier
	// accounts is ENVOY_OIDC_ACCOUNTS, the subjects the projected-token branch admits. Empty admits
	// every subject verifier verifies, as the branch did before the list existed.
	accounts []string
	// google is ENVOY_OIDC_GOOGLE_AUDIENCE and ENVOY_OIDC_GOOGLE_ACCOUNTS's, nil when they are unset.
	google *googleIdentities
	// brokers takes access tokens the secrets broker issued; nil on a listener that takes none.
	brokers *brokerTokens
	// offLoopback is whether the listener binds an address other than loopback: main sets it from
	// config.Service.ListenHost, which with ENVOY_LISTEN_FD is the inherited socket's own address.
	// Off loopback /v1 is closed to a caller that presents no credential (requiresCredential).
	offLoopback bool
	// allowedHosts is ENVOY_ALLOWED_HOSTS, the names an open /v1 serves beside loopback.
	allowedHosts []string
	// logger takes every refusal and every warned admission; apiAuth refuses an auth without one.
	logger *logging.Logger
	// requests is envoy_listener_requests_total (newListenerRequests); nil counts nothing.
	requests *metrics.Counter
	// route names the route a request reaches, the counter's path label (startingGate.route);
	// nil names the pattern the request was routed by (http.Request.Pattern).
	route func(*http.Request) string
	// now is the clock the once-a-minute warnings read; nil is time.Now.
	now func() time.Time
}

// requiresCredential reports whether /v1 is closed to a caller that presents no credential: on a
// listener off loopback, whatever it takes, and on loopback once it takes one of the two bearers, a
// projected service-account token or a Google identity token. A loopback listener with neither is
// the one open /v1 left: it serves a caller with no credential, and with its broker settings it
// still verifies a broker token that is presented. Off loopback the broker settings are a
// credential like the bearers (validateListenerAPIAuth counts them), so the clustered session
// listener, whose only credential they are, answers a request that presents nothing 401.
func (a listenerAPIAuth) requiresCredential() bool {
	return a.offLoopback || a.verifier != nil || a.google != nil
}

// listenerCredentials names the three credentials a listener verifies, for the refusals that tell
// an operator what to set: the two identity-token pairs a caller's platform identity is checked
// against, and the broker settings a session's broker token is checked with.
const listenerCredentials = "the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair, the ENVOY_OIDC_GOOGLE_AUDIENCE/ENVOY_OIDC_GOOGLE_ACCOUNTS pair, or the broker settings (" +
	EnvBrokerURL + " with " + EnvBrokerAccountTokenFile + " or " + EnvBrokerGoogleAudience + ", and " + EnvPublicURL + ")"

// refuseRemovedAPISettings refuses the three settings the shared token's removal ended, each when
// it is set to anything at all, naming it and what replaces it. ENVOY_API_TOKEN was the shared
// bearer: a caller now presents a broker token or a platform identity token, which the listener
// verifies, and a loopback listener needs neither. ENVOY_API_ALLOW_UNAUTHENTICATED opened /v1 off
// loopback, and ENVOY_V1_OPEN_WITH_TOKEN_CHECKS kept /v1 open to a caller with no credential
// beside a token check: the open /v1 is now a loopback listener's alone. A listener that read any
// of them as before would serve callers its deployment meant to fence, or turn away callers it
// meant to serve, so each refuses the start rather than being ignored. Set empty, as a render
// writes a variable it no longer fills, each is unset.
func refuseRemovedAPISettings(getenv func(string) string) error {
	var refusals []error
	if getenv("ENVOY_API_TOKEN") != "" {
		refusals = append(refusals, fmt.Errorf("ENVOY_API_TOKEN is set, and the listener takes no shared token; unset it. "+
			"A caller presents a broker token or a platform identity token in its place, which this listener verifies with %s; "+
			"a listener on loopback needs none of them", listenerCredentials))
	}
	for _, open := range []string{"ENVOY_API_ALLOW_UNAUTHENTICATED", "ENVOY_V1_OPEN_WITH_TOKEN_CHECKS"} {
		if getenv(open) != "" {
			refusals = append(refusals, fmt.Errorf("%s is set, and /v1 serves a caller with no credential only on loopback; unset it. "+
				"Bind ENVOY_LISTEN_HOST to loopback for a listener that serves such callers, or set %s for one off loopback", open, listenerCredentials))
		}
	}
	return errors.Join(refusals...)
}

// googleIdentities admits a caller by its own Google identity token, the credential of a caller
// that runs on Google's platform and holds no Kubernetes token (the alert sink on Cloud Run): a
// token Google signed for audience, naming an account accounts lists whose address Google
// verified.
type googleIdentities struct {
	// audience is ENVOY_OIDC_GOOGLE_AUDIENCE, the audience the caller asks its metadata server for.
	audience string
	// verifier verifies a token Google (oidc.GoogleIssuer) signed for audience; apiAuth refuses
	// Google identities without one.
	verifier *oidc.Verifier
	// accounts is ENVOY_OIDC_GOOGLE_ACCOUNTS, each address in lower case.
	accounts []string
}

// admits judges raw as a Google identity token of a listed account, and answers that account, in
// lower case, the principal the request is admitted as (withVerifiedPrincipal), with a nil error.
// It logs the one line that says why when it is not: the class, the account when the token names
// one, and the path, never the token. The classes are oidc.Reason's for a token that does not
// verify, no_email for one minted without format=full (it names no account), email_not_verified,
// and not_listed. Its error is the verifier's own, which matches oidc.ErrIssuerUnavailable when no
// verdict was reached (refuseIdentityToken).
func (g *googleIdentities) admits(r *http.Request, raw string, logger *logging.Logger) (string, error) {
	claims, err := g.verifier.VerifyGoogleIdentity(r.Context(), raw)
	account := fold.ASCII(claims.Email)
	var reason string
	switch {
	case err != nil:
		reason = oidc.Reason(err)
	case account == "":
		reason = "no_email"
	case !claims.EmailVerified:
		reason = "email_not_verified"
	case !slices.Contains(g.accounts, account):
		reason = "not_listed"
	default:
		return account, nil
	}
	attrs := []slog.Attr{slog.String("reason", reason), slog.String("path", r.URL.Path)}
	if account != "" {
		attrs = append(attrs, slog.String("account", account))
	}
	logger.Warn("listener: Google identity token rejected", attrs...)
	return "", identityRefusal(err, reason)
}

// admitsServiceAccount judges raw as a projected service-account token the verifier verifies, of a
// subject accounts lists when it lists any, and answers that subject,
// system:serviceaccount:<namespace>:<name>, the principal the request is admitted as
// (withVerifiedPrincipal), with a nil error. It logs the one line that says why when it is not:
// oidc.Reason's class, or not_listed naming the subject, and the path, never the token. Its error
// is the verifier's own, which matches oidc.ErrIssuerUnavailable when no verdict was reached
// (refuseIdentityToken).
func (a listenerAPIAuth) admitsServiceAccount(r *http.Request, raw string) (string, error) {
	claims, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		a.logger.Warn("listener: service-account token rejected",
			slog.String("reason", oidc.Reason(err)),
			slog.String("path", r.URL.Path))
		return "", err
	}
	if len(a.accounts) > 0 && !slices.Contains(a.accounts, claims.Subject) {
		a.logger.Warn("listener: service-account token rejected",
			slog.String("reason", "not_listed"),
			slog.String("path", r.URL.Path),
			slog.String("account", claims.Subject))
		return "", identityRefusal(nil, "not_listed")
	}
	return claims.Subject, nil
}

// identityRefusal is the error an identity token that was judged and not admitted reads as: the
// verifier's own error when it gave one, else one naming the refusal's class.
func identityRefusal(err error, reason string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("identity token refused: %s", reason)
}

// refuseIdentityToken answers an identity token this listener did not admit. A token the verifier
// reached no verdict on, because the issuer's discovery or key set could not be read
// (oidc.ErrIssuerUnavailable), is the 503 with Retry-After: 1 a broker token the broker could not
// check gets (refuseBrokerToken): the listener could not run the check, which is its own failure
// rather than the token's. A 401 would tell Dispatch's client its account token was refused: it
// tries the call once more on a fresh token and then ends the call with that refusal
// (cmd/dispatch's listener client), so an issuer outage would fail Dispatch's sends for good,
// role-route messages among them, where a 503 leaves each to be retried. Every other outcome is
// the 401 every other refused credential gets here.
func refuseIdentityToken(w http.ResponseWriter, err error) {
	if errors.Is(err, oidc.ErrIssuerUnavailable) {
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "this identity token could not be checked; try again in a minute")
		return
	}
	writeJSONError(w, http.StatusUnauthorized, "unauthorized")
}

// verifiedPrincipalKey is the context key the principal an identity token proved is carried
// under. It is an unexported zero-size type, so nothing outside this package can read or write
// the value, and no other package's key can collide with it.
type verifiedPrincipalKey struct{}

// withVerifiedPrincipal carries the principal an identity token proved to every handler behind
// apiAuth: the subject of a projected service-account token, or the account a Google identity
// token names. No other credential proves one: a broker token names a session (brokerCaller), and
// a request with no credential names nobody.
func withVerifiedPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, verifiedPrincipalKey{}, principal)
}

// verifiedPrincipal is the principal r's identity token proved (withVerifiedPrincipal), empty for
// a request admitted on any other credential, or on none.
func verifiedPrincipal(r *http.Request) string {
	principal, _ := r.Context().Value(verifiedPrincipalKey{}).(string)
	return principal
}

// admissionWarnings logs an admission on no credential at all, or on a broker token this listener
// serves unverified, once a minute for each, naming the route and the caller's address, so an
// operator can find the callers envoy_listener_requests_total counts under none and
// none_broker_token, which only a loopback listener's open /v1 admits. Each line says how many
// admissions of its credential it held back since the last line. The other credentials stay out of
// the log: they are the ones a caller is meant to hold.
type admissionWarnings struct {
	now func() time.Time
	mu  sync.Mutex
	// last is when each credential last logged, and held how many admissions of it went unlogged
	// since.
	last map[string]time.Time
	held map[string]int
}

func newAdmissionWarnings(now func() time.Time) *admissionWarnings {
	if now == nil {
		now = time.Now
	}
	return &admissionWarnings{now: now, last: map[string]time.Time{}, held: map[string]int{}}
}

func (w *admissionWarnings) admitted(logger *logging.Logger, credential, path string, r *http.Request) {
	var message string
	switch credential {
	case credentialNone:
		message = openAdmittedMessage
	case credentialNoneBrokerToken:
		message = unverifiedBrokerAdmittedMessage
	default:
		return
	}
	now := w.now()
	w.mu.Lock()
	if last, logged := w.last[credential]; logged && now.Sub(last) < admissionWarnEvery {
		w.held[credential]++
		w.mu.Unlock()
		return
	}
	held := w.held[credential]
	w.last[credential], w.held[credential] = now, 0
	w.mu.Unlock()
	logger.Warn(message,
		slog.String("credential", credential),
		slog.String("path", path),
		slog.String("remote", r.RemoteAddr),
		slog.Int("held", held))
}

// apiAuth requires JSON bodies for /v1, then authenticates it with an access token the secrets
// broker issued when one is presented under the DPoP scheme (brokers, brokerauth.go), when a
// verifier is configured with a projected service-account token issued to its issuer and audience,
// of a subject ENVOY_OIDC_ACCOUNTS lists when it lists any, and when Google identities are
// configured with a Google identity token of a listed account (googleIdentities). It takes no
// shared token: ENVOY_API_TOKEN refuses the listener's start (refuseRemovedAPISettings). Exempt
// paths pass in every configuration. /v1 is open, serving a request that presents no credential,
// only on a listener bound to loopback that takes neither bearer (requiresCredential); off loopback
// it is closed whatever the listener takes, so a listener whose only credential is its broker
// settings answers a request that presents nothing 401. An open /v1 serves only a request whose
// Host names this listener (servesHost: a loopback name or address, or one of allowedHosts), so a
// page that rebinds its own name to an address reaching the listener cannot use it; a closed /v1
// checks no Host, since such a page holds no credential. A bearer the verifier rejects is answered
// 401 and never falls back to another authentication path; the failure class goes to the log and
// never to the caller, since naming it would tell an unauthenticated client which credential the
// listener is configured for.
//
// The two identity-token bearers are told apart by the token's own issuer
// (oidc.Verifier.Issues), including Google's scheme-less issuer spelling: a token
// Google issued goes to the Google branch, any other to the projected-token one, and a listener
// that takes only one of them gives it every token, so each refusal is logged with its class and
// no token is judged twice.
//
// Every request it admits is counted in envoy_listener_requests_total under the credential that
// admitted it and the route it reached (route), and an admission on no credential at all or on a
// broker token this listener does not verify also logs a warning once a minute
// (admissionWarnings): those two are the callers a loopback listener's open /v1 serves with no
// credential it verifies. A request admitted on an identity token carries the principal that token
// proved to the handler (withVerifiedPrincipal, verifiedPrincipal), which is how the send route
// tells Dispatch's own sends from every other caller's.
//
// The broker-token branch is taken only on a listener that takes this credential (brokers is
// nil on one that does not), and there the scheme alone decides it, before every bearer one:
// RFC 9449 Section 7.2 refuses a DPoP-bound token presented under any other scheme, so a DPoP
// presentation is this credential whatever the listener's other settings are, and is never
// retried as a bearer. It comes before the open pass-through too, which is what binds a session
// on an open loopback listener: a caller that presents a broker token is held to the session it
// names even where /v1 would have served the request with no credential at all.
//
// A listener with none of the broker variables set reads that header as it read every
// Authorization header it did not know before this credential existed: the open loopback listener
// serves the request, and a closed listener answers it the same 401 it answers every credential it
// does not take. A client sends Authorization: DPoP whenever it has a broker and an identity to
// trade and no Envoy bearer is set, whatever the listener it is calling takes. The open listener
// counts such a request as none_broker_token rather than none, so the census sees which of its
// callers already present a broker token it does not verify; the listeners that serve such
// clients off loopback, the clustered session listener among them, take the broker settings and
// verify it. A private hub's session listener's release enforces the private required mode on the
// callers whose broker token it verifies (infra/gcp's privateModeListenerFloor).
//
// On loopback the broker settings open nothing and close nothing. A request presenting no token is
// judged exactly as it was before this credential existed — the open listener still serves it, and
// the Host and Content-Type fences still apply — because there a broker token is a credential a
// caller may hold rather than one this listener requires. Off loopback the broker settings are the
// credential the listener requires, as either bearer is: validateListenerAPIAuth counts them, and a
// request that presents nothing is refused.
//
// An auth without its logger, or with Google identities that carry no verifier, is a programming
// error, refused here at construction rather than at the first request that would reach either:
// every refusal and every warned admission writes to the logger, and every identity token the
// Google branch takes goes to that verifier.
func apiAuth(auth listenerAPIAuth, next http.Handler) http.Handler {
	if auth.logger == nil {
		panic("listener API auth built without its logger")
	}
	if auth.google != nil && auth.google.verifier == nil {
		panic("listener API auth built with Google identities but no verifier for them")
	}
	open := !auth.requiresCredential()
	warnings := newAdmissionWarnings(auth.now)
	admit := func(w http.ResponseWriter, r *http.Request, credential string) {
		path := r.Pattern
		if auth.route != nil {
			path = auth.route(r)
		}
		auth.requests.Inc([2]string{"credential", credential}, [2]string{"path", path})
		warnings.admitted(auth.logger, credential, path, r)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIAuthExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if open && !servesHost(r.Host, auth.allowedHosts) {
			auth.logger.Warn("listener: request for a host it does not serve",
				slog.String("host", r.Host),
				slog.String("path", r.URL.Path))
			writeJSONError(w, http.StatusMisdirectedRequest,
				fmt.Sprintf("request Host %q is not a host this listener serves; an unauthenticated listener serves loopback and the names in ENVOY_ALLOWED_HOSTS", r.Host))
			return
		}
		if r.ContentLength != 0 {
			contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || contentType != "application/json" {
				writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		if _, presented := ciphertoken.Presented(r); auth.brokers != nil && presented {
			caller, err := auth.brokers.verify(r)
			if err != nil {
				refuseBrokerToken(w, auth.logger, r, auth.brokers, err)
				return
			}
			admit(w, r.WithContext(withBrokerCaller(r.Context(), caller)), credentialBroker)
			return
		}
		if open {
			if _, presented := ciphertoken.Presented(r); presented {
				admit(w, r, credentialNoneBrokerToken)
				return
			}
			admit(w, r, credentialNone)
			return
		}

		authorization := r.Header.Get("Authorization")
		if raw, ok := strings.CutPrefix(authorization, bearerPrefix); ok && oidc.LooksLikeJWT(raw) {
			switch {
			case auth.google != nil && (auth.verifier == nil || auth.google.verifier.Issues(raw)):
				account, err := auth.google.admits(r, raw, auth.logger)
				if err == nil {
					admit(w, r.WithContext(withVerifiedPrincipal(r.Context(), account)), credentialGoogle)
					return
				}
				refuseIdentityToken(w, err)
				return
			case auth.verifier != nil:
				subject, err := auth.admitsServiceAccount(r, raw)
				if err == nil {
					admit(w, r.WithContext(withVerifiedPrincipal(r.Context(), subject)), credentialOIDC)
					return
				}
				refuseIdentityToken(w, err)
				return
			}
		}

		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	})
}

// refuseBrokerToken answers a DPoP presentation this listener did not admit, and logs the one
// line that says why: the class, as the service-account branch logs oidc.Reason's, the path, and
// never the token or the proof.
//
// Three of the outcomes are not refusals of the caller and are not answered as one. A broker
// that gave no answer is a 503, for the reason Dispatch answers one: a caller told its token was
// refused would discard a token that may be perfectly live, where a 503 asks it to come back. A
// spent-proof store that did not answer is the same 503: the listener could not run the check,
// which is its own failure rather than the token's, and a client does not retry a 401, so a NATS
// fault of a few seconds would otherwise fail every send a live token makes. A source address
// with no introspection left in its bucket is the 429 that bucket exists to write, carrying the
// wait it must come back after; nothing of it reached the broker. Every other outcome is the
// same 401 and the same body every other refused credential gets here, so the answer tells an
// unauthenticated caller nothing about which credentials this listener takes.
func refuseBrokerToken(w http.ResponseWriter, logger *logging.Logger, r *http.Request, brokers *brokerTokens, err error) {
	reason := brokerTokenReason(err)
	logger.Warn("listener: broker token rejected",
		slog.String("reason", reason),
		slog.String("path", r.URL.Path))
	switch {
	case errors.Is(err, errBrokerTokensStarting):
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "service starting")
	case errors.Is(err, ciphertoken.ErrBrokerUnreachable):
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "the secrets broker cannot be reached; try again in a minute")
	case errors.Is(err, ciphertoken.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.Itoa(brokers.retryAfter()))
		writeJSONError(w, http.StatusTooManyRequests, "too many access tokens checked from this address")
	case reason == brokerTokenCheckFailed:
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "this access token could not be checked; try again in a minute")
	default:
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	}
}

// brokerTokenCheckFailed is the class of a presentation this listener could not judge at all:
// its spent-proof store did not answer. It is this listener's own failure, so refuseBrokerToken
// answers it the 503 every other unjudged presentation gets.
const brokerTokenCheckFailed = "check_failed"

// brokerTokenReason is the class one refused presentation goes into the log under, the
// vocabulary an operator filters on. It names what was wrong and never what was presented.
func brokerTokenReason(err error) string {
	switch {
	case errors.Is(err, errBrokerTokensStarting):
		return "starting"
	case errors.Is(err, ciphertoken.ErrNotActive):
		return "not_active"
	case errors.Is(err, ciphertoken.ErrKeyMismatch):
		return "key_mismatch"
	case errors.Is(err, ciphertoken.ErrProofInvalid), errors.Is(err, ciphertoken.ErrNotPresented):
		return "proof_invalid"
	case errors.Is(err, ciphertoken.ErrNotASession):
		return "not_a_session"
	case errors.Is(err, ciphertoken.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ciphertoken.ErrBrokerUnreachable):
		return "unavailable"
	}
	// The residue is this listener's own failure to run the check — its spent-proof store did
	// not answer — which ciphertoken reports as neither the broker's nor the caller's.
	return brokerTokenCheckFailed
}

// servesHost reports whether a request's Host names this listener: its name, port stripped,
// brackets off an IPv6 address, case folded and one trailing dot dropped, is a loopback name or
// address or one of allowedHosts, which config.Load normalizes the same way. An empty Host (an
// HTTP/1.0 request that sent none) names nothing.
func servesHost(host string, allowedHosts []string) bool {
	name := config.HostName(host)
	if name == "" {
		return false
	}
	return isLoopbackHost(name) || slices.Contains(allowedHosts, name)
}

func isAPIAuthExemptPath(path string) bool {
	return path == "/healthz" || path == "/metrics" || strings.HasPrefix(path, "/webhook/")
}

// resolveListenerOIDCConfig binds the listener's two variable names to the
// shared both-or-neither rule. They are configured both or neither: one alone
// is a refusal naming the missing one.
func resolveListenerOIDCConfig(getenv func(string) string) (issuer, audience string, err error) {
	return oidc.ConfigFromEnv(getenv, "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE")
}

// serviceAccountSubjectPrefix begins every subject a projected service-account token carries,
// system:serviceaccount:<namespace>:<name>.
const serviceAccountSubjectPrefix = "system:serviceaccount:"

// resolveListenerOIDCAccounts reads ENVOY_OIDC_ACCOUNTS, the subjects the projected-token branch
// admits, each system:serviceaccount:<namespace>:<name> named exactly, separated by commas or
// white space (splitAccountList). Unset or empty, it answers none, which admits every subject the
// verifier verifies. A value that names no account — only blanks or commas — is a refusal naming
// the setting: it is a list emptied by mistake, and read as unset it would admit every pod of the
// cluster. A list beside no ENVOY_OIDC_ISSUER is a refusal, since it would hold nothing to
// anything, and so is an empty entry or an entry of any other shape, which no token could ever
// carry, so a misspelt list is refused at the start rather than refusing its callers one by one.
func resolveListenerOIDCAccounts(getenv func(string) string, issuer string) ([]string, error) {
	value := getenv("ENVOY_OIDC_ACCOUNTS")
	if value == "" {
		return nil, nil
	}
	if strings.TrimFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) == "" {
		return nil, errors.New("ENVOY_OIDC_ACCOUNTS is set but names no account; list the system:serviceaccount:<namespace>:<name> subjects to admit, or leave it unset to admit every subject the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair verifies")
	}
	accounts, err := splitAccountList("ENVOY_OIDC_ACCOUNTS", value)
	if err != nil {
		return nil, err
	}
	if issuer == "" {
		return nil, errors.New("ENVOY_OIDC_ACCOUNTS needs ENVOY_OIDC_ISSUER and ENVOY_OIDC_AUDIENCE: it names the projected-token callers that pair's verifier admits")
	}
	for _, account := range accounts {
		rest, prefixed := strings.CutPrefix(account, serviceAccountSubjectPrefix)
		namespace, name, found := strings.Cut(rest, ":")
		if !prefixed || !found || namespace == "" || name == "" || strings.Contains(name, ":") {
			return nil, fmt.Errorf("ENVOY_OIDC_ACCOUNTS must be system:serviceaccount:<namespace>:<name> subjects separated by commas or spaces, got %q", account)
		}
	}
	return accounts, nil
}

// resolveListenerGoogleConfig reads ENVOY_OIDC_GOOGLE_AUDIENCE, the audience a caller's Google
// identity token must carry, and ENVOY_OIDC_GOOGLE_ACCOUNTS, the addresses of the accounts it
// admits, separated by commas or white space (splitAccountList), each in lower case, the form
// Google's email claim is compared in. They are set both or neither: an audience no account is
// listed for would admit no caller, and a list with no audience would verify nothing. An empty
// entry, and an entry that is not an address, is a refusal naming it.
func resolveListenerGoogleConfig(getenv func(string) string) (audience string, accounts []string, err error) {
	audience = strings.TrimSpace(getenv("ENVOY_OIDC_GOOGLE_AUDIENCE"))
	if value := fold.ASCII(getenv("ENVOY_OIDC_GOOGLE_ACCOUNTS")); strings.TrimSpace(value) != "" {
		if accounts, err = splitAccountList("ENVOY_OIDC_GOOGLE_ACCOUNTS", value); err != nil {
			return "", nil, err
		}
	}
	switch {
	case audience == "" && len(accounts) == 0:
		return "", nil, nil
	case len(accounts) == 0:
		return "", nil, errors.New("ENVOY_OIDC_GOOGLE_ACCOUNTS is required when ENVOY_OIDC_GOOGLE_AUDIENCE is set: an audience no account is listed for admits no caller")
	case audience == "":
		return "", nil, errors.New("ENVOY_OIDC_GOOGLE_AUDIENCE is required when ENVOY_OIDC_GOOGLE_ACCOUNTS is set: it is the audience the listed accounts' tokens must carry")
	}
	for _, account := range accounts {
		if !oidc.EmailShape.MatchString(account) {
			return "", nil, fmt.Errorf("ENVOY_OIDC_GOOGLE_ACCOUNTS must be Google account addresses separated by commas or spaces, got %q", account)
		}
	}
	return audience, accounts, nil
}

// dispatchPrincipalShape is the refusal of an ENVOY_DISPATCH_PRINCIPAL of neither shape
// (resolveListenerDispatchPrincipal).
const dispatchPrincipalShape = "ENVOY_DISPATCH_PRINCIPAL must be one system:serviceaccount:<namespace>:<name> subject or one Google service account address, got %q"

// resolveListenerDispatchPrincipal reads ENVOY_DISPATCH_PRINCIPAL, Dispatch's own principal: the
// one verified caller whose POST /v1/messages/send may carry a role mark (sendHandler). It is a
// projected service-account subject, system:serviceaccount:<namespace>:<name>, or a Google
// service account's address, compared in lower case as Google's email claim is, and it is the
// principal an identity token of that account proves (withVerifiedPrincipal). Unset names nobody,
// and then every send carrying a role is refused. A value of neither shape refuses the start
// naming it, and so does one no credential this listener takes could prove: a subject beside no
// ENVOY_OIDC_ISSUER pair, or one a set ENVOY_OIDC_ACCOUNTS leaves out, and an address beside no
// Google pair, or one ENVOY_OIDC_GOOGLE_ACCOUNTS does not list. Each of those would refuse every
// role send Dispatch makes, so it is refused at the start rather than one send at a time.
func resolveListenerDispatchPrincipal(getenv func(string) string, issuer string, accounts, googleAccounts []string) (string, error) {
	value := strings.TrimSpace(getenv("ENVOY_DISPATCH_PRINCIPAL"))
	if value == "" {
		return "", nil
	}
	if rest, prefixed := strings.CutPrefix(value, serviceAccountSubjectPrefix); prefixed {
		namespace, name, found := strings.Cut(rest, ":")
		switch {
		case !found || namespace == "" || name == "" || strings.Contains(name, ":") || strings.ContainsFunc(value, unicode.IsSpace):
			return "", fmt.Errorf(dispatchPrincipalShape, value)
		case issuer == "":
			return "", fmt.Errorf("ENVOY_DISPATCH_PRINCIPAL %q is a service-account subject, which only a projected token proves, and needs ENVOY_OIDC_ISSUER and ENVOY_OIDC_AUDIENCE", value)
		case len(accounts) > 0 && !slices.Contains(accounts, value):
			return "", fmt.Errorf("ENVOY_DISPATCH_PRINCIPAL %q is not one of the subjects ENVOY_OIDC_ACCOUNTS admits, so no token of it could be admitted", value)
		}
		return value, nil
	}
	address := fold.ASCII(value)
	switch {
	case !oidc.EmailShape.MatchString(address):
		return "", fmt.Errorf(dispatchPrincipalShape, value)
	case len(googleAccounts) == 0:
		return "", fmt.Errorf("ENVOY_DISPATCH_PRINCIPAL %q is an address, which only a Google identity token proves, and needs ENVOY_OIDC_GOOGLE_AUDIENCE and ENVOY_OIDC_GOOGLE_ACCOUNTS", value)
	case !slices.Contains(googleAccounts, address):
		return "", fmt.Errorf("ENVOY_DISPATCH_PRINCIPAL %q is not one of the accounts ENVOY_OIDC_GOOGLE_ACCOUNTS admits, so no token of it could be admitted", value)
	}
	return address, nil
}

// describeListenerDispatchPrincipal names, for the boot log, whose sends may carry a role mark:
// the principal ENVOY_DISPATCH_PRINCIPAL names, or nobody's.
func describeListenerDispatchPrincipal(principal string) string {
	if principal == "" {
		return "refused from every caller (ENVOY_DISPATCH_PRINCIPAL is unset)"
	}
	return "accepted from " + principal + " alone"
}

// splitAccountList is the one rule both account lists are read by: commas and white space alike
// separate entries, so "A, B", "A,B" and "A B" each name A and B, as ENVOY_ALLOWED_HOSTS beside
// them is written with commas. White space never makes an empty entry; a comma with nothing but
// white space before it, after it or beside another comma does, and that is a refusal naming the
// setting and which entry is empty, rather than a list read past what its writer meant. Every
// entry it answers is non-empty and holds no comma and no white space, the separators themselves,
// so no entry can be one no token carries for want of a split.
func splitAccountList(setting, value string) ([]string, error) {
	pieces := strings.Split(value, ",")
	var entries []string
	for i, piece := range pieces {
		fields := strings.Fields(piece)
		if len(fields) == 0 {
			return nil, fmt.Errorf("%s holds an empty entry, number %d of the %d its commas separate in %q; separate its entries with one comma or with spaces, and end it with no comma", setting, i+1, len(pieces), value)
		}
		entries = append(entries, fields...)
	}
	return entries, nil
}

// describeListenerAPIAuth names the credentials /v1 accepts, for the boot log: the projected-token
// issuer with the subjects ENVOY_OIDC_ACCOUNTS holds it to, the Google audience with its accounts,
// and the broker. Broker tokens are named beside the bearers, and an operator reading the line
// beside the `listener API hosts:` one below gets the whole picture: a loopback listener that
// accepts only broker tokens still serves /v1 with no credential, since there that credential says
// who is calling rather than whether they may, while off loopback it is the credential required.
func describeListenerAPIAuth(auth listenerAPIAuth, issuer string) string {
	named := make([]string, 0, 3)
	if issuer != "" {
		described := "oidc " + issuer
		if len(auth.accounts) > 0 {
			described += " for " + strings.Join(auth.accounts, ", ")
		}
		named = append(named, described)
	}
	if auth.google != nil {
		named = append(named, "google "+auth.google.audience+" for "+strings.Join(auth.google.accounts, ", "))
	}
	if described := auth.brokers.describe(); described != "" {
		named = append(named, described)
	}
	if len(named) == 0 {
		return "disabled"
	}
	return strings.Join(named, " and ")
}

// describeListenerAPIHosts names the Hosts /v1 is served to, for the boot log: any Host when a
// credential is required (requiresCredential), loopback and ENVOY_ALLOWED_HOSTS otherwise. A list
// set beside a required credential is said to be unused rather than left to look as if it applied.
func describeListenerAPIHosts(auth listenerAPIAuth) string {
	if auth.requiresCredential() {
		if len(auth.allowedHosts) > 0 {
			return "any (a credential is required, so ENVOY_ALLOWED_HOSTS is unused)"
		}
		return "any (a credential is required)"
	}
	if len(auth.allowedHosts) == 0 {
		return "loopback"
	}
	return "loopback, " + strings.Join(auth.allowedHosts, ", ")
}

// validateListenerAPIAuth refuses a listener on an address other than loopback that takes none of
// the three credentials it verifies (listenerCredentials): the OIDC pair, the Google pair, or the
// broker settings, which off loopback are a credential like either bearer. Off loopback /v1 is
// closed to a caller that presents no credential (requiresCredential), so such a listener would
// admit nobody. It judges cfg.ListenHost, which with ENVOY_LISTEN_FD is the inherited socket's own
// address, so a socket on every interface is refused as ENVOY_LISTEN_HOST=0.0.0.0 is; main judges
// the same address into listenerAPIAuth.offLoopback.
//
// Nothing else passes such a listener: ENVOY_API_ALLOW_UNAUTHENTICATED, which once did, refuses
// the start (refuseRemovedAPISettings). The clustered hub's session listener, which it opened,
// takes the broker settings instead, and a hub listener that pods of the cluster reach takes the
// ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair with ENVOY_OIDC_ACCOUNTS.
func validateListenerAPIAuth(cfg config.Service, auth listenerAPIAuth) error {
	if isLoopbackHost(cfg.ListenHost) || auth.verifier != nil || auth.google != nil || auth.brokers != nil {
		return nil
	}
	listen := fmt.Sprintf("ENVOY_LISTEN_HOST=%q is not loopback", cfg.ListenHost)
	if cfg.ListenSocket != nil {
		listen = cfg.ListenSocket.String() + ", is not on loopback"
	}
	return fmt.Errorf("%s is required when %s: off loopback /v1 serves only a caller that presents a credential this listener verifies. "+
		"Bind ENVOY_LISTEN_HOST to loopback for a listener that serves callers with no credential", listenerCredentials, listen)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
