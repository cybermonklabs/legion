// Package wire is the broker's response shapes: the JSON bodies the routes Dispatch relays
// answer with, each declared once here and exported so that Dispatch's OpenAPI emitter can name
// the shapes its relay routes answer. Dispatch relays them with relayBrokerResponse
// (packages/envoy/internal/dispatch/api/credential_requests.go), which writes the broker's bytes
// unparsed, so the shape a generated client is given has to be named from the broker's own
// declaration rather than read off the relay. It also holds how long the broker works on one
// request (RequestDeadline, RequestWriteMargin), which cmd/cipherd serves by and a caller of the
// broker can count on, so the two read one figure.
//
// It is a leaf: data and its doc comments, importing nothing of the broker's API package and
// nothing holding a store, an OAuth client or a database.
package wire

import "time"

// RequestDeadline is how long the broker lets one request's handler work: cmd/cipherd ends each
// request's context this long after its handler starts (requestDeadline, withRequestDeadline), so
// no write a handler makes under that context begins after it.
const RequestDeadline = 45 * time.Second

// RequestWriteMargin is the time past RequestDeadline that the broker's server gives a handler
// that honours its context to finish what it already began and write its answer: cmd/cipherd's
// http.Server WriteTimeout is the two together (writeTimeout). WriteTimeout bounds the writing of
// that answer, not the broker's database: a COMMIT the handler sent before RequestDeadline is over
// when that database completes it, which no deadline here ends, and a cancel does not undo a commit
// the database already received. A caller may count on the broker's database completing such a
// commit within this margin, and so on a write the broker began for a request being over within
// RequestDeadline + RequestWriteMargin of the handler's start, whether or not that caller is still
// waiting for the answer.
const RequestWriteMargin = 15 * time.Second

