package main

// relay_socket_queue.go is what a relay socket holds for its client: the frames its subscription
// delivered and it has not yet written, in a queue of its own that one writer (pump) drains in
// order, counted against the socket's own limits, its caller's share and the replica's budget
// (relaySocketPendingMsgs, relaySocketPendingBytes, relaySocketCallerPendingBytes,
// relaySocketReplicaPendingBytes).

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/nats-io/nats.go"
)

// queuedFrame is one frame a socket holds for its client: the subscription's sid, the message, and
// the bytes it is counted as, nats.go's size of the message (Msg.Size: its subject, its reply, its
// encoded headers and its data).
type queuedFrame struct {
	sid   int64
	msg   *nats.Msg
	bytes int
}

// enqueue holds msg for the client, unless it would take the socket past what one socket holds
// (pendingMsgs, pendingBytes), its caller past what all of its sockets hold together
// (callerBudget), or the replica past what all of its sockets hold together (budget): each closes
// the socket 1013 slow_consumer instead, and its client opens it again after 5 s.
func (socket *relaySocket) enqueue(sid int64, msg *nats.Msg) {
	bytes := msg.Size()
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
	if !reserveBytes(&socket.caller.buffered, bytes, sockets.callerBudget) {
		socket.queueMu.Unlock()
		socket.endSlowConsumer("its caller's sockets hold the most one caller's sockets hold together")
		return
	}
	if !reserveBytes(&sockets.buffered, bytes, sockets.budget) {
		socket.caller.buffered.Add(-int64(bytes))
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

// reserveBytes counts bytes more on count, unless that would take it past bound, and reports
// whether it did: the count never passes its bound, even for a moment.
func reserveBytes(count *atomic.Int64, bytes int, bound int64) bool {
	for {
		held := count.Load()
		if held+int64(bytes) > bound {
			return false
		}
		if count.CompareAndSwap(held, held+int64(bytes)) {
			return true
		}
	}
}

// pump is the socket's one writer of msg frames: it writes the held frames in the order they came,
// each once the one before it is written, and returns once the socket ends. A frame is held until
// its write returns, so a write blocked on a client that reads nothing stays counted; a write that
// fails drops the connection (end).
func (socket *relaySocket) pump() {
	defer close(socket.pumped)
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
			socket.settle(1, next.bytes)
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

// settle gives back frames the socket no longer holds, and their bytes, to the socket's count, its
// caller's and the replica's: the frame the pump has written, or tried to, and the frames drop
// forgot. It is the one place a held frame leaves any of the counts, as enqueue, through
// reserveBytes, is the one place each takes it (and gives the caller's back at once when the
// replica's budget refuses the frame).
func (socket *relaySocket) settle(frames, bytes int) {
	socket.queueMu.Lock()
	socket.heldMsgs -= frames
	socket.heldBytes -= bytes
	socket.queueMu.Unlock()
	socket.caller.buffered.Add(-int64(bytes))
	socket.sockets.buffered.Add(-int64(bytes))
}

// drop forgets the frames the socket holds and has not begun to write, at its end; the pump settles
// the one it is writing once its write returns.
func (socket *relaySocket) drop() {
	socket.queueMu.Lock()
	frames, bytes := len(socket.queue), 0
	for _, frame := range socket.queue {
		bytes += frame.bytes
	}
	socket.queue = nil
	socket.queueMu.Unlock()
	socket.settle(frames, bytes)
}

// held is what the socket holds for its client: frames and bytes.
func (socket *relaySocket) held() (int, int) {
	socket.queueMu.Lock()
	defer socket.queueMu.Unlock()
	return socket.heldMsgs, socket.heldBytes
}
