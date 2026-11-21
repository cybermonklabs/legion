// Package oauthwire is the wire contract of the broker's OAuth endpoints that the broker and the
// callers it answers share: the clients they present, the scope a machine login asks for and the
// one a read entry's token holds, the names the token exchange knows a subject token by, the
// fields the broker adds to a token answer, and the one grammar of a token's list of projects. It
// imports nothing of the broker — only the standard library and the project-key rule
// internal/contracts generates — so the agent-secrets helper reads it without the broker's
// storage.
package oauthwire

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// tokenTypeURN begins each token type RFC 8693 Section 3 names. SubjectTokenTypeAccessToken is
// built from it because the public leak check reads the whole URN as an access token.
const tokenTypeURN = "urn:ietf:params:oauth:token-type:"

const (
	// ClientID is the OAuth client the agent-secrets helper logs a machine in as, and the client
	// whose access token a dev box session exchanges for a Dispatch one.
	ClientID = "agent-secrets-helper"
	// Scope is the one scope a machine login asks for and is granted.
	Scope = "machine"
	// ScopeRead is the one OAuth scope a token minted from a read entry
	// (BROKER_EXCHANGE_READ_ACCOUNTS) holds, and the only scope the exchange grants: a token
	// that writes nothing and reads only the routes a resource server lists for it, which
	// Dispatch does in its read list and every other service refuses (ciphertoken). The entry
	// decides it, never the request: the exchange client is registered for Scope alone, so the
	// library refuses a request that names this one invalid_scope under every entry.
	ScopeRead = "read"
	// ExtraCredentialID is the token answer's field naming the launcher credential the login holds;
	// the login's session carries it under the same key.
	ExtraCredentialID = "credential_id"
	// ExtraCredentialExpiresAt is the token answer's field holding that credential's expiry
	// (RFC 3339).
	ExtraCredentialExpiresAt = "credential_expires_at"
	// ExchangeClientID is the OAuth client every caller asking the RFC 8693 token exchange for a
	// Dispatch access token presents: a pod, a workflow run, and a dev box session exchanging the
	// access token its machine login holds. It is registered confidential, because the library
	// refuses a public client that grant, but it presents no client credential: the subject
	// token it carries and the proof of its own key are what the broker judges the request on.
	ExchangeClientID = "dispatch-token-exchange"
	// FormSession is the exchange request's own form parameter: the session name the minted
	// token carries. RFC 8693 registers none, so the broker defines this one; an entry
	// that fixes a session refuses a request naming another, and an entry that fixes none
	// requires it.
	FormSession = "session"
	// FormLauncherCredential is the exchange request's second broker-defined form parameter:
	// whether this trade wants the launcher credential its identity holds minted or moved onto
	// the key it proved. Only a caller that goes on to enrol a session and read a secret does —
	// `agent-secrets`, the helper, a person's sign-in — and for those it is the whole point of
	// the trade, so an absent parameter means yes and every client that was written before this
	// parameter keeps its credential. "false" is the one value that declines, and the broker
	// refuses any other spelling rather than guessing.
	//
	// A caller that wants a Dispatch access token and nothing else sends "false": each trade
	// that asks writes a launcher_credentials row keyed by the machine and the key it proved
	// (exchange.MachineSubject) and a token_login audit row, and a key is one process's, so a
	// fleet of short-lived callers that never read a secret would fill an operator's
	// machine-logins list with rows nothing ever removes. Every TypeScript client's machine trade
	// declines (packages/cipher-client/src/environment.ts, brokerTokenClient): none of them reads
	// ExtraCredentialID. A TypeScript client's trade of a person's sign-in sends nothing, since a
	// person's trade may not decline.
	FormLauncherCredential = "launcher_credential"
	// FormLauncherCredentialDeclined is the one value of FormLauncherCredential that declines.
	FormLauncherCredentialDeclined = "false"
	// FormProjects is the exchange request's third broker-defined form parameter: the projects
	// the minted token names, as project keys separated by white space (ParseProjects). A trade
	// that acts as its person names any of the projects that person may see, and the broker asks
	// Dispatch whether they may before it mints; a trade an operator's entry maps names that
	// entry's one project or none, and a trade that names no person names none. Absent or blank
	// is no projects.
	FormProjects = "projects"
	// SubjectTokenTypeKubernetes is the subject_token_type a pod presents its projected
	// Kubernetes service-account token under. RFC 8693 registers no name for one, so the broker
	// defines this one and the exchange client is the only client that may present it.
	SubjectTokenTypeKubernetes = "urn:envoy:params:oauth:token-type:kubernetes-service-account"
	// SubjectTokenTypeGitHub is the subject_token_type a continuous-integration workflow run
	// presents its GitHub token under, broker-defined for the same reason as
	// SubjectTokenTypeKubernetes.
	SubjectTokenTypeGitHub = "urn:envoy:params:oauth:token-type:github-workflow-run"
	// SubjectTokenTypeGoogle is the subject_token_type a machine presents its Google identity
	// token under: the token a Compute Engine VM reads from its metadata server, or one minted
	// for a workload identity pool, which carries the machine's service account in its `email`
	// claim. Broker-defined for the same reason as SubjectTokenTypeKubernetes.
	SubjectTokenTypeGoogle = "urn:envoy:params:oauth:token-type:google-identity"
	// SubjectTokenTypePerson is the subject_token_type a person presents their own Google
	// identity token under: the ID token `agent-secrets login`'s device grant (RFC 8628) got for
	// them, minted for the device-flow client BROKER_SIGNIN_DEVICE_AUDIENCE names. It is the
	// person half of a sign-in on a machine; the machine half is ActorTokenTypeMachine, which
	// the same request carries. Broker-defined for the reason SubjectTokenTypeKubernetes is.
	SubjectTokenTypePerson = "urn:envoy:params:oauth:token-type:person-google-identity"
	// ActorTokenTypeMachine is the actor_token_type a machine presents its own Google identity
	// token under while the person signed in on it trades theirs (RFC 8693 Section 2.1). It is
	// the very token SubjectTokenTypeGoogle names, under a type of its own because what it
	// proves here is different: not who is asking, but which box the person is asking from.
	// Broker-defined for the reason SubjectTokenTypeKubernetes is.
	ActorTokenTypeMachine = "urn:envoy:params:oauth:token-type:machine-google-identity"
	// SubjectTokenTypeAccessToken is RFC 8693's own name for an access token: the only kind the
	// exchange mints, the kind it asks for, and the kind a dev box session presents its machine
	// login's token as. The library spells it in a package its users cannot import.
	SubjectTokenTypeAccessToken = tokenTypeURN + "access_token" //nolint:gosec // a token type's name, not a token
	// PodSessionNotRegistered, then a colon, begins the error_description of the exchange's
	// invalid_grant for a pod whose session no registration names yet (internal/cipher/oauth,
	// registeredPodSession). A pod's trade can reach the broker before its registration does, so
	// it is a refusal of the broker's state that a later trade cures: the Go client's
	// RefusedError.Permanent and the TypeScript client's BrokerRefusedError.permanent read it
	// off these words, and both clients trade again on it for as long as their caller runs.
	PodSessionNotRegistered = "pod_session_not_registered"
	// SessionNameNotThisRegistrars, then a colon, begins the error_description of the exchange's
	// final invalid_grant for a pod a daemon other than the one its session name is bound to
	// registered, or that a daemon registered under a name bound to none (internal/cipher/oauth,
	// registeredPodSession). A name belongs, for good, to the daemon that first registered it, and
	// only a registration, never a later trade, binds a name bound to none, so no later trade cures
	// the refusal. Both clients read it as final: of the exchange's invalid_grant refusals, they
	// trade again only on one whose description begins PodSessionNotRegistered.
	SessionNameNotThisRegistrars = "session_name_not_this_registrars"
	// MembershipHeld is the token endpoint's refusal of a credential whose person Dispatch holds
	// until they sign in to Dispatch again, or a refresh confirms them. It is not permanent.
	MembershipHeld = "membership_held"
)

