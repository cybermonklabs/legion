package testnats

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/network"
)

// Cluster is three nats-server containers of one image with JetStream clustered across them, as
// the clustered hub runs: each member's routes go over a Docker network of the cluster's own, where
// the members reach each other by their server names (n1, n2, n3), and each member's client and
// monitoring ports are published as a lone server's are (start), on the container's default
// network. A member taken off the cluster's network (Isolate) keeps its client port: a client
// connected to it reads what that member alone holds, as a replica cut off from its peers serves
// it, while the other two keep a quorum. Rejoin puts it back.
type Cluster struct {
	network  *testcontainers.DockerNetwork
	docker   *testcontainers.DockerClient
	members  []*tcnats.NATSContainer
	urls     []string
	monitors []string
}

// clusterSize is the cluster's members: the smallest a replicated stream keeps a quorum in with one
// member cut off.
const clusterSize = 3

// clusterReadyTimeout bounds the wait for the cluster's JetStream to elect a meta leader that
// every member follows, past the members' own starts, which Docker's load decides.
const clusterReadyTimeout = 2 * time.Minute

// StartCluster runs a Cluster of image, removed when t ends, and returns it once every member
// follows one meta leader and answers JetStream (AwaitReady).
func StartCluster(t testing.TB, image string) *Cluster {
	t.Helper()
	ctx := context.Background()
	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create the cluster's network: %v", err)
	}
	t.Cleanup(func() {
		if err := net.Remove(context.Background()); err != nil {
			t.Logf("remove the cluster's network %s: %v", net.Name, err)
		}
	})
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("open the Docker client: %v", err)
	}
	t.Cleanup(func() { docker.Close() })
	c := &Cluster{network: net, docker: docker}
	for i := range clusterSize {
		config := clusterConfig(i)
		member, err := start(image, func() []testcontainers.ContainerCustomizer {
			return []testcontainers.ContainerCustomizer{tcnats.WithConfigFile(strings.NewReader(config))}
		})
		if err != nil {
			t.Fatalf("start cluster member %s of %s: %v", memberName(i), image, err)
		}
		testcontainers.CleanupContainer(t, member)
		url, err := clientURL(member)
		if err != nil {
			t.Fatalf("member %s's client URL: %v", memberName(i), err)
		}
		monitoring, err := monitor(member)
		if err != nil {
			t.Fatalf("member %s's monitoring URL: %v", memberName(i), err)
		}
		c.members, c.urls, c.monitors = append(c.members, member), append(c.urls, url), append(c.monitors, monitoring)
		c.Rejoin(t, i)
	}
	for _, monitoring := range c.monitors {
		armServerLeakCheck(t, monitoring)
	}
	c.AwaitReady(t)
	return c
}

// clusterConfig is member i's configuration: its server name, JetStream with a store of its own,
// and a route to every member's name on the cluster's network, its own included, which nats-server
// ignores, so each member's routes are the same list. Each member advertises its route by that
// name too: a member gossips the route address it advertises to its peers, which otherwise is its
// address on every network it is on, the container's default one included, and a member taken off
// the cluster's network would be found again there (Isolate). A route a cut ended is found dead
// within three missed pings a second apart, rather than nats-server's two minutes, so a member put
// back rejoins within seconds (Rejoin).
func clusterConfig(i int) string {
	routes := make([]string, clusterSize)
	for j := range clusterSize {
		routes[j] = fmt.Sprintf("nats-route://%s:6222", memberName(j))
	}
	return fmt.Sprintf(`server_name: %s
ping_interval: "1s"
ping_max: 3
jetstream { store_dir: /data/jetstream }
cluster {
  name: legion-test
  listen: 0.0.0.0:6222
  advertise: "%s:6222"
  routes: [ %s ]
}
`, memberName(i), memberName(i), strings.Join(routes, ", "))
}

// MetaLeader is the JetStream meta group's leader as member i's /jsz names it.
func (c *Cluster) MetaLeader(i int) (string, error) {
	got, err := readJsz(c.monitors[i])
	if err != nil {
		return "", err
	}
	return got.Meta.Leader, nil
}

