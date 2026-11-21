// person_access_test.go drives GET /v1/person-access, the route Dispatch's server asks at a
// sign-out and in its membership pass whether a person has access at the broker that outlives
// their browser, on the same real broker api_test.go mounts.
package api_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// wirePersonAccess is GET /v1/person-access's answer, its fields spelled as Dispatch's broker
// client reads them.
type wirePersonAccess struct {
	Person          string `json:"person"`
	MachineLogins   int    `json:"machine_logins"`
	SignIns         int    `json:"signins"`
	ExchangeEntries int    `json:"exchange_entries"`
}

// personAccessBob is a person who holds nothing at the broker: no machine login, no sign-in and
// no exchange entry.
const personAccessBob = "bob@example.com"

// personAccessEntries is the exchange configuration the route counts in: four entries naming
// ada, one of them spelling her in capitals, one naming grace, and two naming nobody, a read
// entry and a Google entry that names its service alone.
func personAccessEntries(t *testing.T) *exchange.Set {
	t.Helper()
	set, err := exchange.NewSet(
		[]exchange.AccountEntry{
			{Account: "system:serviceaccount:example-ns:example-ada-worker", Project: ciphertest.ExchangeProject, Person: signInAda},
			{Account: "system:serviceaccount:example-ns:example-ada-daemon", Project: ciphertest.ExchangeProject, Person: signInAda, Session: "example-ada-daemon"},
			{Account: "system:serviceaccount:example-ns:example-grace-worker", Project: ciphertest.ExchangeProject, Person: signInGrace},
		},
		[]exchange.ReadEntry{{Account: "system:serviceaccount:example-ns:example-reader", Service: "example-reader", Session: "example-reader"}},
		[]exchange.WorkflowEntry{{
			Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main",
			Project: ciphertest.ExchangeProject, Person: "Ada@Example.com",
		}},
		[]exchange.GoogleEntry{
			{Account: signInFleetOne, Service: signInCredentialFleet, Project: ciphertest.ExchangeProject, Person: signInAda},
			{Account: signInFleetTwo, Service: "example-pool"},
		},
		"example.com",
	)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return set
}

// personAccess reads GET /v1/person-access?person=<person> as Dispatch's server, and fails t on
// anything but a 200 whose body is exactly the four fields Dispatch reads.
func (ts *testServer) personAccess(t *testing.T, person string) wirePersonAccess {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, "/v1/person-access?person="+url.QueryEscape(person), nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/person-access?person=%q = %d: %s", person, status, body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("GET /v1/person-access?person=%q answered %s, which is no JSON object: %v", person, body, err)
	}
	if got, want := slices.Sorted(maps.Keys(fields)), []string{"exchange_entries", "machine_logins", "person", "signins"}; !slices.Equal(got, want) {
		t.Fatalf("GET /v1/person-access?person=%q answered the fields %v, want %v: an absent count reads as 0 to Dispatch, which then forgets the person's token", person, got, want)
	}
	return decode[wirePersonAccess](t, body)
}

