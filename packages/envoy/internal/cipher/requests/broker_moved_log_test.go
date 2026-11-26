// broker_moved_log_test.go holds the operator's line for each pending request Create ends because
// its record is signed for another broker address (RequestEndedBrokerMovedMessage), and the
// grant-log line of the same end: each is written once per ended request, after the transaction
// that ended it has committed, names the request the session is referred to now, and holds no
// secret value and no request object. A request signed for this broker writes neither. The cases
// are broker_moved_test.go's, whose helpers these tests share.
package requests

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// brokerMovedLines are the RequestEndedBrokerMovedMessage lines capture holds, each checked to be
// at INFO, as the attributes they were written with.
func brokerMovedLines(t *testing.T, capture *logtest.Recorder) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range loggedLines(t, capture, RequestEndedBrokerMovedMessage) {
		if line.level != slog.LevelInfo {
			t.Errorf("%q was logged at %s, want INFO", RequestEndedBrokerMovedMessage, line.level)
		}
		lines = append(lines, line.attrs)
	}
	return lines
}

// brokerMovedLine is the attributes the line for ended request req carries when Create referred
// its session to newRequest: those four keys and no other.
func brokerMovedLine(req Request, newRequest, enrollmentID string) map[string]any {
	return map[string]any{"request": req.ID, "record_id": *req.RecordID, "new_request": newRequest, "enrollment": enrollmentID}
}

