// Package kubernetes is the broker's Kubernetes identity provider: a pod's projected
// service-account token, verified against the cluster's issuer, traded at the RFC 8693 exchange
// for the claims an account entry or a read entry gives it, enrolled as the pod it is bound to,
// and presented by a service as an API or NATS caller.
package kubernetes

import (
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// Kind is this provider's kind.
const Kind identity.Kind = "kubernetes"

// SubjectTokenType is the token type a pod's projected token is traded under.
const SubjectTokenType = oauthwire.SubjectTokenTypeKubernetes

// tolerance admits a projected service-account token's whole life. Kubernetes bounds one at 24
// hours, so a shorter window would refuse a freshly minted token outright for expiring too far in
// the future. It is also what a pod-session registration is kept for past its last write
// (oauthstore.PodSessionRetention), since a registration must outlive every token the pod it was
// written for can still present.
const tolerance = 24 * time.Hour

// Config is the cluster this provider trusts, the audiences its tokens are verified at and the
// accounts the exchange lists, as LoadConfig reads them from the broker's environment.
type Config struct {
	// BROKER_K8S_OIDC_ISSUER: the issuer of the Kubernetes service-account tokens pods enroll
	// with. Set it with BROKER_K8S_OIDC_AUDIENCE, or neither, in which case no pod can enroll.
	Issuer string
	// BROKER_K8S_OIDC_AUDIENCE: the audience a pod's projected service-account token must carry.
	Audience string
	// Keys is the issuer's one key set, shared with every verifier of this issuer; the identity
	// registry reads it at boot (identity/boot's Open).
	Keys *oidc.Keys
	// BROKER_EXCHANGE_K8S_AUDIENCE: the audience a pod's projected service-account token must
	// carry to be exchanged (RFC 8693) for a Dispatch access token. Its issuer is the one cluster
	// the broker is told about, BROKER_K8S_OIDC_ISSUER, which this needs; the audience is its own
	// because an exchanging pod projects a token for it beside the one it enrolls with
	// (BROKER_K8S_OIDC_AUDIENCE), so neither token is spendable where the other is. Unset, no
	// pod exchanges.
	ExchangeAudience string
	// BROKER_EXCHANGE_ACCOUNTS, BROKER_EXCHANGE_JOB_ACCOUNTS: the Kubernetes service accounts whose
	// pods exchange their projected token, BROKER_EXCHANGE_JOB_ACCOUNTS' entries after
	// BROKER_EXCHANGE_ACCOUNTS' own. BROKER_EXCHANGE_ACCOUNTS lists them as whitespace-separated
	// <service account>=<project>=<person>[=<session>] entries, such as
	// system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com. The token minted
	// for a pod of the account names that one project, a project key that exchange.NewSet
	// refuses the start over when it is not one, acts for that person, and writes under
	// the session the entry fixes; an entry that fixes none requires the pod to name its own
	// session. An entry may name no project (<service account>==<person>): the shared
	// public-session account's, whose tokens act for that person and name no project. An entry
	// may name neither a project nor a person (<service account>==): an account whose
	// tokens name no person and no project and act as the account itself, which Dispatch records
	// their writes under. An entry naming a project and no person is refused: an entry that names
	// a project names its person, so no account entry's token that names a project is personless. An
	// entry whose fixed session is legion-daemon:<its own project> is a Legion daemon's own: the
	// token minted for its pods holds the OAuth scope legion-status too. Dispatch reads that scope
	// (DISPATCH_LEGION_STATUS_SCOPE): where the setting is on, a write of a Legion issue's status
	// needs it or the person scope, but for a status write of the architect of an issue
	// above, which writes with its own broker token. No other token the exchange mints holds legion-status,
	// whatever session it carries. The session is everything after the third "=", so it alone
	// may carry one. A namespace's default account is refused, as is an account listed twice.
	// Needs BROKER_K8S_OIDC_ISSUER and
	// BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present. A broker of a build older than
	// the entry naming no person refuses to start on one. Unset, no account exchanges.
	//
	// BROKER_EXCHANGE_JOB_ACCOUNTS: the Kubernetes service accounts whose pods run a job that
	// writes for no person, whitespace-separated <service account>=<projects>[=<session>] entries,
	// such as system:serviceaccount:example-ns:example-job=EXAMPLE or
	// system:serviceaccount:example-ns:example-sweeper=public,PRIVA,PRIVB=example-sweeper. The
	// projects are one project key, or public followed by ,<key> for each of at most 16 private
	// projects, the keys in any order, public counting toward none (exchange.ParseJobProjects).
	// The token minted for a pod of the account names the account as its service and no person,
	// holds no OAuth scope, so a route that needs one refuses it before Dispatch's project check
	// runs, and carries the entry's projects whatever the exchange request names: the
	// project scope named and its one project, for which that check judges the token as it judges a
	// person's named token for that project, or the project scope public and the private projects,
	// beside which it reaches every public project. At this release Dispatch does not read a public
	// token's list, so such a token writes wherever a token that names no project may. A request
	// may name no project or only projects of the entry, and one naming any other is refused. It
	// writes under the session the entry fixes; an entry that
	// fixes none requires the pod to name its own session, which a registration
	// (BROKER_POD_REGISTRARS) may choose for it. The session is everything after the second "=", so
	// it alone may carry one. Refused at start, naming the
	// setting and the entry: an entry not of that form or with a blank account; an account that is
	// not system:serviceaccount:<namespace>:<name>, or is a namespace's default account; an
	// account listed twice; projects of any other spelling; an account BROKER_EXCHANGE_ACCOUNTS or
	// BROKER_EXCHANGE_READ_ACCOUNTS lists too, since one account has one answer; and an account
	// BROKER_POD_OPERATORS maps to a person. Needs BROKER_K8S_OIDC_ISSUER and
	// BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present. A broker of a build older than
	// this setting does not read it, so it mints those accounts no token at all. Unset, no account
	// exchanges for a job's token.
	Accounts []AccountEntry
	// BROKER_EXCHANGE_READ_ACCOUNTS: the Kubernetes service accounts whose pods exchange their
	// projected token for one that only reads, whitespace-separated
	// <service account>=<service>=<session> entries, such as
	// system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session. The
	// token minted for a pod of the account holds only the OAuth scope read, the project scope
	// none, the service and the session, and names no person and no project: Dispatch serves it
	// on every GET route it classes as a read and refuses it everywhere else, and every other
	// service that reads the broker's tokens, the Envoy listener among them, refuses it. So that
	// token reads everything a read route answers, across every project: issues, documents, asks,
	// comments and the rest, closed and iceboxed ones included. No setting holds a read entry to
	// some projects.
	// The session is everything after the second "=", so it alone may carry one. A namespace's
	// default account is refused, as is a service that is not lowercase letters, digits and
	// hyphens, at most 64, or is shared, an account listed twice, and an account
	// BROKER_EXCHANGE_ACCOUNTS lists too. Needs BROKER_K8S_OIDC_ISSUER and
	// BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present. A broker of a build older
	// than this setting does not read it, so it mints those accounts no token at all. Unset, no
	// account exchanges for a read-only token.
	Reads []ReadEntry
	// NATSAudience is BROKER_NATS_K8S_AUDIENCE, the audience a service's projected token must
	// carry at the NATS auth service (the NATS settings' own comment in config.Config documents
	// it with the rest of them); "" verifies no Kubernetes token there.
	NATSAudience string
	// BROKER_POD_REGISTRATION_AUDIENCE: an audience POST /v1/oauth/exchange/pod-sessions accepts
	// for the pod token a registration's body carries, beside the broker's own
	// BROKER_K8S_OIDC_AUDIENCE, which a daemon presents today: a token of either audience,
	// verified against BROKER_K8S_OIDC_ISSUER, which this needs, names the pod, and an expired
	// one of either is 401 POD_TOKEN_EXPIRED. No other route accepts a token of this audience:
	// enrolment and every ServiceAccount login verify at the broker's own audience, the token
	// exchange at BROKER_EXCHANGE_K8S_AUDIENCE and the NATS auth service at
	// BROKER_NATS_K8S_AUDIENCE, so a registrar may be handed a pod's token that registers the pod
	// and is spendable nowhere else. Read trimmed. Refused at start, naming it and the other
	// setting, when it equals any of those three, each compared when it is set, since a token of
	// it would then be spendable on that setting's routes too. Unset, the route accepts the
	// broker's own audience alone.
	PodRegistrationAudience string
}

// Provider is the Kubernetes identity provider of one configuration.
type Provider struct {
	config   Config
	accounts map[string]AccountEntry
	reads    map[string]ReadEntry
	listings []exchange.Listing
}

var _ identity.PodProvider = (*Provider)(nil)

// New validates every entry and builds the provider. It refuses, naming the entry, an account
// that is not a service account's subject, an entry missing what it must name, an account listed
// twice, and an account both an account entry and a read entry list. A session name two entries
// fix is refused where the registry composes every provider's listings
// (exchange.NewSet), with the same words.
func New(c Config) (*Provider, error) {
	p := &Provider{
		config:   c,
		accounts: make(map[string]AccountEntry, len(c.Accounts)),
		reads:    make(map[string]ReadEntry, len(c.Reads)),
	}
	for i, entry := range c.Accounts {
		where := fmt.Sprintf("account entry %d, %q", i+1, entry.Account)
		if !oauthwire.AccountSubject.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: an account is system:serviceaccount:<namespace>:<name>", where)
		}
		if err := exchange.CheckGrant(where, entry.Person, entry.Project, entry.Job, entry.Session, true); err != nil {
			return nil, err
		}
		if _, listed := p.accounts[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account writes as one identity", where)
		}
		p.accounts[entry.Account] = entry
		p.listings = append(p.listings, exchange.Listing{
			Where: where, Principal: Principal(entry.Account), Person: entry.Person, Project: entry.Project,
			Session: entry.Session, Job: entry.Job,
		})
	}
	for i, entry := range c.Reads {
		where := fmt.Sprintf("read entry %d, %q", i+1, entry.Account)
		if !oauthwire.AccountSubject.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: an account is system:serviceaccount:<namespace>:<name>", where)
		}
		if strings.TrimSpace(entry.Service) == "" {
			return nil, fmt.Errorf("exchange: %s: the service is required, since a read entry's token names its service and no person", where)
		}
		if !exchange.ValidSession(entry.Session) {
			return nil, fmt.Errorf("exchange: %s: the session is required, since a read entry fixes the session its token names, and %s", where, exchange.SessionRule)
		}
		if _, listed := p.reads[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account reads as one identity", where)
		}
		if _, listed := p.accounts[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: an account entry lists the same account, and one account mints one kind of token", where)
		}
		p.reads[entry.Account] = entry
		p.listings = append(p.listings, exchange.Listing{Where: where, Principal: Principal(entry.Account), Session: entry.Session, Service: entry.Service})
	}
	return p, nil
}

