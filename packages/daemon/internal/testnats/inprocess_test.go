package testnats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// A forced-order test restarts the server it holds and reads the stream back from the same URL,
// as a server restarted on its own volume serves it: the in-process server keeps both.
func TestStartInProcessKeepsItsURLAndItsStreamAcrossARestart(t *testing.T) {
	server, url := StartInProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	js := inProcessJetStream(t, url)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "KEPT", Subjects: []string{"kept.>"}}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	if _, err := js.Publish(ctx, "kept.one", []byte("one")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	server.Stop(t)
	server.Start(t)
	if server.URL() != url {
		t.Fatalf("the restarted server's URL is %s, want %s", server.URL(), url)
	}
	js = inProcessJetStream(t, url)
	var last error
	for {
		stream, err := js.Stream(ctx, "KEPT")
		if err == nil {
			info, err := stream.Info(ctx)
			if err != nil {
				t.Fatalf("read the stream after the restart: %v", err)
			}
			if info.State.Msgs != 1 {
				t.Fatalf("the stream holds %d messages after the restart, want the 1 published before it", info.State.Msgs)
			}
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("the stream was not served again after the restart; the last answer before the deadline: %v", last)
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
}

// inProcessJetStream is a JetStream client of the server at url, named after t and closed when t
// ends.
func inProcessJetStream(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	conn, err := nats.Connect(url, nats.Name(t.Name()), nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect to the in-process server: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	return js
}