// requireBrokerMovedLines fails t unless capture holds exactly the RequestEndedBrokerMovedMessage
// lines want, in any order.
func requireBrokerMovedLines(t *testing.T, capture *logtest.Recorder, want ...map[string]any) {
	t.Helper()
	got := brokerMovedLines(t, capture)
	matched := make([]bool, len(got))
	for _, w := range want {
		found := false
		for i, g := range got {
			if !matched[i] && reflect.DeepEqual(g, w) {
				matched[i], found = true, true
				break
			}
		}
		if !found {
			t.Fatalf("the %q lines are %v; want exactly %v", RequestEndedBrokerMovedMessage, got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the %q lines are %v; want exactly %v", RequestEndedBrokerMovedMessage, got, want)
	}
}

// requireCancelledLine fails t unless the grant log holds one request.cancelled line for ended
// request req that reads as the broker's end of a request signed for another broker address:
// actor and decided_by the broker, BrokerMovedDetail as its detail and under reason in its raw
// detail, and every attribute the same as the route's event for the audit row it stands for.
func requireCancelledLine(t *testing.T, capture *logtest.Recorder, m *Machine, req Request, since time.Time) {
	t.Helper()
	line := lineFor(t, loggedLines(t, capture, store.GrantLogMessage), "request.cancelled", req.ID)
	raw, _ := line.attrs["raw"].(string)
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil || detail["reason"] != BrokerMovedDetail {
		t.Fatalf("the request.cancelled line's raw detail is %q (%v); want reason %q", raw, err, BrokerMovedDetail)
	}
	if line.attrs["actor"] != "broker" || line.attrs["decided_by"] != "broker" || line.attrs["state"] != "cancelled" || line.attrs["detail"] != BrokerMovedDetail {
		t.Fatalf("the request.cancelled line is %v; want actor and decided_by broker, state cancelled and detail %q", line.attrs, BrokerMovedDetail)
	}
	route := routeLine(t, capture, eventFor(t, readGrantLog(t, m, since), "request.cancelled", req.ID))
	for key, got := range line.attrs {
		if want, ok := route.attrs[key]; !ok || !sameAttr(got, want) {
			t.Fatalf("the line carries %s=%v and the route's event %v for the same audit row", key, got, want)
		}
	}
}

// requireNoCancelledLine fails t when the grant log holds any request.cancelled line.
func requireNoCancelledLine(t *testing.T, capture *logtest.Recorder) {
	t.Helper()
	for _, line := range loggedLines(t, capture, store.GrantLogMessage) {
		if line.attrs["kind"] == "request.cancelled" {
			t.Fatalf("the grant log holds a request.cancelled line, %v; want none", line.attrs)
		}
	}
}

// requireNothingSecretLogged fails t when any line capture holds, in any key, holds a value of a
// secret the fixture serves or any part of a request object one of the test's records embeds:
// its header, its claims or its signature.
func requireNothingSecretLogged(t *testing.T, capture *logtest.Recorder, m *Machine) {
	t.Helper()
	text := loggedText(capture)
	forbidden := []string{"stripe-v1"}
	for _, s := range fixtureSecrets() {
		forbidden = append(forbidden, s.Value)
	}
	rows, err := m.Store.Pool.Query(context.Background(), `select body from credential_requests`)
	if err != nil {
		t.Fatalf("read the records: %v", err)
	}
	defer rows.Close()
	objects := 0
	for rows.Next() {
		var canonical string
		if err := rows.Scan(&canonical); err != nil {
			t.Fatalf("read a record: %v", err)
		}
		body, err := record.ParseBody(canonical)
		if err != nil {
			t.Fatalf("parse a record: %v", err)
		}
		forbidden = append(forbidden, strings.Split(body.Request, ".")...)
		objects++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the records: %v", err)
	}
	if objects == 0 {
		t.Fatal("the test's records embed no request object to look for")
	}
	for _, f := range forbidden {
		if f != "" && strings.Contains(text, f) {
			t.Fatalf("the log holds %q, a secret's value or part of a request object:\n%s", f, text)
		}
	}
}

// TestTheEndOfARequestSignedForAnotherBrokerIsLogged: the ask that ends a request left waiting on
// another broker writes one INFO line naming the ended request, its record, the new request the
// session got and the enrollment, and the grant-log line of the request.cancelled row, whose raw
// detail holds the reason.
func TestTheEndOfARequestSignedForAnotherBrokerIsLogged(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture, m, enr, key, _ := newGrantLogFixture(t, "sess-moved")
	since := time.Now().Add(-time.Hour)
	stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")

	again, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked again on this broker", "DEEL_API_KEY"), "")
	if err != nil || again.State != "pending" || again.ID == stale.ID {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want a new pending request", again, err)
	}
	requireBrokerMovedLines(t, capture, brokerMovedLine(stale, again.ID, enr))
	requireCancelledLine(t, capture, m, stale, since)
	requireNothingSecretLogged(t, capture, m)
}

// TestEachStaleRequestEndedIsLoggedNamingTheOneNewRequest: an ask that ends two requests of
// another broker writes one line for each, both naming the one request it made.
func TestEachStaleRequestEndedIsLoggedNamingTheOneNewRequest(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture, m, enr, key, _ := newGrantLogFixture(t, "sess-moved-twice")
	since := time.Now().Add(-time.Hour)
	retagStripeKey(t, m)
	first := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
	earlier, earlierKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
	second := askOfAnotherBroker(t, m, earlier, earlierKey, stripeKey)
	moveSession(t, m, earlier, enr)

	again, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked again for both", "DEEL_API_KEY", stripeKey), "")
	if err != nil || again.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY, %s) = %+v, %v; want a new pending request", stripeKey, again, err)
	}
	requireBrokerMovedLines(t, capture, brokerMovedLine(first, again.ID, enr), brokerMovedLine(second, again.ID, enr))
	requireCancelledLine(t, capture, m, first, since)
	requireCancelledLine(t, capture, m, second, since)
	requireNothingSecretLogged(t, capture, m)
}

