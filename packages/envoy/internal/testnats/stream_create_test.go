package testnats

import (
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// cutStream is the three-copy stream the create rows make.
func cutStream(name string) natsgo.StreamConfig {
	return natsgo.StreamConfig{Name: name, Subjects: []string{strings.ToLower(name) + ".>"}, Replicas: 3, Storage: natsgo.FileStorage, MaxAge: time.Hour}
}

// dropAtCreate holds every message between cluster's members from now until the first create of
// stream reaches member, the one member the create is sent through, and returns how many creates
// of stream member has seen. At that first create it stops member's client relay, so member's
// clients hear nothing more from it, drops every client connection on member but its own watcher's,
// and lets the members talk again; once the stream is answered (streamAnswered) it starts the relay
// again, and only then does the creating client find its connection gone and reconnect. While the
// members cannot talk, the meta leader can commit no assignment and no stream can elect a leader,
// so the create's answer cannot exist before the drop; it is then sent while the creating client
// is connected nowhere and its subscription for the answer is gone, so it reaches no one. member
// sees the create on its own, as a client's publish reaches the member it is connected to without
// a route.
func dropAtCreate(t *testing.T, cluster *Cluster, member *Server, stream string) func() int {
	t.Helper()
	thawRoutes := cluster.FreezeRoutes(t)
	var mu sync.Mutex
	creates := 0
	watch := func(*natsgo.Msg) {
		mu.Lock()
		creates++
		first := creates == 1
		mu.Unlock()
		if !first {
			return
		}
		thawClients := member.Freeze(t)
		defer thawClients()
		srv := member.natsServer()
		connz, err := srv.Connz(&natsserver.ConnzOptions{})
		if err != nil {
			t.Errorf("list member %s's connections: %v", member.Name(), err)
			return
		}
		for _, conn := range connz.Conns {
			if conn.Name != "create-watcher" {
				_ = srv.DisconnectClientByID(conn.Cid)
			}
		}
		thawRoutes()
		// The watcher runs on nats.go's goroutine, where no method of t that ends the test may
		// run, so the wait is the shared wait's helper form, whose give-up is an error.
		if err := testwait.InHelper("testnats", fmt.Sprintf("a member to lead stream %s with every member current after the drop", stream), func() bool {
			return streamAnswered(cluster, stream)
		}); err != nil {
			t.Error(err)
		}
	}
	watcher, err := AwaitConnectSeed(t, member.URL(), "", natsgo.Name("create-watcher"))
	if err != nil {
		t.Fatalf("watch member %s's creates: %v", member.Name(), err)
	}
	t.Cleanup(watcher.Close)
	if _, err := watcher.Subscribe("$JS.API.STREAM.CREATE."+stream, watch); err != nil {
		t.Fatalf("watch member %s's creates: %v", member.Name(), err)
	}
	if err := watcher.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("watch member %s's creates: %v", member.Name(), err)
	}
	t.Cleanup(thawRoutes)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return creates
	}
}

// streamAnswered reports, from the members' own state, whether a member of cluster leads stream
// and every member holds it current. The stream's first leader answers the create as it takes
// over (processStreamLeaderChange), and the other members are current only once its entries have
// reached them over the routes, round trips later. An answer later still would reach the creating
// client once it reconnects, and the create would end after one send, which the drop row fails. A
// member that runs JetStream with no cluster, as a lone server (Start) does, has no stream leader
// to report (nats-server v2.15.1 JetStreamIsStreamLeader is false there, and
// JetStreamIsStreamCurrent true for any name), so it leads stream once its account holds it
// (holdsStream).
func streamAnswered(cluster *Cluster, stream string) bool {
	led := false
	for _, member := range cluster.Servers {
		srv := member.natsServer()
		if srv == nil || !srv.JetStreamIsStreamCurrent(natsserver.DEFAULT_GLOBAL_ACCOUNT, stream) {
			return false
		}
		led = led || srv.JetStreamIsStreamLeader(natsserver.DEFAULT_GLOBAL_ACCOUNT, stream) || (!srv.JetStreamIsClustered() && holdsStream(srv, stream))
	}
	return led
}

