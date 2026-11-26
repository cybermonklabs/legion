package api

// broker_token.go is the credential that is not a bearer: an access token the secrets broker
// issued, presented under RFC 9449's DPoP scheme. internal/ciphertoken decides it; what is here
// is where that decision is made on a request, how it is carried to every layer that resolves
// the caller, what each refusal says, and what an open connection's heartbeat asks instead.
//
// A DPoP proof is single use by its 'jti' and 'htu' (store.DPoPProofs), which is what the two
// pieces below are for: decideBrokerToken decides a request's presentation once however many
// times its handler resolves the caller, and callerStillResolves asks a connection's broker only
// whether the token is still live, never checking the proof the upgrade already spent.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// decideBrokerToken decides a request's DPoP presentation before its handler runs and carries
// the decision as the caller every layer behind it resolves (resolvedCallerKey), because a DPoP
// proof is single use: a route that resolves its caller more than once (an issue create is
// requireAuthenticated and then requireActor, and a keyed write resolves before its handler)
// would present one proof twice and refuse the second as the replay it would be. The refusal is
// carried too, so a caller is told why it was refused rather than told its sound proof was
// spent.
//
// It answers nothing itself: each route refuses a caller that does not resolve in its own way —
// a JSON error, a closed socket — and reaches this decision through optionalActor as it always
// has. A request presenting no such token passes straight through and is resolved where it
// always was. The one layer that reads the decision before the route resolves its caller is the
// read-only gate inside it (refuseReadOnlyToken), which verifies no broker-issued token itself:
// it answers a read-only token on every route outside the read class, and one that carries a cut
// on every read but whoami. A personal token is the gate's own to resolve, and the gate carries
// that result, its refusal included, as this carries its decision.
func (s *server) decideBrokerToken(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, presented := ciphertoken.Presented(r); !presented {
			handler(w, r)
			return
		}
		actor, human, err := s.brokerTokenActor(r)
		decided := resolvedCaller{actor: actor, human: human, err: err}
		handler(w, r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, decided)))
	}
}

// callerStillResolves answers whether the caller a live socket was admitted as still resolves,
// which is what whileCallerResolves asks on that socket's every heartbeat. For every credential
// but one that is the request's own resolution, unchanged. The document socket asks something
// narrower, the sign-in cookie alone, which is all it is admitted by (signInStillResolves).
//
// The exception is a broker-issued token. The heartbeat's copy of the request still carries the
// proof that opened the connection, and that proof is spent: checking it again would be the
// replay it looks like and would close a sound socket on its first heartbeat. It authenticated
// the upgrade, so what is left to ask is whether the broker still holds the token live, which is
// ciphertoken.Verifier.Live. Nothing is admitted through this, only kept.
func (s *server) callerStillResolves(r *http.Request) error {
	if _, presented := ciphertoken.Presented(r); presented {
		if s.deps.BrokerTokens == nil {
			return errNoBrokerConfigured()
		}
		_, err := s.deps.BrokerTokens.Live(r.Context(), r)
		return err
	}
	_, _, err := s.optionalActor(r)
	return err
}

// errNoBrokerConfigured is what a DPoP presentation is to a Dispatch with no broker: there is no
// such credential kind, and this is what an Authorization header that is not a bearer has always
// been here (auth.BearerToken reads no token from it).
func errNoBrokerConfigured() *apiError {
	return errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
}

// brokerPersonScopes is what a broker token that acts for its person holds, a read-only one
// included: the person scope alone, never approve (brokerTokenActor). Every such credential
// shares this one slice, as personCredential shares model.Scopes, so a request allocates none;
// nothing writes it.
var brokerPersonScopes = []string{model.ScopePerson}

// legionStatusScopes is the scopes of a Legion daemon's own broker token: model.ScopeLegionStatus
// alone. It is a slice of its own, as brokerPersonScopes is, so no request allocates one and
// nothing appends to another credential's scopes. Read it, never write it: every daemon's
// credential shares this one slice.
var legionStatusScopes = []string{model.ScopeLegionStatus}

