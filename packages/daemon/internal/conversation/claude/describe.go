package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// Description is what a transcript says about itself without its conversation being rebuilt.
type Description struct {
	// Title is the session's title as `/rename` records it: the customTitle of its last
	// custom-title line (Claude Code writes one on the rename and again as each later turn ends),
	// empty when it has none.
	Title string
	// SessionID, Cwd and Timestamp are the first conversation line's: the session, its working
	// directory, and when the conversation began.
	SessionID string
	Cwd       string
	Timestamp string
}

// Describe reads a transcript's content for its Description. A line that is not JSON is skipped,
// as Claude Code skips it.
func Describe(content []byte) Description {
	var d Description
	_ = describe(&d, conversation.ContentLines(content))
	return d
}

// DescribeFile reads the transcript at path for its Description, a line at a time.
func DescribeFile(path string) (Description, error) {
	file, err := os.Open(path)
	if err != nil {
		return Description{}, err
	}
	defer file.Close()
	var d Description
	if err := describe(&d, conversation.ReaderLines(file)); err != nil {
		return Description{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	return d, nil
}

// customTitle is the marker of the lines describe decodes beyond the first conversation line.
var customTitle = []byte(`"custom-title"`)

func describe(d *Description, read func() ([]byte, error)) error {
	for {
		line, err := read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.SessionID != "" && !bytes.Contains(line, customTitle) {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			UUID        string `json:"uuid"`
			SessionID   string `json:"sessionId"`
			Cwd         string `json:"cwd"`
			Timestamp   string `json:"timestamp"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &e) != nil {
			continue
		}
		switch {
		case e.Type == "custom-title" && e.SessionID != "":
			d.Title = e.CustomTitle
		case d.SessionID == "" && transcriptTypes[e.Type] && e.UUID != "":
			d.SessionID, d.Cwd, d.Timestamp = e.SessionID, e.Cwd, e.Timestamp
		}
	}
}
