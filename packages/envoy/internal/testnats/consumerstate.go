package testnats

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
)

// consumerStatePoll is how often AwaitConsumerStored reads the consumer's info and its state file,
// and consumerStateWait how long it waits for the two to agree: the 20 ms and 5 s the bus tests'
// waitUntil polls with, for the same reason, a state that follows a publish the server answers
// nothing to arriving a moment after it, from a server in this process.
const (
	consumerStatePoll = 20 * time.Millisecond
	consumerStateWait = 5 * time.Second
)

// servers maps the relay address of every Server start made, the host and port its URL names, to
// that Server, from start until the cleanup start registers removes it, so AwaitConsumerStored can
// find the store of the server a connection reaches.
var (
	serversMu sync.Mutex
	servers   = map[string]*Server{}
)

func registerServer(address string, server *Server) {
	serversMu.Lock()
	defer serversMu.Unlock()
	servers[address] = server
}

func unregisterServer(address string) {
	serversMu.Lock()
	defer serversMu.Unlock()
	delete(servers, address)
}

// storedConsumer is a consumer's position: its ack floor and its last delivery, each as a consumer
// and a stream sequence, and how many of its deliveries await an ack.
type storedConsumer struct {
	AckFloorConsumer, AckFloorStream   uint64
	DeliveredConsumer, DeliveredStream uint64
	Pending                            uint64
}

// AwaitConsumerStored waits until the state file of consumer on stream holds the position the
// consumer's JetStream info reports (its ack floor and last delivery, each as a consumer and a
// stream sequence, and NumAckPending), on the server conn is connected to, in the account conn
// belongs to. A test calls it right before it deletes a one-copy consumer that delivered or was
// acked since its create, or before the step that makes the product delete one, when a consumer of
// the same name is created after the delete.
//
// nats-server v2.15.1 writes that file from a goroutine of the consumer's own, its flusher
// (consumerFileStore.flushLoop, server/filestore.go:13729-13782), at most every 100 ms after a
// delivery or an ack changes the consumer's state, and its delete of the consumer does not wait
// for a write under way. writeState (filestore.go:14156-14194) checks that the store is open,
// then writes o.dat.tmp and renames it to o.dat with the store's lock released.
// consumerFileStore.delete (filestore.go:14590-14621) marks the store closed and removes the
// consumer's directory, and a write that passed the check before that goes on. A create of the
// same name right after makes the directory again and writes its own o.dat.tmp at the same path,
// from SetStarting (filestore.go:13785, called by selectStartingSeqNo, server/consumer.go:1402 and
// 6485, for a one-copy consumer only: isR1, consumer.go:6482-6488). The late write's rename takes
// the create's file, and the create fails: "rename .../obs/<name>/o.dat.tmp .../obs/<name>/o.dat:
// no such file or directory" (code 500, err_code 10012). Once the file holds the position the
// info reports, the write that carried it has been renamed, and the flusher writes again only
// when the consumer's state changes.
//
// The file is <store>/jetstream/<account>/streams/<stream>/obs/<consumer>/o.dat, the account being
// the one the server's Connz names for conn's client id ($G when it names none). It starts with
// magic 22 and version 2, then the ack floor's consumer and stream sequence, the last delivery's,
// and the count of pending messages, each an unsigned varint (encodeConsumerState,
// server/store.go:478-504).
//
// It waits only for state the server already holds: an ack still on its way when the info is read
// changes the state after the wait returns, so a caller first waits for the acks it sent to be
// taken. And it compares those five fields alone, so a write that changes none of them - a
// progress reply or a nak with a delay, which only restamp a pending message
// (consumer.progressUpdate and processNak, server/consumer.go:2885 and 3250) - is not waited for.
//
// A consumer the stream does not hold, or a stream that is not there, returns at once: there is no
// write to wait for. It fails the test when conn reaches no server this package started and
// registered, when the server holds no connection by conn's client id, when the file is missing
// or does not read that way (a consumer in memory storage, another format), naming the path, and
// when the file has not caught up with the info within consumerStateWait.
func AwaitConsumerStored(t testing.TB, conn *natsgo.Conn, stream, consumer string) {
	t.Helper()
	path := consumerStatePath(t, conn, stream, consumer)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("testnats: open JetStream to read consumer %s on stream %s: %v", consumer, stream, err)
	}
	deadline := time.Now().Add(consumerStateWait)
	for {
		info, err := js.ConsumerInfo(stream, consumer)
		if errors.Is(err, natsgo.ErrConsumerNotFound) || errors.Is(err, natsgo.ErrStreamNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("testnats: read the info of consumer %s on stream %s: %v", consumer, stream, err)
		}
		want := storedConsumer{
			AckFloorConsumer:  info.AckFloor.Consumer,
			AckFloorStream:    info.AckFloor.Stream,
			DeliveredConsumer: info.Delivered.Consumer,
			DeliveredStream:   info.Delivered.Stream,
			Pending:           uint64(info.NumAckPending),
		}
		stored, err := readStoredConsumer(path)
		if err != nil {
			t.Fatalf("testnats: the state file of consumer %s on stream %s: %v", consumer, stream, err)
		}
		if stored == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("testnats: waited %s for %s to hold the position the server reports for consumer %s; it held %+v, the server reports %+v", consumerStateWait, path, consumer, stored, want)
		}
		time.Sleep(consumerStatePoll)
	}
}

