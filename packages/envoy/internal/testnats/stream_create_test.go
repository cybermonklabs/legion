package testnats

import (
	"fmt"
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
// asked.
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
	if strings.Contains(recorder.message, "subjects") || strings.Contains(recorder.message, "storage") {
		t.Errorf("the create's failure is %q, naming a field the two configurations share", recorder.message)
	}
}