// TestPersonAccessCountsLiveCredentials: the route counts what a person has at the broker that
// outlives their browser. For ada that is her live machine login, her expired login whose box
// still renews, her retired login whose box still renews, her two sign-ins not revoked, and the
// four exchange entries that name her, one spelled in capitals. It leaves out her revoked login,
// her expired login with no live session, the service's login she approved, the credential her
// sign-in holds (a sign-in, not a machine login), her revoked sign-in, and every credential and
// entry of anyone else. Asked in capitals with space around, it answers the same counts for her
// address as Dispatch spells it. Grace's own are counted for her alone, bob holds nothing, a
// request naming no person is 400 PERSON_REQUIRED, and a listed caller without Dispatch's power
// is refused it.
func TestPersonAccessCountsLiveCredentials(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.Exchange = personAccessEntries(t)
		d.APIAccounts[listenerAccount] = ciphertest.Caller(t, listenerCaller)
		// Six machine logins come from one address and five from ada: the default buckets would
		// refuse the sixth, which is not what this test reads.
		d.LauncherLimits = &api.LauncherLimits{
			PerAddress: ratelimit.Limit{Every: time.Millisecond, Burst: 100},
			PerLogin:   ratelimit.Limit{Every: time.Millisecond, Burst: 100},
		}
	})

	// Counted: her live machine login.
	ts.mintLauncherCredential(t, signInAda, "example-host-live")
	// Counted: a login past its expiry whose box still renews.
	expiredBusy, expiredBusyKey := ts.mintLauncherCredential(t, signInAda, "example-host-expired-busy")
	ts.enrolledBox(t, expiredBusyKey, expiredBusy, "box-expired-busy-"+t.Name())
	// Not counted: a login past its expiry with no session.
	expiredIdle, _ := ts.mintLauncherCredential(t, signInAda, "example-host-expired-idle")
	ts.expireLauncherCredential(t, expiredBusy)
	ts.expireLauncherCredential(t, expiredIdle)
	// Counted: a login its machine retired after logging in again, whose box still renews.
	retired, retiredKey := ts.mintLauncherCredential(t, signInAda, "example-host-retired")
	ts.enrolledBox(t, retiredKey, retired, "box-retired-"+t.Name())
	if status, body := ts.launcher(t, retiredKey, retired, http.MethodPost, "/v1/operator/machines/"+retired+"/retire", nil); status != http.StatusNoContent {
		t.Fatalf("retire ada's login %s = %d %s, want 204", retired, status, body)
	}
	// Not counted: a revoked login.
	revoked, _ := ts.mintLauncherCredential(t, signInAda, "example-host-revoked")
	if status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+revoked+"/revoke-by-approver", map[string]any{"approver": signInAda}); status != http.StatusOK {
		t.Fatalf("revoke ada's login %s = %d %s, want 200", revoked, status, body)
	}
	// Not counted: a service's login she approved, and the credential her sign-in on box one holds.
	ts.mintOutstandingServiceCredential(t, signInAda)
	ts.signInCredential(t, signInAda, adaBoxOne, signInFleetOne)

	// Counted: her sign-ins on boxes one and two. Not counted: her sign-in on a third box, revoked.
	adaBoxThree := strings.Repeat("D", len(adaBoxOne))
	for _, box := range []string{adaBoxOne, adaBoxTwo, adaBoxThree} {
		ts.signIn(t, signInAda, box, signInFleetOne)
	}
	for _, row := range ts.personSignIns(t, signInAda) {
		if row.Thumbprint != adaBoxThree {
			continue
		}
		if status, body := ts.ui(t, http.MethodPost, "/v1/person-signins/"+row.SignInID+"/revoke", map[string]any{"person": signInAda}); status != http.StatusOK {
			t.Fatalf("revoke ada's sign-in on her third box = %d %s, want 200", status, body)
		}
	}

	// Grace's own: one machine login, one sign-in and one entry, none of them ada's.
	ts.mintLauncherCredential(t, signInGrace, "example-host-grace")
	ts.signIn(t, signInGrace, graceBox, signInFleetOne)

	for _, c := range []struct {
		asked string
		want  wirePersonAccess
	}{
		{signInAda, wirePersonAccess{Person: signInAda, MachineLogins: 3, SignIns: 2, ExchangeEntries: 4}},
		{" ADA@Example.com\t", wirePersonAccess{Person: signInAda, MachineLogins: 3, SignIns: 2, ExchangeEntries: 4}},
		{signInGrace, wirePersonAccess{Person: signInGrace, MachineLogins: 1, SignIns: 1, ExchangeEntries: 1}},
		{personAccessBob, wirePersonAccess{Person: personAccessBob}},
	} {
		if got := ts.personAccess(t, c.asked); got != c.want {
			t.Errorf("GET /v1/person-access?person=%q = %+v, want %+v", c.asked, got, c.want)
		} else {
			t.Logf("GET /v1/person-access?person=%q = %+v, as wanted", c.asked, got)
		}
	}

	for _, path := range []string{"/v1/person-access", "/v1/person-access?person=", "/v1/person-access?person=" + url.QueryEscape(" \t ")} {
		status, body := ts.ui(t, http.MethodGet, path, nil)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "PERSON_REQUIRED" {
			t.Errorf("GET %s = %d %s, want 400 PERSON_REQUIRED", path, status, body)
		}
	}

	listener := ts.accountToken(t, listenerAccount, "", time.Hour)
	status, body := ts.req(t, http.MethodGet, "/v1/person-access?person="+url.QueryEscape(signInAda), map[string]string{"Authorization": "Bearer " + listener}, nil)
	if status != http.StatusForbidden || decode[wireError](t, body).Code != "ACCOUNT_LACKS_POWER" {
		t.Errorf("GET /v1/person-access as the listener = %d %s, want 403 ACCOUNT_LACKS_POWER: what a person holds is Dispatch's to ask", status, body)
	}
}

// expireLauncherCredential moves launcher credential id's expiry into the past, as a week passing
// does: its sessions renew on, and it is live while one of them is.
func (ts *testServer) expireLauncherCredential(t *testing.T, id string) {
	t.Helper()
	if _, err := ts.Store.Pool.Exec(t.Context(), `update launcher_credentials set expires_at = now() - interval '1 minute' where id = $1`, id); err != nil {
		t.Fatalf("expire launcher credential %s: %v", id, err)
	}
}
