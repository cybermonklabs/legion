package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// sendWholeBodyThenRead posts body to target over a connection of its own, sending all of it
// before it reads a byte of the answer, as WebKit and Firefox send an upload: an answer the server
// writes while body is still unread, and closes the connection after, never reaches such a client.
func sendWholeBodyThenRead(t *testing.T, address, target string, header http.Header, body []byte) (*http.Response, []byte) {
	t.Helper()
	return sendPartOfBody(t, address, target, header, len(body), body).answer()
}

// heldBackBody is a POST over a connection of its own whose head and the first part of whose body
// are sent, the rest held back until the test sends it: a body still arriving over a slow link, or
// one that has stopped.
type heldBackBody struct {
	t          *testing.T
	connection net.Conn
	reader     *bufio.Reader
	// sent is how many bytes of the request, head and body, the client has sent.
	sent int
}

// sendPartOfBody posts a body of size bytes to target, sending its head and first, and none of
// the rest. The connection gives up on the server at the shared wait's bound (testwait.Context),
// so an answer that never comes fails its read by name rather than hanging the binary.
func sendPartOfBody(t *testing.T, address, target string, header http.Header, size int, first []byte) *heldBackBody {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	t.Cleanup(func() { connection.Close() })
	bound, _ := testwait.Context(t).Deadline()
	if err := connection.SetDeadline(bound); err != nil {
		t.Fatal(err)
	}
	var head bytes.Buffer
	fmt.Fprintf(&head, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n", target, address, size)
	if err := header.Write(&head); err != nil {
		t.Fatal(err)
	}
	head.WriteString("\r\n")
	if _, err := connection.Write(head.Bytes()); err != nil {
		t.Fatalf("send the request's head: %v", err)
	}
	if _, err := connection.Write(first); err != nil {
		t.Fatalf("send %d bytes of the %d-byte body before reading the answer: %v", len(first), size, err)
	}
	return &heldBackBody{t: t, connection: connection, reader: bufio.NewReader(connection), sent: head.Len() + len(first)}
}

// finish sends the rest of the body and reads the answer.
func (b *heldBackBody) finish(rest []byte) (*http.Response, []byte) {
	b.t.Helper()
	if _, err := b.connection.Write(rest); err != nil {
		b.t.Fatalf("send the rest of the body: %v", err)
	}
	return b.answer()
}

// answer reads the first answer the server writes, an interim 1xx included.
func (b *heldBackBody) answer() (*http.Response, []byte) {
	b.t.Helper()
	response, err := http.ReadResponse(b.reader, nil)
	if err != nil {
		b.t.Fatalf("read the answer: %v", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatalf("read the answer's body: %v", err)
	}
	return response, answer
}

// closedByServer reports whether the server closed the connection: the next read meets the
// connection's end, rather than a byte or the shared wait's bound.
func (b *heldBackBody) closedByServer() bool {
	b.t.Helper()
	bound, _ := testwait.Context(b.t).Deadline()
	if err := b.connection.SetReadDeadline(bound); err != nil {
		b.t.Fatal(err)
	}
	_, err := b.reader.ReadByte()
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}

// deadlineListener is a test server's listener whose connections record the read deadline in
// effect each time a read on them ended at its deadline (deadlineConn).
type deadlineListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[string]*deadlineConn
}

func (l *deadlineListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	recorded := &deadlineConn{Conn: conn, sent: -1}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns == nil {
		l.conns = map[string]*deadlineConn{}
	}
	l.conns[conn.RemoteAddr().String()] = recorded
	return recorded, nil
}

// of is the server's end of the connection whose client end is at local, once it is accepted.
func (l *deadlineListener) of(t *testing.T, local net.Addr) *deadlineConn {
	t.Helper()
	return testwait.Until(t, "the server to accept the connection from "+local.String(), func() (*deadlineConn, bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		conn, accepted := l.conns[local.String()]
		return conn, accepted
	})
}

// deadlineConn is a server connection that keeps the read deadline last set on it and, each time a
// read ends at its deadline, records the deadline then in effect: whatever was set before it, and
// whatever replaced it before the read ended, the deadline that ended the read.
//
// It also keeps a deadline the server sets off the socket until the server has read every byte the
// client sent (sentAll), then puts that same deadline on it. A drain then reads all of a prefix the
// client sent, however slowly a loaded machine runs the read, and a deadline already passed by then
// ends the next read, the one that waits for bytes that never come, at once. Without it a 413 row's
// verdict depended on reading a megabyte before a 1 s deadline expired.
type deadlineConn struct {
	net.Conn
	mu sync.Mutex
	// current is the read deadline last set on the connection; ended is current at each read that
	// ended at its deadline, in the order they ended.
	current time.Time
	ended   []time.Time
	// read is how many bytes the server has read; sent is how many the client sent, -1 until the
	// test says (sentAll); held says current is kept off the socket until read reaches sent.
	read, sent int
	held       bool
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read += n
	if errors.Is(err, os.ErrDeadlineExceeded) {
		c.ended = append(c.ended, c.current)
	}
	c.releaseLocked()
	return n, err
}

func (c *deadlineConn) SetReadDeadline(at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = at
	if at.IsZero() || c.prefixReadLocked() {
		c.held = false
		return c.Conn.SetReadDeadline(at)
	}
	c.held = true
	return c.Conn.SetReadDeadline(time.Time{})
}

func (c *deadlineConn) SetDeadline(at time.Time) error {
	if err := c.SetReadDeadline(at); err != nil {
		return err
	}
	return c.Conn.SetWriteDeadline(at)
}

// sentAll says the client sent n bytes on the connection, and puts a held deadline on the socket
// if the server has read them all.
func (c *deadlineConn) sentAll(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = n
	c.releaseLocked()
}

// prefixReadLocked reports whether the server has read every byte the client sent.
func (c *deadlineConn) prefixReadLocked() bool {
	return c.sent >= 0 && c.read >= c.sent
}

// releaseLocked puts a held deadline on the socket once the server has read every byte the client
// sent; a read pending on the socket takes it at once.
func (c *deadlineConn) releaseLocked() {
	if c.held && c.prefixReadLocked() {
		c.held = false
		_ = c.Conn.SetReadDeadline(c.current)
	}
}

// endedUnder is the deadline in effect at each read that ended at its deadline.
func (c *deadlineConn) endedUnder() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ended)
}

