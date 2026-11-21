package buckets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testnats"
)

// noHold is the env of a run that must reach no hold: a break-glass create or a usage error. A
// call of either opener fails the test.
func noHold(t *testing.T) env {
	return env{
		records: func(context.Context, writerhold.HubFlags, bool) (*holdrecord.Client, error) {
			t.Error("the run opened the broker's hold routes")
			return nil, errors.New("no hold here")
		},
		hub: func(context.Context, writerhold.HubFlags) (*writerhold.Operator, error) {
			t.Error("the run opened the hub as the operator")
			return nil, errors.New("no hold here")
		},
		now:         time.Now,
		http:        http.DefaultClient,
		restoreRuns: 2,
	}
}

// runCommand runs the command in e with args and returns its exit code and output.
func runCommand(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := command(context.Background(), args, &stdout, &stderr, e)
	return code, stdout.String(), stderr.String()
}

// listenerUsers is the authorization of a server with two users: a listener's, the nkey user the
// hub renders for the in-cluster gke listener, holding exactly core.ListenerHostPermissions("gke");
// and a watcher, which a client presenting no credential becomes and which may do anything, so the
// test reads streams and stream requests through it. It returns the authorization and the listener
// user's seed.
func listenerUsers(t *testing.T) (config, seed string) {
	t.Helper()
	seed, public := testnats.User(t)
	_, watcher := testnats.User(t)
	grant := core.ListenerHostPermissions("gke")
	everything := core.Rules{Allow: []string{">"}}
	config, err := core.Authorization{Users: []core.User{
		{Name: "gke", NKey: public, Publish: &grant.Publish, Subscribe: &grant.Subscribe, HasSeed: true},
		{Name: "watcher", NKey: watcher, Publish: &everything, Subscribe: &everything},
	}, NoAuthUser: "watcher"}.Render(nil)
	if err != nil {
		t.Fatalf("render the server's users: %v", err)
	}
	return config, seed
}

// listenerUserServer starts a NATS server with listenerUsers' users, and returns its URL, its
// monitoring URL and the listener user's seed.
func listenerUserServer(t *testing.T) (uri, monitor, seed string) {
	t.Helper()
	config, seed := listenerUsers(t)
	server, uri := testnats.StartConfig(t, config, []string{"-js"}, true)
	return uri, server.MonitorURL(), seed
}

// jetStream opens the watcher's JetStream on uri (listenerUserServer), or on a server with no users.
func jetStream(t *testing.T, uri string) (*natsgo.Conn, natsgo.JetStreamContext) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return conn, js
}

// connzWithout is a monitoring URL whose /connz is the server's at monitor, less each connection of
// the test's own clients (conns): the watcher and a racer, which read and make streams for the test
// and are no writer of the run. The break-glass create reads it as the server's list, so a test
// whose subject is not the connection check can run beside them. Every other connection stays.
func connzWithout(t *testing.T, monitor string, conns ...*natsgo.Conn) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		resp, err := http.Get(monitor + req.URL.RequestURI())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var list map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hide := map[float64]bool{}
		for _, conn := range conns {
			if cid, err := conn.GetClientID(); err == nil {
				hide[float64(cid)] = true
			}
		}
		kept := []any{}
		connections, _ := list["connections"].([]any)
		for _, c := range connections {
			if entry, ok := c.(map[string]any); ok && hide[entry["cid"].(float64)] {
				continue
			}
			kept = append(kept, c)
		}
		list["connections"], list["total"], list["num_connections"] = kept, len(kept), len(kept)
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// streamRequests records each request of a stream's JetStream API ($JS.API.STREAM.<verb>.<stream>)
// as it reaches the server.
type streamRequests struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

