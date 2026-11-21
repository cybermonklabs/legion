// Package principal holds the two forms in which Dispatch, the secrets broker and the infra
// component name a caller: a person's login and a Kubernetes service account's subject. Each of
// them checks its settings and its input against these rules, so the three agree on what they
// accept. It imports nothing but the standard library, so a tool that checks one of these forms
// compiles nothing of Dispatch's model with it.
package principal

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// CheckPersonLogin reports why login is not a person's email as Dispatch stores it, or nil when it
// is one. Such a login is an @ with something on each side, holds no space or comma, is at most
// 320 bytes long, and holds no control or bidi character, which would show one address and store
// another. It is also trimmed and lowercase throughout. A stored login lowers A-Z alone
// (model.CanonicalLogin), but the people table holds every email lowercase (email = lower(email)),
// so a capital beyond A-Z names nobody who could sign in and is refused here rather than by a
// column's check. A caller that takes a person as typed canonicalises it first
// (access.PersonPrincipal); boot and the infra component hold their setting to this form as given.
func CheckPersonLogin(login string) error {
	at := strings.IndexByte(login, '@')
	if at <= 0 || at == len(login)-1 || strings.ContainsAny(login, " \t\r\n,") || len(login) > 320 {
		return fmt.Errorf("%q is no email", strings.TrimSpace(login))
	}
	for _, r := range login {
		switch {
		case unicode.IsControl(r):
			return fmt.Errorf("%q holds a control character", login)
		case unicode.Is(unicode.Bidi_Control, r):
			return fmt.Errorf("%q holds a bidi control", login)
		}
	}
	if strings.TrimSpace(login) != login {
		return fmt.Errorf("%q has space around it", login)
	}
	if strings.ToLower(login) != login {
		return fmt.Errorf("%q is not lowercase, so it names nobody who could sign in", login)
	}
	return nil
}

// ServiceAccountSubject is a Kubernetes service account's subject as its projected token carries
// it, system:serviceaccount:<namespace>:<name>, the namespace a DNS label and the name a DNS
// subdomain, so it holds no comma. Dispatch's boot holds DISPATCH_OIDC_ACCOUNTS and
// DISPATCH_ACCESS_ACCOUNTS to it, the infra component holds oidcAccounts and accessAccounts to it
// when a stack plans, and the secrets broker holds its own service-account settings and exchange
// entries to it (internal/cipher/config, internal/cipher/exchange).
var ServiceAccountSubject = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)