// TestTheEndBeforeARequestThatVerifiesNamesThatRequest: when the request left after the end is one
// of this broker, the line for the ended request names that one as new_request, the request the
// session is referred to, whether the ask coalesced onto it or was refused for it.
func TestTheEndBeforeARequestThatVerifiesNamesThatRequest(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	for _, c := range []struct {
		name    string
		refused bool
	}{
		{"coalesced", false},
		{"refused", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture, m, enr, key, _ := newGrantLogFixture(t, "sess-moved-"+c.name)
			ctx := context.Background()
			since := time.Now().Add(-time.Hour)
			stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
			earlier, earlierKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
			verifies, err := m.Create(ctx, earlier, signRequest(t, m, earlierKey, "asked of this broker", "DEEL_API_KEY"), "")
			if err != nil || verifies.State != "pending" {
				t.Fatalf("Create(DEEL_API_KEY) on the earlier box = %+v, %v; want a pending request", verifies, err)
			}
			moveSession(t, m, earlier, enr)

			ask := signRequest(t, m, key, "asked again", "DEEL_API_KEY")
			if c.refused {
				ask = signRequestFor(t, m, key, 600, "DEEL_API_KEY")
			}
			got, err := m.Create(ctx, enr, ask, "")
			if c.refused {
				if !errors.Is(err, ErrRequestPending) || !strings.Contains(err.Error(), verifies.ID) {
					t.Fatalf("Create for another lifetime = %+v, %v; want ErrRequestPending naming %s", got, err, verifies.ID)
				}
			} else if err != nil || !got.Coalesced || got.ID != verifies.ID {
				t.Fatalf("Create = %+v, %v; want it coalesced onto %s", got, err, verifies.ID)
			}
			requireBrokerMovedLines(t, capture, brokerMovedLine(stale, verifies.ID, enr))
			requireCancelledLine(t, capture, m, stale, since)
			requireNothingSecretLogged(t, capture, m)
		})
	}
}

// TestARequestSignedForThisBrokerLogsNoEnd: the control. An ask that coalesces onto a request of
// this broker, and one refused for it, end nothing and log no end.
func TestARequestSignedForThisBrokerLogsNoEnd(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture, m, enr, key, _ := newGrantLogFixture(t, "sess-not-moved")
	ctx := context.Background()
	own, err := m.Create(ctx, enr, signRequest(t, m, key, "asked of this broker", "DEEL_API_KEY"), "")
	if err != nil || own.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want a pending request", own, err)
	}
	if same, err := m.Create(ctx, enr, signRequest(t, m, key, "asked again", "DEEL_API_KEY"), ""); err != nil || !same.Coalesced {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want it coalesced onto %s", same, err, own.ID)
	}
	if req, err := m.Create(ctx, enr, signRequestFor(t, m, key, 600, "DEEL_API_KEY"), ""); !errors.Is(err, ErrRequestPending) {
		t.Fatalf("Create(DEEL_API_KEY) for another lifetime = %+v, %v; want ErrRequestPending", req, err)
	}
	requireBrokerMovedLines(t, capture)
	requireNoCancelledLine(t, capture)
}

// TestACreateWhoseCommitFailsLogsNoEnd: an end is logged once its commit has succeeded and never
// before, so a Create whose commit fails writes neither line, on each path that commits an end: a
// new request, a coalesce onto a request of this broker, and a refusal for one. The database fails
// the commit alone: a deferred constraint trigger, which Postgres runs at commit, refuses the
// request.cancelled audit row the end wrote, and the whole transaction rolls back with it.
func TestACreateWhoseCommitFailsLogsNoEnd(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	for _, c := range []struct {
		name string
		// verifies plants a request of this broker after the stale one, which the ask then
		// coalesces onto or is refused for.
		verifies bool
		lifetime int
	}{
		{"a new request", false, 0},
		{"a coalesce", true, 0},
		{"a refusal", true, 600},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture, m, enr, key, _ := newGrantLogFixture(t, "sess-commit-fails")
			ctx := context.Background()
			stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
			if c.verifies {
				earlier, earlierKey := newEnrollment(t, m.Store, "session", "box-b-"+t.Name(), new(fixtureOperator), nil)
				if verifies, err := m.Create(ctx, earlier, signRequest(t, m, earlierKey, "asked of this broker", "DEEL_API_KEY"), ""); err != nil || verifies.State != "pending" {
					t.Fatalf("Create(DEEL_API_KEY) on the earlier box = %+v, %v; want a pending request", verifies, err)
				}
				moveSession(t, m, earlier, enr)
			}
			if _, err := m.Store.Pool.Exec(ctx, `create function refuse_the_commit() returns trigger language plpgsql as
				$$ begin raise exception 'the test refuses this commit'; end $$`); err != nil {
				t.Fatalf("create the refusing function: %v", err)
			}
			if _, err := m.Store.Pool.Exec(ctx, `create constraint trigger refuse_the_commit after insert on audit
				deferrable initially deferred for each row when (new.kind = 'request.cancelled')
				execute function refuse_the_commit()`); err != nil {
				t.Fatalf("create the deferred trigger: %v", err)
			}
			before := len(capture.Records())

			ask := signRequest(t, m, key, "asked again", "DEEL_API_KEY")
			if c.lifetime != 0 {
				ask = signRequestFor(t, m, key, c.lifetime, "DEEL_API_KEY")
			}
			got, err := m.Create(ctx, enr, ask, "")
			if err == nil || errors.Is(err, ErrRequestPending) || !strings.Contains(err.Error(), "the test refuses this commit") {
				t.Fatalf("Create = %+v, %v; want the commit's own error", got, err)
			}
			requireBrokerMovedLines(t, capture)
			requireNoCancelledLine(t, capture)
			if after := capture.Records()[before:]; len(after) != 0 {
				t.Fatalf("a Create whose commit failed logged %d lines, the first %q; want none", len(after), after[0].Message)
			}
			requireStillPending(t, m, stale)
		})
	}
}

