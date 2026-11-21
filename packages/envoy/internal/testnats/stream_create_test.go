package testnats

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
)

// cutStream is the three-copy stream the create rows make.
func cutStream(name string) natsgo.StreamConfig {
	return natsgo.StreamConfig{Name: name, Subjects: []string{strings.ToLower(name) + ".>"}, Replicas: 3, Storage: natsgo.FileStorage, MaxAge: time.Hour}
}

// dropAtCreate holds every message between cluster's members from now until the first create of
// stream reaches a member, then drops every client connection on every member but its own
// watchers', and only then lets the members talk again. While the members cannot talk, the meta
// leader can commit no assignment and no stream can elect a leader, so the create's answer, which
// the stream's first leader sends, cannot exist before the drop: it is sent, if at all, to a
// connection that is gone. The member the create reaches first sees it on its own, as a client's
// publish reaches the member it is connected to without a route. It reports how many drops it
// made, which is one once the create has gone out.
func dropAtCreate(t *testing.T, cluster *Cluster, stream string) func() int {
	t.Helper()
	var thaws []func()
	for _, route := range cluster.routes {
		thaws = append(thaws, route.freeze())
	}
	var mu sync.Mutex
	drops := 0
	drop := func(*natsgo.Msg) {
		mu.Lock()
		defer mu.Unlock()
		if drops > 0 {
			return
		}
		drops++
		for _, member := range cluster.Servers {
			srv := member.natsServer()
			connz, err := srv.Connz(&natsserver.ConnzOptions{})
			if err != nil {
				t.Errorf("list member %s's connections: %v", member.Name(), err)
				continue
			}
			for _, conn := range connz.Conns {
				if conn.Name != "create-watcher" {
					_ = srv.DisconnectClientByID(conn.Cid)
				}
			}
		}
		for _, thaw := range thaws {
			thaw()
		}
	}
	for _, member := range cluster.Servers {
		watcher, err := natsgo.Connect(member.URL(), natsgo.Name("create-watcher"))
		if err != nil {
			t.Fatalf("watch member %s's creates: %v", member.Name(), err)
		}
		t.Cleanup(watcher.Close)
		if _, err := watcher.Subscribe("$JS.API.STREAM.CREATE."+stream, drop); err != nil {
			t.Fatalf("watch member %s's creates: %v", member.Name(), err)
		}
		if err := watcher.Flush(); err != nil {
			t.Fatalf("watch member %s's creates: %v", member.Name(), err)
		}
	}
	t.Cleanup(func() {
		for _, thaw := range thaws {
			thaw()
		}
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return drops
	}
}

// A create whose answer the cluster never delivers, because the connection it was sent on dropped
// before the stream's first leader could answer, is sent again on the connection's reconnect, and
// the cluster answers it with the stream the first send made: the create ends, and the stream
// holds its three copies. The create goes through the meta leader's member alone: the members
// cannot talk until the drop, and only the meta leader answers the account info a client may read
// as it connects.
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
	drops := dropAtCreate(t, cluster, "CUT")
	(&Cluster{Servers: []*Server{cluster.Servers[leader]}}).CreateStream(t, cutStream("CUT"))
	if got := drops(); got != 1 {
		t.Fatalf("the create ended with %d drops, want the one its first send met", got)
	}
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
	<-done
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
	<-done
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
// within dialTimeout leaves them, waits for the connection, which retries its dial, and is
// answered once the member answers.
func TestACreateWhoseFirstDialsDrawNoHandshakeWaitsForTheConnection(t *testing.T) {
	server, _ := Start(t)
	cluster := &Cluster{Servers: []*Server{server}}
	thaw := server.Freeze(t)
	t.Cleanup(thaw)
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cluster.CreateStream(recorder, natsgo.StreamConfig{Name: "LATE", Subjects: []string{"late.>"}, Storage: natsgo.FileStorage})
	}()
	select {
	case <-done:
		t.Fatalf("the create ended while the server could not answer a dial: %q", recorder.message)
	case <-time.After(3 * dialTimeout):
	}
	thaw()
	<-done
	if recorder.message != "" {
		t.Fatalf("the create failed once the server answered: %s", recorder.message)
	}
	if _, err := cluster.JetStream(t).StreamInfo("LATE"); err != nil {
		t.Fatalf("read stream LATE the create made: %v", err)
	}
}

