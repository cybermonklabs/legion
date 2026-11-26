package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The first server takes the nkey but denies the stream read. The later dials reach a server
// without nkey support. The prior grant refusal stays in the diagnostic, but must not turn the
// final dial error into a grant retry.
func TestAnEdgeListenerEndsOnAnUnsupportedNkeyAfterAGrantRefusal(t *testing.T) {
	rig := startEdgeStartServer(t, func(public, fallback string) string {
		return nkeyAuthorization(fallback, nkeyUserDenied(public, streamInfoSubject))
	})
	_, noUsers := testnats.Start(t)
	forwarder := startRotatingForwarder(t, append([]string{rig.uri}, slices.Repeat([]string{noUsers}, 32)...))
	broker := ciphertokentest.NewStubBroker(t)
	account := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(account, []byte("edge-listener-account-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	child := startListenerChild(t, forwarder.url(), rig.seed, edgeStartMachine, func(port int) []string {
		return []string{
			config.EdgeProjectsEnvVar + "=" + edgeProject,
			EnvBrokerURL + "=" + broker.Server.URL,
			EnvBrokerAccountTokenFile + "=" + account,
			EnvPublicURL + "=http://127.0.0.1:" + strconv.Itoa(port),
		}
	})
	const unsupported = "nkeys not supported by the server"
	testwait.Until(t, "the edge start to classify the final dial error", func() (string, bool) {
		output := child.Output.String()
		select {
		case <-child.Exited:
			return output, true
		default:
		}
		for _, line := range linesIn(output, streamRefusedLine, map[string]any{"level": "ERROR"}) {
			text, _ := line["error"].(string)
			if strings.Contains(text, unsupported) {
				t.Fatalf("the edge listener retried the final dial error as a stream grant refusal, want exit 1: %s", text)
			}
		}
		return output, false
	})
	output := child.Output.String()
	if last := lastOutputLine(output); child.Cmd.ProcessState.ExitCode() != 1 ||
		!strings.Contains(last, unsupported) || !strings.Contains(last, "last refusal of the credential") ||
		!strings.Contains(last, "Permissions Violation") {
		t.Fatalf("the edge listener ended with %s, want exit 1 naming the final dial error and the prior grant refusal:\n%s", child.Cmd.ProcessState, output)
	}
}
