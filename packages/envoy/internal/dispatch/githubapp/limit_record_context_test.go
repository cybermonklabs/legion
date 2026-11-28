package githubapp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// A client built without WithRateLimitClock bounds the write of a limit to the shared ledger on
// the real clock, which the synctest bubble fakes, as a deployment's does.
func TestLimitRecordContextEndsABlockedWriteOnItsOwnDeadline(t *testing.T) {
	t.Parallel()
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, "")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	synctest.Test(t, func(t *testing.T) {
		type valueKey struct{}
		caller, cancelCaller := context.WithCancel(context.WithValue(context.Background(), valueKey{}, "kept"))
		defer cancelCaller()
		started := time.Now()
		record, cancelRecord := client.limitRecordContext(caller)
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
		if !bounded || deadline.Sub(started) != LimitRecordTimeout {
			t.Fatalf("the ledger context's deadline is %v (bounded=%v), want exactly %s after %v", deadline, bounded, LimitRecordTimeout, started)
		}

		time.Sleep(LimitRecordTimeout - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-ended:
			t.Fatalf("the blocked ledger write ended before its %s budget: %v", LimitRecordTimeout, err)
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