// holdsStream reports whether srv's global account holds stream, read from the server's own state
// (Jsz) with no connection to it.
func holdsStream(srv *natsserver.Server, stream string) bool {
	info, err := srv.Jsz(&natsserver.JSzOptions{Account: natsserver.DEFAULT_GLOBAL_ACCOUNT, Streams: true})
	if err != nil {
		return false
	}
	for _, account := range info.AccountDetails {
		for _, held := range account.Streams {
			if held.Name == stream {
				return true
			}
		}
	}
	return false
}

// heldStreams names, member by member, the streams each member's global account holds, read
// from the server's own state (Jsz) as holdsStream reads it: a wait's reading of what the members
// held while the stream it waits for was not yet answered. A member with no name of its own (a
// cluster's single server) is named by its place among the members.
func heldStreams(cluster *Cluster) string {
	var members []string
	for i, member := range cluster.Servers {
		label := member.Name()
		if label == "" {
			label = fmt.Sprintf("member %d", i)
		}
		srv := member.natsServer()
		if srv == nil {
			members = append(members, label+" runs no server")
			continue
		}
		info, err := srv.Jsz(&natsserver.JSzOptions{Account: natsserver.DEFAULT_GLOBAL_ACCOUNT, Streams: true})
		if err != nil {
			members = append(members, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		var names []string
		for _, account := range info.AccountDetails {
			for _, held := range account.Streams {
				names = append(names, held.Name)
			}
		}
		members = append(members, fmt.Sprintf("%s holds %v", label, names))
	}
	return strings.Join(members, "; ")
}

// A create whose answer the cluster never delivers, because the connection it was sent on dropped
// before the stream's first leader could answer, is sent again on the connection's reconnect, and
// the cluster answers it with the stream the first send made: the create ends after at least two
// sends, and the stream holds its three copies. The create goes through the meta leader's member
// alone: while the routes are held, a create through another member never reaches the meta leader,
// which alone takes it, so it would go unanswered whatever the drop did.
func TestACreateWhoseAnswerADropLostIsSentAgainAndAnswered(t *testing.T) {
	cluster := StartCluster(t, 3)
	leader := -1
	for i, member := range cluster.Servers {
		if member.natsServer().JetStreamIsLeader() {
			leader = i
		}
	}
	if leader < 0 {
		t.Fatal("no member of the ready cluster leads its meta group")
	}
	creates := dropAtCreate(t, cluster, cluster.Servers[leader], "CUT")
	(&Cluster{Servers: []*Server{cluster.Servers[leader]}}).CreateStream(t, cutStream("CUT"))
	// The watcher hears the answered send as it reaches the member, before the answer, but on a
	// goroutine of its own, so the count is waited for: a create that ended after one send never
	// reaches two, and the wait fails naming the sends it saw.
	testwait.Until(t, "the create to have been sent twice: the send whose answer was lost, and at least one more", func() (int, bool) {
		sends := creates()
		return sends, sends >= 2
	})
	info := AwaitStreamCopies(t, cluster.JetStream(t), "CUT", 3)
	if info.Config.MaxAge != time.Hour {
		t.Fatalf("stream CUT has max age %s, want the create's hour", info.Config.MaxAge)
	}
}

// A create of a name the cluster already holds with another configuration fails the test, naming
// each field the create sets that the stream held sets otherwise, with the value held and the value
// asked, and no field the two share: not one the create leaves unset that the API writes anyway
// and the server stores as its default, as max_msgs, max_bytes, max_consumers and
// max_msgs_per_subject.
func TestACreateOfANameTheClusterHoldsOtherwiseFailsNamingTheDifference(t *testing.T) {
	cluster := StartCluster(t, 3)
	held := cutStream("HELD")
	held.Replicas = 1
	cluster.CreateStream(t, held)
	asked := cutStream("HELD")
	asked.MaxAge = time.Minute
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cluster.CreateStream(recorder, asked)
	}()
	testwait.Receive(t, "the create of a name the cluster holds otherwise to fail", done)
	for _, want := range []string{
		"create stream HELD: the cluster holds a stream of that name with another configuration: ",
		fmt.Sprintf("max_age holds %d, asked %d", time.Hour, time.Minute),
		"num_replicas holds 1, asked 3",
	} {
		if !strings.Contains(recorder.message, want) {
			t.Errorf("the create's failure is %q, want it to name %q", recorder.message, want)
		}
	}
	for _, shared := range []string{"subjects", "storage", "max_msgs", "max_bytes", "max_consumers"} {
		if strings.Contains(recorder.message, shared) {
			t.Errorf("the create's failure is %q, naming %s, a field the two configurations share", recorder.message, shared)
		}
	}
}

// A create of a name the cluster holds otherwise only in a field the create leaves unset, here the
// duplicate window, fails the test naming the whole stream held, as no field the create sets
// differs: a field the create leaves unset that the API writes anyway, as num_replicas and the four
// limits, is the server's default, which is no difference.
func TestACreateOfANameHeldOtherwiseOnlyInAFieldItLeavesUnsetNamesTheWholeStreamHeld(t *testing.T) {
	cluster := StartCluster(t, 3)
	held := cutStream("WINDOW")
	held.Replicas = 1
	held.Duplicates = 30 * time.Minute
	cluster.CreateStream(t, held)
	asked := cutStream("WINDOW")
	asked.Replicas = 0
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cluster.CreateStream(recorder, asked)
	}()
	testwait.Receive(t, "the create of a name held otherwise only in a field it leaves unset to fail", done)
	for _, want := range []string{
		"create stream WINDOW: the cluster holds a stream of that name with another configuration: every field the create sets matches the stream held, which is ",
		fmt.Sprintf(`"duplicate_window":%d`, 30*time.Minute),
	} {
		if !strings.Contains(recorder.message, want) {
			t.Errorf("the create's failure is %q, want it to name %q", recorder.message, want)
		}
	}
}

