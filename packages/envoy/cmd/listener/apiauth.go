package main

import (
	"context"
	"crypto/subtle"
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
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/listenerwire"
)

const bearerPrefix = "Bearer "

// The credential label of envoy_listener_requests_total: which credential admitted a request.
// none is the open /v1, which admits a request that presents no credential it knows, and
// none_broker_token is the open /v1 admitting a request that presents a broker token on a
// listener that takes none (brokers is nil), so serves it unverified: the gate on the open /v1
// counts it apart from a bare request, since a caller that already holds a broker token is served
// as one only once its listener takes that credential.
const (
	credentialSharedToken     = "shared_token"
	credentialNone            = "none"
	credentialNoneBrokerToken = "none_broker_token"
	credentialOIDC            = "oidc"
	credentialGoogle          = "google"
	credentialBroker          = "broker"
)

// The warnings an admission on one of the credentials the shared token's removal waits on logs,
// and how often each logs at most (admissionWarnings).
const (
	sharedTokenAdmittedMessage      = "listener: request admitted on the shared token"
	openAdmittedMessage             = "listener: request admitted with no credential"
	unverifiedBrokerAdmittedMessage = "listener: request admitted with a broker token this listener does not verify"
	admissionWarnEvery              = time.Minute
)

// newListenerRequests registers envoy_listener_requests_total, the census the removal of the
// shared token and of the open /v1 is gated on: credential="shared_token" and credential="none"
// must read zero on every deployed listener for 72 hours before they go, and
// credential="none_broker_token" on every listener but the clustered session listeners, which the
// removal begins to verify broker tokens on.
func newListenerRequests(met *metrics.Registry) *metrics.Counter {
	return met.NewCounter("envoy_listener_requests_total",
		"Requests /v1 admitted, by the credential that admitted each (shared_token, none, none_broker_token, oidc, google, broker) and the route it reached")
}

