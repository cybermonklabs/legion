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
// signing-in person belongs to, and the fleets the exchange lists, as LoadConfig reads them from
// the broker's environment. SignInDomain is the one field no variable of this provider's holds:
// BROKER_SIGNIN_DOMAIN is the broker's own sign-in setting, which config hands over.
type Config struct {
	// BROKER_GOOGLE_OIDC_ISSUER: the issuer of the Google identity tokens machines present,
	// https://accounts.google.com when unset, which is where every real one comes from. It is a
	// variable because a dev stack has no Google: cmd/dev-oidc-issuer stands in for it on a
	// loopback port, so a broker, its machines and Dispatch can be driven end to end on one
	// machine with no cloud. Read only where BROKER_GOOGLE_OIDC_AUDIENCE is set, which is what
	// turns Google identity tokens on at all.
	Issuer string
	// Keys is the issuer's one key set, shared with every verifier of this issuer; the identity
	// registry reads it at boot (identity/boot's Open).
	Keys *oidc.Keys
	// BROKER_GOOGLE_OIDC_AUDIENCE: the audience a machine's Google identity token must carry,
	// for a caller listed in BROKER_API_ACCOUNTS, for an edge listener a google: entry of
	// BROKER_NATS_SERVICES names, and at the RFC 8693 token exchange, where it is a subject token
	// (urn:envoy:params:oauth:token-type:google-identity). Its issuer is BROKER_GOOGLE_OIDC_ISSUER,
	// Google's own unless a dev stack says otherwise. Needs a Google caller, a google: entry in
	// BROKER_API_ACCOUNTS, a VM service in BROKER_VM_SERVICES or a google: edge-listener entry in
	// BROKER_NATS_SERVICES, since with none no token for it could ever be accepted; and any such
	// caller without it refuses the start. API callers and edge listeners need no VM service. The
	// exchange accepts only accounts listed in BROKER_VM_SERVICES, and
	// BROKER_SIGNIN_DEVICE_AUDIENCE needs a VM service for its machine actor token. Unset, the
	// broker accepts no Google identity token, and BROKER_VM_SERVICES, BROKER_VM_PROJECTS,
	// BROKER_VM_INSTANCES, BROKER_VM_EXCHANGE_ACCOUNTS, BROKER_VM_EXCHANGE_JOB_ACCOUNTS and
	// BROKER_SIGNIN_DEVICE_AUDIENCE are refused.
	Audience string
	// BROKER_SIGNIN_DEVICE_AUDIENCE: the audience a person's own Google identity token must
	// carry to be traded at the RFC 8693 token exchange, where such a token is a subject token
	// (urn:envoy:params:oauth:token-type:person-google-identity) and the machine the person
	// signed in on presents its own as the actor token
	// (urn:envoy:params:oauth:token-type:machine-google-identity). It is the OAuth client id of
	// the device-flow client `agent-secrets login` runs Google's device authorization grant as,
	// since Google puts the client's id in an ID token's aud. Its issuer is
	// BROKER_GOOGLE_OIDC_ISSUER, Google's own unless a dev stack says otherwise, and the person
	// it names is held to BROKER_SIGNIN_DOMAIN through the hd claim, exactly as a person
	// signing in to the broker's own approval page is. Needs BROKER_GOOGLE_OIDC_AUDIENCE and
	// BROKER_VM_SERVICES: the listed machines are the boxes a person may sign in on. Unset, the
	// broker accepts no person's sign-in and registers neither token type, so a caller presenting
	// either is refused by name. It is not one of the four SignInSettings, which are required
	// together: those are what a person approves a secret request through and no broker that
	// signs people in serves without them, where this one is optional, as the whole Google
	// exchange is. Beside BROKER_SIGNIN=off it is refused with them, since that broker holds no
	// BROKER_SIGNIN_DOMAIN to hold a person to.
	PersonAudience string
	// SignInDomain is BROKER_SIGNIN_DOMAIN, the Workspace a signing-in person must belong to,
	// which config hands this provider with its own settings. An empty one admits nobody
	// (oidc.WorkspaceLogin).
	SignInDomain string
	// BROKER_VM_EXCHANGE_ACCOUNTS, BROKER_VM_EXCHANGE_JOB_ACCOUNTS: what the token a machine of each
	// fleet is minted writes for. BROKER_VM_EXCHANGE_ACCOUNTS names the person and the Dispatch
	// project each fleet writes for, whitespace-separated
	// <service account email>=<project>=<person>[=<session>] entries, the grammar
	// BROKER_EXCHANGE_ACCOUNTS uses for a pod, such as
	// example-fleet@example-project.iam.gserviceaccount.com=EXAMPLE=ada@example.com. A token the
	// exchange mints for a machine of that account names that one project, acts for that
	// person, and writes under the session the entry fixes; an entry that fixes none requires the
	// machine to name its own session. The session is everything after the third "=", so it alone
	// may carry one. The person an entry names is also the one whose Dispatch Inbox lists a request
	// one of that fleet's machines makes for a shared human-tier secret; it reaches no secret.
	//
	// BROKER_VM_EXCHANGE_JOB_ACCOUNTS names a job's projects in place of a person,
	// whitespace-separated <service account email>=<projects>[=<session>] entries, such as
	// example-fleet@example-project.iam.gserviceaccount.com=public,PRIVA, the projects spelled as
	// BROKER_EXCHANGE_JOB_ACCOUNTS spells them. A token the exchange mints for a machine of that
	// account names the account's address as its service and no person, holds no OAuth scope, and
	// carries the entry's projects whatever the request names, as a job account's token does; the
	// launcher credential the exchange mints beside it still records the account's
	// BROKER_VM_SERVICES service. Every machine of such an account then writes to Dispatch with no
	// person signed in, under the session the entry fixes or, where it fixes none, the one the
	// machine names, so list an account here only when every machine that runs as it is that job's.
	// The session is everything after the second "=", so it alone may carry one.
	//
	// Refused at start, naming the setting and the entry: an entry of either setting not of its
	// form or with a blank field, an account no VM service may name, as BROKER_VM_SERVICES refuses
	// one, an account named twice, a job entry's projects of any other spelling, an account
	// BROKER_VM_SERVICES does not list, and an account both settings name, since one account has
	// one answer. An account BROKER_VM_SERVICES lists and neither setting names gets a token that
	// names its service and no person at all, which a resource server holds to what a service may
	// do. Both need BROKER_GOOGLE_OIDC_AUDIENCE. With both unset, every listed account's machines
	// get such a token.
	//
	// The entries this answers are the whole of what the exchange decides a machine's token on:
	// each one carries its account's service (BROKER_VM_SERVICES), the pins (BROKER_VM_PROJECTS
	// and BROKER_VM_INSTANCES) and, where the two settings name them, the person and the project
	// or the job's projects, and the session.
	Machines []GoogleEntry
	// BROKER_VM_SERVICES: the VM services, whitespace-separated name=<service account email>
	// entries, such as example-service=example-fleet@example-project.iam.gserviceaccount.com. A name
	// is lowercase letters, digits and hyphens, at most 64, not shared; the account is a user-managed
	// Google service account (<account id>@<project id>.iam.gserviceaccount.com, in lowercase, as a
	// token carries it) that the service's machines run as, so every machine of that account is one
	// identity. A
	// service may be named for several accounts, one entry each, but each account is given once and
	// proves exactly one service. A project's default service accounts (Compute Engine's and App
	// Engine's) and Google-managed service agents are refused: a project's workloads run as the first
	// by default and Google's own services as the second, so neither proves which VMs are the
	// service's. An account may be pinned to one Compute Engine project and one instance-name
	// pattern (BROKER_VM_PROJECTS and BROKER_VM_INSTANCES), or to neither, in which case its
	// machines are taken wherever they run. Keyed
	// by account, each answering its service. Needs BROKER_GOOGLE_OIDC_AUDIENCE. Unset, no VM service
	// is listed.
	ServicesByAccount map[string]string
	// NATSAudience is BROKER_NATS_GOOGLE_AUDIENCE, the audience a service's Google identity
	// token must carry at the NATS auth service (the NATS settings' own comment in config.Config
	// documents it with the rest of them); "" verifies no Google token there.
	NATSAudience string
}

// Provider is the Google identity provider of one configuration.
type Provider struct {
	config   Config
	machines map[string]GoogleEntry
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
	p := &Provider{config: c, machines: make(map[string]GoogleEntry, len(c.Machines))}
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