// A create whose connection's first dials draw no handshake, as a member too loaded to answer
// within its dial's handshake timeout (createConnectOptions) leaves them, waits for the
// connection, which retries its dial, and is
// answered once the member answers. The server stays frozen until its relay has joined two dials
// of the create's connection (dialsSince): the second comes only once the first handshake has run
// out of time and the connection has dialled again, and the create must still be waiting then. No
// span of time decides when the server thaws. A create whose goroutine ran late dials late, and
// nats.go starts its handshake's deadline only after the TCP dial the relay sees (v1.55.0
// nats.go:2539 and :2944), so a thaw a fixed span after the create's start or after its first dial
// could come before the first handshake ran out, and the create would then connect with no dial
// retried. The row reads the stream the create made from the server's own state (streamAnswered),
// not over a connection, whose read would wait on the relay as a request bounded by jetStreamWait.
func TestACreateWhoseFirstDialsDrawNoHandshakeWaitsForTheConnection(t *testing.T) {
	server, _ := Start(t)
	cluster := &Cluster{Servers: []*Server{server}}
	thaw := server.Freeze(t)
	t.Cleanup(thaw)
	dials := dialsSince(server)
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cluster.CreateStream(recorder, natsgo.StreamConfig{Name: "LATE", Subjects: []string{"late.>"}, Storage: natsgo.FileStorage})
	}()
	// The reading is how many dials the relay has joined; the wait ends at the second, or at the
	// create's end.
	testwait.Until(t, "the create's connection to dial the frozen server twice, or the create to end", func() (int, bool) {
		select {
		case <-done:
			return dials(), true
		default:
			n := dials()
			return n, n >= 2
		}
	})
	select {
	case <-done:
		t.Fatalf("the create ended while the server could answer no dial, with %d of its connection's dials joined, want 2: %q", dials(), recorder.message)
	default:
	}
	thaw()
	testwait.Receive(t, "the create to end once the member answers its dials", done)
	if recorder.message != "" {
		t.Fatalf("the create failed once the server answered: %s", recorder.message)
	}
	testwait.Until(t, "the server to hold stream LATE, which the create made", func() (string, bool) {
		if streamAnswered(cluster, "LATE") {
			return "LATE answered", true
		}
		return "LATE not answered; " + heldStreams(cluster), false
	})
}

