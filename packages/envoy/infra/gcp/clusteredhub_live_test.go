//go:build natslive

package gcp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The tests in this file run what gcp:ClusteredNatsHub deploys on this machine, so they carry the
// natslive tag. They need no container runtime: each installs the tools it runs with go install,
// at the versions the hub runs, into its own temporary directory, and a tool that does not install
// fails the test. The chart is the vendored archive, so no test reaches the chart repository.
//
// The tools are the helm pulumi-kubernetes v4.34.2's provider renders charts with (its
// provider/go.mod), and the nats-server, config reloader and exporter chart 2.15.1 runs by default
// (values.yaml: container.image.tag 2.15.1-alpine, reloader.image.tag 0.24.0,
// promExporter.image.tag 0.20.2), the versions the hub's pinned images hold.
const (
	helmTool       = "helm.sh/helm/v3/cmd/helm@v3.20.2"
	natsServerTool = "github.com/nats-io/nats-server/v2@v2.15.1"
	reloaderTool   = "github.com/nats-io/nack/cmd/nats-server-config-reloader@v0.24.0"
	exporterTool   = "github.com/nats-io/prometheus-nats-exporter@v0.20.2"

	// clusteredHubChartSHA256 is the SHA-256 the chart index
	// (https://nats-io.github.io/k8s/helm/charts/index.yaml) publishes for nats 2.15.1, whose
	// archive is vendored in testdata.
	clusteredHubChartSHA256 = "b209c62784b1b24c714d5ff6503b4c7dd948ec187d4222beee16b579f1258cc1"
)

// installTool installs module, a main package at a pinned version, with go install into dir, and
// returns the path of its binary, named binary, or why it did not install.
func installTool(dir, module, binary string) (string, error) {
	cmd := exec.Command("go", "install", module)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install %s: %v\n%s", module, err, out)
	}
	path := filepath.Join(dir, binary)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("go install %s wrote no %s: %w", module, binary, err)
	}
	return path, nil
}

// liveTool is a main package at a pinned version and the name of the binary it installs.
type liveTool struct{ module, binary string }

// installTools installs each of tools with installTool into dir, all at once, and returns their
// binaries' paths in tools' order, failing the test with each tool that did not install.
func installTools(t *testing.T, dir string, tools ...liveTool) []string {
	t.Helper()
	paths := make([]string, len(tools))
	errs := make([]error, len(tools))
	var wg sync.WaitGroup
	for i, tool := range tools {
		wg.Go(func() { paths[i], errs[i] = installTool(dir, tool.module, tool.binary) })
	}
	wg.Wait()
	require.NoError(t, errors.Join(errs...))
	return paths
}

// liveHubKeys are the keys of the users the live hub serves: the operator's and the host's seeds,
// which the test's clients connect with, and the session user's public key, whose seed nobody
// keeps.
type liveHubKeys struct {
	operatorSeed, operatorKey string
	hostSeed, hostKey         string
	sessionKey                string
}

// liveHubArgs are clusteredArgs with keys' users: an operator with explicit permissions, a host
// holding core.HostPermissions and the session user, a storage class, a priority class, sizes
// other than the defaults, and a client Service named apart from the release, so each reaches the
// rendering by its own value.
func liveHubArgs(keys liveHubKeys) *gcp.ClusteredNatsHubArgs {
	args := clusteredArgs()
	args.ServiceName = "envoy-nats-client"
	args.StorageClassName = pulumi.StringRef("premium-rwo")
	args.DiskSizeGb = pulumi.IntRef(12)
	args.MaxFileStoreGb = pulumi.IntRef(8)
	args.PriorityClassName = pulumi.StringRef("envoy-critical")
	args.Users = []gcp.NATSUserArgs{
		{Name: "live-operator", NKey: keys.operatorKey, Permissions: &gcp.PermissionsArgs{
			Publish:   nats.RulesArgs{Allow: []string{">"}},
			Subscribe: nats.RulesArgs{Allow: []string{">"}, Deny: []string{"$SYS.>"}},
		}},
		{Name: "live-host", NKey: keys.hostKey, MachineID: pulumi.StringRef("live-host")},
	}
	args.SessionUser = &gcp.SessionUserArgs{Name: "live-sessions", NKey: keys.sessionKey}
	return args
}

// liveAuthConf is the auth.conf the hub renders for args, the text its chart's ConfigMap holds.
func liveAuthConf(t *testing.T, args *gcp.ClusteredNatsHubArgs) string {
	t.Helper()
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	return s.AuthConf
}

// clusteredHubRender is what helm renders from the values the hub registers its chart with: the
// manifests, and the ConfigMap's nats.conf and auth.conf, for the users of keys.
type clusteredHubRender struct {
	keys               liveHubKeys
	settings           gcp.ClusteredNatsHubSettings
	archive            string
	release, namespace string
	manifests          []map[string]any
	natsConf, authConf string
}

var (
	renderMu sync.Mutex
	rendered *clusteredHubRender
)

// renderClusteredHub renders the hub's chart for liveHubArgs with fresh keys once per test binary,
// so the cluster test runs the configuration the chart test checked.
func renderClusteredHub(t *testing.T) *clusteredHubRender {
	t.Helper()
	renderMu.Lock()
	defer renderMu.Unlock()
	if rendered != nil {
		return rendered
	}
	keys := newLiveHubKeys(t)
	rendered = renderClusteredHubArgs(t, keys, liveHubArgs(keys))
	return rendered
}

// newLiveHubKeys makes fresh keys for the live hub's users.
func newLiveHubKeys(t *testing.T) liveHubKeys {
	t.Helper()
	var keys liveHubKeys
	keys.operatorSeed, keys.operatorKey = testnats.User(t)
	keys.hostSeed, keys.hostKey = testnats.User(t)
	_, keys.sessionKey = testnats.User(t)
	return keys
}

// renderClusteredHubArgs renders the hub's chart for args, whose users hold keys: it runs the hub
// under the mocks for args, writes the values its chart registration holds to a file, and runs helm
// template on the vendored archive of the chart and version the registration names, under its
// release name and namespace, after checking the archive's SHA-256.
func renderClusteredHubArgs(t *testing.T, keys liveHubKeys, args *gcp.ClusteredNatsHubArgs) *clusteredHubRender {
	t.Helper()
	settings, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)

	m := runClusteredHub(t, args)
	chart := m.One(t, chartType)
	release, namespace := chart.Inputs["name"].StringValue(), chart.Inputs["namespace"].StringValue()
	archive := filepath.Join("testdata", chart.Inputs["chart"].StringValue()+"-"+chart.Inputs["version"].StringValue()+".tgz")
	require.Equal(t, clusteredHubChartSHA256, fileSHA256(t, archive), "%s is the archive the chart index publishes", archive)

	dir := t.TempDir()
	values, err := json.Marshal(chartValues(t, m))
	require.NoError(t, err)
	valuesFile := filepath.Join(dir, "values.json")
	require.NoError(t, os.WriteFile(valuesFile, values, 0o600))
	helm, err := installTool(dir, helmTool, "helm")
	require.NoError(t, err)
	home := filepath.Join(dir, "helm-home")
	cmd := exec.Command(helm, "template", release, archive, "-n", namespace, "-f", valuesFile)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+home, "HELM_CONFIG_HOME="+home, "HELM_DATA_HOME="+home)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "helm template: %s", stderr.String())

	r := &clusteredHubRender{
		keys: keys, settings: settings, archive: archive, release: release, namespace: namespace,
		manifests: decodeManifests(t, out),
	}
	data := r.one(t, "ConfigMap", release+"-config")["data"].(map[string]any)
	r.natsConf, r.authConf = data["nats.conf"].(string), data[clusteredHubAuthConfKey].(string)
	t.Logf("%s rendered release %s in namespace %s from %s (sha256 %s) with the values of the hub's chart registration: %d manifests",
		helmTool, release, namespace, archive, clusteredHubChartSHA256, len(r.manifests))
	return r
}

// clusteredHubAuthConfKey is the ConfigMap key holding the users' authorization, which nats.conf
// includes.
const clusteredHubAuthConfKey = "auth.conf"

// fileSHA256 is the hex SHA-256 of the file at path.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// decodeManifests decodes helm template's output, a YAML stream, into its non-empty documents.
func decodeManifests(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var manifests []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return manifests
		}
		require.NoError(t, err)
		if doc != nil {
			manifests = append(manifests, doc)
		}
	}
}

// one is the rendered manifest of kind named name, failing the test unless there is exactly one.
func (r *clusteredHubRender) one(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, m := range r.manifests {
		if m["kind"] == kind && dig(t, m, "metadata", "name") == name {
			found = append(found, m)
		}
	}
	require.Len(t, found, 1, "the rendering holds one %s %s", kind, name)
	return found[0]
}

// container is the hub pods' container named name, as the rendered StatefulSet holds it.
func (r *clusteredHubRender) container(t *testing.T, name string) map[string]any {
	t.Helper()
	return podContainer(t, dig(t, r.one(t, "StatefulSet", r.release), "spec", "template").(map[string]any), name)
}

