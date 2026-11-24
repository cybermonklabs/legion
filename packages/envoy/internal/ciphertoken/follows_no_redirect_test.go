package ciphertoken_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAnIntrospectionFollowsNoRedirect: whatever answers at the broker's address answers an
// introspection, which carries the caller's own account token, with a 302 whose Location names a
// stand-in on another host and quotes the token in its query. Under the default client (a nil
// Options.HTTP) and a client the caller gives, the stand-in is sent nothing, so no request carries
// the token on to it, and the verification is ErrBrokerUnreachable on the broker's 302, a status
// it does not answer a sound request with. The twin shows a client that follows the redirect
// sends the stand-in the token, so the row can see one sent on. Captures
// brokertoken.introspect-followed/<client>, what the stand-in was sent.
func TestAnIntrospectionFollowsNoRedirect(t *testing.T) {
	const account = "cj8mt-not-a-secret-RDX-a41e"
	file := filepath.Join(t.TempDir(), "account")
	if err := os.WriteFile(file, []byte(account), 0o600); err != nil {
		t.Fatal(err)
	}
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.EchoBearer(t, location)+ciphertoken.IntrospectPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+account)
		redirectstest.RequireSentOn(t, request, account, sent)
	})
	for _, c := range []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker := redirectstest.EchoBearer(t, location)
			rig := newRig(t, func(o *ciphertoken.Options) {
				o.BrokerURL = broker
				o.Account = ciphertoken.NewAccountToken(file)
				o.HTTP = c.client
			})
			_, err := rig.verifier.Verify(t.Context(), newSigner(t).at(resourcePath, "a-token-asked-about-at-a-redirect"))
			text := sent()
			redacttest.RequireNoPiece(t, "brokertoken.introspect-followed/"+c.name, text, account)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) || !strings.Contains(err.Error(), "the broker answered 302") {
				t.Errorf("a redirected introspection answered %v, want ErrBrokerUnreachable on the broker's 302", err)
			}
		})
	}
}