// brokerTokenActor authenticates a caller presenting an access token the secrets broker issued,
// under the DPoP scheme: the broker is asked whether the token is live and the caller is held to
// the key that token is bound to (internal/ciphertoken). A verified caller is the session the
// token names, acting for the person it names, or, where the broker names none, for the service
// it names: the fleet of the machine holding it, the account an operator's entry lists, or a
// job's identity. It is never retried as another credential. The credential carries the token's
// project scope and its list as the broker answered them (model.Credential.ProjectScope,
// Projects), which is what tells the callers that name a service and no person apart.
//
// A job's token is one of those: the exchange mints it from a job entry, which names an identity
// (a pod's Kubernetes service account, a workflow, or a Google account), that identity's projects
// and no person. Its service is that identity, and its project scope is `named` with the one
// project its entry names or `public` with its entry's private projects, beside which it reaches
// every public project. It writes as its session, for its service and under no person:
// writeNeedsASignedInPerson lets a broker credential naming no person write under those two
// scopes alone (project_scope.go). A `named` one sets Actor.Project (below), so the project check
// judges it as it judges a person's named token for that project; a `public` one names no one
// project, so the project check holds it to none and its writes that belong to the deployment
// follow the rule of a token naming no project. It holds no OAuth scope, so the rows that need
// `person` or `approve` turn it away (gateRouteScopes).
//
// A caller that names a service and no person under the project scope named and names no project
// is an account entry's that names no person and no project (cipher/exchange's Set.Pod): its
// service is the account, system:serviceaccount:<namespace>:<name>, and its writes are recorded
// under that account, as the session the token names acting for that service and for no person
// (model.Actor.Service). writeNeedsASignedInPerson lets it write, since there is somebody to
// record the write under: the account the operator listed. It holds no OAuth scope, so a route
// that needs `person` or `approve` refuses it (gateRouteScopes), and it names no project, so it
// writes wherever Dispatch's own rules let a credential naming none write.
//
// A caller that names a service and no person under the project scope person is a machine nobody
// has signed in on (`agent-secrets login`). It reads as any session does, and
// writeNeedsASignedInPerson refuses its every write at the one chokepoint, so no write is
// recorded for a machine nobody stands behind. It holds no OAuth scope either, so the rows a
// person's own credential reaches turn it away earlier still (gateRouteScopes): there is no
// person for it to act as.
//
// A read-only token writes nothing (ciphertoken.Caller.ReadOnly, which only a verifier built with
// Options.ServesReadOnly answers). It is the read entry's token, which names a service and no
// person and holds the scope read alone, or a token narrowed to reading because its trade asked
// for read, which names the person or the service, and the projects, that the same trade would
// have named without read: an account entry's naming no person among them, narrowed, names its
// account and writes nothing, as every read-only token does. The broker narrows no job's token.
// Its credential is marked read-only (model.Credential.ReadOnly), with the token's cut
// (model.Credential.Cut), and refuseReadOnlyToken answers it 403 READ_ONLY_TOKEN on every route
// outside the read class (readClasses) and on GET /ws/live, ahead of the scope gate, the sign-in
// check, an account entry's write as its account and everything else the route does. While no
// build answers a read at a past time, it answers one that carries a cut 403
// AS_OF_TOKEN_NOT_SERVED on every read but GET /api/v1/whoami. On a read it serves, it is the
// session it names, reading as that session does.
//
// Each refusal is its own code, and none of them repeats the token the caller presented:
//
//   - 401 BROKER_TOKEN_NOT_ACTIVE — the broker answered that the token is not live (unknown,
//     revoked or expired).
//   - 401 BROKER_TOKEN_PROOF_INVALID — the proof did not meet RFC 9449 Section 4.3: a bad
//     signature, a method or target URI that is not this request's, an 'iat' outside the window,
//     a missing or wrong 'ath', or a proof already spent.
//   - 401 BROKER_TOKEN_KEY_MISMATCH — a sound proof signed by a key that is not the token's
//     'jkt', which is what makes a stolen token useless.
//   - 401 BROKER_TOKEN_NOT_A_SESSION — the broker holds the token live, but it is not a
//     session's credential: it names no session, it names neither a person nor a service, it
//     was issued to a client other than the exchange a session's token comes from, or its
//     project scope and its scope are a shape no broker mints (the scope read or a cut beside a
//     project scope that writes, or the project scope none or a narrowed one in any shape but a
//     read-only token's). A machine login's own access token is one of these, and Dispatch
//     records every write under the session its credential proved, so it acts on none of them
//     rather than letting a request body name a session for a credential that names none.
//   - 429 BROKER_TOKEN_RATE_LIMITED — this source address has no introspection left in its
//     bucket; nothing was asked of the broker. It carries the wait in Retry-After as well as in
//     its prose, as the broker's own 429s do.
//   - 503 BROKER_UNAVAILABLE — the broker produced no answer, so the token was never judged. It
//     is a 503 for the reason OIDC_ISSUER_UNAVAILABLE is: a caller told its token was refused
//     would discard a token that may be perfectly live, where a 503 asks it to come back.
func (s *server) brokerTokenActor(r *http.Request) (model.Actor, bool, error) {
	if s.deps.BrokerTokens == nil {
		return model.Actor{}, false, errNoBrokerConfigured()
	}
	caller, err := s.deps.BrokerTokens.Verify(r.Context(), r)
	switch {
	case err == nil:
	case errors.Is(err, ciphertoken.ErrNotActive):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE",
			"the secrets broker does not hold this access token as live")
	case errors.Is(err, ciphertoken.ErrKeyMismatch):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_KEY_MISMATCH",
			"the DPoP proof's key is not the key this access token is bound to")
	case errors.Is(err, ciphertoken.ErrProofInvalid), errors.Is(err, ciphertoken.ErrNotPresented):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_PROOF_INVALID",
			"the DPoP proof did not verify")
	case errors.Is(err, ciphertoken.ErrNotASession):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_A_SESSION",
			"this access token is not a session's credential: the secrets broker names no session for it")
	case errors.Is(err, ciphertoken.ErrRateLimited):
		wait := s.deps.BrokerTokens.RetryAfter()
		refusal := errorf(http.StatusTooManyRequests, "BROKER_TOKEN_RATE_LIMITED",
			"too many access tokens checked from this address; try again in %s", wait)
		// The one rounding every bucket of internal/ratelimit's refusals carries, the broker's
		// own 429s included, so a caller reads one wait the same way whichever refused it.
		refusal.retryAfter = ratelimit.RetryAfterSeconds(wait)
		return model.Actor{}, false, refusal
	default:
		// The class only, as serviceTokenActor logs one: this runs before any authorization, so
		// anything the caller chose would be theirs to write into the operator's log.
		slog.Warn("dispatch: the secrets broker could not be reached to check an access token", "error", err)
		return model.Actor{}, false, errorf(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE",
			"the secrets broker cannot be reached; try again in a minute")
	}
	// A token the broker scoped to its person holds the person scope: that token is the person's
	// own agent acting for them, so it reads, writes and answers their questions as they do. No
	// broker-issued token holds approve, whatever its project scope (brokerPersonScopes): every
	// one is made from a file on the machine that holds it, a machine login or a person's sign-in,
	// so whoever copies that file holds it, and approve settles what only the person settles - an
	// approval, a credential decision or revoke, a personal token. Those need the person's browser
	// session or a personal token the person gave approve. A token scoped to a project gets no
	// person scope, because the deployment mapped that token to a project and not to the person: it
	// proves a session, not that any person stands behind the request. A token that names a
	// service and no person gets none either, whatever scope the broker answered: there is no
	// person for it to act as, and a scope that said otherwise would let it act as the session's
	// own id on the routes that read what belongs to a person. A job's token, named or public, is
	// one of those.
	//
	// A read-only token is read by its base word (ciphertoken.Caller.ProjectScope) as the same
	// token without read would be: one narrowed from a token that acts as its person still acts
	// for that person, so it holds the person scope, and every other holds none. The scope read
	// itself is carried as the credential's ReadOnly mark and never as a scope a route could
	// require, so no recorded act names it.
	//
	// A token scoped to one project whose answer holds legion-status gets that scope alone
	// (legionStatusScopes). The broker grants it to a Legion daemon's own token only. A named
	// token that names no project names a person, as the shared public-session account's token
	// does. legion-status on such a token would pass R26 on the Legion issues of every project,
	// so it gets no scope. Dispatch does not check the token's session: the broker decides who
	// holds the scope. No credential holds it beside person, and no act records it: the act of a
	// proven session (bearerSessionActor) and of a person (actingPerson) record the credential's
	// scopes through actScopes, which leaves legion-status out. oneProject is also the
	// test for the one project the token may write (actor.Project, below), so the two cannot
	// drift apart.
	oneProject := caller.ProjectScope == ciphertoken.ProjectScopeNamed && len(caller.Projects) == 1
	var scopes []string
	switch {
	case caller.Person != "" && caller.ProjectScope == ciphertoken.ProjectScopePerson:
		scopes = brokerPersonScopes
	case caller.LegionStatus && oneProject:
		scopes = legionStatusScopes
	}
	actor := model.Actor{
		Kind: "session",
		ID:   caller.Session,
		// The broker proved this session, so the credential already names the agent behind the
		// request and no request body has to name it, and the actor is Proven. A read-only token
		// is marked as one, with the cut it reads at, which are what refuseReadOnlyToken reads, and
		// the project scope and the list are carried as the broker answered them, the scope as its
		// base word (ciphertoken.Caller.ProjectScope), which is what writeNeedsASignedInPerson
		// reads of a caller naming a service and no person: a job's token and an account entry's
		// token naming no person write, and a machine's nobody signed in on does not. The principal
		// is the one the listener records a registration on this token as, which a successor's
		// reply is held to.
		Credential: &model.Credential{
			Scopes:       scopes,
			Session:      caller.Session,
			ReadOnly:     caller.ReadOnly,
			Cut:          caller.Cut,
			ProjectScope: caller.ProjectScope,
			Projects:     caller.Projects,
			Principal:    caller.Principal(),
		},
		Proven: true,
	}
	// A caller names a person or a service, never both and never neither (ciphertoken.Caller),
	// and the two fields are set from the token alone, never from a body. whoami reports both.
	if caller.Person != "" {
		actor.Owner = &caller.Person
	}
	if caller.Service != "" {
		actor.Service = &caller.Service
	}
	// Only a token an operator's entry mapped to one project names that project as the one it
	// may write, which is today's named-scope rule, and a job's named token, whose entry names one
	// project, names it the same way, so the project check judges the two alike; a named token
	// naming none (the shared public-session account's, and an account entry's naming no person)
	// writes, as a token naming no project does, wherever Dispatch's own rules let it, and
	// ciphertoken refuses a named token naming two, and a named token naming no person, no
	// project and a service that is no account. A token narrowed from a named one names its
	// project too, as the project it reads as, and writes none.
	// A token acting as its person carries the list its trade asked for
	// (ciphertoken.Caller.Projects), and that list limits no write here: a dev box session's
	// person writes wherever they may, and the token of a machine naming a service and no person
	// names no project at all. A job's public token names no one project either, so it writes
	// wherever a token naming no project may: the private projects of its list, which the
	// credential carries (model.Credential.Projects), limit no write here. Nor does a token of the
	// project scope none, the read entry's or one narrowed from it, which names no project and
	// writes none.
	if oneProject {
		project := caller.Projects[0]
		actor.Project = &project
	}
	// The writer the line names: the person, or the service for a token that names none, under
	// the `service:` prefix requestKeyLogin spells a service with, so one field never reads a
	// fleet's name or an account as a person's login.
	writer := caller.Person
	if writer == "" {
		writer = "service:" + caller.Service
	}
	accesslog.Note(r.Context(), accesslog.KindBrokerToken, writer)
	return actor, false, nil
}
