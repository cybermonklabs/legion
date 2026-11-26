// Package enroll issues key-bound launcher credentials and turns them into live enrollments. A
// launcher (an operator's box, a Kubernetes pod, or the host agent-secrets-helper of Plan B)
// signs its own future requests with the private key whose thumbprint and public JWK a launcher
// credential is minted against — machine.Service.ApplyDecision mints one once a human approves a
// typed-code machine login — and, for a pod, also proves a projected
// service-account token bound to that pod, receiving a leased enrollment keyed by its own signing
// key's thumbprint. proof.Verifier reads enrollments back through Lookup and Replay, and launcher
// credentials back through AuthenticateLauncher (its LookupLauncher hook), to authenticate later
// session and machine proofs. There is no bearer token anywhere in this package: a launcher
// credential authenticates by the same key it was minted against, never a shared secret.
package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

var (
	ErrUnauthenticated  = errors.New("launcher credential is not valid")
	ErrOperatorMismatch = errors.New("this launcher credential enrols a different operator")
	ErrAlreadyEnrolled  = errors.New("this runtime is already enrolled and live")
	ErrPodIdentity      = errors.New("the projected token does not identify this pod")
	// ErrPodTokenExpired is a pod token whose expiry has passed, wrapped with the expiry the token
	// names: the launcher or renewer presents the pod's current token with the same key, where
	// ErrPodIdentity is a token for another pod, or no token of this cluster at all. An expired
	// token is reported as expired whatever else is wrong with it (oidc.Verifier reads a failed
	// token's claims before blaming its signature), so its pod UID, which nothing verified, is
	// never compared.
	ErrPodTokenExpired = errors.New("the projected token has expired")
	ErrNotLive         = errors.New("enrollment is not live")
	ErrInvalidSlot     = errors.New("a slot is only for a pod enrollment and must match ^[a-z][a-z0-9-]{0,62}$")
	ErrNoCredential    = errors.New("no such machine login")
	// ErrNotApprover refuses a revoke of a machine login by anyone the rule does not admit: a
	// person's machine login is the person's who approved it, and a person's pod login is the
	// person's that BROKER_POD_OPERATORS maps the pod's account to, although it carries a service.
	ErrNotApprover = errors.New("only the person who approved a person's machine login may revoke it, and only the person BROKER_POD_OPERATORS maps a pod's account to may revoke that pod's login")
	// ErrInvalidClaim refuses a claim stated on an enrollment that is not a pod's, a claim that
	// does not match ^[a-z][a-z0-9-]{0,127}$, and a claim from a pod enrolling itself, which may
	// state no more of whose it is than it may state a session name (ErrPodSelfEnrollNoSession
	// says why). Nothing is written.
	ErrInvalidClaim = errors.New("a claim is only for a pod a launcher enrols and must match ^[a-z][a-z0-9-]{0,127}$")
	// ErrNotEnrollingCredential refuses a revoke of an enrollment by any launcher credential but
	// the one that made it (Revoke). It holds for the enrollments that carry no operator, a
	// service's pod's or a machine's own session. It holds too for the pod enrollments that a
	// person's pod login makes (PersonPodLogin), which carry that person as their operator.
	ErrNotEnrollingCredential = errors.New("only the machine login that made this enrollment may end it")
	ErrPodTokenRequired       = errors.New("a pod enrollment's renewal must carry its projected token")
	// ErrPodSelfEnrollOnly refuses a pod enrollment under the enrolling pod's own token login —
	// one whose subject is the very ServiceAccount the presented pod token proves — whose runtime
	// id names another pod. Such a credential belongs to one pod, the pod whose token logged in
	// (launcher_credentials.pod_uid), so it enrols that pod and nothing else, and under no session
	// name (ErrPodSelfEnrollNoSession, which says why). A person's pod login (PersonPodLogin)
	// enrols its own pod alone too. It also gets this refusal for a pod of any other account,
	// and that refusal names the login's account and the pod's account.
	ErrPodSelfEnrollOnly = errors.New("a pod's own machine login enrols that pod alone")
	// ErrPodSelfEnrollNoSession refuses a session name on the enrolment ErrPodSelfEnrollOnly holds
	// to one pod: a pod enrolling itself under its own token login. Every pod of one account
	// enrolling itself enrols under one principal (carryPrincipal: the account, paired with that
	// same account as its launcher), so a name such a pod picked for itself is a name a sibling
	// pod of the account holds, and this enrollment would take that sibling's grants and pending
	// requests off it while the sibling is still running (adopt). A pod enrolling itself registers
	// no agent session and needs no name: `agent-secrets enroll --pod` refuses --session-id by
	// name. A launcher's enrolment of a pod it launches is another case and states a session name
	// as before.
	ErrPodSelfEnrollNoSession = errors.New("a pod enrolling itself states no session name")
	// ErrPodNotLaunched refuses a pod enrollment under a token login that neither belongs to that
	// pod nor launches the pods of the account its token proves: BROKER_SERVICES does not bind
	// that account to the login's service. Nothing but the deployment's own settings says a
	// service launches an account's pods, and a token login is authorized by those settings
	// alone, so a token it was handed proves whose pod it is and never who may enrol it. A
	// service's login a person approved carries no subject and keeps that person's approval as
	// its authorization, so this never reaches one. A person's pod login (PersonPodLogin) never
	// reaches it either: it enrols its own pod alone and never reads BROKER_SERVICES.
	ErrPodNotLaunched = errors.New("this machine login does not launch the pods of that service account")
	// ErrMachineLoginEnded refuses a machine's own session a renewal because the launcher
	// credential its exchange minted is no longer live: revoked, past the expiry its Google
	// identity token gave it, or its account no longer listed for its service (Renew).
	ErrMachineLoginEnded = errors.New("the machine login that enrolled this session has ended")
	// ErrSessionHeld refuses an enrollment whose session name another principal holds and the
	// enrollment's own principal may not take, read from the one table that says who holds a name
	// — the same one the token exchange binds every token's name in (oauthstore.Store.BindSession,
	// oauthstore.ErrSessionHeld). An account paired with the holder (SessionBinder.PairedWith)
	// takes the name rather than being refused it, unless the configuration fixes the name; every
	// other principal is refused, an account paired only with the holder's own pair among them,
	// since a pair does not chain. Its text names the session and says what to do about it, as
	// that refusal's own text and the exchange's do, and names no holder: whoever holds the name
	// is not this caller's to learn.
	ErrSessionHeld = errors.New("another identity holds this session name")
	// ErrKeyBindsReadToken refuses a launcher credential minted or moved onto a key a live read
	// token is bound to (keyBindsReadToken): a key that signs launcher proofs is bound to no
	// read token, and a read token's key signs no launcher proof. Nothing is written. KeyHeld does
	// not report it: each caller answers it with a refusal of its own, which names the read token
	// and the cure, a new key.
	ErrKeyBindsReadToken = errors.New("a live read token is bound to this key, and a key that signs launcher proofs binds no read token")
	// ErrRestrictedMember refuses an enrollment that would act for a person Dispatch calls a
	// restricted member (projectaccess.MemberRestricted): a person an access list admitted from
	// outside the deployment's sign-in domain. Such a person's machine login enrolls no session,
	// so none of their agents is released a secret; their login still trades for a token that
	// names their projects, which is all their agents need. Nothing is written.
	ErrRestrictedMember = errors.New("a restricted member's machine login enrolls no session, and the broker releases no secret to that person's agents")
	// ErrProjectsInvalid refuses a list of projects that is no credential's list
	// (oauthwire.ValidProjects): more than 16 keys, a key that is no Dispatch project key, or one
	// named twice.
	ErrProjectsInvalid = errors.New("projects is a list of at most 16 Dispatch project keys, each once")
	// ErrProjectsNeedAPerson refuses projects named by an enrollment that acts for no person: a
	// service's pod, whose project is the one its operator's entry maps its account to
	// (exchange.Set.MappedProjects), and a machine's own session. A list is what a person's
	// session asks for, and a person's pod (PersonPodLogin) too, which acts for its person; only a
	// person can be asked whether they may see a project.
	ErrProjectsNeedAPerson = errors.New("only a session or a pod that acts for a person names projects")
	// ErrProjectsUnchecked refuses a session naming projects that the broker cannot check with
	// Dispatch: it holds no client of Dispatch's project access (BROKER_PROJECT_ACCESS_URL), or
	// Dispatch did not answer. A list taken unchecked could name a project its person may not see,
	// so the enrollment is refused rather than made; a session naming no project enrols as before.
	ErrProjectsUnchecked = errors.New("the broker could not check the session's projects with Dispatch")
	// ErrProjectNotVisible refuses a session naming a project its person may not see, or one
	// Dispatch does not hold: the two read alike, so a refusal never says whether a project exists.
	ErrProjectNotVisible = errors.New("the session names a project its person cannot see")
	// ErrProjectsRequired refuses a session under the sign-in of a person Dispatch calls a
	// restricted member that names no project: such a person sees only the projects that name
	// them, so their session names which (projectaccess.ErrListRequired). A restricted member's
	// machine login and pod login never reach it: they enroll nothing at all
	// (ErrRestrictedMember). `agent-secrets register` and `enroll --helper` each read
	// AGENT_SECRETS_PROJECTS, so the one setting the refusal names is the one each of them reads.
	ErrProjectsRequired = errors.New("a restricted member's session names its projects: set AGENT_SECRETS_PROJECTS to the projects this session works on")
)

// KeyHeld reports whether err refuses a launcher credential's key because another live
// credential holds it (a unique violation, store.IsUniqueViolation). A key a live read token is
// bound to is ErrKeyBindsReadToken, which KeyHeld leaves to the caller: the two refusals name
// different holders and different cures.
func KeyHeld(err error) bool {
	return store.IsUniqueViolation(err)
}

type Credential struct {
	ID       uuid.UUID
	Operator *string
	Service  *string
	Host     string
	// Subject is the identity a token login proved (TokenLogin.Subject): a Kubernetes
	// ServiceAccount, or a machine as the token exchange names it (exchange.MachineSubject); nil
	// for every login a person approved.
	Subject *string
	// PodUID is the pod whose token a Kubernetes ServiceAccount's login presented
	// (AccountClaims.PodUID, launcher_credentials.pod_uid): the one pod that credential belongs
	// to. "" for a token bound to no pod, such as one `kubectl create token` mints, for a VM's
	// login, and for every login a person approved.
	PodUID string
}

// LiveCredential is one of the machine logins a person may revoke, as their machine-login page
// lists it: a launcher credential of one of their own machines, of a pod login of theirs (a token
// login of an account that BROKER_POD_OPERATORS maps to them), or of any service's login whoever
// approved it, not revoked, and either unexpired or still holding a live session it enrolled
// (Lookup's live: not revoked, its lease not lapsed).
type LiveCredential struct {
	ID   uuid.UUID
	Host string
	// Service names the service a service's login is for (legion-daemon), or the login service
	// of a person's pod login. It is nil for a person's own machine.
	Service *string
	// Subject is what a token login proved: its ServiceAccount
	// (system:serviceaccount:<namespace>:<name>), or a machine as the token exchange names it
	// (exchange.MachineSubject); nil for every login a person approved.
	Subject *string
	// Person is the person a person's pod login acts for: the person BROKER_POD_OPERATORS maps
	// the login's subject to now, which is the person whose list holds it. It is nil for every
	// other login. liveCredentials sets it from the setting after the query, so the query reads
	// no column for it.
	Person *string `db:"-"`
	// ApprovedBy is the login of the person who approved it, "" for every token login, a person's
	// pod login among them, which no person approves.
	ApprovedBy string
	IssuedAt   time.Time
	ExpiresAt  time.Time
	// Expired is true once ExpiresAt has passed: the login enrols no more sessions, and the ones
	// it enrolled renew with their own keys and still run — every one but a machine's own
	// session, whose renewal is held to its login being live (Renew) — so it stays listed until
	// they end or lapse, or it is revoked.
	Expired bool
}

type Enrollment struct {
	ID         uuid.UUID
	Kind       string
	RuntimeID  string
	Operator   *string
	Thumbprint string
	SessionID  *string
	PodToken   string
	// Slot names one of several independent identities in one pod (record.ValidSlot): each slot
	// of a pod is an enrollment of its own, with its own key, lease and grants. "" is the one
	// identity of every session and single-identity pod enrollment.
	Slot string
	// Subject is a pod enrollment's verified service-account subject, set by Create from the
	// token itself and never from the caller; nil for a session.
	Subject      *string
	LeaseExpires time.Time
	Existing     bool // true when Create returned an already-live enrollment for the same key
	// Projects are the Dispatch projects the session names, sorted and each once
	// (oauthwire.ParseProjects), nil for none: the projects whose private secrets the broker may
	// release to it (requests.Machine). Create takes a list only from a session that acts for a
	// person, or a person's pod (PersonPodLogin), and only when Dispatch says that person may see
	// every project on it; the list is fixed for the enrollment's life, and an adoption never
	// carries it to a successor.
	Projects []string
	// Claim is what a pod's launcher states the pod runs for, kept as enrollments.claim: for the
	// Legion daemon, the role claim token (legion-<project>-<issue>-<role>), which every role
	// generation and every replacement pod of that role states again. "" states none. It is only
	// for a pod a launcher enrols (ErrInvalidClaim), and it is what a pod's owner is read from
	// (ownerKey).
	Claim string
	// carry is the principal the enrollment's grants follow, as enrollments.carry_principal
	// records it, and name the principal its session name is held under (bindSessionName). The
	// two differ for a pod alone, whose grants follow the pair of its account and the login that
	// enrolled it while its name is held under its account (carryPrincipal, namePrincipal).
	// Create works both out from what the caller proved and nothing reads them back off this
	// struct, so they are the package's own and never a caller's to state.
	carry *string
	name  *string
	// launcher is the account the token login enrolling a pod proved (Credential.Subject), nil off
	// a pod and for a login that proved none. adopt reads it twice: to spell each paired account's
	// pair under the same login account (exchange.LaunchedBy), and to take the same daemon's rows
	// an older broker wrote under the account alone.
	launcher *string
	// owner is the principal a request this enrollment makes belongs to, as enrollments.owner
	// records it (ownerKey): Create works it out from what the caller proved and from the claim a
	// launcher stated, so, like carry, it is never a caller's to state.
	owner *string
}

type PodClaims struct {
	Subject string
	PodUID  string
}

// PodVerifier verifies the cluster's projected ServiceAccount tokens (K8sPodVerifier): Verify a
// pod's own token, bound to its pod, for enrollment and renewal; VerifyAccount a service's token,
// bound to a pod or to none, for its token login (machine.Service.TokenLogin).
type PodVerifier interface {
	Verify(ctx context.Context, raw string) (PodClaims, error)
	VerifyAccount(ctx context.Context, raw string) (AccountClaims, error)
}

// SessionBinder binds a session name to the principal that holds it and refuses it to every other
// principal but an account paired with the holder, which takes it (PairedWith):
// oauthstore.Store, whose BindSession the token exchange binds the name of every token it mints
// with. BindSession answers the principal it took the name from, "" when it took it from nobody.
// It is an interface here for the same reason PodVerifier is one — so a test wires a binder of
// its own — and it joins the transaction the context carries (oauthstore.WithTx), which is how an
// enrollment's bind and its own rows commit or roll back together.
type SessionBinder interface {
	BindSession(ctx context.Context, session, principal string, fixed bool, expires time.Time) (string, error)
	// PairedWith is every principal paired with principal (oauthstore.Store.SetSessionPairs,
	// which writes each pair both ways): each takes a session name principal holds, and principal
	// takes one any of them holds. Pairs are of name principals (namePrincipal), so adopt's session
	// rule matches each paired account's enrollments of the same name beside the enrollment's own,
	// a pod's under the same login account alone (adopt, exchange.LaunchedBy). It is nil for a
	// principal in no pair, and the caller does not write to the slice.
	PairedWith(principal string) []string
}

type Service struct {
	Store *store.Store
	Lease time.Duration
	// Carry is BROKER_SESSION_CARRY_SECONDS: how long a dormant enrollment holds what it held for
	// the next enrollment of its session name (endEnrollment) before the carry sweep ends it for
	// good (EndCarried), and so the end of the window Carried answers for a dormant row. One field
	// for both, so the window the sweep keeps and the window a launcher is told of are one
	// window. 0 is no carry: the next sweep ends every dormant enrollment it finds.
	Carry time.Duration
	// Pod verifies the cluster's projected ServiceAccount tokens against BROKER_K8S_OIDC_ISSUER and
	// BROKER_K8S_OIDC_AUDIENCE; nil on a broker that trusts no Kubernetes token issuer, which enrolls
	// no pod and serves no Kubernetes token login.
	Pod PodVerifier

	// Projects asks Dispatch whether a person may see a project and what kind of member they are
	// (projectaccess.Client, BROKER_PROJECT_ACCESS_URL). Create asks it, before its transaction
	// begins, first of the person every enrollment under a person's machine login or pod login acts
	// for, refusing a restricted member (ErrRestrictedMember), and then checks a session's list
	// through it. nil on a broker with no such client, which asks nothing, enrolls a session naming
	// no project as before and refuses one that names any (ErrProjectsUnchecked).
	Projects projectaccess.Access

	// Sessions binds every enrollment that names a session to the principal its name is held
	// under (namePrincipal), in the enrollment's own transaction, and refuses it a name another
	// principal holds (Create, ErrSessionHeld), unless that principal is an account paired with
	// the enrollment's own, from which the enrollment takes the name (SessionBinder.PairedWith).
	// It is the broker's one record of which principal holds a session name,
	// oauth_session_bindings, written here and by the token exchange alike, so the name a session
	// writes its Dispatch tokens under and the name its grant follows have the same holder: for a
	// pod that holder is its account, which the pair its grant follows begins with
	// (carryPrincipal). Its pairs are also the accounts beside the enrollment's own whose
	// enrollments of the same name adopt's session rule takes from.
	//
	// nil on a broker that binds nothing: such a broker writes no binding, refuses no
	// enrollment its name, pairs nothing, and so lets two principals enrol one name, each
	// adopting nothing of the other's (adopt). cmd/cipherd/main.go wires the real
	// oauthstore.Store.
	Sessions SessionBinder

	// Chain re-verifies a launcher credential's issuance chain on every AuthenticateLauncher
	// call: a launcher_credentials row is never trusted on its own, since it must still trace
	// back to a genuinely signed, human-approved credential-request record. NewChainVerifier
	// builds one against this same Store.
	Chain *record.ChainVerifier

	// ServiceLogins is BROKER_SERVICE_LOGINS: each ServiceAccount subject that logs in as a service
	// with its token, answering that service. AuthenticateLauncher reads it for every credential a
	// Kubernetes token login issued.
	ServiceLogins map[string]string
	// PodOperators is BROKER_POD_OPERATORS: each Kubernetes ServiceAccount subject that the
	// deployment maps to a person, answering that person's login (record.CanonicalLogin).
	// machine.Service.TokenLogin reads it at each login of a mapped account from a pod-bound
	// token, and writes the person as the credential's operator. That credential is a person's
	// pod login (PersonPodLogin).
	//
	// This package reads the setting as it is now, and not the person that a row recorded.
	// Create and Revoke admit a person's pod login only while the setting still maps its subject
	// to its operator (livePersonPodLogin). LiveCredentials and RevokeCredential give each
	// credential of a mapped subject to that person alone, whatever its row's operator. Unset,
	// the setting maps no account.
	PodOperators map[string]string
	// ServicesByAccount is BROKER_SERVICES: each ServiceAccount a service's pods run as, answering
	// that service. It is the deployment's own statement of which pods a service launches, and so
	// of which pods a service's token login may enrol besides its own (Create, ErrPodNotLaunched);
	// requests.Machine reads the same setting to decide whether an enrolled pod is that service's.
	// Unset, a token login enrols its own pod alone.
	ServicesByAccount map[string]string
	// VMServicesByAccount is BROKER_VM_SERVICES, keyed by a Google service account's email: the
	// service the machines of that account are listed as. AuthenticateLauncher reads it for
	// every credential the token exchange minted for a machine, which keeps that credential
	// authenticating only while its account is still listed.
	VMServicesByAccount map[string]string

	// testConflictHook, when set, runs once a 23505 insert conflict is detected in createAttempt,
	// before the recovery lookup. It exists only so a test can deterministically reproduce the
	// race where the conflicting row is revoked between the failed insert and that lookup; no
	// production caller sets it.
	testConflictHook func()

	// testSchemaReadHook, when set, runs in IssueTokenLoginTx between the schema read that
	// chooses the conflict target (perPodLogins) and the upsert that names it. It exists only
	// so a test can deterministically reproduce the race where the next release's migration
	// drops the index that read saw; no production caller sets it.
	testSchemaReadHook func()
}