// The wire names of a token's project scope, which an introspection answer's ProjectScope carries
// and the exchange writes onto the session it stores (internal/cipher/oauth, ClaimProjectScope).
// They are short and fixed so a reader of either reads one word rather than the prose
// exchange.ProjectScope.String prints for an operator.
const (
	// ProjectScopeNamed is a token an operator's entry mapped: it acts for the entry's person, or
	// as the account itself for an account entry naming no person, and its Projects holds the
	// entry's one project, or none for an account entry naming none (the shared public-session
	// account's, and every account entry naming no person). It never holds two.
	ProjectScopeNamed = "named"
	// ProjectScopePerson is a token that acts as its person: its Projects holds the 0 to
	// MaxProjects projects the trade asked for, each one its person could see at the trade.
	ProjectScopePerson = "person"
	// ProjectScopeNone is a token that may write no project and names none: the token a read
	// entry mints, which holds the scope ScopeRead and nothing else. A resource server of a
	// build that predates it refuses it, since it accepts only the two words above, and one of
	// this build accepts it only where it asks to (ciphertoken).
	ProjectScopeNone = "none"
)

// AccountSubjectPrefix opens a Kubernetes service account's subject,
// system:serviceaccount:<namespace>:<name>. It tells a Kubernetes subject from a Google account
// address where a reader holds one string that may be either (exchange.Set.ProjectsForAccount);
// whether a string is a whole subject is AccountSubject's to answer.
const AccountSubjectPrefix = "system:serviceaccount:"

