// person_subjects_test.go drives POST /v1/person-subjects/revoke, the route Dispatch's server
// calls when another Google account takes an address, and the machine-login approval it holds to
// the account that approves, on the same real broker api_test.go mounts.
package api_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// googleIssuer is the identity provider every account in these tests is of, as
// oidc.CanonicalIssuer spells Google's; googleSchemeless is Google's other spelling of it.
const (
	googleIssuer     = "https://accounts.google.com"
	googleSchemeless = "accounts.google.com"
)

// accountOf is the account that signed person in first, and holderOf the one that holds the
// address after it moved.
func accountOf(person string) store.IssuerSubject {
	return store.IssuerSubject{Issuer: googleIssuer, Subject: "109" + person}
}

func holderOf(person string) store.IssuerSubject {
	return store.IssuerSubject{Issuer: googleIssuer, Subject: "209" + person}
}

// revokeSubject posts POST /v1/person-subjects/revoke as Dispatch's server does when another
// account takes person's address: subject the account it replaced, holder the one that holds it,
// and generation the session_generation it carries, none for nil, which the route refuses.
func (ts *testServer) revokeSubject(t *testing.T, person, issuer, subject, holder string, generation *int64) (int, []byte) {
	t.Helper()
	body := map[string]any{"person": person, "issuer": issuer, "subject": subject, "holder_subject": holder}
	if generation != nil {
		body["session_generation"] = *generation
	}
	return ts.ui(t, http.MethodPost, "/v1/person-subjects/revoke", body)
}

// revokedSubject is revokeSubject carrying generation, failing the test unless the broker answered
// 200 revoked, and answering what it ended.
func (ts *testServer) revokedSubject(t *testing.T, person string, subject, holder store.IssuerSubject, generation int64) wire.PersonSubjectRevokedResponse {
	t.Helper()
	status, body := ts.revokeSubject(t, person, subject.Issuer, subject.Subject, holder.Subject, &generation)
	answer := decode[wire.PersonSubjectRevokedResponse](t, body)
	if status != http.StatusOK || answer.State != "revoked" {
		t.Fatalf("POST /v1/person-subjects/revoke (%s replaced by %s) = %d %s, want 200 revoked", subject.Subject, holder.Subject, status, body)
	}
	return answer
}

// pendingMachineLogin opens a machine login of person's on host through the device grant and
// looks it up by its code, as Dispatch's page does, answering its record id and code.
func (ts *testServer) pendingMachineLogin(t *testing.T, person, host string) (recordID, code string) {
	t.Helper()
	code = ts.openMachineLogin(t, newSigningKey(t), person, host)
	status, body := ts.ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("look the machine login up by its code = %d %s, want 200", status, body)
	}
	return decode[wireRecord](t, body).RecordID, code
}

// approveLoginAs approves the machine login recordID as person, relaying account as the account
// person signed in to Dispatch as, and generation as the session generation Dispatch's check of
// the approver read; nil relays none, as a Dispatch from before the field does.
func (ts *testServer) approveLoginAs(t *testing.T, recordID, code, person string, account *store.IssuerSubject, generation *int64) (int, []byte) {
	t.Helper()
	body := map[string]any{"approver": person, "code": code}
	if account != nil {
		body["approver_issuer"], body["approver_subject"] = account.Issuer, account.Subject
	}
	if generation != nil {
		body["approver_session_generation"] = *generation
	}
	return ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/approve", body)
}

// replacedAt is the session generation the broker records for account as replaced at person's
// address, nil for none.
func (ts *testServer) replacedAt(t *testing.T, person string, account store.IssuerSubject) *int64 {
	t.Helper()
	var generation *int64
	if err := ts.Store.Pool.QueryRow(context.Background(), `select session_generation from replaced_person_subjects
		where person = $1 and issuer = $2 and subject = $3`, person, account.Issuer, account.Subject).Scan(&generation); err != nil {
		t.Fatalf("read the session generation %s's replacement of %s is recorded at: %v", person, account.Subject, err)
	}
	return generation
}

