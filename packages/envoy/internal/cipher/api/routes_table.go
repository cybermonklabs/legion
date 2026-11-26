package api

import (
	"maps"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/approval"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/machine"
)

type routeAuth int

const (
	authNone routeAuth = iota
	authLauncher
	authProof
	authRenewProof
	authAccount
	authService
	authEdgeListener
	authSignIn
	authOperator
)

// caller is who server.authenticate proved a request came from. Which field is set follows from
// the route's authentication, and only the adapter for that authentication reads it.
type caller struct {
	launcher   enroll.Credential
	enrollment string
	// account is the caller BROKER_API_ACCOUNTS gives the service whose ServiceAccount token
	// authenticated the request: its name, which a handler that acts logs, and the powers that
	// name carries, which authenticate checks against the route's before the handler runs.
	account Account
	// service is the login the ServiceAccount token that authenticated the request made,
	// machine.Service.VerifyServiceToken's own answer: the service that account logs in as,
	// the account whose token it presented, and the pod that token was bound to, empty for one
	// bound to none. serviceAuth hands the whole of it to the handler and says why all three
	// travel together.
	service machine.ServiceAccount
	// signIn is the broker sign-in session a request to the approval page carries, the zero
	// approval.Session when it carries none that is live.
	signIn approval.Session
	// operator is the account of the NATS writer hold's operator, the operator entry of
	// BROKER_NATS_SERVICES whose platform token authenticated the request.
	operator string
	// edge is the edge-listener entry of BROKER_NATS_SERVICES whose account the identity token
	// that authenticated the request names: the projects it may ask about.
	edge EdgeListener
}

// power is one authority over the broker a caller of an accountAuth route may hold: one bit, so
// a set of them is one value and an Account stays comparable. Every such route declares the one
// it needs and every caller name answers the ones it has, so a caller listed for one power never
// reaches a route that needs another.
type power uint

const (
	// noPower is the empty set, which no caller holds and no route may declare: a row that names
	// no power reaches nothing (may refuses it), and TestEveryAccountRouteDeclaresItsPower fails
	// on one.
	noPower power = 0
	// readsTokensBack is reading one of the broker's own access tokens back (RFC 7662): whether
	// it is live and what it carries. A resource server needs this and nothing else.
	readsTokensBack power = 1 << iota
	// relaysDecisions is reading what a person has to decide and relaying the decisions
	// themselves — approvals, denials, dismissals of an expired request and both revocations by
	// approver — and ending one of the broker's own access tokens (RFC 7009), which is that same
	// authority over a credential someone holds. It is an approval credential over every
	// person's secrets, so Dispatch's server alone holds it.
	relaysDecisions
	// readsGrantLog is reading every audit row of a closed period: the whole grant log, each row
	// naming the session that asked, the person behind it, the service the broker verified, the
	// secrets and the decision. A quarterly access review needs it to answer "who held what, and
	// who said yes" from the broker's own record rather than from anyone's recollection. It
	// grants nothing: it decides no request, releases no secret value and writes no row, so a
	// caller holding it alone can read the broker's history and change none of it.
	readsGrantLog
)

// Account is a caller of the accountAuth routes: the name BROKER_API_ACCOUNTS gives its
// ServiceAccount, and the powers that name carries. Only AccountFor builds one with powers, so a
// wiring site cannot invent a caller's authority, and the zero Account reaches no such route.
type Account struct {
	// Name is what the caller calls as, the identity a handler logs when it acts.
	Name   string
	powers power
}

// may reports whether this caller holds p. noPower is held by nobody, so a route that declares
// none is refused every caller rather than open to all of them.
func (a Account) may(p power) bool { return p != noPower && a.powers&p == p }

// accountPowers is the broker's whole table of caller names: every name BROKER_API_ACCOUNTS may
// give a ServiceAccount, and what that name may do. dispatch is Dispatch's server, which relays a
// person's decisions and reads tokens back; listener is the Envoy listener, which reads a token
// back and relays no decision; access-review is the identity a quarterly access review calls as,
// which reads the grant log and nothing else. The review is a third name rather than another of
// Dispatch's powers because Dispatch's server already holds approval authority over every
// person's secrets: giving the whole audit trail to that same name would widen what one stolen
// token reads, while the review identity decides nothing and so is safe to hand to whoever runs
// the review. A new caller is a deliberate row here, since a name is an authority and not a
// label: a name this table does not know refuses the broker's boot.
var accountPowers = map[string]power{
	"access-review": readsGrantLog,
	"dispatch":      readsTokensBack | relaysDecisions,
	"listener":      readsTokensBack,
}

// AccountFor answers the account a caller name carries, and whether the broker knows the name at
// all. config.Load calls it for each BROKER_API_ACCOUNTS entry, so an unknown name is refused at
// boot rather than silently becoming a caller that reaches nothing.
func AccountFor(name string) (Account, bool) {
	powers, known := accountPowers[name]
	if !known {
		return Account{}, false
	}
	return Account{Name: name, powers: powers}, true
}

// KnownCallers is every caller name the broker knows, sorted, so a refusal can name them all.
func KnownCallers() []string {
	return slices.Sorted(maps.Keys(accountPowers))
}

// routeHandler is a handler together with the authentication its signature needs, and, for a
// route a listed caller's ServiceAccount token authenticates, the power its caller must hold.
// Only the adapters below build one, and each fixes both halves: a handler taking a launcher
// credential can only ever run behind launcher authentication, so no route row can pair a
// handler with the wrong kind of caller.
type routeHandler struct {
	auth routeAuth
	// power is what a caller must hold to reach this route, noPower on every route no
	// ServiceAccount token authenticates.
	power power
	// hold is the route's hold step, which handle runs once authenticate has proved the caller and
	// before the route: it asks the hold gate (Deps.Hold) about the person the credential names,
	// and writes the refusal and reports true for a person it refuses. Nil is a route that asks the
	// hold nothing: one that only ends access or lists what to end, and the lease renewal.
	hold  func(s *server, w http.ResponseWriter, r *http.Request, who caller) bool
	serve func(s *server, w http.ResponseWriter, r *http.Request, who caller)
}

// public is a route anyone may call, with no credential.
func public(h func(*server, http.ResponseWriter, *http.Request)) routeHandler {
	return routeHandler{auth: authNone, serve: func(s *server, w http.ResponseWriter, r *http.Request, _ caller) { h(s, w, r) }}
}

