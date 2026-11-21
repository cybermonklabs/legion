package buckets

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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
	"github.com/sjawhar/envoy/internal/testnats"
)

// `create --break-glass` runs as a listener's NATS user: here one holding exactly the grant the hub
// renders for the in-cluster gke listener (core.ListenerHostPermissions), which receives its
// replies on the default _INBOX. prefix. It creates the missing session bucket with the stream
// configuration the copied buckets take (the same configuration the watcher makes through
// bus.CopiedBucketConfig on a server of its own), sends the bucket's stream reads and one create and
// no update or delete, opens no hold, and logs one line naming the operator, the server, the
// bucket and break-glass. The control is the same user's run without the flag, which is a usage
// error (it needs the hold's flags) and creates nothing.
func TestBreakGlassCreatesAMissingBucketAsAListenersUser(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	stream := "KV_" + bus.SessionBucket
	// The copied bucket's configuration, on a server of its own.
	_, referenceURI := testnats.Start(t)
	_, referenceJS := jetStream(t, referenceURI)
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	if _, err := referenceJS.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the reference bucket: %v", err)
	}
	reference, err := referenceJS.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the reference stream: %v", err)
	}

	uri, monitor, seed := listenerUserServer(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	watcher, js := jetStream(t, uri)
	requests := watchStreamRequests(t, watcher)
	connz := connzWithout(t, monitor, watcher)

	code, stdout, stderr := runCommand(t, noHold(t), "create", "--bucket", bus.SessionBucket, "--nats", uri)
	if code != 2 || !strings.Contains(stderr, "--broker-url is required") {
		t.Fatalf("control: create as a listener's user without --break-glass = %d, want the usage error\n%s%s", code, stdout, stderr)
	}
	if _, err := js.StreamInfo(stream); !errors.Is(err, natsgo.ErrStreamNotFound) {
		t.Fatalf("control: the create without --break-glass made %s: %v", stream, err)
	}
	requests.take(t)

	code, stdout, stderr = runCommand(t, noHold(t), "create", "--break-glass", "--operator", "operator@example.com", "--bucket", bus.SessionBucket, "--nats", uri, "--connz", connz)
	if code != 0 || stdout != "created "+bus.SessionBucket+" (1 copies)\n" {
		t.Fatalf("create --break-glass as a listener's user = %d\n%s%s", code, stdout, stderr)
	}
	logged := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(logged) != 1 || !strings.Contains(logged[0], "break-glass") || !strings.Contains(logged[0], "operator operator@example.com") ||
		!strings.Contains(logged[0], " "+bus.SessionBucket+" ") || !strings.Contains(logged[0], " on "+uri+",") {
		t.Fatalf("create --break-glass logged %q, want one line naming break-glass, the operator, the bucket and the server", stderr)
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
	if !reflect.DeepEqual(created.Config, reference.Config) {
		t.Fatalf("create --break-glass made %+v\nthe copied bucket's configuration is %+v", created.Config, reference.Config)
	}
}

// A break-glass create only creates. With the session bucket there, holding a key, it changes
// nothing and exits 0 (an upgrade's `up`), sending the bucket's stream one read and nothing else;
// with a missing bucket named beside it, it refuses and creates neither. Either way the stream stays
// as it was: the same stream, configuration, messages and key. --break-glass with recreate is
// refused as a usage error before it connects, and sends the stream nothing.
func TestBreakGlassRefusesABucketThatExistsAndChangesNothing(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	uri, monitor, seed := listenerUserServer(t)
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
	connz := connzWithout(t, monitor, watcher)

	for _, c := range []struct {
		name string
		args []string
		code int
		says string
		sent []string
	}{
		{"create of the bucket that exists", []string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
			0, "nothing to create", []string{"INFO." + stream}},
		{"create of a missing bucket beside it", []string{"create", "--break-glass", "--operator", "op", "--bucket", bus.RoleClaimBucket, "--bucket", bus.SessionBucket},
			1, "refused: " + stream + " exists", []string{"INFO." + missing, "INFO." + stream}},
		{"recreate", []string{"recreate", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket},
			2, "--break-glass is create's alone", nil},
		{"--break-glass before recreate", []string{"--break-glass", "recreate", "--operator", "op", "--bucket", bus.SessionBucket},
			2, "usage: envoy-infra buckets", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			requests.take(t) // the test's own reads of the streams, after the run before
			code, stdout, stderr := runCommand(t, noHold(t), append(c.args, "--nats", uri, "--connz", connz)...)
			if code != c.code || !strings.Contains(stdout+stderr, c.says) || strings.Contains(stdout, "created") {
				t.Fatalf("%v = %d, want %d saying %q and nothing created\n%s%s", c.args, code, c.code, c.says, stdout, stderr)
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
// recreate or restore, beside a hold flag it would leave doing nothing, without --operator or with
// one that spans lines, without a --bucket, without --connz, or with no one --nats; and --operator
// and --connz are refused without it. The URL names no server, so a run that went on would fail to
// connect, with 1.
func TestBreakGlassIsRefusedWhereItIsMisused(t *testing.T) {
	const nowhere = "nats://127.0.0.1:1"
	const connz = "http://127.0.0.1:1"
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"recreate", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz}, "--break-glass is create's alone"},
		{[]string{"restore", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz}, "--break-glass is create's alone"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz, "--hold", "h"}, "so --hold would do nothing"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz, "--broker-url", "https://b", "--reason", "r"}, "so --broker-url and --reason would do nothing"},
		{[]string{"create", "--break-glass", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", " ", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", "op\nenvoy-infra buckets: forged", "--bucket", bus.SessionBucket, "--nats", nowhere, "--connz", connz}, "--break-glass needs --operator"},
		{[]string{"create", "--break-glass", "--operator", "op", "--nats", nowhere, "--connz", connz}, "--break-glass needs each missing --bucket"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", nowhere}, "--break-glass needs --connz"},
		{[]string{"create", "--break-glass", "--operator", "op", "--bucket", bus.SessionBucket, "--connz", connz}, "--break-glass needs one --nats"},
		{[]string{"create", "--operator", "op", "--bucket", bus.SessionBucket, "--nats", nowhere}, "--operator and --connz are --break-glass's"},
		{[]string{"create", "--connz", connz, "--bucket", bus.SessionBucket, "--nats", nowhere}, "--operator and --connz are --break-glass's"},
	} {
		code, stdout, stderr := runCommand(t, noHold(t), c.args...)
		if code != 2 || !strings.Contains(stderr, c.says) {
			t.Errorf("%q = %d, want the usage error %q\n%s%s", c.args, code, c.says, stdout, stderr)
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
	conn   *natsgo.Conn       // the racer's own connection, which the run's connection check leaves out
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
	r := &racer{conn: conn, done: make(chan struct{})}
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
			connz := connzWithout(t, cluster.Servers[0].MonitorURL(), watcher, r.conn)
			code, stdout, stderr := runCommand(t, noHold(t), "create", "--break-glass", "--operator", "op", "--bucket", c.bucket, "--nats", relay.url, "--connz", connz)
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
