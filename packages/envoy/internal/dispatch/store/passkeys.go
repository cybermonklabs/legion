package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The refusals of a passkey's enrollment and sign-in (PgPasskeys), each answered by its own code
// at the passkey routes (routes/passkey.go).
var (
	// ErrInvitationUnknown means no link of a project's owner has the token: never made, or
	// ended by a newer link for the same person and project, a removal from that project's list
	// or the owner's Reset of the person on any project.
	ErrInvitationUnknown = errors.New("no invitation has this token")
	// ErrInvitationUsed means the link was used: it enrolls one passkey, once.
	ErrInvitationUsed = errors.New("the invitation was used")
	// ErrInvitationExpired means the link lapsed, 24 hours after the owner made it.
	ErrInvitationExpired = errors.New("the invitation has expired")
	// ErrInvitationNotListed means the project's access list no longer names the link's email
	// directly.
	ErrInvitationNotListed = errors.New("the project's access list no longer names the invited email")
	// ErrEnrollmentSuperseded means another enrollment recorded another subject for the address
	// while this one was in flight, so the user handle this one's authenticator stored is not the
	// address's: the person starts over.
	ErrEnrollmentSuperseded = errors.New("another enrollment of this address finished first")
	// ErrListedByAnotherOwner means a project whose owner did not make the link names the
	// person directly: the passkey would read that project too, so its owner did not agree to it.
	ErrListedByAnotherOwner = errors.New("another owner's project names this person: only an owner of every project that names them makes their link")
	// ErrPasskeyUnknown means no passkey has the credential id: never enrolled, or deleted by the
	// owner's Reset, a removal from the last list or the hourly check.
	ErrPasskeyUnknown = errors.New("no passkey has this credential")
	// ErrPasskeySignCount means the sign-in's signature counter is not above the stored one
	// while either is non-zero (WebAuthn Level 3 §7.2 step 22: a cloned authenticator, or a
	// sign-in with the same credential that finished first).
	ErrPasskeySignCount = errors.New("the passkey's signature counter did not advance")
	// ErrPasskeyAccountMoved means the address records another account than the passkey's user
	// handle: a sign-in by another account took the address since the passkey was enrolled.
	ErrPasskeyAccountMoved = errors.New("the address records another account than this passkey's")
)

// InvitationLifetime is how long an owner's link lasts: it ends at its first use or 24 hours after
// it was made (R5).
const InvitationLifetime = 24 * time.Hour

// Invitation is one link an owner made (outside_invitations), as its token's hash reads it: the
// email and project it is for, who made it, whether it was used, and whether the database's clock
// is at or past when it lapses.
type Invitation struct {
	Email   string
	Project string
	MadeBy  string
	Used    bool
	Expired bool
}

// PasskeyCredential is one WebAuthn credential as passkeys stores it: its id, the user handle the
// authenticator holds with it, its COSE public key, signature counter, AAGUID (16 bytes, or none),
// the transports the browser reported and its flags in go-webauthn's one-byte packing
// (webauthn.CredentialFlags.MsgpByte).
type PasskeyCredential struct {
	ID         []byte
	UserHandle []byte
	PublicKey  []byte
	SignCount  uint32
	AAGUID     []byte
	Transports []string
	Flags      byte
}

// StoredPasskey is a passkey and the person it belongs to.
type StoredPasskey struct {
	Email string
	PasskeyCredential
}

// PasskeyAccount is what a person's enrollment begins from: Subject, the passkey subject their
// row records under the outside origin ("" for none, so the enrollment makes one); FullMember,
// whether the row is a full member's, which no passkey enrolls; and the ids of the passkeys they
// hold, which the browser is told not to enroll again.
type PasskeyAccount struct {
	Subject     string
	FullMember  bool
	Credentials [][]byte
}

// PgPasskeys is the passkeys restricted members sign in with at the outside origin and the links
// their projects' owners make (migration 20261120165538), whose sign-ins go through people's
// account binding (PgPeopleStore.signInAccountTx) under issuer, the outside origin, as
// oidc.CanonicalIssuer spells it.
type PgPasskeys struct {
	people *PgPeopleStore
	issuer string
}

// NewPgPasskeys returns the passkeys over people's database, recording each passkey's subject
// under issuer (people.sub_issuer).
func NewPgPasskeys(people *PgPeopleStore, issuer string) *PgPasskeys {
	return &PgPasskeys{people: people, issuer: issuer}
}

