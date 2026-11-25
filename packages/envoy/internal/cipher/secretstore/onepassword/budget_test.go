package onepassword_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/onepassword/onepasswordtest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The lines the budget writes.
const (
	heldLine    = "onepassword requests held"
	resumedLine = "onepassword requests resumed"
	passedLine  = "onepassword budget passed by value reads"
)

// budgetRig is a vault every replica of a deployment reads, one Postgres count they share, and
// one fake clock, at a daily budget of limit.
type budgetRig struct {
	clock *clock
	fake  *onepasswordtest.Fake
	// db is the broker's database as cipherd hands it to a store, and pool the test's own reads
	// of the count beside it.
	db    secretstore.Database
	pool  *pgxpool.Pool
	log   *logtest.Buffer
	limit int
	// titles and ids are the vault's items under testPrefix, in one order.
	titles, ids []string
}

// newBudgetRig holds ten owned items under testPrefix and one outside it, each call of the fake
// spending what budget.go's costs say.
func newBudgetRig(t *testing.T, limit int) *budgetRig {
	t.Helper()
	at := &clock{now: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
	st := storetest.Open(t)
	r := &budgetRig{
		clock: at,
		fake: &onepasswordtest.Fake{Now: at.Now, Cost: map[string]int{
			onepasswordtest.CallList: onepassword.ListRequests, onepasswordtest.CallGet: onepassword.GetRequests(1),
			onepasswordtest.CallCreate: onepassword.CreateRequests, onepasswordtest.CallPut: onepassword.PutRequests,
			onepasswordtest.CallArchive: onepassword.ArchiveRequests,
		}},
		db: st.SecretStoreDatabase(), pool: st.Pool, log: &logtest.Buffer{}, limit: limit,
	}
	for _, slug := range []string{"a-key", "b-key", "c-key", "d-key", "e-key", "f-key", "g-key", "h-key", "i-key", "j-key"} {
		item := r.fake.Hold(onepassword.Item{Title: testPrefix + slug, Credential: slug + "-value", Cipher: owned()})
		r.titles, r.ids = append(r.titles, item.Title), append(r.ids, item.ID)
	}
	r.fake.Hold(onepassword.Item{Title: "someone-elses-login", Credential: "v"})
	return r
}

// replica is one broker replica's store: its own kept items, the rig's budget, vault and clock.
func (r *budgetRig) replica() *onepassword.Store {
	budget := onepassword.NewBudget(r.db, r.limit, r.clock.Now, slog.New(slog.NewTextHandler(r.log, nil)))
	return onepassword.New(r.fake, testVault, budget, r.clock.Now)
}

// count is the requests the shared count holds.
func (r *budgetRig) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `select coalesce(sum(requests), 0)::bigint from onepassword_requests`).Scan(&n); err != nil {
		t.Fatalf("read the count: %v", err)
	}
	return n
}

// setCount makes the shared count n, all of it in the clock's hour.
func (r *budgetRig) setCount(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `delete from onepassword_requests`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `insert into onepassword_requests (hour, requests) values ($1, $2)`, r.clock.Now().Truncate(time.Hour), n); err != nil {
		t.Fatal(err)
	}
}

// dayStep is one call of runDay: what it was, and how it went.
type dayStep struct {
	what     string
	listings []secretstore.Listing
	value    string
	want     string
	err      error
}

