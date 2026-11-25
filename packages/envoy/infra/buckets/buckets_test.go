package buckets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// world is what the fake kubectl and ssh answer: the pods the API lists, each node's runtime list
// (or the error reading it), and each host query's output, this machine's (--this-machine, run
// through sh) under the key here. hostAnswers is consumed in order, so a test can make the second
// proof differ from the first.
type world struct {
	pods        string
	nodes       string
	running     map[string]string
	runningErr  map[string]error
	hostAnswers map[string][]string
	hostErr     map[string]error
	// hang names the programs ("kubectl", or the ssh program) whose calls never answer until
	// their context ends, as a connected request that stops answering does.
	hang  map[string]bool
	calls []string
}

const noPods = `{"items":[]}`

// here is the key of world's answers to the host query --this-machine runs through sh.
const here = "this machine"

func clean(processes int) string {
	return fmt.Sprintf("processes=%d\n%s\n", processes, hostQueryDone)
}

func (w *world) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	w.calls = append(w.calls, name+" "+strings.Join(args, " "))
	if w.hang[name] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if name == "kubectl" {
		if len(args) > 2 && args[0] == "--context" {
			args = args[2:]
		}
		if !strings.HasPrefix(args[0], "--request-timeout=") {
			return nil, fmt.Errorf("kubectl called without its request timeout: %v", args)
		}
		args = args[1:]
		switch {
		case args[0] == "get" && args[1] == "pods":
			return []byte(w.pods), nil
		case args[0] == "get" && args[1] == "nodes":
			return []byte(w.nodes), nil
		case args[0] == "get" && args[1] == "--raw":
			node := strings.Split(args[2], "/")[4]
			if err := w.runningErr[node]; err != nil {
				return nil, err
			}
			return []byte(w.running[node]), nil
		}
		return nil, fmt.Errorf("unexpected kubectl %v", args)
	}
	host := args[len(args)-2]
	if name == "sh" && len(args) == 2 && args[0] == "-c" {
		host = here
	}
	if err := w.hostErr[host]; err != nil {
		return nil, err
	}
	answers := w.hostAnswers[host]
	if len(answers) == 0 {
		return nil, fmt.Errorf("no answer left for host %s", host)
	}
	w.hostAnswers[host] = answers[1:]
	return []byte(answers[0]), nil
}

// A kubectl or ssh call that stops answering is ended at subprocessTimeout and blocks the proof,
// so the command answers, refused, rather than waiting until the operator interrupts it. At
// 7b262c2e0a the calls had no deadline and the command never returned.
func TestACallThatStopsAnsweringBlocksTheProofWithinItsBound(t *testing.T) {
	saved := subprocessTimeout
	subprocessTimeout = 200 * time.Millisecond
	t.Cleanup(func() { subprocessTimeout = saved })
	uri, _ := server(t)
	for _, c := range []struct {
		program string
		args    []string
		blocked string
	}{
		{"kubectl", []string{"--kube-context", "prod"}, "kubernetes prod: list pods: kubectl did not finish within 200ms"},
		{"ssh", []string{"--host", "box-1"}, "host box-1: unreachable or the query failed: ssh did not finish within 200ms"},
	} {
		t.Run(c.program, func(t *testing.T) {
			w := &world{hang: map[string]bool{c.program: true}}
			answered := make(chan [3]string, 1)
			go func() {
				code, stdout, stderr := runCommand(t, w, nil, append([]string{"check", "--nats", uri}, c.args...)...)
				answered <- [3]string{strconv.Itoa(code), stdout, stderr}
			}()
			select {
			case got := <-answered:
				if got[0] != "1" || !strings.Contains(got[2], c.blocked) {
					t.Fatalf("check with a %s call that never answers = %s, want refused naming %q\n%s%s", c.program, got[0], c.blocked, got[1], got[2])
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("check with a %s call that never answers had not returned after 15s", c.program)
			}
		})
	}
}

func podJSON(namespace, name, uid, node, container string, terminated bool) string {
	state := `{"running":{}}`
	if terminated {
		state = `{"terminated":{}}`
	}
	return fmt.Sprintf(`{"items":[{"metadata":{"name":%q,"namespace":%q,"uid":%q},"spec":{"nodeName":%q,"containers":[{"name":%q}]},"status":{"containerStatuses":[{"name":%q,"state":%s}]}}]}`,
		name, namespace, uid, node, container, container, state)
}

// server starts a NATS server of the test's own and returns its URL and a JetStream context on it.
func server(t *testing.T) (string, natsgo.JetStreamContext) {
	t.Helper()
	_, uri := testnats.Start(t)
	_, js := jetStream(t, uri)
	return uri, js
}

func runCommand(t *testing.T, w *world, client *http.Client, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if client == nil {
		client = http.DefaultClient
	}
	code := command(context.Background(), args, &stdout, &stderr, w.run, client)
	return code, stdout.String(), stderr.String()
}

func created(t *testing.T, js natsgo.JetStreamContext, bucket string) time.Time {
	t.Helper()
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read %s: %v", bucket, err)
	}
	return info.Created
}

// Every writer proven ended, recreate deletes the bucket and creates it again with its copied
// configuration: a new stream with the session bucket's age. The kubelet's runtime list of every
// node is read, and the host query is taken twice, before the delete and before the create.
func TestRecreateOnceEveryWriterIsProvenEnded(t *testing.T) {
	uri, js := server(t)
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	before := created(t, js, bus.SessionBucket)
	w := &world{pods: noPods, nodes: "node-a node-b", running: map[string]string{"node-a": noPods, "node-b": noPods},
		hostAnswers: map[string][]string{"box-1": {clean(212), clean(213)}}}
	code, stdout, stderr := runCommand(t, w, nil, "recreate", "--nats", uri, "--bucket", bus.SessionBucket, "--kube-context", "prod", "--host", "box-1")
	if code != 0 {
		t.Fatalf("recreate = %d\n%s%s", code, stdout, stderr)
	}
	info, err := js.StreamInfo("KV_" + bus.SessionBucket)
	if err != nil {
		t.Fatalf("read the recreated bucket: %v", err)
	}
	if !info.Created.After(before) || info.Config.MaxAge != bus.SessionBucketMaxAge {
		t.Fatalf("the bucket after recreate: created %s (before %s), max age %s; want a new stream with %s", info.Created, before, info.Config.MaxAge, bus.SessionBucketMaxAge)
	}
	for _, call := range []string{"kubectl --context prod --request-timeout=45s get --raw /api/v1/nodes/node-a/proxy/runningpods/", "kubectl --context prod --request-timeout=45s get --raw /api/v1/nodes/node-b/proxy/runningpods/"} {
		if !strings.Contains(strings.Join(w.calls, "\n"), call) {
			t.Fatalf("the proof never read %q; calls:\n%s", call, strings.Join(w.calls, "\n"))
		}
	}
	if len(w.hostAnswers["box-1"]) != 0 {
		t.Fatalf("the host was queried %d times, want twice", 2-len(w.hostAnswers["box-1"]))
	}
}

