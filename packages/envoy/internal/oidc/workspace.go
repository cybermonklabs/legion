package oidc

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The reasons WorkspaceLogin and VerifiedLogin refuse a Google sign-in, one per rule, so each
// caller can word its own refusal: Dispatch's sign-in page and the secrets broker's approval page.
var (
	ErrNoSubject        = errors.New("oidc: the Google ID token carries no subject")
	ErrNoEmail          = errors.New("oidc: the Google ID token carries no email address")
	ErrNotInDomain      = errors.New("oidc: the Google account is not in the Workspace domain")
	ErrEmailNotVerified = errors.New("oidc: the Google account's email is not verified")
)

// EmailShape is an address with one @ and a dotted domain, the shape the deployment repository's
// identity_email accepts: Google's email claim here, and Dispatch's federated username
// (identity.GroupRule).
var EmailShape = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// WorkspaceLogin applies the Google Workspace person rule to the claims a domain-mode code flow read
// from a verified Google ID token, and returns the person's login: the lowercased email, the
// canonical form logins are compared in. The account must have a subject, an email of an address's
// shape that Google has verified, and hd, the account's Workspace domain, equal to domain; the
// domain is read from hd alone, never from the email's suffix, which a personal Google account can
// share with a Workspace. An empty domain admits nobody, rather than every account outside any
// Workspace. A refusal wraps one of ErrNoSubject, ErrNoEmail, ErrNotInDomain or
// ErrEmailNotVerified, and still returns the login when the email has an address's shape, so the
// refusal can name the person.
func WorkspaceLogin(claims Claims, domain string) (string, error) {
	login, err := accountLogin(claims)
	if err != nil {
		return "", err
	}
	if domain == "" || claims.HostedDomain != domain {
		return login, fmt.Errorf("%s (hd %q): %w", login, claims.HostedDomain, ErrNotInDomain)
	}
	if !claims.EmailVerified {
		return login, fmt.Errorf("%s: %w", login, ErrEmailNotVerified)
	}
	return login, nil
}

// VerifiedLogin applies WorkspaceLogin's rules but the domain to a Google ID token's claims, and
// returns the person's login, the lowercased email: the account must have a subject and an email
// of an address's shape that Google has verified, whatever its hd. It is for an account outside
// any Workspace, which a caller admits on a rule of its own (Dispatch's outside sign-in, admitting
// an address a project's access list names). A refusal wraps ErrNoSubject, ErrNoEmail or
// ErrEmailNotVerified, and still returns the login when the email has an address's shape.
func VerifiedLogin(claims Claims) (string, error) {
	login, err := accountLogin(claims)
	if err != nil {
		return "", err
	}
	if !claims.EmailVerified {
		return login, fmt.Errorf("%s: %w", login, ErrEmailNotVerified)
	}
	return login, nil
}

// accountLogin is the lowercased email of claims, refusing an account with no subject
// (ErrNoSubject) or with no email of an address's shape (ErrNoEmail).
func accountLogin(claims Claims) (string, error) {
	if claims.Subject == "" {
		return "", ErrNoSubject
	}
	if !EmailShape.MatchString(claims.Email) {
		return "", ErrNoEmail
	}
	return strings.ToLower(claims.Email), nil
}
