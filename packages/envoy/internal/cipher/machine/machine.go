// Package machine implements machine logins. A person's machine (an operator's box) logs in
// through an OAuth device grant (LoginDevice) whose typed-code approval mints a key-bound launcher
// credential: it signs a credential-request object carrying a single launcher_credential
// authorization detail, naming the operator it logs in as in its login_hint, and that person
// approves the user code the device authorization minted. Machine-login records are decided here,
// not in requests.Machine: ApplyDecision takes the deciding human's Dispatch login and the typed
// code and, on approval, mints the credential directly — there is no Dispatch ask anywhere in this
// flow, and no bearer token in any response. A service (the Legion daemon, whose pods it
// enrolls) logs in with its own projected Kubernetes ServiceAccount token instead (TokenLogin),
// and no person approves it. A machine holding a Google identity token logs in nowhere here: the
// RFC 8693 token exchange mints its launcher credential when it trades that token.
package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

var (
	// ErrNotFound is returned by ApplyDecision and LookupByCode when no record matches the given
	// record id or code.
	ErrNotFound = errors.New("machine login not found")
	// ErrCodeMismatch is ApplyDecision's refusal when the caller's code does not match the
	// record's own: the human is deciding a different login than the one their terminal or
	// dashboard actually shows, refused before the approver is even checked.
	ErrCodeMismatch = errors.New("confirmation code does not match")
	// ErrAlreadyDecided is ApplyDecision's refusal for a record that already carries its one
	// decision: a second approve or deny, or one that lost the race to a concurrent one.
	ErrAlreadyDecided = errors.New("this machine login has already been decided")
	// ErrLoginExpired is ApplyDecision's refusal for a login that expired undecided: the sweeper
	// recorded it expired, or its expires_at has passed before the sweeper got to it.
	ErrLoginExpired = errors.New("this machine login expired before its approver acted on it")
	// ErrKeyHoldsLiveCredential is ApplyDecision's refusal to approve a pending login whose key
	// already holds a live launcher credential under another record: a machine signed two logins
	// with one key and the first was approved. The record stays pending.
	ErrKeyHoldsLiveCredential = errors.New("this machine login's key already holds a live launcher credential")
	// ErrApproverReplaced is ApplyDecision's refusal to approve a login as an account that no
	// longer holds the approver's address: a revoke named this account replaced and another one
	// the holder (enroll.Service.RevokePersonSubject, the broker's half of POST
	// /v1/person-subjects/revoke, which no caller in this tree sends). Nothing is minted and the
	// record stays pending, for the account that holds the address now.
	ErrApproverReplaced = errors.New("the account approving this machine login no longer holds its approver's address: another account holds it now")
	// ErrApprovalNamesNoAccount is ApplyDecision's refusal to approve, naming no account and
	// carrying no session generation, a login whose approver's address Dispatch has named an
	// account replaced at within ApprovalInFlightWindow: the broker cannot tell such an approval is
	// not the replaced account's. Only a Dispatch that records an address's account and sends the
	// approver fields names one; Dispatch in this tree relays the approver and the code alone, and
	// so does an older one. So an approval naming none may be one whose check passed before a
	// replacement and lands after its revocation, which would otherwise mint a login recording none
	// that the paid revocation no longer ends. An approve that carries the generation its check read
	// is decided exactly by ErrApprovalCheckedBeforeReplacement instead. It is an
	// ErrApproverReplaced, answered 403 APPROVER_REPLACED; nothing is minted and the record stays
	// pending.
	ErrApprovalNamesNoAccount = fmt.Errorf("%w: this approval names no account, and another account has held the address since, so approve it again from Dispatch, which names the account that holds it now", ErrApproverReplaced)
	// ErrApprovalCheckedBeforeReplacement is ApplyDecision's refusal of an approval whose session
	// generation, the one its caller's check of the approver read, is below the generation a
	// revoke recorded a replacement of the approver's address at: that check passed before the
	// address passed to another account, whichever account the approval names, and the revoke that
	// ended what the replaced account and every credential recording no account held has already
	// committed, so it would not end the login this approval mints. No caller in this tree sends a
	// generation, so nothing here is refused this way; the field comes with the change that records
	// accounts at Dispatch. It is an ErrApproverReplaced, answered 403 APPROVER_REPLACED; nothing
	// is minted and the record stays pending, for an approval checked again at Dispatch.
	ErrApprovalCheckedBeforeReplacement = fmt.Errorf("%w: Dispatch checked this approval before another account took the address, so approve it again from Dispatch", ErrApproverReplaced)
	// ErrKeyBindsReadToken is ApplyDecision's and TokenLogin's refusal of a key a live read token
	// is bound to (enroll.ErrKeyBindsReadToken): a read token's key signs no launcher proof, so
	// the machine makes a new key and logs in with it. The record stays pending.
	ErrKeyBindsReadToken = errors.New("a live read token is bound to this machine login's key, and a key that signs launcher proofs is bound to no read token: make a new key and log in with it")
	// ErrNotLive is Refreshed's and Live's refusal for a launcher credential that is revoked or
	// past its expiry: a refresh never revives a login that has ended. It is an
	// oauth.ErrLoginEnded, so the token endpoint refuses it invalid_grant.
	ErrNotLive = fmt.Errorf("%w: this machine login's credential is revoked or expired", oauth.ErrLoginEnded)
)

