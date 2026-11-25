package mcpbridge

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects"
)

// HTTPServer connects to a pre-existing MCP server over HTTP/SSE transport.
type HTTPServer struct {
	cfg        ServerConfig
	session    *session
	messageURL string
	sseResp    *http.Response
	state      atomic.Int32
	onNotify   func(uri string)
	stopCh     chan struct{}
	closeOnce  sync.Once
	exitCh     chan error
	stopped    atomic.Bool
}

// NewHTTPServer creates an HTTP/SSE server from config. Call Start() to connect.
func NewHTTPServer(cfg ServerConfig, onNotify func(uri string)) *HTTPServer {
	s := &HTTPServer{
		cfg:      cfg,
		onNotify: onNotify,
		stopCh:   make(chan struct{}),
		exitCh:   make(chan error, 1),
	}
	s.state.Store(int32(StateStarting))
	return s
}

// Name returns the server's configured name.
func (s *HTTPServer) Name() string {
	return s.cfg.Name
}

// State returns the current server state.
func (s *HTTPServer) State() ServerState {
	return ServerState(s.state.Load())
}

// Stopped returns true if Stop() was called intentionally (as opposed to SSE failure).
func (s *HTTPServer) Stopped() bool {
	return s.stopped.Load()
}

// Start connects to the MCP server's SSE endpoint, performs the handshake, and subscribes.
func (s *HTTPServer) Start() error {
	// 1. Connect to {url}/sse: the configured URL with sse joined onto its path and its query kept
	// (urljoin.Join), where /sse appended to the URL's text would sit inside a query. The
	// configured URL can carry a credential, in its user information, which net/http sends as the
	// request's Basic Authorization, or in a query value: url.Parse's error quotes it, so a URL that
	// does not parse or names no host, or whose SSE URL does not parse again (url.URL.String can
	// write text url.Parse refuses, such as an IPv6 zone escape), is refused in the bridge's own
	// words. The client follows a redirect only on the server's own origin (redirects.SameOrigin):
	// the server is any MCP server a person configures, which may move its stream on its own origin
	// (a trailing slash, say).
	joined, err := urljoin.Join(s.cfg.URL, "sse")
	if err != nil {
		return fmt.Errorf("create SSE request: the configured URL does not parse")
	}
	sseURL := joined.String()
	shown := redact.URL(sseURL)
	req, err := http.NewRequest("GET", sseURL, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: the SSE URL does not parse")
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := redirects.SameOrigin(http.DefaultClient).Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect to %s: %w", shown, redact.Cause(err, sseURL))
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("SSE connect to %s: status %d", shown, resp.StatusCode)
	}
	s.sseResp = resp

	// 2. Read the first SSE event (type "endpoint") to get the message POST URL. The event stream
	// is data the session reads for its whole life (redact.StreamAnswer), bounded by that life
	// and not by its size.
	scanner := bufio.NewScanner(redact.StreamAnswer(resp, math.MaxInt64))
	messageURL, err := readEndpointEvent(scanner, req.URL)
	if err != nil {
		resp.Body.Close()
		return fmt.Errorf("read endpoint event: %w", err)
	}
	s.messageURL = messageURL

	// 3. Create session with HTTP POST send function. The endpoint can carry the session in its
	// query and a credential in its user information (the POST's Basic Authorization): its own, or
	// the configured URL's on the configured URL's origin. Whatever answers can quote either in a
	// Location; a followed redirect also sends the endpoint, query included, as the next request's
	// Referer, and a followed 307 or 308 sends the message again.
	poster := redirects.SameOrigin(http.DefaultClient)
	s.session = newSession(s.cfg.Name, func(data []byte) error {
		postResp, postErr := poster.Post(s.messageURL, "application/json", bytes.NewReader(data))
		if postErr != nil {
			return fmt.Errorf("POST to %s: %w", redact.Field(s.messageURL), redact.Cause(postErr, s.messageURL))
		}
		defer postResp.Body.Close()
		if postResp.StatusCode >= http.StatusMultipleChoices {
			said := redirectNotFollowed
			if !redirects.Status(postResp.StatusCode) {
				refused, _ := redact.ReadAnswer(postResp, 64<<10)
				said = refused.Words()
			}
			return fmt.Errorf("POST to %s: status %d: %s", redact.Field(s.messageURL), postResp.StatusCode, said)
		}
		return nil
	}, s.onNotify, s.stopCh)

	// 4. Start read loop for SSE events.
	go s.readLoop(scanner)

	// 5. Perform MCP handshake.
	if err := s.session.initialize(); err != nil {
		s.closeSSE()
		return fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}

	// 6. Subscribe to configured resources.
	for _, uri := range s.cfg.Resources {
		if err := s.session.subscribe(uri); err != nil {
			s.closeSSE()
			return fmt.Errorf("subscribe %s to %s: %w", s.cfg.Name, uri, err)
		}
	}

	s.state.Store(int32(StateReady))
	log.Printf("mcp-bridge: server %s ready (http)", s.cfg.Name)
	return nil
}

