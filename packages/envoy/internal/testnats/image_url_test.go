//go:build natslive

package testnats

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

// publishedAt is a container whose client port Docker published at hostIP, on a daemon at host.
type publishedAt struct{ hostIP, host string }

func (p publishedAt) Inspect(context.Context) (*container.InspectResponse, error) {
	return &container.InspectResponse{NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{
		Ports: nat.PortMap{imageClientPort: {{HostIP: p.hostIP, HostPort: "32801"}}},
	}}}, nil
}

func (p publishedAt) Host(context.Context) (string, error) { return p.host, nil }

// A Docker daemon on another machine publishes a container's client port on its default binding,
// every address of that machine (startImage), and the URL names the daemon's host there: 0.0.0.0
// or :: dialed here is this machine. A port on a loopback address of its own is named at that
// address, as a daemon on this machine publishes it.
func TestAnImageURLNamesTheDaemonsHostForAPortOnEveryAddress(t *testing.T) {
	const daemon = "198.51.100.7"
	for _, row := range []struct{ name, hostIP, want string }{
		{"every IPv4 address", "0.0.0.0", "nats://198.51.100.7:32801"},
		{"every IPv6 address", "::", "nats://198.51.100.7:32801"},
		{"no address", "", "nats://198.51.100.7:32801"},
		{"a loopback address of its own", "127.44.5.6", "nats://127.44.5.6:32801"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := ImageURL(t, publishedAt{hostIP: row.hostIP, host: daemon}); got != row.want {
				t.Errorf("ImageURL of a port published at %q on a daemon at %s = %s, want %s", row.hostIP, daemon, got, row.want)
			}
		})
	}
}

// Only a daemon testcontainers places on this machine has a container's port published on a
// loopback address: "localhost" (a unix socket) or a loopback address. A daemon at another
// machine's address or name, or at the gateway a container reaches the host's daemon through,
// publishes on a machine whose loopback this one does not reach.
func TestOnlyADaemonOnThisMachinePublishesOnALoopbackAddress(t *testing.T) {
	for host, local := range map[string]bool{
		"localhost":      true,
		"127.0.0.1":      true,
		"127.9.8.7":      true,
		"::1":            true,
		"172.20.0.1":     false,
		"198.51.100.7":   false,
		"docker.example": false,
	} {
		if got := localDaemonHost(host); got != local {
			t.Errorf("localDaemonHost(%q) = %t, want %t", host, got, local)
		}
	}
}