// ApprovalInFlightWindow is how long after a revoke records an account replaced at an address an
// approval is refused when no session generation places it against that replacement: one that
// carries no generation (every approve Dispatch in this tree relays, and an older Dispatch's) and
// names no account (ErrApprovalNamesNoAccount). It applies only to that one: every revoke carries a
// generation, so an approval carrying one is decided by the two generations alone.
//
// It is the relay's own bound, the longest an approve can be in flight from the read of its
// approver to the broker's decision, so the fallback refuses every approve read before the
// replacement. Two of its three parts are this tree's:
//
//   - the broker's client at Dispatch gives one call 5 s (internal/dispatch/cipher's
//     defaultTimeout), so an approve reaches the broker within 5 s of that call or not at all;
//   - the broker ends every request's context 45 s after it arrives (cmd/cipherd's
//     requestDeadline, wire.RequestDeadline, which withRequestDeadline puts on the request), so the
//     approval's transaction, whose now() the window is measured to, begins within 45 s of the
//     approve's arrival or not at all.
//
// The third is the caller's: it must read its approver and make that call under one deadline, so
// the read is no older than the call. Dispatch in this tree holds to no such deadline — its
// approveCredentialRecord reads the acting person (actingPerson) and then the request's body for
// the typed code (decisionFor), and its server sets no ReadTimeout — and it sends neither an
// account nor a generation. Nor does anything here send the revoke that records a replacement:
// Dispatch's broker client has no call for POST /v1/person-subjects/revoke, so only a caller of
// that route writes a row this window reads. The change that records accounts at Dispatch brings
// all three: the revoke, the approver fields, and the one deadline over the check and the relay.
//
// Under that deadline, such an approval's transaction begins less than 5 s + 45 s = 50 s after its
// check. The revoke it is weighed against is sent only once the replacing sign-in has committed,
// after that check, so its replaced_at (the revoke transaction's now()) is later than the check,
// and the approval's now() is less than 50 s after it: the window refuses it. Both times the
// window compares are the broker's own clock. The cost is that an approve naming no account is
// refused for 50 s after a replacement at its address, and is approved again from Dispatch.
//
// For context only, the measured times are far inside that bound: on 2026-11-18, between 12:37Z
// and 13:08Z, with go1.27.3, GOMAXPROCS=8 and -p 4 on a 288-core box at load averages from 207 to
// 308, the longest time from Dispatch's handler being handed an approve to the test's broker
// receiving its relay was 13.29 ms (internal/dispatch/api's tests, 20 relays; cmd/dispatch's
// longest of 10 was 6.85 ms; `ENVOY_TIME_APPROVALS=1 go test -count=1 -v -timeout 40m` on each,
// their APPROVE-RELAYED-IN lines), and the longest ApplyDecision of an approval, start to return,
// lock waits included, was 43.08 ms (internal/cipher/machine's tests, 633 approvals, among them the
// 600 of TestARevokeAndAnApprovalByTheReplacedAccountNeverLeaveItALiveLogin, timed by the tests'
// applyDecision, their APPROVAL-DECIDED-IN lines).
const ApprovalInFlightWindow = 50 * time.Second

// NewApprovalsWithoutGeneration registers broker_machine_login_approvals_without_generation_total
// on r and answers it, for Service.ApprovalsWithoutGeneration: each machine-login approve that
// carries no approver_session_generation, which only ApprovalInFlightWindow decides against a
// replacement. Dispatch in this tree sends no approver fields, so every approve it relays is
// counted; once a Dispatch that sends them is deployed everywhere and the counter stays at zero,
// the fallback can come out.
func NewApprovalsWithoutGeneration(r *metrics.Registry) *metrics.Counter {
	return r.NewCounter("broker_machine_login_approvals_without_generation_total",
		"Machine-login approves that carried no approver_session_generation, from a Dispatch that sends no approver fields; the no-generation fallback (machine.ApprovalInFlightWindow) comes out once it stays at zero.")
}