// Each thing that may still run a writer refuses, and leaves the bucket as it was: a pod the API
// lists, a force-deleted pod its node's runtime still runs, a node whose runtime list does not
// answer, a host running envoy-listener, an unreachable host, a host query cut short or reading no
// process, and an open writer connection in the census.
func TestEveryWriterNotProvenEndedRefuses(t *testing.T) {
	census := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total": 1, "offset": 0, "connections": []map[string]any{{"name": name, "ip": "100.64.0.9", "port": 41000, "authorized_user": "UABC"}}})
		}))
	}
	for _, c := range []struct {
		name    string
		world   world
		connz   string
		blocked string
	}{
		{"a listener pod the API lists", world{pods: podJSON("hub", "hub-listener-0", "uid-1", "node-a", "listener", false), nodes: "node-a", running: map[string]string{"node-a": noPods}},
			"", "pod hub/hub-listener-0 (uid uid-1, node node-a) runs container listener"},
		{"a force-deleted pod its runtime still runs", world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": podJSON("dispatch", "dispatch-7", "uid-2", "node-a", "envoy-listener", false)}},
			"", "node node-a's runtime still runs pod dispatch/dispatch-7 (uid uid-2) container envoy-listener"},
		{"a runtime list that does not answer", world{pods: noPods, nodes: "node-a", runningErr: map[string]error{"node-a": errors.New("Error from server (Forbidden)")}},
			"", "node node-a: read its runtime's running pods: Error from server (Forbidden)"},
		{"no node to read", world{pods: noPods, nodes: ""}, "", "the node list is empty"},
		{"a host running envoy-listener", world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": noPods}, hostAnswers: map[string][]string{"box-1": {"4242 envoy-listener\nprocesses=180\n" + hostQueryDone + "\n"}}},
			"", "host box-1: process 4242 envoy-listener runs"},
		{"an unreachable host", world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": noPods}, hostErr: map[string]error{"box-1": errors.New("ssh: connect to host box-1 port 22: Connection timed out")}},
			"", "host box-1: unreachable or the query failed"},
		{"a host query cut short", world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": noPods}, hostAnswers: map[string][]string{"box-1": {"processes=12\n"}}},
			"", "host box-1: the query did not run to its end"},
		{"a host query that read no process", world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": noPods}, hostAnswers: map[string][]string{"box-1": {clean(0)}}},
			"", "host box-1: the query read no process at all"},
		{"an open listener connection", world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": noPods}, hostAnswers: map[string][]string{"box-1": {clean(150)}}},
			"envoy-role-lane", `connection "envoy-role-lane" from 100.64.0.9:41000 as "UABC" is open`},
	} {
		t.Run(c.name, func(t *testing.T) {
			uri, js := server(t)
			config, _ := bus.CopiedBucketConfig(bus.InterestBucket, 1)
			if _, err := js.CreateKeyValue(&config); err != nil {
				t.Fatalf("create the bucket: %v", err)
			}
			before := created(t, js, bus.InterestBucket)
			args := []string{"recreate", "--nats", uri, "--bucket", bus.InterestBucket, "--kube-context", "prod", "--host", "box-1"}
			if c.connz != "" {
				monitor := census(c.connz)
				t.Cleanup(monitor.Close)
				args = append(args, "--connz", monitor.URL)
			}
			if c.world.hostAnswers == nil && c.world.hostErr == nil {
				c.world.hostAnswers = map[string][]string{"box-1": {clean(150)}}
			}
			code, stdout, stderr := runCommand(t, &c.world, nil, args...)
			if code != 1 || !strings.Contains(stderr, "refused") || !strings.Contains(stderr, c.blocked) {
				t.Fatalf("recreate = %d, want 1 refused naming %q\n%s%s", code, c.blocked, stdout, stderr)
			}
			if after := created(t, js, bus.InterestBucket); !after.Equal(before) {
				t.Fatalf("a refused recreate changed the bucket: created %s, was %s", after, before)
			}
		})
	}
}

// A writer that starts between the proof before the delete and the proof before the create may
// have written to the deleted bucket, so the bucket is left deleted and nothing is created: the
// operator stops it and runs create.
func TestRecreateLeavesTheBucketDeletedWhenAWriterStartsBetweenItsProofs(t *testing.T) {
	uri, js := server(t)
	config, _ := bus.CopiedBucketConfig(bus.RoleClaimBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	w := &world{hostAnswers: map[string][]string{"box-1": {clean(150), "77 envoy-listener\nprocesses=151\n" + hostQueryDone + "\n"}}}
	code, stdout, stderr := runCommand(t, w, nil, "recreate", "--nats", uri, "--bucket", bus.RoleClaimBucket, "--host", "box-1")
	if code != 1 || !strings.Contains(stdout, "deleted "+bus.RoleClaimBucket) || !strings.Contains(stderr, "the buckets are deleted and not created") {
		t.Fatalf("recreate = %d\n%s%s", code, stdout, stderr)
	}
	if _, err := js.KeyValue(bus.RoleClaimBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the bucket after a refused create: %v, want it missing", err)
	}
}

// A first deployment's server holds no notification stream and no bucket, and create makes every
// copied bucket with its configuration once the inventory is proven; a server a listener has
// connected to holds the stream, and --first-deployment refuses it.
func TestFirstDeploymentCreatesEveryBucketOnlyOnAServerNoWriterReached(t *testing.T) {
	uri, js := server(t)
	w := &world{hostAnswers: map[string][]string{"box-1": {clean(150), clean(150)}}}
	code, stdout, stderr := runCommand(t, w, nil, "create", "--nats", uri, "--first-deployment", "--host", "box-1", "--replicas", "1")
	if code != 0 {
		t.Fatalf("create --first-deployment = %d\n%s%s", code, stdout, stderr)
	}
	for _, bucket := range bus.CopiedBuckets() {
		want, _ := bus.CopiedBucketConfig(bucket, 1)
		info, err := js.StreamInfo("KV_" + bucket)
		if err != nil || info.Config.MaxAge != want.TTL || info.Config.Storage != natsgo.FileStorage {
			t.Fatalf("%s after create: %v, %+v; want file storage and max age %s", bucket, err, info, want.TTL)
		}
	}

	if _, err := js.AddStream(&natsgo.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("add the notification stream, as a listener's first connect does: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete a bucket: %v", err)
	}
	code, stdout, stderr = runCommand(t, w, nil, "create", "--nats", uri, "--first-deployment", "--host", "box-1")
	if code != 1 || !strings.Contains(stderr, "the server holds "+bus.Stream) {
		t.Fatalf("create --first-deployment on a server a writer reached = %d\n%s%s", code, stdout, stderr)
	}
	if _, err := js.KeyValue(bus.SessionBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the refused create made the bucket: %v", err)
	}
}

// makeBuckets creates each of buckets with its copied configuration, as `envoy-infra buckets
// create` makes it.
func makeBuckets(t *testing.T, js natsgo.JetStreamContext, buckets ...string) {
	t.Helper()
	for _, bucket := range buckets {
		config, _ := bus.CopiedBucketConfig(bucket, 1)
		if _, err := js.CreateKeyValue(&config); err != nil {
			t.Fatalf("create %s: %v", bucket, err)
		}
	}
}

// bucketFlags is each of buckets as a --bucket flag.
func bucketFlags(buckets ...string) []string {
	var flags []string
	for _, bucket := range buckets {
		flags = append(flags, "--bucket", bucket)
	}
	return flags
}

// A writer of the buckets "runs" on every place named, and a create whose buckets all exist still
// exits 0: it changes nothing, so it runs no proof - the fake runner records no call - says that
// every bucket exists, and leaves each as it was. That holds for the four buckets a listener with
// no GitHub route copies, the single-node compose file's, named with --bucket, and for every
// copied bucket, named by none. At 036c9e7a60 the create took the proof and refused, exit 1.
func TestACreateWithEveryBucketThereTakesNoProof(t *testing.T) {
	uri, js := server(t)
	single := []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.KnownSessionBucket}
	makeBuckets(t, js, single...)
	writer := "4242 envoy-listener\nprocesses=180\n" + hostQueryDone + "\n"
	for _, c := range []struct {
		name    string
		make    []string
		buckets []string
		flags   []string
	}{
		{"the four a listener with no GitHub route copies", nil, single, bucketFlags(single...)},
		{"every copied bucket", []string{bus.CIStateBucket}, bus.CopiedBuckets(), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			makeBuckets(t, js, c.make...)
			before := map[string]time.Time{}
			for _, bucket := range c.buckets {
				before[bucket] = created(t, js, bucket)
			}
			w := &world{hostAnswers: map[string][]string{"box-1": {writer}, here: {writer}}}
			code, stdout, stderr := runCommand(t, w, nil, append([]string{"create", "--nats", uri, "--host", "box-1", "--this-machine"}, c.flags...)...)
			want := "every bucket named exists (" + strings.Join(c.buckets, ", ") + "): nothing to create, so no writer proof is taken\n"
			if code != 0 || stdout != want || len(w.calls) != 0 {
				t.Fatalf("create with every bucket there while a writer runs = %d, calls %q, want 0, no call and %q\n%s%s", code, w.calls, want, stdout, stderr)
			}
			for bucket, at := range before {
				if after := created(t, js, bucket); !after.Equal(at) {
					t.Fatalf("the create changed %s: created %s, was %s", bucket, after, at)
				}
			}
		})
	}
}

