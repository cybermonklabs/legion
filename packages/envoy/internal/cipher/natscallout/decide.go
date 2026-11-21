// Package natscallout is the NATS auth service: the hub sends it the authorization of every
// connection its configuration lists no static user for (nats-server's auth callout, config mode),
// and it answers with a NATS user of that connection's own, or refuses it.
//
// A connection presents one of two credentials. A session sends its broker access token in the
// CONNECT's token field and, in its signature field, a proof of the key the token is bound to over
// the server's own per-connection nonce (ciphertoken.VerifyConnectionProof); it is answered with
// its own delivery channel and nothing of another session's (natsgrant.Session), and refused while
// Dispatch holds the person its token names (decideSession). A service sends
// user "service" (natscred.ServiceUser) and its platform identity token as the password: a
// projected Kubernetes ServiceAccount token or a VM's Google identity token, which the broker
// already verifies for its own callers; it is answered with its kind's grant, for the account
// BROKER_NATS_SERVICES lists it under (ParseServices). Every other connection is refused.
//
// The service runs inside the secrets broker (cmd/cipherd), whose replicas each subscribe the
// authorization subject in one queue group, so a stopped replica delays no connection.
package natscallout

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// Deps is what the service asks of the broker it runs in.
type Deps struct {
	// Introspect is what the broker knows of one of its access tokens: the provider's own
	// introspection, in the broker's process.
	Introspect func(ctx context.Context, token string) (oauthwire.Introspection, error)
	// ServiceToken verifies a service's platform identity token for the NATS audiences
	// (BROKER_NATS_K8S_AUDIENCE, BROKER_NATS_GOOGLE_AUDIENCE) and answers the account it names,
	// whether that is a Google account, and when the token was issued and expires.
	ServiceToken func(ctx context.Context, raw string) (account string, google bool, issued, expiry time.Time, err error)
	// Now is time.Now unless a test replaces it.
	Now func() time.Time
	// Log is where the service's reporter writes the refusals, failed answers and drops it
	// counted: a line for each kind at most once a minute and one more as the replica closes, each
	// key with its first event's detail, which for a refusal names the connection's host, and never
	// a credential (reportWhileRunning). Nil logs nothing.
	Log *slog.Logger
	// Metrics is the registry the service's decisions are counted on (NewMetrics); nil counts
	// nothing.
	Metrics *Metrics
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// Request is what the server asks about one connection, read from its authorization request.
type Request struct {
	// UserNkey is the user key the server made for this connection, which the answer's user
	// must name; ServerID is the server, which the answer's audience must name.
	UserNkey, ServerID string
	// Nonce is the nonce the server sent this connection, which a session's proof signs.
	Nonce string
	// Token and Sig are the CONNECT's token and signature fields: a session's access token
	// and its proof, which nats.go sends base64url-encoded.
	Token, Sig string
	// User and Password are the CONNECT's user and password: natscred.ServiceUser and a
	// service's platform identity token.
	User, Password string
	// Host is the client's address, for the log.
	Host string
}

// Decision is the user the service answers a connection with.
type Decision struct {
	// Name is the user's name, "session/<id>" or "<kind>/<machine id or account>", which the
	// server reports as the connection's authorized user (/connz).
	Name string
	// Grant is what the user may publish and subscribe.
	Grant natsgrant.Permissions
	// Expires is when the user expires: when the credential it was issued for does.
	Expires time.Time
}

// The credential kinds a request is decided as, the kind label of the decision counter.
const (
	credentialSession = "session"
	credentialService = "service"
	credentialNone    = "none"
)

// Refusal is a connection the service refuses. Reason is the one line the server is answered with,
// and names nothing of the credential. Label is the decision counter's reason label, one fixed
// word per kind of refusal, so a reworded Reason changes no series. Cause, when a step on the
// broker's side failed, is why: it is never answered, and the reporter's line carries it in the
// label's first detail (answer). The credential the connection presented is the request's own
// (credentialKind), which the answer reads where it counts.
type Refusal struct {
	Label, Reason string
	Cause         error
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(label, reason string) *Refusal {
	return &Refusal{Label: label, Reason: reason}
}

// because records cause as r's Cause, for the reporter's line.
func (r *Refusal) because(cause error) *Refusal {
	r.Cause = cause
	return r
}

// The reason labels of the decision counter, one per kind of refusal.
const (
	labelNoCredential      = "no_credential"
	labelBrokerUnreachable = "broker_unreachable"
	labelHoldUncheckable   = "hold_uncheckable"
	labelTokenNotLive      = "token_not_live"
	labelMembershipHeld    = "membership_held"
	labelDeadlinePassed    = "deadline_passed"
	labelKeyMismatch       = "key_mismatch"
	labelNotASession       = "not_a_session"
	labelProofInvalid      = "proof_invalid"
	labelNoExpiry          = "no_expiry"
	labelBadSessionID      = "bad_session_id"
	labelServiceToken      = "service_token_not_verified"
	labelTokenTooLong      = "token_too_long"
	labelServiceNotListed  = "service_not_listed"
	labelHeldToProjects    = "held_to_projects"
	labelSystemSubject     = "system_subject"
	labelOpenDirection     = "open_direction"
	labelBudget            = "budget"
	labelServiceFailed     = "service_failed"
	labelUnsealed          = "unsealed"
	labelWaitingBound      = "waiting_bound"
)

// credentialKind is which credential req presents: a session's token beside a proof and no user,
// a service's user and password, or neither.
func credentialKind(req Request) string {
	switch {
	case req.Token != "" && req.Sig != "" && req.User == "":
		return credentialSession
	case req.User == natscred.ServiceUser && req.Password != "":
		return credentialService
	}
	return credentialNone
}

// serviceGrant is entry's kind's grant, before it is held to the entry's projects.
func serviceGrant(entry ServiceEntry) natsgrant.Permissions {
	switch entry.Kind {
	case KindDispatch:
		return natsgrant.Dispatch(entry.MachineID)
	case KindListener:
		return natsgrant.Listener(entry.MachineID)
	case KindDaemon:
		return natsgrant.Daemon()
	case KindMeasure:
		return natsgrant.Measure()
	}
	return natsgrant.Permissions{}
}

// Decide is the user the service answers req with, or the *Refusal it refuses it with. A session
// is answered with natsgrant.Session of the session its live token names, once its proof verifies
// over req's nonce and while Dispatch does not hold the person its token names (decideSession); a
// service with its kind's grant, held to its projects (scopeGrant), once its platform token
// verifies and lives no longer than a token login may (an hour, beside the proof clock skew) and
// its account is listed in services. The grant it answers is not yet checked: every answer passes
// the guard in admit (callout.go), whatever decided it.
func Decide(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
	switch credentialKind(req) {
	case credentialSession:
		return decideSession(ctx, deps, req)
	case credentialService:
		return decideService(ctx, deps, services, req)
	}
	return Decision{}, refuse(labelNoCredential, "no credential this hub admits")
}

// guard refuses a grant the service must never answer with, whatever produced it; admit
// (callout.go) runs it on every answer before any user is encoded.
//
// A grant naming a system subject (natsgrant.NamesSystemSubject): the server merges a deny of the
// publish of $SYS.REQ.USER.AUTH into every client of the callout account (nats-server
// server/auth.go, mergeDenyPermissions), and that deny is publish only, so such a grant would give
// a connection a subscribe to the authorization requests that nothing takes away. And a grant that
// opens a direction (natsgrant.OpenDirection): an empty allow list is every subject in that
// direction, not none, whatever deny list stands beside it.
func guard(grant natsgrant.Permissions) *Refusal {
	if entry, named := natsgrant.NamesSystemSubject(grant); named {
		return refuse(labelSystemSubject, "grant names a system subject: "+entry)
	}
	if direction, open := natsgrant.OpenDirection(grant); open {
		return refuse(labelOpenDirection, openDirectionReason(direction))
	}
	return nil
}

// openDirectionReason is the refusal of a grant whose allow list in direction is empty.
func openDirectionReason(direction string) string {
	return "grant opens every subject: its " + direction + " allow list is empty"
}

// sessionNamePrefix opens the name of a session's user.
const sessionNamePrefix = "session/"

// decideSession decides a session's connection from the broker's introspection of its token, the
// provider's own in this process, which asks the hold gate (hold.Gate.CheckUntil) about the
// person the token names. After the credential's own checks (live, bound to the key the proof is
// signed with, a session's shape, an expiry, a session id that is one subject token), a person
// Dispatch holds is refused, as every broker route refuses them; a not-held answer whose deadline
// (not_held_until) has passed by this broker's clock is refused, and the user of one admitted
// before it expires at it if that comes before the token's expiry, so the connection does not
// outlive it, as the airlock cuts a call it carries then. Dispatch's unavailable answer, whose
// deadline is its own as_of, never reaches here as a not-held answer: the gate reads it from
// those two times and introspection answers it as a gate that cannot tell, which is refused under
// its own label, apart from a broker that did not answer.
func decideSession(ctx context.Context, deps Deps, req Request) (Decision, error) {
	answer, err := deps.Introspect(ctx, req.Token)
	if err != nil {
		if ctx.Err() != nil {
			return Decision{}, budgetRefusal("the token's introspection")
		}
		var uncheckable *oauth.HoldUncheckableError
		if errors.As(err, &uncheckable) {
			return Decision{}, refuse(labelHoldUncheckable, "hold unchecked: whether Dispatch holds the token's person cannot be read").because(err)
		}
		return Decision{}, refuse(labelBrokerUnreachable, "broker unreachable: the token's introspection failed").because(err)
	}
	caller, err := ciphertoken.SessionCaller(answer)
	switch {
	case errors.Is(err, ciphertoken.ErrNotActive):
		return Decision{}, refuse(labelTokenNotLive, "token not live")
	case errors.Is(err, ciphertoken.ErrKeyMismatch):
		return Decision{}, refuse(labelKeyMismatch, "key mismatch")
	case err != nil:
		return Decision{}, refuse(labelNotASession, "token is no session's credential")
	}
	proof, err := base64.RawURLEncoding.DecodeString(req.Sig)
	if err != nil {
		return Decision{}, refuse(labelProofInvalid, "proof invalid: jws")
	}
	if err := ciphertoken.VerifyConnectionProof(string(proof), req.Token, req.Nonce, answer.Thumbprint, deps.now()); err != nil {
		var claim *ciphertoken.ProofClaimError
		if errors.As(err, &claim) {
			return Decision{}, refuse(labelProofInvalid, "proof invalid: "+claim.Claim)
		}
		return Decision{}, refuse(labelKeyMismatch, "key mismatch")
	}
	if answer.ExpiresAt <= 0 {
		return Decision{}, refuse(labelNoExpiry, "token names no expiry")
	}
	grant, err := natsgrant.Session(caller.Session)
	if err != nil {
		return Decision{}, refuse(labelBadSessionID, "session id is not one subject token")
	}
	if caller.Held {
		return Decision{}, refuse(labelMembershipHeld, "membership held: Dispatch holds the token's person")
	}
	expires := time.Unix(answer.ExpiresAt, 0)
	if answer.NotHeldUntil > 0 {
		deadline := time.Unix(answer.NotHeldUntil, 0)
		if !deadline.After(deps.now()) {
			return Decision{}, refuse(labelDeadlinePassed, "hold unchecked: Dispatch's answer that the token's person is not held has passed its deadline")
		}
		if deadline.Before(expires) {
			expires = deadline
		}
	}
	return Decision{Name: sessionNamePrefix + caller.Session, Grant: grant, Expires: expires}, nil
}

// longestServiceToken is the longest life a service's platform token may have from its issue: a
// token login's (machine.MaxTokenLoginLifetime), beside the proof clock skew, the bound the
// broker's API already holds its listed callers to.
const longestServiceToken = machine.MaxTokenLoginLifetime + ciphertoken.ProofClockSkew

func decideService(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
	account, google, issued, expiry, err := deps.ServiceToken(ctx, req.Password)
	if err != nil {
		if ctx.Err() != nil {
			return Decision{}, budgetRefusal("the service token's verification")
		}
		// The cause names which: a wrong audience, an expired token, an issuer this broker takes no
		// NATS token from, or a key set it could not fetch. The verifiers' errors carry no token
		// (internal/oidc), so the cause is safe to log; it is never answered.
		return Decision{}, refuse(labelServiceToken, "service token not verified").because(err)
	}
	if expiry.Sub(issued) > longestServiceToken {
		return Decision{}, refuse(labelTokenTooLong, "token lives longer than an hour")
	}
	if !expiry.After(deps.now()) {
		return Decision{}, refuse(labelTokenNotLive, "token not live")
	}
	var entry *ServiceEntry
	for index := range services {
		if services[index].Account == account && services[index].Google == google {
			entry = &services[index]
			break
		}
	}
	if entry == nil {
		return Decision{}, refuse(labelServiceNotListed, "service not listed: "+account)
	}
	grant, err := scopeGrant(*entry, serviceGrant(*entry))
	if err != nil {
		return Decision{}, err
	}
	return Decision{Name: entry.name(), Grant: grant, Expires: expiry}, nil
}
