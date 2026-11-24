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

// Config is the cluster this provider trusts and the accounts the exchange lists.
type Config struct {
	// Issuer is the cluster's token issuer (BROKER_K8S_OIDC_ISSUER); "" trusts no cluster.
	Issuer string
	// Keys is the issuer's one key set, shared with every verifier of this issuer.
	Keys *oidc.Keys
	// ExchangeAudience is the audience a subject token traded at the exchange must carry
	// (BROKER_EXCHANGE_K8S_AUDIENCE); "" registers the subject token type unconfigured.
	ExchangeAudience string
	// Accounts and Reads are the exchange's account entries and read entries.
	Accounts []exchange.AccountEntry
	Reads    []exchange.ReadEntry
}

// Provider is the Kubernetes identity provider of one configuration.
type Provider struct {
	config   Config
	accounts map[string]exchange.AccountEntry
	reads    map[string]exchange.ReadEntry
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
		accounts: make(map[string]exchange.AccountEntry, len(c.Accounts)),
		reads:    make(map[string]exchange.ReadEntry, len(c.Reads)),
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
