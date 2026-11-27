package claudebridge

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A LEGION_DAEMON_URL whose made-up password holds an unencoded "@" leaves the password's tail
// where the host goes, and a failed dial's error names that host: the line logging a report that
// got no answer holds no part of the password, its tail in its ASCII form or in the `xn--` form a
// tail that is not ASCII is dialed by. The dial here fails naming the address it was given, with
// an error daemonLink.unanswered sorts into no case of its own, so the line quotes the error
// (`the transport failed: …`) and only redact.RequestError, in daemonLink.attempt, keeps the host
// out of it. The report is sent again until it is answered (daemonLink.untilAnswered), so the row
// ends its sending once the line is logged.
func TestAFailedTurnReportLogsNoPartOfThePassword(t *testing.T) {
	passwordAt := "legion:madeuphead0123" + "@"
	for name, tail := range map[string]string{
		"an ASCII tail":            "madeuptail0123.invalid",
		"a tail that is not ASCII": "madeuptail0123\u00f6rd.invalid",
	} {
		t.Run(name, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "boot-token")
			if err := os.WriteFile(tokenFile, []byte("boot-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logged := &lockedBuffer{}
			var dialed atomic.Value
			failNamingTheAddress := func(_ context.Context, network, address string) (net.Conn, error) {
				dialed.Store(address)
				return nil, &net.OpError{Op: "dial", Net: network, Err: fmt.Errorf("no route to %s", address)}
			}
			link := dialDaemon(Config{DaemonURL: "http://" + passwordAt + tail, BootTokenFile: tokenFile, Generation: 1,
				calls: daemonCalls{dial: failNamingTheAddress}}, log.New(logged, "", 0))
			ctx, cancel := context.WithCancel(context.Background())
			reports := &turnReports{daemon: link, ctx: ctx, refused: func(uint64) {}}
			done := make(chan struct{})
			go func() {
				defer close(done)
				reports.post(claim.TurnOutcomeRequest{BootCaller: claim.BootCaller{BootToken: "boot-token"}, Generation: 1,
					Sequence: 1, Outcome: claim.TurnClean})
			}()
			const failed = "claims/turn-outcome of turn 1 got no answer from the daemon (the transport failed: "
			testwait.Eventually(t, "the bridge to log the report that got no answer", func() bool {
				return strings.Contains(logged.String(), failed)
			})
			cancel()
			testwait.Receive(t, "the report's sending to end with its context", done)
			if address, _ := dialed.Load().(string); !strings.Contains(strings.ToLower(address), "madeuptail") && !strings.Contains(address, "xn--") {
				t.Fatalf("the dial was given %q, which holds no part of the password's tail, so the log could not hold it", address)
			}
			lower := strings.ToLower(logged.String())
			for _, part := range []string{"madeuphead", "madeuptail", "xn--"} {
				if strings.Contains(lower, part) {
					t.Errorf("the log %q holds %q of the password", logged.String(), part)
				}
			}
		})
	}
}