type Service struct {
	Store  *store.Store
	Enroll *enroll.Service
	// Policy is the secret policy, whose version a machine-login record carries.
	Policy *policy.Current

	// Audience is what a new login's request object must name as its aud exactly
	// (BROKER_PUBLIC_URL): the broker's own address as the login's caller reached it.
	Audience string
	// EdgeAudience is BROKER_EDGE_URL, the broker's address at its edge, and "" for a broker with
	// no edge. A login the edge opens is held to it exactly (AtEdge), and a record read back for
	// its lookup or its decision, which Dispatch relays to Audience, is held to the origin of
	// either address (record.VerifyStoredRequestObject): each is this broker, and the record's
	// object was held to the one its caller reached when the login was opened.
	EdgeAudience string
	Skew         time.Duration
	PendingTTL   time.Duration
	// CredentialLifetime is how long a minted credential lasts, and how far each refresh of its
	// device login moves its expiry.
	CredentialLifetime time.Duration

	// OAuth is the device grant a person's machine logs in through; nil when the broker runs none.
	// ApplyDecision records each decision on it, so the machine's token poll learns the outcome.
	OAuth *oauth.Provider

	// Replay records a request object's jti (the proof_jtis table), answering false when it was
	// already seen, in the transaction ctx carries (oauthstore.TxFrom) and on the pool when it
	// carries none. Wired to enroll.Service.Replay in production, exactly like
	// requests.Machine.Replay: without it a captured signed machine-login request object could be
	// resubmitted repeatedly within its freshness window, each call minting a fresh pending record
	// and confirmation code — a confirmation-fatigue/notification-spam vector against the named
	// operator.
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)

	// ApprovalsWithoutGeneration counts each approve ApplyDecision is handed that carries no
	// session generation (NewApprovalsWithoutGeneration); nil counts nothing.
	ApprovalsWithoutGeneration *metrics.Counter
}

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's and requests.Machine's own retention margin.
const jtiRetentionMargin = time.Minute

