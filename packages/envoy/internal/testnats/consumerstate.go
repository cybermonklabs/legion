package testnats

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// servers maps the relay address of every Server start made, the host and port its URL names, to
// that Server, from start until the cleanup start registers removes it, so AwaitConsumerStored and
// AwaitNoSubscription can find the server a connection reaches.
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
// when the file has not caught up with the info by the time the shared wait gives up
// (testwait.Until), whose failure prints the last position each held.
func AwaitConsumerStored(t testing.TB, conn *natsgo.Conn, stream, consumer string) {
	t.Helper()
	path := consumerStatePath(t, conn, stream, consumer)
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("testnats: open JetStream to read consumer %s on stream %s: %v", consumer, stream, err)
	}
	testwait.Until(t, fmt.Sprintf("%s to hold the position the server reports for consumer %s", path, consumer), func() (string, bool) {
		info, err := js.ConsumerInfo(stream, consumer)
		if errors.Is(err, natsgo.ErrConsumerNotFound) || errors.Is(err, natsgo.ErrStreamNotFound) {
			return "the server holds no such consumer", true
		}
		if err != nil {
			t.Fatalf("testnats: read the info of consumer %s on stream %s: %v", consumer, stream, err)
		}
		want := positionOf(info.AckFloor.Consumer, info.AckFloor.Stream, info.Delivered.Consumer, info.Delivered.Stream, info.NumAckPending)
		stored, err := readStoredConsumer(path)
		if err != nil {
			t.Fatalf("testnats: the state file of consumer %s on stream %s: %v", consumer, stream, err)
		}
		return fmt.Sprintf("the file held %+v, the server reports %+v", stored, want), stored == want
	})
}

// positionOf is the position a consumer's info reports, as its state file holds it.
func positionOf(ackFloorConsumer, ackFloorStream, deliveredConsumer, deliveredStream uint64, pending int) storedConsumer {
	return storedConsumer{
		AckFloorConsumer:  ackFloorConsumer,
		AckFloorStream:    ackFloorStream,
		DeliveredConsumer: deliveredConsumer,
		DeliveredStream:   deliveredStream,
		Pending:           uint64(pending),
	}
}

// awaitConsumersStored waits until the state file of every consumer of stream in the global account
// that keeps its state in a file holds the position the server reports for it: CutStream's wait
// before it stops the server. A consumer's state must be on disk at the stop, or the server
// restarted after the cut recovers the consumer from an older position than the one the test read.
//
// nats-server v2.15.1's stop of a consumer writes its state only while the store marks it dirty
// (consumerFileStore.Stop, server/filestore.go:14524-14565), and waits at most 100 ms for a write of
// the flusher's under way. The flusher encodes the state, releases the store's lock, and only then
// clears that mark as it writes (flushLoop and writeState, filestore.go:13764-13777 and 14179), and
// it writes again at the soonest 100 ms after its last write. So a delivery or an ack that lands
// between that encode and that clear is in neither the write nor, for a stop in the next 100 ms,
// the stop's own: the file keeps the earlier position. A machine that runs the flusher's goroutine
// late widens the first window and a stop that follows the test's last ack closely lands in the
// second: in a run at a load of 900 to 5,000, the server restarted after the cut reconciled a
// consumer that had delivered sequence 24 as one that had delivered 21. The file is current once
// it holds what the server reports; the flusher writes again only when the state changes.
//
// It reads each consumer from the server itself (Jsz), so it needs no connection, and it skips a
// consumer in memory storage, which keeps no state file. It fails the test when the shared wait
// gives up (testwait.Until), whose failure names the files that had not caught up, or the error
// that kept the consumers from being read.
func (s *Server) awaitConsumersStored(t testing.TB, stream string) {
	t.Helper()
	srv := s.natsServer()
	if srv == nil {
		return
	}
	testwait.Until(t, fmt.Sprintf("the state files of the consumers of stream %s to hold the positions the server reports before the cut", stream), func() (string, bool) {
		behind, err := s.consumersNotStored(srv, stream)
		if err != nil {
			return fmt.Sprintf("the consumers of stream %s could not be read: %v", stream, err), false
		}
		if len(behind) == 0 {
			return "every state file holds the position the server reports", true
		}
		return strings.Join(behind, "; "), false
	})
}