// approvedLogin opens person's machine login on host and approves it as account, failing the test
// unless that minted a credential, whose id it answers.
func (ts *testServer) approvedLogin(t *testing.T, person, host string, account *store.IssuerSubject) string {
	t.Helper()
	recordID, code := ts.pendingMachineLogin(t, person, host)
	status, body := ts.approveLoginAs(t, recordID, code, person, account, nil)
	approved := decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if status != http.StatusOK || approved.CredentialID == nil {
		t.Fatalf("approve %s's machine login on %s = %d %s, want 200 with a credential", person, host, status, body)
	}
	return *approved.CredentialID
}

// mintedFrom counts the launcher credentials minted from record recordID.
func (ts *testServer) mintedFrom(t *testing.T, recordID string) int {
	t.Helper()
	var n int
	if err := ts.Store.Pool.QueryRow(context.Background(), `select count(*) from launcher_credentials where record_id = $1`, recordID).Scan(&n); err != nil {
		t.Fatalf("count the credentials minted from %s: %v", recordID, err)
	}
	return n
}

// approverAccountOf reads the approving account launcher credential id records, the zero value
// for none.
func (ts *testServer) approverAccountOf(t *testing.T, id string) store.IssuerSubject {
	t.Helper()
	var account store.IssuerSubject
	if err := ts.Store.Pool.QueryRow(context.Background(), `select coalesce(approver_issuer, ''), coalesce(approver_subject, '')
		from launcher_credentials where id = $1`, id).Scan(&account.Issuer, &account.Subject); err != nil {
		t.Fatalf("read the approving account of %s: %v", id, err)
	}
	return account
}

