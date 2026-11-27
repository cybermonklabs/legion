package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Marshal encodes v with Go's JSON encoder, without HTML escaping or a trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// JSONObjectOrEmpty returns raw unchanged if it is a valid JSON object, and {} otherwise.
func JSONObjectOrEmpty(raw json.RawMessage) json.RawMessage {
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) && json.Valid(raw) {
		return raw
	}
	return json.RawMessage("{}")
}

// ISOTime is JavaScript's Date.prototype.toISOString.
func ISOTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ContentLines returns successive lines of content without their terminating newline.
// The last line need not end in a newline. The function returns io.EOF when no lines remain.
func ContentLines(content []byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(content) == 0 {
			return nil, io.EOF
		}
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		return line, nil
	}
}

// ReaderLines returns successive lines from r, including any terminating newline.
// It returns a final unterminated line before reporting io.EOF.
func ReaderLines(r io.Reader) func() ([]byte, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	return func() ([]byte, error) {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && errors.Is(err, io.EOF) {
			err = nil
		}
		return line, err
	}
}
