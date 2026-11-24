package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestGithub: Github accepts only a signature its secret made. An empty secret checks nothing, so
// it answers false for every signature, the one made with the empty key too.
func TestGithub(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	sign := func(secret string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	for _, tc := range []struct {
		name, secret, signature string
		want                    bool
	}{
		{name: "no secret, a signature made with the empty key", signature: sign("")},
		{name: "control: a secret and its signature", secret: "secret", signature: sign("secret"), want: true},
		{name: "control: another secret's signature", secret: "wrong", signature: sign("secret")},
		{name: "control: a secret and a bad digest", secret: "secret", signature: "sha256=deadbeef"},
		{name: "control: a secret and no signature", secret: "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Github(tc.secret, body, tc.signature)
			t.Logf("Github(secret set %v, signature %q) = %v", tc.secret != "", tc.signature, got)
			if got != tc.want {
				t.Fatalf("Github(secret=%q, signature=%q) = %v, want %v", tc.secret, tc.signature, got, tc.want)
			}
		})
	}
}
