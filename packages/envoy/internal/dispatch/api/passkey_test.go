package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// browserSessions is the rig's header identity with each caller a browser's session cookie of the
// address's current session generation, as a person signed in at their browser is to the reads
// that bind a credential to its account (mintCookie, approverStanding): a passkey's sign-in is a
// cookie, and only a cookie's caller is bound to the account people records.
type browserSessions struct {
	identity.HeaderIdentity
	sessions *store.PgSessionStore
}

func (b browserSessions) SessionCookie(r *http.Request) (string, int64, bool) {
	login, err := b.Login(r)
	if err != nil {
		return "", 0, false
	}
	generation, _, err := b.sessions.CurrentSessionGeneration(r.Context(), login)
	return login, generation, err == nil
}

// passkeyOutsideOrigin is the outside origin of these rows: the issuer Dispatch records a passkey's
// subject under (people.sub_issuer), and the passkey store's.
const passkeyOutsideOrigin = "https://outside.example.com"

// withPasskeys turns passkey sign-in on at passkeyOutsideOrigin, beside the restricted rig's
// Google sign-in, whose issuer the deployment's sign-in records accounts of.
func withPasskeys(options *testServerOptions) {
	options.passkeyIssuer = passkeyOutsideOrigin
	options.signInIssuer = "https://accounts.google.com"
}

// recordPasskeyAccount writes erin's people row as her first passkey enrollment leaves it: the
// subject the enrollment made, under the outside origin, and her sign-in standing.
func recordPasskeyAccount(t *testing.T, database *store.Store, email, subject string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update people set sub = $2, sub_issuer = $3, confirmed_at = now(), signed_in_at = now()
		where email = $1`, email, subject, passkeyOutsideOrigin); err != nil {
		t.Fatalf("record %s's passkey account: %v", email, err)
	}
}

// TestPasskeyApprovalRelaysTheOutsideOriginAsTheApproversIssuer (H7, Dispatch's half): a restricted
// member whose account is a passkey's, its subject under the outside origin, approves their own
// machine login at their browser, and Dispatch relays the approving account to the secrets broker
// as that pair: the broker's minted login records the outside origin and the passkey's subject,
// never Google's issuer. The broker's half, that it compares the pair it recorded, is the broker's
// TestPasskeyApproverIssuerIsRecordedAndComparedAtTheBroker.
//
// Control: relay every account under the sign-in issuer (approve ignoring
// credentialAccount.issuer), and the login records Google's issuer and the row fails naming it.
func TestPasskeyApprovalRelaysTheOutsideOriginAsTheApproversIssuer(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	r.server.deps.Identity = browserSessions{HeaderIdentity: r.server.deps.Identity.(identity.HeaderIdentity), sessions: store.NewPgSessionStore(r.database.Pool)}
	const subject = "cGFzc2tleS1zdWJqZWN0IQ"
	recordPasskeyAccount(t, r.database, accessErin, subject)
	_, _, credential := r.approvedLogin(t, accessErin, "erin-laptop")
	var issuer, approver string
	if err := r.broker.Store.Pool.QueryRow(t.Context(), `select coalesce(approver_issuer, ''), coalesce(approver_subject, '')
		from launcher_credentials where id = $1::uuid`, credential).Scan(&issuer, &approver); err != nil {
		t.Fatalf("read the approving account of erin's login: %v", err)
	}
	if issuer != passkeyOutsideOrigin || approver != subject {
		t.Fatalf("erin's machine login records the approving account (%q, %q), want her passkey's (%q, %q)", issuer, approver, passkeyOutsideOrigin, subject)
	}
}

// TestAResetRevokesTheMembersDeviceLogins (R5, R16): the owner's Reset of a restricted member ends
// their sign-in, deletes their passkeys, and records in the same transaction that Dispatch owes the
// secrets broker the revocation of every machine login they approved, as a removal from the list
// does; the broker revocation loop's next pass pays it, so the device login a hijacker approved
// with the person's passkey ends too: its agent's token introspects not active, and its refresh is
// refused. The person stays on ACC's list.
//
// Control: owe nothing on a Reset (resetProjectAccessPerson not calling
// recordMachineLoginRevocation), and the row fails naming the revocation not owed.
func TestAResetRevokesTheMembersDeviceLogins(t *testing.T) {
	r := newRestrictedTokenRig(t, withPasskeys)
	ctx := t.Context()
	recordPasskeyAccount(t, r.database, accessErin, "cmVzZXQtc3ViamVjdC0xIQ")
	erins, erinsDevice, _ := r.approvedLogin(t, accessErin, "erin-laptop")
	erinsToken := r.sessionToken(t, erins, erinsDevice, "erin-agent", "ACC")

	reset := dispatchRequest(t, r.dispatch, http.MethodPost, "/api/v1/projects/ACC/access/people/"+accessErin+"/reset", nil, "alice")
	if reset.Code != http.StatusOK {
		t.Fatalf("alice's Reset of erin: %d %s, want 200", reset.Code, reset.Body.String())
	}
	if people := decodeBody[struct {
		People []string `json:"people"`
	}](t, reset).People; len(people) != 1 || people[0] != accessErin {
		t.Fatalf("ACC's list after the Reset is %v, want erin still on it", people)
	}
	if owed, _, err := store.NewMachineLoginRevocations(r.database.Pool).Owing(ctx, accessErin); err != nil || !owed {
		t.Fatalf("the revocation of erin's machine logins is owed %t (%v) after the Reset, want owed", owed, err)
	}
	var confirmed bool
	if err := r.database.Pool.QueryRow(ctx, `select confirmed_at is not null from people where email = $1`, accessErin).Scan(&confirmed); err != nil || confirmed {
		t.Fatalf("erin's sign-in stands %t (%v) after the Reset, want it ended", confirmed, err)
	}
	r.server.revokeBrokerDebts(ctx)
	if r.introspect(t, erinsToken).Active {
		t.Errorf("erin's agent's token introspects active after the Reset, want it ended")
	}
	if refreshed, err := r.client.Refresh(t, erins.key, erinsDevice); err == nil {
		t.Errorf("erin's device login refreshed after the Reset, answering a token expiring %s; want it refused", refreshed.Expiry)
	}
}
