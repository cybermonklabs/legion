package nettimeout

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"
)

// InChain finds a timeout at any depth of the chain, through both forms of Unwrap, and in both
// shapes net/http gives one read that timed out: the read's error directly under the client's
// *url.Error, or that error wrapped first by the transport's read loop. *url.Error's own Timeout
// answers false for the second, so a rule that stops at it misses that timeout. A chain in which no
// error timed out, a *url.Error over a refused connection among them, is no timeout.
func TestInChainFindsATimeoutAnywhereInTheChain(t *testing.T) {
	read := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ETIMEDOUT)}
	wrapped := &url.Error{Op: "Get", URL: "http://peer/x", Err: fmt.Errorf("readLoopPeekFailLocked: %w", read)}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the read's error directly under *url.Error", &url.Error{Op: "Get", URL: "http://peer/x", Err: read}, true},
		{"the read's error wrapped by the transport's read loop under *url.Error", wrapped, true},
		{"a timeout inside errors.Join", errors.Join(errors.New("close the response body"), wrapped), true},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"a refused connection under *url.Error", &url.Error{Op: "Get", URL: "http://peer/x", Err: refused}, false},
		{"a plain error", errors.New("the request failed"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InChain(tc.err); got != tc.want {
				t.Fatalf("InChain(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// InChain reads context.DeadlineExceeded wherever errors.Is finds it, which is what lets a caller
// ask InChain alone whether a failure timed out: the sentinel's own Timeout answers true, so a
// context's error once its deadline passed, which is the sentinel itself, and the sentinel wrapped
// with %w, under the client's *url.Error or inside errors.Join, is a timeout to both. A cancelled
// context's error is neither's.
func TestInChainReadsContextDeadlineExceededWhereverErrorsIsFindsIt(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a context's error once its deadline passed", expired.Err(), true},
		{"the sentinel wrapped", fmt.Errorf("mint the token: %w", context.DeadlineExceeded), true},
		{"the sentinel under *url.Error", &url.Error{Op: "Post", URL: "http://broker/v1/mint", Err: context.DeadlineExceeded}, true},
		{"the sentinel inside errors.Join", errors.Join(errors.New("close the response body"), context.DeadlineExceeded), true},
		{"a cancelled context's error", cancelled.Err(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if is := errors.Is(tc.err, context.DeadlineExceeded); is != tc.want {
				t.Fatalf("errors.Is(%v, context.DeadlineExceeded) = %t, want %t: the case's premise", tc.err, is, tc.want)
			}
			if got := InChain(tc.err); got != tc.want {
				t.Fatalf("InChain(%v) = %t, want %t, as errors.Is answers", tc.err, got, tc.want)
			}
		})
	}
}