// Routes is the names of the members member i holds a route to, as its /routez lists them.
func (c *Cluster) Routes(i int) ([]string, error) {
	response, err := monitorClient.Get(c.monitors[i] + "/routez")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/routez answered %s", c.monitors[i], response.Status)
	}
	var routez struct {
		Routes []struct {
			Name string `json:"remote_name"`
			IP   string `json:"ip"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&routez); err != nil {
		return nil, fmt.Errorf("decode %s/routez: %w", c.monitors[i], err)
	}
	names := make([]string, 0, len(routez.Routes))
	for _, route := range routez.Routes {
		names = append(names, route.Name+"@"+route.IP)
	}
	return names, nil
}

// memberName is member i's server_name, and its name on the cluster's network.
func memberName(i int) string { return fmt.Sprintf("n%d", i+1) }

// Name is member i's server_name, as a stream's cluster info names its leader and replicas.
func (c *Cluster) Name(i int) string { return memberName(i) }

// Member is the index of the member whose server_name is name, or -1 for none.
func (c *Cluster) Member(name string) int {
	for i := range clusterSize {
		if memberName(i) == name {
			return i
		}
	}
	return -1
}

// Connect is a client of member i alone, named after t and closed when t ends, with its
// JetStream. It ignores the other members' addresses the server tells it, so a connection Docker
// drops as it takes a member off a network (Isolate) comes back to member i, never to another
// member a read would then be served by.
func (c *Cluster) Connect(t testing.TB, i int, options ...nats.Option) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	conn, err := nats.Connect(c.urls[i], append([]nats.Option{nats.Name(t.Name() + " " + memberName(i)), nats.Timeout(5 * time.Second),
		nats.IgnoreDiscoveredServers(), nats.MaxReconnects(-1), nats.ReconnectWait(50 * time.Millisecond)}, options...)...)
	if err != nil {
		t.Fatalf("connect to member %s: %v", memberName(i), err)
	}
	t.Cleanup(conn.Close)
	if name := conn.ConnectedServerName(); name != memberName(i) {
		t.Fatalf("the client of member %s reached %s", memberName(i), name)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open member %s's JetStream: %v", memberName(i), err)
	}
	return conn, js
}

// Isolate takes each of members off the cluster's network, all before it waits for any, and
// returns once each is cut off: it holds no route, and no other member holds one to it, so it
// shares nothing with the others and serves its clients from what it alone holds. A route outlives
// the cut until its pings go unanswered (clusterConfig), and until then a message can still reach
// the member. Members cut together leave the others no moment with only some of them gone: two
// cut one after the other could let the member left win a stream's election with the second
// before it went, and then lead alone with no quorum.
func (c *Cluster) Isolate(t testing.TB, members ...int) {
	t.Helper()
	for _, i := range members {
		if err := c.docker.NetworkDisconnect(context.Background(), c.network.ID, c.members[i].GetContainerID(), true); err != nil {
			t.Fatalf("take member %s off the cluster's network: %v", memberName(i), err)
		}
	}
	deadline := time.Now().Add(clusterReadyTimeout)
	for _, i := range members {
		for {
			err := c.cutOff(i)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("member %s was not cut off within %s of leaving the cluster's network: %v", memberName(i), clusterReadyTimeout, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// cutOff is why member i is not cut off from the others yet, or nil once it is (Isolate).
func (c *Cluster) cutOff(i int) error {
	for j := range clusterSize {
		routes, err := c.Routes(j)
		if err != nil {
			return fmt.Errorf("member %s's routes: %w", memberName(j), err)
		}
		for _, route := range routes {
			if j == i || strings.HasPrefix(route, memberName(i)+"@") {
				return fmt.Errorf("member %s still holds the route %s", memberName(j), route)
			}
		}
	}
	return nil
}

// Rejoin puts member i back on the cluster's network under its name, where its peers' routes find
// it again.
func (c *Cluster) Rejoin(t testing.TB, i int) {
	t.Helper()
	if err := c.docker.NetworkConnect(context.Background(), c.network.ID, c.members[i].GetContainerID(),
		&dockernetwork.EndpointSettings{Aliases: []string{memberName(i)}}); err != nil {
		t.Fatalf("put member %s on the cluster's network: %v", memberName(i), err)
	}
}

// AwaitReady waits, within clusterReadyTimeout, until every member follows one meta leader that
// counts all three as current peers, and answers a JetStream account info.
func (c *Cluster) AwaitReady(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(clusterReadyTimeout)
	for {
		err := c.metaReady()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster's JetStream was not ready within %s: %v", clusterReadyTimeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// metaReady is why the cluster is not ready yet, or nil once it is (AwaitReady).
func (c *Cluster) metaReady() error {
	leader := ""
	for i, monitoring := range c.monitors {
		jsz, err := readJsz(monitoring)
		if err != nil {
			return fmt.Errorf("member %s's /jsz: %w", memberName(i), err)
		}
		if jsz.Meta.Leader == "" || jsz.Meta.Size != clusterSize {
			return fmt.Errorf("member %s follows meta leader %q of a group of %d", memberName(i), jsz.Meta.Leader, jsz.Meta.Size)
		}
		if leader != "" && jsz.Meta.Leader != leader {
			return fmt.Errorf("member %s follows meta leader %s, another member %s", memberName(i), jsz.Meta.Leader, leader)
		}
		leader = jsz.Meta.Leader
		if jsz.Meta.Leader == memberName(i) {
			current := 0
			for _, peer := range jsz.Meta.Replicas {
				if peer.Current && !peer.Offline {
					current++
				}
			}
			if current != clusterSize-1 {
				return fmt.Errorf("meta leader %s counts %d current peers online, want %d", memberName(i), current, clusterSize-1)
			}
		}
	}
	for i, url := range c.urls {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, _, err := ready(ctx, url, "cluster readiness "+memberName(i))
		cancel()
		if err != nil {
			return fmt.Errorf("member %s's JetStream: %w", memberName(i), err)
		}
		conn.Close()
	}
	return nil
}

// jsz is the part of a member's /jsz the readiness reads: the JetStream meta group as it sees it.
type jsz struct {
	Meta struct {
		Leader   string `json:"leader"`
		Size     int    `json:"cluster_size"`
		Replicas []struct {
			Name    string `json:"name"`
			Current bool   `json:"current"`
			Offline bool   `json:"offline"`
		} `json:"replicas"`
	} `json:"meta_cluster"`
}

func readJsz(monitoring string) (jsz, error) {
	response, err := monitorClient.Get(monitoring + "/jsz")
	if err != nil {
		return jsz{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return jsz{}, fmt.Errorf("GET %s/jsz answered %s", monitoring, response.Status)
	}
	var got jsz
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		return jsz{}, fmt.Errorf("decode %s/jsz: %w", monitoring, err)
	}
	return got, nil
}
