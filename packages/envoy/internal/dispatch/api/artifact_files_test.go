package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
)

// clientClosingFileStore is a file store whose Put and Get close the request's client while they
// run, as a client that leaves while the store works, and answer what the call's context then
// answers, as a store call the close cut off. Until closeClient is set it is the memory store it
// wraps, so a test uploads through it first.
type clientClosingFileStore struct {
	*filestest.Memory
	closeClient context.CancelFunc
}

func (s *clientClosingFileStore) Put(ctx context.Context, sha, mime string, body []byte) error {
	if s.closeClient == nil {
		return s.Memory.Put(ctx, sha, mime, body)
	}
	return s.closed(ctx)
}

func (s *clientClosingFileStore) Get(ctx context.Context, sha string) (*files.Object, error) {
	if s.closeClient == nil {
		return s.Memory.Get(ctx, sha)
	}
	return nil, s.closed(ctx)
}

// closed closes the client and answers the call's context's error: the request's cancel, which
// ends the call's context at once when the call runs on the request's context, and otherwise an
// error that is no cancel, which the route then logs as the store's fault.
func (s *clientClosingFileStore) closed(ctx context.Context) error {
	s.closeClient()
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("the store call's context did not end with its client's")
}

// requireClosedByClient fails the test unless logged holds one request line for method and route
// reading 499, the one debug line of a request its client closed and no level=ERROR line, and
// unless response is the answer the route gives a store that failed, 502 FILE_STORE_UNAVAILABLE,
// which a client still reading takes for the failure it is.
func requireClosedByClient(t *testing.T, logged *logBuffer, response *httptest.ResponseRecorder, method, route string) {
	t.Helper()
	lines := routeLines(t, logged, method, route)
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf(" status=%d ", accesslog.StatusClientClosed)) {
		t.Errorf("request lines %q, want one with status=%d", lines, accesslog.StatusClientClosed)
	}
	if faults := errorLines(logged); len(faults) != 0 {
		t.Errorf("a request its client closed wrote level=ERROR lines:\n%s", strings.Join(faults, "\n"))
	}
	if closed, _ := handlerErrorLines(logged); len(closed) != 1 {
		t.Errorf("logged %q, want one debug line for a request its client closed", closed)
	}
	if response.Code != http.StatusBadGateway || responseCode(t, response) != "FILE_STORE_UNAVAILABLE" {
		t.Errorf("answered %d %s, want 502 FILE_STORE_UNAVAILABLE", response.Code, response.Body.String())
	}
}

// An upload whose client closes it while the file store writes its bytes is no fault of the
// store's: the request line reads 499, it writes the debug line and no level=ERROR line, it is
// still answered 502 FILE_STORE_UNAVAILABLE, and no version is left behind.
func TestAnUploadItsClientClosedIsLoggedAs499(t *testing.T) {
	// Not parallel: it swaps the default logger (captureDebugLog, logtest.SetDefault).
	store := &clientClosingFileStore{Memory: filestest.NewMemory()}
	handler, database, _ := newTestServer(t, testServerOptions{files: store})
	issue := fileIssue(t, handler)
	logged := captureDebugLog(t)

	ctx, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	store.closeClient = closeClient
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "notes.bin"}, "notes.bin", "application/octet-stream", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", body)
	request.Header.Set("Content-Type", bodyType)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	requireClosedByClient(t, logged, response, http.MethodPost, "/api/v1/issues/{key}/artifacts")
	var artifacts int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifacts where issue_key = $1 and kind <> 'doc'`, issue).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("%d file artifacts exist after an upload its client closed, want none", artifacts)
	}
}

// A file version's read whose client closes it while the file store opens the object is no fault
// of the store's: the request line reads 499, it writes the debug line and no level=ERROR line, and
// it is still answered 502 FILE_STORE_UNAVAILABLE. The version's row holds no bytes, since the
// server stores them in the bucket, so the read goes to the store.
func TestAFileReadItsClientClosedIsLoggedAs499(t *testing.T) {
	// Not parallel: it swaps the default logger (captureDebugLog, logtest.SetDefault).
	store := &clientClosingFileStore{Memory: filestest.NewMemory()}
	handler, _, _ := newTestServer(t, testServerOptions{files: store})
	issue := fileIssue(t, handler)
	upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.bin"}, "shot.bin", "application/octet-stream", []byte("shot bytes"), "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	version := decodeBody[uploaded](t, upload)
	logged := captureDebugLog(t)

	ctx, closeClient := context.WithCancel(context.Background())
	defer closeClient()
	store.closeClient = closeClient
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	requireClosedByClient(t, logged, response, http.MethodGet, "/api/v1/artifacts/{id}/versions/{number}")
}