// computedDeadline is a read deadline a drain computed, and the moment it began.
type computedDeadline struct{ began, deadline time.Time }

// drainDeadlines records each deadline the server's drains compute (server.drainDeadlineSet).
type drainDeadlines struct {
	mu       sync.Mutex
	computed []computedDeadline
}

func (d *drainDeadlines) set(began, deadline time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.computed = append(d.computed, computedDeadline{began: began, deadline: deadline})
}

// judge says what is wrong with the deadlines that ended conn's reads, or "" when nothing is: every
// deadline a drain computed lies limit past the moment the drain began, at least one read ended
// under such a deadline, and none ended under a deadline no drain computed. A read ended under no
// deadline, or under net/http's own of 1970 (its abort of a read it keeps in the background), is
// net/http's and is passed over. Each comparison is between values, not with a clock read when the
// test looks: a deadline replaced after the drain set it, or a drain that chose another bound, fails
// however long the machine took to run either.
func (d *drainDeadlines) judge(conn *deadlineConn, limit time.Duration) string {
	d.mu.Lock()
	computed := slices.Clone(d.computed)
	d.mu.Unlock()
	for _, drain := range computed {
		if span := drain.deadline.Sub(drain.began); span != limit {
			return fmt.Sprintf("a drain computed its deadline %v past the moment it began, want %v", span, limit)
		}
	}
	ended := conn.endedUnder()
	drained := 0
	for _, at := range ended {
		switch {
		case at.IsZero() || at.Equal(time.Unix(1, 0)):
		case slices.ContainsFunc(computed, func(drain computedDeadline) bool { return drain.deadline.Equal(at) }):
			drained++
		default:
			return fmt.Sprintf("a read ended under the deadline %s, which no drain computed (the drains computed %s)",
				at.UTC().Format(time.StampMicro), deadlineStamps(computed))
		}
	}
	if drained == 0 {
		return fmt.Sprintf("no read ended under a deadline a drain computed: %d reads ended at a deadline, and the drains computed %s",
			len(ended), deadlineStamps(computed))
	}
	return ""
}

// deadlineStamps writes each computed deadline as its clock time.
func deadlineStamps(computed []computedDeadline) string {
	stamps := make([]string, 0, len(computed))
	for _, drain := range computed {
		stamps = append(stamps, drain.deadline.UTC().Format(time.StampMicro))
	}
	return "[" + strings.Join(stamps, ", ") + "]"
}