// embeddedJWK extracts the raw JWK embedded in a request object's JWS header. It is called only
// after record.VerifyStoredRequestObject already checked that JWS's signature and structure, so
// ApplyDecision can store the exact public key material a launcher credential is bound to.
func embeddedJWK(compact string) (json.RawMessage, error) {
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(sig.Signatures) != 1 || sig.Signatures[0].Protected.JSONWebKey == nil {
		return nil, fmt.Errorf("%w: no embedded key", record.ErrRequestInvalid)
	}
	return sig.Signatures[0].Protected.JSONWebKey.MarshalJSON()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// AtEdge is s as the broker's edge opens a login with: the same store, enrollment, policy and
// provider, with the edge's address as the audience a login's request object must name. It
// answers nil for a service with no EdgeAudience, a broker with no edge. The edge opens logins
// and decides none, so what it answers is read only by LoginDevice, by CheckPersonLogin (the
// edge's pre-check of its shared device bucket) and by the device route's own bucket spend, which
// verify an object against Audience.
func (s *Service) AtEdge() *Service {
	if s.EdgeAudience == "" {
		return nil
	}
	edge := *s
	edge.Audience = s.EdgeAudience
	return &edge
}

// verifyLauncherRequest verifies a machine request object for a launcher credential at now
// (checkLauncherRequest), then its jti, which it records, so a replayed request is refused. Every
// refusal but check's own is record.ErrRequestInvalid.
func (s *Service) verifyLauncherRequest(ctx context.Context, compact string, now time.Time, check func(record.RequestObject) error) (record.RequestObject, error) {
	obj, err := s.checkLauncherRequest(compact, now, check)
	if err != nil {
		return record.RequestObject{}, err
	}
	fresh, err := s.Replay(ctx, obj.JTI, obj.Expires.Add(s.Skew+jtiRetentionMargin))
	if err != nil {
		return record.RequestObject{}, err
	}
	if !fresh {
		return record.RequestObject{}, fmt.Errorf("%w: replayed jti", record.ErrRequestInvalid)
	}
	return obj, nil
}

// checkLauncherRequest is verifyLauncherRequest short of its jti: the object's signature and
// freshness (record.VerifyRequestObject), exactly one launcher_credential detail, on a broker that
// holds a project-access client (the enrollment's Projects) a reason naming no more projects than
// a new request may (record.CheckReasonProjects), then the caller's own check. It reads and writes
// nothing. A broker with no project-access client takes any reason, as before that bound (R9).
func (s *Service) checkLauncherRequest(compact string, now time.Time, check func(record.RequestObject) error) (record.RequestObject, error) {
	obj, err := record.VerifyRequestObject(compact, s.Audience, s.Skew, now)
	if err != nil {
		return record.RequestObject{}, err
	}
	if len(obj.Details) != 1 || obj.Details[0].Type != record.KindLauncherCredential {
		return record.RequestObject{}, fmt.Errorf("%w: exactly one launcher_credential detail is required", record.ErrRequestInvalid)
	}
	if s.Enroll.Projects != nil {
		if err := record.CheckReasonProjects(obj.Reason); err != nil {
			return record.RequestObject{}, err
		}
	}
	if err := check(obj); err != nil {
		return record.RequestObject{}, err
	}
	return obj, nil
}

// CheckPersonLogin reports, with nil, that compact and dpopJKT are a person's machine login
// LoginDevice would open at now, short of the one check that needs the store, whether its jti was
// seen before: every other refusal of verifyPersonLogin, the same way. It reads and writes
// nothing, so a caller can judge a request before it spends anything on it (the edge's shared
// device bucket).
func (s *Service) CheckPersonLogin(compact, dpopJKT string, now time.Time) error {
	_, err := s.checkLauncherRequest(compact, now, personLoginCheck(dpopJKT))
	return err
}

// LoginDevice opens a person's machine login as an OAuth device authorization: the provider's
// onRequest, run in the transaction ctx carries (oauthstore.TxFrom) with the device request's form
// and the user code the provider minted. form's request is the machine's signed request object
// (verifyPersonLogin, its jti recorded, so a replay is refused), and its dpop_jkt must be that
// object's key thumbprint, so the tokens the login is issued are bound to the key its credential
// is (record.ErrRequestInvalid otherwise). The jti, like the record, is written in that
// transaction (Replay reads it from ctx), so a login holds the one pooled connection its
// transaction took and never waits for another, and a refused login leaves no jti behind. The
// record's code is the user code; the machine polls with its device code. The record also carries
// the request object, its approver, the credential lifetime an approval mints (CredentialLifetime),
// the policy version, and an expiry PendingTTL after now.
func (s *Service) LoginDevice(ctx context.Context, form url.Values, userCode string) error {
	tx, ok := oauthstore.TxFrom(ctx)
	if !ok {
		return errors.New("machine: LoginDevice runs in the provider's transaction")
	}
	now := time.Now()
	compact := form.Get("request")
	approver, err := s.verifyPersonLogin(ctx, compact, now, form.Get("dpop_jkt"))
	if err != nil {
		return err
	}
	body := record.Body{
		Request:         compact,
		Approver:        approver,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-", Operator: ""},
		LifetimeSeconds: new(int(s.CredentialLifetime.Seconds())),
		RulesVersion:    s.Policy.Get().Version,
		ExpiresAt:       now.Add(s.PendingTTL).UTC().Truncate(time.Second),
		Code:            userCode,
	}
	_, err = tx.Exec(ctx, `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt)
	return err
}

// verifyPersonLogin verifies a person's machine login request object at now (verifyLauncherRequest)
// and answers the approver its login_hint names. A request naming a service is
// ErrServiceLogsInWithToken, and one naming no person, or record.AnyoneApprover, or whose key
// thumbprint is not dpopJKT, is record.ErrRequestInvalid, each checked before the jti is recorded
// (personLoginCheck).
func (s *Service) verifyPersonLogin(ctx context.Context, compact string, now time.Time, dpopJKT string) (string, error) {
	obj, err := s.verifyLauncherRequest(ctx, compact, now, personLoginCheck(dpopJKT))
	if err != nil {
		return "", err
	}
	return record.CanonicalLogin(obj.LoginHint), nil
}

// personLoginCheck is the check of a person's machine login request object: no service, a login_hint
// naming a person other than record.AnyoneApprover, and a key thumbprint that is dpopJKT.
func personLoginCheck(dpopJKT string) func(record.RequestObject) error {
	return func(obj record.RequestObject) error {
		if obj.Service() != "" {
			return ErrServiceLogsInWithToken
		}
		switch record.CanonicalLogin(obj.LoginHint) {
		case "":
			return fmt.Errorf("%w: login_hint is required for a person's machine login", record.ErrRequestInvalid)
		case record.AnyoneApprover:
			return fmt.Errorf("%w: a person's machine login's login_hint names its operator, never %q", record.ErrRequestInvalid, record.AnyoneApprover)
		}
		if dpopJKT != obj.Thumbprint {
			return fmt.Errorf("%w: dpop_jkt is not the request object's key thumbprint", record.ErrRequestInvalid)
		}
		return nil
	}
}

// ApplyDecision decides a pending machine login. code must match the record's own — a wrong code
// means the human is looking at a different login than the one they're deciding, refused before
// anything else is checked (CODE_MISMATCH). login, the deciding human's Dispatch login, must be one
// the record's rule admits (record.ErrNotApprover), checked next against the request object its
// record embeds, re-verified as of the record's creation: a person's machine login is decided by
// that person alone, and a service's login, which a broker before TokenLogin opened, is approved by
// no one (a service logs in with its token now), though it may still be denied, by anyone signed in
// but a person Dispatch calls a restricted member, who is no approver of a secret (serviceDenier).
// So another login is refused the same way whatever the record's state. A record that already
// carries a decision is
// ErrAlreadyDecided, and one that expired undecided — recorded expired by the sweeper, or past its
// expires_at before the sweeper got to it — is ErrLoginExpired, both checked under the record's
// row lock before anything is minted, so a second decision, one racing the first and one after
// expiry all answer the same way. Approval mints the
// credential — bound to the request object's own key (thumbprint and embedded JWK), with the lifetime
// its record names (LoginDevice) counted from the decision — in the same transaction that records the
// decision, so a crash between the two never orphans a credential no decision names. A key that
// already holds a live credential under another record is ErrKeyHoldsLiveCredential, one that a
// live read token is bound to (enroll.ErrKeyBindsReadToken) is ErrKeyBindsReadToken, and either
// way the record stays pending.
//
// account is the identity provider's account the deciding human signed in to Dispatch as, which
// the approve carries beside login, and the zero value for an approve that names none, which is
// every approve Dispatch relays in this tree. An approval records it on the credential it mints,
// and is ErrApproverReplaced, minting nothing, when that account no longer holds login's address
// (store.SubjectReplaced). It reads that after taking login's lock (store.LockPerson), which the
// revoke of a replaced account takes too (enroll.Service.RevokePersonSubject): an approval that
// took it first has committed its credential before the revoke reads login's machine logins, and
// the revoke ends it; one that waited reads what the revoke recorded, and is refused.
//
// generation is the session generation of login's address at the caller, as that caller's check of
// the approver read it (approver_session_generation), and nil for an approve carrying none, which
// is every approve Dispatch relays in this tree. A caller that records accounts moves that
// generation in the transaction that records each one, and the revoke records the generation its
// replacement left. So an approval carrying a generation is ErrApprovalCheckedBeforeReplacement,
// whatever account it names, when a replacement of login's address was recorded at a greater one
// (store.SubjectReplacedAfter), read under the same lock: its check passed before the address
// moved, and the revoke, having ended every credential of the address recording none, would no
// longer end the login it mints. An approval carrying no generation that names no account is
// ErrApprovalNamesNoAccount when a revoke named any account replaced at login's address within
// ApprovalInFlightWindow (store.SubjectReplacedWithin), read under the same lock, since nothing
// else places it before or after the move; one carrying no generation that names an account is
// held to that account alone, as above. A denial mints nothing and takes away nothing anyone
// holds, so it is decided whatever the account and the generation.
func (s *Service) ApplyDecision(ctx context.Context, recordID string, approve bool, login, code string, account store.IssuerSubject, generation *int64) (state, credentialID string, err error) {
	if approve && generation == nil {
		s.ApprovalsWithoutGeneration.Inc()
	}
	var denier serviceDenier
	if !approve {
		denier = s.readServiceDenier(ctx, recordID, login)
	}
	tx, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)

	// The row lock serializes every decision of this record; a lock fires no update trigger, so
	// the append-only record allows it. It is `for no key update`, not `for update`: the foreign
	// key check of another transaction's event insert (the sweeper's 'expired') takes `for key
	// share` on this row, which `for update` would block while this transaction then waited on
	// that insert's unique-index entry, a deadlock; the weaker lock lets that insert commit, and
	// this decision's own insert then hits that same unique index. Decisions of one record
	// serialize on this lock, so the only other writer that can win that index while this
	// transaction holds it is the sweeper's ExpirePending — a concurrent decision never reaches
	// the insert at all; it queues on this same lock and reads the sweeper's event from the
	// switch below instead. So that collision always means the login expired while this decision
	// was in flight, and the insert answers ErrLoginExpired, never ErrAlreadyDecided.
	var canonical, storedCode string
	var createdAt time.Time
	var expired bool
	err = tx.QueryRow(ctx, `select body, code, created_at, expires_at <= now() from credential_requests where id=$1 and kind='launcher_credential' for no key update`, recordID).
		Scan(&canonical, &storedCode, &createdAt, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if oauth.NormalizeUserCode(code) != storedCode {
		return "", "", ErrCodeMismatch
	}

	body, err := record.VerifyBodyReproducesID(canonical, recordID)
	if err != nil {
		if errors.Is(err, record.ErrBodyIDMismatch) {
			return "", "", fmt.Errorf("%w: stored body does not reproduce its own id", record.ErrRequestInvalid)
		}
		return "", "", err
	}

	// now is the record's own creation time: the request object's iat and exp bound how fresh it
	// had to be when its login was opened, never how long the login may wait for its decision.
	obj, err := record.VerifyStoredRequestObject(body.Request, s.Audience, s.EdgeAudience, s.Skew, createdAt)
	if err != nil {
		return "", "", err
	}
	// A machine login's approver is never a group, so no group reader is asked.
	login, err = body.ApproverLogin(record.KindLauncherCredential, obj.Service(), login, nil)
	if err != nil {
		return "", "", err
	}
	if !approve && obj.Service() != "" {
		switch {
		case denier.err != nil:
			return "", "", denier.err
		case denier.restricted:
			return "", "", errRestrictedDenier
		}
	}
	var decided string
	if err := tx.QueryRow(ctx, `select coalesce((select event from credential_request_events where record_id=$1 and event = any($2)), '')`, recordID, record.TerminalEventNames()).
		Scan(&decided); err != nil {
		return "", "", err
	}
	switch {
	case decided == "expired" || decided == "" && expired:
		return "", "", ErrLoginExpired
	case decided != "":
		return "", "", ErrAlreadyDecided
	}
	event := "denied"
	if approve {
		event = "approved"
	}

	var credID *string
	if approve {
		jwk, err := embeddedJWK(body.Request)
		if err != nil {
			return "", "", err
		}
		detail := obj.Details[0]
		if detail.Service != "" {
			return "", "", ErrServiceLogsInWithToken
		}
		if body.LifetimeSeconds == nil {
			return "", "", fmt.Errorf("machine login record has no credential lifetime")
		}
		if err := store.LockPerson(ctx, tx, login); err != nil {
			return "", "", err
		}
		replaced, err := store.SubjectReplaced(ctx, tx, login, account)
		if err != nil {
			return "", "", err
		}
		if replaced {
			return "", "", ErrApproverReplaced
		}
		switch {
		case generation != nil:
			checkedBefore, err := store.SubjectReplacedAfter(ctx, tx, login, *generation)
			if err != nil {
				return "", "", err
			}
			if checkedBefore {
				return "", "", ErrApprovalCheckedBeforeReplacement
			}
		case !account.Recorded():
			recent, err := store.SubjectReplacedWithin(ctx, tx, login, ApprovalInFlightWindow)
			if err != nil {
				return "", "", err
			}
			if recent {
				return "", "", ErrApprovalNamesNoAccount
			}
		}
		id, err := s.Enroll.MintLauncherCredentialTx(ctx, tx, login, detail.Identifier, obj.Thumbprint, jwk, recordID,
			time.Now().Add(time.Duration(*body.LifetimeSeconds)*time.Second), account)
		if enroll.KeyHeld(err) {
			return "", "", ErrKeyHoldsLiveCredential
		}
		if errors.Is(err, enroll.ErrKeyBindsReadToken) {
			return "", "", ErrKeyBindsReadToken
		}
		if err != nil {
			return "", "", err
		}
		idStr := id.String()
		credID = &idStr
	}

	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, login, credential_id, actor) values ($1,$2,$3,$4,$5)`,
		recordID, event, login, credID, record.HumanActor(login)); store.IsUniqueViolation(err) {
		// Only ExpirePending can win this race: see the row-lock comment above.
		return "", "", ErrLoginExpired
	} else if err != nil {
		return "", "", err
	}
	if err := s.decideDevice(ctx, tx, body.Code, approve, recordID, credID, login, obj.Details[0].Identifier); err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	if approve {
		return "issued", *credID, nil
	}
	return "denied", "", nil
}

// errRestrictedDenier is ApplyDecision's refusal of a deny of a service's login by a person
// Dispatch calls a restricted member: record.ErrNotApprover, which the API answers 403
// NOT_APPROVER with these words (projectaccess.NoApprover).
var errRestrictedDenier = projectaccess.NoApprover(record.ErrNotApprover, "a secret or of a service's machine login, so they deny no service's machine login")

// serviceDenier is what a deny reads of its deciding person before ApplyDecision's transaction,
// for a service's login (readServiceDenier): whether Dispatch calls the person a restricted member,
// or why that could not be read. Its zero value asks nothing of the transaction.
type serviceDenier struct {
	restricted bool
	err        error
}

// readServiceDenier reads, on the pool and before ApplyDecision's transaction, since it may ask
// Dispatch, whether a deny of record recordID by login is a restricted member's deny of a service's
// login, the one deny such a person may not make: a person who sees only the projects that name
// them is no approver of a secret (requests' restricted_approver.go), and a service's login is
// every signed-in person's to deny for no other reason. It asks Dispatch (projectaccess.Restricted,
// through the enroll service's client) only for a record that is a service's login, so a person's
// deny of their own machine login asks nothing; a broker with no client of Dispatch asks nothing
// either. The read runs within projectaccess.RelayedReadBound, since Dispatch relays the deny and
// waits for its answer no longer than that read could otherwise take. A failure to ask, one the
// bound cut short included, is the *projectaccess.MemberKindUncheckable it carries, and a failure
// to read the record is that read's error; the transaction answers either for a service's login
// alone, after the code and the record's own rule: a check that cannot decide refuses. It reads
// the record's request object as the transaction does (record.VerifyStoredRequestObject, this
// broker's by the origin of its address or its edge's), so every record the transaction reads as
// a service's login, one whose aud spells the broker's address with a trailing slash included, is
// one it asks about. A record it finds no row for, or cannot parse or verify, asks nothing: the
// transaction reads it again by the same rule and refuses it.
func (s *Service) readServiceDenier(ctx context.Context, recordID, login string) serviceDenier {
	if s.Enroll == nil || s.Enroll.Projects == nil {
		return serviceDenier{}
	}
	var canonical string
	var createdAt time.Time
	err := s.Store.Pool.QueryRow(ctx, `select body, created_at from credential_requests where id=$1 and kind='launcher_credential'`, recordID).
		Scan(&canonical, &createdAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return serviceDenier{}
	case err != nil:
		return serviceDenier{err: err}
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		return serviceDenier{}
	}
	obj, err := record.VerifyStoredRequestObject(body.Request, s.Audience, s.EdgeAudience, s.Skew, createdAt)
	if err != nil || obj.Service() == "" {
		return serviceDenier{}
	}
	ctx, cancel := context.WithTimeout(ctx, projectaccess.RelayedReadBound)
	defer cancel()
	restricted, err := projectaccess.Restricted(ctx, s.Enroll.Projects, record.CanonicalLogin(login))
	return serviceDenier{restricted: restricted, err: err}
}

// decideDevice records a decision on the device login whose user code is code, in tx: an
// approval's session names the record, the minted credential, the operator and the host. A record
// with no device code (oauth2.ErrNotFound: one an earlier broker's POST /v1/launcher-credentials
// opened, still pending at the deploy) has nothing to record; a device code past its lifetime
// (oauth2.ErrInvalidGrant) is ErrLoginExpired, and the decision rolls back. A broker with no OAuth
// provider records nothing.
func (s *Service) decideDevice(ctx context.Context, tx pgx.Tx, code string, approve bool, recordID string, credID *string, login, host string) error {
	if s.OAuth == nil {
		return nil
	}
	extra := map[string]any{"record_id": recordID, "operator": login, "host": host}
	if credID != nil {
		extra[oauthwire.ExtraCredentialID] = *credID
	}
	err := s.OAuth.ApproveDevice(oauthstore.WithTx(ctx, tx), code, approve, extra)
	switch {
	case errors.Is(err, oauth2.ErrNotFound):
		return nil
	case errors.Is(err, oauth2.ErrInvalidGrant):
		return ErrLoginExpired
	}
	return err
}

// Refreshed moves a live launcher credential's expiry to CredentialLifetime from now, as its
// device login refreshes, and answers the new expiry. A credential revoked or already past its
// expiry is ErrNotLive and is left as it is. Expiry is compared with clock_timestamp(), the
// statement's own moment, not now(), the transaction's start, so a retire (expires_at set to its
// now) that commits after the token request's transaction began still ends the login for it. It
// runs only in the transaction ctx carries (oauthstore.TxFrom), and its update holds the
// credential's row lock until that transaction ends: a revoke (enroll.Service.RevokeCredential
// locks the same row) either commits first, so the update finds no live row, or waits for the
// refresh's tokens to commit and then ends them.
func (s *Service) Refreshed(ctx context.Context, credentialID string) (time.Time, error) {
	tx, err := tokenTx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var expires time.Time
	err = tx.QueryRow(ctx, `update launcher_credentials set expires_at = now() + make_interval(secs => $2)
		where id = $1 and revoked_at is null and expires_at > clock_timestamp() returning expires_at`, credentialID, s.CredentialLifetime.Seconds()).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotLive
	}
	return expires, err
}

// Live holds a launcher credential's row lock until the transaction ctx carries
// (oauthstore.TxFrom) ends, leaving its expiry as it is; a credential revoked or past its expiry
// (against clock_timestamp(), as in Refreshed) is ErrNotLive. A revoke
// (enroll.Service.RevokeCredential locks the same row) either commits first, so Live finds no live
// row, or waits for the device code grant's tokens to commit and then ends them.
//
// The lock itself is oauthstore.LockCredential, which the RFC 8693 exchange of a machine access
// token takes too: one statement decides what a live credential is, so the device grant and the
// exchange cannot drift apart on the question a revoke turns on. Only the sentinel differs, since
// this service's callers read ErrNotLive.
func (s *Service) Live(ctx context.Context, credentialID string) error {
	if _, err := tokenTx(ctx); err != nil {
		return err
	}
	if err := oauthstore.LockCredential(ctx, credentialID); err != nil {
		if errors.Is(err, oauthstore.ErrCredentialEnded) {
			return ErrNotLive
		}
		return err
	}
	return nil
}

// tokenTx is the token request's transaction ctx carries (oauthstore.TxFrom): the row locks the
// token hooks take hold only within it, so there is no running without one.
func tokenTx(ctx context.Context) (pgx.Tx, error) {
	tx, ok := oauthstore.TxFrom(ctx)
	if !ok {
		return nil, errors.New("machine: the token hooks run in the provider's transaction")
	}
	return tx, nil
}

// TokenHooks are the device login's token hooks, run in the token request's transaction: a
// refresh moves its credential's expiry first (Refreshed), the device code grant locks the
// credential's row as it is (Live), and either is refused when the credential has ended; every
// answer carries credential_id and credential_expires_at (RFC 3339) as the credential's row holds
// it in that transaction: the minted expiry on the device code grant, the one Refreshed moved it
// to on a refresh.
func (s *Service) TokenHooks() oauth.TokenHooks {
	var hooks oauth.TokenHooks
	hooks.Before = func(ctx context.Context, grant oauth2.GrantType, extra map[string]any) error {
		id, err := credentialOf(extra)
		if err != nil {
			return err
		}
		switch grant {
		case oauth2.GrantTypeRefreshToken:
			_, err = s.Refreshed(ctx, id)
			return err
		case oauth2.GrantTypeDeviceCode:
			return s.Live(ctx, id)
		default:
			return fmt.Errorf("machine: a device login has no %q grant", grant)
		}
	}
	hooks.After = func(ctx context.Context, _ oauth2.GrantType, extra map[string]any) (map[string]any, error) {
		id, err := credentialOf(extra)
		if err != nil {
			return nil, err
		}
		tx, err := tokenTx(ctx)
		if err != nil {
			return nil, err
		}
		var expires time.Time
		if err := tx.QueryRow(ctx, `select expires_at from launcher_credentials where id=$1`, id).Scan(&expires); err != nil {
			return nil, err
		}
		return map[string]any{oauthwire.ExtraCredentialID: id, oauthwire.ExtraCredentialExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
	}
	return hooks
}

// credentialOf is the launcher credential a device login's session extra names. A session naming
// none has no login to refresh, so it is an oauth.ErrLoginEnded.
func credentialOf(extra map[string]any) (string, error) {
	id, ok := extra[oauthwire.ExtraCredentialID].(string)
	if !ok || id == "" {
		return "", fmt.Errorf("%w: the login's session names no credential", oauth.ErrLoginEnded)
	}
	return id, nil
}

// recordState answers a record's current state ("pending" absent any terminal event, else the
// event's own name) and, once issued, its minted credential's id.
func (s *Service) recordState(ctx context.Context, recordID string) (state, credentialID string, err error) {
	var event string
	var credID *string
	err = s.Store.Pool.QueryRow(ctx, `select event, credential_id from credential_request_events where record_id=$1 and event = any($2)`, recordID, record.TerminalEventNames()).
		Scan(&event, &credID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "pending", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if event == "approved" {
		return "issued", deref(credID), nil
	}
	return event, "", nil
}

// RecordView is a machine login record as the operator's UI sees it, resolved by its
// human-readable confirmation code: enough to show what is being decided.
type RecordView struct {
	RecordID     string
	Host         string
	Service      string // "" for a personal (non-service) login
	Approver     string
	State        string
	CredentialID string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// LookupByCode resolves a pending machine login by its confirmation code, for the operator's own
// UI: the code its terminal or dashboard shows.
func (s *Service) LookupByCode(ctx context.Context, code string) (RecordView, error) {
	var id, canonical, approver string
	var createdAt, expiresAt time.Time
	err := s.Store.Pool.QueryRow(ctx, `select id, body, approver, created_at, expires_at from credential_requests
		where code=$1 and kind='launcher_credential' order by created_at desc limit 1`, oauth.NormalizeUserCode(code)).
		Scan(&id, &canonical, &approver, &createdAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecordView{}, ErrNotFound
	}
	if err != nil {
		return RecordView{}, err
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		return RecordView{}, err
	}
	obj, err := record.VerifyStoredRequestObject(body.Request, s.Audience, s.EdgeAudience, s.Skew, createdAt)
	if err != nil {
		return RecordView{}, err
	}
	detail := obj.Details[0]
	state, credentialID, err := s.recordState(ctx, id)
	if err != nil {
		return RecordView{}, err
	}
	return RecordView{
		RecordID: id, Host: detail.Identifier, Service: detail.Service, Approver: approver,
		State: state, CredentialID: credentialID, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}, nil
}

// ExpirePending writes an 'expired' event for every launcher_credential record whose own
// expires_at has passed now and that has no terminal event yet. The insert's own ON CONFLICT
// target is credential_request_events' partial unique index on (record_id) where event names a
// decision, so a record a concurrent ApplyDecision just decided is silently left alone rather
// than raising a constraint violation. That target spells the index's own predicate literally,
// not record.TerminalEventNames(): Postgres infers a partial index only from a predicate it can
// prove implies the index's, and a bound parameter proves nothing.
func (s *Service) ExpirePending(ctx context.Context, now time.Time) error {
	_, err := s.Store.Pool.Exec(ctx, `insert into credential_request_events (record_id, event, actor)
		select id, 'expired', 'broker' from credential_requests
		where kind='launcher_credential' and expires_at < $1
		on conflict (record_id) where event in ('approved','denied','expired','cancelled') do nothing`, now)
	return err
}
