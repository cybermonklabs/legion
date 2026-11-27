package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestLeaverHoldBrokerCredentials is the broker half of c-leaver-end-to-end. Removing the
// production hold gate from a new enrollment makes the first held assertion answer 201, so the
// red is a real broker still serving ada, not a fixture failure. Every remaining assertion names
// the production route whose gate must refuse her, while bob remains served.
func TestLeaverHoldBrokerCredentials(t *testing.T) {
	r := newLeaverRig(t, withBrokerGate())
	ada, bob := r.person(t, adaEmail), r.person(t, bobEmail)
	recordID, code := r.pendingMachineLogin(t, ada)

	r.refuse(adaEmail)
	r.ageConfirmation(t, adaEmail, confirmationAge)
	r.ageConfirmation(t, bobEmail, confirmationAge)
	// The broker asked Dispatch about ada and bob as the rig gave them their machine logins and
	// sessions, and keeps each not-held answer 30 seconds. They must lapse, or ada's rows below read
	// the answer from before Google refused her rather than ask Dispatch, which now holds her.
	r.pastBrokerCache(t)

	for _, row := range []struct {
		what   string
		status int
		call   func() (int, string)
	}{
		{"bob@example.com's refresh grant after ada is held", http.StatusOK, func() (int, string) { return r.refreshGrant(t, bob) }},
		{"bob@example.com's new enrollment after ada is held", http.StatusCreated, func() (int, string) { return r.enrol(t, bob) }},
		{"bob@example.com's lease renewal after ada is held", http.StatusOK, func() (int, string) { return r.renewLease(t, bob) }},
		{"bob@example.com's broker token mint after ada is held", http.StatusOK, func() (int, string) { return r.mintBrokerToken(t, bob) }},
		{"bob@example.com's broker token at Dispatch after ada is held", http.StatusOK, func() (int, string) { return r.brokerTokenAtDispatch(t, bob) }},
		{"bob@example.com's agent-tier secret request after ada is held", http.StatusOK, func() (int, string) { return r.askAgentSecret(t, bob) }},
		{"bob@example.com's agent-tier grant read after ada is held", http.StatusOK, func() (int, string) { return r.readAgentGrant(t, bob) }},
		{"bob@example.com's human-tier secret request after ada is held", http.StatusOK, func() (int, string) { return r.askHumanSecret(t, bob) }},
		{"bob@example.com's human-tier grant read after ada is held", http.StatusOK, func() (int, string) { return r.readHumanGrant(t, bob) }},
	} {
		expectAnswer(t, row.what, row.status, "").got(row.call())
	}
	expectAnswer(t, "ada@example.com's personal token, the first request after Google refuses her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, ada.dsp))
	expectAnswer(t, "ada@example.com's refresh grant while Dispatch holds her", http.StatusOK, "").got(r.refreshGrant(t, ada))
	expectAnswer(t, "ada@example.com's new enrollment after Dispatch holds her", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.enrol(t, ada))
	if t.Failed() {
		return
	}

	expectAnswer(t, "ada@example.com's lease renewal while Dispatch holds her", http.StatusOK, "").got(r.renewLease(t, ada))
	expectAnswer(t, "ada@example.com's broker token mint while Dispatch holds her", http.StatusBadRequest, oauthwire.MembershipHeld).got(r.mintBrokerToken(t, ada))
	expectAnswer(t, "ada@example.com's existing broker token at Dispatch while Dispatch holds her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.brokerTokenAtDispatch(t, ada))
	expectAnswer(t, "ada@example.com's agent-tier secret request while Dispatch holds her", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.askAgentSecret(t, ada))
	expectAnswer(t, "ada@example.com's agent-tier grant read while Dispatch holds her", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.readAgentGrant(t, ada))
	expectAnswer(t, "ada@example.com's human-tier secret request while Dispatch holds her", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.askHumanSecret(t, ada))
	expectAnswer(t, "ada@example.com's human-tier grant read while Dispatch holds her", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.readHumanGrant(t, ada))
	decision := bearerRequest(r.handler, http.MethodPost, "/api/v1/credential-requests/"+recordID+"/approve",
		leaverBody(t, map[string]string{"code": code}), ada.dsp.plaintext)
	expectAnswer(t, "ada@example.com's pending machine-login approval while Dispatch holds her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(dispatchAnswer(decision))
}

// TestLeaverHoldRelease proves the hold revokes neither Dispatch nor broker state. Removing the
// production release on a fresh Google sign-in leaves the first post-release credential refused;
// deleting or replacing a stored credential changes one of the five independent row snapshots.
func TestLeaverHoldRelease(t *testing.T) {
	t.Parallel()
	r := newLeaverRig(t, withBrokerGate(), withPersonSignIns())
	ada := r.person(t, adaEmail)
	r.personSignIn(t, ada)
	before := snapshotLeaverCredentials(t, r)

	r.refuse(adaEmail)
	r.ageConfirmation(t, adaEmail, confirmationAge)
	// The broker asked Dispatch about ada at her sign-in trade above and keeps that not-held answer
	// 30 seconds. It must lapse for her enrollment below to ask Dispatch, which holds her once her
	// personal token's request has had Google refuse her refresh.
	r.pastBrokerCache(t)
	expectAnswer(t, "ada@example.com's personal token, the first request after Google refuses her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, ada.dsp))
	expectAnswer(t, "ada@example.com's new enrollment before she signs in again", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.enrol(t, ada))
	if t.Failed() {
		return
	}

	ageSignInCredential(t, r, ada)
	r.signInAgain(t, adaEmail)
	// R4: her credentials are served again within 40 seconds of this sign-in: 30 of them the broker
	// keeping the held answer it read at her enrollment above, 10 Dispatch keeping the broker's
	// introspection answer. The broker's clock moves by its 30 seconds and no more, so a broker that
	// keeps that answer longer refuses her sign-in trade below, made at once, and the test fails
	// there; the wait after the trade gives Dispatch its 10 seconds on the wall clock.
	r.advanceBrokerClock(t, leaverSpecBrokerCache)
	r.personSignIn(t, ada)
	waitForLeaverCredentials(t, r, ada, leaverSpecIntrospection)
	expectLeaverSnapshot(t, before, snapshotLeaverCredentials(t, r))

	// A 25-hour outage (c-release): Google answers none of ada's refreshes, so Dispatch holds her by
	// the 24-hour rule. An outage is no refusal: it ends no sign-in and drops no refresh token, so the
	// hold ends with no sign-in once Google answers a refresh again.
	r.google.FailRefreshFor(adaEmail, http.StatusServiceUnavailable)
	r.ageConfirmation(t, adaEmail, 25*time.Hour)
	// The release wait above had the broker ask Dispatch about ada seconds ago, while she was served
	// again, and the broker keeps that answer 30 seconds. It must lapse for the enrollment below to
	// ask Dispatch, which holds her once its refresh finds Google out of reach.
	r.pastBrokerCache(t)
	expectAnswer(t, "ada@example.com's new enrollment while held for the 25-hour release", http.StatusForbidden, "MEMBERSHIP_HELD").got(r.enrol(t, ada))
	if t.Failed() {
		return
	}
	waitForOutageRelease(t, r, ada, 40*time.Second)
}

// TestLeaverHoldBound holds the membership pass off and polls the three broker-facing rows by
// the wall clock. Removing the production gate leaves the real introspection response unheld, so
// the first failure names the broker row rather than a timer or a fake. Dispatch refreshes ada
// through a code flow no sign-in has used (withRefreshFlowOfItsOwn), as a process started after
// her sign-in does, so a refused refresh that makes two token requests is seen: at eight seconds
// each it overruns the refresh's 15 seconds, Google reads as not reached, nothing is held, and no
// refresh starts for a minute, so the eight-second row fails past its bound.
//
// The eight-second row then takes bob, in the same Dispatch process: his confirmation is dated an
// hour and ten minutes back, with no request of his in between, Google waits eight seconds and
// refuses him, and each of his three broker-facing credentials is refused within 55 seconds of
// his first request (waitForFirstRefusals), which logs the second each was first refused at. His
// refresh starts at that request, so this is the spec's window from a refresh's start, which a
// broker that kept Dispatch's answer about him longer than it allows misses.
//
// Each row, and the test, runs in parallel with the other real-time tests of the leaver hold,
// which envoy-go runs in a step of their own (LEAVER_BOUND_TESTS in .github/scripts/ci_job_reads.py):
// each builds a rig of its own, its Postgres databases, its servers and its processes, and shares
// nothing with another.
func TestLeaverHoldBound(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		delay time.Duration
		bound time.Duration
		// thenBob is whether bob's step follows ada's.
		thenBob bool
	}{
		{name: "an eight-second Google refusal", delay: 8 * time.Second, bound: 120 * time.Second, thenBob: true},
		{name: "a half-second Google refusal", delay: 500 * time.Millisecond, bound: 40 * time.Second},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newLeaverRig(t, withBrokerGate(), withRefreshFlowOfItsOwn())
			// bob is set up before ada. The broker answers ada held only from an answer it asked
			// at least projectaccess.CacheTTL after her setup's ask, so by then its answer about
			// bob is older than that and it serves it no more: his first request below asks
			// Dispatch, which starts his refresh.
			var bob *leaverPerson
			if row.thenBob {
				bob = r.person(t, bobEmail)
			}
			ada := r.person(t, adaEmail)
			r.ageConfirmation(t, adaEmail, time.Hour-time.Second)
			expectAnswer(t, "ada@example.com's broker token at Dispatch before the confirmation lapses", http.StatusOK, "").got(r.brokerTokenAtDispatch(t, ada))
			status, held, code := r.introspection(t, ada)
			if status != http.StatusOK || held || code != "" {
				t.Fatalf("ada@example.com's broker introspection before the confirmation lapses answered %d held=%t %q, want 200 held=false", status, held, code)
			}
			expectAnswer(t, "ada@example.com's session route before the confirmation lapses", http.StatusOK, "").got(r.readAgentGrant(t, ada))
			if t.Failed() {
				return
			}

			// The fake Google waits row.delay and then refuses each refresh invalid_grant, under
			// oidc's 10-second token timeout, so only a refusal (never a confirmation) can end it.
			r.refuse(adaEmail)
			r.google.DelayToken(row.delay)
			r.ageConfirmation(t, adaEmail, time.Hour)
			waitForBrokerHold(t, r, ada, row.bound)
			if bob == nil {
				return
			}

			r.refuse(bobEmail)
			r.ageConfirmation(t, bobEmail, time.Hour+10*time.Minute)
			waitForFirstRefusals(t, r, bob, laterRefusalBound)
		})
	}
}

// snapshotLeaverCredentials reads exactly the persisted rows c-release protects. It removes only
// the use-time fields the contract permits to move, a refreshed login's expires_at included, and
// rejects an empty table, so a snapshot cannot pass because its query silently saw no fixture data.
func snapshotLeaverCredentials(t *testing.T, r *leaverRig) map[string][]string {
	t.Helper()
	snapshot := make(map[string][]string)
	for _, table := range []string{"launcher_credentials", "enrollments", "grants", "person_signins", "agent_tokens"} {
		excluded := ` - 'last_used_at'`
		switch table {
		case "launcher_credentials":
			excluded += ` - 'expires_at'`
		case "enrollments":
			excluded += ` - 'lease_expires_at'`
		}
		query := `select (to_jsonb(t)` + excluded + `)::text from ` + table + ` as t order by id`
		var rows pgx.Rows
		var err error
		if table == "agent_tokens" {
			rows, err = r.outside.database.Pool.Query(t.Context(), query)
		} else {
			rows, err = r.broker.Store.Pool.Query(t.Context(), query)
		}
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatalf("read snapshot row of %s: %v", table, err)
			}
			snapshot[table] = append(snapshot[table], row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("finish snapshot of %s: %v", table, err)
		}
		rows.Close()
		if len(snapshot[table]) == 0 {
			t.Fatalf("snapshot of %s has no rows, so it cannot prove a hold leaves that table unchanged", table)
		}
	}
	return snapshot
}

