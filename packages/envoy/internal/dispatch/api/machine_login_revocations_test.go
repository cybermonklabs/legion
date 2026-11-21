package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
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
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
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
// comes machineLoginSettleAfter after the removal.
//
// Control: record the revocation without taking the person's approval lock, and the removal commits
// while the approval is in flight; the first pass that follows lists no login, so the login the
// approval then mints is not among its revokes, named.
func TestALoginApprovedInFlightAsTheRemovalCommitsIsRevoked(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const (
		list    = "GET /v1/launcher-credentials"
		approve = "POST /v1/credential-requests/rec-erin/approve"
	)
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	rig.bodyByRoute[list] = `{"credentials":[]}`
	reached, release := make(chan struct{}), make(chan struct{})
	rig.beforeAnswer[approve] = func() {
		close(reached)
		<-release
	}
	approved := make(chan int, 1)
	go func() {
		approved <- dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin).Code
	}()
	<-reached
	removed := make(chan int, 1)
	go func() {
		removed <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice").Code
	}()
	waitingOnALock := func() bool {
		var n int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&n); err != nil {
			t.Fatalf("read whether the removal waits on a lock: %v", err)
		}
		return n > 0
	}
	removedEarly := 0
	var firstPass []string
	passed := false
	for giveUp := time.Now().Add(30 * time.Second); removedEarly == 0 && !waitingOnALock(); time.Sleep(20 * time.Millisecond) {
		select {
		case removedEarly = <-removed:
			// The removal did not wait: its first pass runs now, before the approval's login exists.
			registered.revokeBrokerDebts(ctx)
			firstPass, passed = rig.revokes(), true
		default:
			if time.Now().After(giveUp) {
				t.Fatalf("alice's removal of erin neither finished nor waited on a lock within 30 s")
			}
		}
	}
	rig.bodyByRoute[list] = `{"credentials":[{"credential_id":"cred-in-flight","host":"erin-desktop","service":null,"subject":null,"approved_by":"erin@other.example","issued_at":"` + time.Now().UTC().Format(time.RFC3339) + `","expires_at":"2026-12-20T00:00:00Z","expired":false}]}`
	close(release)
	if code := <-approved; code != http.StatusOK {
		t.Fatalf("erin's approve in flight answered %d, want 200: it was admitted before the removal", code)
	}
	if removedEarly == 0 {
		removedEarly = <-removed
	}
	if removedEarly != http.StatusOK {
		t.Fatalf("alice's removal of erin answered %d, want 200", removedEarly)
	}

	if !passed {
		// The removal's first pass, after its commit, which waited for the approval.
		registered.revokeBrokerDebts(ctx)
		firstPass = rig.revokes()
	}
	if !slices.Contains(firstPass, "POST /v1/launcher-credentials/cred-in-flight/revoke-by-approver") {
		t.Errorf("the first pass after the removal sent %v; want the login erin's approval in flight minted revoked", firstPass)
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
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
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
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const (
		list    = "GET /v1/launcher-credentials"
		approve = "POST /v1/credential-requests/rec-erin/approve"
	)
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	rig.bodyByRoute[list] = `{"credentials":[]}`
	reached, release := make(chan struct{}), make(chan struct{})
	rig.beforeAnswer[approve] = func() {
		close(reached)
		<-release
	}
	commit := sync.OnceFunc(func() { close(release) })
	t.Cleanup(commit)

	if approved := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin); approved.Code == http.StatusOK {
		t.Fatalf("erin's approve answered 200 while the broker held it, want Dispatch's call to give up: %s", approved.Body.String())
	}
	select {
	case <-reached:
	default:
		t.Fatalf("erin's approve never reached the broker; the broker was sent %v", rig.routes)
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
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
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
// anew with its own moment as its removal. The pass after it lists the login the approve minted and
// revokes it, and leaves the debt owed until a claim comes machineLoginSettleAfter after the block.
//
// Control: the block as it was, with no approval lock and recorded_at kept, and it commits while
// the approve is in flight; the pass that follows lists no login and settles the old removal's
// debt, so the login the approve then mints is never revoked, named.
func TestAnApproveInFlightDuringTheBrokerRestoredBlockIsRevoked(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	ctx := t.Context()
	const (
		list    = "GET /v1/launcher-credentials"
		approve = "POST /v1/credential-requests/rec-erin/approve"
	)
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

	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	reached, release := make(chan struct{}), make(chan struct{})
	rig.beforeAnswer[approve] = func() {
		close(reached)
		<-release
	}
	commit := sync.OnceFunc(func() { close(release) })
	t.Cleanup(commit)
	approved := make(chan int, 1)
	go func() {
		approved <- dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin).Code
	}()
	<-reached

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
	waitingOnALock := func() bool {
		var n int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&n); err != nil {
			t.Fatalf("read whether the block waits on a lock: %v", err)
		}
		return n > 0
	}
	finishedEarly, early := false, error(nil)
	for giveUp := time.Now().Add(30 * time.Second); !finishedEarly && !waitingOnALock(); time.Sleep(20 * time.Millisecond) {
		select {
		case early = <-restored:
			// The block did not wait: a pass now finds no login of erin's.
			finishedEarly = true
			claimable()
			registered.revokeBrokerDebts(ctx)
		default:
			if time.Now().After(giveUp) {
				t.Fatalf("the runbook's block neither finished nor waited on a lock within 30 s")
			}
		}
	}
	rig.bodyByRoute[list] = erinsLoginListed("cred-in-flight")
	commit()
	if code := <-approved; code != http.StatusOK {
		t.Fatalf("erin's approve in flight answered %d, want 200: it was admitted before the block", code)
	}
	if !finishedEarly {
		early = <-restored
	}
	if early != nil {
		t.Fatalf("the runbook's block: %v", early)
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
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const approve = "POST /v1/credential-requests/rec-erin/approve"
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	reached, release := make(chan struct{}), make(chan struct{})
	var arrivals atomic.Int32
	rig.beforeAnswer[approve] = func() {
		if arrivals.Add(1) == 1 {
			close(reached)
			<-release
		}
	}
	answer := sync.OnceFunc(func() { close(release) })
	t.Cleanup(answer)
	first := make(chan int, 1)
	go func() {
		first <- dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin).Code
	}()
	<-reached

	second := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), `"code":"MACHINE_LOGIN_APPROVAL_IN_FLIGHT"`) {
		t.Errorf("erin's second approve while her first is at the broker = %d %s, want 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT", second.Code, second.Body.String())
	}
	if got := arrivals.Load(); got != 1 {
		t.Errorf("the broker was sent %d approves of erin's, want the first alone", got)
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
// Control: take the person's approval lock after the project's owner lock (in the record alone), as
// the head before this row did, and carol's addition waits behind the removal, named.
func TestARemovalWaitingOnAnApprovalHoldsUpNoOtherWriteOfTheProject(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, _ := newRegisteredAccessServer(t, testServerOptions{
		admitsRestricted: true,
		agentSecretsURL:  rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	ctx := t.Context()
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	const approve = "POST /v1/credential-requests/rec-erin/approve"
	rig.bodyByRoute["POST /v1/machine-logins/lookup"] = erinsOwnLogin
	reached, release := make(chan struct{}), make(chan struct{})
	rig.beforeAnswer[approve] = func() {
		close(reached)
		<-release
	}
	answer := sync.OnceFunc(func() { close(release) })
	t.Cleanup(answer)
	approved := make(chan int, 1)
	go func() {
		approved <- dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-requests/rec-erin/approve", map[string]any{"code": "ABCD-1234"}, accessErin).Code
	}()
	<-reached
	lockWaiters := func() int {
		var n int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&n); err != nil {
			t.Fatalf("read how many writes wait on a lock: %v", err)
		}
		return n
	}
	removed := make(chan int, 1)
	go func() {
		removed <- dispatchRequest(t, served, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice").Code
	}()
	for giveUp := time.Now().Add(30 * time.Second); lockWaiters() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(giveUp) {
			t.Fatalf("alice's removal of erin did not wait on erin's approval within 30 s")
		}
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
			if waiting := lockWaiters(); waiting > 1 {
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