// dialsSince returns a func counting the connections server's relay has joined to the server
// since dialsSince was called: each a client's dial of the server's URL, which the relay accepts
// and joins while it is frozen too. A frozen relay moves none of a connection's bytes, so the
// server's INFO never reaches the client, and it notices neither side's close: each connection
// stays joined until the thaw, so while the relay is frozen the count only grows, and a dial is
// counted however late the func is asked. In the dial row every dial it counts is the create
// connection's: Start leaves no connection to the server, and the row makes no other.
func dialsSince(server *Server) func() int {
	r := server.relay
	r.mu.Lock()
	before := maps.Clone(r.joins)
	r.mu.Unlock()
	return func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		n := 0
		for j := range r.joins {
			if _, held := before[j]; !held {
				n++
			}
		}
		return n
	}
}

const (
	// unansweredBucketChildEnv has TestAnUnansweredBucketCreateFailsByNameBeforeTheTestBinarysDeadline
	// run as its own child (testwait.RunChild), which makes the create on the server the parent
	// started, whose URL the variable holds.
	unansweredBucketChildEnv = "TESTNATS_UNANSWERED_BUCKET_CHILD"

	// unansweredBucketGuard is the child's -timeout (testwait.RunChild): a hang guard only, which a
	// child whose create never gives up runs into, and never what decides the row. The child's
	// whole run measured 0.03 s beside two busy loops (a builds Job at the commit that made this
	// form) and 0.04 s in an issue pod; two minutes is over a thousand times the slower, and a
	// setup held 25 s still passes inside it.
	unansweredBucketGuard = 2 * time.Minute

	// accountInfoDelay is how late the child's stand-in for JetStream answers the account
	// information a bucket's create reads first, on the clock the waits read (testwait.Pause),
	// when the stand-in reads that request before the send's bound has passed on the child's step
	// clock.
	accountInfoDelay = 8 * time.Second
)

// A bucket create that nothing answers fails the test naming its sends before the test binary's
// deadline: a send under way as the shared wait gives up is its last, and ends inside the margin.
// The child sets up the stand-in and the creating client on the wall clock, under the shared
// wait's own bound, and then takes the give-up as a step of its own (testwait.SetUpDone), so the
// create's wait asks once and its one send runs on the child's step clock, bounded there as a
// whole by createSendWait (sendOnTheWaitsClock). The step clock fires that bound's timer once a
// wait is blocked on it and no earlier timer is armed, and the send's first request reaches the
// stand-in over a socket, so the bound can pass before the stand-in has read the request: the
// send then ends createSendWait into the step with nats.go's timeout, 20 s before the deadline on
// that clock, and the stand-in's late answer never comes. When the stand-in reads the request
// first, its answer comes accountInfoDelay into the step, the create of the bucket's stream never,
// and the send ends at the same moment. Either way nothing answers the create, and the row reads
// only the give-up's text. A send held past the 30 s margin would end the child at the deadline,
// as go test's alarm does, and the row fails. The
// parent starts the server under its own deadline and passes its URL to the child, so a server
// whose start is slow costs the parent time and not the child. Under a fixed -timeout of 48 s the
// child's waits gave up 18 s in, and a setup still running then failed the child with a wait of
// its setup's own rather than the create's.
func TestAnUnansweredBucketCreateFailsByNameBeforeTheTestBinarysDeadline(t *testing.T) {
	if uri := os.Getenv(unansweredBucketChildEnv); uri != "" {
		createUnansweredBucket(t, uri)
		return
	}
	server := start(t, "", nil)
	// The child fails by design: its create draws no answer.
	stdout, stderr, _ := testwait.RunChild(t, unansweredBucketGuard, "^"+t.Name()+"$", unansweredBucketChildEnv+"="+server.URL())
	output := stdout + stderr
	if strings.Contains(output, "panic: test timed out") ||
		!strings.Contains(output, "gave up waiting for create KV bucket UNANSWERED: an answer to one of its sends") ||
		!strings.Contains(output, " sends drew no answer, the last ") {
		t.Fatalf("the child's create of a bucket nothing answers did not fail naming its sends before its deadline:\n%s", output)
	}
}