func expectLeaverSnapshot(t *testing.T, before, after map[string][]string) {
	t.Helper()
	for _, table := range []string{"launcher_credentials", "enrollments", "grants", "person_signins", "agent_tokens"} {
		if !slices.Equal(before[table], after[table]) {
			t.Errorf("%s after release = %v, want the pre-hold rows %v except their excluded use-time fields", table, after[table], before[table])
		}
	}
}

// ageSignInCredential simulates an over-hour hold without waiting on a wall clock. The later
// person trade must renew its real launcher credential rather than make a second sign-in row.
func ageSignInCredential(t *testing.T, r *leaverRig, p *leaverPerson) {
	t.Helper()
	tag, err := r.broker.Store.Pool.Exec(t.Context(), `update launcher_credentials set expires_at = now() - interval '2 hours' where id = $1`, p.signInCredential)
	if err != nil {
		t.Fatalf("age %s's sign-in credential: %v", p.email, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("age %s's sign-in credential changed %d rows, want 1", p.email, tag.RowsAffected())
	}
}

func waitForLeaverCredentials(t *testing.T, r *leaverRig, p *leaverPerson, bound time.Duration) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		failures := leaverCredentialFailures(t, r, p)
		if len(failures) == 0 {
			t.Logf("%s's credentials were served again within %s", p.email, bound)
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s's credentials were still not served after %s: %s", p.email, bound, failures)
			return
		}
		time.Sleep(time.Second)
	}
}