// Issuer is the issuer people.sub_issuer records for a passkey's subject.
func (p *PgPasskeys) Issuer() string {
	return p.issuer
}

// BeginCeremony records a ceremony a begin started, by its challenge, lapsing lifetime after the
// database's now() (passkey_ceremonies), after deleting every ceremony that lapsed, so the table
// holds no more than one lifetime of begins.
func (p *PgPasskeys) BeginCeremony(ctx context.Context, challenge string, lifetime time.Duration) error {
	hash := sha256.Sum256([]byte(challenge))
	if _, err := p.people.pool.Exec(ctx, `delete from passkey_ceremonies where expires_at <= now()`); err != nil {
		return fmt.Errorf("delete the lapsed passkey ceremonies: %w", err)
	}
	if _, err := p.people.pool.Exec(ctx, `insert into passkey_ceremonies (challenge_sha256, expires_at)
		values ($1, now() + make_interval(secs => $2))`, hash[:], lifetime.Seconds()); err != nil {
		return fmt.Errorf("record the passkey ceremony: %w", err)
	}
	return nil
}

// TakeCeremony deletes the ceremony challenge names and reports whether it was there and had not
// lapsed: a finish takes its ceremony once, before it verifies anything, so its answer is used at
// most once however often it is sent.
func (p *PgPasskeys) TakeCeremony(ctx context.Context, challenge string) (bool, error) {
	hash := sha256.Sum256([]byte(challenge))
	var live bool
	err := p.people.pool.QueryRow(ctx, `delete from passkey_ceremonies where challenge_sha256 = $1 returning expires_at > now()`, hash[:]).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("take the passkey ceremony: %w", err)
	}
	return live, nil
}

// PasskeyReader is a pool or a transaction, which the passkey reads take.
type PasskeyReader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MakeInvitationTx records, in tx, the link an owner makes for email on project: tokenHash, the
// SHA-256 of its 32 random bytes, which no row holds, lapsing InvitationLifetime after the
// database's now(). It first deletes the person's unused links of that project, so one link at a
// time can enroll them, and answers when the new one lapses. Who may make it, and for whom, is the
// caller's to decide first (api.createInvitation).
func MakeInvitationTx(ctx context.Context, tx pgx.Tx, tokenHash [32]byte, email, project, madeBy string) (time.Time, error) {
	if err := DeleteInvitationsTx(ctx, tx, project, email); err != nil {
		return time.Time{}, err
	}
	var expiresAt time.Time
	err := tx.QueryRow(ctx, `insert into outside_invitations (token_sha256, email, project_key, made_by, expires_at)
		values ($1, $2, $3, $4, now() + $5::interval) returning expires_at`,
		tokenHash[:], model.CanonicalLogin(email), project, madeBy, fmt.Sprintf("%d seconds", int(InvitationLifetime/time.Second))).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("record the invitation: %w", err)
	}
	return expiresAt, nil
}

// DeleteInvitationsTx deletes, in tx, email's unused links of project: a newer link and a removal
// from the project's list each end them (the owner's Reset ends every project's,
// DeletePersonInvitationsTx).
func DeleteInvitationsTx(ctx context.Context, tx pgx.Tx, project, email string) error {
	if _, err := tx.Exec(ctx, `delete from outside_invitations where project_key = $1 and email = $2 and used_at is null`,
		project, model.CanonicalLogin(email)); err != nil {
		return fmt.Errorf("end the person's earlier invitations: %w", err)
	}
	return nil
}

// EndPasskeySignInTx ends, in tx, email's sign-in and deletes their passkeys: a removal from the
// last list that names a restricted member (api), the hourly check (settle) and the owner's Reset
// (ResetTx). It advances the address's session generation, so no cookie of theirs acts again,
// forgets the row's refresh token and confirmation, and answers how many passkeys it deleted. An
// address no row holds has neither.
func EndPasskeySignInTx(ctx context.Context, tx pgx.Tx, email string) (int64, error) {
	login := model.CanonicalLogin(email)
	if _, err := tx.Exec(ctx, addressLockSQL, login); err != nil {
		return 0, fmt.Errorf("end the sign-in: lock the address: %w", err)
	}
	if err := tx.QueryRow(ctx, `select 1 from people where email = $1 for update`, login).Scan(new(int)); errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("end the sign-in: %w", err)
	}
	deleted, err := tx.Exec(ctx, `delete from passkeys where email = $1`, login)
	if err != nil {
		return 0, fmt.Errorf("delete the person's passkeys: %w", err)
	}
	if _, err := tx.Exec(ctx, revokeSessionsSQL, login); err != nil {
		return 0, fmt.Errorf("end the person's sessions: %w", err)
	}
	if _, err := tx.Exec(ctx, `update people set refresh_token = null, confirmed_at = null where email = $1`, login); err != nil {
		return 0, fmt.Errorf("end the person's sign-in: %w", err)
	}
	return deleted.RowsAffected(), nil
}