// execer is satisfied by both *pgxpool.Pool (via Store.Pool) and pgx.Tx, so Replay can run either
// standalone or joined to a caller's own transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// mintLauncherCredential inserts a launcher credential bound to a signing key — never a bearer
// token: a launcher's own key (thumbprint, embedded JWK) is exactly what a later
// proof.SignLauncher proof presents, and exactly what AuthenticateLauncher's issuance-chain
// re-verification checks against recordID's approved credential-request record. recordID may be
// "" only for a credential with no backing record at all (test fixtures unrelated to the
// machine-login flow); AuthenticateLauncher refuses such a credential outright. approver is the
// account that approved it (migration 0046), which a revoke of that account, once it no longer
// holds the operator's address, ends it by; the zero value records none, as an approval an
// older Dispatch relayed carries none.
//
// It runs on db, which is the caller's own transaction — machine.Service.ApplyDecision's own
// approval commit, so a crash between minting and recording its decision rolls back both together
// instead of leaving an orphaned, unrecoverable credential — or, for a test fixture, the pool. It
// refuses a key a live read token is bound to first (keyBindsReadToken).
func (s *Service) mintLauncherCredential(ctx context.Context, db oauthstore.Querier, operator, service *string, host, thumbprint string, jwk json.RawMessage, recordID string, expires time.Time, approver store.IssuerSubject) (uuid.UUID, error) {
	if operator != nil {
		operator = new(record.CanonicalLogin(*operator))
	}
	if err := keyBindsReadToken(ctx, db, thumbprint); err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	_, err := db.Exec(ctx, `insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, expires_at, approver_issuer, approver_subject)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, operator, service, host, thumbprint, []byte(jwk), nullable(recordID), expires, nullable(approver.Issuer), nullable(approver.Subject))
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// keyBindsReadToken refuses, on db, a launcher credential minted or moved onto thumbprint while a
// live read token is bound to it (oauthstore.KeyHoldsReadToken, which takes the key's advisory
// lock first, the lock oauthstore.Store.CreateAccessTokenSession takes before it stores a read
// token, so of the two writers binding one key in transactions at once only one commits). It is
// the check in both statements that write launcher_credentials.key_thumbprint, so every path that
// reaches them is refused: a device login's approval, the token login route, a Google machine's
// trade and a person's.
//
// The refusal is ErrKeyBindsReadToken, naming the key. Each caller answers it with a refusal of
// its own beside the one it gives a key another live credential holds (KeyHeld):
// machine.ErrKeyBindsReadToken (409 KEY_BINDS_READ_TOKEN) for an approval and a token login, and
// oauth.ErrMachineKeyBindsReadToken (invalid_grant, with a hint of its own) for a trade.
func keyBindsReadToken(ctx context.Context, db oauthstore.Querier, thumbprint string) error {
	held, err := oauthstore.KeyHoldsReadToken(ctx, db, thumbprint)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("%w: the key %s", ErrKeyBindsReadToken, thumbprint)
	}
	return nil
}

// MintLauncherCredentialTx mints a person's launcher credential, operating operator's machine and
// approved by the account approver, inside tx, joined to the caller's own transaction —
// machine.Service.ApplyDecision's own approval commit.
func (s *Service) MintLauncherCredentialTx(ctx context.Context, tx pgx.Tx, operator, host, thumbprint string, jwk json.RawMessage, recordID string, expires time.Time, approver store.IssuerSubject) (uuid.UUID, error) {
	return s.mintLauncherCredential(ctx, tx, &operator, nil, host, thumbprint, jwk, recordID, expires, approver)
}

// TokenLogin is one verified token login, as IssueTokenLogin records it: the identity the token
// proved (a ServiceAccount subject, a machine as the token exchange names it,
// exchange.MachineSubject, or a person's sign-in on a machine, exchange.PersonSubject), the
// service BROKER_SERVICE_LOGINS or BROKER_VM_SERVICES maps it to,
// the person its enrollments act for (Operator), the host the credential names (a ServiceAccount's
// <namespace>/<name>, a machine's <project>/<zone>/<instance name>), the pod its token is bound
// to ("" for none) and when the token expires, the key the request object or the DPoP proof
// proved (its thumbprint and public JWK), and when the credential expires.
type TokenLogin struct {
	Subject, Service, Host, PodUID string
	// Operator is the person the credential's enrollments run for. Two logins set it, and no
	// other login does:
	//
	//   - A person's sign-in on a machine. Its sessions are that person's sessions, exactly as a
	//     dev box's machine login makes them (authorized).
	//   - A Kubernetes token login of an account that BROKER_POD_OPERATORS maps, from a token
	//     bound to a pod (machine.Service.TokenLogin). The credential is then a person's pod
	//     login (PersonPodLogin). It enrols its own pod alone, and that pod acts as the person.
	//
	// A machine's own credential and every other Kubernetes login act for no person. Their
	// enrollments reach their service's secrets alone.
	Operator     string
	TokenExpires time.Time
	Thumbprint   string
	JWK          json.RawMessage
	Expires      time.Time
}

// IssueTokenLogin issues or moves the launcher credential of a token login, which no person
// approves, in a transaction of its own: machine.Service.TokenLogin verified everything in
// login and has none to join.
func (s *Service) IssueTokenLogin(ctx context.Context, login TokenLogin) (uuid.UUID, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	id, err := s.IssueTokenLoginTx(ctx, tx, login)
	if err != nil {
		return uuid.Nil, err
	}
	return id, tx.Commit(ctx)
}

// IssueTokenLoginTx issues or moves the launcher credential of a token login inside tx, joined
// to the caller's own transaction — the RFC 8693 exchange's, which must write the credential
// and the access token that names it together or not at all
// (oauth.ExchangeConfig.MachineCredentials).
//
// A machine login becomes per pod rather than per account over two releases, and this reads
// which of the two schemas it is on and names the conflict target that schema has, so both
// generations of the broker are right on either while a roll serves them together
// (migration 0029, and legacySubjectIndex below).
//
// While 0015's launcher_credentials_live_subject is still there — this release — one subject
// holds one live credential, and every login of that subject moves it: that is what the previous
// broker does, and what the previous broker's own upsert needs the index for. The login's pod is
// written to the row all the same (migration 0028), which is what lets a pod's own login enrol
// that pod and no other (podUnderTokenLogin).
//
// Once the release after this one has dropped that index, a login a pod's own projected token
// made belongs to that pod, so every pod of an account may hold one at the same time, each with
// its own key, and a login from a token bound to no pod — a launcher's, which names no pod of
// its own, a machine's and a person's sign-in — is its subject's one such credential. A pod
// whose credential predates that release carries no pod on its row, so the per-pod conflict
// target cannot match it; such a pod claims that row for itself on its next login, by its
// subject and the key the login signed with, and keeps its id, rather than being refused its own
// key for as long as the row lives.
//
// Either way the credential is created at its first login and each later login of the same pod,
// or of the same subject, moves it to that login's key, person and expiry under the same id, so
// a login proving the key the credential already holds changes only its expiry and every session
// enrolled under it stays there. A machine's subject names that one machine
// (exchange.MachineSubject) and a sign-in's names one person on one box
// (exchange.PersonSubject), so each machine of a fleet, and each person signed in on each of its
// boxes, holds a credential of its own and no login moves another's. Its host and its pod are
// login's. Each login writes a launcher_credential.token_login audit row naming the subject, the
// pod the token is bound to, the key, and both expiries. A key another live credential holds is
// a unique violation (store.IsUniqueViolation), and a key a live read token is bound to is
// ErrKeyBindsReadToken (keyBindsReadToken); either way nothing is written.
func (s *Service) IssueTokenLoginTx(ctx context.Context, tx pgx.Tx, login TokenLogin) (uuid.UUID, error) {
	var operator *string
	if login.Operator != "" {
		operator = new(record.CanonicalLogin(login.Operator))
	}
	// The schema read below and the statement that uses its answer must see one schema. `drop
	// index` takes ACCESS EXCLUSIVE on the table, and this transaction has touched nothing yet,
	// so without a lock of its own the next release's drop lands between the two and the upsert
	// names a conflict target the schema no longer has: 42P10, and a refused login. ROW
	// EXCLUSIVE is the lock the insert takes anyway and is held to commit either way. It
	// conflicts with ACCESS EXCLUSIVE, so the drop waits for a login in flight, or the login
	// waits for the drop and then reads the schema the drop left; and it does not conflict with
	// itself, so two logins never wait for each other.
	if _, err := tx.Exec(ctx, `lock table launcher_credentials in row exclusive mode`); err != nil {
		return uuid.Nil, err
	}
	if err := keyBindsReadToken(ctx, tx, login.Thumbprint); err != nil {
		return uuid.Nil, err
	}
	// Postgres resolves one conflict target against one index, so which index the schema has
	// decides which target this names. perPodLogins is false while 0015's index is still there,
	// and the statement is then the previous broker's exactly.
	perPod, err := perPodLogins(ctx, tx)
	if err != nil {
		return uuid.Nil, err
	}
	if s.testSchemaReadHook != nil {
		s.testSchemaReadHook()
	}
	conflict := `(subject) where subject is not null and revoked_at is null`
	switch {
	case !perPod:
	case login.PodUID == "":
		conflict = `(subject) where subject is not null and pod_uid is null and revoked_at is null`
	default:
		conflict = `(subject, pod_uid) where subject is not null and pod_uid is not null and revoked_at is null`
		// Migration 0028 added pod_uid empty, so the credential a pod's token made before
		// the per-pod schema is a live row of that subject carrying no pod, which the
		// conflict target just named cannot match. Left alone, the insert below would
		// collide with that row on launcher_credentials_live_key -- the key is the one the
		// running daemon is renewing with -- and be refused 409 KEY_HOLDS_LIVE_CREDENTIAL
		// for as long as the row exists, which expiry does not end, since that index covers
		// expired rows too. So the pod the token just proved claims the row first, found by
		// its subject and by the very key this login signed with: only the holder of that
		// key can, and the holder is the pod that made it. The row keeps its id, so the
		// upsert then finds it through (subject, pod_uid) and moves it as any later login of
		// that pod, and every session enrolled under it stays there. Nothing is claimed when
		// the pod already holds a live row of its own -- a pod presenting a second key --
		// which would be a (subject, pod_uid) collision of its own; the upsert decides that
		// login as it decides any other whose key a live credential holds.
		if _, err := tx.Exec(ctx, `update launcher_credentials set pod_uid=$1
			where subject=$2 and key_thumbprint=$3 and pod_uid is null and revoked_at is null
			  and not exists (select 1 from launcher_credentials held
				where held.subject=$2 and held.pod_uid=$1 and held.revoked_at is null)`,
			login.PodUID, login.Subject, login.Thumbprint); err != nil {
			return uuid.Nil, err
		}
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into launcher_credentials (id, operator, service, host, key_thumbprint, public_jwk, record_id, subject, pod_uid, expires_at)
		values ($1, $2, $3, $4, $5, $6, null, $7, nullif($8::text,''), $9)
		on conflict `+conflict+`
		do update set operator=excluded.operator, service=excluded.service, host=excluded.host,
			key_thumbprint=excluded.key_thumbprint, public_jwk=excluded.public_jwk,
			pod_uid=excluded.pod_uid, expires_at=excluded.expires_at
		returning id`,
		uuid.New(), operator, login.Service, login.Host, login.Thumbprint, []byte(login.JWK), login.Subject, login.PodUID, login.Expires).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.token_login',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'service',$3::text,'subject',$4::text,'pod_uid',nullif($5::text,''),
			'operator',nullif($6::text,''),'key_thumbprint',$7::text,'expires_at',$8::timestamptz,'token_expires_at',$9::timestamptz)))`,
		record.LauncherActor(id.String()), id.String(), login.Service, login.Subject, login.PodUID, login.Operator,
		login.Thumbprint, login.Expires, login.TokenExpires); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// legacySubjectIndex is 0015's one-live-credential-per-subject index, which migration 0029
// deliberately leaves in place and the release after it drops. While it exists the previous
// broker is still able to serve: its upsert names `on conflict (subject) where subject is not
// null and revoked_at is null`, which Postgres resolves against this index and against neither
// index 0029 adds, and a roll of the broker serves both generations at once.
const legacySubjectIndex = "launcher_credentials_live_subject"

// perPodLogins reports whether the schema tx runs on gives each pod of a subject a launcher
// credential of its own, which it does once legacySubjectIndex is gone: that index allows one
// live credential per subject whatever its pod, so while it is there a second pod's row cannot
// exist at all and naming a per-pod conflict target would only turn that into a unique
// violation. It is read per login rather than once at start because the index goes while this
// process is running — the next release's own pod migrates the database at its boot, with this
// one still serving — and a cached answer would name a conflict target the schema no longer has
// and fail with 42P10, which is the failure this whole staging exists to avoid. One catalog
// lookup on a route a service reaches a few times an hour.
//
// Reading it per login is not on its own enough: the drop could still land between this read
// and the statement that uses its answer. What makes the answer hold is the caller's lock on
// launcher_credentials, taken before this and held to commit (IssueTokenLoginTx), which `drop
// index` waits for. This answers only what the lock already holds still.
func perPodLogins(ctx context.Context, tx pgx.Tx) (bool, error) {
	var legacy bool
	if err := tx.QueryRow(ctx, `select to_regclass($1) is not null`, legacySubjectIndex).Scan(&legacy); err != nil {
		return false, err
	}
	return !legacy, nil
}

// NewChainVerifier builds the record.ChainVerifier AuthenticateLauncher's issuance-chain
// re-verification uses, scoped to launcher_credential records.
func NewChainVerifier(st *store.Store, audience string, skew time.Duration) *record.ChainVerifier {
	return st.ChainVerifier(record.KindLauncherCredential, audience, skew)
}

// AuthenticateLauncher answers proof.Verifier's LookupLauncher hook directly: given a launcher
// credential's own id (the "lid" claim of a proof.SignLauncher proof), it resolves the live,
// unexpired credential row and checks what vouches for it. A credential a person approved has its
// entire issuance chain re-verified through s.Chain — a launcher_credentials row is never trusted
// on its own, so a row inserted without a genuine approved credential-request record behind it
// never authenticates. A credential a token login issued (IssueTokenLoginTx, its subject set,
// and so its service, as the table's check constraint holds) authenticates while its subject is
// still listed for its service (tokenLoginListed), so removing an account from
// BROKER_SERVICE_LOGINS or BROKER_VM_SERVICES ends its credential at its next call. A missing,
// expired, revoked, chain-broken or no-longer-listed credential all answer ("", false, nil); only a
// genuine dependency failure is a non-nil error, matching Lookup's own contract.
func (s *Service) AuthenticateLauncher(ctx context.Context, lid string) (string, bool, error) {
	if _, err := uuid.Parse(lid); err != nil {
		return "", false, nil
	}
	var thumbprint string
	var recordID, service, subject *string
	var expiresAt time.Time
	err := s.Store.Pool.QueryRow(ctx, `select key_thumbprint, record_id, service, subject, expires_at from launcher_credentials where id=$1 and revoked_at is null`, lid).
		Scan(&thumbprint, &recordID, &service, &subject, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !expiresAt.After(time.Now()) {
		return "", false, nil
	}
	if subject != nil {
		if !s.tokenLoginListed(*subject, *service) {
			return "", false, nil
		}
		return thumbprint, true, nil
	}
	if recordID == nil {
		return "", false, nil
	}
	if _, err := s.Chain.Verify(ctx, *recordID); err != nil {
		if errors.Is(err, record.ErrChainBroken) {
			return "", false, nil
		}
		return "", false, err
	}
	return thumbprint, true, nil
}

// tokenLoginListed reports whether a token login's subject still logs in as service: a machine
// and a person's sign-in on one while BROKER_VM_SERVICES still lists that box's Google account
// for that service, and a Kubernetes ServiceAccount while BROKER_SERVICE_LOGINS names it.
// Which of the three a subject is, and the account a machine's and a sign-in's carry, are one
// reading (exchange.ReadSubject).
//
// A machine's own test is the account alone, because the account is what a listed identity is:
// where the machine runs, and which of two machines of one account this is, were decided by the
// entry when the exchange minted the credential (exchange.Set.Google), and the subject keeps
// only enough to tell the machines apart (exchange.MachineSubject). So dropping an account from
// BROKER_VM_SERVICES ends every one of its machines' credentials at their next call, and
// narrowing an entry's pins takes effect at the next exchange, where the pins are read.
//
// A sign-in's subject carries the same account (exchange.PersonSubject), so the same test holds
// it to the same listed fleet: a person signs in from a box the deployment lists, and dropping
// the account ends their sign-in's credential with the box's own. What ends a sign-in the
// person has revoked is not this but the revoke itself, which ends that credential in the
// transaction it revokes the sign-in in (RevokePersonSignIn): "listed" is a question about the
// configuration, and a revoke is a fact about the row.
func (s *Service) tokenLoginListed(subject, service string) bool {
	if read := exchange.ReadSubject(subject); read.Kind != exchange.SubjectOther {
		listed, isListed := s.VMServicesByAccount[read.Account]
		return isListed && listed == service
	}
	listed, found := s.ServiceLogins[subject]
	return found && listed == service
}

// CredentialForKey loads a launcher credential's own identity (operator, service, host, and a
// token login's subject) for a caller a launcher proof authenticated: it reads the row by
// its id and the key the proof proved together (proof.Subject's LauncherID and Thumbprint, the
// pair Verify compared), and answers ErrNoCredential when no row holds both — the error every
// other read of a launcher credential by something that does not name one answers
// (revokeCredential, RetireCredential).
//
// The pair is what makes the handler's credential the proved key's. A login upserts the
// credential row and writes the login's own key_thumbprint and pod_uid onto it
// (IssueTokenLoginTx), so a login landing between the proof's own lookup (AuthenticateLauncher)
// and this read moves the row out from under the proof: a read by launcher id alone would hand
// the caller whatever identity the row then holds, and Revoke's own-account branch would judge
// the request as the pod that row names. A login bound to no pod takes one row per subject on
// either schema (IssueTokenLoginTx's pod-less conflict target), so another pod-less login of
// that subject still moves this caller's row, and a pod's own re-login moves its own row's key
// under the same id, so the pair is right on either schema. A row whose key has moved is no
// credential of this caller's, and the route refuses it (401 LAUNCHER_INVALID) rather than
// acting under another pod's identity.
func (s *Service) CredentialForKey(ctx context.Context, id, thumbprint string) (Credential, error) {
	var cred Credential
	err := s.Store.Pool.QueryRow(ctx, `select id, operator, service, host, subject, coalesce(pod_uid,'') from launcher_credentials where id=$1 and key_thumbprint=$2`, id, thumbprint).
		Scan(&cred.ID, &cred.Operator, &cred.Service, &cred.Host, &cred.Subject, &cred.PodUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNoCredential
	}
	if err != nil {
		return Credential{}, err
	}
	return cred, nil
}

// Create enrolls in under cred. An enrollment's operator is its credential's, recorded whether or
// not the launcher states one. For a person's machine login it is the email of the person who
// approved that login (machine.Service.ApplyDecision). For a person's pod login (PersonPodLogin)
// it is the person that BROKER_POD_OPERATORS mapped the pod's account to when the pod logged in.
// A stated operator that names anyone else, or any operator stated under a service credential, is
// ErrOperatorMismatch. A live enrollment is unique per
// launcher credential, runtime id and slot: the same key in the same slot gets its live enrollment
// back (Existing), a different key in a live slot is ErrAlreadyEnrolled, and another slot of the
// same pod is an enrollment of its own. A slot is valid only on a pod enrollment (ErrInvalidSlot);
// a pod's runtime id is always the pod UID its projected token proves, whichever slot it enrolls.
// A pod token past its expiry is ErrPodTokenExpired, naming that expiry; any other token that does
// not prove this pod is ErrPodIdentity. Under a token login, whose whole authorization is the
// deployment's settings, the pod must be one of two (podUnderTokenLogin): the login's own pod,
// when the token proves the login's own account (ErrPodSelfEnrollOnly for any other pod of it),
// or a pod of an account BROKER_SERVICES binds to the login's service, which is the deployment
// saying that service launches those pods (ErrPodNotLaunched for anything else). A pod enrolling
// its own pod states no session name either (ErrPodSelfEnrollNoSession), while a launcher's
// enrolment of a pod it launches states one as before; podUnderTokenLogin holds both rules and
// says why. A person's pod login enrols its own pod alone and states no session name. It never
// reads BROKER_SERVICES, and a pod of any other account is ErrPodSelfEnrollOnly. It enrols no
// other kind, and it enrols nothing once BROKER_POD_OPERATORS no longer maps its account to its
// person (authorized, ErrOperatorMismatch). It names its projects as its person's session does
// (checkProjects). A machine whose
// Google identity token the exchange
// accepted enrolls a session under the credential that exchange minted it, which is why it
// needs no kind and no token of its own here: the credential is the proof. Such a session
// records that credential's subject, which is what makes it its fleet's service's and no
// person's (requests.Machine.requester), as a pod's recorded service account makes it its
// service's.
// A credential revoked (RevokeCredential: by the person who approved a person's machine login, by
// the person that BROKER_POD_OPERATORS maps a pod login's account to, or by anyone signed in for
// a service's), even after the caller's launcher proof was verified, is
// ErrUnauthenticated.
//
// An enrollment that acts for a person — under a person's machine login or pod login — is made
// only once Dispatch has said that person is no restricted member (checkMember): a restricted
// member's login enrolls nothing (ErrRestrictedMember), and while Dispatch cannot be asked
// nothing is enrolled (projectaccess.MemberKindUncheckable). A person's sign-in on a machine is
// not held to that check; its session is held to the list rule alone (checkProjects), which asks
// Dispatch the person's member kind for an empty list and whether they may see each project
// named.
//
// An enrollment that names a session also records the principal its grants follow
// (carryPrincipal), and takes what every predecessor of that name under that principal, or under
// an account paired with its own (SessionBinder.PairedWith), still holds: its grants, its pending
// requests and its withholds move to this row, and a predecessor a box's end left dormant is
// finished by the move (adopt). That is how a grant follows a session to the next box it comes up
// on, and to the next pod of a deployment an operator moved to another account, which a login of
// the same account enrols. It changes none of the identity checks above and nothing in the
// answer: an enrollment that names no session, one whose caller proved no principal and one with
// no predecessor each adopt nothing, an idempotent re-enrollment (Existing) adopts nothing, and a
// pod's token check and the pod-uid ratchet run exactly as they did. Only the rows move.
//
// Because a grant follows a name under one principal, the name has to belong to one principal,
// and before anything is adopted the enrollment binds its session name to the principal it is
// held under (bindSessionName, namePrincipal): the principal its grants follow for every kind but
// a pod, whose name is bound to its account alone while its grants follow the pair of that account
// and its login's (carryPrincipal), or nothing under a login that proved no account. So a pod of
// another daemon that runs as the same account is admitted under the name, and adopts none of
// what the session holds. A name no principal holds becomes this one's, a name an account paired
// with this one holds is taken from it, which the broker logs once the enrollment has committed,
// and a name any other principal holds is ErrSessionHeld, the whole enrollment refused and nothing
// written. Either account of a pair takes a name the other holds; a pair does not chain, and a
// name the configuration fixes is never taken. An enrollment that names no session, and one with
// no principal to hold its name (namePrincipal), bind nothing and are refused nothing.
//
// Every enrollment also records whose its requests are (ownerKey): the person its credential
// carries, the claim its launcher stated for a pod, or the machine. Any enrollment of the same
// owner reads and cancels a request of another (requests.Machine.OwnerAndState), and the next pod of a
// claim takes what a dormant pod of that claim left (adopt). A claim is only for a pod a launcher
// enrols, in the shape a claim token takes (ErrInvalidClaim).
func (s *Service) Create(ctx context.Context, cred Credential, in Enrollment) (Enrollment, error) {
	if in.Slot != "" && (in.Kind != "pod" || !record.ValidSlot(in.Slot)) {
		return Enrollment{}, ErrInvalidSlot
	}
	if in.Claim != "" && (in.Kind != "pod" || !claimPattern.MatchString(in.Claim)) {
		return Enrollment{}, ErrInvalidClaim
	}
	if in.Operator != nil && (cred.Operator == nil || record.CanonicalLogin(*in.Operator) != *cred.Operator) {
		return Enrollment{}, ErrOperatorMismatch
	}
	in.Operator = cred.Operator
	if !s.authorized(cred, in.Kind, in.Operator) {
		return Enrollment{}, ErrOperatorMismatch
	}
	in.Subject = nil
	if in.Kind == "session" && machineCredential(cred) {
		// A machine's session is its fleet's, and the credential's subject is what says which
		// fleet: the enrollment records it as a pod's records the account its token proved.
		in.Subject = cred.Subject
	}
	if in.Kind == "pod" {
		claims, err := s.verifyPodToken(ctx, in.PodToken)
		if err != nil {
			return Enrollment{}, err
		}
		if claims.PodUID != in.RuntimeID {
			return Enrollment{}, ErrPodIdentity
		}
		if err := s.podUnderTokenLogin(cred, claims, in); err != nil {
			return Enrollment{}, err
		}
		in.Subject = &claims.Subject
	}
	// The list is held sorted, the one order a credential's list takes (oauthwire.ValidProjects),
	// so a caller's order is no refusal and two spellings of one list are one list.
	in.Projects = slices.Sorted(slices.Values(in.Projects))
	if len(in.Projects) == 0 {
		in.Projects = nil
	}
	// The list's own shape is refused before Dispatch is asked anything (checkProjectList); then
	// the person's member kind (checkMember), and then the list against that person
	// (checkProjects).
	if err := checkProjectList(in); err != nil {
		return Enrollment{}, err
	}
	if err := s.checkMember(ctx, cred, in); err != nil {
		return Enrollment{}, err
	}
	if err := s.checkProjects(ctx, in); err != nil {
		return Enrollment{}, err
	}
	// The other half of the pair a grant follows, read from what the caller has just proved and
	// written beside the session's name in the insert below (carryPrincipal), the principal the
	// name itself is held under (namePrincipal), and whose the enrollment's requests are
	// (ownerKey).
	in.carry, in.name = carryPrincipal(cred, in), namePrincipal(cred, in)
	if in.Kind == "pod" {
		in.launcher = cred.Subject
	}
	in.owner = ownerKey(cred, in)
	in.ID = uuid.New()
	// To the microsecond, as Postgres stores it (pgx truncates a time it encodes), so the lease end
	// this answers is the row's own and not a moment after it.
	in.LeaseExpires = time.Now().Add(s.Lease).Truncate(time.Microsecond)

	// The row that conflicts with our insert can stop blocking it before the recovery lookup
	// runs — a concurrent Revoke racing this Create — or turn out to be dead already, its lease
	// lapsed with no revoke; either way the next attempt's insert can succeed, so Create retries.
	const maxAttempts = 3
	for range maxAttempts {
		result, retry, err := s.createAttempt(ctx, cred, in)
		if retry {
			continue
		}
		return result, err
	}
	return Enrollment{}, fmt.Errorf("create enrollment: exhausted retries after concurrent changes to runtime %s", in.RuntimeID)
}

// checkMember is Create's rule for the person an enrollment acts for, run once every other check
// of the caller has passed and before any transaction begins, so that no call to Dispatch holds
// a row lock or a pooled connection (the rule internal/cipher/projectaccess states for every
// caller). It asks Dispatch what kind of member in.Operator is: a restricted member, a person an
// access list admitted from outside the deployment's sign-in domain, is ErrRestrictedMember, so
// that person's agents get no session to be released a secret under, however their machine login
// was approved and whichever route the launcher reached; a full member, and a person Dispatch
// holds no row for, enroll as before. Any failure to ask — Dispatch not answering, Dispatch
// refusing the broker's own account (dispatchread.ErrBrokerRefused), a kind this broker does not
// know (projectaccess.ErrUnknownMemberKind) — is the *projectaccess.MemberKindUncheckable
// projectaccess.Restricted returns: a check that cannot decide refuses, and the launcher enrols
// again.
//
// It asks nothing for an enrollment that acts for no person (a service's pod, a machine's own
// session), for a person's sign-in on a machine, whose person the broker verified against
// BROKER_SIGNIN_DOMAIN itself when the token exchange minted the credential (such a session is
// held to checkProjects' list rule alone), and on a broker with no client of Dispatch (Projects
// nil).
func (s *Service) checkMember(ctx context.Context, cred Credential, in Enrollment) error {
	if s.Projects == nil || in.Operator == nil || signInCredential(cred.Subject) {
		return nil
	}
	restricted, err := projectaccess.Restricted(ctx, s.Projects, *in.Operator)
	switch {
	case err != nil:
		return err
	case restricted:
		return fmt.Errorf("%w: %s is a restricted member", ErrRestrictedMember, *in.Operator)
	}
	return nil
}

// checkProjectList is Create's rule for the shape of the list a session names, which asks Dispatch
// nothing and so runs before any question: the list must be a credential's list
// (ErrProjectsInvalid), and only an enrollment acting for a person names one
// (ErrProjectsNeedAPerson).
func checkProjectList(in Enrollment) error {
	if err := oauthwire.ValidProjects(in.Projects); err != nil {
		return fmt.Errorf("%w: %v", ErrProjectsInvalid, err)
	}
	if len(in.Projects) > 0 && in.Operator == nil {
		return ErrProjectsNeedAPerson
	}
	return nil
}

// checkProjects is Create's rule for the projects a session names, run once checkProjectList and
// checkMember have passed and before any transaction begins, so that no call to Dispatch holds a
// row lock or a pooled connection: a list is taken only where the broker can check it with
// Dispatch (ErrProjectsUnchecked); and then projectaccess.ListAllowed, the one statement of the
// rule the token exchange applies too, decides: every project named must be one the person may
// see (ErrProjectNotVisible), and a restricted member must name at least one
// (ErrProjectsRequired), which an enrollment meets only where checkMember asked nothing (a
// person's sign-in) or Dispatch's answer changed between the two reads.
// An empty list enrolls as before with no client, and with a client whose Dispatch could not be
// asked the member kind (projectaccess.ErrUnchecked): the session is enrolled, and a line says it
// was enrolled unchecked. A list Dispatch did not answer for is refused, since taking it
// unchecked could name a project its person may not see; so is any list, empty or not, when
// Dispatch refused the broker's own account or the broker could not read its token
// (dispatchread.ErrBrokerRefused), a deployment that has not listed the broker, through which no
// caller enrolls. An empty list is refused too when Dispatch answered a member kind the broker
// does not know (projectaccess.ErrUnknownMemberKind), since the kind is what an empty list asks; a
// list naming projects asks only whether the person may see each, and no kind. Each refusal wraps
// the project-access error it met.
//
// A person's pod (PersonPodLogin) is held to this rule exactly as its person's session is: it acts
// for its person, so it names its projects, and they are checked against that person. A
// restricted member's pod never reaches here: checkMember refuses it first, as it refuses that
// member's machine login, since a restricted member's login enrolls no session at all. Its
// operator is what says so: authorized has already refused an operator to every pod but a live
// person's pod login's own, so a pod reaches here with an operator only as that, and every other
// pod, a service's, names no project.
func (s *Service) checkProjects(ctx context.Context, in Enrollment) error {
	if s.Projects == nil || in.Operator == nil {
		if len(in.Projects) > 0 {
			return fmt.Errorf("%w: this broker holds no client of Dispatch's project access (BROKER_PROJECT_ACCESS_URL)", ErrProjectsUnchecked)
		}
		return nil
	}
	err := projectaccess.ListAllowed(ctx, s.Projects, *in.Operator, in.Projects)
	var unseen *projectaccess.NotVisibleError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &unseen):
		return fmt.Errorf("%w: %s", ErrProjectNotVisible, unseen.Project)
	case errors.Is(err, projectaccess.ErrListRequired):
		return ErrProjectsRequired
	case errors.Is(err, projectaccess.ErrUnchecked) && len(in.Projects) == 0:
		slog.WarnContext(ctx, "broker: a session naming no project enrolled unchecked, since Dispatch could not be asked what kind of member its person is",
			"person", *in.Operator, "runtime_id", in.RuntimeID, "error", err)
		return nil
	}
	return fmt.Errorf("%w: %w", ErrProjectsUnchecked, err)
}

// createAttempt makes one insert-then-recover attempt. retry is true when the row that conflicted
// with our insert no longer blocks a fresh one — it was revoked before the recovery lookup ran, or
// its lease had lapsed and recovery ended it — in which case Create should try again. It first
// takes the credential's row for share, which RevokeCredential's lock waits behind, and refuses a
// revoked credential (ErrUnauthenticated): an insert either commits before a revoke reads the
// credential's enrollments, and is ended with them, or finds the credential revoked. Then it
// binds the enrollment's session name to its principal (bindSessionName), which is the one other
// thing that can refuse an attempt outright, and a bind that took the name from the account paired
// with the enrollment's own is logged once the attempt has committed, and never by one that did
// not. It runs at READ COMMITTED, named rather than left to the database's default, since each of
// its lock waits is followed by a statement that must read what the transaction it waited on
// committed: an adoption that waited on the session's lock (store.LockSession) reads the names an
// operator's revoke withheld, which a snapshot taken before the wait would not hold.
func (s *Service) createAttempt(ctx context.Context, cred Credential, in Enrollment) (result Enrollment, retry bool, err error) {
	tx, err := s.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `select 1 from launcher_credentials where id=$1 and revoked_at is null for share`, cred.ID).Scan(nil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, false, ErrUnauthenticated
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	taken, err := s.bindSessionName(ctx, tx, in)
	if err != nil {
		return Enrollment{}, false, err
	}
	// pod_token_renewed_at starts null on every row, a pod's included: the first renewal carrying
	// the pod's token sets it, and from then on Renew refuses a token-less renewal from every live
	// enrollment naming that pod UID (podTokenRatcheted).
	_, err = tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, slot, operator, thumbprint, session_id, carry_principal, subject, launcher_credential_id, lease_expires_at, claim, owner, projects)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,coalesce($14::text[], '{}'))`,
		in.ID, in.Kind, in.RuntimeID, in.Slot, in.Operator, in.Thumbprint, in.SessionID, in.carry, in.Subject, cred.ID, in.LeaseExpires, nullable(in.Claim), in.owner, in.Projects)
	if store.IsUniqueViolation(err) {
		// The failed insert aborted tx, which still holds its pooled connection. Release it before
		// recovery asks the pool for one: holding one connection while waiting for a second is how
		// enough concurrent retries of one enrollment deadlock the whole pool.
		if err := tx.Rollback(ctx); err != nil {
			return Enrollment{}, false, err
		}
		if s.testConflictHook != nil {
			s.testConflictHook()
		}
		return s.recoverConflict(ctx, cred, in)
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ('enrollment.created',$1,$2,
		jsonb_strip_nulls(jsonb_build_object('kind',$3::text,'runtime_id',$4::text,'thumbprint',$5::text,'slot',nullif($6::text,''),'claim',nullif($7::text,''))))`,
		in.ID, record.LauncherActor(cred.ID.String()), in.Kind, in.RuntimeID, in.Thumbprint, in.Slot, in.Claim); err != nil {
		return Enrollment{}, false, err
	}
	// The accounts paired with the one the enrollment's name is held under: either account of a
	// pair takes a name the other holds (bindSessionName), and adopt's session rule takes what
	// their enrollments of the name hold. None for an enrollment that holds no name.
	var paired []string
	if s.Sessions != nil && in.name != nil {
		paired = s.Sessions.PairedWith(*in.name)
	}
	if err := adopt(ctx, tx, in, paired, record.LauncherActor(cred.ID.String())); err != nil {
		return Enrollment{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Enrollment{}, false, err
	}
	if taken != "" {
		// An enrollment's claim is never fixed (bindSessionName), so a name it took from a live
		// holder is a pair's take: the one way a principal loses a name it used within the
		// retention to an enrollment. Logged once it has committed, as the exchange logs its own.
		slog.Warn("broker: an enrollment took a session name from the account paired with its own",
			"session", *in.SessionID, "from", taken, "to", *in.name, "enrollment", in.ID)
	}
	return in, false, nil
}

