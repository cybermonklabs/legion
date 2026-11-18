package bus_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// deniedRequests is how many Requests the test below has in flight on one window at once, each to
// a denied subject of its own. The outbox forwards one role route per event and sends a whole
// batch through one window, so a grant that withholds several holders' subjects has this shape.
const deniedRequests = 8

// Every Request of one window the server denied is answered ErrPublishDenied naming the subject it
// sent, within its own timeout.
//
// The flush cannot name them all: nats.go keeps one last error per connection, so a violation of
// another subject recorded between a request's publish and its flush's read leaves that flush
// unable to confirm the publish (confirmPublished), and every request but the one the connection
// kept the violation of ended as a send nobody acknowledged, carrying another subject's violation
// - which its caller reports as a bus that did not answer rather than a grant that refused it.
// The request reads the violation the connection recorded under its own subject instead
// (deniedPublish), on the goroutine that waits for its answer: when the flush comes back unable
// to confirm the publish, and once more before the timeout is answered, since the connection
// records the violation on nats.go's error callback, which often runs after the flush returns.
// So a denial found at the first read answers at once, and one found at the second answers when
// the request's timeout runs out.
//
// Each request's timeout is a timer on the client's clock, which the test moves to the deadline
// they armed once every request either has answered or is waiting on it: so a request answered
// before the move is one the first read named, and one answered after it is the timeout's, each
// naming its own subject.
func TestEveryDeniedRequestOfOneWindowNamesItsOwnSubject(t *testing.T) {
	clock := clocktest.New(time.Now())
	client, _ := grantedClient(t, bus.WithClock(clock))
	window := client.WindowUntil(nil)

	type answered struct {
		subject string
		err     error
	}
	answers := make(chan answered, deniedRequests)
	var wg sync.WaitGroup
	for index := range deniedRequests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subject := fmt.Sprintf("%s-%d", deniedRole, index)
			err := <-window.Request(subject, roleEnvelope(subject), bus.RequestOptions{MsgID: bus.ForwardMsgID(subject, subject)})
			answers <- answered{subject: subject, err: err}
		}()
	}
	// Every request has answered or armed its timeout: the clock then moves to the latest of those
	// deadlines, which is what the requests still waiting are waiting for.
	armed := testwait.Until(t, fmt.Sprintf("the %d requests to answer or arm their timeouts", deniedRequests), func() ([]time.Time, bool) {
		armed := clock.Armed()
		return armed, len(armed)+len(answers) >= deniedRequests
	})
	if len(armed) > 0 {
		clock.Set(armed[len(armed)-1])
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	testwait.Receive(t, fmt.Sprintf("all %d denied Requests to be answered", deniedRequests), done)
	close(answers)

	for got := range answers {
		if !errors.Is(got.err, bus.ErrPublishDenied) || !errors.Is(got.err, natsgo.ErrPermissionViolation) {
			t.Errorf("the Request to %s answered %v, want ErrPublishDenied wrapping the permissions violation", got.subject, got.err)
			continue
		}
		if errors.Is(got.err, bus.ErrNotAcknowledged) {
			t.Errorf("the Request to %s answered %v, which reads as a send nobody acknowledged", got.subject, got.err)
		}
		if !strings.Contains(got.err.Error(), fmt.Sprintf("Publish to %q", got.subject)) {
			t.Errorf("the Request to %s answered %v, which carries another subject's violation", got.subject, got.err)
		}
	}
}
