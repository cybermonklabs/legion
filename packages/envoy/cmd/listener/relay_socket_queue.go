package main

// relay_socket_queue.go is what a relay socket holds for its client: the frames its subscription
// delivered and it has not yet written, in a queue of its own that one writer (pump) drains in
// order, counted against the socket's own limits and the replica's budget
// (relaySocketPendingMsgs, relaySocketPendingBytes, relaySocketReplicaPendingBytes).

import (
	"encoding/base64"
	"errors"
	"log/slog"

	"github.com/nats-io/nats.go"
)

// queuedFrame is one frame a socket holds for its client: the subscription's sid, the message, and
// the bytes it is counted as (relayFrameBytes).
type queuedFrame struct {
	sid   int64
	msg   *nats.Msg
	bytes int
}

// relayFrameBytes is what a held message is counted as: its subject, its reply, its headers and
// its data, as nats.go counts a pending message.
func relayFrameBytes(msg *nats.Msg) int {
	bytes := len(msg.Subject) + len(msg.Reply) + len(msg.Data)
	for name, values := range msg.Header {
		for _, value := range values {
			bytes += len(name) + len(value)
		}
	}
	return bytes
}

// enqueue holds msg for the client, unless it would take the socket past what one socket holds
// (pendingMsgs, pendingBytes) or the replica past what all of its sockets hold together (budget):
// either closes the socket 1013 slow_consumer instead, and its client opens it again after 5 s.
func (socket *relaySocket) enqueue(sid int64, msg *nats.Msg) {
	bytes := relayFrameBytes(msg)
	sockets := socket.sockets
	socket.queueMu.Lock()
	if socket.ended.Load() {
		socket.queueMu.Unlock()
		return
	}
	if socket.heldMsgs+1 > sockets.pendingMsgs || socket.heldBytes+bytes > sockets.pendingBytes {
		socket.queueMu.Unlock()
		socket.endSlowConsumer("it holds the most a socket holds for its client")
		return
	}
	if !sockets.reserve(bytes) {
		socket.queueMu.Unlock()
		socket.endSlowConsumer("the replica's sockets hold the most they hold together")
		return
	}
	socket.queue = append(socket.queue, queuedFrame{sid: sid, msg: msg, bytes: bytes})
	socket.heldMsgs++
	socket.heldBytes += bytes
	socket.queueMu.Unlock()
	select {
	case socket.wake <- struct{}{}:
	default:
	}
}

// reserve counts bytes more as held by the replica's sockets, unless that would take the count past
// budget, and reports whether it did: the count never passes the budget, even for a moment.
func (s *relaySockets) reserve(bytes int) bool {
	for {
		held := s.buffered.Load()
		if held+int64(bytes) > s.budget {
			return false
		}
		if s.buffered.CompareAndSwap(held, held+int64(bytes)) {
			return true
		}
	}
}

// pump is the socket's one writer of msg frames: it writes the held frames in the order they came,
// each once the one before it is written, and returns once the socket ends. A frame is held until
// its write returns, so a write blocked on a client that reads nothing stays counted; a write that
// fails drops the connection (end).
func (socket *relaySocket) pump() {
	for {
		select {
		case <-socket.done:
			return
		case <-socket.wake:
		}
		for {
			socket.queueMu.Lock()
			if socket.ended.Load() || len(socket.queue) == 0 {
				socket.queueMu.Unlock()
				break
			}
			next := socket.queue[0]
			socket.queue[0] = queuedFrame{}
			socket.queue = socket.queue[1:]
			socket.queueMu.Unlock()

			socket.remember(next.msg.Reply)
			headers := next.msg.Header
			if headers == nil {
				headers = nats.Header{}
			}
			err := socket.write(relayMsgFrame{
				Op: "msg", SID: next.sid, Subject: next.msg.Subject, Reply: next.msg.Reply, Headers: headers,
				Data: base64.StdEncoding.EncodeToString(next.msg.Data),
			})
			socket.settle(next.bytes)
			if err != nil {
				if !errors.Is(err, errRelaySocketEnded) {
					socket.sockets.logger.Warn("listener: the relay socket could not write a frame; it is closed",
						slog.String("session_id", socket.session), slog.String("error", err.Error()))
					socket.end(0, "")
				}
				return
			}
		}
	}
}

// settle forgets a frame the pump has written, or tried to: the socket and the replica hold it no
// longer.
func (socket *relaySocket) settle(bytes int) {
	socket.queueMu.Lock()
	socket.heldMsgs--
	socket.heldBytes -= bytes
	socket.queueMu.Unlock()
	socket.sockets.buffered.Add(-int64(bytes))
}

// held is what the socket holds for its client: frames and bytes.
func (socket *relaySocket) held() (int, int) {
	socket.queueMu.Lock()
	defer socket.queueMu.Unlock()
	return socket.heldMsgs, socket.heldBytes
}
