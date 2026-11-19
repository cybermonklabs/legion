// grant_chain_log_test.go holds the operator's line for each release that finds a grant's approval
// chain fails (GrantChainInvalidMessage): the session's release (at=values), the airlock proxy's
// read (at=airlock) and Create's reuse of a live grant (at=reuse), each written once per failed
// chain with the grant, its record, the enrollment, the place and the reason, and none for a
// chain that verifies or a database that failed. It also holds reuse's order: newest grant
// first, a grant whose chain fails passed over for the next.
//
// A grant whose chain fails is made as a broker of another BROKER_PUBLIC_URL that shares the
// database makes one: m.Audience is set to anotherBroker around the Create and the ApplyDecision,
// while m.Chain, which newFixture built for testAudience, still verifies for this broker, so the
// record's request object fails here with "aud does not match", as a moved session's grant does.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/logtest"
)

// approvedGrant is a live grant of names to enrollment enr, approved by approver, as the broker of
// audience makes one: the request object, its record and the approval are all made while m
// answers for audience, so a grant of anotherBroker carries a record signed for that address. m
// answers for testAudience again before it returns, whatever the calls answered.
func approvedGrant(t *testing.T, m *Machine, audience, enr string, key *ecdsa.PrivateKey, approver string, names ...string) (request Request, grantID string) {
	t.Helper()
	m.Audience = audience
	defer func() { m.Audience = testAudience }()
	req, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked of "+audience, names...), "", nil)
	if err != nil || req.State != "pending" || req.RecordID == nil {
		t.Fatalf("Create(%v) on %s = %+v, %v; want a pending request with a record", names, audience, req, err)
	}
	dec, err := m.ApplyDecision(context.Background(), *req.RecordID, true, approver)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(%s) on %s = %+v, %v; want a grant", *req.RecordID, audience, dec, err)
	}
	return req, dec.GrantID
}

// failedChain is what one GrantChainInvalidMessage line names: the grant, the record its chain
// rests on, the enrollment that holds it and the release that found it failing.
type failedChain struct {
	grant, record, enrollment, at string
}

// chainLines are the GrantChainInvalidMessage lines capture holds, each checked to be at WARN, as
// the attributes they were written with, in the order they were written.
func chainLines(t *testing.T, capture *logtest.Recorder) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range loggedLines(t, capture, GrantChainInvalidMessage) {
		if line.level != slog.LevelWarn {
			t.Errorf("%q was logged at %s, want WARN", GrantChainInvalidMessage, line.level)
		}
		lines = append(lines, line.attrs)
	}
	return lines
}

// requireChainLines fails t unless capture holds exactly the GrantChainInvalidMessage lines want,
// in that order, each carrying its four ids and a reason and no other key. The reason is the
// refusal's text: ErrGrantChainInvalid's, then the chain's own, which for a record signed for
// another broker address says "aud does not match".
func requireChainLines(t *testing.T, capture *logtest.Recorder, want ...failedChain) {
	t.Helper()
	got := chainLines(t, capture)
	if len(got) != len(want) {
		t.Fatalf("the %q lines are %v; want %d of them, for %v", GrantChainInvalidMessage, got, len(want), want)
	}
	for i, w := range want {
		reason, _ := got[i]["reason"].(string)
		if !strings.HasPrefix(reason, ErrGrantChainInvalid.Error()) || !strings.Contains(reason, "aud does not match") {
			t.Fatalf("line %d's reason is %q; want ErrGrantChainInvalid's text saying aud does not match", i, reason)
		}
		ids := map[string]any{"grant": w.grant, "record_id": w.record, "enrollment": w.enrollment, "at": w.at, "reason": reason}
		if !reflect.DeepEqual(got[i], ids) {
			t.Fatalf("line %d is %v; want %v", i, got[i], ids)
		}
	}
}

// requireNoneLogged fails t when any line capture holds, in any key, holds one of forbidden: a
// placeholder or a password the release handed out, or a part of a request object the test
// signed that no record holds, which requireNothingSecretLogged cannot read back.
func requireNoneLogged(t *testing.T, capture *logtest.Recorder, forbidden ...string) {
	t.Helper()
	text := loggedText(capture)
	for _, f := range forbidden {
		if f != "" && strings.Contains(text, f) {
			t.Fatalf("the log holds %q, a token or part of a request object:\n%s", f, text)
		}
	}
}

// objectParts are the header, the claims and the signature of each compact request object.
func objectParts(compacts ...string) []string {
	var parts []string
	for _, c := range compacts {
		parts = append(parts, strings.Split(c, ".")...)
	}
	return parts
}

