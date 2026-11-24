// Package google is the broker's Google identity provider: a machine's Google identity token,
// traded at the RFC 8693 exchange for the claims its fleet's entry gives it and the launcher
// credential it holds; a person's own Google identity token, traded with the machine's as the
// actor token for the person's sign-in on that machine; and a machine's token presented as an API
// or NATS caller.
package google

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// Kind is this provider's kind.
const Kind identity.Kind = "google"

// The token types this provider registers: a machine's own identity token as a subject, a
// person's own identity token as a subject, and the machine's token as the actor of a person's
// trade.
const (
	SubjectTokenType      = oauthwire.SubjectTokenTypeGoogle
	PersonTokenType       = oauthwire.SubjectTokenTypePerson
	ActorTokenTypeMachine = oauthwire.ActorTokenTypeMachine
)

// tolerance admits a machine's Google identity token, which Google mints for an hour and which its
// metadata server hands out again until it is nearly spent, so the window has to cover a whole
// lifetime rather than a fresh token alone.
const tolerance = time.Hour

// googleAccountPattern is a Google service account's email address as a Google identity token
// carries it in its `email` claim: lowercase, since that is the one spelling a token holds. It is
// the grammar the broker's configuration states for the same accounts.
var googleAccountPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]*@[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)

// Config is the issuer this provider trusts, the audiences it holds tokens to, the Workspace a
// signing-in person belongs to, and the fleets the exchange lists.
type Config struct {
	// Issuer is BROKER_GOOGLE_OIDC_ISSUER.
	Issuer string
	// Keys is the issuer's one key set, shared with every verifier of this issuer.
	Keys *oidc.Keys
	// Audience is BROKER_GOOGLE_OIDC_AUDIENCE, what a machine's identity token must carry as a
	// subject and as an actor; "" registers the machine's subject token type unconfigured.
	Audience string
	// PersonAudience is BROKER_SIGNIN_DEVICE_AUDIENCE, what a person's own identity token must
	// carry: the device-flow client `agent-secrets login` runs Google's device grant as. It is an
	// audience of its own, so neither a machine's token nor a person's is spendable where the
	// other is. "" registers the person subject type and the machine actor type unconfigured.
	PersonAudience string
	// SignInDomain is BROKER_SIGNIN_DOMAIN, the Workspace a signing-in person must belong to. An
	// empty one admits nobody (oidc.WorkspaceLogin).
	SignInDomain string
	// Machines are the exchange's Google entries.
	Machines []exchange.GoogleEntry
}

// Provider is the Google identity provider of one configuration.
type Provider struct {
	config   Config
	machines map[string]exchange.GoogleEntry
	listings []exchange.Listing
}

var _ identity.Provider = (*Provider)(nil)

// errPersonWithoutMachines refuses a person's audience without the machine's: the machine the
// person signs in on presents its own identity token as the actor.
var errPersonWithoutMachines = errors.New("oauth: a broker that accepts a person's Google identity token needs the Google issuer too: the machine the person signs in on presents its own identity token as the actor, and the entries that list that machine are the Google ones")