// A create with one named bucket missing takes the whole proof: while a writer runs it refuses
// naming it and makes nothing, and once none runs it makes the missing bucket alone. The control
// is the same writer once every bucket exists: that create exits 0 with no proof, so the refusal
// was the missing bucket's (at 036c9e7a60 the control fails, refused). --first-deployment, which
// says the server holds no bucket, keeps its proof with every bucket there, and refuses.
func TestACreateWithABucketMissingTakesTheProof(t *testing.T) {
	uri, js := server(t)
	single := []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.KnownSessionBucket}
	makeBuckets(t, js, bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket)
	writer := "4242 envoy-listener\nprocesses=180\n" + hostQueryDone + "\n"
	args := append([]string{"create", "--nats", uri, "--host", "box-1"}, bucketFlags(single...)...)

	w := &world{hostAnswers: map[string][]string{"box-1": {writer}}}
	code, stdout, stderr := runCommand(t, w, nil, args...)
	if code != 1 || !strings.Contains(stderr, "blocked: host box-1: process 4242 envoy-listener runs") || len(w.calls) != 1 {
		t.Fatalf("create with %s missing while a writer runs = %d, calls %q, want refused by the proof\n%s%s", bus.KnownSessionBucket, code, w.calls, stdout, stderr)
	}
	if _, err := js.KeyValue(bus.KnownSessionBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the refused create made %s: %v", bus.KnownSessionBucket, err)
	}

	w = &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
	code, stdout, stderr = runCommand(t, w, nil, args...)
	if code != 0 || !strings.Contains(stdout, "created "+bus.KnownSessionBucket+" (1 copies)") || !strings.Contains(stdout, bus.SessionBucket+" exists; left as it is") || len(w.calls) != 1 {
		t.Fatalf("create with %s missing and no writer = %d, calls %q, want it created after the proof\n%s%s", bus.KnownSessionBucket, code, w.calls, stdout, stderr)
	}

	w = &world{hostAnswers: map[string][]string{"box-1": {writer}}}
	if code, stdout, stderr := runCommand(t, w, nil, args...); code != 0 || len(w.calls) != 0 {
		t.Fatalf("control: create with every bucket there while the writer runs = %d, calls %q, want 0 with no proof\n%s%s", code, w.calls, stdout, stderr)
	}

	w = &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
	code, stdout, stderr = runCommand(t, w, nil, append(args, "--first-deployment")...)
	if code != 1 || !strings.Contains(stderr, "--first-deployment: the server holds KV_") || len(w.calls) != 1 {
		t.Fatalf("create --first-deployment with every bucket there = %d, calls %q, want its proof taken and refused\n%s%s", code, w.calls, stdout, stderr)
	}
}

// No inventory is a usage error, with --first-deployment as without it: an empty server is no
// proof that no writer runs (a hub on a new disk is empty while its old clients still run), so
// nothing would be proven. At 7b262c2e0a --first-deployment alone was accepted and created the
// buckets with no writer proof. --this-machine alone is an inventory, of the machine the command
// runs on; at 1160350af8 it was no flag at all.
func TestTheCommandNeedsAnInventory(t *testing.T) {
	for _, args := range [][]string{{"create", "--nats", "nats://127.0.0.1:1"}, {"create", "--nats", "nats://127.0.0.1:1", "--first-deployment"}} {
		code, _, stderr := runCommand(t, &world{}, nil, args...)
		if code != 2 || !strings.Contains(stderr, "name every place a writer of the deployment runs") {
			t.Fatalf("%v with no inventory = %d: %s", args, code, stderr)
		}
	}
	code, _, stderr := runCommand(t, &world{}, nil, "recreate", "--nats", "nats://127.0.0.1:1", "--host", "box-1", "--bucket", "legion_agents")
	if code != 2 || !strings.Contains(stderr, "legion_agents is no bucket a listener copies") {
		t.Fatalf("recreate of a bucket no listener copies = %d: %s", code, stderr)
	}
	// --this-machine names a place: the run goes on to connect, and fails there, at a closed port.
	code, _, stderr = runCommand(t, &world{}, nil, "create", "--nats", "nats://127.0.0.1:1", "--this-machine")
	if code != 1 || !strings.Contains(stderr, "connect to nats://127.0.0.1:1") {
		t.Fatalf("create --this-machine = %d, want past the inventory check and refused at the connect: %s", code, stderr)
	}
}

// fixtureSecret is a credential a test hands the command: random, so no literal of the source reads
// as one and no word of it is on a line for any other reason, and tag names it in a failure. Its "+"
// is no host name character, so a host read from it fails its lookup at once: Go's resolver refuses
// a name that is no domain name before it asks DNS.
func fixtureSecret(tag string) string {
	return tag + "+" + rand.Text()
}