// bindSessionName binds in's session name to the principal it is held under (in.name,
// namePrincipal), in the enrollment's own transaction, which oauthstore.WithTx is how the binder
// joins: the name is this principal's from here on, and the binding and the enrollment commit or
// roll back together, so a refused name leaves no enrollment and a rolled-back enrollment leaves
// no binding. It answers the principal it took the name from, an account paired with in.name
// whose claim had not run out, and "" when the name was unheld, run out or already in.name's, and
// when it binds nothing.
//
// It is the same claim the token exchange makes for every token it mints
// (oauthstore.Store.BindSession) on the same table, which is the point: a Legion role's pod
// trades a token under its session name before or after it enrolls under that name, and the two
// claims are the same principal's (exchange.AccountPrincipal of the ServiceAccount subject its
// projected token proved, which namePrincipal spells the same way), so neither refuses the
// other. That is why a pod's name is bound under its account and not under the pair its grants
// follow (carryPrincipal): the exchange knows no launcher, and a name bound to the pair would
// refuse the pod its own trade. A name an account paired with this principal holds
// (SessionBinder.PairedWith) is taken from it, so a deployment whose pods an operator moved to
// another account keeps each role's session name: either account of a pair takes a name the
// other holds, whether by this claim or by the exchange's, and a pair does not chain. A name any
// other principal holds is ErrSessionHeld, naming the session and no holder.
//
// An enrollment with no principal beside its name binds nothing and is refused nothing: that is
// an enrollment stating no session, and one whose credential proved no principal at all
// (namePrincipal). The claim is made for the enrollment's lease, so the name stays this
// principal's for oauthstore.SessionRetention past the lease, as a token's name stays its
// principal's past the token.
//
// The claim is not fixed: the configuration fixes a name for a principal at the exchange's own
// entries (exchange.Set.Reservations), and an enrollment is no entry, so a name the
// configuration fixes for someone else refuses an enrollment exactly as it refuses a trade, even
// where that someone is an account paired with this one, since no pair takes a fixed name. So
// this claim takes a live name only from a pair, and any principal it answers is a pair's.
//
// Locks: this takes the credential's row (createAttempt, for share) and then the name's own row,
// which is the order the exchange takes them in too — it mints the launcher credential before it
// claims the name (oauth.Provider.claimSession) — so neither can deadlock with the other.
func (s *Service) bindSessionName(ctx context.Context, tx pgx.Tx, in Enrollment) (string, error) {
	if s.Sessions == nil || in.name == nil {
		return "", nil
	}
	taken, err := s.Sessions.BindSession(oauthstore.WithTx(ctx, tx), *in.SessionID, *in.name, false, in.LeaseExpires)
	if errors.Is(err, oauthstore.ErrSessionHeld) {
		return "", fmt.Errorf("%w: %s. Enrol under a name of your own.", ErrSessionHeld, *in.SessionID)
	}
	if err != nil {
		return "", fmt.Errorf("bind the enrollment's session name: %w", err)
	}
	return taken, nil
}