// New validates every entry and builds the provider. It refuses a person's audience with no
// machine audience, and, naming the entry, an account that is not a lowercase email address, an
// entry with no service, half a pin (a Compute Engine project and no instance pattern, or the
// other way round), an instance pattern that is not a pattern, half a grant (a person and no
// project, or the other way round), a job list beside a person or a project, and an account
// listed twice. A session name two entries fix is refused where the registry composes every
// provider's listings (exchange.NewSet), with the same words.
func New(c Config) (*Provider, error) {
	if c.PersonAudience != "" && c.Audience == "" {
		return nil, errPersonWithoutMachines
	}
	p := &Provider{config: c, machines: make(map[string]exchange.GoogleEntry, len(c.Machines))}
	for i, entry := range c.Machines {
		where := fmt.Sprintf("google entry %d, %q", i+1, entry.Account)
		if !googleAccountPattern.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: a Google account is the lowercase email address its identity tokens carry", where)
		}
		if strings.TrimSpace(entry.Service) == "" {
			return nil, fmt.Errorf("exchange: %s: the service is required, since a machine whose entry names no person is its service and nothing else", where)
		}
		if (entry.CloudProject == "") != (entry.Instance == "") {
			return nil, fmt.Errorf("exchange: %s: an entry pins the Compute Engine project and the instance names together or neither (project %q, instance names %q)", where, entry.CloudProject, entry.Instance)
		}
		if _, err := path.Match(entry.Instance, ""); err != nil {
			return nil, fmt.Errorf("exchange: %s: the instance-name pattern %q is not a pattern: %w", where, entry.Instance, err)
		}
		if !entry.Job.Listed() && (entry.Project == "") != (entry.Person == "") {
			return nil, fmt.Errorf("exchange: %s: an entry names the person and the project together or neither (project %q, person %q)", where, entry.Project, entry.Person)
		}
		if entry.Job.Listed() || entry.Person != "" {
			if err := exchange.CheckGrant(where, entry.Person, entry.Project, entry.Job, entry.Session, false); err != nil {
				return nil, err
			}
		} else if entry.Session != "" && !exchange.ValidSession(entry.Session) {
			// An entry naming neither a person nor a job list is a machine that is its service and
			// nothing else, so its session is all there is to check.
			return nil, fmt.Errorf("exchange: %s: %s", where, exchange.SessionRule)
		}
		if _, listed := p.machines[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account writes as one identity", where)
		}
		p.machines[entry.Account] = entry
		p.listings = append(p.listings, exchange.Listing{
			Where: where, Principal: Principal(entry.Account), Person: entry.Person, Project: entry.Project,
			Session: entry.Session, Service: entry.Service, Job: entry.Job,
		})
	}
	return p, nil
}

// Kind is Kind.
func (p *Provider) Kind() identity.Kind { return Kind }

// TokenTypes are the machine's subject type, configured with Audience, and the person's subject
// type and the machine's actor type, configured together with PersonAudience: a person's trade
// carries both, and neither is presentable alone. None is single use. A machine reads one identity
// token from its metadata server and is handed the same token until it is nearly spent, and a
// person's sign-in mints one from Google's refresh token whenever one is wanted, so single use
// would refuse every renewal; what keeps a copied token from being spendable is the DPoP key the
// minted token is bound to, and for a sign-in the box key the sign-in is recorded against.
func (p *Provider) TokenTypes() []identity.TokenType {
	machine := identity.TokenType{Name: SubjectTokenType, Tolerance: tolerance, Launcher: identity.LauncherMachine}
	person := identity.TokenType{Name: PersonTokenType, Tolerance: tolerance, Actor: ActorTokenTypeMachine, Launcher: identity.LauncherSignIn}
	actor := identity.TokenType{Name: ActorTokenTypeMachine, Tolerance: tolerance, ActorOnly: true}
	if p.config.Audience != "" {
		p.configure(&machine, p.config.Audience)
	}
	if p.config.PersonAudience != "" {
		p.configure(&person, p.config.PersonAudience)
		p.configure(&actor, p.config.Audience)
	}
	return []identity.TokenType{machine, person, actor}
}

// configure points tt at the provider's issuer and keys, for audience.
func (p *Provider) configure(tt *identity.TokenType, audience string) {
	tt.Issuer, tt.Audience = p.config.Issuer, audience
	if p.config.Keys != nil {
		tt.Keys = p.config.Keys
	}
}

// Listings are the Google entries, each under its account's principal and carrying the service
// BROKER_VM_SERVICES lists the account for.
func (p *Provider) Listings() []exchange.Listing {
	return append([]exchange.Listing(nil), p.listings...)
}

// Decide answers a machine's trade (machine) and a person's sign-in (person).
func (p *Provider) Decide(trade identity.Trade) (exchange.Claims, error) {
	switch trade.TokenType {
	case SubjectTokenType:
		return p.machine(trade.Subject, trade.Session, trade.Projects)
	case PersonTokenType:
		return p.person(trade.Subject, trade.Actor, trade.Session, trade.Projects)
	}
	return exchange.Claims{}, fmt.Errorf("google: the %s token type is not one this provider decides", trade.TokenType)
}

// Principal is the principal of a Google account.
func (p *Provider) Principal(address string) string { return Principal(address) }

// Principal is a Google account's spelling of a principal: the fleet a machine's own identity
// belongs to, led by its kind so it never spells another kind's principal. Every machine of one
// account is one holder of a session name. Postgres holds this spelling in
// oauth_session_bindings.principal and enrollments.carry_principal, so it never changes.
func Principal(account string) string { return "google:" + account }
