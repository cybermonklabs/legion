package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTwoRemovalsThatLeaveTheMemberOnNoListOweTheRevocation: alice and carol take erin off ACC's
// and OTH's lists at once, her last two. Each removal reads, after its own delete, whether a list
// still names her (removePerson), so with nothing ordering them each would read the other's row,
// not yet deleted by a commit, and neither would owe the revocation of her machine logins. Removals
// of one person take that person's lock in turn (access.LockPersonRemoval), so the second waits for
// the first's commit, reads both rows gone, and owes the revocation.
//
// Control: removePerson without the person's lock, and the second removal reads ACC's row the first
// has not committed, so nothing is owed, named.
func TestTwoRemovalsThatLeaveTheMemberOnNoListOweTheRevocation(t *testing.T) {
	rig := newFakeBrokerRig(t)
	served, database, registered := newRegisteredAccessServer(t, testServerOptions{
		signInOutside:   true,
		agentSecretsURL: rig.server.URL, agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
	ctx := t.Context()
	if created := dispatchRequest(t, served, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Another project"}, accessCarol); created.Code != http.StatusCreated {
		t.Fatalf("carol creates OTH: %d %s", created.Code, created.Body.String())
	}
	owners := map[string]string{"ACC": "alice", "OTH": accessCarol}
	for key, owner := range owners {
		if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/"+key+"/access/people/"+accessErin, nil, owner); listed.Code != http.StatusOK {
			t.Fatalf("%s names erin on %s's list: %d %s", owner, key, listed.Code, listed.Body.String())
		}
	}
	remove := func(tx pgx.Tx, key string) error {
		_, _, err := registered.removePerson(ctx, tx, registered.deps.Events, key, access.PrincipalPerson, accessErin, model.Actor{Kind: "user", ID: owners[key]})
		return err
	}
	waitingOnALock := func() bool {
		var n int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&n); err != nil {
			t.Fatalf("read whether a removal waits on a lock: %v", err)
		}
		return n > 0
	}

	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	if err := remove(first, "ACC"); err != nil {
		t.Fatalf("alice's removal of erin from ACC: %v", err)
	}
	second, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Rollback(ctx) }()
	done := make(chan error, 1)
	go func() { done <- remove(second, "OTH") }()
	// The second removal waits on the lock the first holds, or, with no lock, has already read.
	finished, secondErr := false, error(nil)
	for giveUp := time.Now().Add(30 * time.Second); !finished && !waitingOnALock(); time.Sleep(20 * time.Millisecond) {
		select {
		case secondErr = <-done:
			finished = true
		default:
			if time.Now().After(giveUp) {
				t.Fatalf("carol's removal of erin from OTH neither finished nor waited on a lock within 30 s")
			}
		}
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !finished {
		secondErr = <-done
	}
	if secondErr != nil {
		t.Fatalf("carol's removal of erin from OTH: %v", secondErr)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if owed, _, err := store.NewMachineLoginRevocations(database.Pool).Owing(ctx, accessErin); err != nil || !owed {
		t.Errorf("the revocation of erin's machine logins is owed %t (%v) after two removals at once took her off her last two lists, want it owed", owed, err)
	}
}

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
