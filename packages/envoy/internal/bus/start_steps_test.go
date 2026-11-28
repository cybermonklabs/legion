package bus

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// startTriesOf counts the connections server took from the client named name, open and closed, as
// its settled record of them lists them (testnats.Server.Connections): one for each try of that
// client's start. A start ends right after it closes its last try's connection, and one read of the
// server's listing at that moment can find that connection on neither its list of open connections
// nor its list of closed ones, and so count a try too few.
func startTriesOf(t *testing.T, server *testnats.Server, name string) int {
	t.Helper()
	tries := 0
	for _, connection := range server.Connections(t) {
		if connection.Name == name {
			tries++
		}
	}
	return tries
}

// A start that waits out refusals of its credential (WithCredentialWait) ends at once on an ensure
// of the stream that failed for any other reason, as a start without the wait does: here a single
// server refusing three copies of the stream, as the deployment's ENVOY_NATS_REPLICAS=3 is refused
// by a hub that is no cluster. Trying again cannot change that answer, and the start's hang guard
// (startHangGuard), the other way an ensure fails, has already waited out its whole bound. So the
// server takes one connection of the start, the start reports no refusal to its caller, and its
// error names the ensure's own failure and not the credential wait. The start once tried such an
// ensure again a second later, ten times, and ended saying the credential wait ran out.
func TestAnEnsureThatFailsForAnotherReasonEndsTheStartAtItsFirstTry(t *testing.T) {
	server, uri := testnats.Start(t)
	name := "ensure-fails-" + t.Name()
	var refused atomic.Int32
	started := time.Now()
	client, err := ConnectOwningStream([]string{uri}, WithName(name), WithReplicas(3),
		WithCredentialWait(time.Minute, func(error) { refused.Add(1) }, nil))
	took := time.Since(started)
	if client != nil {
		client.Close()
		t.Fatalf("a start whose ensure the server refused for its replica count started, after %s", took)
	}
	tries := startTriesOf(t, server, name)
	t.Logf("the start ended %s after it began, after %d tries: %v", took, tries, err)
	if err == nil || !strings.Contains(err.Error(), "ENVOY_NATS_REPLICAS=3") {
		t.Fatalf("the start ended with %v, want the ensure's own failure, naming ENVOY_NATS_REPLICAS=3", err)
	}
	if strings.Contains(err.Error(), "credential wait") || credentialRefusal(err) {
		t.Fatalf("the start ended with %q, which names the credential wait or reads as a refusal of the credential, though nothing refused it", err)
	}
	if tries != 1 {
		t.Fatalf("the server took %d connections of the start, want 1: the ensure's failure ends the start at its first try", tries)
	}
	if n := refused.Load(); n != 0 {
		t.Fatalf("the start reported %d refusals of its credential, want none", n)
	}
}

// A start that met no refusal of its credential ends after main's ten tries in a row, whatever its
// bound, and where main's connect ends, a second after the tenth (connectTrying), with its last
// try's own error, naming the step that failed, and not with words of the credential wait it never
// needed.
func TestACredentialWaitThatMetNoRefusalEndsWithItsLastTrysOwnError(t *testing.T) {
	other := errors.New("nats: no servers available for connection")
	clock := &fakeStartClock{at: time.Date(2026, 11, 22, 0, 3, 50, 0, time.UTC)}
	tries := 0
	_, err := waitOutCredentialRefusals(3*time.Second, nil, clock, func() (*Client, error) {
		tries++
		return nil, other
	})
	if tries != connectTries {
		t.Errorf("the start made %d tries, want main's %d, a second apart, its bound of 3 s notwithstanding", tries, connectTries)
	}
	if want := slices.Repeat([]time.Duration{connectRetryWait}, connectTries); !slices.Equal(clock.slept, want) {
		t.Errorf("the start waited %v, want main's %v, a wait after each try, the tenth too", clock.slept, want)
	}
	if !errors.Is(err, other) {
		t.Fatalf("the start ended with %v, want its last try's error", err)
	}
	if strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "refus") {
		t.Fatalf("the start ended with %q, which names a credential wait or a refusal it never met", err)
	}
}