// Kind is Kind.
func (p *Provider) Kind() identity.Kind { return Kind }

// TokenTypes is the one subject token type, configured where ExchangeAudience is set. A pod's
// projected token is a standing credential its kubelet refreshes, and a long-running role
// exchanges many times with the one it holds, so it is not single use; what keeps a token from
// being spendable outside a pod is the pod binding the decision requires.
func (p *Provider) TokenTypes() []identity.TokenType {
	tt := identity.TokenType{Name: SubjectTokenType, Tolerance: tolerance, Launcher: identity.LauncherNone}
	if p.config.ExchangeAudience != "" {
		tt.Issuer, tt.Audience = p.config.Issuer, p.config.ExchangeAudience
		if p.config.Keys != nil {
			tt.Keys = p.config.Keys
		}
	}
	return []identity.TokenType{tt}
}

// Listings are the account entries and then the read entries, each under its account's
// principal.
func (p *Provider) Listings() []exchange.Listing {
	return append([]exchange.Listing(nil), p.listings...)
}

// Decide answers a pod's trade (Pod).
func (p *Provider) Decide(trade identity.Trade) (exchange.Claims, error) {
	if trade.TokenType != SubjectTokenType {
		return exchange.Claims{}, fmt.Errorf("kubernetes: the %s token type is not one this provider decides", trade.TokenType)
	}
	return p.pod(trade.Subject, trade.Session, trade.Projects)
}

// LauncherSubject is never asked: a pod's trade mints no launcher credential.
func (p *Provider) LauncherSubject(identity.Trade, exchange.Claims, string) (string, string) {
	return "", ""
}

// ReadSubject claims no launcher subject: a Kubernetes token login's subject is the account
// itself, which the broker holds to BROKER_SERVICE_LOGINS rather than to an entry.
func (p *Provider) ReadSubject(string) (identity.Subject, bool) { return identity.Subject{}, false }

// Principal is the principal of a service account.
func (p *Provider) Principal(address string) string { return Principal(address) }
