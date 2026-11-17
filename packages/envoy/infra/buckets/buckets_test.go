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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// world is what the fake kubectl and ssh answer: the pods the API lists, each node's runtime list
// (or the error reading it), and each host query's output. hostAnswers is consumed in order, so a
// test can make the second proof differ from the first.
type world struct {
	pods        string
	nodes       string
	running     map[string]string
	runningErr  map[string]error
	hostAnswers map[string][]string
	hostErr     map[string]error
	calls       []string
}

const noPods = `{"items":[]}`

func clean(processes int) string {
	return fmt.Sprintf("processes=%d\n%s\n", processes, hostQueryDone)
}

func (w *world) run(_ context.Context, name string, args ...string) ([]byte, error) {
	w.calls = append(w.calls, name+" "+strings.Join(args, " "))
	if name == "kubectl" {
		if len(args) > 2 && args[0] == "--context" {
			args = args[2:]
		}
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
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
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
	for _, call := range []string{"kubectl --context prod get --raw /api/v1/nodes/node-a/proxy/runningpods/", "kubectl --context prod get --raw /api/v1/nodes/node-b/proxy/runningpods/"} {
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
// copied bucket with its configuration; a server a listener has connected to holds the stream, and
// --first-deployment refuses it.
func TestFirstDeploymentCreatesEveryBucketOnlyOnAServerNoWriterReached(t *testing.T) {
	uri, js := server(t)
	code, stdout, stderr := runCommand(t, &world{}, nil, "create", "--nats", uri, "--first-deployment", "--replicas", "1")
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
	code, stdout, stderr = runCommand(t, &world{}, nil, "create", "--nats", uri, "--first-deployment")
	if code != 1 || !strings.Contains(stderr, "the server holds "+bus.Stream) {
		t.Fatalf("create --first-deployment on a server a writer reached = %d\n%s%s", code, stdout, stderr)
	}
	if _, err := js.KeyValue(bus.SessionBucket); !errors.Is(err, natsgo.ErrBucketNotFound) {
		t.Fatalf("the refused create made the bucket: %v", err)
	}
}

// No inventory and no --first-deployment is a usage error: nothing would be proven.
func TestTheCommandNeedsAnInventory(t *testing.T) {
	code, _, stderr := runCommand(t, &world{}, nil, "create", "--nats", "nats://127.0.0.1:1")
	if code != 2 || !strings.Contains(stderr, "name every place a writer of the deployment runs") {
		t.Fatalf("create with no inventory = %d: %s", code, stderr)
	}
	code, _, stderr = runCommand(t, &world{}, nil, "recreate", "--nats", "nats://127.0.0.1:1", "--host", "box-1", "--bucket", "legion_agents")
	if code != 2 || !strings.Contains(stderr, "legion_agents is no bucket a listener copies") {
		t.Fatalf("recreate of a bucket no listener copies = %d: %s", code, stderr)
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
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create a user: %v", err)
	}
	seed, _ := user.Seed()
	public, _ := user.PublicKey()
	_, uri := testnats.StartConfig(t, testnats.SubscribeGrantConfig(public, InboxPrefix+".>"), []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", string(seed))

	control, err := testnats.ConnectSeed(t, uri, string(seed))
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
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("no sleep binary to run as a stand-in writer: %v", err)
	}
	raw, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatalf("read sleep: %v", err)
	}
	ssh := filepath.Join(t.TempDir(), "local-ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nfor last; do :; done\nexec sh -c \"$last\"\n"), 0o755); err != nil {
		t.Fatalf("write the local ssh: %v", err)
	}
	uri, _ := server(t)
	for _, name := range []string{"envoy-listener", "listener", "envoy-listener-heldport", "envoy-dispatch", "dispatch", "dispatch-e2e"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(binary, raw, 0o755); err != nil {
				t.Fatalf("write the stand-in: %v", err)
			}
			process := exec.Command(binary, "300")
			if err := process.Start(); err != nil {
				t.Fatalf("start the stand-in: %v", err)
			}
			t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
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

var _ = io.Discard
