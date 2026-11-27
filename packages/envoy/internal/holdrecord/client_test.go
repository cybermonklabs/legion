package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/redact"
)

// New refuses a configuration it cannot work with: no broker, no way to get the operator's token,
// and no key to verify an answer with, the last of which would read every row as unknown.
func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := func(context.Context) (string, error) { return "t", nil }
	whole := holdrecord.Config{BrokerURL: "https://broker.example", Token: token, VerifyKeys: []ed25519.PublicKey{pub}}
	if _, err := holdrecord.New(whole); err != nil {
		t.Fatalf("New of a whole read-only configuration: %v", err)
	}
	for name, cfg := range map[string]holdrecord.Config{
		"an empty BrokerURL": {Token: token, VerifyKeys: whole.VerifyKeys},
		"a nil Token":        {BrokerURL: whole.BrokerURL, VerifyKeys: whole.VerifyKeys},
		"no VerifyKeys":      {BrokerURL: whole.BrokerURL, Token: token},
	} {
		if c, err := holdrecord.New(cfg); err == nil {
			t.Errorf("New with %s answered %v and no error", name, c)
		}
	}
}

// New's refusal of a broker URL that is not an absolute URL prints it through redact.URL, so a
// password in the URL's user info, or the operator's token pasted where the URL goes, reaches no
// error, log or terminal; the refusal says the value was not printed. Fails while New prints the
// value whole with %q, as at a0a2c7622f, where each canary is in the error.
func TestNewPrintsNoSecretInARefusedBrokerURL(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := func(context.Context) (string, error) { return "t", nil }
	for name, c := range map[string]struct{ url, canary string }{
		"a password in the URL's user info":      {userinfoURL("https", "operator", "PASSWORD_CANARY%zz", "broker.example"), "PASSWORD_CANARY"},
		"a token pasted in place of the URL":     {"eyJhbGciTOKEN_CANARY.payload.sig", "TOKEN_CANARY"},
		"a password in a URL that names no host": {userinfoURL("https", "operator", "PASSWORD_CANARY", ""), "PASSWORD_CANARY"},
	} {
		_, err := holdrecord.New(holdrecord.Config{BrokerURL: c.url, Token: token, VerifyKeys: []ed25519.PublicKey{pub}})
		if err == nil {
			t.Errorf("New with %s answered no error", name)
			continue
		}
		if text := err.Error(); strings.Contains(text, c.canary) || !strings.Contains(text, redact.NotPrinted) {
			t.Errorf("New with %s answered %q: want the value not printed (%q) and no %s", name, text, redact.NotPrinted, c.canary)
		}
	}
}

// userinfoURL is scheme://user:password@rest, built here so no test line spells a URL that
// carries a credential in the form a secret scanner reads as one.
func userinfoURL(scheme, user, password, rest string) string {
	return scheme + "://" + user + ":" + password + "@" + rest
}
