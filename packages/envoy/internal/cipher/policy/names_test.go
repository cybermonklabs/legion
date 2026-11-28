package policy_test

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

func TestNameSlugRoundTrip(t *testing.T) {
	for name, slug := range map[string]string{"DEEL_API" + "_KEY": "deel-api-key", "A0": "a0", "A_B": "a-b"} {
		got, err := policy.NameToSlug(name)
		if err != nil || got != slug {
			t.Fatalf("NameToSlug(%q) = %q, %v; want %q", name, got, err, slug)
		}
		if back := policy.SlugToName(slug); back != name {
			t.Fatalf("SlugToName(%q) = %q, want %q", slug, back, name)
		}
	}
	for _, bad := range []string{"deel_api_key", "_X", "A__B", "A-B", ""} {
		if _, err := policy.NameToSlug(bad); !errors.Is(err, policy.ErrNameInvalid) {
			t.Fatalf("NameToSlug(%q) = %v, want ErrNameInvalid", bad, err)
		}
	}
}

// TestDisplayNameIsTheNameASessionAsksFor: a store name under the prefix whose slug maps back to
// a NAME displays as that NAME, on either store's shape of name; one whose slug maps to none
// (an underscore, an uppercase letter, two hyphens) displays as the store's whole name.
func TestDisplayNameIsTheNameASessionAsksFor(t *testing.T) {
	for _, c := range []struct{ prefix, id, want string }{
		{"dev/agent-secrets/", "dev/agent-secrets/deel-api-key", "DEEL_API" + "_KEY"},
		{"projects/1/secrets/agent-secrets-", "projects/1/secrets/agent-secrets-a-b", "A_B"},
		{"dev/agent-secrets/", "dev/agent-secrets/deel_api_key", "dev/agent-secrets/deel_api_key"},
		{"dev/agent-secrets/", "dev/agent-secrets/Deel-key", "dev/agent-secrets/Deel-key"},
		{"dev/agent-secrets/", "dev/agent-secrets/a--b", "dev/agent-secrets/a--b"},
	} {
		if got := policy.DisplayName(c.prefix, c.id); got != c.want {
			t.Errorf("DisplayName(%q, %q) = %q, want %q", c.prefix, c.id, got, c.want)
		}
	}
}