// dig is the value at path in v, each step a key of an object or an index of an array, failing
// the test when a step is missing.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for i, step := range path {
		switch key := step.(type) {
		case string:
			object, ok := v.(map[string]any)
			require.True(t, ok, "%v is an object", path[:i])
			v, ok = object[key]
			require.True(t, ok, "%v is set", path[:i+1])
		case int:
			array, ok := v.([]any)
			require.True(t, ok && key < len(array), "%v has an item %d", path[:i], key)
			v = array[key]
		}
	}
	return v
}

// stringMap is v, a YAML object of strings, as a map of strings.
func stringMap(t *testing.T, v any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for key, value := range v.(map[string]any) {
		s, ok := value.(string)
		require.True(t, ok, "%s is a string", key)
		out[key] = s
	}
	return out
}

// stringList is v, a YAML array of strings, as a slice.
func stringList(t *testing.T, v any) []string {
	t.Helper()
	var out []string
	for _, item := range v.([]any) {
		s, ok := item.(string)
		require.True(t, ok, "%v is a string", item)
		out = append(out, s)
	}
	return out
}

// The chart the hub installs renders what the hub's values say. helm, at the version
// pulumi-kubernetes's provider renders with, renders the vendored chart from the values the hub
// registers its chart with: one StatefulSet of three servers spread one per zone, with their
// claims, images, priority class and session-user annotation and no config checksum; the reloader
// watching auth.conf beside nats.conf; the ConfigMap holding the hub's auth.conf and a nats.conf
// that includes it, clusters and runs JetStream within the store size; the budget; and the client
// Service, on the nats port alone. No nats-box Deployment and no test Pod.
func TestClusteredNatsHubChart(t *testing.T) {
	r := renderClusteredHub(t)
	s := r.settings
	t.Logf("chart archive %s sha256 %s", r.archive, clusteredHubChartSHA256)

	var kinds []string
	for _, m := range r.manifests {
		kinds = append(kinds, fmt.Sprintf("%s %s", m["kind"], dig(t, m, "metadata", "name")))
	}
	t.Logf("rendered: %s", strings.Join(kinds, ", "))
	require.ElementsMatch(t, []string{
		"StatefulSet " + s.Name, "Service " + s.ServiceName, "Service " + s.Name + "-headless",
		"ConfigMap " + s.Name + "-config", "PodDisruptionBudget " + s.Name,
	}, kinds, "no nats-box Deployment, no test Pod, nothing else")

	statefulSet := r.one(t, "StatefulSet", s.Name)
	require.EqualValues(t, 3, dig(t, statefulSet, "spec", "replicas"))
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, statefulSet, "spec", "selector", "matchLabels")),
		"the hub's selector is the StatefulSet's")
	// pulumi-kubernetes applies the StatefulSet only after the object this annotation names, which
	// must be the chart's own ConfigMap, the one holding nats.conf and auth.conf, or the order would
	// not hold.
	configMap := r.one(t, "ConfigMap", s.Name+"-config")
	require.Contains(t, dig(t, configMap, "data").(map[string]any), "auth.conf")
	require.Contains(t, dig(t, configMap, "data").(map[string]any), "nats.conf")
	require.Equal(t, "/namespaces/"+r.namespace+"/ConfigMap/"+s.Name+"-config",
		dig(t, statefulSet, "metadata", "annotations", "config.kubernetes.io/depends-on"),
		"the StatefulSet is applied after the ConfigMap holding nats.conf and auth.conf")
	pod := dig(t, statefulSet, "spec", "template")
	spread := dig(t, pod, "spec", "topologySpreadConstraints").([]any)
	require.Len(t, spread, 1)
	require.Equal(t, "topology.kubernetes.io/zone", dig(t, spread[0], "topologyKey"))
	require.EqualValues(t, 1, dig(t, spread[0], "maxSkew"))
	require.EqualValues(t, 3, dig(t, spread[0], "minDomains"))
	require.Equal(t, "DoNotSchedule", dig(t, spread[0], "whenUnsatisfiable"))
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, spread[0], "labelSelector", "matchLabels")))

	claims := dig(t, statefulSet, "spec", "volumeClaimTemplates").([]any)
	require.Len(t, claims, 1)
	require.Equal(t, fmt.Sprintf("%dGi", s.DiskSizeGb), dig(t, claims[0], "spec", "resources", "requests", "storage"))
	require.Equal(t, s.StorageClassName, dig(t, claims[0], "spec", "storageClassName"))

	var names []string
	for _, c := range dig(t, pod, "spec", "containers").([]any) {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	require.Equal(t, []string{"nats", "reloader", "prom-exporter"}, names)
	require.Equal(t, s.NatsImage, r.container(t, "nats")["image"])
	require.Equal(t, s.ReloaderImage, r.container(t, "reloader")["image"])
	require.Equal(t, s.ExporterImage, r.container(t, "prom-exporter")["image"])
	// nats-server runs under GOMEMLIMIT at 80% of its memory limit, the chart's container.env
	// rendered as a plain env var: liveHubArgs leave natsMemory at 2Gi, so ⌊0.8 × 2048⌋ MiB.
	require.Equal(t, "2Gi", s.NatsMemory)
	require.Equal(t, "2Gi", dig(t, r.container(t, "nats"), "resources", "limits", "memory"))
	require.Contains(t, r.container(t, "nats")["env"], map[string]any{"name": "GOMEMLIMIT", "value": "1638MiB"})
	require.Equal(t, []string{
		"-pid", "/var/run/nats/nats.pid", "-config", "/etc/nats-config/nats.conf", "-config", "/etc/nats-config/auth.conf",
	}, stringList(t, r.container(t, "reloader")["args"]), "the reloader watches auth.conf beside nats.conf")
	require.Equal(t, []any{map[string]any{"name": "prom-metrics", "containerPort": 7777}},
		r.container(t, "prom-exporter")["ports"], "the exporter's port is the one the PodMonitoring scrapes")

	annotations := dig(t, pod, "metadata", "annotations").(map[string]any)
	require.NotContains(t, annotations, "checksum/config")
	require.Equal(t, map[string]any{
		"envoy-infra.sjawhar.dev/no-auth-user":   r.keys.sessionKey,
		"envoy-infra.sjawhar.dev/max-file-store": fmt.Sprintf("%dGi", s.MaxFileStoreGb),
	}, annotations, "the two values nats-server cannot reload roll the pods")
	require.Equal(t, s.PriorityClassName, dig(t, pod, "spec", "priorityClassName"))

	require.Equal(t, s.AuthConf, r.authConf, "the ConfigMap's auth.conf is the hub's rendering")
	require.Regexp(t, `(?m)^\s*include auth\.conf;$`, r.natsConf)
	compact := strings.Join(strings.Fields(r.natsConf), "")
	var routes []string
	for i := range 3 {
		routes = append(routes, fmt.Sprintf(`"nats://%s-%d.%s-headless:6222"`, s.Name, i, s.Name))
	}
	require.Contains(t, compact, fmt.Sprintf(`"cluster":{"name":%q,"no_advertise":true,"port":6222,"routes":[%s]}`, s.Name, strings.Join(routes, ",")))
	require.Contains(t, compact, fmt.Sprintf(`"jetstream":{"max_file_store":%dGi,"max_memory_store":0,"store_dir":"/data"}`, s.MaxFileStoreGb))
	t.Logf("nats.conf:\n%s", r.natsConf)

	budget := r.one(t, "PodDisruptionBudget", s.Name)
	require.EqualValues(t, 1, dig(t, budget, "spec", "maxUnavailable"))
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, budget, "spec", "selector", "matchLabels")), "the hub's selector is the budget's")

	service := r.one(t, "Service", s.ServiceName)
	require.Equal(t, s.PodSelector, stringMap(t, dig(t, service, "spec", "selector")), "the hub's selector is the client Service's")
	ports := dig(t, service, "spec", "ports").([]any)
	require.Len(t, ports, 1, "the client Service exposes one port")
	require.Equal(t, "nats", dig(t, ports[0], "name"))
	require.EqualValues(t, 4222, dig(t, ports[0], "port"))
}

// hubServer is one nats-server of the local cluster, on the rendered nats.conf and auth.conf in a
// directory of its own. nats-server takes no socket from the test, so at each start it binds a
// client, a monitoring and a route port the kernel chooses, and logs each one (start). The test
// holds a front for each of the three (testnats.Front) from newHubServer until it ends, and gives
// every client, peer and exporter a front's address, which relays to the port the running server
// bound: a restarted server keeps its three addresses, as a restarted pod keeps its own, and no
// address the test hands out is a port that was chosen before the server bound it.
type hubServer struct {
	name         string
	dir          string
	binary       string
	podArgs, env []string
	// client, monitor and route are the server's fronts, each relaying to its port in bound.
	client, monitor, route *testnats.Front
	// peers are the servers whose route fronts it solicits, itself included, as the chart's
	// cluster.routes names all three pods; nats-server ignores a route to itself.
	peers []*hubServer
	// routeURLs, when set, are routes it solicits in place of its peers' route fronts, as a route
	// through a proxy.
	routeURLs []string
	proc      *cmdtest.Process

	mu sync.Mutex
	// bound is the ports the running server bound and logged at its start, all zero from the
	// moment the test stops it (stop).
	bound hubPorts
}