// A --nats URL whose user info a parse does not read where it is written prints no part of itself.
// One that does not parse fails nats.go's own parse with a *url.Error, whose text quotes the whole
// URL, password included; one whose user or password holds a `/` ends its authority there, so it
// parses with the credential's head as its host, which nats.go dials, and the failed lookup names
// that head. The command refuses both before any dial. One with a space before it reads well once
// bus trims it, so it reaches nats.go, which parses it as given and fails with that *url.Error:
// connectFailure prints fixed text for it. The command reaches nats.go with any URL, since it sets
// bus.AllowRemoteEnvVar. Both connects, the normal path's and --break-glass's, print the same fixed
// text for each: no part of the URL, not even the host. At ff9c03a4a0 the two URLs that parse
// printed `lookup <the credential's head>: no such host`.
func TestAConnectToAURLThatDoesNotParsePrintsNoPartOfIt(t *testing.T) {
	at := string(rune(64))
	user, head, tail := fixtureSecret("user"), fixtureSecret("head"), fixtureSecret("tail")
	for _, c := range []struct {
		name, url string
		secrets   []string
	}{
		{"a password holding a slash, which does not parse", "nats://" + user + ":" + head + "/" + tail + at + "db.example:4222", []string{user, head, tail}},
		{"a token holding a slash, its head read as the host", "nats://" + head + "/" + tail + at + "db.example:4222", []string{head, tail}},
		{"a password holding a slash after digits, the user read as the host", "nats://" + user + ":2024/" + tail + at + "db.example:4222", []string{user, tail}},
		{"a space before the URL, which nats.go does not trim", " nats://" + user + ":" + tail + at + "db.example:4222", []string{user, tail}},
	} {
		for _, args := range [][]string{
			{"create", "--nats", c.url, "--this-machine"},
			{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", c.url},
		} {
			code, stdout, stderr := runCommand(t, &world{}, nil, args...)
			const want = "envoy-infra buckets: connect: the --nats URL does not parse as user info and a server, so no part of it is printed: percent-encode every character of its user and its password but letters, digits and `-._~` (`/`, `?`, `#`, `@` and `:` among them)\n"
			if code != 1 || !strings.Contains(stderr, want) {
				t.Errorf("%s, %s = %d, want 1 and %q\n%s%s", c.name, args[1], code, want, stdout, stderr)
			}
			for _, part := range append(c.secrets, "db.example") {
				if strings.Contains(stdout+stderr, part) {
					tag, _, _ := strings.Cut(part, "+")
					t.Errorf("%s, %s printed the URL's %s:\n%s%s", c.name, args[1], tag, stdout, stderr)
				}
			}
		}
	}
}

// --this-machine queries the machine the command runs on with no ssh: the host query, run through
// sh -c, judged as a host's. It refuses while a writer process runs, naming it under "this
// machine", passes on a machine where none runs, and blocks on a query that failed, did not run to
// its end or read no process. No ssh is run. The session bucket alone is named, so the writers are
// the listener's names alone. At 1160350af8 there was no such flag, and every case is a usage error.
func TestThisMachineIsQueriedHereWithNoSSH(t *testing.T) {
	uri, _ := server(t)
	query := "sh -c " + hostQuery(fence{}.writerProcesses())
	for _, c := range []struct {
		name    string
		answer  string
		err     error
		code    int
		printed string
	}{
		{"a writer runs", "4242 envoy-listener\nprocesses=180\n" + hostQueryDone + "\n", nil, 1, "blocked: this machine: process 4242 envoy-listener runs"},
		{"no writer runs", clean(180), nil, 0, "proven: this machine: none of its 180 processes is envoy-listener or envoy-listener-* or listener"},
		{"the query failed", "", errors.New("sh: exit status 2"), 1, "blocked: this machine: the query failed: sh: exit status 2"},
		{"the query cut short", "processes=12\n", nil, 1, "blocked: this machine: the query did not run to its end"},
		{"the query read no process", clean(0), nil, 1, "blocked: this machine: the query read no process at all"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := &world{hostAnswers: map[string][]string{here: {c.answer}}}
			if c.err != nil {
				w.hostErr = map[string]error{here: c.err}
			}
			code, stdout, stderr := runCommand(t, w, nil, "check", "--nats", uri, "--this-machine", "--bucket", bus.SessionBucket)
			if code != c.code || !strings.Contains(stdout+stderr, c.printed) {
				t.Fatalf("check --this-machine = %d, want %d printing %q\n%s%s", code, c.code, c.printed, stdout, stderr)
			}
			if !slices.Equal(w.calls, []string{query}) {
				t.Fatalf("check --this-machine ran %q, want the host query through sh alone: %q", w.calls, query)
			}
		})
	}
}

// A --this-machine proof by a command that is process 1 of its PID namespace blocks, and runs no
// query: such a command is a container's entrypoint in a PID namespace of its own, whose query
// would read that container's processes alone and call the machine clean. The same command with any
// other process id runs the query and is judged by its answer. Without the check, the process-1
// case ran the query and passed on a clean answer.
func TestThisMachineBlocksAsProcessOneOfItsNamespace(t *testing.T) {
	for _, c := range []struct {
		pid     int
		blocked bool
		calls   int
	}{{1, true, 0}, {4242, false, 1}} {
		w := &world{hostAnswers: map[string][]string{here: {clean(180)}}}
		f := fence{thisMachine: true, pid: c.pid, run: w.run}
		var out bytes.Buffer
		blockers := f.proveThisMachine(context.Background(), &out)
		if blocked := len(blockers) > 0; blocked != c.blocked || len(w.calls) != c.calls {
			t.Fatalf("pid %d: blockers %q, calls %q; want blocked %t and %d calls", c.pid, blockers, w.calls, c.blocked, c.calls)
		}
		if c.blocked && !strings.Contains(blockers[0], "process 1 of its PID namespace") {
			t.Fatalf("pid 1 blocked with %q, want it to name process 1 of its PID namespace", blockers[0])
		}
	}
}

// --this-machine runs the real host query here: it refuses while a real stand-in named
// envoy-listener runs, naming its pid, and passes once the stand-in has ended. The query is the
// one --host sends (TestTheHostQuerySeesARealWriterProcessUnderEachOfItsNamesUntilItEnds), and only
// the stand-in's own line is judged, since this machine may run writers of its own. At 1160350af8
// the flag did not exist and the run was a usage error.
func TestThisMachineRefusesARealWriterProcessUntilItEnds(t *testing.T) {
	uri, _ := server(t)
	writer := startStandIn(t, "envoy-listener")
	judged := ownProcesses(func() []*exec.Cmd { return []*exec.Cmd{writer} })
	var stdout, stderr bytes.Buffer
	code := command(context.Background(), []string{"check", "--nats", uri, "--this-machine"}, &stdout, &stderr, judged, http.DefaultClient)
	if want := fmt.Sprintf("this machine: process %d envoy-listener runs", writer.Process.Pid); code != 1 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("check --this-machine while the stand-in runs = %d, want refused naming %q\n%s%s", code, want, stdout.String(), stderr.String())
	}
	_ = writer.Process.Kill()
	_ = writer.Wait()
	stdout.Reset()
	stderr.Reset()
	if code := command(context.Background(), []string{"check", "--nats", uri, "--this-machine"}, &stdout, &stderr, judged, http.DefaultClient); code != 0 || !strings.Contains(stdout.String(), "proven: this machine: none of its") {
		t.Fatalf("check --this-machine once the stand-in ended = %d, want proven\n%s%s", code, stdout.String(), stderr.String())
	}
}

// The command is run from an operator's workstation against the deployment's hub, which is on
// another machine: it connects to such a server rather than refusing it as bus.Connect refuses a
// non-loopback server by default (ENVOY_ALLOW_REMOTE_NATS). The control is bus.Connect itself with
// the process environment, which refuses the same URL. The .invalid name never resolves, so the
// command's connect fails, for its own reason, not the refusal's. At 7b262c2e0a it was refused.
func TestTheCommandReachesAServerOnAnotherMachine(t *testing.T) {
	const remote = "nats://envoy-buckets-test.invalid:4222"
	if _, err := bus.Connect([]string{remote}); !errors.Is(err, bus.ErrRemoteNATS) {
		t.Fatalf("control: bus.Connect(%s) = %v, want the remote refusal", remote, err)
	}
	w := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
	code, stdout, stderr := runCommand(t, w, nil, "check", "--nats", remote, "--host", "box-1")
	if code != 1 || strings.Contains(stderr, bus.ErrRemoteNATS.Error()) || !strings.Contains(stderr, "connect to "+remote) {
		t.Fatalf("check against a server on another machine = %d, want a connect attempt, not the remote refusal\n%s%s", code, stdout, stderr)
	}
}

