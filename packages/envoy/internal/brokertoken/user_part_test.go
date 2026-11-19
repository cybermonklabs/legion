package brokertoken

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"authelia.com/provider/oauth2/handler/rfc9449"
)

// noReplay stands in for the replay store New requires; this test calls none of its methods.
type noReplay struct{ rfc9449.DPoPReplayStorage }

// The broker URL (DISPATCH_AGENT_SECRETS_URL) and the origin may carry a user and a password. An
// introspection built from a broker URL that does not parse fails naming no part of it, though the
// parse error quotes the URL whole, and New's refusal of an origin names it through redact.URL.
// The control is the same values with no user part, refused in the same way. The password is made
// for this test.
func TestARefusalNamesNoUserPartOfTheBrokerOrTheOrigin(t *testing.T) {
	const (
		user     = "made-up-user"
		password = "not-a-secret-4f1e8a"
	)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("made-up-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	holds := func(err error) bool {
		return strings.Contains(err.Error(), password) || strings.Contains(err.Error(), user)
	}
	for _, userPart := range []string{user + ":" + password + "@", ""} {
		broken := &Verifier{brokerURL: "http://" + userPart + "[broker", account: NewAccountToken(token)}
		if _, err := broken.introspect(context.Background(), "made-up-token"); err == nil || !errors.Is(err, ErrBrokerUnreachable) {
			t.Errorf("user part %q: introspect err = %v, want ErrBrokerUnreachable", userPart, err)
		} else if holds(err) {
			t.Errorf("the introspection's refusal holds the user part: %q", err)
		}
		_, err := New(Options{
			BrokerURL: "https://broker.test",
			Account:   NewAccountToken(token),
			Replay:    noReplay{},
			Address:   func(*http.Request) string { return "" },
			Origin:    "https://" + userPart + "dispatch.test/dispatch",
		})
		if err == nil || !strings.Contains(err.Error(), "origin") {
			t.Errorf("user part %q: New err = %v, want a refusal of the origin", userPart, err)
		} else if holds(err) {
			t.Errorf("New's refusal holds the user part: %q", err)
		}
	}
}

// The acceptance test's red test of the broker's transport error, kept as it ran. The Dispatch
// server's boot admits a broker URL (DISPATCH_AGENT_SECRETS_URL) with a user part, and the verifier
// introspects a presented DPoP token before it checks the proof, so any caller that sends
// `Authorization: DPoP <anything>` while the broker cannot be reached gets the introspection's
// transport error, which internal/dispatch/api/broker_token.go logs whole at WARN. Go's http.Client
// puts the request's URL in that error with only the password masked: the user is printed, and a
// token written as the whole user part is printed whole. The control inside the test: the same URL
// with no user part is named, so the error line is reached. The user part's values are made for this
// test.
func TestTheBrokersTransportErrorNamesNoPartOfItsUserPart(t *testing.T) {
	const (
		user     = "made-up-user-6b1d"
		password = "made-up-pw-not-a-secret-91e2"
		token    = "not-a-secret-madeuptoken0c4f"
	)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("made-up-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	verify := func(broker string) string {
		v, err := New(Options{
			BrokerURL: broker,
			Account:   NewAccountToken(file),
			Replay:    noReplay{},
			Address:   func(*http.Request) string { return "192.0.2.1" },
			Origin:    "https://dispatch.test",
		})
		if err != nil {
			t.Fatalf("New(%q): %v", broker, err)
		}
		r := httptest.NewRequest(http.MethodGet, "https://dispatch.test/api/v1/issues", nil)
		r.Header.Set("Authorization", "DPoP made-up-access-token")
		r.Header.Set("DPoP", "made.up.proof")
		_, err = v.Verify(context.Background(), r)
		if err == nil {
			t.Fatalf("Verify against %q passed", broker)
		}
		return err.Error()
	}
	control := verify("http://127.0.0.1:1")
	t.Logf("control (no user part): %s", control)
	if !strings.Contains(control, "127.0.0.1:1") {
		t.Fatalf("UNKNOWN: the control's error names no broker, so this test saw nothing: %q", control)
	}
	for name, broker := range map[string]string{
		"user and password": "http://" + user + ":" + password + "@127.0.0.1:1",
		"token as the user": "http://" + token + "@127.0.0.1:1",
	} {
		text := verify(broker)
		shown := strings.NewReplacer(user, "<USER>", password, "<PASSWORD>", token, "<TOKEN>").Replace(text)
		t.Logf("%s: %s", name, shown)
		for _, part := range []string{user, password, token} {
			if strings.Contains(text, part) {
				t.Errorf("RED %s: the error Dispatch logs holds a part of the broker URL's user part: %s", name, shown)
				break
			}
		}
	}
}
