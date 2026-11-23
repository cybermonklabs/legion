package oauth_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// A token whose HMAC is not the broker's is answered not live without a read of the store: the
// auth service introspects every session token a NATS connection presents, so a client sending
// forged tokens must cost the broker no database query. With the store closed, each forged token
// is still answered not live with no error, and a real token — whose MAC is the broker's, so its
// row is read — now answers the store's failure, which shows the store really is gone for every
// token that would reach it.
//
// The control is the check removed from Introspect: the wrong-MAC token then reaches the closed
// store and answers its error, and this test fails (run by hand for the record of this change).
func TestAnUnsignedTokenIsAnsweredWithNoStoreRead(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	token := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	random, _, found := strings.Cut(token, ".")
	if !found {
		t.Fatalf("the broker's token %q has no '.', so the forged tokens below do not have its shape", "<redacted>")
	}
	// The real token's key half with another signature: the one forgery that looks like a token
	// the broker issued, and the one only the MAC check refuses.
	wrongMAC := random + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	f.pool.Close()
	for name, forged := range map[string]string{
		"the broker's key half with another signature": wrongMAC,
		"no signature at all":                          random,
		"two random halves":                            "forged-key-half.forged-signature-half",
		"empty":                                        "",
	} {
		answer, err := f.provider.Introspect(ctx, forged)
		if err != nil {
			t.Fatalf("%s: Introspect answered %v with the store closed: the forged token reached the store", name, err)
		}
		if answer.Active {
			t.Fatalf("%s: a forged token was answered live", name)
		}
	}
	if _, err := f.provider.Introspect(ctx, token); err == nil {
		t.Fatal("the real token was answered with no error on a closed store: the store was never read, so the forged rows prove nothing")
	}
}
