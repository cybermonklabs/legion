package urlsetting_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/urlsetting"
)

// Each form whose user part net/url reads apart is refused by its length and its cause, naming
// the setting and no part of the value; each honest form, with a user and a password net/url
// reads whole or with none, is admitted. The user, the password and the token are made for this
// test.
func TestAURLSettingWhoseUserPartNetURLReadsApartIsRefusedByItsLength(t *testing.T) {
	const (
		user     = "made-up-user-3f1a"
		password = "not-a-secret-8c2e"
		token    = "not-a-secret-tok5b7"
	)
	for _, tc := range []struct {
		name, raw string
		why       redact.Misreading
	}{
		// net/url ends the authority at the first /, ? or #, so the token's head is read as the host.
		{"a token with a / as the whole user part", "https://" + token + "/" + password + "@envoy.example", redact.AtOutsideUserPart},
		{"a token with a ? as the whole user part", "nats://" + token + "?" + password + "@nats.example:4222", redact.AtOutsideUserPart},
		{"a token with a # as the whole user part", "http://" + token + "#" + password + "@127.0.0.1:13370", redact.AtOutsideUserPart},
		// The password's head is then read as a port, which net/url refuses with the value whole.
		{"a / in the password", "http://" + user + ":" + password + "/x@127.0.0.1:9020", redact.Unparsed},
		{"no scheme before the user part", user + ":" + password + "@envoy.example", redact.NoScheme},
		{"a line ending left from a file", "https://" + user + ":" + password + "@envoy.example\n", redact.ControlCharacter},
		{"a user part that does not parse", "http://" + user + ":" + password + "\\x@host:1", redact.Unparsed},
		{"a user and a password net/url reads whole", "https://" + user + ":" + password + "@envoy.example:9020/base", ""},
		{"a token as the whole user part", "nats://" + token + "@nats.example:4222", ""},
		{"no user part", "https://envoy.example:9020", ""},
		{"no URL at all", "envoy-listener", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := urlsetting.Refuse("envoy_url", tc.raw)
			if tc.why == "" {
				if err != nil {
					t.Fatalf("Refuse(%q) = %v, want nil", tc.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Refuse(%q) = nil, want a refusal for %q", tc.raw, tc.why)
			}
			if want := redact.Refusal("envoy_url", tc.raw, tc.why).Error(); err.Error() != want {
				t.Errorf("Refuse = %q, want %q", err, want)
			}
			for _, piece := range []string{user, password, token} {
				if strings.Contains(err.Error(), piece) {
					t.Errorf("the refusal %q holds %q", err, piece)
				}
			}
		})
	}
}
