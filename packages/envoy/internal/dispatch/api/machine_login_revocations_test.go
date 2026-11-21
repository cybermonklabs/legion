package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// removalAged sets every owed machine-login revocation's removal back to age before now, and its
// last claim, where a pass made one, back past the retry window, as that much time passing would:
// the next pass claims it, and its claim comes age after the removal on the database's clock.
func removalAged(t *testing.T, database *store.Store, age time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		update machine_login_revocations
		set recorded_at = clock_timestamp() - make_interval(secs => $1),
			tried_at = case when tried_at is null then null else clock_timestamp() - make_interval(secs => $2) end
		where done_at is null
	`, age.Seconds(), brokerRevocationRetry.Seconds()); err != nil {
		t.Fatalf("set the owed machine-login revocations' removal %s back: %v", age, err)
	}
}

// approveHeldAtBroker sends erin's browser approve of her own machine login, rec-erin, whose
// lookup the broker answers as hers (erinsOwnLogin), and returns once the broker holds it
// unanswered. The broker answers it when release is called, which the test's cleanup calls too,
// and code then receives Dispatch's answer to the approve. The broker answers at once every other
// approve of rec-erin that reaches it meanwhile.
func approveHeldAtBroker(t *testing.T, rig *fakeBrokerRig, served http.Handler) (release func(), code <-chan int) {
	t.Helper()
	const approve = "POST /v1/credential-requests/rec-erin/approve"
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	reached, held := make(chan struct{}), make(chan struct{})
	var arrivals atomic.Int32
	rig.beforeAnswer[approve] = func() {
		if arrivals.Add(1) == 1 {
			close(reached)
			<-held
		}
	}
	release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(release)
	answered := make(chan int, 1)
	go func() {
		answered <- dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin).Code
	}()
	select {
	case <-reached:
	case early := <-answered:
		t.Fatalf("erin's approve answered %d before the broker held it; the broker was sent %v", early, rig.routes)
	}
	return release, answered
}

// advisoryLockWaiters is how many sessions of the test's database, its own aside, wait on an
// advisory lock: a person's approval lock, a project's owner lock, or another.
func advisoryLockWaiters(t *testing.T, database *store.Store) int {
	t.Helper()
	var n int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&n); err != nil {
		t.Fatalf("read how many sessions wait on an advisory lock: %v", err)
	}
	return n
}

// answeredOrWaiting returns once answer yields, with what it yielded and answered true, or once a
// session of the test's database waits on an advisory lock (advisoryLockWaiters), with answered
// false, and fails the test after 30 s with neither; what names the request answer is for. Its
// caller decides what an answer before any wait means.
func answeredOrWaiting[T any](t *testing.T, database *store.Store, answer <-chan T, what string) (early T, answered bool) {
	t.Helper()
	for giveUp := time.Now().Add(30 * time.Second); advisoryLockWaiters(t, database) == 0; time.Sleep(20 * time.Millisecond) {
		select {
		case early = <-answer:
			return early, true
		default:
			if time.Now().After(giveUp) {
				t.Fatalf("%s neither answered nor waited on a lock within 30 s", what)
			}
		}
	}
	return early, false
}

// holdOwnerLock begins a transaction of the test's database that holds project key's owner lock,
// as another write of the project's access holds it, until the row commits or rolls it back; the
// test's cleanup rolls it back.
func holdOwnerLock(t *testing.T, database *store.Store, key string) pgx.Tx {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(key)); err != nil {
		t.Fatalf("hold %s's owner lock: %v", key, err)
	}
	return tx
}

// TestAMachineLoginPassRevokesWhatTheRemovalOwesWhileItsClaimMaySend: one owed revocation costs
// the person's list and a revoke for each login they approved for their own machines, and the pass
// asks before each of those calls whether its claim may still send one. Of erin's four logins and a
// service's login she approved, the pass revokes the four, whatever each one's issued_at: no
// approval of hers was relayed since the removal (store.HoldMachineLoginApprovals). A claim that may
// send two calls lists and revokes one, then stops with errRevocationWindowSpent, and the next
// claim, given the broker's list without the one it revoked, revokes the other three.
//
// Control: send with no check, and the stopped pass revokes all four, named.
func TestAMachineLoginPassRevokesWhatTheRemovalOwesWhileItsClaimMaySend(t *testing.T) {
	rig := newFakeBrokerRig(t)
	client := cipher.New(rig.server.URL, accountTokenFile(t, credentialTestAccountToken))
	removal := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	login := func(id, issued, service string) string {
		svc := "null"
		if service != "" {
			svc = `"` + service + `"`
		}
		return `{"credential_id":"` + id + `","host":"h","service":` + svc + `,"subject":null,"approved_by":"erin@other.example","issued_at":"` + issued + `","expires_at":"2026-12-20T00:00:00Z","expired":false}`
	}
	own := []string{
		login("cred-1", "2026-11-20T09:00:00Z", ""),
		login("cred-2", "2026-11-20T10:00:00Z", ""),
		login("cred-3", "2026-11-20T11:59:59Z", ""),
		login("cred-4", "2026-11-20T12:00:01Z", ""),
	}
	listing := func(logins ...string) string { return `{"credentials":[` + strings.Join(logins, ",") + `]}` }
	service := login("cred-service", "2026-11-20T09:00:00Z", "legion-daemon")
	owed := store.OwedMachineLoginRevocation{Email: "erin@other.example", Generation: 1, RecordedAt: removal}
	ctx := t.Context()

	rig.bodyByRoute["GET /v1/launcher-credentials"] = listing(append(slices.Clone(own), service)...)
	calls := 0
	twoCalls := func() (bool, error) { calls++; return calls <= 2, nil }
	revoked, err := revokeMachineLoginsOf(ctx, client, owed, twoCalls)
	if !errors.Is(err, errRevocationWindowSpent) || revoked != 1 || !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-1/revoke-by-approver"}) {
		t.Fatalf("a claim that may send two calls revoked %d (%v), sending %v; want cred-1 alone and errRevocationWindowSpent", revoked, err, rig.revokes())
	}

	rig.routes = nil
	rig.bodyByRoute["GET /v1/launcher-credentials"] = listing(own[1], own[2], own[3], service)
	always := func() (bool, error) { return true, nil }
	revoked, err = revokeMachineLoginsOf(ctx, client, owed, always)
	want := []string{"POST /v1/launcher-credentials/cred-2/revoke-by-approver", "POST /v1/launcher-credentials/cred-3/revoke-by-approver", "POST /v1/launcher-credentials/cred-4/revoke-by-approver"}
	if err != nil || revoked != 3 || !slices.Equal(rig.revokes(), want) {
		t.Errorf("the next claim revoked %d (%v), sending %v; want cred-2, cred-3 and cred-4, and not the service's", revoked, err, rig.revokes())
	}
}

// TestARemovalRevokesALoginTheBrokersClockStampsAfterIt: the broker stamps a login's issued_at on
// its own clock, and a removal's recorded_at is Dispatch's database's. carol takes erin, a
// restricted member ACC's list still names, off OTH's list, and the broker, whose clock runs an hour
// ahead of Dispatch's, lists erin's login approved before the removal as issued an hour after it.
// The pass revokes it, comparing no clock, and a pass whose claim comes machineLoginSettleAfter
// after the removal settles the revocation.
//
// Control: revoke only the logins issued before the removal's recorded_at, as the head before this
// row did, and the pass revokes nothing, named.
func TestARemovalRevokesALoginTheBrokersClockStampsAfterIt(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if created := dispatchRequest(t, served, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Another project"}, accessCarol); created.Code != http.StatusCreated {
		t.Fatalf("carol creates OTH: %d %s", created.Code, created.Body.String())
	}
	for _, write := range []struct{ method, key, owner, does string }{
		{http.MethodPut, "ACC", "alice", "names erin on"},
		{http.MethodPut, "OTH", accessCarol, "names erin on"},
		{http.MethodDelete, "OTH", accessCarol, "takes erin off"},
	} {
		if response := dispatchRequest(t, served, write.method, "/api/v1/projects/"+write.key+"/access/people/"+accessErin, nil, write.owner); response.Code != http.StatusOK {
			t.Fatalf("%s %s %s's list: status=%d body=%s", write.owner, write.does, write.key, response.Code, response.Body.String())
		}
	}
	ahead := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	rig.bodyByRoute["GET /v1/launcher-credentials"] = `{"credentials":[{"credential_id":"cred-erin","host":"erin-laptop","service":null,"subject":null,"approved_by":"erin@other.example","issued_at":"` + ahead + `","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`

	removalAged(t, database, machineLoginSettleAfter)
	registered.revokeBrokerDebts(ctx)
	if !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-erin/revoke-by-approver"}) {
		t.Errorf("the pass sent %v for erin, her login stamped an hour after the removal on the broker's clock; want it revoked", rig.revokes())
	}
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after the pass, want it settled", owed, err)
	}
}

// TestALoginApprovedInFlightAsTheRemovalCommitsIsRevoked: erin's browser approve of her own machine
// login reaches the broker, which has not answered it when alice takes her off ACC's list. The
// approval holds erin's approval lock until the broker answers (relayMachineLoginApproval), so the
// removal waits for it, and records the revocation only after the login exists; the first pass
// after the removal then lists it and revokes it, and the revocation settles once a pass's claim
// comes machineLoginSettleAfter after the removal. The deployment runs passkey sign-in, so the
// removal also takes erin's links and address and ends her account, as production's does.
//
// Control: access.LockForAccessWrite taking no approval lock for the person a removal takes off,
// and the removal does not wait for the approval: the record's own try of that lock finds the
// approval holding it, and the removal answers while the approval is in flight, named; with the
// record's own lock dropped too, it answers 200 then, named.
func TestALoginApprovedInFlightAsTheRemovalCommitsIsRevoked(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
		passkeyIssuer:    passkeyOutsideOrigin,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const list = "GET /v1/launcher-credentials"
	rig.bodyByRoute[list] = `{"credentials":[]}`
	release, approved := approveHeldAtBroker(t, rig, served)
	removed := make(chan int, 1)
	go func() {
		removed <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice").Code
	}()
	if early, answered := answeredOrWaiting(t, database, removed, "alice's removal of erin"); answered {
		t.Fatalf("alice's removal of erin answered %d while erin's approve was in flight, want it waiting on erin's approval lock", early)
	}
	rig.bodyByRoute[list] = `{"credentials":[{"credential_id":"cred-in-flight","host":"erin-desktop","service":null,"subject":null,"approved_by":"erin@other.example","issued_at":"` + time.Now().UTC().Format(time.RFC3339) + `","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
	release()
	if code := <-approved; code != http.StatusOK {
		t.Fatalf("erin's approve in flight answered %d, want 200: it was admitted before the removal", code)
	}
	if code := <-removed; code != http.StatusOK {
		t.Fatalf("alice's removal of erin answered %d, want 200", code)
	}

	// The removal's first pass, after its commit, which waited for the approval.
	registered.revokeBrokerDebts(ctx)
	if first := rig.revokes(); !slices.Contains(first, "POST /v1/launcher-credentials/cred-in-flight/revoke-by-approver") {
		t.Errorf("the first pass after the removal sent %v; want the login erin's approval in flight minted revoked", first)
	}
	removalAged(t, database, machineLoginSettleAfter)
	registered.revokeBrokerDebts(ctx)
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after a pass past the bound, want it settled", owed, err)
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
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
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

// erinsLoginListed is the broker's list of erin's machine logins holding one login of hers, id.
func erinsLoginListed(id string) string {
	return `{"credentials":[{"credential_id":"` + id + `","host":"erin-desktop","service":null,"subject":null,"person":null,"approved_by":"erin@other.example","issued_at":"2026-11-20T00:00:00Z","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
}

// TestALoginTheBrokerCommitsAfterTheRelayTimedOutIsRevoked: Dispatch holds erin's approval lock
// only for its own call to the broker, which its client ends after five seconds, while the broker
// decides the approve on its own request's context and may commit after that. erin's browser
// approve of her own machine login reaches the broker, which does not answer within Dispatch's
// call: the relay gives up and releases the lock, alice takes erin off ACC's list, and the pass
// that follows at once lists no login of erin's. The broker then commits the login the approve
// asked. The revocation stays owed until a pass's claim comes machineLoginSettleAfter after the
// removal, and that pass lists the late login, revokes it and settles.
//
// Control: settle at any claim, whatever its age (Settle's bound dropped), and the pass at once
// settles on its empty list, so the late login is never revoked, named.
func TestALoginTheBrokerCommitsAfterTheRelayTimedOutIsRevoked(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const list = "GET /v1/launcher-credentials"
	rig.bodyByRoute[list] = `{"credentials":[]}`
	commit, approved := approveHeldAtBroker(t, rig, served)
	if code := <-approved; code == http.StatusOK {
		t.Fatalf("erin's approve answered 200 while the broker held it, want Dispatch's call to give up")
	}
	if removed := dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); removed.Code != http.StatusOK {
		t.Fatalf("alice's removal of erin after the relay gave up answered %d %s, want 200", removed.Code, removed.Body.String())
	}
	registered.revokeBrokerDebts(ctx)
	if !slices.Contains(rig.routes, list) {
		t.Fatalf("the pass after the removal sent %v, want erin's list", rig.routes)
	}

	// The broker commits the login erin's approve asked, after the relay gave up and after the
	// removal was recorded, while its own handler of the approve still runs.
	rig.bodyByRoute[list] = erinsLoginListed("cred-late")
	commit()
	removalAged(t, database, machineLoginSettleAfter)
	registered.revokeBrokerDebts(ctx)
	if !slices.Contains(rig.revokes(), "POST /v1/launcher-credentials/cred-late/revoke-by-approver") {
		t.Errorf("the broker was sent %v; want the login it committed after the relay gave up and the removal was recorded revoked", rig.revokes())
	}
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after the pass past the bound, want it settled", owed, err)
	}
}

// TestAPassBeforeTheBrokersDecisionBoundLeavesTheRevocationOwed: a pass whose claim comes sooner
// after a removal than the broker may still commit an approve it took before it
// (machineLoginSettleAfter) revokes the logins it lists and leaves the revocation owed. alice takes
// erin off ACC's list; the pass at once revokes erin's login and settles nothing, and neither does
// a pass whose claim comes wire.RequestDeadline after the removal, inside the broker's write
// margin. The pass whose claim comes machineLoginSettleAfter after it settles.
//
// Control: settle at any claim, whatever its age (Settle's bound dropped), and the pass at once
// settles, named.
func TestAPassBeforeTheBrokersDecisionBoundLeavesTheRevocationOwed(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	for _, write := range []struct{ method, does string }{{http.MethodPut, "names erin on"}, {http.MethodDelete, "takes erin off"}} {
		if response := dispatchRequest(t, served, write.method, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("alice %s ACC's list: status=%d body=%s", write.does, response.Code, response.Body.String())
		}
	}
	const list = "GET /v1/launcher-credentials"
	rig.bodyByRoute[list] = erinsLoginListed("cred-erin")
	revocations := store.NewMachineLoginRevocations(database.Pool)
	owing := func() bool {
		t.Helper()
		owed, _, err := revocations.Owing(ctx, accessErin)
		if err != nil {
			t.Fatalf("read the revocation of erin's machine logins: %v", err)
		}
		return owed
	}

	registered.revokeBrokerDebts(ctx)
	if !slices.Equal(rig.revokes(), []string{"POST /v1/launcher-credentials/cred-erin/revoke-by-approver"}) {
		t.Errorf("the pass at once sent %v, want erin's login revoked", rig.revokes())
	}
	if !owing() {
		t.Errorf("the revocation of erin's machine logins is settled by the pass at once, want it owed until a claim comes %s after the removal", machineLoginSettleAfter)
	}

	// The broker revoked erin's login, so it lists it no more.
	rig.bodyByRoute[list] = `{"credentials":[]}`
	removalAged(t, database, wire.RequestDeadline)
	registered.revokeBrokerDebts(ctx)
	if !owing() {
		t.Errorf("the revocation of erin's machine logins is settled by a pass whose claim came %s after the removal, inside the broker's write margin, want it owed", wire.RequestDeadline)
	}
	removalAged(t, database, machineLoginSettleAfter)
	registered.revokeBrokerDebts(ctx)
	if owing() {
		t.Errorf("the revocation of erin's machine logins is owed after a pass whose claim came %s after the removal, want it settled", machineLoginSettleAfter)
	}
}

// TestAnApproveInFlightDuringTheBrokerRestoredBlockIsRevoked: the runbook's block for a broker
// restored alone owes again a machine-login revocation Dispatch settled since the backup, and a
// login the member approves while it runs must be revoked too. alice takes erin off ACC's list,
// a pass settles that revocation, and alice puts erin back. erin's browser approve of a new machine
// login reaches the broker, which has not answered it when an operator runs the block: the block
// takes erin's approval lock as a removal does, so it waits for the approve, and records the debt
// anew with its own moment as its removal. A block that commits while the approve is in flight
// fails the row at once: an approve that read nothing owed just before such a block committed may
// reach the broker after the block's moment, so its login could commit after a claim that settles
// the debt. The pass after the block lists the login the approve minted and revokes it, and leaves
// the debt owed until a claim comes machineLoginSettleAfter after the block.
//
// Controls: the block's machine-login statement taking no approval lock (pg_advisory_xact_lock
// dropped from its select), or that lock under another key than store.LockMachineLoginApprovals',
// and the block commits while the approve is in flight, named; the block keeping the debt's
// recorded_at, and the pass at once settles the debt, named.
func TestAnApproveInFlightDuringTheBrokerRestoredBlockIsRevoked(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	const list = "GET /v1/launcher-credentials"
	rig.bodyByRoute[list] = `{"credentials":[]}`
	for _, write := range []struct{ method, does string }{
		{http.MethodPut, "names erin on"}, {http.MethodDelete, "takes erin off"}, {http.MethodPut, "puts erin back on"},
	} {
		if write.does == "puts erin back on" {
			removalAged(t, database, machineLoginSettleAfter)
			registered.revokeBrokerDebts(ctx)
		}
		if response := dispatchRequest(t, served, write.method, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("alice %s ACC's list: status=%d body=%s", write.does, response.Code, response.Body.String())
		}
	}
	revocations := store.NewMachineLoginRevocations(database.Pool)
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || owed {
		t.Fatalf("the revocation of erin's machine logins is owed %t (%v) before the restore, want it settled", owed, err)
	}
	// Each pass below runs as if the claim before it were past the retry window.
	claimable := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `update machine_login_revocations set tried_at = clock_timestamp() - make_interval(secs => $1)`, brokerRevocationRetry.Seconds()); err != nil {
			t.Fatal(err)
		}
	}

	commit, approved := approveHeldAtBroker(t, rig, served)

	block := runbookBlockAfter(t, brokerRestoredLead)
	if len(block.statements) < 2 || block.statements[0] != "begin" || block.statements[len(block.statements)-1] != "commit" {
		t.Fatalf("the runbook's block for a broker restored alone is %q; want one transaction", block.statements)
	}
	restored := make(chan error, 1)
	go func() {
		restored <- func() error {
			background := context.Background()
			tx, err := database.Pool.Begin(background)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(background) }()
			args := pgx.NamedArgs{"backup": time.Now().Add(-30 * time.Minute)}
			for _, statement := range block.statements[1 : len(block.statements)-1] {
				if _, err := tx.Exec(background, statement, args); err != nil {
					return err
				}
			}
			return tx.Commit(background)
		}()
	}()
	if early, answered := answeredOrWaiting(t, database, restored, "the runbook's block"); answered {
		t.Fatalf("the runbook's block ended (error %v) while erin's approve was in flight, want it to wait on erin's approval lock", early)
	}
	rig.bodyByRoute[list] = erinsLoginListed("cred-in-flight")
	commit()
	if code := <-approved; code != http.StatusOK {
		t.Fatalf("erin's approve in flight answered %d, want 200: it was admitted before the block", code)
	}
	if err := <-restored; err != nil {
		t.Fatalf("the runbook's block: %v", err)
	}

	claimable()
	registered.revokeBrokerDebts(ctx)
	if !slices.Contains(rig.revokes(), "POST /v1/launcher-credentials/cred-in-flight/revoke-by-approver") {
		t.Errorf("the broker was sent %v; want the login erin approved while the block ran revoked", rig.revokes())
	}
	if owed, _, err := revocations.Owing(ctx, accessErin); err != nil || !owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after the pass at once, want it owed until a claim comes %s after the block", owed, err, machineLoginSettleAfter)
	}
}

