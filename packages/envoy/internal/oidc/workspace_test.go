package oidc

import (
	"errors"
	"fmt"
	"testing"
	"unicode"
	"unicode/utf8"
)

// workspaceAccount is a verified Google ID token's claims, as a domain-mode code flow reads them,
// for an account of the Workspace domain flowDomain.
func workspaceAccount() Claims {
	return Claims{Subject: "110169484474386276334", Email: "A.Person@example.com", EmailVerified: true, HostedDomain: flowDomain}
}

// The login is the lowercased email of an account whose hd is the domain and whose email Google
// has verified. The domain is read from hd alone: an account of the domain whose address is at
// another of the Workspace's domains is the domain's.
func TestWorkspaceLoginIsTheLowercasedVerifiedEmailOfTheDomain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		email string
		want  string
	}{
		{"an address at the domain", "A.Person@example.com", "a.person@example.com"},
		{"an address at another of the Workspace's domains", "A.Person@Alias.example", "a.person@alias.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := workspaceAccount()
			claims.Email = tc.email
			login, err := WorkspaceLogin(claims, flowDomain)
			if err != nil || login != tc.want {
				t.Fatalf("WorkspaceLogin = %q, %v; want %q", login, err, tc.want)
			}
		})
	}
}

// Each refusal wraps its own reason, and still names the login when the email has an address's
// shape, so the refusal can name the person.
func TestWorkspaceLoginRefusesEachWayWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Claims)
		domain string
		want   error
		login  string
	}{
		{"an hd of another domain", func(c *Claims) { c.HostedDomain = "other.example" }, flowDomain, ErrNotInDomain, "a.person@example.com"},
		{"an hd that only matches when lowercased", func(c *Claims) { c.HostedDomain = "Example.com" }, flowDomain, ErrNotInDomain, "a.person@example.com"},
		{"no hd, with an email at the domain: a personal account", func(c *Claims) { c.HostedDomain = "" }, flowDomain, ErrNotInDomain, "a.person@example.com"},
		{"no domain configured, for an account with no hd", func(c *Claims) { c.HostedDomain = "" }, "", ErrNotInDomain, "a.person@example.com"},
		{"an email Google has not verified", func(c *Claims) { c.EmailVerified = false }, flowDomain, ErrEmailNotVerified, "a.person@example.com"},
		{"no email", func(c *Claims) { c.Email = "" }, flowDomain, ErrNoEmail, ""},
		{"an email of no address's shape", func(c *Claims) { c.Email = "not-an-address" }, flowDomain, ErrNoEmail, ""},
		{"an email with two @", func(c *Claims) { c.Email = "a@b@example.com" }, flowDomain, ErrNoEmail, ""},
		{"an email ending in a no-break space, which a trim would take off", func(c *Claims) { c.Email = "A.Person@example.com\u00a0" }, flowDomain, ErrNoEmail, ""},
		{"an email starting with an em space", func(c *Claims) { c.Email = "\u2003A.Person@example.com" }, flowDomain, ErrNoEmail, ""},
		{"no subject", func(c *Claims) { c.Subject = "" }, flowDomain, ErrNoSubject, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := workspaceAccount()
			tc.change(&claims)
			login, err := WorkspaceLogin(claims, tc.domain)
			if !errors.Is(err, tc.want) || login != tc.login {
				t.Errorf("WorkspaceLogin = %q, %v; want %q, %v", login, err, tc.login, tc.want)
			}
		})
	}
}

// VerifiedLogin applies every rule of WorkspaceLogin but the domain: it ignores hd, and refuses
// each other way with that rule's own reason, naming the login when the email has an address's
// shape.
func TestVerifiedLoginRefusesEachWayWithItsReason(t *testing.T) {
	for _, hd := range []string{"", flowDomain, "other.example"} {
		claims := workspaceAccount()
		claims.HostedDomain = hd
		if login, err := VerifiedLogin(claims); err != nil || login != "a.person@example.com" {
			t.Errorf("VerifiedLogin with hd %q = %q, %v; want a.person@example.com, whatever the hd", hd, login, err)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*Claims)
		want   error
		login  string
	}{
		{"no subject", func(c *Claims) { c.Subject = "" }, ErrNoSubject, ""},
		{"no email", func(c *Claims) { c.Email = "" }, ErrNoEmail, ""},
		{"an email of no address's shape", func(c *Claims) { c.Email = "not-an-address" }, ErrNoEmail, ""},
		{"an email ending in a no-break space, which a trim would take off", func(c *Claims) { c.Email = "A.Person@example.com\u00a0" }, ErrNoEmail, ""},
		{"an email starting with an em space", func(c *Claims) { c.Email = "\u2003A.Person@example.com" }, ErrNoEmail, ""},
		{"an email Google has not verified", func(c *Claims) { c.EmailVerified = false }, ErrEmailNotVerified, "a.person@example.com"},
		{"an unverified email and no hd", func(c *Claims) { c.EmailVerified, c.HostedDomain = false, "" }, ErrEmailNotVerified, "a.person@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := workspaceAccount()
			tc.change(&claims)
			login, err := VerifiedLogin(claims)
			if !errors.Is(err, tc.want) || login != tc.login {
				t.Errorf("VerifiedLogin = %q, %v; want %q, %v", login, err, tc.login, tc.want)
			}
		})
	}
}

// EmailShape refuses an address holding a Unicode space (unicode.IsSpace) in any of its parts, not
// only the ASCII ones Go's \s names: a trim (model.CanonicalLogin) takes U+00A0 NO-BREAK SPACE or
// U+2003 EM SPACE off an address's end, so an address holding one could be read as another's.
// Every other character but @ is taken in each part, as before.
func TestEmailShapeRefusesEveryUnicodeSpaceAndNoOtherCharacter(t *testing.T) {
	var wrong []string
	spaces, others := 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r == '@' || !utf8.ValidRune(r) {
			continue
		}
		c := string(r)
		if !unicode.IsSpace(r) {
			others++
			if address := "a" + c + "@ex" + c + "ample.c" + c + "m"; !EmailShape.MatchString(address) {
				wrong = append(wrong, fmt.Sprintf("refuses %U in %q", r, address))
			}
			continue
		}
		spaces++
		for _, address := range []string{"a" + c + "@example.com", "a@ex" + c + "ample.com", "a@example.c" + c + "m", "a@example.com" + c} {
			if EmailShape.MatchString(address) {
				wrong = append(wrong, fmt.Sprintf("takes the space %U in %q", r, address))
			}
		}
	}
	if spaces == 0 || others == 0 {
		t.Fatalf("tried %d spaces and %d other characters: the walk of the runes read nothing", spaces, others)
	}
	if len(wrong) > 0 {
		t.Fatalf("EmailShape is wrong for %d addresses, first %q", len(wrong), wrong[:min(len(wrong), 8)])
	}
	t.Logf("refused each of %d spaces in every part, and took %d other characters", spaces, others)
}
