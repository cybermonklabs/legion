//go:build multiprocess

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// ageKeyLease sets the lease the row under key holds to age old on the database's clock, the
// clock a repeat judges it by against contracts.IdempotencyLease, and returns the moment that
// lease ends on that clock. Nothing renews that lease - the process that took it is dead - so the
// time it has left is wall clock and nothing about the take-over, which is what the row is for;
// the webhook redelivery row moves its claim's time the same way (backDateRedeliveryClaim).
func ageKeyLease(t *testing.T, database *store.Store, key string, age time.Duration) time.Time {
	t.Helper()
	var rows int
	var end time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		with aged as (
			update request_keys set leased_at = now() - make_interval(secs => $2)
			where idempotency_key = $1 and status is null and leased_at is not null
			returning leased_at
		)
		select count(*), coalesce(max(leased_at), now()) + make_interval(secs => $3) from aged
	`, key, age.Seconds(), contracts.IdempotencyLease.Seconds()).Scan(&rows, &end); err != nil {
		t.Fatalf("set the lease of %s %v old: %v", key, age, err)
	}
	if rows != 1 {
		t.Fatalf("set the lease of %s %v old: %d rows, want the one the dead process holds", key, age, rows)
	}
	return end
}

// leaseMargin is how far short of its end the row first sets the dead run's lease: leaseMargin
// before the end, where a repeat must still be refused, and then past the end, where the
// take-over must happen. It is the boundary's resolution: a lease judged at anything below
// contracts.IdempotencyLease-leaseMargin takes the key over at the first step and fails the row.
// A repeat that reaches its lease check more than leaseMargin after the lease was set, on a
// loaded machine, finds the lease ended and takes the key over rightly. The row tells that from a
// lease judged short by the database's clock read once the repeat has answered: before the
// lease's end, the take-over was early; at or past it, the run came too late to place the end,
// logs so, and goes on to the take-over's own checks.
const leaseMargin = 10 * time.Second

// INFRA-258. A process killed while it runs a keyed write leaves the write's key held under a lease
// nothing renews any more. The other process answers a repeat under that key 409 KEY_IN_FLIGHT while
// the lease lasts - the first run might still be alive - and, once the lease has lapsed with nothing
// committed, takes the key over and makes the write, once; a repeat after that is its replay. The
// lease is set rather than waited out (ageKeyLease), in two steps, so the row keeps the lease's
// end and not only what happens past it: to leaseMargin before the end, where the repeat must
// still be refused, and then past the end, where the take-over must come.
//
// The slow-write test hook holds A's run after its key is claimed and before its handler writes,
// which is where a process dies with nothing of the write committed; the kill is SIGKILL, so no
// shutdown drains it.
func TestAKeyedWriteAProcessDiedRunningIsTakenOverOnceItsLeaseLapses(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "KIL")
	pair.a.createProject(t, project, "Killed mid-write")
	issue, _ := pair.a.createIssueIn(t, project, "A write a process died running", "spec")
	path := "/api/v1/issues/" + issue + "/comments"
	const body = `{"body":"Made once, whoever makes it."}`
	pair.a.call(t, http.MethodPost, "/api/v1/writes/_test/slow",
		`{"method":"POST","path":"`+path+`","delay_ms":600000}`, http.StatusOK)

	first := make(chan error, 1)
	go func() {
		answer, err := pair.a.trySend(http.MethodPost, path, body, asKeyed(dispatchTestLogin, "killed-press"))
		if err == nil {
			err = errors.New("process A answered the held write: " + answer.String())
		}
		first <- err
	}()
	held := func() bool {
		var rows int
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select count(*) from request_keys where idempotency_key = 'killed-press' and status is null`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows == 1
	}
	testwait.Until(t, "process A to claim the key", func() (fmt.Stringer, bool) {
		return pair.a.Output, held()
	})
	if err := pair.a.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill process A: %v", err)
	}
	<-pair.a.Exited
	if err := <-first; err == nil {
		t.Fatal("the held write's client saw no failure when its process died")
	}

	repeat := func() httpAnswer {
		t.Helper()
		return pair.b.send(t, http.MethodPost, path, body, asKeyed(dispatchTestLogin, "killed-press"))
	}
	answer := repeat()
	if answer.status != http.StatusConflict || !json.Valid(answer.body) || answer.header.Get("Retry-After") != "1" {
		t.Fatalf("a repeat inside the dead run's lease: %v, want 409 %s with Retry-After: 1", answer, contracts.IdempotencyKeyInFlight)
	}
	if answer.code() != contracts.IdempotencyKeyInFlight {
		t.Fatalf("a repeat inside the dead run's lease: code %s, want %s", answer.code(), contracts.IdempotencyKeyInFlight)
	}
	// The refusal under a lease just taken is read; the rest of the lease is wall clock, since
	// nothing renews a lease whose process is gone, so it is set rather than waited out - first to
	// leaseMargin before its end, where the key is still the dead run's and a repeat must still be
	// refused. A lease judged shorter than it is lapses here, and the row says so; a repeat that
	// reached its check only once the end had passed on the database's clock took the key over
	// rightly and places nothing, which the row logs.
	end := ageKeyLease(t, pair.database, "killed-press", contracts.IdempotencyLease-leaseMargin)
	answer = repeat()
	if answer.status != http.StatusConflict || answer.code() != contracts.IdempotencyKeyInFlight {
		if checked := testdb.Now(t, pair.database.Pool); checked.Before(end) {
			t.Fatalf("a repeat %v before the dead run's lease ends: %v, want 409 %s: the key is still the dead run's "+
				"until its %v lease is out (the database's clock read %s once it answered, before the lease's end at %s)",
				leaseMargin, answer, contracts.IdempotencyKeyInFlight, contracts.IdempotencyLease,
				checked.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
		}
		t.Logf("the repeat sent %v before the dead run's lease ends answered %v once the lease had ended on the "+
			"database's clock, so this run does not place the lease's end", leaseMargin, answer)
	} else {
		// Then past the end, where the take-over must come.
		ageKeyLease(t, pair.database, "killed-press", contracts.IdempotencyLease+time.Second)
		testwait.Until(t, "process B to take the key over once the dead run's lease lapsed", func() (httpAnswer, bool) {
			answer = repeat()
			return answer, answer.status != http.StatusConflict
		})
	}
	if answer.status != http.StatusCreated || answer.header.Get(contracts.IdempotencyReplayedHeader) != "" {
		t.Fatalf("the repeat once the lease lapsed: %v replayed=%q, want the write made by process B", answer,
			answer.header.Get(contracts.IdempotencyReplayedHeader))
	}
	if answer = repeat(); answer.status != http.StatusCreated || answer.header.Get(contracts.IdempotencyReplayedHeader) != "?1" {
		t.Fatalf("a repeat after the takeover: %v, want the takeover's answer replayed", answer)
	}
	var comments int
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select count(*) from comments where issue_key = $1 and body = 'Made once, whoever makes it.'`, issue).Scan(&comments); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if comments != 1 {
		t.Fatalf("the issue holds %d comments, want the one process B made", comments)
	}
}
