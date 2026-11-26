// packages/envoy/internal/cipher/proxy/prefix_alone_test.go
//
// The leftover-placeholder refusal (leftoverPlaceholder) is about placeholders: the prefix and the
// placeholder.Length letters and digits the broker mints after it. A request whose path or query
// holds the prefix with no placeholder after it - a code search for the prefix itself, which an
// agent working on the broker makes - carries no proxy password and must reach the service.
package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// TestThePrefixWithNoPlaceholderAfterItIsNotRefused: the prefix alone, and the prefix followed by
// fewer letters and digits than a placeholder holds, are no placeholder, so a request carrying
// them in its query, beside the own grant's placeholder in the swapped header, is swapped and sent
// with its query as the command wrote it. The control, in the same run: the prefix followed by a
// whole placeholder's worth of letters and digits that no grant holds is still refused.
func TestThePrefixWithNoPlaceholderAfterItIsNotRefused(t *testing.T) {
	r := newRuledRig(t, enrollmentRules(), Options{})
	client := r.client(workerPlaceholder)
	for i, query := range []string{
		placeholder.Prefix + " repo:acme-org/x",
		placeholder.Prefix + "abc repo:acme-org/x",
	} {
		answer, body := r.get(client, "https://"+boundHost+"/search/code?q="+url.QueryEscape(query), "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("a search for %q answered %d %q; want 200: the prefix with no placeholder after it opens nothing", query, answer.StatusCode, body)
		}
		seen := r.requests()
		if len(seen) != i+1 {
			t.Fatalf("the service saw %d requests; want %d", len(seen), i+1)
		}
		if got := seen[i].Header.Get("Authorization"); got != "Bearer "+workerValue {
			t.Errorf("a search for %q: the service saw Authorization %q; want the swapped value", query, got)
		}
	}

	unknown := placeholder.Prefix + "unknownunknownunknownunknownunkn"
	answer, _ := r.get(client, "https://"+boundHost+"/search/code?q="+url.QueryEscape(unknown), "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusForbidden {
		t.Fatalf("a whole placeholder no grant holds answered %d; want 403 PLACEHOLDER_NOT_SWAPPED", answer.StatusCode)
	}
}