// The command takes its replies under its own inbox prefix (InboxPrefix), so a NATS user granted
// subscribe on _INBOX_envoy_buckets.> alone - the deployment's buckets user - can run it: here it
// creates a bucket through that user. The control is the same user on a connection with nats.go's
// default _INBOX prefix, which gets no JetStream answer: the grant bites. At 7b262c2e0a the
// command used the default prefix and failed as the control does.
func TestAUserGrantedOnlyTheCommandsInboxRunsIt(t *testing.T) {
	seed, public := testnats.User(t)
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, InboxPrefix+".>"), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)

	control, err := testnats.ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("control: connect as the user: %v", err)
	}
	defer control.Close()
	controlJS, err := control.JetStream(natsgo.MaxWait(2 * time.Second))
	if err != nil {
		t.Fatalf("control: jetstream: %v", err)
	}
	if _, err := controlJS.AccountInfo(); err == nil {
		t.Fatal("control: the user got a JetStream answer on the default _INBOX prefix, so its grant does not restrict replies")
	}

	w := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
	code, stdout, stderr := runCommand(t, w, nil, "create", "--nats", uri, "--bucket", bus.SessionBucket, "--host", "box-1")
	if code != 0 || !strings.Contains(stdout, "created "+bus.SessionBucket) {
		t.Fatalf("create as a user granted only %s.> = %d\n%s%s", InboxPrefix, code, stdout, stderr)
	}
}

// grantConfig is the authorization of a server whose one restricted user, the nkey user public, may
// publish only to publish and subscribe only to subscribe, and whose users unrestricted, each an
// nkey user's public key, may do anything, as the hub's users are rendered (core.Authorization).
func grantConfig(t *testing.T, public string, publish, subscribe []string, unrestricted ...string) string {
	t.Helper()
	users := []core.User{{Name: "granted", NKey: public, Publish: &core.Rules{Allow: publish}, Subscribe: &core.Rules{Allow: subscribe}}}
	everything := core.Rules{Allow: []string{">"}}
	for i, key := range unrestricted {
		users = append(users, core.User{Name: fmt.Sprintf("unrestricted-%d", i), NKey: key, Publish: &everything, Subscribe: &everything})
	}
	config, err := core.Authorization{Users: users}.Render(nil)
	if err != nil {
		t.Fatalf("render the server's users: %v", err)
	}
	return config
}

// A NATS user granted exactly Grant() - the deployment's buckets user - runs every step the command
// has, against a real server, with no refusal: create --first-deployment, check, recreate and
// create. The control removes one subject at a time from the grant and runs the step that needs
// it, which is refused, and the test names the subject. At 7b262c2e0a no such grant existed, and
// the command's default _INBOX prefix was refused by any user granted its own inbox alone.
func TestAUserGrantedExactlyTheCommandsGrantRunsEveryStep(t *testing.T) {
	publish, subscribe := Grant()
	run := func(t *testing.T, publish []string, args ...string) (int, string) {
		t.Helper()
		seed, public := testnats.User(t)
		_, uri := testnats.StartConfig(t, grantConfig(t, public, publish, subscribe), []string{"-js"}, false)
		t.Setenv("NATS_NKEY_SEED", seed)
		answers := []string{}
		for range 8 {
			answers = append(answers, clean(150))
		}
		w := &world{hostAnswers: map[string][]string{"box-1": answers}}
		code, stdout, stderr := runCommand(t, w, nil, append(append([]string{}, args...), "--nats", uri, "--host", "box-1")...)
		return code, stdout + stderr
	}
	t.Run("every step with the whole grant", func(t *testing.T) {
		seed, public := testnats.User(t)
		_, uri := testnats.StartConfig(t, grantConfig(t, public, publish, subscribe), []string{"-js"}, false)
		t.Setenv("NATS_NKEY_SEED", seed)
		for _, args := range [][]string{
			{"create", "--first-deployment"},
			{"check"},
			{"recreate", "--bucket", bus.SessionBucket},
			{"create"},
		} {
			w := &world{hostAnswers: map[string][]string{"box-1": {clean(150), clean(150)}}}
			code, stdout, stderr := runCommand(t, w, nil, append(append([]string{}, args...), "--nats", uri, "--host", "box-1")...)
			if code != 0 {
				t.Fatalf("%v as a user granted exactly Grant() = %d:\n%s%s", args, code, stdout, stderr)
			}
		}
	})
	for _, c := range []struct {
		subject string
		args    []string
	}{
		{"$JS.API.STREAM.CREATE.KV_" + bus.SessionBucket, []string{"create", "--bucket", bus.SessionBucket}},
		{"$JS.API.STREAM.INFO.KV_" + bus.SessionBucket, []string{"create", "--bucket", bus.SessionBucket}},
		{"$JS.API.STREAM.DELETE.KV_" + bus.SessionBucket, []string{"recreate", "--bucket", bus.SessionBucket}},
		{"$JS.API.STREAM.INFO." + bus.Stream, []string{"create", "--first-deployment", "--bucket", bus.SessionBucket}},
	} {
		t.Run("control without "+c.subject, func(t *testing.T) {
			without := slices.DeleteFunc(slices.Clone(publish), func(subject string) bool { return subject == c.subject })
			if len(without) != len(publish)-1 {
				t.Fatalf("%s is not in Grant()", c.subject)
			}
			if code, output := run(t, without, c.args...); code == 0 {
				t.Fatalf("%v as a user granted Grant() without %s = 0, want refused:\n%s", c.args, c.subject, output)
			}
		})
	}
}

// The host query, run on this machine, sees a real process under every name this repository runs a
// writer as - the listener as envoy-listener, as listener (`go run`, dev-stack.sh, e2e-refill.sh)
// and as the held-port wrapper, Dispatch as envoy-dispatch, dispatch and a two-build dispatch-<x> -
// by its pid, and no longer once it has ended: the instrument the fence trusts can fail. The
// command, through an ssh that runs the query here, refuses while each runs and names it. Other
// writers may run on the machine, so only each started process's own line is judged.
//
// At 7b262c2e0a the query knew only envoy-listener and envoy-dispatch, and the listener and
// dispatch cases fail.
func TestTheHostQuerySeesARealWriterProcessUnderEachOfItsNamesUntilItEnds(t *testing.T) {
	ssh := localSSH(t)
	uri, _ := server(t)
	for _, name := range []string{"envoy-listener", "listener", "envoy-listener-heldport", "envoy-dispatch", "dispatch", "dispatch-e2e"} {
		t.Run(name, func(t *testing.T) {
			process := startStandIn(t, name)
			comm := name[:min(len(name), 15)]
			line := strconv.Itoa(process.Process.Pid) + " " + comm
			query := func() string {
				out, err := exec.Command("sh", "-c", hostQuery(fence{dispatch: true}.writerProcesses())).Output()
				if err != nil {
					t.Fatalf("run the host query: %v", err)
				}
				return string(out)
			}
			if out := query(); !strings.Contains(out, line+"\n") || !strings.HasSuffix(strings.TrimSpace(out), hostQueryDone) {
				t.Fatalf("the host query did not list the running stand-in %q:\n%s", line, out)
			}
			var stdout, stderr bytes.Buffer
			code := command(context.Background(), []string{"check", "--nats", uri, "--host", "this-machine", "--ssh", ssh}, &stdout, &stderr, execRunner, http.DefaultClient)
			if code != 1 || !strings.Contains(stderr.String(), "host this-machine: process "+line+" runs") {
				t.Fatalf("check while the stand-in runs = %d\n%s%s", code, stdout.String(), stderr.String())
			}
			_ = process.Process.Kill()
			_ = process.Wait()
			if out := query(); strings.Contains(out, line+"\n") {
				t.Fatalf("the host query still lists the ended stand-in %q:\n%s", line, out)
			}
		})
	}
}

