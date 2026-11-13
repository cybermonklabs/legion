package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// A write is one request an agent made: the cases that share an event and a source id. A planted
// copy of a write is a write of its own, told apart by its copy id.
type writeKey struct {
	EventID  int64
	SourceID string
	Copy     string
}

func (k writeKey) String() string {
	s := strconv.FormatInt(k.EventID, 10) + "/" + k.SourceID
	if k.Copy != "" {
		s += "/" + k.Copy
	}
	return s
}

// familyOrder is the surface families in the order every table lists them. A write whose cases
// fall in two families (a document version with its summary) is of the one listed first.
var familyOrder = []string{
	"comment", "comment_edit", "message", "delivery_reply", "send_to_agent", "broadcast",
	"ask", "ask_answer", "ask_resolution", "issue_title", "document", "version_summary",
	"approval_summary", "review_reason", "result",
}

// surfaceFamilies is the family of each surface fetch writes.
var surfaceFamilies = map[string]string{
	surfaceComment:               "comment",
	surfaceCommentSuggestion:     "comment",
	surfaceCommentEdit:           "comment_edit",
	surfaceMessage:               "message",
	surfaceCommentDeliveryReply:  "delivery_reply",
	surfaceDeliveryReply:         "delivery_reply",
	surfaceSendToAgent:           "send_to_agent",
	surfaceBroadcast:             "broadcast",
	surfaceAskQuestion:           "ask",
	surfaceAskOption:             "ask",
	surfaceAskAnswer:             "ask_answer",
	surfaceAskDecline:            "ask_resolution",
	surfaceAskResolution:         "ask_resolution",
	surfaceIssueTitle:            "issue_title",
	surfaceDocument:              "document",
	surfaceVersionSummary:        "version_summary",
	surfaceApprovalSummary:       "approval_summary",
	surfaceReviewReason:          "review_reason",
	surfaceResultRun:             "result",
	surfaceResultObserved:        "result",
	surfaceResultNegativeControl: "result",
}

// familyRank is each family's place in familyOrder.
var familyRank = func() map[string]int {
	rank := make(map[string]int, len(familyOrder))
	for i, f := range familyOrder {
		rank[f] = i
	}
	return rank
}()

// mainSurfaces is the surfaces in the order a write's main prose field is chosen from: the body a
// person reads first. A planted sentence goes into the main field.
var mainSurfaces = []string{
	surfaceDocument, surfaceComment, surfaceCommentEdit, surfaceCommentDeliveryReply,
	surfaceMessage, surfaceDeliveryReply, surfaceSendToAgent, surfaceBroadcast,
	surfaceAskQuestion, surfaceAskAnswer, surfaceAskDecline, surfaceAskResolution,
	surfaceApprovalSummary, surfaceReviewReason, surfaceResultObserved, surfaceResultRun,
	surfaceResultNegativeControl, surfaceVersionSummary, surfaceCommentSuggestion,
	surfaceAskOption, surfaceIssueTitle,
}

// lineFields are the surfaces whose text is one line, a label or a title, that holds no
// paragraph a sentence could be planted beside.
var lineFields = map[string]bool{surfaceIssueTitle: true, surfaceAskOption: true, surfaceCommentSuggestion: true}

// caseRef is the part of a case that every subcommand reads without its text.
type caseRef struct {
	EventID  int64  `json:"event_id"`
	SourceID string `json:"source_id"`
	Field    string `json:"field"`
	Surface  string `json:"surface"`
	Project  string `json:"project"`
	Issue    string `json:"issue"`
	Group    string `json:"group"`
	Copy     string `json:"copy"`
}

func (c caseRef) key() writeKey { return writeKey{c.EventID, c.SourceID, c.Copy} }

// writeRef is one write as scanWrites reads it: its key, family and project, and the surfaces of
// its cases, without their text.
type writeRef struct {
	Key      writeKey
	Family   string
	Project  string
	Issue    string
	Surfaces []string
}

// eachLine calls fn with every non-blank line of a JSONL file and its line number.
func eachLine(path string, fn func(line []byte, n int) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for n := 1; scanner.Scan(); n++ {
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		if err := fn(scanner.Bytes(), n); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
	}
	return scanner.Err()
}

// familyOf is the family of a write with these surfaces: the family listed first among them.
func familyOf(surfaces []string) (string, error) {
	best := ""
	for _, s := range surfaces {
		f, ok := surfaceFamilies[s]
		if !ok {
			return "", fmt.Errorf("unknown surface %q", s)
		}
		if best == "" || familyRank[f] < familyRank[best] {
			best = f
		}
	}
	return best, nil
}

