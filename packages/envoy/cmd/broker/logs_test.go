package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	smithylogging "github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestTheBrokersLogMasksWhatALibraryWrites: the broker's own logging (setUpLogging), which main
// sets up first, is what every line of slog, the log package and the AWS SDK goes through. A
// placeholder written through any of them is written masked; a library's line has no date of its
// own, its first line is cut to 256 bytes as its message and the rest is its stack; a run of bytes
// net/http's transport quotes from a service - here an answer's bytes on an idle connection, the
// line no ErrorLog reaches - is written as a fixed phrase; and the AWS SDK's line is a WARNING of
// the broker's own shape.
func TestTheBrokersLogMasksWhatALibraryWrites(t *testing.T) {
	placeholder := requests.PlaceholderPrefix + "ZzYyXxWwVvUuTtSsRrQqPpOoNnMmLlKk"
	const mark, shown = "[placeholder]", `"[service bytes not shown]"`
	out := &logtest.Buffer{}
	logtest.Swap(t, slog.NewJSONHandler(io.Discard, nil))
	setUpLogging(out)

	// The default logger, and the log package with a first line past the cut and a second line.
	slog.Info("broker: a planted line", "planted", placeholder)
	first := placeholder + " " + strings.Repeat("x", 300)
	log.Print(first + "\nsecond line " + placeholder)

	// A service that answers two bytes and sends the placeholder after them in the same write:
	// net/http's transport reads the answer, finds the rest on the idle connection, and logs it
	// with log.Printf.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 4096)
		if _, err := conn.Read(buffer); err != nil {
			return
		}
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"+placeholder)
		io.Copy(io.Discard, conn)
	}()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	response, err := (&http.Client{Transport: transport, Timeout: 20 * time.Second}).Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET the service: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "Unsolicited response"); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("net/http's transport logged no unsolicited response: %q", out.String())
		}
	}

	// The AWS SDK, through the configuration the broker loads, with no file or instance of
	// AWS's own to read.
	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	awsCfg, err := loadAWSConfig(t.Context())
	if err != nil {
		t.Fatalf("load the AWS configuration: %v", err)
	}
	awsCfg.Logger.Logf(smithylogging.Warn, "falling back to IMDSv1: %v", errors.New("token "+placeholder))

	written := out.String()
	if strings.Contains(strings.ToLower(written), strings.ToLower(placeholder)) {
		t.Fatalf("the broker's log holds the placeholder: %q", written)
	}
	lines := map[string]map[string]any{}
	for _, text := range strings.Split(strings.TrimSpace(written), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("a line of the broker's log is no JSON object: %q: %v", text, err)
		}
		message, _ := line["message"].(string)
		lines[message] = line
	}
	planted, ok := lines["broker: a planted line"]
	if !ok || planted["severity"] != "INFO" || planted["planted"] != mark {
		t.Errorf("the default logger's line = %v; want its own message, at INFO, with the attribute masked (log: %q)", planted, written)
	}
	cut := mark + answer.Clip(first)[len(placeholder):]
	library, ok := lines[cut]
	if !ok || library["stack"] != "second line "+mark {
		t.Errorf("no line of the log package is its first line cut to %d bytes and masked, with no date before it, and the rest as its stack: %q", answer.FieldBytes, written)
	}
	unsolicited := "Unsolicited response received on idle HTTP channel starting with " + shown + "; err=<nil>"
	if _, ok := lines[unsolicited]; !ok {
		t.Errorf("no line of net/http's transport is %q: %q", unsolicited, written)
	}
	aws, ok := lines[awsSDKMessage]
	if !ok || aws["severity"] != "WARNING" || aws["text"] != "falling back to IMDSv1: token "+mark {
		t.Errorf("the AWS SDK's line = %v; want %q at WARNING, its text masked (log: %q)", aws, awsSDKMessage, written)
	}
}
