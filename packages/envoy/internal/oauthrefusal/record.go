package oauthrefusal

import (
	"context"
	"io"
	"net/http"

	"golang.org/x/oauth2"
)

// Record is ctx carrying a copy of the client x/oauth2 and go-oidc send their requests with - the
// one ctx carries (oauth2.HTTPClient), else http.DefaultClient - wrapped as Client wraps it.
func Record(ctx context.Context, answered *Recorder) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, Client(oauth2.NewClient(ctx, nil), answered))
}

// Client is a copy of base (nil is http.DefaultClient), timeout and all, with its transport wrapped
// so answered records what became of each request it carries, for a library that takes its client
// as an option rather than from ctx. A reader whose library error drops an answer's status, or
// quotes its body, reads the answer from answered instead.
func Client(base *http.Client, answered *Recorder) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	wrapped := *base
	answered.base = wrapped.Transport
	if answered.base == nil {
		answered.base = http.DefaultTransport
	}
	wrapped.Transport = answered
	return &wrapped
}

// Recorder is a transport that records what became of the last request it carried: the host it
// went to (Host); the answer's status, or 0 where the request got no answer (Status), so a
// redirect whose target fails is not read as the redirect; and the answer's body, which records
// how a read of it failed (ReadErr), so an answer the client could not read whole is not read as
// one it read, and keeps what was read of an answer that is no 2xx (Answer). It keeps nothing of a
// 2xx answer's body, a token answer holding a person's tokens among them, so no copy of a
// successful answer stays in memory. It records the status of every answer its transport returns,
// whatever the error beside it. One call at a time uses it: it is built for one call, whose
// requests and body reads run in the caller's goroutine.
type Recorder struct {
	// Host is the host the last request went to.
	Host string
	// Status is the last answer's status, and 0 where the last request got no answer.
	Status int
	base   http.RoundTripper
	body   *readOf
}

func (r *Recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	r.Host, r.Status, r.body = request.URL.Host, 0, nil
	if response != nil {
		r.Status = response.StatusCode
		r.body = &readOf{ReadCloser: response.Body, keep: response.StatusCode/100 != 2}
		response.Body = r.body
	}
	return response, err
}

// ReadErr is the first error other than io.EOF a read of the last answer's body ended in, and nil
// where that body read whole, or so far, or there was no answer.
func (r *Recorder) ReadErr() error {
	if r.body == nil {
		return nil
	}
	return r.body.err
}

// Answer is what was read of the last answer's body, at most answerKept bytes, and nil for a 2xx
// answer, whose body a Recorder does not keep. It is the server's text, for a reader that prints a
// registered code alone (internal/redact, Answered).
func (r *Recorder) Answer() []byte {
	if r.body == nil {
		return nil
	}
	return r.body.kept
}

// answerKept is how much of an answer's body a Recorder keeps: an error body is short, and a
// longer one is no refusal a reader prints.
const answerKept = 64 << 10

// readOf is an answer's body that keeps the first error other than io.EOF a read of it ends in,
// and, when keep is set, the first answerKept bytes read of it.
type readOf struct {
	io.ReadCloser
	keep bool
	err  error
	kept []byte
}

func (r *readOf) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if room := answerKept - len(r.kept); r.keep && room > 0 {
		r.kept = append(r.kept, p[:min(n, room)]...)
	}
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}