func watchStreamRequests(t *testing.T, conn *natsgo.Conn) *streamRequests {
	t.Helper()
	sub, err := conn.SubscribeSync("$JS.API.STREAM.>")
	if err != nil {
		t.Fatalf("watch the stream requests: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("watch the stream requests: %v", err)
	}
	return &streamRequests{conn: conn, sub: sub}
}

// take returns each request since the last take, as <verb>.<stream> in the order they came, but
// those of the notification stream, whose info every connect reads (bus.Connect), and a racer's own
// (racerInbox). The flush, a round trip after the run ended, leaves each request that reached the
// server queued here.
func (r *streamRequests) take(t *testing.T) []string {
	t.Helper()
	if err := r.conn.Flush(); err != nil {
		t.Fatalf("flush the stream request watch: %v", err)
	}
	var requests []string
	for {
		msg, err := r.sub.NextMsg(50 * time.Millisecond)
		if err != nil {
			return requests
		}
		if strings.HasPrefix(msg.Reply, racerInbox+".") {
			continue
		}
		if request := strings.TrimPrefix(msg.Subject, "$JS.API.STREAM."); !strings.HasSuffix(request, "."+bus.Stream) {
			requests = append(requests, request)
		}
	}
}

// The single-node compose file's first `up`: the server holds no bucket, and no client but the
// buckets service's own is connected, since compose starts the listener only once that service has
// ended. `create --break-glass` creates each missing bucket at the deployment's copies, opens no
// hold, and logs one line per bucket naming the operator, the server and break-glass. Its next `up`,
// an upgrade beside the running listener, finds every bucket there, changes nothing and exits 0,
// which the compose file's every later `up` reaches. It fails when a first create on an empty,
// unwatched server is refused, or when an upgrade's `up` fails for the listener it started.
func TestBreakGlassCreatesTheMissingBucketsAtTheFirstUp(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, monitor, seed := listenerUserServer(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	single := []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.KnownSessionBucket}
	args := []string{"create", "--break-glass", "--operator", "compose", "--nats", uri, "--connz", monitor}
	for _, bucket := range single {
		args = append(args, "--bucket", bucket)
	}
	code, stdout, stderr := runCommand(t, noHold(t), args...)
	if code != 0 || strings.Count(stdout, "created ") != len(single) || strings.Count(stderr, "break-glass: operator compose creates") != len(single) {
		t.Fatalf("the first up's create = %d, want every bucket created and logged\n%s%s", code, stdout, stderr)
	}
	_, js := jetStream(t, uri)
	for _, bucket := range single {
		if _, err := js.StreamInfo("KV_" + bucket); err != nil {
			t.Fatalf("the first up made no %s: %v", bucket, err)
		}
	}
	// The next up, an upgrade: the listener the first up started still serves, and every bucket is
	// there, so the run changes nothing and exits 0 without reading the connection list.
	listener, err := testnats.ConnectSeed(t, uri, seed, natsgo.Name("envoy-listener"))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(listener.Close)
	code, stdout, stderr = runCommand(t, noHold(t), args...)
	if code != 0 || !strings.Contains(stdout, "nothing to create") || strings.Contains(stdout, "created") {
		t.Fatalf("the next up's create beside the listener = %d, want 0 with nothing created\n%s%s", code, stdout, stderr)
	}
}

// A break-glass create refuses while the server's connection list shows any client but its own:
// a listener still running after a bucket's loss, whose writes the new bucket would serve. Nothing
// is created, and the refusal names the connection. A list that does not show the command's own
// connection (another server's, or one whose read fails) refuses too, since it cannot show the
// server is alone. The control is the same run without the check (--connz pointing at a list with
// the listener left out), which creates the bucket under the running listener: the check is what
// stops it.
func TestBreakGlassRefusesWhileAnotherClientIsConnected(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, monitor, seed := listenerUserServer(t)
	listener, err := testnats.ConnectSeed(t, uri, seed, natsgo.Name("envoy-listener"))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(listener.Close)
	t.Setenv("NATS_NKEY_SEED", seed)
	watcher, js := jetStream(t, uri)

	code, stdout, stderr := runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", uri, "--connz", monitor)
	if code != 1 || !strings.Contains(stderr, `name "envoy-listener"`) || strings.Contains(stdout, "created") {
		t.Fatalf("create --break-glass beside a running listener = %d, want refused naming it\n%s%s", code, stdout, stderr)
	}
	if _, err := js.StreamInfo("KV_" + bus.SessionBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the refused run made the bucket: %v", err)
	}

	_, otherMonitor, _ := listenerUserServer(t)
	code, _, stderr = runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", uri, "--connz", otherMonitor)
	if code != 1 || !strings.Contains(stderr, "cannot see its own connection") {
		t.Fatalf("create --break-glass with another server's list = %d, want refused\n%s", code, stderr)
	}
	code, _, stderr = runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", uri, "--connz", "http://127.0.0.1:1")
	if code != 1 || !strings.Contains(stderr, "could not be read") {
		t.Fatalf("create --break-glass with an unreadable list = %d, want refused\n%s", code, stderr)
	}

	// The control: with the listener (and the test's watcher) left out of the list, the same run
	// creates the bucket while the listener still runs: the check is what stops it.
	hidden := connzWithout(t, monitor, listener, watcher)
	code, stdout, stderr = runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", uri, "--connz", hidden)
	if code != 0 || !strings.Contains(stdout, "created "+bus.SessionBucket) {
		t.Fatalf("control: the create without the check = %d, want it to create under the listener\n%s%s", code, stdout, stderr)
	}
	if _, err := js.StreamInfo("KV_" + bus.SessionBucket); err != nil {
		t.Fatalf("control: the create without the check made nothing: %v", err)
	}
}

// A create that finds every bucket it names already there changes nothing, and needs no hold: it
// reads each bucket's stream on the operator's connection, says every one exists, and exits 0
// without opening the broker's hold routes for a hold. A run with one bucket missing goes on to its
// hold (its records opened); here the routes are closed, so it stops there, having created nothing.
func TestACreateWithEveryBucketThereNeedsNoHold(t *testing.T) {
	uri, _, _ := listenerUserServer(t)
	_, js := jetStream(t, uri)
	present := []string{bus.InterestBucket, bus.SessionBucket}
	for _, bucket := range present {
		config, _ := bus.CopiedBucketConfig(bucket, 1)
		if _, err := js.CreateKeyValue(&config); err != nil {
			t.Fatalf("create %s: %v", bucket, err)
		}
	}
	opened := 0
	e := env{
		records: func(context.Context, writerhold.HubFlags, bool) (*holdrecord.Client, error) {
			opened++
			return nil, errors.New("the hold routes are closed")
		},
		hub: func(context.Context, writerhold.HubFlags) (*writerhold.Operator, error) {
			return &writerhold.Operator{Connect: func(context.Context) (*natsgo.Conn, error) { return natsgo.Connect(uri) }}, nil
		},
		now: time.Now,
	}
	args := []string{"create", "--reason", "a deploy", "--broker-url", "https://broker", "--token-file", "/dev/null",
		"--verify-keys", "k", "--signing-key-secret", "s", "--minters-report", "r", "--probe-token-file", "p", "--nats", uri}
	run := func(buckets ...string) (int, string, string) {
		full := slices.Clone(args)
		for _, bucket := range buckets {
			full = append(full, "--bucket", bucket)
		}
		return runCommand(t, e, full...)
	}
	code, stdout, stderr := run(present...)
	if code != 0 || !strings.Contains(stdout, "nothing to create, so no hold is set") || opened != 0 {
		t.Fatalf("create with every bucket there = %d, hold routes opened %d times\n%s%s", code, opened, stdout, stderr)
	}
	code, stdout, stderr = run(bus.InterestBucket, bus.RoleClaimBucket)
	if code != 1 || !strings.Contains(stderr, "the hold routes are closed") || strings.Contains(stdout, "created") {
		t.Fatalf("create with a bucket missing = %d, want it to reach its hold and stop there\n%s%s", code, stdout, stderr)
	}
	if _, err := js.StreamInfo("KV_" + bus.RoleClaimBucket); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("the run with no hold created %s: %v", bus.RoleClaimBucket, err)
	}
}

