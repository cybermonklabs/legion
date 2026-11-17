package testnats

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// relay holds the port a server's URL names for the test's life and joins each connection it
// accepts to wherever that server listens at the moment the connection arrives. The server's own
// port is one it picks at each start, which a restart therefore changes and the relay hides; a
// connection made while the server is down is closed at once, as a refused dial would be, and one
// the server drops is dropped on the client's side too.
//
// The relay of a server's client port (newRelay's clientPort) can be held: a hold stops each
// client's bytes to the server at a message boundary while the server's bytes keep reaching the
// client, and a barrier then sends the server a PING of the relay's own on each connection and
// waits for its PONG, which is how Server.Reload keeps a reload clear of client traffic. A gate's
// relay and a cluster's route relays cannot be held.
type relay struct {
	listener   net.Listener
	target     func() (string, error)
	clientPort bool
	done       chan struct{}
	closing    sync.Once

	mu sync.Mutex
	// gate is nil while the relay moves bytes, and open while it is frozen: every copy waits for
	// it to close, which thawing does.
	gate chan struct{}
	// holds is how many holds are in force and closed whether the relay has closed; moved is
	// signalled whenever either changes.
	holds  int
	closed bool
	moved  *sync.Cond
	// joins are the connections of a client port's relay that the server has not yet closed.
	joins map[*joined]struct{}
}

// joined is one connection a client port's relay joins to the server: upstream is the relay's own
// connection to the server. The rest is guarded by the relay's mu, but for received, which only
// the copy from the server reads.
type joined struct {
	upstream net.Conn
	// sent is the client's stream as far as the relay has written it to the server, and writing
	// whether a write of it to the server is under way.
	sent    protocolStream
	writing bool
	// injected is how many PINGs of its own the relay has sent the server on the connection, and
	// pongs how many PONGs the server has sent on it; drop holds the numbers of the PONGs that
	// answer the relay's own PINGs, which the client never sent and never sees.
	injected int64
	pongs    int64
	drop     map[int64]bool
	// received is the server's stream to the client.
	received protocolStream
}

// port is the local port of the relay's connection to the server, the port the server reports
// the client by.
func (j *joined) port() int { return j.upstream.LocalAddr().(*net.TCPAddr).Port }

// newRelay starts a relay that serves listener and dials target for each connection it accepts;
// clientPort says it is the relay of a server's client port, which a hold can stop.
func newRelay(listener net.Listener, target func() (string, error), clientPort bool) *relay {
	r := &relay{listener: listener, target: target, clientPort: clientPort, done: make(chan struct{}), joins: map[*joined]struct{}{}}
	r.moved = sync.NewCond(&r.mu)
	go r.serve()
	return r
}

// serve joins each connection the relay accepts to the server, until the relay closes.
func (r *relay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.join(client)
	}
}

// join copies client and the server's connection into each other until either side ends, then
// closes both. One direction runs on this goroutine and the other on one of its own, and whichever
// ends first closes both connections, which ends the other. The relay of a client port ends a
// connection as joinClient says instead.
func (r *relay) join(client net.Conn) {
	address, err := r.target()
	if err != nil {
		_ = client.Close()
		return
	}
	upstream, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		_ = client.Close()
		return
	}
	if r.clientPort {
		r.joinClient(client, upstream)
		return
	}
	ended := sync.OnceFunc(func() {
		_ = upstream.Close()
		_ = client.Close()
	})
	go func() {
		defer ended()
		r.stream(upstream, client)
	}()
	defer ended()
	r.stream(client, upstream)
}