// leaverCredentialFailures is each of p's credentials that is not served, by name and answer:
// none when every one is.
func leaverCredentialFailures(t *testing.T, r *leaverRig, p *leaverPerson) []string {
	t.Helper()
	var failures []string
	for _, row := range []struct {
		name string
		call func() (int, string)
	}{
		{"cookie", func() (int, string) { return r.cookie(t, p) }},
		{"personal token", func() (int, string) { return r.personalToken(t, p.dsp) }},
		{"refresh grant", func() (int, string) { return r.refreshGrant(t, p) }},
		{"lease renewal", func() (int, string) { return r.renewLease(t, p) }},
		{"broker token mint", func() (int, string) { return r.mintBrokerToken(t, p) }},
		{"broker token at Dispatch", func() (int, string) { return r.brokerTokenAtDispatch(t, p) }},
		{"agent-tier secret request", func() (int, string) { return r.askAgentSecret(t, p) }},
		{"agent-tier grant read", func() (int, string) { return r.readAgentGrant(t, p) }},
		{"human-tier secret request", func() (int, string) { return r.askHumanSecret(t, p) }},
		{"human-tier grant read", func() (int, string) { return r.readHumanGrant(t, p) }},
	} {
		status, code := row.call()
		if status != http.StatusOK || code != "" {
			failures = append(failures, fmt.Sprintf("%s answered %d %q", row.name, status, code))
		}
	}
	return failures
}

