package main

// brokerauth.go is the listener's third credential: an access token the secrets broker issued,
// presented under RFC 9449's DPoP scheme (Authorization: DPoP <token> with a DPoP: proof header).
// internal/ciphertoken decides it; what is here is the settings a deployment configures it with,
// the spent-proof store it needs, how a decided caller reaches the handlers, and which requests
// are held to the session that caller names.
//
// It is the one credential that says who is calling. The shared token and the OIDC pair say only
// that the caller holds a credential this listener trusts, so a request under either names its
// own session in its body and is believed; a broker token names the session the broker issued it
// to, so a write that acts as a session is held to that one (sessionNotBound).

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/redact"
)

// The variables a listener takes broker access tokens on, beside ENVOY_API_TOKEN and the
// ENVOY_OIDC_* pair. They are one group: a listener that names any of them must name the broker,
// an origin and exactly one account identity, since a half-configured credential would otherwise
// be a listener that quietly takes none (resolveListenerBrokerConfig).
const (
	// EnvBrokerURL is the secrets broker's base URL, the origin the RFC 7662 introspection route
	// is asked on. Setting it is what turns the credential on; unset, the listener takes no
	// broker token at all.
	EnvBrokerURL = "ENVOY_AGENT_SECRETS_URL"
	// EnvBrokerAccountTokenFile is the file holding this listener's own projected Kubernetes
	// ServiceAccount token, the bearer each introspection presents. A listener in a pod sets it.
	EnvBrokerAccountTokenFile = "ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"
	// EnvBrokerGoogleAudience is the audience this listener's own Google identity token is
	// minted for, read from the Compute Engine metadata server at call time
	// (internal/googleidentity). A listener on a VM, which holds no Kubernetes token, sets this
	// instead.
	EnvBrokerGoogleAudience = "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE"
	// EnvPublicURL is this listener's own public origin: the absolute http(s) URL, with no path,
	// that a caller reaches /v1 at. A proof's 'htu' is checked against it plus the request's
	// path and never against a forwarded header, so a caller behind a proxy cannot choose the
	// URL its proof names.
	EnvPublicURL = "ENVOY_PUBLIC_URL"
)

// listenerBrokerConfig is what the four variables above say. The zero value is a listener that
// takes no broker token.
type listenerBrokerConfig struct {
	BrokerURL        string
	AccountTokenFile string
	GoogleAudience   string
	Origin           string
}

// configured reports whether this listener takes broker access tokens at all.
func (c listenerBrokerConfig) configured() bool { return c.BrokerURL != "" }

// account is the platform identity each introspection presents: the projected token file, or
// this machine's Google identity token for the audience named. resolveListenerBrokerConfig has
// already refused a configuration naming neither or both.
func (c listenerBrokerConfig) account() ciphertoken.Account {
	if c.AccountTokenFile != "" {
		return ciphertoken.NewAccountToken(EnvBrokerAccountTokenFile, c.AccountTokenFile)
	}
	return ciphertoken.NewGoogleAccount(c.GoogleAudience)
}

// resolveListenerBrokerConfig binds the listener's four variable names to one all-or-nothing
// rule, as resolveListenerOIDCConfig binds its pair: none of them set is a listener that takes no
// broker token, and any of them set requires the broker URL, the origin, and exactly one account
// identity. Each refusal names the variable that is missing or the two that cannot both be set.
//
// The account identity is one or the other because the two are different platforms' answers to
// the same question — which service is calling — and a listener that read both would have to
// decide which identity the broker is to resolve it as, a decision the deployment makes by
// setting one.
func resolveListenerBrokerConfig(getenv func(string) string) (listenerBrokerConfig, error) {
	config := listenerBrokerConfig{
		BrokerURL:        strings.TrimSpace(getenv(EnvBrokerURL)),
		AccountTokenFile: strings.TrimSpace(getenv(EnvBrokerAccountTokenFile)),
		GoogleAudience:   strings.TrimSpace(getenv(EnvBrokerGoogleAudience)),
		Origin:           strings.TrimSpace(getenv(EnvPublicURL)),
	}
	if config == (listenerBrokerConfig{}) {
		return listenerBrokerConfig{}, nil
	}
	if config.BrokerURL == "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s is required to take broker access tokens, which %s, %s or %s asks for",
			EnvBrokerURL, EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvPublicURL)
	}
	if config.AccountTokenFile != "" && config.GoogleAudience != "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s and %s are two identities to call %s as; set one",
			EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvBrokerURL)
	}
	if config.AccountTokenFile == "" && config.GoogleAudience == "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s or %s is required when %s is set: the broker resolves the calling service from the identity token each introspection presents",
			EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvBrokerURL)
	}
	if config.Origin == "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s is required when %s is set: a DPoP proof's target URI is checked against this listener's own origin", EnvPublicURL, EnvBrokerURL)
	}
	return config, nil
}

