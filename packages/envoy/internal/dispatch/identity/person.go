package identity

import (
	"context"
	"errors"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

var (
	// ErrNoPerson means a verified ID token's username was not issued by a federated provider for
	// an email address, or a Google ID token carries no subject, so it names no person Dispatch
	// can trust.
	ErrNoPerson = errors.New("the sign-in names no person by email")
	// ErrNotMember means the person is not in the group that may use Dispatch.
	ErrNotMember = errors.New("not a member of the group that may use Dispatch")
	// ErrNotInDomain means a Google account is not in the Workspace domain that may use Dispatch.
	ErrNotInDomain = errors.New("not in the domain that may use Dispatch")
	// ErrEmailNotVerified means Google has not verified the account's email.
	ErrEmailNotVerified = errors.New("email not verified by Google")
	// ErrNoEmail means a Google ID token carries no email address to name the person by.
	ErrNoEmail = errors.New("no email in the sign-in")
	// ErrSignInSuperseded means another account's sign-in has already taken the address since
	// this one recorded its subject, so the cookie this sign-in would issue must not be given out.
	ErrSignInSuperseded = errors.New("another account signed in to this address meanwhile")
	// ErrNotListed means a restricted member, a person from outside the sign-in domain, is named by
	// no project's access list (OutsideRule.AdmitsRestricted), so they may not act.
	ErrNotListed = errors.New("no Dispatch project's access list names this account")
	// ErrMembershipUnreadable means the access lists could not be read to decide whether a
	// restricted member may act (OutsideRule.AdmitsRestricted): no refusal, so a caller keeps any
	// sign-in the person already holds and fails only the request.
	ErrMembershipUnreadable = errors.New("the access lists could not be read")
	// ErrOutsideSignInOff means the rule admits nobody from outside its domain (GroupRule,
	// DomainRule: a deployment that admits no restricted member), so a restricted member it is
	// asked about (RestrictedAdmission) is not admitted.
	ErrOutsideSignInOff = errors.New("this deployment signs in nobody from outside its Google Workspace domain")
)

// RestrictedAdmission is the one question every credential acting for a restricted member asks
// at each use: whether the sign-in rule admits email, a person from outside the Workspace, now. A
// SignInRule answers it, so a restricted member's personal token and a token the secrets broker
// issued naming them (cmd/dispatch hands the router its rule, the API asks it) are held to exactly
// the rule that admits the member, and to no second copy of it.
type RestrictedAdmission interface {
	// AdmitsRestricted answers nil while the rule admits email as a restricted member, judged by
	// the address alone, else why not: ErrOutsideSignInOff under a rule that admits nobody from
	// outside, ErrNotInDomain, ErrWorkspaceAddress or ErrNotListed under OutsideRule, and an error
	// matching ErrMembershipUnreadable, which is no refusal, when the lists cannot be read.
	AdmitsRestricted(ctx context.Context, email string) error
}

// SignedIn is the person a sign-in rule admits: Email, their lowercase email, and Kind, how they
// belong to the deployment (model.MemberFull or model.MemberRestricted). A refusal that names the
// person returns their Email alone, Kind left model.MemberNone.
type SignedIn struct {
	Email string
	Kind  model.MemberKind
}

// SignInRule is who may sign in to Dispatch and how Dispatch treats their sign-in: GroupRule for
// the members of a Cognito sign-in pool's group, DomainRule for the people of a Google Workspace
// domain, or PasskeyRule for those people beside the restricted members an access list names, who
// sign in with a passkey.
// cmd/dispatch's signInRule chooses one from boot's settings, once, and every other package
// consults the value it returns rather than branching on which settings are set.
type SignInRule interface {
	// Person names the person a verified ID token signs in under this rule, with their member
	// kind, and checks they may use Dispatch; it reads only the claims of the rule's own issuer.
	// The email is lowercased, and a refusal that has one returns it so the refusal can name them.
	Person(ctx context.Context, claims oidc.Claims) (SignedIn, error)
	// RestrictedAdmission asks whether the rule admits a restricted member, by address alone, for
	// a credential that acts for one.
	RestrictedAdmission
	// RefusalLogFields names the slog fields a sign-in refusal's log line carries. Callers append
	// "reason", err to what this returns.
	RefusalLogFields(claims oidc.Claims) []any
	// Subject is the account a sign-in under this rule records with the person
	// (auth.PeopleStore.SignInSubject's subject); empty for a rule that records none.
	Subject(claims oidc.Claims) string
	// BootLog logs that boot is signing people in under this rule through issuer as clientID.
	BootLog(issuer, clientID string)
	// CanRefresh says whether stored, the subject a membership's row records
	// (auth.PersonMembership.Subject, empty when it records none), lets this rule confirm it
	// without asking the issuer first.
	CanRefresh(stored string) bool
	// ConfirmSubject is the subject Membership.Confirm should record for a refresh whose verified
	// claims, and whose membership's stored subject, are given.
	ConfirmSubject(claims oidc.Claims, stored string) (string, error)
	// End ends email's sign-in under this rule, returning ErrNoIdentity once it no longer stands.
	// read is the membership the failed confirmation read: every rule ends only while the row
	// still records its subject and holds its refresh token, advancing the session generation in
	// that transaction (auth.PeopleStore.EndSubject).
	End(ctx context.Context, people auth.PeopleStore, email string, read auth.PersonMembership, reason string) error
	// SignedOutReason is the reason a 401 tells the person whose session for email has ended
	// (SignedOut.Reason), read from people and whatever else the rule reads, or "" for none. A
	// rule tells one only while no sign-in of the address stands (endedRestricted: its refresh
	// token is gone) and only for a person it no longer admits, so a full member, and a person
	// whose sign-in another account took or who signed in again, is told none; GroupRule tells
	// none at all.
	SignedOutReason(ctx context.Context, people auth.PeopleStore, email string) (string, error)
	// WriteSignIn records person's sign-in under this rule (auth.PeopleStore.SignIn or
	// .SignInSubject), with the member kind Person named, given the subject this rule's own
	// Subject method already resolved; the store dates it by the database's clock. A restricted
	// member's sign-in to the address of a full member under another account is
	// auth.ErrAddressHeldByMember, a refusal that records nothing.
	WriteSignIn(ctx context.Context, people auth.PeopleStore, person SignedIn, subject, refreshToken string) error
	// WriteConfirm records a refresh that confirmed person's membership under this rule
	// (auth.PeopleStore.ConfirmSubject), with the member kind Person named again, given the subject
	// this rule's own ConfirmSubject method already resolved and read, the membership the refresh
	// read: every rule writes only while the row still records read's subject and holds read's
	// refresh token. The store dates it by the database's
	// clock.
	WriteConfirm(ctx context.Context, people auth.PeopleStore, read auth.PersonMembership, person SignedIn, subject, refreshToken string) error
	// ConfirmCookie confirms, after a sign-in has read the session generation it will put in a
	// cookie, that email's row still names subject as the account that just signed in
	// (auth.PeopleStore.Membership): a sign-in by another account that committed since has
	// already taken the address, and issuing a cookie here would hand it to the wrong account.
	// ErrSignInSuperseded means it has. A rule that records no subject has nothing to confirm.
	ConfirmCookie(ctx context.Context, people auth.PeopleStore, email, subject string) error
}
