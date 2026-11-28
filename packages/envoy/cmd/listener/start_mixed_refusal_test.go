package main

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
)

// A later connect of the start met a refusal of its credential, got past it, and then failed ten
// times in a row for another reason, here an account at its connection limit. The bus returns the
// last of those errors and names the earlier refusal in it (bus.withLastRefusal). The README's
// start rule retries a connection the server refuses for its connection limit, so the start must
// try the step again and not end on the refusal named inside the error.
func TestAStartRetriesTheFinalDialErrorDespiteAnEarlierRefusal(t *testing.T) {
	const child = "ENVOY_TEST_MIXED_START_REFUSAL"
	if os.Getenv(child) == "1" {
		start := newNATSStart(listenerCredentialWait)
		var tries int
		_ = openStartConnection(logging.NewWithWriter("test", os.Stderr), start, "retrying connection", "start ended", "grant refused", func() (int, error) {
			tries++
			if tries == 1 {
				return 0, fmt.Errorf("%w (last refusal of the credential: %w)", nats.ErrMaxConnectionsExceeded, nats.ErrAuthorization)
			}
			return 1, nil
		})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAStartRetriesTheFinalDialErrorDespiteAnEarlierRefusal$", "-test.timeout=1m")
	cmd.Env = append(os.Environ(), child+"=1")
	process := cmdtest.Start(t, "listener start with a retryable final error", cmd)
	process.WaitExit(t, "the retry to succeed")
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the listener exited %d rather than retrying a connection-limit error: %s", code, process.Output.String())
	}
}