// startStandIn runs a copy of sleep named name for the test's length, a stand-in for a writer the
// host query reads by name, and returns it.
func startStandIn(t *testing.T, name string) *exec.Cmd {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("no sleep binary to run as a stand-in writer: %v", err)
	}
	raw, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatalf("read sleep: %v", err)
	}
	binary := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(binary, raw, 0o755); err != nil {
		t.Fatalf("write the stand-in: %v", err)
	}
	process := exec.Command(binary, "300")
	if err := process.Start(); err != nil {
		t.Fatalf("start the stand-in: %v", err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	return process
}

// localSSH is a program taking ssh's arguments that runs the remote command here.
func localSSH(t *testing.T) string {
	t.Helper()
	ssh := filepath.Join(t.TempDir(), "local-ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nfor last; do :; done\nexec sh -c \"$last\"\n"), 0o755); err != nil {
		t.Fatalf("write the local ssh: %v", err)
	}
	return ssh
}

// running reports whether process still runs under name (its /proc/<pid>/comm).
func running(process *exec.Cmd, name string) bool {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", process.Process.Pid))
	return err == nil && strings.TrimSpace(string(comm)) == name[:min(len(name), 15)]
}

// ownProcesses is a Runner that runs the real host query here (execRunner, through localSSH) and
// drops every writer line but those of the processes own names: this machine may run writers of its
// own (a dev box runs listeners), and a test judges only the stand-ins it started.
func ownProcesses(own func() []*exec.Cmd) Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := execRunner(ctx, name, args...)
		if err != nil {
			return out, err
		}
		var kept []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			pid, _, isWriter := strings.Cut(line, " ")
			if isWriter && !slices.ContainsFunc(own(), func(p *exec.Cmd) bool { return p != nil && strconv.Itoa(p.Process.Pid) == pid }) {
				continue
			}
			kept = append(kept, line)
		}
		return []byte(strings.Join(kept, "\n") + "\n"), nil
	}
}

// Every run but a break-glass create prints the best-effort warning first, on standard error,
// whatever it then does: a run whose proof holds, a refused one, a usage error. A break-glass
// create takes no proof, and TestBreakGlassCreatesAMissingBucketAsAListenersUser asserts it prints
// no warning. At 7b262c2e0a no run printed it.
func TestEveryRunPrintsTheBestEffortWarning(t *testing.T) {
	uri, _ := server(t)
	for _, c := range []struct {
		name  string
		world *world
		args  []string
		code  int
	}{
		{"a proof that holds", &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}, []string{"check", "--nats", uri, "--host", "box-1"}, 0},
		{"a refused run", &world{hostAnswers: map[string][]string{"box-1": {"7 envoy-listener\nprocesses=150\n" + hostQueryDone + "\n"}}}, []string{"check", "--nats", uri, "--host", "box-1"}, 1},
		{"a usage error", &world{}, []string{"create", "--nats", uri}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := runCommand(t, c.world, nil, c.args...)
			if code != c.code || !strings.HasPrefix(stderr, bestEffortWarning) {
				t.Fatalf("%v = %d, want %d with the warning first on standard error:\n%s", c.args, code, c.code, stderr)
			}
			for _, way := range []string{"an omitted host or dev box", "a renamed binary", "a writer that starts between the final scan and the create", "an unheld restore", "confirm on each host that it has ended"} {
				if !strings.Contains(stderr, way) {
					t.Fatalf("the warning does not name %q:\n%s", way, stderr)
				}
			}
		})
	}
}

// `envoy-infra buckets --help` lists every flag the command takes. The usage is written by hand,
// so each flag of the flag set, read from the flag package's own listing (an action's --help), is
// looked for in it as a line of its own. At 69ddf68a60 the usage had lost --ssh, which the command
// still took and infra/README.md documents.
func TestTheUsageListsEveryFlag(t *testing.T) {
	_, _, listing := runCommand(t, &world{}, nil, "check", "--help")
	defined := regexp.MustCompile(`(?m)^  -([a-z-]+)`).FindAllStringSubmatch(listing, -1)
	if len(defined) == 0 {
		t.Fatalf("`check --help` listed no flag, so nothing was compared:\n%s", listing)
	}
	code, _, help := runCommand(t, &world{}, nil, "--help")
	if code != 0 {
		t.Fatalf("--help = %d, want 0:\n%s", code, help)
	}
	for _, flag := range defined {
		if !regexp.MustCompile(`(?m)^  --` + flag[1] + `\b`).MatchString(help) {
			t.Errorf("--help lists no --%s, which the command takes:\n%s", flag[1], help)
		}
	}
	t.Logf("compared the %d flags the command takes with --help", len(defined))
}

// BEST EFFORT - INVERT WHEN THE SERVER-SIDE HOLD LEAVES NO WRITER A CONNECTION. A
// writer on a host the operator did not name gets past the check: the command creates the bucket
// while it runs. Today's outcome, asserted so that the hold, which excludes every writer at the
// server whatever host it runs on, has to invert it. The same writer named with --host refuses, so
// the pass is not vacuous.
func TestBestEffortProofMissesAWriterOnAnOmittedHostUntilTheServerSideHoldLands(t *testing.T) {
	uri, js := server(t)
	writer := "4242 envoy-listener\nprocesses=180\n" + hostQueryDone + "\n"
	named := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}, "dev-box": {writer}}}
	if code, _, stderr := runCommand(t, named, nil, "create", "--nats", uri, "--host", "box-1", "--host", "dev-box", "--bucket", bus.SessionBucket); code != 1 || !strings.Contains(stderr, "host dev-box: process 4242 envoy-listener runs") {
		t.Fatalf("control: create with the writer's host named = %d, want refused naming it:\n%s", code, stderr)
	}
	omitted := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}, "dev-box": {writer}}}
	code, stdout, stderr := runCommand(t, omitted, nil, "create", "--nats", uri, "--host", "box-1", "--bucket", bus.SessionBucket)
	if code != 0 || !strings.Contains(stdout, "created "+bus.SessionBucket) {
		t.Fatalf("create with the writer's host omitted = %d, want today's best-effort outcome, the bucket created:\n%s%s", code, stdout, stderr)
	}
	if _, err := js.KeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("the bucket after the create: %v", err)
	}
}

// BEST EFFORT - INVERT WHEN THE SERVER-SIDE HOLD LEAVES NO WRITER A CONNECTION. A
// writer whose binary was renamed gets past the host check, which reads process names: the command
// creates the bucket while the renamed writer runs on the named host. The hold refuses and kicks
// its connection whatever its name. The stand-in runs throughout, so the pass is not vacuous.
func TestBestEffortProofMissesARenamedBinaryUntilTheServerSideHoldLands(t *testing.T) {
	uri, js := server(t)
	renamed := startStandIn(t, "renamed-writer")
	var stdout, stderr bytes.Buffer
	code := command(context.Background(), []string{"create", "--nats", uri, "--host", "this-machine", "--ssh", localSSH(t), "--bucket", bus.SessionBucket}, &stdout, &stderr,
		ownProcesses(func() []*exec.Cmd { return []*exec.Cmd{renamed} }), http.DefaultClient)
	if !running(renamed, "renamed-writer") {
		t.Fatal("the renamed stand-in ended during the proof, so the test proves nothing")
	}
	if code != 0 {
		t.Fatalf("create while a renamed writer runs = %d, want today's best-effort outcome, the bucket created:\n%s%s", code, stdout.String(), stderr.String())
	}
	if _, err := js.KeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("the bucket after the create: %v", err)
	}
}