// PendingEntry is one undecided record in GET /v1/pending's answer: one a decision may still
// settle, or one whose deadline passed before anybody decided it.
type PendingEntry struct {
	// The credential-request record's id, which the approval page and the machine login's approve
	// and deny routes take.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// The secrets a secret request asks for, or the machine a machine login names.
	Identifiers []string `json:"identifiers"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// The session this request names, read independently of each other (LEGION-587); see
	// RecordSessionResp, this row's exact shape here too.
	Session RecordSessionResp `json:"session"`
	// A secret request's page on the broker, <BROKER_PUBLIC_URL>/approve/<record>, where a person
	// signed in to the broker decides it; absent for a machine login, which is decided with its
	// typed code, and on a broker with BROKER_SIGNIN=off, which serves no page. An expired row
	// carries it too, and that page shows the request expired, with nothing left to decide; the
	// Inbox and `dispatch inbox` link no expired row to it.
	ApproveURL string `json:"approve_url,omitempty"`
	// "pending" for a record a decision may still settle, and "expired" for one whose deadline
	// passed before anybody decided it, which every decision refuses and which a list asking for
	// ?state=expired holds until somebody dismisses it (POST
	// /v1/credential-requests/{record}/dismiss). A broker from before records had a state sends
	// none, and a reader takes a row with no state as pending.
	State string `json:"state,omitempty" enum:"pending,expired"`
	// The record's deadline: a secret request's own expiry, the moment its decision compares
	// with, and a machine login's. A broker from before records had a state sends none.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// PendingResponse is GET /v1/pending's answer.
type PendingResponse struct {
	// The undecided records the named person decides. With no ?state=, every record still
	// pending by what is stored, newest first, an overdue one included until the expiry sweep
	// ends it; with ?state=, the records of the states asked for, the pending ones first and the
	// expired ones nobody has dismissed after them, each newest first.
	Pending []PendingEntry `json:"pending"`
}

// DismissResponse is what POST /v1/credential-requests/{record}/dismiss answers: the record's one
// dismissal, the one this call made or, for a record dismissed already, the earlier one, whose
// values a second dismissal answers unchanged.
type DismissResponse struct {
	// The dismissed record's id.
	RecordID string `json:"record_id"`
	// Who dismissed it: the canonical login of the person whose dismissal took it off every list,
	// or "broker" for a record that had already left the lists, expired, when the broker began
	// keeping dismissals (store migration 0033).
	DismissedBy string `json:"dismissed_by"`
	// When it was dismissed.
	DismissedAt time.Time `json:"dismissed_at"`
}

// ApproverGrantResp is one grant in GET /v1/grants?approver=<login>: a grant on a session the login
// operates, automatic or approved by anyone, or a grant the login approved on anyone's session.
type ApproverGrantResp struct {
	// The grant's id, which the revoke route takes.
	GrantID string `json:"grant_id"`
	// How the session got it: "automatic" (the policy gave it without asking) or "approval".
	// GrantsForApprover writes one of the two (machine_read.go, policy.Automatic and
	// policy.Approval), so it is a closed set.
	Granted string `json:"granted" enum:"automatic,approval"`
	// The credential-request record its approval rests on; null for an automatic grant.
	RecordID *string `json:"record_id"`
	// The session holding it.
	Enrollment RecordEnrollmentResp `json:"enrollment"`
	// The secrets it covers.
	Names []string `json:"names"`
	// The person who approved it; null for an automatic grant. It is who the grant belongs to,
	// which is what the revoke rule reads (requests.Machine.RevokeByApprover's mayRevoke), and
	// for a grant the model judge approved it is the record's own approver — the owner, or
	// "anyone" for a shared secret — in whose stead the judge decided. Who decided it is
	// DecidedBy, which for such a grant is the judge and not this person.
	Approver *string `json:"approver"`
	// Who decided the request this grant rests on (requests.decided_by): the approving person's
	// own login, or "judge:<model id>" where the model judge approved it. Null for an automatic
	// grant, which nobody decided. A page that says who decided a grant says this, so that a
	// judged grant names the judge rather than a person who never saw the request.
	DecidedBy *string `json:"decided_by"`
	// When it expires; null for a grant that lasts until its session ends.
	ExpiresAt *time.Time `json:"expires_at"`
	// When it was granted.
	CreatedAt time.Time `json:"created_at"`
}

// ApproverGrantsResponse is GET /v1/grants's answer.
type ApproverGrantsResponse struct {
	// The live grants of the named person's sessions, and those the person approved, newest first.
	Grants []ApproverGrantResp `json:"grants"`
}

// LauncherCredentialResp is one machine login in GET /v1/launcher-credentials?approver=<email> and
// GET /v1/operator/machines.
type LauncherCredentialResp struct {
	// The launcher credential's id, which the revoke route takes.
	CredentialID string `json:"credential_id"`
	// The machine's host name, as a person's machine login named it; for a token login, its
	// ServiceAccount's <namespace>/<name>, or a machine's <project>/<zone>/<instance name>.
	Host string `json:"host"`
	// The service a service's login is for (legion-daemon), whose sessions are its pods, the
	// login service of a person's pod login, or the fleet a machine belongs to; null for a
	// person's own machine.
	Service *string `json:"service"`
	// What a token login proved: its ServiceAccount
	// (system:serviceaccount:<namespace>:<name>), or a machine as the token exchange names it
	// (exchange.MachineSubject); null for every login a person approved.
	Subject *string `json:"subject"`
	// For a person's pod login, the person it acts for. A person's pod login is the token login
	// of a Kubernetes ServiceAccount that BROKER_POD_OPERATORS maps to a person, and this is that
	// person as the setting maps the account now. Only that person lists and revokes the login.
	// Null for every other login, a service's and a person's own machine's among them.
	Person *string `json:"person"`
	// The login of the person who approved the login (anyone signed in may have approved a service's
	// login before token logins); "" for every token login, a service's and a person's pod login
	// among them, which no person approves.
	ApprovedBy string `json:"approved_by"`
	// When the login was approved, or a token login's first login.
	IssuedAt time.Time `json:"issued_at"`
	// When the credential expires. Each refresh of a person's device login moves it to
	// BROKER_LAUNCHER_CREDENTIAL_SECONDS from then; a login through the earlier route never
	// refreshes; a token login moves it later at each login, at most an hour away.
	ExpiresAt time.Time `json:"expires_at"`
	// True once expires_at has passed: the login enrolls nothing more, but sessions it enrolled
	// still run (each renews with its own key), and revoking it ends them.
	Expired bool `json:"expired"`
}

// LauncherCredentialsResponse is GET /v1/launcher-credentials's and GET /v1/operator/machines's
// answer.
type LauncherCredentialsResponse struct {
	// The machine logins that are not revoked and are either unexpired or expired with a session
	// still running, newest first: every service's, the named person's own machines' and that
	// person's pod logins on Dispatch's page; the calling operator's own machines' and pod logins
	// alone on the operator route. A person's pod login is on its person's list alone.
	Credentials []LauncherCredentialResp `json:"credentials"`
}

// PersonSignInResp is one live sign-in in GET /v1/person-signins?person=<email>.
type PersonSignInResp struct {
	// The sign-in's id, which the revoke route takes.
	SignInID string `json:"signin_id"`
	// The Google account of the machine the person signed in on, as BROKER_VM_SERVICES lists it:
	// the fleet's account, which is what names the box on the person's own list.
	Machine string `json:"machine"`
	// The RFC 7638 thumbprint of the box key the sign-in is bound to, which is what tells this
	// sign-in from the person's others on the same fleet. It is public: it is what the box's
	// DPoP proofs already carry.
	Thumbprint string `json:"thumbprint"`
	// When the person first signed in on that box.
	CreatedAt time.Time `json:"created_at"`
	// When that box last traded a token under this sign-in.
	LastUsedAt time.Time `json:"last_used_at"`
}

// PersonSignInsResponse is GET /v1/person-signins's answer.
type PersonSignInsResponse struct {
	// The named person's live sign-ins, newest first: one per box key they signed in with and
	// have not revoked. A revoked sign-in is not listed, and neither is anyone else's.
	SignIns []PersonSignInResp `json:"signins"`
}

// PersonAccessResponse is GET /v1/person-access's answer: what the named person has at the
// broker that outlives their browser session. The broker writes it for Dispatch's account, and
// Dispatch reads it at a sign-out and in its membership pass, to decide whether it keeps the
// person's refresh token so that Google's later refusal still holds them.
type PersonAccessResponse struct {
	// The person asked about, a login as record.CanonicalLogin spells it.
	Person string `json:"person"`
	// How many of the person's own machine logins are live: approved for no service, not
	// revoked, and unexpired or with a session still running (enroll.Service.OwnLiveCredentials),
	// a live login of an account BROKER_POD_OPERATORS maps to the person among them. A service's
	// machine login the person approved is not counted.
	MachineLogins int `json:"machine_logins"`
	// How many of the person's sign-ins on a machine are not revoked.
	SignIns int `json:"signins"`
	// How many entries of the broker's exchange configuration let a pod, a workflow or a machine
	// trade for a token that names the person.
	ExchangeEntries int `json:"exchange_entries"`
	// How many Kubernetes service accounts BROKER_POD_OPERATORS maps to the person, whether or not
	// a pod of one holds a live login: a pod of each can log in as the person at any time with its
	// own token (enroll.Service.PodOperatorMappings).
	PodOperatorMappings int `json:"pod_operator_mappings"`
}

// PersonSignInRevokedResponse is POST /v1/person-signins/{id}/revoke's answer.
type PersonSignInRevokedResponse struct {
	// Always "revoked": the sign-in no longer trades, whether this call ended it or an earlier
	// one did.
	State string `json:"state"`
	// How many of that person's live access tokens this call ended with it, the ones minted on
	// that box under that sign-in. It is 0 for a sign-in revoked already, and for one whose
	// tokens had all expired.
	TokensEnded int `json:"tokens_ended"`
}

// RevokePersonSubjectRequest is POST /v1/person-subjects/revoke's body: Dispatch tells the broker
// that one Google account no longer holds an address, which another account holds now. Every field
// is required, and Subject differs from HolderSubject.
type RevokePersonSubjectRequest struct {
	// The address, a login as record.CanonicalLogin spells it.
	Person string `json:"person"`
	// The identity provider both accounts are of, as oidc.CanonicalIssuer spells it.
	Issuer string `json:"issuer"`
	// The `sub` of the account that no longer holds the address: every credential it holds under
	// the address ends, and the broker refuses it any new one.
	Subject string `json:"subject"`
	// The `sub` of the account that holds the address now, which keeps every credential it holds
	// and which the broker no longer refuses if an earlier revocation named it.
	HolderSubject string `json:"holder_subject"`
	// The address's session generation at Dispatch once the replacing sign-in committed (Dispatch's
	// user_sessions.generation, which every recording of an account moves): the broker refuses a
	// machine-login approval whose approver_session_generation is lower, since Dispatch checked
	// that approver before this replacement, and answers a revoke below the greatest generation it
	// applied at the address superseded. Required: the broker answers 400
	// SESSION_GENERATION_REQUIRED without it. It is a pointer so that a body that leaves it out is
	// told from one carrying 0. The route and the field came in one change, so no caller sends the
	// route without it.
	SessionGeneration *int64 `json:"session_generation,omitempty"`
}

// PersonSubjectRevokedResponse is POST /v1/person-subjects/revoke's answer.
type PersonSubjectRevokedResponse struct {
	// PersonSubjectRevoked: the replaced account holds no credential and no approval-page session
	// under the address, and the broker refuses it any new one, whether this call or an earlier one
	// ended them. PersonSubjectSuperseded: the revoke carried a session generation below the
	// greatest one a revoke the broker applied at the address carried (SupersededBy), so a later
	// replacement at the address has been applied since, and this one changed nothing; its caller
	// can tell from it that its own generations for the address are below the broker's. The
	// broker's migration 0047 says how such a revoke can arrive.
	State string `json:"state" enum:"revoked,superseded"`
	// The greatest session generation a revoke the broker applied at the address carried, which is
	// above this revoke's; given with PersonSubjectSuperseded alone.
	SupersededBy *int64 `json:"superseded_by,omitempty"`
	// How many of the replaced account's sign-ins on a machine this call ended; 0 for a repeat
	// and for a superseded revoke.
	SignIns int `json:"signins"`
	// How many of the replaced account's own machines' logins this call ended; 0 for a repeat and
	// for a superseded revoke.
	MachineLogins int `json:"machine_logins"`
	// How many live access tokens those revokes ended with them.
	TokensEnded int `json:"tokens_ended"`
	// How many of the replaced account's sessions on the broker's own approval page this call
	// ended; 0 for a repeat and for a superseded revoke.
	ApprovalSessions int `json:"approval_sessions"`
}

// The states PersonSubjectRevokedResponse.State answers.
const (
	PersonSubjectRevoked    = "revoked"
	PersonSubjectSuperseded = "superseded"
)

// RecordSessionResp is a credential request's two possibly-differing session ids, read
// independently (LEGION-587): Enrollment is the id the requesting session's own enrollment
// stated when it enrolled, Request is the id the request itself stated as an unsigned override
// in its body. Either is null when that id was never set; a machine login (which has no request
// row and no requesting enrollment) always answers both null, so every record POST
// /v1/machine-logins/lookup answers carries two nulls. GET /v1/pending's own
// PendingEntry.Session carries this same shape, where a secret request's row names the ids it
// has.
type RecordSessionResp struct {
	// The id the request itself stated as an unsigned override in its body; null when it named
	// none.
	Request *string `json:"request"`
	// The id the requesting session's own enrollment stated when it enrolled; null when it named
	// none, or for a machine login, which has no requesting enrollment at all.
	Enrollment *string `json:"enrollment"`
}

// RecordEnrollmentResp is a record's or a grant's requesting enrollment. Slot is a pod
// enrollment's slot, one of several independent identities in one pod, and null for every
// enrollment without one.
type RecordEnrollmentResp struct {
	// "session" (an agent session on a machine its person logged in) or "pod".
	Kind string `json:"kind"`
	// The session's runtime: whatever names the session on its machine (the helper's
	// host:pid:start time, a container's id), or a pod's UID.
	RuntimeID string `json:"runtime_id"`
	// The person the session runs for. On a record it is as the enrollment row held it when the
	// request was made: for a pod, the person that a person's pod login (enroll.PersonPodLogin)
	// carried when it enrolled the pod, and empty for any other pod. On a grant list (GET
	// /v1/grants and GET /v1/operator/grants) it is the person the enrollment acts for now, as the
	// broker decides it (requests' operatorOf): empty for a person's pod that BROKER_POD_OPERATORS
	// no longer maps.
	Operator string `json:"operator"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

// RecordDecisionResp is a decided record's terminal event. CredentialID is null unless the
// decision minted a launcher credential: an agent_secret record's approval names none.
type RecordDecisionResp struct {
	// "approved", "denied", "expired" or "cancelled".
	Event string `json:"event"`
	// When it was decided.
	At time.Time `json:"at"`
	// The machine credential an approved machine login minted; null otherwise.
	CredentialID *string `json:"credential_id"`
}

// RecordResponse is a credential-request record as POST /v1/machine-logins/lookup answers it.
type RecordResponse struct {
	// The record's id: the SHA-256 of its text.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// "pending", or how it ended: "approved", "denied", "expired", "cancelled" or "revoked".
	State string `json:"state"`
	// The one login that may decide it, or "anyone": any signed-in person may decide a request
	// for a shared human-tier secret, and may deny a service's machine login (which only a broker
	// before token logins opened, and which nobody approves any more), whatever person it names.
	Approver string `json:"approver"`
	// The session asking; null for a machine login.
	Enrollment *RecordEnrollmentResp `json:"enrollment"`
	// The session this record names, read independently of each other; null fields where that id
	// was never set (LEGION-587).
	Session RecordSessionResp `json:"session"`
	// The secrets asked for, or the machine logging in.
	Identifiers []string `json:"identifiers"`
	// The service a machine login is for, when it logs a service in rather than a person's
	// machine; null otherwise.
	Service *string `json:"service"`
	// The reason the session gave, verbatim.
	Reason string `json:"reason"`
	// How many seconds the grant or credential lasts once approved; null for a grant that lasts
	// until its session ends. A machine login's credential always names one.
	LifetimeSeconds *int `json:"lifetime_seconds"`
	// The version of the secret policy it was decided under (the SHA-256 of every agent secret's
	// name, owner, tier and ARN); for a machine login, the version current when it was asked.
	RulesVersion string `json:"rules_version"`
	// When it expires undecided.
	ExpiresAt time.Time `json:"expires_at"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// How it was decided; null while nobody has decided it: while it is pending, and while it is
	// expired before the expiry sweep records its expiry.
	Decided *RecordDecisionResp `json:"decided"`
}

// DecisionResponse is what a machine login's approval answers.
type DecisionResponse struct {
	// The machine credential the approval minted.
	CredentialID *string `json:"credential_id"`
	// "approved": decideMachineLogin writes this body for an approval alone
	// (handlers_ui_records.go), and a denial answers StateResponse instead.
	State string `json:"state" enum:"approved"`
}

// StateResponse is what a denial, a cancel or a revoke answers.
type StateResponse struct {
	// The state the call left the request, record or grant in: "denied", "cancelled" or
	// "revoked". Every writer of this body states one of the three as a literal (a denial, a
	// cancel, a grant's revoke and a launcher credential's).
	State string `json:"state" enum:"denied,cancelled,revoked"`
}

// MachineLookupRequest is the body POST /v1/machine-logins/lookup takes, which Dispatch relays
// to the broker unparsed (its own POST /api/v1/credential-requests/machine-lookup).
type MachineLookupRequest struct {
	// The XXXX-XXXX confirmation code the machine shows, as the approver typed it.
	Code string `json:"code" required:"true"`
}
