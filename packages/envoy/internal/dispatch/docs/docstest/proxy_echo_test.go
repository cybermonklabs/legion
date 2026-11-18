package docstest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// echoSession is the session cookie the rows' peers present, as socket_admission_test's do: long
// enough that excerpt redacts every run of eight of its characters, and holding no word, so no
// piece of it turns up in a message's own prose.
const echoSession = "ds-7c1e9f3a5b2d48e6a0c4"

// echoHeader is a peer's header: the person it names in the identity header and its session
// cookie.
func echoHeader() http.Header {
	return http.Header{"X-Dispatch-User": {"ada"}, "Cookie": {"dispatch_session=" + echoSession}}
}

// leaked is the first piece of credential that out holds: a run of eight of its characters, the
// shortest run excerpt redacts wherever it stands; "" where out holds none.
func leaked(out, credential string) string {
	for at := 0; at+8 <= len(credential); at++ {
		if piece := credential[at : at+8]; strings.Contains(out, piece) {
			return piece
		}
	}
	return ""
}

// TestProxyEchoRefusedHandshake: a handshake a gateway refuses in the room's place, with a page
// echoing the request back (the session cookie bare on a line of its own first, so only the
// redaction of the cookie's value can hide it), is named in one line with that value redacted.
func TestProxyEchoRefusedHandshake(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var page strings.Builder
		page.WriteString("<!DOCTYPE html>\n<html><head><title>Gateway</title></head><body>\n<h1>Gateway</h1>\n")
		for _, c := range r.Cookies() {
			page.WriteString("<p>\n" + c.Value + "\n</p>\n")
		}
		fmt.Fprintf(&page, "<pre>\n%s %s %s\n", r.Method, r.URL.RequestURI(), r.Proto)
		for name, values := range r.Header {
			for _, value := range values {
				fmt.Fprintf(&page, "%s: %s\n", name, value)
			}
		}
		page.WriteString("</pre>\n</body></html>\n")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, page.String())
	}))
	t.Cleanup(gateway.Close)
	_, err := DialContext(context.Background(), "ws"+strings.TrimPrefix(gateway.URL, "http")+"/ws/doc/room", echoHeader(), "room", crdt.New())
	if err == nil {
		t.Fatal("a gateway's 502 page opened a connection")
	}
	if piece := leaked(err.Error(), echoSession); piece != "" {
		t.Errorf("the refusal printed %q, a piece of the session cookie the handshake carried:\n%v", piece, err)
	}
	if want := `a body that is not Dispatch's error JSON (`; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the gateway's page as %q:\n%v", want, err)
	}
	t.Logf("the refusal reads: %v", err)
}

// TestProxyEchoRefusedHandshakeReason: a reason phrase a gateway chose, here the session cookie the
// handshake carried, is named with that value redacted, since whatever answered wrote it.
func TestProxyEchoRefusedHandshakeReason(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		request, err := http.ReadRequest(bufio.NewReader(connection))
		if err != nil {
			return
		}
		cookie, _ := request.Cookie("dispatch_session")
		_, _ = fmt.Fprintf(connection, "HTTP/1.1 403 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", cookie.Value)
	}()
	_, err = DialContext(context.Background(), "ws://"+listener.Addr().String()+"/ws/doc/room", echoHeader(), "room", crdt.New())
	if err == nil {
		t.Fatal("a gateway's 403 opened a connection")
	}
	if piece := leaked(err.Error(), echoSession); piece != "" {
		t.Errorf("the refusal printed %q, a piece of the session cookie the handshake carried:\n%v", piece, err)
	}
	if want := "403 [redacted] with an empty body"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the gateway's answer as %q:\n%v", want, err)
	}
	t.Logf("the refusal reads: %v", err)
}

// TestProxyEchoRefusedHandshakeKeepsDispatchsOwn: Dispatch's own error JSON is printed as it always
// was, quoted whole after the status.
func TestProxyEchoRefusedHandshakeKeepsDispatchsOwn(t *testing.T) {
	const refusal = `{"code":"SIGNIN_UNAVAILABLE","error":"sign-in is unavailable"}`
	room := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, refusal+"\n")
	}))
	t.Cleanup(room.Close)
	_, err := DialContext(context.Background(), "ws"+strings.TrimPrefix(room.URL, "http")+"/ws/doc/room", echoHeader(), "room", crdt.New())
	if want := fmt.Sprintf("503 Service Unavailable %q", refusal); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal reads %v, want %s", err, want)
	}
}
