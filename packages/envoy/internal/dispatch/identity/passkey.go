package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PasskeyRule is the sign-in rule of a deployment whose people from outside sign in with a passkey
// (cmd/dispatch: DISPATCH_SIGNIN_PASSKEY=1): Google signs in Domain's people alone, as full members,
// as DomainRule does, and a restricted member signs in only with a passkey at the outside origin
// (Passkeys). It is the OutsideRule it embeds but for BootLog and WorkspaceAddress:
// AdmitsRestricted, which the passkey ceremonies, the hourly confirmation of a passkey's sign-in and
// every credential acting for a restricted member ask, reads the access lists and refuses an
// address on a domain the Workspace owns; Person is DomainRule's, so no Google account outside the
// Workspace signs in.
type PasskeyRule struct {
	OutsideRule
}

var _ SignInRule = PasskeyRule{}

// BootLog logs the issuer, client and Workspace domain Google signs people in for, the Workspace's
// other domains, and that restricted members sign in with a passkey.
func (r PasskeyRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in with Google for a Workspace domain, and the people outside it that a project names with a passkey", "issuer", issuer, "client_id", clientID, "domain", r.Domain, "workspace_domains", r.WorkspaceDomains, "passkey", true)
}

// WorkspaceAddress reports whether email is an address on a domain the Workspace owns (Domain, one
// of WorkspaceDomains, or a subdomain of either: the package's WorkspaceAddress), which
// AdmitsRestricted refuses (ErrWorkspaceAddress) without reading the lists: the owner's link is
// refused for it, and the Access tab offers none (api: the invitable list).
func (r PasskeyRule) WorkspaceAddress(email string) bool {
	return WorkspaceAddress(model.CanonicalLogin(email), r.Domain, r.WorkspaceDomains)
}

// The refusals of the passkey ceremonies beside the store's (store.ErrInvitationUnknown and the
// rest) and the rule's (ErrWorkspaceAddress, ErrNotListed, ErrMembershipUnreadable).
var (
	// ErrPasskeyRefused means the browser's answer to a ceremony did not verify: it does not
	// parse, names another origin or relying party, carries no user verification, or its
	// signature or challenge is wrong.
	ErrPasskeyRefused = errors.New("the passkey ceremony's answer did not verify")
	// ErrPasskeyCeremonyExpired means the ceremony's state is missing or lapsed: the person starts
	// the ceremony again.
	ErrPasskeyCeremonyExpired = errors.New("the passkey ceremony expired; start it again")
)

// passkeyCeremonyTimeout is how long a ceremony may take between its begin and its finish, which the
// browser is told and the server enforces (webauthn.TimeoutConfig.Enforce).
const passkeyCeremonyTimeout = 5 * time.Minute

// passkeySubjectBytes is how many random bytes a passkey's subject holds (R5's design paragraph:
// 16, base64url).
const passkeySubjectBytes = 16

// PasskeyCeremony is a ceremony in flight: what its finish needs from its begin, carried by the
// browser (routes/passkey.go signs it into a cookie) rather than held by the process that began it,
// so whichever process the finish reaches completes it. TokenHash, Email and Subject are an
// enrollment's (the link's hash, the invited address, and the user handle the authenticator
// stores), and empty for a sign-in.
type PasskeyCeremony struct {
	Enrollment bool                 `json:"enrollment"`
	Session    webauthn.SessionData `json:"session"`
	TokenHash  []byte               `json:"token_hash,omitempty"`
	Email      string               `json:"email,omitempty"`
	Subject    string               `json:"subject,omitempty"`
}

// Expired reports whether the ceremony lapsed at now.
func (c PasskeyCeremony) Expired(now time.Time) bool {
	return c.Session.Expires.IsZero() || !now.Before(c.Session.Expires)
}

// Passkeys runs the passkey ceremonies of a restricted member at the outside origin: the relying
// party is the outside origin's host and the origin it answers is the outside origin alone, every
// credential is discoverable, user verification is required, and no attestation is asked for. An
// enrollment takes an owner's link and admits the person as Rule's AdmitsRestricted does; a
// finished ceremony signs the person in through the account binding (store.PgPasskeys), under a
// subject of 16 random bytes, base64url, that their first enrollment makes, issued by the outside
// origin.
type Passkeys struct {
	relyingParty *webauthn.WebAuthn
	store        *store.PgPasskeys
	rule         PasskeyRule
}

