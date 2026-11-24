package mcpbridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEndpointResolvesAgainstTheSSEURL: the MCP server's SSE endpoint event names the URL the
// bridge posts each message to, which the SSE transport lets it name by a relative reference, and
// the configured URL carries a credential in its user information (net/http's Basic Authorization).
// An endpoint that names a path on the server's own origin, /message?session_id=... (relative) or
// message?session_id=..., which RFC 3986 merges with the SSE URL's /sse (relative-path), is posted
// to there, with the session and the configured credential, and the handshake completes; so does an
// absolute endpoint on that origin (absolute). An endpoint on another origin, named by a
// network-path reference (elsewhere, //host:port/...) or absolutely (elsewhere-absolute), is posted
// to there, as the bridge has always posted to an absolute endpoint, and is sent no run of the
// configured credential: a relative reference takes the SSE URL's user information only with its
// host. A relative endpoint with an access token in its query that the server refuses (refused),
// or whose POST's connection it drops (dropped), fails Start naming the endpoint as the rule reads
// it, with none of the configured credential it took on and no query value: the rule reads a
// relative reference as words, not as a URL, and printed this one whole. An endpoint that does not
// parse fails Start in the bridge's own words, with none of the endpoint's (unparsed). The twin
// shows each capture can see a credential: the endpoint the refused row resolved carries the
// password and the token, net/http's own error for a dropped POST quotes the user, the other
// origin's stand-in keeps the password of a Basic Authorization it is sent, and url.Parse's error
// quotes the unparsed endpoint's credentials. Captures mcpbridge.relative-endpoint/<case>.
func TestAnEndpointResolvesAgainstTheSSEURL(t *testing.T) {
	const session = "s3ss10n-7Qz4k"
	relative := func(string) string { return "/message?session_id=" + session }
	tokened := func(string) string {
		return "/message?session_id=" + session + "&access" + "_token=" + redacttest.Token
	}
	configured := redacttest.User + ":" + redacttest.Password
	start := func(address string) (*HTTPServer, error) {
		s := NewHTTPServer(ServerConfig{Name: "relative", Transport: "http", URL: withUser("http://" + address)}, func(string) {})
		err := s.Start()
		if err == nil {
			s.Stop()
		}
		return s, err
	}
	for _, c := range []struct {
		name     string
		endpoint func(host string) string
		want     string // how each line the stand-in keeps of a POST starts
	}{
		{"relative", relative, "/message?session_id=" + session + " " + configured},
		{"relative-path", func(string) string { return "message?session_id=" + session }, "/message?session_id=" + session + " " + configured},
		{"absolute", func(host string) string { return "http://" + host + "/message?session_id=" + session }, "/message?session_id=" + session + " "},
	} {
		t.Run(c.name, func(t *testing.T) {
			var posts kept
			if _, err := start(standIn(t, c.endpoint, handshake(&posts))); err != nil {
				t.Fatalf("Start = %v; want the handshake to complete over the endpoint the server named", err)
			}
			lines := posts.lines()
			if len(lines) == 0 {
				t.Fatal("the server's endpoint was sent no POST")
			}
			for _, line := range lines {
				if !strings.HasPrefix(line, c.want) {
					t.Errorf("the server's endpoint was sent %q; want each POST to start %q", line, c.want)
				}
			}
		})
	}
	for _, c := range []struct {
		name     string
		endpoint func(other string) string
	}{
		{"elsewhere", func(other string) string { return "//" + other + "/message?session_id=" + session }},
		{"elsewhere-absolute", func(other string) string { return "http://" + other + "/message?session_id=" + session }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var posts kept
			other := elsewhere(t, &posts)
			_, err := start(standIn(t, func(string) string { return c.endpoint(other) }, refusing))
			text := posts.text()
			redacttest.RequireNoPiece(t, "mcpbridge.relative-endpoint/"+c.name, text, redacttest.Password)
			if !strings.HasPrefix(text, "/message?session_id="+session+" ") {
				t.Errorf("the endpoint on another origin was sent %q; want the POST, with the session", text)
			}
			if want := "POST to http://" + other + "/message?session_id=: status 500"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Start = %v; want it refused on the other origin's answer: %q", err, want)
			}
		})
	}
	for _, c := range []struct {
		name, failure string
		message       func(say func(string), w http.ResponseWriter, r *http.Request)
	}{
		{"refused", "status 400: refused", refusing},
		{"dropped", redact.ClassRequestFailed, dropping},
	} {
		t.Run(c.name, func(t *testing.T) {
			address := standIn(t, tokened, c.message)
			_, err := start(address)
			if err == nil {
				t.Fatal("Start succeeded against a server whose endpoint takes no POST")
			}
			redacttest.RequireNoPiece(t, "mcpbridge.relative-endpoint/"+c.name, err.Error(), redacttest.Password, session)
			if want := "POST to http://" + address + "/message?session_id=&access" + "_token=: " + c.failure; !strings.Contains(err.Error(), want) {
				t.Errorf("Start = %q; want it to name the resolved endpoint as the rule reads it and the failure: %q", err, want)
			}
		})
	}
	t.Run("unparsed", func(t *testing.T) {
		_, err := start(standIn(t, unparsedEndpoint, refusing))
		if err == nil {
			t.Fatal("Start succeeded against an endpoint that does not parse")
		}
		redacttest.RequireNoPiece(t, "mcpbridge.relative-endpoint/unparsed", err.Error())
		if want := "read endpoint event: endpoint URL in SSE event does not parse"; !strings.Contains(err.Error(), want) {
			t.Errorf("Start = %q; want %q", err, want)
		}
	})
	t.Run("twin", func(t *testing.T) {
		s, _ := start(standIn(t, tokened, refusing))
		if !strings.Contains(s.messageURL, redacttest.Password) || !strings.Contains(s.messageURL, redacttest.Token) {
			t.Errorf("the endpoint the bridge resolved, %q, carries not both the configured password and the token, so the refused row cannot see them printed", s.messageURL)
		}
		if _, err := http.Post(withUser("http://"+standIn(t, relative, dropping))+"/message?session_id="+session, "application/json", strings.NewReader(`{}`)); err == nil || !strings.Contains(err.Error(), redacttest.User) {
			t.Errorf("net/http's error for a dropped POST = %v; want it to quote the user, so the dropped row can see it", err)
		}
		var posts kept
		if response, err := http.Post(withUser("http://"+elsewhere(t, &posts))+"/message", "application/json", strings.NewReader(`{}`)); err == nil {
			_ = response.Body.Close()
		}
		if text := posts.text(); !strings.Contains(text, redacttest.Password) {
			t.Errorf("the other origin's stand-in kept %q of a POST with a Basic Authorization; want its password, so the elsewhere rows can see one sent", text)
		}
		if _, err := url.Parse(unparsedEndpoint("127.0.0.1:1")); err == nil || len(redacttest.Held(err.Error())) == 0 {
			t.Errorf("url.Parse's error for the unparsed endpoint = %v; want it to quote the endpoint's credentials", err)
		}
	})
}