// runDay runs one day from the rig's clock, in 30-second steps, on two replicas: each reloads (a
// List under testPrefix) every 5 minutes, the two 150 s apart, 288 times each; an item changes
// every 2 hours, 12 times; every hour each replica rereads a name (a Describe), as every replica
// does for one accepted reread, 24 rereads; and every 30 minutes one of them reads a value, 48
// reads. check sees each call.
func (r *budgetRig) runDay(t *testing.T, replicas [2]*onepassword.Store, check func(dayStep)) {
	t.Helper()
	ctx := context.Background()
	sources := map[string]string{}
	for at := 0; at < 24*60*60; at += 30 {
		switch {
		case at%300 == 0 || at%300 == 150:
			listings, err := replicas[at%300/150].List(ctx, testPrefix, secretstore.ReadOptions{})
			for _, ls := range listings {
				sources[ls.ID] = ls.Source
			}
			check(dayStep{what: "list", listings: listings, err: err})
		case (at-1020)%7200 == 0:
			i := (at - 1020) / 7200 % len(r.ids)
			r.fake.Hold(onepassword.Item{ID: r.ids[i], Title: r.titles[i], Credential: r.titles[i] + "-changed-at-" + r.clock.Now().Format(time.Kitchen), Cipher: owned()})
		case (at-2010)%3600 == 0:
			title := r.titles[(at-2010)/3600%len(r.titles)]
			for _, replica := range replicas {
				ls, err := replica.Describe(ctx, title, secretstore.ReadOptions{})
				check(dayStep{what: "describe " + title, listings: []secretstore.Listing{ls}, err: err})
			}
		case (at-420)%1800 == 0:
			k := (at - 420) / 1800
			i := k % len(r.ids)
			item, _ := r.fake.Item(r.ids[i])
			value, err := replicas[k%2].Read(ctx, sources[r.titles[i]])
			check(dayStep{what: "read " + r.titles[i], value: value, want: item.Credential, err: err})
		}
		r.clock.Add(30 * time.Second)
	}
}

// TestOneDayStaysInTheBudget (c-op-budget): two replicas over one vault and one count, at a
// budget of 900, run a day of reloads every 5 minutes each, with item changes, rereads and value
// reads: every call answers, every value read answers the item's current credential, no call
// holds (the test fails at the first that does), the day ends with the count under 900, and the
// count is every request the replicas made of 1Password.
func TestOneDayStaysInTheBudget(t *testing.T) {
	r := newBudgetRig(t, 900)
	r.runDay(t, [2]*onepassword.Store{r.replica(), r.replica()}, func(step dayStep) {
		if step.err != nil {
			t.Fatalf("%s: %v", step.what, step.err)
		}
		if step.value != step.want {
			t.Fatalf("%s answered %q, want %q", step.what, step.value, step.want)
		}
		if held := r.log.Lines(heldLine); held != "" {
			t.Fatalf("%s held, with the count at %d: %s", step.what, r.count(t), held)
		}
	})
	count := r.count(t)
	t.Logf("the day spent %d of the budget's 900 requests (the hold starts past 720): calls %v", count, r.fake.Calls())
	if count >= 900 {
		t.Errorf("the day spent %d requests, want fewer than 900", count)
	}
	if spent := r.fake.Requests(); count != spent {
		t.Errorf("the count holds %d requests, and the replicas made %d of 1Password", count, spent)
	}
}

// TestALowBudgetHoldsListsAndDescribesOnly: at a budget of 300 the same day holds: the hold is
// logged once; List and Describe answer their last results all the same, never empty; every value
// read gets the item's current credential; the count never passes 300. Once the window has
// room again, a List calls 1Password and the end of the hold is logged.
func TestALowBudgetHoldsListsAndDescribesOnly(t *testing.T) {
	r := newBudgetRig(t, 300)
	replicas := [2]*onepassword.Store{r.replica(), r.replica()}
	r.runDay(t, replicas, func(step dayStep) {
		if step.err != nil {
			t.Fatalf("%s: %v", step.what, step.err)
		}
		if strings.HasPrefix(step.what, "list") && len(step.listings) != len(r.titles) {
			t.Fatalf("a held List answered %d secrets, want its last result's %d", len(step.listings), len(r.titles))
		}
		if strings.HasPrefix(step.what, "describe") && (len(step.listings) != 1 || !strings.HasSuffix(step.what, step.listings[0].ID)) {
			t.Fatalf("%s answered %+v, want its last result", step.what, step.listings)
		}
		if step.value != step.want {
			t.Fatalf("%s answered %q, want %q", step.what, step.value, step.want)
		}
		if count := r.count(t); count > 300 {
			t.Fatalf("after %s the count is %d, past the budget of 300", step.what, count)
		}
	})
	if held := strings.Count(r.log.Lines(heldLine), "\n"); held != 2 {
		t.Errorf("the hold was logged %d times, want once on each replica:\n%s", held, r.log.String())
	}
	if !strings.Contains(r.log.Lines(heldLine), "reserve=60") {
		t.Errorf("the hold's line names no reserve of 60: %s", r.log.Lines(heldLine))
	}
	r.clock.Add(24 * time.Hour)
	before := r.fake.Calls()[onepasswordtest.CallList]
	if _, err := replicas[0].List(context.Background(), testPrefix, secretstore.ReadOptions{}); err != nil {
		t.Fatalf("List a day later: %v", err)
	}
	if after := r.fake.Calls()[onepasswordtest.CallList]; after != before+1 {
		t.Errorf("a List a day later made %d Lists of 1Password, want 1", after-before)
	}
	if resumed := r.log.Lines(resumedLine); resumed == "" {
		t.Errorf("no %q line once the window had room:\n%s", resumedLine, r.log.String())
	}
}

