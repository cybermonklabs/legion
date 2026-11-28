package exchange_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// secondCredentialID is a second launcher credential of the person credentialID's sign-in names:
// a second Dispatch sign-in of that person.
const secondCredentialID = "77777777-8888-9999-aaaa-bbbbbbbbbbbb"

// TestEachDispatchSignInHoldsAPrincipalOfItsOwn: the claims of a Dispatch sign-in, the one Machine
// decides with a consented scope set, name the principal consented:<person>:<credential>, written
// out here so a change of the form fails beside the person's own. It is not the person's
// (person:<person>), which a machine login of the same person holds, and it is not another
// sign-in's: the broker holds a session name to one principal (oauthstore.Store.BindSession), so
// a sign-in takes no name its person's machine login or other sign-in wrote under, and the
// listener records it as no session of theirs. A second trade naming the same credential is the
// same principal; that a refresh keeps the credential, and so the principal, is api's
// TestEachRefreshOfADispatchSignInIsADispatchSignIn.
//
// Controls: building a consented claim's principal from its person alone
// (oauthwire.ConsentedPrincipal(approver, "")), or giving it PersonPrincipal, fails the first row,
// which names the sign-in's own credential.
func TestEachDispatchSignInHoldsAPrincipalOfItsOwn(t *testing.T) {
	set := listed(t)
	first := "consented:" + person + ":" + credentialID
	if got := oauthwire.ConsentedPrincipal(person, credentialID); got != first {
		t.Fatalf("ConsentedPrincipal(%s, %s) = %q, want %q", person, credentialID, got, first)
	}
	approved := []string{"person"}
	signIn, err := set.Machine(credentialID, person, "ada-dispatch-session", nil, exchange.ApproverUnrestricted, &approved)
	if err != nil || signIn.Principal != first {
		t.Fatalf("Machine(a Dispatch sign-in of %s) = %+v, %v; want the principal %q", person, signIn, err, first)
	}
	none := []string{}
	approvedNone, err := set.Machine(credentialID, person, "ada-dispatch-session", nil, exchange.ApproverUnrestricted, &none)
	if err != nil || approvedNone.Principal != first {
		t.Fatalf("Machine(the same sign-in's credential, approved with no scope) = %+v, %v; want the principal %q", approvedNone, err, first)
	}
	again, err := set.Machine(credentialID, person, "ada-dispatch-session-2", nil, exchange.ApproverUnrestricted, &approved)
	if err != nil || again.Principal != first {
		t.Fatalf("Machine(a second trade of the same sign-in) = %+v, %v; want the principal %q, the sign-in's own", again, err, first)
	}
	second, err := set.Machine(secondCredentialID, person, "ada-dispatch-session", nil, exchange.ApproverUnrestricted, &approved)
	if err != nil || second.Principal != "consented:"+person+":"+secondCredentialID {
		t.Fatalf("Machine(a second sign-in of %s) = %+v, %v; want the principal consented:%s:%s", person, second, err, person, secondCredentialID)
	}
	login, err := set.Machine(credentialID, person, "ada-devbox", nil, exchange.ApproverUnrestricted, nil)
	if err != nil || login.Principal != personPrincipal {
		t.Fatalf("Machine(a machine login of %s) = %+v, %v; want the principal %q, its person's own", person, login, err, personPrincipal)
	}
}
