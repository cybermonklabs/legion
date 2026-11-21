// Package natscallout is the NATS auth service: the hub sends it the authorization of every
// connection its configuration lists no static user for (nats-server's auth callout, config mode),
// and it answers with a NATS user of that connection's own, or refuses it.
//
// A connection presents one of two credentials. A session sends its broker access token in the
// CONNECT's token field and, in its signature field, a proof of the key the token is bound to over
// the server's own per-connection nonce (ciphertoken.VerifyConnectionProof); it is answered with
// its own delivery channel and nothing of another session's (natsgrant.Session). A service sends
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
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
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
	// Exchange is the person and the projects the broker's own exchange list gives account, and
	// false for an account no entry lists (exchange.Set.MappedForAccount). Nil, or a nil Access,
	// holds no service to its projects, and refuses every entry that is not an all-projects one.
	Exchange func(account string) (person string, projects []string, ok bool)
	// Access is Dispatch's answer about one project (projectaccess). A decision never calls it:
	// the service asks it in the background and decides on the pass it holds (heldAccess).
	Access func(ctx context.Context, project, person string) (projectaccess.Answer, error)
	// HubAdmissionToken verifies a projected Kubernetes ServiceAccount token for exactly
	// natsgrant.HubAdmissionAudience, Dispatch's hub-admission proof (PlatformTokens'
	// VerifyHubAdmission), and answers the account it names and when it was issued and expires. A
	// token naming that audience goes to it and never to ServiceToken (hubAdmissionToken). Nil
	// verifies none, so every proof is refused as a token not verified.
	HubAdmissionToken func(ctx context.Context, raw string) (account string, issued, expiry time.Time, err error)
	// PrivateProjects is the broker's private union as it stands now (PrivateProjects): the
	// projects BROKER_PRIVATE_PROJECTS declares, with every project Dispatch's privacy read has
	// answered private. A decision reads it only for a request from a hub not rendered private
	// (taggedPrivate) whose credential names a project, or reaches every project as Dispatch's
	// own does. Nil is an empty union, which refuses nothing, so a broker that wires none decides
	// every credential as it did before private projects existed. Whoever wires it owns how old an
	// answer it may hold: the decision takes what it answers.
	PrivateProjects func(ctx context.Context) (PrivateProjects, error)
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

	// held is the pass of Dispatch's answers a held entry's grant is built from, which Start makes
	// and a decision reads (scopeGrant); nil until Start, when no held entry is admitted.
	held *heldAccess
}

// PrivateProjects is the broker's private union, in its two parts, each a list of project keys.
// A credential naming a project of either is admitted only on a hub rendered private; Dispatch's
// own all-projects entry is refused on any other hub once Switched holds a project, and not for a
// project that is only declared, since declaring a project switches nothing yet.
type PrivateProjects struct {
	// Declared are the projects BROKER_PRIVATE_PROJECTS names.
	Declared []string
	// Switched are the projects Dispatch's privacy read has answered private.
	Switched []string
}

// holds reports whether project is in either part of the union.
func (p PrivateProjects) holds(project string) bool {
	return slices.Contains(p.Declared, project) || slices.Contains(p.Switched, project)
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
	// ServerTags are the tags of the server that sent the request (its server_tags), which
	// nats-server copies into every authorization request (claims.Server.Tags); taggedPrivate
	// reads them.
	ServerTags []string
	// ClientName is the name the client gave its connection (claims.ClientInformation.Name), which
	// a listener in required privacy mode ends with natsgrant.PrivacyRequiredSuffix.
	ClientName string
}

