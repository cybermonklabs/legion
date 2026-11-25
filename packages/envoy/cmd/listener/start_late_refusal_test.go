package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The main client's connect starts after the start's credential wait has run out, as when NATS
// did not answer for the whole wait and then answers with a refusal of the credential. No earlier
// try met a refusal. The README says the start then ends with exit 1, its last line naming the
// credential (`nkey user <public key>`), as the later connects already do (openStartConnection).
func TestACredentialRefusedAfterTheStartupBoundStillNamesTheUser(t *testing.T) {
	const childURL = "ENVOY_TEST_LATE_REFUSAL_URL"
	const childSeed = "ENVOY_TEST_LATE_REFUSAL_SEED"
	if uri := os.Getenv(childURL); uri != "" {
		t.Setenv(bus.NkeySeedEnvVar, os.Getenv(childSeed))
		start := newNATSStart(listenerCredentialWait)
		start.once.Do(func() { start.began = time.Now().Add(-listenerCredentialWait) })
		client := connectListener(config.Service{MachineID: "late-refusal", NATSURLs: []string{uri}, NATSReplicas: 1}, nil, listenerTryInstance, logging.NewWithWriter("test", os.Stderr), start)
		client.Close()
		t.Fatal("the listener connected despite a refused credential")
	}
	seed, public := testnats.User(t)
	_, fallback := testnats.User(t)
	_, uri := testnats.StartConfig(t, nkeyAuthorization(fallback), []string{"-js"}, true)
	cmd := exec.Command(os.Args[0], "-test.run=^TestACredentialRefusedAfterTheStartupBoundStillNamesTheUser$", "-test.timeout=1m")
	cmd.Env = append(os.Environ(), childURL+"="+uri, childSeed+"="+seed)
	process := cmdtest.Start(t, "listener after the credential wait", cmd)
	process.WaitExit(t, "the refused start to exit")
	output := process.Output.String()
	if code := cmd.ProcessState.ExitCode(); code != 1 || !strings.Contains(output, authorizationViolation) || !strings.Contains(output, "the NATS credential: nkey user "+public) {
		t.Fatalf("the refused start exited %d with %q, want exit 1 naming its authorization refusal and public nkey", code, output)
	}
}