// hubPorts are a nats-server's client, monitoring and route ports.
type hubPorts struct{ client, monitor, route int }

// newHubServer is a server named name of the pods r renders, on natsConf and r's auth.conf, as the
// pod runs nats-server: the container's arguments with the ConfigMap's mount replaced by the
// server's directory, and SERVER_NAME set to its pod's name. Its three fronts are held from here
// until the test ends, so the servers' routes can name each other's before any of them starts.
func (r *clusteredHubRender) newHubServer(t *testing.T, name, binary, natsConf string) *hubServer {
	t.Helper()
	s := &hubServer{name: name, dir: t.TempDir(), binary: binary}
	s.client, s.monitor, s.route = s.front(t, &s.bound.client), s.front(t, &s.bound.monitor), s.front(t, &s.bound.route)
	// Two values are replaced in the server's copy of nats.conf, since no command-line override
	// serves for either. jetstream.store_dir: /data, the pod's claim: nats-server v2.15.1 reads the
	// file onto the options its flags set and refuses -sd beside a store_dir in the file ("Duplicate
	// 'store_dir' configuration").
	storeDir := `"store_dir": "/data"`
	require.Equal(t, 1, strings.Count(natsConf, storeDir), "the rendered nats.conf sets %s once", storeDir)
	natsConf = strings.Replace(natsConf, storeDir, fmt.Sprintf(`"store_dir": %q`, filepath.Join(s.dir, "data")), 1)
	// cluster.port: 6222, the route port, which becomes -1, one the kernel chooses as the server
	// binds it. --cluster nats://127.0.0.1:-1 binds such a port too, but nats-server v2.15.1 keeps
	// that flag as nats://127.0.0.1:0 (server/opts.go overrideCluster), so every reload reads route
	// port 0 and refuses the whole configuration ("config reload not supported for cluster port:
	// old=<port>, new=0"); a -1 in the file is put back to the bound port on a reload
	// (server/reload.go ReloadOptions). The pod's route port listens on every interface, and so
	// does this one.
	clusterPort := `"port": 6222`
	require.Equal(t, 1, strings.Count(natsConf, clusterPort), "the rendered nats.conf sets %s once, in its cluster block", clusterPort)
	natsConf = strings.Replace(natsConf, clusterPort, `"port": -1`, 1)
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "nats.conf"), []byte(natsConf), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, clusteredHubAuthConfKey), []byte(r.authConf), 0o600))
	s.podArgs = podArgs(t, stringList(t, r.container(t, "nats")["args"]), map[string]string{"/etc/nats-config/": s.dir + "/"})
	// server_name: $SERVER_NAME, which the container sets to its pod's name.
	require.Contains(t, r.container(t, "nats")["env"], map[string]any{"name": "SERVER_NAME", "value": "$(POD_NAME)"},
		"each server is named after its pod")
	s.env = []string{"SERVER_NAME=" + s.name}
	return s
}

// front holds a front for one of the server's ports, which relays to *port, a field of s.bound,
// while the server runs, and closes each connection while it does not.
func (s *hubServer) front(t *testing.T, port *int) *testnats.Front {
	t.Helper()
	return testnats.StartFront(t, func() (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if *port == 0 {
			return "", fmt.Errorf("%s is not running", s.name)
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)), nil
	})
}

// commandArgs is the container's own arguments (--config /etc/nats-config/nats.conf, with the
// ConfigMap's mount replaced by the server's directory, podArgs), then -D, the one argument the
// test adds for itself, then a command-line override for each other value of nats.conf that a pod
// fixes, since three servers here share one host and filesystem. Each of its three ports is -1, one
// the kernel chooses as the server binds it (the route port's in the server's nats.conf,
// newHubServer), behind the server's front for it.
func (s *hubServer) commandArgs() []string {
	routes := s.routeURLs
	if routes == nil {
		for _, peer := range s.peers {
			routes = append(routes, peer.routeURL())
		}
	}
	return append(slices.Clone(s.podArgs),
		// -D, debug logging, which no pod runs with: nats-server v2.15.1 prints a Raft group's
		// election and vote lines only at debug level (server/raft.go: initRaftNode sets the node's
		// dflag from the server's debug flag, and debug prints nothing without it), and startLogged
		// prints a server's output only when the test failed, so a failed election shows its votes.
		"-D",
		// port: 4222, the client port each pod has to itself.
		"-p", "-1",
		// http_port: 8222, the monitoring port.
		"-m", "-1",
		// cluster.advertise, which the chart leaves unset because a pod's route port keeps its pod's
		// address: here the route front, which keeps the server's, so a peer that learns of this
		// server from another's gossip routes to it through the front as well (server/route.go,
		// processImplicitRoute dials the INFO's IP, which advertise sets), never to a port the server
		// bound at an earlier start.
		"--cluster_advertise", s.route.Addr(),
		// cluster.routes, the three pods' DNS names in the headless Service, each pod's own
		// included: here every peer's route front, its own included, or routeURLs.
		"--routes", strings.Join(routes, ","),
		// pid_file: /var/run/nats/nats.pid, the emptyDir the pod shares with the reloader.
		"-P", filepath.Join(s.dir, "nats.pid"),
	)
}

// routeRemotes is the set of servers the server's /routez names at the other end of its routes.
func (s *hubServer) routeRemotes() (map[string]bool, error) {
	var routez struct {
		Routes []struct {
			RemoteName string `json:"remote_name"`
		} `json:"routes"`
	}
	if err := s.monitorJSON("/routez", &routez); err != nil {
		return nil, err
	}
	remotes := map[string]bool{}
	for _, route := range routez.Routes {
		remotes[route.RemoteName] = true
	}
	return remotes, nil
}

// The lines nats-server v2.15.1 logs once it has bound each of its ports, with the port the kernel
// chose (server/server.go:2836-2837 and 3171-3172, server/route.go:2760-2761).
var (
	clientBound  = regexp.MustCompile(`Listening for client connections on \S+:(\d+)`)
	monitorBound = regexp.MustCompile(`Starting http monitor on \S+:(\d+)`)
	routeBound   = regexp.MustCompile(`Listening for route connections on \S+:(\d+)`)
)

// start starts the server and waits until it has logged the client, monitoring and route ports it
// bound, then points its fronts at them; the test kills it by its PID when it ends if it is still
// running, after it has stopped the fronts (stop): the test runs its cleanups last first, so the
// stop registered here runs before the kill startServing registered. Each start binds three ports
// the kernel chooses, so a restart takes no port the server gave up, which another program can hold
// by then, and the fronts keep the addresses every client, peer and exporter was given, as a
// restarted pod keeps its address.
func (s *hubServer) start(t *testing.T) {
	t.Helper()
	cmd := exec.Command(s.binary, s.commandArgs()...)
	cmd.Env = append(os.Environ(), s.env...)
	var bound hubPorts
	s.proc = startServing(t, s.name, cmd, func(p *cmdtest.Process) error {
		out := p.Output.String()
		for _, port := range []struct {
			line *regexp.Regexp
			into *int
		}{{clientBound, &bound.client}, {monitorBound, &bound.monitor}, {routeBound, &bound.route}} {
			m := port.line.FindStringSubmatch(out)
			if m == nil {
				return fmt.Errorf("no line matching %q yet", port.line)
			}
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return err
			}
			*port.into = n
		}
		return nil
	})
	t.Cleanup(s.stop)
	s.mu.Lock()
	s.bound = bound
	s.mu.Unlock()
	t.Logf("%s (pid %d) bound client port %d, monitoring port %d and route port %d, behind its fronts %s, %s and %s",
		s.name, s.proc.Cmd.Process.Pid, bound.client, bound.monitor, bound.route, s.client.Addr(), s.monitor.Addr(), s.route.Addr())
}

// startLogged starts cmd as cmdtest.Start does, and logs its output when the test fails.
func startLogged(t *testing.T, name string, cmd *exec.Cmd) *cmdtest.Process {
	t.Helper()
	p := cmdtest.Start(t, name, cmd)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s output:\n%s", name, p.Output.String())
		}
	})
	return p
}

// startServing starts cmd as startLogged does and waits up to 30 s for ready to return nil, failing
// the test with the process's output when it does not. Every port a process here serves on is one
// the test holds or one the process bound as the kernel chose it, so no other program can hold a
// port it binds, and a start is never retried.
func startServing(t *testing.T, name string, cmd *exec.Cmd, ready func(*cmdtest.Process) error) *cmdtest.Process {
	t.Helper()
	p := startLogged(t, name, cmd)
	if err := awaitReady(p, ready); err != nil {
		t.Fatalf("%s did not start: %v\n%s", name, err, p.Output.String())
	}
	return p
}

