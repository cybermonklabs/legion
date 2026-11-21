package natscallout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// A pass one question of which fails holds no addition, but every refusal it got applies at once:
// the token exchange refuses a removed person's token within 30 s whatever else Dispatch answers,
// so the connection may not wait on an unrelated question. PUB's daemon is admitted on a first
// complete pass; then PUB is taken from its person (not visible) or from Dispatch (not found),
// while another entry's project, OTHER, answers 500. The next decision is refused in that same
// pass. The control holds nothing of an incomplete pass: each row is admitted on the first pass.
func TestAnIncompletePassStillAppliesItsRefusals(t *testing.T) {
	for _, row := range []struct {
		name   string
		revoke func(d *dispatchAnswers)
		label  string
		reason string
	}{
		{"PUB hidden from its person", func(d *dispatchAnswers) { d.hide("PUB", true) },
			"project_not_visible", "project not visible to the account's person: PUB"},
		{"PUB gone from Dispatch", func(d *dispatchAnswers) { d.drop("PUB") },
			"project_not_found", "project not found: PUB"},
	} {
		t.Run(row.name, func(t *testing.T) {
			now := time.Now()
			services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: slowDaemonAccount}}
			tokens := map[string]platformToken{"pub-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
			dispatch := &dispatchAnswers{
				answers: map[string][]string{"PUB": {"acme/pub"}, "OTHER": {"acme/other"}},
				private: map[string]bool{"PUB": true},
			}
			deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}, slowDaemonAccount: {"OTHER"}}, dispatch, services)
			_, err := Decide(t.Context(), deps, services, serviceRequest("pub-token"))
			require.NoError(t, err, "PUB's daemon was refused on the first complete pass")

			row.revoke(dispatch)
			deps.held.access = func(ctx context.Context, project, person string) (projectaccess.Answer, error) {
				if project == "OTHER" {
					return projectaccess.Answer{}, errors.New(`projectaccess: dispatch answered /api/v1/access/projects/OTHER 500 with the code ""`)
				}
				return dispatch.access(ctx, project, person)
			}
			deps.held.refresh(t.Context())

			_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
			requireRefused(t, err, row.label, row.reason)
		})
	}
}

// An incomplete pass gives back nothing a complete pass refused: a project answered visible again
// in a pass that another question fails stays refused until a complete pass. PUB is hidden from
// its person at a first complete pass; then its person is put back while OTHER answers 500. PUB's
// daemon is still refused, and the next complete pass admits it. The control lets an incomplete
// pass's visible answer replace the held refusal: the daemon is admitted on it.
func TestAnIncompletePassGivesBackNoRefusal(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: slowDaemonAccount}}
	tokens := map[string]platformToken{"pub-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{
		answers: map[string][]string{"PUB": {"acme/pub"}, "OTHER": {"acme/other"}},
		private: map[string]bool{"PUB": true},
		unseen:  map[string]bool{"PUB": true},
	}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}, slowDaemonAccount: {"OTHER"}}, dispatch, services)
	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	requireRefused(t, err, labelProjectNotVisible, "project not visible to the account's person: PUB")

	dispatch.hide("PUB", false)
	dispatch.mu.Lock()
	dispatch.failingFor = map[string]error{"OTHER": errors.New(`projectaccess: dispatch answered /api/v1/access/projects/OTHER 500 with the code ""`)}
	dispatch.mu.Unlock()
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	requireRefused(t, err, labelProjectNotVisible, "project not visible to the account's person: PUB")

	dispatch.mu.Lock()
	dispatch.failingFor = nil
	dispatch.mu.Unlock()
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	require.NoError(t, err, "the complete pass after PUB's person was put back did not admit its daemon")
}
