package ciphertoken_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// formSent is redirectstest.Sent's stand-in keeping each request's body beside its method, its
// request URI and its headers: a client that follows a 307 or a 308 sends the introspection's form
// on again, the presented token in it, which Sent does not keep.
func formSent(t *testing.T) (host string, sent func() string) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked strings.Builder
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("the stand-in could not read a body it was sent: %v", err)
		}
		mu.Lock()
		fmt.Fprintf(&asked, "%s %s %v %s\n", r.Method, r.RequestURI, r.Header, body)
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		text := asked.String()
		asked.Reset()
		return text
	}
}

// TestAnIntrospectionFollowsNoRedirect: whatever answers at the broker's address answers an
// introspection, which carries the caller's own account token as its bearer and the token it asks
// about in its form, with a 302, a 307 or a 308 whose Location names a stand-in on another host and
// quotes the account token in its query. A client that follows the 302 asks that Location, and one
// that follows the 307 or the 308 sends the form there again. Under the default client (a nil
// Options.HTTP) and a client the caller gives, the stand-in is sent nothing, so no request carries
// either token on to it, and the verification is ErrBrokerUnreachable on the broker's 3xx, a
// status it does not answer a sound request with. The twins show a client that follows each of
// them sends the stand-in the tokens, so the rows can see one sent on. Captures
// brokertoken.introspect-followed/<client> (the 302) and
// brokertoken.introspect-followed/<client>-<status>, what the stand-in was sent.
func TestAnIntrospectionFollowsNoRedirect(t *testing.T) {
	const (
		account   = "cj8mt-not-a-secret-RDX-a41e"
		presented = "ty9qp-not-a-secret-RDX-e05b"
	)
	file := filepath.Join(t.TempDir(), "account")
	if err := os.WriteFile(file, []byte(account), 0o600); err != nil {
		t.Fatal(err)
	}
	target, sent := redirectstest.Sent(t)
	location := func(bearer string) string { return redirectstest.To(target, bearer) }
	formTarget, formSentText := formSent(t)
	// moved is the stand-in at the broker's address for the 307 and 308 rows:
	// redirectstest.EchoBearer's, answering status where that one answers 302, to formSent's
	// stand-in.
	moved := func(t *testing.T, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", redirectstest.To(formTarget, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.EchoBearer(t, location)+ciphertoken.IntrospectPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+account)
		redirectstest.RequireSentOn(t, request, account, sent)
	})
	clients := []struct {
		name   string
		client *http.Client
	}{
		{"default", nil},
		{"given", &http.Client{}},
	}
	for _, c := range clients {
		t.Run(c.name, func(t *testing.T) {
			broker := redirectstest.EchoBearer(t, location)
			rig := newRig(t, func(o *ciphertoken.Options) {
				o.BrokerURL = broker
				o.Account = ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", file)
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
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		code := strconv.Itoa(status)
		t.Run("twin-"+code, func(t *testing.T) {
			broker := moved(t, status)
			for _, credential := range []string{account, presented} {
				request, err := http.NewRequest(http.MethodPost, broker+ciphertoken.IntrospectPath, strings.NewReader(url.Values{"token": {presented}}.Encode()))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				request.Header.Set("Authorization", "Bearer "+account)
				redirectstest.RequireSentOn(t, request, credential, formSentText)
			}
		})
		for _, c := range clients {
			t.Run(c.name+"-"+code, func(t *testing.T) {
				broker := moved(t, status)
				rig := newRig(t, func(o *ciphertoken.Options) {
					o.BrokerURL = broker
					o.Account = ciphertoken.NewAccountToken("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", file)
					o.HTTP = c.client
				})
				_, err := rig.verifier.Verify(t.Context(), newSigner(t).at(resourcePath, presented))
				text := formSentText()
				redacttest.RequireNoPiece(t, "brokertoken.introspect-followed/"+c.name+"-"+code, text, account, presented)
				if text != "" {
					t.Errorf("the stand-in the Location names was asked: %q", text)
				}
				if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) || !strings.Contains(err.Error(), "the broker answered "+code) {
					t.Errorf("a redirected introspection answered %v, want ErrBrokerUnreachable on the broker's %d", err, status)
				}
			})
		}
	}
}