// consumersNotStored names each consumer of stream in the global account whose state file does not
// hold the position srv reports for it, with both (awaitConsumersStored).
func (s *Server) consumersNotStored(srv *natsserver.Server, stream string) ([]string, error) {
	jsz, err := srv.Jsz(&natsserver.JSzOptions{Account: "$G", Streams: true, Consumer: true, Config: true})
	if err != nil {
		return nil, err
	}
	var behind []string
	for _, account := range jsz.AccountDetails {
		if account.Name != "$G" {
			continue
		}
		for _, detail := range account.Streams {
			if detail.Name != stream {
				continue
			}
			for _, info := range detail.Consumer {
				if info.Config != nil && info.Config.MemoryStorage {
					continue
				}
				path := filepath.Join(s.store, "jetstream", "$G", "streams", stream, "obs", info.Name, "o.dat")
				want := positionOf(info.AckFloor.Consumer, info.AckFloor.Stream, info.Delivered.Consumer, info.Delivered.Stream, info.NumAckPending)
				stored, err := readStoredConsumer(path)
				if err != nil {
					behind = append(behind, err.Error())
				} else if stored != want {
					behind = append(behind, fmt.Sprintf("%s holds %+v, the server reports %+v", path, stored, want))
				}
			}
		}
	}
	return behind, nil
}

// consumerStatePath is the path of consumer's state file on the server conn is connected to, in
// the account conn belongs to (AwaitConsumerStored).
func consumerStatePath(t testing.TB, conn *natsgo.Conn, stream, consumer string) string {
	t.Helper()
	server, _, account := connectedServer(t, conn)
	return filepath.Join(server.store, "jetstream", account, "streams", stream, "obs", consumer, "o.dat")
}

// ConnAccount is the account the server conn is connected to holds conn in, as that server's
// Connz names it for conn's client id, "$G" where it names none: a test of where a hub placed a
// client reads it here. It fails the test as connectedServer does.
func ConnAccount(t testing.TB, conn *natsgo.Conn) string {
	t.Helper()
	_, _, account := connectedServer(t, conn)
	return account
}

// AwaitNoSubscription waits until the server conn is connected to holds no subscription that a
// message published on subject reaches, in the account conn belongs to: the server's own list of
// subscriptions (Subsz, tested against subject), which is what it routes a publish by, and which
// names them whichever connection holds them, so the wait needs no handle on the connection that
// closed. A client that closed its connection stays a subscriber until the server reads the close:
// until its read loop meets the end of the socket (nats-server v2.15.1, server/client.go:1566-1570),
// both Connz and Subsz still list it, and a request on subject is delivered to it, into a socket no
// one reads, so the request gets neither an answer nor the no-responders status. Once the server
// reads the close, closeConnection marks the client closed (:6292) before it drops the connection
// from Connz (removeClient, :6335) and its subscriptions from Subsz (clearAccountSubs, :6344), and
// deliverMsg delivers nothing to a closed client (:4036), so from then on a request that reaches no
// other subscriber gets the no-responders status. Once this returns, a request on subject that
// conn sends reaches no subscriber that was gone by then.
//
// It fails the test when conn reaches no server this package started, or that server holds no
// connection by conn's client id, and when a subscription on subject stays by the time the shared
// wait gives up (testwait.Until), whose failure prints the subscriptions last listed.
func AwaitNoSubscription(t testing.TB, conn *natsgo.Conn, subject string) {
	t.Helper()
	_, srv, account := connectedServer(t, conn)
	testwait.Until(t, fmt.Sprintf("the server to hold no subscription on %s in account %s", subject, account), func() (string, bool) {
		listed, err := srv.Subsz(&natsserver.SubszOptions{Subscriptions: true, Account: account, Test: subject})
		if err != nil {
			t.Fatalf("testnats: list the subscriptions on %s: %v", subject, err)
		}
		held := make([]string, 0, len(listed.Subs))
		for _, sub := range listed.Subs {
			held = append(held, fmt.Sprintf("%s (connection %d)", sub.Subject, sub.Cid))
		}
		return fmt.Sprintf("%d subscriptions: %v", len(held), held), len(held) == 0
	})
}

// connectedServer is the Server this package started that conn is connected to, its running
// nats-server, and the account conn belongs to, as that server's Connz names it for conn's client
// id ($G when it names none). It fails the test when there is none of them.
func connectedServer(t testing.TB, conn *natsgo.Conn) (*Server, *natsserver.Server, string) {
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
	return server, srv, account
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
