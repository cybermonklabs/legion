package mcpbridge

import (
	"bufio"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// An MCP server's words - a JSON-RPC error's message, a notification's URI, a line its child writes
// on standard error - reach the bridge's errors and log lines only as the rule for remote text
// reads them: each URL with no user information and no query or fragment value. These rows read
// each print site's final output.

// capturedLog is what the log package writes from now until t ends, each line without its date
// and time.
func capturedLog(t *testing.T) *logtest.Buffer {
	t.Helper()
	logtest.Keep(t)
	logged := new(logtest.Buffer)
	log.SetOutput(logged)
	log.SetFlags(0)
	return logged
}

// stdioConfig is a managed server named whatsapp that runs bin, the mock MCP server, with env.
func stdioConfig(bin string, env map[string]string) ServerConfig {
	return ServerConfig{Name: "whatsapp", Transport: "stdio", Command: []string{bin}, Env: env}
}

// TestAnRPCErrorPrintsTheServersWordsAsTheRuleReadsThem: an MCP server refuses the handshake with a
// JSON-RPC error whose message quotes the probe's URL, over HTTP (http) and from a managed child's
// standard output (stdio). Start's error names the refusal's code and its message as the rule reads
// it, with no user information and no query value. Captures mcpbridge.rpc-message/<case>.
func TestAnRPCErrorPrintsTheServersWordsAsTheRuleReadsThem(t *testing.T) {
	const want = "initialize whatsapp: rpc error -32600: " + redacttest.Shown
	check := func(t *testing.T, capture string, start func() error) {
		t.Helper()
		err := start()
		if err == nil {
			t.Fatal("Start succeeded against a server that refuses the handshake")
		}
		redacttest.RequireNone(t, capture, err.Error())
		if err.Error() != want {
			t.Errorf("Start = %q; want the refusal's code and its words as the rule reads them: %q", err, want)
		}
	}
	t.Run("http", func(t *testing.T) {
		mock := newMockHTTPMCPServer(func(m *mockHTTPMCPServer) { m.failInitialize, m.refusal = true, redacttest.Words })
		defer mock.close()
		check(t, "mcpbridge.rpc-message/http", NewHTTPServer(httpServerConfig(mock.url()), nil).Start)
	})
	t.Run("stdio", func(t *testing.T) {
		env := map[string]string{"MOCK_MCP_INITIALIZE_ERROR": redacttest.Words}
		check(t, "mcpbridge.rpc-message/stdio", NewManagedServer(stdioConfig(buildMockBinary(t), env), nil).Start)
	})
}

// TestAChildsStandardErrorPrintsAsTheRuleReadsIt: a managed server's child writes a line that
// quotes the probe's URL on its standard error, then a line that holds no URL. What reaches the
// bridge's standard error, where the child's own writes and the bridge's log both go, holds the
// first line as the rule reads it, named by its server, and none of the credentials; the second
// prints as the child wrote it. Captures mcpbridge.child-stderr.
func TestAChildsStandardErrorPrintsAsTheRuleReadsIt(t *testing.T) {
	bin := buildMockBinary(t)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("make a pipe for the bridge's standard error: %v", err)
	}
	defer read.Close()
	var captured logtest.Buffer
	go io.Copy(&captured, read)
	restoreLog := logtest.Keep(t)
	stderr := os.Stderr
	restore := func() {
		os.Stderr = stderr
		restoreLog()
	}
	defer restore()
	os.Stderr = write
	log.SetOutput(write)
	log.SetFlags(0)

	s := NewManagedServer(stdioConfig(bin, map[string]string{"MOCK_MCP_STDERR": redacttest.Words}), nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	testwait.Eventually(t, "the child's last line of standard error", func() bool {
		return strings.Contains(captured.String(), "mock: stderr done\n")
	})
	s.Stop()
	restore()
	write.Close()

	text := captured.String()
	redacttest.RequireNone(t, "mcpbridge.child-stderr", text)
	for _, want := range []string{"mcp-bridge: whatsapp: stderr: " + redacttest.Shown + "\n", "mcp-bridge: whatsapp: stderr: mock: stderr done\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the bridge's standard error is %q; want it to hold %q", text, want)
		}
	}
}

// failedRead is a reader whose every read fails with err.
type failedRead struct{ err error }

func (r failedRead) Read([]byte) (int, error) { return 0, r.err }

