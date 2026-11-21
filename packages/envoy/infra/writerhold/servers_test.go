package writerhold

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rowWithin bounds one row's calls into the hold, under the box's load.
const rowWithin = 5 * time.Minute

// rowContext is a context for one row's calls into the hold, ended with the test.
func rowContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rowWithin)
	t.Cleanup(cancel)
	return ctx
}

// hubOf is the KubeHub of kube's fake API, as the hold reads the clustered hub.
func hubOf(kube *holdtest.Kube) KubeHub {
	return KubeHub{
		Client: kube.Client, Namespace: holdtest.Namespace, StatefulSet: holdtest.StatefulSet,
		ConfigMap: holdtest.ConfigMap, Forward: kube,
	}
}

// varzPorts is the part of a server's /varz that gives its route and leafnode listeners.
type varzPorts struct {
	Cluster struct {
		Host string `json:"addr"`
		Port int    `json:"cluster_port"`
	} `json:"cluster"`
	Leaf struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"leaf"`
}

// route is the server's own route listener, host:port.
func (p varzPorts) route() string {
	return net.JoinHostPort(p.Cluster.Host, strconv.Itoa(p.Cluster.Port))
}

// portsOf reads pod's route and leafnode listeners from its /varz.
func portsOf(t *testing.T, hub KubeHub, pod string) varzPorts {
	t.Helper()
	var ports varzPorts
	monitor, stop, err := hub.Monitor(rowContext(t), pod)
	require.NoError(t, err)
	defer stop()
	require.NoError(t, monitor.Get(rowContext(t), "/varz", &ports))
	return ports
}

// startOutsider runs a server named name, with no JetStream, in the declared servers' cluster
// name, configured by more, shut down when the test ends: a server nobody declared.
func startOutsider(t *testing.T, name string, more func(*natsserver.Options)) {
	t.Helper()
	opts := &natsserver.Options{ServerName: name, Host: "127.0.0.1", Port: -1, NoSigs: true, NoLog: true}
	more(opts)
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	require.True(t, srv.ReadyForConnections(30*time.Second), "the outsider %s did not start", name)
}

// routeTo is the outsider option of a route to url, in the test cluster's name.
func routeTo(t *testing.T, route string) func(*natsserver.Options) {
	t.Helper()
	routeURL, err := url.Parse(route)
	require.NoError(t, err)
	return func(opts *natsserver.Options) {
		opts.Cluster = natsserver.ClusterOpts{Name: "envoy-test", Host: "127.0.0.1", Port: -1}
		opts.Routes = []*url.URL{routeURL}
	}
}

// lists reports whether pod's server lists a route to the server named name.
func lists(t *testing.T, hub KubeHub, pod, name string) bool {
	t.Helper()
	monitor, stop, err := hub.Monitor(rowContext(t), pod)
	if err != nil {
		return false
	}
	defer stop()
	var routez Routez
	if monitor.Get(rowContext(t), "/routez", &routez) != nil {
		return false
	}
	for _, route := range routez.Routes {
		if route.RemoteName == name {
			return true
		}
	}
	return false
}

// TestHoldDiscovery shows step 1's discovery of the declared servers through their monitoring
// ports: each pod's own /varz names the declared servers (n1 to n3 on pods envoy-nats-0 to -2, so
// a name is never taken from a pod); a declared server whose /varz does not answer is UNKNOWN; a
// fourth server that routes to one declared server is listed by the declared servers' /routez and
// stops the hold, named; a leafnode in /leafz or a gateway in /gatewayz stops the hold; and, with
// the cluster's route authorization, a route without the credential is refused by the cluster
// and so never listed, while the same server with the credential joins and stops the hold. Each
// refusal's control is the same cluster read clean before. It fails when Discover reads the
// declared names from the pods, skips a route whose remote is not declared, counts a leafnode or a
// gateway as harmless, or reads a server that does not answer as clean.
func TestHoldDiscovery(t *testing.T) {
	t.Run("declared servers and strangers", func(t *testing.T) {
		cluster := testnats.StartCluster(t, 3)
		hub := hubOf(holdtest.NewKube(t, cluster))
		ctx := rowContext(t)

		declared, err := Discover(ctx, hub)
		require.NoError(t, err, "the declared servers alone, routed to each other: the control every refusal below is read against")
		require.Len(t, declared, 3)
		for i, server := range declared {
			require.Equal(t, holdtest.Pod(i), server.Pod)
			require.Equal(t, cluster.Name(i), server.Varz.ServerName, "the declared name is the server's own")
			require.NotEmpty(t, server.Varz.ServerID)
		}
		require.Equal(t, defaultAuthTimeout, declared.AuthTimeout())

		cluster.Servers[1].Stop(t)
		_, err = Discover(ctx, hub)
		requireUnknown(t, err, StepPreflight, holdtest.Pod(1))
		cluster.Servers[1].Start(t)
		cluster.AwaitReady(t)
		_, err = Discover(ctx, hub)
		require.NoError(t, err, "with every member answering again")

		ports := portsOf(t, hub, holdtest.Pod(0))
		startOutsider(t, "outsider", routeTo(t, "nats-route://"+ports.route()))
		testwait.Eventually(t, "a declared server lists the outsider's route", func() bool {
			return lists(t, hub, holdtest.Pod(0), "outsider")
		})
		_, err = Discover(ctx, hub)
		requireRefusal(t, err, StepPreflight, "outsider", "no other declared server")
	})

	t.Run("a leafnode", func(t *testing.T) {
		cluster := testnats.StartClusterConfig(t, 2, "leafnodes {\n  listen: \"127.0.0.1:-1\"\n}\n")
		hub := hubOf(holdtest.NewKube(t, cluster))
		ctx := rowContext(t)
		_, err := Discover(ctx, hub)
		require.NoError(t, err, "a leafnode listener with no leafnode connected is no leafnode")

		ports := portsOf(t, hub, holdtest.Pod(0))
		leafURL, err := url.Parse("nats-leaf://" + net.JoinHostPort(ports.Leaf.Host, strconv.Itoa(ports.Leaf.Port)))
		require.NoError(t, err)
		startOutsider(t, "leaf", func(opts *natsserver.Options) {
			opts.LeafNode.Remotes = []*natsserver.RemoteLeafOpts{{URLs: []*url.URL{leafURL}}}
		})
		testwait.Eventually(t, "the declared server's /leafz counts the leafnode", func() bool {
			monitor, stop, err := hub.Monitor(ctx, holdtest.Pod(0))
			if err != nil {
				return false
			}
			defer stop()
			var leafz Leafz
			return monitor.Get(ctx, "/leafz", &leafz) == nil && leafz.Leafnodes == 1
		})
		_, err = Discover(ctx, hub)
		requireRefusal(t, err, StepPreflight, holdtest.Pod(0), "1 leafnode")
	})

	t.Run("a gateway", func(t *testing.T) {
		cluster := testnats.StartClusterConfig(t, 2, "gateway {\n  name: \"envoy-test\"\n  listen: \"127.0.0.1:-1\"\n}\n")
		_, err := Discover(rowContext(t), hubOf(holdtest.NewKube(t, cluster)))
		requireRefusal(t, err, StepPreflight, "a gateway", `name "envoy-test"`)
	})

	t.Run("a route without credentials is refused by the cluster", func(t *testing.T) {
		cluster := testnats.StartClusterRouteAuth(t, 2, "route", "secret1", "")
		hub := hubOf(holdtest.NewKube(t, cluster))
		ctx := rowContext(t)
		ports := portsOf(t, hub, holdtest.Pod(0))
		target := ports.route()

		startOutsider(t, "stranger", routeTo(t, "nats-route://"+target))
		testwait.Eventually(t, "the declared server logs the stranger's route refused", func() bool {
			return strings.Contains(cluster.Servers[0].Log(), "authentication error")
		})
		require.False(t, lists(t, hub, holdtest.Pod(0), "stranger"), "the cluster listed a route that presented no credential")
		_, err := Discover(ctx, hub)
		require.NoError(t, err, "a refused route is no route")

		// Control: the same server with the cluster's credential joins, and stops the hold.
		startOutsider(t, "insider", routeTo(t, (&url.URL{Scheme: "nats-route", User: url.UserPassword("route", "secret1"), Host: target}).String()))
		testwait.Eventually(t, "the declared server lists the route that presented the credential", func() bool {
			return lists(t, hub, holdtest.Pod(0), "insider")
		})
		_, err = Discover(ctx, hub)
		requireRefusal(t, err, StepPreflight, "insider", "no other declared server")
	})
}
