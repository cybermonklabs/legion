package main

import (
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	smithylogging "github.com/aws/smithy-go/logging"
	"github.com/go-logr/stdr"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/logmask"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

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
// OpenTelemetry goes through, one subtest for each route, each reading the lines written while
// it ran. A placeholder written through any of them is written masked, in a value, a key and a
// group's name alike; a line of the log package is a library's (logmask.StandardLog): the bytes it
// quotes from a peer hidden, its first line cut to 256 bytes with no date before it, and the rest
// of the write its stack; the AWS SDK's line is a WARNING of the broker's own shape, cut as a call
// line's fields are, and its debug line is not written; grpc's error is an ERROR, cut as a call
// line's fields are and with no newline at its end, its info and its warning are not written, and
// its V is true at level 0 and false past it; and OpenTelemetry's error is an ERROR.
func TestTheBrokersLogMasksWhatALibraryWrites(t *testing.T) {
	placeholder := placeholder.Prefix + "ZzYyXxWwVvUuTtSsRrQqPpOoNnMmLlKk"
	out := &logtest.Buffer{}
	logtest.Keep(t)
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

	// during is the lines written to the broker's log while write ran, each a JSON object of its
	// own, none holding the placeholder in any case.
	during := func(t *testing.T, write func()) (string, []map[string]any) {
		t.Helper()
		before := out.Len()
		write()
		written := out.String()[before:]
		if strings.Contains(strings.ToLower(written), strings.ToLower(placeholder)) {
			t.Fatalf("the broker's log holds the placeholder: %q", written)
		}
		if written == "" {
			return "", nil
		}
		return written, logtest.JSONLines(t, written)
	}
	message := func(want string) func(map[string]any) bool {
		return func(line map[string]any) bool { return line["message"] == want }
	}

	t.Run("slog", func(t *testing.T) {
		written, lines := during(t, func() {
			slog.Info("broker: a planted line", "planted", placeholder, placeholder, "a key", slog.Group(placeholder, "k", "v"))
		})
		planted, ok := logtest.LineWhere(lines, message("broker: a planted line"))
		if !ok || planted["severity"] != "INFO" || planted["planted"] != logmask.Mark || planted[logmask.Mark] == nil {
			t.Errorf("the default logger's line = %v; want its own message, at INFO, with the value, the key and the group's name masked (log: %q)", planted, written)
		}
	})

	// The log package's route is the one net/http's transport writes what a service sent on an
	// idle connection on, with log.Printf, and a bound service is sent the real value the swap put
	// in. slog.SetDefault alone points the log package at a writer of its own, which writes each
	// write as one INFO line of the same masked handler; only logmask.StandardLog hides the bytes a
	// line quotes from a peer, cuts its first line and keeps the rest as its stack, so the rows
	// read those three.
	t.Run("the log package", func(t *testing.T) {
		value := strings.Join([]string{"sk", "live", "REAL", "VALUE"}, "-") + strings.Repeat("Q", 300)
		long := "a library's first line naming " + placeholder + " " + strings.Repeat("x", 300)
		written, lines := during(t, func() {
			log.Printf("Unsolicited response received on idle HTTP channel starting with %q; err=%v", value, nil)
			log.Print(long + "\nsecond line " + placeholder)
		})
		if strings.Contains(written, "REAL-VALUE") {
			t.Errorf("the broker's log holds the value a service sent: %q", written)
		}
		unsolicited := "Unsolicited response received on idle HTTP channel starting with " + logmask.PeerBytesShown
		if len(lines) != 2 || lines[0]["message"] != unsolicited || lines[0]["severity"] != "INFO" || lines[0]["stack"] != nil {
			t.Errorf("the transport's line is %v; want one line at INFO, %q, with no date before it and no stack (log: %q)", lines, unsolicited, written)
		}
		cut := strings.Replace(answer.Clip(long), placeholder, logmask.Mark, 1)
		if len(lines) != 2 || lines[1]["message"] != cut || lines[1]["stack"] != "second line "+logmask.Mark {
			t.Errorf("the write of two lines is %v; want its first line cut to %d bytes and masked, %q, and the second, masked, as its stack (log: %q)", lines, answer.FieldBytes, cut, written)
		}
	})

	t.Run("the AWS SDK", func(t *testing.T) {
		isolateAWS(t)
		awsCfg, err := loadAWSConfig(t.Context())
		if err != nil {
			t.Fatalf("load the AWS configuration: %v", err)
		}
		written, lines := during(t, func() {
			awsCfg.Logger.Logf(smithylogging.Warn, "falling back to IMDSv1: %v", errors.New("token "+placeholder))
			awsCfg.Logger.Logf(smithylogging.Warn, "%s", strings.Repeat("y", 300))
			awsCfg.Logger.Logf(smithylogging.Debug, "a debug line of the SDK's")
		})
		aws := func(text string) func(map[string]any) bool {
			return func(line map[string]any) bool { return line["message"] == awsSDKMessage && line["text"] == text }
		}
		if imds, ok := logtest.LineWhere(lines, aws("falling back to IMDSv1: token "+logmask.Mark)); !ok || imds["severity"] != "WARNING" {
			t.Errorf("the AWS SDK's warning = %v; want %q at WARNING, its text masked (log: %q)", imds, awsSDKMessage, written)
		}
		if _, ok := logtest.LineWhere(lines, aws(strings.Repeat("y", answer.FieldBytes))); !ok {
			t.Errorf("no line of the AWS SDK is its long warning cut to %d bytes: %q", answer.FieldBytes, written)
		}
		if strings.Contains(written, "a debug line of the SDK's") {
			t.Errorf("the broker's log holds the AWS SDK's debug line, written at DEBUG, which its INFO handler drops: %q", written)
		}
	})

	t.Run("grpc's errors", func(t *testing.T) {
		longGRPC := "a long grpc error: " + strings.Repeat("z", 300)
		written, lines := during(t, func() {
			grpclog.Errorf("a grpc error of %s", placeholder)
			grpclog.Errorf("%s", longGRPC)
			// grpc's component loggers write through the *ln calls, whose text ends in a newline.
			grpclog.Errorln("a grpc error line", "of two words")
		})
		grpc := func(text string) func(map[string]any) bool {
			return func(line map[string]any) bool { return line["message"] == grpcMessage && line["text"] == text }
		}
		if line, ok := logtest.LineWhere(lines, grpc("a grpc error of "+logmask.Mark)); !ok || line["severity"] != "ERROR" {
			t.Errorf("grpc's error line = %v; want %q at ERROR, its text masked (log: %q)", line, grpcMessage, written)
		}
		if line, ok := logtest.LineWhere(lines, grpc(longGRPC[:answer.FieldBytes])); !ok || line["severity"] != "ERROR" {
			t.Errorf("grpc's long error line = %v; want %q at ERROR, its text cut to %d bytes (log: %q)", line, grpcMessage, answer.FieldBytes, written)
		}
		if _, ok := logtest.LineWhere(lines, grpc("a grpc error line of two words")); !ok {
			t.Errorf("no grpc line is its Errorln's text without the newline that ends it: %q", written)
		}
	})

	t.Run("grpc's infos, warnings and verbosity", func(t *testing.T) {
		written, _ := during(t, func() {
			grpclog.Warningf("a grpc warning line")
			grpclog.Infof("a grpc info line")
		})
		if written != "" {
			t.Errorf("the broker's log holds grpc's info or warning line, each written at DEBUG, which its INFO handler drops: %q", written)
		}
		if !grpclog.V(0) || grpclog.V(1) {
			t.Errorf("grpc's V(0), V(1) = %v, %v; want true, false, as grpc's default answers with no GRPC_GO_LOG_VERBOSITY_LEVEL", grpclog.V(0), grpclog.V(1))
		}
	})

	t.Run("OpenTelemetry", func(t *testing.T) {
		// Its global state logs an error when its tracer provider is set to the one it holds.
		written, lines := during(t, func() { otel.SetTracerProvider(otel.GetTracerProvider()) })
		if _, ok := logtest.LineWhere(lines, func(line map[string]any) bool {
			return line["severity"] == "ERROR" && strings.Contains(stringOf(line["message"]), "tracer provider")
		}); !ok {
			t.Errorf("no ERROR line of OpenTelemetry's says its tracer provider was set to its current value: %q", written)
		}
	})
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