// errBrokerTokensStarting is a DPoP presentation that reaches apiAuth before the spent-proof
// bucket is open. It is the starting gate's 503 rather than a 401: the credential is configured
// and the listener cannot judge it yet, and a caller told its token was refused would discard a
// live one. main's /v1 answers the closed gate before apiAuth (v1API) and opens the bucket before
// the gate, so there it is not reached; wherever it is, it spends no proof.
var errBrokerTokensStarting = errors.New("the listener is still opening the store this credential needs")

// brokerTokens is the listener's broker-token credential as apiAuth reads it: nil on a listener
// that takes none, and otherwise a holder whose verifier is set once the spent-proof bucket is
// open (open). It is a holder rather than the verifier itself because /v1 is mounted before NATS
// is connected (main), while the verifier cannot exist before the bucket it records spent proofs
// in does.
type brokerTokens struct {
	config   listenerBrokerConfig
	verifier atomic.Pointer[ciphertoken.Verifier]
}

// newBrokerTokens is the credential config names, or nil when it names none.
func newBrokerTokens(config listenerBrokerConfig) *brokerTokens {
	if !config.configured() {
		return nil
	}
	return &brokerTokens{config: config}
}

// open opens the spent-proof bucket on conn's JetStream (ensureSpentProofBucket) and, on a listener
// that takes broker tokens, builds the verifier over it. Every listener opens the bucket at its
// start, whether or not it takes broker tokens: an edge listener's grant creates no bucket
// (natsgrant.Edge), so it finds the one a full listener of this build made. A listener that takes
// none and cannot open it refuses to start as every listener refuses a bucket of its own; one
// that takes broker tokens on a bus with no JetStream fails here, naming EnvBrokerURL, rather than
// falling back to a store in its own memory, which would let one replica accept a proof another
// replica had already spent. ctx is the start's (bus.EnsureKeyValue).
func (b *brokerTokens) open(ctx context.Context, conn *nats.Conn, replicas int) error {
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		if b == nil {
			return fmt.Errorf("JetStream for the spent-proof bucket %s: %w", bus.DPoPProofBucket, err)
		}
		return fmt.Errorf("%s needs JetStream for the spent-proof bucket %s: %w", EnvBrokerURL, bus.DPoPProofBucket, err)
	}
	kv, err := ensureSpentProofBucket(ctx, conn, js, replicas)
	if b == nil {
		if err != nil {
			return fmt.Errorf("the spent-proof bucket %s, which every listener opens so an edge listener finds it, did not open: %w", bus.DPoPProofBucket, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s needs the spent-proof bucket %s, which this bus did not open: %w", EnvBrokerURL, bus.DPoPProofBucket, err)
	}
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL: b.config.BrokerURL,
		Account:   b.config.account(),
		Origins:   []string{strings.TrimSuffix(b.config.Origin, "/")},
		Replay:    spentProofs{kv: kv},
		// The listener trusts a forwarding header for this only where it is the edge behind
		// Google's load balancer, and only from that balancer's own proxies (edgeCallerAddress,
		// which apiAuth reads): a bucket keyed on a header any caller writes bounds nothing.
		// Everywhere else the connection is what is left, which is right for the listener's other
		// deployments, where /v1 is reached directly.
		Address: listenerSourceAddress,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", EnvBrokerURL, err)
	}
	b.verifier.Store(verifier)
	return nil
}

