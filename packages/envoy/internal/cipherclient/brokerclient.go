// Package cipherclient is this repository's one Go client of the secrets broker's RFC 8693 token
// exchange: a machine or a person trades the platform identity it already holds for one of the
// broker's access tokens, which Dispatch and the Envoy listener read back at the broker's
// introspection route. `agent-secrets`, `agent-secrets-helper`, `legion controller start` and the
// Legion daemon's own Dispatch client (NewTokenTransport) use it; the TypeScript client of
// packages/cipher-client is its other half, and the two are held to one wire contract by the
// corpus in testdata/wire.json, which both suites replay.
//
// What the exchange is: the caller presents the subject token its platform already gives it (a
// Source), proves a key of its own with a DPoP proof (RFC 9449), and is answered an access token
// bound to that key. The answer carries no refresh token at all — the grant issues none — so a
// renewal is a fresh subject token traded again, which is why a Source is read at every trade
// rather than once. A person's sign-in trades their own Google identity token with the machine's
// as the actor token (RFC 8693 Section 2.1), which is the one request that carries two tokens.
//
// A caller sends only the public half of the key it proves: the proof embeds the public JWK, and
// no code here marshals a private member onto the wire. That key is one its process makes, a
// person's sign-in box key, or, for a role in a pod, the role's own agent-secrets key
// (MachineKey), the key the broker holds a pod session's trade to.
//
// When it trades again: at the renewal moment worked out once as a token lands (renewalAt states
// the rule) and stored on the token. Token answers the token until then, Run sleeps until then,
// and RenewalAt answers that moment for it. The TypeScript client holds a token to the same rule,
// and testdata/renewal.json holds both clients to it (TestRenewalCorpus).
//
// What it refuses to do: hand out a token it could not renew. Token answers the held token while
// that token is live and the broker is merely unreachable, and answers the error once the broker
// has refused the identity, the issuer has ended the person's sign-in it trades, or the held
// token has run out, so a caller that writes only with what Token gave it never writes with no
// identity.
package cipherclient

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/redirects"
)

const (
	// TokenPath is the broker's token endpoint, where every trade is posted.
	TokenPath = "/v1/oauth/token"
	// GrantTypeTokenExchange is RFC 8693's own grant name: what every trade at the broker's
	// token endpoint asks for, and what Google's security token service takes for the same
	// exchange (pool.go). The library the broker is built on spells it in a package its callers
	// cannot import.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"

	// DefaultRenewBefore is how long before an access token's expiry the client trades again, so
	// a renewal lands with the held token still good and nothing of the caller's restarts. The
	// broker mints a machine's token for an hour or less.
	DefaultRenewBefore = 5 * time.Minute
	// liveMargin is how long the held token must still be good for live to answer it, so a
	// request that presents it reaches its server and is read there before it lapses. It is a
	// small part of RenewBefore, so a renewal loop (Run) that waits out a broker that does not
	// answer leaves requests presenting the token in hand for nearly all of that window.
	liveMargin = 10 * time.Second
	// defaultRetryWait is the wait after a trade the broker did not answer or answered 5xx,
	// doubling to maxRetryWait. The grant has no rotation window to miss, so an outage longer
	// than one token's life is the same event as a short one: the loop keeps trading until the
	// broker answers again.
	defaultRetryWait = 15 * time.Second
	maxRetryWait     = 5 * time.Minute
	// unregisteredLogEvery is how often the renewal loop says again that the broker has no
	// registration for this pod's session yet, after saying so at the first such refusal.
	unregisteredLogEvery = 30 * time.Minute
	// tradeTimeout bounds one trade, the token endpoint being one request.
	tradeTimeout = 30 * time.Second
)

// Token is one of the broker's access tokens as this client holds it.
type Token struct {
	// AccessToken is the token itself, presented as `Authorization: DPoP <token>` beside a
	// proof of the key it is bound to.
	AccessToken string
	// TokenType is the answer's token_type, TokenTypeDPoP for every token this grant mints: read
	// refuses an answer that names any other.
	TokenType TokenType
	// Expiry is when the token lapses, from the answer's expires_in.
	Expiry time.Time
	// CredentialID is the launcher credential the trade minted for this machine or sign-in
	// (oauthwire.ExtraCredentialID), under which sessions of this machine enrol. It is empty
	// where the answer carries none.
	CredentialID string
	// CredentialExpiresAt is when that credential lapses (oauthwire.ExtraCredentialExpiresAt),
	// zero where the answer carries none.
	CredentialExpiresAt time.Time
	// renewsAt is when the client trades again for this token: renewalAt as of the moment it
	// landed, set by read from the same now as Expiry. Token answers the token until then, Run
	// sleeps until then, and RenewalAt answers it. It is the client's own reckoning, no member of
	// the broker's answer, so testdata/wire.json names it nowhere.
	renewsAt time.Time
}

// TokenType is a token's token_type, read as the closed set this grant mints: DPoP alone. It is
// decided from the broker's token_type as it came (read), and holds none of the broker's text.
type TokenType uint8

