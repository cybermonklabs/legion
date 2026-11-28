package omp

import (
	"fmt"
	"strconv"
	"strings"
)

// executionMessage mirrors bashExecutionToText and pythonExecutionToText. Only
// Bash records carry images into the rebuilt request.
func executionMessage(m *message) llmMessage {
	var text strings.Builder
	bash := m.Role == "bashExecution"
	if bash {
		fmt.Fprintf(&text, "Ran `%s`\n", m.Command)
	} else {
		fmt.Fprintf(&text, "Ran Python:\n```python\n%s\n```\n", m.Code)
	}
	if m.Output == "" {
		text.WriteString("(no output)")
	} else {
		if !bash {
			text.WriteString("Output:\n")
		}
		fmt.Fprintf(&text, "```\n%s\n```", m.Output)
	}
	if m.Cancelled {
		if bash {
			text.WriteString("\n\n(command cancelled)")
		} else {
			text.WriteString("\n\n(execution cancelled)")
		}
	} else if m.ExitCode != nil && *m.ExitCode != 0 {
		if bash {
			text.WriteString("\n\nCommand exited with code ")
		} else {
			text.WriteString("\n\nExecution failed with code ")
		}
		text.WriteString(strconv.FormatFloat(*m.ExitCode, 'f', -1, 64))
	}
	if notice := m.Meta.notice(); notice != "" {
		fmt.Fprintf(&text, "\n\n[%s]", notice)
	}
	blocks := []block{{Type: "text", Text: text.String()}}
	if bash {
		blocks = append(blocks, m.Images...)
	}
	return llmMessage{role: "user", attribution: "user", timestamp: m.Timestamp, blocks: blocks}
}

// executionMeta is the output metadata the pinned Bash and Python runners
// write. Bash also records minimization metadata, which formatOutputNotice
// does not render. The shared output sink can report a line-width limit.
type executionMeta struct {
	ArtifactError string `json:"artifactError"`
	Truncation    *struct {
		Direction           string     `json:"direction"`
		TruncatedBy         string     `json:"truncatedBy"`
		TotalLines          int        `json:"totalLines"`
		OutputLines         int        `json:"outputLines"`
		TotalBytes          int        `json:"totalBytes"`
		OutputBytes         int        `json:"outputBytes"`
		MaxBytes            *int       `json:"maxBytes"`
		ShownRange          *lineRange `json:"shownRange"`
		HeadRange           *lineRange `json:"headRange"`
		TailRange           *lineRange `json:"tailRange"`
		ElidedBytes         *int       `json:"elidedBytes"`
		ElidedLines         *int       `json:"elidedLines"`
		PartialLine         bool       `json:"partialLine"`
		NextOffset          *int       `json:"nextOffset"`
		ArtifactID          *string    `json:"artifactId"`
		ArtifactElidedBytes int        `json:"artifactElidedBytes"`
	} `json:"truncation"`
	Limits struct {
		ColumnTruncated *struct {
			MaxColumn           float64 `json:"maxColumn"`
			Unit                *string `json:"unit"`
			ArtifactID          *string `json:"artifactId"`
			ArtifactElidedBytes int     `json:"artifactElidedBytes"`
			Lines               *struct {
				First int `json:"first"`
				Last  int `json:"last"`
			} `json:"lines"`
			SelectorBase *string `json:"selectorBase"`
			RawByteCap   *int    `json:"rawByteCap"`
		} `json:"columnTruncated"`
	} `json:"limits"`
}

type lineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (m executionMeta) notice() string {
	var parts []string
	if t := m.Truncation; t != nil {
		var notice string
		switch {
		case t.Direction == "middle":
			bytes := orDefault(t.ElidedBytes, max(0, t.TotalBytes-t.OutputBytes))
			lines := orDefault(t.ElidedLines, max(0, t.TotalLines-t.OutputLines))
			switch {
			case t.HeadRange != nil && t.TailRange != nil:
				notice = fmt.Sprintf("Showing lines %d-%d and %d-%d of %d; %s middle %s (%s) elided", t.HeadRange.Start, t.HeadRange.End, t.TailRange.Start, t.TailRange.End, t.TotalLines, groupedNumber(lines), lineLabel(lines), formatOutputBytes(bytes))
			case bytes > 0:
				notice = fmt.Sprintf("Showing head and tail bytes of %s %s; %s elided", groupedNumber(t.TotalLines), lineLabel(t.TotalLines), formatOutputBytes(bytes))
			default:
				notice = fmt.Sprintf("Showing %d of %d lines; middle elided", min(t.OutputLines, t.TotalLines), t.TotalLines)
			}
		case t.PartialLine:
			line := 1
			if t.ShownRange != nil {
				line = t.ShownRange.Start
			}
			notice = fmt.Sprintf("Showing line %d (partial, %s of %s) of %d", line, formatOutputBytes(t.OutputBytes), formatOutputBytes(t.TotalBytes), t.TotalLines)
		default:
			if span := t.ShownRange; span != nil && span.End >= span.Start {
				notice = fmt.Sprintf("Showing lines %d-%d of %d", span.Start, span.End, t.TotalLines)
			} else {
				notice = fmt.Sprintf("Showing %d of %d lines", t.OutputLines, t.TotalLines)
			}
			if t.TruncatedBy == "bytes" {
				notice += " (" + formatOutputBytes(orDefault(t.MaxBytes, t.OutputBytes)) + " limit)"
			}
		}
		if t.NextOffset != nil && (t.Direction == "middle" || !t.PartialLine) {
			notice += fmt.Sprintf(". Use :%d to continue", *t.NextOffset)
		}
		if t.ArtifactID != nil {
			notice += ". " + fullOutputReference(*t.ArtifactID, t.ArtifactElidedBytes)
		}
		parts = append(parts, notice)
	}
	if m.ArtifactError != "" {
		parts = append(parts, "Full output was not saved completely (artifact "+m.ArtifactError+" failed)")
	}
	if c := m.Limits.ColumnTruncated; c != nil {
		unit := "chars"
		if c.Unit != nil {
			unit = *c.Unit
		}
		notice := fmt.Sprintf("Some lines truncated to %s %s", strconv.FormatFloat(c.MaxColumn, 'f', -1, 64), unit)
		selector := c.SelectorBase
		if selector == nil && c.ArtifactID != nil && c.ArtifactElidedBytes == 0 {
			base := "artifact://" + *c.ArtifactID
			selector = &base
		}
		if c.Lines != nil && selector != nil {
			first := c.Lines.First
			notice += fmt.Sprintf(". Use %s:raw:%d-%d to read line %d", *selector, first, first, first)
			if c.RawByteCap == nil {
				notice += " whole"
			} else {
				notice += " uncut, byte-capped at " + formatOutputBytes(*c.RawByteCap)
			}
			if c.Lines.Last > first {
				notice += fmt.Sprintf("; cut lines run to line %d, each read the same way", c.Lines.Last)
			}
		} else if c.ArtifactID != nil {
			notice += ". " + fullOutputReference(*c.ArtifactID, c.ArtifactElidedBytes)
		}
		parts = append(parts, notice)
	}
	return strings.Join(parts, ". ")
}

func fullOutputReference(id string, elidedBytes int) string {
	if elidedBytes > 0 {
		return "Read artifact://" + id + " for a head/tail sample of the output; " + formatOutputBytes(elidedBytes) + " from its middle was not saved"
	}
	return "Read artifact://" + id + " for full output"
}

func orDefault(value *int, fallback int) int {
	if value != nil {
		return *value
	}
	return fallback
}

func lineLabel(count int) string {
	if count == 1 {
		return "line"
	}
	return "lines"
}

func groupedNumber(n int) string {
	s := strconv.Itoa(n)
	var out strings.Builder
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 && s[i-1] != '-' {
			out.WriteByte(',')
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func formatOutputBytes(n int) string {
	if n < 1024 {
		return strconv.Itoa(n) + "B"
	}
	unit, suffix := int64(1024), "KB"
	switch {
	case n >= 1024*1024*1024:
		unit, suffix = 1024*1024*1024, "GB"
	case n >= 1024*1024:
		unit, suffix = 1024*1024, "MB"
	}
	// JavaScript's toFixed rounds a positive half up, not to even.
	count := int64(n)
	fraction := (count%unit*10 + unit/2) / unit
	return fmt.Sprintf("%d.%d%s", count/unit+fraction/10, fraction%10, suffix)
}

// fileMentionMessages sends the text-only files first, then the image files
// and their images. Responses accepts images in the user slot only.
func fileMentionMessages(m *message) []llmMessage {
	var textFiles, imageFiles []string
	var images []block
	for _, f := range m.Files {
		inner := "\n"
		if f.Content != "" {
			inner += f.Content + "\n"
		}
		wrapped := `<file path="` + f.Path + `">` + inner + `</file>`
		if f.Image == nil {
			textFiles = append(textFiles, wrapped)
		} else {
			imageFiles = append(imageFiles, wrapped)
			images = append(images, *f.Image)
		}
	}
	var out []llmMessage
	for _, fragment := range []struct {
		files     []string
		images    []block
		developer bool
	}{{textFiles, nil, true}, {imageFiles, images, false}} {
		if len(fragment.files) > 0 {
			blocks := append([]block{{Type: "text", Text: strings.Join(fragment.files, "\n")}}, fragment.images...)
			out = append(out, llmMessage{role: "user", attribution: "user", developer: fragment.developer, timestamp: m.Timestamp, blocks: blocks})
		}
	}
	return out
}