// TestASecondApproveOfThePersonWhileOneIsInFlightIsRefused: an approval Dispatch relays for a
// restricted member holds a database connection, with their approval lock, until the broker
// answers, so a burst of approves must not each hold one. erin's first browser approve of her own
// machine login reaches the broker, which has not answered it; her second approve while it is in
// flight is 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT at once and reaches the broker as no decision.
// Once the broker answers, the first approve is 200.
//
// Control: take the approval lock shared and waiting, as the head before this row did, and the
// second approve reaches the broker and is answered 200, named.
func TestASecondApproveOfThePersonWhileOneIsInFlightIsRefused(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, _, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const approve = "POST /v1/credential-requests/rec-erin/approve"
	answer, first := approveHeldAtBroker(t, rig, served)

	second := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), `"code":"MACHINE_LOGIN_APPROVAL_IN_FLIGHT"`) {
		t.Errorf("erin's second approve while her first is at the broker = %d %s, want 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT", second.Code, second.Body.String())
	}
	sent := 0
	for _, route := range rig.routes {
		if route == approve {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("the broker was sent %d approves of erin's, want the first alone", sent)
	}
	answer()
	if code := <-first; code != http.StatusOK {
		t.Errorf("erin's first approve answered %d once the broker answered, want 200", code)
	}
}