// taggedPrivate reports whether req came from a hub rendered with private-project support: its
// tags hold natsgrant.PrivateProjectsServerTag. Only such a hub's render can carry the tag, so the
// tag in a request is the hub's own word that it was rendered private.
func taggedPrivate(req Request) bool {
	return slices.Contains(req.ServerTags, natsgrant.PrivateProjectsServerTag)
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
	labelTokenNotLive      = "token_not_live"
	labelKeyMismatch       = "key_mismatch"
	labelNotASession       = "not_a_session"
	labelProofInvalid      = "proof_invalid"
	labelNoExpiry          = "no_expiry"
	labelBadSessionID      = "bad_session_id"
	labelServiceToken      = "service_token_not_verified"
	labelTokenTooLong      = "token_too_long"
	labelServiceNotListed  = "service_not_listed"
	labelHeldToProjects    = "held_to_projects"
	labelNoExchangeEntry   = "no_exchange_entry"
	labelProjectNotFound   = "project_not_found"
	labelProjectNotVisible = "project_not_visible"
	labelAccessRefused     = "project_access_refused"
	labelNoProjects        = "no_exchange_projects"
	labelGrantNotHoldable  = "grant_not_holdable"
	labelNoCompletePass    = "no_complete_pass"
	labelSystemSubject     = "system_subject"
	labelOpenDirection     = "open_direction"
	labelBudget            = "budget"
	labelServiceFailed     = "service_failed"
	labelUnsealed          = "unsealed"
	labelWaitingBound      = "waiting_bound"
	// The private-hub admission's refusals: Dispatch's hub-admission proof on a hub not rendered
	// private, or for another account; a listener not in required privacy mode on a hub rendered
	// private; a credential naming a private project on a hub not rendered private; and the
	// broker's private union not read.
	labelHubAdmissionUntagged    = "hub_admission_untagged"
	labelHubAdmissionNotDispatch = "hub_admission_not_dispatch"
	labelListenerNotRequired     = "listener_not_required"
	labelPrivateUntaggedHub      = "private_project_untagged_hub"
	labelPrivateUnknown          = "private_projects_unknown"
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

// serviceGrant is entry's kind's grant, before it is held to the entry's projects: on a hub
// rendered private (tagged), Dispatch's own account takes natsgrant.DispatchApp, which binds no
// listener durable and no role lane, and everywhere else every kind takes the grant its hub's
// static user of that kind holds; Dispatch's listener takes natsgrant.DispatchListener on every
// hub.
func serviceGrant(entry ServiceEntry, tagged bool) natsgrant.Permissions {
	switch entry.Kind {
	case KindDispatch:
		if tagged {
			return natsgrant.DispatchApp(entry.MachineID)
		}
		return natsgrant.Dispatch(entry.MachineID)
	case KindDispatchListener:
		return natsgrant.DispatchListener(entry.MachineID)
	case KindListener:
		return natsgrant.Listener(entry.MachineID)
	case KindDaemon:
		return natsgrant.Daemon()
	case KindMeasure:
		return natsgrant.Measure()
	}
	return natsgrant.Permissions{}
}

// privateProjectRefusal is the refusal of a credential naming projects, on a request from a hub
// not rendered private, when one of them is in the broker's private union, naming it; nil on a
// hub rendered private, for a credential naming no project, and with no union wired. A union that
// cannot be read refuses the credential, since none of its projects is known to be public; past
// the decision's budget the refusal names the read.
func privateProjectRefusal(ctx context.Context, deps Deps, req Request, projects []string) *Refusal {
	if taggedPrivate(req) || len(projects) == 0 || deps.PrivateProjects == nil {
		return nil
	}
	union, refused := readPrivateProjects(ctx, deps)
	if refused != nil {
		return refused
	}
	for _, project := range projects {
		if union.holds(project) {
			return privateProjectNeedsTag(project)
		}
	}
	return nil
}

// readPrivateProjects reads the broker's private union, refusing the decision when it cannot.
func readPrivateProjects(ctx context.Context, deps Deps) (PrivateProjects, *Refusal) {
	union, err := deps.PrivateProjects(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return PrivateProjects{}, budgetRefusal("the read of the broker's private projects")
		}
		return PrivateProjects{}, refuse(labelPrivateUnknown, "the broker's private projects could not be read").because(err)
	}
	return union, nil
}

// privateProjectNeedsTag is the refusal of a credential reaching the private project project on a
// hub not rendered private.
func privateProjectNeedsTag(project string) *Refusal {
	return refuse(labelPrivateUntaggedHub, "a private project needs a hub rendered private: "+project)
}

// Decide is the user the service answers req with, or the *Refusal it refuses it with. A session
// is answered with natsgrant.Session of the session its live token names, once its proof verifies
// over req's nonce; a service with its kind's grant, held to its projects (scopeGrant), once its
// platform token verifies and lives no longer than a token login may (an hour, beside the proof
// clock skew) and its account is listed in services; Dispatch's hub-admission token with
// natsgrant.HubAdmission (decideHubAdmission). On a hub not rendered private (taggedPrivate), a
// credential naming a project of the broker's private union is refused (privateProjectRefusal);
// on a hub rendered private, a listener not in required privacy mode is. The grant it answers is
// not yet checked: every answer passes the guard in admit (callout.go), whatever decided it.
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

