// packages/envoy/internal/broker/helper/renew_outage_test.go
//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/leasetime"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows below run renewLoop on the server's own clock, a fake the row moves, at the
// production lease, sweep and floor: each renewal goes out on the instant the loop armed it for,
// and each failure takes its delay on that same clock, so the rig walks the model's own instants
// and no lateness of the machine can move a verdict.

// outageBroker is a broker that stops serving between gapStart and gapEnd, counted from the
// renewal that granted the lease. Each renewal it refuses takes the delay at that renewal's
// own place in delays to fail: nothing when the connection is refused, the whole client
// timeout when the outage black-holes it, and anything between when a proxy gives up on it or
// a terminating pod resets it. Which delay each renewal meets is part of the worst case the
// model searches, and both renewers wait from the instant the failure comes back. Once a
// broker serves again it answers either a new lease or, if the enrollment's own lease ran out
// while nothing served, ENROLLMENT_NOT_LIVE — the answer the broker's first sweep leaves
// behind, since a gap this short counts as a broker serving and is given back to nobody.
type outageBroker struct {
	clock            *clocktest.Fake
	start            time.Time
	gapStart, gapEnd time.Duration
	delays           []time.Duration
	timeout          time.Duration
	lease            time.Duration

	mu        sync.Mutex
	attempts  []time.Duration
	renewedOK bool
}

// renewed answers whether a renewal has got through, for a row to wait on.
func (b *outageBroker) renewed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.renewedOK
}

// RoundTrip answers one renewal at the instant the server's clock reads. A refused renewal that
// takes a delay to fail moves that clock by it before answering: the call took that long, and
// nothing of renewLoop is armed while it is in the call.
func (b *outageBroker) RoundTrip(*http.Request) (*http.Response, error) {
	at := b.clock.Now().Sub(b.start)
	b.mu.Lock()
	i := len(b.attempts)
	b.attempts = append(b.attempts, at)
	b.mu.Unlock()
	switch {
	case at >= b.gapStart && at < b.gapEnd:
		var delay time.Duration
		if i < len(b.delays) {
			delay = min(max(b.delays[i], 0), b.timeout)
		}
		if delay > 0 {
			b.clock.Advance(delay)
		}
		if delay >= b.timeout {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	case at >= b.lease:
		return reply(http.StatusUnauthorized, map[string]string{"code": "ENROLLMENT_NOT_LIVE", "error": "enrollment is not live"}), nil
	default:
		b.mu.Lock()
		b.renewedOK = true
		b.mu.Unlock()
		return reply(http.StatusOK, map[string]any{"lease_expires_at": b.clock.Now().Add(b.lease)}), nil
	}
}

func reply(status int, body any) *http.Response {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}
}

// TestRenewLoopOutlivesAnOutageOnlyAboveTheLeaseBound drives the real renewLoop through an
// outage of leasetime.NotServingSweeps sweeps, beginning at the worst instant for it, at a
// lease the model says it cannot outlive and one it says it can. At the first the renewal that
// gets through finds the enrollment gone and the loop answers "enroll again"; at the second it
// renews and goes on holding the session. Both run on the server's clock at the production
// scale, so the rig's renewals land on the model's own instants.
//
// In a pod this row runs -run-filtered: the package's contract rows need SO_PEERPIDFD, which
// gVisor does not answer, and fail PEER_UNIDENTIFIED. CI's envoy-go job runs the whole package.
func TestRenewLoopOutlivesAnOutageOnlyAboveTheLeaseBound(t *testing.T) {
	model := leasetime.Production()
	const sweep = 5 * time.Second
	for _, c := range []struct {
		what      string
		lease     time.Duration
		wantLapse bool
	}{
		{"a lease the outage runs out", 45 * time.Second, true},
		{"a lease that outlives it", 180 * time.Second, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			want := model.WorstFor(leasetime.RenewerHelper, c.lease, sweep)
			if want.Survived() == c.wantLapse {
				t.Fatalf("the model renews a lease of %v at %v, which survives %v; this row wants a lapse: %v", c.lease, want.Renewed, want.Survived(), c.wantLapse)
			}
			var delays []time.Duration
			for _, a := range want.Attempts {
				if a.Refused() {
					delays = append(delays, a.Delay())
				}
			}
			sess, err := newSession(os.Getpid(), 0, "runtime-1", "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			sess.setEnrolledAt("enrollment-1", sess.identity().Generation)
			fake := clocktest.New(time.Now())
			start := fake.Now()
			broker := &outageBroker{clock: fake, start: start, gapStart: want.GapStart, gapEnd: want.GapEnd, delays: delays, timeout: model.ClientTimeout, lease: c.lease}
			srv := &Server{
				Broker:   &Broker{URL: brokeraddr.New("http://broker.invalid"), HTTP: &http.Client{Transport: broker}},
				Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
				MinRenew: model.HelperMinRenew,
				Clock:    fake,
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lapsed := make(chan bool, 1)
			go func() { lapsed <- srv.renewLoop(ctx, sess, start.Add(c.lease), sess.identity().Generation) }()
			// Fire each renewal the loop arms, one at a time, until the loop ends (the lease
			// ran out) or a renewal gets through (the lease outlived the outage). A renewal
			// that got through is read before the next one is fired, so the rig launches no
			// renewal the model does not walk.
			ended, got := false, false
			for !ended && !(!c.wantLapse && broker.renewed()) {
				next := testwait.Until(t, "the renew loop to arm its next renewal, end, or renew", func() (time.Time, bool) {
					select {
					case got = <-lapsed:
						ended = true
						return time.Time{}, true
					default:
					}
					if !c.wantLapse && broker.renewed() {
						return time.Time{}, true
					}
					armed := fake.Armed()
					if len(armed) == 0 {
						return time.Time{}, false
					}
					return armed[0], true
				})
				if !next.IsZero() {
					fake.Set(next)
				}
			}
			if c.wantLapse {
				if !got {
					t.Fatal("the renew loop gave up on the session instead of enrolling it again")
				}
			} else {
				cancel()
				if got := testwait.Receive(t, "the renew loop to return once its context ended", lapsed); got {
					t.Fatal("the renew loop lapsed the session, want it renewed and held")
				}
			}
			broker.mu.Lock()
			defer broker.mu.Unlock()
			if len(broker.attempts) != len(want.Attempts) {
				t.Errorf("the renewer launched %d renewals, at %v; the model walked %d, at %v", len(broker.attempts), broker.attempts, len(want.Attempts), want.Attempts)
			}
			if broker.renewedOK == c.wantLapse {
				t.Errorf("a renewal got through: %v; the model's lease ran out first: %v", broker.renewedOK, c.wantLapse)
			}
		})
	}
}
