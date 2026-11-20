package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/sjawhar/envoy/internal/holdrecord"
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