// DeletePersonInvitationsTx deletes, in tx, email's unused links of every project: the owner's
// Reset ends them all, so the person enrolls again only with a link made after it. A caller that
// also takes the address's lock deletes the links first, the order Enroll locks them in (the link,
// then the address).
func DeletePersonInvitationsTx(ctx context.Context, tx pgx.Tx, email string) error {
	if _, err := tx.Exec(ctx, `delete from outside_invitations where email = $1 and used_at is null`, model.CanonicalLogin(email)); err != nil {
		return fmt.Errorf("end the person's unused invitations: %w", err)
	}
	return nil
}

// LockAddressTx takes email's address lock for tx (addressLockSQL), the lock every write that
// signs the address in, ends its passkey sign-in or deletes its passkeys takes first: a removal
// takes it before it reads whether a list still names the person, so two removals from the last
// two lists that name them read one after the other, and the second ends the sign-in.
func LockAddressTx(ctx context.Context, tx pgx.Tx, email string) error {
	if _, err := tx.Exec(ctx, addressLockSQL, model.CanonicalLogin(email)); err != nil {
		return fmt.Errorf("lock the address: %w", err)
	}
	return nil
}

// ResetTx is the owner's Reset of email, in tx (api.resetProjectAccessPerson), whose caller
// deleted the person's unused links first (DeletePersonInvitationsTx), so the links are locked
// before the address, as Enroll locks them. Where the address records a passkey's account (a
// subject under the outside origin), it records subject, a new one, through the account binding
// (signInAccountTx), as another account's sign-in would: that revokes the address's personal
// tokens and, where a broker is configured, owes the broker the revocation of the old account at
// the session generation the Reset leaves, which ends every machine login and sign-in the old
// account approved at the broker, one minted after the Reset's own revocation pass too, and refuses
// every later approve that names it or an older generation. An address that records no passkey's
// account has its personal tokens revoked alone. Last it ends the sign-in and deletes the passkeys
// (EndPasskeySignInTx), so nobody signs in as the person until they enroll with a new link, under
// the new account. It answers how many passkeys it deleted. A full member's row, read under the
// address's lock, is auth.ErrAddressHeldByMember and writes nothing.
func (p *PgPasskeys) ResetTx(ctx context.Context, tx pgx.Tx, email, subject string) (int64, error) {
	login := model.CanonicalLogin(email)
	if _, err := tx.Exec(ctx, addressLockSQL, login); err != nil {
		return 0, fmt.Errorf("reset the person: lock the address: %w", err)
	}
	var recorded, full bool
	if err := tx.QueryRow(ctx, `select coalesce(bool_or(sub_issuer = $2), false), coalesce(bool_or(member_kind = 'full'), false)
		from people where email = $1`, login, p.issuer).Scan(&recorded, &full); err != nil {
		return 0, fmt.Errorf("reset the person: read their account: %w", err)
	}
	// Read under the address's lock, which a Google sign-in takes too: a full member whose first
	// sign-in committed after the caller checked the member kind is no one's to reset.
	if full {
		return 0, auth.ErrAddressHeldByMember
	}
	if recorded {
		if _, err := p.people.signInAccountTx(ctx, tx, login, signInAccount{issuer: p.issuer, subject: subject}, nil, model.MemberRestricted); err != nil {
			return 0, fmt.Errorf("reset the person: record a new account: %w", err)
		}
	} else if _, err := tx.Exec(ctx, revokeAddressTokensSQL, login); err != nil {
		return 0, fmt.Errorf("reset the person: revoke their personal tokens: %w", err)
	}
	return EndPasskeySignInTx(ctx, tx, login)
}