// consumerStatePath is the path of consumer's state file on the server conn is connected to, in
// the account conn belongs to (AwaitConsumerStored).
func consumerStatePath(t testing.TB, conn *natsgo.Conn, stream, consumer string) string {
	t.Helper()
	server, account := connPlace(t, conn)
	return filepath.Join(server.store, "jetstream", account, "streams", stream, "obs", consumer, "o.dat")
}

// ConnAccount is the account the server conn is connected to holds conn in, as that server's
// Connz names it for conn's client id, "$G" where it names none: a test of where a hub placed a
// client reads it here. It fails the test as connPlace does.
func ConnAccount(t testing.TB, conn *natsgo.Conn) string {
	t.Helper()
	_, account := connPlace(t, conn)
	return account
}

// connPlace is the server conn is connected to, and the account that server holds conn in, as its
// Connz names it for conn's client id ("$G" when it names none). It fails the test when conn
// reaches no server this package started and registered, and when the server holds no connection
// by conn's client id.
func connPlace(t testing.TB, conn *natsgo.Conn) (*Server, string) {
	t.Helper()
	connected := conn.ConnectedUrlRedacted()
	parsed, err := url.Parse(connected)
	if err != nil || parsed.Host == "" {
		t.Fatalf("testnats: the connection is connected to %q, which names no server", connected)
	}
	serversMu.Lock()
	server := servers[parsed.Host]
	serversMu.Unlock()
	if server == nil {
		t.Fatalf("testnats: no server this package started is at %s, the address the connection is connected to", parsed.Host)
	}
	srv := server.natsServer()
	if srv == nil {
		t.Fatalf("testnats: the server at %s is stopped", parsed.Host)
	}
	cid, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("testnats: read the connection's client id: %v", err)
	}
	connz, err := srv.Connz(&natsserver.ConnzOptions{CID: cid, Username: true})
	if err != nil {
		t.Fatalf("testnats: read connection %d from the server at %s: %v", cid, parsed.Host, err)
	}
	if len(connz.Conns) != 1 {
		t.Fatalf("testnats: the server at %s holds %d open connections with client id %d, want the connection's one", parsed.Host, len(connz.Conns), cid)
	}
	// Connz leaves the account out for a client of the global account.
	account := connz.Conns[0].Account
	if account == "" {
		account = "$G"
	}
	return server, account
}

// readStoredConsumer reads the position in the consumer state file at path (AwaitConsumerStored).
// A file that is not there, or that does not read as that format, is an error naming it.
func readStoredConsumer(path string) (storedConsumer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storedConsumer{}, err
	}
	if len(data) < 2 || data[0] != 22 || data[1] != 2 {
		return storedConsumer{}, fmt.Errorf("%s does not start with magic 22 and version 2: % x", path, data[:min(len(data), 2)])
	}
	var fields [5]uint64
	rest := data[2:]
	for i := range fields {
		value, n := binary.Uvarint(rest)
		if n <= 0 {
			return storedConsumer{}, fmt.Errorf("%s: field %d of 5 is not an unsigned varint", path, i+1)
		}
		fields[i], rest = value, rest[n:]
	}
	return storedConsumer{
		AckFloorConsumer:  fields[0],
		AckFloorStream:    fields[1],
		DeliveredConsumer: fields[2],
		DeliveredStream:   fields[3],
		Pending:           fields[4],
	}, nil
}
