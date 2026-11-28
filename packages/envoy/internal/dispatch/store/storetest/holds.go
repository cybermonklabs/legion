package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AdvisoryHold is one transaction's hold of a transaction advisory lock as SampleAdvisoryHolds saw
// it: the backend that held it, when that backend's transaction began, the first and the last
// sample that saw it held, how many samples did, and the statement the backend ran at the last.
type AdvisoryHold struct {
	PID         uint32
	XactStart   time.Time
	First, Last time.Time
	Samples     int
	Query       string
}

// Held is how long the samples saw the lock held: a lower bound of the hold, short of it by up to
// one sampling interval at each end, and zero for a hold one sample alone saw.
func (h AdvisoryHold) Held() time.Duration { return h.Last.Sub(h.First) }

// String names the hold as a test's log prints it.
func (h AdvisoryHold) String() string {
	return fmt.Sprintf("backend %d held it %s over %d samples, running %q", h.PID, h.Held().Round(time.Microsecond), h.Samples, h.Query)
}

// sampleEvery is how often SampleAdvisoryHolds reads pg_locks.
const sampleEvery = time.Millisecond

// SampleAdvisoryHolds starts reading, every millisecond on a connection of its own opened from
// pool's configuration, which session holds the two-key transaction advisory lock (key1, key2) in
// pool's database, the lock `pg_advisory_xact_lock(key1, key2)` takes, and answers stop, which ends
// the reading and answers each transaction that held the lock once, in the order it was first
// seen. An advisory lock is a lock of one database, so a session holding the same keys in another
// database of the server, another test's, holds nothing this one waits on and is not read. A
// transaction is one backend's one xact_start, so a backend of a pool that holds the lock in two
// transactions answers two holds. Each read is a statement of its own, so each reads
// pg_stat_activity afresh: a transaction's first read of it freezes the view for the rest of it.
// The connection is its own so that the code under test has every connection of its pool.
func SampleAdvisoryHolds(t testing.TB, pool *store.Pool, key1, key2 int32) (stop func() []AdvisoryHold) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		cancel()
		t.Fatalf("connect the lock sampler: %v", err)
	}
	// pg_locks and pg_stat_activity are two views read at slightly different moments, so a sample
	// can find the lock still granted to a backend whose activity already shows no transaction
	// once its commit has let the lock go. That row's xact_start is null, which pgx cannot scan
	// into a time.Time, and it is no hold: the hold it ended was read by the samples before it.
	query := fmt.Sprintf(`
		select l.pid, a.xact_start, coalesce(a.query, '')
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.locktype = 'advisory' and l.classid = %d and l.objid = %d and l.objsubid = 2 and l.granted
		  and l.database = (select oid from pg_database where datname = current_database())
		  and a.xact_start is not null
	`, key1, key2)
	type holder struct {
		pid   uint32
		start time.Time
	}
	done := make(chan struct{})
	var holds []AdvisoryHold
	var failure error
	go func() {
		defer close(done)
		index := map[holder]int{}
		ticker := time.NewTicker(sampleEvery)
		defer ticker.Stop()
		for {
			var pid uint32
			var start time.Time
			var statement string
			err := conn.QueryRow(ctx, query).Scan(&pid, &start, &statement)
			at := time.Now()
			switch {
			case err == nil:
				key := holder{pid: pid, start: start}
				if i, seen := index[key]; seen {
					holds[i].Last, holds[i].Query = at, statement
					holds[i].Samples++
				} else {
					index[key] = len(holds)
					holds = append(holds, AdvisoryHold{PID: pid, XactStart: start, First: at, Last: at, Samples: 1, Query: statement})
				}
			case errors.Is(err, pgx.ErrNoRows):
			case ctx.Err() != nil:
				return
			default:
				failure = err
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() []AdvisoryHold {
		t.Helper()
		cancel()
		<-done
		_ = conn.Close(context.Background())
		if failure != nil {
			t.Fatalf("sample the holders of the advisory lock (%d, %d): %v", key1, key2, failure)
		}
		return holds
	}
}