// A --nats URL may carry its credential in its userinfo, a token or a user and password, which
// nats.go authenticates with, and bus.Connect takes it without a scheme too. No line the command
// writes carries that credential: neither the break-glass line, which names the server, nor its
// connect failure.
func TestNoLineTheCommandWritesCarriesTheURLsCredential(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	for _, name := range []string{"NATS_NKEY_SEED", "NATS_NKEY_SEED_FILE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	const secret = "s3cret-token"
	server, uri := testnats.Start(t)
	hub := strings.TrimPrefix(uri, "nats://")
	code, stdout, stderr := runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket,
		"--nats", "user:"+secret+"@"+hub, "--connz", server.MonitorURL())
	if code != 0 || !strings.Contains(stderr, " on nats://"+hub+",") {
		t.Fatalf("create --break-glass over a scheme-less URL with a credential = %d, want 0 and a line naming nats://%s\n%s%s", code, hub, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, secret) {
		t.Fatalf("create --break-glass wrote the URL's credential:\n%s%s", stdout, stderr)
	}
	code, stdout, stderr = runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket,
		"--nats", "user:"+secret+"@127.0.0.1:1", "--connz", server.MonitorURL())
	if code != 1 || !strings.Contains(stderr, "connect to nats://127.0.0.1:1:") || strings.Contains(stdout+stderr, secret) {
		t.Fatalf("create --break-glass to a closed port = %d, want 1 naming nats://127.0.0.1:1 and no credential\n%s%s", code, stdout, stderr)
	}
}