// ensureSpentProofBucket opens the spent-proof bucket, creating it when the server holds none and
// raising it to replicas when it holds fewer copies (bus.EnsureKeyValue): bus.DPoPProofBucket, file
// storage, replicas copies (ENVOY_NATS_REPLICAS), and spentProofRetention as each key's age. Its
// other settings are nats.go's defaults for a bucket: one value per key and no size limit. A bucket
// found with another age, history or storage is opened as it is.
func ensureSpentProofBucket(ctx context.Context, conn *nats.Conn, js nats.JetStreamContext, replicas int) (bus.KeyValue, error) {
	return bus.EnsureKeyValue(ctx, conn, js, &nats.KeyValueConfig{
		Bucket:   bus.DPoPProofBucket,
		TTL:      spentProofRetention,
		Storage:  nats.FileStorage,
		Replicas: replicas,
	})
}

// verify decides the caller of r, and is the whole of what apiAuth asks of this credential. It is
// reached only on a listener that takes broker tokens (apiAuth checks that before it reads the
// scheme), so the one state it still answers for itself is a holder whose spent-proof bucket is
// not open yet.
func (b *brokerTokens) verify(r *http.Request) (ciphertoken.Caller, error) {
	verifier := b.verifier.Load()
	if verifier == nil {
		return ciphertoken.Caller{}, errBrokerTokensStarting
	}
	return verifier.Verify(r.Context(), r)
}

// lapsed reports whether caller, one verify answered, is a not-held answer whose deadline has
// passed by the verifier's own clock (ciphertoken.Verifier.Lapsed), which apiAuth refuses as
// unavailable.
func (b *brokerTokens) lapsed(caller ciphertoken.Caller) bool {
	verifier := b.verifier.Load()
	return verifier != nil && verifier.Lapsed(caller)
}

// retryAfter is the wait a rate-limited caller is given, in seconds.
func (b *brokerTokens) retryAfter() int {
	if verifier := b.verifier.Load(); verifier != nil {
		return ratelimit.RetryAfterSeconds(verifier.RetryAfter())
	}
	return 1
}

// describeBrokerTokens names this credential for the boot line: the broker it asks, through
// redact.URL, and the identity it asks as, the token file by the setting that names it, and nothing
// of either credential's bytes or of the file's path, where a token pasted in its place would be.
func (b *brokerTokens) describe() string {
	if b == nil {
		return ""
	}
	identity := "google audience " + b.config.GoogleAudience
	if b.config.AccountTokenFile != "" {
		identity = "the projected token " + EnvBrokerAccountTokenFile + " names"
	}
	return "broker tokens at " + redact.URL(b.config.BrokerURL) + " as " + identity
}

