package identity

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/oidc"
)

// GroupRule signs in the members of Group, a Cognito sign-in pool's group (`cognito:groups`).
type GroupRule struct {
	Group string
}

var _ SignInRule = GroupRule{}

// Person names the person by the email the username a federated provider issued carries,
// `<provider>_<email>` for a provider the token's identities name, and never from the email
// claim, which a person can write. A member of the group is a full member; a person outside it is
// ErrNotMember.
func (r GroupRule) Person(_ context.Context, claims oidc.Claims) (SignedIn, error) {
	email, ok := providerEmail(claims)
	if !ok {
		return SignedIn{}, fmt.Errorf("%w: username %q", ErrNoPerson, claims.Username)
	}
	if !slices.Contains(claims.Groups, r.Group) {
		return SignedIn{Email: email}, fmt.Errorf("%s: %w", email, ErrNotMember)
	}
	return SignedIn{Email: email, Kind: model.MemberFull}, nil
}

// RefusalLogFields names the pool username a refusal's log line carries.
func (r GroupRule) RefusalLogFields(claims oidc.Claims) []any {
	return []any{"username", claims.Username}
}

// Subject is always empty under a group: Dispatch records no account subject for a pool sign-in.
func (r GroupRule) Subject(claims oidc.Claims) string {
	return ""
}

// BootLog logs the pool issuer, client and group boot is signing people in under.
func (r GroupRule) BootLog(issuer, clientID string) {
	slog.Info("dispatch: signing people in through the sign-in pool", "issuer", issuer, "client_id", clientID, "group", r.Group)
}

// DomainHint is false: a sign-in pool's group has no Workspace domain to name.
func (r GroupRule) DomainHint() bool {
	return false
}

// CanRefresh is always true: a group's row always has something to refresh.
func (r GroupRule) CanRefresh(stored string) bool {
	return true
}

// ConfirmSubject always records no subject under a group.
func (r GroupRule) ConfirmSubject(claims oidc.Claims, stored string) (string, error) {
	return "", nil
}

// End ends email's sessions and forgets their refresh token (auth.SessionStore.RevokeSessions,
// auth.PeopleStore.End).
func (r GroupRule) End(ctx context.Context, people auth.PeopleStore, sessions auth.SessionStore, email, subject, reason string) error {
	slog.Warn("dispatch: sign-in ended: the sign-in pool did not confirm the person's membership", "email", email, "reason", reason)
	if err := sessions.RevokeSessions(ctx, email); err != nil {
		return err
	}
	if err := people.End(ctx, email); err != nil {
		return err
	}
	return ErrNoIdentity
}

// ConfirmCookie never refuses under a group: Dispatch records no subject for a pool sign-in, so
// there is nothing another sign-in could have taken the address under.
func (r GroupRule) ConfirmCookie(ctx context.Context, people auth.PeopleStore, email, subject string) error {
	return nil
}

// WriteSignIn records a sign-in through the Cognito sign-in pool (auth.PeopleStore.SignIn), which
// records a full member; subject, always empty under a group, is unused.
func (r GroupRule) WriteSignIn(ctx context.Context, people auth.PeopleStore, person SignedIn, subject, refreshToken string) error {
	return people.SignIn(ctx, person.Email, refreshToken)
}

// WriteConfirm records a refresh that confirmed person's membership (auth.PeopleStore.Confirm),
// which records a full member; subject, always empty under a group, is unused.
func (r GroupRule) WriteConfirm(ctx context.Context, people auth.PeopleStore, person SignedIn, subject, refreshToken string) error {
	return people.Confirm(ctx, person.Email, refreshToken)
}

// providerEmail is the lowercased email a federated provider's username carries, the Cognito
// sign-in pool's `<provider>_<email>` shape.
func providerEmail(claims oidc.Claims) (string, bool) {
	for _, provider := range claims.Providers {
		if provider == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(claims.Username, provider+"_"); ok && oidc.EmailShape.MatchString(rest) {
			return strings.ToLower(rest), true
		}
	}
	return "", false
}
