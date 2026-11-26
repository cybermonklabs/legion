package requests

import (
	"context"
	"crypto/ecdsa"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
)

// concurrentAsks has n asks for names for an hour from one enrollment go to Create at once, each
// signed beforehand, and answers the grant id of each, failing on an error or an ask granted no
// grant. Each ask is held after its first attempt's reuse read until all n have read
// (testBeforeCreateTx), so every run opens the window where two asks both find nothing to reuse,
// rather than a run the scheduler happens to order. A second attempt is not held.
func concurrentAsks(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, n int, names ...string) []string {
	t.Helper()
	objects := make([]string, n)
	for i := range objects {
		objects[i] = signRequestFor(t, m, key, 3600, names...)
	}
	var arrived sync.WaitGroup
	arrived.Add(n)
	released := make(chan struct{})
	go func() {
		arrived.Wait()
		close(released)
	}()
	m.testBeforeCreateTx = func(attempt int) {
		if attempt != 1 {
			return
		}
		arrived.Done()
		select {
		case <-released:
		case <-time.After(10 * time.Second):
		}
	}
	defer func() { m.testBeforeCreateTx = nil }()
	grants := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range objects {
		wg.Go(func() {
			<-start
			request, err := m.Create(context.Background(), enrollmentID, objects[i], "")
			switch {
			case err != nil:
				errs[i] = err
			case request.GrantID == nil:
				errs[i] = errNoGrant
			default:
				grants[i] = *request.GrantID
			}
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ask %d of %d: %v", i+1, n, err)
		}
	}
	return grants
}

var errNoGrant = errorString("the ask was granted no grant")

type errorString string

func (e errorString) Error() string { return string(e) }

// grantRows is how many grants rows enrollmentID holds.
func grantRows(t *testing.T, m *Machine, enrollmentID string) int {
	t.Helper()
	var n int
	if err := m.Store.Pool.QueryRow(context.Background(), `select count(*) from grants where enrollment_id=$1`, enrollmentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// concurrentRounds and concurrentAskers size the concurrency tests: each round is a fresh session
// whose asks are all held past their first reuse read (concurrentAsks), so each round opens the
// race, and a second grant in any round fails the test.
const (
	concurrentRounds = 3
	concurrentAskers = 8
)

// TestConcurrentAsksInTheSecondHalfGetOneNewGrant: eight asks for the same names at once, 31
// minutes into an automatic grant of an hour, get one new grant between them, in each of
// concurrentRounds rounds: Create reads the reuse again under the names' locks (reuseUnderLock), so each ask that
// waited on them finds the grant the first one wrote.
//
// The named revert: drop reuseUnderLock's call from Create, and a round writes more than one new
// grant.
func TestConcurrentAsksInTheSecondHalfGetOneNewGrant(t *testing.T) {
	for round := range concurrentRounds {
		m, enr, key, _ := newFixture(t)
		first := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
		age(t, m, *first.GrantID, 31*time.Minute)

		grants := concurrentAsks(t, m, enr, key, concurrentAskers, "AUTO_TOKEN")
		for _, id := range grants {
			if id != grants[0] || id == *first.GrantID {
				t.Fatalf("round %d: %d concurrent asks in the second half got grants %v; want one new grant, not %s, for all of them", round+1, concurrentAskers, grants, *first.GrantID)
			}
		}
		if rows := grantRows(t, m, enr); rows != 2 {
			t.Fatalf("round %d: the session holds %d grants; want 2, the first and the one new grant", round+1, rows)
		}
	}
}

// TestConcurrentFirstAsksGetOneGrant: eight first asks for the same names at once, with no live
// grant, get one grant between them, in each of concurrentRounds rounds.
//
// The named revert: drop reuseUnderLock's call from Create, and a round writes more than one grant.
func TestConcurrentFirstAsksGetOneGrant(t *testing.T) {
	for round := range concurrentRounds {
		m, enr, key, _ := newFixture(t)
		grants := concurrentAsks(t, m, enr, key, concurrentAskers, "AUTO_TOKEN")
		for _, id := range grants {
			if id != grants[0] {
				t.Fatalf("round %d: %d concurrent first asks got grants %v; want one grant for all of them", round+1, concurrentAskers, grants)
			}
		}
		if rows := grantRows(t, m, enr); rows != 1 {
			t.Fatalf("round %d: the session holds %d grants; want 1", round+1, rows)
		}
	}
}

// grantOfAnHour asks for names for 3600 seconds and answers the request, which must be granted
// at once.
func grantOfAnHour(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, names ...string) Request {
	t.Helper()
	request, err := m.Create(context.Background(), enrollmentID, signRequestFor(t, m, key, 3600, names...), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(%v, 3600 s) = %+v, %v; want a grant at once", names, request, err)
	}
	return request
}

// decidedBy is requests.decided_by of requestID, "" for none.
func decidedBy(t *testing.T, m *Machine, requestID string) string {
	t.Helper()
	var by *string
	if err := m.Store.Pool.QueryRow(context.Background(), `select decided_by from requests where id=$1`, requestID).Scan(&by); err != nil {
		t.Fatalf("read decided_by of %s: %v", requestID, err)
	}
	if by == nil {
		return ""
	}
	return *by
}

// TestAnAutomaticGrantIsHandedBackInItsFirstHalf: an automatic grant of an hour, nobody's
// decision, is handed back to an ask for the same names while at least half of its life is left,
// 29 minutes in.
//
// The named revert: drop inReuseWindow's half-life arm and this still passes; it is the control
// of the next test, which that revert fails.
func TestAnAutomaticGrantIsHandedBackInItsFirstHalf(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	first := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	if by := decidedBy(t, m, first.ID); by != "" {
		t.Fatalf("the automatic grant's request records decided_by %q; want none, which is what marks it automatic", by)
	}
	age(t, m, *first.GrantID, 29*time.Minute)
	again := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	if again.ID != first.ID || *again.GrantID != *first.GrantID {
		t.Fatalf("an ask 29 minutes into the hour = request %s grant %s; want request %s and grant %s handed back", again.ID, *again.GrantID, first.ID, *first.GrantID)
	}
}

// TestAnAutomaticGrantInItsSecondHalfGetsANewGrant: 31 minutes into an automatic grant of an
// hour, an ask for the same names gets a new grant of its own hour, and the old grant still
// releases until its own expiry, so a client renewing ahead of the expiry moves over with no gap.
//
// The named revert: drop inReuseWindow from reuseLiveGrant's query, and the ask gets the old
// grant back.
func TestAnAutomaticGrantInItsSecondHalfGetsANewGrant(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	first := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	age(t, m, *first.GrantID, 31*time.Minute)

	renewed := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	if renewed.ID == first.ID || *renewed.GrantID == *first.GrantID {
		t.Fatalf("an ask 31 minutes into the hour = request %s grant %s; want a new request and grant, not %s handed back", renewed.ID, *renewed.GrantID, *first.GrantID)
	}
	release, err := m.Values(ctx, *renewed.GrantID, enr, ReleaseCall{})
	if err != nil || release.Expires == nil || time.Until(*release.Expires) < 59*time.Minute {
		t.Fatalf("Values(the new grant) = expires %v, %v; want an hour of its own", release.Expires, err)
	}
	if _, err := m.Values(ctx, *first.GrantID, enr, ReleaseCall{}); err != nil {
		t.Fatalf("Values(the old grant) after the new one = %v; want it live until its own expiry", err)
	}
}

// TestTwoAsksInTheSecondHalfGetOneNewGrant: two asks for the same names in an automatic grant's
// second half get one new grant between them, not one each: the first makes it, and it is in its
// own first half when the second comes.
//
// The named revert: make inReuseWindow refuse every automatic grant, and the second ask gets a
// grant of its own.
func TestTwoAsksInTheSecondHalfGetOneNewGrant(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	first := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	age(t, m, *first.GrantID, 50*time.Minute)

	renewed := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	again := grantOfAnHour(t, m, enr, key, "AUTO_TOKEN")
	if renewed.ID == first.ID {
		t.Fatalf("the first ask 50 minutes into the hour got request %s back; want a new grant", first.ID)
	}
	if again.ID != renewed.ID || *again.GrantID != *renewed.GrantID {
		t.Fatalf("the second ask = request %s grant %s; want the new request %s and grant %s handed back", again.ID, *again.GrantID, renewed.ID, *renewed.GrantID)
	}
	var grants int
	if err := m.Store.Pool.QueryRow(context.Background(), `select count(*) from grants where enrollment_id=$1`, enr).Scan(&grants); err != nil || grants != 2 {
		t.Fatalf("the session holds %d grants, %v; want 2, the first and the one new grant", grants, err)
	}
}

// TestAPersonsGrantIsHandedBackInItsSecondHalf: a grant a person approved is handed back for as
// long as it lives, 50 minutes into its hour among them, with no second card for that person: a
// new ask would ask them again.
//
// The named revert: drop inReuseWindow's decided_by arm, and the ask makes a second pending card.
func TestAPersonsGrantIsHandedBackInItsSecondHalf(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	ctx := context.Background()
	pending, err := m.Create(ctx, enr, signRequestFor(t, m, key, 3600, "DEEL_API_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY, 3600 s) = %+v, %v; want pending", pending, err)
	}
	approved, err := m.ApplyDecision(ctx, *pending.RecordID, true, approver)
	if err != nil || approved.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", approved, err)
	}
	age(t, m, approved.GrantID, 50*time.Minute)

	again, err := m.Create(ctx, enr, signRequestFor(t, m, key, 3600, "DEEL_API_KEY"), "")
	if err != nil || again.ID != pending.ID || again.GrantID == nil || *again.GrantID != approved.GrantID {
		t.Fatalf("an ask 50 minutes into the approved hour = %+v, %v; want request %s and grant %s handed back", again, err, pending.ID, approved.GrantID)
	}
	var records int
	if err := m.Store.Pool.QueryRow(ctx, `select count(*) from credential_requests`).Scan(&records); err != nil || records != 1 {
		t.Fatalf("%d credential-request records, %v; want the one the person decided and no second card", records, err)
	}
}

// TestASessionLongGrantIsHandedBackAWeekOn: a grant that lasts until its session ends has no
// expiry, so no half of its life ever runs out, and it is handed back a week on.
func TestASessionLongGrantIsHandedBackAWeekOn(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	ctx := context.Background()
	sessionDefaultFixture(t, m)
	first, err := m.Create(ctx, enr, signRequest(t, m, key, "until the session ends", "AUTO_TOKEN"), "")
	if err != nil || first.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN, no lifetime) = %+v, %v; want a grant", first, err)
	}
	age(t, m, *first.GrantID, 7*24*time.Hour)
	again, err := m.Create(ctx, enr, signRequest(t, m, key, "until the session ends", "AUTO_TOKEN"), "")
	if err != nil || again.ID != first.ID {
		t.Fatalf("an ask a week into the session-long grant = %+v, %v; want request %s handed back", again, err, first.ID)
	}
}