// TestAReleaseOfAGrantWhoseChainFailsIsLogged: the session's release of a grant whose record is
// signed for another broker address is refused GRANT_CHAIN_INVALID, as before, and writes one
// WARN line naming the grant, its record, the enrollment, at=values and the refusal's own text.
func TestAReleaseOfAGrantWhoseChainFailsIsLogged(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, approver := newFixture(t)
	stale, grantID := approvedGrant(t, m, anotherBroker, enr, key, approver, "DEEL_API_KEY")

	release, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{})
	if !errors.Is(err, ErrGrantChainInvalid) || release.Values != nil {
		t.Fatalf("Values of a grant signed for %s = %v, %v; want ErrGrantChainInvalid and no value", anotherBroker, release.Values, err)
	}
	requireChainLines(t, capture, failedChain{grantID, *stale.RecordID, enr, "values"})
	if reason := chainLines(t, capture)[0]["reason"]; reason != err.Error() {
		t.Fatalf("the line's reason is %q; want the refusal Values answered, %q", reason, err.Error())
	}
	requireNothingSecretLogged(t, capture, m)
}

// TestReusingAGrantWhoseChainFailsIsLoggedAndAsksAfresh: an ask for the very names and lifetime
// of a live grant whose chain fails gets no such grant back, as before, and writes one WARN line
// at=reuse naming it; the session gets a new pending request of this broker instead.
func TestReusingAGrantWhoseChainFailsIsLoggedAndAsksAfresh(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, approver := newFixture(t)
	stale, grantID := approvedGrant(t, m, anotherBroker, enr, key, approver, "DEEL_API_KEY")

	again, err := m.Create(t.Context(), enr, signRequest(t, m, key, "asked again on this broker", "DEEL_API_KEY"), "", nil)
	if err != nil || again.ID == stale.ID || again.State != "pending" || again.RecordID == nil {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want a new pending request, not the grant of %s", again, err, stale.ID)
	}
	requireChainLines(t, capture, failedChain{grantID, *stale.RecordID, enr, "reuse"})
	requireNothingSecretLogged(t, capture, m)
}

// TestTheAirlocksReadOfAGrantWhoseChainFailsIsLogged: the airlock proxy's read of a grant whose
// chain fails is refused as the session's release is, and writes the same line at=airlock, the
// grant's own enrollment named, since the proxy proves no enrollment of its own.
func TestTheAirlocksReadOfAGrantWhoseChainFailsIsLogged(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("DEEL_API_KEY", "")}})
	stale, grantID := approvedGrant(t, m, anotherBroker, enr, key, fixtureOperator, "DEEL_API_KEY")

	values, version, err := m.AirlockValues(t.Context(), grantID)
	if !errors.Is(err, ErrGrantChainInvalid) || values != nil || version != "" {
		t.Fatalf("AirlockValues of a grant signed for %s = %v, %q, %v; want ErrGrantChainInvalid and no value", anotherBroker, values, version, err)
	}
	requireChainLines(t, capture, failedChain{grantID, *stale.RecordID, enr, "airlock"})
	if reason := chainLines(t, capture)[0]["reason"]; reason != err.Error() {
		t.Fatalf("the line's reason is %q; want the refusal AirlockValues answered, %q", reason, err.Error())
	}
	requireNothingSecretLogged(t, capture, m)
}

// TestAGrantOfThisBrokerLogsNoFailedChain: the control. A grant whose record is signed for this
// broker is released to the session and to the proxy, and handed back to the session's next ask
// for the same names, and none of the three writes the line.
func TestAGrantOfThisBrokerLogsNoFailedChain(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, _ := newPlaceholderFixture(t, airlock.Rules{Secrets: []airlock.SecretRule{rule("DEEL_API_KEY", "")}})
	own, grantID := approvedGrant(t, m, testAudience, enr, key, fixtureOperator, "DEEL_API_KEY")

	release, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{})
	if err != nil || release.Airlock == nil {
		t.Fatalf("Values = %+v, %v; want DEEL_API_KEY released behind the airlock", release, err)
	}
	values, _, err := m.AirlockValues(t.Context(), grantID)
	if err != nil || values["DEEL_API_KEY"].Slots[SlotValue] != "deel-v1" {
		t.Fatalf("AirlockValues = %v, %v; want DEEL_API_KEY's value", values, err)
	}
	compact := signRequest(t, m, key, "asked again", "DEEL_API_KEY")
	again, err := m.Create(t.Context(), enr, compact, "", nil)
	if err != nil || again.ID != own.ID || again.GrantID == nil || *again.GrantID != grantID {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want the grant %s handed back", again, err, grantID)
	}
	if lines := chainLines(t, capture); len(lines) != 0 {
		t.Fatalf("the %q lines are %v; want none for a grant of this broker", GrantChainInvalidMessage, lines)
	}
	requireNothingSecretLogged(t, capture, m)
	forbidden := []string{release.Airlock.ProxyPassword}
	for _, placeholder := range release.Airlock.Names["DEEL_API_KEY"].Slots {
		forbidden = append(forbidden, placeholder)
	}
	requireNoneLogged(t, capture, append(forbidden, objectParts(compact)...)...)
}

