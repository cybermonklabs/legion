package harness

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The case groups of R1, as a case's group names them.
const (
	GroupQuestion = "question"
	GroupAnswer   = "answer"
	GroupPlain    = "plain"
	GroupPhrase   = "phrase"
)

// Groups is every case group, in the order the scores give them.
var Groups = []string{GroupQuestion, GroupAnswer, GroupPlain, GroupPhrase}

// The parts of the split (plan Decision 17).
const (
	PartDevelopment = "development"
	PartHeldOut     = "held_out"
)

// The files of a dataset build that the harness reads.
const (
	casesFile     = "cases.jsonl.gz"
	documentsFile = "documents.jsonl.gz"
)

// Case is one case of the dataset, one line of cases.jsonl.gz, as `writing-check dataset build`
// writes it (dsCase in cmd/writing-check/dataset.go).
type Case struct {
	ID            string      `json:"id"`
	Group         string      `json:"group"`
	Part          string      `json:"part"`
	SplitGroup    string      `json:"split_group"`
	Tree          string      `json:"tree"`
	Kind          string      `json:"kind"`
	Conversation  bool        `json:"conversation"`
	WriterSession string      `json:"writer_session,omitempty"`
	WriteTime     string      `json:"write_time"`
	NamedByOwner  bool        `json:"named_by_owner,omitempty"`
	Found         string      `json:"found,omitempty"`
	Agent         CaseText    `json:"agent_text"`
	OwnerReply    *CaseText   `json:"owner_reply,omitempty"`
	Question      *CaseText   `json:"question,omitempty"`
	Phrase        *CasePhrase `json:"phrase,omitempty"`
	Record        CaseRecord  `json:"record"`
}

// CaseText is one text of a case: the agent's, the owner's or one of the thread's.
type CaseText struct {
	Key       string       `json:"key"`
	EventID   int64        `json:"event_id"`
	EventType string       `json:"event_type"`
	Time      string       `json:"time"`
	Kind      string       `json:"kind"`
	Family    string       `json:"family,omitempty"`
	SourceID  string       `json:"source_id,omitempty"`
	ActorKind string       `json:"actor_kind"`
	Actor     string       `json:"actor"`
	Session   string       `json:"session,omitempty"`
	Text      string       `json:"text"`
	Cut       bool         `json:"cut,omitempty"`
	Selected  []string     `json:"selected,omitempty"`
	Verdict   *CaseVerdict `json:"verdict,omitempty"`
}

// CaseVerdict is the selection prompt's verdict on an owner's text.
type CaseVerdict struct {
	Verdict string   `json:"verdict"`
	Words   []string `json:"words"`
	Model   string   `json:"model"`
}

// CasePhrase is a phrase case's shapes and sources.
type CasePhrase struct {
	Shapes  []string `json:"shapes"`
	Sources []string `json:"sources"`
}

// CaseRecord is Dispatch's record just before the agent's text.
type CaseRecord struct {
	Project   string         `json:"project"`
	Issue     *CaseIssue     `json:"issue,omitempty"`
	Documents []CaseDocument `json:"documents"`
	Thread    []CaseText     `json:"thread"`
}

// CaseIssue is the issue of a record, with its title at the time.
type CaseIssue struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Parent string `json:"parent,omitempty"`
	Root   string `json:"root"`
}

// CaseDocument is one document of a record at its version; its Markdown is a line of
// documents.jsonl.gz.
type CaseDocument struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Missing    bool   `json:"missing,omitempty"`
}

// docKey names one document version.
type docKey struct {
	artifact string
	version  int
}

// Dataset is the cases of one part of a build, and the Markdown of each document version their
// records name.
type Dataset struct {
	Cases []*Case
	// docs holds each document version a record names, whole or its start (LoadDataset).
	docs map[docKey]string
}

// wholeDocuments is LoadDataset's limit that keeps each document whole: what `harness load`
// writes to the local Dispatch. A score keeps docLimit, all a grading call reads.
const wholeDocuments = 0