// BEST EFFORT - INVERT WHEN THE SERVER-SIDE HOLD LEAVES NO WRITER A CONNECTION. A
// writer that starts after the final scan and before the create gets past the check: here it starts
// the moment the host query returns clean, and the command still creates the bucket. The hold, kept
// from the check through the change, refuses its connection. The writer runs when the create
// completes, so the pass is not vacuous.
func TestBestEffortProofMissesAWriterStartedAfterTheFinalScanUntilTheServerSideHoldLands(t *testing.T) {
	uri, js := server(t)
	ssh := localSSH(t)
	var writer *exec.Cmd
	judged := ownProcesses(func() []*exec.Cmd { return []*exec.Cmd{writer} })
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := judged(ctx, name, args...)
		if name == ssh && writer == nil {
			writer = startStandIn(t, "envoy-listener")
		}
		return out, err
	}
	var stdout, stderr bytes.Buffer
	code := command(context.Background(), []string{"create", "--nats", uri, "--host", "this-machine", "--ssh", ssh, "--bucket", bus.SessionBucket}, &stdout, &stderr, run, http.DefaultClient)
	if writer == nil || !running(writer, "envoy-listener") {
		t.Fatal("no writer started after the scan, so the test proves nothing")
	}
	if code != 0 {
		t.Fatalf("create with a writer started after the final scan = %d, want today's best-effort outcome, the bucket created:\n%s%s", code, stdout.String(), stderr.String())
	}
	if _, err := js.KeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("the bucket after the create: %v", err)
	}
}

// BEST EFFORT - INVERT WHEN THE SERVER-SIDE HOLD LEAVES NO WRITER A CONNECTION.
// check holds nothing once it returns: a writer starts after it, and the operator's restore (here
// the bucket created as `nats stream restore` creates it) goes ahead under that writer with nothing
// refusing it. The hold, kept through the restore, refuses the writer's connection. A second check
// now refuses, so the writer is real.
func TestBestEffortCheckHoldsNothingThroughARestoreUntilTheServerSideHoldLands(t *testing.T) {
	uri, js := server(t)
	ssh := localSSH(t)
	var writer *exec.Cmd
	judged := ownProcesses(func() []*exec.Cmd { return []*exec.Cmd{writer} })
	var stdout, stderr bytes.Buffer
	if code := command(context.Background(), []string{"check", "--nats", uri, "--host", "this-machine", "--ssh", ssh}, &stdout, &stderr, judged, http.DefaultClient); code != 0 {
		t.Fatalf("check before the writer starts = %d:\n%s%s", code, stdout.String(), stderr.String())
	}
	writer = startStandIn(t, "envoy-listener")
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("the restore after check, under a writer = %v, want today's best-effort outcome, nothing refusing it", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := command(context.Background(), []string{"check", "--nats", uri, "--host", "this-machine", "--ssh", ssh}, &stdout, &stderr, judged, http.DefaultClient); code != 1 ||
		!strings.Contains(stderr.String(), fmt.Sprintf("process %d envoy-listener runs", writer.Process.Pid)) {
		t.Fatalf("control: check with the writer running = %d, want refused naming it:\n%s", code, stderr.String())
	}
}

// listenerUsers is the authorization of a hub with two users: a listener's, the nkey user the hub
// renders for the in-cluster gke listener, holding exactly core.ListenerHostPermissions("gke"); and
// a watcher, which a client presenting no credential becomes and which may do anything, so the test
// reads streams and stream requests through it. It returns the authorization and the listener
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
		t.Fatalf("render the hub's users: %v", err)
	}
	return config, seed
}

