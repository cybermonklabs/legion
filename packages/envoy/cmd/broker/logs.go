// packages/envoy/cmd/broker/logs.go
// The broker's own logging: the one handler every line of slog and the log package goes through,
// and the loggers of the AWS SDK, grpc and OpenTelemetry, each of which writes to stderr with a
// logger of its own unless it is given one.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	smithylogging "github.com/aws/smithy-go/logging"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc/grpclog"

	"github.com/sjawhar/envoy/internal/broker/airlockproxy"
	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/logging"
)

// setUpLogging is the first thing main does, so a subcommand that opens the store logs the same
// shape as the server. Every line slog and the log package write is one JSON object on stderr,
// which Cloud Logging reads as an entry with its own severity, message and fields
// (logging.NewCloudHandler), and it is written through the airlock's placeholder mask
// (airlockproxy.SetDefaultLog): a line a library writes with the log package - net/http's
// transport logs what a service sent on an idle connection with log.Printf, which no ErrorLog
// reaches - has the bytes it quotes from a peer written as a fixed phrase, its first line cut as a
// call line's fields are, and each placeholder masked. grpc and OpenTelemetry keep loggers of
// their own on stderr, which write past all of that, so both are pointed at the default logger
// too (grpcLog, and logr's slog handler): grpc's before any gRPC call, as grpclog asks.
func setUpLogging(stderr io.Writer) {
	airlockproxy.SetDefaultLog(logging.NewCloudHandler(stderr))
	grpclog.SetLoggerV2(grpcLog{})
	otel.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
}

// grpcMessage is the message of a line grpc writes (grpcLog).
const grpcMessage = "broker: grpc"

// grpcLog is grpc's logger once setUpLogging has run: each line a slog line of the default
// logger, with grpc's text as its text field, cut to answer.FieldBytes and masked as every line
// is. grpc's own default writes its errors to stderr and drops its infos and warnings, so the
// infos and warnings here are DEBUG, which the broker's INFO handler drops in the same way, its
// errors ERROR, and its verbose lines none (V answers false past level 0, as grpc's default does
// with no GRPC_GO_LOG_VERBOSITY_LEVEL). A fatal line is written and then ends the process, as
// grpc's own Fatal does.
type grpcLog struct{}

func (grpcLog) line(level slog.Level, text string) {
	slog.Log(context.Background(), level, grpcMessage, answer.ClipFields([]any{"text", text})...)
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

// awsSDKMessage is the message of a line the AWS SDK writes (awsLog).
const awsSDKMessage = "broker: aws sdk"

// awsLog is the logger the AWS SDK writes through (loadAWSConfig). The SDK's default logger is a
// log.Logger of its own on os.Stderr, with its own date prefix, which no handler of the process
// reads; and with the AWS store or an IAM database the SDK does write, at WARN and without any
// ClientLogMode, for example when the instance metadata service falls back to IMDSv1 or a
// credential's expiry is extended. So each of its lines is a slog line of the default logger,
// at WARNING (DEBUG for the SDK's debug lines, which the broker's INFO handler drops), with the
// SDK's text as its text field, cut to answer.FieldBytes and masked as every line is.
type awsLog struct{}

func (awsLog) Logf(classification smithylogging.Classification, format string, v ...any) {
	level := slog.LevelWarn
	if classification == smithylogging.Debug {
		level = slog.LevelDebug
	}
	slog.Log(context.Background(), level, awsSDKMessage, answer.ClipFields([]any{"text", fmt.Sprintf(format, v...)})...)
}

// loadAWSConfig is the AWS configuration the broker loads (openStore), with awsLog as its logger.
func loadAWSConfig(ctx context.Context) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(awsLog{}))
}