const (
	// unansweredBucketChildEnv has TestAnUnansweredBucketCreateFailsByNameBeforeTheTestBinarysDeadline
	// run as its own child, which makes the create under a deadline of unansweredBucketDeadline.
	unansweredBucketChildEnv = "TESTNATS_UNANSWERED_BUCKET_CHILD"

	// unansweredBucketDeadline is the child's go test -timeout, and accountInfoDelay how late the
	// child's stand-in for JetStream answers the account information a bucket's create reads first.
	// One send of the create then takes accountInfoDelay and createSendWait, as nothing answers the
	// create of the bucket's stream: 18 s of the child's 32. A margin that covers one request a
	// send (2*createSendWait+1s) leaves the wait a bound of about 11 s, less the child's start: the
	// first send runs past it, and the one more send the shared wait makes then would end at about
	// 36 s, so go test's timeout panic comes first. Under the bucket's own margin the bound has
	// passed before the first send ends, and that send's failure names it.
	unansweredBucketDeadline = 32 * time.Second
	accountInfoDelay         = 8 * time.Second
)

// A bucket create that nothing answers fails the test naming its sends before the test binary's
// deadline, though one send of it makes two requests, each bounded by createSendWait, and the
// first is answered late.
func TestAnUnansweredBucketCreateFailsByNameBeforeTheTestBinarysDeadline(t *testing.T) {
	if os.Getenv(unansweredBucketChildEnv) != "" {
		createUnansweredBucket(t)
		return
	}
	child := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.count=1", "-test.timeout="+unansweredBucketDeadline.String())
	child.Env = append(os.Environ(), unansweredBucketChildEnv+"=1")
	output, _ := child.CombinedOutput() // the child fails by design: its create draws no answer
	if strings.Contains(string(output), "panic: test timed out") ||
		!strings.Contains(string(output), "create KV bucket UNANSWERED: ") ||
		!strings.Contains(string(output), " drew no answer before the test binary's deadline") {
		t.Fatalf("the child's create of a bucket nothing answers did not fail naming its sends before its %s deadline:\n%s", unansweredBucketDeadline, output)
	}
}

// createUnansweredBucket is the child of
// TestAnUnansweredBucketCreateFailsByNameBeforeTheTestBinarysDeadline: a bucket create on a server
// without JetStream, whose stand-in answers each read of the account's JetStream information
// accountInfoDelay late and never answers the create of the bucket's stream.
func createUnansweredBucket(t *testing.T) {
	server := start(t, "", nil)
	standIn, err := natsgo.Connect(server.URL())
	if err != nil {
		t.Fatalf("connect the stand-in for JetStream: %v", err)
	}
	t.Cleanup(standIn.Close)
	if _, err := standIn.Subscribe("$JS.API.INFO", func(msg *natsgo.Msg) {
		time.AfterFunc(accountInfoDelay, func() {
			_ = msg.Respond([]byte(`{"type":"io.nats.jetstream.api.v1.account_info_response"}`))
		})
	}); err != nil {
		t.Fatalf("answer account information reads: %v", err)
	}
	if _, err := standIn.Subscribe("$JS.API.STREAM.CREATE.KV_UNANSWERED", func(*natsgo.Msg) {}); err != nil {
		t.Fatalf("take the bucket's create: %v", err)
	}
	if err := standIn.Flush(); err != nil {
		t.Fatalf("flush the stand-in's subscriptions: %v", err)
	}
	conn, err := natsgo.Connect(server.URL())
	if err != nil {
		t.Fatalf("connect the creating client: %v", err)
	}
	t.Cleanup(conn.Close)
	(&Cluster{}).CreateKeyValue(t, conn, &natsgo.KeyValueConfig{Bucket: "UNANSWERED"})
	t.Fatal("the create of a bucket nothing answers returned")
}
