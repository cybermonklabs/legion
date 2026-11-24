package api

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The tree read's answer is thousands of nodes and twice as many tuples, and the reflection of
// encoding/json over them cost the read more than any statement it sends. writeIssueTreeRead
// writes the same bytes WriteJSON writes - json.NewEncoder's, v1 semantics, the newline after the
// value - appending each member the way encoding/json does: a string quoted as it quotes one
// (appendTreeString), a time in RFC 3339 with nanoseconds, a map's keys in sorted order, a display
// tree reformatted by jsontext with the options encoding/json writes a Marshaler's output with
// (jsontext.AppendFormat with json.DefaultOptionsV1), and each member few nodes carry - a
// decision, an ask, a plan, a result, a check, a document, an actor with more than its kind, its
// id and its proof - through json.Marshal itself. A value it cannot write - a time outside RFC
// 3339, a display tree that is no JSON - is written by WriteJSON, which answers it as it always
// did.

// treeAnswerBuffers holds the buffers answers are appended into, so a read of a large tree does
// not allocate its answer's bytes again.
var treeAnswerBuffers = sync.Pool{New: func() any { return new([]byte) }}

// errTreeTime is a time encoding/json refuses to write: its year or its zone's hour is out of the
// range RFC 3339 holds.
var errTreeTime = errors.New("time outside RFC 3339")

