// Forced-order hooks, shared by every hooked nats-server version. packages/envoy/scripts/forced-order.sh
// adds this file to the server package of a copy of the module, beside the version's
// zz_measure_write.go and server.patch, which puts the calls into the server's own code. Not part
// of nats-server: MeasureHook is nil unless a test of internal/testnats's hooks.go sets it, and
// every call below then returns at once.
package server

import (
	"encoding/json"
	"sync/atomic"
)

// MeasureEvent is one instrumented point inside the server.
type MeasureEvent struct {
	// Point names the instrumented point, one of the names hooks.go declares.
	Point string
	// Gen is the consumer file store's generation, one per ConsumerStore call; 0 if none.
	Gen uint64
	// Name is the consumer's name, at a file store point and at meta.apply.
	Name string
	// Store is the stream's store directory at a file store point, and the server's own store
	// directory (Options.StoreDir) at a server point.
	Store string
	// Path is the file or directory involved at a file store point. At api.worker it says
	// whether the request was served from the queue ("queued") or in its client's read loop
	// ("inline"): v2.10.29 serves a request from a client of the server itself in that client's
	// read loop, and queues only one that came over a route, a gateway or a leafnode.
	Path string
	// State is the encoded consumer state involved (the bytes being written), if any.
	State []byte
	// Seq is the sequence involved, if any: at stream.apply, the stream sequence the entry stores.
	Seq uint64
	// Err is the error result, if any.
	Err error
	// Server is the server's name, at a server point.
	Server string
	// ServerID is the server's ID at server.shutdown and server.down: nats-server makes a new one
	// at each start, so it names one run of a server where its name and its store do not.
	ServerID string
	// Account is the account's name, at a server point.
	Account string
	// Subject is the request's subject at api.worker, the message's at client.publish and
	// stream.apply.
	Subject string
	// Stream is the stream's name at stream.apply and meta.apply.
	Stream string
	// Client is the publishing connection's name at client.publish, and Kind its kind (CLIENT,
	// JETSTREAM, SYSTEM, ACCOUNT).
	Client string
	Kind   string
	// Recovering is set at stream.apply and meta.apply while the server replays its log at start.
	Recovering bool
}

// MeasureHook is called synchronously at each instrumented point. A hook that blocks holds
// the calling goroutine at that point, with whatever locks the original code holds there.
var MeasureHook atomic.Pointer[func(*MeasureEvent)]

var measureGen atomic.Uint64

func measure(ev *MeasureEvent) {
	if h := MeasureHook.Load(); h != nil {
		(*h)(ev)
	}
}

// MeasureDecodeConsumerState decodes an o.dat state file with the server's own decoder.
func MeasureDecodeConsumerState(buf []byte) (*ConsumerState, error) {
	return decodeConsumerState(buf)
}

func (o *consumerFileStore) mev(point string, buf []byte, seq uint64, err error) *MeasureEvent {
	var store string
	if o.fs != nil {
		store = o.fs.fcfg.StoreDir
	}
	return &MeasureEvent{Point: point, Gen: o.mgen, Name: o.name, Store: store, Path: o.ifn, State: buf, Seq: seq, Err: err}
}

// measureAPI reports a JetStream API request the server has taken and not yet served, with the
// account that sent it, which the request's Nats-Request-Info header names as the API's handlers
// read it: the account the request reached the API in is the system account's.
func measureAPI(s *Server, c *client, rmsg []byte, subject, how, point string) {
	if MeasureHook.Load() == nil {
		return
	}
	var account string
	hdr, _ := c.msgParts(rmsg)
	if raw := sliceHeader(ClientInfoHdr, hdr); len(raw) > 0 {
		var ci ClientInfo
		if json.Unmarshal(raw, &ci) == nil {
			account = ci.Account
		}
	}
	measure(&MeasureEvent{Point: point, Server: s.Name(), Store: s.getOpts().StoreDir, Account: account, Subject: subject, Path: how})
}

// measurePublish reports a publish that passed its permission checks and is not yet matched
// against any subscription.
func measurePublish(c *client, acc *Account, point string) {
	if MeasureHook.Load() == nil {
		return
	}
	s := c.srv
	measure(&MeasureEvent{Point: point, Server: s.Name(), Store: s.getOpts().StoreDir, Account: acc.Name,
		Subject: string(c.pa.subject), Client: c.opts.Name, Kind: c.kindString()})
}

// measureApply reports one committed stream entry its server is about to store.
func measureApply(s *Server, mset *stream, subject string, seq uint64, recovering bool, point string) {
	if MeasureHook.Load() == nil {
		return
	}
	measure(&MeasureEvent{Point: point, Server: s.Name(), Store: s.getOpts().StoreDir, Account: mset.accName(),
		Stream: mset.name(), Subject: subject, Seq: seq, Recovering: recovering})
}

// measureMeta reports one consumer assignment the meta layer is about to apply on this server
// (applyMetaEntries), before the server creates or updates the consumer it assigns.
func measureMeta(s *Server, ca *consumerAssignment, recovering bool, point string) {
	if MeasureHook.Load() == nil {
		return
	}
	measure(&MeasureEvent{Point: point, Server: s.Name(), Store: s.getOpts().StoreDir, Account: ca.Client.serviceAccount(),
		Stream: ca.Stream, Name: ca.Name, Recovering: recovering})
}

// measureServer reports a point of the server's own life: server.shutdown when its Shutdown
// begins, before it stops anything, and server.down once every goroutine it waits for has ended.
// v2.10.29 can report server.shutdown more than once for one run, from concurrent Shutdown calls,
// and a late report can follow server.down: the run's ID is what lets the harness ignore it.
func measureServer(s *Server, point string) {
	if MeasureHook.Load() == nil {
		return
	}
	measure(&MeasureEvent{Point: point, Server: s.Name(), ServerID: s.ID(), Store: s.getOpts().StoreDir})
}
