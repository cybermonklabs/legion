package auth

import (
	"context"
	"errors"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// ErrAddressHeldByMember is PeopleStore.SignInSubject's refusal of a restricted member's sign-in
// (a person from outside the domain) to an address whose row records a full member under another
// Google account, or under none: the address is a member's, often on another domain the Workspace
// owns, so a person from outside never takes it, and the sign-in writes nothing.
var ErrAddressHeldByMember = errors.New("the address is a full member's, under another Google account")

// PersonMembership is what Dispatch holds to confirm that a person signed in through the sign-in
// issuer may still use it: the issuer's refresh token for them and how long ago their membership
// was last confirmed. A person recorded any other way (the local/test identity header, the local
// dev sign-in, the people migration), whose sign-in has ended, or whose stored refresh token does
// not open (store.PgPeopleStore keeps it sealed under the signing key), holds neither: an empty
// RefreshToken and Confirmed false. ConfirmedAge is the confirmation's age as the store's clock
// measures it (the database's now() minus the time it recorded), never a server process's clock,
// so every process judges a confirmation's age alike; it is zero when Confirmed is false. Subject
// is the Google account (`sub`) that last signed in to the address with Google for a Workspace
// domain; empty for an address no such sign-in recorded. Ending a sign-in keeps it, and keeps Kind,
// the member kind the address's last sign-in or confirmation recorded.
type PersonMembership struct {
	RefreshToken string
	Confirmed    bool
	ConfirmedAge time.Duration
	Subject      string
	Kind         model.MemberKind
}

// PeopleStore persists the people who have signed in to Dispatch, keyed by lowercase email: the
// assignee picker's options, and each sign-in's membership. Every sign-in and confirmation is
// dated by the store's own clock (the database's now()), not by the caller's.
type PeopleStore interface {
	// Record notes that email has signed in, keeping any membership it already holds.
	Record(ctx context.Context, email string) error
	// SignIn records a sign-in through a Cognito sign-in pool, confirmed now, as a full member;
	// the stored subject is neither read nor written. When email already holds a refresh token
	// stored in plain text, the sign-in retires it first.
	SignIn(ctx context.Context, email, refreshToken string) error
	// SignInSubject records a sign-in for a Workspace domain, confirmed now: subject is the Google
	// account's `sub`, and kind the member kind the sign-in rule named (model.MemberFull or
	// model.MemberRestricted; any other kind is refused before any write). Recording a subject
	// email's row did not record, a fresh row's first included, ends every session of email in the
	// same statement (the database does, migration 20261118041916), and when the row recorded
	// another subject, which the sign-in replaces, it revokes every personal token of email too,
	// whatever member kind the row held, and records that Dispatch owes the secrets broker a
	// revocation of every credential that account holds under email, of either member kind (a
	// restricted account can have made broker credentials while it was full, under the same
	// subject); all of these happen in one transaction, so a revoke that fails leaves the old
	// subject for the next sign-in to revoke again. It is the one write that changes the subject an
	// address records, so no cookie or token of a replaced account acts as the new one. When kind
	// is not the member kind the row held, the same transaction announces the change to every
	// process (store.AccessChangedChannel).
	// When email already holds a refresh token stored in plain text, the sign-in retires it
	// first. A restricted sign-in to an address whose row records a full member under another
	// subject, or none, is ErrAddressHeldByMember and writes nothing, so the member's sessions,
	// tokens and refresh token stand.
	SignInSubject(ctx context.Context, email, subject, refreshToken string, kind model.MemberKind) error
	// Membership reads email's membership; found is false for a person who never signed in.
	Membership(ctx context.Context, email string) (membership PersonMembership, found bool, err error)
	// ConfirmSubject records a refresh that confirmed email's membership now, as kind (refused as
	// SignInSubject refuses it), with refreshToken to use next: a Google sign-in's under its
	// subject, a sign-in pool's under the empty subject its row records. The write happens only
	// while the row still records subject (empty: records none), the account the refresh read, and
	// still holds readToken, the refresh token the refresh read, so a refresh that began before
	// another account, or the same person again, signed in to the address cannot overwrite the new
	// sign-in's token or member kind. A write that changes the member kind announces it to every
	// process in the same transaction (store.AccessChangedChannel), so the person's open document
	// sockets and live views stop carrying a project they no longer read.
	ConfirmSubject(ctx context.Context, email, subject, readToken, refreshToken string, kind model.MemberKind) error
	// EndSubject ends email's sign-in only while its row still records subject (an
	// empty subject: records none) and still holds refreshToken (an empty one: holds none), the
	// refresh token the failed confirmation read. In one transaction it advances email's session
	// generation and forgets the refresh token and confirmation, and ended reports that it did; a
	// row another Google account has signed in to since, or the same account again with a new
	// refresh token, is left as it is, so a failed refresh that began before a sign-in never ends
	// that sign-in's sessions.
	EndSubject(ctx context.Context, email, subject, refreshToken string) (ended bool, err error)
	// End forgets email's refresh token and confirmation, keeping the person and their subject.
	End(ctx context.Context, email string) error
}