// carryPrincipal is the identity an enrollment's grants follow, which the insert writes into
// enrollments.carry_principal: the half of the pair a grant follows that keeps one session name
// from reaching another principal's grant (adopt), an account paired with this one under the same
// login account (SessionBinder.PairedWith, exchange.LaunchedBy) excepted. It is read from what the
// caller has already proved and never from anything the launcher states, and every spelling is
// exchange's own, the broker's one definition of each.
//
//   - A pod's grants follow the service account its own projected token proved
//     (exchange.AccountPrincipal, the same account requests.Machine.requester reads a pod's
//     service authority from) paired with the account of the token login that enrolled it
//     (exchange.PodLaunchPrincipal). The pods of several daemons run as one account — every
//     public Legion project's pods run as legion-worker — and each daemon logs in as an account
//     of its own, so a role of one daemon's pod that names a session of another daemon's pod
//     takes none of its grants (plan E's t-e-54). A pod enrolled under a
//     login that proved no account, one a person approved through a machine-login code, carries
//     nothing: no such login says which launcher it is. The one credential with an operator that
//     enrols a pod, a person's pod login (PersonPodLogin), enrols its own pod under no session
//     name (podUnderTokenLogin), so that pod carries nothing either.
//   - A session under a credential that carries an operator — a person's own box, and that
//     person's sign-in on a machine — belongs to that person (exchange.PersonPrincipal), so it
//     follows them from one box to the next.
//   - A machine's own session belongs to its fleet's Google account
//     (exchange.GooglePrincipal), which is what its credential's subject names
//     (machineCredential, exchange.ReadSubject).
//
// Anything else belongs to no principal at all: a null carries nothing, is adopted by nobody and
// adopts nothing. So does an enrollment that states no session, whatever its credential proved,
// because a principal with no name beside it is no pair.
func carryPrincipal(cred Credential, in Enrollment) *string {
	if in.SessionID == nil || *in.SessionID == "" {
		return nil
	}
	if in.Kind == "pod" && in.Subject != nil {
		if cred.Subject == nil {
			return nil
		}
		return nullable(exchange.PodLaunchPrincipal(*in.Subject, exchange.AccountPrincipal(*cred.Subject)))
	}
	return namePrincipal(cred, in)
}

// namePrincipal is the principal an enrollment's session name is held under (bindSessionName):
// the principal its grants follow (carryPrincipal) for every kind but a pod, whose name is held
// under its account alone, the principal its own token exchange claims the name under, so that a
// pod's enrollment and its trade under one name never refuse each other. A pod enrolled under a
// login that proved no account still holds its name under its account, as it did before its
// grants followed a pair.
func namePrincipal(cred Credential, in Enrollment) *string {
	if in.SessionID == nil || *in.SessionID == "" {
		return nil
	}
	switch {
	case in.Kind == "pod" && in.Subject != nil:
		return nullable(exchange.AccountPrincipal(*in.Subject))
	case cred.Operator != nil:
		return nullable(exchange.PersonPrincipal(*cred.Operator))
	case machineCredential(cred):
		return nullable(exchange.GooglePrincipal(exchange.ReadSubject(*cred.Subject).Account))
	}
	return nil
}

// claimPattern is the shape a claim takes (ErrInvalidClaim): a Legion claim token,
// legion-<project>-<issue>-<role>, fits it, and nothing that could read as another kind of owner
// does, since it holds no slash and no colon.
var claimPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,127}$`)

// claimOwnerPrefix opens every owner ownerKey reads from a claim: the one kind of owner adopt moves
// grants by (adopt's claim rule), and the predicate of the partial index that serves it
// (migration 0035).
const claimOwnerPrefix = "claim:"

// ownerKey is whose the requests an enrollment makes are, which the insert writes into
// enrollments.owner: any enrollment with the same owner reads and cancels them
// (requests.Machine.OwnerAndState), and the next pod of a claim takes what a dormant pod of that claim
// left (adopt). It is read from what the caller has already proved and from the claim a launcher
// stated, which is the launcher's word as the session name it gives each pod is
// (podUnderTokenLogin), and from nothing else:
//
//   - an enrollment under a credential that carries an operator — a person's own box, that
//     person's sign-in on a machine, and a person's pod (PersonPodLogin) — is that person's
//     (exchange.PersonPrincipal), whichever box or session it is. A person's pod has that owner
//     only while BROKER_POD_OPERATORS maps its account to the person (requests.Machine.owner).
//     After the setting drops the account, or maps it to another person, the pod's requests are
//     its own enrollment's alone;
//   - a pod its launcher states a claim for is that claim's under the account the launcher's own
//     token login proved, claim:<login subject>/<claim>, so a claim token is one owner however
//     many generations and pods it runs in, and the same token stated by another login is another
//     owner. The login's account and not its service: every project's Legion daemon logs in as
//     the one service legion-daemon, each under an account of its own, so a key on the service
//     would make one project's daemon that states another project's claim that claim's owner,
//     taking what its dormant pods left (adopt). A claim comes only from a login whose own token
//     proved its subject (ErrInvalidClaim), and a claim holds no slash, so no two pairs of a
//     login and a claim spell one owner;
//   - a machine's own session is that machine's, machine:<service>/<subject>, the subject its
//     credential proved naming one machine and its key (exchange.MachineSubject), so two machines
//     of one fleet are two owners;
//   - a pod that states no claim is its own pod's, machine:<service>/<pod uid>, and a slot of it
//     is that slot's, machine:<service>/<pod uid>/<slot>. Every role process of an issue pod
//     enrols under the pod's one uid in a slot of its own (<role>-g<generation>), so without the
//     slot one role would read and cancel another's requests, which each slot holds alone. A slot
//     holds no slash (record.ValidSlot), and a machine's subject holds a '#' before its key's
//     thumbprint (exchange.MachineSubject), which neither a pod uid nor a slot holds, so no two of
//     these spell one owner.
//
// Anything else, and every row from before migration 0035, records none, and a request of an
// enrollment with no owner is that enrollment's alone. Unlike carryPrincipal, an owner needs no
// session name: a person's request is the person's whether or not their session named itself.
func ownerKey(cred Credential, in Enrollment) *string {
	switch {
	case cred.Operator != nil:
		return nullable(exchange.PersonPrincipal(*cred.Operator))
	case cred.Service == nil:
		return nil
	case in.Kind == "pod" && in.Claim != "" && cred.Subject != nil:
		return new(claimOwnerPrefix + *cred.Subject + "/" + in.Claim)
	case in.Kind == "pod" && in.Slot != "":
		return new("machine:" + *cred.Service + "/" + in.RuntimeID + "/" + in.Slot)
	case in.Kind == "pod":
		return new("machine:" + *cred.Service + "/" + in.RuntimeID)
	case machineCredential(cred):
		return new("machine:" + *cred.Service + "/" + *cred.Subject)
	}
	return nil
}

// holdsForItsSession is the one SQL text for "enrollment e still holds something for its
// session": a live grant, or a request nobody has answered yet. Four statements read it, and
// they must read it identically — endEnrollment's probe, which decides whether an end leaves the
// row dormant instead of revoking what it holds, adopt's predecessor query, which decides which
// ended rows the next enrolment of the name takes a grant or a request from, Carried, which
// answers a dormant row only while it holds something adopt would take (Carried's own test
// TestAnEmptyCarriedAnswerHandsTheNextEnrolmentNothing binds its answer to adopt's), and
// requests.enrollmentWouldGoDormant, the requests package's copy of endEnrollment's probe
// character for character, which decides whether an approval on a lapsed row the sweep has not
// ended yet mints a grant. Were one wider than another, an end could leave a row dormant that no
// successor ever adopts (its grants standing until the carry sweep finds them), adopt could take
// from a row an end revoked under it, Carried could leave out a row adopt takes from, or an
// approval could be granted onto a row the sweep then revokes. What holds this text and that copy
// together is requests.TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes, which runs the
// copy and Service.EndLapsed over every shape of row and compares their answers, so an edit here
// that leaves the copy behind fails that test. All four splice it in with e as the enrollment, and
// each reads the whole pair beside it: the two probes test that the row names a session and
// records a principal, adopt matches on both, and Carried on the session and the principal of the
// caller's login. adopt adds the withheld names outside this text, because a withhold follows the
// session too but is no part of what makes an end carry: it outlives an end of either kind
// (adopt's second part).
const holdsForItsSession = `exists (select 1 from grants g where g.enrollment_id=e.id and g.revoked_at is null and (g.expires_at is null or g.expires_at > now()))
				or exists (select 1 from requests r where r.enrollment_id=e.id and r.state='pending')`

// carriesForItsSession is "the carry left something on enrollment e for its session": it still
// holds a live grant or a pending request (holdsForItsSession) and no end has revoked them — it
// is live, or it is dormant, which is the end of a box that left what it held standing
// (endEnrollment). adopt reads it twice in one statement: as the first part of its predecessor
// rule, and as the flag saying whether that predecessor's grants and requests move beside its
// withheld names.
const carriesForItsSession = `(e.revoked_at is null or e.dormant_since is not null) and (` + holdsForItsSession + `)`

// adopt moves what every predecessor of this enrollment's session name holds to the enrollment
// createAttempt has just inserted, inside that same transaction, revoking none of it, and then
// what a dormant enrollment of the same claim still holds. paired is the accounts paired with the
// one the enrollment's name is held under (SessionBinder.PairedWith), as createAttempt reads them,
// whose enrollments of the name the session rule takes from beside the enrollment's own (below).
// Both kinds of enrollment adopt by the session rule, a session's and a pod's; only a pod a
// launcher stated a claim for adopts by the claim rule.
//
// A pod's predecessor also includes a row of the same session an older broker wrote, whose carry
// principal is the pod's account alone, when the token login that enrolled it proved the same
// account as this enrollment's login: the same daemon's own row, written by a broker replica that
// had not yet taken the pair (exchange.PodLaunchPrincipal). The broker rolls one replica at a
// time, so the old one writes such rows after migration 0037 has rewritten the rest, and without
// this they would be adopted by nothing and their grants swept. Another daemon's legacy row is
// still no predecessor, so no pod takes another daemon's grants under either spelling.
//
// A predecessor is another enrollment of the same session under a spelling the rule matches, and
// the rule has two parts, because what a grant may follow and what a withhold must follow are not
// the same thing:
//
//   - An enrollment the carry left something on (carriesForItsSession) is a predecessor for
//     everything under it: its grants, its requests and its withheld names. It is one of two
//     things, a live enrollment, the box the session is moving off while that box is still up, or
//     a dormant one, a box whose end left what it held for the session to take (endEnrollment).
//     What that reading keeps out is a row ended for good, whose grants its own end revoked and
//     whose requests it cancelled: there is no live grant on it to follow the session.
//   - An enrollment of the session under a spelling the rule matches that holds withheld names is a
//     predecessor for those names, whatever state its row is in — live, dormant, or ended for
//     good. That last is the usual case rather than a corner: an operator's revoke of an
//     automatic grant writes the withheld row and takes the grant, so the box is commonly left
//     holding no live grant and no pending request at all, its end is therefore terminal and its
//     row never goes dormant. Were the names left on it, the next enrolment of the session would
//     find the secret automatic again and a replaced box would undo what the operator said. Off a
//     row ended for good, taking them hands nothing over: nothing reaches the broker through an
//     enrollment that is over, so the names only ever sat there. Off a live predecessor it does
//     hand something over, and that is accepted here: requests.Machine reads a requester's
//     withheld names by the enrollment's own id, so the box the names move off asks for that
//     name automatically again. What bounds it is that a withhold binds a session name and never
//     the principal behind it — the same principal can start a session under a name it has not
//     used and get a box with nothing withheld on it at all — and that the session the operator
//     took the name from is left without it, because the names move to the enrollment that
//     session runs on now.
//
// An enrollment of the session under a spelling the rule matches that holds neither is no
// predecessor and is left exactly as it is, which is what keeps a second live session of one name
// from being ended by its neighbour's enrolment.
//
// The accounts beside the enrollment's own are its account's pairs (paired): either account of a
// pair takes a session name the other holds (bindSessionName), so an enrollment of the name under
// one takes what the other account's enrollments of the name hold, by this same rule, as a
// successor of the same principal would. For a pod the rule matches each paired account in both
// of its spellings, the pair of that account and this enrollment's own login (exchange.LaunchedBy)
// and the legacy row of that account alone that a login of the same account enrolled, so it takes
// nothing another daemon launched under either account: a pair moves a session between two
// accounts one daemon launches pods as, never between two daemons. The rule matches them at every
// enrollment, not only at the one whose bind took the name, because the token exchange takes the
// name for a paired account too: a pod whose own exchange took the name before it enrolled is the
// name's holder by the time it enrols, and still takes what the other account's enrollments of
// that name hold. A pair gives no more than the session rule gives a successor of the same
// principal, withheld names included, since a withhold binds a session name and never the
// principal behind it (the second part above), and the name is this enrollment's principal's by
// the time it adopts. Only the enrollment's own pairs are matched, so a pair does not chain. Pairs
// are of Kubernetes service accounts (config.SessionAccountPairs), which a pod's name alone is held
// under, so no other kind's rule widens.
//
// The claim rule is the one move by owner (ownerKey), and it is narrower than the session rule on
// purpose. A Legion role's next pod comes up under a session of its own — a fresh session, a
// replaced pod — so the session rule cannot follow it, and the role's grants and pending requests
// would wait on a session that never comes back until the carry sweep revoked them
// (the gap this change closes). So an enrollment whose owner is a claim also takes everything a
// dormant enrollment of the same claim still holds (holdsForItsSession on a row whose box has
// ended): its grants and its requests, and not its withheld names, which bind a session name
// rather than whoever runs it (the second part above). A live enrollment of the claim keeps what
// it holds, as a live predecessor of the session rule's would not: it is a process of the role
// still running, and this is no end of it. A person's and a machine's grants move by the session
// rule alone, so a session that resumes gets its own grant back even after another session of
// the same person enrolled first, and that other session, which reads and cancels the request
// (requests.Machine.OwnerAndState), never takes the grant. A pair changes nothing of the claim
// rule, which reads the owner alone, and a row both rules find, a paired account's included, is
// the session rule's.
//
// What moves is the rows keyed by the predecessor: grants.enrollment_id and
// requests.enrollment_id from a predecessor of the first part, withheld_secrets.enrollment_id
// from a predecessor of either. The grants are what the session was approved for, the requests
// are what it is still waiting on a person for — an approval must land on the session as it
// stands when the answer comes, not on the box that asked — and the withholds are the one way a
// person takes a secret back from a session, which a move must not undo. A withheld name the
// successor already holds is kept once, since (enrollment_id, name) is that table's primary key.
// Off a first-part predecessor every grant and request moves, the ended grants and decided
// requests beside the live ones, so it is left holding nothing at all rather than a part of what
// it held; off a row ended for good none does, since its own end revoked and cancelled them, and
// moving those dead rows would only hang a grant.moved trail off grants that grant nothing.
//
// A dormant predecessor is then finished: dormant_since returns to null, because the row was
// kept only for what it held and that is here now. Its revoked_at stands where its own end wrote
// it, because the box ended then and not now. So it is no longer dormant, the carry sweep has
// nothing of it to revoke (EndCarried) and it is a predecessor of nothing. A live predecessor is
// left live: the box is still up and this is no end of it, and its own launcher ends it when the
// session's process goes (Revoke), by which time there is nothing under it to revoke. A
// predecessor already ended for good is left ended, the one thing that outlived its end taken
// off it. Nothing is revoked or cancelled here in any of the three, which is why this does not go
// through endEnrollment, whose terminal path would revoke grants the row no longer has.
//
// The trail names both enrollments: one enrollment.adopted row on the successor naming the
// predecessor it took from and how much, and one grant.moved row per grant, naming the grant, the
// successor it now hangs off and the predecessor it came from, so an operator asking why a
// session reads a secret it never asked this box for lands on the box that did.
//
// Locks, in the order this transaction takes them: the launcher credential's row (for share,
// taken by createAttempt before anything else), then the session name's own row
// (bindSessionName), then the new enrollment's own row (its insert), then the session's lock
// (store.LockSession, for an enrollment that carries a principal), then the enrollments rows,
// then the grant, request and withheld rows under them. The session's lock is the one every
// person's revoke of a grant takes before it locks the session's live and dormant enrollments
// (store.LockSession says who takes it and why), so an adoption reads its predecessors only once
// no such revoke is in flight, and takes the names an operator's revoke withheld: without it, a
// revoke that committed while this adoption had read its predecessors and not yet committed would
// withhold on boxes the session has left and not on this one. The predecessors of both rules are
// taken for update in one statement, in id order, so no end, revoke or sweep moves what they hold
// while it is moving, and two concurrent adoptions of one pair or one claim cannot hold each
// other's first row. That is the order every other writer of these tables takes them in —
// endCredential takes the credential, then its enrollments in id order; Revoke, EndLapsed and
// EndCarried take an enrollment before the grants and requests under it; a person's revoke takes
// the session's lock and then its live and dormant enrollments in id order; and this package's
// enrollment-then-request writers take the enrollment first too (ApplyDecision says so itself) —
// so none of them can deadlock with this one.
func adopt(ctx context.Context, tx pgx.Tx, in Enrollment, paired []string, actor string) error {
	claim := in.owner
	if claim != nil && !strings.HasPrefix(*claim, claimOwnerPrefix) {
		claim = nil
	}
	if in.carry == nil && claim == nil {
		return nil
	}
	if in.carry != nil {
		if err := store.LockSession(ctx, tx, *in.SessionID, *in.carry); err != nil {
			return err
		}
	}
	// The spellings the session rule matches. carries is the pair a grant follows: the
	// enrollment's own and, for a pod, each paired account's launched by the same login
	// (exchange.LaunchedBy). legacy is a pod's account alone, its own and each paired account's, as
	// an older broker wrote a row a login of the same account enrolled, and launcher is that
	// login's account. A null array matches no row: an enrollment that carries nothing sends no
	// carries, and legacy and launcher are nil off a pod, where the legacy arm of ofThisSession
	// matches nothing.
	var carries, legacy []string
	var launcher *string
	if in.carry != nil {
		carries = []string{*in.carry}
		if in.Kind == "pod" && in.launcher != nil {
			legacy, launcher = slices.Concat([]string{*in.name}, paired), in.launcher
			for _, account := range paired {
				carries = append(carries, exchange.LaunchedBy(account, exchange.AccountPrincipal(*in.launcher)))
			}
		}
	}
	// ofThisSession is the session rule's match: a row of this enrollment's own pair or a paired
	// account's, or a pod's legacy row of the same session, written under its own account or a
	// paired account alone by a login that proved this enrollment's login's account. Its three
	// readers below read it alike, so a legacy row is the session rule's wherever a row of the
	// pair is.
	const ofThisSession = `e.session_id=$2 and (e.carry_principal = any($1::text[])
				or (e.carry_principal = any($5::text[]) and e.kind='pod' and exists (select 1 from launcher_credentials c
					where c.id=e.launcher_credential_id and c.subject=$6::text)))`
	// Every predecessor of both rules in one statement. An enrollment that carries nothing sends
	// null arrays, and one whose owner is no claim a null owner: a comparison with a null is never
	// true, so that rule's arm finds no row. The third column says which rule a row is the session
	// rule's, and the fourth whether the carry left something on it: a session-rule row the carry
	// left something on gives up its grants and requests as well as its withheld names, one that
	// matched the withheld clause alone gives up the names, and a claim-rule row gives up its
	// grants and requests. The claim arm spells the partial index's own predicate, which is what
	// lets it read enrollments_claim_owner_dormant (migration 0035).
	rows, err := tx.Query(ctx, `select e.id, e.dormant_since is not null,
			coalesce(`+ofThisSession+`, false),
			coalesce(`+ofThisSession+`, false) and `+carriesForItsSession+`
		from enrollments e
		where e.id <> $3
			and ((`+ofThisSession+`
					and (`+carriesForItsSession+`
						or exists (select 1 from withheld_secrets w where w.enrollment_id=e.id)))
				or (e.owner=$4 and e.owner like '`+claimOwnerPrefix+`%' and e.dormant_since is not null
					and (`+holdsForItsSession+`)))
		order by e.id for update of e`, carries, in.SessionID, in.ID, claim, legacy, launcher)
	if err != nil {
		return err
	}
	type predecessor struct {
		ID      uuid.UUID
		Dormant bool
		// Session is the session rule: the row is of this enrollment's session under a spelling
		// ofThisSession matches, its own account's or a paired account's. False is a row the claim
		// rule alone found, a dormant enrollment of the same claim, which gives up its grants and
		// requests and keeps its withheld names.
		Session bool
		// Carries is the session rule's first part: the carry left a live grant or a pending
		// request on this row, so its grants and requests move beside its withheld names. False on
		// a session-rule row is one matched by the withheld clause alone, which gives up its names
		// and keeps the rest.
		Carries bool
	}
	predecessors, err := pgx.CollectRows(rows, pgx.RowToStructByPos[predecessor])
	if err != nil {
		return err
	}
	for _, from := range predecessors {
		rule := "session"
		if !from.Session {
			rule = "claim"
		}
		var grants, requests, withheld int64
		if from.Carries || !from.Session {
			grants, requests, err = moveHeld(ctx, tx, from.ID, in.ID, actor, !from.Session, in.SessionID)
			if err != nil {
				return err
			}
		}
		if from.Session {
			// The delete and the insert are one statement over one snapshot, so a name the
			// successor already holds leaves the predecessor's row deleted and the successor's as
			// it was: the withhold is kept once, from when it was first made.
			moved, err := tx.Exec(ctx, `with moved as (
					delete from withheld_secrets where enrollment_id=$1 returning name, created_at
				)
				insert into withheld_secrets (enrollment_id, name, created_at)
				select $2, name, created_at from moved on conflict do nothing`, from.ID, in.ID)
			if err != nil {
				return err
			}
			withheld = moved.RowsAffected()
		}
		if from.Dormant {
			if _, err := tx.Exec(ctx, `update enrollments set dormant_since=null where id=$1`, from.ID); err != nil {
				return err
			}
		}
		if err := auditAdoption(ctx, tx, from.ID, in.ID, actor, from.Dormant, rule, grants, requests, withheld); err != nil {
			return err
		}
	}
	return nil
}

// moveHeld moves every grant and every request of enrollment from onto enrollment to inside tx,
// writing one grant.moved audit row per grant, naming the grant, the enrollment it hangs off now
// and the one it came from, and answers how many grants and requests moved. One statement per
// table, each writing its own trail, so the insert's own count is how many grants moved.
//
// byClaim is a move by the claim rule, adopt's or a hand-over's (handOver), which moves a
// request onto a pod running under a session of its own. A pending request that named a session
// for its wake (requests.session_id, the session of the process that asked) then names session,
// the session of the pod it moved to, or none where that pod names none, so its expiry wakes the
// pod now waiting on it and not the session of the pod whose box ended; a request that named
// none still names none, and its wake falls back to the enrollment it hangs off, the new one. A
// decided request keeps the session it named, which no wake reads again. The session rule moves
// a request to an enrollment of the same session, so it renames nothing.
func moveHeld(ctx context.Context, tx pgx.Tx, from, to uuid.UUID, actor string, byClaim bool, session *string) (grants, requests int64, err error) {
	movedGrants, err := tx.Exec(ctx, `with moved as (
			update grants set enrollment_id=$2 where enrollment_id=$1 returning id
		)
		insert into audit (kind, enrollment_id, grant_id, actor, detail)
		select 'grant.moved', $2, id, $3, jsonb_build_object('from',$1::uuid,'to',$2::uuid) from moved`,
		from, to, actor)
	if err != nil {
		return 0, 0, err
	}
	movedRequests, err := tx.Exec(ctx, `update requests set enrollment_id=$2,
			session_id = case when $3::boolean and state='pending' and session_id is not null then $4::text else session_id end
		where enrollment_id=$1`, from, to, byClaim, session)
	if err != nil {
		return 0, 0, err
	}
	return movedGrants.RowsAffected(), movedRequests.RowsAffected(), nil
}

// auditAdoption writes the enrollment.adopted row on enrollment to: the predecessor it took from,
// whether that predecessor was dormant, the rule it moved by (session or claim) and how many
// grants, requests and withheld names moved.
func auditAdoption(ctx context.Context, tx pgx.Tx, from, to uuid.UUID, actor string, dormant bool, rule string, grants, requests, withheld int64) error {
	_, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ('enrollment.adopted',$1,$2,
		jsonb_build_object('from',$3::uuid,'to',$1::uuid,'dormant',$4::boolean,'rule',$8::text,
			'grants',$5::bigint,'requests',$6::bigint,'withheld_secrets',$7::bigint))`,
		to, actor, from, dormant, grants, requests, withheld, rule)
	return err
}