// TokenTypeDPoP is a token bound to the caller's key (RFC 9449), presented as
// `Authorization: DPoP <token>` beside a proof of that key.
const TokenTypeDPoP TokenType = 1

// String is the token type as RFC 9449 spells it, and empty for the zero TokenType.
func (t TokenType) String() string {
	if t == TokenTypeDPoP {
		return "DPoP"
	}
	return ""
}

// RefusedError is a trade the broker refused: a plain error naming what it said. Almost every
// one is a refusal no retry can cure — an identity no entry lists, a sign-in a person revoked,
// a token of the wrong audience — and the renewal loop stops on those rather than asking again
// every quarter minute. The exceptions are an outage, a 408 or a 429 from the broker or a proxy
// before it (oauthrefusal.Outage), its two DPoP refusals, a pod session it holds no registration
// for yet, and a person Dispatch holds (Permanent).
//
// The broker's code and description are its words, and they can quote a URL that carries a
// credential, so the refusal holds them as redact.RemoteText, which prints only the rule's
// reading of them under every verb, in JSON and in a log field. A caller that tells refusals
// apart compares through HasCode and DescriptionHasPrefix, which answer no text.
type RefusedError struct {
	status            int
	code, description redact.RemoteText
	// Identity names the identity the broker refused (refusedIdentity): a person's sign-in on
	// this machine names the person, and every machine source reads "this machine's identity".
	// Empty is a refusal of an identity no Source named.
	Identity string
	// answer is this same refusal as x/oauth2's own token-endpoint error, which Unwrap says
	// why it is kept: its status, and the broker's code and description as the rule reads them
	// (redact.NewRetrieveError), so the answer printed whole prints nothing the rule withholds.
	answer *oauth2.RetrieveError
}

// newRefusedError is the refusal a broker answered with an OAuth error body to a trade of
// identity, as refusedIdentity names it.
func newRefusedError(identity string, status int, code, description redact.RemoteText) *RefusedError {
	return &RefusedError{status: status, code: code, description: description, Identity: identity, answer: redact.NewRetrieveError(status, code, description)}
}

// refusedIdentity is how a refusal of a trade of s names the identity refused. A person's sign-in
// names the person, so a person whose own sign-in the broker refused never reads "this machine".
// Every other source reads "this machine's identity", as a refusal always has: a machine source's
// Describe names configured values (a token file's path, a pool's audiences and account), which a
// refusal's text has never carried.
func refusedIdentity(s Source) string {
	if person, ok := s.(*PersonIdentity); ok {
		return person.Describe()
	}
	return "this machine's identity"
}

// Error is this refusal's sentence: fixed words, the identity refused (Identity, as
// refusedIdentity names it, or "the identity traded" where none is named), the status, and the
// broker's code and description each as it prints itself (redact.Field), so a URL in them prints
// with no user information and no query or fragment value, and a field the rule cannot read
// prints as redact.WordsNotReadable while the status and the other field still print.
func (e *RefusedError) Error() string {
	identity := e.Identity
	if identity == "" {
		identity = "the identity traded"
	}
	if e.description.IsEmpty() {
		return fmt.Sprintf("the secrets broker refused %s: HTTP %d %s", identity, e.status, e.code)
	}
	return fmt.Sprintf("the secrets broker refused %s: HTTP %d %s: %s", identity, e.status, e.code, e.description)
}

// PrintableText is this refusal's sentence, Error, built from fixed words, the identity refused,
// and the answered OAuth error's own status, code and description alone, which is what lets
// internal/redact print it beside a clean broker URL as it prints that answer (redact.Printable).
// The identity is this client's own words for the source it traded, held before the trade, which
// neither the answer nor a caller writes: a fixed phrase for every machine source, and for a
// person's sign-in the person that sign-in's record names (PersonIdentity.Describe). Where it
// holds a control character redact prints the answer's own text instead. redact reads the text
// again before it prints it. A caller's words around this error still print nothing there.
func (e *RefusedError) PrintableText() string { return e.Error() }

// Format prints Error under the verb, flags, width and precision it is given, as fmt prints that
// string, so %q quotes it, %x and %X hex-encode it, and %v, %+v and %#v print it rather than this
// refusal's fields.
func (e *RefusedError) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), e.Error())
}

