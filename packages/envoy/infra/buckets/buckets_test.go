package buckets

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logtest"
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
	// hang names the programs ("kubectl", or the ssh program) whose calls never answer until
	// their context ends, as a connected request that stops answering does.
	hang  map[string]bool
	calls []string
}

const noPods = `{"items":[]}`

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

// No inventory is a usage error, with --first-deployment as without it: an empty server is no
// proof that no writer runs (a hub on a new disk is empty while its old clients still run), so
// nothing would be proven. At 7b262c2e0a --first-deployment alone was accepted and created the
// buckets with no writer proof.
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

// Until the auth service's cutover no hub has the buckets user's key, so `create --break-glass` runs as a
// listener's NATS user: here one holding exactly the grant the hub renders for the in-cluster gke
// listener (core.ListenerHostPermissions), which receives its replies on the default _INBOX.
// prefix. It creates the missing session bucket with the stream configuration the normal path
// creates, sends the bucket's stream reads and one create and no update or delete, takes no writer
// proof (no kubectl or ssh call), prints no best-effort warning, and logs one line naming the
// operator, the hub, the bucket and break-glass.
//
// The control is the same user's create without the flag: the normal path takes its replies under
// _INBOX_envoy_buckets, whose subscription the server refuses a listener's grant ("Permissions
// Violation for Subscription to _INBOX_envoy_buckets..."), so its read of the bucket gets no answer
// ("read envoy_sessions: context deadline exceeded") and nothing is created. At 7b0c789c53 that was
// the one create a listener's user had.
func TestBreakGlassCreatesAMissingBucketAsAListenersUser(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	stream := "KV_" + bus.SessionBucket
	proven := map[string][]string{"box-1": {clean(150)}}
	// The normal path's stream, on a server of its own, before the listener user's seed is set.
	normalURI, normalJS := server(t)
	if code, stdout, stderr := runCommand(t, &world{hostAnswers: proven}, nil, "create", "--nats", normalURI, "--bucket", bus.SessionBucket, "--host", "box-1"); code != 0 {
		t.Fatalf("the normal path's create = %d\n%s%s", code, stdout, stderr)
	}
	normal, err := normalJS.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the normal path's stream: %v", err)
	}

	uri, seed := listenerUserServer(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	watcher, js := jetStream(t, uri)
	requests := watchStreamRequests(t, watcher)

	// The server's refusal reaches the connection as an async error, which the bus logs (slog).
	logs := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	code, stdout, stderr := runCommand(t, &world{hostAnswers: map[string][]string{"box-1": {clean(150)}}}, nil, "create", "--nats", uri, "--bucket", bus.SessionBucket, "--host", "box-1")
	const unanswered = "envoy-infra buckets: read " + bus.SessionBucket + ": context deadline exceeded"
	refusal := `Permissions Violation for Subscription to \"` + InboxPrefix + `.`
	if code != 1 || !strings.Contains(stderr, unanswered) || !strings.Contains(logs.String(), refusal) {
		t.Fatalf("control: create as a listener's user without --break-glass = %d, want 1, %q, and the server's %q\n%s%s\nlogs:\n%s", code, unanswered, refusal, stdout, stderr, logs.String())
	}
	t.Logf("control: create without --break-glass = %d: %q, after the server's %q", code, unanswered, refusal)
	if _, err := js.StreamInfo(stream); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("control: the create without --break-glass made %s: %v", stream, err)
	}
	requests.take(t)

	w := &world{}
	code, stdout, stderr = runCommand(t, w, nil, "create", "--break-glass", "--operator", "operator@example.com", "--bucket", bus.SessionBucket, "--nats", uri)
	if code != 0 || stdout != "created "+bus.SessionBucket+" (1 copies)\n" {
		t.Fatalf("create --break-glass as a listener's user = %d\n%s%s", code, stdout, stderr)
	}
	if len(w.calls) != 0 {
		t.Fatalf("create --break-glass took a writer proof: %v", w.calls)
	}
	if strings.Contains(stderr, bestEffortWarning) {
		t.Fatalf("create --break-glass printed the best-effort warning, which tells the operator to stop every listener:\n%s", stderr)
	}
	logged := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(logged) != 1 || !strings.Contains(logged[0], "break-glass") || !strings.Contains(logged[0], "operator operator@example.com") ||
		!strings.Contains(logged[0], " "+bus.SessionBucket+" ") || !strings.Contains(logged[0], " on "+uri+",") {
		t.Fatalf("create --break-glass logged %q, want one line naming break-glass, the operator, the bucket and the hub", stderr)
	}
	// The run's read, the create's own lookup (bus.CreateMissingCopiedBucket), the create, and the
	// handle open after it.
	if got, want := requests.take(t), []string{"INFO." + stream, "INFO." + stream, "CREATE." + stream, "INFO." + stream}; !slices.Equal(got, want) {
		t.Fatalf("create --break-glass sent %v to the bucket's stream, want %v", got, want)
	}
	created, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the stream create --break-glass made: %v", err)
	}
	if !reflect.DeepEqual(created.Config, normal.Config) {
		t.Fatalf("create --break-glass made %+v\nthe normal path makes %+v", created.Config, normal.Config)
	}
}