// eachGzipLine calls fn with every non-blank line of a gzipped file.
func eachGzipLine(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	r := bufio.NewReaderSize(gz, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if ferr := fn(line); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// LoadDataset reads the cases of part from the build directory dir (every case where part is
// empty), in id order, and the Markdown of each document version their records name: its first
// limit bytes, or all of it for wholeDocuments.
func LoadDataset(dir, part string, limit int) (*Dataset, error) {
	ds := &Dataset{docs: map[docKey]string{}}
	err := eachGzipLine(filepath.Join(dir, casesFile), func(line []byte) error {
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return fmt.Errorf("%s: %w", casesFile, err)
		}
		if part == "" || c.Part == part {
			ds.Cases = append(ds.Cases, &c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ds.Cases, func(a, b *Case) int { return strings.Compare(a.ID, b.ID) })
	want := map[docKey]bool{}
	for _, c := range ds.Cases {
		for _, d := range c.Record.Documents {
			if !d.Missing {
				want[docKey{d.ArtifactID, d.Version}] = true
			}
		}
	}
	err = eachGzipLine(filepath.Join(dir, documentsFile), func(line []byte) error {
		var d struct {
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
			Markdown   string `json:"markdown"`
		}
		if err := json.Unmarshal(line, &d); err != nil {
			return fmt.Errorf("%s: %w", documentsFile, err)
		}
		if k := (docKey{d.ArtifactID, d.Version}); want[k] {
			if limit > 0 {
				d.Markdown = head(d.Markdown, limit)
			}
			ds.docs[k] = d.Markdown
		}
		return nil
	})
	return ds, err
}

// Case is the case of id, nil for none.
func (ds *Dataset) Case(id string) *Case {
	i, found := slices.BinarySearchFunc(ds.Cases, id, func(c *Case, id string) int { return strings.Compare(c.ID, id) })
	if !found {
		return nil
	}
	return ds.Cases[i]
}

// PersonQuestion is the owner's question a case's reader answers: for a question case his text
// about the agent's words, with the words he selected; for an answer case the question the agent
// text answers. Other cases have none.
func (c *Case) PersonQuestion() string {
	var q *CaseText
	switch c.Group {
	case GroupQuestion:
		q = c.OwnerReply
	case GroupAnswer:
		q = c.Question
	}
	if q == nil {
		return ""
	}
	text := strings.TrimSpace(q.Text)
	if len(q.Selected) > 0 {
		text += "\n\nHe selected these words of the text:\n"
		for _, s := range q.Selected {
			text += "- " + strings.TrimSpace(s) + "\n"
		}
	}
	return text
}

// How much of each part of a record a grading call reads. The text itself is never cut.
const (
	// docLimit is how much of one document the call reads, from its start.
	docLimit = 8000
	// docsShown is how many documents of the record the call reads; it lists the names of the
	// others.
	docsShown = 3
	// entryLimit is how much of one text of the thread the call reads, and threadShown how many
	// of the thread's newest texts.
	entryLimit  = 4000
	threadShown = 30
)

// cutMark stands where a part was cut.
const cutMark = "[…]"

// head is the first limit bytes of s, cut at a character boundary, and a mark where it cut.
func head(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + cutMark
}

// documentOrder is the names whose documents a record shows first, after the document the text
// stands in.
var documentOrder = []string{"spec.md", "decisions.md", "plan.md"}

// textDocument is the document the agent's text is a version of, "" for a text of another kind.
func (c *Case) textDocument() string {
	rest, ok := strings.CutPrefix(c.Agent.Key, "version:")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, ":")
	return id
}

// author names the writer of a thread text as a record shows it.
func (c *Case) author(t CaseText) string {
	switch {
	case t.ActorKind == "session" && t.Actor == c.WriterSession, t.Session != "" && t.Session == c.WriterSession:
		return "the writer agent"
	case t.ActorKind == "user" && t.Session == "":
		return "person " + t.Actor
	case t.ActorKind == "user", t.ActorKind == "session":
		return "another agent"
	}
	return t.ActorKind
}

// RecordPart is Dispatch's record of the case's text as a grading call reads it, each part in a
// tag of its own: the issue, the project, the documents, and the thread before the text. Each
// text it holds, a document's name and an author's among them, is embedded (messageTags).
func (ds *Dataset) RecordPart(c *Case) string {
	var b strings.Builder
	b.WriteString("<record>\n")
	if is := c.Record.Issue; is != nil {
		fmt.Fprintf(&b, "<issue key=%s>\n%s\n</issue>\n", strconv.Quote(is.Key), embedded(is.Title))
	}
	if c.Record.Project != "" {
		fmt.Fprintf(&b, "<project>\n%s\n</project>\n", embedded(c.Record.Project))
	}
	docs := slices.Clone(c.Record.Documents)
	own := c.textDocument()
	rank := func(d CaseDocument) int {
		if d.ArtifactID == own {
			return -1
		}
		if i := slices.Index(documentOrder, d.Name); i >= 0 {
			return i
		}
		return len(documentOrder)
	}
	slices.SortStableFunc(docs, func(a, b CaseDocument) int { return rank(a) - rank(b) })
	var others []string
	shown := 0
	for _, d := range docs {
		text, ok := ds.docs[docKey{d.ArtifactID, d.Version}]
		if !ok || shown == docsShown {
			others = append(others, d.Name)
			continue
		}
		shown++
		role := "a document of the issue"
		if d.ArtifactID == own {
			role = "the document the text is a version of, as it stood before the text"
		}
		fmt.Fprintf(&b, "<document name=%s version=%d role=%s>\n%s\n</document>\n", strconv.Quote(embedded(d.Name)), d.Version, strconv.Quote(role), embedded(head(text, docLimit)))
	}
	if len(others) > 0 {
		b.WriteString("<other_documents>\n")
		for _, name := range others {
			b.WriteString("- " + embedded(name) + "\n")
		}
		b.WriteString("</other_documents>\n")
	}
	thread := c.Record.Thread
	if len(thread) > threadShown {
		thread = thread[len(thread)-threadShown:]
	}
	if len(thread) > 0 {
		b.WriteString("<thread>\n")
		for _, t := range thread {
			fmt.Fprintf(&b, "<entry kind=%s author=%s at=%s>\n%s\n</entry>\n", strconv.Quote(t.Kind), strconv.Quote(embedded(c.author(t))), strconv.Quote(t.Time), embedded(head(t.Text, entryLimit)))
		}
		b.WriteString("</thread>\n")
	}
	b.WriteString("</record>\n")
	return b.String()
}

// Words is the number of words of text, counted as WordsBefore counts them: its runs of
// characters between white space that hold more than punctuation.
func Words(text string) int { return len(normalWords(text)) }

// FirstParagraph is the text's first paragraph: its lines up to the first blank line, after any
// blank lines it starts with.
func FirstParagraph(text string) string {
	text = strings.TrimLeft(strings.ReplaceAll(text, "\r\n", "\n"), " \t\n")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// WordsBefore is the number of words of text before the first place quote stands in it, matched
// word by word with case and punctuation at a word's ends ignored; when the whole quote is not
// found, its first words are tried, down to three. It is -1 when quote is not found.
func WordsBefore(text, quote string) int {
	words := normalWords(text)
	q := normalWords(quote)
	for n := len(q); n >= min(3, len(q)) && n > 0; n-- {
		if i := indexWords(words, q[:n]); i >= 0 {
			return i
		}
	}
	return -1
}

// normalWords is text's words, lowercased, with punctuation at each word's ends trimmed.
func normalWords(text string) []string {
	fields := strings.Fields(strings.ToLower(text))
	out := fields[:0]
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool { return strings.ContainsRune("\"'`*_()[]{}<>.,;:!?“”‘’«»-—–", r) })
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func indexWords(words, sub []string) int {
	for i := 0; i+len(sub) <= len(words); i++ {
		if slices.Equal(words[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}