// launcherAuth is a route for a machine's launcher (agent-secrets-helper, or the Legion daemon),
// which signs each call's Proof header with the key its machine login's credential is bound to.
// Once the proof and the credential check, its hold step asks the hold gate (Deps.Hold) about the
// person the credential names as its operator (refuseHeldLauncher): one Dispatch holds is refused
// 403 MEMBERSHIP_HELD, and every person is 503 HOLD_UNCHECKABLE while the gate cannot decide. A
// service's login names no operator and is asked nothing.
func launcherAuth(h func(*server, http.ResponseWriter, *http.Request, enroll.Credential)) routeHandler {
	return routeHandler{auth: authLauncher, hold: (*server).refuseHeldLauncher, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.launcher)
	}}
}

// launcherEndAuth is launcherAuth without the hold step, for the routes that only end access or
// list what to end: a person Dispatch holds may call them, because they only end or list access.
func launcherEndAuth(h func(*server, http.ResponseWriter, *http.Request, enroll.Credential)) routeHandler {
	return routeHandler{auth: authLauncher, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.launcher)
	}}
}

// sessionAuth is a route for an enrolled agent session, which signs each call's Proof header with
// its own key, and which acts only on its own enrollment, requests and grants. Once the proof
// checks, its hold step asks the hold gate (Deps.Hold) about the person the session's enrollment
// names as its operator (refuseHeldProof): one Dispatch holds is refused 403 MEMBERSHIP_HELD, and
// every person is 503 HOLD_UNCHECKABLE while the gate cannot decide. A pod's enrollment names no
// operator and is asked nothing.
func sessionAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authProof, hold: (*server).refuseHeldProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.enrollment)
	}}
}

// sessionEndAuth is sessionAuth without the hold step, for the routes that only end access: a
// session whose person Dispatch holds may call them, because they only end access.
func sessionEndAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.enrollment)
	}}
}

// renewAuth is session authentication for the one route that gives an ended enrollment a distinct
// refusal. A skewed/malformed proof remains PROOF_INVALID; only proof.ErrNotLive is
// ENROLLMENT_NOT_LIVE so the pod renewer can stop rather than retry an enrollment that cannot
// recover. It asks no hold: a session renews its lease while Dispatch holds its person, so a hold
// ends no session.
func renewAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authRenewProof, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.enrollment)
	}}
}

// accountAuth is a route for a service calling the broker with its own Kubernetes ServiceAccount
// token: Dispatch's server, which lists what waits on a person, relays a person's machine-login
// decisions, dismissals and revokes, and links the broker's own page for a secret request; and
// whatever resource server reads a token back through introspection. The token is verified
// against the broker's Kubernetes issuer and its account must be one BROKER_API_ACCOUNTS lists,
// and the handler is given that entry's name — the caller's identity, which it logs when it acts.
//
// Each such route declares the power its caller must hold, and the caller's name answers the
// powers it has (accountPowers), which authenticate checks before the handler runs: being listed
// reaches the routes of that caller's own powers and no others.
func accountAuth(needs power, h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authAccount, power: needs, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.account.Name)
	}}
}

// serviceAuth is a route for a service calling the broker with its own Kubernetes ServiceAccount
// token, the account BROKER_SERVICE_LOGINS lets log in as that service — a deployment's Legion
// daemon, which registers the pods it starts and releases the session names it registered. The
// token is verified and mapped to its service by the check a token login makes
// (machine.Service.VerifyServiceToken). The handler is given that check's own answer, one
// machine.ServiceAccount: the service the caller logs in as, the account whose token it
// presented, and the pod that token was bound to. The account and the pod come with it because
// BROKER_SERVICE_LOGINS may name an account BROKER_SERVICES binds to a service: every pod of such
// an account logs that service in, so a caller here may be the very pod a request names, and the
// account and the pod uid are what tell the two apart.
//
// An account that BROKER_POD_OPERATORS maps to a person is refused, 403 PERSON_ACCOUNT
// (serviceCaller), for a token bound to a pod and for a token bound to no pod. A person's pod
// calls no route of this class.
//
// It is a class of its own rather than a power of accountAuth's because the two lists are two
// authorities: BROKER_API_ACCOUNTS names the callers that relay a person's decisions and read
// tokens back, and a daemon that registers its own pods is not one of them. A deployment already
// lists its per-project daemons in BROKER_SERVICE_LOGINS for their token logins, so the class
// needs no new login. The login is all a release needs, and a registration needs an entry of
// BROKER_POD_REGISTRARS too (registerPodSession), since a session pod logs in as a service as
// well. A release is the login account's (releasePodSessions): it releases every registration
// that account wrote of the names it sends, whichever of its pods wrote it. So every pod of an
// account paired with itself in BROKER_POD_REGISTRARS logs in as that registrar and can release
// every registration the account wrote, by session name alone, with no token of the pod whose
// trade it ends. Every pod that runs as any registrar's account logs in as that registrar in the
// same way and can release every registration that account wrote. So a deployment lists as a
// registrar only an account whose pods may end the trades of every pod that account registers. An
// account paired with itself is the common case of an account whose pods may not.
func serviceAuth(h func(*server, http.ResponseWriter, *http.Request, machine.ServiceAccount)) routeHandler {
	return routeHandler{auth: authService, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.service)
	}}
}

// edgeListenerAuth is a route for an edge listener: an Envoy listener that serves the sessions of
// the projects its entry names to agents outside the deployment's network. It calls with its own
// identity token, the one a listed caller presents (a projected ServiceAccount token for the
// broker's audience, or a Google identity token), verified the same way, and is known by the
// edge-listener entry of BROKER_NATS_SERVICES that names that token's account, not by
// BROKER_API_ACCOUNTS. The handler is given that entry, whose projects are the only ones it may
// ask about.
func edgeListenerAuth(h func(*server, http.ResponseWriter, *http.Request, EdgeListener)) routeHandler {
	return routeHandler{auth: authEdgeListener, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.edge)
	}}
}

// signInAuth is a route of the broker's own approval page, for a person in a browser signed in to
// the broker itself (OpenID Connect with the broker's own client, BROKER_SIGNIN_*). The handler is
// given the session the request's __Host-broker-session cookie names, or the zero session when it
// names none that is live, since what that means is the route's to answer: the page starts a
// sign-in, a decision answers a page linking the record it names. Every route of the class spends
// its source address's bucket first, the one the account class spends (authenticate), so no
// caller drives a record read, a token request to the sign-in provider or a session write
// unmetered. Mounted only when the broker builds its approval page, which every broker does but
// one with BROKER_SIGNIN=off.
func signInAuth(h func(*approval.Page, http.ResponseWriter, *http.Request, approval.Session)) routeHandler {
	return routeHandler{auth: authSignIn, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s.deps.Approval, w, r, who.signIn)
	}}
}