// A break-glass create only creates. With the session bucket there, holding a key, it refuses,
// sends the bucket's stream one read and nothing else, and leaves the stream as it was: the same
// stream, configuration, messages and key. With a missing bucket named beside it, it creates
// neither. --break-glass with recreate is refused as a usage error before it connects, and sends
// the stream nothing.
func TestBreakGlassRefusesABucketThatExistsAndChangesNothing(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, seed := listenerUserServer(t)
	watcher, js := jetStream(t, uri)
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	kv, err := js.CreateKeyValue(&config)
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	if _, err := kv.Put("session-1", []byte("held")); err != nil {
		t.Fatalf("write a key: %v", err)
	}
	stream, missing := "KV_"+bus.SessionBucket, "KV_"+bus.RoleClaimBucket
	before, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the bucket: %v", err)
	}
	t.Setenv("NATS_NKEY_SEED", seed)
	requests := watchStreamRequests(t, watcher)

	for _, c := range []struct {
		name string
		args []string
		code int
		says string
		sent []string
	}{
		{"create of the bucket that exists", []string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
			1, "refused: " + stream + " exists", []string{"INFO." + stream}},
		{"create of a missing bucket beside it", []string{"create", "--break-glass", "--operator", "op", "--bucket", bus.RoleClaimBucket, "--bucket", bus.SessionBucket},
			1, "refused: " + stream + " exists", []string{"INFO." + missing, "INFO." + stream}},
		{"recreate", []string{"recreate", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
			2, "--break-glass is create's alone", nil},
		{"--break-glass before recreate", []string{"--break-glass", "recreate", "--operator", "op", "--bucket", bus.SessionBucket},
			2, "usage: envoy-infra buckets", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			requests.take(t) // the test's own reads of the streams, after the run before
			w := &world{}
			code, stdout, stderr := runCommand(t, w, nil, append(c.args, "--nats", uri)...)
			if code != c.code || !strings.Contains(stderr, c.says) || strings.Contains(stdout, "created") || len(w.calls) != 0 {
				t.Fatalf("%v = %d, want %d saying %q, nothing created and no proof taken (%v)\n%s%s", c.args, code, c.code, c.says, w.calls, stdout, stderr)
			}
			if got := requests.take(t); !slices.Equal(got, c.sent) {
				t.Fatalf("%v sent %v to the buckets' streams, want %v", c.args, got, c.sent)
			}
			after, err := js.StreamInfo(stream)
			if err != nil || !after.Created.Equal(before.Created) || !reflect.DeepEqual(after.Config, before.Config) ||
				after.State.Msgs != before.State.Msgs || after.State.LastSeq != before.State.LastSeq {
				t.Fatalf("%s after the refused run: %v, %+v; want it as it was, %+v", stream, err, after, before)
			}
			if entry, err := kv.Get("session-1"); err != nil || string(entry.Value()) != "held" {
				t.Fatalf("the key after the refused run: %v, %v", entry, err)
			}
			if _, err := js.StreamInfo(missing); !errors.Is(err, natsgo.ErrStreamNotFound) {
				t.Fatalf("the refused run made %s: %v", missing, err)
			}
		})
	}
}