// recoverConflict resolves an insert that collided with an unrevoked enrollment of the same
// runtime and slot under the same launcher credential. The launcher may be retrying after a crash
// between our 201 and its persist: the same key gets its live enrollment back, and a different key
// for a live slot is a refusal. A colliding row whose lease has lapsed is not live, whatever its
// revoked_at says: it is ended here — like a revoke, with an enrollment.expired audit row — and
// the caller retries, so a lapsed lease never leaves the slot permanently locked.
func (s *Service) recoverConflict(ctx context.Context, cred Credential, in Enrollment) (Enrollment, bool, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	var existing Enrollment
	var live bool
	err = tx.QueryRow(ctx, `select id, thumbprint, lease_expires_at, enrollment_lease_live(lease_expires_at) from enrollments
		where launcher_credential_id=$1 and runtime_id=$2 and slot=$3 and revoked_at is null for update`, cred.ID, in.RuntimeID, in.Slot).
		Scan(&existing.ID, &existing.Thumbprint, &existing.LeaseExpires, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, true, nil
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	if !live {
		if _, err := endLapsed(ctx, tx, existing.ID.String()); err != nil {
			return Enrollment{}, false, err
		}
		return Enrollment{}, true, tx.Commit(ctx)
	}
	if existing.Thumbprint != in.Thumbprint {
		return Enrollment{}, false, ErrAlreadyEnrolled
	}
	// The same key back is a retry of the same enrollment, which names the same projects: a list
	// is fixed for an enrollment's life, so a retry naming another one is a second enrollment of
	// a live runtime, and refused as one rather than answered with a list it did not ask for. The
	// list is read here, from the row the statement above already locks, and not in that
	// statement: Dispatch's lock guard (dispatch/api's TestNoForUpdateOnOwnerTables) refuses a
	// `for update` statement that spells an owner table's name, and the column shares its name
	// with Dispatch's projects table.
	if err := tx.QueryRow(ctx, `select projects from enrollments where id=$1`, existing.ID).Scan(&existing.Projects); err != nil {
		return Enrollment{}, false, err
	}
	if !slices.Equal(existing.Projects, in.Projects) {
		return Enrollment{}, false, fmt.Errorf("%w under the projects %q, not %q", ErrAlreadyEnrolled, oauthwire.JoinProjects(existing.Projects), oauthwire.JoinProjects(in.Projects))
	}
	in.ID, in.LeaseExpires, in.Existing = existing.ID, existing.LeaseExpires, true
	return in, false, nil
}

// endMode says what an end does to what its enrollment still holds, and every caller states which
// it is, because the two kinds of end are about different things: an end of the box is not an end
// of the session that ran on it, and an end a person made is an end of both.
type endMode int

const (
	// endMayCarry leaves a session's grants and pending requests where they are, the enrollment
	// dormant, when it holds any and names a session. These are the ends a move makes: the lease
	// lapsing (EndLapsed), recoverConflict's end of the lapsed row holding the slot a fresh
	// enrollment wants, and the launcher's own revoke (Revoke) — the helper's when a session's
	// process ends, and the daemon's when it lets a role's process go
	// (packages/daemon/internal/supervise/secrets.go, RevokeEnrollment). A box is replaced far
	// more often than the session it runs, so none of these says the session is over.
	endMayCarry endMode = iota
	// endTerminal revokes the grants and cancels the pending requests, as every end did before
	// the carry. These are the ends that mean every secret this enrollment reached: a
	// machine-login revoke (endCredential, reached from RevokeCredential, RevokeOwnCredential and
	// RevokeReusedCredential), a person's revoke of their sign-in on a box (RevokePersonSignIn,
	// which ends that sign-in's credential the same way), and the carry sweep itself
	// (EndCarried), which is the end a dormant enrollment is waiting for. An operator's or an
	// approver's revoke of a grant is terminal too and never reaches here: it revokes the grant
	// itself (requests.Machine).
	endTerminal
)

// endedEnrollment is what one end did to what its enrollment held: how many live grants it
// revoked, how many pending requests it cancelled, and whether it left both where they were
// instead, the enrollment dormant, or handed both to a live pod of its claim (HandedTo, the id of
// that pod's enrollment).
type endedEnrollment struct {
	GrantsRevoked     int64
	RequestsCancelled int
	Dormant           bool
	HandedTo          string
}

// endEnrollment ends enrollment id inside tx: it is marked revoked, every live grant under it is
// revoked, and every request still pending under it is cancelled — an approval must never land
// on a session that has ended — with its audit row and its record's cancelled event
// (store.EndPendingRequests, so no approver's pending list keeps showing it), and one kind row
// for the enrollment. It reports how many grants it revoked and requests it cancelled. The caller
// holds the enrollment row's lock.
//
// One end does none of that. An enrollment that names a session (session_id), records the
// principal that session belongs to (carry_principal), holds a live grant or a pending request,
// and is ended by an end of its box rather than by a person (endMayCarry), is marked revoked and
// dormant_since, and what it holds stays where it is for the next enrollment of that session
// name under the same principal to take. Ending the box's enrollment is how a session moves: a
// helper restarts, a machine is rebuilt, a person signs in on a second box, a Legion pod is
// replaced, and in each of those the box's enrollment ends while the session goes on and asks
// the broker for the same secret again. Both kinds carry, a session's and a pod's: a pod's
// enrollment is ended before its successor enrols, as a rule — by the daemon's launcher revoke
// when it lets the role's process go, or by EndLapsed one lease after the pod stopped renewing —
// so a pod carry that waited for a live predecessor would find nothing.
//
// The exception is the next pod of a claim that comes up before the end reaches the last one: the
// daemon's revoke lands late or not at all, and the old row lives on until its lease lapses. The
// claim rule finds no dormant row when that pod enrols (adopt), so an end that would leave a row
// of a claim dormant hands what it holds to the live pod of the same claim instead (handOver),
// the most recent if there are several, and the row ends for good holding nothing: a person who
// approves the request after the next pod came up has the grant land on that pod, and not on a
// row only a third pod would take from. A live pod another end or a renewal holds at that moment
// is passed over rather than waited for (skip locked), because two pods of one claim ending at
// once would each wait on the other; with none to take it the row goes dormant as before, and the
// claim's next enrolment takes it.
//
// The probe reads the principal as well as the name because dormancy is a promise that some
// later enrolment can take what the row still holds, and adopt matches on the whole pair
// (carry_principal, session_id): a row with no principal can be adopted by nobody, whatever its
// session_id says. Leaving such a row dormant would hold its grants unrevoked for
// BROKER_SESSION_CARRY_SECONDS with no one able to take them, and leave its pending request's
// card standing in an approver's Inbox where approving it is denied, since the enrollment behind
// it has ended and nothing will inherit it. So an enrollment with no principal ends as every end
// did before the carry. That is no corner of the deploy but the whole of it on the day it ships:
// every enrollment live when migration 0027 applies has carry_principal null, among them every
// daemon pod enrollment, which does carry a session_id
// (packages/daemon/internal/supervise/secrets.go). Each of those ends once, as it always did,
// and the enrolments that replace them record a principal and carry.
//
// The probe below is copied character for character into the requests package as
// requests.enrollmentWouldGoDormant, where an approval on a lapsed row no end has reached yet
// reads it to decide whether that approval mints a grant, so an edit to the probe has to be made
// there too. requests.TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes runs that copy
// and Service.EndLapsed over every shape of row and compares their answers, and is what fails
// when the two drift.
//
// A dormant enrollment proves nothing from the moment it goes dormant, because its key died with
// its box: revoked_at is set, and Lookup, Get, Renew, SessionID, recoverConflict, liveCredentials
// and requests.Machine's own grant read all answer on exactly that. What it holds is reachable
// only through the next enrollment of its session name, and is revoked by EndCarried once
// BROKER_SESSION_CARRY_SECONDS has passed with no such enrollment.
func endEnrollment(ctx context.Context, tx pgx.Tx, id, actor, kind, reason string, mode endMode) (endedEnrollment, error) {
	var dormant bool
	var owner *string
	if mode == endMayCarry {
		if err := tx.QueryRow(ctx, `select coalesce(e.session_id,'') <> '' and e.carry_principal is not null and (
				`+holdsForItsSession+`), e.owner
			from enrollments e where e.id=$1`, id).Scan(&dormant, &owner); err != nil {
			return endedEnrollment{}, err
		}
	}
	if dormant && owner != nil && strings.HasPrefix(*owner, claimOwnerPrefix) {
		handed, err := handOver(ctx, tx, id, *owner, actor, kind, reason)
		if err != nil || handed.HandedTo != "" {
			return handed, err
		}
	}
	if dormant {
		if _, err := tx.Exec(ctx, `update enrollments set revoked_at=now(), dormant_since=now() where id=$1`, id); err != nil {
			return endedEnrollment{}, err
		}
		// The end's own audit row, with the one fact that makes it different: nothing under it was
		// revoked. An operator reading the trail of a session that moved sees which end left the
		// session's grants standing and when the window on them started.
		if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3,
			jsonb_build_object('reason',$4::text,'dormant',true))`, kind, id, actor, reason); err != nil {
			return endedEnrollment{}, err
		}
		return endedEnrollment{Dormant: true}, nil
	}
	// coalesce keeps the time the enrollment ended where a dormant end already wrote it: EndCarried
	// and a person's revoke each reach a row that ended hours ago, and what they finish is that
	// end, not a second one now. dormant_since returns to null because the row now holds nothing
	// any session could take.
	if _, err := tx.Exec(ctx, `update enrollments set revoked_at=coalesce(revoked_at, now()), dormant_since=null where id=$1`, id); err != nil {
		return endedEnrollment{}, err
	}
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where enrollment_id=$1 and revoked_at is null`, id, actor)
	if err != nil {
		return endedEnrollment{}, err
	}
	detail := "the requesting enrollment ended: " + reason
	cancelled, err := store.EndPendingRequests(ctx, tx, store.RequestsOfEnrollment, id, store.RequestEnd{
		State: "cancelled", Actor: actor, DecidedBy: &actor, Detail: detail, AuditDetail: map[string]any{"reason": detail},
	})
	if err != nil {
		return endedEnrollment{}, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3, jsonb_build_object('reason',$4::text))`, kind, id, actor, reason); err != nil {
		return endedEnrollment{}, err
	}
	return endedEnrollment{GrantsRevoked: tag.RowsAffected(), RequestsCancelled: len(cancelled)}, nil
}

// handOver is endEnrollment's end of enrollment id, a row of claim owner that would otherwise go
// dormant, when a pod of the same claim is live: everything id holds moves to the most recently
// enrolled of them by the claim rule (moveHeld), id ends for good holding nothing, and the trail
// is what adopt's claim rule writes, an enrollment.adopted row on the pod that took it (rule
// claim, dormant false), beside id's own end row naming that pod (handed_to). Its withheld names
// stay on id, as the claim rule leaves them: they bind a session name, and the next enrolment of
// that name takes them (adopt). It answers an empty HandedTo and writes nothing when no live pod
// of the claim can take it, and the caller leaves id dormant.
//
// The pod is locked before anything moves onto it, so its own end cannot read what it holds
// before the move and miss the rows moved here. One held by another transaction — its own end, or
// its renewal — is passed over (skip locked): two pods of one claim ending at once would each
// otherwise wait on the other's row. enrollments_claim_owner_live (migration 0035) serves the
// lookup, which spells its predicate.
func handOver(ctx context.Context, tx pgx.Tx, id, owner, actor, kind, reason string) (endedEnrollment, error) {
	var to uuid.UUID
	var session *string
	err := tx.QueryRow(ctx, `select e.id, e.session_id from enrollments e
		where e.owner=$1 and e.owner like '`+claimOwnerPrefix+`%' and e.revoked_at is null and e.id <> $2
			and enrollment_lease_live(e.lease_expires_at)
		order by e.created_at desc, e.id limit 1 for update skip locked`, owner, id).Scan(&to, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return endedEnrollment{}, nil
	}
	if err != nil {
		return endedEnrollment{}, err
	}
	from, err := uuid.Parse(id)
	if err != nil {
		return endedEnrollment{}, err
	}
	grants, requests, err := moveHeld(ctx, tx, from, to, actor, true, session)
	if err != nil {
		return endedEnrollment{}, err
	}
	if _, err := tx.Exec(ctx, `update enrollments set revoked_at=now(), dormant_since=null where id=$1`, id); err != nil {
		return endedEnrollment{}, err
	}
	if err := auditAdoption(ctx, tx, from, to, actor, false, "claim", grants, requests, 0); err != nil {
		return endedEnrollment{}, err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, actor, detail) values ($1,$2,$3,
		jsonb_build_object('reason',$4::text,'handed_to',$5::uuid))`, kind, id, actor, reason, to); err != nil {
		return endedEnrollment{}, err
	}
	return endedEnrollment{HandedTo: to.String()}, nil
}

