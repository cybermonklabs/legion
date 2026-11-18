package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	smithylogging "github.com/aws/smithy-go/logging"
	"github.com/go-logr/stdr"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/config"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// brokerLogLines is each line of the broker's log, decoded, failing t on a line that is no JSON
// object of its own.
func brokerLogLines(t *testing.T, written string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, text := range strings.Split(strings.TrimSpace(written), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil || line == nil {
			t.Fatalf("a line of the broker's log is no JSON object: %q: %v", text, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// lineWhere is the first line for which holds says true, and false when no line does.
func lineWhere(lines []map[string]any, holds func(map[string]any) bool) (map[string]any, bool) {
	for _, line := range lines {
		if holds(line) {
			return line, true
		}
	}
	return nil, false
}

// isolateAWS points the AWS SDK at no file and no instance of AWS's own for the rest of t, and
// empties every other AWS_ variable of the environment, each of which the SDK reads as unset when
// it is empty, so the configuration it loads reads nothing of the machine's.
func isolateAWS(t *testing.T) {
	t.Helper()
	for _, variable := range os.Environ() {
		if name, _, _ := strings.Cut(variable, "="); strings.HasPrefix(name, "AWS_") {
			t.Setenv(name, "")
		}
	}
	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// TestTheBrokersLogMasksWhatALibraryWrites: the broker's own logging (setUpLogging), which main
// sets up first, is what every line of slog, the log package, the AWS SDK, grpc and
// OpenTelemetry goes through. A placeholder written through any of them is written masked; a
// library's line has no date of its own, its first line is cut to 256 bytes as its message and
// the rest is its stack; a run of bytes net/http's transport quotes from a service - here an
// answer's bytes on an idle connection, the line no ErrorLog reaches - is written as a fixed
// phrase; the AWS SDK's line is a WARNING of the broker's own shape, cut as a call line's fields
// are, and its debug line is not written; grpc's error is an ERROR, cut as a call line's fields
// are, its info and its warning are not written, and its V is true at level 0 and false past it;
// and OpenTelemetry's error is an ERROR.
func TestTheBrokersLogMasksWhatALibraryWrites(t *testing.T) {
	placeholder := requests.PlaceholderPrefix + "ZzYyXxWwVvUuTtSsRrQqPpOoNnMmLlKk"
	const mark, shown = "[placeholder]", `"[service bytes not shown]"`
	out := &logtest.Buffer{}
	logtest.Swap(t, slog.NewJSONHandler(io.Discard, nil))
	// setUpLogging points grpc's and OpenTelemetry's global loggers at the broker's, and neither
	// library hands back the logger it held, so the test cannot put back what it found. It puts
	// back each library's own default for an environment with no GRPC_GO_LOG_* variable, built as
	// the library builds it: grpclog's init (errors to stderr, infos and warnings dropped,
	// verbosity 0, text lines) and OpenTelemetry's internal/global (stdr on a log.Logger of
	// stderr). Otherwise a later test's OpenTelemetry lines would go into this test's finished
	// buffer, and its grpc lines through grpcLog rather than grpc's own logger. Their stderr is
	// libraryStderr, os.Stderr but for the check below.
	libraryStderr := &redirectedStderr{}
	// The check, which runs once the defaults are back (cleanups run last-registered first) and
	// while the broker's handler is still the default: a grpc error and an OpenTelemetry error
	// reach each library's own default, and not the broker's log.
	t.Cleanup(func() {
		var stderr logtest.Buffer
		libraryStderr.point(&stderr)
		defer libraryStderr.point(nil)
		before := out.Len()
		grpclog.Errorf("a grpc error after the test")
		otel.SetTracerProvider(otel.GetTracerProvider())
		if late := out.String()[before:]; late != "" {
			t.Errorf("after the test, the broker's log took %q; want grpc's and OpenTelemetry's lines on their own loggers", late)
		}
		written := stderr.String()
		if !strings.Contains(written, "ERROR: ") || !strings.Contains(written, "a grpc error after the test") {
			t.Errorf("after the test, grpc's error did not reach grpc's own logger, which writes \"ERROR: \" and the text: %q", written)
		}
		if !strings.Contains(written, "Setting tracer provider to its current value") {
			t.Errorf("after the test, OpenTelemetry's error did not reach its own logger: %q", written)
		}
	})
	t.Cleanup(func() {
		grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, libraryStderr))
		otel.SetLogger(stdr.New(log.New(libraryStderr, "", log.LstdFlags|log.Lshortfile)))
	})
	setUpLogging(out)

	// The default logger, and the log package with a first line past the cut and a second line.
	slog.Info("broker: a planted line", "planted", placeholder)
	first := placeholder + " " + strings.Repeat("x", 300)
	log.Print(first + "\nsecond line " + placeholder)

	// A service that answers two bytes and sends the placeholder after them in the same write:
	// net/http's transport reads the answer, finds the rest on the idle connection, logs it with
	// log.Printf and then closes the connection, which is when the service hears from it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	closed := make(chan struct{})
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
		close(closed)
	}()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	response, err := (&http.Client{Transport: transport, Timeout: 20 * time.Second}).Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET the service: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Minute):
		t.Fatalf("net/http's transport never closed the connection whose answer ended in bytes after it: %q", out.String())
	}

	// The AWS SDK, through the configuration the broker loads: a warning holding the
	// placeholder, a warning past the cut, and a debug line.
	isolateAWS(t)
	awsCfg, err := loadAWSConfig(t.Context())
	if err != nil {
		t.Fatalf("load the AWS configuration: %v", err)
	}
	awsCfg.Logger.Logf(smithylogging.Warn, "falling back to IMDSv1: %v", errors.New("token "+placeholder))
	awsCfg.Logger.Logf(smithylogging.Warn, "%s", strings.Repeat("y", 300))
	awsCfg.Logger.Logf(smithylogging.Debug, "a debug line of the SDK's")

	// grpc, an error, an error past the cut, a warning and an info, and its verbosity; and
	// OpenTelemetry, whose global state logs an error when its tracer provider is set to the one
	// it already holds.
	grpclog.Errorf("a grpc error of %s", placeholder)
	longGRPC := "a long grpc error: " + strings.Repeat("z", 300)
	grpclog.Errorf("%s", longGRPC)
	grpclog.Warningf("a grpc warning line")
	grpclog.Infof("a grpc info line")
	if !grpclog.V(0) || grpclog.V(1) {
		t.Errorf("grpc's V(0), V(1) = %v, %v; want true, false, as grpc's default answers with no GRPC_GO_LOG_VERBOSITY_LEVEL", grpclog.V(0), grpclog.V(1))
	}
	otel.SetTracerProvider(otel.GetTracerProvider())

	written := out.String()
	if strings.Contains(strings.ToLower(written), strings.ToLower(placeholder)) {
		t.Fatalf("the broker's log holds the placeholder: %q", written)
	}
	lines := brokerLogLines(t, written)
	message := func(want string) func(map[string]any) bool {
		return func(line map[string]any) bool { return line["message"] == want }
	}
	if planted, ok := lineWhere(lines, message("broker: a planted line")); !ok || planted["severity"] != "INFO" || planted["planted"] != mark {
		t.Errorf("the default logger's line = %v; want its own message, at INFO, with the attribute masked (log: %q)", planted, written)
	}
	cut := mark + answer.Clip(first)[len(placeholder):]
	if library, ok := lineWhere(lines, message(cut)); !ok || library["stack"] != "second line "+mark {
		t.Errorf("no line of the log package is its first line cut to %d bytes and masked, with no date before it, and the rest as its stack: %q", answer.FieldBytes, written)
	}
	unsolicited := "Unsolicited response received on idle HTTP channel starting with " + shown + "; err=<nil>"
	if _, ok := lineWhere(lines, message(unsolicited)); !ok {
		t.Errorf("no line of net/http's transport is %q: %q", unsolicited, written)
	}
	aws := func(text string) func(map[string]any) bool {
		return func(line map[string]any) bool { return line["message"] == awsSDKMessage && line["text"] == text }
	}
	if imds, ok := lineWhere(lines, aws("falling back to IMDSv1: token "+mark)); !ok || imds["severity"] != "WARNING" {
		t.Errorf("the AWS SDK's warning = %v; want %q at WARNING, its text masked (log: %q)", imds, awsSDKMessage, written)
	}
	if _, ok := lineWhere(lines, aws(strings.Repeat("y", answer.FieldBytes))); !ok {
		t.Errorf("no line of the AWS SDK is its long warning cut to %d bytes: %q", answer.FieldBytes, written)
	}
	if strings.Contains(written, "a debug line of the SDK's") || strings.Contains(written, "a grpc info line") || strings.Contains(written, "a grpc warning line") {
		t.Errorf("the broker's log holds the AWS SDK's debug line or grpc's info or warning line, each written at DEBUG, which its INFO handler drops: %q", written)
	}
	grpc := func(text string) func(map[string]any) bool {
		return func(line map[string]any) bool { return line["message"] == grpcMessage && line["text"] == text }
	}
	if line, ok := lineWhere(lines, grpc("a grpc error of "+mark)); !ok || line["severity"] != "ERROR" {
		t.Errorf("grpc's error line = %v; want %q at ERROR, its text masked (log: %q)", line, grpcMessage, written)
	}
	if line, ok := lineWhere(lines, grpc(longGRPC[:answer.FieldBytes])); !ok || line["severity"] != "ERROR" {
		t.Errorf("grpc's long error line = %v; want %q at ERROR, its text cut to %d bytes (log: %q)", line, grpcMessage, answer.FieldBytes, written)
	}
	if _, ok := lineWhere(lines, func(line map[string]any) bool {
		return line["severity"] == "ERROR" && strings.Contains(stringOf(line["message"]), "tracer provider")
	}); !ok {
		t.Errorf("no ERROR line of OpenTelemetry's says its tracer provider was set to its current value: %q", written)
	}
}