// --break-glass is refused as a usage error, before anything connects, where it is misused: on
// recreate or check, beside a proof flag it would leave unchecked, without --operator or with one
// that spans lines, and without a --bucket; and --operator is refused without it. The URL names no
// server, so a run that went on would fail to connect, with 1.
func TestBreakGlassIsRefusedWhereItIsMisused(t *testing.T) {
	const nowhere = "nats://127.0.0.1:1"
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"recreate", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket}, "--break-glass is create's alone"},
		{[]string{"check", "--break-glass", "--operator", "op"}, "--break-glass is create's alone"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--host", "box-1"}, "so --host would check nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--kube-context", "prod", "--connz", "http://hub:8222"}, "so --connz and --kube-context would check nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--first-deployment"}, "so --first-deployment would check nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--ssh", "ssm-ssh"}, "so --ssh would check nothing"},
		{[]string{"create", "--break-glass", "--bucket", bus.SessionBucket}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", " ", "--bucket", bus.SessionBucket}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", "op\nenvoy-infra buckets: forged", "--bucket", bus.SessionBucket}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", "op"}, "--break-glass needs each missing --bucket"},
		{[]string{"create", "--operator", "op", "--bucket", bus.SessionBucket, "--host", "box-1"}, "--operator names who runs a --break-glass create"},
	} {
		w := &world{}
		code, stdout, stderr := runCommand(t, w, nil, append(c.args, "--nats", nowhere)...)
		if code != 2 || !strings.Contains(stderr, c.says) || len(w.calls) != 0 {
			t.Errorf("%q = %d, want the usage error %q and no proof taken (%v)\n%s%s", c.args, code, c.says, w.calls, stdout, stderr)
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

// holdingRelay relays a break-glass run's connections to a hub, the run's side a protocol message
// at a time, and holds the run between two of its requests: once it has passed on the run's first
// publish on trigger, it reads nothing more from the run until the hold ends. That publish reaches
// the hub, which answers it, and nothing the run sends after it reaches the hub before the hold
// ends, however the scheduler orders the run and the racer.
type holdingRelay struct {
	trigger  string
	url      string
	listener net.Listener
	once     sync.Once
	reply    string        // the held publish's reply subject, set before held is closed
	held     chan struct{} // closed once the publish on trigger has been passed on
	release  chan struct{} // closed by end
	end      func()        // ends the hold; the test's end ends it too
}

func startHoldingRelay(t *testing.T, target, trigger string) *holdingRelay {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the hub's URL %q: %v", target, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the holding relay: %v", err)
	}
	r := &holdingRelay{trigger: trigger, url: "nats://" + listener.Addr().String(), listener: listener,
		held: make(chan struct{}), release: make(chan struct{})}
	r.end = sync.OnceFunc(func() { close(r.release) })
	t.Cleanup(func() {
		r.end()
		_ = listener.Close()
	})
	go r.serve(server.Host)
	return r
}

// serve joins each connection the relay accepts to the hub until the listener closes.
func (r *holdingRelay) serve(target string) {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", target)
		if err != nil {
			_ = client.Close()
			continue
		}
		go func() {
			_, _ = io.Copy(client, server)
			_ = client.Close()
			_ = server.Close()
		}()
		go r.pass(client, server)
	}
}

// pass relays what the client sends to the hub, a protocol message at a time, and holds after the
// first publish on trigger. A client sends CONNECT, SUB, UNSUB, PING, PONG and publishes, and a
// publish (`PUB <subject> [reply] <bytes>`, `HPUB <subject> [reply] <header bytes> <bytes>`) is
// followed by that many bytes and a CRLF.
func (r *holdingRelay) pass(client, server net.Conn) {
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if _, err := io.WriteString(server, line); err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		publish, headed := strings.EqualFold(fields[0], "PUB"), strings.EqualFold(fields[0], "HPUB")
		if !publish && !headed {
			continue
		}
		size, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return
		}
		if _, err := io.CopyN(server, reader, int64(size)+2); err != nil {
			return
		}
		if fields[1] != r.trigger {
			continue
		}
		reply := ""
		if (publish && len(fields) == 4) || (headed && len(fields) == 5) {
			reply = fields[2]
		}
		r.once.Do(func() {
			r.reply = reply
			close(r.held)
			<-r.release
		})
	}
}

