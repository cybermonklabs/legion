package bus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// connectionsListed is what a server's monitoring says of its client connections: how many it has
// taken since it started (/varz total_connections), how many it lists, open or closed
// (/connz?state=all), and the name each one it lists carries.
type connectionsListed struct {
	Taken  int
	Listed int
	Names  []string
}

// startTriesOf counts the connections the server whose monitoring is at monitor took from the client
// named name, open and closed: one for each try of that client's start. It counts them once the
// server lists, open or closed, as many client connections as it has taken, which it waits for,
// bounded only by the test binary's deadline (testwait.Until). nats-server v2.15.1 saves a closed
// connection among its closed ones on a goroutine of its own (server/client.go:2137-2145), which
// can run after the close has taken the connection off its open ones (server/client.go:6335,
// server/server.go:3786-3787), and /connz reads the open ones before the closed ones
// (server/monitor.go:304-331). A read in between lists the connection as neither, and one across
// the save can list it as both. A start's tries close one after another, so at most one of its
// connections is between the two lists at a time, and a list as long as the count of the
// connections the server took holds each of them once.
func startTriesOf(t *testing.T, monitor, name string) int {
	t.Helper()
	ctx := testwait.Context(t)
	connections := testwait.Until(t, "the server to list every client connection it took, open or closed", func() (connectionsListed, bool) {
		var varz struct {
			TotalConnections int `json:"total_connections"`
		}
		readMonitor(t, ctx, monitor+"/varz", &varz)
		var connz struct {
			Total       int `json:"total"`
			Connections []struct {
				Name string `json:"name"`
			} `json:"connections"`
		}
		readMonitor(t, ctx, monitor+"/connz?state=all&limit=4096", &connz)
		listed := connectionsListed{Taken: varz.TotalConnections, Listed: connz.Total}
		for _, connection := range connz.Connections {
			listed.Names = append(listed.Names, connection.Name)
		}
		return listed, listed.Listed == listed.Taken
	})
	tries := 0
	for _, connection := range connections.Names {
		if connection == name {
			tries++
		}
	}
	return tries
}

// readMonitor decodes what the server's monitoring answers at endpoint into into, under ctx, and
// fails the test when the request or the decode fails.
func readMonitor(t *testing.T, ctx context.Context, endpoint string, into any) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", endpoint, err)
	}
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
	tries := startTriesOf(t, server.MonitorURL(), name)
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
	_, err := waitOutCredentialRefusals(3*time.Second, nil, clock.clock(), func() (*Client, error) {
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