// LogValue is Error, so a structured log field names the refusal only as a message may.
func (e *RefusedError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// HasCode reports whether the broker refused with code, compared as it came. It answers no text,
// so a caller that tells refusals apart prints nothing it read.
func (e *RefusedError) HasCode(code string) bool { return e.code.Equal(code) }

// DescriptionHasPrefix reports whether the broker's description begins with prefix, compared as
// it came. Like HasCode, it answers no text.
func (e *RefusedError) DescriptionHasPrefix(prefix string) bool {
	return e.description.HasPrefix(prefix)
}

// DescriptionContains reports whether the broker's description holds sub, compared as it came.
// Like HasCode, it answers no text.
func (e *RefusedError) DescriptionContains(sub string) bool {
	return e.description.Contains(sub)
}

// RefusalCode is the broker's code read as an OAuth error code, decided from the code as it came,
// which is what redact.RefusalCode answers for an error that holds this refusal: the
// *oauth2.RetrieveError this refusal unwraps to holds the rule's reading of the code, not the code.
func (e *RefusedError) RefusalCode() redact.OAuthCode { return e.code.OAuthCode() }

// Unwrap is this refusal as the answered OAuth token-endpoint error it is, which is how every
// caller of this package reads it. A broker failure is printed through internal/redact, whose
// allow-list prints an error's own text only where it can prove that text carries nothing of
// the URL the deployment configured, and an answered HTTP refusal is one of the kinds it
// passes, with this refusal's sentence over it (PrintableText), read by the rule for remote
// text. Without this a refusal read as an unplaceable failure and printed as "the request
// failed", losing the broker's reason — that the request names no session, that no entry lists
// this account — which is the only part a person can act on; beside a broker URL that carries a
// credential, whose text redact prints no part of, the status on this answer is what names the
// refusal rather than that same "the request failed".
func (e *RefusedError) Unwrap() error { return e.answer }

// Permanent reports whether this refusal is one no retry can cure, which every refusal is but
// five.
//
// An outage (oauthrefusal.Outage): a 408, a broker or a proxy before it that did not take the
// request, and the broker's 429, whatever OAuth error either body names; no 5xx is a refusal,
// which is read only from a 4xx. The token endpoint spends a bucket per source address, and a
// bucket refills. A client that stopped trading on one would stop for good over a burst — its
// own, or that of another process behind the same address — or over one request a proxy did not
// pass on, and enrol nothing once the credential it holds lapsed, with nothing but a restart to
// cure it.
//
// `invalid_dpop_proof`, which is what the broker answers a proof whose iat is outside its 60 s
// skew window. A helper started at boot, before NTP has corrected the clock, would otherwise
// stop trading for good and hold no launcher credential until somebody restarted it; the next
// trade after the correction succeeds.
//
// `use_dpop_nonce`, which the transport already retries once with the nonce the answer carried
// (dpop.go). One that reaches here is an answer that carried no nonce, or a nonce the broker
// then refused in turn — a server state the next trade can find changed.
//
// A pod session the broker holds no registration for yet (PodSessionUnregistered): a pod's
// trade can reach the broker before its daemon's or its session watch's registration does, and
// a registrar can be down for minutes, so the trade that follows the registration succeeds.
//
// `membership_held` (oauthwire.MembershipHeld): Dispatch holds the person this credential
// names, because Google refused their Dispatch sign-in, or no refresh has confirmed them for 24
// hours. A hold revokes nothing, and the person's next Dispatch sign-in, or the next refresh that
// confirms them, ends it, so the identity this client trades is served again then with no new
// login. A client that stopped on it would need a restart after every hold.
func (e *RefusedError) Permanent() bool {
	switch {
	case oauthrefusal.Outage(e.status), e.HasCode("invalid_dpop_proof"), e.HasCode("use_dpop_nonce"), e.PodSessionUnregistered(),
		e.HasCode(oauthwire.MembershipHeld):
		return false
	}
	return true
}

// PodSessionUnregistered reports whether this is the broker's refusal of a pod whose session no
// registration names yet: invalid_grant, its description beginning
// oauthwire.PodSessionNotRegistered and a colon. The same words under another code, or anywhere
// but at the start, are some other refusal.
func (e *RefusedError) PodSessionUnregistered() bool {
	return e.HasCode("invalid_grant") && e.DescriptionHasPrefix(oauthwire.PodSessionNotRegistered+":")
}

// finalRefusal reports whether err ends the identity a client trades, which no retry cures: the
// broker refusing it for good (RefusedError.Permanent), or the issuer refusing to renew the
// person's sign-in the client trades (ErrSignInEnded, from DeviceFlow.Refresh through
// PersonIdentity). Token drops the token in hand on one, though it is still live, and Run stops on
// one, as the TypeScript client's isFinalRefusal decides for both.
func finalRefusal(err error) bool {
	var refused *RefusedError
	return (errors.As(err, &refused) && refused.Permanent()) || errors.Is(err, ErrSignInEnded)
}

// Config is one client: the broker it trades at, the identity it trades, and the key it proves.
type Config struct {
	// BrokerURL is the broker's base URL, with or without the slashes it ends in: New drops
	// every one of them (WithoutTrailingSlashes), and an address that is nothing but slashes
	// counts as no broker URL at all. It is a cipheraddr.Address so that no log line or error of
	// this client can print the credential such an address may carry.
	BrokerURL cipheraddr.Address
	// Key is the DPoP key this client proves, whose public half alone reaches the broker. A
	// person's sign-in proves the box key `agent-secrets login` made, and a role in a pod its own
	// agent-secrets key (MachineKey); every other caller proves a key it made at start.
	Key *ecdsa.PrivateKey
	// Subject is the identity being traded.
	Subject Source
	// Actor is the machine a person signed in on, presented as the actor token beside their own
	// Google identity token. Nil for every trade a machine makes for itself.
	Actor Source
	// Session is the session name the minted token carries, which an entry that fixes one
	// refuses to be told and an entry that fixes none requires.
	Session string
	// Projects is the projects the minted token names, sorted and each once (TradeProjects reads
	// them from AGENT_SECRETS_PROJECTS), sent as oauthwire.FormProjects. Only a trade that acts as
	// its person sets it, and the broker names only projects that person may see; a machine
	// trading for itself sets none, since its entry decides its project. natsprobe's trade sets
	// it. Nil sends no parameter at all.
	Projects []string
	// NoLauncherCredential declines the launcher credential this identity holds
	// (oauthwire.FormLauncherCredential), for a caller that wants a Dispatch access token and
	// nothing else: a trade that asks writes a launcher_credentials row and a token_login
	// audit row, both of which an operator reads through, and nothing removes either. The zero
	// value asks, because every caller of this package that goes on to enrol a session or read
	// a secret — `agent-secrets`, `agent-secrets-helper`, a sign-in — could do nothing without
	// the credential. The callers that set it enrol nothing: `legion controller start`'s one
	// check that the controller's machine can hold a Dispatch credential at all
	// (controller_dispatch_token.go), whose key dies with the call. A person's trade may not
	// set it at all: a sign-in holds its credential, and the broker refuses one that declines
	// (invalid_request, oauth.personDeclinesNothing).
	NoLauncherCredential bool
	// Scope is the OAuth scope this trade asks for, sent as the form's scope: oauthwire.ScopeRead
	// narrows the minted token to reading, on the identity the same trade would have had. Empty
	// sends no parameter, which is every Go trade of this release: no Go caller makes a read
	// trade, and this client sends no holder proof (oauthwire.HeaderDPoPHolder).
	Scope string
	// AsOf is the cut a read trade asks for (oauthwire.FormAsOf), an RFC 3339 time sent as
	// written. Empty sends no parameter. The broker refuses a cut beside any scope but
	// oauthwire.ScopeRead.
	AsOf string
	// HTTP is the client every call goes through; nil is http.DefaultClient. The DPoP proof is
	// signed by a transport this client wraps around it, so a caller passing its own client
	// keeps its timeout and its dialler.
	HTTP *http.Client
	// Log records each trade and each failed renewal: the identity, the session and the
	// broker's refusal code, never a token. Nil logs nothing.
	Log *slog.Logger
	// RenewBefore overrides DefaultRenewBefore. Only a test shortens it.
	RenewBefore time.Duration
	// Now and After are time.Now and time.After unless a test replaces them.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

// Client trades Config's identity for the broker's access tokens and keeps one live.
type Client struct {
	brokerURL cipheraddr.Address
	key       *ecdsa.PrivateKey
	subject   Source
	actor     Source
	session   string
	// projects is Config.Projects in its form spelling (oauthwire.JoinProjects), empty for none.
	projects string
	// noLauncherCredential is Config.NoLauncherCredential: this trade declines the credential.
	noLauncherCredential bool
	// scope and asOf are Config.Scope and Config.AsOf, each sent only when set.
	scope       string
	asOf        string
	http        *http.Client
	log         *slog.Logger
	renewBefore time.Duration
	now         func() time.Time
	after       func(time.Duration) <-chan time.Time
	// wake carries a Wake to the renewal loop's sleep, one slot deep: the instruction is "trade
	// now", which two of are one.
	wake chan struct{}

	// mu serialises the trades, and every write of held with them: one trade runs at a time, and
	// no token is replaced by what an older trade answered.
	mu sync.Mutex
	// held is the token this client holds: nil before its first trade, after Drop, and after a
	// failed trade with nothing live left. Only a holder of mu writes it, and live reads it
	// without mu, so a request that finds a token still good here never queues behind a trade.
	held atomic.Pointer[Token]
}

// WithoutTrailingSlashes is raw without the slashes it ends in: the one rule both halves of this
// client read an address by, the TypeScript half's being `withoutTrailingSlashes` in
// packages/contracts/src/trailing-slash.ts. An operator types the address by hand into a unit
// file, a pod's environment or a shell, so an address ending in more than one slash is a typo
// rather than a shape to refuse; dropping one slash of two would leave this half posting
// `//v1/...` where the other half posts `/v1/...`, and a proof's htu is the URL the caller built,
// so a path the broker's own mux then cleans no longer matches the proof and the trade is refused
// naming the credential rather than the URL an operator could fix.
func WithoutTrailingSlashes(raw string) string { return strings.TrimRight(raw, "/") }

// New is the client Config describes. It refuses a configuration that could never trade — no
// broker, no key, no identity — rather than failing at the first call.
func New(cfg Config) (*Client, error) {
	brokerURL := cipheraddr.New(WithoutTrailingSlashes(strings.TrimSpace(cfg.BrokerURL.Raw())))
	switch {
	case brokerURL.Empty():
		return nil, errors.New("brokerclient: no broker URL")
	case cfg.Key == nil:
		return nil, errors.New("brokerclient: no DPoP key: a trade is judged on a proof of the caller's own key")
	case cfg.Subject == nil:
		return nil, errors.New("brokerclient: no subject token source: nothing names the identity to trade")
	}
	if oauthwire.ValidProjects(cfg.Projects) != nil {
		return nil, fmt.Errorf("brokerclient: the projects to trade for are not at most %d project keys, sorted and each named once", oauthwire.MaxProjects)
	}
	client := &Client{
		wake:                 make(chan struct{}, 1),
		brokerURL:            brokerURL,
		key:                  cfg.Key,
		subject:              cfg.Subject,
		actor:                cfg.Actor,
		session:              cfg.Session,
		projects:             oauthwire.JoinProjects(cfg.Projects),
		noLauncherCredential: cfg.NoLauncherCredential,
		scope:                cfg.Scope,
		asOf:                 cfg.AsOf,
		http:                 cfg.HTTP,
		log:                  cfg.Log,
		renewBefore:          cfg.RenewBefore,
		now:                  cfg.Now,
		after:                cfg.After,
	}
	if client.http == nil {
		client.http = http.DefaultClient
	}
	if client.renewBefore <= 0 {
		client.renewBefore = DefaultRenewBefore
	}
	if client.now == nil {
		client.now = time.Now
	}
	if client.after == nil {
		client.after = time.After
	}
	return client, nil
}

// Key is the key this client proves, whose public half the broker records: a caller that enrols
// under the minted credential signs with it.
func (c *Client) Key() *ecdsa.PrivateKey { return c.key }

// Token is a live access token: the held one until the renewal moment stored on it as it landed
// (Token.renewsAt, as the TypeScript client holds HeldToken.renewsAt in
// packages/cipher-client/src/token-client.ts), and otherwise a fresh trade. A trade the broker did
// not answer, or answered with a refusal a later trade may cure (an outage, a 408 or a 429; a DPoP
// refusal; a pod session it holds no registration for yet; a person Dispatch holds), while the
// held token is still live answers that token, since a client that already holds an identity need
// not stop for a broker that is merely down or busy; a refusal no retry can cure (finalRefusal: the
// broker's RefusedError.Permanent, or a sign-in the issuer has ended, ErrSignInEnded), and a
// failure with nothing live left, answer the error and drop the token in hand, so no caller writes
// with no identity. Token trades once; a caller that waits for a registration to land runs Run,
// which trades again.
func (c *Client) Token(ctx context.Context) (*Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.held.Load()
	if held != nil && c.now().Before(held.renewsAt) {
		return held, nil
	}
	token, err := c.tradeHeld(ctx)
	if err == nil {
		return token, nil
	}
	if held := c.held.Load(); held != nil && c.now().Before(held.Expiry) {
		what, attrs := c.retriedTrade(err)
		c.logf(ctx, what+"; the token in hand is still live", attrs...)
		return held, nil
	}
	c.held.Store(nil)
	return nil, err
}

// live is the held token while it is good for at least liveMargin more, and nil otherwise. It
// takes no lock, so it never waits on a trade in flight: a caller holding a token inside its
// renewal window presents it while a renewal loop (Run) trades for the next one, or waits out a
// broker that does not answer or hangs, rather than queue behind that trade (NewTokenTransport).
func (c *Client) live() *Token {
	if held := c.held.Load(); held != nil && c.now().Add(liveMargin).Before(held.Expiry) {
		return held
	}
	return nil
}

// Drop forgets token when it is still the one this client holds, so the next Token trades afresh
// rather than answering it again: for a caller a server has told that the token it presented is
// no longer live, such as one the broker ended at a revoke before it lapsed (NewTokenTransport).
// It clears the held token as the TypeScript client's drop() does
// (packages/cipher-client/src/token-client.ts), but only while that is the token named: a trade
// that landed since the caller was answered holds a fresh token, and dropping it would cost the
// next caller a trade for nothing.
func (c *Client) Drop(token *Token) {
	if token == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if held := c.held.Load(); held != nil && held.AccessToken == token.AccessToken {
		c.held.Store(nil)
	}
}

// Run keeps one token live until ctx ends, handing each one to onToken as it lands: the first
// trade, and every renewal, traded at the renewal moment stored on the token in hand as it landed
// (Token.renewsAt). It is the shape a caller with something to keep fresh takes — a credential to
// install, a token file to rewrite — and nothing of that caller restarts across a renewal.
//
// It stops with a plain error on a refusal no retry can cure (finalRefusal) — the broker refusing
// the identity for good (RefusedError.Permanent), or the issuer ending the person's sign-in it
// trades (ErrSignInEnded) — dropping the token in hand as Token does, so a Token on this client
// after Run returns asks again rather than answering it; and it stops when onToken fails. A
// broker that does not answer, or answers with an outage (a 5xx, a 408 or a 429), either of its
// DPoP refusals, that it holds no registration for this pod's session yet, or that Dispatch holds
// the person the identity names, is retried, from defaultRetryWait doubling to maxRetryWait, for
// as long as ctx lives: an outage past the token's own life is the same event as a short one,
// since this grant has no rotation window to miss, a host whose clock NTP corrects after boot
// trades again on the next pass rather than holding no credential until somebody restarts it, a
// pod whose trade reached the broker before its registration did trades once that registration
// lands, however long it takes, and a person whom Dispatch held is traded again after their next
// Dispatch sign-in, or the next refresh that confirms them, with nothing restarted. A missing
// registration is logged once, naming the session, and again every unregisteredLogEvery while it
// lasts, rather than at every trade. A cancelled ctx is no error.
func (c *Client) Run(ctx context.Context, onToken func(context.Context, *Token) error) error {
	wait := defaultRetryWait
	// since is when the broker first refused this pod's session as unregistered, and logged when
	// the loop last said so; both are zero until such a refusal, and again once a trade lands.
	var since, logged time.Time
	for {
		c.mu.Lock()
		token, err := c.tradeHeld(ctx)
		c.mu.Unlock()
		switch {
		case err == nil:
			if !since.IsZero() {
				// The wait was said; so is its end, once, as the TypeScript client says it.
				c.logf(ctx, "the secrets broker answered this pod's session once it was registered",
					"session", c.session, "waited", c.now().Sub(since))
			}
			wait = defaultRetryWait
			since, logged = time.Time{}, time.Time{}
			if err := onToken(ctx, token); err != nil {
				return err
			}
			// The renewal comes at the moment stored on the token as it landed: one worked out
			// again after onToken would come later, by half of onToken's time for a half-life
			// renewal.
			if !c.sleep(ctx, token.renewsAt.Sub(c.now())) {
				return nil
			}
			continue
		case ctx.Err() != nil:
			return nil
		}
		var refused *RefusedError
		isRefusal := errors.As(err, &refused)
		switch {
		case finalRefusal(err):
			return err
		case isRefusal && refused.PodSessionUnregistered():
			now := c.now()
			if since.IsZero() {
				since = now
			}
			if logged.IsZero() || now.Sub(logged) >= unregisteredLogEvery {
				c.logf(ctx, "the secrets broker has no registration for this pod's session yet; trading again until it lands",
					"session", c.session, "waited", now.Sub(since), "error", err.Error(), "in", wait.String())
				logged = now
			}
		default:
			what, attrs := c.retriedTrade(err)
			c.logf(ctx, what+"; asking again", append(attrs, "in", wait.String())...)
		}
		if !c.sleep(ctx, wait) {
			return nil
		}
		if wait *= 2; wait > maxRetryWait {
			wait = maxRetryWait
		}
	}
}

// RenewalAt is the renewal moment stored on token as it landed (Token.renewsAt): when Run trades
// again after handing token to onToken, and until when Token answers token while it holds it,
// however long after the landing the caller asks. It reads token alone, so it never waits on a
// trade in progress. A Token built by hand carries no stored moment, having landed through no
// trade, so RenewalAt works one out for it by renewalAt as of now. A caller that holds an older
// token beside the one Run handed it reads from this whether the older one outlives the wait.
func (c *Client) RenewalAt(token *Token) time.Time {
	if token.renewsAt.IsZero() {
		return c.renewalAt(token, c.now())
	}
	return token.renewsAt
}

// retriedTrade is what a log line says of a trade the renewal retries, and its fields: a refusal's
// status and its code as the broker's address lets its answer print (cipheraddr.Address.WordsOf),
// as the trade's error prints it - the rule's reading beside a clean address, and
// cipheraddr.WordsNotPrinted beside any other, where whoever answers can repeat the credential the
// address carries in the code - or the failure alone.
func (c *Client) retriedTrade(err error) (string, []any) {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return "the secrets broker refused a trade, which a later trade may cure",
			[]any{"status", refused.status, "code", c.brokerURL.WordsOf(refused.code), "error", err.Error()}
	}
	return "a trade got no usable answer from the secrets broker", []any{"error", err.Error()}
}

// renewalAt is when to trade again for token as of now: the later of RenewBefore ahead of its
// expiry and half of what it has left, and now for a token already past its expiry. A token of at
// least twice RenewBefore renews RenewBefore ahead of its expiry, and a shorter one at its half
// life — a deployment that mints short tokens, or a dev stack proving a renewal in seconds. That
// half is never later than the token's own expiry, and it is what keeps such a client from trading
// again soon after each token lands, which would spend the broker's per-address token bucket in a
// loop and end in a 429. The TypeScript client's BrokerTokenClient holds the same rule (renewalAt
// in packages/cipher-client/src/token-client.ts), and testdata/renewal.json holds the two to it
// (TestRenewalCorpus).
func (c *Client) renewalAt(token *Token, now time.Time) time.Time {
	left := token.Expiry.Sub(now)
	if left <= 0 {
		return now
	}
	if half := now.Add(left / 2); half.After(token.Expiry.Add(-c.renewBefore)) {
		return half
	}
	return token.Expiry.Add(-c.renewBefore)
}

// sleep waits d, reporting false when ctx ended first. A Wake cuts the wait short, so the
// caller trades at once rather than at the renewal it was waiting for.
func (c *Client) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-c.wake:
		return true
	case <-c.after(d):
		return true
	}
}