// listenerSourceAddress is the address one introspection bucket is keyed on: the address an edge
// listener read for the caller from Google's load balancer (withCallerAddress), and otherwise the
// connection's own, with its port dropped so a caller's every connection shares one bucket.
func listenerSourceAddress(r *http.Request) string {
	if address, ok := r.Context().Value(callerAddressKey{}).(string); ok {
		return address
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// spentProofRetention is how long one spent proof's record is kept: the whole window the proof it
// records can still be presented in, and a minute more.
//
// A proof is accepted while now is between its 'iat' less ciphertoken.ProofClockSkew and its
// 'iat' plus ciphertoken.ProofLifespan plus that skew, so a proof recorded now can be presented
// again for at most two skews and a lifespan from this moment, whatever its 'iat'. The extra
// minute covers the skew between the listener that recorded it and the one that reads it, as the
// margin on Dispatch's own record of the same thing does (store.jtiRetentionMargin).
//
// It is the bucket's TTL rather than a per-key one because every key means the same thing and
// lives the same span: nothing reads a record back, so a key whose window has passed says
// nothing, and letting it expire costs no delete.
const spentProofRetention = 2*ciphertoken.ProofClockSkew + ciphertoken.ProofLifespan + time.Minute

// spentProofs is the listener's rfc9449.DPoPReplayStorage: one key per proof already spent, in
// the KV bucket every listener of a deployment shares (bus.DPoPProofBucket), so a proof captured
// from one listener cannot be presented to another inside its window. The listener has no
// database; this is the store it does have, and the one it must have — a store in one process's
// memory would make a replica's first sight of a proof its first.
type spentProofs struct {
	kv bus.KeyValue
}

// CheckAndSetDPoPProofUsed records a proof as spent and reports whether it already was, which the
// bucket answers by itself: a create of a key that is there is a lost compare-and-swap
// (bus.IsCASConflict), and the key is gone once the bucket's TTL has outlived the proof's window.
// Two listeners creating one key at the same moment therefore see one success and one conflict,
// as two Dispatch pods see one insert and one conflict. A key whose last record is the delete
// marker a release leaves (proofSpend.release), which only an edge listener makes, is created
// again over that marker: the edge's bucket handles read with stream message gets
// (bus.WithStreamMessageReads), so its create reads the marker from the stream's leader and
// writes against the marker's revision, and of two such creates the one that loses gets the
// server's wrong-last-sequence error, the second write's own answer, 10071 on a bucket of one copy
// and 10164 on a replicated one; it is the same answer, that another presentation holds the proof
// (bus.IsCASConflict reads both codes).
//
// The key is the proof's 'jti' and normalized target URI hashed together, for the reason
// Dispatch's store keys on the same two: jkt, nonce and htm only ever widen the key, and this
// store serves one resource server whose proofs all carry an 'ath' the library has already
// checked. It is hashed because a KV key is a NATS subject token, which a 'jti' a caller chose
// and a URI are not (bus.KeyValue refuses the dots and spaces either can hold), and the hash of
// the pair is the same length whatever they were.
//
// exp is not read: it dates the end of one proof's window on the clock of the listener that
// checked it, and every key here lives spentProofRetention, which is that window's longest.
//
// A create this request's own presentation made is written into the request's proofSpend, when
// apiAuth gave it one (withProofSpend), so a 503 the route answers before serving anything can
// give the proof back. Only a create the bucket answered as made is: one that did not answer may
// or may not have landed, and a record the request cannot prove it wrote may be the record of an
// earlier presentation that was served.
func (s spentProofs) CheckAndSetDPoPProofUsed(ctx context.Context, jti, _, _, _, htu string, _ time.Time) (bool, error) {
	sum := sha256.Sum256([]byte(jti + "\x00" + htu))
	key := hex.EncodeToString(sum[:])
	// Bounded ends the wait with the request, as every other bucket call this listener makes
	// does, and waits within it for a connection nats.go is reconnecting; the abandoned create
	// may still land once sent, which is the right way round for a record whose whole purpose is
	// to be there.
	revision, _, err := bus.Bounded(ctx, s.kv.Conn(), func() (uint64, error) {
		return s.kv.Create(key, nil)
	})
	if bus.IsCASConflict(err) {
		return true, nil
	}
	if err != nil {
		// Nothing is known about this proof, so it is neither spent nor sound: the library turns
		// this into its server_error, which ciphertoken reports as a failure of the check rather
		// than as a refusal of the caller.
		return false, fmt.Errorf("record the spent DPoP proof in %s: %w", bus.DPoPProofBucket, err)
	}
	if spend, ok := ctx.Value(proofSpendKey{}).(*proofSpend); ok {
		*spend = proofSpend{kv: s.kv, key: key, revision: revision}
	}
	return false, nil
}

// proofReleaseBudget bounds the delete that gives a proof back (proofSpend.release). A 503 waits
// for it, and a route answers a bus that does not answer within sendRequestBudget, so the two
// together stay inside the 5 s the client gives each attempt (packages/envoy-client/src/transport.ts).
const proofReleaseBudget = time.Second

// proofSpend is the spent-proof record one request's own presentation wrote: the key and the
// revision its create made, and the bucket it is in. It is empty until CheckAndSetDPoPProofUsed
// fills it, and stays empty when the create did not answer as made.
type proofSpend struct {
	kv       bus.KeyValue
	key      string
	revision uint64
}

// proofSpendKey is the context key a request's proofSpend is carried under (withProofSpend).
type proofSpendKey struct{}

// withProofSpend carries spend to the spent-proof store the request's presentation is recorded
// in, which fills it.
func withProofSpend(ctx context.Context, spend *proofSpend) context.Context {
	return context.WithValue(ctx, proofSpendKey{}, spend)
}

// release gives the proof back: it deletes the record this request's create made, and no other,
// since the delete is made against that create's revision. The proof can then be presented once
// more inside its window, as the first presentation could. It runs on a context the request's own
// end does not cancel, bounded by proofReleaseBudget, and a spend with no record is no call.
func (s *proofSpend) release(ctx context.Context) error {
	if s.key == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), proofReleaseBudget)
	defer cancel()
	_, _, err := bus.Bounded(ctx, s.kv.Conn(), func() (struct{}, error) {
		return struct{}{}, s.kv.Delete(s.key, nats.LastRevision(s.revision))
	})
	s.key = ""
	return err
}

