package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAMachineLoginPassRevokesWhatTheRemovalOwesWhileItsClaimMaySend: one owed revocation costs
// the person's list and a revoke for each login they approved for their own machines before the
// removal, and the pass asks before each of those calls whether its claim may still send one. Of
// erin's three logins issued before the removal, her one issued after it, and a service's login she
// approved, the pass revokes the three. A claim that may send two calls lists and revokes one, then
// stops with errRevocationWindowSpent, and the next claim, given the broker's list without the one
// it revoked, revokes the other two.
//
// Control: revoke every own login whatever its issued_at, and the row fails naming cred-after; send
// with no check, and the stopped pass revokes all three, named.
func TestAMachineLoginPassRevokesWhatTheRemovalOwesWhileItsClaimMaySend(t *testing.T) {
	rig := newFakeBrokerRig(t)
	client := agentsecrets.New(rig.server.URL, accountTokenFile(t, credentialTestAccountToken))
	removal := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	login := func(id, issued, service string) string {
		svc := "null"
		if service != "" {
			svc = `"` + service + `"`
		}
		return `{"credential_id":"` + id + `","host":"h","service":` + svc + `,"subject":null,"approved_by":"erin@other.example","issued_at":"` + issued + `","expires_at":"2026-12-20T00:00:00Z","expired":false}`
	}
	before := []string{
		login("cred-1", "2026-11-20T09:00:00Z", ""),
		login("cred-2", "2026-11-20T10:00:00Z", ""),
		login("cred-3", "2026-11-20T11:59:59Z", ""),
	}
	listing := func(logins ...string) string { return `{"credentials":[` + strings.Join(logins, ",") + `]}` }
	after := login("cred-after", "2026-11-20T12:00:01Z", "")
	service := login("cred-service", "2026-11-20T09:00:00Z", "legion-daemon")
	owed := store.OwedMachineLoginRevocation{Email: "erin@other.example", Generation: 1, RecordedAt: removal}
	ctx := t.Context()

	rig.bodyByRoute["GET /v1/launcher-credentials"] = listing(append(slices.Clone(before), after, service)...)
	calls := 0
	twoCalls := func() (bool, error) { calls++; return calls <= 2, nil }
	revoked, err := revokeMachineLoginsOf(ctx, client, owed, twoCalls)
	if !errors.Is(err, errRevocationWindowSpent) || revoked != 1 || !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-1/revoke-by-approver"}) {
		t.Fatalf("a claim that may send two calls revoked %d (%v), sending %v; want cred-1 alone and errRevocationWindowSpent", revoked, err, rig.revokes())
	}

	rig.routes = nil
	rig.bodyByRoute["GET /v1/launcher-credentials"] = listing(before[1], before[2], after, service)
	always := func() (bool, error) { return true, nil }
	revoked, err = revokeMachineLoginsOf(ctx, client, owed, always)
	want := []string{"POST /v1/launcher-credentials/cred-2/revoke-by-approver", "POST /v1/launcher-credentials/cred-3/revoke-by-approver"}
	if err != nil || revoked != 2 || !slices.Equal(rig.revokes(), want) {
		t.Errorf("the next claim revoked %d (%v), sending %v; want cred-2 and cred-3, and neither cred-after, issued after the removal, nor the service's", revoked, err, rig.revokes())
	}
}

// TestAClaimedMachineLoginRevocationPastItsSendWindowSendsNoRevoke: the loop's pass asks the store
// before each broker call whether its claim may still send one (store.MachineLoginRevocations'
// Sendable). alice takes erin, a restricted member, off ACC's list, and the pass claims the
// revocation that owes and lists erin's machine logins. While the list is in flight the claim ages
// past the retry window less the broker client's bound on a call (its tried_at set back that far and
// a second more, as a slow list would leave it), so the pass sends no revoke of erin's login, which
// another process may claim again before such a call is over, and the revocation stays owed for the
// next claim. The list reaching the broker is the row's own control: the pass ran, and its claim
// was within its window then.
//
// Control: revokeOwedMachineLogins' mayCall answering true whatever the claim's age, and the pass
// revokes erin's login, named.
func TestAClaimedMachineLoginRevocationPastItsSendWindowSendsNoRevoke(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		signInOutside:   true,
		agentSecretsURL: rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	ctx := t.Context()
	for _, write := range []struct{ method, does string }{{http.MethodPut, "names erin on"}, {http.MethodDelete, "takes erin off"}} {
		if response := dispatchRequest(t, served, write.method, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("alice %s ACC's list: status=%d body=%s", write.does, response.Code, response.Body.String())
		}
	}
	const list = "GET /v1/launcher-credentials"
	rig.bodyByRoute[list] = `{"credentials":[{"credential_id":"cred-erin","host":"erin-laptop","service":null,"subject":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
	aged := brokerRevocationRetry - registered.deps.AgentSecrets.HTTP.Timeout + time.Second
	rig.beforeAnswer[list] = func() {
		if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set tried_at = tried_at - make_interval(secs => $1)`, aged.Seconds()); err != nil {
			t.Errorf("age the claim while the list is in flight: %v", err)
		}
	}

	registered.revokeBrokerDebts(ctx)
	if !slices.Contains(rig.routes, list) {
		t.Fatalf("the pass sent %v, want erin's list first: the claim was within its window then", rig.routes)
	}
	if revokes := rig.revokes(); len(revokes) != 0 {
		t.Errorf("the pass sent %v after its claim aged past its window during the list, want no revoke", revokes)
	}
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || !owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after the pass, want it still owed", owed, err)
	}
}