// TestAnAskReadsAgainUnderTheLocksOnItsFirstAttemptAlone: an ask makes at most two attempts. Before
// each of its attempts' transactions, another ask writes a grant for the same names whose chain
// fails (a grant of anotherBroker, approvedGrant), which the read under the names' locks counts as
// new and reuseLiveGrant passes over. The first attempt reads again, finds that grant and writes
// nothing; the second does not read again, so it writes the ask's own pending request although a
// grant came meanwhile once more.
//
// The named revert: have every attempt read again under the locks, and the second attempt finds
// the second grant and writes nothing either, so the ask answers an error, or with a loop of
// attempts, makes more than two.
func TestAnAskReadsAgainUnderTheLocksOnItsFirstAttemptAlone(t *testing.T) {
	m, enr, key, approver := newFixture(t)
	var attempts []int
	nested := false
	m.testBeforeCreateTx = func(attempt int) {
		if nested {
			return
		}
		attempts = append(attempts, attempt)
		if len(attempts) > 5 {
			return // so that a run without the bound ends
		}
		nested = true
		approvedGrant(t, m, anotherBroker, enr, key, approver, "DEEL_API_KEY")
		nested = false
	}
	defer func() { m.testBeforeCreateTx = nil }()

	request, err := m.Create(context.Background(), enr, signRequest(t, m, key, "the ask whose attempts are counted", "DEEL_API_KEY"), "")
	if err != nil || request.State != "pending" || request.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) with a grant written before each attempt = %+v, %v; want the ask's own pending request", request, err)
	}
	if !slices.Equal(attempts, []int{1, 2}) {
		t.Fatalf("the ask made attempts %v; want [1 2], a second only after the first read again", attempts)
	}
}