// refusalCode is the code of a JSON refusal's body, or "" for any other body.
func refusalCode(body []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Code
}

// createClosedIssue creates an issue in the project createArtifactIssue made, and closes it.
func createClosedIssue(t *testing.T, handler http.Handler) model.Issue {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Closed"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the closed issue: status=%d body=%s", created.Code, created.Body.String())
	}
	closed := decodeBody[model.Issue](t, created)
	closeArtifactIssue(t, handler, closed.Key)
	return closed
}

// INFRA-258. An answer given before an upload's body is read -
// 409 KEY_IN_FLIGHT to a repeat whose first upload still runs, 409 ISSUE_CLOSED to an upload to a
// closed issue - reaches the client as that answer over a real connection: a client that sends its
// whole multi-megabyte body before it reads, as WebKit and Firefox do, and net/http's own client,
// which reads while it sends. Without the drain net/http writes the answer with the body unread and
// closes the connection under the client still sending, which resets it. The repeat is the first
// upload sent again, its file the same and its boundary its own, since a repeat is judged by its
// body once it has arrived (TestARepeatFoundInFlightIsJudgedOnceItsBodyHasArrived).
func TestAnEarlyAnswerToAnUploadReachesAClientThatSendsItsWholeBodyFirst(t *testing.T) {
	held := &heldFileStore{Memory: filestest.NewMemory(), reached: make(chan struct{})}
	handler, _, _ := newTestServer(t, testServerOptions{files: held})
	release, releaseOnce := heldUntilReleased(t)
	held.release = release
	served := httptest.NewServer(handler)
	t.Cleanup(served.Close)
	running := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)

	// The first upload under the key runs in this process and stops at the file store, so the key
	// stays in flight while the repeats below arrive.
	file := bytes.Repeat([]byte("a"), 24<<20)
	first := make(chan sentUpload, 1)
	go func() { first <- sendUpload(t, handler, running.Key, "running-upload", file) }()
	testwait.Receive(t, "the first upload to reach the file store", held.reached)
	t.Cleanup(func() {
		releaseOnce()
		testwait.Receive(t, "the first upload to answer once released", first)
	})

	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "big.bin"}, "big.bin",
		"application/octet-stream", file)
	if err != nil {
		t.Fatal(err)
	}
	upload := body.Bytes()
	for _, early := range []struct {
		name  string
		issue string
		key   string
		code  string
	}{
		{name: "a repeat while its first upload runs", issue: running.Key, key: "running-upload", code: contracts.IdempotencyKeyInFlight},
		{name: "a keyed upload to a closed issue", issue: closed.Key, key: "closed-upload", code: "ISSUE_CLOSED"},
		{name: "an unkeyed upload to a closed issue", issue: closed.Key, code: "ISSUE_CLOSED"},
	} {
		header := http.Header{}
		header.Set("Content-Type", bodyType)
		header.Set("X-Dispatch-User", "alice")
		if early.key != "" {
			header.Set(contracts.IdempotencyKeyHeader, early.key)
		}
		target := "/api/v1/issues/" + early.issue + "/artifacts"
		assertAnswer := func(t *testing.T, status int, answer []byte) {
			t.Helper()
			var refusal struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(answer, &refusal); status != http.StatusConflict || err != nil || refusal.Code != early.code {
				t.Fatalf("status=%d body=%q (%v), want 409 %s", status, answer, err, early.code)
			}
		}
		t.Run(early.name+", to a client that sends its whole body first", func(t *testing.T) {
			response, answer := sendWholeBodyThenRead(t, served.Listener.Addr().String(), target, header, upload)
			assertAnswer(t, response.StatusCode, answer)
		})
		t.Run(early.name+", to net/http's client", func(t *testing.T) {
			request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodPost, served.URL+target, bytes.NewReader(upload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header = header.Clone()
			response, err := served.Client().Do(request)
			if err != nil {
				t.Fatalf("post the upload: %v", err)
			}
			defer response.Body.Close()
			answer, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read the answer's body: %v", err)
			}
			assertAnswer(t, response.StatusCode, answer)
		})
	}
}

// endlessBody is a request body of n bytes that holds none of them in memory.
func endlessBody(n int64) *countingBody {
	return &countingBody{Reader: io.LimitReader(repeatedByte('a'), n)}
}

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = byte(b)
	}
	return len(p), nil
}