// operatorAuth is a route of the NATS writer hold, for its one caller: the operator entry of
// BROKER_NATS_SERVICES, presenting its own platform token (Authorization: Bearer), the
// Kubernetes ServiceAccount token for the NATS audience that the auth service verifies the
// operator's connection by. The token is verified with the auth service's own verifier
// (NATSHold.OperatorToken), held to the hour the auth service holds every service's token to, and
// must name the operator entry's account; anything else, every other entry's token and a
// person's broker token among them, is 403 not_operator. A person's broker token is also every
// session that person's agents run, so it cannot single out one operator. Every route of the
// class spends its source address's bucket first, the one the account class spends
// (authenticate), so a flood of forged tokens costs no verification. The handler is given the
// operator's account, which every row it writes names as its writer.
//
// It is a class of its own rather than a power of accountAuth's because the operator is no caller
// BROKER_API_ACCOUNTS lists: its authority is the operator entry of the NATS auth service's list,
// and its token is minted only under a person's approved release of the operator's credential.
func operatorAuth(h func(*server, http.ResponseWriter, *http.Request, string)) routeHandler {
	return routeHandler{auth: authOperator, serve: func(s *server, w http.ResponseWriter, r *http.Request, who caller) {
		h(s, w, r, who.operator)
	}}
}

type apiRoute struct {
	Method  string
	Pattern string
	Handler routeHandler
}

