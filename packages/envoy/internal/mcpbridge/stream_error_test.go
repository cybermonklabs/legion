package mcpbridge

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// goAwayServer is an MCP server whose SSE stream speaks HTTP/2 by hand, so that it can end the
// stream with a GOAWAY frame that carries debug data: net/http's client puts that data, quoted, in
// the error the stream's body read answers (net/http/internal/http2's GoAwayError), so the error
// holds whatever the server wrote there. The stream answers the request the client sends with 200
// and an event stream, writes each event sent on events, in order, and once end has been called
// and every event sent is written, a GOAWAY whose debug data is debug; then it closes the
// connection. Its message endpoint, messageURL, is a plain HTTP server that answers the handshake's
// initialize on the stream.
type goAwayServer struct {
	url, messageURL string
	events          chan string
	goAway          chan struct{}
	end             func()
}

// sseEvent is one SSE event of type kind whose data is data.
func sseEvent(kind, data string) string { return "event: " + kind + "\ndata: " + data + "\n\n" }

func newGoAwayServer(t *testing.T, debug string) *goAwayServer {
	t.Helper()
	g := &goAwayServer{events: make(chan string, 16), goAway: make(chan struct{})}
	var once sync.Once
	g.end = func() { once.Do(func() { close(g.goAway) }) }
	messages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Method == "initialize" {
			g.events <- sseEvent("message", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(messages.Close)
	g.messageURL = messages.URL + "/message"

	stream := httptest.NewUnstartedServer(http.NotFoundHandler())
	stream.EnableHTTP2 = true
	stream.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) { g.serve(conn, debug) },
	}
	stream.StartTLS()
	t.Cleanup(stream.Close)
	// Ended before the server closes, so a test that fails before it ends the stream leaves no
	// connection open.
	t.Cleanup(g.end)
	g.url = stream.URL

	// The SSE connect goes through http.DefaultTransport, which trusts the server's certificate
	// until t ends and asks for HTTP/2 by ALPN.
	transport := http.DefaultTransport.(*http.Transport)
	previous := transport.TLSClientConfig
	trusting := new(tls.Config)
	if previous != nil {
		trusting = previous.Clone()
	}
	trusting.RootCAs = stream.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if !slices.Contains(trusting.NextProtos, "h2") {
		trusting.NextProtos = append([]string{"h2"}, trusting.NextProtos...)
	}
	transport.TLSClientConfig = trusting
	t.Cleanup(func() {
		transport.TLSClientConfig = previous
		transport.CloseIdleConnections()
	})
	return g
}

// serve speaks HTTP/2 on conn: it answers the first stream the client opens with 200 and an event
// stream, writes each event, then the GOAWAY carrying debug, and closes conn.
func (g *goAwayServer) serve(conn *tls.Conn, debug string) {
	defer conn.Close()
	if _, err := io.ReadFull(conn, make([]byte, len(http2.ClientPreface))); err != nil {
		return
	}
	framer := http2.NewFramer(conn, conn)
	var writing sync.Mutex
	write := func(frame func() error) error {
		writing.Lock()
		defer writing.Unlock()
		return frame()
	}
	if write(func() error { return framer.WriteSettings() }) != nil {
		return
	}
	opened := make(chan uint32, 1)
	// read ends when the client's side of the connection does.
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() && write(framer.WriteSettingsAck) != nil {
					return
				}
			case *http2.HeadersFrame:
				select {
				case opened <- f.StreamID:
				default:
				}
			}
		}
	}()
	var stream uint32
	select {
	case stream = <-opened:
	case <-read:
		return
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	encoder.WriteField(hpack.HeaderField{Name: "content-type", Value: "text/event-stream"})
	if write(func() error {
		return framer.WriteHeaders(http2.HeadersFrameParam{StreamID: stream, BlockFragment: block.Bytes(), EndHeaders: true})
	}) != nil {
		return
	}
	data := func(event string) error {
		return write(func() error { return framer.WriteData(stream, false, []byte(event)) })
	}
	for {
		// Every event sent goes out before the GOAWAY.
		select {
		case event := <-g.events:
			if data(event) != nil {
				return
			}
			continue
		default:
		}
		select {
		case event := <-g.events:
			if data(event) != nil {
				return
			}
		case <-g.goAway:
			write(func() error { return framer.WriteGoAway(stream, http2.ErrCodeProtocol, []byte(debug)) })
			return
		case <-read:
			return
		}
	}
}

// TestAStreamReadErrorPrintsItsClassAlone: an MCP server over HTTP/2 ends its SSE stream with a
// GOAWAY whose debug data quotes the probe's URL, which net/http's error for the stream's read
// quotes: before the stream names its message endpoint, so Start fails (endpoint), and after the
// handshake, so the session's read loop ends (loop) and WaitForExit answers what the bridge logs
// as the server's exit. Each error names the failure by its class (redact.Cause), and none of the
// server's words. Captures mcpbridge.stream-error/<case>.
func TestAStreamReadErrorPrintsItsClassAlone(t *testing.T) {
	config := func(g *goAwayServer) ServerConfig {
		return ServerConfig{Name: "whatsapp", Transport: "http", URL: g.url}
	}
	t.Run("endpoint", func(t *testing.T) {
		g := newGoAwayServer(t, redacttest.Words)
		g.end()
		s := NewHTTPServer(config(g), nil)
		err := s.Start()
		if err == nil {
			s.Stop()
			t.Fatal("Start succeeded against a stream that ended before it named its endpoint")
		}
		redacttest.RequireNone(t, "mcpbridge.stream-error/endpoint", err.Error())
		if want := "read endpoint event: " + redact.ClassRequestFailed; err.Error() != want {
			t.Errorf("Start = %q; want %q", err, want)
		}
	})
	t.Run("loop", func(t *testing.T) {
		g := newGoAwayServer(t, redacttest.Words)
		g.events <- sseEvent("endpoint", g.messageURL)
		s := NewHTTPServer(config(g), nil)
		if err := s.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		defer s.Stop()
		g.end()
		err := s.WaitForExit()
		if err == nil {
			t.Fatal("WaitForExit answered no error for a stream the server ended")
		}
		redacttest.RequireNone(t, "mcpbridge.stream-error/loop", err.Error())
		if err.Error() != redact.ClassRequestFailed {
			t.Errorf("WaitForExit = %q; want %q", err, redact.ClassRequestFailed)
		}
	})
}