// TestAHeldListWithNoEarlierResultSpendsFromTheReserve: a replica that starts during a hold, and
// so holds no earlier result, spends its first List and its read of the items from the reserve and
// answers the vault's secrets; with the whole budget spent, a new replica's List and Describe
// answer ErrBudgetSpent, never an empty answer, while a replica that holds an earlier result
// answers it.
func TestAHeldListWithNoEarlierResultSpendsFromTheReserve(t *testing.T) {
	r := newBudgetRig(t, 300)
	ctx := context.Background()
	r.setCount(t, 250)
	started := r.replica()
	listings, err := started.List(ctx, testPrefix, secretstore.ReadOptions{})
	if err != nil || len(listings) != len(r.titles) {
		t.Fatalf("a first List during a hold = %d secrets, %v; want the vault's %d", len(listings), err, len(r.titles))
	}
	if got := r.count(t); got != 252 {
		t.Errorf("the first List counted %d requests, want 2 (its List and its Get) from the reserve", got-250)
	}
	r.setCount(t, 300)
	fresh := r.replica()
	if listings, err := fresh.List(ctx, testPrefix, secretstore.ReadOptions{}); !errors.Is(err, onepassword.ErrBudgetSpent) || listings != nil {
		t.Errorf("a first List with the budget spent = %v, %v; want ErrBudgetSpent and no listing", listings, err)
	}
	if ls, err := r.replica().Describe(ctx, r.titles[0], secretstore.ReadOptions{}); !errors.Is(err, onepassword.ErrBudgetSpent) {
		t.Errorf("a first Describe with the budget spent = %+v, %v; want ErrBudgetSpent", ls, err)
	}
	if listings, err := started.List(ctx, testPrefix, secretstore.ReadOptions{}); err != nil || len(listings) != len(r.titles) {
		t.Errorf("a List with the budget spent, on a replica with an earlier result = %d secrets, %v; want that result", len(listings), err)
	}
	if got := r.count(t); got != 300 {
		t.Errorf("the count is %d after the refused calls, want 300: a call the budget refuses counts nothing", got)
	}
}

// TestValueReadsPastTheReserveAreCountedAndLogged (Review focus 5): with the whole budget spent,
// value reads still call 1Password and answer, the count passes the budget by their requests
// alone, and the pass is logged once an hour.
func TestValueReadsPastTheReserveAreCountedAndLogged(t *testing.T) {
	r := newBudgetRig(t, 300)
	ctx := context.Background()
	replica := r.replica()
	listings, err := replica.List(ctx, testPrefix, secretstore.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r.setCount(t, 300)
	calls := r.fake.Requests()
	for i := range 3 {
		r.clock.Add(time.Minute)
		if value, err := replica.Read(ctx, listings[i].Source); err != nil || value == "" {
			t.Fatalf("a value read past the budget = %q, %v; want its value", value, err)
		}
	}
	if got, made := r.count(t), r.fake.Requests()-calls; made == 0 || got != 300+made {
		t.Errorf("the count is %d after value reads that made %d requests, want 300 and those", got, made)
	}
	if passed := strings.Count(r.log.Lines(passedLine), "\n"); passed != 1 {
		t.Errorf("%q was logged %d times within an hour, want once:\n%s", passedLine, passed, r.log.String())
	}
	if _, err := replica.List(ctx, testPrefix, secretstore.ReadOptions{}); err != nil {
		t.Fatalf("a held List: %v", err)
	}
	r.clock.Add(time.Hour)
	if _, err := replica.Read(ctx, listings[0].Source); err != nil {
		t.Fatal(err)
	}
	if passed := strings.Count(r.log.Lines(passedLine), "\n"); passed != 2 {
		t.Errorf("%q was logged %d times over two hours, want twice:\n%s", passedLine, passed, r.log.String())
	}
}