// TestARequestEndedWithNoAuditDetailLogsAnEmptyObject: an end that gives its audit row no detail
// of its own, as Cancel and the sweep's expiry do, leaves the column's default, {}, and its
// grant-log line carries that same {} as its raw detail, never null.
func TestARequestEndedWithNoAuditDetailLogsAnEmptyObject(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture, m, enr, key, _ := newGrantLogFixture(t, "sess-no-detail")
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	cancelled, err := m.Create(ctx, enr, signRequest(t, m, key, "never mind", "DEEL_API_KEY"), "")
	if err != nil || cancelled.State != "pending" {
		t.Fatalf("Create(DEEL_API_KEY) = %+v, %v; want a pending request", cancelled, err)
	}
	if err := m.Cancel(ctx, cancelled.ID, enr); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	expired, err := m.Create(ctx, enr, signRequest(t, m, key, "ask alice", "ALICE_KEY"), "")
	if err != nil || expired.State != "pending" {
		t.Fatalf("Create(ALICE_KEY) = %+v, %v; want a pending request", expired, err)
	}
	if _, err := m.Store.Pool.Exec(ctx, `update requests set pending_expires_at = now() - interval '1 minute' where id=$1`, expired.ID); err != nil {
		t.Fatalf("age the pending request: %v", err)
	}
	if ended, err := m.ExpirePending(ctx, time.Now()); err != nil || ended != 1 {
		t.Fatalf("ExpirePending = %d, %v; want the one aged request", ended, err)
	}

	lines := loggedLines(t, capture, store.GrantLogMessage)
	events := readGrantLog(t, m, since)
	for kind, id := range map[string]string{"request.cancelled": cancelled.ID, "request.expired": expired.ID} {
		line := lineFor(t, lines, kind, id)
		if line.attrs["raw"] != "{}" {
			t.Fatalf("the %s line's raw detail is %v; want {}", kind, line.attrs["raw"])
		}
		if route := eventFor(t, events, kind, id); string(route.Raw) != "{}" {
			t.Fatalf("the %s audit row's detail is %s; want {}", kind, route.Raw)
		}
	}
}

