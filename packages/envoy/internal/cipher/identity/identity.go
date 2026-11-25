// Package identity holds what every caller identity provider of the broker implements and what
// every consumer of one reads: a provider's kind, the token types it adds to the RFC 8693
// exchange, the caller verifier the broker's API and NATS auth service authenticate its tokens
// with, the exchange decision for its claims, and the principal and subject strings it writes.
//
// Each provider is one package beside this one (identity/kubernetes, identity/google,
// identity/github), and a build's providers are listed in identity/boot, one file per provider
// behind a build tag. Nothing outside a provider's package names its kind: the exchange, the
// API's callers, the NATS auth service and the enrollment service ask a Registry.
package identity

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/redact"
)

// Kind names one identity provider: "kubernetes", "google", "github".
type Kind string

// Account is an identity a provider verified, as the broker's settings name it: a Kubernetes
// service account's subject, a Google service account's email address, a workflow's
// <repository>/<path>@<ref>. Two accounts of different kinds never compare equal, however they
// are spelled.
type Account struct {
	Kind    Kind
	Address string
}

// Caller is a verified caller token: the account it names, its own `sub`, and the times its
// registered claims give it (IssuedAt zero for a token with no iat).
type Caller struct {
	Account  Account
	Subject  string
	IssuedAt time.Time
	Expiry   time.Time
}

// CallerVerifier verifies one provider's tokens for one audience, for a caller of the broker's
// API or of the NATS auth service.
type CallerVerifier interface {
	Kind() Kind
	// Issuer is the `iss` this verifier's tokens carry, which the NATS auth service picks a
	// verifier by.
	Issuer() string
	// VerifyCaller verifies raw for this verifier's audience. An error matching oidc.ErrIssuer
	// is another issuer's token; *NoAccountError is a sound token that names no account.
	VerifyCaller(ctx context.Context, raw string) (Caller, error)
}

// NoAccountError is a token whose issuer, audience, signature and expiry all hold but which names
// no account its provider reads one from. Reason says why, in the words an API caller is told
// (401 ACCOUNT_TOKEN_INVALID); Brief says it in the few words the NATS auth service logs the
// refused connection with.
type NoAccountError struct{ Reason, Brief string }

func (e *NoAccountError) Error() string { return e.Reason }

// KeySource answers the public key a token's header names: *oidc.Keys is one, reading the
// issuer's published set.
type KeySource interface {
	// Key is the public key a token naming kid and signed with alg is checked against.
	Key(ctx context.Context, kid, alg string) (crypto.PublicKey, error)
}

// Launcher says what launcher credential a trade of a token type mints beside its access token.
type Launcher int

const (
	// LauncherNone mints none: a pod's and a workflow run's trade.
	LauncherNone Launcher = iota
	// LauncherMachine mints the machine's own credential, which acts for no person.
	LauncherMachine
	// LauncherSignIn mints a person's sign-in credential on a machine, which acts for that person
	// and is held to the sign-in it records.
	LauncherSignIn
)

// TokenType is one token type a provider adds to the exchange. Name is the URN a request names it
// by. Issuer, Audience and Keys are the issuer its tokens come from, the audience they must
// carry, and the issuer's published keys: all three set is a configured type the exchange
// registers, none set a type the broker was not told about, whose tokens the library refuses
// before any handler runs.
type TokenType struct {
	Name, Issuer, Audience string
	Keys                   KeySource
	// SingleUse refuses a token presented twice (its jti), for a token minted per request.
	SingleUse bool
	// Tolerance is the lifetime window the library holds a token's iat and exp to.
	Tolerance time.Duration
	// ActorOnly is a type registered as an actor token type only, which no trade presents as its
	// subject.
	ActorOnly bool
	// Actor is the actor token type a trade of this subject type carries, "" for none. A trade
	// of a type with no actor never reads the request's actor token.
	Actor string
	// Launcher is the launcher credential a trade of this type mints.
	Launcher Launcher
}

// Configured reports whether the broker was told about this type's issuer at all.
func (t TokenType) Configured() bool {
	return t.Issuer != "" || t.Audience != "" || t.Keys != nil
}

// Validate refuses a type that is configured in part: half a configuration would register a type
// whose tokens could not be checked, which is worse than not accepting that kind of caller.
func (t TokenType) Validate() error {
	if !t.Configured() {
		return nil
	}
	if t.Issuer == "" || t.Audience == "" || t.Keys == nil {
		return fmt.Errorf("identity: the token type %s is configured in part (issuer %q, audience %q, keys %t); it needs all three or none", t.Name, t.Issuer, t.Audience, t.Keys != nil)
	}
	return nil
}

// Trade is one exchange a provider decides: the subject token's type, the verified claims of the
// subject token and of the actor token (nil for none), the session name the request names, and
// the projects it asks for.
type Trade struct {
	TokenType string
	Subject   map[string]any
	Actor     map[string]any
	Session   string
	Projects  []string
}