// endLapsed ends an enrollment whose lease lapsed: endEnrollment with actor broker and an
// enrollment.expired audit row, for recoverConflict and the sweep alike. A lapse is an end of the
// box (endMayCarry), since a renewer that stopped is what a replaced box looks like from here.
func endLapsed(ctx context.Context, tx pgx.Tx, id string) (endedEnrollment, error) {
	return endEnrollment(ctx, tx, id, "broker", "enrollment.expired", "its lease lapsed", endMayCarry)
}

// LapsedEnrollment is one enrollment EndLapsed ended.
type LapsedEnrollment struct {
	ID                string
	Kind              string
	RuntimeID         string
	Slot              string
	LeaseExpires      time.Time
	GrantsRevoked     int64
	RequestsCancelled int
	// Dormant says the lapse left this enrollment's grants and pending requests where they were,
	// for the next enrollment of its session name to take (endEnrollment): the session is on
	// another box, or about to be, and GrantsRevoked and RequestsCancelled are both 0.
	Dormant bool
	// HandedTo is the enrollment of the live pod of its claim the lapse handed what it held to
	// instead (handOver), "" for every other lapse; GrantsRevoked and RequestsCancelled are both 0.
	HandedTo string
}

// lapsedBatch is how many lapsed enrollments EndLapsed ends in one transaction.
const lapsedBatch = 100

// EndLapsed ends every enrollment whose lease has lapsed and that nothing has ended yet: a pod that
// is gone, a box whose launcher stopped renewing, a machine session whose helper died. Each is ended
// as Revoke ends one (endEnrollment, actor "broker", an enrollment.expired audit row), so its
// grants are revoked and its pending requests cancelled and dropped from the approver's list,
// rather than waiting there for their own expiry with an approval that could grant nothing. The
// one lapse that revokes nothing is the lapse of an enrollment holding a named session's grants
// or pending requests, which goes dormant instead and is ended by EndCarried: a renewer that
// stopped is what a replaced box looks like from here, and the session is on another box by then.
//
// Lapsed means what Lookup means by not live: lease_expires_at no later than Postgres's now(), and
// not a lease that ran out while no broker was serving (enrollment_lease_live, migration 0019),
// which is never ended for that: the next ServingClock beat gives it the time back. A session that
// keeps renewing keeps its lease ahead of now() and is never selected, and Renew refuses an
// enrollment once its lease has lapsed, so ending one takes nothing a session could still use. Rows
// a concurrent Renew or Revoke has locked are skipped (the next sweep sees them as they are then),
// and an ended row has revoked_at set and is never selected again, so a sweep that finds nothing
// changes nothing. It works in batches of lapsedBatch, each one transaction, and returns what it
// ended.
func (s *Service) EndLapsed(ctx context.Context) ([]LapsedEnrollment, error) {
	var ended []LapsedEnrollment
	for {
		batch, err := s.endLapsedBatch(ctx)
		ended = append(ended, batch...)
		if err != nil || len(batch) < lapsedBatch {
			return ended, err
		}
	}
}

func (s *Service) endLapsedBatch(ctx context.Context) ([]LapsedEnrollment, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `select id, kind, runtime_id, slot, lease_expires_at from enrollments
		where revoked_at is null and lease_expires_at <= now() and not enrollment_lease_live(lease_expires_at)
		order by lease_expires_at limit $1 for update skip locked`, lapsedBatch)
	if err != nil {
		return nil, err
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LapsedEnrollment, error) {
		var e LapsedEnrollment
		err := row.Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Slot, &e.LeaseExpires)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	for i := range batch {
		ended, err := endLapsed(ctx, tx, batch[i].ID)
		if err != nil {
			return nil, err
		}
		batch[i].GrantsRevoked, batch[i].RequestsCancelled, batch[i].Dormant, batch[i].HandedTo = ended.GrantsRevoked, ended.RequestsCancelled, ended.Dormant, ended.HandedTo
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return batch, nil
}

// DormantEnrollment is one dormant enrollment EndCarried ended for good.
type DormantEnrollment struct {
	ID                string
	Kind              string
	RuntimeID         string
	Slot              string
	SessionID         string
	DormantSince      time.Time
	GrantsRevoked     int64
	RequestsCancelled int
}

// carriedBatch is how many dormant enrollments EndCarried ends in one transaction.
const carriedBatch = 100

// EndCarried ends, for good, every dormant enrollment that has been dormant for s.Carry or
// longer: it revokes the grants and cancels the pending requests that were left for the session
// to take (endEnrollment's terminal path, actor "broker"), with an enrollment.carry_expired audit
// row naming why. A Carry of 0 ends every dormant enrollment the next sweep finds, which is how a
// deployment turns the carry off.
//
// Dormant is dormant_since is not null: an ended enrollment still holding what the next
// enrollment of its session name may take (endEnrollment). The column returns to null the moment
// the row holds nothing more for anyone, here or in the adoption, so nothing is ended twice and
// a sweep that finds nothing changes nothing. An enrollment is dormant only after it ended, so
// nothing a live session could still use is ever taken here: what passes is the window a person's
// approval waits in, not a lease.
//
// It follows EndLapsed's shape, for the same reasons: batches of carriedBatch, each one
// transaction, rows a concurrent writer has locked skipped for the next sweep, and what it ended
// returned so the sweeper can log each one.
func (s *Service) EndCarried(ctx context.Context) ([]DormantEnrollment, error) {
	var ended []DormantEnrollment
	for {
		batch, err := s.endCarriedBatch(ctx, s.Carry)
		ended = append(ended, batch...)
		if err != nil || len(batch) < carriedBatch {
			return ended, err
		}
	}
}

func (s *Service) endCarriedBatch(ctx context.Context, carry time.Duration) ([]DormantEnrollment, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `select id, kind, runtime_id, slot, coalesce(session_id,''), dormant_since from enrollments
		where dormant_since is not null and dormant_since <= now() - make_interval(secs => $1)
		order by dormant_since limit $2 for update skip locked`, carry.Seconds(), carriedBatch)
	if err != nil {
		return nil, err
	}
	batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DormantEnrollment, error) {
		var e DormantEnrollment
		err := row.Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Slot, &e.SessionID, &e.DormantSince)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	for i := range batch {
		ended, err := endEnrollment(ctx, tx, batch[i].ID, "broker", "enrollment.carry_expired",
			"no enrollment of its session took what it held before the carry window passed", endTerminal)
		if err != nil {
			return nil, err
		}
		batch[i].GrantsRevoked, batch[i].RequestsCancelled = ended.GrantsRevoked, ended.RequestsCancelled
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return batch, nil
}

// Renewal is what Renew answers: the new lease, and, for a pod enrollment renewed without its
// projected token, which pod and slot did so (the API logs each such renewal).
type Renewal struct {
	LeaseExpires    time.Time
	WithoutPodToken bool
	RuntimeID, Slot string
}

// verifyPodToken verifies a pod's projected service-account token (issuer, audience, signature,
// expiry) and requires it to name a pod UID. Create and Renew both call it before any transaction
// opens, so the issuer's key-set fetch on an unknown key id never holds a row lock or a pooled
// connection. A token past its expiry is ErrPodTokenExpired, naming that expiry; every other
// failure, and a broker with no BROKER_K8S_OIDC_ISSUER, is ErrPodIdentity.
func (s *Service) verifyPodToken(ctx context.Context, raw string) (PodClaims, error) {
	if s.Pod == nil {
		return PodClaims{}, fmt.Errorf("%w: a pod's token needs BROKER_K8S_OIDC_ISSUER", ErrPodIdentity)
	}
	claims, err := s.Pod.Verify(ctx, raw)
	if errors.Is(err, oidc.ErrExpired) {
		return PodClaims{}, fmt.Errorf("%w (%v)", ErrPodTokenExpired, err)
	}
	if err != nil || claims.PodUID == "" {
		return PodClaims{}, ErrPodIdentity
	}
	return claims, nil
}

// Renew extends a live enrollment's lease by Lease. The caller's session proof says only that it
// holds the enrollment's key; a pod enrollment's renewal proves only that its renewer holds an
// unexpired token the issuer minted for that pod: no API-server call proves the pod still exists,
// so a deleted pod's enrollment can outlive it for the token's remaining life (at most 3600
// seconds) plus one lease. podToken is verified as Create verifies it (verifyPodToken, before the
// row is locked) and required to name the pod UID (runtime_id) and the service-account subject the
// enrollment recorded at Create, else ErrPodIdentity and the lease stays where it was. A token from
// another pod or account never renews this one. A machine session renews on its proof alone; a
// token on its renewal is ErrPodIdentity.
//
// A machine's own session renews only while the launcher credential that enrolled it is still
// live — ErrMachineLoginEnded otherwise — which is the one thing a session proof cannot say:
// the key and the enrollment id a machine's helper holds are files on its disk, and a copy of
// the pair taken off the box would otherwise renew for ever, long after the box was deleted or
// its fleet dropped from BROKER_VM_SERVICES. "Live" is what AuthenticateLauncher means by it,
// read in this transaction: not revoked, not past its expiry — which is the machine's Google
// identity token's, so a machine that stops being able to prove its identity
// stops renewing within the hour, as requiring a fresh identity token on every renewal once
// did — and its account still listed for its service. A revoke ends such a session outright
// (RevokeCredential), so what this adds is the two endings a revoke is not: the expiry, and the
// delisting. The session ends one lease later at most, swept as any lapsed one is (EndLapsed).
//
// A person's session and a pod's are deliberately not held this way. A pod's renewal already
// proves the pod's own projected token, and a person's sessions must outlive the hour their
// box's credential lives: a person signs in once per box and their sessions are theirs until
// they revoke the sign-in, which ends them at once (RevokePersonSignIn).
//
// The ratchet is keyed on the pod UID. Every pod enrollment starts with pod_token_renewed_at null,
// whether migration 0014 found it or Create made it, and the first renewal carrying an accepted
// token sets that row's column, the record that the pod's renewer sends its token. A renewal
// without a token is ErrPodTokenRequired once any pod enrollment naming the same runtime_id, live or
// ended, has the column set (podTokenRatcheted), whichever launcher credential made that row or this
// one; otherwise it still extends the lease and answers WithoutPodToken, which the API logs, naming
// the enrollment. So the copied-token gap closes for every pod UID a current-image pod has renewed
// under: a key enrolled under another login with a token copied out of that pod gets a row of its
// own (rows are unique per launcher credential, pod UID and slot), and that row is refused
// token-less as soon as any enrollment of the real pod has renewed with its token, whichever came
// first, and stays refused across the pod's role generations and after the pod is gone. What stays
// open: a copied token's renewals before the pod's own first token renewal (seconds while the
// daemon holds an approved machine login; after a daemon restart its pods enroll only once a person
// approves the new login, and a pod that ends before it enrolls never marks its UID), and every
// renewal of an old-image pod, whose renewer never sends a token, until the contract release. A pod
// UID names one pod, so a mark refuses nothing of another. Every deploy order keeps every pod
// renewing: an older broker ignores the token a newer client sends, and this broker accepts a
// token-less renewal of a pod UID no enrollment has marked. That bounds liveness, not exposure: a
// grant lasts until its session ends, so a copy renewing an unmarked UID keeps its grant while it
// renews, which the operate page's rollout closes by replacing old-image pods and revoking the
// logins of the copies left.
// The contract release removes the token-less path once
// `select count(*) from enrollments where kind='pod' and revoked_at is null and pod_token_renewed_at is null and created_at < now() - interval '1 minute'`
// answers 0, the broker has logged no "renewed without its projected token" line for one full
// BROKER_LEASE_SECONDS, and no issue pod runs a worker image older than the one whose renewer sends
// the token (an old-image issue pod between role generations holds no enrollment, so the count and
// the log miss it, yet its next phase enrolls an unmarked row). That release also drops
// podTokenRatcheted and its partial index, which only the ratchet needs.
func (s *Service) Renew(ctx context.Context, id, podToken string) (Renewal, error) {
	withToken := podToken != ""
	var claims PodClaims
	if withToken {
		var err error
		if claims, err = s.verifyPodToken(ctx, podToken); err != nil {
			return Renewal{}, err
		}
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return Renewal{}, err
	}
	defer tx.Rollback(ctx)

	var kind string
	var subject *string
	var cred Credential
	var credLive bool
	renewal := Renewal{}
	err = tx.QueryRow(ctx, `select e.kind, e.runtime_id, e.slot, e.subject,
			c.operator, c.service, c.subject, c.revoked_at is null and c.expires_at > now()
		from enrollments e join launcher_credentials c on c.id = e.launcher_credential_id
		where e.id=$1 and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at) for update of e`, id).
		Scan(&kind, &renewal.RuntimeID, &renewal.Slot, &subject, &cred.Operator, &cred.Service, &cred.Subject, &credLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return Renewal{}, ErrNotLive
	}
	if err != nil {
		return Renewal{}, err
	}
	// The row's own half of "live" is credLive, read above; what is left is the configuration's
	// half, the account still listed for the credential's service, which no column holds. cred
	// is one machineCredential has answered for, so its subject and its service are both set.
	if kind == "session" && machineCredential(cred) &&
		!(credLive && s.tokenLoginListed(*cred.Subject, *cred.Service)) {
		return Renewal{}, ErrMachineLoginEnded
	}
	switch {
	case kind != "pod" && withToken:
		return Renewal{}, ErrPodIdentity
	case kind == "pod" && withToken:
		if claims.PodUID != renewal.RuntimeID || subject == nil || claims.Subject != *subject {
			return Renewal{}, ErrPodIdentity
		}
	case kind == "pod":
		ratcheted, err := podTokenRatcheted(ctx, tx, renewal.RuntimeID)
		if err != nil {
			return Renewal{}, err
		}
		if ratcheted {
			return Renewal{}, ErrPodTokenRequired
		}
		renewal.WithoutPodToken = true
	}
	renewal.LeaseExpires = time.Now().Add(s.Lease).Truncate(time.Microsecond) // as stored, as in Create
	if _, err := tx.Exec(ctx, `update enrollments set lease_expires_at=$2,
		pod_token_renewed_at = case when $3 then coalesce(pod_token_renewed_at, now()) else pod_token_renewed_at end
		where id=$1`, id, renewal.LeaseExpires, kind == "pod" && withToken); err != nil {
		return Renewal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Renewal{}, err
	}
	return renewal, nil
}

// podTokenRatcheted reports whether any pod enrollment naming podUID has renewed with its token
// (pod_token_renewed_at set), live or ended. A pod UID is the API server's uid of one pod object and
// is never given to another pod, so every row naming it is that one pod's or a copy of its token,
// and a mark refuses nothing of any other pod. Ended rows count on purpose. A pod's enrollments end
// at every role generation (the daemon revokes each one it lets go), so between generations the pod
// can hold no live row at all; were only live rows read, a key enrolled under another login with a
// token copied out of the pod would renew token-less again in every such gap, and forever once the
// pod is gone, since a token-less renewal needs no token and the copied token's expiry stops
// mattering after Create. A later enrollment of the same pod (a new role generation, or under the
// daemon's next login) is the pod's own current-image renewer, which sends its token and is never
// refused. The partial index enrollments_pod_token_renewed (migration 0014) serves the probe.
func podTokenRatcheted(ctx context.Context, tx pgx.Tx, podUID string) (bool, error) {
	var ratcheted bool
	err := tx.QueryRow(ctx, `select exists(select 1 from enrollments
		where kind = 'pod' and runtime_id = $1 and pod_token_renewed_at is not null)`, podUID).Scan(&ratcheted)
	return ratcheted, err
}

// PersonPodLogin reports whether cred is a person's pod login: a token login of a Kubernetes
// ServiceAccount that carries an operator. Only machine.Service.TokenLogin writes one, for a
// pod-bound token of an account that BROKER_POD_OPERATORS maps to a person. The answer reads the
// credential's row alone, so it holds whatever the setting says now. No other credential is one.
// A person's machine login has no subject. The subject of a person's sign-in reads
// exchange.SubjectSignIn. A machine's own credential has no operator.
//
// Each rule of this package that sets a person's pod login apart keys on this test. So no rule
// reads a stale one as a person's machine login. A stale one is a person's pod login whose entry
// the setting dropped, or moved to another person. Only livePersonPodLogin admits a person's pod
// login to anything.
func PersonPodLogin(cred Credential) bool {
	return cred.Operator != nil && cred.Subject != nil && exchange.ReadSubject(*cred.Subject).Kind == exchange.SubjectOther
}

// livePersonPodLogin reports whether cred is a person's pod login (PersonPodLogin) that the
// setting still admits: it names a pod, and BROKER_POD_OPERATORS maps its subject to its operator
// now. After the setting drops the entry or moves it to another person, the row keeps the first
// person until the pod logs in again. Until then, this answers false.
func (s *Service) livePersonPodLogin(cred Credential) bool {
	if !PersonPodLogin(cred) || cred.PodUID == "" {
		return false
	}
	owner := s.PodOperator(cred.Subject)
	return owner != "" && owner == *cred.Operator
}

// PodOperator answers the person that BROKER_POD_OPERATORS maps subject to now, in
// record.CanonicalLogin's form. It answers "" for no subject and for a subject that the setting
// does not map. A caller that compares the answer with an operator tests for "" first.
// liveCredentials compares it with a person only after it refuses an empty person. So an
// unmapped subject never matches an operator or a person. machine.Service reads it as the person
// of a token login (ServiceAccount.Person), where "" is no person.
func (s *Service) PodOperator(subject *string) string {
	if subject == nil {
		return ""
	}
	return record.CanonicalLogin(s.PodOperators[*subject])
}

