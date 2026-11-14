// Scratch reproductions for review 6 of sjawhar/legion PR #2544 at 0abf2096, package api
// (packages/envoy/internal/dispatch/api). Each test asserts the correct behaviour, so it FAILS at
// that head. Findings 5 and 6 of the review (see each test's comment).
//
// Run (from packages/envoy):
//
//	DISPATCH_TEST_DATABASE_URL=$LEGION_TEST_PG_DSN GOCACHE=$TMPDIR/gocache go test \
//	  -overlay /legion/scratch/tmp/r6repro/overlay-api.json ./internal/dispatch/api \
//	  -run 'TestR6' -count=1 -v -timeout 10m
package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// Finding 5 (write_limit.go:71-80, server.go:1490-1497): a request's second transaction takes a
// fresh write-limit slot under docs.BoundWriteWait, measured from the request's start, so 15.5 s
// into a request it is refused SERVICE_BUSY with every slot free. No hook backdates the caller
// mark from package api (callerRequestKey is docs-internal), so this waits the 15.5 s for real.
func TestR6Finding5SecondTransactionRefusedBusyWithEverySlotFree(t *testing.T) {
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	// A live request's context has a Done channel; a nil Done reads as the server's own work.
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := docs.WithCallerRequest(request)
	first, err := served.begin(ctx)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	_ = first.Rollback(ctx)
	time.Sleep(15500 * time.Millisecond)
	free := served.writes.semaphore().TryAcquire(apiWriteLimit)
	if free {
		served.writes.semaphore().Release(apiWriteLimit)
	}
	second, err := served.begin(ctx)
	t.Logf("15.5s into the request: all %d slots free=%t; second begin err=%v", apiWriteLimit, free, err)
	if second != nil {
		_ = second.Rollback(ctx)
	}
	if err != nil {
		t.Fatalf("a second transaction 15.5s into a request was refused with every slot free: %v", err)
	}
}

// Finding 6 (write_limit.go:174-213, body_drain.go:53-57): deadlinedBody sets the drain's deadline
// once at the first read, so a body still arriving, slowly but steadily, is cut at drainTime.
func TestR6Finding6SlowButProgressingBodyIsCutByTheDrainDeadline(t *testing.T) {
	const drainLimit = time.Second
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.drainReadLimit = drainLimit
	server := startTestHTTP(t, handler)
	issue := createArtifactIssue(t, handler)
	body := `{"body":"` + strings.Repeat("a", 30) + `"}`
	conn, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /api/v1/issues/%s/messages HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nX-Dispatch-User: alice\r\nContent-Length: %d\r\n\r\n", issue.Key, len(body))
	started := time.Now()
	go func() {
		// 3 bytes every 150 ms: ~2.0 s for the whole body, always progressing.
		for i := 0; i < len(body); i += 3 {
			if _, err := io.WriteString(conn, body[i:min(i+3, len(body))]); err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(response.Body)
	t.Logf("drain limit %v; %d-byte body sent over ~%v; answered after %v: %d %s",
		drainLimit, len(body), time.Duration(len(body)/3)*150*time.Millisecond, time.Since(started), response.StatusCode, answer)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("a slow but progressing body was answered %d %s, want 201", response.StatusCode, answer)
	}
}

func startTestHTTP(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return listener.Addr().String()
}