// proofReleasingWriter is the writer a body-less request an edge listener admitted on a broker
// token is served through. The first status it writes, when that status is 503, releases the
// request's proof (proofSpend.release) before the status is written, so the balancer in front of
// the edge cannot send its retry before the record is gone. Every other status keeps the proof
// spent: the request was served, or refused, and a proof reused after that is a replay. No other
// listener has that balancer in front of it, so no other listener releases a proof: there a
// release would gain nothing and would leave the proof good for one more request.
//
// A 503 says the listener cannot tell whether it acted, and a client may ask again: a route
// answers it for a fault it met before it served the request (a list copy refilling, a bus that
// did not answer, a broker answer it could not get, a replica with no room for a relay socket),
// or for a write whose answer was lost after it may have landed (writeStoreError), and the
// body-less routes are idempotent, so the retry the release admits has the outcome the client's
// own retry with a fresh proof would have (packages/envoy-client/src/transport.ts retries every
// status at or past 500). Google's load balancer sends such a request again, once, after a 502,
// 503 or 504, when it carries no body, with the same headers, the proof among them.
//
// A proof signs the method, the path and the token, and not the query, the body or any other
// header (RFC 9449 Section 4.2's htu carries no query). So a given-back proof can be presented
// once more on the same method and path with another query, a DELETE of a session without the
// conditions its query carried among them. That is accepted: it takes the token and the proof in
// plaintext, inside the proof's window. TLS covers the hop from the client to the balancer only;
// the hop from Google's proxy to the pod is HTTP inside Google's network, where the token and the
// proof travel in plaintext, and that replay stays the accepted risk. A request that carries a
// body keeps its proof spent whatever its status, since a proof given back there would let whoever
// holds it send another body under it; its client sends it again with a fresh proof, and the
// balancer does not retry it. A proof presented once more after the retry was served is refused
// as a replay.
//
// The balancer's retry is still the 401 of a replay in four cases, and the status stays 503 in
// each, the status every route documents for a fault it can be asked again about:
//   - the 503 came from the spent-proof store itself (refuseBrokerToken's check_failed): its create
//     did not answer, so nothing this request can prove it wrote is there to release, though the
//     create may have landed;
//   - the release did not reach the bucket within proofReleaseBudget, which a bus that did not
//     answer the route makes likely, and which logs a WARN;
//   - the balancer wrote the 502 or 504 itself after the proof was recorded: the pod dropped the
//     connection (a killed pod, a handler that panicked, a shutdown that ended with the request in
//     flight) or the request ran past the backend timeout, so no 503 of this listener released it;
//   - the retry's create met the release's delete marker and its read of the key from the stream's
//     leader got no answer, so the create could not tell the marker from a live record and
//     reported the key held (nats.ErrKeyExists, bus's streamMessageReads.Create): the retry reads
//     as a replay, failing closed.
//
// A copy of a replicated bucket that has not applied the release's delete marker plays no part:
// the retry's create reads the marker from the stream's leader (CheckAndSetDPoPProofUsed).
//
// The relay socket's client reads an upgrade refused 401 as a refused token and opens again after
// a second with a fresh token and a fresh proof (envoy-client's listener relay). A plain /v1 call
// gets the generic 401 every refused credential gets, which names nothing to the caller, and this
// listener logs it as `listener: broker token rejected` with the reason proof_invalid, the class a
// replay is logged under; the client sends no request again on a 401.
//
// It passes Hijack through, which the relay socket's WebSocket upgrade takes the connection by,
// and Unwrap, which http.ResponseController reads.
type proofReleasingWriter struct {
	http.ResponseWriter
	r       *http.Request
	spend   *proofSpend
	logger  *logging.Logger
	decided bool
}