// NewPasskeys is the passkey ceremonies of origin, the outside origin (scheme://host[:port], as
// weborigin.Parse writes it), over passkeys, admitting a person as rule does.
func NewPasskeys(origin string, passkeys *store.PgPasskeys, rule PasskeyRule) (*Passkeys, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("passkeys: the outside origin %q names no host", origin)
	}
	relyingParty, err := webauthn.New(&webauthn.Config{
		RPID:                  parsed.Hostname(),
		RPDisplayName:         "Dispatch",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyTimeout, TimeoutUVD: passkeyCeremonyTimeout},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyTimeout, TimeoutUVD: passkeyCeremonyTimeout},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("passkeys: %w", err)
	}
	return &Passkeys{relyingParty: relyingParty, store: passkeys, rule: rule}, nil
}

// Store is the store the ceremonies write, which the owner's link, Reset and access answer write
// and read too (api.Deps.Passkeys).
func (p *Passkeys) Store() *store.PgPasskeys {
	return p.store
}

// WorkspaceAddress is the rule's check of an address on a domain the Workspace owns
// (PasskeyRule.WorkspaceAddress), for which no link is made (api.Deps.WorkspaceAddress).
func (p *Passkeys) WorkspaceAddress(email string) bool {
	return p.rule.WorkspaceAddress(email)
}

// passkeyUser is a person as go-webauthn reads them: their user handle (their subject's bytes),
// their address, and their passkeys.
type passkeyUser struct {
	handle      []byte
	email       string
	credentials []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { return u.handle }
func (u passkeyUser) WebAuthnName() string                       { return u.email }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.email }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// InvitationHash is the SHA-256 outside_invitations keys a link by, of token, the link's URL
// fragment: the base64url, unpadded, of its 32 random bytes. Any other value names no link.
func InvitationHash(token string) ([32]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return [32]byte{}, false
	}
	return sha256.Sum256(raw), true
}

