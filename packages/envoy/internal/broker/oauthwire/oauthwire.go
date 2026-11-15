// Package oauthwire is the wire contract of the broker's OAuth endpoints that the broker and the
// callers it answers share: the clients they present, the scope a machine login asks for, the
// names the token exchange knows a subject token by, and the fields the broker adds to a token
// answer. It imports nothing, so the agent-secrets helper reads it without the broker's storage.
package oauthwire

const (
	// ClientID is the OAuth client the agent-secrets helper logs a machine in as, and the client
	// whose access token a dev box session exchanges for a Dispatch one.
	ClientID = "agent-secrets-helper"
	// Scope is the one scope a machine login asks for and is granted.
	Scope = "machine"
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
	// token writes under. RFC 8693 registers none, so the broker defines this one; an entry
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
	// declines (packages/broker-client/src/environment.ts, brokerTokenClient): none of them reads
	// ExtraCredentialID. A TypeScript client's trade of a person's sign-in sends nothing, since a
	// person's trade may not decline.
	FormLauncherCredential = "launcher_credential"
	// FormLauncherCredentialDeclined is the one value of FormLauncherCredential that declines.
	FormLauncherCredentialDeclined = "false"
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
	SubjectTokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // a token type's name, not a token
)

// The wire names of a token's project scope, which an introspection answer's ProjectScope carries
// and the exchange writes onto the session it stores (internal/broker/oauth, ClaimProjectScope).
// They are short and fixed so a reader of either reads one word rather than the prose
// exchange.ProjectScope.String prints for an operator.
const (
	// ProjectScopeNamed is a token that may write the one project it names.
	ProjectScopeNamed = "named"
	// ProjectScopePerson is a token that may write every project its person may write, and names
	// no project.
	ProjectScopePerson = "person"
)

// CapabilityCIResults is the one capability a token can carry: a token minted for a
// continuous-integration run of a workflow BROKER_EXCHANGE_RESULT_WORKFLOWS lists, which writes
// that run's test results to its one project and nothing else. A token that carries no
// capability is held to no route by it; a reader that meets a capability it does not know
// refuses the token rather than read it as one with no limit (internal/brokertoken).
const CapabilityCIResults = "ci-results"

// CIRun is the continuous-integration run a CapabilityCIResults token was minted for, read from
// the claims of the GitHub token the run presented to the exchange: which repository and
// workflow ran, on which ref and commit, for which event, which run and which attempt of it, on
// which kind of runner, started by whom, and which check run its job is. A resource server
// records it beside each result the token writes and holds the result's commit to SHA.
type CIRun struct {
	// Repository is <owner>/<name>, the token's `repository` claim.
	Repository string `json:"repository"`
	// WorkflowRef is <owner>/<name>/<path>@<ref>, the workflow the run started in
	// (`workflow_ref`).
	WorkflowRef string `json:"workflow_ref"`
	// Ref is the git ref the run is on (`ref`), such as refs/pull/7/merge.
	Ref string `json:"ref"`
	// SHA is the commit the run checked out (`sha`), 40 lowercase hexadecimal digits.
	SHA string `json:"sha"`
	// EventName is the event that started the run (`event_name`), such as push or pull_request.
	EventName string `json:"event_name"`
	// RunID is the run's id (`run_id`), a positive decimal integer: a re-run keeps it.
	RunID string `json:"run_id"`
	// RunAttempt is which attempt of the run this is (`run_attempt`), a positive decimal
	// integer, 1 for the first.
	RunAttempt string `json:"run_attempt"`
	// RunnerEnvironment is github-hosted or self-hosted (`runner_environment`).
	RunnerEnvironment string `json:"runner_environment"`
	// Actor is the GitHub account that started the run (`actor`).
	Actor string `json:"actor"`
	// CheckRunID is the check run of the one job that asked for the token (`check_run_id`), a
	// positive decimal integer, which tells two legs of one matrix apart. It is empty for a
	// token that carries none, as GitHub's older tokens do not.
	CheckRunID string `json:"check_run_id"`
}

// Introspection is what the broker knows about one of its access tokens: the RFC 7662 answer the
// broker's introspection route serialises and a resource server reads a presented token back
// through (internal/brokertoken). The broker's tokens are opaque, so everything here comes from
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
	// belongs to, as BROKER_VM_SERVICES names it. A token names a person or a service and never
	// both, so a reader that records who is answerable for a write reads Person, and one that
	// decides what a machine may reach reads this.
	Service string `json:"service,omitempty"`
	// Machine is the machine a person signed in on acted through: the Google account of the
	// actor token the exchange was made with, as BROKER_VM_SERVICES lists it. It is set only
	// beside Person, and only for a token a person's sign-in on a machine minted; every other
	// token names no machine. The token's own RFC 8693 'act' claim records the same delegation
	// as the actor token's issuer spells it, which is a Google subject rather than an account,
	// so this is the name a reader can act on.
	Machine string `json:"machine,omitempty"`
	// ProjectScope is what Project means: ProjectScopeNamed or ProjectScopePerson. A machine
	// login's own token, which no exchange decided for, carries neither this nor Project.
	ProjectScope string `json:"project_scope,omitempty"`
	// Project is the one project key a ProjectScopeNamed token may write.
	Project string `json:"project,omitempty"`
	// Scope is the OAuth scope the token was granted, space-separated as RFC 7662 spells it.
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
	// Capability is the one thing a token may do, when an exchange limited it to one:
	// CapabilityCIResults for a token minted for a run of a workflow
	// BROKER_EXCHANGE_RESULT_WORKFLOWS lists. Every other token carries none.
	Capability string `json:"capability,omitempty"`
	// CI is the continuous-integration run a CapabilityCIResults token was minted for, and is
	// set exactly where Capability is.
	CI *CIRun `json:"ci,omitempty"`
}
