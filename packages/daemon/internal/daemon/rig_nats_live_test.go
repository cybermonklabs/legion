//go:build e2e

// The rig's nats-server (RIG.md): the real binary scripts/e2e/lib/live-daemon-proof.sh installs,
// with JetStream, its monitoring port on for /connz, and the grants production renders, so a
// session, the listener and the daemon each reach NATS as production's issue pods, its GKE
// listener and its daemons do:
//
//   - a client that presents no credential is the server's no_auth_user, `members`, holding
//     core.SessionPermissions: the issue pods' sessions present none;
//   - the listener connects as a user of its own holding core.ListenerHostPermissions, the set the
//     GKE listener's user carries (the deployment repository's envoy-cluster/Pulumi.prod.yaml);
//   - the daemon connects as a user of its own holding daemonpermissions.Grant, read at this head,
//     since the daemon the rig runs is this head's (natsauth.DaemonSeed, `nats_daemon_nkey_seed_file`);
//   - Dispatch connects as a user of its own holding core.DispatchPermissions;
//   - the rig's own reads (a stored notice, a consumer) go through a user of the harness's that
//     holds every subject, which no process under test is handed.
//
// The three sets of core are read at the listener release production pins, out of the tree of
// testenvoy.PinnedRevision, by scripts/e2e/lib/live-rig-grants.go, which lib/live-rig.sh runs there and
// whose answer it names in LEGION_E2E_RIG_GRANTS: core is a module of its own, which no package of
// this one imports, and the grants that matter are that release's, not this head's.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/daemonpermissions"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testenvoy"
	"github.com/sjawhar/legion/daemon/internal/testnkeys"
)

// rigGrantsEnv names the JSON file scripts/e2e/lib/live-rig-grants.go wrote (rigGrants), and
// rigNATSBinEnv the nats-server binary lib/live-rig.sh installed, which live-daemon-proof.sh's own run
// names the same way.
const (
	rigGrantsEnv  = "LEGION_E2E_RIG_GRANTS"
	rigNATSBinEnv = "LEGION_E2E_NATS_BIN"
)

// rigGrants is live-rig-grants.go's answer: the machine the listener's set is scoped to, and the three
// sets core renders for production's users.
type rigGrants struct {
	Machine  string    `json:"machine"`
	Listener natsGrant `json:"listener"`
	Sessions natsGrant `json:"sessions"`
	Dispatch natsGrant `json:"dispatch"`
	// Revision is the tree it was read from, which lib/live-rig.sh adds beside the printer's answer.
	Revision string `json:"revision"`
}

// natsGrant is one user's permissions as nats-server's configuration spells them.
type natsGrant struct {
	Publish   natsRules `json:"publish"`
	Subscribe natsRules `json:"subscribe"`
}

type natsRules struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
}

// readRigGrants reads the grants file the script names, refusing one that names no set: an empty
// allow list is read by nats-server as no restriction at all, which is not production's grant.
func readRigGrants(t *testing.T, path string) rigGrants {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the rig's grants %s: %v", path, err)
	}
	var grants rigGrants
	if err := json.Unmarshal(body, &grants); err != nil {
		t.Fatalf("decode the rig's grants %s: %v", path, err)
	}
	for name, grant := range map[string]natsGrant{"listener": grants.Listener, "sessions": grants.Sessions, "dispatch": grants.Dispatch} {
		if len(grant.Publish.Allow) == 0 || len(grant.Subscribe.Allow) == 0 {
			t.Fatalf("the rig's grants %s give the %s user no publish or no subscribe entry; nats-server reads an empty allow list as everything", path, name)
		}
	}
	if grants.Machine == "" || grants.Revision == "" {
		t.Fatalf("the rig's grants %s name no machine or no revision: %s", path, body)
	}
	return grants
}

// daemonGrant is the daemon's own set, at this head.
func daemonGrant() natsGrant {
	grant := daemonpermissions.Grant()
	return natsGrant{Publish: natsRules{Allow: grant.Publish}, Subscribe: natsRules{Allow: grant.Subscribe}}
}

// everything is the harness's own set.
var everything = natsGrant{Publish: natsRules{Allow: []string{">"}}, Subscribe: natsRules{Allow: []string{">"}}}

// rigNATS is the rig's running nats-server: its client and monitoring ports, kept across a
// restart (stop, start), its store, the credential files of the users a process under test
// connects as, and the harness's own connection.
type rigNATS struct {
	t      *testing.T
	binary string
	dir    string
	grants rigGrants

	port, monitorPort int
	// url is what a session and the daemon dial; dispatchURL is the same server with Dispatch's
	// user and password in it, since the Dispatch a test runs (scratchDispatch) takes no seed of
	// its own (its environment drops every NATS_ variable).
	url, dispatchURL string

	listenerSeedFile, daemonSeedFile string
	adminSeed                        string
	dispatchPassword                 string
	sessionPublic                    string
	listenerPublic, daemonPublic     string
	adminPublic                      string

	log    string
	cmd    *exec.Cmd
	exited chan struct{}

	admin *nats.Conn
	js    jetstream.JetStream
}

