package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rereadSpare is what sessionReread leaves of the test binary's deadline when it gives up: the
// margin testwait's own waits leave, for the failure and the test's cleanups to run in.
const rereadSpare = 30 * time.Second

// sessionReread asks the broker to reread name as a session, the caller the reread route answers
// once the reread is done, with whether the broker now serves the name, why not and as which type;
// a caller with no proof is answered 202 at once instead, before the reread reads the store. The
// session is one this helper writes straight into databaseURL, the broker's own database
// (ciphertest.InsertSession), since nothing a row of this package holds can log a machine in to
// enrol one, and the proof names the public URL the binary derives from the address it bound
// (BROKER_PUBLIC_URL=http://127.0.0.1:0). It answers the status, the headers and the body as a
// JSON object, as post does.
//
// Each of its waits - the connection, the session's two inserts and the broker's answer - ends on
// its fact, with no span of its own: they run under one context that ends rereadSpare before the
// test binary's deadline (testwait.Remaining), so a broker or a database that never answers fails
// the row naming the wait, with the broker's log, rather than holding it until go test's alarm
// panics the binary with no line of the row's own (gaveUp).
func (p *brokerProcess) sessionReread(t *testing.T, databaseURL, name string) (int, http.Header, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testwait.Remaining(t, rereadSpare))
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		p.failReread(t, ctx, "a connection to the broker's database", err)
	}
	defer conn.Close(ctx)
	insert := execUnder{ctx: ctx, conn: conn, failed: func(err error) {
		p.gaveUp(t, ctx, "the session's insert into the broker's database", err)
	}}
	enrollmentID, key := ciphertest.InsertSession(t, insert, ciphertest.RereaderOperator)
	url := "http://" + p.Addr + "/v1/secrets/" + name + "/reread"
	signed, err := proof.Sign(key, enrollmentID, http.MethodPost, url, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Proof", signed)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		p.failReread(t, ctx, "the broker's answer to POST "+url+" as a session", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		p.failReread(t, ctx, "the broker's answer to POST "+url+" as a session", err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, resp.Header, body
}

// failReread fails t on err, which ended sessionReread's wait for what, with the broker's log.
func (p *brokerProcess) failReread(t *testing.T, ctx context.Context, what string, err error) {
	t.Helper()
	if line := p.gaveUp(t, ctx, what, err); line != "" {
		t.Fatalf("%s; the broker's log:\n%s", line, p.Log())
	}
	t.Fatalf("%s: %v; the broker's log:\n%s", what, err, p.Log())
}

// gaveUp answers the line that says ctx's deadline ended sessionReread's wait for what with err,
// and "" where ctx did not end it. It writes that line and the broker's log to stderr at once, as
// testwait's give-up writes its line: without -v go test holds a test's log until the test ends,
// and the cleanup that follows a stopped broker waits the broker's whole stop budget (90 s), past
// rereadSpare, so go test's alarm would panic the binary with the row's log never printed.
func (p *brokerProcess) gaveUp(t *testing.T, ctx context.Context, what string, err error) string {
	if ctx.Err() == nil {
		return ""
	}
	line := "sessionReread: " + t.Name() + ": gave up waiting for " + what + " " + rereadSpare.String() + " before the test binary's deadline: " + err.Error()
	_, _ = os.Stderr.WriteString(line + "; the broker's log:\n" + p.Log() + "\n")
	return line
}

// execUnder is conn with every Exec run under ctx, whatever context its caller passes:
// ciphertest.InsertSession writes under a context of its own with no deadline, and fails t itself
// on an error. failed runs with each error first, on the caller's goroutine, so the deadline's
// line reaches stderr before that failure (gaveUp).
type execUnder struct {
	ctx    context.Context
	conn   *pgx.Conn
	failed func(error)
}

func (e execUnder) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := e.conn.Exec(e.ctx, sql, args...)
	if err != nil {
		e.failed(err)
	}
	return tag, err
}