// outageRefreshWait bounds how long waitForOutageRelease waits for Google to be asked a refresh of
// a person once an outage ends. That refresh waits out Standing's pause after the outage's last
// failed refresh (60 seconds), then a request that asks Dispatch, which the broker makes once its
// 30-second cache of Dispatch's answer lapses.
const outageRefreshWait = 2 * time.Minute

// waitForOutageRelease ends Google's outage for p and requires p's hold by the 24-hour rule to end,
// with no sign-in, within bound of the first refresh Google answers after the outage (c-release),
// polling p's enrollment through the broker each second on the wall clock. The bound runs from that
// refresh, which the fake Google's refresh count marks, all of whose refreshes it answers once the
// outage ends; not from the outage's end, because Standing's pause after the outage's last failed
// refresh delays it.
func waitForOutageRelease(t *testing.T, r *leaverRig, p *leaverPerson, bound time.Duration) {
	t.Helper()
	before := r.google.Refreshes()
	ended := time.Now()
	r.google.FailRefreshFor(p.email, 0)
	var answered time.Time
	noteAnswered := func() {
		if answered.IsZero() && r.google.Refreshes() > before {
			answered = time.Now()
			t.Logf("Google answered its first refresh of %s %s after the outage ended", p.email, answered.Sub(ended).Round(100*time.Millisecond))
		}
	}
	for enrolled := (time.Time{}); ; time.Sleep(100 * time.Millisecond) {
		noteAnswered()
		if time.Since(enrolled) >= time.Second {
			enrolled = time.Now()
			status, code := r.enrol(t, p)
			noteAnswered()
			switch {
			case status == http.StatusCreated && code == "" && answered.IsZero():
				t.Errorf("%s's 25-hour hold ended before Google answered any refresh after the outage", p.email)
				return
			case status == http.StatusCreated && code == "":
				t.Logf("%s's 25-hour hold ended, with no sign-in, %s after Google's first answered refresh", p.email, time.Since(answered).Round(100*time.Millisecond))
				return
			case status != http.StatusForbidden || code != "MEMBERSHIP_HELD":
				t.Errorf("%s's 25-hour hold answered %d %q, want 201 after release or 403 MEMBERSHIP_HELD while it waits", p.email, status, code)
				return
			}
		}
		switch {
		case answered.IsZero() && time.Since(ended) > outageRefreshWait:
			t.Errorf("Google was asked no refresh of %s within %s of the outage's end, so the release was never measured", p.email, outageRefreshWait)
			return
		case !answered.IsZero() && time.Since(answered) > bound:
			var confirmed bool
			if err := r.outside.database.Pool.QueryRow(t.Context(), `select coalesce(confirmed_at > $2, false) from people where email = $1`, p.email, ended).Scan(&confirmed); err != nil {
				t.Fatalf("read %s's confirmation: %v", p.email, err)
			}
			t.Errorf("%s's 25-hour hold did not end within %s of Google's first answered refresh after the outage (Dispatch recorded a confirmation since the outage ended: %t)", p.email, bound, confirmed)
			return
		}
	}
}