// routes is the one list of the broker's routes; a new route is a new row here, never a bare
// mux.HandleFunc. The comment above each row says what the route does; the broker's generated
// HTTP reference (cmd/cipher-refgen, published at
// https://cybermonklabs.github.io/legion/broker/reference/api/) prints it and refuses a row without one.
func routes() []apiRoute {
	return []apiRoute{
		// The broker's own OAuth 2.0 authorization server metadata (RFC 8414): its issuer, its
		// token, device authorization, introspection and revocation endpoints, the grants it
		// serves, how a client authenticates at the token endpoint, the algorithms a DPoP proof
		// may be signed with, and the subject token types its RFC 8693 exchange takes. It names
		// the device code and the device authorization endpoint only while the broker serves
		// the device grant, which is while BROKER_MACHINE_LOGIN_URL is set. It needs no
		// credential: it is what a client reads before it holds one, and every word of it is
		// already public in the broker's own reference. Mounted only when the broker runs its
		// OAuth provider, since with none it would name endpoints that are not there.
		{http.MethodGet, "/.well-known/oauth-authorization-server", public((*server).authorizationServerMetadata)},
		// Enroll a session's or pod's signing key under the caller's machine credential. Where
		// BROKER_PROJECT_ACCESS_URL is set, an enrollment under a person's machine login or pod
		// login is made only once Dispatch has said what kind of member that person is: a
		// restricted member's is refused 403 RESTRICTED_MEMBER_ENROLLS_NOTHING, and every one is
		// refused 503 MEMBER_KIND_UNCHECKABLE while that cannot be read from Dispatch. The broker
		// keeps the kind Dispatch last answered for each person, so while Dispatch does not answer
		// a person it last answered a full member enrolls as one; a person it last answered a
		// restricted member, or of whom it keeps no kind, is refused 503.
		{http.MethodPost, "/v1/enrollments", launcherAuth((*server).createEnrollment)},
		// Revoke an enrollment made under the caller's machine credential's operator, or a pod
		// enrollment the caller's own machine credential made.
		{http.MethodDelete, "/v1/enrollments/{id}", launcherEndAuth((*server).deleteEnrollment)},
		// Answer which pod enrollments the caller's account launched still carry something for the
		// named sessions, so a Legion daemon frees a session id it holds only once the answer is
		// empty. The body names 1 to 4096 session ids of 1 to 255 bytes each (400
		// INVALID_SESSION_IDS otherwise, as for a body that is no such object), read one id at a time
		// and refused at the entry past 4096. Its cap is the longest list those bounds allow with
		// every byte of every id escaped as \u00XX, about 6 MiB, in place of the 1 MiB of every other
		// JSON route (413 REQUEST_TOO_LARGE past it). It answers each pod enrollment of one of the ids
		// that the next enrollment of that id under a login of the caller's account could take from:
		// a live one whatever it holds, a dormant one while it holds a live grant or a pending
		// request, and never one ended for good, nor one a login of another account launched,
		// another daemon's pods of the same id and pod account among them. A credential whose login
		// names no account, one a person approved through a code, is 403 SERVICE_LOGIN_REQUIRED
		// before its body is read.
		{http.MethodPost, "/v1/enrollments/carried", launcherAuth((*server).carriedEnrollments)},
		// Log a service in with its own projected Kubernetes ServiceAccount token, which no person
		// approves: answers its launcher credential's id and expiry.
		{http.MethodPost, "/v1/launcher-credentials/token", public((*server).tokenLogin)},
		// Start a person's machine login as an OAuth device authorization (RFC 8628): answers the
		// device code the machine polls with and the code the person types. Rate-limited per source
		// address and per person its login_hint names; mounted only when the broker runs its OAuth
		// provider.
		{http.MethodPost, "/v1/oauth/device_authorization", public((*server).deviceAuthorization)},
		// Exchange an approved device code, or a refresh token, for a DPoP-bound token pair naming the
		// machine login's launcher credential; mounted only when the broker runs its OAuth provider.
		{http.MethodPost, "/v1/oauth/token", public((*server).oauthToken)},
		// Read one of the broker's access tokens back (RFC 7662): whether it is live and, when it
		// is, the session, the person and the projects it carries, and the key it is bound to.
		// Mounted only when the broker runs its OAuth provider.
		{http.MethodPost, "/v1/oauth/introspect", accountAuth(readsTokensBack, (*server).introspect)},
		// End one of the broker's own access tokens (RFC 7009), an exchanged pod's or workflow
		// run's included, which nothing else can end before its own expiry: the form names the
		// token, the client it was issued to, and optionally which kind to look for first. It
		// needs relaysDecisions rather than readsTokensBack because ending one of the broker's
		// tokens is the same authority class as ending a machine login or a grant, both already
		// behind that power; a caller listed only to read a token back must not be able to end
		// one. Mounted only when the broker runs its OAuth provider.
		{http.MethodPost, "/v1/oauth/revoke", accountAuth(relaysDecisions, (*server).revokeOAuthToken)},
		// Register that one pod may exchange under one session name: the row the token exchange
		// reads before it mints a token for a pod of an account that fixes no session, written by
		// the deployment's own daemon as it starts that pod. A name's first registration binds it,
		// for good, to the daemon that wrote it, and another daemon's registration of the name is
		// 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR. Mounted only when the broker runs its OAuth
		// provider.
		{http.MethodPost, "/v1/oauth/exchange/pod-sessions", serviceAuth((*server).registerPodSession)},
		// Release session names the caller's login account registered: each live registration of
		// each name that the account wrote, whichever of its pods wrote it, is marked released, so
		// the token exchange refuses the trade under it of the pod the registration names, and the
		// name stays bound to that account, as it is for good. Mounted only when the broker runs
		// its OAuth provider.
		{http.MethodPost, "/v1/oauth/exchange/pod-sessions/release", serviceAuth((*server).releasePodSessions)},
		// Extend the calling session's lease.
		{http.MethodPost, "/v1/enrollments/{id}/renew", renewAuth((*server).renewEnrollment)},
		// Describe the calling session's enrollment and its live grants.
		{http.MethodGet, "/v1/enrollments/self", sessionAuth((*server).readSelf)},
		// Request secrets: the policy grants or denies them at once, or a person must approve them.
		// Where BROKER_PROJECT_ACCESS_URL is set, a session whose person Dispatch calls a
		// restricted member is denied every secret, restricted-member, and a session whose person's
		// kind cannot be read from Dispatch is refused 503 MEMBER_KIND_UNCHECKABLE, no request
		// written; while Dispatch does not answer, a person it last answered a full member is read
		// as one, from the kind the broker keeps.
		{http.MethodPost, "/v1/requests", sessionAuth((*server).createRequest)},
		// Read a request the calling session's owner made: its person, its Legion claim or its
		// machine, or, for a session that records no owner, the session alone; with ?wait=N (whole
		// seconds, 1 to 20), hold the read while it is pending, answering as soon as it is decided
		// or once N seconds pass. Every other session is 403 NOT_YOURS. Before that, a session other
		// than the one the request hangs off is answered 404 NOT_FOUND, as for an id no request has,
		// when the request touches a private Dispatch project the session cannot see (one its own
		// credential does not name, or whose list no longer holds its person), whoever owns it, and
		// 503 PROJECT_ACCESS_UNAVAILABLE while Dispatch cannot say.
		{http.MethodGet, "/v1/requests/{id}", sessionAuth((*server).readRequest)},
		// Cancel a pending request the calling session's owner made, by the same rule as the read,
		// its 404 NOT_FOUND for a request of a private project the session cannot see and its 503
		// PROJECT_ACCESS_UNAVAILABLE included; neither cancels anything.
		{http.MethodPost, "/v1/requests/{id}/cancel", sessionEndAuth((*server).cancelRequest)},
		// Release a live grant's values to the session that holds it. Every other session is 403
		// NOT_YOURS, but one the grant's private project hides it from, as the read's rule says, is
		// answered 403 GRANT_NOT_LIVE, as for an id no grant has, and 503 PROJECT_ACCESS_UNAVAILABLE
		// while Dispatch cannot say; neither releases anything. Where BROKER_PROJECT_ACCESS_URL is
		// set, a restricted member's session is refused 403 GRANT_NOT_LIVE and the grant ended, and
		// one whose person's kind cannot be read from Dispatch is refused 503
		// MEMBER_KIND_UNCHECKABLE with nothing released; while Dispatch does not answer, a person it
		// last answered a full member is read as one, from the kind the broker keeps.
		{http.MethodPost, "/v1/grants/{id}/values", sessionAuth((*server).grantValues)},
		// End a grant the calling session holds. Every other session is 403 NOT_YOURS, but one the
		// grant's private project hides it from, as the read's rule says, is answered 404 NOT_FOUND,
		// as for an id no grant has, and 503 PROJECT_ACCESS_UNAVAILABLE while Dispatch cannot say;
		// neither revokes anything.
		{http.MethodPost, "/v1/grants/{id}/revoke", sessionEndAuth((*server).revokeGrant)},
		// List the undecided requests and machine logins the named person is asked to decide, each
		// with its state, pending or expired, and its deadline, and each secret request with the
		// broker's own page where it is decided. A request for a shared secret is listed for the
		// person responsible for the agent that made it, or for everyone when no person is. With
		// no ?state=, it is every record still pending by what is stored, newest first, an overdue
		// one included until the expiry sweep ends it. Each ?state=pending or ?state=expired asks
		// for that state: the records a decision may still settle, and the records whose deadline
		// passed before anybody decided them that nobody has dismissed, before the sweep and after
		// it, the pending ones first and each state newest first. Any other state is 400
		// STATE_INPUT. A person Dispatch calls a restricted member is no approver of a secret or of
		// a service's machine login, so their list holds no secret request and no service's login
		// an older broker opened, even one naming them, and while Dispatch cannot say what kind of
		// member the person is it holds none of those either; the person's own machine logins stay
		// on it. While Dispatch does not answer, a person it last answered a full member is read as
		// one, from the kind the broker keeps, and is listed as while it answers.
		{http.MethodGet, "/v1/pending", accountAuth(relaysDecisions, (*server).listPending)},
		// Approve a pending machine login as the named person, who must type its code: the broker
		// takes the person's name on the caller's word, from a listed service's ServiceAccount
		// token whose name carries relaysDecisions. It reads the record only once the caller is
		// authenticated, so a caller with no token, or one the broker does not accept, is refused
		// the same whether the id names a record or not, and an id no record has is 404 NOT_FOUND.
		// A secret request is decided only on the broker's own page by a person signed in to the
		// broker, so for one this answers the listed caller 403 DECIDE_ON_BROKER_PAGE naming that
		// page (or naming none, on a broker that signs nobody in), or 403 JUDGE_DECIDES while the
		// model judge holds it, writing nothing. To a person a private project hides the request
		// from — the body's approver, or a body naming none — it is the same 404 NOT_FOUND as an id
		// no record has, and while Dispatch cannot say whether the person may see it, 503
		// PROJECT_ACCESS_UNAVAILABLE.
		{http.MethodPost, "/v1/credential-requests/{record}/approve", accountAuth(relaysDecisions, (*server).approveRecord)},
		// Deny a pending machine login as the named person, who must type its code, on the
		// caller's word and behind the same authentication and the same power as approve. A
		// service's login a broker from before token logins opened is denied by anyone signed in
		// but a person Dispatch calls a restricted member, who is no approver of a secret: 403
		// NOT_APPROVER, and 503 MEMBER_KIND_UNCHECKABLE while Dispatch cannot say what kind of
		// member the person is, denying nothing; while Dispatch does not answer, a person it last
		// answered a full member is read as one, from the kind the broker keeps. A
		// secret request is refused the same way, writing nothing: 404 NOT_FOUND to a person its
		// private project hides it from, as for an id no record has, 503 PROJECT_ACCESS_UNAVAILABLE
		// while Dispatch cannot answer for it, and otherwise 403 JUDGE_DECIDES or 403
		// DECIDE_ON_BROKER_PAGE naming the page where a person signed in to the broker denies it.
		{http.MethodPost, "/v1/credential-requests/{record}/deny", accountAuth(relaysDecisions, (*server).denyRecord)},
		// Dismiss an expired credential request, a secret request or a machine login whose deadline
		// passed before anybody decided it, as the named person, for good: it leaves every list
		// that asks for expired records, the person's own and, for a shared request, everybody's.
		// An id no record has, and a secret request of a private project the person may not see,
		// are one 404 NOT_FOUND, and a request Dispatch cannot answer for is 503
		// PROJECT_ACCESS_UNAVAILABLE, dismissing nothing. A record that can still be decided is 409
		// RECORD_PENDING, and one that was decided, or cancelled before its deadline, is 409
		// RECORD_TERMINAL. A second dismissal answers the first one's values and writes nothing.
		// A person Dispatch calls a restricted member dismisses only their own machine logins: a
		// secret request, or a service's login an older broker opened, even one naming them, is 403
		// NOT_APPROVER to them, saying why, and while Dispatch cannot say what kind of member the
		// person whose list holds such a record is, its dismissal is 503 MEMBER_KIND_UNCHECKABLE,
		// dismissing nothing; while Dispatch does not answer, a person it last answered a full
		// member is read as one, from the kind the broker keeps. The
		// person is the login the body names, which the broker takes on the caller's word: a
		// dismissal decides nothing and releases nothing.
		{http.MethodPost, "/v1/credential-requests/{record}/dismiss", accountAuth(relaysDecisions, (*server).dismissRecord)},
		// Find a pending machine login by the confirmation code its machine shows.
		{http.MethodPost, "/v1/machine-logins/lookup", accountAuth(relaysDecisions, (*server).lookupMachineLogin)},
		// List the machine logins the named person may revoke, every service's, such as the Legion
		// daemon's, whoever approved it, the person's own machines', and the logins of the person's
		// own pods, that are not revoked and are unexpired or expired with a session still running,
		// each naming who approved it. A person's pod login is a token login of a Kubernetes account
		// that BROKER_POD_OPERATORS maps to a person, made with a token of one of its pods or with
		// one bound to no pod. It carries a service and a subject, but only that person's list holds
		// it, and its person field names that person. A person Dispatch calls a restricted member
		// is listed their own machines' and pods' logins alone, and so is anyone while Dispatch
		// cannot say what kind of member the person is, but for a person it last answered a full
		// member, read as one from the kind the broker keeps while Dispatch does not answer. The
		// person is the one the caller names.
		{http.MethodGet, "/v1/launcher-credentials", accountAuth(relaysDecisions, (*server).listLauncherCredentials)},
		// End a machine login, expired or not: a service's (its token login, or one a person approved
		// before token logins) as anyone signed in, a person's machine's as the person who approved
		// it, and a person's pod login as the person BROKER_POD_OPERATORS maps the pod's account to,
		// and as nobody else. Its launcher proofs stop authenticating and every session it enrolled,
		// pods included, ends with its grants and pending requests. The revoke does not keep a pod
		// out: the pod logs in again with its token under a new login while BROKER_SERVICE_LOGINS
		// names its account, and that login acts for the person only while BROKER_POD_OPERATORS
		// still maps the account to the person. A person Dispatch calls a restricted member ends
		// their own logins alone: a service's is 404 NOT_FOUND to them, as an id no login has, and
		// so it is to anyone while Dispatch cannot say what kind of member the person is, who still
		// ends their own, but for a person it last answered a full member, read as one from the
		// kind the broker keeps while Dispatch does not answer. The revoking person is the login
		// the request names, which the broker still takes on the caller's word: a revoke only takes
		// power away.
		{http.MethodPost, "/v1/launcher-credentials/{id}/revoke-by-approver", accountAuth(relaysDecisions, (*server).revokeLauncherCredential)},
		// List the live grants the named person's Live grants page shows: those the person
		// approved, those of their own sessions, those the model judge approved in anyone's stead,
		// and those a pod that acts for no person took without asking (a service's pod, or a
		// person's pod BROKER_POD_OPERATORS no longer maps), which nobody may revoke. A grant the
		// person approved, or whose session acts for them, is listed whatever project it touches;
		// an unmapped person's pod acts for no one, so its other grants are listed to its former
		// person naming no operator. Any other grant of a private project the person may not see is
		// left out, and one Dispatch cannot answer for is left out while it cannot. A person
		// Dispatch calls a restricted member is no approver of a secret and sees nothing beyond
		// their own projects, so their list holds the grants of the sessions that act for them
		// alone: no grant they approved or could approve, no grant of a service's pod, and none of
		// a person's pod that BROKER_POD_OPERATORS no longer maps to them. While Dispatch cannot
		// say what kind of member the person is, the list holds no grant of a session that does not
		// act for them either; while Dispatch does not answer, a person it last answered a full
		// member is read as one, from the kind the broker keeps. The person is the one the caller
		// names.
		{http.MethodGet, "/v1/grants", accountAuth(relaysDecisions, (*server).listGrantsForApprover)},
		// End a grant as its approver or as the person its enrollment acts for (an unmapped person's
		// pod acts for no one), whatever project it touches and whether or not Dispatch answers for it,
		// or, for a grant the model judge approved in anyone's stead, as anyone the grant's private
		// project, if it has one, lets see it. The operator's
		// revoke also withholds the secrets the grant's request got automatically from that session,
		// even when the grant had already ended: its other grants that got them automatically end
		// too, and it asks before it gets them again. To anyone else, a grant id no grant has and a
		// grant a private project hides from the person are one 404 NOT_FOUND, a grant Dispatch
		// cannot answer for is 503 PROJECT_ACCESS_UNAVAILABLE, and a grant they may see that names
		// them nowhere, of an approver other than anyone, is 403 NOT_APPROVER. A grant an adoption
		// moved to another box each time the revoke read it, three times in a row, is 409
		// GRANT_MOVING. A person Dispatch calls a restricted member ends a grant only as the person
		// its enrollment acts for, and one the grant admits only as an approver is 403
		// NOT_APPROVER, saying why. Each of these revokes nothing. While Dispatch does not answer
		// what kind of member the person is (a timeout, a failed connection, a gateway's 502, 503
		// or 504, Dispatch's own DATABASE_UNAVAILABLE or OIDC_ISSUER_UNAVAILABLE, or a 500 naming
		// no code or INTERNAL, which Dispatch answers while its database fails), a person it last
		// answered a full member is read as one, from the kind the broker keeps, and revokes as
		// while it answers; for anyone else, only the person the grant names as its approver ends
		// it as an approver. Every revoke through a group or as anyone by such a person then,
		// whatever session holds the grant, and every revoke while Dispatch answers
		// anything else the broker cannot read (a refused account, a kind this broker does not
		// know, another code, a certificate or a name that does not resolve), is 503
		// MEMBER_KIND_UNCHECKABLE, revoking nothing. A grant on a person's session can still be
		// ended then by that person, its operator, who asks Dispatch nothing; a grant on a session
		// that acts for no person (a service's pod, a person's pod the broker no longer maps, or a
		// machine's own session) is ended through the broker by nobody but its named approver
		// until Dispatch answers. A revoke whose caller hung up after the member-kind read is 503
		// REQUEST_ENDED. The revoking person is the login the request names, which the broker
		// still takes on the caller's word: a revoke only takes power away.
		{http.MethodPost, "/v1/grants/{id}/revoke-by-approver", accountAuth(relaysDecisions, (*server).revokeByApprover)},
		// List the sign-ins the named person made on their machines with `agent-secrets login`,
		// by machine: one row per box key they signed in with and have not revoked, each naming
		// the machine's Google account, the box key's thumbprint, and when the sign-in was made
		// and last used. The person is the one the caller names, as it is for every list
		// Dispatch's server relays.
		{http.MethodGet, "/v1/person-signins", accountAuth(relaysDecisions, (*server).listPersonSignIns)},
		// End one of the named person's sign-ins: that box key trades no token at the exchange
		// again, and every access token already minted on that box under that sign-in ends in
		// the revoke's own transaction, so none of them outlives it. The person's sign-ins on
		// their other boxes keep working, and the box signs in again with a key of its own. The
		// revoking person is the one the request names, which the broker takes on the caller's
		// word: a revoke only takes power away.
		{http.MethodPost, "/v1/person-signins/{id}/revoke", accountAuth(relaysDecisions, (*server).revokePersonSignIn)},
		// End everything one Google account holds under an address another account now holds,
		// whenever it was issued, and refuse it anything new there: a Dispatch that records
		// accounts is to call it when another account takes an address it holds for a person
		// (Dispatch in this tree does not call it yet), naming the address, the identity
		// provider, the replaced account's `sub` and the holder's. Every sign-in on
		// a machine that account's person token made, every login of the address's own machines
		// it approved and every session on the broker's own approval page it signed in ends, with
		// the tokens and sessions each holds, and so does every row that records no account; the
		// holder's, and a service's login, are untouched. From then on a sign-in trade with that
		// account's token is refused invalid_grant, an approval by it 403 APPROVER_REPLACED and
		// its sign-in to the approval page 403, and a revoke naming it as the holder stops
		// refusing it. A repeat ends nothing and answers zeros. The broker takes the accounts on
		// the caller's word, which does more than take power away: naming the holder lifts the
		// refusal an earlier revoke recorded for that account, and nothing else.
		{http.MethodPost, "/v1/person-subjects/revoke", accountAuth(relaysDecisions, (*server).revokePersonSubject)},
		// Read the broker's grant log for one closed period, for a quarterly access review:
		// every audit row between ?since and ?until (RFC 3339, half-open), oldest first, each
		// naming the session that wrote it, that session's person, the service the broker
		// verified, the secrets asked for and their decisions, the state, the approver and who
		// decided. ?after resumes after an id and ?limit bounds the page, which is a thousand
		// rows at most. ?until must be at least an hour before now, so that every row of the
		// period has committed before the review reads it. It is the one route readsGrantLog
		// reaches: the review reads the broker's record and changes nothing in it.
		{http.MethodGet, "/v1/grant-log", accountAuth(readsGrantLog, (*server).listGrantLog)},
		// Answer an edge listener the repositories the issues of one project resolve to,
		// ?project=<KEY>, as Dispatch answers them (GET /api/v1/access/projects/{key}/repositories
		// at BROKER_PROJECT_ACCESS_URL), read from Dispatch on every call and never kept:
		// {"project", "repositories": ["owner/name", …]}, [] when none. Only an edge listener
		// whose BROKER_NATS_SERVICES entry lists the project is answered; a project the entry does
		// not list is 403 PROJECT_NOT_LISTED and Dispatch is not asked. Whenever the broker cannot
		// get Dispatch's whole answer within 2 seconds (Dispatch's 503 or any other refusal, a
		// stall, a body it cannot read, or no BROKER_PROJECT_ACCESS_URL at all) it is 503
		// REPOSITORIES_UNKNOWN, never a list; and past 16 calls of one edge listener waiting on
		// Dispatch at once on one broker process, 503 REPOSITORIES_BUSY. That bound is each
		// process's, so N replicas let one edge listener hold 16 × N calls on Dispatch: a guard on
		// Dispatch's load, not a security bound. The edge-listener entries are read whether or not
		// the broker runs the NATS auth service.
		{http.MethodGet, "/v1/project-repositories", edgeListenerAuth((*server).projectRepositories)},
		// List the calling credential's operator's own logins, their machines' logins and the token
		// logins of the accounts BROKER_POD_OPERATORS maps to them, as Dispatch's machine-logins
		// page lists them, without the service logins that page also lists. This route, the machine
		// revoke below and the two grant routes refuse a person's pod login 403 PERSON_POD_LOGIN
		// and a service's login 403 SERVICE_CREDENTIAL (operatorOf). The retire refuses neither.
		{http.MethodGet, "/v1/operator/machines", launcherEndAuth((*server).listOperatorMachines)},
		// End one of the operator's own logins: no proof signed with it authenticates again
		// and every session it enrolled ends. A service's login is not found here, like an unknown
		// id. Revoking the calling machine's own login ends this machine's access.
		{http.MethodPost, "/v1/operator/machines/{id}/revoke", launcherEndAuth((*server).revokeOperatorMachine)},
		// Retire the calling machine login after its machine logged in again: it enrolls nothing
		// more and cannot refresh, while the sessions it enrolled keep running and renewing with their
		// own keys. {id} must be the calling credential.
		{http.MethodPost, "/v1/operator/machines/{id}/retire", launcherEndAuth((*server).retireOperatorMachine)},
		// List the live grants of the operator's sessions and those the operator approved, exactly as
		// Dispatch's Live grants page lists them.
		{http.MethodGet, "/v1/operator/grants", launcherEndAuth((*server).listOperatorGrants)},
		// End a grant as the operator: the revocation Dispatch's page offers, which also withholds an
		// automatic grant's secrets from its session. It answers as the person's own revoke of the
		// grant does: a grant whose approver is the operator, or whose enrollment acts for the
		// operator (an unmapped person's pod acts for no one), is ended whatever project it touches
		// and whether or not Dispatch answers. Of any other grant, an id no grant has and a grant a
		// private project hides from the operator are one 404 NOT_FOUND, a grant Dispatch cannot
		// answer for is 503 PROJECT_ACCESS_UNAVAILABLE, and a grant the operator may see that names
		// them nowhere, of an approver other than anyone, is 403 NOT_APPROVER. A grant an adoption
		// moved to another box each time the revoke read it, three times in a row, is 409
		// GRANT_MOVING. Each of these revokes nothing.
		{http.MethodPost, "/v1/operator/grants/{id}/revoke", launcherEndAuth((*server).revokeOperatorGrant)},
		// The store, the namespace prefix, the agent-secrets key and the account or project it is
		// in, and the Google exchange audience: what the CLI needs so it and the broker never
		// disagree about which secret a name means. It needs no credential, because a machine's
		// CLI reads the exchange audience here before it holds any credential of the broker's, to
		// mint the identity token it trades for its first one, and the secret forms read the rest
		// under a cloud credential alone.
		{http.MethodGet, "/v1/settings", public((*server).readSettings)},
		// Reread one secret from the secret store now, as whoever just wrote it may ask: the change
		// is served, or gone, within seconds instead of at the next five-minute reload, on every pod.
		// Open to any caller, and rate-limited both per source address and over every caller at
		// once, since each reread holds the policy's writer lock while it reads; a reread only moves
		// the broker toward the store's own truth. A caller that sends no proof is answered 202 at
		// once, the same for every name, and the reread runs after that answer; a session that
		// signs the call is answered once the reread is done, with whether the broker now serves
		// the name, why not, and as which type; a proof the broker does not accept as a session's
		// is 401 PROOF_INVALID, whatever the name; and a session whose person Dispatch holds is
		// refused as on every session route, 403 MEMBERSHIP_HELD or 503 HOLD_UNCHECKABLE.
		{http.MethodPost, "/v1/secrets/{name}/reread", public((*server).rereadSecret)},
		// A person's approval page for one credential-request record, an HTML page on the broker's
		// own origin: the record's secret names, the enrollment the broker verified asked, the
		// session the request claims when it differs, the reason (empty only on a record an older
		// build wrote before the broker required one wherever a person decides), the
		// lifetime, the times, the approver and the state, one sentence saying what Approve gives
		// and for how long and one saying what Deny and the expiry do, then every other pending
		// secret request the person may decide, with Approve and Deny for each the person may
		// decide and one "Approve all N shown". It checks the id's shape, then the session, then
		// the record: a malformed id is a 400, a request with no live session, or with ?switch=1,
		// redirects to the sign-in provider and returns here, and only a signed-in person's
		// unknown record id, or a record a private project hides from them, is the one 404. A
		// machine login's record is printed with the note that it is decided by typing its code
		// on Dispatch's machine-login page, and offers no button.
		{http.MethodGet, "/approve/{record}", signInAuth((*approval.Page).ServeRecord)},
		// Decide records an approval page printed, as the signed-in person: a form from the
		// broker's own origin only, carrying the page's token, which binds it to the session and to
		// exactly the records the page printed with a button, for an hour. The session is read
		// again once the form has arrived, so one that ended while its body was on the way
		// decides nothing. Each record is decided on its own, and the answer is a 303 back to the
		// page, which says what became of each.
		{http.MethodPost, "/approve", signInAuth((*approval.Page).ServeDecide)},
		// Finish a sign-in to the approval page, the provider's redirect back with a code, checked
		// against the sign-in this browser started (its __Host-broker-signin cookie, used once):
		// state, PKCE, the ID token and the Workspace domain. A Google account that no longer
		// holds its address, which Dispatch named replaced, is refused 403 with no session. It
		// makes a new session recording the account that signed in, ending the one the browser
		// held, and answers a 303 to the record's page the sign-in started from.
		{http.MethodGet, "/signin/callback", signInAuth((*approval.Page).ServeCallback)},
		// Sign out of the approval page, from the broker's own origin only: the session ends and
		// its cookie is cleared, and the answer links back to the record.
		{http.MethodPost, "/signin/logout", signInAuth((*approval.Page).ServeLogout)},
		// Report the broker's sign-in: whether its sign-in provider accepted the broker's own
		// client at the last hourly check and when, when a person last signed in, when a
		// sign-in last failed and the coded reason it failed with, and when a decision was last
		// made on the approval page. It needs no credential because a production check fetches
		// it from a pod where no person is signed in, and because it names no login, no secret
		// and no provider's message: every word it answers is one of two closed sets this
		// broker chose (internal/cipher/store's signin_status). It spends its source address's
		// bucket in its own handler, as the page's routes spend it in authenticate, so this read
		// of the broker's own row is metered too.
		{http.MethodGet, "/signin/status", public((*server).signInStatus)},
		// The approval page's one stylesheet, embedded in the broker; it needs no credential and
		// spends no rate-limit bucket, since every page it dresses is read before anyone has
		// signed in and the bytes are the binary's.
		{http.MethodGet, "/approve/assets/approve.css", public((*server).approvalStylesheet)},
		// Report whether the broker can reach its database. From SIGTERM onward it answers 503
		// SHUTTING_DOWN instead (Deps.Draining) while the process goes on serving every other route,
		// which is what takes a stopping pod out of its Service's endpoints before it stops
		// accepting connections. So this is a readiness probe and must never be used as a liveness
		// probe: a liveness probe on it would have the kubelet kill the container in the middle of
		// the drain, dropping the requests the pod was draining.
		{http.MethodGet, "/healthz", public((*server).healthz)},
		// Read the NATS writer hold's row: its gen, phase, hold id, reason, who wrote it and when,
		// and the time the operator signed it with its signature, which the operator's commands
		// verify before they act on it; 404 no_row while there is none, and 409 below_floor for a
		// verified row this replica's floor of the row reads as an older row put back or another
		// row at the gen it served (holdRowFloor, a signedrecord.Floor).
		{http.MethodGet, "/v1/nats-hold", operatorAuth((*server).readNATSHold)},
		// Move the writer hold's row, in one statement with its log row, from the gen the body
		// names to one above it: released to prepare with a new hold id, never empty and never one
		// an earlier hold held; prepare to held with the same hold id; prepare or held to released
		// with the same hold id, which the row keeps. The body is signed by a key of
		// BROKER_NATS_HOLD_SIGNING_KEYS over the gen it will store (400 bad_signature otherwise).
		// A move from an older row put back is 409 gen_mismatch with no row, and one from another
		// row at the gen the floor took (a fork), or whose gen the log already holds (a row put back
		// below the log), 409 below_floor with no row; a gen the row no longer holds is 409
		// gen_mismatch and a hold id not the row's 409 hold_mismatch, each with the row, and any
		// other move 400 bad_transition. A row signed more than five minutes from the broker's
		// clock, or more than five minutes before the latest signing time the floor took or the
		// log's verified entries hold, is 409 clock_skew, judged before the write, storing
		// nothing. The move commits before it answers, so every decision of the auth service that
		// starts after the answer reads the new phase.
		{http.MethodPut, "/v1/nats-hold", operatorAuth((*server).moveNATSHold)},
		// Write the writer hold's row while there is none, the first row or one a database admin
		// deleted, signed within five minutes of the broker's clock, the floor's latest time and
		// the log's (409 clock_skew otherwise, storing nothing), judged by the log's latest entry,
		// which must verify (409 below_floor otherwise, storing nothing): held with a hold id, only
		// after a prepare or held entry of that hold id, or released, only after a released entry,
		// a prepare entry of the hold id it names (409 hold_mismatch otherwise, naming that hold,
		// so the prepared hold is not left where no command reaches it), or a held one whose hold's
		// signed journal shows it ended (400 bad_transition otherwise, so no restore frees the
		// clients mid-hold, and a hold the log shows ended is not held again); and, whatever the
		// latest entry, never while any hold's journal, an ended hold's included, is below the
		// record floor, does not verify, or holds an entry whose hold row the verified log does
		// not hold or that names none (409 below_floor), nor while a hold's journal shows hold set
		// done, or hold set not done with the hold's held entry in the log, and no end (a done
		// hold release or hold abort, or a verified released entry naming the hold while its
		// journal shows hold release's or hold abort's intent as that command's latest entry and
		// no other intent pending) (400 bad_transition), each naming the hold, the log's latest
		// entry, and the key id of a signature whose key the list no longer holds; at one above
		// the highest gen nats_hold_log holds (1 for an empty log), signed over that gen. Another
		// gen is 409 gen_mismatch naming restore_gen, the gen to sign afresh, since the operator's
		// commands cannot read the log; a row that is there is 409 row_exists.
		{http.MethodPost, "/v1/nats-hold/restore", operatorAuth((*server).restoreNATSHold)},
		// Write one record of the hold the row names, whatever its phase: its cohort, its
		// conversion's saved set, its journal or its selection-recovery inputs, signed over the
		// gen it will store. expected_gen 0 inserts a record that is not there, and any other gen
		// updates the record that holds it, one above; a record moved since is 409 gen_mismatch,
		// and so is a write storing a gen at or below the record's floor (recordFloors), an insert
		// of a key the routes deleted while its signed delete verifies, and a hold id other than
		// the row's 409 hold_mismatch.
		{http.MethodPut, "/v1/nats-hold/records", operatorAuth((*server).putNATSHoldRecord)},
		// List one hold's records of one kind (?hold_id= and ?kind=), each with its gen, when it
		// was stored and its signature; a record below its floor, or one the floor has seen that
		// is now absent with no signed delete of it that verifies, is 409 below_floor.
		{http.MethodGet, "/v1/nats-hold/records", operatorAuth((*server).readNATSHoldRecords)},
		// Delete the cohort record of the hold the row names (?hold_id=, ?kind=cohort, ?key=,
		// ?gen=, ?sig=), for hold abort from prepare, which deletes it before it writes released,
		// and for hold abort on a row lost in prepare and restored released: signed over the
		// record's current gen (400 bad_signature otherwise), only while the row is prepare with
		// that hold id, or released with it while that hold's done record verifies and shows
		// prepare, or there is no done record (409 hold_mismatch otherwise), and only at that gen
		// (409 gen_mismatch otherwise). The same statement keeps the signed delete beside the
		// records, so every replica reads the cohort as deleted. No other kind is deleted, so the
		// journal outlives an abort.
		{http.MethodDelete, "/v1/nats-hold/records", operatorAuth((*server).deleteNATSHoldCohort)},
		// Write one family's cutover row, done by the hold's conversion or rolled_back by its
		// rollback, with hold_gen the hold row's current gen, only while the row is held with the
		// body's hold id (409 hold_mismatch, with the row, otherwise). The body is signed over that
		// gen, which the operator reads and verifies just before it writes; a signature over any
		// other gen is 400 bad_signature.
		{http.MethodPut, "/v1/nats-hold/cutover", operatorAuth((*server).putNATSHoldCutover)},
		// List every family's cutover row, each with its signature.
		{http.MethodGet, "/v1/nats-hold/cutover", operatorAuth((*server).readNATSHoldCutovers)},
	}
}
