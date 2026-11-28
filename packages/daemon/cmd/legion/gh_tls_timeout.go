package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/sjawhar/envoy/tlstimeout"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// tlsTimeoutCurl, tlsTimeoutStampDir and tlsTimeoutBound are the seams of the record `legion gh`
// and `legion leak-check pre-push` write of a TLS handshake timeout (ghTLSTimeouts): its curl, the
// directory of its rate limit's stamps and the bound of its probes. Each is zero in the binary,
// where the record runs /usr/bin/curl, keeps its stamps under the system's temporary directory and
// bounds each probe at 5 s (tlstimeout.Recorder). Only this package's tests set them (TestMain,
// and the build legionBuild links); no code of the `legion` binary reads a variable for them.
var (
	tlsTimeoutCurl     string
	tlsTimeoutStampDir string
	tlsTimeoutBound    time.Duration
)

// ghTLSTimeouts is the record of a TLS handshake timeout `legion gh` writes, its line to stderr
// under the client name legion-gh: where the real gh of a call whose output is the caller's printed
// Go's `<Method> "<URL>": net/http: TLS handshake timeout` and exited non-zero (runGhChild), with
// that gh's CPU as childCPU; and where the leak guard's own gh read met one
// (leakguard.Options.OnTLSTimeout), once the call is over, with no child CPU, since the gh that
// failed was the guard's, which `legion gh` did not wait for. Its curl starts through
// procgroup.NoReaper: `legion gh` reaps no child it did not start.
func ghTLSTimeouts(stderr io.Writer, childCPU func() (user, sys time.Duration, ok bool)) *tlstimeout.Recorder {
	return &tlstimeout.Recorder{
		Client: "legion-gh", Stderr: stderr, Start: procgroup.NoReaper, ChildCPU: childCPU,
		Curl: tlsTimeoutCurl, StampDir: tlsTimeoutStampDir, Bound: tlsTimeoutBound,
	}
}

// recordGuardTLSTimeout records the TLS handshake timeout to host that the leak guard's own gh read
// met (leakguard.Options.OnTLSTimeout), where it met one (host is not empty) and a signal ended
// none of contexts, the contexts the call ran under (signalled): a call a signal stopped waits for
// no probe.
func recordGuardTLSTimeout(host string, stderr io.Writer, contexts ...context.Context) {
	if host == "" {
		return
	}
	for _, ctx := range contexts {
		if _, ended := signalled(ctx); ended {
			return
		}
	}
	ghTLSTimeouts(stderr, nil).Record(host)
}

// ghStderrTailBytes is how much of the end of gh's standard error runGhChild keeps, to read a TLS
// handshake timeout's line from (tlstimeout.HostInLine): gh prints that line last, alone.
const ghStderrTailBytes = 4 << 10

// ghStderrTail is the standard error runGhChild hands gh where gh's output is the caller's and
// this process's standard error is no terminal: every byte gh writes goes on to to at once, as
// gh's own write to that descriptor would, and the last ghStderrTailBytes of them are kept. A
// write to to that fails is dropped rather than handed back, so the copy goes on reading and gh
// never blocks on a full pipe: gh meets what its own write would have, nothing more. Only exec's
// copy goroutine writes it, and runGhChild reads kept once Wait has returned, which waits for that
// goroutine.
type ghStderrTail struct {
	to   io.Writer
	kept []byte
}

func (t *ghStderrTail) Write(p []byte) (int, error) {
	_, _ = t.to.Write(p)
	if len(p) >= ghStderrTailBytes {
		t.kept = append(t.kept[:0], p[len(p)-ghStderrTailBytes:]...)
		return len(p), nil
	}
	if over := len(t.kept) + len(p) - ghStderrTailBytes; over > 0 {
		t.kept = append(t.kept[:0], t.kept[over:]...)
	}
	t.kept = append(t.kept, p...)
	return len(p), nil
}

// ghChildCPU is the user and system CPU of the gh state describes, which has exited, for the
// record's ChildCPU.
func ghChildCPU(state *os.ProcessState) func() (time.Duration, time.Duration, bool) {
	return func() (time.Duration, time.Duration, bool) {
		if state == nil {
			return 0, 0, false
		}
		return state.UserTime(), state.SystemTime(), true
	}
}