// A word the command cannot read as its own is never echoed, since an nkey seed may have been
// pasted there: a stray argument is named by its position, a value its flag cannot parse by the
// flag, an unknown or malformed flag not at all, and a flag value that holds a seed by the flag.
// Each is a usage error, 2, before anything connects. The seed is a real one (testnats.User).
func TestNoWordTheCommandCannotReadIsEchoed(t *testing.T) {
	seed, _ := testnats.User(t)
	const nowhere = "nats://127.0.0.1:1"
	for _, c := range []struct {
		name string
		args []string
		says string
	}{
		{"a stray argument", []string{"create", "--nats", nowhere, seed}, `word 4 after "buckets" is no flag and no flag's value`},
		{"a stray argument after --", []string{"backup", "--", seed}, `word 3 after "buckets" is no flag and no flag's value`},
		{"a malformed number", []string{"create", "--nats", nowhere, "--replicas", seed}, "--replicas: a malformed value"},
		{"a malformed number after =", []string{"create", "--nats", nowhere, "--replicas=" + seed}, "--replicas: a malformed value"},
		{"a malformed boolean", []string{"create", "--nats", nowhere, "--break-glass=" + seed}, "--break-glass: a malformed value"},
		{"an unknown flag", []string{"create", "--nats", nowhere, "--" + seed}, "a flag this command does not take, or a malformed one"},
		{"a flag of bad syntax", []string{"create", "---" + seed}, "a flag this command does not take, or a malformed one"},
		{"a seed as --reason", []string{"create", "--nats", nowhere, "--reason", seed}, "--reason: an nkey seed, which is never an argument"},
		{"a seed inside --bucket", []string{"create", "--nats", nowhere, "--bucket", "x-" + seed}, "--bucket: an nkey seed, which is never an argument"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, stderr := runCommand(t, noHold(t), c.args...)
			if strings.Contains(stdout+stderr, seed) || strings.Contains(stdout+stderr, seed[:20]) {
				t.Fatalf("%s echoed the seed (exit %d):\n%s%s", c.name, code, stdout, stderr)
			}
			if code != 2 || !strings.Contains(stderr, c.says) {
				t.Fatalf("%s = %d, want the usage error %q\n%s", c.name, code, c.says, stderr)
			}
		})
	}
}

// The replica count a bucket is created with is the deployment's, read as every program of it
// reads ENVOY_NATS_REPLICAS (bus.ParseNATSReplicas): a count outside 1 to bus.MaxReplicas, from
// --replicas or from the variable, is refused as a usage error, with 2, before the command
// connects.
func TestAReplicaCountOutsideOneToFiveIsAUsageError(t *testing.T) {
	const nowhere = "nats://127.0.0.1:1"
	held := []string{"--reason", "r", "--broker-url", "https://broker", "--token-file", "f", "--verify-keys", "k",
		"--signing-key-secret", "s", "--minters-report", "m", "--probe-token-file", "p"}
	breakGlass := []string{"--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--connz", "http://127.0.0.1:1"}
	for _, c := range []struct {
		env  string
		args []string
		says string
	}{
		{"", append(slices.Clone(held), "--replicas", "6"), "--replicas 6"},
		{"", append(slices.Clone(held), "--replicas", "0"), "--replicas 0"},
		{"", append(slices.Clone(held), "--replicas", "-1"), "--replicas -1"},
		{"6", held, bus.ReplicasEnvVar + ` "6"`},
		{"three", held, bus.ReplicasEnvVar + ` "three"`},
		{"6", breakGlass, bus.ReplicasEnvVar + ` "6"`},
	} {
		t.Run(fmt.Sprintf("%s=%q %v", bus.ReplicasEnvVar, c.env, c.args), func(t *testing.T) {
			t.Setenv(bus.ReplicasEnvVar, c.env)
			args := append([]string{"create", "--nats", nowhere}, c.args...)
			code, stdout, stderr := runCommand(t, noHold(t), args...)
			if code != 2 || !strings.Contains(stderr, c.says) || !strings.Contains(stderr, "from 1 to 5") {
				t.Fatalf("%q = %d, want the usage error naming %s and 1 to 5\n%s%s", args, code, c.says, stdout, stderr)
			}
		})
	}
}

// A --bucket named twice is one bucket: a break-glass create reads, logs and creates it once, as
// one named once.
func TestABucketNamedTwiceIsOneBucket(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, monitor, seed := listenerUserServer(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	watcher, _ := jetStream(t, uri)
	requests := watchStreamRequests(t, watcher)
	stream := "KV_" + bus.SessionBucket
	code, stdout, stderr := runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--bucket", bus.SessionBucket,
		"--nats", uri, "--connz", connzWithout(t, monitor, watcher))
	if code != 0 || stdout != "created "+bus.SessionBucket+" (1 copies)\n" || strings.Count(stderr, "break-glass: operator op creates") != 1 {
		t.Fatalf("create --break-glass with %s named twice = %d, want it created and logged once\n%s%s", bus.SessionBucket, code, stdout, stderr)
	}
	// The run's read, the create's own lookup, the create and the handle open, once each.
	if got, want := requests.take(t), []string{"INFO." + stream, "INFO." + stream, "CREATE." + stream, "INFO." + stream}; !slices.Equal(got, want) {
		t.Fatalf("create --break-glass with %s named twice sent %v, want %v", bus.SessionBucket, got, want)
	}
}

var _ = io.Discard
