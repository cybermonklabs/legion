package buckets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
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
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--this-machine"}, "so --this-machine would check nothing"},
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
