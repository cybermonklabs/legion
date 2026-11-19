package oidc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
)

// An issuer (DISPATCH_OIDC_ISSUER, DISPATCH_SIGNIN_ISSUER) may carry a user and a password. Each
// refusal of a discovery that failed and each line the background discovery logs names it with
// neither; the control is the same issuer with no user part, which every one of them names. The
// issuer listens nowhere, so each discovery fails at once. The password is made for this test.
func TestAnIssuerWithAUserPartIsNamedWithNoneOfIt(t *testing.T) {
	const (
		user     = "made-up-user"
		password = "not-a-secret-61b0e3"
	)
	for _, userPart := range []string{user + ":" + password + "@", ""} {
		issuer := "http://" + userPart + "127.0.0.1:1"
		ctx := context.Background()
		var texts []string
		if _, err := NewKeys(ctx, issuer); err == nil {
			t.Fatalf("NewKeys(%q) found an issuer that listens nowhere", issuer)
		} else {
			texts = append(texts, err.Error())
		}
		if _, err := discoverCodeFlow(ctx, issuer, "made-up-client", "not-a-secret-client"); err == nil {
			t.Fatalf("the code flow found an issuer that listens nowhere")
		} else {
			texts = append(texts, err.Error())
		}
		if _, err := CheckClient(ctx, issuer, "made-up-client", "not-a-secret-client"); err == nil {
			t.Fatalf("the client check found an issuer that listens nowhere")
		} else {
			texts = append(texts, err.Error())
		}
		var logged bytes.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		found := newDiscovery(issuer, func(context.Context) (int, error) { return 1, nil })
		found.logFailure(errors.New("made-up failure"))
		found.run(ctx)
		texts = append(texts, logged.String())
		for _, text := range texts {
			if strings.Contains(text, password) || strings.Contains(text, user) {
				t.Errorf("user part %q: %q holds the user part", userPart, text)
			}
			if userPart == "" && !strings.Contains(text, "127.0.0.1:1") {
				t.Errorf("the control with no user part: %q names no issuer", text)
			}
		}
	}
}