// authorized reports whether cred may act on an enrollment of the given kind and operator. It is
// the trust boundary that both Create (when it creates an enrollment) and Revoke (when it ends
// one) enforce. A credential is of one of three kinds:
//
//   - A person's pod login (PersonPodLogin) may act only on a pod enrollment of its own operator,
//     and only while livePersonPodLogin holds. It may act on no session. So it never enrols under
//     the session name of its person's box, and never takes that session's grants (adopt). Once
//     BROKER_POD_OPERATORS no longer maps its subject to its operator, it may act on nothing.
//   - Any other operator credential, a person's own machine login or that person's sign-in on a
//     machine, may only act on that same operator's own sessions, and never on a pod.
//   - A service credential (no operator) may only act on an enrollment that carries no operator
//     either, a pod's or a machine's own session.
//
// A machine's credential, which the token exchange mints, names its fleet's service and never a
// person, whatever that fleet's entry says: secret authority follows the access token, so the
// machine's sessions reach its service's secrets and no person's, and a person reaches theirs on
// that machine only through their own sign-in. Its session is the one kind a service credential
// may enrol beside a pod, and only a machine's credential may: a Kubernetes service's login
// enrols the pods its tokens prove and nothing else.
func (s *Service) authorized(cred Credential, kind string, operator *string) bool {
	if PersonPodLogin(cred) {
		return kind == "pod" && operator != nil && *operator == *cred.Operator && s.livePersonPodLogin(cred)
	}
	serviceKind := kind == "pod" || (kind == "session" && machineCredential(cred))
	switch {
	case cred.Operator != nil && (operator == nil || *operator != *cred.Operator):
		return false
	case cred.Operator != nil && serviceKind:
		return false
	case cred.Operator == nil && (!serviceKind || operator != nil):
		return false
	}
	return true
}

// podUnderTokenLogin holds a token login of a Kubernetes account to the pods the deployment's
// settings give it, and lets every other credential through. Such a login is a service's login,
// or a person's pod login (PersonPodLogin). A token login is authorized by those settings and by
// nothing else — no person approved it — so a projected token handed to it proves whose pod the
// token is and never who may enrol that pod. There are two pods it may enrol:
//
//   - its own, when the token proves the login's own account (cred.Subject). Only a pod running
//     as that account holds such a login, and the login belongs to the one pod whose token made
//     it (Credential.PodUID, from its kubernetes.io pod uid), so it enrols that pod and no other
//     pod of the account. A login from a token
//     bound to no pod has no pod of its own and so enrols none of them.
//     Such an enrolment also states no session name (ErrPodSelfEnrollNoSession, whose own doc
//     says why), and no claim either (ErrInvalidClaim), for the same reason: a claim it picked
//     for itself would make it a sibling pod's owner (ownerKey), reading that sibling's requests
//     and taking what it left (adopt). The pod UID is read first, so a request with both faults —
//     another pod of the account, under a session name — is ErrPodSelfEnrollOnly.
//
//   - a pod of an account BROKER_SERVICES binds to the login's own service, which is the
//     deployment saying that service launches those pods. That is how the Legion daemon enrols
//     its workers, and it is the same fact requests.Machine reads to decide an enrolled pod is
//     that service's, so nothing new has to be configured for a deployment that already serves a
//     service's secrets to its pods. It states a session name and a claim as before: the
//     launcher names the session each pod it starts runs, and what it runs for, and both are
//     the launcher's own to give.
//
// A person's pod login takes the first rule alone, and it never reads BROKER_SERVICES. It reads
// the pod's account first: a pod of any other account is ErrPodSelfEnrollOnly, which names the
// login's account and the pod's account. Then it reads the pod UID and the session name, as the
// first rule does.
//
// A credential a person approved — a person's own machine login, or a service's login approved
// through a machine-login code, neither of which carries a subject — keeps that person's approval
// as its authorization and is not held to either rule, with one exception: it states no claim
// (ErrInvalidClaim). No token proved whose launcher it is, so a claim it stated would be any
// claim of its service, and the next pod of that claim would take what a dormant pod of it left
// (adopt). The Legion daemon logs in with its own token, so its login carries a subject. A
// person's sign-in on a machine carries an operator and a subject of its own kind
// (exchange.SubjectSignIn). Neither rule holds it, and authorized has already refused it a pod.
//
// It reads three fields of the enrollment, its runtime id, its session name and its claim, and
// takes the enrollment itself rather than the values, so no caller can pair one request's pod UID
// with another's session name.
func (s *Service) podUnderTokenLogin(cred Credential, claims PodClaims, in Enrollment) error {
	if in.Claim != "" && cred.Subject == nil {
		return fmt.Errorf("%w: only a launcher whose own token proved its login states a claim", ErrInvalidClaim)
	}
	person := PersonPodLogin(cred)
	if cred.Subject == nil || (cred.Operator != nil && !person) {
		return nil
	}
	own := ownPod(cred, claims.Subject)
	if person && !own {
		return fmt.Errorf("%w: this login is account %s's, the enrollment's pod runs as %s", ErrPodSelfEnrollOnly, *cred.Subject, claims.Subject)
	}
	if own {
		if cred.PodUID != in.RuntimeID {
			return fmt.Errorf("%w: this login is pod %q's, the enrollment names %q", ErrPodSelfEnrollOnly, cred.PodUID, in.RuntimeID)
		}
		if in.SessionID != nil && *in.SessionID != "" {
			return ErrPodSelfEnrollNoSession
		}
		if in.Claim != "" {
			return fmt.Errorf("%w: a pod enrolling itself states no claim", ErrInvalidClaim)
		}
		return nil
	}
	if !s.launches(cred.Service, claims.Subject) {
		service := ""
		if cred.Service != nil {
			service = *cred.Service
		}
		return fmt.Errorf("%w: BROKER_SERVICES does not bind %s to %q, the service %s logs in as", ErrPodNotLaunched, claims.Subject, service, *cred.Subject)
	}
	return nil
}

// launches reports whether BROKER_SERVICES binds podAccount, the ServiceAccount a pod runs as, to
// service, the one a login logs in as: the deployment's own statement that this service launches
// that pod. The one lookup, read by Create through podUnderTokenLogin and by Revoke through
// sameLauncher, so a pod a login may enrol is exactly a pod it may end.
func (s *Service) launches(service *string, podAccount string) bool {
	if service == nil {
		return false
	}
	bound, listed := s.ServicesByAccount[podAccount]
	return listed && bound == *service
}

// ownPod reports whether podAccount, the ServiceAccount a pod runs as, is cred's own login
// subject: a pod of the login's own account, which is a pod enrolling itself rather than one a
// launcher enrols. The one comparison, read by podUnderTokenLogin at Create and by Revoke's
// own-account branch, so no other rule has to tell a pod that enrolled itself from a pod a
// launcher enrolled.
func ownPod(cred Credential, podAccount string) bool {
	return cred.Subject != nil && *cred.Subject == podAccount
}

// sameLauncher reports whether cred is the same launcher as the credential that made a pod
// enrollment, logging in again from another pod: the same ServiceAccount (both token logins of
// one subject), the same service, and a pod running as an account BROKER_SERVICES binds to that
// service — the launcher relation Create already reads (launches). It is what lets a replaced
// Legion daemon pod end the enrollments its predecessor made, since a machine login is per pod
// and the new pod holds a credential of its own.
//
// It decides launchers and nothing else. A pod enrollment of the login's own account never
// reaches it: Revoke's own-account branch answers that case itself (ownPod), whatever
// BROKER_SERVICES binds the account to, and reads this only for the enrollments that branch
// leaves. It gives nothing across services either. It gives nothing to a credential a person
// approved, which carries no subject, and nothing to a credential with an operator. So a
// person's pod login ends no pod enrollment that another login of its person made (Revoke).
// enrolledLoginSubject, enrolledLoginService and enrolledPodAccount are the enrolling
// credential's subject and service and the enrollment's own verified pod account, each nil where
// the row has none.
func (s *Service) sameLauncher(cred Credential, enrolledLoginSubject, enrolledLoginService, enrolledPodAccount *string) bool {
	if cred.Operator != nil || cred.Subject == nil || enrolledLoginSubject == nil || enrolledPodAccount == nil {
		return false
	}
	if *enrolledLoginSubject != *cred.Subject {
		return false
	}
	if cred.Service == nil || enrolledLoginService == nil || *enrolledLoginService != *cred.Service {
		return false
	}
	return s.launches(cred.Service, *enrolledPodAccount)
}

// machineCredential reports whether cred is one the token exchange minted for a machine: a
// credential with a service, no person and a subject that reads as a machine's
// (exchange.SubjectMachine). A Kubernetes token login holds a subject of its own shape, and a
// person's pod login (PersonPodLogin) holds an operator as well. A person's sign-in on a machine
// holds a subject of its own kind, and a person's machine login holds an operator.
func machineCredential(cred Credential) bool {
	if cred.Operator != nil || cred.Service == nil || cred.Subject == nil {
		return false
	}
	return exchange.ReadSubject(*cred.Subject).Kind == exchange.SubjectMachine
}

// Revoke ends the enrollment — revoking every live grant and cancelling every pending request
// under it — in one transaction. The one exception is the carry: an enrollment naming a session
// that still holds a live grant or a pending request goes dormant instead, holding both for that
// session's next enrollment (endEnrollment, endMayCarry). This is a launcher ending its own
// enrollment, which happens on every move: the helper revokes when a session's process ends, and
// the daemon revokes when it lets a role's process go
// (packages/daemon/internal/supervise/secrets.go, RevokeEnrollment), while the session itself
// comes up again on the next box.
//
// cred must be authorized for the target enrollment's own kind and
// operator — the same trust boundary Create enforces — checked before anything else, so a
// wrong-operator or wrong-kind caller can never revoke an enrollment it doesn't own, whether that
// enrollment is live, already revoked, or (were its id guessed rather than read back) merely
// plausible-looking. A row that does not exist at all has no operator/kind to check ownership
// against, so that case alone falls through to ErrNotLive below rather than ErrOperatorMismatch.
//
// A service's pod enrollment and a machine's own session carry no operator, so the operator
// cannot tell one service's sessions from another's. A pod enrollment that a person's pod login
// made carries its person, and that operator cannot tell one of the person's pods from another.
// Which credential ends one of these is decided in the two branches below, and a pod enrollment
// of the login's own account takes the first of them:
//
//   - such an enrollment ends under the credential that made it, and only while that credential
//     is the one its own pod holds: cred.PodUID is the enrollment's runtime id (ownPod, the
//     reading podUnderTokenLogin makes of the same pair at Create). cred.PodUID is the caller's
//     own pod, because api's authLauncher reads the credential by launcher id and proved key
//     thumbprint together (CredentialForKey, whose doc gives the race a read by id alone would
//     lose). The enrolling-credential test alone refuses a pod whose own enrollment another
//     credential made — a service login a person approved, which enrols any pod — since that
//     case brings a matching pod uid. The pod uid refuses a sibling pod's enrollment on either
//     schema, and it is the only test that refuses one made while the two pods shared one
//     credential row. So a session pod of an account both settings name ends its own
//     enrollment and no other pod's of that account, on either schema.
//
//   - every other enrollment these branches reach ends under the credential that made it, and a
//     pod enrollment under the same launcher logging in again from another pod as well
//     (sameLauncher): never another service's credential, nor a second credential of the same
//     service, nor another machine of the same fleet (ErrNotEnrollingCredential for every other
//     caller). A launcher's machine login is per pod (IssueTokenLoginTx), so a restarted Legion
//     daemon in the pod it already runs in keeps its credential, while a replaced daemon pod
//     holds a credential of its own rather than its predecessor's; without sameLauncher one
//     deploy of the daemon would leave every enrollment its previous pods made with nothing able
//     to end it. It gives the new credential its predecessor's pods and no others, since it
//     takes the same ServiceAccount logging in and the pod's own account being one
//     BROKER_SERVICES binds to that login's service — the launcher relation POD_NOT_LAUNCHED
//     reads at Create.
//
// A person's pod login (PersonPodLogin) is held to the same two branches. authorized has already
// refused it every enrollment but a pod enrollment of its own person, and refused a stale one
// everything. So the first branch lets it end only the pod enrollment that it made of its own
// pod, and never a sibling pod's, on either schema. The second branch gives it nothing that
// another credential made, since sameLauncher admits no credential with an operator. So it never
// ends a pod of another mapped account of the same person. It never ends a box session either,
// since authorized refuses it every session.
//
// An enrollment neither branch reaches — a service login a person approved before token logins,
// or the daemon's credential before someone revoked it — ends when its pod stops renewing, one
// lease (BROKER_LEASE_SECONDS) after its renewer stops, or at once when a person revokes that
// credential (RevokeCredential). A box or host enrollment keeps the operator rule alone: a helper
// that logs in again revokes the enrollments its earlier credential made for the same operator.
//
// Once ownership passes, Revoke returns ErrNotLive, changing nothing, when the enrollment is
// already revoked — the guard that keeps the audit trail honest: without it a no-op call would
// still write an "enrollment.revoked" audit row for an enrollment that never transitioned.
func (s *Service) Revoke(ctx context.Context, cred Credential, id, by string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var operator, enrolledSubject, enrolledLoginSubject, enrolledLoginService *string
	var kind, enrolledRuntimeID string
	var enrolledBy uuid.UUID
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `select e.operator, e.kind, e.launcher_credential_id, e.revoked_at, e.subject, e.runtime_id, c.subject, c.service
		from enrollments e join launcher_credentials c on c.id = e.launcher_credential_id
		where e.id=$1 for update of e`, id).
		Scan(&operator, &kind, &enrolledBy, &revokedAt, &enrolledSubject, &enrolledRuntimeID, &enrolledLoginSubject, &enrolledLoginService)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotLive
	}
	if err != nil {
		return err
	}
	if !s.authorized(cred, kind, operator) {
		return ErrOperatorMismatch
	}
	// The two branches the doc above describes. They hold a credential with no operator and a
	// person's pod login. Any other credential with an operator, a person's own machine login or
	// sign-in, is held to neither: the operator test above is its whole ownership rule.
	if cred.Operator == nil || PersonPodLogin(cred) {
		if kind == "pod" && enrolledSubject != nil && ownPod(cred, *enrolledSubject) {
			if cred.PodUID != enrolledRuntimeID || enrolledBy != cred.ID {
				return ErrNotEnrollingCredential
			}
		} else if enrolledBy != cred.ID &&
			!(kind == "pod" && s.sameLauncher(cred, enrolledLoginSubject, enrolledLoginService, enrolledSubject)) {
			return ErrNotEnrollingCredential
		}
	}
	if revokedAt != nil {
		return ErrNotLive
	}
	if _, err := endEnrollment(ctx, tx, id, by, "enrollment.revoked", "revoked by its launcher", endMayCarry); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LiveCredentials lists the machine logins person may list and revoke that can still reach a
// secret, newest first: every unrevoked launcher credential minted from a launcher_credential
// record, a service's whoever approved it, a person's own machine's only when it is the person
// whose record names them, every service's token login (machine.Service.TokenLogin), and a token
// login of an account BROKER_POD_OPERATORS maps only when it maps that account to person, while it
// is unexpired or a session it enrolled is live as Lookup means it: not revoked, its lease not
// lapsed. A login's sessions outlive its expiry, since each renews with its own key and Renew
// reads the credential only for a machine's own session, so an expired login stays listed,
// Expired set, until its last session ends or lapses (a lapsed session renews no more, swept or
// not): revoking every listed login ends every session the person's machines started. A
// machine's own login is the exception its sessions make: they stop renewing at its expiry and
// lapse within one lease. A service's login, such as the Legion daemon's, has no
// operator and acts as no person, so anyone signed in to Dispatch lists it and revokes it, its
// Service set. Each login names who approved it, the login its approved event records, or "" for a
// token login, which no person approved. The sentinel record.AnyoneApprover and an empty login list
// nothing. OwnLiveCredentials lists the person's own machines' and pod logins alone.
//
// A token login whose subject BROKER_POD_OPERATORS maps is its person's own machine login, with
// or without a pod and an operator on its row. It is listed to that person alone, here and by
// OwnLiveCredentials, and to nobody else, whatever its row's operator. Its Person names that
// person, so a page can show the login as the person's pod and not as a service's login. Every
// other login's Person is nil. The list reads the setting as it is now and not the row, because
// the row can lose its person. On the schema this release ships, every pod of one account shares
// one row (IssueTokenLoginTx). A login of the account from a token bound to no pod moves that row
// to no operator and no pod. Read by the row, the login would then be every person's to revoke,
// and that revoke would end the person's pod enrollments (endCredential).
//
// A person's sign-in on a machine holds a launcher credential too, and that one is never listed
// here: it is one person's, it is already listed as that person's sign-in
// (store.Store.PersonSignIns), and ending it alone would end nothing, since the sign-in row
// behind it mints it again at the box's next trade. Its subject says so
// (exchange.PersonSubject), which is what signInCredential reads.
func (s *Service) LiveCredentials(ctx context.Context, person string) ([]LiveCredential, error) {
	return s.liveCredentials(ctx, person, true)
}

// OwnLiveCredentials is LiveCredentials without any service's login: the logins of the person's
// own machines, those minted from a launcher_credential record that names them, and the token
// logins of the accounts that BROKER_POD_OPERATORS maps to the person, as the person's own
// machine lists them (GET /v1/operator/machines).
func (s *Service) OwnLiveCredentials(ctx context.Context, person string) ([]LiveCredential, error) {
	return s.liveCredentials(ctx, person, false)
}

// signInCredential reports whether a launcher credential's subject is a person's sign-in on a
// machine rather than a machine's own identity or a Kubernetes account: the one fact that tells
// the sign-ins list's credentials from the machine-logins list's, written by
// exchange.PersonSubject and read back by exchange.ReadSubject. nil is a credential a person
// approved, which has no subject at all.
func signInCredential(subject *string) bool {
	if subject == nil {
		return false
	}
	return exchange.ReadSubject(*subject).Kind == exchange.SubjectSignIn
}

// podAccounts answers the subjects BROKER_POD_OPERATORS maps to person, a login as
// record.CanonicalLogin spells it (mine), and every subject it maps (mapped).
func (s *Service) podAccounts(person string) (mine, mapped []string) {
	mine, mapped = []string{}, make([]string, 0, len(s.PodOperators))
	for subject := range s.PodOperators {
		mapped = append(mapped, subject)
		if s.PodOperator(&subject) == person {
			mine = append(mine, subject)
		}
	}
	return mine, mapped
}

