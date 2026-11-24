package google_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
)

// exchangeClaims is the claims a trade decided, which a machine's launcher subject reads nothing
// of.
func exchangeClaims() exchange.Claims { return exchange.Claims{} }

// TestReadGoogleIdentityReadsGooglesClaimShapes: the one reading of a Google identity token's own
// claims, which the provider decides on and its caller verifier reads the same token through. A
// claim of another shape reads as absent, and a token minted off a Compute Engine instance reads as
// carrying no instance rather than as a malformed one.
func TestReadGoogleIdentityReadsGooglesClaimShapes(t *testing.T) {
	whole := google.ReadIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	if whole.Account != pinnedAccount || !whole.EmailVerified || !whole.Compute.Whole() || whole.Compute.Empty() {
		t.Fatalf("ReadGoogleIdentity(a VM's token) = %+v, want the account, a verified email and the whole instance", whole)
	}
	if whole.Compute.Project != cloudProject || whole.Compute.Instance != instanceName ||
		whole.Compute.Zone != "example-zone-a" || whole.Compute.InstanceID != "1234567890" {
		t.Errorf("ReadGoogleIdentity read the instance as %+v", whole.Compute)
	}
	none := google.ReadIdentity(googleToken(poolAccount, "true", "", ""))
	if none.Account != poolAccount || !none.EmailVerified || !none.Compute.Empty() || none.Compute.Whole() {
		t.Fatalf("ReadGoogleIdentity(a pool's token) = %+v, want the account, a verified email and no instance", none)
	}
	odd := google.ReadIdentity(map[string]any{"email": 7, "email_verified": "yes", "google": "not an object"})
	if odd != (google.Identity{}) {
		t.Errorf("ReadGoogleIdentity(claims of other shapes) = %+v, want every field empty", odd)
	}
}

// machineRead is what the provider's ReadSubject answers for a machine of account.
func machineRead(account string) identity.Subject {
	return identity.Subject{Kind: identity.SubjectMachine, Account: identity.Account{Kind: google.Kind, Address: account}, Principal: "google:" + account}
}

// TestMachineSubjectNamesOneMachineAndTheKeyItHolds: the subject the broker keys a machine's
// launcher credential by. A machine is named by where it runs and by the key its proof
// presented, so a copy of its Google identity token presented with another key asks for a
// subject of its own and moves nothing of the machine's; two machines of one account are two
// credentials; and two instances of one account presenting one key are two subjects, which the
// live-key index refuses rather than letting them share a credential. Every one of them reads
// its account and its key back, and a Kubernetes ServiceAccount reads as no machine at all.
func TestMachineSubjectNamesOneMachineAndTheKeyItHolds(t *testing.T) {
	p := listed(t)
	onInstance := google.ReadIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	onNone := google.ReadIdentity(googleToken(poolAccount, true, "", ""))

	instanceSubject := google.MachineSubject(onInstance, "thumbprint-one")
	if want := pinnedAccount + "@" + cloudProject + "/example-zone-a/1234567890#thumbprint-one"; instanceSubject != want {
		t.Fatalf("MachineSubject(a Compute Engine machine) = %q, want %q", instanceSubject, want)
	}
	if other := google.MachineSubject(onInstance, "thumbprint-two"); other == instanceSubject {
		t.Errorf("another key on the same instance asks for the subject %q the machine's own credential holds; a copied token must move no credential", instanceSubject)
	}
	first := google.MachineSubject(onNone, "thumbprint-one")
	if want := poolAccount + "#thumbprint-one"; first != want {
		t.Fatalf("MachineSubject(a machine minted on no instance) = %q, want %q", first, want)
	}
	if second := google.MachineSubject(onNone, "thumbprint-two"); second == first {
		t.Errorf("two machines of %s with keys of their own share the subject %q; each must hold a credential of its own", poolAccount, first)
	}
	for _, c := range []struct {
		subject string
		want    identity.Subject
		claimed bool
	}{
		{subject: instanceSubject, want: machineRead(pinnedAccount), claimed: true},
		{subject: first, want: machineRead(poolAccount), claimed: true},
		{subject: "system:serviceaccount:legion-system:legion-daemon"},
		{subject: pinnedAccount},
		{subject: "#thumbprint-one"},
		{subject: ""},
	} {
		if got, claimed := p.ReadSubject(c.subject); got != c.want || claimed != c.claimed {
			t.Errorf("ReadSubject(%q) = %+v, %t; want %+v, %t", c.subject, got, claimed, c.want, c.claimed)
		}
	}
}

