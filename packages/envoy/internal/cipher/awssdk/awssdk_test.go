package awssdk_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	smithylogging "github.com/aws/smithy-go/logging"

	"github.com/sjawhar/envoy/internal/cipher/awssdk"
)

// TestLoadConfigLogsThroughTheBrokersLogger: the AWS configuration the broker loads writes the
// SDK's lines through Log, as slog lines of the default logger, a warning at WARN and a debug line
// at DEBUG, each with the SDK's text in its text field.
func TestLoadConfigLogsThroughTheBrokersLogger(t *testing.T) {
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
	cfg, err := awssdk.LoadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Logger.(awssdk.Log); !ok {
		t.Fatalf("the configuration logs through %T, want awssdk.Log", cfg.Logger)
	}
	var written bytes.Buffer
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewJSONHandler(&written, &slog.HandlerOptions{Level: slog.LevelDebug})))
	cfg.Logger.Logf(smithylogging.Warn, "falling back to %s", "IMDSv1")
	cfg.Logger.Logf(smithylogging.Debug, "a debug line")
	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(written.String()), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("a line is no JSON: %q", line)
		}
		lines = append(lines, fields)
	}
	if len(lines) != 2 || lines[0]["msg"] != awssdk.Message || lines[0]["level"] != "WARN" || lines[0]["text"] != "falling back to IMDSv1" ||
		lines[1]["msg"] != awssdk.Message || lines[1]["level"] != "DEBUG" || lines[1]["text"] != "a debug line" {
		t.Fatalf("the SDK's lines are %v; want a WARN and a DEBUG line of %q with their text", lines, awssdk.Message)
	}
}
