// answer_id_test.go drives the answer id from a broker's refusal to this client's error: the id
// the broker's own record of the refusal names (internal/broker/answer), which a refused person
// or agent quotes to whoever runs the broker.
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/answer"
)

// TestARefusalNamesTheBrokersAnswerID: a broker behind the answer recorder its binary installs
// refuses in its JSON envelope, which carries answer_id, and in the text of a path it does not
// serve, which carries the id in its header alone; either way the error this client reads ends
// with the id that answer carried, on the line of the broker's message, the text's own line
// ending cut. An answer_id the body names is the one it ends with, over the header's id, so the
// body is what it reads. A broker from before the id sends neither, and the error reads as it
// always did.
func TestARefusalNamesTheBrokersAnswerID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/requests/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"NOT_YOURS","error":"this request belongs to another session"}` + "\n"))
	})
	// A refusal whose body already names an answer id, which the recorder leaves as it is.
	mux.HandleFunc("GET /v1/taken", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"TAKEN","error":"this name is taken","answer_id":"from-body"}` + "\n"))
	})
	// sent is each answer id the recorded broker sent, once its answer is written.
	sent := make(chan string, 1)
	recorder := answer.Middleware(mux, nil)
	recorded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.ServeHTTP(w, r)
		sent <- w.Header().Get(answer.Header)
	}))
	defer recorded.Close()
	older := httptest.NewServer(mux)
	defer older.Close()

	driven := 0
	for _, row := range []struct {
		name, base, path, want string
		// bodyID is the answer id the body names, which the error ends with over the header's;
		// "" for a body that names none.
		bodyID string
	}{
		{"a refusal in the broker's envelope", recorded.URL, "/v1/requests/req-1", "this request belongs to another session (NOT_YOURS)", ""},
		{"a refusal whose body names its own answer id", recorded.URL, "/v1/taken", "this name is taken (TAKEN)", "from-body"},
		{"a path the broker does not serve", recorded.URL, "/v1/nowhere", "HTTP 404: 404 page not found", ""},
		{"a broker from before the id", older.URL, "/v1/requests/req-1", "this request belongs to another session (NOT_YOURS)", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			_, err := newClient(row.base).do(context.Background(), http.MethodGet, row.base+row.path, nil, nil)
			var refusal *apiError
			if !errors.As(err, &refusal) {
				t.Fatalf("the refusal read as %v, want an *apiError", err)
			}
			want := row.want
			if row.base == recorded.URL {
				id := <-sent
				if id == "" {
					t.Fatal("the recorded broker's answer carries no answer id")
				}
				if row.bodyID != "" {
					id = row.bodyID
				}
				want += " (answer " + id + ")"
			}
			if got := refusal.Error(); got != want {
				t.Errorf("the refusal reads %q, want %q", got, want)
			}
		})
	}
	if driven != 4 {
		t.Fatalf("the table drove %d refusals, want 4", driven)
	}
}
