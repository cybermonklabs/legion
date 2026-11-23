package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
)

// The registry answers the body a key began receiving last, and forgets each body once it stops.
func TestReceivingBodiesAnswerTheLatestBodyOfAKeyUntilItStops(t *testing.T) {
	t.Parallel()
	var bodies receivingBodies
	if _, _, ok := bodies.received("alice", "k"); ok {
		t.Fatal("a key no body began reports one")
	}
	first, stopFirst := bodies.start("alice", "k", io.NopCloser(strings.NewReader("abcdef")), 6)
	second, stopSecond := bodies.start("alice", "k", io.NopCloser(strings.NewReader("xyz")), 3)
	if _, err := io.CopyN(io.Discard, first, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, second, 1); err != nil {
		t.Fatal(err)
	}
	if read, length, ok := bodies.received("alice", "k"); !ok || read != 1 || length != 3 {
		t.Fatalf("received = %d of %d (%v), want the later body's 1 of 3", read, length, ok)
	}
	if _, _, ok := bodies.received("bob", "k"); ok {
		t.Fatal("another login's key reports alice's body")
	}
	stopSecond()
	if read, length, ok := bodies.received("alice", "k"); !ok || read != 4 || length != 6 {
		t.Fatalf("received once the later body stopped = %d of %d (%v), want the first's 4 of 6", read, length, ok)
	}
	stopFirst()
	if _, _, ok := bodies.received("alice", "k"); ok {
		t.Fatal("a key whose every body stopped still reports one")
	}
}

// An upload's keyed body is counted as it arrives: GET /api/v1/request-keys/{key}/received answers
// its owner how much of it Dispatch has read while it still comes, answers anyone else 404, and
// answers 404 once the upload has been answered. The body comes through a pipe, so the test
// decides how much of it has arrived.
func TestAKeyedUploadSaysHowMuchOfItsBodyHasArrivedWhileItArrives(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createArtifactIssue(t, handler)
	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "arriving.bin"}, "arriving.bin",
		"application/octet-stream", bytes.Repeat([]byte("a"), 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	whole := body.Bytes()
	reader, writer := io.Pipe()
	// A failure part way leaves the handler reading the pipe: closing it ends that read.
	t.Cleanup(func() { writer.CloseWithError(io.ErrUnexpectedEOF) })
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", reader)
	request.ContentLength = int64(len(whole))
	request.Header.Set("Content-Type", bodyType)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "arriving-upload")
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		answered <- response
	}()
	asked := func(login string) (*httptest.ResponseRecorder, requestKeyReceived) {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/request-keys/arriving-upload/received", nil, login)
		var answer requestKeyReceived
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
				t.Fatalf("the answer %s: %v", response.Body.String(), err)
			}
		}
		return response, answer
	}
	half := len(whole) / 2
	if _, err := writer.Write(whole[:half]); err != nil {
		t.Fatal(err)
	}
	// The pipe's write returns once the server has taken the bytes; the count follows the read
	// that took them, so it is waited for, never assumed.
	var answer requestKeyReceived
	var response *httptest.ResponseRecorder
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		response, answer = asked("alice")
		if response.Code == http.StatusOK && answer.Received == int64(half) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with half the body sent: status=%d body=%s, want 200 {received: %d}", response.Code, response.Body.String(), half)
		}
	}
	if answer.Length != int64(len(whole)) {
		t.Fatalf("length = %d, want the request's %d", answer.Length, len(whole))
	}
	if other, _ := asked("bob"); other.Code != http.StatusNotFound {
		t.Fatalf("bob asking of alice's key: status=%d body=%s, want 404", other.Code, other.Body.String())
	}
	if _, err := writer.Write(whole[half:]); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-answered:
		if response.Code != http.StatusCreated {
			t.Fatalf("the upload: status=%d body=%s, want 201", response.Code, response.Body.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upload was never answered")
	}
	if after, _ := asked("alice"); after.Code != http.StatusNotFound {
		t.Fatalf("once the upload was answered: status=%d body=%s, want 404", after.Code, after.Body.String())
	}
}