// racerInbox is the reply prefix of a racer's own requests, which the request watch leaves out.
const racerInbox = "_RACER"

// racer is another client of the hub, the watcher (listenerUsers) on the hub directly, that makes a
// bucket while a holdingRelay holds a break-glass run.
type racer struct {
	answer string             // the hub's answer to the run's held request
	made   *natsgo.StreamInfo // the bucket's stream once the racer made it
	err    error
	done   chan struct{}
}

// race starts a racer on the hub at uri: once relay holds the run, it waits for the hub's answer to
// the held request, makes the bucket whose stream is stream with makeBucket, waits until the hub
// answers for that stream, and ends the hold.
func race(t *testing.T, uri string, relay *holdingRelay, stream string, makeBucket func(natsgo.JetStreamContext) error) *racer {
	t.Helper()
	conn, err := testnats.ConnectSeed(t, uri, "", natsgo.CustomInboxPrefix(racerInbox))
	if err != nil {
		t.Fatalf("connect the racer: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("the racer's jetstream: %v", err)
	}
	replies, err := conn.SubscribeSync(natsgo.InboxPrefix + ">")
	if err != nil {
		t.Fatalf("the racer's watch of the run's answers: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the racer's watch: %v", err)
	}
	r := &racer{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		defer relay.end()
		select {
		case <-relay.held:
		case <-time.After(30 * time.Second):
			r.err = fmt.Errorf("the run sent nothing on %s within 30 s", relay.trigger)
			return
		}
		if r.answer, r.err = answerTo(replies, relay.reply); r.err != nil {
			return
		}
		if r.err = makeBucket(js); r.err != nil {
			return
		}
		r.made, r.err = awaitStream(js, stream)
	}()
	return r
}

// answerTo is the payload of the first message on replies whose subject is reply.
func answerTo(replies *natsgo.Subscription, reply string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := replies.NextMsg(time.Until(deadline))
		if err != nil {
			return "", fmt.Errorf("no answer to %s: %w", reply, err)
		}
		if msg.Subject == reply {
			return string(msg.Data), nil
		}
	}
}

