package githubapp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestLimitRecordContextEndsABlockedWriteOnItsOwnDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		type valueKey struct{}
		caller, cancelCaller := context.WithCancel(context.WithValue(context.Background(), valueKey{}, "kept"))
		defer cancelCaller()
		started := time.Now()
		record, cancelRecord := limitRecordContext(caller)
		defer cancelRecord()
		ended := make(chan error, 1)
		go func() {
			<-record.Done() // A blocked ledger write gives up only when its real context ends.
			ended <- record.Err()
		}()

		cancelCaller()
		synctest.Wait()
		select {
		case err := <-ended:
			t.Fatalf("the caller's cancellation ended the blocked ledger write: %v", err)
		default:
		}
		if got := record.Value(valueKey{}); got != "kept" {
			t.Fatalf("the ledger context lost the caller's value: %v", got)
		}
		deadline, bounded := record.Deadline()
		if !bounded || deadline.Sub(started) != limitRecordTimeout {
			t.Fatalf("the ledger context's deadline is %v (bounded=%v), want exactly %s after %v", deadline, bounded, limitRecordTimeout, started)
		}

		time.Sleep(limitRecordTimeout - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-ended:
			t.Fatalf("the blocked ledger write ended before its %s budget: %v", limitRecordTimeout, err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-ended:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the blocked ledger write ended with %v, want DeadlineExceeded", err)
			}
		default:
			t.Fatal("the ledger context's own timeout did not end the blocked write")
		}
		if cause := context.Cause(record); !errors.Is(cause, context.DeadlineExceeded) {
			t.Fatalf("the ledger context's cause is %v, want DeadlineExceeded", cause)
		}
	})
}
