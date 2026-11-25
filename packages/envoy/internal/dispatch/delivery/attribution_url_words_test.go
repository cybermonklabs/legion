package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnAttributionReadThatFailsPrintsNoneOfThePullRequestsURL: the read of the issues a pull
// request's stored inputs name, failing (its context is cancelled before it starts, failed-read),
// for a pull request whose stored URL echoes redacttest's credentials, names the pull request by
// what it is, and the refusal holds no run of a credential. The refusal named the stored URL. The
// twin shows the URL holds every credential and the read still fails, so the row can see one
// printed. Captures delivery.attribution-url/failed-read.
func TestAnAttributionReadThatFailsPrintsNoneOfThePullRequestsURL(t *testing.T) {
	pool, _ := deliveryTestPool(t)
	url := "https://" + redacttest.User + ":" + redacttest.Password + "@github.com/acme/widgets/pull/7?access" + "_token=" + redacttest.Token
	attributor := unrenamed()
	read := func() error {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		facts := attributionFacts{Repo: "acme/widgets", URL: url, Title: "a title"}
		_, err := attributor.resolveAttributionCandidates(cancelled, pool, facts.Repo, facts.URL, attributor.attributionInputsFrom(facts))
		return err
	}
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(url); len(held) != len(redacttest.Secrets) {
			t.Errorf("the URL %q holds %v; want every credential, so the row can see one printed", url, held)
		}
		if err := read(); err == nil || !strings.Contains(err.Error(), "resolve the") {
			t.Errorf("the read = %v; want it to fail resolving the pull request's issue", err)
		}
	})
	t.Run("failed-read", func(t *testing.T) {
		err := read()
		if err == nil {
			t.Fatal("the read with a cancelled context answered; want it to fail")
		}
		redacttest.RequireNoPiece(t, "delivery.attribution-url/failed-read", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
	})
}
