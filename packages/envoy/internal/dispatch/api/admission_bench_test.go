package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// BenchmarkAnAdmittedRequest is admission's own cost for one request the server has room for:
// admitted around a handler that does nothing, on a request whose context is cancellable and
// carries the store's mark, as net/http's and trackTransactions make every request's.
func BenchmarkAnAdmittedRequest(b *testing.B) {
	handler := (&server{}).admitted(admission.New(admission.DefaultLimits()), 1, admission.NoLane, false, withContext(func(http.ResponseWriter, *http.Request) {}))
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil).WithContext(store.WithTransactionTracking(parent))
	answer := httptest.NewRecorder()
	b.ReportAllocs()
	for b.Loop() {
		handler(answer, request)
	}
}

// BenchmarkAnAdmittedRead is a read's own wrapping for a request the server has room for:
// admitted around servedAsRead, as routeHandler builds them for every read row with nothing
// between the two, around a handler that answers a small 200 JSON body, on the request
// BenchmarkAnAdmittedRequest sends. The read is an ordinary one, bounded for its whole length at
// the deadline register gives every read (defaultReadDeadline), and pinned or not
// (apiRoute.pinsConnection); the pinned read's handler runs no statement, so its pin is made and
// given back without a connection. The layers outside admission - the access log, compression,
// the transaction tracking and the broker token's decision - and those between the deadline and
// a route's own handler are not in it.
func BenchmarkAnAdmittedRead(b *testing.B) {
	for _, read := range []struct {
		name string
		pin  bool
	}{{"unpinned", false}, {"pinned", true}} {
		b.Run(read.name, func(b *testing.B) {
			served := &server{}
			body := &struct {
				Status string `json:"status"`
			}{Status: "ok"}
			handler := served.admitted(admission.New(admission.DefaultLimits()), 1, admission.NoLane, false,
				served.servedAsRead(defaultReadDeadline, false, read.pin, func(w http.ResponseWriter, _ *http.Request) {
					WriteJSON(w, http.StatusOK, body)
				}))
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil).WithContext(store.WithTransactionTracking(parent))
			answer := &benchConnection{header: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				handler(answer, request)
			}
			if answer.status != http.StatusOK || answer.written == 0 {
				b.Fatalf("the read answered %d with %d bytes, want 200 with its JSON body", answer.status, answer.written)
			}
		})
	}
}

// BenchmarkAnAdmittedWriteThroughTheWriterGate is a write's admission and its document's writer
// gate where nobody is ahead of it in either: the write class's admitted around a handler that
// takes one document's gate (docs.Service.AcquireDocumentWriter) and releases it, on the request
// BenchmarkAnAdmittedRequest sends but a POST. The gate needs nothing of its service but the
// writers registry, so the service is the zero one and no database is asked anything.
func BenchmarkAnAdmittedWriteThroughTheWriterGate(b *testing.B) {
	documents := new(docs.Service)
	handler := (&server{}).admitted(admission.New(admission.DefaultLimits()), 1, admission.NoLane, false, withContext(func(_ http.ResponseWriter, r *http.Request) {
		_, release, err := documents.AcquireDocumentWriter(r.Context(), "art-bench")
		if err != nil {
			b.Fatalf("take a writer gate nobody holds: %v", err)
		}
		release()
	}))
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/art-bench/edits", nil).WithContext(store.WithTransactionTracking(parent))
	answer := httptest.NewRecorder()
	b.ReportAllocs()
	for b.Loop() {
		handler(answer, request)
	}
}

// benchConnection is an answer's writer as net/http's own is to the layers that bound it: it
// takes the write deadlines and the flush they set, and keeps nothing of what is written, so an
// iteration costs what the layers and the handler do and nothing for a recorder's growing body or
// for the error http.ResponseController makes for every deadline a writer cannot take.
type benchConnection struct {
	header  http.Header
	status  int
	written int
}

func (c *benchConnection) Header() http.Header { return c.header }

func (c *benchConnection) WriteHeader(status int) { c.status = status }

func (c *benchConnection) Write(p []byte) (int, error) {
	c.written = len(p)
	return len(p), nil
}

func (c *benchConnection) FlushError() error { return nil }

func (c *benchConnection) SetWriteDeadline(time.Time) error { return nil }