var redirectNotFollowed = redact.NewRemoteText("a redirect, which the bridge does not follow")

// Stop gracefully shuts down the SSE connection.
func (s *HTTPServer) Stop() {
	s.stopped.Store(true)
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.state.Store(int32(StateDead))
	s.closeSSE()
}

// WaitForExit blocks until the SSE connection is lost or intentionally closed.
func (s *HTTPServer) WaitForExit() error {
	return <-s.exitCh
}

// ReadResource calls resources/read via HTTP POST and returns the content.
func (s *HTTPServer) ReadResource(uri string) ([]resourceContent, error) {
	return s.session.readResource(uri)
}

func (s *HTTPServer) closeSSE() {
	if s.sseResp != nil {
		s.sseResp.Body.Close()
	}
}

func (s *HTTPServer) readLoop(scanner *bufio.Scanner) {
	for {
		eventType, data, err := readSSEEvent(scanner)
		if err != nil {
			s.state.Store(int32(StateDead))
			if s.stopped.Load() {
				s.exitCh <- nil
			} else {
				s.exitCh <- err
			}
			return
		}
		if eventType == "message" && len(data) > 0 {
			s.session.handleMessage(data)
		}
	}
}

// readEndpointEvent reads SSE events until it finds one with type "endpoint", and answers the
// endpoint it names as resolveEndpoint reads it against base, the SSE URL the bridge connected to.
// An endpoint that does not parse is refused in the bridge's own words: url.Parse's error quotes
// the endpoint, the server's words, user information and query included.
func readEndpointEvent(scanner *bufio.Scanner, base *url.URL) (string, error) {
	for {
		eventType, data, err := readSSEEvent(scanner)
		if err != nil {
			return "", fmt.Errorf("reading SSE: %w", err)
		}
		if eventType == "endpoint" {
			u := strings.TrimSpace(string(data))
			if u == "" {
				return "", fmt.Errorf("empty endpoint URL in SSE event")
			}
			ref, err := url.Parse(u)
			if err != nil {
				return "", fmt.Errorf("endpoint URL in SSE event does not parse")
			}
			return resolveEndpoint(base, ref)
		}
	}
}

// resolveEndpoint is ref, the endpoint an SSE event names, resolved against base, the SSE URL
// (RFC 3986, section 5.2): the SSE transport lets a server name it by a relative reference, such
// as /messages?session_id=..., which takes base's scheme, host and user information.
func resolveEndpoint(base, ref *url.URL) (string, error) {
	endpoint := base.ResolveReference(ref)
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" {
		return "", fmt.Errorf("endpoint URL in SSE event is not an http or https URL with a host")
	}
	if endpoint.User == nil && redirects.SameOriginURL(endpoint, base) {
		endpoint.User = base.User
	}
	return endpoint.String(), nil
}

// readSSEEvent reads a single SSE event from the scanner.
// Returns event type (defaults to "message") and data payload.
func readSSEEvent(scanner *bufio.Scanner) (string, []byte, error) {
	var eventType string
	var dataLines [][]byte

	for scanner.Scan() {
		line := scanner.Text()

		// Empty line marks end of event.
		if line == "" {
			if len(dataLines) > 0 || eventType != "" {
				data := bytes.Join(dataLines, []byte("\n"))
				if eventType == "" {
					eventType = "message"
				}
				return eventType, data, nil
			}
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, []byte(strings.TrimPrefix(line, "data: ")))
		} else if line == "data:" {
			dataLines = append(dataLines, []byte{})
		} else if strings.HasPrefix(line, ":") {
			// Comment, ignore.
		}
	}

	if err := scanner.Err(); err != nil {
		return "", nil, err
	}
	return "", nil, io.EOF
}
