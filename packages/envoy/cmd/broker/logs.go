// packages/envoy/cmd/broker/logs.go
// The broker's own logging: the one handler every line of slog and the log package goes through,
// and the AWS SDK's logger, which writes to stderr with a logger of its own unless it is given one.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	smithylogging "github.com/aws/smithy-go/logging"

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
// call line's fields are, and each placeholder masked.
func setUpLogging(stderr io.Writer) {
	airlockproxy.SetDefaultLog(logging.NewCloudHandler(stderr))
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
