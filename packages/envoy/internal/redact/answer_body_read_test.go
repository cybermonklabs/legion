package redact

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// rawAnswer serves, until t ends, a stand-in that reads each request on a connection of its own
// and writes answer of it on the connection itself, then closes it: an answer net/http's own
// server never writes. It answers the stand-in's base URL.
func rawAnswer(t *testing.T, answer func(request *http.Request) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, answer(request))
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

// bearerOf is the credential request carried in its Authorization, as a stand-in echoes it.
func bearerOf(request *http.Request) string {
	return strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
}

// TestAnAnswersBodyThatCannotBeReadPrintsNoLineOfIt: whatever answers in a server's place answers
// 200 with a chunked body whose trailer line quotes the credential the request carried and does
// not parse, which net/http's error of the body's read quotes (the twin). The error ReadAnswer
// answers (read-answer), and the error a read of StreamAnswer's stream answers (stream-answer),
// hold no run of the credential: each prints the failure by its class, and still unwraps to
// net/http's error. A body cut short (cut-read-answer, cut-stream-answer) is a transport failure,
// which each answers as net/http's own error, io.ErrUnexpectedEOF itself, as before. Captures
// redact.body-read/<reader>.
func TestAnAnswersBodyThatCannotBeReadPrintsNoLineOfIt(t *testing.T) {
	const token = "zt3wb-not-a-secret-RDX-a91c"
	trailer := rawAnswer(t, func(r *http.Request) string {
		return "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n" + bearerOf(r) + "\r\n\r\n"
	})
	cut := redacttest.CutServer(t, http.StatusOK, `{"answer":"cut short"}`)
	get := func(t *testing.T, base string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, base+"/repos/acme/widgets", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("the request got no answer: %v", err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	t.Run("twin", func(t *testing.T) {
		_, err := io.ReadAll(get(t, trailer).Body)
		if err == nil || !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error of the body's read holds no echo of the token, so the rows cannot see a leak: %v", err)
		}
	})
	readers := []struct {
		name string
		read func(response *http.Response) error
	}{
		{"read-answer", func(response *http.Response) error {
			_, err := ReadAnswer(response, 1<<20)
			return err
		}},
		{"stream-answer", func(response *http.Response) error {
			_, err := io.ReadAll(StreamAnswer(response, 1<<20))
			return err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			err := reader.read(get(t, trailer))
			if err == nil {
				t.Fatal("the body read whole")
			}
			redacttest.RequireNoPiece(t, "redact.body-read/"+reader.name, err.Error(), token)
			var protocol textproto.ProtocolError
			if err.Error() != ClassRequestFailed || !errors.As(err, &protocol) {
				t.Errorf("the read's error is %q (%#v), want %q over net/http's error of the trailer line", err, err, ClassRequestFailed)
			}
		})
		t.Run("cut-"+reader.name, func(t *testing.T) {
			if err := reader.read(get(t, cut)); err != io.ErrUnexpectedEOF {
				t.Errorf("the read of a body cut short answered %#v, want io.ErrUnexpectedEOF itself", err)
			}
		})
	}
}