// listenerAPIAuth is everything apiAuth admits a /v1 caller with, and where it records each caller
// it admits.
type listenerAPIAuth struct {
	// token is ENVOY_API_TOKEN, the shared bearer; empty takes none.
	token string
	// verifier is the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair's, nil when the pair is unset. It
	// reads its issuer when first needed, so until it has, a projected token is answered 503.
	verifier *oidc.LazyVerifier
	// accounts is ENVOY_OIDC_ACCOUNTS, the subjects the projected-token branch admits. Empty admits
	// every subject verifier verifies, as the branch did before the list existed.
	accounts []string
	// google is ENVOY_OIDC_GOOGLE_AUDIENCE and ENVOY_OIDC_GOOGLE_ACCOUNTS's, nil when they are unset.
	google *googleIdentities
	// brokers takes access tokens the secrets broker issued; nil on a listener that takes none.
	brokers *brokerTokens
	// edgeProjects is ENVOY_EDGE_PROJECTS (config.Service.EdgeProjects): the projects an edge
	// listener serves, empty on every other listener. Non-empty, /v1 admits a broker token alone,
	// and only one EdgeAdmits admits (project_scope.go).
	edgeProjects []string
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

// requiresBearer reports whether /v1 takes a bearer, which is what closes it to a caller that
// presents none. An edge listener always does: it admits a broker token alone.
func (a listenerAPIAuth) requiresBearer() bool {
	return a.token != "" || a.verifier != nil || a.google != nil || len(a.edgeProjects) > 0
}

// googleIdentities admits a caller by its own Google identity token, the credential of a caller
// that runs on Google's platform and holds no Kubernetes token (the alert sink on Cloud Run): a
// token Google signed for audience, naming an account accounts lists whose address Google
// verified.
type googleIdentities struct {
	// audience is ENVOY_OIDC_GOOGLE_AUDIENCE, the audience the caller asks its metadata server for.
	audience string
	// verifier verifies a token Google (oidc.GoogleIssuer) signed for audience; apiAuth refuses
	// Google identities without one. It reads Google's discovery document when first needed, so
	// until it has, a Google identity token is answered 503.
	verifier *oidc.LazyVerifier
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
	var claims oidc.Claims
	check, err := identityCheck(r, g.verifier)
	if err == nil {
		claims, err = g.verifier.VerifyGoogleIdentity(check, raw)
	}
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
	var claims oidc.Claims
	check, err := identityCheck(r, a.verifier)
	if err == nil {
		claims, err = a.verifier.Verify(check, raw)
	}
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

// identityBudget bounds the whole of an identity token's check against its issuer, from the moment
// the check begins, as the request arrives: the wait for the issuer's discovery document and then
// the wait for its key set,
// together. Each read can take longer on its own (an issuer that accepts the connection and never
// answers holds a discovery attempt for 10 s and a key-set read for oidc.DefaultKeysFetchTimeout,
// 10 s too), and one after the other they can pass the 10 s newHTTPServer gives a route to write
// its answer, which would cut the request off with no answer at all rather than the 503
// refuseIdentityToken gives. It is below the 5 s Dispatch's client gives a call to the listener,
// so Dispatch reads the 503 rather than its own timeout. A read the budget cuts short carries on
// in the background and is kept for the next request.
const identityBudget = 4 * time.Second

// identityCheck waits, until identityBudget after it begins at the latest, for verifier
// to have read its issuer's discovery document, reading it if no attempt is under way, and answers
// the context to verify the token on: the request's own, with the rest of that budget as its keys
// deadline (oidc.WithKeysDeadline). Its error matches oidc.ErrIssuerUnavailable. The budget never
// ends that context itself: a verification whose context ends is oidc.ErrCancelled, which is a
// 401, while a key read cut short by the keys deadline is oidc.ErrIssuerUnavailable, the 503.
func identityCheck(r *http.Request, verifier *oidc.LazyVerifier) (context.Context, error) {
	deadline := time.Now().Add(identityBudget)
	if !verifier.Discovered() {
		wait, cancel := context.WithDeadline(r.Context(), deadline)
		err := verifier.Discover(wait)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	return oidc.WithKeysDeadline(r.Context(), deadline), nil
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
// rather than the token's. A 401 would tell Dispatch's client its account token was refused, and
// that client makes a refused call again on the shared token with no role mark (cmd/dispatch's
// listener client), so an issuer outage would send Dispatch's role-route messages unmarked, where
// no takeover copies them for the role's next holder. Every other outcome is the 401 every other
// refused credential gets here.
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
// token names. No other credential proves one: the shared token is any holder's, a broker token
// names a session (brokerCaller), and a request with no credential names nobody.
func withVerifiedPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, verifiedPrincipalKey{}, principal)
}

// verifiedPrincipal is the principal r's identity token proved (withVerifiedPrincipal), empty for
// a request admitted on any other credential, or on none.
func verifiedPrincipal(r *http.Request) string {
	principal, _ := r.Context().Value(verifiedPrincipalKey{}).(string)
	return principal
}

// admissionWarnings logs an admission on the shared token, on no credential at all, or on a broker
// token this listener serves unverified once a minute for each, naming the route and the caller's
// address, so an operator can find the callers envoy_listener_requests_total counts there before
// those credentials go. Each line says how many admissions of its credential it held back since
// the last line. The other credentials stay out of the log: they are the ones a caller is meant to
// hold.
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
	case credentialSharedToken:
		message = sharedTokenAdmittedMessage
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

// apiAuth requires JSON bodies for /v1, then authenticates it with the shared ENVOY_API_TOKEN
// bearer, with an access token the secrets broker issued when one is presented under the DPoP
// scheme (brokers, brokerauth.go), when a verifier is configured with a projected service-account
// token issued to its issuer and audience, of a subject ENVOY_OIDC_ACCOUNTS lists when it lists
// any, and when Google identities are configured with a Google identity token of a listed account
// (googleIdentities). Exempt paths pass in every configuration; /v1 is open only when no bearer
// kind is configured, so an empty shared token beside a configured verifier requires a token
// rather than opening the tree. An open /v1 serves only a request whose Host names this listener
// (servesHost: a loopback name or address, or one of allowedHosts), so a page that rebinds its own
// name to an address reaching the listener cannot use it; a listener that requires a bearer
// checks no Host, since such a page holds no bearer. A bearer the verifier rejects is answered
// 401 and never falls back to another authentication path; the failure class goes to the log and
// never to the caller, since naming it would tell an unauthenticated client which credential the
// listener is configured for.
//
// The two identity-token bearers are told apart by the token's own issuer
// (oidc.LazyVerifier.Issues), including Google's scheme-less issuer spelling: a token
// Google issued goes to the Google branch, any other to the projected-token one, and a listener
// that takes only one of them gives it every token, so each refusal is logged with its class and
// no token is judged twice.
//
// Every request it admits is counted in envoy_listener_requests_total under the credential that
// admitted it and the route it reached (route), and an admission on the shared token, on none at
// all or on a broker token this listener does not verify also logs a warning once a minute
// (admissionWarnings): those three are what the removal of the shared token and of the open /v1
// is gated on. A request admitted on an identity token carries the principal that token proved to
// the handler (withVerifiedPrincipal, verifiedPrincipal), which is how the send route tells
// Dispatch's own sends from every other caller's.
//
// The broker-token branch is taken only on a listener that takes this credential (brokers is
// nil on one that does not), and there the scheme alone decides it, before every bearer one:
// RFC 9449 Section 7.2 refuses a DPoP-bound token presented under any other scheme, so a DPoP
// presentation is this credential whatever the listener's other settings are, and is never
// retried as a bearer. It comes before the open pass-through too, which is what binds a session
// on an open listener: a caller that presents a broker token is held to the session it names
// even where /v1 would have served the request with no credential at all. A token that passes
// every check but whose broker answers its person held is refused 401 MEMBERSHIP_HELD
// (refuseHeldBrokerToken), and served again once the broker answers otherwise; one whose broker
// answers its person not held only until a deadline that has passed is 503 BROKER_UNAVAILABLE
// (refuseLapsedBrokerToken), and served again once the broker answers a later deadline.
//
// A listener with none of the broker variables set reads that header as it read every
// Authorization header it did not know before this credential existed: the open listener serves
// the request, and a listener that requires a bearer answers it the same 401 it always did.
// That is what keeps the no-bearer listeners the GCP VMs and the clustered hub run serving the
// clients that hold a broker route of their own. A VM's listener is on loopback, and the clustered
// hub's tailnet-only listener (a tailnet.listener with no clientNamespaces) serves off loopback
// behind its NetworkPolicy; neither takes a bearer. A clustered hub listener that pods of the
// cluster reach takes the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair, so a caller that presents
// only DPoP gets its 401. A client sends Authorization: DPoP whenever it has a broker and an
// identity to trade and no Envoy bearer is set, whatever the listener it is calling takes. The
// open listener counts such a request as none_broker_token rather than none, so the gate on the
// open /v1 sees which of its callers already present a broker token it does not verify.
//
// Configuring it opens nothing and closes nothing. A request presenting no token is judged
// exactly as it was before this credential existed — the open listener still serves it, the Host
// and Content-Type fences still apply, and validateListenerAPIAuth still refuses a non-loopback
// listener with no bearer and no ENVOY_API_ALLOW_UNAUTHENTICATED — because a broker token is a
// credential a caller may hold rather than one this listener can require: every client of /v1
// that holds none would be locked out by a setting whose purpose is to tell callers apart.
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
	open := !auth.requiresBearer()
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
		r, read := auth.readEdgeCaller(w, r)
		if !read {
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
		_, presented := ciphertoken.Presented(r)
		if auth.brokers != nil && presented {
			// An edge listener gives a body-less request its proof back on a 503
			// (proofReleasingWriter), so the one retry Google's load balancer sends of it is not
			// refused as a replay. No other listener has that balancer in front of it.
			var spend *proofSpend
			if r.ContentLength == 0 && len(auth.edgeProjects) > 0 {
				spend = &proofSpend{}
				r = r.WithContext(withProofSpend(r.Context(), spend))
			}
			caller, err := auth.brokers.verify(r)
			if err != nil {
				refuseBrokerToken(w, auth.logger, r, auth.brokers, err)
				return
			}
			switch {
			case caller.Held:
				refuseHeldBrokerToken(w, auth.logger, r, caller)
				return
			case auth.brokers.lapsed(caller):
				refuseLapsedBrokerToken(w, auth.logger, r)
				return
			}
			ctx := withBrokerCaller(r.Context(), caller)
			if len(auth.edgeProjects) > 0 {
				if err := EdgeAdmits(auth.edgeProjects, caller); err != nil {
					auth.logger.Warn("listener: edge listener refused a broker token",
						slog.String("session", caller.Session),
						slog.String("reason", err.Error()),
						slog.Any("served", auth.edgeProjects),
						slog.String("path", r.URL.Path))
					writeJSON(w, http.StatusForbidden, listenerwire.EdgeRefusal{Reason: edgeTokenNotAdmitted, Error: err.Error()})
					return
				}
				ctx = withEdgeHold(ctx, auth.logger)
			}
			if spend != nil {
				w = &proofReleasingWriter{ResponseWriter: w, r: r, spend: spend, logger: auth.logger}
			}
			admit(w, r.WithContext(ctx), credentialBroker)
			return
		}
		if open {
			if presented {
				admit(w, r, credentialNoneBrokerToken)
				return
			}
			admit(w, r, credentialNone)
			return
		}
		// An edge listener admits a broker token alone (EdgeAdmits): whatever else a request
		// presents, it is refused here as an unknown credential is, before any bearer is compared.
		if len(auth.edgeProjects) > 0 {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		authorization := r.Header.Get("Authorization")
		if auth.token != "" && subtle.ConstantTimeCompare(
			[]byte(authorization),
			[]byte(bearerPrefix+auth.token),
		) == 1 {
			admit(w, r, credentialSharedToken)
			return
		}

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

// readEdgeCaller is r as an edge listener reads it before anything else of it: carrying the
// address Google's load balancer forwarded for its caller (withCallerAddress), which the broker
// token's introspection bucket keys on (listenerSourceAddress), so the address is read once,
// here. It reports false when it refused r, a request from the balancer's proxies that names no
// such address (edgeCallerAddress), with the 400 and the log line that says why. Every other
// listener's r is answered as it is.
func (a listenerAPIAuth) readEdgeCaller(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if len(a.edgeProjects) == 0 {
		return r, true
	}
	address, forwarded, err := edgeCallerAddress(r)
	if err != nil {
		a.logger.Warn("listener: edge request without the address Google's load balancer forwards",
			slog.String("reason", err.Error()),
			slog.String("path", r.URL.Path),
			slog.String("remote", r.RemoteAddr))
		writeJSONError(w, http.StatusBadRequest, edgeForwardedRefusal)
		return nil, false
	}
	if forwarded {
		r = r.WithContext(withCallerAddress(r.Context(), address))
	}
	return r, true
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
//
// None of the 503s here comes after the proof is spent, so a retry of the same request with the
// same proof, the one Google's load balancer sends after a 503, is judged afresh: the broker is
// asked before the proof is checked (ciphertoken.Verify), and a spent-proof store that did not
// answer recorded nothing the listener knows of. That store's create may still land after it
// stopped waiting, and the retry then meets it as a replay; proofReleasingWriter says why no
// release follows it.
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

// refuseHeldBrokerToken answers a broker token that passed every check of this listener, the
// proof and the key among them, whose broker answered its person held (ciphertoken.Caller.Held):
// Dispatch holds that person's credentials until it confirms them again, by their next sign-in
// under a Google rule or the Workspace Directory's next active answer under Identity-Aware Proxy.
// The answer carries no rule and the listener knows none, so it is 401 MEMBERSHIP_HELD in words
// true under both, naming the person; the token is still live, so a client keeps it and the same
// token is served once the broker answers otherwise. It logs the rejection line every refused
// presentation writes, under the class membership_held.
func refuseHeldBrokerToken(w http.ResponseWriter, logger *logging.Logger, r *http.Request, caller ciphertoken.Caller) {
	logger.Warn("listener: broker token rejected",
		slog.String("reason", relaySocketReasonMembershipHeld),
		slog.String("path", r.URL.Path))
	writeJSON(w, http.StatusUnauthorized, listenerwire.APIError{Code: "MEMBERSHIP_HELD", Error: fmt.Sprintf(
		"Dispatch holds %s's credentials; they work again once Dispatch confirms them", caller.Person)})
}

// refuseLapsedBrokerToken answers a broker token that passed every check of this listener whose
// broker answered its person not held only until a deadline that has passed
// (ciphertoken.Verifier.Lapsed): an IAP deployment's Dispatch has had no active Workspace Directory
// answer for that person in 24 hours, so whether they may still act cannot be read. It is 503
// BROKER_UNAVAILABLE with Retry-After 60, as its words ask, and never a refusal of the token, which
// is served again once the broker answers a later deadline (Dispatch reads no deadline yet, and
// serves such a token). It logs the rejection line every refused presentation writes, under the
// class deadline_passed.
func refuseLapsedBrokerToken(w http.ResponseWriter, logger *logging.Logger, r *http.Request) {
	logger.Warn("listener: broker token rejected",
		slog.String("reason", relaySocketReasonDeadlinePassed),
		slog.String("path", r.URL.Path))
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusServiceUnavailable, listenerwire.APIError{Code: "BROKER_UNAVAILABLE",
		Error: "whether this token's person may still act could not be read; try again in a minute"})
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

// describeListenerAPIAuth names the credentials /v1 accepts, for the boot log: the shared token,
// the projected-token issuer with the subjects ENVOY_OIDC_ACCOUNTS holds it to, the Google
// audience with its accounts, and the broker. Broker tokens are named beside the bearers, and an
// operator reading the line beside the `listener API hosts:` one below gets the whole picture: a
// listener that accepts only broker tokens still serves /v1 with no credential, since that
// credential says who is calling rather than whether they may.
func describeListenerAPIAuth(auth listenerAPIAuth, issuer string) string {
	named := make([]string, 0, 4)
	if auth.token != "" {
		named = append(named, "shared token")
	}
	if issuer != "" {
		described := "oidc " + redact.URL(issuer)
		if len(auth.accounts) > 0 {
			described += " for " + strings.Join(auth.accounts, ", ")
		}
		named = append(named, described)
	}
	if auth.google != nil {
		named = append(named, "google "+auth.google.audience+" for "+strings.Join(auth.google.accounts, ", "))
	}
	if described := auth.brokers.describe(); described != "" {
		if len(auth.edgeProjects) > 0 {
			described += " alone, as the edge listener of " + strings.Join(auth.edgeProjects, ", ")
		}
		named = append(named, described)
	}
	if len(named) == 0 {
		return "disabled"
	}
	return strings.Join(named, " and ")
}

// describeListenerAPIHosts names the Hosts /v1 is served to, for the boot log: any Host when a
// bearer is required, loopback and ENVOY_ALLOWED_HOSTS otherwise. A list set beside a bearer is
// said to be unused rather than left to look as if it applied.
func describeListenerAPIHosts(auth listenerAPIAuth) string {
	if auth.requiresBearer() {
		if len(auth.allowedHosts) > 0 {
			return "any (a bearer is required, so ENVOY_ALLOWED_HOSTS is unused)"
		}
		return "any (a bearer is required)"
	}
	if len(auth.allowedHosts) == 0 {
		return "loopback"
	}
	return "loopback, " + strings.Join(auth.allowedHosts, ", ")
}

// validateListenerAPIAuth refuses a listener that would serve /v1 with no credential on an address
// other than loopback. It judges cfg.ListenHost, which with ENVOY_LISTEN_FD is the inherited
// socket's own address, so a socket on every interface is refused as ENVOY_LISTEN_HOST=0.0.0.0 is.
//
// ENVOY_API_ALLOW_UNAUTHENTICATED=1 passes such a listener, and it is how an operator states that
// something else fences the port. The clustered hub's tailnet-only session listener (a
// tailnet.listener with no clientNamespaces) is the one deployment here that sets it: it binds
// 0.0.0.0 in a pod of its own, and its NetworkPolicy admits TCP 9020 from the tailnet entry's pods
// alone (infra/gcp/clusteredhub_listener.go). A hub listener that pods of the cluster reach sets
// the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair and ENVOY_OIDC_ACCOUNTS instead. The flag opens
// nothing else: an open /v1 still serves only the Hosts servesHost admits, and answers 421 to the
// rest.
func validateListenerAPIAuth(cfg config.Service, auth listenerAPIAuth, allowUnauthenticated string) error {
	if auth.requiresBearer() || allowUnauthenticated == "1" || isLoopbackHost(cfg.ListenHost) {
		return nil
	}
	listen := fmt.Sprintf("ENVOY_LISTEN_HOST=%q is not loopback", cfg.ListenHost)
	if cfg.ListenSocket != nil {
		listen = cfg.ListenSocket.String() + ", is not on loopback"
	}
	return fmt.Errorf("ENVOY_API_TOKEN, the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair or the ENVOY_OIDC_GOOGLE_AUDIENCE/ENVOY_OIDC_GOOGLE_ACCOUNTS pair is required when %s. Set ENVOY_API_ALLOW_UNAUTHENTICATED=1 only where something else fences the port. The clustered hub's tailnet-only session listener sets it, behind a NetworkPolicy that admits the tailnet entry's pods alone; one that pods of the cluster reach takes the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair with ENVOY_OIDC_ACCOUNTS", listen)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