// TestANarrowedAskKeepsItsProfileAndTargetOnItsSecondAttempt: an ask with a github_app field
// (CreateNarrowed) for the person's own App, OWN_APP, is served as OWN_APP_WRITE, the profile
// gitHubAppProfile chooses for a write narrowing, with the field's target. Before its first
// attempt's transaction the same ask, made again, writes a grant for that profile, which the read
// under the names' locks finds, so the first attempt writes nothing and the second hands that
// grant over: one grant, of the chosen profile and the field's target, recorded as narrowed.
//
// The named revert: give the second attempt no narrowing, and it serves the stored secret with
// the signed object's target, which no narrowed grant matches, so the ask is not handed the grant
// the first attempt saw written.
func TestANarrowedAskKeepsItsProfileAndTargetOnItsSecondAttempt(t *testing.T) {
	m, enr, key, _ := newFixture(t)
	local := fixtureStore(m)
	local.Put(policytest.TypeSecret("OWN_APP", fixtureOperator, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
	m.Policy = policytest.ProfiledCurrent(t, local, policytest.ProfilesFile(
		`{"name": "OWN_APP", "github": {"accounts": ["acme"], "permissions": {"contents": "read", "metadata": "read"}, "value": "installations"}}`,
		`{"name": "OWN_APP_WRITE", "source": "OWN_APP", "github": {"accounts": ["acme"], "permissions": {"contents": "write", "metadata": "read"}, "value": "installations"}}`,
	), fixtureService)
	m.Chain = NewChainVerifier(m.Store, testAudience, time.Minute, m.Policy)
	m.GitHubAppFieldSecrets = "OWN_APP"
	narrowing := GitHubAppNarrowing{Owner: "acme", Repositories: []string{"widgets"}}
	ctx := context.Background()
	ask := func() (Request, error) {
		return m.CreateNarrowed(ctx, enr, signRequestFor(t, m, key, 3600, "OWN_APP"), "", narrowing)
	}

	var attempts []int
	nested := false
	var meanwhile Request
	m.testBeforeCreateTx = func(attempt int) {
		if nested {
			return
		}
		attempts = append(attempts, attempt)
		if attempt != 1 {
			return
		}
		nested = true
		var err error
		if meanwhile, err = ask(); err != nil || meanwhile.GrantID == nil {
			t.Errorf("the ask written meanwhile = %+v, %v; want a grant", meanwhile, err)
		}
		nested = false
	}
	defer func() { m.testBeforeCreateTx = nil }()

	got, err := ask()
	if err != nil || got.GrantID == nil {
		t.Fatalf("CreateNarrowed(OWN_APP) with a grant written meanwhile = %+v, %v; want a grant", got, err)
	}
	// The second attempt hands the grant over at its reuse read, before its own transaction, so
	// the hook sees the first attempt alone.
	if !slices.Equal(attempts, []int{1}) {
		t.Fatalf("the ask's transactions began for attempts %v; want [1], the second handing over the grant written meanwhile", attempts)
	}
	if meanwhile.GrantID == nil || *got.GrantID != *meanwhile.GrantID {
		t.Fatalf("the ask was handed grant %v; want %v, the one written meanwhile", *got.GrantID, meanwhile.GrantID)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "OWN_APP_WRITE" {
		t.Fatalf("the ask's secrets = %+v; want OWN_APP_WRITE, the profile the field's write narrowing chooses", got.Secrets)
	}
	var narrowed bool
	var account string
	var repositories []string
	if err := m.Store.Pool.QueryRow(ctx, `select github_app_field, coalesce(target_account, ''), target_repositories from request_secrets where request_id=$1`, got.ID).
		Scan(&narrowed, &account, &repositories); err != nil {
		t.Fatal(err)
	}
	if !narrowed || account != "acme" || !slices.Equal(repositories, []string{"widgets"}) {
		t.Fatalf("the granted request records github_app_field %v and target %q %v; want true and acme [widgets]", narrowed, account, repositories)
	}
	if n := grantRows(t, m, enr); n != 1 {
		t.Fatalf("%d grants; want the one written meanwhile", n)
	}
}