// awaitReady waits up to 30 s for ready to return nil while p runs, and returns ready's last error
// otherwise.
func awaitReady(p *cmdtest.Process, ready func(*cmdtest.Process) error) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-p.Exited:
			return errors.New("the process exited")
		default:
		}
		err := ready(p)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// kill sends the server SIGKILL and waits for it to exit, its fronts refusing new connections from
// before the signal (stop).
func (s *hubServer) kill(t *testing.T) {
	t.Helper()
	s.stop()
	require.NoError(t, s.proc.Cmd.Process.Signal(syscall.SIGKILL))
	s.proc.WaitExit(t, "SIGKILL")
}

// terminate sends the server SIGTERM, as a deploy stops a pod, and waits for it to exit, its
// fronts refusing new connections from before the signal (stop).
func (s *hubServer) terminate(t *testing.T) {
	t.Helper()
	s.stop()
	s.proc.Terminate(t)
	s.proc.WaitExit(t, "SIGTERM")
}

// stop points the server's fronts at nothing, before the signal that stops the server (kill,
// terminate, and the kill at the test's end, start): from then on each closes a new connection at
// once, as a stopped pod's address refuses it, so none is relayed to a port the server is giving
// up, which another program can bind once it has. A connection the fronts relay already ends when
// the server does.
func (s *hubServer) stop() {
	s.mu.Lock()
	s.bound = hubPorts{}
	s.mu.Unlock()
}

// TestHubServerFrontsStopBeforeItsServerAtTheEnd ends a test that started a hubServer and reads,
// the moment the server has exited, where its fronts relay. A front that still relayed to the ports
// the server had bound would hand a connection to a port the server gave up, which another program
// can bind by then, until the front itself closes, after every later cleanup of the test. The
// server is a stand-in that logs the three lines start reads, as nats-server does, for three ports
// the test holds, since only the order of the end is under test.
func TestHubServerFrontsStopBeforeItsServerAtTheEnd(t *testing.T) {
	atExit := make(chan hubPorts, 1)
	if !t.Run("a test that starts a server", func(t *testing.T) {
		dir := t.TempDir()
		var lines string
		for _, what := range []string{"Listening for client connections on", "Starting http monitor on", "Listening for route connections on"} {
			held, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = held.Close() })
			lines += fmt.Sprintf("echo '[INF] %s %s'\n", what, held.Addr())
		}
		binary := filepath.Join(dir, "nats-server")
		require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\n"+lines+"exec sleep 600\n"), 0o700))
		s := &hubServer{name: "stand-in", dir: dir, binary: binary}
		s.client, s.monitor, s.route = s.front(t, &s.bound.client), s.front(t, &s.bound.monitor), s.front(t, &s.bound.route)
		s.start(t)
		go func() {
			<-s.proc.Exited
			s.mu.Lock()
			defer s.mu.Unlock()
			atExit <- s.bound
		}()
	}) {
		return
	}
	select {
	case bound := <-atExit:
		require.Equal(t, hubPorts{}, bound, "the fronts relayed to the ports the server had bound at the moment it exited")
	case <-time.After(30 * time.Second):
		t.Fatal("the stand-in server had not exited 30 s after its test ended")
	}
}

func (s *hubServer) url() string { return "nats://" + s.client.Addr() }

// routeURL is the server's route front as a route URL.
func (s *hubServer) routeURL() string { return "nats://" + s.route.Addr() }

// monitorJSON decodes the answer of the server's monitoring endpoint path into into.
func (s *hubServer) monitorJSON(path string, into any) error {
	return getJSON("http://"+s.monitor.Addr()+path, into)
}

var monitorClient = &http.Client{Timeout: 2 * time.Second}

