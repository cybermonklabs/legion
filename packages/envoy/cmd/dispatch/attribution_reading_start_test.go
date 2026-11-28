package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// keyRemakeRows is how many texts rows each database of the remake test holds on the reading "old".
const keyRemakeRows = 2000

// keyRemakeWait is how long the remake test waits for the loop to remake every row. The control
// waits as long before it counts the rows that stay on the old reading.
const keyRemakeWait = 30 * time.Second

// keysRemade is the message of the line that the key remake loop writes after each remake.
const keysRemade = `msg="dispatch delivery: keys remade"`

// remadeEveryRow is the attribute of a remake line whose remake took every row of the test.
var remadeEveryRow = "rows=" + strconv.Itoa(keyRemakeRows)

// A process with no GitHub App key remakes the keys of each texts row on another reading. It does
// this at its start, and again after a change of the reading while it runs, with no restart. The
// delivery reconcile does not do this work: with no App key it is idle and records no pass.
//
// The test fails when main.go starts no KeyRemakeLoop, or starts it only with an App key or under
// the reconcile's lease. It fails when the loop checks only at its start, so that a change of the
// reading needs a restart. It also fails when the count route does not count the rows on another
// reading.
//
// Control in the same test: a process with DISPATCH_TEST_KEY_REMAKE_OFF=1 keeps every row on the
// old reading for 30 s. So no migration, boot step or route remakes the keys, and the route can
// count more than 0.
func TestDispatchRemakesTheKeysWithNoAppKeyAndNoRestart(t *testing.T) {
	t.Run("control: with DISPATCH_TEST_KEY_REMAKE_OFF=1 every row stays on the old reading", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		database := storetest.Open(t)
		storeTextsOnAnOldReading(t, ctx, database)
		process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
			Env: map[string]string{
				"DISPATCH_TEST_KEY_REMAKE_INTERVAL": "1s",
				"DISPATCH_TEST_KEY_REMAKE_OFF":      "1",
			},
		})
		process.waitHealthy(t)
		deadline := time.Now().Add(keyRemakeWait)
		for time.Now().Before(deadline) {
			if rows, _, old := textsReadings(t, ctx, database); rows != keyRemakeRows || old != keyRemakeRows {
				t.Fatalf("control: %d of %d texts rows on the reading old with no loop, want all %d:\n%s", old, rows, keyRemakeRows, process.Output.String())
			}
			select {
			case <-process.Exited:
				t.Fatalf("control: the process exited, so it proves nothing about the rows:\n%s", process.Output.String())
			case <-time.After(200 * time.Millisecond):
			}
		}
		if _, other := process.keysOnAnotherReading(t); other != keyRemakeRows {
			t.Errorf("control: keys_on_another_reading = %d after %v with no loop, want %d", other, keyRemakeWait, keyRemakeRows)
		}
		if remade := process.logLines(keysRemade); len(remade) > 0 {
			t.Errorf("control: the process remade keys with DISPATCH_TEST_KEY_REMAKE_OFF=1:\n%s", strings.Join(remade, "\n"))
		}
	})

	t.Run("the loop remakes the keys at the start and after a change of the reading", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		database := storetest.Open(t)
		storeTextsOnAnOldReading(t, ctx, database)
		// startDispatchProcessWith sets DISPATCH_TEST_HOOKS=1 and gives no App key. The reconcile keeps
		// its default interval.
		process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
			Env: map[string]string{"DISPATCH_TEST_KEY_REMAKE_INTERVAL": "1s"},
		})
		process.waitHealthy(t)
		// The reconcile's goroutine can log after the listener serves, so the test waits for the line.
		process.waitForLog(t, 10*time.Second, "dispatch delivery: no GitHub App key", "the reconcile is idle")

		// The start: the loop remakes each row that the database holds on the reading "old".
		atStart := process.waitForNoKeysOnAnotherReading(t, "at the start")
		if rows, current, _ := textsReadings(t, ctx, database); rows != keyRemakeRows || current != keyRemakeRows {
			t.Fatalf("%d of %d texts rows on the reading %s after the start, want all %d", current, rows, atStart, keyRemakeRows)
		}
		if remade := process.logLines(keysRemade, remadeEveryRow); len(remade) != 1 {
			t.Errorf("%d lines %s %s after the start, want 1:\n%s", len(remade), keysRemade, remadeEveryRow, process.Output.String())
		}
		if passes := reconcilePasses(t, ctx, database); passes != 0 {
			t.Errorf("%d duty_passes rows of the lease %s after the start, want 0: the reconcile ran", passes, delivery.ReconcileLease)
		}

		// The change: a migration replaces the reading while the process runs.
		before, after := replaceTheReading(t, ctx, database)
		if before != atStart {
			t.Fatalf("the database read the reading %s before the change, and the route read %s", before, atStart)
		}
		if changed := process.waitForNoKeysOnAnotherReading(t, "after the change"); changed != after {
			t.Errorf("the route reads the reading %s after the change, want %s", changed, after)
		}
		if rows, current, _ := textsReadings(t, ctx, database); rows != keyRemakeRows || current != keyRemakeRows {
			t.Fatalf("%d of %d texts rows on the reading %s after the change, want all %d", current, rows, after, keyRemakeRows)
		}
		if remade := process.logLines(keysRemade, remadeEveryRow); len(remade) != 2 {
			t.Errorf("%d lines %s %s after the change, want 2:\n%s", len(remade), keysRemade, remadeEveryRow, process.Output.String())
		}
		if passes := reconcilePasses(t, ctx, database); passes != 0 {
			t.Errorf("%d duty_passes rows of the lease %s after the change, want 0: the reconcile ran", passes, delivery.ReconcileLease)
		}

		// No restart: the process that remade the keys at its start remade them after the change.
		select {
		case <-process.Exited:
			t.Fatalf("the process (pid %d) exited:\n%s", process.Cmd.Process.Pid, process.Output.String())
		default:
		}
		if served := strings.Count(process.Output.String(), process.listeningLine()); served != 1 {
			t.Errorf("the process (pid %d) logged %d times that it serves, want 1:\n%s", process.Cmd.Process.Pid, served, process.Output.String())
		}
	})
}