// awaitStream reads stream until the hub answers for it, or 10 s pass.
func awaitStream(js natsgo.JetStreamContext, stream string) (*natsgo.StreamInfo, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := js.StreamInfo(stream)
		if err == nil || time.Now().After(deadline) {
			return info, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A bucket another client makes after a break-glass run's read is left as it is, and the run is
// refused for it, rather than raising, updating or deleting it. The other client is a racer (race):
// the run reaches a three-member hub through a relay that holds it after its first request on the
// trigger (holdingRelay), and in that hold the racer makes the bucket, then lets the run go on.
//
//   - At the run's read of the session bucket, which the hub answers "stream not found", the racer
//     makes the bucket at one copy where the run asks for three. The create's own lookup finds it,
//     and the run is refused there, having sent the bucket's stream its two reads alone. The control
//     is the open the normal path takes (bus.CreateCopiedBucket), which raises a bucket its lookup
//     finds with a stream update.
//   - Between the create's lookup and its create - at the account info nats.go asks for before
//     every create - the racer makes the role-claim bucket with the discard policy and direct get
//     of a KV made before nats-server 2.7.2 and 2.9. The hub answers the create "stream name
//     already in use"; nats.go reads the stream and sends no update, since the hub's metadata keeps
//     the two configurations apart (bus, createOnly); and the run is refused.
func TestBreakGlassLeavesABucketMadeAfterItsReadAsItIs(t *testing.T) {
	config, seed := listenerUsers(t)
	cluster := testnats.StartClusterConfig(t, 3, config)
	uri := cluster.Servers[0].URL()
	t.Setenv("NATS_NKEY_SEED", seed)
	t.Setenv(bus.ReplicasEnvVar, "3")
	watcher, js := jetStream(t, uri)
	requests := watchStreamRequests(t, watcher)
	for _, c := range []struct {
		name, bucket, trigger string
		makeBucket            func(natsgo.JetStreamContext) error
		answered              string // what the hub answered the run's request on trigger
		says                  []string
		sent                  []string // the run's requests of the bucket's stream, by verb
	}{
		{
			name:    "at fewer copies when the run's read arrives",
			bucket:  bus.SessionBucket,
			trigger: "$JS.API.STREAM.INFO.KV_" + bus.SessionBucket,
			makeBucket: func(js natsgo.JetStreamContext) error {
				config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
				_, err := js.CreateKeyValue(&config)
				return err
			},
			answered: "stream not found",
			says:     []string{"KV_" + bus.SessionBucket + " STREAM.INFO"},
			sent:     []string{"INFO", "INFO"},
		},
		{
			name:    "with another discard policy and direct get between the create's lookup and its create",
			bucket:  bus.RoleClaimBucket,
			trigger: "$JS.API.INFO",
			makeBucket: func(js natsgo.JetStreamContext) error {
				config, _ := bus.CopiedBucketConfig(bus.RoleClaimBucket, 3)
				if _, err := js.CreateKeyValue(&config); err != nil {
					return err
				}
				info, err := js.StreamInfo("KV_" + bus.RoleClaimBucket)
				if err != nil {
					return err
				}
				legacy := info.Config
				legacy.Discard, legacy.AllowDirect = natsgo.DiscardOld, false
				_, err = js.UpdateStream(&legacy)
				return err
			},
			says: []string{"KV_" + bus.RoleClaimBucket + " create", "stream name already in use"},
			sent: []string{"INFO", "INFO", "CREATE", "INFO"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			stream := "KV_" + c.bucket
			requests.take(t)
			relay := startHoldingRelay(t, uri, c.trigger)
			r := race(t, uri, relay, stream, c.makeBucket)
			code, stdout, stderr := runCommand(t, &world{}, nil, "create", "--break-glass", "--operator", "op", "--bucket", c.bucket, "--nats", relay.url)
			<-r.done
			if r.err != nil || !strings.Contains(r.answer, c.answered) {
				t.Fatalf("the racer: %v; the hub answered the run's request %q, want %q", r.err, r.answer, c.answered)
			}
			// The run's requests are taken before the read below, which is the test's own.
			var sent []string
			for _, request := range requests.take(t) {
				verb, _ := strings.CutSuffix(request, "."+stream)
				if verb != "INFO" && verb != "CREATE" {
					t.Errorf("the run sent %s, which changes a stream", request)
				}
				sent = append(sent, verb)
			}
			if after, err := js.StreamInfo(stream); err != nil || !after.Created.Equal(r.made.Created) || !reflect.DeepEqual(after.Config, r.made.Config) {
				t.Errorf("%s after the run: %v, %+v; want it as the racer made it, %+v", stream, err, after, r.made)
			}
			if !slices.Equal(sent, c.sent) {
				t.Errorf("the run sent %v to %s, want %v", sent, stream, c.sent)
			}
			refused := "refused: " + stream + " was made by another client after the read"
			if code != 1 || !strings.Contains(stderr, refused) || strings.Contains(stdout, "created") {
				t.Errorf("the run = %d, want 1 and %q\n%s%s", code, refused, stdout, stderr)
			}
			for _, says := range c.says {
				if !strings.Contains(stderr, says) {
					t.Errorf("the refusal does not name %q:\n%s", says, stderr)
				}
			}
		})
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
	// The run's read, the create's own lookup, the create and the handle open, once each.
	if got, want := requests.take(t), []string{"INFO." + stream, "INFO." + stream, "CREATE." + stream, "INFO." + stream}; !slices.Equal(got, want) {
		t.Fatalf("create --break-glass with %s named twice sent %v, want %v", bus.SessionBucket, got, want)
	}
}