func getJSON(url string, into any) error {
	resp, err := monitorClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// streamLastSeq is the last sequence of the server's own copy of stream, from its /jsz.
func (s *hubServer) streamLastSeq(stream string) (uint64, error) {
	var jsz struct {
		Accounts []struct {
			Streams []struct {
				Name  string `json:"name"`
				State struct {
					LastSeq uint64 `json:"last_seq"`
				} `json:"state"`
			} `json:"stream_detail"`
		} `json:"account_details"`
	}
	if err := s.monitorJSON("/jsz?accounts=true&streams=true", &jsz); err != nil {
		return 0, err
	}
	for _, a := range jsz.Accounts {
		for _, st := range a.Streams {
			if st.Name == stream {
				return st.State.LastSeq, nil
			}
		}
	}
	return 0, fmt.Errorf("%s holds no copy of %s", s.name, stream)
}

// metaCluster is the JetStream meta group's leader and size as the server's /jsz reports them.
// The leader is the empty string while the group has none.
func (s *hubServer) metaCluster() (leader string, size int, err error) {
	var jsz struct {
		Meta struct {
			Leader string `json:"leader"`
			Size   int    `json:"cluster_size"`
		} `json:"meta_cluster"`
	}
	if err := s.monitorJSON("/jsz", &jsz); err != nil {
		return "", 0, err
	}
	return jsz.Meta.Leader, jsz.Meta.Size, nil
}

// agreedMetaLeader is the JetStream meta leader every server of servers names, or why they name
// none: a server whose /jsz did not answer, one that names no leader, or two that disagree.
func agreedMetaLeader(servers []*hubServer) (string, error) {
	agreed := ""
	for _, s := range servers {
		l, _, err := s.metaCluster()
		if err != nil {
			return "", err
		}
		if l == "" {
			return "", fmt.Errorf("%s names no JetStream meta leader", s.name)
		}
		if agreed != "" && l != agreed {
			return "", fmt.Errorf("the servers name the JetStream meta leaders %s and %s", agreed, l)
		}
		agreed = l
	}
	return agreed, nil
}

// authorizedUser is the user the server authorized conn's connection as: an nkey user's public
// key, the session user's for a client presenting no credential.
func (s *hubServer) authorizedUser(t *testing.T, conn *natsgo.Conn) string {
	t.Helper()
	cid, err := conn.GetClientID()
	require.NoError(t, err)
	var connz struct {
		Connections []struct {
			AuthorizedUser string `json:"authorized_user"`
		} `json:"connections"`
	}
	require.NoError(t, s.monitorJSON(fmt.Sprintf("/connz?auth=true&cid=%d", cid), &connz))
	require.Len(t, connz.Connections, 1, "%s has the connection %d", s.name, cid)
	return connz.Connections[0].AuthorizedUser
}

// count is how often the server's output holds text.
func (s *hubServer) count(text string) int { return strings.Count(s.proc.Output.String(), text) }

// within calls check every 100 ms until it returns nil, and returns how long that took, failing
// the test with check's last error when timeout passes first.
func within(t *testing.T, timeout time.Duration, what string, check func() error) time.Duration {
	t.Helper()
	start := time.Now()
	var err error
	if !testwait.Holds(timeout, 100*time.Millisecond, func() bool {
		err = check()
		return err == nil
	}) {
		t.Fatalf("%s: not within %s: %v", what, timeout, err)
	}
	return time.Since(start)
}

// stepDownStreamLeader asks the cluster to move stream's leadership to the server named preferred,
// and returns the answer's body as text.
//
// Who answers $JS.API.STREAM.LEADER.STEPDOWN (nats-server v2.15.1, server/jetstream_api.go,
// jsStreamLeaderStepDownRequest): a member that knows the JetStream meta group has no leader
// refuses with 503/10008 before it looks at the stream at all (:2282-2286), a member that then
// knows the stream's own group has no leader refuses the same way (:2315-2319), and otherwise
// only the stream's current leader answers (:2322-2324). So a request sent while the stream is
// between leaders is either refused or goes unanswered, and the caller issues it again.
func stepDownStreamLeader(conn *natsgo.Conn, stream, preferred string) (string, error) {
	body, err := json.Marshal(map[string]any{"placement": map[string]string{"preferred": preferred}})
	if err != nil {
		return "", err
	}
	msg, err := conn.Request("$JS.API.STREAM.LEADER.STEPDOWN."+stream, body, 2*time.Second)
	if err != nil {
		return "", err
	}
	return string(msg.Data), nil
}

// podArgs is a container's rendered arguments with each key of replace, a value fixed by the pod,
// replaced by its value on this machine, failing the test when one is in no argument, so a chart
// that moved it is caught here.
func podArgs(t *testing.T, rendered []string, replace map[string]string) []string {
	t.Helper()
	args := slices.Clone(rendered)
	for old, local := range replace {
		found := false
		for i := range args {
			if strings.Contains(args[i], old) {
				args[i], found = strings.ReplaceAll(args[i], old, local), true
			}
		}
		require.True(t, found, "%q is in the rendered arguments %q", old, rendered)
	}
	return args
}

// seedOption authenticates a connection as the nkey user whose seed is seed.
func seedOption(t *testing.T, seed string) natsgo.Option {
	t.Helper()
	user, err := nkeys.FromSeed([]byte(seed))
	require.NoError(t, err)
	public, err := user.PublicKey()
	require.NoError(t, err)
	return natsgo.Nkey(public, user.Sign)
}

// connectHub connects to urls as the user whose seed is seed, reconnecting to any of them without
// limit, closed when the test ends.
func connectHub(t *testing.T, urls []string, seed string, opts ...natsgo.Option) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(strings.Join(urls, ","), append([]natsgo.Option{
		seedOption(t, seed), natsgo.Timeout(time.Second), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100 * time.Millisecond),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn
}

// awaitViolation requires the next error the server sent conn to be its permissions violation for
// want, within 5 s.
func awaitViolation(t *testing.T, conn *natsgo.Conn, errs <-chan error, want string) {
	t.Helper()
	require.NoError(t, conn.Flush())
	select {
	case err := <-errs:
		require.ErrorIs(t, err, natsgo.ErrPermissionViolation)
		require.EqualError(t, err, "nats: permissions violation: Permissions Violation for "+want)
	case <-time.After(5 * time.Second):
		t.Fatalf("the server reported no permissions violation within 5s, want one for %s", want)
	}
}

// writeAtomically replaces the file at path with text by a rename, as Kubernetes swaps a
// ConfigMap's files: a reader sees the old file or the new one, never part of either.
func writeAtomically(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".new"
	require.NoError(t, os.WriteFile(tmp, []byte(text), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// promSeries is one series of an exporter's scrape.
type promSeries struct {
	name   string
	labels map[string]string
	value  float64
}

var promLabel = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// scrape reads every series the exporter on port serves.
func scrape(port int) ([]promSeries, error) {
	resp, err := monitorClient.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var series []promSeries
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := promSeries{labels: map[string]string{}}
		rest := line
		if open := strings.IndexByte(line, '{'); open >= 0 {
			end := strings.LastIndexByte(line, '}')
			s.name, rest = line[:open], line[end+1:]
			for _, m := range promLabel.FindAllStringSubmatch(line[open+1:end], -1) {
				s.labels[m[1]] = m[2]
			}
		} else {
			s.name, rest, _ = strings.Cut(line, " ")
		}
		if s.value, err = strconv.ParseFloat(strings.Fields(rest)[0], 64); err != nil {
			return nil, fmt.Errorf("%q: %w", line, err)
		}
		series = append(series, s)
	}
	return series, nil
}

// listeningPort is the one TCP port the process pid listens on, as the kernel reports it: the
// sockets among the process's descriptors (/proc/<pid>/fd, each a socket:[<inode>] link) and the
// LISTEN rows (state 0A) of /proc/net/tcp and /proc/net/tcp6 whose inode is one of them. It names
// the port the process itself bound, so no port another program holds can be read for it, and it
// fails while the process listens on no port and when it listens on more than one.
func listeningPort(pid int) (int, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	sockets := map[string]bool{}
	for _, fd := range fds {
		// A descriptor closed since the directory was read has no link, and is no socket of the
		// process any more.
		link, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if inode, ok := strings.CutPrefix(link, "socket:["); err == nil && ok {
			sockets[strings.TrimSuffix(inode, "]")] = true
		}
	}
	var ports []int
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		text, err := os.ReadFile(table)
		if errors.Is(err, os.ErrNotExist) {
			// A kernel without IPv6 has no tcp6 table, and nothing listens there.
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(text), "\n")[1:] {
			// sl, local_address (the address and the port in hexadecimal), rem_address, st, and the
			// inode tenth.
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" || !sockets[fields[9]] {
				continue
			}
			_, hexPort, _ := strings.Cut(fields[1], ":")
			port, err := strconv.ParseUint(hexPort, 16, 16)
			if err != nil {
				return 0, fmt.Errorf("%s row %q: %w", table, line, err)
			}
			ports = append(ports, int(port))
		}
	}
	if len(ports) != 1 {
		return 0, fmt.Errorf("pid %d listens on %d TCP ports %v, from %d sockets among %d descriptors, want one", pid, len(ports), ports, len(sockets), len(fds))
	}
	return ports[0], nil
}

// lastSeqMetric is the exporter's metric of a stream copy's last sequence, under the chart's
// -prefix=nats.
const lastSeqMetric = "nats_stream_last_seq"

// streamSeries is every series of stream the exporters on ports serve, each exporter scraped
// once; an exporter whose server is down serves none.
func streamSeries(ports []int, stream string) ([]promSeries, error) {
	var out []promSeries
	for _, port := range ports {
		series, err := scrape(port)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			if s.labels["stream_name"] == stream {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// lastSeqs is the last_seq series among series, by the server_name label.
func lastSeqs(series []promSeries) map[string]float64 {
	out := map[string]float64{}
	for _, s := range series {
		if s.name == lastSeqMetric {
			out[s.labels["server_name"]] = s.value
		}
	}
	return out
}

// awaitLastSeqs waits up to 30 s for the exporters on ports to report want last_seq series of
// stream, each value, and returns them.
func awaitLastSeqs(t *testing.T, ports []int, stream string, want int, value float64) map[string]float64 {
	t.Helper()
	var got map[string]float64
	within(t, 30*time.Second, fmt.Sprintf("%d %s series of %s at %v", want, lastSeqMetric, stream, value), func() error {
		series, err := streamSeries(ports, stream)
		if err != nil {
			return err
		}
		got = lastSeqs(series)
		if len(got) != want {
			return fmt.Errorf("%d series: %v", len(got), got)
		}
		for server, v := range got {
			if v != value {
				return fmt.Errorf("%s reports %v", server, v)
			}
		}
		return nil
	})
	return got
}

// The configuration the chart renders for the hub runs as a cluster on three local nats-server
// v2.15.1 processes, each on the rendered nats.conf and auth.conf: they form one cluster with a
// JetStream meta leader and serve the hub's users; a stream at three copies keeps every
// acknowledged message exactly once across a SIGKILL of its leader and recovers the killed copy;
// the exporter, run as the chart runs it, reports a copy's last sequence per server; and the
// reloader, run as the chart runs it, applies a users change to every server with no restart and
// no reconnect, while a session-user change is refused by every server's reload and takes effect
// only on a restart, which is why the hub rolls its pods for one.
func TestClusteredNatsHubCluster(t *testing.T) {
	r := renderClusteredHub(t)
	bin := t.TempDir()
	tools := installTools(t, bin,
		liveTool{natsServerTool, "nats-server"},
		liveTool{reloaderTool, "nats-server-config-reloader"},
		liveTool{exporterTool, "prometheus-nats-exporter"})
	natsServer, reloader, exporter := tools[0], tools[1], tools[2]

	// Each server's fronts are held from newHubServer on, so every server's routes name all three
	// route fronts before the first of them starts.
	servers := make([]*hubServer, 3)
	for i := range servers {
		servers[i] = r.newHubServer(t, fmt.Sprintf("%s-%d", r.release, i), natsServer, r.natsConf)
		servers[i].peers = servers
	}
	for _, s := range servers {
		s.start(t)
		t.Logf("%s: nats-server %s", s.name, strings.Join(s.commandArgs(), " "))
	}
	urls := make([]string, len(servers))
	for i, s := range servers {
		urls[i] = s.url()
	}
	byName := map[string]*hubServer{}
	for _, s := range servers {
		byName[s.name] = s
	}

	clusterName := regexp.MustCompile(`"cluster":\{"name":"([^"]+)"`).FindStringSubmatch(strings.Join(strings.Fields(r.natsConf), ""))
	require.Len(t, clusterName, 2, "the rendered nats.conf names its cluster")

	// (a) One cluster with a JetStream meta leader, serving the hub's users.
	var metaLeader string
	formed := within(t, 60*time.Second, "the three servers form one cluster with a meta leader", func() error {
		leaders := map[string]bool{}
		for _, s := range servers {
			var varz struct {
				ServerName string `json:"server_name"`
				Cluster    struct {
					Name string `json:"name"`
				} `json:"cluster"`
			}
			if err := s.monitorJSON("/varz", &varz); err != nil {
				return err
			}
			if varz.ServerName != s.name || varz.Cluster.Name != clusterName[1] {
				return fmt.Errorf("%s's /varz names server %q in cluster %q", s.name, varz.ServerName, varz.Cluster.Name)
			}
			remotes, err := s.routeRemotes()
			if err != nil {
				return err
			}
			if len(remotes) != 2 || remotes[s.name] {
				return fmt.Errorf("%s routes to %v", s.name, remotes)
			}
			leader, size, err := s.metaCluster()
			if err != nil {
				return err
			}
			if leader == "" || size != 3 {
				return fmt.Errorf("%s's meta cluster: leader %q, size %d", s.name, leader, size)
			}
			leaders[leader] = true
			metaLeader = leader
		}
		if len(leaders) != 1 {
			return fmt.Errorf("the servers name meta leaders %v", leaders)
		}
		return nil
	})
	t.Logf("(a) cluster %q (from the rendered nats.conf, as each /varz reports it) formed in %s: each server routes to the other two; JetStream meta leader %s",
		clusterName[1], formed.Round(time.Millisecond), metaLeader)

	for _, s := range servers {
		conn, err := testnats.ConnectSeed(t, s.url(), r.keys.hostSeed)
		require.NoError(t, err)
		require.Equal(t, r.keys.hostKey, s.authorizedUser(t, conn))
		conn.Close()
		t.Logf("(a) the keyed user live-host (core.HostPermissions) connected to %s as %s", s.name, r.keys.hostKey)
	}
	session := core.SessionPermissions().Publish.Allow
	allowed := strings.NewReplacer("*", "live", ">", "live").Replace(session[0])
	require.True(t, bus.SubjectContains(session[0], allowed))
	refused := "notifications.live"
	for _, pattern := range session {
		require.False(t, bus.SubjectContains(pattern, refused), "%s is outside core.SessionPermissions' publish grant", refused)
	}
	// A session receives on its own relay subject and on no notification topic of its own, so the
	// hub refuses the credential-free user the plain notifications.> subscribe a session of an
	// earlier release held.
	relay := bus.DeliverSubject("live")
	for _, s := range servers {
		errs := make(chan error, 16)
		conn, err := testnats.ConnectSeed(t, s.url(), "", natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
		require.NoError(t, err)
		require.Equal(t, r.keys.sessionKey, s.authorizedUser(t, conn), "a client with no credential is the session user")
		_, err = conn.SubscribeSync(relay)
		require.NoError(t, err)
		require.NoError(t, conn.Publish(allowed, nil))
		require.NoError(t, conn.Publish(refused, nil))
		awaitViolation(t, conn, errs, fmt.Sprintf("Publish to %q", refused))
		_, err = conn.SubscribeSync(bus.NotificationTopicPrefix + ">")
		require.NoError(t, err)
		awaitViolation(t, conn, errs, `Subscription to "`+bus.NotificationTopicPrefix+`>"`)
		conn.Close()
		t.Logf("(a) a client with no credential on %s became the session user %s, subscribed to %s, published to %s, and was refused a publish to %s and a subscribe to %s>",
			s.name, r.keys.sessionKey, relay, allowed, refused, bus.NotificationTopicPrefix)
	}

	// (b) A stream at three copies across a SIGKILL of its leader.
	const r3, messages = "LIVE_R3", 1000
	operator := connectHub(t, urls, r.keys.operatorSeed)
	js, err := operator.JetStream(natsgo.MaxWait(2 * time.Second))
	require.NoError(t, err)
	within(t, 30*time.Second, "the R3 stream is created", func() error {
		_, err := js.AddStream(&natsgo.StreamConfig{Name: r3, Subjects: []string{"live.r3"}, Replicas: 3, Storage: natsgo.FileStorage})
		return err
	})
	// The leader is polled with a short wait, so a request left unanswered while the stream has no
	// leader delays the election's measure by at most 500 ms.
	poll, err := operator.JetStream(natsgo.MaxWait(500 * time.Millisecond))
	require.NoError(t, err)
	streamLeader := func() (string, error) {
		info, err := poll.StreamInfo(r3)
		if err != nil {
			return "", err
		}
		if info.Cluster == nil || info.Cluster.Leader == "" {
			return "", errors.New("the stream has no leader")
		}
		return info.Cluster.Leader, nil
	}
	awaitCurrent := func(what string) time.Duration {
		return within(t, 30*time.Second, what, func() error {
			info, err := js.StreamInfo(r3)
			if err != nil {
				return err
			}
			if info.Cluster == nil || info.Cluster.Leader == "" || len(info.Cluster.Replicas) != 2 {
				return fmt.Errorf("cluster %+v", info.Cluster)
			}
			for _, p := range info.Cluster.Replicas {
				if !p.Current || p.Offline || p.Lag != 0 {
					return fmt.Errorf("the copy on %s: current %t, offline %t, lag %d", p.Name, p.Current, p.Offline, p.Lag)
				}
			}
			for _, s := range servers {
				last, err := s.streamLastSeq(r3)
				if err != nil {
					return err
				}
				if last != info.State.LastSeq {
					return fmt.Errorf("%s's copy ends at %d, the stream at %d", s.name, last, info.State.LastSeq)
				}
			}
			return nil
		})
	}

	// forceCoLocated moves the stream's leadership onto the server that leads the JetStream meta
	// group, so that one SIGKILL takes both groups from the cluster at once.
	forceCoLocated := func(bound time.Duration, what string) (meta string) {
		var first, answer string
		took := within(t, bound, what, func() error {
			var err error
			meta, err = agreedMetaLeader(servers)
			if err != nil {
				return err
			}
			stream, err := streamLeader()
			if err != nil {
				return err
			}
			if first == "" {
				first = stream
			}
			if stream == meta {
				return nil
			}
			// The request goes out on every pass: nothing guarantees the preferred peer wins the
			// election its stepdown starts, and a 400 with err_code 10005 ("preferred server ...
			// is already leader") is no failure of its own, so the next pass's comparison, not
			// this answer, decides.
			reply, err := stepDownStreamLeader(operator, r3, meta)
			if err != nil {
				return fmt.Errorf("%s leads %s and %s leads the JetStream meta group; the stepdown to %s got %v",
					stream, r3, meta, meta, err)
			}
			answer = reply
			return fmt.Errorf("%s leads %s and %s leads the JetStream meta group; the stepdown to %s answered %s",
				stream, r3, meta, meta, reply)
		})
		stepdown := "no stepdown was needed"
		if answer != "" {
			stepdown = "the last stepdown answered " + answer
		}
		t.Logf("(b) %s: %s led %s at the first look; %s leads the JetStream meta group and now leads %s too; %s; the loop took %s",
			what, first, r3, meta, r3, stepdown, took.Round(time.Millisecond))
		return meta
	}
	// 30 s holds several stepdown attempts at the 56-109 ms leadership transfer measured on a
	// scratch cluster of this server version, and it matches awaitCurrent's 30 s.
	forceCoLocated(30*time.Second, "the JetStream meta leader leads "+r3+" before the publisher starts")

	// The servers' debug output names the stream's Raft group in each of its election and vote
	// lines (RAFT [<node> - <group>]), so the line logged at the kill gives the group with the
	// kill's time, in the layout and local clock of those lines. The group is read here, where a
	// read costs the kill nothing, and a group's name never changes.
	r3Info, err := js.StreamInfo(r3)
	require.NoError(t, err)
	require.NotNil(t, r3Info.Cluster, "%s reports its cluster", r3)
	require.NotEmpty(t, r3Info.Cluster.RaftGroup, "%s names its Raft group", r3)
	group := r3Info.Cluster.RaftGroup

	publisher := connectHub(t, urls, r.keys.operatorSeed)
	pubJS, err := publisher.JetStream()
	require.NoError(t, err)
	var acked, retries atomic.Int64
	published := make(chan error, 1)
	go func() {
		for i := 1; i <= messages; i++ {
			id := strconv.Itoa(i)
			deadline := time.Now().Add(30 * time.Second)
			for {
				msg := natsgo.NewMsg("live.r3")
				msg.Data = []byte(id)
				_, err := pubJS.PublishMsg(msg, natsgo.MsgId(id), natsgo.AckWait(2*time.Second))
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					published <- fmt.Errorf("message %s unacknowledged for 30s: %w", id, err)
					return
				}
				retries.Add(1)
				time.Sleep(50 * time.Millisecond)
			}
			acked.Store(int64(i))
			time.Sleep(5 * time.Millisecond)
		}
		published <- nil
	}()
	within(t, 30*time.Second, "the publisher's first 200 acknowledgements", func() error {
		if acked.Load() < 200 {
			return fmt.Errorf("%d acknowledged", acked.Load())
		}
		return nil
	})
	// One pass is the ordinary cost here, three /jsz reads and one StreamInfo, about 10 ms,
	// because nothing between this call and the one above it stops a server; the call is here so
	// the co-location holds at the instant of the kill rather than 1.5 s before it. About 5 s of
	// publishing is left when the 200-acknowledgement wait returns (795 messages at about 6 ms
	// each), so a longer bound would let the require.Less below fail for an unrelated reason.
	meta := forceCoLocated(3*time.Second, "the JetStream meta leader still leads "+r3+" at the kill")
	leader, err := streamLeader()
	require.NoError(t, err)
	require.Equal(t, meta, leader, "the server about to be killed leads both %s and the JetStream meta group", r3)
	killed := byName[leader]
	require.NotNil(t, killed, "the stream leader %s is one of the servers", leader)
	// The control for the leaderless meta group this phase is about. The node that wins the meta
	// election logs selfElected unconditionally (nats-server v2.15.1,
	// server/jetstream_cluster.go:10080), but only once its first entry as leader is committed and
	// applied (server/raft.go:6321 and 1603-1613), which is later than the moment /jsz names it
	// (server/raft.go:6307 and 4839-4841, read at server/jetstream_cluster.go:14075): the line
	// reached the test's buffer 2.8-13.7 ms after both survivors named the new leader in four of
	// twelve measured runs. So the wait below requires the line as well as the agreement, rather
	// than reading the log once after it. A raft group reaches a new leader only through a term
	// that has none, so counting the line proves the leaderless state happened. Sampling /jsz for
	// an empty leader would not: that state lasted 18-49 ms in five of six measured runs, and
	// within polls every 100 ms.
	const selfElected = "Self is new JetStream cluster metadata leader"
	survivors := make([]*hubServer, 0, len(servers)-1)
	elections := map[string]int{}
	for _, s := range servers {
		if s == killed {
			continue
		}
		survivors = append(survivors, s)
		elections[s.name] = s.count(selfElected)
	}
	killed.kill(t)
	killedAt := time.Now()
	t.Logf("(b) SIGKILL to %s, which led %s's Raft group %s, at %s",
		killed.name, r3, group, killedAt.Format("2006/01/02 15:04:05.000000"))
	atKill := acked.Load()
	require.Less(t, atKill, int64(messages), "the leader dies mid-run")
	for _, s := range survivors {
		l, _, err := s.metaCluster()
		since := time.Since(killedAt).Round(time.Millisecond)
		if err != nil {
			t.Logf("(b) %s's /jsz, read %s after the kill: %v", s.name, since, err)
			continue
		}
		t.Logf("(b) %s names the JetStream meta leader %q, read %s after the kill", s.name, l, since)
	}
	var newLeader string
	// nats-server v2.15.1's Raft waits a random 4-9 s without the leader's heartbeat before a
	// follower campaigns (server/raft.go, minElectionTimeoutDefault and maxElectionTimeoutDefault).
	// A candidate writes its term and its vote for itself to disk before it sends its vote request
	// (requestVote, writeTermVote). When the other survivor's timer fires inside that window, both
	// are candidates in one term, each refuses the other (processVoteRequest), and the group waits
	// for a second draw: a split vote. Measured: 13 of 12,482 runs of this phase in a scratch
	// harness took a second round, every one a split vote, one of them at 15.40 s; CI run
	// 43462859890 failed this wait at 15 s with the same signature (dispatch://INFRA-3529). One
	// round is at most 9 s plus 0.8 s (maxCampaignTimeoutDefault), the campaign of a survivor that
	// refused a candidate with a shorter log. Three rounds and the read's 500 ms fit in 30 s, so
	// the wait fails only when three draws in a row split. The publisher's 30 s per message is the
	// cap: a longer outage fails the publisher instead.
	within(t, 30*time.Second, "a new stream leader after the SIGKILL", func() error {
		l, err := streamLeader()
		if err != nil {
			return err
		}
		if l == leader {
			return fmt.Errorf("the leader is still %s", l)
		}
		newLeader = l
		return nil
	})
	elected := time.Since(killedAt)
	var newMeta *hubServer
	// The wait is on both survivors, not on the one that leads the stream, because either one can
	// receive the read and refuse it: jsMsgGetRequest answers 503 with err_code 10008 while the
	// server that receives the request knows no JetStream meta leader (nats-server v2.15.1,
	// server/jetstream_api.go:3922-3926, with the predicate js.isLeaderless at
	// server/jetstream_cluster.go:1481-1497), and it decides that before it decides which server
	// leads the stream. The meta election is an independent random 4-9 s draw (server/raft.go,
	// minElectionTimeoutDefault and maxElectionTimeoutDefault), so the stream election above is no
	// sign that it finished. The meta group takes split votes as the stream's group does, so 30 s
	// holds three rounds, as the stream wait's does.
	within(t, 30*time.Second, "a new JetStream meta leader after the SIGKILL, with its own election in its log", func() error {
		name, err := agreedMetaLeader(survivors)
		if err != nil {
			return err
		}
		if name == killed.name {
			return fmt.Errorf("the survivors still name the killed %s the JetStream meta leader", name)
		}
		newMeta = byName[name]
		if newMeta == nil {
			return fmt.Errorf("the survivors name the JetStream meta leader %s, which is no server of the cluster", name)
		}
		if newMeta.count(selfElected) == elections[name] {
			return fmt.Errorf("%s, which the survivors name the new JetStream meta leader, has not logged %q since the kill", name, selfElected)
		}
		return nil
	})
	metaElected := time.Since(killedAt)
	t.Logf("(b) SIGKILL of %s (pid %d), which led both %s and the JetStream meta group, after %d of %d messages acknowledged; %s leads %s %s later, and %s leads the meta group %s after the kill",
		leader, killed.proc.Cmd.Process.Pid, r3, atKill, messages, newLeader, r3, elected.Round(time.Millisecond),
		newMeta.name, metaElected.Round(time.Millisecond))
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatalf("the publisher did not finish within 2m: %d acknowledged", acked.Load())
	}
	info, err := js.StreamInfo(r3)
	require.NoError(t, err)
	require.EqualValues(t, messages, info.State.Msgs)
	ids := map[string]int{}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		msg, err := js.GetMsg(r3, seq)
		require.NoError(t, err)
		ids[msg.Header.Get(natsgo.MsgIdHdr)]++
	}
	for i := 1; i <= messages; i++ {
		require.Equal(t, 1, ids[strconv.Itoa(i)], "message %d is in the stream exactly once", i)
	}
	require.Len(t, ids, messages, "the stream holds no message that was not published")
	t.Logf("(b) the stream holds each of the %d acknowledged messages exactly once (sequences %d-%d; %d publish attempts retried)",
		messages, info.State.FirstSeq, info.State.LastSeq, retries.Load())
	killed.start(t)
	caughtUp := awaitCurrent("all three copies current after the killed server restarts on its store")
	t.Logf("(b) %s restarted on its store (pid %d); all three copies current at sequence %d %s later",
		killed.name, killed.proc.Cmd.Process.Pid, info.State.LastSeq, caughtUp.Round(time.Millisecond))

	// (c) The exporter, with the chart's arguments, against each server.
	exporterArgs := stringList(t, r.container(t, "prom-exporter")["args"])
	exporterPorts := make([]int, len(servers))
	for i, s := range servers {
		// The chart's arguments with the exporter's port, -port=7777, and the monitoring port of
		// http://localhost:8222/, both fixed by the pod, replaced: -port=0, a port the kernel
		// chooses as the exporter binds it, and the server's monitoring front, on 127.0.0.1 since
		// the front holds IPv4 loopback alone and localhost may name ::1 first. The exporter logs the
		// address it was given (prometheus-nats-exporter v0.20.2, exporter/exporter.go:445, "0.0.0.0:0"
		// here) and not the one it bound, so its port is read from the kernel instead
		// (listeningPort). It is never restarted, so it holds that port until the test ends.
		args := podArgs(t, exporterArgs, map[string]string{
			"-port=7777":     "-port=0",
			"localhost:8222": s.monitor.Addr(),
		})
		p := startServing(t, s.name+" exporter", exec.Command(exporter, args...), func(p *cmdtest.Process) error {
			port, err := listeningPort(p.Cmd.Process.Pid)
			if err != nil {
				return err
			}
			if _, err := scrape(port); err != nil {
				return err
			}
			exporterPorts[i] = port
			return nil
		})
		t.Logf("%s: prometheus-nats-exporter %s (pid %d), serving on port %d", s.name, strings.Join(args, " "), p.Cmd.Process.Pid, exporterPorts[i])
	}
	atRest := awaitLastSeqs(t, exporterPorts, r3, 3, float64(messages))
	series, err := streamSeries(exporterPorts, r3)
	require.NoError(t, err)
	var metricNames []string
	for _, s := range series {
		if !slices.Contains(metricNames, s.name) {
			metricNames = append(metricNames, s.name)
		}
	}
	sort.Strings(metricNames)
	t.Logf("(c) the exporters' series of %s: %s", r3, strings.Join(metricNames, ", "))
	t.Logf("(c) %s at rest: %d %s series, equal: %v", r3, len(atRest), lastSeqMetric, atRest)

	leader, err = streamLeader()
	require.NoError(t, err)
	var down *hubServer
	for _, s := range servers {
		if s.name != leader {
			down = s
			break
		}
	}
	down.kill(t)
	whileDown := awaitLastSeqs(t, exporterPorts, r3, 2, float64(messages))
	require.NotContains(t, whileDown, down.name)
	t.Logf("(c) while %s is down: %d %s series of %s: %v", down.name, len(whileDown), lastSeqMetric, r3, whileDown)
	down.start(t)
	awaitCurrent("all three copies current after " + down.name + " restarts")
	awaitLastSeqs(t, exporterPorts, r3, 3, float64(messages))

	const r1 = "LIVE_R1"
	within(t, 30*time.Second, "the R1 stream is created", func() error {
		_, err := js.AddStream(&natsgo.StreamConfig{Name: r1, Subjects: []string{"live.r1"}, Replicas: 1, Storage: natsgo.FileStorage})
		return err
	})
	for i := range 5 {
		_, err := js.Publish("live.r1", []byte(strconv.Itoa(i)))
		require.NoError(t, err)
	}
	single := awaitLastSeqs(t, exporterPorts, r1, 1, 5)
	t.Logf("(c) the R1 stream %s: %d %s series: %v", r1, len(single), lastSeqMetric, single)

	// (d) The reloader, with the chart's arguments, beside each server: a users change applies
	// with no restart and no reconnect.
	reloaderArgs := stringList(t, r.container(t, "reloader")["args"])
	pids := make([]int, len(servers))
	for i, s := range servers {
		pids[i] = s.proc.Cmd.Process.Pid
		// The chart's arguments with the pid file's emptyDir, /var/run/nats/, and the
		// ConfigMap's mount, /etc/nats-config/, replaced by the server's directory.
		args := podArgs(t, reloaderArgs, map[string]string{"/var/run/nats/": s.dir + "/", "/etc/nats-config/": s.dir + "/"})
		p := startLogged(t, s.name+" reloader", exec.Command(reloader, args...))
		p.WaitForOutput(t, fmt.Sprintf("Live, ready to kick pid %d", pids[i]))
		t.Logf("%s: nats-server-config-reloader %s, watching pid %d", s.name, strings.Join(args, " "), pids[i])
	}
	var disconnects atomic.Int64
	throughout := connectHub(t, urls[:1], r.keys.operatorSeed,
		natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { disconnects.Add(1) }))
	serverID := throughout.ConnectedServerId()
	cid, err := throughout.GetClientID()
	require.NoError(t, err)
	t.Logf("(d) a client connected throughout: server_id %s, cid %d", serverID, cid)

	reloaded := "Reloaded server configuration"
	writeAll := func(authConf string) []int {
		before := make([]int, len(servers))
		for i, s := range servers {
			before[i] = s.count(reloaded)
			writeAtomically(t, filepath.Join(s.dir, clusteredHubAuthConfKey), authConf)
		}
		return before
	}
	addedSeed, addedKey := testnats.User(t)
	withAdded := liveHubArgs(r.keys)
	withAdded.Users = append(withAdded.Users, gcp.NATSUserArgs{Name: "live-added", NKey: addedKey, Permissions: &gcp.PermissionsArgs{
		Publish: nats.RulesArgs{Allow: []string{"live.added.>"}}, Subscribe: nats.RulesArgs{Allow: []string{"_INBOX.>"}},
	}})
	before := writeAll(liveAuthConf(t, withAdded))
	for i, s := range servers {
		took := within(t, 30*time.Second, "the added user connects to "+s.name, func() error {
			if s.count(reloaded) == before[i] {
				return errors.New("no reload yet")
			}
			conn, err := testnats.ConnectSeed(t, s.url(), addedSeed)
			if err != nil {
				return err
			}
			conn.Close()
			return nil
		})
		t.Logf("(d) %s reloaded the auth.conf adding live-added (%q in its log) and admits it, %s after the rename",
			s.name, reloaded, took.Round(time.Millisecond))
	}
	before = writeAll(r.authConf)
	for i, s := range servers {
		took := within(t, 30*time.Second, "the removed user is refused by "+s.name, func() error {
			if s.count(reloaded) == before[i] {
				return errors.New("no reload yet")
			}
			conn, err := testnats.ConnectSeed(t, s.url(), addedSeed)
			if err == nil {
				conn.Close()
				return errors.New("still admitted")
			}
			if !errors.Is(err, natsgo.ErrAuthorization) {
				return err
			}
			return nil
		})
		t.Logf("(d) %s reloaded the auth.conf without live-added and refuses it (authorization violation), %s after the rename",
			s.name, took.Round(time.Millisecond))
	}
	require.NoError(t, throughout.Flush())
	require.True(t, throughout.IsConnected())
	require.Equal(t, serverID, throughout.ConnectedServerId())
	stillCID, err := throughout.GetClientID()
	require.NoError(t, err)
	require.Equal(t, cid, stillCID)
	require.Zero(t, throughout.Stats().Reconnects)
	require.Zero(t, disconnects.Load())
	for i, s := range servers {
		require.Equal(t, pids[i], s.proc.Cmd.Process.Pid)
		select {
		case <-s.proc.Exited:
			t.Fatalf("%s exited", s.name)
		default:
		}
		pidFile, err := os.ReadFile(filepath.Join(s.dir, "nats.pid"))
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(pids[i]), string(pidFile))
	}
	t.Logf("(d) through both changes the client kept server_id %s and cid %d with 0 reconnects and 0 disconnects; server pids unchanged: %v",
		serverID, cid, pids)

	// (e) A session-user change: every server's reload refuses it.
	_, newSessionKey := testnats.User(t)
	withNewSession := liveHubArgs(r.keys)
	withNewSession.SessionUser.NKey = newSessionKey
	refusal := "config reload not supported for NoAuthUser"
	refusals := make([]int, len(servers))
	for i, s := range servers {
		refusals[i] = s.count(refusal)
	}
	before = writeAll(liveAuthConf(t, withNewSession))
	for i, s := range servers {
		within(t, 30*time.Second, s.name+" refuses the session-user change", func() error {
			if s.count(refusal) == refusals[i] {
				return errors.New("no refusal yet")
			}
			return nil
		})
		var line string
		for _, l := range strings.Split(s.proc.Output.String(), "\n") {
			if strings.Contains(l, refusal) {
				line = l
			}
		}
		require.Contains(t, line, fmt.Sprintf("old=%s, new=%s", r.keys.sessionKey, newSessionKey))
		require.Equal(t, before[i], s.count(reloaded), "%s applied nothing", s.name)
		t.Logf("(e) %s refused the reload; its log: %s", s.name, strings.TrimSpace(line))

		conn, err := testnats.ConnectSeed(t, s.url(), "")
		require.NoError(t, err)
		require.Equal(t, r.keys.sessionKey, s.authorizedUser(t, conn), "the previous session user stays in force")
		conn.Close()
		conn, err = testnats.ConnectSeed(t, s.url(), r.keys.hostSeed)
		require.NoError(t, err)
		conn.Close()
		t.Logf("(e) %s still makes a client with no credential the previous session user %s and admits live-host", s.name, r.keys.sessionKey)
	}
	restarted := servers[0]
	restarted.terminate(t)
	restarted.start(t)
	var user string
	within(t, 30*time.Second, restarted.name+" admits a client with no credential after its restart", func() error {
		conn, err := testnats.ConnectSeed(t, restarted.url(), "")
		if err != nil {
			return err
		}
		defer conn.Close()
		user = restarted.authorizedUser(t, conn)
		return nil
	})
	require.Equal(t, newSessionKey, user)
	t.Logf("(e) %s restarted (pid %d) makes a client with no credential the new session user %s", restarted.name, restarted.proc.Cmd.Process.Pid, user)

	// (f) A lower max_file_store: nats-server refuses the whole reload, so a users change written
	// beside it is not applied either, and every later reload repeats the refusal, until the server
	// restarts on the new nats.conf. This is why the hub's pods carry maxFileStoreGb in an
	// annotation (envoy-infra.sjawhar.dev/max-file-store), which rolls them when it changes.
	natsConfPath := filepath.Join(restarted.dir, "nats.conf")
	natsConf, err := os.ReadFile(natsConfPath)
	require.NoError(t, err)
	storeLine := fmt.Sprintf(`"max_file_store": %dGi`, r.settings.MaxFileStoreGb)
	require.Equal(t, 1, strings.Count(string(natsConf), storeLine), "the rendered nats.conf sets %s once", storeLine)
	lowered := strings.Replace(string(natsConf), storeLine, fmt.Sprintf(`"max_file_store": %dGi`, r.settings.MaxFileStoreGb-1), 1)
	withAddedNewSession := liveHubArgs(r.keys)
	withAddedNewSession.SessionUser.NKey = newSessionKey
	withAddedNewSession.Users = withAdded.Users
	storeRefusal := "config reload not supported for decreasing jetstream max memory and store"
	storeRefusals := restarted.count(storeRefusal)
	writeAtomically(t, natsConfPath, lowered)
	writeAtomically(t, filepath.Join(restarted.dir, clusteredHubAuthConfKey), liveAuthConf(t, withAddedNewSession))
	// The server's own SIGHUP, as the reloader sends it: the reloader may still name the pid the
	// server had before (e)'s restart.
	require.NoError(t, restarted.proc.Cmd.Process.Signal(syscall.SIGHUP))
	within(t, 30*time.Second, restarted.name+" refuses the lower max_file_store", func() error {
		if restarted.count(storeRefusal) == storeRefusals {
			return errors.New("no refusal yet")
		}
		return nil
	})
	conn, err := testnats.ConnectSeed(t, restarted.url(), addedSeed)
	if err == nil {
		conn.Close()
	}
	require.ErrorIs(t, err, natsgo.ErrAuthorization, "the users change written beside the lower store is not applied")
	t.Logf("(f) %s refused the reload of a lower max_file_store (%q) and with it the users change beside it: live-added is refused", restarted.name, storeRefusal)
	restarted.terminate(t)
	restarted.start(t)
	within(t, 30*time.Second, restarted.name+" admits live-added after its restart", func() error {
		conn, err := testnats.ConnectSeed(t, restarted.url(), addedSeed)
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	})
	t.Logf("(f) %s restarted (pid %d) on the lower max_file_store admits live-added", restarted.name, restarted.proc.Cmd.Process.Pid)
}