// legacySignIn writes person's sign-in on thumbprint as a broker from before migration 0042 did:
// with no account recorded.
func (ts *testServer) legacySignIn(t *testing.T, person, thumbprint, machine string) {
	t.Helper()
	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into person_signins (person, thumbprint, machine_account)
		values ($1, $2, $3)`, person, thumbprint, machine); err != nil {
		t.Fatalf("write %s's sign-in from before migration 0042: %v", person, err)
	}
}

// pageSession writes person's session on the approval page under the cookie secret secret,
// signed in by account, as the page's callback does; nil writes it as a broker from before
// migration 0042 did, with no account recorded.
func (ts *testServer) pageSession(t *testing.T, person, secret string, account *store.IssuerSubject) {
	t.Helper()
	hash, expires := sha256.Sum256([]byte(secret)), time.Now().Add(time.Hour)
	if account == nil {
		if _, err := ts.Store.Pool.Exec(context.Background(), `insert into browser_sessions (secret_hash, login, expires_at)
			values ($1, $2, $3)`, hash[:], person, expires); err != nil {
			t.Fatalf("write %s's approval-page session from before migration 0042: %v", person, err)
		}
		return
	}
	if err := ts.Store.CreateBrowserSession(context.Background(), store.BrowserSignIn{Hash: hash, Login: person, Account: *account, Expires: expires}); err != nil {
		t.Fatalf("sign %s in to the approval page as %s: %v", person, account.Subject, err)
	}
}

// pageSessionLive reports whether the approval-page session under the cookie secret secret
// still signs anyone in.
func (ts *testServer) pageSessionLive(t *testing.T, secret string) bool {
	t.Helper()
	_, err := ts.Store.BrowserSession(context.Background(), sha256.Sum256([]byte(secret)), time.Now())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read the approval-page session %q: %v", secret, err)
	}
	return err == nil
}

// TestAnApprovalByAReplacedAccountIsRefusedAndTheHoldersMints: once Dispatch has named an
// account replaced, an approval relayed as that account is 403 APPROVER_REPLACED, mints nothing
// and leaves the login pending, whichever of Google's two spellings of its issuer either side
// used; the same login approved as the account that holds the address now mints, and the
// credential records that account. An approval that names half an account is 400.
func TestAnApprovalByAReplacedAccountIsRefusedAndTheHoldersMints(t *testing.T) {
	ts := newTestServer(t)
	replaced, holder := accountOf(testApprover), holderOf(testApprover)
	if status, body := ts.revokeSubject(t, testApprover, googleSchemeless, replaced.Subject, holder.Subject, new(int64(1))); status != http.StatusOK {
		t.Fatalf("the revoke, its issuer spelled without a scheme = %d %s, want 200", status, body)
	}

	recordID, code := ts.pendingMachineLogin(t, testApprover, "example-host-replaced")
	status, body := ts.approveLoginAs(t, recordID, code, testApprover, &replaced, nil)
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "APPROVER_REPLACED" {
		t.Fatalf("an approval as the replaced account = %d %s, want 403 APPROVER_REPLACED", status, body)
	}
	if n := ts.mintedFrom(t, recordID); n != 0 {
		t.Fatalf("the refused approval minted %d credential(s), want none", n)
	}
	half := replaced
	half.Issuer = ""
	if status, body := ts.approveLoginAs(t, recordID, code, testApprover, &half, nil); status != http.StatusBadRequest || decode[wireError](t, body).Code != "APPROVER_ACCOUNT_INPUT" {
		t.Fatalf("an approval naming a subject and no issuer = %d %s, want 400 APPROVER_ACCOUNT_INPUT", status, body)
	}

	status, body = ts.approveLoginAs(t, recordID, code, testApprover, &holder, nil)
	approved := decode[struct {
		CredentialID *string `json:"credential_id"`
	}](t, body)
	if status != http.StatusOK || approved.CredentialID == nil {
		t.Fatalf("the same login approved as the holder = %d %s, want 200 with a credential: the refusal left it pending", status, body)
	}
	if got := ts.approverAccountOf(t, *approved.CredentialID); got != holder {
		t.Fatalf("the holder's credential records the approving account %+v, want %+v", got, holder)
	}
}

// TestRevokingAReplacedAccountEndsWhatItAndNoAccountMade: the revoke ends the replaced account's
// sign-in, its own machine's login and its session on the approval page, and the sign-in, the
// login and the session that record no account, which might be its; it leaves the holder's
// sign-in, login and session, a service's login and another person's sign-in and session alone.
// A repeat ends nothing and answers zeros. A revoke that names the holder as replaced and the
// first account as holder — the address coming back — stops refusing the first account and ends
// what the holder made.
func TestRevokingAReplacedAccountEndsWhatItAndNoAccountMade(t *testing.T) {
	ts := newTestServer(t)
	replaced, holder := accountOf(testApprover), holderOf(testApprover)

	ts.signInAs(t, testApprover, adaBoxOne, signInFleetOne, replaced)
	ts.legacySignIn(t, testApprover, adaBoxTwo, signInFleetOne)
	ts.signInAs(t, testApprover, graceBox, signInFleetTwo, holder)
	ts.legacySignIn(t, signInGrace, graceBox, signInFleetOne)
	replacedLogin := ts.approvedLogin(t, testApprover, "example-host-replaced", &replaced)
	legacyLogin := ts.approvedLogin(t, testApprover, "example-host-legacy", nil)
	holderLogin := ts.approvedLogin(t, testApprover, "example-host-holder", &holder)
	serviceLogin, _ := ts.mintOutstandingServiceCredential(t, testApprover)
	ts.pageSession(t, testApprover, "replaced page", &replaced)
	ts.pageSession(t, testApprover, "legacy page", nil)
	ts.pageSession(t, testApprover, "holder page", &holder)
	// Another address's session under the very account replaced here: the revoke is of one
	// address, so the account's `sub` alone ends nothing.
	ts.pageSession(t, signInGrace, "grace page", &replaced)

	got := ts.revokedSubject(t, testApprover, replaced, holder, 1)
	if got.SignIns != 2 || got.MachineLogins != 2 || got.ApprovalSessions != 2 {
		t.Fatalf("the revoke ended %d sign-in(s), %d machine login(s) and %d approval-page session(s), want 2, 2 and 2: the replaced account's and those recording no account",
			got.SignIns, got.MachineLogins, got.ApprovalSessions)
	}
	for secret, want := range map[string]bool{"replaced page": false, "legacy page": false, "holder page": true, "grace page": true} {
		if live := ts.pageSessionLive(t, secret); live != want {
			t.Errorf("approval-page session %q live=%t after the revoke, want %t", secret, live, want)
		}
	}
	if left := ts.personSignIns(t, testApprover); len(left) != 1 || left[0].Thumbprint != graceBox {
		t.Fatalf("%s's sign-ins after the revoke = %+v, want the holder's alone", testApprover, left)
	}
	if grace := ts.personSignIns(t, signInGrace); len(grace) != 1 {
		t.Fatalf("%s's sign-ins after the revoke = %+v, want theirs untouched: the revoke is of one address", signInGrace, grace)
	}
	for id, want := range map[string]bool{replacedLogin: true, legacyLogin: true, holderLogin: false, serviceLogin: false} {
		if revoked := ts.credentialRevoked(t, id); revoked != want {
			t.Errorf("machine login %s revoked=%t after the revoke, want %t", id, revoked, want)
		}
	}

	if again := ts.revokedSubject(t, testApprover, replaced, holder, 1); again != (wire.PersonSubjectRevokedResponse{State: "revoked"}) {
		t.Fatalf("the repeat revoke = %+v, want revoked with every count zero", again)
	}

	back := ts.revokedSubject(t, testApprover, holder, replaced, 2)
	if back.SignIns != 1 || back.MachineLogins != 1 || back.ApprovalSessions != 1 {
		t.Fatalf("the revoke of the holder ended %d sign-in(s), %d machine login(s) and %d approval-page session(s), want its 1, 1 and 1",
			back.SignIns, back.MachineLogins, back.ApprovalSessions)
	}
	if revoked := ts.credentialRevoked(t, serviceLogin); revoked {
		t.Fatal("a service's login was revoked, and a replaced account's revoke never touches one")
	}
	ts.approvedLogin(t, testApprover, "example-host-returned", &replaced)
	recordID, code := ts.pendingMachineLogin(t, testApprover, "example-host-moved-on")
	if status, body := ts.approveLoginAs(t, recordID, code, testApprover, &holder, nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "APPROVER_REPLACED" {
		t.Fatalf("an approval as the account the address moved away from = %d %s, want 403 APPROVER_REPLACED", status, body)
	}
}

// TestTheSubjectRevokeRefusesARequestItCannotServe: a body missing any of its five fields, or
// naming one account as both the replaced one and the holder, is 400 with the code that says
// which, and records nothing: the account it named still approves.
func TestTheSubjectRevokeRefusesARequestItCannotServe(t *testing.T) {
	ts := newTestServer(t)
	replaced := accountOf(testApprover)
	for _, c := range []struct {
		name, person, issuer, subject, holder string
		generation                            *int64
		code                                  string
	}{
		{"no person", "", googleIssuer, replaced.Subject, "209", new(int64(1)), "PERSON_REQUIRED"},
		{"no issuer", testApprover, "", replaced.Subject, "209", new(int64(1)), "ISSUER_REQUIRED"},
		{"no subject", testApprover, googleIssuer, " ", "209", new(int64(1)), "SUBJECT_REQUIRED"},
		{"no holder", testApprover, googleIssuer, replaced.Subject, "", new(int64(1)), "HOLDER_SUBJECT_REQUIRED"},
		{"one account twice", testApprover, googleIssuer, replaced.Subject, replaced.Subject, new(int64(1)), "SUBJECT_IS_HOLDER"},
		{"no session generation", testApprover, googleIssuer, replaced.Subject, "209", nil, "SESSION_GENERATION_REQUIRED"},
	} {
		status, body := ts.revokeSubject(t, c.person, c.issuer, c.subject, c.holder, c.generation)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != c.code {
			t.Errorf("the revoke with %s = %d %s, want 400 %s", c.name, status, body, c.code)
		}
	}
	ts.approvedLogin(t, testApprover, "example-host-unrefused", &replaced)
}

// TestARevokeRecordsItsGenerationAndAnApprovalCheckedBeforeItIsRefused: the revoke records the
// session_generation it carries beside the replaced account, and a repeat raises it to the greater
// of the two. A repeat carrying a lower one was superseded by the revoke that recorded the greater:
// it answers 200 "superseded", with superseded_by that greater generation, and records nothing. A
// machine-login approval carrying an approver_session_generation below the recorded generation,
// checked at Dispatch before the replacement, is 403 APPROVER_REPLACED and mints nothing, the login
// staying pending, whether it names no account or the holder; one carrying that generation, or a
// greater one, is approved and mints.
func TestARevokeRecordsItsGenerationAndAnApprovalCheckedBeforeItIsRefused(t *testing.T) {
	ts := newTestServer(t)
	replaced, holder := accountOf(testApprover), holderOf(testApprover)
	for _, revoke := range []struct {
		name    string
		carried *int64
		state   string
		// by is the superseded_by the revoke answers, nil when it applies.
		by   *int64
		want int64
	}{
		{"the first revoke, at 5", new(int64(5)), "revoked", nil, 5},
		{"a repeat at 3", new(int64(3)), "superseded", new(int64(5)), 5},
		{"a repeat at 7", new(int64(7)), "revoked", nil, 7},
	} {
		status, body := ts.revokeSubject(t, testApprover, replaced.Issuer, replaced.Subject, holder.Subject, revoke.carried)
		answer := decode[wire.PersonSubjectRevokedResponse](t, body)
		if status != http.StatusOK || answer.State != revoke.state || !reflect.DeepEqual(answer.SupersededBy, revoke.by) {
			t.Fatalf("%s answered %d %s, want 200 %s", revoke.name, status, body, revoke.state)
		}
		if got := ts.replacedAt(t, testApprover, replaced); got == nil || *got != revoke.want {
			t.Fatalf("after %s the replacement is recorded at %v, want %d", revoke.name, got, revoke.want)
		}
	}

	recordID, code := ts.pendingMachineLogin(t, testApprover, "example-host-checked-before")
	for _, account := range []*store.IssuerSubject{nil, &holder} {
		status, body := ts.approveLoginAs(t, recordID, code, testApprover, account, new(int64(6)))
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "APPROVER_REPLACED" {
			t.Fatalf("an approval naming %v checked at generation 6, before the replacement at 7 = %d %s, want 403 APPROVER_REPLACED", account, status, body)
		}
		if n := ts.mintedFrom(t, recordID); n != 0 {
			t.Fatalf("the refused approval naming %v minted %d credential(s), want none", account, n)
		}
	}
	for _, approval := range []struct {
		account    *store.IssuerSubject
		generation int64
	}{{nil, 7}, {&holder, 8}} {
		if approval.account != nil {
			recordID, code = ts.pendingMachineLogin(t, testApprover, "example-host-checked-after")
		}
		status, body := ts.approveLoginAs(t, recordID, code, testApprover, approval.account, new(approval.generation))
		approved := decode[struct {
			CredentialID *string `json:"credential_id"`
		}](t, body)
		if status != http.StatusOK || approved.CredentialID == nil || ts.mintedFrom(t, recordID) != 1 {
			t.Fatalf("an approval naming %v checked at generation %d, at or after the replacement at 7 = %d %s, want 200 minting one credential", approval.account, approval.generation, status, body)
		}
	}
}

// approvalsWithoutGeneration is ts's broker_machine_login_approvals_without_generation_total, 0
// when it has no sample.
func (ts *testServer) approvalsWithoutGeneration(t *testing.T) int {
	t.Helper()
	w := httptest.NewRecorder()
	ts.registry.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(w.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "broker_machine_login_approvals_without_generation_total "); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("broker_machine_login_approvals_without_generation_total is no count: %q", value)
			}
			return n
		}
	}
	return 0
}

// TestAnApprovalWithoutAGenerationIsCounted: a machine-login approve whose body carries no
// approver_session_generation adds one to broker_machine_login_approvals_without_generation_total,
// and one that carries it adds none, so the counter reads how many approves the no-generation
// fallback (machine.ApprovalInFlightWindow) still decides.
func TestAnApprovalWithoutAGenerationIsCounted(t *testing.T) {
	ts := newTestServer(t)
	if n := ts.approvalsWithoutGeneration(t); n != 0 {
		t.Fatalf("before any approve the counter reads %d, want 0", n)
	}
	recordID, code := ts.pendingMachineLogin(t, testApprover, "example-host-without-generation")
	if status, body := ts.approveLoginAs(t, recordID, code, testApprover, nil, nil); status != http.StatusOK {
		t.Fatalf("an approve without a generation = %d %s, want 200", status, body)
	}
	if n := ts.approvalsWithoutGeneration(t); n != 1 {
		t.Fatalf("after one approve without a generation the counter reads %d, want 1", n)
	}
	recordID, code = ts.pendingMachineLogin(t, testApprover, "example-host-with-generation")
	if status, body := ts.approveLoginAs(t, recordID, code, testApprover, nil, new(int64(0))); status != http.StatusOK {
		t.Fatalf("an approve with generation 0 = %d %s, want 200", status, body)
	}
	if n := ts.approvalsWithoutGeneration(t); n != 1 {
		t.Fatalf("after an approve with a generation the counter reads %d, want 1 still", n)
	}
}

// passkeyIssuer stands for Dispatch's outside origin, the issuer Dispatch records for a restricted
// member who signs in with a passkey (people.sub_issuer), and passkeyAccountOf for the account
// such a sign-in records: 16 random bytes, base64url, never a Google `sub`.
const passkeyIssuer = "https://dispatch.outside.example"

func passkeyAccountOf(subject string) store.IssuerSubject {
	return store.IssuerSubject{Issuer: passkeyIssuer, Subject: subject}
}

// TestPasskeyApproverIssuerIsRecordedAndComparedAtTheBroker (plan hardening H7): a machine-login
// approval Dispatch relays for a restricted member who signed in with a passkey names the outside
// origin as the approving account's issuer. The broker records that issuer on the login it mints,
// and later compares it, issuer and subject both: a revoke that names the same subject under
// Google's issuer ends nothing of it, the revoke naming the outside origin ends it, and an approval
// as that account afterwards is 403 APPROVER_REPLACED and mints nothing.
func TestPasskeyApproverIssuerIsRecordedAndComparedAtTheBroker(t *testing.T) {
	ts := newTestServer(t)
	member := passkeyAccountOf("Zm9yLXRoZS1wYXNza2V5IQ")
	holder := passkeyAccountOf("bmV3LWVucm9sbG1lbnQhIQ")
	login := ts.approvedLogin(t, testApprover, "example-host-passkey", &member)
	if got := ts.approverAccountOf(t, login); got != member {
		t.Fatalf("the login a passkey member approved records the approving account %+v, want %+v", got, member)
	}

	sameSubjectOfGoogle := store.IssuerSubject{Issuer: googleIssuer, Subject: member.Subject}
	if got := ts.revokedSubject(t, testApprover, sameSubjectOfGoogle, holderOf(testApprover), 1); got.MachineLogins != 0 {
		t.Fatalf("a revoke naming the member's subject under Google's issuer ended %d machine login(s), want none: the broker compared the subject alone", got.MachineLogins)
	}
	if revoked := ts.credentialRevoked(t, login); revoked {
		t.Fatal("the passkey member's login was revoked by a revoke of another issuer's account")
	}

	if got := ts.revokedSubject(t, testApprover, member, holder, 2); got.MachineLogins != 1 {
		t.Fatalf("the revoke naming the outside origin's account ended %d machine login(s), want the member's 1", got.MachineLogins)
	}
	if revoked := ts.credentialRevoked(t, login); !revoked {
		t.Fatal("the passkey member's login stands after the revoke naming its own account")
	}
	recordID, code := ts.pendingMachineLogin(t, testApprover, "example-host-passkey-again")
	if status, body := ts.approveLoginAs(t, recordID, code, testApprover, &member, nil); status != http.StatusForbidden || decode[wireError](t, body).Code != "APPROVER_REPLACED" {
		t.Fatalf("an approval as the revoked passkey account = %d %s, want 403 APPROVER_REPLACED", status, body)
	}
	if n := ts.mintedFrom(t, recordID); n != 0 {
		t.Fatalf("the refused approval minted %d credential(s), want none", n)
	}
}
