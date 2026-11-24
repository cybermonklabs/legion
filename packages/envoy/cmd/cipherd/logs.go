// packages/envoy/cmd/cipherd/logs.go
// The broker's own logging: the one handler every line of slog and the log package goes through,
// the placeholder mask under it, and the loggers of the AWS SDK, grpc and OpenTelemetry, each of
// which writes to stderr with a logger of its own unless it is given one.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/logmask"
	"github.com/sjawhar/envoy/internal/logging"
)

// setUpLogging is the first thing main does, so a subcommand that opens the store logs the same
// shape as the server. Every line slog and the log package write is one JSON object on stderr,
// which Cloud Logging reads as an entry with its own severity, message and fields
// (logging.NewCloudHandler), written through the placeholder mask (logmask.Writer), which masks
// the bytes the handler writes: the message, every key and group name, and every value in the
// form the handler wrote it. The log package writes through the same handler (logmask.SetDefault):
// a line a library writes with it - net/http's transport logs what a service sent on an idle
// connection with log.Printf, which no ErrorLog reaches - has the bytes it quotes from a peer
// written as a fixed phrase and its first line cut as a call line's fields are. grpc and
// OpenTelemetry keep loggers of their own on stderr, which write past all of that, so both are
// pointed at the default logger too (grpcLog, and logr's slog handler): grpc's before any gRPC
// call, as grpclog asks.
func setUpLogging(stderr io.Writer) {
	logmask.SetDefault(logging.NewCloudHandler(logmask.Writer(stderr)))
	grpclog.SetLoggerV2(grpcLog{})
	otel.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
}

// grpcMessage is the message of a line grpc writes (grpcLog).
const grpcMessage = "broker: grpc"

// grpcLog is grpc's logger once setUpLogging has run: each line a slog line of the default
// logger, with grpc's text as its text field, without the newline its *ln calls end it with,
// which every component logger of grpc's makes, cut to answer.FieldBytes and masked as every
// line is. grpc's own default writes its errors to stderr and drops its infos and warnings, so
// the infos and warnings here are DEBUG, which the broker's INFO handler drops in the same way,
// its errors ERROR, and its verbose lines none (V answers false past level 0, as grpc's default
// does with no GRPC_GO_LOG_VERBOSITY_LEVEL). A fatal line is written and then ends the process, as
// grpc's own Fatal does.
type grpcLog struct{}

func (grpcLog) line(level slog.Level, text string) {
	slog.Log(context.Background(), level, grpcMessage, answer.ClipFields([]any{"text", strings.TrimSuffix(text, "\n")})...)
}

func (l grpcLog) Info(args ...any)   { l.line(slog.LevelDebug, fmt.Sprint(args...)) }
func (l grpcLog) Infoln(args ...any) { l.line(slog.LevelDebug, fmt.Sprintln(args...)) }
func (l grpcLog) Infof(format string, args ...any) {
	l.line(slog.LevelDebug, fmt.Sprintf(format, args...))
}
func (l grpcLog) Warning(args ...any)   { l.line(slog.LevelDebug, fmt.Sprint(args...)) }
func (l grpcLog) Warningln(args ...any) { l.line(slog.LevelDebug, fmt.Sprintln(args...)) }
func (l grpcLog) Warningf(format string, args ...any) {
	l.line(slog.LevelDebug, fmt.Sprintf(format, args...))
}
func (l grpcLog) Error(args ...any)   { l.line(slog.LevelError, fmt.Sprint(args...)) }
func (l grpcLog) Errorln(args ...any) { l.line(slog.LevelError, fmt.Sprintln(args...)) }
func (l grpcLog) Errorf(format string, args ...any) {
	l.line(slog.LevelError, fmt.Sprintf(format, args...))
}
func (grpcLog) V(level int) bool { return level <= 0 }

func (l grpcLog) Fatal(args ...any) {
	l.line(slog.LevelError, fmt.Sprint(args...))
	os.Exit(1)
}

func (l grpcLog) Fatalln(args ...any) {
	l.line(slog.LevelError, fmt.Sprintln(args...))
	os.Exit(1)
}

func (l grpcLog) Fatalf(format string, args ...any) {
	l.line(slog.LevelError, fmt.Sprintf(format, args...))
	os.Exit(1)
}
