package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestLoggerOutputsValidJSON(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	logger := New("test-machine")
	logger.Info("test message", slog.String("key", "value"))

	w.Close()
	os.Stderr = oldStderr

	// Read captured output
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal([]byte(output), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, output)
	}

	// Verify required fields
	if logEntry["machine_id"] != "test-machine" {
		t.Errorf("expected machine_id=test-machine, got %v", logEntry["machine_id"])
	}
	if logEntry["level"] != "INFO" {
		t.Errorf("expected level=INFO, got %v", logEntry["level"])
	}
	if logEntry["msg"] != "test message" {
		t.Errorf("expected msg=test message, got %v", logEntry["msg"])
	}
	if logEntry["key"] != "value" {
		t.Errorf("expected key=value, got %v", logEntry["key"])
	}

	// Verify timestamp is ISO 8601
	ts, ok := logEntry["ts"].(string)
	if !ok {
		t.Fatalf("ts field is not a string: %v", logEntry["ts"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("ts is not valid ISO 8601: %v", err)
	}
}

func TestNewWithWriterWritesToTheGivenWriter(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWithWriter("test-machine", &buf)
	logger.DeliveryLog(slog.LevelWarn, "captured", "ses_1", "notifications.role.x", "evt_1", "receipt_timeout")

	var logEntry map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, buf.String())
	}
	if logEntry["msg"] != "captured" || logEntry["delivery_status"] != "receipt_timeout" || logEntry["machine_id"] != "test-machine" {
		t.Fatalf("unexpected log entry: %s", buf.String())
	}
}

func TestDeliveryLogIncludesDeliveryFields(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	logger := New("test-machine")
	logger.DeliveryLog(slog.LevelInfo, "delivery test", "session-123", "topic.test", "event-456", "delivered")

	w.Close()
	os.Stderr = oldStderr

	// Read captured output
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal([]byte(output), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, output)
	}

	// Verify delivery-specific fields
	if logEntry["session_id"] != "session-123" {
		t.Errorf("expected session_id=session-123, got %v", logEntry["session_id"])
	}
	if logEntry["topic"] != "topic.test" {
		t.Errorf("expected topic=topic.test, got %v", logEntry["topic"])
	}
	if logEntry["event_id"] != "event-456" {
		t.Errorf("expected event_id=event-456, got %v", logEntry["event_id"])
	}
	if logEntry["delivery_status"] != "delivered" {
		t.Errorf("expected delivery_status=delivered, got %v", logEntry["delivery_status"])
	}

	// Verify base fields still present
	if logEntry["machine_id"] != "test-machine" {
		t.Errorf("expected machine_id=test-machine, got %v", logEntry["machine_id"])
	}
}

func TestLoggerErrorLevel(t *testing.T) {
	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	logger := New("test-machine")
	logger.Error("error message", slog.String("error", "test error"))

	w.Close()
	os.Stderr = oldStderr

	// Read captured output
	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal([]byte(output), &logEntry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\nOutput: %s", err, output)
	}

	if logEntry["level"] != "ERROR" {
		t.Errorf("expected level=ERROR, got %v", logEntry["level"])
	}
}

// cloudEntry hands one record to NewCloudHandler and answers what it wrote, read as JSON, failing t
// unless the handler wrote exactly one line holding one JSON object. It calls Handle, not a
// Logger: a Logger asks Enabled first and drops a record below INFO before the handler sees it,
// and what a row here reads is what the handler writes for the record it is given.
func cloudEntry(t *testing.T, record slog.Record) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := NewCloudHandler(&buf).Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	line := buf.Bytes()
	if bytes.IndexByte(line, '\n') != len(line)-1 {
		t.Fatalf("the handler wrote %q; want one line, ended by its newline", line)
	}
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		t.Fatalf("the handler's line is not one JSON object: %v\n%s", err, line)
	}
	return entry
}

// TestTheCloudHandlerWritesEachLevelAsItsSeverity: each of slog's four levels is written under
// severity as the LogSeverity name Cloud Logging reads an entry's severity from, a level between
// two of them as the one below it, and no entry carries slog's own level key.
func TestTheCloudHandlerWritesEachLevelAsItsSeverity(t *testing.T) {
	for _, row := range []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelInfo + 2, "INFO"},
		{slog.LevelWarn, "WARNING"},
		{slog.LevelError, "ERROR"},
		{slog.LevelError + 4, "ERROR"},
	} {
		t.Run(row.level.String(), func(t *testing.T) {
			entry := cloudEntry(t, slog.NewRecord(time.Now(), row.level, "a line", 0))
			if entry["severity"] != row.want {
				t.Fatalf("severity = %v, want %s: %v", entry["severity"], row.want, entry)
			}
			if level, ok := entry[slog.LevelKey]; ok {
				t.Fatalf("the entry carries %s=%v beside its severity: %v", slog.LevelKey, level, entry)
			}
		})
	}
}

// TestTheCloudHandlerIsAtInfo: the handler writes INFO and above, as the listener's does.
func TestTheCloudHandlerIsAtInfo(t *testing.T) {
	handler := NewCloudHandler(&bytes.Buffer{})
	if handler.Enabled(context.Background(), slog.LevelDebug) || !handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatalf("Enabled(DEBUG) = %t, Enabled(INFO) = %t; want false and true",
			handler.Enabled(context.Background(), slog.LevelDebug), handler.Enabled(context.Background(), slog.LevelInfo))
	}
}

// TestTheCloudHandlerWritesMessageTimeAndDurationsAsCloudLoggingReadsThem: the message is under
// message and not msg, the time is RFC 3339 with its nanoseconds in UTC whatever zone the record
// was stamped in, every attribute is a field of its own, and a time.Duration, at the top or
// inside a group, is Go's duration string rather than slog's integer nanoseconds.
func TestTheCloudHandlerWritesMessageTimeAndDurationsAsCloudLoggingReadsThem(t *testing.T) {
	stamped := time.Date(2026, 11, 13, 9, 30, 15, 123456789, time.FixedZone("UTC+2", 2*60*60))
	record := slog.NewRecord(stamped, slog.LevelInfo, "broker reread rate", 0)
	record.AddAttrs(
		slog.Int("replicas", 3),
		slog.Duration("every", 90*time.Second),
		slog.String("name", "example/agent-secrets/demo-key"),
		slog.Group("wait", slog.Duration("budget", 1500*time.Millisecond)),
	)
	entry := cloudEntry(t, record)

	if entry["message"] != "broker reread rate" {
		t.Errorf("message = %v, want the record's message: %v", entry["message"], entry)
	}
	if msg, ok := entry[slog.MessageKey]; ok {
		t.Errorf("the entry carries %s=%v beside its message: %v", slog.MessageKey, msg, entry)
	}
	if want := "2026-11-13T07:30:15.123456789Z"; entry["time"] != want {
		t.Errorf("time = %v, want %s, the record's instant in UTC with its nanoseconds: %v", entry["time"], want, entry)
	}
	if entry["replicas"] != float64(3) || entry["name"] != "example/agent-secrets/demo-key" {
		t.Errorf("replicas = %v and name = %v, want 3 and the name, each a field of its own: %v", entry["replicas"], entry["name"], entry)
	}
	if entry["every"] != "1m30s" {
		t.Errorf("every = %v, want 1m30s: %v", entry["every"], entry)
	}
	if wait, ok := entry["wait"].(map[string]any); !ok || wait["budget"] != "1.5s" {
		t.Errorf("wait = %v, want a group whose budget is 1.5s: %v", entry["wait"], entry)
	}
}