// Wake makes the renewal loop trade now rather than at the renewal it is waiting for, and is
// how a caller that has just thrown the credential away — the broker refused a launcher proof
// signed with it — gets another without waiting out the token's life. It never blocks: a wake
// already waiting is the same instruction, and a client with no loop running trades at its next
// Token anyway, so the one sent to nobody is spent by the next sleep and costs one early trade.
func (c *Client) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) logf(ctx context.Context, message string, args ...any) {
	if c.log == nil {
		return
	}
	c.log.InfoContext(ctx, message, append([]any{"broker", c.brokerURL, "identity", c.subject.Describe()}, args...)...)
}

// tradeHeld is trade, leaving in c.held what the trade leaves in hand: the new token when one
// lands, and nothing on a refusal no retry can cure (finalRefusal), since the token in hand is
// then the ended identity's; every other failure leaves the token in hand as it was. Token and
// Run both trade through it, so the two keep one rule. The caller holds c.mu.
func (c *Client) tradeHeld(ctx context.Context) (*Token, error) {
	token, err := c.trade(ctx)
	switch {
	case err == nil:
		c.held.Store(token)
	case finalRefusal(err):
		c.held.Store(nil)
	}
	return token, err
}

// trade is one RFC 8693 exchange: the subject token this moment's Source answers, the actor
// token where a person is signing in, the session name, whether this trade declines the launcher
// credential its identity holds, the scope and the cut a read trade asks for, and a DPoP proof of
// this client's key.
func (c *Client) trade(ctx context.Context) (*Token, error) {
	subject, subjectType, err := c.subject.Token(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":         {GrantTypeTokenExchange},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subject},
		"subject_token_type": {subjectType},
	}
	if c.session != "" {
		form.Set(oauthwire.FormSession, c.session)
	}
	if c.projects != "" {
		// Sent only when there is a list: a trade naming none reads exactly as one written before
		// the parameter existed.
		form.Set(oauthwire.FormProjects, c.projects)
	}
	// Each sent only when set: a trade that asks for neither reads exactly as one written before
	// the parameters existed.
	if c.scope != "" {
		form.Set("scope", c.scope)
	}
	if c.asOf != "" {
		form.Set(oauthwire.FormAsOf, c.asOf)
	}
	if c.noLauncherCredential {
		// Sent only to decline: an absent parameter asks, so a broker that has never heard of
		// this parameter answers a client that does ask exactly as it always has.
		form.Set(oauthwire.FormLauncherCredential, oauthwire.FormLauncherCredentialDeclined)
	}
	if c.actor != nil {
		actor, actorType, err := c.actor.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the machine this person signed in on: %w", err)
		}
		if actorType == oauthwire.SubjectTokenTypeGoogle {
			// The same token under the type that says what it proves here: not who is
			// asking, but which box the person is asking from.
			actorType = oauthwire.ActorTokenTypeMachine
		}
		form.Set("actor_token", actor)
		form.Set("actor_token_type", actorType)
	}
	answer, err := c.post(ctx, form)
	if err != nil {
		return nil, err
	}
	c.logf(ctx, "traded an identity for one of the broker's access tokens",
		"session", c.session, "credential", answer.CredentialID, "expires", answer.Expiry.UTC().Format(time.RFC3339))
	return answer, nil
}