// INFRA-258. An early answer drains at most the route's own limit and one byte
// more (routeBody): a body that runs past it is answered as the limit answers it, 413 with the
// route's refusal and none of the early answer's headers, and the rest is never read. A body inside
// the limit is read to its end and the early answer stands, whichever layer gave it: the keyed
// layer's KEY_IN_FLIGHT, the closed-issue refusal of an upload, LOGIN_CHANGED on a JSON route. A
// 401 stands past the limit too: a caller that resolves to nobody is refused before any limit is
// judged. A client that asked to be told to continue before sending its body is answered with none
// of it read.
func TestAnEarlyAnswerDrainsTheBodyUpToTheRoutesLimit(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	open := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)
	// A first upload under the key that is still running: a row in flight, inside its lease, held
	// for the request the repeat inside the limit sends again.
	const uploadPattern = "/api/v1/issues/{key}/artifacts"
	repeated := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+open.Key+"/artifacts", endlessBody(keyedBodyMax))
	repeated.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	repeated.SetPathValue("key", open.Key)
	holdKeyInFlight(t, database, "alice", "held", repeated.URL.Path, requestDigestOf(t, repeated, uploadPattern))

	multipartBody := map[string]string{"Content-Type": "multipart/form-data; boundary=x"}
	keyed := func(key string, header map[string]string) map[string]string {
		with := map[string]string{contracts.IdempotencyKeyHeader: key}
		for name, value := range header {
			with[name] = value
		}
		return with
	}
	toOpen := "/api/v1/issues/" + open.Key + "/artifacts"
	toClosed := "/api/v1/issues/" + closed.Key + "/artifacts"
	message := "/api/v1/issues/" + open.Key + "/messages"
	otherLogin := map[string]string{"Content-Type": "application/json", contracts.DispatchLoginHeader: "bob"}
	jsonBody := map[string]string{"Content-Type": "application/json"}
	for _, early := range []struct {
		name      string
		target    string
		header    map[string]string
		anonymous bool
		size      int64
		status    int
		code      string
		wantRead  int64
	}{
		{
			name: "a repeat while its first upload runs, past the upload limit", target: toOpen, header: keyed("held", multipartBody),
			size: keyedBodyMax + 1<<20, status: http.StatusRequestEntityTooLarge, code: "CAP_EXCEEDED", wantRead: keyedBodyMax + 1,
		},
		{
			name: "a repeat while its first upload runs, inside the upload limit", target: toOpen, header: keyed("held", multipartBody),
			size: keyedBodyMax, status: http.StatusConflict, code: contracts.IdempotencyKeyInFlight, wantRead: keyedBodyMax,
		},
		{
			name: "an upload to a closed issue past the upload limit", target: toClosed, header: multipartBody,
			size: keyedBodyMax + 1<<20, status: http.StatusRequestEntityTooLarge, code: "CAP_EXCEEDED", wantRead: keyedBodyMax + 1,
		},
		{
			name: "an upload to a closed issue inside the upload limit", target: toClosed, header: multipartBody,
			size: keyedBodyMax, status: http.StatusConflict, code: "ISSUE_CLOSED", wantRead: keyedBodyMax,
		},
		{
			name: "a message pressed by another login, past the JSON limit", target: message, header: otherLogin,
			size: 2 << 20, status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE", wantRead: maxJSONRequestBytes + 1,
		},
		{
			name: "a message pressed by another login, inside the JSON limit", target: message, header: otherLogin,
			size: 512 << 10, status: http.StatusConflict, code: contracts.DispatchLoginChanged, wantRead: 512 << 10,
		},
		{
			name: "an issue from a caller that resolves to nobody, past the JSON limit", target: "/api/v1/issues", header: jsonBody, anonymous: true,
			size: 8 << 20, status: http.StatusUnauthorized, wantRead: maxJSONRequestBytes + 1,
		},
		{
			name: "an issue from a caller that resolves to nobody, inside the JSON limit", target: "/api/v1/issues", header: jsonBody, anonymous: true,
			size: 512 << 10, status: http.StatusUnauthorized, wantRead: 512 << 10,
		},
		{
			name: "an upload to a closed issue that waits to be told to continue", target: toClosed,
			header: map[string]string{"Content-Type": "multipart/form-data; boundary=x", "Expect": "100-continue"},
			size:   4 << 20, status: http.StatusConflict, code: "ISSUE_CLOSED", wantRead: 0,
		},
	} {
		t.Run(early.name, func(t *testing.T) {
			sent := endlessBody(early.size)
			request := httptest.NewRequest(http.MethodPost, early.target, sent)
			if !early.anonymous {
				request.Header.Set("X-Dispatch-User", "alice")
			}
			for name, value := range early.header {
				request.Header.Set(name, value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if code, _ := refusalOf(t, response); response.Code != early.status || (early.code != "" && code != early.code) {
				t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), early.status, early.code)
			}
			if read := sent.read.Load(); read != early.wantRead {
				t.Fatalf("read %d bytes of the %d-byte body, want %d", read, early.size, early.wantRead)
			}
			if early.status == http.StatusRequestEntityTooLarge {
				for _, name := range []string{contracts.IdempotencyKeyHeader, "Retry-After"} {
					if value := response.Header().Get(name); value != "" {
						t.Fatalf("the 413 carries the replaced answer's %s: %q", name, value)
					}
				}
				if !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("the 413's Content-Type = %q, want JSON", response.Header().Get("Content-Type"))
				}
			}
		})
	}
}