// TestTheEndAfterARequestThatVerifiesNamesThatRequest: R5 v9's fourth case with the stale request
// newer than the request that verifies. The line for the ended request names the request that
// verifies as new_request, whether the ask coalesced onto it or was refused for it.
func TestTheEndAfterARequestThatVerifiesNamesThatRequest(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	for _, c := range []struct {
		name    string
		refused bool
	}{
		{"coalesced", false},
		{"refused", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture, m, enr, key, _ := newGrantLogFixture(t, "sess-moved-after-"+c.name)
			since := time.Now().Add(-time.Hour)
			verifies, stale := staleAfterOneThatVerifies(t, m, enr, key)

			ask := signRequest(t, m, key, "asked again", "DEEL_API_KEY")
			if c.refused {
				ask = signRequestFor(t, m, key, 600, "DEEL_API_KEY")
			}
			got, err := m.Create(context.Background(), enr, ask, "")
			if c.refused {
				if !errors.Is(err, ErrRequestPending) || !strings.Contains(err.Error(), verifies.ID) {
					t.Fatalf("Create for another lifetime = %+v, %v; want ErrRequestPending naming %s", got, err, verifies.ID)
				}
			} else if err != nil || !got.Coalesced || got.ID != verifies.ID {
				t.Fatalf("Create = %+v, %v; want it coalesced onto %s", got, err, verifies.ID)
			}
			requireBrokerMovedLines(t, capture, brokerMovedLine(stale, verifies.ID, enr))
			requireCancelledLine(t, capture, m, stale, since)
			requireNothingSecretLogged(t, capture, m)
		})
	}
}

// TestTheEndOnAnAskDecidedAtOnceNamesTheRequestWrittenAtOnce: R5 v9's fifth case. The line for
// the stale request an ask decided at once ends names the request that ask wrote, granted or
// denied, as new_request.
func TestTheEndOnAnAskDecidedAtOnceNamesTheRequestWrittenAtOnce(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	for _, c := range askDecidedAtOnce {
		t.Run(c.name, func(t *testing.T) {
			capture, m, enr, key, _ := newGrantLogFixture(t, "sess-moved-at-once-"+c.name)
			since := time.Now().Add(-time.Hour)
			stale := askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")
			c.prepare(t, m)

			got, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked again on this broker", c.names...), "")
			if err != nil || got.State != c.state {
				t.Fatalf("Create(%v) again = %+v, %v; want a new request %s at once", c.names, got, err, c.state)
			}
			requireBrokerMovedLines(t, capture, brokerMovedLine(stale, got.ID, enr))
			requireCancelledLine(t, capture, m, stale, since)
			requireNothingSecretLogged(t, capture, m)
		})
	}
}

// TestTheEndOnAReusedGrantNamesItsRequest: R5 v9's sixth case. The line for the stale request an
// ask a live grant answers ends names the reused grant's request as new_request.
func TestTheEndOnAReusedGrantNamesItsRequest(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture, m, enr, key, approver := newGrantLogFixture(t, "sess-moved-reused")
	since := time.Now().Add(-time.Hour)
	granted, _, stale := liveGrantBesideAStaleRequest(t, m, enr, key, approver)

	got, err := m.Create(context.Background(), enr, signRequest(t, m, key, "asked again on this broker", "DEEL_API_KEY"), "")
	if err != nil || got.ID != granted.ID {
		t.Fatalf("Create(DEEL_API_KEY) again = %+v, %v; want the live grant of request %s handed back", got, err, granted.ID)
	}
	requireBrokerMovedLines(t, capture, brokerMovedLine(stale, granted.ID, enr))
	requireCancelledLine(t, capture, m, stale, since)
	requireNothingSecretLogged(t, capture, m)
}

// TestAnAskRefusedBeforeAnythingIsWrittenLogsNoEnd: R5 v9's seventh case. An ask refused before
// Create writes a request ends nothing, so it writes no end line and no request.cancelled line.
func TestAnAskRefusedBeforeAnythingIsWrittenLogsNoEnd(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.newGrantLogFixture, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	for _, c := range askRefusedBeforeAnyWrite {
		t.Run(c.name, func(t *testing.T) {
			capture, m, enr, key, _ := newGrantLogFixture(t, "sess-moved-refused-early")
			askOfAnotherBroker(t, m, enr, key, "DEEL_API_KEY")

			if got, err := m.Create(context.Background(), enr, c.ask(t, m, key), ""); err == nil {
				t.Fatalf("Create = %+v; want it refused", got)
			}
			requireBrokerMovedLines(t, capture)
			requireNoCancelledLine(t, capture)
		})
	}
}