// tokenAnswer is the broker's token answer as this client reads it. Every member the contract
// names is here and nothing else is read: the corpus holds the client to exactly this set.
type tokenAnswer struct {
	AccessToken         string              `json:"access_token"`
	TokenType           redact.RemoteText   `json:"token_type"`
	IssuedTokenType     string              `json:"issued_token_type"`
	ExpiresIn           redact.RemoteNumber `json:"expires_in"`
	RefreshToken        string              `json:"refresh_token"`
	CredentialID        string              `json:"credential_id"`
	CredentialExpiresAt redact.RemoteText   `json:"credential_expires_at"`
	Error               redact.RemoteText   `json:"error"`
	ErrorDescription    redact.RemoteText   `json:"error_description"`
}

// post sends one trade and reads the answer. A refusal the broker answered with an OAuth error
// is a RefusedError; everything else — a broker that did not answer, a 5xx, an answer that is
// not this grant's — is an error a renewal may ask about again.
//
// The endpoint is TokenPath joined onto the address's path, the address's query kept and its
// fragment dropped (cipheraddr.Address.Join), since the request must reach the broker as
// configured: the path appended to the address's text would sit inside a query or a fragment the
// address holds, and the broker would be asked another path. Every error names the endpoint as
// shown, cipheraddr's Call of it, and wraps a failure of the request itself through the address's
// Error, so no error of a trade carries a user name, password, query or fragment the configured
// address holds. An address that does not parse or names no host is refused by its class, "not a
// valid URL" (redact.ClassNotAURL), never url.Parse's error, which quotes the address whole.
func (c *Client) post(ctx context.Context, form url.Values) (*Token, error) {
	ctx, cancel := context.WithTimeout(ctx, tradeTimeout)
	defer cancel()
	joined, err := c.brokerURL.Join(TokenPath)
	if err != nil {
		return nil, fmt.Errorf("trade %s at %s: %w", c.subject.Describe(), c.brokerURL, redact.Masked(err, redact.ClassNotAURL))
	}
	endpoint := joined.String()
	shown := c.brokerURL.Call(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, c.brokerURL.Error(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	// The form carries the subject token, and the actor token on a person's trade, so a
	// redirect is refused (redirects.Refused): a broker answering 3xx, or anyone answering for
	// it, gets the 3xx back as the non-2xx it is, and its Location is sent nothing.
	proofed := redact.RefusalClient(redirects.Refused(&http.Client{Transport: NewDPoPTransport(c.key, base), Timeout: c.http.Timeout}))
	resp, err := proofed.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trade %s at %s: %w", c.subject.Describe(), shown, c.brokerURL.Error(err))
	}
	defer resp.Body.Close()
	read, err := redact.ReadAnswer(resp, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("read the broker's answer from %s: %w", shown, c.brokerURL.Error(err))
	}
	var answer tokenAnswer
	if err := read.Decode(&answer); err != nil && resp.StatusCode/100 == 2 {
		return nil, fmt.Errorf("the broker's answer from %s is not JSON: %w", shown, err)
	}
	if resp.StatusCode/100 != 2 {
		// The broker's own words are its answer's text, and a caller prints what it is
		// handed: `legion controller start` with %w, `agent-secrets self` on stderr, and
		// this client's own log. So they are redacted here, where the configured address
		// is known, rather than at each of those. On a clean address the redactor prints
		// the refusal's sentence - RefusedError is a redact.Printable over an answered
		// OAuth error with a code and no error_uri - or, where the broker's words hold a
		// control character, that answer's own quoted text; on one that is not clean it
		// prints the status alone, since a broker, or anyone answering for it, can repeat
		// the credential the address carries in the words it answers with; any other
		// answer prints its words as the address lets them (cipheraddr.Address.WordsOf).
		said, described := answer.Error, answer.ErrorDescription
		if resp.StatusCode/100 == 4 && !said.IsEmpty() {
			return nil, c.brokerURL.Error(newRefusedError(refusedIdentity(c.subject), resp.StatusCode, said, described))
		}
		return nil, fmt.Errorf("the broker at %s answered HTTP %d %s", shown, resp.StatusCode, c.brokerURL.WordsOf(said))
	}
	return c.read(shown, answer)
}

// read is the answer as a Token, refusing one that is not this grant's: no access token, a token
// type that is not DPoP, no expiry, or a refresh token — the exchange issues none, and a client
// that kept one would hold a credential this contract says is never minted. shown is the token
// endpoint as a message may print it (cipheraddr.Address.Call), never the endpoint as built.
//
// A refusal here quotes no value the broker wrote, as the TypeScript client's quotes none
// (packages/cipher-client, TokenClient's read): whatever answers for the broker can put in them the
// subject token it was sent, and a 200's values are read through no request record. Each of the
// three it reads is held as redact.RemoteText and read only through the box: the token type
// compared (EqualFold), the expiry and the credential's expiry parsed (Int64, Time), and none of
// them is kept as text.
func (c *Client) read(shown string, answer tokenAnswer) (*Token, error) {
	switch {
	case answer.AccessToken == "":
		return nil, fmt.Errorf("the broker at %s answered no access token", shown)
	case !answer.TokenType.EqualFold(TokenTypeDPoP.String()):
		return nil, fmt.Errorf("the broker at %s answered a token_type that is not DPoP, and this grant mints DPoP-bound tokens alone", shown)
	case answer.RefreshToken != "":
		return nil, fmt.Errorf("the broker at %s answered a refresh token, which this grant never issues; a renewal trades a fresh identity token instead", shown)
	}
	seconds, ok := answer.ExpiresIn.Int64()
	if !ok || seconds <= 0 {
		return nil, fmt.Errorf("the broker at %s answered no usable expires_in (a positive number of seconds)", shown)
	}
	now := c.now()
	token := &Token{
		AccessToken:  answer.AccessToken,
		TokenType:    TokenTypeDPoP,
		Expiry:       now.Add(time.Duration(seconds) * time.Second),
		CredentialID: answer.CredentialID,
	}
	token.renewsAt = c.renewalAt(token, now)
	if !answer.CredentialExpiresAt.IsEmpty() {
		expires, ok := answer.CredentialExpiresAt.Time(time.RFC3339)
		if !ok {
			return nil, fmt.Errorf("the broker at %s answered a %s that is no RFC 3339 time", shown, oauthwire.ExtraCredentialExpiresAt)
		}
		token.CredentialExpiresAt = expires.UTC()
	}
	return token, nil
}