// liveCredentials is LiveCredentials, every service's login among them only when services is set.
// It gives the statement two arrays: the subjects that BROKER_POD_OPERATORS maps to person ($3),
// and every subject that the setting maps ($4) (podAccounts). So one statement holds the whole
// rule. A credential with no subject matches neither array, and the rule of an unmapped credential
// decides it.
func (s *Service) liveCredentials(ctx context.Context, person string, services bool) ([]LiveCredential, error) {
	person = record.CanonicalLogin(person)
	if person == "" || person == record.AnyoneApprover {
		return nil, nil
	}
	mine, mapped := s.podAccounts(person)
	// c.subject = any(...) is null for a credential with no subject, and coalesce reads that
	// null as "not mapped". So a login a person approved, which has no subject, keeps the rule of
	// an unmapped credential.
	rows, err := s.Store.Pool.Query(ctx, `select c.id, c.host, c.service, c.subject,
			coalesce((select ev.login from credential_request_events ev where ev.record_id = r.id and ev.event = 'approved'), ''),
			c.created_at, c.expires_at, c.expires_at <= now() from launcher_credentials c
		left join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where (coalesce(c.subject = any($3::text[]), false)
				or (not coalesce(c.subject = any($4::text[]), false)
					and ((c.service is null and r.approver = $1) or ($2 and c.service is not null and (r.id is not null or c.subject is not null)))))
			and c.revoked_at is null
			and (c.expires_at > now() or exists (select 1 from enrollments e where e.launcher_credential_id = c.id and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at)))
		order by c.created_at desc, c.id`, person, services, mine, mapped)
	if err != nil {
		return nil, err
	}
	live, err := pgx.CollectRows(rows, pgx.RowToStructByPos[LiveCredential])
	if err != nil {
		return nil, err
	}
	// A sign-in's credential names its fleet's service, so the query above would take it for a
	// service's login. Which list a credential belongs on is its subject's to say, and the
	// subject is read here rather than matched in SQL so that exchange.PersonSubject's shape is
	// written down once.
	live = slices.DeleteFunc(live, func(c LiveCredential) bool { return signInCredential(c.Subject) })
	// A login whose subject the setting maps is in this list only as person's own ($3), so its
	// Person is person. It is read from the setting, as the statement's arrays are, and not from
	// the row's operator, which can name no one or a person the setting no longer names.
	for i := range live {
		if owner := s.PodOperator(live[i].Subject); owner != "" {
			live[i].Person = &owner
		}
	}
	return live, nil
}

// RevokeCredential ends launcher credential id on the word of person, expired or not: from then on
// no launcher proof signed with it authenticates (AuthenticateLauncher), and every enrollment it
// made that has not ended — a person's machine sessions and boxes, every pod a service's login
// enrolled, or the pods a person's pod login enrolled, which outlive the credential's expiry — and
// every dormant one it made, holding a
// moved session's grants, is ended terminally (endEnrollment, an
// enrollment.revoked audit row), revoking each one's grants and cancelling its pending requests,
// all in one transaction with one launcher_credential.revoked audit row. person may revoke a
// service's login whoever approved it, a service's token login (which no person approved, and whose
// service logs in again with its token under a new credential unless BROKER_SERVICE_LOGINS no
// longer names its account), a person's own machine's only as the approver of the
// launcher_credential record it was minted from, and a token login of an account that
// BROKER_POD_OPERATORS maps only as the person it maps that account to (ErrNotApprover); the
// sentinel record.AnyoneApprover and an empty login revoke nothing. An unknown id, like a
// credential with neither a record nor a token login's subject, is ErrNoCredential, and so is a
// person's sign-in's credential, which is no machine login: it is ended whole, with the sign-in
// row that mints it, by RevokePersonSignIn, and ending it here would leave that row to mint it
// again at the box's next trade. actor
// is what every row it writes records as having done it, which the caller proved:
// "human:<person>" for a revoke Dispatch relays from its signed-in person,
// "launcher:<credential id>" for one the person's own machine login makes. Revoking a credential
// already revoked succeeds and changes nothing. It takes the
// credential's row before its enrollments' rows, so an enrollment Create is inserting under the
// credential (which holds the row for share) commits first and is ended here, or waits and finds
// the credential revoked; and it takes those enrollments' rows, so a launcher's own Revoke of one
// either commits first, leaving it out of the list here, or waits and finds it ended.
//
// A token login whose subject BROKER_POD_OPERATORS maps is its person's own machine login,
// whatever its row's operator, as LiveCredentials lists it. Only that person revokes it, and
// RevokeOwnCredential revokes it too. The pod logs in again with its token under a new
// credential while BROKER_SERVICE_LOGINS names its account (machine.Service.verifyKubernetes);
// BROKER_POD_OPERATORS decides only whether that new login acts for the person. So the revoke
// ends the enrollments that the login made, but it does not keep the pod out.
func (s *Service) RevokeCredential(ctx context.Context, id, person, actor string) error {
	return s.revokeCredential(ctx, id, person, actor, true)
}

// RevokeOwnCredential is RevokeCredential for the logins OwnLiveCredentials lists: a service's
// login is ErrNoCredential, as an unknown id is, and ends nothing. A token login whose subject
// BROKER_POD_OPERATORS maps is its person's own login and no service's, so this revokes it as
// RevokeCredential does.
func (s *Service) RevokeOwnCredential(ctx context.Context, id, person, actor string) error {
	return s.revokeCredential(ctx, id, person, actor, false)
}

// revokeCredential is RevokeCredential, a service's login revocable only when services is set.
func (s *Service) revokeCredential(ctx context.Context, id, person, actor string, services bool) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var approver, host string
	var service, subject *string
	var revokedAt *time.Time
	// A token login's credential has no record, so no approver. Anyone signed in may revoke a
	// service's token login, as for every service's login (record.MayDecide). Its service logs in
	// again with its token at its next call, under a new credential, unless BROKER_SERVICE_LOGINS
	// no longer names its account. A token login of a mapped account is the exception below.
	err = tx.QueryRow(ctx, `select coalesce(r.approver, ''), c.service, c.subject, c.host, c.revoked_at from launcher_credentials c
		left join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential'
		where c.id = $1 and (r.id is not null or c.subject is not null) for no key update of c`, id).Scan(&approver, &service, &subject, &host, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoCredential
	}
	if err != nil {
		return err
	}
	// A sign-in's credential names its fleet's service and no record, so the rule below would
	// read it as a service's login and let anyone signed in end it. It is one person's, it is
	// listed as that person's sign-in and not here (liveCredentials), and the revoke that means
	// anything is RevokePersonSignIn's, which ends the sign-in row with it; so this route does
	// not find it, exactly as it does not find a login of another kind than it lists.
	if signInCredential(subject) {
		return ErrNoCredential
	}
	var svc string
	if owner := s.PodOperator(subject); owner != "" {
		// BROKER_POD_OPERATORS maps the credential's subject, so the credential is its person's
		// own machine login, as liveCredentials lists it. The setting decides and the row does not:
		// a login from a token bound to no pod moves the row to no operator (LiveCredentials says
		// why that matters). The person is the approver and no service decides it. So anyone else
		// is ErrNotApprover, and RevokeOwnCredential finds it.
		approver = owner
	} else if service != nil {
		if !services {
			return ErrNoCredential
		}
		svc = *service
	}
	// A machine login's approver is never a group, so no group reader is asked.
	if !record.MayDecide(record.KindLauncherCredential, approver, svc, person, nil) {
		return ErrNotApprover
	}
	if revokedAt != nil {
		return nil
	}
	if _, err := endCredential(ctx, tx, id, host, service, actor, "its machine login was revoked"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetireCredential ends launcher credential id as the login a newer login of its machine replaced,
// in one transaction: its expiry becomes now, so its key enrolls nothing more and its refresh is
// refused, and its OAuth access and refresh tokens are deleted, so a retired refresh token
// presented later (an old copy of the machine file, a late answer to an abandoned refresh) is a
// plain invalid_grant rather than a reuse that would revoke the credential. Unlike a revoke it
// leaves every enrollment it made running: a session renews with its own proof, which an expired
// credential's sessions already do. The one exception is a machine's own session, whose renewal
// is held to a live login (Renew) and which therefore lapses within one lease of a retire; no
// machine's credential is ever retired, since a retire follows a device-grant machine login that
// a machine proving its own identity never makes.
// It writes one launcher_credential.retired audit row as actor.
// A credential already revoked or past its expiry changes nothing; an unknown id is
// ErrNoCredential. It takes the credential's row as a revoke does, so an enrollment Create holding
// it for share commits first.
func (s *Service) RetireCredential(ctx context.Context, id, actor string) error {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var host string
	var service *string
	var live bool
	err = tx.QueryRow(ctx, `select host, service, revoked_at is null and expires_at > now() from launcher_credentials
		where id = $1 for no key update`, id).Scan(&host, &service, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoCredential
	}
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	if _, err := tx.Exec(ctx, `update launcher_credentials set expires_at = now() where id = $1`, id); err != nil {
		return err
	}
	if err := oauthstore.DeleteCredentialTokens(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.retired',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'host',$3::text,'service',$4::text)))`, actor, id, host, service); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReuseActor is the actor RevokeReusedCredential records: the broker, on finding a rotated
// refresh token redeemed after its grace.
const ReuseActor = "broker:refresh-token-reuse"

// RevokeReusedCredential revokes launcher credential id in tx, as RevokeCredential does, because
// a refresh token of its machine login was redeemed after it had been rotated and its grace had
// passed: someone else holds a copy of the login, and the key it holds keeps enrolling sessions
// until the credential itself ends. Every row it writes names ReuseActor. It takes the
// credential's row first, as RevokeCredential does. A credential already revoked, or unknown,
// changes nothing. It is the broker's oauthstore.Store.Reused.
func RevokeReusedCredential(ctx context.Context, tx pgx.Tx, id string) error {
	var host string
	var service *string
	var revokedAt *time.Time
	err := tx.QueryRow(ctx, `select host, service, revoked_at from launcher_credentials where id = $1 for no key update`, id).Scan(&host, &service, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && revokedAt != nil {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = endCredential(ctx, tx, id, host, service, ReuseActor, "its machine login's refresh token was reused")
	return err
}

// RevokePersonSignIn ends the sign-in id of person — the sign-in `agent-secrets login` made on
// one box — and everything that sign-in still holds, in one transaction, and answers how many
// of that person's live access tokens it ended. An id no sign-in of that person has is
// store.ErrNoSignIn; a sign-in already revoked changes nothing and answers no error, as
// revoking a machine login twice does.
//
// Three things end together, because a person revoking a sign-in means all three and would be
// surprised by any one of them outliving the others: the row, so no later trade of that box key
// is answered (store.RevokePersonSignInTx, which refuses it by name); the access tokens already
// minted under it, so none goes on writing Dispatch for the rest of its life; and the launcher
// credential that sign-in's trades minted, so no launcher proof signed with the box key
// authenticates again (AuthenticateLauncher) and every session it enrolled ends with its grants
// and pending requests, exactly as revoking a machine login ends them (endCredential).
//
// The credential is found by the box key's thumbprint, which names it exactly: a live
// credential holds one key (launcher_credentials_live_key), and that key is the one
// `agent-secrets login` made for this sign-in and no other. A sign-in whose box has not traded
// since it was recorded holds no credential yet, and nothing is ended but the row.
func (s *Service) RevokePersonSignIn(ctx context.Context, id, person, actor string) (int, error) {
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	_, ended, err := endSignIn(ctx, tx, id, person, actor, "the person revoked this machine's sign-in")
	if err != nil {
		return 0, err
	}
	return ended, tx.Commit(ctx)
}

// endSignIn ends sign-in id of person in tx and everything it holds, as RevokePersonSignIn
// describes, writing actor and reason on every row it writes. It answers whether this call
// ended it (false for a sign-in already revoked, which changes nothing) and how many live access
// tokens ended with it: those its trades minted, and any other naming the credential it held.
// An id no sign-in of that person has is store.ErrNoSignIn.
func endSignIn(ctx context.Context, tx pgx.Tx, id, person, actor, reason string) (bool, int, error) {
	thumbprint, ended, err := store.RevokePersonSignInTx(ctx, tx, id, person)
	if err != nil || thumbprint == "" {
		return false, 0, err
	}
	more, err := endKeyCredential(ctx, tx, thumbprint, nil, actor, reason)
	if err != nil {
		return false, 0, err
	}
	return true, ended + more, nil
}

// NoAccountSignInActor is the actor RecordPersonSignIn records when a trade ends a sign-in that
// recorded no account: the broker, which cannot tell whose that sign-in was.
const NoAccountSignInActor = "broker:sign-in-recording-no-account"

// RecordPersonSignIn records a person's trade in tx (store.RecordPersonSignIn), the exchange's own
// transaction, and, when that trade ended a sign-in from before migration 0046, which records no
// account and so might have been any account's, ends the launcher credential that sign-in held and
// every session it enrolled (endCredential), as revoking the sign-in does. The sessions another
// account enrolled there would otherwise go on reading that account's secrets on this box after a
// revoke of that account, which ends the sign-ins recording it or none and so no longer finds a
// row recording this trade's. The trade then mints the sign-in's credential afresh
// (oauth.Provider.personCredential), so the box enrols its sessions again once. The credential is
// found by the box key's thumbprint and by the subject that names this person's sign-in on this
// machine with this key (exchange.PersonSubject), which is the credential the sign-in's own trades
// minted and no other credential that came to hold the key. Every refusal is store's, unchanged.
func RecordPersonSignIn(ctx context.Context, tx pgx.Tx, in store.SignInTrade) error {
	ended, err := store.RecordPersonSignIn(ctx, tx, in)
	if err != nil || !ended {
		return err
	}
	subject := exchange.PersonSubject(in.Person, in.MachineAccount, in.Thumbprint)
	_, err = endKeyCredential(ctx, tx, in.Thumbprint, &subject, NoAccountSignInActor,
		"a trade ended the sign-in that held it, which recorded no account")
	return err
}

// endKeyCredential ends, in tx, the live launcher credential that holds the box key thumbprint and
// everything it holds (endCredential), writing actor and reason on every row it writes, and answers
// how many live access tokens ended with it: 0 when no live credential holds the key. subject, when
// not nil, ends only the credential that subject names, and leaves one of any other subject that
// holds the key. The row is read `for no key update`, which is the hold endCredential asks of its
// caller.
func endKeyCredential(ctx context.Context, tx pgx.Tx, thumbprint string, subject *string, actor, reason string) (int, error) {
	var credential, host string
	var service *string
	err := tx.QueryRow(ctx, `select id, host, service from launcher_credentials
		where key_thumbprint = $1 and ($2::text is null or subject = $2) and revoked_at is null for no key update`,
		thumbprint, subject).Scan(&credential, &host, &service)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, err
	}
	return endCredential(ctx, tx, credential, host, service, actor, reason)
}

// endCredential revokes launcher credential id in tx, whose row the caller holds: it sets
// revoked_at, ends its OAuth tokens, ends every enrollment it made that has not ended
// (endEnrollment, reason), and writes one launcher_credential.revoked audit row, each as actor.
// It answers how many live access tokens it ended.
//
// A person revoking a machine login, or their sign-in on a box, means every secret that login
// reached, so each of its enrollments ends terminally: the grants are revoked and the pending
// requests cancelled, with no carry (endTerminal). The dormant enrollments it made end here too,
// which is why the read below takes them beside the live ones: a dormant row has revoked_at set
// and still holds its session's grants, and leaving them would let the next enrollment of that
// session name, on a box the person has not revoked, adopt a grant the person has just taken
// back.
func endCredential(ctx context.Context, tx pgx.Tx, id, host string, service *string, actor, reason string) (int, error) {
	if _, err := tx.Exec(ctx, `update launcher_credentials set revoked_at=now() where id=$1`, id); err != nil {
		return 0, err
	}
	// A device login's tokens name the credential, so its next refresh is refused too.
	tokens, err := oauthstore.EndCredential(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `select id from enrollments where launcher_credential_id=$1 and (revoked_at is null or dormant_since is not null) order by id for update`, id)
	if err != nil {
		return 0, err
	}
	ended, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for _, enrollment := range ended {
		if _, err := endEnrollment(ctx, tx, enrollment, actor, "enrollment.revoked", reason, endTerminal); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `insert into audit (kind, actor, detail) values ('launcher_credential.revoked',$1,
		jsonb_strip_nulls(jsonb_build_object('credential_id',$2::text,'host',$3::text,'service',$4::text,'enrollments',coalesce(to_jsonb($5::text[]),'[]'::jsonb))))`,
		actor, id, host, service, ended); err != nil {
		return 0, err
	}
	return tokens, nil
}

// Lookup answers proof.Verifier's Lookup callback. id comes from an untrusted proof's eid claim,
// so a value that is not even a well-formed UUID is refused as "not live" here — never handed to
// Postgres, whose uuid column would otherwise reject it with a type-mismatch error (SQLSTATE
// 22P02) that propagates out as a Lookup error instead of ("", false, nil), which
// proof.Verifier.Verify would in turn surface as a 500 instead of the 401 a forged eid deserves.
func (s *Service) Lookup(ctx context.Context, id string) (string, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", false, nil
	}
	var thumbprint string
	var live bool
	err := s.Store.Pool.QueryRow(ctx, `select thumbprint, revoked_at is null and enrollment_lease_live(lease_expires_at)
		from enrollments where id=$1`, id).Scan(&thumbprint, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return thumbprint, live, nil
}

// Get answers this enrollment's own metadata (kind, operator, slot, lease expiry) for
// GET /v1/enrollments/self. Added in Task 11 for that read, following the same query pattern as
// Lookup above. Unlike Lookup, which proof.Verifier calls with an untrusted, possibly malformed id
// straight off a forged proof, Get is only ever called with an id a proof has already
// authenticated, so it need not guard against a non-UUID id the way Lookup does.
func (s *Service) Get(ctx context.Context, id string) (Enrollment, error) {
	var e Enrollment
	err := s.Store.Pool.QueryRow(ctx, `select id, kind, runtime_id, operator, slot, lease_expires_at, projects from enrollments where id=$1 and revoked_at is null`, id).
		Scan(&e.ID, &e.Kind, &e.RuntimeID, &e.Operator, &e.Slot, &e.LeaseExpires, &e.Projects)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrollment{}, ErrNotLive
	}
	return e, err
}

// SessionID answers a live (non-revoked) enrollment's own session_id — the Envoy session a
// launcher recorded at Create time — for wake.Envoy's notification target. It follows Get's own
// "non-revoked" filter, not Lookup's stricter lease-not-expired one: main.go's waker calls this
// only as a fallback after requests.Machine.SessionID and discards its error, so a null
// session_id or an id that matches no live row both answer ("", nil); only a genuine Postgres
// failure is a non-nil error.
func (s *Service) SessionID(ctx context.Context, id string) (string, error) {
	var sessionID *string
	err := s.Store.Pool.QueryRow(ctx, `select session_id from enrollments where id=$1 and revoked_at is null`, id).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if sessionID == nil {
		return "", nil
	}
	return *sessionID, nil
}

// Replay records a jti, answering false when it was already recorded. It runs in the transaction
// ctx carries (oauthstore.TxFrom), as machine.Service.LoginDevice records a device login's request
// object in the OAuth provider's transaction, and on the pool when ctx carries none, so a caller
// holding a transaction never waits for a second pooled connection. A jti recorded on the pool
// also prunes the expired ones, so the table stays bounded without a separate job; one recorded in
// a transaction prunes nothing, since the prune's row locks would hold every concurrent caller's
// prune behind that transaction until it ends.
func (s *Service) Replay(ctx context.Context, jti string, expires time.Time) (bool, error) {
	var db execer = s.Store.Pool
	tx, inTx := oauthstore.TxFrom(ctx)
	if inTx {
		db = tx
	}
	tag, err := db.Exec(ctx, `insert into proof_jtis (jti, expires_at) values ($1,$2) on conflict (jti) do nothing`, jti, expires)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if !inTx {
		_, _ = s.Store.Pool.Exec(ctx, `delete from proof_jtis where expires_at < now()`)
	}
	return true, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