// TestARemovalWaitingOnAnApprovalHoldsUpNoOtherWriteOfTheProject: a removal of a restricted member
// waits for an approval of theirs that Dispatch is relaying, and must not hold the project's owner
// lock while it does, or every other write of the project's access waits on the broker too.
// erin's browser approve of her own machine login reaches the broker, which has not answered it;
// alice's removal of erin from ACC waits for it; alice's addition of carol to ACC's list meanwhile
// answers 200 without waiting. Once the broker answers, the removal is 200.
//
// Control: access.LockForAccessWrite taking the approval lock of the person it removes after the
// project's owner lock, and carol's addition waits behind the removal, named.
func TestARemovalWaitingOnAnApprovalHoldsUpNoOtherWriteOfTheProject(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	answer, approved := approveHeldAtBroker(t, rig, served)
	removed := make(chan int, 1)
	go func() {
		removed <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice").Code
	}()
	if early, answered := answeredOrWaiting(t, database, removed, "alice's removal of erin"); answered {
		t.Fatalf("alice's removal of erin answered %d while erin's approve was in flight, want it waiting on erin's approval lock", early)
	}

	added := make(chan int, 1)
	go func() {
		added <- dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessCarol, nil, "alice").Code
	}()
	addedCode := 0
	for giveUp := time.Now().Add(30 * time.Second); addedCode == 0; time.Sleep(20 * time.Millisecond) {
		select {
		case addedCode = <-added:
		default:
			if waiting := advisoryLockWaiters(t, database); waiting > 1 {
				t.Errorf("alice's addition of carol to ACC waits on a lock behind erin's removal (%d writes waiting), want it answered while the removal waits on erin's approval", waiting)
				answer()
				addedCode = <-added
			} else if time.Now().After(giveUp) {
				t.Fatalf("alice's addition of carol to ACC neither finished nor waited on a lock within 30 s")
			}
		}
	}
	if addedCode != http.StatusOK {
		t.Errorf("alice's addition of carol to ACC answered %d, want 200", addedCode)
	}
	answer()
	if code := <-approved; code != http.StatusOK {
		t.Errorf("erin's approve answered %d once the broker answered, want 200", code)
	}
	if code := <-removed; code != http.StatusOK {
		t.Errorf("alice's removal of erin answered %d once erin's approval ended, want 200", code)
	}
}

