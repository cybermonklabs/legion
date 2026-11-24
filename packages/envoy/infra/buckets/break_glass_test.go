package buckets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

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
	// The normal path's stream, on a server of its own, before the listener user's seed is set.
	normalURI, normalJS := server(t)
	if code, stdout, stderr := runCommand(t, &world{}, nil, "create", "--nats", normalURI, "--bucket", bus.SessionBucket); code != 0 {
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
	code, stdout, stderr := runCommand(t, &world{}, nil, "create", "--nats", uri, "--bucket", bus.SessionBucket)
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
	resends := recordResends(t)
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
	// The run's read, the create's own lookup (bus.CreateMissingCopiedBucket), the create, the
	// lookup after it, which waits for the member to apply the create, and the handle open, each
	// counted once however often the bus sent it (distinctSends).
	if got, want := distinctSends(t, requests.take(t), resends), []string{"INFO." + stream, "INFO." + stream, "CREATE." + stream, "INFO." + stream, "INFO." + stream}; !slices.Equal(got, want) {
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

// A break-glass create changes no bucket that is there. With the session bucket there at one copy,
// holding a key, a create at one copy says it is present and exits 0; a create at three copies,
// with a missing bucket named beside it, is refused naming the copies and creates neither. Each
// sends the buckets' streams one read each and nothing else, and leaves the stream as it was: the
// same stream, configuration, messages and key. --break-glass with recreate is refused as a usage
// error before it connects, and sends the stream nothing. At b20f45e30a (#4296's head) the present
// bucket refused the run, exit 1 ("refused: KV_envoy_sessions exists").
func TestBreakGlassLeavesABucketThatIsThereAsItIs(t *testing.T) {
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
		{"create of the bucket there at its copies", []string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
			0, bus.SessionBucket + " present at 1 copies with its own configuration", []string{"INFO." + stream}},
		{"create at other copies, a missing bucket beside it", []string{"create", "--break-glass", "--operator", "op", "--replicas", "3", "--bucket", bus.RoleClaimBucket, "--bucket", bus.SessionBucket},
			1, "differs: " + stream + ": replicas 1, want 3", []string{"INFO." + missing, "INFO." + stream}},
		{"recreate", []string{"recreate", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
			2, "--break-glass is create's alone", nil},
		{"--break-glass before recreate", []string{"--break-glass", "recreate", "--operator", "op", "--bucket", bus.SessionBucket},
			2, "usage: envoy-infra buckets", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			requests.take(t) // the test's own reads of the streams, after the run before
			w := &world{}
			code, stdout, stderr := runCommand(t, w, nil, append(c.args, "--nats", uri)...)
			if code != c.code || !strings.Contains(stdout+stderr, c.says) || strings.Contains(stdout, "created") || len(w.calls) != 0 {
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
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--this-machine"}, "so --this-machine would check nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--kube-context", "prod", "--connz", "http://hub:8222"}, "so --connz and --kube-context would check nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--first-deployment"}, "so --first-deployment would check nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--ssh", "ssm-ssh"}, "so --ssh would check nothing"},
		{[]string{"create", "--break-glass", "--bucket", bus.SessionBucket}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", " ", "--bucket", bus.SessionBucket}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", "op\nenvoy-infra buckets: forged", "--bucket", bus.SessionBucket}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", "op"}, "--break-glass needs each --bucket it creates"},
		{[]string{"create", "--operator", "op", "--bucket", bus.SessionBucket, "--host", "box-1"}, "--operator names who runs a --break-glass create"},
	} {
		w := &world{}
		code, stdout, stderr := runCommand(t, w, nil, append(c.args, "--nats", nowhere)...)
		if code != 2 || !strings.Contains(stderr, c.says) || len(w.calls) != 0 {
			t.Errorf("%q = %d, want the usage error %q and no proof taken (%v)\n%s%s", c.args, code, c.says, w.calls, stdout, stderr)
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
	// The run's replies arrive under nats.go's _INBOX. on the break-glass path and under the
	// command's own prefix on the normal one, so both are watched.
	replies := make(chan *natsgo.Msg, 1024)
	for _, prefix := range []string{natsgo.InboxPrefix, InboxPrefix + "."} {
		if _, err := conn.ChanSubscribe(prefix+">", replies); err != nil {
			t.Fatalf("the racer's watch of the run's answers under %s: %v", prefix, err)
		}
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
func answerTo(replies <-chan *natsgo.Msg, reply string) (string, error) {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-replies:
			if msg.Subject == reply {
				return string(msg.Data), nil
			}
		case <-deadline:
			return "", fmt.Errorf("no answer to %s in 10 s", reply)
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

// resentRequest is the line the bus logs each time it sends a stream request again because the
// server left the send before it unanswered (sentAgain, internal/bus/start_wait.go), naming the
// request "<stream> <request>". A lookup or a handle open waits 630 ms for its answer before it
// is read again (firstReadLimit), so a hub slow under load, or a holdingRelay holding the run,
// receives that read more often than the run made it. Were the line renamed, no resend would be
// counted and distinctSends would compare as strictly as a bare count: never more leniently.
const resentRequest = "envoy nats stream request unanswered; sending it again"

// resentAs is the request of streamRequests.take that each request the bus names in
// resentRequest sends: a lookup, a stream read and a handle open are each one STREAM.INFO, and a
// create one STREAM.CREATE after the account $JS.API.INFO, which take does not see.
var resentAs = map[string]string{"STREAM.INFO": "INFO", "handle open": "INFO", "create": "CREATE", "STREAM.CREATE": "CREATE"}

// recordResends records the default logger until t ends, for distinctSends.
func recordResends(t *testing.T) *logtest.Recorder {
	t.Helper()
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)
	return recorder
}

// distinctSends is requests, as streamRequests.take returns them, with each send the bus logged as
// sent again (resentRequest) taken out. The run makes one request at a time, so a resend reaches
// the hub right after the send it repeats, and is taken out there. A logged resend of a request
// this cannot name, or one the hub did not receive after a send of the same request, fails t, so
// a resend it misreads is never counted as one it can read.
func distinctSends(t *testing.T, requests []string, logs *logtest.Recorder) []string {
	t.Helper()
	resent := map[string]int{}
	for _, record := range logs.Records() {
		if record.Message != resentRequest {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key != "request" {
				return true
			}
			stream, request, _ := strings.Cut(attr.Value.String(), " ")
			verb, named := resentAs[request]
			switch {
			case stream == bus.Stream:
				// take leaves the notification stream's requests out, and so does this.
			case !named:
				t.Errorf("the bus sent %q again, a request this test cannot name", attr.Value.String())
			default:
				resent[verb+"."+stream]++
			}
			return false
		})
	}
	var distinct []string
	for _, request := range requests {
		if n := len(distinct); n > 0 && distinct[n-1] == request && resent[request] > 0 {
			resent[request]--
			continue
		}
		distinct = append(distinct, request)
	}
	if taken := len(requests) - len(distinct); taken > 0 {
		t.Logf("the hub received %v; %d of them the bus logged as sent again", requests, taken)
	}
	for request, left := range resent {
		if left > 0 {
			t.Errorf("the bus logged %d more resends of %s than the hub received after a send of it: %v", left, request, requests)
		}
	}
	return distinct
}

// A bucket another client makes after a break-glass run's read is left as it is: the run reads it
// again and applies create's rule to it, rather than raising, updating or deleting it. The other
// client is a racer (race): the run reaches a three-member hub through a relay that holds it after
// its first request on the trigger (holdingRelay), and in that hold the racer makes the bucket, then
// lets the run go on.
//
//   - At the run's read of the session bucket, which the hub answers "stream not found", the racer
//     makes the bucket at one copy where the run asks for three. The create's own lookup finds it,
//     the run reads it again, and is refused naming the copies, having sent the bucket's stream its
//     three reads alone.
//   - Between the create's lookup and its create - at the account info the create asks for first,
//     as nats.go does (bus, addBucketStream) - the racer makes the role-claim bucket with the
//     discard policy and direct get of a KV made before nats-server 2.7.2 and 2.9. The hub answers
//     the create "stream name already in use"; the create neither reads nor updates the stream
//     after that answer (bus, createOnly), so no metadata of the hub's keeps an update unsent
//     (bus, TestCreateMissingCopiedBucketUpdatesNoStreamItsCreateFindsInUse); the run reads it
//     again and is refused naming both.
//   - At the run's read of the known-session bucket, the racer makes it at the three copies the run
//     asks for, with its own configuration. The create's own lookup finds it, the run reads it again
//     and says it is present, exit 0. At b20f45e30a (#4296's head) that run was refused, exit 1.
//
// Each case runs on both paths: --break-glass, as the gke listener's user on the default _INBOX.
// prefix, and the normal create, its replies under InboxPrefix, as the hub's unrestricted watcher
// user. Neither sends a STREAM.UPDATE or anything else but reads and the one create: at b20f45e30a
// (#4296's head) the normal create, with the inventory it then required, created through
// bus.CreateCopiedBucket, which raises a bucket its own lookup finds with a stream update.
//
// Each request is counted once however often the bus sent it (distinctSends): the hold keeps the
// run's next request from the hub for as long as the racer takes, and a lookup held past its read
// limit, 630 ms, is sent again by design, which a loaded machine reaches.
func TestCreateLeavesABucketMadeAfterItsReadAsItIs(t *testing.T) {
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
		code                  int
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
			code:     1,
			says: []string{"refused: KV_" + bus.SessionBucket + " was made by another client after the read", "KV_" + bus.SessionBucket + " STREAM.INFO",
				"differs: KV_" + bus.SessionBucket + ": replicas 1, want 3"},
			sent: []string{"INFO", "INFO", "INFO"},
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
			code: 1,
			says: []string{"refused: KV_" + bus.RoleClaimBucket + " was made by another client after the read", "KV_" + bus.RoleClaimBucket + " create", "stream name already in use",
				"discard policy DiscardOld, want DiscardNew", "direct get false, want true"},
			// The run's read, the create's lookup, the create, and the run's read again: no read of
			// the stream after the create's "already in use", and no update.
			sent: []string{"INFO", "INFO", "CREATE", "INFO"},
		},
		{
			name:    "at the copies asked for with its own configuration when the run's read arrives",
			bucket:  bus.KnownSessionBucket,
			trigger: "$JS.API.STREAM.INFO.KV_" + bus.KnownSessionBucket,
			makeBucket: func(js natsgo.JetStreamContext) error {
				config, _ := bus.CopiedBucketConfig(bus.KnownSessionBucket, 3)
				_, err := js.CreateKeyValue(&config)
				return err
			},
			answered: "stream not found",
			code:     0,
			says:     []string{bus.KnownSessionBucket + " present at 3 copies with its own configuration, made by another client after the read"},
			sent:     []string{"INFO", "INFO", "INFO"},
		},
	} {
		for _, path := range []string{"break-glass", "normal"} {
			t.Run(c.name+", "+path, func(t *testing.T) {
				args := []string{"create", "--break-glass", "--operator", "op", "--bucket", c.bucket}
				if path == "normal" {
					args = []string{"create", "--bucket", c.bucket}
					// No seed: the run connects as the hub's no_auth_user, the watcher.
					t.Setenv("NATS_NKEY_SEED", "")
					if err := os.Unsetenv("NATS_NKEY_SEED"); err != nil {
						t.Fatalf("unset NATS_NKEY_SEED: %v", err)
					}
				}
				stream := "KV_" + c.bucket
				t.Cleanup(func() {
					if err := js.DeleteStream(stream); err != nil && !errors.Is(err, natsgo.ErrStreamNotFound) {
						t.Errorf("delete %s for the next run: %v", stream, err)
					}
				})
				requests.take(t)
				resends := recordResends(t)
				relay := startHoldingRelay(t, uri, c.trigger)
				r := race(t, uri, relay, stream, c.makeBucket)
				code, stdout, stderr := runCommand(t, &world{}, nil, append(args, "--nats", relay.url)...)
				<-r.done
				if r.err != nil || !strings.Contains(r.answer, c.answered) {
					t.Fatalf("the racer: %v; the hub answered the run's request %q, want %q", r.err, r.answer, c.answered)
				}
				// The run's requests are taken before the read below, which is the test's own.
				var sent []string
				for _, request := range distinctSends(t, requests.take(t), resends) {
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
				if code != c.code || strings.Contains(stdout, "created") {
					t.Errorf("the run = %d, want %d and nothing created\n%s%s", code, c.code, stdout, stderr)
				}
				for _, says := range c.says {
					if !strings.Contains(stdout+stderr, says) {
						t.Errorf("the run does not name %q:\n%s%s", says, stdout, stderr)
					}
				}
			})
		}
	}
}