// withUser is raw, an http URL, with redacttest's user information in it.
func withUser(raw string) string {
	return strings.Replace(raw, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
}

// unparsedEndpoint is an endpoint on host that url.Parse refuses (an invalid escape), with
// redacttest's credentials in its user information and its query.
func unparsedEndpoint(host string) string {
	return "http://" + redacttest.User + ":" + redacttest.Password + "@" + host + "/%zz?access" + "_token=" + redacttest.Token
}

// kept is the lines a stand-in keeps of the requests it is sent: each one's request URI and the user
// and password of its Basic Authorization.
type kept struct {
	mu      sync.Mutex
	entries []string
}

func (k *kept) keep(r *http.Request) {
	user, password, _ := r.BasicAuth()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries = append(k.entries, r.RequestURI+" "+user+":"+password)
}

func (k *kept) lines() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.entries...)
}

func (k *kept) text() string { return strings.Join(k.lines(), "\n") }

// standIn serves, until t ends, an MCP server over SSE whose endpoint event names endpoint(host),
// host being the address the connect was sent to, and whose /message route is message, which can
// say a message event on the open stream. It answers the server's address (host:port).
func standIn(t *testing.T, endpoint func(host string) string, message func(say func(string), w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	return standInAt(t, "", nil, endpoint, message)
}

// standInAt is standIn with its routes under prefix (prefix/sse, prefix/message), keeping each SSE
// connect, and each request that reaches neither route (answered 404), in connects where connects
// is not nil.
func standInAt(t *testing.T, prefix string, connects *kept, endpoint func(host string) string, message func(say func(string), w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	var (
		mu     sync.Mutex
		stream http.ResponseWriter
	)
	say := func(data string) {
		mu.Lock()
		defer mu.Unlock()
		if stream != nil {
			fmt.Fprintf(stream, "event: message\ndata: %s\n\n", data)
			stream.(http.Flusher).Flush()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/sse", func(w http.ResponseWriter, r *http.Request) {
		if connects != nil {
			connects.keep(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		mu.Lock()
		stream = w
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint(r.Host))
		w.(http.Flusher).Flush()
		mu.Unlock()
		<-r.Context().Done()
		mu.Lock()
		stream = nil
		mu.Unlock()
	})
	mux.HandleFunc(prefix+"/message", func(w http.ResponseWriter, r *http.Request) { message(say, w, r) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if connects != nil {
			connects.keep(r)
		}
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server.Listener.Addr().String()
}

// handshake is a /message route that keeps each POST in posts and answers initialize on the
// stream, as an MCP server does.
func handshake(posts *kept) func(say func(string), w http.ResponseWriter, r *http.Request) {
	return func(say func(string), w http.ResponseWriter, r *http.Request) {
		posts.keep(r)
		var request jsonrpcRequest
		if json.NewDecoder(r.Body).Decode(&request) == nil && request.Method == "initialize" {
			say(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"stand-in","version":"1.0.0"}}}`, request.ID))
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// refusing is a /message route that refuses every POST with 400.
func refusing(_ func(string), w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "refused", http.StatusBadRequest)
}

// dropping is a /message route that closes the connection of every POST, unanswered.
func dropping(_ func(string), w http.ResponseWriter, _ *http.Request) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// elsewhere serves, until t ends, a stand-in on an origin of its own that keeps each request it is
// sent in posts and answers 500, and answers its address (host:port).
func elsewhere(t *testing.T, posts *kept) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.keep(r)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.Listener.Addr().String()
}