// listenerUserServer starts a NATS server with listenerUsers' users, and returns its URL and the
// listener user's seed.
func listenerUserServer(t *testing.T) (uri, seed string) {
	t.Helper()
	config, seed := listenerUsers(t)
	_, uri = testnats.StartConfig(t, config, []string{"-js"}, true)
	return uri, seed
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

// A --nats URL may carry its credential in its userinfo, a token or a user and password, which
// nats.go authenticates with, and bus.Connect takes it without a scheme too. No line the command
// writes carries that credential: neither the break-glass line, which names the hub, nor the
// connect failure of either path. At 0668a6db6e the break-glass line named the scheme-less URL
// whole, token included, since it parses as the scheme "user" with no host, and both connect
// failures printed --nats as it was given.
func TestNoLineTheCommandWritesCarriesTheURLsCredential(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	// No nkey seed, from either variable: the run connects with the URL's userinfo alone. t.Setenv
	// puts each back when the test ends.
	for _, name := range []string{"NATS_NKEY_SEED", "NATS_NKEY_SEED_FILE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	const secret = "s3cret-token"
	uri, js := server(t)
	hub := strings.TrimPrefix(uri, "nats://")
	code, stdout, stderr := runCommand(t, &world{}, nil, "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", "user:"+secret+"@"+hub)
	if code != 0 || !strings.Contains(stderr, " on nats://"+hub+",") {
		t.Fatalf("create --break-glass over a scheme-less URL with a credential = %d, want 0 and a line naming the hub nats://%s\n%s%s", code, hub, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, secret) {
		t.Fatalf("create --break-glass wrote the URL's credential:\n%s%s", stdout, stderr)
	}
	if _, err := js.StreamInfo("KV_" + bus.SessionBucket); err != nil {
		t.Fatalf("control: the break-glass run made no bucket, so its line was never written: %v", err)
	}
	for _, args := range [][]string{
		{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
		{"create", "--host", "box-1"},
	} {
		code, stdout, stderr := runCommand(t, &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}, nil, append(args, "--nats", "user:"+secret+"@127.0.0.1:1")...)
		if code != 1 || !strings.Contains(stderr, "connect to nats://127.0.0.1:1:") {
			t.Fatalf("%q to a closed port = %d, want 1 naming the server nats://127.0.0.1:1 it could not reach\n%s%s", args, code, stdout, stderr)
		}
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("%q wrote the URL's credential in its connect failure:\n%s%s", args, stdout, stderr)
		}
	}
}

// A word the command cannot read as its own is never echoed, since an nkey seed may have been
// pasted there: a stray argument is named by its position, a value its flag cannot parse by the
// flag, an unknown or malformed flag not at all, and a flag value that holds a seed by the flag.
// Each is a usage error, 2, before any proof. The seed is a real one (testnats.User), so the
// seeded-value rows read it as bus.HoldsSeed does. At d92d4615cf the stray argument and every
// parse error printed the word, and a seeded --host or --bucket went on (a --bucket refusal quoting
// it, a --host into the proof).
func TestNoWordTheCommandCannotReadIsEchoed(t *testing.T) {
	seed, _ := testnats.User(t)
	const nowhere = "nats://127.0.0.1:1"
	for _, c := range []struct {
		name string
		args []string
		says string
	}{
		{"a stray argument", []string{"create", "--nats", nowhere, "--host", "box-1", seed}, `word 6 after "buckets" is no flag and no flag's value`},
		{"a stray argument after --", []string{"check", "--host", "box-1", "--", seed}, `word 5 after "buckets" is no flag and no flag's value`},
		{"a malformed number", []string{"create", "--nats", nowhere, "--host", "box-1", "--replicas", seed}, "--replicas: a malformed value"},
		{"a malformed number after =", []string{"create", "--nats", nowhere, "--host", "box-1", "--replicas=" + seed}, "--replicas: a malformed value"},
		{"a malformed boolean", []string{"create", "--nats", nowhere, "--host", "box-1", "--this-machine=" + seed}, "--this-machine: a malformed value"},
		{"an unknown flag", []string{"create", "--nats", nowhere, "--" + seed}, "a flag this command does not take, or a malformed one"},
		{"a flag of bad syntax", []string{"create", "---" + seed}, "a flag this command does not take, or a malformed one"},
		{"a seed as --host", []string{"create", "--nats", nowhere, "--host", seed}, "--host: an nkey seed, which is never an argument"},
		{"a seed inside --bucket", []string{"create", "--nats", nowhere, "--host", "box-1", "--bucket", "x-" + seed}, "--bucket: an nkey seed, which is never an argument"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
			code, stdout, stderr := runCommand(t, w, nil, c.args...)
			if strings.Contains(stdout+stderr, seed) || strings.Contains(stdout+stderr, seed[:20]) {
				t.Fatalf("%s echoed the seed (exit %d):\n%s%s", c.name, code, stdout, stderr)
			}
			if code != 2 || !strings.Contains(stderr, c.says) || len(w.calls) != 0 {
				t.Fatalf("%s = %d, calls %d, want the usage error %q before any proof\n%s", c.name, code, len(w.calls), c.says, stderr)
			}
		})
	}
}

// The replica count a bucket is created with is the deployment's, read as every program of it
// reads ENVOY_NATS_REPLICAS (bus.ParseNATSReplicas): a count outside 1 to bus.MaxReplicas, from
// --replicas or from the variable, is refused as a usage error, with 2, before the command
// connects. The URL names no server, so a run that went on would fail to connect, with 1. At
// 0668a6db6e --replicas was not checked at all and the variable was read by a parser of the
// command's own that took any count above 0, so both went on to the server.
func TestAReplicaCountOutsideOneToFiveIsAUsageError(t *testing.T) {
	const nowhere = "nats://127.0.0.1:1"
	for _, c := range []struct {
		env  string
		args []string
		says string
	}{
		{"", []string{"--replicas", "6"}, "--replicas 6"},
		{"", []string{"--replicas", "0"}, "--replicas 0"},
		{"", []string{"--replicas", "-1"}, "--replicas -1"},
		{"6", nil, bus.ReplicasEnvVar + ` "6"`},
		{"three", nil, bus.ReplicasEnvVar + ` "three"`},
		{"6", []string{"--break-glass", "--operator", "op", "--bucket", bus.SessionBucket}, bus.ReplicasEnvVar + ` "6"`},
	} {
		t.Run(fmt.Sprintf("%s=%q %v", bus.ReplicasEnvVar, c.env, c.args), func(t *testing.T) {
			t.Setenv(bus.ReplicasEnvVar, c.env)
			args := append([]string{"create", "--nats", nowhere}, c.args...)
			if !slices.Contains(c.args, "--break-glass") {
				args = append(args, "--host", "box-1")
			}
			w := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
			code, stdout, stderr := runCommand(t, w, nil, args...)
			if code != 2 || !strings.Contains(stderr, c.says) || !strings.Contains(stderr, "from 1 to 5") || len(w.calls) != 0 {
				t.Fatalf("%q = %d, want the usage error naming %s and 1 to 5, before any proof (%v)\n%s%s", args, code, c.says, w.calls, stdout, stderr)
			}
		})
	}
}

// A deployment's copied buckets get their copies here, where `envoy-infra buckets create` makes
// them, since no listener creates one (cmd/listener/stream_replicas_test.go,
// TestADefaultListenerOnASingleServerCreatesEverythingAtOneCopyAndItsNextStartWritesNothing). On a
// three-server cluster, as a clustered hub runs, a create at --replicas 3, one at
// ENVOY_NATS_REPLICAS=3 and a break-glass create at ENVOY_NATS_REPLICAS=3 each make streams of three
// copies, two of them on the other servers. The control is a create with neither setting, which
// makes one copy, so the reading tells one copy from three.
func TestCreateMakesTheCopiedBucketsAtTheDeploymentsCopies(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	cluster := testnats.StartCluster(t, 3)
	uri := cluster.Servers[0].URL()
	_, js := jetStream(t, uri)
	for _, step := range []struct {
		name     string
		replicas string // ENVOY_NATS_REPLICAS
		args     []string
		buckets  []string
		copies   int
	}{
		{"control: neither setting", "", []string{"create", "--host", "box-1", "--bucket", bus.SessionBucket}, []string{bus.SessionBucket}, 1},
		{"--replicas 3", "", []string{"create", "--host", "box-1", "--replicas", "3", "--bucket", bus.InterestBucket, "--bucket", bus.RoleClaimBucket},
			[]string{bus.InterestBucket, bus.RoleClaimBucket}, 3},
		{"ENVOY_NATS_REPLICAS=3", "3", []string{"create", "--host", "box-1", "--bucket", bus.KnownSessionBucket}, []string{bus.KnownSessionBucket}, 3},
		{"--break-glass at ENVOY_NATS_REPLICAS=3", "3", []string{"create", "--break-glass", "--operator", "op", "--bucket", bus.CIStateBucket}, []string{bus.CIStateBucket}, 3},
	} {
		t.Setenv(bus.ReplicasEnvVar, step.replicas)
		w := &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}
		if code, stdout, stderr := runCommand(t, w, nil, append(step.args, "--nats", uri)...); code != 0 {
			t.Fatalf("%s: %v = %d\n%s%s", step.name, step.args, code, stdout, stderr)
		}
		for _, bucket := range step.buckets {
			info, err := js.StreamInfo("KV_" + bucket)
			if err != nil {
				t.Fatalf("%s: read %s: %v", step.name, bucket, err)
			}
			placed := 0
			if info.Cluster != nil {
				placed = 1 + len(info.Cluster.Replicas)
			}
			if info.Config.Replicas != step.copies || placed != step.copies {
				t.Fatalf("%s: %s has %d copies configured and %d placed, want %d", step.name, bucket, info.Config.Replicas, placed, step.copies)
			}
		}
	}
}

// A --bucket named twice is one bucket: a break-glass create reads, logs and creates it once, as
// one named once. At 47e78cf03a each name was a bucket of its own: the run read the bucket twice
// before its create, and the second create, through the create-only open, found the first's bucket
// and refused the run.
func TestABucketNamedTwiceIsOneBucket(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, seed := listenerUserServer(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	watcher, _ := jetStream(t, uri)
	requests := watchStreamRequests(t, watcher)
	stream := "KV_" + bus.SessionBucket
	code, stdout, stderr := runCommand(t, &world{}, nil, "create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--bucket", bus.SessionBucket, "--nats", uri)
	if code != 0 || stdout != "created "+bus.SessionBucket+" (1 copies)\n" || strings.Count(stderr, "break-glass: operator op creates") != 1 {
		t.Fatalf("create --break-glass with %s named twice = %d, want it created and logged once\n%s%s", bus.SessionBucket, code, stdout, stderr)
	}
	// The run's read, the create's own lookup, the create, the lookup after it and the handle open,
	// once each.
	if got, want := requests.take(t), []string{"INFO." + stream, "INFO." + stream, "CREATE." + stream, "INFO." + stream, "INFO." + stream}; !slices.Equal(got, want) {
		t.Fatalf("create --break-glass with %s named twice sent %v, want %v", bus.SessionBucket, got, want)
	}
}
