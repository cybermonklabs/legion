package redact_test

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// readsInTurn is a body that answers each Read with the next of its reads, then io.EOF.
type readsInTurn struct{ reads []func(p []byte) (int, error) }

func (r *readsInTurn) Read(p []byte) (int, error) {
	if len(r.reads) == 0 {
		return 0, io.EOF
	}
	read := r.reads[0]
	r.reads = r.reads[1:]
	return read(p)
}

func (*readsInTurn) Close() error { return nil }

func bytesOf(text string) func(p []byte) (int, error) {
	return func(p []byte) (int, error) { return copy(p, text), nil }
}

func nothing(error) func(p []byte) (int, error) {
	return func([]byte) (int, error) { return 0, nil }
}

func failing(err error) func(p []byte) (int, error) {
	return func([]byte) (int, error) { return 0, err }
}

// TestAStreamAtItsLimitTellsTheBodysEndFromWhatFollows: a stream read to its limit reads one byte
// more to tell a body that ends there from one that runs past it. A body that answers that read
// with no byte and no error, then a byte, runs past the limit (empty-then-byte): ErrAnswerTooLarge,
// where the stream ended the body silently at the limit. A body whose read at the limit fails
// (failed) is that failure, where the stream read it as the body's end. A body that ends at the
// limit (ends) is the whole body, and one that answers nothing forever (no-progress) fails with
// io.ErrNoProgress rather than spinning.
func TestAStreamAtItsLimitTellsTheBodysEndFromWhatFollows(t *testing.T) {
	for _, c := range []struct {
		name  string
		reads []func(p []byte) (int, error)
		check func(err error) bool
	}{
		{"empty-then-byte", []func(p []byte) (int, error){bytesOf("abcd"), nothing(nil), bytesOf("e")},
			func(err error) bool { return errors.Is(err, redact.ErrAnswerTooLarge) }},
		{"failed", []func(p []byte) (int, error){bytesOf("abcd"), failing(io.ErrUnexpectedEOF)},
			func(err error) bool { return errors.Is(err, io.ErrUnexpectedEOF) }},
		{"ends", []func(p []byte) (int, error){bytesOf("abcd")},
			func(err error) bool { return err == nil }},
		{"no-progress", append([]func(p []byte) (int, error){bytesOf("abcd")}, repeat(nothing(nil), 200)...),
			func(err error) bool { return errors.Is(err, io.ErrNoProgress) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			stream := redact.StreamAnswer(&http.Response{StatusCode: http.StatusOK, Body: &readsInTurn{reads: c.reads}}, 4)
			got, err := io.ReadAll(stream)
			if string(got) != "abcd" || !c.check(err) {
				t.Errorf("the stream read %q, %v", got, err)
			}
		})
	}
}

func repeat(read func(p []byte) (int, error), n int) []func(p []byte) (int, error) {
	reads := make([]func(p []byte) (int, error), n)
	for i := range reads {
		reads[i] = read
	}
	return reads
}