func waitForBrokerHold(t *testing.T, r *leaverRig, p *leaverPerson, bound time.Duration) {
	t.Helper()
	started := time.Now()
	deadline := started.Add(bound)
	for {
		last := ""
		if status, code := r.brokerTokenAtDispatch(t, p); status != http.StatusUnauthorized || code != "MEMBERSHIP_HELD" {
			last = fmt.Sprintf("%s's broker token at Dispatch answered %d %q, want 401 MEMBERSHIP_HELD", p.email, status, code)
		}
		status, held, code := r.introspection(t, p)
		if last == "" && (status != http.StatusOK || !held || code != "") {
			last = fmt.Sprintf("%s's broker introspection answered %d held=%t %q, want 200 held=true", p.email, status, held, code)
		}
		if status, code := r.readAgentGrant(t, p); last == "" && (status != http.StatusForbidden || code != "MEMBERSHIP_HELD") {
			last = fmt.Sprintf("%s's session route answered %d %q, want 403 MEMBERSHIP_HELD", p.email, status, code)
		}
		if last == "" {
			t.Logf("%s's broker rows were held in %s", p.email, time.Since(started))
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s within %s", last, bound)
			return
		}
		time.Sleep(time.Second)
	}
}

// laterRefusalBound is c-bound's bound for a person whose confirmation lapsed before their first
// request, in one Dispatch process: each of their broker-facing credentials is refused within it
// of that request, which starts their refresh. It is the spec's window from a refresh's start.
const laterRefusalBound = 55 * time.Second

// waitForFirstRefusals asks p's three broker-facing credentials each second, starting with p's
// first request, until each is refused: their broker token at Dispatch (401 MEMBERSHIP_HELD), the
// broker's introspection of it (held) and their session route at the broker (403
// MEMBERSHIP_HELD). It logs the second, from that first request, at which each was first refused,
// and fails t for one first refused past bound, one not refused once bound has passed, and one
// served again after it was refused.
func waitForFirstRefusals(t *testing.T, r *leaverRig, p *leaverPerson, bound time.Duration) {
	t.Helper()
	credentials := []struct {
		name string
		ask  func() (refused bool, answer string)
	}{
		{"broker token at Dispatch", func() (bool, string) {
			status, code := r.brokerTokenAtDispatch(t, p)
			return status == http.StatusUnauthorized && code == "MEMBERSHIP_HELD", fmt.Sprintf("%d %q, want 401 MEMBERSHIP_HELD", status, code)
		}},
		{"broker introspection", func() (bool, string) {
			status, held, code := r.introspection(t, p)
			return status == http.StatusOK && held && code == "", fmt.Sprintf("%d held=%t %q, want 200 held=true", status, held, code)
		}},
		{"session route", func() (bool, string) {
			status, code := r.readAgentGrant(t, p)
			return status == http.StatusForbidden && code == "MEMBERSHIP_HELD", fmt.Sprintf("%d %q, want 403 MEMBERSHIP_HELD", status, code)
		}},
	}
	first := time.Now()
	refusedAt := make([]time.Duration, len(credentials))
	for {
		served := ""
		for i, credential := range credentials {
			refused, answer := credential.ask()
			at := time.Since(first)
			switch {
			case refused && refusedAt[i] == 0:
				refusedAt[i] = at
				t.Logf("%s's %s was first refused %s after their first request", p.email, credential.name, at.Round(100*time.Millisecond))
				if at > bound {
					t.Errorf("%s's %s was first refused %s after their first request, past %s", p.email, credential.name, at.Round(100*time.Millisecond), bound)
				}
			case !refused && refusedAt[i] != 0:
				t.Errorf("%s's %s answered %s %s after their first request, after it was refused at %s", p.email, credential.name, answer, at.Round(100*time.Millisecond), refusedAt[i].Round(100*time.Millisecond))
				return
			case !refused && served == "":
				served = fmt.Sprintf("%s's %s answered %s", p.email, credential.name, answer)
			}
		}
		if served == "" {
			return
		}
		if time.Since(first) > bound {
			t.Errorf("%s, %s after their first request, past %s", served, time.Since(first).Round(100*time.Millisecond), bound)
			return
		}
		time.Sleep(time.Second)
	}
}