// stringOf is v when it is a string, and "" otherwise.
func stringOf(v any) string {
	text, _ := v.(string)
	return text
}

// redirectedStderr writes to os.Stderr, or to the writer a check points it at for a moment.
type redirectedStderr struct {
	mu sync.Mutex
	to io.Writer
}

func (r *redirectedStderr) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.to != nil {
		return r.to.Write(p)
	}
	return os.Stderr.Write(p)
}

// point sends what r is written to w, or to os.Stderr again when w is nil.
func (r *redirectedStderr) point(w io.Writer) {
	r.mu.Lock()
	r.to = w
	r.mu.Unlock()
}

// TestOpenStoreLoadsTheAWSConfigurationWithTheBrokersLogger: the AWS configuration openStore
// loads for the AWS store, or an IAM database, is the one with awsLog as its logger, whose lines
// are the broker's own rather than the SDK's own log.Logger on stderr.
func TestOpenStoreLoadsTheAWSConfigurationWithTheBrokersLogger(t *testing.T) {
	isolateAWS(t)
	st, awsCfg, err := openStore(t.Context(), config.Config{DatabaseURL: storetest.URL(t)}, true)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, ok := awsCfg.Logger.(awsLog); !ok {
		t.Fatalf("openStore's AWS configuration logs through %T; want awsLog, the broker's own", awsCfg.Logger)
	}
}