// holdKeyInFlight leaves login's key held by a first request to path that is still running: a row
// in flight, inside its lease, judged by digest.
func holdKeyInFlight(t *testing.T, database *store.Store, login, key, path, digest string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into request_keys (login, idempotency_key, request_digest, method, path)
		values ($1, $2, $3, 'POST', $4)
	`, login, key, digest, path); err != nil {
		t.Fatalf("hold the key %s in flight: %v", key, err)
	}
}

// heldFirstRun is a key's first run in TestARepeatFoundInFlightIsJudgedOnceItsBodyHasArrived: it
// says it is running, waits for its release, and answers 201, or 503 when it fails. A later run of
// the key answers at once.
type heldFirstRun struct {
	running chan struct{}
	release chan struct{}
	fails   bool
	once    sync.Once
}

// keyedAnswered is what a client read back from a keyed write.
type keyedAnswered struct {
	status int
	body   string
	err    error
}

// INFRA-258. A repeat whose key a first request still holds when
// it arrives is judged once its body has arrived, by that body's digest, as a fresh repeat is - not
// by what the key held before the body came, which a slow upload outlives. A first request that
// answered while the repeat's body arrived is replayed that answer; a request whose body differs
// from the first's is 422 KEY_REUSED, whether the first has answered or still runs; a first still
// running is 409 KEY_IN_FLIGHT; and one that released its key meanwhile (a 5xx) leaves the repeat
// to ask again, 409 KEY_IN_FLIGHT, since the repeat's body was read and not kept: that ask runs the
// request.
func TestARepeatFoundInFlightIsJudgedOnceItsBodyHasArrived(t *testing.T) {
	cases := []struct {
		name        string
		key         string
		repeat      string
		finishFirst bool
		firstFails  bool
		status      int
		code        string
	}{
		{
			name: "a repeat whose first request answers while its body arrives is replayed that answer", key: "answered",
			repeat: `{"n":1}`, finishFirst: true, status: http.StatusCreated,
		},
		{
			name: "a different request under a key whose first request answers while its body arrives", key: "answered-other",
			repeat: `{"n":2}`, finishFirst: true, status: http.StatusUnprocessableEntity, code: contracts.IdempotencyKeyReused,
		},
		{
			name: "a different request under a key whose first request still runs", key: "running-other",
			repeat: `{"n":2}`, status: http.StatusUnprocessableEntity, code: contracts.IdempotencyKeyReused,
		},
		{
			name: "a repeat whose first request still runs once its body has arrived", key: "running",
			repeat: `{"n":1}`, status: http.StatusConflict, code: contracts.IdempotencyKeyInFlight,
		},
		{
			name: "a repeat whose first request failed while its body arrived", key: "failed",
			repeat: `{"n":1}`, finishFirst: true, firstFails: true, status: http.StatusConflict, code: contracts.IdempotencyKeyInFlight,
		},
	}
	firsts := map[string]*heldFirstRun{}
	for _, judged := range cases {
		firsts[judged.key] = &heldFirstRun{running: make(chan struct{}), release: make(chan struct{}), fails: judged.firstFails}
	}
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		first := firsts[r.Header.Get(contracts.IdempotencyKeyHeader)]
		held := false
		first.once.Do(func() { held = true })
		if held {
			close(first.running)
			<-first.release
			if first.fails {
				writeError(w, "BROKEN", http.StatusServiceUnavailable, "the first run failed")
				return
			}
		}
		answerRun(run, w, r)
	})
	found := make(chan struct{}, 1)
	rig.server.repeatFoundInFlight = func() { found <- struct{}{} }
	server := httptest.NewServer(rig.handler)
	t.Cleanup(server.Close)
	const path = "/test/a/writes"

	for _, judged := range cases {
		t.Run(judged.name, func(t *testing.T) {
			first := firsts[judged.key]
			release := sync.OnceFunc(func() { close(first.release) })
			t.Cleanup(release)
			firstAnswer := make(chan keyedAnswered, 1)
			// The first request carries the shared wait's bound itself, not a timeout taken from it:
			// a goroutine that starts past the bound would take a negative timeout, which is none
			// (TestAKeyedPostBegunPastItsBoundEndsAtOnce).
			ctx := testwait.Context(t)
			go func() {
				status, body, _, err := keyedPostContext(ctx, server, path, `{"n":1}`, judged.key)
				firstAnswer <- keyedAnswered{status: status, body: body, err: err}
			}()
			testwait.Receive(t, "the first request to run", first.running)
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("X-Dispatch-User", "alice")
			header.Set(contracts.IdempotencyKeyHeader, judged.key)
			repeat := sendPartOfBody(t, server.Listener.Addr().String(), path, header, len(judged.repeat), []byte(judged.repeat[:5]))
			testwait.Receive(t, "the repeat to find its key in flight", found)
			finishFirst := func() keyedAnswered {
				t.Helper()
				release()
				answered := testwait.Receive(t, "the first request to answer once released", firstAnswer)
				if answered.err != nil {
					t.Fatalf("the first request: %v", answered.err)
				}
				return answered
			}
			var firstGot keyedAnswered
			if judged.finishFirst {
				firstGot = finishFirst()
			}
			response, body := repeat.finish([]byte(judged.repeat[5:]))
			if !judged.finishFirst {
				firstGot = finishFirst()
			}

			if response.StatusCode != judged.status || refusalCode(body) != judged.code {
				t.Fatalf("the repeat: status=%d body=%s, want %d %s", response.StatusCode, body, judged.status, judged.code)
			}
			if echoed := response.Header.Get(contracts.IdempotencyKeyHeader); echoed != judged.key {
				t.Fatalf("the repeat's answer echoes the key as %q, want %q", echoed, judged.key)
			}
			if judged.status == http.StatusConflict && response.Header.Get("Retry-After") != "1" {
				t.Fatalf("the repeat's 409 carries Retry-After %q, want 1", response.Header.Get("Retry-After"))
			}
			if judged.status == http.StatusCreated &&
				(response.Header.Get(contracts.IdempotencyReplayedHeader) != "?1" || string(body) != firstGot.body) {
				t.Fatalf("the repeat answered %s replayed=%q, want the first request's answer %s replayed",
					body, response.Header.Get(contracts.IdempotencyReplayedHeader), firstGot.body)
			}
			if judged.firstFails {
				if firstGot.status != http.StatusServiceUnavailable {
					t.Fatalf("the first request answered %d %s, want its 503", firstGot.status, firstGot.body)
				}
				status, again, header, err := keyedPostContext(ctx, server, path, judged.repeat, judged.key)
				if err != nil || status != http.StatusCreated || header.Get(contracts.IdempotencyReplayedHeader) != "" {
					t.Fatalf("the ask after the 409: %d %s replayed=%q (%v), want the request run",
						status, again, header.Get(contracts.IdempotencyReplayedHeader), err)
				}
			}
		})
	}
}

// A keyed post begun after its context's deadline ends at once, with that deadline's error. The
// first request of TestARepeatFoundInFlightIsJudgedOnceItsBodyHasArrived carries the shared bound
// (testwait.Context) from a goroutine of its own, which a machine that pauses the test can start
// past that bound; a client timeout taken from the bound (time.Until) is negative there, and a
// client whose Timeout is not positive has none, so its post waited for an answer that the binary's
// alarm ended with no failure of its own. The server here never answers: a post that waited for it
// is ended only by the wait below, at the shared bound, which names it. The server's handler ends
// with the test as well, so that its Close, a cleanup, does not wait on a post that never ended.
func TestAKeyedPostBegunPastItsBoundEndsAtOnce(t *testing.T) {
	testEnded := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-testEnded:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(testEnded) })
	past, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		_, _, _, err := keyedPostContext(past, server, "/test/a/writes", `{"n":1}`, "past-its-bound")
		ended <- err
	}()
	if err := testwait.Receive(t, "a keyed post begun past its bound to end", ended); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a keyed post begun past its bound ended with %v, want its context's deadline", err)
	}
}

// INFRA-258. A drain is bounded in time as well as in bytes: a body that stops arriving is waited
// for at most bodyDrainLimit, and then the answer goes out as it stood, with the connection closed
// after it, rather than the request held open for as long as its client cares to. Both drains: the
// one every early answer waits behind (drainBeforeAnswer), here an upload to a closed issue and a
// message pressed by another login, and the keyed layer's reading of a repeat found in flight.
//
// The drain's deadline stays on the connection once a drain ends short of the body's end, whether
// its read failed or the body ran past the route's limit: net/http itself reads on after the
// answer, up to 256 KiB more of a body whose Content-Length leaves that little unread (its discard
// before the answer's header, and its Close of the body once the handler returns), and a deadline
// lifted there let a client that stalled hold the connection open after its answer - with the
// answer withheld too, where the body ran past the limit. So a 100 KB body that stalls is closed as
// soon as its answer is out, and a body that stalls just past the limit is answered 413 within the
// deadline and closed after it, as a 4 MiB body, of which net/http reads nothing more, always was.
//
// The drain's bound is judged by value, not by how long the answer took: the server's drains report
// each deadline they compute and the moment they began (server.drainDeadlineSet), the connection
// records the deadline in effect each time a read on it ends at its deadline (deadlineConn), and
// every such deadline must be one a drain computed, drainLimit past its beginning. A deadline
// replaced after the drain set it, or a drain that chose another bound, fails however loaded the
// machine is, and a right one passes however long the machine took. A 409 answered before
// drainLimit had passed still fails too: load only lengthens a step. The connection keeps each
// deadline off its socket until the server has read every byte the client sent (deadlineConn), so
// a row that sends a megabyte past the JSON limit is answered 413 however slowly the drain reads it.
func TestADrainStopsWaitingForABodyThatStalls(t *testing.T) {
	const drainLimit = time.Second
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{files: filestest.NewMemory()})
	served.drainReadLimit = drainLimit
	drains := &drainDeadlines{}
	served.drainDeadlineSet = drains.set
	server := httptest.NewUnstartedServer(handler)
	listener := &deadlineListener{Listener: server.Listener}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	open := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)
	toOpen := "/api/v1/issues/" + open.Key + "/artifacts"
	toClosed := "/api/v1/issues/" + closed.Key + "/artifacts"
	message := "/api/v1/issues/" + open.Key + "/messages"
	holdKeyInFlight(t, database, "alice", "held", toOpen, "digest")
	holdKeyInFlight(t, database, "alice", "held-message", message, "digest")
	const multipart, jsonBody = "multipart/form-data; boundary=x", "application/json"
	const jsonLimit = int(maxJSONRequestBytes)
	opening := []byte("--x\r\n")
	pastTheLimit := bytes.Repeat([]byte("a"), jsonLimit+1)
	for _, stalled := range []struct {
		name, target, contentType, key, login string
		size                                  int
		sent                                  []byte
		status                                int
		code                                  string
	}{
		{
			name: "a 4 MiB upload to a closed issue", target: toClosed, contentType: multipart,
			size: 4 << 20, sent: opening, status: http.StatusConflict, code: "ISSUE_CLOSED",
		},
		{
			name: "a 100 KB upload to a closed issue", target: toClosed, contentType: multipart,
			size: 100_000, sent: opening, status: http.StatusConflict, code: "ISSUE_CLOSED",
		},
		{
			name: "a 4 MiB repeat whose first upload still runs", target: toOpen, contentType: multipart, key: "held",
			size: 4 << 20, sent: opening, status: http.StatusConflict, code: contracts.IdempotencyKeyInFlight,
		},
		{
			name: "a 100 KB repeat whose first upload still runs", target: toOpen, contentType: multipart, key: "held",
			size: 100_000, sent: opening, status: http.StatusConflict, code: contracts.IdempotencyKeyInFlight,
		},
		{
			name: "a message pressed by another login that stalls past the JSON limit", target: message, contentType: jsonBody,
			login: "bob", size: jsonLimit + 100_000, sent: pastTheLimit,
			status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE",
		},
		{
			name: "a repeat of a message whose first still runs that stalls past the JSON limit", target: message,
			contentType: jsonBody, key: "held-message", size: jsonLimit + 100_000, sent: pastTheLimit,
			status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE",
		},
	} {
		t.Run(stalled.name, func(t *testing.T) {
			header := http.Header{}
			header.Set("Content-Type", stalled.contentType)
			header.Set("X-Dispatch-User", "alice")
			if stalled.key != "" {
				header.Set(contracts.IdempotencyKeyHeader, stalled.key)
			}
			if stalled.login != "" {
				header.Set(contracts.DispatchLoginHeader, stalled.login)
			}
			started := time.Now()
			body := sendPartOfBody(t, server.Listener.Addr().String(), stalled.target, header, stalled.size, stalled.sent)
			listener.of(t, body.connection.LocalAddr()).sentAll(body.sent)
			response, answer := body.answer()
			elapsed := time.Since(started)
			if response.StatusCode != stalled.status || refusalCode(answer) != stalled.code {
				t.Fatalf("status=%d body=%s, want %d %s", response.StatusCode, answer, stalled.status, stalled.code)
			}
			if stalled.status == http.StatusConflict && elapsed < drainLimit {
				t.Fatalf("answered %v after the body stalled, want once the drain's second was up", elapsed)
			}
			if !response.Close {
				t.Fatal("the answer to a stalled body did not say Connection: close")
			}
			if !body.closedByServer() {
				t.Fatalf("the connection stayed open after the answer to a stalled %d-byte body", stalled.size)
			}
			if wrong := drains.judge(listener.of(t, body.connection.LocalAddr()), drainLimit); wrong != "" {
				t.Fatalf("the stalled %d-byte body's drain: %s", stalled.size, wrong)
			}
		})
	}
}

// INFRA-258. A client that asks to be told to continue before it
// sends its body (Expect: 100-continue) and is refused early gets the refusal with none of its body
// sent and no 100 Continue ahead of it, and the connection closes after it. Nothing of Dispatch
// reads such a body: the drain leaves it (drainBeforeAnswer), and the keyed layer answers a repeat
// found in flight at once rather than reading it to judge its digest. A read would have net/http
// send 100 Continue and wait for the whole body before the refusal went out.
func TestAnEarlyAnswerToAClientWaitingToContinueAsksForNoBody(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	open := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)
	toOpen := "/api/v1/issues/" + open.Key + "/artifacts"
	holdKeyInFlight(t, database, "alice", "held", toOpen, "digest")
	for _, early := range []struct {
		name, target, key, contentType, login, code string
	}{
		{
			name: "an upload to a closed issue", target: "/api/v1/issues/" + closed.Key + "/artifacts",
			contentType: "multipart/form-data; boundary=x", code: "ISSUE_CLOSED",
		},
		{
			name: "a repeat whose first upload still runs", target: toOpen, key: "held",
			contentType: "multipart/form-data; boundary=x", code: contracts.IdempotencyKeyInFlight,
		},
		{
			name: "a message pressed by another login", target: "/api/v1/issues/" + open.Key + "/messages",
			contentType: "application/json", login: "bob", code: contracts.DispatchLoginChanged,
		},
	} {
		t.Run(early.name, func(t *testing.T) {
			header := http.Header{}
			header.Set("Content-Type", early.contentType)
			header.Set("X-Dispatch-User", "alice")
			header.Set("Expect", "100-continue")
			if early.key != "" {
				header.Set(contracts.IdempotencyKeyHeader, early.key)
			}
			if early.login != "" {
				header.Set(contracts.DispatchLoginHeader, early.login)
			}
			body := sendPartOfBody(t, server.Listener.Addr().String(), early.target, header, 4<<20, nil)
			response, answer := body.answer()
			if response.StatusCode == http.StatusContinue {
				t.Fatal("the server asked for the body (100 Continue) instead of answering")
			}
			if response.StatusCode != http.StatusConflict || refusalCode(answer) != early.code {
				t.Fatalf("status=%d body=%s, want 409 %s", response.StatusCode, answer, early.code)
			}
			if !response.Close || !body.closedByServer() {
				t.Fatalf("the refusal left open a connection whose body was never sent (Connection: %q)", response.Header.Get("Connection"))
			}
		})
	}
}
