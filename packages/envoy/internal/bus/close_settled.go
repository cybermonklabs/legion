package bus

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// failedStartCloseBound bounds closeSettled's wait for the work a failed start's close sets off.
// That work is nats.go running the connection's disconnect and close callbacks on its callback
// goroutine and a recovery that finds the client stopped and returns, so it takes a scheduling
// delay and nothing more; the bound only keeps a callback that hangs from holding the start, and
// is connectTimeout, the most one dial of a try may take.
const failedStartCloseBound = connectTimeout

// closeWatch is the close of conn that closeSettled waits for: handled is closed once onClosed has
// handled it (closeHandled).
type closeWatch struct {
	conn    *nats.Conn
	handled chan struct{}
}

// runEventWork runs work on a goroutine of its own that eventWork counts: the recovery a close
// starts (onClosed) and the run after a reconnect (onReconnect), so closeSettled can wait for
// them.
func (c *Client) runEventWork(work func()) {
	c.eventWork.Add(1)
	go func() {
		defer c.eventWork.Done()
		work()
	}()
}

// closeHandled records that onClosed has handled the close of nc, after it has started the
// recovery that close calls for (runEventWork), and wakes a closeSettled that waits for it.
func (c *Client) closeHandled(nc *nats.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastClosed = nc
	if c.closeWatch != nil && c.closeWatch.conn == nc {
		close(c.closeWatch.handled)
		c.closeWatch = nil
	}
}

// closeFailedStart closes c, a client its start made and gives up on because its start check
// failed (checkedStart), as closeSettled says.
func (c *Client) closeFailedStart() {
	c.closeSettled(c.Connection(), c.Close)
}

// closeSettled closes nc, a connection of a start that failed, by running closeIt, and returns once
// what the close set off has run, or once its bound has passed: nats.go's disconnect and close
// callbacks for nc, which log the disconnect and the close and run onClosed, and every goroutine
// the client's connection events started (eventWork), the recovery that close starts among them,
// which logs that it was cancelled. nats.go runs a connection's callbacks on a goroutine of its
// own, in order, after Close has returned, so without the wait those lines can follow whatever the
// start's caller does next: cmd/listener's last line, which names the credential and the refusal
// its start ended on, was once followed by "envoy nats recovery cancelled". The client must be
// stopped already, or stop in closeIt, so the recovery dials nothing and returns at once. A close
// that onClosed handled before the call is not waited for again (lastClosed).
//
// The wait is bounded, by failedStartCloseBound (connectTimeout, 5 s) or by a test's own bound
// (closeSettleBound): past it, closeSettled logs at WARN what it still waited for and returns, and
// a line of that close's teardown can then follow the caller's next line. So the order it gives is
// the order of a close whose callbacks run within the bound, not of one whose callback hangs.
func (c *Client) closeSettled(nc *nats.Conn, closeIt func()) {
	var handled chan struct{}
	if nc != nil {
		handled = make(chan struct{})
		c.mu.Lock()
		if c.lastClosed == nc {
			close(handled)
		} else {
			c.closeWatch = &closeWatch{conn: nc, handled: handled}
		}
		c.mu.Unlock()
	}
	closeIt()
	limit := failedStartCloseBound
	if c.closeSettleBound > 0 {
		limit = c.closeSettleBound
	}
	bound := time.NewTimer(limit)
	defer bound.Stop()
	if handled != nil {
		select {
		case <-handled:
		case <-bound.C:
			slog.Warn("envoy nats a failed start's connection close was not handled in time; the start goes on",
				slog.String("name", c.name), slog.Duration("bound", limit))
			return
		}
	}
	// Every eventWork goroutine was started before the close was handled: onClosed starts its
	// recovery first, and a closed connection reports no later event. So no Add can race this Wait.
	settled := make(chan struct{})
	go func() {
		c.eventWork.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-bound.C:
		slog.Warn("envoy nats a failed start's recovery did not end in time; the start goes on",
			slog.String("name", c.name), slog.Duration("bound", limit))
	}
}
