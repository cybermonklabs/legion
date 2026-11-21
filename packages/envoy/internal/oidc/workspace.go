package oidc

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/sjawhar/envoy/internal/fold"
)

// The reasons WorkspaceLogin refuses a Google sign-in, one per rule, so each caller can word its own
// refusal: Dispatch's sign-in page and the secrets broker's approval page.
var (
	ErrNoSubject        = errors.New("oidc: the Google ID token carries no subject")
	ErrNoEmail          = errors.New("oidc: the Google ID token carries no email address")
	ErrNotInDomain      = errors.New("oidc: the Google account is not in the Workspace domain")
	ErrEmailNotVerified = errors.New("oidc: the Google account's email is not verified")
)

// EmailShape is an address with one @ and a dotted domain, the shape the deployment repository's
// identity_email accepts, holding no space: Google's email claim here, and Dispatch's federated
// username (identity.GroupRule). A space is any character unicode.IsSpace names, not only the ASCII
// ones Go's \s does: a trim (model.CanonicalLogin) takes U+00A0 NO-BREAK SPACE off an address's
// end, so an address holding one could be read as another person's.
var EmailShape = regexp.MustCompile(`^` + addressPart + `@` + addressPart + `\.` + addressPart + `$`)

// addressPart is a run of characters that are neither @ nor a space: \s is \t, \n, \f, \r and the
// space, and \v, U+0085 and the separators (\p{Z}) are the rest of unicode.IsSpace.
const addressPart = `[^@\s\v\x{85}\p{Z}]+`

// WorkspaceLogin applies the Google Workspace person rule to the claims a domain-mode code flow read
// from a verified Google ID token, and returns the person's login: the email with its ASCII letters
// lowered (fold.ASCII), the canonical form logins are compared in, so no Unicode letter is folded
// onto the ASCII one of another person's address. The account must have a subject, an email of an
// address's shape that Google has verified, and hd, the account's Workspace domain, equal to
// domain; the domain is read from hd alone, never from the email's suffix, which a personal Google
// account can share with a Workspace. An empty domain admits nobody, rather than every account
// outside any Workspace. A refusal wraps one of ErrNoSubject, ErrNoEmail, ErrNotInDomain or
// ErrEmailNotVerified, and still returns the login when the email has an address's shape, so the
// refusal can name the person.
func WorkspaceLogin(claims Claims, domain string) (string, error) {
	if claims.Subject == "" {
		return "", ErrNoSubject
	}
	if !EmailShape.MatchString(claims.Email) {
		return "", ErrNoEmail
	}
	login := fold.ASCII(claims.Email)
	if domain == "" || claims.HostedDomain != domain {
		return login, fmt.Errorf("%s (hd %q): %w", login, claims.HostedDomain, ErrNotInDomain)
	}
	if !claims.EmailVerified {
		return login, fmt.Errorf("%s: %w", login, ErrEmailNotVerified)
	}
	return login, nil
}
