package natscallout

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
)

// otherFails is the error OTHER's question answers with in these tests.
var otherFails = errors.New(`projectaccess: dispatch answered /api/v1/access/projects/OTHER 500 with the code ""`)

// No rule joins the answers of one refresh: a project's own answer admits or refuses its daemon
// while another entry's project keeps failing. OTHER fails from the broker's start, so its daemon
// is refused as not yet known, while PUB's daemon is admitted on PUB's first answer. Then PUB is
// hidden from its person and refused, and once its person is put back, PUB's daemon is admitted on
// that answer; then PUB is taken out of Dispatch and refused as not found, OTHER failing
// throughout. The control holds a refresh's answers only when every question of it is answered:
// PUB's daemon is refused on the first refresh, and the test fails naming PUB.
func TestAProjectIsAdmittedOnItsOwnAnswerWhileAnotherFails(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: otherDaemonAccount}}
	tokens := map[string]platformToken{
		"pub-token":   {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)},
		"other-token": {account: otherDaemonAccount, issued: now, expiry: now.Add(time.Hour)},
	}
	dispatch := &dispatchAnswers{
		projects:   map[string]bool{"PUB": true, "OTHER": true},
		private:    map[string]bool{"PUB": true},
		failingFor: map[string]error{"OTHER": otherFails},
	}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}, otherDaemonAccount: {"OTHER"}}, dispatch, services)
	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	require.NoError(t, err, "PUB's daemon was refused on PUB's answer while OTHER failed: PUB")
	_, err = Decide(t.Context(), deps, services, serviceRequest("other-token"))
	requireRefused(t, err, labelProjectAccessUnknown, "project access not yet known: OTHER")

	dispatch.hide("PUB", true)
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	requireRefused(t, err, labelProjectNotVisible, "project not visible to the account's person: PUB")

	dispatch.hide("PUB", false)
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	require.NoError(t, err, "PUB's daemon was refused after its person was put back, while OTHER failed: PUB")

	dispatch.drop("PUB")
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	requireRefused(t, err, labelProjectNotFound, "project not found: PUB")
}

// An answer is held whatever else its refresh got, a refusal of the broker's token included, so a
// revocation Dispatch answered beside that refusal is not lost once the refusal clears. A first
// refresh admits PUB's daemon. In a second, Dispatch refuses the broker's token on OTHER's
// question and answers that PUB's person no longer sees PUB: the daemon is refused
// project_access_refused. In a third, OTHER is answered and PUB's question fails: the refusal
// clears, and the daemon is refused project_not_visible on the revocation the second refresh
// read. The control drops a refresh's answers when it holds a refusal: the third decision admits
// the daemon on its answer from before the revocation, and the test fails naming PUB.
func TestARevocationBesideARefusalOfTheBrokersTokenIsKept(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}, {Kind: KindDaemon, Account: otherDaemonAccount}}
	tokens := map[string]platformToken{"pub-token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	dispatch := &dispatchAnswers{
		projects: map[string]bool{"PUB": true, "OTHER": true},
		private:  map[string]bool{"PUB": true},
	}
	deps := heldDeps(t, tokens, map[string][]string{pubDaemonAccount: {"PUB"}, otherDaemonAccount: {"OTHER"}}, dispatch, services)
	_, err := Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	require.NoError(t, err, "PUB's daemon was refused on its first answer")

	dispatch.mu.Lock()
	dispatch.failingFor = map[string]error{"OTHER": fmt.Errorf("%w: dispatch answered /api/v1/access/projects/OTHER 403", projectaccess.ErrBrokerRefused)}
	dispatch.mu.Unlock()
	dispatch.hide("PUB", true)
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	requireRefused(t, err, labelAccessRefused, "Dispatch refuses the broker's own account token")

	dispatch.mu.Lock()
	dispatch.failingFor = map[string]error{"PUB": errors.New(`projectaccess: dispatch answered /api/v1/access/projects/PUB 500 with the code ""`)}
	dispatch.mu.Unlock()
	deps.held.refresh(t.Context())
	_, err = Decide(t.Context(), deps, services, serviceRequest("pub-token"))
	require.Error(t, err, "PUB's daemon was admitted on its answer from before the revocation: PUB")
	requireRefused(t, err, labelProjectNotVisible, "project not visible to the account's person: PUB")
}