func decideSession(ctx context.Context, deps Deps, req Request) (Decision, error) {
	answer, err := deps.Introspect(ctx, req.Token)
	if err != nil {
		if ctx.Err() != nil {
			return Decision{}, budgetRefusal("the token's introspection")
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
	if refused := privateProjectRefusal(ctx, deps, req, caller.Projects); refused != nil {
		return Decision{}, refused
	}
	grant, err := natsgrant.Session(caller.Session)
	if err != nil {
		return Decision{}, refuse(labelBadSessionID, "session id is not one subject token")
	}
	return Decision{Name: sessionNamePrefix + caller.Session, Grant: grant, Expires: time.Unix(answer.ExpiresAt, 0)}, nil
}

// longestServiceToken is the longest life a service's platform token may have from its issue: a
// token login's (machine.MaxTokenLoginLifetime), beside the proof clock skew, the bound the
// broker's API already holds its listed callers to.
const longestServiceToken = machine.MaxTokenLoginLifetime + ciphertoken.ProofClockSkew

// verifiedFor is the refusal of a verified platform token's life, or nil when it lives no longer
// than longestServiceToken from its issue and has not expired.
func verifiedFor(deps Deps, issued, expiry time.Time) *Refusal {
	if expiry.Sub(issued) > longestServiceToken {
		return refuse(labelTokenTooLong, "token lives longer than an hour")
	}
	if !expiry.After(deps.now()) {
		return refuse(labelTokenNotLive, "token not live")
	}
	return nil
}

// listedService is the entry of services for account, nil for an account no entry lists.
func listedService(services []ServiceEntry, account string, google bool) *ServiceEntry {
	for index := range services {
		if services[index].Account == account && services[index].Google == google {
			return &services[index]
		}
	}
	return nil
}

func decideService(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
	// The token's audience, read before it is verified, picks the verifier: a proof token is
	// verified for the hub-admission audience alone and decided as a proof, never as a service's
	// connection, though both are the cluster issuer's tokens.
	if hubAdmissionToken(req.Password) {
		return decideHubAdmission(ctx, deps, services, req)
	}
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
	if refused := verifiedFor(deps, issued, expiry); refused != nil {
		return Decision{}, refused
	}
	entry := listedService(services, account, google)
	if entry == nil {
		return Decision{}, refuse(labelServiceNotListed, "service not listed: "+account)
	}
	tagged := taggedPrivate(req)
	switch {
	case tagged && (entry.Kind == KindListener || entry.Kind == KindDispatchListener) &&
		!strings.HasSuffix(req.ClientName, natsgrant.PrivacyRequiredSuffix):
		// A listener build that predates required privacy mode, or one rolled back to its
		// all-public render, names its connections without the suffix, and would relay a private
		// project's messages without reading its privacy; on a hub rendered private it is refused.
		return Decision{}, refuse(labelListenerNotRequired, "a hub rendered private admits only a listener in required privacy mode: "+account)
	case !tagged && entry.Kind == KindDispatch && entry.AllProjects && deps.PrivateProjects != nil:
		// Dispatch's own account reaches every project, so on a hub not rendered private it is
		// refused once any project has been switched private, and a project only declared private
		// refuses nothing yet.
		union, refused := readPrivateProjects(ctx, deps)
		if refused != nil {
			return Decision{}, refused
		}
		if switched := slices.Sorted(slices.Values(union.Switched)); len(switched) > 0 {
			return Decision{}, privateProjectNeedsTag(switched[0])
		}
	}
	grant, err := scopeGrant(ctx, deps, *entry, serviceGrant(*entry, tagged), req)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Name: entry.name(), Grant: grant, Expires: expiry}, nil
}

// hubAdmissionNamePrefix opens the name of the user Dispatch's hub-admission proof is answered
// with, which the server reports as the connection's authorized user (/connz).
const hubAdmissionNamePrefix = "hub-admission/"

// decideHubAdmission decides Dispatch's hub-admission proof, a service token whose audience is
// natsgrant.HubAdmissionAudience: once the hub-admission verifier verifies it for that audience
// and it lives as a service's token may, it is admitted with natsgrant.HubAdmission only for
// Dispatch's own account (its dispatch entry of services) and only on a request carrying
// natsgrant.PrivateProjectsServerTag. So the proof succeeds only through a hub rendered private,
// and the connection it makes holds nothing but its own round trip.
func decideHubAdmission(ctx context.Context, deps Deps, services []ServiceEntry, req Request) (Decision, error) {
	if deps.HubAdmissionToken == nil {
		return Decision{}, refuse(labelServiceToken, "service token not verified").because(errors.New("natscallout: this broker verifies no hub-admission token"))
	}
	account, issued, expiry, err := deps.HubAdmissionToken(ctx, req.Password)
	if err != nil {
		if ctx.Err() != nil {
			return Decision{}, budgetRefusal("the hub-admission token's verification")
		}
		return Decision{}, refuse(labelServiceToken, "service token not verified").because(err)
	}
	if refused := verifiedFor(deps, issued, expiry); refused != nil {
		return Decision{}, refused
	}
	entry := listedService(services, account, false)
	switch {
	case entry == nil:
		return Decision{}, refuse(labelServiceNotListed, "service not listed: "+account)
	case entry.Kind != KindDispatch:
		return Decision{}, refuse(labelHubAdmissionNotDispatch, "hub admission is Dispatch's own account's alone: "+account)
	case !taggedPrivate(req):
		return Decision{}, refuse(labelHubAdmissionUntagged, "hub admission needs a hub rendered private: "+account)
	}
	return Decision{Name: hubAdmissionNamePrefix + entry.MachineID, Grant: natsgrant.HubAdmission(account), Expires: expiry}, nil
}