// writeIssueTreeRead writes read as the tree read's 200 answer, byte for byte as WriteJSON writes
// it.
func writeIssueTreeRead(w http.ResponseWriter, read *model.IssueTreeRead) {
	buffer := treeAnswerBuffers.Get().(*[]byte)
	defer treeAnswerBuffers.Put(buffer)
	if size := issueTreeReadSize(read); cap(*buffer) < size {
		*buffer = make([]byte, 0, size)
	}
	body, err := appendIssueTreeRead((*buffer)[:0], read)
	*buffer = body[:0]
	if err != nil {
		WriteJSON(w, http.StatusOK, *read)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// issueTreeReadSize is about how many bytes read's JSON takes, a little more than most answers
// take, so its buffer is allocated once.
func issueTreeReadSize(read *model.IssueTreeRead) int {
	size := 512 + 192*len(read.Tuples)
	for index := range read.Nodes {
		node := &read.Nodes[index]
		size += 640 + len(node.ID) + len(node.Text) + len(node.Text)/8 + len(node.TextTree) + len(node.TextTree)/8
	}
	return size
}

// appendIssueTreeRead appends read's JSON, as json.Marshal writes it, to b.
func appendIssueTreeRead(b []byte, read *model.IssueTreeRead) ([]byte, error) {
	var err error
	b = append(b, `{"issue":`...)
	b = appendTreeString(b, read.Issue)
	b = append(b, `,"view":`...)
	b = appendTreeString(b, read.View)
	b = append(b, `,"version":`...)
	if read.Version == nil {
		b = append(b, "null"...)
	} else {
		b = strconv.AppendInt(b, int64(*read.Version), 10)
	}
	b = append(b, `,"converted":`...)
	b = strconv.AppendBool(b, read.Converted)
	b = append(b, `,"tree":`...)
	b = appendTreeStringPointer(b, read.Tree)
	b = append(b, `,"view_of":`...)
	b = appendTreeStringPointer(b, read.ViewOf)
	b = append(b, `,"documents":`...)
	if b, err = appendTreeMarshaled(b, read.Documents); err != nil {
		return b, err
	}
	b = append(b, `,"nodes":`...)
	if read.Nodes == nil {
		b = append(b, "null"...)
	} else {
		b = append(b, '[')
		for index := range read.Nodes {
			if index > 0 {
				b = append(b, ',')
			}
			if b, err = appendIssueTreeNode(b, &read.Nodes[index]); err != nil {
				return b, err
			}
		}
		b = append(b, ']')
	}
	if read.DocumentNodes != nil {
		b = append(b, `,"document_nodes":[`...)
		for index := range read.DocumentNodes {
			if index > 0 {
				b = append(b, ',')
			}
			document := &read.DocumentNodes[index]
			b = append(b, `{"artifact_id":`...)
			b = appendTreeString(b, document.ArtifactID)
			b = append(b, `,"slug":`...)
			b = appendTreeString(b, document.Slug)
			b = append(b, `,"block_count":`...)
			b = strconv.AppendInt(b, int64(document.BlockCount), 10)
			b = append(b, '}')
		}
		b = append(b, ']')
	}
	b = append(b, `,"tuples":`...)
	if read.Tuples == nil {
		b = append(b, "null"...)
	} else {
		b = append(b, '[')
		for index := range read.Tuples {
			if index > 0 {
				b = append(b, ',')
			}
			tuple := &read.Tuples[index]
			b = append(b, `{"kind":`...)
			b = appendTreeString(b, tuple.Kind)
			b = append(b, `,"from":`...)
			b = appendTreeEnd(b, tuple.From)
			b = append(b, `,"to":`...)
			b = appendTreeEnd(b, tuple.To)
			b = append(b, '}')
		}
		b = append(b, ']')
	}
	b = append(b, `,"cursor":`...)
	b = appendTreeStringPointer(b, read.Cursor)
	if len(read.CommentsBefore) > 0 {
		b = append(b, `,"comments_before":`...)
		if b, err = appendTreeMarshaled(b, read.CommentsBefore); err != nil {
			return b, err
		}
	}
	if len(read.OwedDocuments) > 0 {
		b = append(b, `,"owed_documents":`...)
		if b, err = appendTreeMarshaled(b, read.OwedDocuments); err != nil {
			return b, err
		}
	}
	return append(b, '}'), nil
}

// appendIssueTreeNode appends node's JSON, as json.Marshal writes it, to b.
func appendIssueTreeNode(b []byte, node *model.IssueTreeNode) ([]byte, error) {
	var err error
	b = append(b, `{"kind":`...)
	b = appendTreeString(b, node.Kind)
	b = append(b, `,"id":`...)
	b = appendTreeString(b, node.ID)
	if node.BlockID != "" {
		b = append(b, `,"block_id":`...)
		b = appendTreeString(b, node.BlockID)
	}
	b = append(b, `,"supertag":`...)
	b = appendTreeString(b, node.Supertag)
	if node.Issue != "" {
		b = append(b, `,"issue":`...)
		b = appendTreeString(b, node.Issue)
	}
	if node.ReadOnly {
		b = append(b, `,"read_only":true`...)
	}
	b = append(b, `,"role":`...)
	b = appendTreeString(b, node.Role)
	b = append(b, `,"parent":`...)
	if parent := node.Parent; parent == nil {
		b = append(b, "null"...)
	} else {
		b = append(b, `{"kind":`...)
		b = appendTreeString(b, parent.Kind)
		b = append(b, `,"id":`...)
		b = appendTreeString(b, parent.ID)
		b = append(b, `,"supertag":`...)
		b = appendTreeString(b, parent.Supertag)
		b = append(b, `,"first_line":`...)
		b = appendTreeString(b, parent.FirstLine)
		if parent.Issue != "" {
			b = append(b, `,"issue":`...)
			b = appendTreeString(b, parent.Issue)
		}
		b = append(b, '}')
	}
	b = append(b, `,"rank":`...)
	b = appendTreeStringPointer(b, node.Rank)
	b = append(b, `,"author":`...)
	if node.Author == nil {
		b = append(b, "null"...)
	} else {
		b = append(b, '[')
		for index := range node.Author {
			if index > 0 {
				b = append(b, ',')
			}
			if b, err = appendTreeActor(b, &node.Author[index]); err != nil {
				return b, err
			}
		}
		b = append(b, ']')
	}
	b = append(b, `,"state":`...)
	b = appendTreeString(b, node.State)
	b = append(b, `,"visibility":`...)
	b = appendTreeString(b, node.Visibility)
	b = append(b, `,"made_at":`...)
	if node.MadeAt == nil {
		b = append(b, "null"...)
	} else if b, err = appendTreeTime(b, *node.MadeAt); err != nil {
		return b, err
	}
	b = append(b, `,"attributes":`...)
	b = appendTreeAttributes(b, node.Attributes)
	b = append(b, `,"text":`...)
	b = appendTreeString(b, node.Text)
	if len(node.TextTree) > 0 {
		b = append(b, `,"text_tree":`...)
		if b, err = jsontext.AppendFormat(b, []byte(node.TextTree), json.DefaultOptionsV1()); err != nil {
			return b, err
		}
	}
	if node.NoConversation {
		b = append(b, `,"no_conversation":true`...)
	}
	for _, member := range [...]struct {
		name  string
		value any
		held  bool
	}{
		{`,"document":`, node.Document, node.Document != nil},
		{`,"ask":`, node.Ask, node.Ask != nil},
		{`,"decision":`, node.Decision, node.Decision != nil},
		{`,"result":`, node.Result, node.Result != nil},
		{`,"check":`, node.Check, node.Check != nil},
		{`,"plan":`, node.Plan, node.Plan != nil},
	} {
		if !member.held {
			continue
		}
		b = append(b, member.name...)
		if b, err = appendTreeMarshaled(b, member.value); err != nil {
			return b, err
		}
	}
	if issue := node.IssueTreeIssue; issue != nil {
		b = append(b, `,"converted":`...)
		b = strconv.AppendBool(b, issue.Converted)
		b = append(b, `,"tree":`...)
		b = appendTreeStringPointer(b, issue.Tree)
		b = append(b, `,"view_of":`...)
		b = appendTreeStringPointer(b, issue.ViewOf)
		b = append(b, `,"documents":`...)
		if b, err = appendTreeMarshaled(b, issue.Documents); err != nil {
			return b, err
		}
	}
	return append(b, '}'), nil
}

// appendTreeEnd appends a tuple's end, as json.Marshal writes it, to b.
func appendTreeEnd(b []byte, end model.IssueTreeEnd) []byte {
	b = append(b, `{"kind":`...)
	b = appendTreeString(b, end.Kind)
	b = append(b, `,"id":`...)
	b = appendTreeString(b, end.ID)
	return append(b, '}')
}

// appendTreeActor appends actor, as json.Marshal writes it, to b: an actor that holds no more than
// its kind, its id and its proof by hand, and any other through json.Marshal.
func appendTreeActor(b []byte, actor *model.Actor) ([]byte, error) {
	if actor.Origin != nil || actor.Owner != nil || actor.Service != nil || actor.Project != nil || actor.Act != nil {
		return appendTreeMarshaled(b, actor)
	}
	b = append(b, `{"kind":`...)
	b = appendTreeString(b, actor.Kind)
	b = append(b, `,"id":`...)
	b = appendTreeString(b, actor.ID)
	if actor.Proven {
		b = append(b, `,"proven":true`...)
	}
	return append(b, '}'), nil
}

// appendTreeAttributes appends a node's attributes, as json.Marshal writes a map of strings, to
// b: null for a nil map, and each key in sorted order.
func appendTreeAttributes(b []byte, attributes map[string]string) []byte {
	if attributes == nil {
		return append(b, "null"...)
	}
	var held [16]string
	keys := held[:0]
	for key := range attributes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	b = append(b, '{')
	for index, key := range keys {
		if index > 0 {
			b = append(b, ',')
		}
		b = appendTreeString(b, key)
		b = append(b, ':')
		b = appendTreeString(b, attributes[key])
	}
	return append(b, '}')
}

// appendTreeTime appends t, as json.Marshal writes a time.Time, to b: RFC 3339 with
// nanoseconds, quoted; a time whose year has other than four digits, or whose zone's hour is 24
// or more, is errTreeTime.
func appendTreeTime(b []byte, t time.Time) ([]byte, error) {
	b = append(b, '"')
	start := len(b)
	b = t.AppendFormat(b, time.RFC3339Nano)
	written := b[start:]
	switch {
	case written[len("9999")] != '-':
		return b, errTreeTime
	case written[len(written)-1] != 'Z':
		c := written[len(written)-len("Z07:00")]
		zone := written[len(written)-len("07:00"):]
		if ('0' <= c && c <= '9') || int(zone[0]-'0')*10+int(zone[1]-'0') >= 24 {
			return b, errTreeTime
		}
	}
	return append(b, '"'), nil
}

// appendTreeStringPointer appends s, as json.Marshal writes a *string, to b.
func appendTreeStringPointer(b []byte, s *string) []byte {
	if s == nil {
		return append(b, "null"...)
	}
	return appendTreeString(b, *s)
}

// appendTreeMarshaled appends value, as json.Marshal writes it, to b.
func appendTreeMarshaled(b []byte, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return b, err
	}
	return append(b, encoded...), nil
}

// treeStringEscapes marks the ASCII bytes json.Marshal escapes in a string: the control
// characters, '"' and '\\', and '<', '>' and '&', which it escapes for HTML.
var treeStringEscapes = [utf8.RuneSelf]bool{
	'"': true, '\\': true, '<': true, '>': true, '&': true,
	0x00: true, 0x01: true, 0x02: true, 0x03: true, 0x04: true, 0x05: true, 0x06: true, 0x07: true,
	0x08: true, 0x09: true, 0x0a: true, 0x0b: true, 0x0c: true, 0x0d: true, 0x0e: true, 0x0f: true,
	0x10: true, 0x11: true, 0x12: true, 0x13: true, 0x14: true, 0x15: true, 0x16: true, 0x17: true,
	0x18: true, 0x19: true, 0x1a: true, 0x1b: true, 0x1c: true, 0x1d: true, 0x1e: true, 0x1f: true,
}

// appendTreeString appends s quoted, as json.Marshal writes a string, to b: '"' and '\\' escaped
// by a backslash, \b, \f, \n, \r and \t by their short escapes, every other control character and
// '<', '>' and '&' as \u00XX, U+2028 and U+2029 as \u2028 and \u2029, and each byte of invalid
// UTF-8 replaced with U+FFFD.
func appendTreeString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for index := 0; index < len(s); {
		c := s[index]
		if c < utf8.RuneSelf {
			index++
			if !treeStringEscapes[c] {
				continue
			}
			b = append(b, s[start:index-1]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			start = index
			continue
		}
		r, size := utf8.DecodeRuneInString(s[index:])
		index += size
		switch {
		case r == utf8.RuneError && size == 1:
			b = append(b, s[start:index-size]...)
			b = append(b, "\ufffd"...)
			start = index
		case r == '\u2028' || r == '\u2029':
			b = append(b, s[start:index-size]...)
			b = append(b, '\\', 'u', '2', '0', '2', hex[r&0xf])
			start = index
		}
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