// SubjectKind says which kind of identity a launcher credential's subject names. The zero value
// is a subject no provider claims: a Kubernetes token login's, which the broker holds to a
// setting of its own.
type SubjectKind string

const (
	// SubjectOther is every subject no provider wrote as a machine's or a sign-in's.
	SubjectOther SubjectKind = ""
	// SubjectMachine is a machine's own credential, which acts for no person.
	SubjectMachine SubjectKind = "machine"
	// SubjectSignIn is a person's sign-in on a machine, whose credential acts for that person.
	SubjectSignIn SubjectKind = "signin"
)

// Subject is a launcher credential's subject read back: its kind, the account it carries (the
// machine's, for a sign-in the box's) and that account's principal. Every field is empty for
// SubjectOther.
type Subject struct {
	Kind      SubjectKind
	Account   Account
	Principal string
}

// Subjects reads the subjects and spells the principals that enrollment and requests act on.
type Subjects interface {
	// ReadSubject answers SubjectOther for a subject no provider wrote, a malformed one included.
	ReadSubject(subject string) Subject
	// PodPrincipal is the pod provider's principal of a service account, "" without one.
	PodPrincipal(account string) string
}

// AccountClaims are a verified service-account token's subject, the pod it is bound to ("" for
// none), its expiry, and when it was issued (zero for a token with no iat).
type AccountClaims struct {
	Subject, PodUID  string
	Expiry, IssuedAt time.Time
}

// PodClaims are a verified pod token's subject and the pod it is bound to.
type PodClaims struct {
	Subject, PodUID string
}

// PodVerifier verifies the cluster's projected service-account tokens: Verify a pod's own token,
// bound to its pod, for enrollment and renewal; VerifyAccount a service's token, bound to a pod or
// to none, for its token login.
type PodVerifier interface {
	Verify(ctx context.Context, raw string) (PodClaims, error)
	VerifyAccount(ctx context.Context, raw string) (AccountClaims, error)
}

// Provider is one caller identity provider.
type Provider interface {
	Kind() Kind
	// TokenTypes is every type it registers, configured or not.
	TokenTypes() []TokenType
	// Listings are the identities its entries list, each spelled with its principal.
	Listings() []exchange.Listing
	// Decide answers the claims a trade of one of its subject types mints.
	Decide(trade Trade) (exchange.Claims, error)
	// LauncherSubject is the subject and host of the launcher credential a trade of a type whose
	// Launcher is not LauncherNone mints, thumbprint being the key the trade's proof presented.
	LauncherSubject(trade Trade, claims exchange.Claims, thumbprint string) (subject, host string)
	// CallerVerifier verifies its tokens for audience, for the API's and NATS's callers.
	CallerVerifier(audience string) CallerVerifier
	// ReadSubject claims a launcher subject this provider wrote, and false for any other.
	ReadSubject(subject string) (Subject, bool)
	// Principal spells the principal of one of its accounts.
	Principal(address string) string
}

// PodProvider is the provider whose tokens are pods' own: a pod enrolls and registers its session
// with them.
type PodProvider interface {
	Provider
	// PodVerifier verifies pod tokens for audience; nil on a provider with no issuer.
	PodVerifier(audience string) PodVerifier
	// Registration refuses what a pod-session registration must refuse before it is written.
	Registration(account, podUID, session string) error
}

// Use names what a caller verifier authenticates callers of.
type Use int

const (
	// UseAPI is a caller of the broker's own API (BROKER_API_ACCOUNTS).
	UseAPI Use = iota
	// UseNATS is a service connecting to the hub through the NATS auth service.
	UseNATS
)

// Site is where the broker's settings met an account a provider must serve: the setting that names
// it, the entry's text where the provider's sentence quotes one ("" for a setting whose refusal
// names no entry), and whether that entry is an edge listener's, which the API serves whether or
// not the NATS auth service runs.
type Site struct {
	Setting, Entry string
	Edge           bool
}

// Mention is one place a provider's own settings name an account or a service: the variable and
// the entry's text, the person it acts for ("" for none), the service it names, an account
// listed to read alone (ReadOnly) among them, and the job list a job entry gives it. A setting a
// provider does not own is checked against what the provider's settings say through these.
type Mention struct {
	Variable, Entry, Person, Service string
	ReadOnly                         bool
	// Job is the job list a job entry gives the account in place of a person; the zero
	// JobProjects for every other entry.
	Job exchange.JobProjects
}

// ErrAccountForm is what every refusal of an account written in no provider's form wraps: the
// registry's, for a setting that names a caller, and a provider's own, for a setting that names
// an account of its kind. A setting matches it (errors.Is) to refuse in its own words, naming
// its entry form.
var ErrAccountForm = errors.New("the account is written in no form this build of cipherd reads")