// NewInvitationToken is a new link's token: 32 random bytes, base64url, unpadded, and the hash
// outside_invitations keeps of it.
func NewInvitationToken() (string, [32]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [32]byte{}, fmt.Errorf("make the invitation's token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), sha256.Sum256(raw[:]), nil
}

// NewPasskeySubject is a new passkey subject: 16 random bytes, base64url, unpadded. A person's
// first enrollment makes one, and the owner's Reset makes another (store.PgPasskeys.ResetTx).
func NewPasskeySubject() (string, error) {
	var raw [passkeySubjectBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("make the passkey subject: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// invitedPerson reads the link token names and the person it is for, and refuses it as its finish
// would: no such link (store.ErrInvitationUnknown), used, lapsed, the project's list no longer
// naming its email directly, the rule no longer admitting the address (ErrWorkspaceAddress,
// ErrNotListed, or ErrMembershipUnreadable, which is no refusal), or a full member's address
// (auth.ErrAddressHeldByMember, as the finish's account binding refuses it: the person signed in
// with Google as a full member after the owner made the link).
func (p *Passkeys) invitedPerson(ctx context.Context, token string) ([32]byte, store.Invitation, store.PasskeyAccount, error) {
	hash, ok := InvitationHash(token)
	if !ok {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, store.ErrInvitationUnknown
	}
	invitation, err := p.store.Invitation(ctx, hash)
	if err != nil {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, err
	}
	if err := invitation.Refusal(); err != nil {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, err
	}
	named, err := p.store.NamedDirectly(ctx, invitation.Project, invitation.Email)
	if err != nil {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, err
	}
	if !named {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, store.ErrInvitationNotListed
	}
	if err := p.rule.AdmitsRestricted(ctx, invitation.Email); err != nil {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, err
	}
	account, err := p.store.Account(ctx, invitation.Email)
	if err != nil {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, err
	}
	if account.FullMember {
		return [32]byte{}, store.Invitation{}, store.PasskeyAccount{}, auth.ErrAddressHeldByMember
	}
	return hash, invitation, account, nil
}

// BeginEnrollment begins the enrollment of a passkey with the link token names, refused as
// invitedPerson refuses it: the options the browser passes to navigator.credentials.create, and the
// ceremony its finish completes. It uses no link; it records the ceremony as one a finish may take
// once (store.PgPasskeys.BeginCeremony). The user handle is the
// person's passkey subject, the one their row records under the outside origin or, at their first
// enrollment, a new one; the passkeys they hold already are excluded.
func (p *Passkeys) BeginEnrollment(ctx context.Context, token string) (*protocol.CredentialCreation, PasskeyCeremony, error) {
	hash, invitation, account, err := p.invitedPerson(ctx, token)
	if err != nil {
		return nil, PasskeyCeremony{}, err
	}
	subject := account.Subject
	if subject == "" {
		if subject, err = NewPasskeySubject(); err != nil {
			return nil, PasskeyCeremony{}, err
		}
	}
	excluded := make([]protocol.CredentialDescriptor, 0, len(account.Credentials))
	for _, id := range account.Credentials {
		excluded = append(excluded, protocol.CredentialDescriptor{Type: protocol.PublicKeyCredentialType, CredentialID: id})
	}
	user := passkeyUser{handle: []byte(subject), email: invitation.Email}
	creation, session, err := p.relyingParty.BeginRegistration(user, webauthn.WithExclusions(excluded))
	if err != nil {
		return nil, PasskeyCeremony{}, fmt.Errorf("begin the passkey enrollment: %w", err)
	}
	if err := p.store.BeginCeremony(ctx, session.Challenge, passkeyCeremonyTimeout); err != nil {
		return nil, PasskeyCeremony{}, err
	}
	return creation, PasskeyCeremony{Enrollment: true, Session: *session, TokenHash: hash[:], Email: invitation.Email, Subject: subject}, nil
}

// FinishEnrollment completes ceremony, an enrollment BeginEnrollment began, with body, the browser's
// answer (the PublicKeyCredential navigator.credentials.create made, as JSON): it verifies it
// (ErrPasskeyRefused when it does not), asks the rule again (ErrWorkspaceAddress, ErrNotListed), and
// enrolls the passkey, using the link and signing the person in as a restricted member in one
// transaction (store.PgPasskeys.Enroll, whose refusals it returns). It answers the person's address
// and subject, which the session cookie and its confirmation name.
func (p *Passkeys) FinishEnrollment(ctx context.Context, ceremony PasskeyCeremony, body io.Reader) (string, string, error) {
	if !ceremony.Enrollment || len(ceremony.TokenHash) != sha256.Size {
		return "", "", ErrPasskeyCeremonyExpired
	}
	if err := p.takeCeremony(ctx, ceremony); err != nil {
		return "", "", err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(body)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrPasskeyRefused, err)
	}
	user := passkeyUser{handle: []byte(ceremony.Subject), email: ceremony.Email}
	credential, err := p.relyingParty.CreateCredential(user, ceremony.Session, parsed)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrPasskeyRefused, err)
	}
	if err := p.rule.AdmitsRestricted(ctx, ceremony.Email); err != nil {
		return "", "", err
	}
	var hash [32]byte
	copy(hash[:], ceremony.TokenHash)
	if err := p.store.Enroll(ctx, hash, ceremony.Email, ceremony.Subject, storedCredential(*credential, user.handle)); err != nil {
		return "", "", err
	}
	return ceremony.Email, ceremony.Subject, nil
}

// BeginSignIn begins a sign-in with a discoverable passkey: the options the browser passes to
// navigator.credentials.get, naming no credential, with user verification required, and the
// ceremony its finish completes. It records the ceremony as one a finish may take once
// (store.PgPasskeys.BeginCeremony) and reads nothing else.
func (p *Passkeys) BeginSignIn(ctx context.Context) (*protocol.CredentialAssertion, PasskeyCeremony, error) {
	assertion, session, err := p.relyingParty.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, PasskeyCeremony{}, fmt.Errorf("begin the passkey sign-in: %w", err)
	}
	if err := p.store.BeginCeremony(ctx, session.Challenge, passkeyCeremonyTimeout); err != nil {
		return nil, PasskeyCeremony{}, err
	}
	return assertion, PasskeyCeremony{Session: *session}, nil
}

// takeCeremony takes ceremony's row, the moment its finish starts and before anything is verified
// (store.PgPasskeys.TakeCeremony): ErrPasskeyCeremonyExpired when no begin recorded it, an earlier
// finish took it, or it lapsed, so a ceremony's answer is used at most once.
func (p *Passkeys) takeCeremony(ctx context.Context, ceremony PasskeyCeremony) error {
	live, err := p.store.TakeCeremony(ctx, ceremony.Session.Challenge)
	if err != nil {
		return err
	}
	if !live {
		return ErrPasskeyCeremonyExpired
	}
	return nil
}

