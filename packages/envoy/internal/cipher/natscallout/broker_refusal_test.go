package natscallout

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// The refusal of the broker's token is the one rule that joins a refresh, and it holds before
// any answer: a first refresh in which Dispatch refuses the token on every question refuses PUB's
// daemon project_access_refused, naming that Dispatch refuses the broker's token, and not
// project_access_unknown, which would tell the operator to wait for an answer that cannot come.
// The control checks the projects' answers before the refusal: the daemon is refused
// project_access_unknown, and the test fails naming that label.
func TestARefusalOfTheBrokersTokenBeforeAnyAnswerIsNamedSo(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	tokens := map[string]platformToken{"token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	deps := Deps{
		ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"PUB"}}),
		Access: func(context.Context, string, string) (projectaccess.Answer, error) {
			return projectaccess.Answer{}, projectaccess.ErrBrokerRefused
		},
	}
	deps.held = newHeldAccess(deps, services)
	require.NotNil(t, deps.held)
	deps.held.refresh(t.Context())
	require.False(t, deps.held.known("PUB", entryPerson), "a refused question was held as an answer")

	_, err := Decide(t.Context(), deps, services, serviceRequest("token"))
	requireRefused(t, err, labelAccessRefused, "Dispatch refuses the broker's own account token")
}