// TestADatabaseFailureInTheChainCheckLogsNoFailedChain: a chain check that cannot read the record
// is no failed chain. The release answers the database's error as it is, not
// ErrGrantChainInvalid, Create's reuse answers it too rather than asking afresh, and neither
// writes the line, though the grant is one whose chain would fail once the record was read.
func TestADatabaseFailureInTheChainCheckLogsNoFailedChain(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, approver := newFixture(t)
	_, grantID := approvedGrant(t, m, anotherBroker, enr, key, approver, "DEEL_API_KEY")
	unreachable := errors.New("read the record: the database is unreachable")
	chain := *m.Chain
	chain.FetchRecord = func(context.Context, string) (string, time.Time, bool, error) {
		return "", time.Time{}, false, unreachable
	}
	m.Chain = &chain

	if release, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{}); !errors.Is(err, unreachable) || errors.Is(err, ErrGrantChainInvalid) || release.Values != nil {
		t.Fatalf("Values while the record cannot be read = %v, %v; want that error and not ErrGrantChainInvalid", release.Values, err)
	}
	compact := signRequest(t, m, key, "asked again", "DEEL_API_KEY")
	if again, err := m.Create(t.Context(), enr, compact, "", nil); !errors.Is(err, unreachable) || errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Create(DEEL_API_KEY) again while the record cannot be read = %+v, %v; want that error and not ErrGrantChainInvalid", again, err)
	}
	if lines := chainLines(t, capture); len(lines) != 0 {
		t.Fatalf("the %q lines are %v; want none for a database failure", GrantChainInvalidMessage, lines)
	}
	requireNothingSecretLogged(t, capture, m)
	requireNoneLogged(t, capture, objectParts(compact)...)
}

// TestReuseTakesTheNewestGrantAndPassesOverOneWhoseChainFails: a session that moved from a broker
// of another address holds that broker's grant, G1, adopted from its earlier enrollment, and a
// newer grant of this broker, G2, for the same names and lifetime. Each ask hands back G2, with
// no new request and no line, since G2 is the newest and verifies. With G2 made the older of the
// two, each ask meets G1 first, passes it over with one line at=reuse, and still hands back G2.
func TestReuseTakesTheNewestGrantAndPassesOverOneWhoseChainFails(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, approver := newFixture(t)
	earlier, earlierKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	stale, staleGrant := approvedGrant(t, m, anotherBroker, earlier, earlierKey, approver, "DEEL_API_KEY")
	own, ownGrant := approvedGrant(t, m, testAudience, enr, key, approver, "DEEL_API_KEY")
	moveSession(t, m, earlier, enr)
	requests, _, records := heldRows(t, m)

	var compacts []string
	ask := func(round string) {
		t.Helper()
		compact := signRequest(t, m, key, "asked again", "DEEL_API_KEY")
		compacts = append(compacts, compact)
		again, err := m.Create(t.Context(), enr, compact, "", nil)
		if err != nil || again.ID != own.ID || again.GrantID == nil || *again.GrantID != ownGrant {
			t.Fatalf("Create(DEEL_API_KEY) %s = %+v, %v; want this broker's grant %s handed back", round, again, err, ownGrant)
		}
		if gotRequests, _, gotRecords := heldRows(t, m); gotRequests != requests || gotRecords != records {
			t.Fatalf("after Create %s the broker holds %d requests and %d records; want %d and %d, none added", round, gotRequests, gotRecords, requests, records)
		}
	}

	for _, round := range []string{"first", "second", "third"} {
		ask(round + " with G2 the newest")
	}
	requireChainLines(t, capture)

	if _, err := m.Store.Pool.Exec(t.Context(), `update grants set created_at = (select created_at from grants where id=$2) - interval '1 minute' where id=$1`, ownGrant, staleGrant); err != nil {
		t.Fatalf("make G2 older than G1: %v", err)
	}
	var want []failedChain
	for _, round := range []string{"first", "second", "third"} {
		ask(round + " with G1 the newest")
		want = append(want, failedChain{staleGrant, *stale.RecordID, enr, "reuse"})
		requireChainLines(t, capture, want...)
	}
	requireNothingSecretLogged(t, capture, m)
	requireNoneLogged(t, capture, objectParts(compacts...)...)
}