func (w *proofReleasingWriter) WriteHeader(status int) {
	if !w.decided && status >= http.StatusOK {
		w.decided = true
		if status == http.StatusServiceUnavailable {
			if err := w.spend.release(w.r.Context()); err != nil {
				w.logger.Warn("listener: a 503 could not give its request's DPoP proof back; the load balancer's retry of it is refused as a replay",
					slog.String("path", w.r.URL.Path),
					slog.String("error", err.Error()))
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *proofReleasingWriter) Write(body []byte) (int, error) {
	w.decided = true
	return w.ResponseWriter.Write(body)
}

func (w *proofReleasingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.decided = true
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *proofReleasingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// brokerCallerKey is the context key the decided caller is carried under. It is an unexported
// zero-size type, so nothing outside this package can read or write the value, and no other
// package's key can collide with it.
type brokerCallerKey struct{}

// withBrokerCaller carries the caller a broker token proved to every handler behind apiAuth.
//
// The whole Caller is carried rather than its session alone: the broker's answer also names the
// person or the service it acts for and the project it is scoped to, and a later route that
// reports or records either reads it here instead of asking the broker a second time, which a
// spent proof makes impossible anyway. It is set exactly once, by the one place that verifies a
// presentation (apiAuth), so no handler can be reached with a caller nothing checked, and no
// handler presents the proof a second time.
func withBrokerCaller(ctx context.Context, caller ciphertoken.Caller) context.Context {
	return context.WithValue(ctx, brokerCallerKey{}, caller)
}

// brokerCaller is the caller r's credential proved, and whether it proved one at all. A request
// under the shared token, under a service-account token, or under no credential proves none, and
// every route serves it as it always has.
func brokerCaller(r *http.Request) (ciphertoken.Caller, bool) {
	caller, ok := r.Context().Value(brokerCallerKey{}).(ciphertoken.Caller)
	return caller, ok
}

// sessionNotBound is the refusal a write that acts as session gets from the credential the
// request presented: empty when the request presented no broker token, or when the token names
// this very session, and otherwise the message the route answers 403 with. It logs that refusal
// where it decides it, one line naming both sessions and the path and never the token, as
// apiAuth's own refusals do.
//
// It is called by each of the seven routes that act as a session, after that route has read the
// session it names, rather than being a wrapper around them: the routes name the session in five
// different shapes (two message bodies' source_session, subscribe's and unsubscribe's
// session_id, the role claim's, and the path of the two DELETEs), and a wrapper would have to
// decode each body a second time to find it. The read routes that name another session by design
// — a registry entry, a session's interests, the session list — call nothing here: narrowing
// those would stop every session looking another up.
//
// The two DELETEs are held to the token's session like the rest, the Claude Code /clear handoff
// included: that handoff removes the registration of the session it hands off from while it is
// still that session (`identity.set(next)` runs after the deregistration,
// packages/claude-envoy/src/envoy-channel-server.ts, adoptHandoff), and the token it presents
// names the session its client is acting as at the moment of the call
// (packages/envoy-client/src/broker-credential.ts). So the handoff's DELETE names the session its
// own token names, and no rule beyond this one is needed to keep it working. The
// client_instance_id that DELETE carries decides something else and keeps deciding it: which
// client instance's registration under that one session id may be removed.
//
// An empty session is no refusal: the body names no session, so the request acts as none, and
// nothing is recorded under another session's name. The two message routes alone allow that, and
// what they produce is an envelope with no source session.
func sessionNotBound(r *http.Request, logger *logging.Logger, session string) string {
	caller, ok := brokerCaller(r)
	if !ok || session == "" || caller.Session == session {
		return ""
	}
	logger.Warn("listener: broker token acts as another session",
		slog.String("token_session", caller.Session),
		slog.String("request_session", session),
		slog.String("path", r.URL.Path))
	return fmt.Sprintf("this access token acts as session %s, so it may not act as %s", caller.Session, session)
}