// createUnansweredBucket is the child of
// TestAnUnansweredBucketCreateFailsByNameBeforeTheTestBinarysDeadline: a bucket create on the server
// without JetStream the parent started at uri, whose stand-in answers a read of the account's
// JetStream information accountInfoDelay late when it reads that request in time, and never
// answers the create of the bucket's stream.
// Everything before the create is its setup, and the give-up is its next step (testwait.SetUpDone).
func createUnansweredBucket(t *testing.T, uri string) {
	standIn := connectWithoutJetStream(t, uri, "the stand-in for JetStream")
	if _, err := standIn.Subscribe("$JS.API.INFO", func(msg *natsgo.Msg) {
		go func() {
			testwait.Pause(accountInfoDelay)
			_ = msg.Respond([]byte(`{"type":"io.nats.jetstream.api.v1.account_info_response"}`))
		}()
	}); err != nil {
		t.Fatalf("answer account information reads: %v", err)
	}
	if _, err := standIn.Subscribe("$JS.API.STREAM.CREATE.KV_UNANSWERED", func(*natsgo.Msg) {}); err != nil {
		t.Fatalf("take the bucket's create: %v", err)
	}
	if err := standIn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush the stand-in's subscriptions: %v", err)
	}
	conn := connectWithoutJetStream(t, uri, "the creating client")
	testwait.SetUpDone(t)
	(&Cluster{}).CreateKeyValue(t, conn, &natsgo.KeyValueConfig{Bucket: "UNANSWERED"})
	t.Fatal("the create of a bucket nothing answers returned")
}

// connectWithoutJetStream connects who to the server at uri through the shared wait, as Connect
// does, each dial carrying the wait's give-up (giveUpDial), and closes the connection when the test
// ends. Unlike Connect it waits for no JetStream: the unanswered-bucket child's server has none,
// and its stand-in answers JetStream's account reads late on purpose. A connect whose 2 s of
// nats.go's own ran out on a loaded machine is made again, where a bare connect would fail the
// child's setup.
func connectWithoutJetStream(t testing.TB, uri, who string) *natsgo.Conn {
	t.Helper()
	dial := testwait.Until(t, fmt.Sprintf("a connection of %s to the NATS server at %s", who, uri), func() (dialAttempt, bool) {
		conn, err := natsgo.Connect(uri, natsgo.NoReconnect(), giveUpDial(t))
		return dialAttempt{conn: conn, err: err}, err == nil
	})
	t.Cleanup(dial.conn.Close)
	return dial.conn
}

// A bucket create the connection's grant refuses fails the test naming the server's refusal after
// the one send it refused, though the server reports the refusal on the connection and the create
// itself draws no answer.
func TestARefusedBucketCreateFailsNamingTheRefusalAfterItsOneSend(t *testing.T) {
	seed, public := User(t)
	uri := StartNkeyDenied(t, public, "$JS.API.STREAM.CREATE.>")
	conn, err := AwaitConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect as the refused user: %v", err)
	}
	t.Cleanup(conn.Close)
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Cluster{}).CreateKeyValue(recorder, conn, &natsgo.KeyValueConfig{Bucket: "DENIED"})
	}()
	// A refusal read as no answer is sent again until the shared wait gives up, and the create then
	// fails naming that give-up; the refusal read as the answer ends the create after its one send.
	testwait.Receive(t, "the refused create to fail", done)
	for _, want := range []string{"create KV bucket DENIED: ", `Permissions Violation for Publish to "$JS.API.STREAM.CREATE.KV_DENIED"`} {
		if !strings.Contains(recorder.message, want) {
			t.Errorf("the refused create failed with %q, want it to name %q", recorder.message, want)
		}
	}
}