// TestPersonSubjectNamesOneSignInAndReadsItsMachineBack: a sign-in's subject tells one person's
// sign-in on one box from every other — two people on one box, and one person on two boxes, are
// four different strings — and the machine's account reads back out of it, which is what holds
// the credential to a listed fleet on every call. No subject of any other kind reads as a
// sign-in's, and a sign-in's never reads as a machine's, whose credential acts for nobody.
func TestPersonSubjectNamesOneSignInAndReadsItsMachineBack(t *testing.T) {
	p := listed(t)
	const (
		boxOne = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		boxTwo = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	)
	subjects := map[string]struct {
		subject, account string
	}{
		"one person on one box":                  {google.PersonSubject(person, pinnedAccount, boxOne), pinnedAccount},
		"the same person on another box":         {google.PersonSubject(person, pinnedAccount, boxTwo), pinnedAccount},
		"the same person on another fleet's box": {google.PersonSubject(person, poolAccount, boxOne), poolAccount},
		"another person on the first box":        {google.PersonSubject(stranger, pinnedAccount, boxOne), pinnedAccount},
	}
	seen := map[string]string{}
	for name, c := range subjects {
		if other, taken := seen[c.subject]; taken {
			t.Errorf("%s and %s spell one subject, %q: each sign-in holds a credential of its own", name, other, c.subject)
		}
		seen[c.subject] = name
		want := identity.Subject{Kind: identity.SubjectSignIn, Account: identity.Account{Kind: google.Kind, Address: c.account}, Principal: "google:" + c.account}
		if got, claimed := p.ReadSubject(c.subject); got != want || !claimed {
			t.Errorf("ReadSubject(%q) = %+v, %t; want %+v: it is what holds the sign-in to a listed fleet", c.subject, got, claimed, want)
		}
	}
	onInstance := google.ReadIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	onNone := google.ReadIdentity(googleToken(poolAccount, true, "", ""))
	for _, subject := range []string{
		google.MachineSubject(onInstance, boxOne),
		google.MachineSubject(onNone, boxOne),
		"system:serviceaccount:example-ns:example-worker",
		"signin:" + person,
		"signin:|" + pinnedAccount + "#" + boxOne,
		"",
	} {
		if got, _ := p.ReadSubject(subject); got.Kind == identity.SubjectSignIn {
			t.Errorf("ReadSubject(%q) = %+v; want no sign-in: it is a subject of another kind", subject, got)
		}
	}
}

// TestReadSubjectReadsEveryShapeAsBefore: every shape of subject today's reader read, the
// malformed ones included, reads the same kind and account: a provider that claimed too much
// would turn a person's pod login into a machine's.
func TestReadSubjectReadsEveryShapeAsBefore(t *testing.T) {
	p := listed(t)
	signInRead := func(account string) identity.Subject {
		return identity.Subject{Kind: identity.SubjectSignIn, Account: identity.Account{Kind: google.Kind, Address: account}, Principal: "google:" + account}
	}
	for _, c := range []struct {
		subject string
		want    identity.Subject
		claimed bool
	}{
		{subject: "signin:ada@example.com|" + pinnedAccount + "#tp", want: signInRead(pinnedAccount), claimed: true},
		{subject: "signin:ada@example.com", claimed: false},
		{subject: "signin:|" + pinnedAccount + "#tp", claimed: false},
		{subject: "signin:ada@example.com|#tp", claimed: false},
		{subject: "signin:ada@example.com|" + pinnedAccount, claimed: false},
		{subject: pinnedAccount + "@" + cloudProject + "/example-zone-a/1#tp", want: machineRead(pinnedAccount), claimed: true},
		{subject: pinnedAccount + "#tp", want: machineRead(pinnedAccount), claimed: true},
		{subject: "acct@proj/zone/1", claimed: false},
		{subject: "#tp", claimed: false},
		{subject: "", claimed: false},
		{subject: "system:serviceaccount:example-ns:example-worker", claimed: false},
		// An address with "@" and no "/" after it is the account itself, as today.
		{subject: "a@b#tp", want: machineRead("a@b"), claimed: true},
		// "@" first leaves nothing before it, so the whole machine is the account, as today.
		{subject: "@zone/1#tp", want: machineRead("@zone/1"), claimed: true},
	} {
		if got, claimed := p.ReadSubject(c.subject); got != c.want || claimed != c.claimed {
			t.Errorf("ReadSubject(%q) = %+v, %t; want %+v, %t", c.subject, got, claimed, c.want, c.claimed)
		}
	}
}

// TestTheLauncherSubjectIsTodaysSpelling: a machine's trade mints its credential under its own
// subject and host, and a person's sign-in under the sign-in's subject and the acting machine's
// host, each as the exchange minted them before.
func TestTheLauncherSubjectIsTodaysSpelling(t *testing.T) {
	p := listed(t)
	onInstance := googleToken(pinnedAccount, true, cloudProject, instanceName)
	subject, host := p.LauncherSubject(identity.Trade{TokenType: google.SubjectTokenType, Subject: onInstance}, exchangeClaims(), "tp")
	if want := pinnedAccount + "@" + cloudProject + "/example-zone-a/1234567890#tp"; subject != want || host != cloudProject+"/example-zone-a/"+instanceName {
		t.Errorf("LauncherSubject(a machine on an instance) = %q, %q; want %q and the instance", subject, host, want)
	}
	onNone := googleToken(poolAccount, true, "", "")
	if subject, host := p.LauncherSubject(identity.Trade{TokenType: google.SubjectTokenType, Subject: onNone}, exchangeClaims(), "tp"); subject != poolAccount+"#tp" || host != poolAccount {
		t.Errorf("LauncherSubject(a machine on no instance) = %q, %q; want %s#tp and the account", subject, host, poolAccount)
	}
	claims := exchangeClaims()
	claims.Person, claims.Actor = person, pinnedAccount
	trade := identity.Trade{TokenType: google.PersonTokenType, Subject: personToken(person, true, signInDomain), Actor: onInstance}
	if subject, host := p.LauncherSubject(trade, claims, "tp"); subject != "signin:"+person+"|"+pinnedAccount+"#tp" || host != cloudProject+"/example-zone-a/"+instanceName {
		t.Errorf("LauncherSubject(a sign-in) = %q, %q; want the sign-in's subject and the machine's instance", subject, host)
	}
}