// scanWrites reads a cases file's writes in the order of each write's first case, without
// keeping any text.
func scanWrites(path string) ([]*writeRef, error) {
	var writes []*writeRef
	index := map[writeKey]*writeRef{}
	err := eachLine(path, func(line []byte, _ int) error {
		var c caseRef
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		if _, ok := surfaceFamilies[c.Surface]; !ok {
			return fmt.Errorf("unknown surface %q", c.Surface)
		}
		w := index[c.key()]
		if w == nil {
			w = &writeRef{Key: c.key(), Project: c.Project, Issue: c.Issue}
			index[c.key()] = w
			writes = append(writes, w)
		}
		if !slices.Contains(w.Surfaces, c.Surface) {
			w.Surfaces = append(w.Surfaces, c.Surface)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, w := range writes {
		w.Family, _ = familyOf(w.Surfaces)
	}
	return writes, nil
}

// loadWrites reads the whole cases of the writes keep holds, in file order, by write.
func loadWrites(path string, keep map[writeKey]bool) (map[writeKey][]Case, error) {
	out := make(map[writeKey][]Case, len(keep))
	err := eachLine(path, func(line []byte, _ int) error {
		var ref caseRef
		if err := json.Unmarshal(line, &ref); err != nil {
			return err
		}
		if !keep[ref.key()] {
			return nil
		}
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		out[ref.key()] = append(out[ref.key()], c)
		return nil
	})
	return out, err
}

// readWriteKeys is the writes a manifest names: every distinct event_id and source_id of its
// lines, in the order they first appear. Both split's set manifests and rule-cost's manifest
// are read this way.
func readWriteKeys(path string) ([]writeKey, error) {
	var keys []writeKey
	seen := map[writeKey]bool{}
	err := eachLine(path, func(line []byte, _ int) error {
		var ref caseRef
		if err := json.Unmarshal(line, &ref); err != nil {
			return err
		}
		if ref.EventID == 0 {
			return fmt.Errorf("no event_id")
		}
		k := writeKey{EventID: ref.EventID, SourceID: ref.SourceID}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
		return nil
	})
	return keys, err
}

// mainCase is the index of a write's main prose field: the case whose surface comes first in
// mainSurfaces, the longest text among several of one surface.
func mainCase(cases []Case) int {
	best, bestRank := -1, len(mainSurfaces)
	for i, c := range cases {
		rank := slices.Index(mainSurfaces, c.Surface)
		if rank < 0 {
			rank = len(mainSurfaces)
		}
		if best < 0 || rank < bestRank || rank == bestRank && len(c.Text) > len(cases[best].Text) {
			best, bestRank = i, rank
		}
	}
	return best
}

// mainSurface is the surface mainCase picks among these.
func mainSurface(surfaces []string) string {
	for _, s := range mainSurfaces {
		if slices.Contains(surfaces, s) {
			return s
		}
	}
	return ""
}

// paragraphStarts is the byte offsets in a Markdown text at which a new paragraph can be put
// without changing what the rest of the text is: the start of the text (after its front matter),
// the start of each line that follows a blank line, opens with no white space and stands outside
// a fenced code block and the front matter, and the end of a text that does not end inside a
// fence.
func paragraphStarts(text string) []int {
	var starts []int
	offset, lineNo := 0, 0
	inFront := strings.HasPrefix(text, "---\n")
	fence := ""
	prevBlank := true
	if !inFront {
		starts = append(starts, 0)
	}
	for offset < len(text) {
		end := strings.IndexByte(text[offset:], '\n')
		next := len(text)
		if end >= 0 {
			next = offset + end + 1
		}
		line := strings.TrimRight(text[offset:next], "\r\n")
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case inFront:
			if lineNo > 0 && (line == "---" || line == "...") {
				inFront = false
				starts = append(starts, next)
			}
		case fence != "":
			if strings.HasPrefix(trimmed, fence) && strings.Trim(strings.TrimSpace(trimmed), fence[:1]) == "" {
				fence = ""
			}
		default:
			if offset > 0 && prevBlank && line != "" && line[0] != ' ' && line[0] != '\t' && (len(starts) == 0 || starts[len(starts)-1] != offset) {
				starts = append(starts, offset)
			}
			if f := fenceOpener(trimmed); f != "" && len(line)-len(trimmed) < 4 {
				fence = f
			}
		}
		prevBlank = strings.TrimSpace(line) == ""
		offset = next
		lineNo++
	}
	if fence == "" && !inFront && (len(starts) == 0 || starts[len(starts)-1] != len(text)) {
		starts = append(starts, len(text))
	}
	return starts
}

// fenceOpener is the run of backticks or tildes a line opens a fenced code block with, or "".
func fenceOpener(line string) string {
	for _, mark := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, mark) {
			n := len(line) - len(strings.TrimLeft(line, mark[:1]))
			return line[:n]
		}
	}
	return ""
}

// insertParagraph puts paragraph into text at a paragraph start and answers the new text and the
// offset the paragraph starts at. At the end of the text the paragraph follows a blank line; at
// any other start, a blank line follows it.
func insertParagraph(text string, at int, paragraph string) (string, int) {
	if at < len(text) {
		return text[:at] + paragraph + "\n\n" + text[at:], at
	}
	prefix := text
	switch {
	case prefix == "":
	case strings.HasSuffix(prefix, "\n\n"):
	case strings.HasSuffix(prefix, "\n"):
		prefix += "\n"
	default:
		prefix += "\n\n"
	}
	return prefix + paragraph, len(prefix)
}

// sha256Hex is the hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeJSONL writes each value as one JSON line to path, and answers the file's SHA-256.
func writeJSONL[T any](path string, values []T) (string, error) {
	var buf strings.Builder
	for _, v := range values {
		line, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return "", err
	}
	return sha256Hex([]byte(buf.String())), nil
}

// writeJSON writes v, indented, to path.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// flagGiven is whether the command line set the named flag.
func flagGiven(flags *flag.FlagSet, name string) bool {
	given := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// pct is a share as a percentage with one decimal.
func pct(share float64) string { return strconv.FormatFloat(100*share, 'f', 1, 64) + "%" }