// ListedByAnotherOwner reports whether a project whose owner is not owner names email directly on
// its access list, read through q. A passkey signs its person in to every project that names them,
// so only an owner of every such project makes a link for them, and an enrollment uses a link only
// while its maker still is (R5: no project's owner binds a passkey that reads another owner's
// project).
func ListedByAnotherOwner(ctx context.Context, q PasskeyReader, email, owner string) (bool, error) {
	var other bool
	if err := q.QueryRow(ctx, `select exists (select 1 from project_access a join projects p on p.key = a.project_key
		where a.principal_kind = 'person' and a.principal = $1 and canonical_login(coalesce(p.owner, '')) <> canonical_login($2))`,
		model.CanonicalLogin(email), owner).Scan(&other); err != nil {
		return false, fmt.Errorf("read whether another owner's project names %s: %w", model.CanonicalLogin(email), err)
	}
	return other, nil
}

// ListedByAnotherOwner is the package's ListedByAnotherOwner over the people store's pool, for an
// enrollment's begin (identity.Passkeys), which reads before any transaction.
func (p *PgPasskeys) ListedByAnotherOwner(ctx context.Context, email, owner string) (bool, error) {
	return ListedByAnotherOwner(ctx, p.people.pool, email, owner)
}

// HeldByAnotherOwnersPasskey reports whether email's address holds a passkey enrolled through a
// link someone other than owner made, read through q: a list of owner's that named the person then
// would let that passkey read owner's project, which owner never agreed to, so the list refuses it
// (api.putProjectAccessPerson) until the link's maker resets the person.
func HeldByAnotherOwnersPasskey(ctx context.Context, q PasskeyReader, email, owner string) (bool, error) {
	var held bool
	if err := q.QueryRow(ctx, `select exists (select 1 from passkeys
		where email = $1 and canonical_login(made_by) <> canonical_login($2))`, model.CanonicalLogin(email), owner).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether another owner's link enrolled a passkey of %s: %w", model.CanonicalLogin(email), err)
	}
	return held, nil
}