// AccountSubject is a Kubernetes service account's whole subject, the one a pod's projected token
// carries: system:serviceaccount:<namespace>:<name>, the namespace a DNS label and the name a DNS
// subdomain. It is the one spelling of that grammar: the broker's configuration holds every
// account its settings name to it (config), and the exchange every account entry and read entry
// (exchange.NewSet), so it is also the service a ProjectScopeNamed token that names no person
// carries: the one entry that mints that shape is an account entry naming no person, whose token
// acts as the account (exchange.Set.Pod). A resource server reads a named token that names no
// person and a service this does not match whole as no token the broker mints (ciphertoken).
var AccountSubject = regexp.MustCompile(`^` + AccountSubjectPrefix + `[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)

// MaxProjects is the most projects one token names.
const MaxProjects = 16

// projectKey is a whole project key, the one rule Dispatch creates keys by.
var projectKey = regexp.MustCompile(contracts.ProjectKeyPattern)

// ParseProjects reads a list of projects as the exchange's form parameter, the stored claim and
// AGENT_SECRETS_PROJECTS all spell it: project keys separated by white space. It answers the keys
// sorted, each once, and nil for a blank list. A key that is not a project key, a key named twice
// and a list of more than MaxProjects are refused, naming the rule, so a list that is not one is
// never read as some part of it.
func ParseProjects(raw string) ([]string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil, nil
	}
	if len(fields) > MaxProjects {
		return nil, fmt.Errorf("%d projects are named, and a token names at most %d", len(fields), MaxProjects)
	}
	for _, key := range fields {
		if !projectKey.MatchString(key) {
			return nil, fmt.Errorf("%q is not a project key: %s", key, contracts.ProjectKeyRule)
		}
	}
	slices.Sort(fields)
	for i := 1; i < len(fields); i++ {
		if fields[i] == fields[i-1] {
			return nil, fmt.Errorf("%s is named twice, and a list names each project once", fields[i])
		}
	}
	return fields, nil
}

// ValidProjects holds a list to exactly what ParseProjects answers: at most MaxProjects project
// keys, sorted, each once. It is how a reader of a list it did not parse itself — an
// introspection answer — refuses one no exchange could have minted.
func ValidProjects(projects []string) error {
	if len(projects) > MaxProjects {
		return fmt.Errorf("%d projects are named, and a token names at most %d", len(projects), MaxProjects)
	}
	for i, key := range projects {
		if !projectKey.MatchString(key) {
			return fmt.Errorf("%q is not a project key: %s", key, contracts.ProjectKeyRule)
		}
		if i > 0 && key <= projects[i-1] {
			return fmt.Errorf("the projects %q and %q are not sorted and named once each", projects[i-1], key)
		}
	}
	return nil
}

// JoinProjects is a list in its stored and wire spelling: the keys separated by one space, and
// the empty string for none. ParseProjects reads it back to the same list.
func JoinProjects(projects []string) string { return strings.Join(projects, " ") }

// Introspection is what the broker knows about one of its access tokens: the RFC 7662 answer the
// broker's introspection route serialises and a resource server reads a presented token back
// through (internal/ciphertoken). The broker's tokens are opaque, so everything here comes from
// the row the token is stored in rather than from the token's own bytes, and a token the broker
// has revoked or let expire is simply not live.
//
// The JSON names are RFC 7662's where it has one and the broker's own where it does not; the
// route serialises this struct and adds no field of its own. Both ends read this one declaration,
// so a field added here reaches each of them and the compiler holds them together.
type Introspection struct {
	// Active is whether the token is live: one of the broker's access tokens, not revoked, not
	// expired. Everything else is set only when it is.
	Active bool `json:"active"`
	// Session is the session the token's writes are recorded as.
	Session string `json:"session,omitempty"`
	// Person is the person the session acts for.
	Person string `json:"person,omitempty"`
	// Service is the service a token that names no person was minted for: the fleet its machine
	// belongs to, as BROKER_VM_SERVICES names it, the service a read entry names
	// (BROKER_EXCHANGE_READ_ACCOUNTS), or the account of a BROKER_EXCHANGE_ACCOUNTS entry naming
	// no person (system:serviceaccount:<namespace>:<name>), which that token acts as. A token
	// names a person or a service and never both, so a reader that records who is answerable for
	// a write reads Person, or this where Person is empty, and one that decides what a machine may
	// reach reads this.
	Service string `json:"service,omitempty"`
	// Machine is the machine a person signed in on acted through: the Google account of the
	// actor token the exchange was made with, as BROKER_VM_SERVICES lists it. It is set only
	// beside Person, and only for a token a person's sign-in on a machine minted; every other
	// token names no machine. The token's own RFC 8693 'act' claim records the same delegation
	// as the actor token's issuer spells it, which is a Google subject rather than an account,
	// so this is the name a reader can act on.
	Machine string `json:"machine,omitempty"`
	// ProjectScope is what Projects means: ProjectScopeNamed, ProjectScopePerson or
	// ProjectScopeNone. A machine login's own token, which no exchange decided for, carries
	// neither this nor Projects.
	ProjectScope string `json:"project_scope,omitempty"`
	// Projects is the token's list of projects: for a ProjectScopeNamed token, the entry's one
	// project, or none for an account entry naming none; for a ProjectScopePerson token, the 0 to
	// MaxProjects projects its trade asked for, each one its person could see then; for a
	// ProjectScopeNone token, none. Sorted, each once; absent for none.
	Projects []string `json:"projects,omitempty"`
	// Scope is the OAuth scope the token was granted, space-separated as RFC 7662 spells it. An
	// exchanged token holds ScopeRead when a read entry minted it and no scope otherwise, and a
	// machine login's own token holds Scope.
	Scope string `json:"scope,omitempty"`
	// ExpiresAt is when the token expires, in seconds since the epoch (RFC 7662's 'exp'), so a
	// caller that remembers an affirmative answer can keep it no longer than the token lives.
	ExpiresAt int64 `json:"exp,omitempty"`
	// ClientID is the OAuth client the token was issued to.
	ClientID string `json:"client_id,omitempty"`
	// Thumbprint is the JWK thumbprint of the key the token is DPoP-bound to (RFC 9449's 'jkt').
	// It is what makes a stolen token useless: whoever reads this answer checks the caller's
	// proof against it.
	Thumbprint string `json:"jkt,omitempty"`
}
