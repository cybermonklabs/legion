// Package awssdk is how the broker loads the AWS SDK's configuration: with the broker's own logger,
// for each part of the broker that calls AWS - the AWS secret store, the RDS IAM sign-in of its
// database.
package awssdk

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	smithylogging "github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/internal/cipher/answer"
)

// Message is the message of a line the AWS SDK writes (Log).
const Message = "broker: aws sdk"

// Log is the logger the AWS SDK writes through (LoadConfig). The SDK's default logger is a
// log.Logger of its own on os.Stderr, with its own date prefix, which no handler of the process
// reads; and with the AWS store or an IAM database the SDK does write, at WARN and without any
// ClientLogMode, for example when the instance metadata service falls back to IMDSv1 or a
// credential's expiry is extended. So each of its lines is a slog line of the default logger,
// at WARNING (DEBUG for the SDK's debug lines, which the broker's INFO handler drops), with the
// SDK's text as its text field, cut to answer.FieldBytes and masked as every line is.
type Log struct{}

func (Log) Logf(classification smithylogging.Classification, format string, v ...any) {
	level := slog.LevelWarn
	if classification == smithylogging.Debug {
		level = slog.LevelDebug
	}
	slog.Log(context.Background(), level, Message, answer.ClipFields([]any{"text", fmt.Sprintf(format, v...)})...)
}

// LoadConfig is the AWS configuration the broker loads, with Log as its logger.
func LoadConfig(ctx context.Context) (aws.Config, error) {
	return awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(Log{}))
}