// FinishSignIn completes ceremony, a sign-in BeginSignIn began, with body, the browser's answer (the
// PublicKeyCredential navigator.credentials.get made, as JSON). The passkey is read by the
// credential id the answer names (store.ErrPasskeyUnknown for none: reset or deleted) and the
// answer verified against it, its user handle the passkey's own (ErrPasskeyRefused). A signature
// counter that did not advance while either it or the stored one is non-zero is
// store.ErrPasskeySignCount (WebAuthn Level 3 §7.2 step 22): a cloned authenticator, or the same
// answer again. Then the rule is asked again: a person it no longer admits has their sign-in ended
// and their passkeys deleted, as the hourly confirmation does (store.PgPasskeys.End), and the
// refusal is returned (ErrNotListed, ErrWorkspaceAddress); lists that cannot be read are
// ErrMembershipUnreadable and change nothing. Last, the sign-in is recorded through the account
// binding (store.PgPasskeys.SignIn, whose refusals it returns). It answers the person's address and
// subject.
func (p *Passkeys) FinishSignIn(ctx context.Context, ceremony PasskeyCeremony, body io.Reader) (string, string, error) {
	if ceremony.Enrollment {
		return "", "", ErrPasskeyCeremonyExpired
	}
	if err := p.takeCeremony(ctx, ceremony); err != nil {
		return "", "", err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(body)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrPasskeyRefused, err)
	}
	var stored store.StoredPasskey
	var lookup error
	credential, err := p.relyingParty.ValidateDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		stored, lookup = p.store.Passkey(ctx, rawID)
		if lookup != nil {
			return nil, lookup
		}
		if !bytes.Equal(stored.UserHandle, userHandle) {
			lookup = fmt.Errorf("%w: the answer's user handle is not the passkey's", ErrPasskeyRefused)
			return nil, lookup
		}
		return passkeyUser{handle: stored.UserHandle, email: stored.Email, credentials: []webauthn.Credential{webauthnCredential(stored.PasskeyCredential)}}, nil
	}, ceremony.Session, parsed)
	if lookup != nil {
		return "", "", lookup
	}
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrPasskeyRefused, err)
	}
	if credential.Authenticator.CloneWarning {
		slog.Warn("dispatch: a passkey sign-in was refused: its signature counter did not advance", "email", stored.Email,
			"stored_count", stored.SignCount, "answered_count", parsed.Response.AuthenticatorData.Counter)
		return "", "", store.ErrPasskeySignCount
	}
	subject := string(stored.UserHandle)
	if err := p.rule.AdmitsRestricted(ctx, stored.Email); err != nil {
		if errors.Is(err, ErrMembershipUnreadable) {
			return "", "", err
		}
		if _, endErr := p.store.End(ctx, stored.Email, subject); endErr != nil {
			return "", "", endErr
		}
		return "", "", err
	}
	if err := p.store.SignIn(ctx, stored, credential.Authenticator.SignCount, credential.Flags.MsgpByte()); err != nil {
		return "", "", err
	}
	return model.CanonicalLogin(stored.Email), subject, nil
}

// storedCredential is a verified credential as the store keeps it, under handle.
func storedCredential(credential webauthn.Credential, handle []byte) store.PasskeyCredential {
	transports := make([]string, 0, len(credential.Transport))
	for _, transport := range credential.Transport {
		transports = append(transports, string(transport))
	}
	return store.PasskeyCredential{
		ID: credential.ID, UserHandle: handle, PublicKey: credential.PublicKey,
		SignCount: credential.Authenticator.SignCount, AAGUID: credential.Authenticator.AAGUID,
		Transports: transports, Flags: credential.Flags.MsgpByte(),
	}
}

// webauthnCredential is a stored passkey as go-webauthn verifies a sign-in against it.
func webauthnCredential(stored store.PasskeyCredential) webauthn.Credential {
	transports := make([]protocol.AuthenticatorTransport, 0, len(stored.Transports))
	for _, transport := range stored.Transports {
		transports = append(transports, protocol.AuthenticatorTransport(transport))
	}
	return webauthn.Credential{
		ID: stored.ID, PublicKey: stored.PublicKey, AttestationType: "none", AttestationFormat: "none",
		Transport: transports, Flags: webauthn.CredentialFlagsFromMsgpByte(stored.Flags),
		Authenticator: webauthn.Authenticator{AAGUID: stored.AAGUID, SignCount: stored.SignCount},
	}
}
