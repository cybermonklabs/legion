package redacttest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
)

// Reason is a status line's reason phrase that quotes the probe's URL, credentials and all, as
// whatever answers in a server's place can write it: the phrase is the answering party's own
// words (net/http's Response.Status holds it), and net/http's own server writes only the standard
// one, so StatusLineServer writes it on the connection itself.
const Reason = "see " + Code

// StatusLineServer answers every request with "HTTP/1.1 <status> Reason", header (lines of its
// own, each ending in CRLF) and body, written on the connection itself, and answers its base URL.
// A message that prints the answer's status line prints the probe unless it names the status
// through the code and the standard phrase (redact.StatusLine).
func StatusLineServer(t testing.TB, status int, header, body string) string {
	t.Helper()
	return serve(t, func(conn io.Writer) bool {
		_, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n%sContent-Length: %d\r\n\r\n%s", status, Reason, header, len(body), body)
		return err == nil
	})
}

// CutServer is StatusLineServer writing half of body under a Content-Length for the whole of it,
// then closing the connection, as a reset mid-body reads to the client: the status line, part of
// the body, then an unexpected end.
func CutServer(t testing.TB, status int, body string) string {
	t.Helper()
	return serve(t, func(conn io.Writer) bool {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", status, Reason, len(body), body[:len(body)/2])
		return false
	})
}

// serve accepts connections until t ends and, for each request read on one, has write answer it
// on the connection and report whether the connection serves another request. It answers the
// server's base URL.
func serve(t testing.TB, write func(conn io.Writer) bool) string {
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
				reader := bufio.NewReader(conn)
				for {
					request, err := http.ReadRequest(reader)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, request.Body)
					if !write(conn) {
						return
					}
				}
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}