// TestAManagedServersReadErrorNamesTheReadAlone: the read of a managed server's child's standard
// output fails with an error that quotes the probe's URL, as an *fs.PathError quotes the path it
// read. The line the server logs names the read and the system's reason, and none of the error's
// own text. Captures mcpbridge.read-error/stdio.
func TestAManagedServersReadErrorNamesTheReadAlone(t *testing.T) {
	logged := capturedLog(t)
	s := NewManagedServer(ServerConfig{Name: "whatsapp"}, nil)
	s.scanner = bufio.NewScanner(failedRead{&fs.PathError{Op: "read", Path: redacttest.Code, Err: syscall.EIO}})
	s.readLoop()
	text := logged.String()
	redacttest.RequireNone(t, "mcpbridge.read-error/stdio", text)
	if want := "mcp-bridge: whatsapp: read error: read: " + syscall.EIO.Error() + "\n"; text != want {
		t.Errorf("the server logged %q; want %q", text, want)
	}
}

// TestRenderTopicNamesTheURIAsTheRuleReadsIt: a notification's URI that the server's pattern does
// not match quotes the probe's URL. RenderTopic's error names the URI as the rule reads it, and the
// pattern. Captures mcpbridge.render-topic.
func TestRenderTopicNamesTheURIAsTheRuleReadsIt(t *testing.T) {
	cfg := httpServerConfig("http://127.0.0.1:1")
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	_, err := cfg.RenderTopic(redacttest.Words)
	if err == nil {
		t.Fatal("RenderTopic rendered a URI its pattern does not match")
	}
	redacttest.RequireNone(t, "mcpbridge.render-topic", err.Error())
	if want := `uri "` + redacttest.Shown + `" does not match pattern ` + cfg.URIPattern; err.Error() != want {
		t.Errorf("RenderTopic = %q; want %q", err, want)
	}
}

// TestADecodeErrorNamesTheMemberAndQuotesNoValue: an MCP server sends a message, a notification's
// parameters and a read's result, each with a member that does not decode and holds the probe. The
// line the session logs, and the error ReadResource answers, name the member and the type it
// decodes into, and quote none of what the server sent (redact.DecodeError). encoding/json quotes
// no string value it fails to decode into these types, so these rows hold the shape of what
// prints, not a leak. Captures mcpbridge.decode/<case>.
func TestADecodeErrorNamesTheMemberAndQuotesNoValue(t *testing.T) {
	probe := jsonString(redacttest.Words)
	started := func(t *testing.T, option func(*mockHTTPMCPServer)) *HTTPServer {
		t.Helper()
		mock := newMockHTTPMCPServer(func(m *mockHTTPMCPServer) { m.notifyAfterSubscribe = false }, option)
		t.Cleanup(mock.close)
		s := NewHTTPServer(httpServerConfig(mock.url()), nil)
		if err := s.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(s.Stop)
		return s
	}
	for _, row := range []struct{ name, message, names string }{
		{"message", `{"jsonrpc":"2.0","id":` + probe + `}`, "mcp-bridge: whatsapp: invalid JSON from server: the answer's \"id\" is a JSON string where a mcpbridge.jsonrpcResponse holds a int"},
		{"params", `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":` + probe + `}`, "mcp-bridge: whatsapp: invalid notification params: the answer's value is a JSON string where a mcpbridge.notificationParams holds a mcpbridge.notificationParams"},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := started(t, func(*mockHTTPMCPServer) {})
			logged := capturedLog(t)
			s.session.handleMessage([]byte(row.message))
			text := logged.String()
			redacttest.RequireNone(t, "mcpbridge.decode/"+row.name, text)
			if text != row.names+"\n" {
				t.Errorf("the session logged %q; want %q", text, row.names+"\n")
			}
		})
	}
	t.Run("result", func(t *testing.T) {
		s := started(t, func(m *mockHTTPMCPServer) { m.readResult = `{"contents":` + probe + `}` })
		_, err := s.ReadResource("whatsapp://messages/15551234567/5551234567@s.whatsapp.net")
		if err == nil {
			t.Fatal("ReadResource decoded a result whose contents are no list")
		}
		redacttest.RequireNone(t, "mcpbridge.decode/result", err.Error())
		if want := "parse read result: the answer's \"contents\" is a JSON string where a mcpbridge.readResult holds a list"; err.Error() != want {
			t.Errorf("ReadResource = %q; want %q", err, want)
		}
	})
}
