package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// WriteAfter appends data to the file at path in one O_APPEND write, refusing with an error that
// Is ErrMoved when the file is no longer size bytes, the size its session was read at. noun is
// what the errors call the file: "session" for Oh My Pi's, "transcript" for Claude Code's.
func WriteAfter(path, noun string, size int64, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if info.Size() != size {
		_ = file.Close()
		return fmt.Errorf("%s %s: %w: read %d bytes, now %d", noun, path, ErrMoved, size, info.Size())
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("append to %s %s: %w", noun, path, err)
	}
	return file.Close()
}

// Marshal is JSON as JavaScript's JSON.stringify writes it: no HTML escaping, no trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// ISOTime is JavaScript's Date.prototype.toISOString.
func ISOTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