// TestARefusedRemovalHoldsUpNoApproveOfThePerson: a removal takes the approval lock of the person
// it takes off the list before the project's owner lock, and that person's approve does not wait
// for the lock: it is 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT while another transaction holds it. So
// a removal the caller may not make must take no lock before the caller is known to own the
// project, or anyone who sees the project could keep a person's approves refused by sending such
// removals. carol, a full member who sees ACC and does not own it, sends the removal of erin from
// ACC while another write of ACC's access holds its owner lock: she is answered 403
// PROJECT_OWNER_REQUIRED without waiting on a lock, and erin's approve of her own machine login
// meanwhile is 200.
//
// Control: ownerWrite taking access.LockForAccessWrite's locks before it reads the owner, as the
// head before this row did, and carol's removal holds erin's approval lock while it waits on ACC's
// owner lock, so erin's approve is 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT, named.
func TestARefusedRemovalHoldsUpNoApproveOfThePerson(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	// Another write of ACC's access, holding ACC's owner lock until the row releases it.
	other := holdOwnerLock(t, database, "ACC")

	removal := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		removal <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, accessCarol)
	}()
	// The removal is refused before it takes any lock, so its answer before any wait is this row's
	// own case.
	refused, answered := answeredOrWaiting(t, database, removal, "carol's removal of erin from ACC")
	approved := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin)
	if approved.Code != http.StatusOK {
		t.Errorf("erin's approve while carol's removal of her, which carol may not make, is in flight = %d %s, want 200", approved.Code, approved.Body.String())
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatalf("release ACC's owner lock: %v", err)
	}
	if !answered {
		refused = <-removal
	}
	wantRefusal(t, refused, http.StatusForbidden, "PROJECT_OWNER_REQUIRED", accessCarol, "DELETE erin from ACC's list")
}

