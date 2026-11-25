package intake

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A handler's RetryLater that is Waiting is an expected wait, not a failure: the workflow returns
// one for a wake report that finds its claim in a release that has not ended. consumeMessage naks
// the delivery after the retry's own delay, so JetStream delivers it again then, and logs the nak
// at INFO. A retry that does not wait is still logged at WARN.
func TestAWaitingRetryIsNakedAfterItsDelayAtInfo(t *testing.T) {
	pool := migratedPool(t)
	at := strconv.FormatInt(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC).UnixMilli(), 10)
	for _, tc := range []struct {
		name    string
		waiting bool
		level   string
		line    string
	}{
		{name: "a waiting retry", waiting: true, level: "INFO", line: "an intake fact waits and is applied again after its delay"},
		{name: "a retry that does not wait", waiting: false, level: "WARN", line: "retryable intake processing failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			retry := &RetryLater{After: 30 * time.Second, Waiting: tc.waiting,
				Err: errors.New("the agent_message wake for session-1 finds INFRA-12's implementer in a release")}
			handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, retry })
			delivery := &wakeDelivery{data: envelopeJSON(t, "wake-"+tc.level, "envoy",
				`{"kind":"agent_message","session":"session-1","at":`+at+`}`)}

			if consumeWake(t, delivery, wakeSpec(logs), pool, handler) {
				t.Fatal("consumeMessage = true, want false: a naked delivery is not handled")
			}
			acks, delays, others := delivery.answers()
			if acks != 0 || len(delays) != 1 || delays[0] != 30*time.Second || len(others) != 0 {
				t.Fatalf("answers = %d acknowledgements, naks after %v, others %v; want one nak after 30s and nothing else", acks, delays, others)
			}

			levels := map[string][]string{}
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				for _, level := range []string{"INFO", "WARN", "ERROR"} {
					if strings.Contains(line, "level="+level+" ") {
						levels[level] = append(levels[level], line)
					}
				}
			}
			lines := levels[tc.level]
			if len(lines) != 1 || !strings.Contains(lines[0], tc.line) || !strings.Contains(lines[0], "retry_in=30s") ||
				!strings.Contains(lines[0], "in a release") {
				t.Fatalf("%s lines = %q, want one %q naming the delay and the error; log:\n%s", tc.level, lines, tc.line, logs.String())
			}
			if total := len(levels["INFO"]) + len(levels["WARN"]) + len(levels["ERROR"]); total != 1 {
				t.Fatalf("log = %s, want the one %s line and no other", logs.String(), tc.level)
			}
		})
	}
}

// A waiting retry is no failure, so its delay does not grow with the delivery count: at the
// seventh delivery it is still its own 30 s, and the fact is applied again as soon as the wait it
// names is over. The control is the same retry that does not wait, whose delay at that delivery is
// the doubled nak delay, 64 s, which is longer than its 30 s.
func TestAWaitingRetryWaitsItsOwnDelayAtEveryDelivery(t *testing.T) {
	spec, err := normalizedSpec(wakeSpec(&lockedBuffer{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		waiting bool
		want    time.Duration
	}{
		{name: "a waiting retry", waiting: true, want: 30 * time.Second},
		{name: "control: a retry that does not wait", waiting: false, want: 64 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry := &RetryLater{After: 30 * time.Second, Waiting: tc.waiting, Err: errors.New("the wake finds its claim in a release")}
			if got, _ := retryDelay(7, spec, retry); got != tc.want {
				t.Fatalf("retryDelay at delivery 7 = %s, want %s", got, tc.want)
			}
		})
	}
}
