package natscallout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

func TestTNOneProjectRefusingBrokerRefusesEveryHeldDaemon(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{
		{Kind: KindDaemon, Account: pubDaemonAccount},
		{Kind: KindDaemon, Account: slowDaemonAccount},
	}
	dispatch := &dispatchAnswers{answers: map[string][]string{
		"PUB": {"acme/pub"}, "OTHER": {"acme/other"},
	}}
	deps := heldDeps(t,
		map[string]platformToken{"token": {
			account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour),
		}},
		map[string][]string{pubDaemonAccount: {"PUB"}, slowDaemonAccount: {"OTHER"}},
		dispatch, services)
	deps.held.access = func(ctx context.Context, project, person string) (projectaccess.Answer, error) {
		if project == "OTHER" {
			return projectaccess.Answer{}, projectaccess.ErrBrokerRefused
		}
		return dispatch.access(ctx, project, person)
	}
	deps.held.refresh(t.Context())
	_, err := Decide(t.Context(), deps, services, serviceRequest("token"))
	requireRefused(t, err, labelAccessRefused, "Dispatch refuses the broker's own account token")
}

// A refusal of the broker's token on one project's question refuses every held daemon even when
// the pass's other question failed: the refusal is the pass's outcome, failures included.
func TestTNOneProjectRefusalBesideAFailedProjectMustRefuse(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"PUB": {"acme/pub"}, "OPS": {"acme/ops"}}}
	deps := heldDeps(t,
		map[string]platformToken{"token": {
			account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour),
		}},
		map[string][]string{pubDaemonAccount: {"OPS", "PUB"}}, dispatch, services)
	deps.held.access = func(_ context.Context, project, _ string) (projectaccess.Answer, error) {
		if project == "OPS" {
			return projectaccess.Answer{}, projectaccess.ErrBrokerRefused
		}
		return projectaccess.Answer{}, errors.New("connection reset")
	}
	deps.held.refresh(t.Context())
	_, err := Decide(t.Context(), deps, services, serviceRequest("token"))
	requireRefused(t, err, labelAccessRefused, "Dispatch refuses the broker's own account token")
}