// startRigNATS writes the server's configuration and starts it. The client and monitoring ports
// are reserved for the whole run (testenvoy.ReservePort) and named in the configuration, so every
// later start binds the same ones, which every process under test was told: a port the server
// chose and freed at its stop was any program's while it was down, and on 2026-11-18 an outbound
// connection took the client port during w4's NATS outage, so the server's restart failed with
// `address already in use`.
func startRigNATS(t *testing.T, binary string, grants rigGrants, logDir string) *rigNATS {
	t.Helper()
	n := &rigNATS{t: t, binary: binary, dir: t.TempDir(), grants: grants, log: filepath.Join(logDir, "nats.log"),
		port: testenvoy.ReservePort(t), monitorPort: testenvoy.ReservePort(t)}
	var listenerSeed, daemonSeed string
	listenerSeed, n.listenerPublic = testnkeys.User(t)
	daemonSeed, n.daemonPublic = testnkeys.User(t)
	n.adminSeed, n.adminPublic = testnkeys.User(t)
	_, n.sessionPublic = testnkeys.User(t)
	n.dispatchPassword = strings.ToLower(randomSuffix(t)) + strings.ToLower(randomSuffix(t))
	n.listenerSeedFile = filepath.Join(n.dir, "listener.seed")
	n.daemonSeedFile = filepath.Join(n.dir, "daemon.seed")
	writeProofFile(t, n.listenerSeedFile, listenerSeed+"\n")
	writeProofFile(t, n.daemonSeedFile, daemonSeed+"\n")
	if err := os.MkdirAll(filepath.Join(n.dir, "store"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.stop)
	n.start()
	return n
}

// configuration is the server's file: listen and monitor addresses, JetStream, and the users.
func (n *rigNATS) configuration() string {
	user := func(comment, key string, grant natsGrant) string {
		body, err := marshalNoEscape(map[string]any{"nkey": key, "permissions": grant})
		if err != nil {
			n.t.Fatalf("encode the %s user: %v", comment, err)
		}
		return "    # " + comment + "\n    " + body + "\n"
	}
	dispatch, err := marshalNoEscape(map[string]any{"user": "dispatch", "password": n.dispatchPassword, "permissions": n.grants.Dispatch})
	if err != nil {
		n.t.Fatalf("encode the Dispatch user: %v", err)
	}
	var b strings.Builder
	b.WriteString("# Written by packages/daemon/internal/daemon/rig_nats_live_test.go.\n")
	fmt.Fprintf(&b, "server_name: legion-rig\nlisten: 127.0.0.1:%d\nhttp: 127.0.0.1:%d\n", n.port, n.monitorPort)
	fmt.Fprintf(&b, "jetstream {\n  store_dir: %q\n}\n", filepath.Join(n.dir, "store"))
	b.WriteString("authorization {\n  users = [\n")
	b.WriteString(user("the listener's own user: core.ListenerHostPermissions("+n.grants.Machine+") at "+n.grants.Revision, n.listenerPublic, n.grants.Listener))
	b.WriteString(user("the daemon's own user: daemonpermissions.Grant at this head", n.daemonPublic, daemonGrant()))
	b.WriteString("    # Dispatch's user: core.DispatchPermissions(" + n.grants.Machine + ") at " + n.grants.Revision + "\n    " + dispatch + "\n")
	b.WriteString(user("the rig's own reads, which no process under test is handed", n.adminPublic, everything))
	b.WriteString(user("members: core.SessionPermissions at "+n.grants.Revision+", the no_auth_user every session that presents no credential is", n.sessionPublic, n.grants.Sessions))
	b.WriteString("  ]\n}\n")
	fmt.Fprintf(&b, "no_auth_user: %q\n", n.sessionPublic)
	return b.String()
}

// marshalNoEscape is JSON that nats-server reads: Go's HTML escaping of '>' as \u003e is not.
func marshalNoEscape(value any) (string, error) {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// start runs the server and returns once it says it is ready, and once the harness has
// connected to it as its own user.
func (n *rigNATS) start() {
	t := n.t
	t.Helper()
	config := filepath.Join(n.dir, "nats-server.conf")
	writeProofFile(t, config, n.configuration())
	logFile, err := os.OpenFile(n.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the nats-server's log %s: %v", n.log, err)
	}
	from, err := logFile.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(n.binary, "-c", config)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start %s: %v", n.binary, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(exited)
	}()
	n.cmd, n.exited = cmd, exited
	deadline := time.Now().Add(60 * time.Second)
	for {
		text := readFrom(t, n.log, from)
		if strings.Contains(text, "Server is ready") {
			break
		}
		select {
		case <-exited:
			t.Fatalf("the nats-server exited at startup; its log %s:\n%s", n.log, text)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the nats-server was not ready within 60s; its log %s:\n%s", n.log, text)
		}
		time.Sleep(100 * time.Millisecond)
	}
	n.url = "nats://127.0.0.1:" + strconv.Itoa(n.port)
	n.dispatchURL = "nats://dispatch:" + url.QueryEscape(n.dispatchPassword) + "@127.0.0.1:" + strconv.Itoa(n.port)
	if n.admin == nil {
		admin, err := natsauth.Connect([]string{n.url}, n.adminSeed, "legion-rig harness",
			nats.Timeout(10*time.Second), natsauth.ReconnectForever())
		if err != nil {
			t.Fatalf("connect the rig's harness user: %v", err)
		}
		t.Cleanup(admin.Close)
		js, err := jetstream.New(admin)
		if err != nil {
			t.Fatalf("open JetStream as the harness: %v", err)
		}
		n.admin, n.js = admin, js
	}
}

// stop ends the server and waits for it; one that has stopped is left as it is. A check that
// drops every session's NATS connection (w4) stops it and starts it again.
func (n *rigNATS) stop() {
	if n.cmd == nil {
		return
	}
	select {
	case <-n.exited:
		return
	default:
	}
	_ = n.cmd.Process.Signal(os.Interrupt)
	select {
	case <-n.exited:
	case <-time.After(15 * time.Second):
		_ = n.cmd.Process.Kill()
		<-n.exited
	}
}

// natsConnection is one client connection /connz lists: the server's id, the name its client
// carries, the user it authenticated as (the no_auth_user's key for a session), and when it
// opened.
type rigConnz struct {
	CID   uint64 `json:"cid"`
	Name  string `json:"name"`
	User  string `json:"authorized_user"`
	Start string `json:"start"`
	IP    string `json:"ip"`
	Port  int    `json:"port"`
}

// connections is /connz with each connection's user, which a check reads to say a session kept
// its connection (w4) and to show which user each process connected as.
func (n *rigNATS) connections() []rigConnz {
	n.t.Helper()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/connz?auth=true&limit=1024", n.monitorPort))
	if err != nil {
		n.t.Fatalf("read /connz: %v", err)
	}
	defer response.Body.Close()
	var connz struct {
		Connections []rigConnz `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		n.t.Fatalf("decode /connz: %v", err)
	}
	return connz.Connections
}

// userOf names the rig's user a /connz row authenticated as, in the words this file gives them.
func (n *rigNATS) userOf(c rigConnz) string {
	switch c.User {
	case n.listenerPublic:
		return "listener"
	case n.daemonPublic:
		return "daemon"
	case n.adminPublic:
		return "harness"
	case n.sessionPublic:
		return "members"
	case "dispatch":
		return "dispatch"
	}
	return "unknown(" + c.User + ")"
}

// storedOn is every message the stream holds on subject, oldest first, with the moment the
// stream stored each: the harness's own ordered consumer, which reads and leaves nothing behind.
func (n *rigNATS) storedOn(subject string) []storedMessage {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := n.js.Stream(ctx, "ENVOY_NOTIFICATIONS")
	if err != nil {
		n.t.Fatalf("open the stream: %v", err)
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(subject))
	if err != nil {
		n.t.Fatalf("read the stream: %v", err)
	}
	// A wildcard filter answers each subject it matched, so the stored count is their sum.
	var want uint64
	for _, count := range info.State.Subjects {
		want += count
	}
	if want == 0 {
		return nil
	}
	consumer, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subject}, DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		n.t.Fatalf("read %s: %v", subject, err)
	}
	var stored []storedMessage
	for uint64(len(stored)) < want {
		batch, err := consumer.Fetch(int(want)-len(stored), jetstream.FetchMaxWait(10*time.Second))
		if err != nil {
			n.t.Fatalf("fetch %s: %v", subject, err)
		}
		got := 0
		for msg := range batch.Messages() {
			got++
			meta, err := msg.Metadata()
			if err != nil {
				n.t.Fatalf("read a stored message's metadata: %v", err)
			}
			stored = append(stored, storedMessage{at: meta.Timestamp, seq: meta.Sequence.Stream, data: append([]byte(nil), msg.Data()...)})
		}
		if err := batch.Error(); err != nil && got == 0 {
			n.t.Fatalf("fetch %s: %v", subject, err)
		}
	}
	return stored
}

// storedMessage is one message the stream holds.
type storedMessage struct {
	at   time.Time
	seq  uint64
	data []byte
}

// readFrom is path's contents after offset from.
func readFrom(t *testing.T, path string, from int64) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer file.Close()
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		b.WriteString(scanner.Text())
		b.WriteByte('\n')
	}
	return b.String()
}