// joinClient is join on the relay of a client port. The connection is the relay's until the server
// closes it: a client that ends its side has the server read the end of the stream after
// everything the client sent, as a client connected to the server itself does, and the server's
// replies are read and dropped until the server closes. What the server sends the client passes
// through j.received, which drops the PONGs that answer the relay's own PINGs (barrier).
func (r *relay) joinClient(client, upstream net.Conn) {
	j := &joined{upstream: upstream, drop: map[int64]bool{}, received: protocolStream{fromServer: true}}
	r.mu.Lock()
	r.joins[j] = struct{}{}
	r.mu.Unlock()
	defer func() {
		_ = upstream.Close()
		_ = client.Close()
		r.mu.Lock()
		delete(r.joins, j)
		r.mu.Unlock()
	}()
	go r.streamClient(j, upstream, client)
	buffer := make([]byte, 32*1024)
	delivering := true
	for {
		r.pass()
		read, err := upstream.Read(buffer)
		if read > 0 {
			passed := j.received.filter(buffer[:read], func() bool { return r.pong(j) })
			if len(passed) > 0 && delivering {
				r.pass()
				if _, err := client.Write(passed); err != nil {
					delivering = false
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// pong counts a PONG the server sent on j and reports whether it answers one of the relay's own
// PINGs, which the client is not to see.
func (r *relay) pong(j *joined) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	j.pongs++
	own := j.drop[j.pongs]
	delete(j.drop, j.pongs)
	return own
}

// streamClient copies what the client sends into the server's connection, as stream does, under
// the relay's holds (enter), recording in j.sent what each write carried. Once the client ends its
// side, the server's connection is closed for writing, which the server reads as the end of the
// client's stream.
func (r *relay) streamClient(j *joined, upstream, client net.Conn) {
	buffer := make([]byte, 32*1024)
	for {
		r.pass()
		read, err := client.Read(buffer)
		for rest := buffer[:read]; len(rest) > 0; {
			r.pass()
			written, werr := upstream.Write(rest[:r.enter(j, rest)])
			r.leave(j, rest[:written])
			if werr != nil {
				return
			}
			rest = rest[written:]
		}
		if err != nil {
			if half, ok := upstream.(interface{ CloseWrite() error }); ok {
				_ = half.CloseWrite()
			}
			return
		}
	}
}

// stream copies src into dst, a frozen relay moving nothing: the read and the write each wait for
// the thaw, so the bytes stay where they were and both connections stay open.
func (r *relay) stream(dst io.Writer, src io.Reader) {
	buffer := make([]byte, 32*1024)
	for {
		r.pass()
		read, err := src.Read(buffer)
		if read > 0 {
			r.pass()
			if _, err := dst.Write(buffer[:read]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pass waits while the relay is frozen, and returns at once once it is thawed or closed.
func (r *relay) pass() {
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-r.done:
	}
}

// freeze stops the relay moving bytes and returns the func that starts it again, which runs once
// however often it is called.
func (r *relay) freeze() func() {
	r.mu.Lock()
	if r.gate == nil {
		r.gate = make(chan struct{})
	}
	gate := r.gate
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		owned := r.gate == gate
		if owned {
			r.gate = nil
		}
		r.mu.Unlock()
		// A second freeze while this one held the gate shares it: only the thaw that still owns
		// the gate closes it, so the other returns rather than closing it twice.
		if !owned {
			return
		}
		close(gate)
	})
}

// hold stops the relay of a client port writing what its clients send to the server, each
// connection at the end of a protocol operation, and returns once every connection has stopped
// there, with the func that lets the writes through again, which runs once however often it is
// called. Holds nest: the relay writes again once every hold has been released. Bytes a client
// sends meanwhile wait in the relay, as a freeze keeps them, while the server's own bytes keep
// reaching the client. Within connectTimeout, hold fails on a frozen relay, whose PONGs barrier
// could not read, on a connection the relay cannot read as NATS protocol, and on one that stays
// part-way through an operation; the func it returns must run all the same.
func (r *relay) hold() (func(), error) {
	r.mu.Lock()
	r.holds++
	frozen := r.gate != nil
	r.mu.Unlock()
	release := sync.OnceFunc(func() {
		r.mu.Lock()
		r.holds--
		r.moved.Broadcast()
		r.mu.Unlock()
	})
	if frozen {
		return release, errors.New("the relay is frozen, so the server's answer to the relay's PING could not reach it")
	}
	return release, r.await("every client's bytes to the server to stop at the end of an operation", func() (bool, error) {
		for j := range r.joins {
			if j.sent.broken != nil {
				return false, fmt.Errorf("the relay cannot read what the client on port %d sends as NATS protocol: %w", j.port(), j.sent.broken)
			}
			if j.writing || !j.sent.atBoundary() {
				return false, nil
			}
		}
		return true, nil
	})
}

// enter waits while a hold is in force and j's stream to the server is at the end of an operation,
// unless the relay has closed, then marks a write of j's as under way until leave, and returns how
// much of data to write: all of it with no hold in force, or else the bytes that end the operation
// the stream is part-way through, so a hold stops it at the end of that operation.
func (r *relay) enter(j *joined, data []byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.holds > 0 && !r.closed && j.sent.atBoundary() {
		r.moved.Wait()
	}
	j.writing = true
	if r.holds > 0 && !r.closed {
		return j.sent.toBoundary(data)
	}
	return len(data)
}

// leave ends the write to the server that enter began, which carried written.
func (r *relay) leave(j *joined, written []byte) {
	r.mu.Lock()
	j.sent.wrote(written)
	j.writing = false
	r.mu.Unlock()
}

// barrier sends the server a PING of the relay's own on each connection whose client has sent its
// CONNECT, with a hold in force, and returns once the server has answered each with its PONG or
// closed the connection, within connectTimeout. The server reads a connection in order and answers
// a PING only once it has processed everything before it, so every request a client sent before
// the hold has been processed: the server's unlocked read of a service import's account
// (Server.Reload), and the importing account's lock it takes after that read, are behind it, which
// orders them before a reload's write under that lock. The client never sees that PONG.
func (r *relay) barrier() error {
	type ping struct {
		j      *joined
		number int64
	}
	var pings []ping
	r.mu.Lock()
	for j := range r.joins {
		if !j.sent.connected {
			continue
		}
		j.injected++
		number := j.sent.pings + j.injected
		j.drop[number] = true
		pings = append(pings, ping{j, number})
	}
	r.mu.Unlock()
	for _, p := range pings {
		// A write that fails is a connection the server has closed, which the wait below sees.
		_, _ = p.j.upstream.Write([]byte("PING\r\n"))
	}
	return r.await("the server's answer to the relay's PING on every connection", func() (bool, error) {
		for _, p := range pings {
			if _, open := r.joins[p.j]; open && p.j.pongs < p.number {
				return false, nil
			}
		}
		return true, nil
	})
}

// await waits, within connectTimeout, for settled, called under the relay's mu, to say the
// relay's connections are where it wants them, and returns settled's error, or one naming what it
// waited for if the time ran out. A relay that has closed has nothing to wait for.
func (r *relay) await(what string, settled func() (bool, error)) error {
	deadline := time.Now().Add(connectTimeout)
	for {
		r.mu.Lock()
		done, err := settled()
		closed := r.closed
		r.mu.Unlock()
		if done || closed || err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s for %s", connectTimeout, what)
		}
		time.Sleep(barrierPoll)
	}
}

// Close frees the port the relay held and releases every copy a freeze or a hold holds, which then
// runs on as a free copy does: a copy keeps running until one of its endpoints ends.
func (r *relay) Close() {
	r.closing.Do(func() {
		close(r.done)
		_ = r.listener.Close()
		r.mu.Lock()
		r.closed = true
		r.moved.Broadcast()
		r.mu.Unlock()
	})
}

// maxControlLine bounds the control line a protocolStream keeps while it waits for the line's end.
// A message line past nats-server's max_control_line (4,096 bytes unless a configuration raises
// it) closes the client, so a line past a mebibyte is a stream that is not NATS protocol.
const maxControlLine = 1 << 20

// protocolStream reads one direction of a connection to a NATS server as the reading side's parser
// does: a control line ends at its LF, and a message line (PUB or HPUB from a client, MSG or HMSG
// from the server) is followed by as many bytes as its last field says and a CRLF. It is shown the
// stream in order, in pieces split anywhere.
type protocolStream struct {
	// fromServer says the stream is the server's to the client.
	fromServer bool
	// line is the control line read so far, and payload how many bytes of the current message,
	// its CRLF among them, are still to come: 0 while a control line is being read.
	line    []byte
	payload int
	// broken is why the stream stopped reading as NATS protocol.
	broken error
	// connected says a client's stream has sent its CONNECT, and pings counts its PINGs.
	connected bool
	pings     int64
}

// atBoundary reports whether the stream so far ends between two operations.
func (p *protocolStream) atBoundary() bool {
	return p.broken == nil && len(p.line) == 0 && p.payload == 0
}

// toBoundary is how many bytes of data, the stream's next bytes, end the operation the stream is
// in: all of data when that end is beyond it, or when the stream cannot be read.
func (p *protocolStream) toBoundary(data []byte) int {
	if p.broken != nil {
		return len(data)
	}
	if p.payload > 0 {
		return min(p.payload, len(data))
	}
	end := bytes.IndexByte(data, '\n')
	if end < 0 {
		return len(data)
	}
	line := append(p.line[:len(p.line):len(p.line)], data[:end+1]...)
	payload, err := payloadAfter(string(line), p.fromServer)
	if err != nil {
		return len(data)
	}
	return min(end+1+payload, len(data))
}

// wrote records the next piece of a client's stream, which reached the server.
func (p *protocolStream) wrote(piece []byte) {
	for len(piece) > 0 && p.broken == nil {
		if p.payload > 0 {
			taken := min(p.payload, len(piece))
			p.payload -= taken
			piece = piece[taken:]
			continue
		}
		rest, whole := p.read(piece)
		piece = rest
		if !whole {
			return
		}
		switch operation(p.line) {
		case "CONNECT":
			p.connected = true
		case "PING":
			p.pings++
		}
		p.payload, p.broken = payloadAfter(string(p.line), p.fromServer)
		p.line = p.line[:0]
	}
}

// filter returns what of piece, the server's next bytes to the client, the client is to get: all
// of it but each PONG that drop, asked of every PONG in order, says to drop. A control line is
// passed on once it is whole, so no PONG is passed on in part; once the stream cannot be read,
// everything is passed on as it comes.
func (p *protocolStream) filter(piece []byte, drop func() bool) []byte {
	var passed []byte
	for len(piece) > 0 {
		if p.broken != nil {
			return append(passed, piece...)
		}
		if p.payload > 0 {
			taken := min(p.payload, len(piece))
			passed = append(passed, piece[:taken]...)
			p.payload -= taken
			piece = piece[taken:]
			continue
		}
		rest, whole := p.read(piece)
		piece = rest
		if !whole {
			if p.broken != nil {
				passed = append(passed, p.line...)
				p.line = nil
			}
			return passed
		}
		if operation(p.line) != "PONG" || !drop() {
			passed = append(passed, p.line...)
			p.payload, p.broken = payloadAfter(string(p.line), p.fromServer)
		}
		p.line = p.line[:0]
	}
	return passed
}

// read adds piece to the control line being read, up to the line's LF, and returns the rest of
// piece and whether the line is now whole.
func (p *protocolStream) read(piece []byte) ([]byte, bool) {
	end := bytes.IndexByte(piece, '\n')
	if end < 0 {
		p.line = append(p.line, piece...)
		if len(p.line) > maxControlLine {
			p.broken = fmt.Errorf("a control line runs past %d bytes", maxControlLine)
		}
		return nil, false
	}
	p.line = append(p.line, piece[:end+1]...)
	return piece[end+1:], true
}

// operation is the operation a control line names, in upper case, as the server reads it.
func operation(line []byte) string {
	fields := bytes.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(string(fields[0]))
}

// payloadAfter is how many bytes follow the control line line before the next one: a message's
// payload and its CRLF after a PUB or an HPUB from a client, or a MSG or an HMSG from the server,
// and none after any other operation.
func payloadAfter(line string, fromServer bool) (int, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, nil
	}
	arguments := len(fields) - 1
	switch op := strings.ToUpper(fields[0]); {
	case !fromServer && op == "PUB":
		// PUB <subject> [reply-to] <#bytes>
		if arguments != 2 && arguments != 3 {
			return 0, fmt.Errorf("a PUB line has %d arguments: %q", arguments, line)
		}
	case !fromServer && op == "HPUB":
		// HPUB <subject> [reply-to] <#header bytes> <#total bytes>
		if arguments != 3 && arguments != 4 {
			return 0, fmt.Errorf("an HPUB line has %d arguments: %q", arguments, line)
		}
	case fromServer && op == "MSG":
		// MSG <subject> <sid> [reply-to] <#bytes>
		if arguments != 3 && arguments != 4 {
			return 0, fmt.Errorf("a MSG line has %d arguments: %q", arguments, line)
		}
	case fromServer && op == "HMSG":
		// HMSG <subject> <sid> [reply-to] <#header bytes> <#total bytes>
		if arguments != 4 && arguments != 5 {
			return 0, fmt.Errorf("an HMSG line has %d arguments: %q", arguments, line)
		}
	default:
		return 0, nil
	}
	size, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || size < 0 {
		return 0, fmt.Errorf("a message line's size is not a count of bytes: %q", line)
	}
	return size + len("\r\n"), nil
}