// --- What a machine's own client proves, the other half of a provider: where the server verifies
// a token, the client makes one. Each provider declares the sources its kind of machine holds,
// and the clients choose among them through identity/providers. ---

// MachineTokenFileVar and MachineAudienceVar are read by sources of more than one provider — a
// pod's projected token file is also what a workload identity pool federates, and the machine
// audience is the Google token's — so they are named here rather than in one provider.
const (
	MachineTokenFileVar = "AGENT_SECRETS_MACHINE_TOKEN_FILE"
	MachineAudienceVar  = "AGENT_SECRETS_MACHINE_AUDIENCE"
)

// Source is one platform identity a process can prove, read fresh each time it is asked: the
// subject token of the broker's RFC 8693 exchange, and the type the exchange reads it under. The
// grant issues no refresh token, so every renewal asks its Source again — a projected token the
// kubelet rewrote in place, a metadata token minted anew, a pool token traded again, a GitHub
// Actions run's token asked of GitHub again — and a Source that caches must cache for less than
// the token it answers lives.
type Source interface {
	// Token is the subject token and its type, read now.
	Token(ctx context.Context) (token, tokenType string, err error)
	// Describe names the identity in a log line and in a refusal, naming no credential.
	Describe() string
}

// Variable is one environment variable a source reads, and whether its value is a path, which a
// program that writes a machine's environment (the helper's install) treats as one. Platform
// marks a variable the platform sets for each run, such as the URL and token GitHub gives one
// job: it is no deployment's setting, so no such program carries it over.
type Variable struct {
	Name     string
	Path     bool
	Platform bool
}

// SourceEntry is one identity AGENT_SECRETS_MACHINE_IDENTITY names, declared by the provider
// whose tokens it makes.
type SourceEntry struct {
	// Name is the value AGENT_SECRETS_MACHINE_IDENTITY takes for it.
	Name string
	// Provider is the kind of provider that verifies its tokens.
	Provider Kind
	// Launcher says the broker mints a launcher credential for this source's tokens, so a
	// machine naming it can register a session of its own; false for a pod's projected token.
	Launcher bool
	// Variables are the variables it reads, in the order it asks for them.
	Variables []Variable
	// FromEnv builds the source, asking need for each variable it cannot do without; need
	// answers the refusal a missing one gets.
	FromEnv func(need func(variable string) (string, error), client *http.Client) (Source, error)
}

// Skipped is one place a detector looked and found nothing it could trade, and why, in words a
// refusal prints as they stand.
type Skipped struct {
	Source, Reason string
}

// Detector finds a machine's identity where no variable names one, from a field of the broker's
// own GET /v1/settings answer.
type Detector struct {
	Provider Kind
	// SettingsField is the field of the settings answer that carries the audience it needs
	// ("google_exchange_audience").
	SettingsField string
	// KeptFile is the file name a client keeps that audience under, beside its credentials.
	KeptFile string
	// Words is how a message names the audience ("Google exchange audience").
	Words string
	// Detect is the identity this machine holds for audience, with the places looked first.
	Detect func(ctx context.Context, audience string, client *http.Client) (Source, []Skipped, error)
}

// TokenReader reads a service's own platform token for the NATS auth service; it is natscred's
// TokenReader shape, which cannot be named here because natscred reads this registry.
type TokenReader interface {
	Read() (string, error)
}

// ServiceTokenSource is one way a service of the bus proves itself to the NATS auth service with
// a token of its provider's kind, declared by that provider: the variable a process names it by,
// natsprobe's flag for it, the row `envoy-dispatch settings` prints, and the words a line naming
// the credential a process connects as (bus.CredentialName) puts before the variable's value.
type ServiceTokenSource struct {
	Provider    Kind
	Variable    string
	Flag        string
	Usage       string
	Description string
	// Named is how bus.CredentialName names this credential, followed by the variable's value:
	// "the Google identity token for the audience".
	Named  string
	Reader func(value string) TokenReader
}

// ReadTokenFile is the token in the file at path, trimmed, where described is how a message names
// that file ("the projected service-account token <variable> names"). A file that cannot be read,
// or that holds nothing, is refused by those words alone and never by its path: for a person who
// pasted a token into that variable, the path is the token (redact.File).
func ReadTokenFile(path, described string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", described, redact.File(err))
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s is empty", described)
	}
	return token, nil
}

// AudienceText names the audience MachineAudienceVar sets, as an identity's Describe prints it:
// as written when it is a plain http(s) URL (internal/redact's allow-list), and by the variable
// alone otherwise, since the audience is a configured value a person can paste a credential into,
// and an identity's Describe is in every trade's log line and error.
func AudienceText(audience string) string {
	if redact.Clean(audience) {
		return audience + " (" + MachineAudienceVar + ")"
	}
	return "the audience " + MachineAudienceVar + " names"
}