// TestAnOwnerWhoLosesTheProjectBeforeTheLocksIsRefused: ownerWrite refuses a caller who does not own
// the project by a read that takes no lock (TestARefusedRemovalHoldsUpNoApproveOfThePerson), and
// that read only refuses: the owner it reads under the project's owner lock decides. alice, ACC's
// owner as the read with no lock finds her, sends the removal of erin from ACC while another write
// of ACC's access, holding ACC's owner lock, gives ACC to carol; the removal waits on that lock and,
// once the transfer commits, is 403 PROJECT_OWNER_REQUIRED, and erin stays on the list and owes
// nothing.
//
// Control: ownerWrite not checking the owner LockForAccessWrite read under the locks, and alice's
// removal answers 200 and takes erin off a project that is no longer hers, named.
func TestAnOwnerWhoLosesTheProjectBeforeTheLocksIsRefused(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	// Another write of ACC's access, which gives ACC to carol under ACC's owner lock and commits
	// when the row says.
	transfer := holdOwnerLock(t, database, "ACC")
	if _, err := transfer.Exec(ctx, `update projects set owner = $1 where key = 'ACC'`, accessCarol); err != nil {
		t.Fatalf("give ACC to carol: %v", err)
	}

	removal := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		removal <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice")
	}()
	if early, answered := answeredOrWaiting(t, database, removal, "alice's removal of erin"); answered {
		t.Fatalf("alice's removal of erin answered %d %s before the transfer committed, want it waiting on ACC's owner lock", early.Code, early.Body.String())
	}
	if err := transfer.Commit(ctx); err != nil {
		t.Fatalf("commit the transfer of ACC to carol: %v", err)
	}
	refused := <-removal
	if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"PROJECT_OWNER_REQUIRED"`) {
		t.Errorf("alice's removal of erin, ACC given to carol while it waited on ACC's owner lock = %d %s, want 403 PROJECT_OWNER_REQUIRED", refused.Code, refused.Body.String())
	}
	if people := readAccess(t, served, accessCarol).People; !slices.Contains(people, accessErin) {
		t.Errorf("ACC's list after alice's refused removal names %v, want erin still on it", people)
	}
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after alice's refused removal, want nothing owed", owed, err)
	}
}

// ownCreated has carol create project OWN, which she then owns, and whose list names nobody.
func ownCreated(t *testing.T, served http.Handler) {
	t.Helper()
	if created := dispatchRequest(t, served, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OWN", "name": "Carol's own project"}, accessCarol); created.Code != http.StatusCreated {
		t.Fatalf("carol creates OWN: status=%d body=%s", created.Code, created.Body.String())
	}
}

// TestARemovalOfAPersonTheListDoesNotNameHoldsUpNoApproveOfThePerson: a removal takes the approval
// lock of the person it takes off only where it may owe the revocation of their machine logins, a
// read that takes no lock finding that the list names them and that they are a restricted member,
// since that person's approve does not wait for the lock: it is 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT
// while another transaction holds it. Any full member owns a project they create, so otherwise any
// of them could keep a person's approves refused by removing that person from their own project's
// list, which records nothing. carol creates OWN, whose list does not name erin, and sends the
// removal of erin from OWN while another write of OWN's access holds its owner lock: the removal
// waits on that lock, erin's approve of her own machine login meanwhile is 200, and the removal
// then answers 200 and owes nothing.
//
// Control: access.LockForAccessWrite taking the approval lock of the person a removal takes off
// whatever that read finds, as the head before this row did, and carol's removal holds erin's
// approval lock while it waits on OWN's owner lock, so erin's approve is 409
// MACHINE_LOGIN_APPROVAL_IN_FLIGHT, named.
func TestARemovalOfAPersonTheListDoesNotNameHoldsUpNoApproveOfThePerson(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	ownCreated(t, served)
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	// Another write of OWN's access, holding OWN's owner lock until the row releases it.
	other := holdOwnerLock(t, database, "OWN")

	removal := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		removal <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/OWN/access/people/"+accessErin, nil, accessCarol)
	}()
	if early, answered := answeredOrWaiting(t, database, removal, "carol's removal of erin from OWN"); answered {
		t.Fatalf("carol's removal of erin from OWN answered %d %s while another write held OWN's owner lock, want it waiting on that lock", early.Code, early.Body.String())
	}
	approved := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin)
	if approved.Code != http.StatusOK {
		t.Errorf("erin's approve while carol's removal of her from OWN, whose list does not name her, waits = %d %s, want 200", approved.Code, approved.Body.String())
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatalf("release OWN's owner lock: %v", err)
	}
	if removed := <-removal; removed.Code != http.StatusOK {
		t.Errorf("carol's removal of erin from OWN once its owner lock was released = %d %s, want 200", removed.Code, removed.Body.String())
	}
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after a removal from a list that does not name her, want nothing owed", owed, err)
	}
}

// TestARemovalThatFindsThePersonListedOnlyUnderItsLocksIsRefusedWhileTheirApproveIsInFlight: a
// removal whose read that takes no lock finds the person off the list takes no approval lock of
// theirs (TestARemovalOfAPersonTheListDoesNotNameHoldsUpNoApproveOfThePerson); but an addition
// that commits while it waits on the project's owner lock puts them on it, and the removal, which
// decides by its reads under that lock, then takes them off and owes the revocation of their
// machine logins. Its record takes their approval lock without waiting
// (store.RecordMachineLoginRevocation), since the removal holds the project's lock: while an
// approve of theirs is in flight, the removal is 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT and changes
// nothing, and once the approve is answered the owner removes them again and owes it. erin's
// approve of her own machine login reaches the broker, which has not answered it; carol's addition
// of erin to OWN, in a transaction that holds OWN's owner lock, has not committed when carol's
// removal of erin from OWN is sent, and the removal waits on that lock; once the addition commits,
// the removal is 409, OWN's list names erin, and nothing is owed.
//
// Control: store.RecordMachineLoginRevocation taking no lock, and carol's removal records while
// erin's approve is in flight and answers 200, named.
func TestARemovalThatFindsThePersonListedOnlyUnderItsLocksIsRefusedWhileTheirApproveIsInFlight(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL,
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	ownCreated(t, served)
	release, approved := approveHeldAtBroker(t, rig, served)
	// carol's addition of erin to OWN, holding OWN's owner lock until the row commits it.
	addition := holdOwnerLock(t, database, "OWN")
	if _, _, err := access.AddPrincipal(ctx, addition, events.NewBroker(), "OWN", access.PrincipalPerson, accessErin, model.Actor{Kind: "user", ID: accessCarol}); err != nil {
		t.Fatalf("carol's addition of erin to OWN: %v", err)
	}
	remove := func() <-chan *httptest.ResponseRecorder {
		removal := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			removal <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/OWN/access/people/"+accessErin, nil, accessCarol)
		}()
		return removal
	}
	owing := func() bool {
		t.Helper()
		owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin)
		if err != nil {
			t.Fatalf("read whether the revocation of erin's machine logins is owed: %v", err)
		}
		return owed
	}

	removal := remove()
	if early, answered := answeredOrWaiting(t, database, removal, "carol's removal of erin from OWN"); answered {
		t.Fatalf("carol's removal of erin from OWN answered %d %s before her addition of erin committed, want it waiting on OWN's owner lock", early.Code, early.Body.String())
	}
	if err := addition.Commit(ctx); err != nil {
		t.Fatalf("commit carol's addition of erin to OWN: %v", err)
	}
	var refused *httptest.ResponseRecorder
	select {
	case refused = <-removal:
	case <-time.After(30 * time.Second):
		t.Fatalf("carol's removal of erin from OWN did not answer within 30 s of the addition's commit, with erin's approve in flight: it waits on erin's approval lock under OWN's owner lock")
	}
	wantRefusal(t, refused, http.StatusConflict, "MACHINE_LOGIN_APPROVAL_IN_FLIGHT", accessCarol, "DELETE erin from OWN's list, which names her only under its locks, while her approve is in flight")
	// The record's try of the lock fails behind any holder of it: an approve in flight, another
	// owner's removal of the person or the runbook's restore block, so the answer names no one cause.
	if body := refused.Body.String(); !strings.Contains(body, "an approval or another removal of this person is in progress") {
		t.Errorf("carol's refused removal answers %s; want it to say an approval or another removal of this person is in progress", body)
	}
	listed := dispatchRequest(t, served, http.MethodGet, "/api/v1/projects/OWN/access", nil, accessCarol)
	if people := decodeBody[model.ProjectAccess](t, listed).People; !slices.Contains(people, accessErin) {
		t.Errorf("OWN's list after carol's refused removal names %v, want erin still on it", people)
	}
	if owing() {
		t.Errorf("the revocation of erin's machine logins is owed after carol's refused removal, want nothing owed")
	}

	release()
	if code := <-approved; code != http.StatusOK {
		t.Fatalf("erin's approve in flight answered %d, want 200", code)
	}
	if again := <-remove(); again.Code != http.StatusOK {
		t.Fatalf("carol's removal of erin from OWN once erin's approve was answered = %d %s, want 200", again.Code, again.Body.String())
	}
	if !owing() {
		t.Errorf("the revocation of erin's machine logins is not owed after carol took her off OWN, want it owed")
	}
}
