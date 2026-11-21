package testnats

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"

	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// A start Docker refuses, because something took the port it probed before Docker bound it, is
// started again on a fresh address on its own configuration. In daemon-go a server meant to admit
// its nkey user alone came back from such a second start with an empty configuration, which admits
// every client, and TestNoSeedIsRefusedByAServerThatRequiresOne failed on it: "admitted a client
// with no credential: it enforces no nkey users". The row holds the first start's client port at
// its address, so Docker refuses that publish as in use, and the server of the second start must
// refuse a client with no credential (answering, inside StartNkeyAuthorizedUsers) and hold the
// configuration naming the user. It counts the starts, so a run in which Docker was never refused
// cannot pass for one that was. Docker refuses only where this process can hold the port it
// publishes: on a daemon on this machine. On a daemon on another machine the row fails as UNKNOWN,
// naming the daemon, rather than fail on a start count that says nothing of the configuration.
func TestARefusedStartIsStartedAgainOnItsOwnConfiguration(t *testing.T) {
	if local, err := localDaemon(context.Background()); err != nil {
		t.Fatalf("find where the Docker daemon publishes a container's ports: %v", err)
	} else if !local {
		daemon := "another machine"
		if provider, err := testcontainers.NewDockerProvider(); err == nil {
			if host, err := provider.DaemonHost(context.Background()); err == nil {
				daemon = host
			}
			provider.Close()
		}
		t.Fatalf("UNKNOWN: the Docker daemon is at %s, not this machine, so this process cannot hold the port Docker publishes there and cannot make Docker refuse a start; run the row against a daemon on this machine", daemon)
	}
	choose := choosePorts
	var held net.Listener
	t.Cleanup(func() {
		choosePorts = choose
		if held != nil {
			_ = held.Close()
		}
	})
	starts := 0
	choosePorts = func(n int) (string, []int, error) {
		host, ports, err := choose(n)
		if err != nil {
			return host, ports, err
		}
		starts++
		if starts == 1 {
			// The kernel has just given this port at this address and nothing holds it, so the
			// listen succeeds, and Docker's own bind of the same address and port is refused.
			if held, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(ports[0]))); err != nil {
				t.Fatalf("hold the first start's client port %s:%d: %v", host, ports[0], err)
			}
		}
		return host, ports, nil
	}
	_, public := testnkeys.User(t)
	ctr, url := StartNkeyAuthorizedUsers(t, Image, NkeyUser{Public: public})
	if starts != 2 {
		t.Fatalf("the server came up after %d starts, want 2: one whose publish Docker refused, then one on a fresh address", starts)
	}
	file, err := ctr.CopyFileFromContainer(context.Background(), "/etc/nats.conf")
	if err != nil {
		t.Fatalf("read the configuration of the server at %s: %v", url, err)
	}
	defer file.Close()
	config, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read the configuration of the server at %s: %v", url, err)
	}
	if !strings.Contains(string(config), public) {
		t.Errorf("the server at %s, started again after a refused publish, holds the configuration %q, which does not name its user %s", url, config, public)
	}
}