// ProjectNamesAnotherOwnersPasskey reports whether a person project key's list names directly holds
// a passkey enrolled through a link someone other than owner made, read in tx after taking each such
// person's address lock, in the order of their addresses: an enrollment takes the same lock before
// it reads who owns the projects that name its person (Enroll), so a transfer of the project to
// owner and an enrollment through the old owner's link read one after the other.
func ProjectNamesAnotherOwnersPasskey(ctx context.Context, tx pgx.Tx, key, owner string) (bool, error) {
	rows, err := tx.Query(ctx, `select principal from project_access where project_key = $1 and principal_kind = 'person'
		order by principal collate "C"`, key)
	if err != nil {
		return false, fmt.Errorf("read the people project %s names: %w", key, err)
	}
	people, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, fmt.Errorf("read the people project %s names: %w", key, err)
	}
	for _, person := range people {
		if err := LockAddressTx(ctx, tx, person); err != nil {
			return false, err
		}
	}
	var held bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from passkeys
		where email = any($1) and canonical_login(made_by) <> canonical_login($2))`, people, owner).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether another owner's link enrolled a passkey of a person project %s names: %w", key, err)
	}
	return held, nil
}

// EnrolledPasskeys answers each passkey of the people emails names: when it was enrolled, the
// project whose link enrolled it and who made that link, sorted by email and then by that time;
// empty, never nil, for none.
func EnrolledPasskeys(ctx context.Context, q PasskeyReader, emails []string) ([]model.EnrolledPasskey, error) {
	enrolled := []model.EnrolledPasskey{}
	if len(emails) == 0 {
		return enrolled, nil
	}
	rows, err := q.Query(ctx, `select email, created_at, project_key, made_by from passkeys where email = any($1)
		order by email collate "C", created_at, credential_id`, emails)
	if err != nil {
		return nil, fmt.Errorf("read the passkeys' enrollment times: %w", err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.EnrolledPasskey, error) {
		var passkey model.EnrolledPasskey
		var project string
		if err := row.Scan(&passkey.Email, &passkey.EnrolledAt, &project, &passkey.MadeBy); err != nil {
			return passkey, err
		}
		passkey.Project = &project
		return passkey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the passkeys' enrollment times: %w", err)
	}
	return append(enrolled, read...), nil
}

// FullMembers answers which of emails a full member's people row holds, sorted.
func FullMembers(ctx context.Context, q PasskeyReader, emails []string) ([]string, error) {
	rows, err := q.Query(ctx, `select email from people where email = any($1) and member_kind = 'full' order by email collate "C"`, emails)
	if err != nil {
		return nil, fmt.Errorf("read which people are full members: %w", err)
	}
	full, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read which people are full members: %w", err)
	}
	return full, nil
}

// Invitation reads the link tokenHash names: ErrInvitationUnknown when none does.
func (p *PgPasskeys) Invitation(ctx context.Context, tokenHash [32]byte) (Invitation, error) {
	return readInvitation(ctx, p.people.pool, tokenHash, "")
}

// readInvitation reads the link tokenHash names through q, with lock appended to its select.
func readInvitation(ctx context.Context, q PasskeyReader, tokenHash [32]byte, lock string) (Invitation, error) {
	var invitation Invitation
	err := q.QueryRow(ctx, `select email, project_key, made_by, used_at is not null, expires_at <= now()
		from outside_invitations where token_sha256 = $1 `+lock, tokenHash[:]).
		Scan(&invitation.Email, &invitation.Project, &invitation.MadeBy, &invitation.Used, &invitation.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrInvitationUnknown
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("read the invitation: %w", err)
	}
	return invitation, nil
}

// Refusal is the refusal a link's state is, nil for a link that still enrolls: used, then lapsed.
func (i Invitation) Refusal() error {
	switch {
	case i.Used:
		return ErrInvitationUsed
	case i.Expired:
		return ErrInvitationExpired
	}
	return nil
}

// NamedDirectly reports whether project's access list names email as a person.
func (p *PgPasskeys) NamedDirectly(ctx context.Context, project, email string) (bool, error) {
	return namedDirectly(ctx, p.people.pool, project, email)
}

func namedDirectly(ctx context.Context, q PasskeyReader, project, email string) (bool, error) {
	var named bool
	if err := q.QueryRow(ctx, `select exists (select 1 from project_access
		where project_key = $1 and principal_kind = 'person' and principal = $2)`, project, model.CanonicalLogin(email)).Scan(&named); err != nil {
		return false, fmt.Errorf("read whether the project's list names the invited email: %w", err)
	}
	return named, nil
}

// Account reads what email's enrollment begins from (PasskeyAccount).
func (p *PgPasskeys) Account(ctx context.Context, email string) (PasskeyAccount, error) {
	login := model.CanonicalLogin(email)
	var account PasskeyAccount
	err := p.people.pool.QueryRow(ctx, `select case when sub_issuer = $2 then sub else '' end, member_kind = 'full'
		from people where email = $1`, login, p.issuer).Scan(&account.Subject, &account.FullMember)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PasskeyAccount{}, fmt.Errorf("read the person's account: %w", err)
	}
	rows, err := p.people.pool.Query(ctx, `select credential_id from passkeys where email = $1 order by created_at`, login)
	if err != nil {
		return PasskeyAccount{}, fmt.Errorf("read the person's passkeys: %w", err)
	}
	if account.Credentials, err = pgx.CollectRows(rows, pgx.RowTo[[]byte]); err != nil {
		return PasskeyAccount{}, fmt.Errorf("read the person's passkeys: %w", err)
	}
	return account, nil
}

// Enroll records a passkey enrolled with the link tokenHash names, in one transaction: it takes
// the link (unused, unlapsed, its project's list still naming its email directly, and for email,
// else the refusal that says which), records it used, signs the person in as a restricted member
// under subject, the passkey's user handle, through the account binding (PgPeopleStore's
// signInAccountTx, which ends what an account it replaces held), and stores the passkey. The link
// is locked first, which a removal from the project's list and the owner's Reset lock to delete
// it, so either commits first and the other reads what it wrote. A row that records another
// passkey subject meanwhile is ErrEnrollmentSuperseded; a full member's row is
// auth.ErrAddressHeldByMember. Each refusal writes nothing.
func (p *PgPasskeys) Enroll(ctx context.Context, tokenHash [32]byte, email, subject string, credential PasskeyCredential) error {
	login := model.CanonicalLogin(email)
	if err := p.people.refuseAMembersAddress(ctx, login, subject, model.MemberRestricted); err != nil {
		return err
	}
	if err := p.people.retirePlainBeforeSignIn(ctx, login); err != nil {
		return err
	}
	tx, err := p.people.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("enroll the passkey: %w", err)
	}
	defer tx.Rollback(ctx)
	invitation, err := readInvitation(ctx, tx, tokenHash, "for update")
	if err != nil {
		return err
	}
	if invitation.Email != login {
		return ErrInvitationUnknown
	}
	if err := invitation.Refusal(); err != nil {
		return err
	}
	named, err := namedDirectly(ctx, tx, invitation.Project, login)
	if err != nil {
		return err
	}
	if !named {
		return ErrInvitationNotListed
	}
	if _, err := tx.Exec(ctx, `update outside_invitations set used_at = now() where token_sha256 = $1`, tokenHash[:]); err != nil {
		return fmt.Errorf("use the invitation: %w", err)
	}
	if err := p.refuseAnotherPasskeySubject(ctx, tx, login, subject); err != nil {
		return err
	}
	// Read again under the address's lock: a project of another owner that named the person
	// between the link and its use is no project the link's maker may bind a passkey to.
	if other, err := ListedByAnotherOwner(ctx, tx, login, invitation.MadeBy); err != nil {
		return err
	} else if other {
		return ErrListedByAnotherOwner
	}
	recorded, err := p.people.signInAccountTx(ctx, tx, login, signInAccount{issuer: p.issuer, subject: subject}, nil, model.MemberRestricted)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into passkeys (credential_id, email, user_handle, public_key, sign_count, aaguid, transports, flags, project_key, made_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		credential.ID, login, credential.UserHandle, credential.PublicKey, int64(credential.SignCount), aaguidOf(credential.AAGUID), transportsOf(credential.Transports), int16(credential.Flags),
		invitation.Project, invitation.MadeBy); err != nil {
		return fmt.Errorf("store the passkey: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("enroll the passkey: %w", err)
	}
	recorded.log(login)
	slog.Info("dispatch: a restricted member enrolled a passkey with an owner's invitation", "email", login, "project", invitation.Project)
	return nil
}

// refuseAnotherPasskeySubject is ErrEnrollmentSuperseded when email's row records a passkey subject
// under the outside origin other than subject, read under the address's lock in tx: the subject an
// enrollment begins from is the row's, so another one is an enrollment that finished meanwhile.
func (p *PgPasskeys) refuseAnotherPasskeySubject(ctx context.Context, tx pgx.Tx, email, subject string) error {
	if _, err := tx.Exec(ctx, addressLockSQL, email); err != nil {
		return fmt.Errorf("enroll the passkey: lock the address: %w", err)
	}
	var stored string
	err := tx.QueryRow(ctx, `select coalesce(sub, '') from people where email = $1 and sub_issuer = $2`, email, p.issuer).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the person's passkey subject: %w", err)
	}
	if stored != subject {
		return ErrEnrollmentSuperseded
	}
	return nil
}

// Passkey reads the passkey credentialID names: ErrPasskeyUnknown when none does.
func (p *PgPasskeys) Passkey(ctx context.Context, credentialID []byte) (StoredPasskey, error) {
	return readPasskey(ctx, p.people.pool, credentialID, "")
}

func readPasskey(ctx context.Context, q PasskeyReader, credentialID []byte, lock string) (StoredPasskey, error) {
	var passkey StoredPasskey
	var signCount int64
	var flags int16
	err := q.QueryRow(ctx, `select email, credential_id, user_handle, public_key, sign_count, aaguid, transports, flags
		from passkeys where credential_id = $1 `+lock, credentialID).
		Scan(&passkey.Email, &passkey.ID, &passkey.UserHandle, &passkey.PublicKey, &signCount, &passkey.AAGUID, &passkey.Transports, &flags)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredPasskey{}, ErrPasskeyUnknown
	}
	if err != nil {
		return StoredPasskey{}, fmt.Errorf("read the passkey: %w", err)
	}
	passkey.SignCount, passkey.Flags = uint32(signCount), byte(flags)
	return passkey, nil
}

// SignIn records a sign-in with the passkey read, which WebAuthn verified with signature counter
// signCount and flags, in one transaction under the address's lock, which every write that ends a
// passkey's sign-in or deletes its passkeys takes first (EndPasskeySignInTx, settle), so none of
// them waits on another in a cycle: the passkey, locked, must still exist (else
// ErrPasskeyUnknown) with the counter read held (else ErrPasskeySignCount: another sign-in with it
// finished first), and the address must still record the passkey's user handle under the outside
// origin (else ErrPasskeyAccountMoved), so a passkey never replaces the account an address
// records. Then it stores the new counter and flags, and signs the person in as a restricted
// member through the account binding (signInAccountTx), which under the same account ends nothing.
func (p *PgPasskeys) SignIn(ctx context.Context, read StoredPasskey, signCount uint32, flags byte) error {
	tx, err := p.people.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sign in with the passkey: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, addressLockSQL, read.Email); err != nil {
		return fmt.Errorf("sign in with the passkey: lock the address: %w", err)
	}
	locked, err := readPasskey(ctx, tx, read.ID, "for update")
	if err != nil {
		return err
	}
	if locked.SignCount != read.SignCount || locked.Email != read.Email {
		return ErrPasskeySignCount
	}
	err = tx.QueryRow(ctx, `select 1 from people where email = $1 and sub = $2 and sub_issuer = $3 for update`,
		locked.Email, string(locked.UserHandle), p.issuer).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPasskeyAccountMoved
	}
	if err != nil {
		return fmt.Errorf("read the account the address records: %w", err)
	}
	if _, err := tx.Exec(ctx, `update passkeys set sign_count = $2, flags = $3, last_used_at = now() where credential_id = $1`,
		locked.ID, int64(signCount), int16(flags)); err != nil {
		return fmt.Errorf("record the passkey's use: %w", err)
	}
	if _, err := p.people.signInAccountTx(ctx, tx, locked.Email, signInAccount{issuer: p.issuer, subject: string(locked.UserHandle)}, nil, model.MemberRestricted); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sign in with the passkey: %w", err)
	}
	return nil
}

// Confirm is the hourly confirmation's write for a passkey's sign-in the access lists still admit:
// while email's row records subject under the outside origin with its sign-in standing and at least
// one passkey, it stamps the confirmation now and answers true. A row that records another account,
// or whose sign-in has ended, is left as it is (false). A row with no passkey left has its sign-in
// ended in the same transaction (false).
func (p *PgPasskeys) Confirm(ctx context.Context, email, subject string) (bool, error) {
	return p.settle(ctx, email, subject, false)
}

// End is the hourly confirmation's write for a passkey's sign-in the access lists no longer admit:
// while email's row records subject under the outside origin, it ends the sign-in and deletes the
// person's passkeys in one transaction (EndPasskeySignInTx), and answers whether it did.
func (p *PgPasskeys) End(ctx context.Context, email, subject string) (bool, error) {
	return p.settle(ctx, email, subject, true)
}

// settle is Confirm, or End when end is true, in one transaction under the address's lock.
func (p *PgPasskeys) settle(ctx context.Context, email, subject string, end bool) (bool, error) {
	login := model.CanonicalLogin(email)
	tx, err := p.people.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("confirm the passkey sign-in of %q: %w", login, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, addressLockSQL, login); err != nil {
		return false, fmt.Errorf("confirm the passkey sign-in of %q: lock the address: %w", login, err)
	}
	var standing bool
	var passkeys int
	err = tx.QueryRow(ctx, `select confirmed_at is not null, (select count(*) from passkeys where email = $1)
		from people where email = $1 and sub = $2 and sub_issuer = $3 for update`, login, subject, p.issuer).Scan(&standing, &passkeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the passkey sign-in of %q: %w", login, err)
	}
	if !end && !standing {
		// Signed out, or ended by another request's check: nothing to confirm or end.
		return false, nil
	}
	if !end && passkeys > 0 {
		if _, err := tx.Exec(ctx, `update people set confirmed_at = now() where email = $1`, login); err != nil {
			return false, fmt.Errorf("confirm the passkey sign-in of %q: %w", login, err)
		}
		return true, tx.Commit(ctx)
	}
	deleted, err := EndPasskeySignInTx(ctx, tx, login)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("end the passkey sign-in of %q: %w", login, err)
	}
	slog.Warn("dispatch: the hourly confirmation ended a passkey sign-in and deleted its passkeys", "email", login, "passkeys_deleted", deleted, "listed", !end)
	return false, nil
}

// aaguidOf is an authenticator's AAGUID as passkeys stores it: 16 bytes, or none for an all-zero
// or absent one.
func aaguidOf(aaguid []byte) []byte {
	for _, b := range aaguid {
		if b != 0 {
			return aaguid
		}
	}
	return []byte{}
}

// transportsOf is a passkey's transports as passkeys stores them, never null.
func transportsOf(transports []string) []string {
	if transports == nil {
		return []string{}
	}
	return transports
}