// storeTextsOnAnOldReading stores keyRemakeRows merged results rows of acme/widgets, each with a
// texts row. Then it sets each texts row's reading to "old" with the triggers off, as a row that
// an earlier reading stored stands. It deletes the rows after the test's processes stop.
func storeTextsOnAnOldReading(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `insert into pull_request_heads (repo, number, head_sha, head_ref, state)
		select 'acme/widgets', n, 'aaaa', 'legion/ACME-' || n, 'merged' from pg_catalog.generate_series(1, $1::integer) n`, keyRemakeRows); err != nil {
		t.Fatalf("store the results rows: %v", err)
	}
	// A process starts after this cleanup is registered, so its kill runs first.
	t.Cleanup(func() {
		if _, err := database.Pool.Exec(context.Background(), `delete from pull_request_heads where repo = 'acme/widgets'`); err != nil {
			t.Errorf("delete the rows of the test: %v", err)
		}
	})
	if _, err := database.Pool.Exec(ctx, `insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body)
		select 'acme/widgets', n, 'aaaa', 'legion/ACME-' || n, 'acme/widgets', 'main', 'Change ' || n || ' for ACME-1', 'Follows #12.'
		from pg_catalog.generate_series(1, $1::integer) n`, keyRemakeRows); err != nil {
		t.Fatalf("store the texts rows: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `update pull_request_texts set reading = 'old'`); err != nil {
		t.Fatalf("put the texts rows on the reading old: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _, old := textsReadings(t, ctx, database); rows != keyRemakeRows || old != keyRemakeRows {
		t.Fatalf("%d of %d texts rows on the reading old after the store, want all %d", old, rows, keyRemakeRows)
	}
}

// textsReadings answers how many texts rows the database holds, how many are on the reading that
// runs now, and how many are on the reading "old".
func textsReadings(t *testing.T, ctx context.Context, database *store.Store) (rows, current, old int) {
	t.Helper()
	if err := database.Pool.QueryRow(ctx, `select count(*), count(*) filter (where reading = attribution_reading()),
		count(*) filter (where reading = 'old') from pull_request_texts`).Scan(&rows, &current, &old); err != nil {
		t.Fatalf("count the readings of the texts rows: %v", err)
	}
	return rows, current, old
}

// reconcilePasses is how many duty_passes rows the lease of the delivery reconcile holds: the row
// of each period it ran at, and the row that an earlier build keys by the lease name alone.
func reconcilePasses(t *testing.T, ctx context.Context, database *store.Store) int {
	t.Helper()
	var passes int
	if err := database.Pool.QueryRow(ctx, `select count(*) from duty_passes where lease = $1 or starts_with(lease, $1 || '/')`,
		delivery.ReconcileLease).Scan(&passes); err != nil {
		t.Fatalf("count the passes of the delivery reconcile: %v", err)
	}
	return passes
}

// replaceTheReading changes the body of attribution_keys as a migration does. In one transaction,
// it locks pull_request_texts and replaces the function. It answers the identity of the reading
// before and after the change, and fails the test when the identity stays the same.
func replaceTheReading(t *testing.T, ctx context.Context, database *store.Store) (before, after string) {
	t.Helper()
	var definition string
	if err := database.Pool.QueryRow(ctx, `select pg_catalog.pg_get_functiondef('attribution_keys(text, text, text, text, text[])'::pg_catalog.regprocedure),
		attribution_reading()`).Scan(&definition, &before); err != nil {
		t.Fatalf("read the definition of the reading: %v", err)
	}
	// A bare #n of two to six digits becomes a bare #n of two to five digits.
	const sixDigits, fiveDigits = "[0-9]{1,5}", "[0-9]{1,4}"
	if found := strings.Count(definition, sixDigits); found != 1 {
		t.Fatalf("the reading holds %d places of %q, want 1:\n%s", found, sixDigits, definition)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `lock table pull_request_texts in share row exclusive mode`); err != nil {
		t.Fatalf("lock the texts rows: %v", err)
	}
	if _, err := tx.Exec(ctx, strings.Replace(definition, sixDigits, fiveDigits, 1)); err != nil {
		t.Fatalf("replace attribution_keys: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the change of the reading: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select attribution_reading()`).Scan(&after); err != nil {
		t.Fatalf("read the identity of the reading after the change: %v", err)
	}
	if after == before {
		t.Fatalf("the identity of the reading stayed %s after a change of its body", before)
	}
	return before, after
}

// attributionKeyCounts is the part of the answer of GET /api/v1/delivery/attribution that the
// remake test reads. Each field is a pointer, so an answer without the field fails the test and
// does not read as 0.
type attributionKeyCounts struct {
	Reading              *string `json:"reading"`
	KeysOnAnotherReading *int    `json:"keys_on_another_reading"`
}

// keysOnAnotherReading reads GET /api/v1/delivery/attribution as the test's login. It answers the
// identity of the reading and the number of texts rows on another reading.
func (p *dispatchProcess) keysOnAnotherReading(t *testing.T) (reading string, other int) {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/delivery/attribution", "", http.StatusOK)
	var counts attributionKeyCounts
	if err := json.Unmarshal(answer, &counts); err != nil || counts.Reading == nil || counts.KeysOnAnotherReading == nil {
		t.Fatalf("GET /api/v1/delivery/attribution answered %s (%v), want reading and keys_on_another_reading", answer, err)
	}
	return *counts.Reading, *counts.KeysOnAnotherReading
}

// waitForNoKeysOnAnotherReading waits up to keyRemakeWait for the count route to count no texts
// row on another reading. It answers the identity of the reading that the route read last.
func (p *dispatchProcess) waitForNoKeysOnAnotherReading(t *testing.T, when string) string {
	t.Helper()
	var reading string
	other := -1
	if !testwait.Holds(keyRemakeWait, 200*time.Millisecond, func() bool {
		reading, other = p.keysOnAnotherReading(t)
		return other == 0
	}) {
		t.Fatalf("keys_on_another_reading = %d %s after %v, want 0:\n%s", other, when, keyRemakeWait, p.Output.String())
	}
	return reading
}
