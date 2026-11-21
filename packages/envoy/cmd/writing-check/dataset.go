package main

// dataset builds the dataset of INFRA-3231's R1 from Dispatch's event log, offline: fetch reads
// the log up to an end time into one snapshot file, classify gives each text a person wrote one
// verdict of the selection prompt, build is a pure function of the snapshot, the verdicts and a
// seed, and pack puts those three inputs into one bundle that build --from reads. Every file
// these steps write holds Dispatch text, so each belongs under $TMPDIR and never in a repository;
// the repository holds the prompt (internal/dispatch/wordrecords/select.md, which the server's
// records job reads too) and the digests (dataset/DATASET). named (named.go) is apart from these
// steps: it builds cases from document versions read from the live Dispatch. README.md,
// "dataset", says what each step does.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	xrate "golang.org/x/time/rate"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/cmd/writing-check/harness"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

const (
	// dsFormat is the snapshot's format, written in its header.
	dsFormat = 1
	// dsNamingIssue is the issue whose owner texts name cases by their links.
	dsNamingIssue = "INFRA-3231"
	// dsHeldOutShare is the share of each case group that goes to the held-out part.
	dsHeldOutShare = 0.4
	// dsLogPage is the events one read of the log asks for, the route's most.
	dsLogPage = 1000
	// dsEndMargin is how far past the end time fetch reads before it stops: an event committed
	// late can carry a creation time a little below the events before it.
	dsEndMargin = 10 * time.Minute
	// dsStoreLastLoad is the transcript store's last load: a case written after it has no
	// session until the store is restored (Decision 16).
	dsStoreLastLoad = "2026-11-12T18:05:37Z"
	// dsPartSize is the largest part pack writes, under Dispatch's 25 MiB artifact bound.
	dsPartSize = 24 << 20
	// dsConfirmedSize is the size of R1's confirmation read: the draw reads 50 question cases.
	dsConfirmedSize = 50
)

// Bounds of classify's calls. The parts of each call's user message are cut at
// wordrecords.AgentChars, wordrecords.QuoteChars and wordrecords.PersonChars.
const (
	dsDefaultModel         = "claude-sonnet-4-6"
	dsDefaultKeyFile       = "/var/run/model-gateway-key/key"
	dsClassifyOutput       = 1024
	dsClassifyCallTime     = 180 * time.Second
	dsClassifyRetries      = 6
	dsClassifyBackoff      = 2 * time.Second
	dsClassifyReplyRetries = 3
)

// The case groups of R1, in the order the split takes them: the rarest first.
const (
	dsGroupQuestion = "question"
	dsGroupAnswer   = "answer"
	dsGroupPhrase   = "phrase"
	dsGroupPlain    = "plain"
)

var dsGroups = []string{dsGroupQuestion, dsGroupAnswer, dsGroupPhrase, dsGroupPlain}

// The two parts of the split.
const (
	dsHeldOut     = "held_out"
	dsDevelopment = "development"
)

// The kinds of text. An agent's text is a comment, a message, an ask or a document (R1's
// kinds); a person's text is one of those or an answer, a note on an ask or a review.
const (
	dsKindComment    = "comment"
	dsKindMessage    = "message"
	dsKindAsk        = "ask"
	dsKindDocument   = "document"
	dsKindAnswer     = "answer"
	dsKindResolution = "resolution"
	dsKindReview     = "review"
)

// How the agent text a person's text answers was found.
const (
	dsFoundReplyChain   = "reply_chain"
	dsFoundThread       = "thread"
	dsFoundConversation = "conversation"
	dsFoundIssue        = "issue"
	dsFoundDirectReply  = "direct_reply"
	dsFoundAct          = "act"
)

// dsCaseFamilies is the labelled sets' families whose writes are of R1's kinds, and the kind.
var dsCaseFamilies = map[string]string{
	"comment": dsKindComment, "delivery_reply": "", "send_to_agent": dsKindMessage,
	"message": dsKindMessage, "broadcast": dsKindMessage, "ask": dsKindAsk,
	"approval_summary": dsKindAsk, "ask_resolution": dsKindAsk, "document": dsKindDocument,
	"version_summary": dsKindDocument,
}

// dsPhraseShapes is the six phrase shapes that skills/plain-words held when the dataset was
// measured (the skill has since deleted its "name" row), each as the agent writes it
// (the expressions of INFRA-3231's decisions document, "How the problem's numbers were measured")
// and as the owner names it when he corrects it.
var dsPhraseShapes = []struct {
	name         string
	agent, owner *regexp.Regexp
}{
	{"land", regexp.MustCompile(`(?i)\bland(s|ed|ing)? the (change|PR|pull request|work)\b`), regexp.MustCompile(`(?i)\bland(s|ed|ing)?\b`)},
	{"unblock", regexp.MustCompile(`(?i)\bunblocks? the\b`), regexp.MustCompile(`(?i)\bunblock(s|ed|ing)?\b`)},
	{"name", regexp.MustCompile(`(?i)\b(names|named|naming) (the|a|an|its|each|every|what|who|which|that)\b`), regexp.MustCompile(`(?i)\bnam(e|es|ed|ing)\b`)},
	{"pay", regexp.MustCompile(`(?i)\bpays? (for itself|off)\b`), regexp.MustCompile(`(?i)\bpa(y|ys|id|ying)\b`)},
	{"carry", regexp.MustCompile(`(?i)\bcarr(y|ies|ied) (it|the finding|that|this)\b`), regexp.MustCompile(`(?i)\bcarr(y|ies|ied|ying)\b`)},
	{"show", regexp.MustCompile(`(?i)\b(shows|showed|showing) (the|a|an|that|what|how|its|no)\b`), regexp.MustCompile(`(?i)\bshow(s|ed|n|ing)?\b`)},
}

// dsQuoted is a quoted span of one line: in double quotes, curly quotes or backticks, or in
// single quotes that stand apart from the words around them, so an apostrophe opens none.
var dsQuoted = regexp.MustCompile("\"[^\"\\n]{1,80}\"|“[^”\\n]{1,80}”|‘[^’\\n]{1,80}’|`[^`\\n]{1,80}`|(?:^|[\\s(\\[:,])'[^'\\n]{1,60}'(?:$|[\\s).,;:!?\\]])")

// dsLink is a link to a comment, an ask or a message in a text: dispatch://KEY/comment/<id> or
// the web app's address of one.
var dsLink = regexp.MustCompile(`\b(comment|ask|message)s?/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)

// dsCarriedForward opens the reason Dispatch itself writes on an approval it carries forward
// (docs/approval_carry.go), which is no person's text.
const dsCarriedForward = "Carried forward from the approval"

const dsUsage = `usage: writing-check dataset <step> [flags]

steps:
  fetch        read the event log up to an end time, every body an event cut and the document versions the cases read, into one snapshot
  classify     give each text a person wrote one verdict of the selection prompt (internal/dispatch/wordrecords/select.md)
  build        build the cases, their documents, the session list and the counts from a snapshot, its verdicts and a seed
  pack         put the snapshot, the verdicts and the seed into one bundle that build --from reads
  sessions     check the session export's parts and give each case its place in its writer session
  owner-texts  write each text the owner wrote himself, with its time, from the snapshot build reads
  followups    write what the owner did after each case's text, and the question cases' explanations as answer cases
  named        build plain cases of the development part from the document versions --doc lists, read from the live Dispatch
  lists        write the case lists of harness score --case-list: the question, answer and plain cases of the development part

writing-check dataset <step> -h lists its flags.
`

func runDataset(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, dsUsage)
		return 2
	}
	switch args[0] {
	case "fetch":
		return runDatasetFetch(ctx, args[1:], stdout, stderr)
	case "classify":
		return runDatasetClassify(ctx, args[1:], stdout, stderr)
	case "build":
		return runDatasetBuild(args[1:], stdout, stderr)
	case "pack":
		return runDatasetPack(args[1:], stdout, stderr)
	case "sessions":
		return runDatasetSessions(args[1:], stdout, stderr)
	case "owner-texts":
		return runDatasetOwnerTexts(args[1:], stdout, stderr)
	case "followups":
		return runDatasetFollowups(args[1:], stdout, stderr)
	case "named":
		return runDatasetNamed(ctx, args[1:], stdout, stderr)
	case "lists":
		return runDatasetLists(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, dsUsage)
		return 0
	}
	fmt.Fprintf(stderr, "writing-check dataset: unknown step %q\n\n%s", args[0], dsUsage)
	return 2
}

// ---------------------------------------------------------------------------------------------
// The snapshot file.

// snapLine is one line of a snapshot, a gzipped JSONL file, told apart by Kind:
//
//   - "header", first: the end time, the owner, the issue whose links name cases, and counts;
//   - "event": one row of GET /api/v1/events/log as Dispatch served it, in id order;
//   - "body": the whole body of a comment or a message whose event cut it (body_truncated);
//   - "version": the Markdown of one document version that a case's text or record reads;
//   - "label": one write of the labelled sets that holds a phrase stating no fact.
//
// Text is nil where Dispatch would not serve the body or the version, and Unreadable says why.
// Kind is the first field, so a reader can tell a line's kind before it decodes the line.
type snapLine struct {
	Kind        string          `json:"kind"`
	Format      int             `json:"format,omitempty"`
	End         string          `json:"end,omitempty"`
	Owner       string          `json:"owner,omitempty"`
	NamingIssue string          `json:"naming_issue,omitempty"`
	FirstEvent  int64           `json:"first_event,omitempty"`
	LastEvent   int64           `json:"last_event,omitempty"`
	Events      int             `json:"events,omitempty"`
	Bodies      int             `json:"bodies,omitempty"`
	Versions    int             `json:"versions,omitempty"`
	Labels      int             `json:"labels,omitempty"`
	Event       json.RawMessage `json:"event,omitempty"`
	Of          string          `json:"of,omitempty"`
	ID          string          `json:"id,omitempty"`
	ArtifactID  string          `json:"artifact_id,omitempty"`
	Number      int             `json:"number,omitempty"`
	Text        *string         `json:"text,omitempty"`
	Unreadable  string          `json:"unreadable,omitempty"`
	EventID     int64           `json:"event_id,omitempty"`
	SourceID    string          `json:"source_id,omitempty"`
	Family      string          `json:"family,omitempty"`
	Sets        []string        `json:"sets,omitempty"`
	Shapes      []string        `json:"shapes,omitempty"`
}

// snapKind is the kind of a snapshot line, read from its first field.
func snapKind(line []byte) string {
	const prefix = `{"kind":"`
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return ""
	}
	rest := line[len(prefix):]
	if i := bytes.IndexByte(rest, '"'); i >= 0 {
		return string(rest[:i])
	}
	return ""
}

// eachGzipLine calls fn with every non-blank line of a gzipped file.
func eachGzipLine(path string, fn func(line []byte) error) error {
	return eachGzipLineTo(path, nil, fn)
}

// eachGzipLineTo is eachGzipLine that, when raw is not nil, also writes every byte of the file
// to raw, read to its end, on the same pass: raw holds the whole file once the lines are read.
func eachGzipLineTo(path string, raw io.Writer, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var file io.Reader = f
	if raw != nil {
		file = io.TeeReader(f, raw)
	}
	gz, err := gzip.NewReader(bufio.NewReaderSize(file, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer gz.Close()
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<30)
	for n := 1; scanner.Scan(); n++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if err := fn(scanner.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if raw != nil {
		// What the buffer had not read when the gzip stream ended is the file's too.
		if _, err := io.Copy(io.Discard, file); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// dsGzip is a gzip writer whose bytes depend only on what is written to it: no name and no time
// in its header.
func dsGzip(w io.Writer) *gzip.Writer {
	gz, _ := gzip.NewWriterLevel(w, gzip.DefaultCompression)
	return gz
}

// dsJSONLine writes v as one JSON line.
func dsJSONLine(w io.Writer, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = w.Write(line)
	return err
}

// ---------------------------------------------------------------------------------------------
// The index: the texts of the log and the links between them.

// dsText is one text of the log: what a person or an agent wrote in one event. Key names the
// text: "comment:<id>", "message:<id>", "ask:<id>" (an edit is a later version of the same key),
// "answer:<ask id>:<event id>", "resolution:<ask id>:<event id>", "review:<event id>",
// "version:<artifact id>:<n>" and "summary:<artifact id>:<n>".
type dsText struct {
	Key       string
	EventID   int64
	EventType string
	Time      string
	Project   string
	Issue     string
	Kind      string
	// Family is the family the labelled sets give the write (writes.go's familyOrder).
	Family    string
	SourceID  string
	ActorKind string
	ActorID   string
	// Session is the writer session of an agent's text, and Person the login of a person who
	// wrote the text themselves; a daemon's or Dispatch's own text has neither.
	Session string
	Person  string
	Agent   bool
	Text    string
	// Cut is a text whose whole body the snapshot does not hold, kept as the event carried it.
	Cut bool
	// Parent is the key of the text this one replies to, Thread the key of its thread's head
	// ("" for a document version), and Conv, for a message on no issue, the session it is with.
	Parent       string
	Thread       string
	Conv         string
	Conversation bool
	// Artifact and Version are a document version's; Quote is the part of a document a comment
	// points to; Selected the labels an answer chose.
	Artifact string
	Version  int
	Quote    string
	Selected []string
	// needBody is "comment" or "message" when the event cut the body, and bodySessions the
	// sessions a message's whole body is read through.
	needBody     string
	bodySessions []string
	// copyOf is, for a copy of a broadcast after the first, the first copy: the one write the
	// broadcast counts as. Such a copy stands in its own recipient's thread and nowhere else.
	copyOf *dsText
}

// written is the text that counts as the write: a broadcast's first copy for any of its copies,
// else the text itself.
func (t *dsText) written() *dsText {
	if t.copyOf != nil {
		return t.copyOf
	}
	return t
}

// tid is the text's id across its versions: its key and the event that wrote it.
func (t *dsText) tid() string { return t.Key + "@" + strconv.FormatInt(t.EventID, 10) }

// isVersion is whether the text is a document version's Markdown.
func (t *dsText) isVersion() bool {
	return t.Kind == dsKindDocument && strings.HasPrefix(t.Key, "version:")
}

// usable is whether the text holds words: a document version counts before its Markdown is read.
func (t *dsText) usable() bool { return t.isVersion() || strings.TrimSpace(t.Text) != "" }

// caseKind is whether an agent's text can be a case: one of R1's kinds, with words.
func (t *dsText) caseKind() bool {
	switch t.Kind {
	case dsKindComment, dsKindMessage, dsKindAsk, dsKindDocument:
		return t.Agent && strings.TrimSpace(t.Text) != ""
	}
	return false
}

type dsVerKey struct {
	artifact string
	number   int
}

func versionKey(artifact string, number int) string {
	return "version:" + artifact + ":" + strconv.Itoa(number)
}

type dsVersionRef struct {
	number  int
	eventID int64
}

type dsArtifact struct {
	id, kind, issue, project string
	// names is each name the document had from an event on. Dispatch calls a document's name its
	// title (artifact_rename.go), so each is a titleEntry.
	names    []titleEntry
	versions []dsVersionRef
}

// before is the version in force just before event, nil when none was written before it.
func (a *dsArtifact) before(event int64) *dsVersionRef {
	if a == nil {
		return nil
	}
	i := sort.Search(len(a.versions), func(i int) bool { return a.versions[i].eventID >= event })
	if i == 0 {
		return nil
	}
	return &a.versions[i-1]
}

func (a *dsArtifact) has(number int) bool {
	return slices.ContainsFunc(a.versions, func(v dsVersionRef) bool { return v.number == number })
}

type dsIssue struct {
	project, parent string
	titles          []titleEntry
}

// titleAt is the title entries give just before event: the last one set before it, else the
// first.
func titleAt(entries []titleEntry, event int64) string {
	title := ""
	for _, e := range entries {
		if e.eventID >= event {
			break
		}
		title = e.title
	}
	if title == "" && len(entries) > 0 {
		title = entries[0].title
	}
	return title
}

// withTitle is entries with title set from event on. An empty title, or the one already in
// force, adds nothing.
func withTitle(entries []titleEntry, event int64, title string) []titleEntry {
	if title == "" || len(entries) > 0 && entries[len(entries)-1].title == title {
		return entries
	}
	return append(entries, titleEntry{eventID: event, title: title})
}

// dsAct is an act of the owner on an agent's text that wrote no text: an answer that only chose
// options, or a review with no reason of his own.
type dsAct struct {
	eventID   int64
	eventType string
	time      string
	target    string
	selected  []string
}

// dsIndex is what one pass over the log learns: every text, the links between texts, each
// issue's title and parent, each document's versions, and the owner's acts.
type dsIndex struct {
	owner, namingIssue, end string
	header                  snapLine
	events                  int
	firstEvent, lastEvent   int64
	versionBytes            int64
	// endAt is the end of a snapshot's header, which loadIndex reads, and endFault the first
	// fault loadIndex finds against it: an end that is no RFC 3339 time, or an event created
	// after it or at no such time. loadIndex refuses a snapshot with one, so every step that
	// reads a snapshot does.
	endAt    time.Time
	endFault error

	texts          map[string][]*dsText
	byEvent        map[int64][]*dsText
	all            []*dsText
	children       map[string][]*dsText
	threadTexts    map[string][]*dsText
	threadAgents   map[string][]*dsText
	issueAgents    map[string][]*dsText
	convAgents     map[string][]*dsText
	issues         map[string]*dsIssue
	artifacts      map[string]*dsArtifact
	issueArtifacts map[string][]string
	askApproval    map[string]string
	broadcasts     map[string]*dsText // the first copy of each broadcast, by its id
	copies         []*dsText          // every later copy of a broadcast
	acts           []dsAct
	labels         []snapLine

	bodies      map[string]snapLine
	versionsIn  map[dsVerKey]bool
	versionSHA  map[dsVerKey]string
	versionText map[dsVerKey]string
}

func newDSIndex(owner, namingIssue string) *dsIndex {
	return &dsIndex{
		owner: canonicalLogin(owner), namingIssue: namingIssue,
		texts: map[string][]*dsText{}, byEvent: map[int64][]*dsText{}, children: map[string][]*dsText{},
		threadTexts: map[string][]*dsText{}, threadAgents: map[string][]*dsText{}, issueAgents: map[string][]*dsText{},
		convAgents: map[string][]*dsText{}, issues: map[string]*dsIssue{}, artifacts: map[string]*dsArtifact{},
		issueArtifacts: map[string][]string{}, askApproval: map[string]string{}, broadcasts: map[string]*dsText{},
		bodies: map[string]snapLine{}, versionsIn: map[dsVerKey]bool{}, versionSHA: map[dsVerKey]string{},
		versionText: map[dsVerKey]string{},
	}
}

// newText is a text of ev with the event's actor: an agent's when the writing check reads the
// actor's writes, a person's own when a person wrote it with no session acting for them.
func (ix *dsIndex) newText(ev event, key, kind, family, sourceID string) *dsText {
	act := ev.Actor.actSession()
	checked, _ := writingcheck.Checks(ev.Actor.Kind, ev.Actor.ID, act)
	t := &dsText{
		Key: key, EventID: ev.ID, EventType: ev.Type, Time: ev.CreatedAt, Project: ev.Project,
		Issue: deref(ev.IssueKey), Kind: kind, Family: family, SourceID: sourceID,
		ActorKind: ev.Actor.Kind, ActorID: ev.Actor.ID, Agent: checked,
	}
	if checked {
		t.Session = ev.Actor.ID
		if ev.Actor.Kind == "user" {
			t.Session = act
		}
	}
	if ev.Actor.Kind == "user" && act == "" {
		t.Person = canonicalLogin(ev.Actor.ID)
	}
	return t
}

func (ix *dsIndex) add(t *dsText) {
	ix.texts[t.Key] = append(ix.texts[t.Key], t)
	ix.byEvent[t.EventID] = append(ix.byEvent[t.EventID], t)
	ix.all = append(ix.all, t)
	if t.Parent != "" {
		ix.children[t.Parent] = append(ix.children[t.Parent], t)
	}
	if t.Thread != "" {
		ix.threadTexts[t.Thread] = append(ix.threadTexts[t.Thread], t)
	}
	if !t.Agent {
		return
	}
	if t.Thread != "" {
		ix.threadAgents[t.Thread] = append(ix.threadAgents[t.Thread], t)
	}
	if t.Issue != "" {
		ix.issueAgents[t.Issue] = append(ix.issueAgents[t.Issue], t)
	}
	if t.Conv != "" {
		ix.convAgents[t.Conv] = append(ix.convAgents[t.Conv], t)
	}
}

// addCopy indexes a broadcast's later copy in its own recipient's thread alone: its key and the
// replies to it find it, and its thread's records hold it. It is no write of its own, so no list
// of texts, of agent texts or of the owner's texts holds it.
func (ix *dsIndex) addCopy(t *dsText) {
	ix.texts[t.Key] = append(ix.texts[t.Key], t)
	ix.threadTexts[t.Thread] = append(ix.threadTexts[t.Thread], t)
	ix.copies = append(ix.copies, t)
}

// threadOf is the thread of the text key names, the key itself when no text holds it.
func (ix *dsIndex) threadOf(key string) string {
	if ts := ix.texts[key]; len(ts) > 0 && ts[0].Thread != "" {
		return ts[0].Thread
	}
	return key
}

// at is the version of the text key names in force just before event, nil when none is. A
// broadcast's later copy answers its first copy, the one write.
func (ix *dsIndex) at(key string, event int64) *dsText {
	var found *dsText
	for _, t := range ix.texts[key] {
		if t.EventID >= event {
			break
		}
		found = t
	}
	if found == nil {
		return nil
	}
	return found.written()
}

func (ix *dsIndex) issue(key string) *dsIssue {
	is := ix.issues[key]
	if is == nil {
		is = &dsIssue{}
		ix.issues[key] = is
	}
	return is
}

func (ix *dsIndex) artifact(id string) *dsArtifact {
	a := ix.artifacts[id]
	if a == nil {
		a = &dsArtifact{id: id}
		ix.artifacts[id] = a
	}
	return a
}

// placeArtifact files a document under its issue.
func (ix *dsIndex) placeArtifact(a *dsArtifact, issue string) {
	if a.issue != "" || issue == "" {
		return
	}
	a.issue = issue
	ix.issueArtifacts[issue] = append(ix.issueArtifacts[issue], a.id)
}

// root is the top of an issue's ancestors, the issue itself when it has no parent.
func (ix *dsIndex) root(key string) string {
	for range 64 {
		is := ix.issues[key]
		if is == nil || is.parent == "" {
			return key
		}
		key = is.parent
	}
	return key
}

// addEvent learns what one event of the log holds.
func (ix *dsIndex) addEvent(raw []byte) error {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	ix.events++
	if ix.firstEvent == 0 {
		ix.firstEvent = ev.ID
	}
	ix.lastEvent = ev.ID
	if ix.endFault == nil && !ix.endAt.IsZero() {
		if at, err := time.Parse(time.RFC3339Nano, ev.CreatedAt); err != nil {
			ix.endFault = fmt.Errorf("the snapshot holds event %d, created at %q, which is no RFC 3339 time", ev.ID, ev.CreatedAt)
		} else if at.After(ix.endAt) {
			ix.endFault = fmt.Errorf("the snapshot holds event %d, created at %s, after its end %s", ev.ID, ev.CreatedAt, ix.end)
		}
	}
	var err error
	switch ev.Type {
	case "issue.created", "issue.updated":
		err = ix.addIssue(ev)
	case "artifact.created":
		err = ix.addArtifactCreated(ev)
	case "artifact.version":
		err = ix.addArtifactVersion(ev)
	case "artifact.renamed":
		err = ix.addArtifactRenamed(ev)
	case "comment.created", "comment.answered":
		err = ix.addComment(ev)
	case "message.created", "message.answered":
		err = ix.addMessage(ev)
	case "ask.opened", "ask.edited":
		err = ix.addAsk(ev)
	case "ask.answered":
		err = ix.addAskAnswer(ev)
	case "ask.resolved":
		err = ix.addAskResolution(ev)
	case "artifact.approved", "artifact.changes_requested":
		err = ix.addReview(ev)
	}
	if err != nil {
		return fmt.Errorf("event %d (%s): %w", ev.ID, ev.Type, err)
	}
	return nil
}

func (ix *dsIndex) addIssue(ev event) error {
	var p struct {
		Key               string          `json:"key"`
		Project           string          `json:"project"`
		Title             string          `json:"title"`
		Parent            json.RawMessage `json:"parent"`
		PrimaryArtifactID *string         `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.Key == "" {
		return nil
	}
	is := ix.issue(p.Key)
	is.project = cmp.Or(p.Project, is.project)
	if p.Parent != nil {
		var parent *string
		_ = json.Unmarshal(p.Parent, &parent)
		is.parent = deref(parent)
	}
	is.titles = withTitle(is.titles, ev.ID, p.Title)
	if id := deref(p.PrimaryArtifactID); ev.Type == "issue.created" && id != "" {
		a := ix.artifact(id)
		// Dispatch names an issue's primary document spec.md (internal/dispatch/api/issue_create.go).
		a.kind, a.names, a.project = "doc", withTitle(a.names, ev.ID, "spec.md"), cmp.Or(p.Project, ev.Project)
		ix.placeArtifact(a, p.Key)
		if !a.has(1) {
			ix.addVersion(ev, a, 1, "", 0)
		}
	}
	return nil
}

func (ix *dsIndex) addArtifactCreated(ev event) error {
	var p struct {
		Artifact struct {
			ID       string  `json:"id"`
			Kind     string  `json:"kind"`
			Name     string  `json:"name"`
			IssueKey *string `json:"issue_key"`
			Project  *string `json:"project"`
			Versions []struct {
				Number  int     `json:"number"`
				Size    int64   `json:"size"`
				Summary *string `json:"summary"`
			} `json:"versions"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.Artifact.ID == "" {
		return nil
	}
	a := ix.artifact(p.Artifact.ID)
	a.kind, a.names = p.Artifact.Kind, withTitle(a.names, ev.ID, p.Artifact.Name)
	a.project = cmp.Or(deref(p.Artifact.Project), ev.Project)
	ix.placeArtifact(a, cmp.Or(deref(p.Artifact.IssueKey), deref(ev.IssueKey)))
	if a.kind != "doc" {
		return nil
	}
	number, summary, size := 1, "", int64(0)
	if len(p.Artifact.Versions) > 0 {
		v := p.Artifact.Versions[0]
		number, summary, size = cmp.Or(v.Number, 1), deref(v.Summary), v.Size
	}
	if !a.has(number) {
		ix.addVersion(ev, a, number, summary, size)
	}
	return nil
}

func (ix *dsIndex) addArtifactVersion(ev event) error {
	var p struct {
		ArtifactID string `json:"artifact_id"`
		Name       string `json:"name"`
		Version    struct {
			Number  int     `json:"number"`
			MIME    *string `json:"mime"`
			Size    int64   `json:"size"`
			Summary *string `json:"summary"`
		} `json:"version"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	// A document's version carries no MIME type; a file's carries its own.
	if mime := deref(p.Version.MIME); p.ArtifactID == "" || mime != "" && !strings.HasPrefix(mime, "text/markdown") {
		return nil
	}
	a := ix.artifact(p.ArtifactID)
	if a.kind == "" {
		a.kind = "doc"
	}
	if a.kind != "doc" || a.has(p.Version.Number) {
		return nil
	}
	a.names = withTitle(a.names, ev.ID, p.Name)
	a.project = cmp.Or(a.project, ev.Project)
	ix.placeArtifact(a, deref(ev.IssueKey))
	ix.addVersion(ev, a, p.Version.Number, deref(p.Version.Summary), p.Version.Size)
	return nil
}

// addArtifactRenamed follows a person's correction of a document's name: a record written after
// the event shows the new name, and one written before it keeps the old one.
func (ix *dsIndex) addArtifactRenamed(ev event) error {
	var p struct {
		Artifact struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.Artifact.ID == "" {
		return nil
	}
	a := ix.artifact(p.Artifact.ID)
	a.names = withTitle(a.names, ev.ID, p.Artifact.Name)
	return nil
}

// addVersion records a document version written by ev: the version, its Markdown as a text
// (read from the snapshot later), and its summary as a text of its own.
func (ix *dsIndex) addVersion(ev event, a *dsArtifact, number int, summary string, size int64) {
	a.versions = append(a.versions, dsVersionRef{number: number, eventID: ev.ID})
	ix.versionBytes += size
	source := a.id + "@" + strconv.Itoa(number)
	t := ix.newText(ev, versionKey(a.id, number), dsKindDocument, "document", source)
	t.Issue, t.Project = cmp.Or(a.issue, t.Issue), cmp.Or(a.project, t.Project)
	t.Artifact, t.Version = a.id, number
	ix.add(t)
	if summary != "" {
		s := ix.newText(ev, "summary:"+a.id+":"+strconv.Itoa(number), dsKindDocument, "version_summary", source)
		s.Issue, s.Project, s.Artifact, s.Version, s.Text = t.Issue, t.Project, a.id, number, summary
		ix.add(s)
	}
}

func (ix *dsIndex) addComment(ev event) error {
	var p struct {
		ID            string  `json:"id"`
		Body          string  `json:"body"`
		BodyTruncated bool    `json:"body_truncated"`
		IssueKey      *string `json:"issue_key"`
		ReplyTo       *string `json:"reply_to"`
		Answers       *string `json:"answers"`
		AskID         *string `json:"ask_id"`
		Anchor        *struct {
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
			Quote      string `json:"quote"`
		} `json:"anchor"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return nil
	}
	family := "comment"
	if ev.Type == "comment.answered" {
		family = "delivery_reply"
	}
	t := ix.newText(ev, "comment:"+p.ID, dsKindComment, family, p.ID)
	t.Text, t.Issue = p.Body, cmp.Or(deref(p.IssueKey), t.Issue)
	if p.BodyTruncated {
		t.needBody, t.Cut = "comment", true
	}
	reply, head, ask := cmp.Or(deref(p.Answers), deref(p.ReplyTo)), cmp.Or(deref(p.ReplyTo), deref(p.Answers)), deref(p.AskID)
	switch {
	case reply != "":
		t.Parent, t.Thread, t.Conversation = "comment:"+reply, ix.threadOf("comment:"+head), true
	case ask != "":
		t.Parent, t.Thread, t.Conversation = "ask:"+ask, "ask:"+ask, true
	case p.Anchor != nil && p.Anchor.ArtifactID != "":
		t.Parent, t.Thread, t.Quote = versionKey(p.Anchor.ArtifactID, p.Anchor.Version), t.Key, p.Anchor.Quote
	default:
		t.Thread = t.Key
	}
	ix.add(t)
	return nil
}

func (ix *dsIndex) addMessage(ev event) error {
	var p struct {
		ID            string  `json:"id"`
		Body          string  `json:"body"`
		BodyTruncated bool    `json:"body_truncated"`
		Target        *string `json:"target"`
		IssueKey      *string `json:"issue_key"`
		InReplyTo     *string `json:"in_reply_to"`
		BroadcastID   *string `json:"broadcast_id"`
		Author        struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"author"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return nil
	}
	target, broadcast := deref(p.Target), deref(p.BroadcastID)
	family, source := "message", p.ID
	var first *dsText
	switch {
	case ev.Type == "message.answered":
		family = "delivery_reply"
	case broadcast != "":
		// A broadcast stores one message for each recipient, all with the one body: the first
		// copy is the write, and each later copy is that write in its own recipient's thread.
		family, source, first = "broadcast", broadcast, ix.broadcasts[broadcast]
	case target != "":
		family = "send_to_agent"
	}
	t := ix.newText(ev, "message:"+p.ID, dsKindMessage, family, source)
	t.Text, t.Issue = p.Body, cmp.Or(deref(p.IssueKey), t.Issue)
	if p.BodyTruncated {
		t.needBody, t.Cut = "message", true
		t.bodySessions = []string{strings.TrimPrefix(target, "session:"), p.Author.ID}
	}
	if r := deref(p.InReplyTo); r != "" {
		t.Parent, t.Thread, t.Conversation = "message:"+r, ix.threadOf("message:"+r), true
	} else {
		t.Thread = t.Key
	}
	if t.Issue == "" {
		if p.Author.Kind == "session" && p.Author.ID != "" {
			t.Conv = "session:" + p.Author.ID
		} else if session, ok := strings.CutPrefix(target, "session:"); ok {
			t.Conv = "session:" + session
		}
	}
	if first != nil {
		t.copyOf = first
		ix.addCopy(t)
		return nil
	}
	if family == "broadcast" {
		ix.broadcasts[broadcast] = t
	}
	ix.add(t)
	return nil
}

// dsAskPayload is the part of an ask.* event's payload the index reads.
type dsAskPayload struct {
	ID       string      `json:"id"`
	Kind     string      `json:"kind"`
	Question string      `json:"question"`
	Options  []askOption `json:"options"`
	IssueKey *string     `json:"issue_key"`
	Approval *struct {
		ArtifactID string `json:"artifact_id"`
		Version    int    `json:"version"`
	} `json:"approval"`
	Answer *struct {
		Text     *string  `json:"text"`
		Selected []string `json:"selected"`
	} `json:"answer"`
	Resolution *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"resolution"`
}

// dsAskText is an ask's question with its options, one line each.
func dsAskText(question string, options []askOption) string {
	var b strings.Builder
	b.WriteString(question)
	for _, o := range options {
		b.WriteString("\n- " + o.Label)
		if o.Description != "" {
			b.WriteString(": " + o.Description)
		}
	}
	return b.String()
}

func (ix *dsIndex) addAsk(ev event) error {
	var p dsAskPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return nil
	}
	family, text := "ask", dsAskText(p.Question, p.Options)
	if p.Kind == "approval" {
		// Dispatch writes an approval request's question and options; the requester's own
		// words are its summary.
		family, text = "approval_summary", approvalSummary(p.Question)
		if p.Approval != nil && p.Approval.ArtifactID != "" {
			ix.askApproval[p.ID] = versionKey(p.Approval.ArtifactID, p.Approval.Version)
		}
	}
	t := ix.newText(ev, "ask:"+p.ID, dsKindAsk, family, p.ID)
	if text == "" || ev.Type == "ask.edited" && !t.Agent {
		return nil
	}
	t.Text, t.Issue, t.Thread = text, cmp.Or(deref(p.IssueKey), t.Issue), t.Key
	ix.add(t)
	return nil
}

// askParent is the text an answer or a note on an ask replies to: the ask, or, for an approval
// request with no words of its requester, the document version it asks about.
func (ix *dsIndex) askParent(id string) string {
	if len(ix.texts["ask:"+id]) == 0 {
		if v, ok := ix.askApproval[id]; ok {
			return v
		}
	}
	return "ask:" + id
}

func (ix *dsIndex) addAskAnswer(ev event) error {
	var p dsAskPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" || p.Answer == nil {
		return nil
	}
	t := ix.newText(ev, "answer:"+p.ID+":"+strconv.FormatInt(ev.ID, 10), dsKindAnswer, "ask_answer", p.ID)
	if t.Person == "" {
		return nil
	}
	parent, text := ix.askParent(p.ID), deref(p.Answer.Text)
	if strings.TrimSpace(text) == "" {
		if t.Person == ix.owner {
			ix.acts = append(ix.acts, dsAct{eventID: ev.ID, eventType: ev.Type, time: ev.CreatedAt, target: parent, selected: p.Answer.Selected})
		}
		return nil
	}
	t.Text, t.Issue, t.Selected = text, cmp.Or(deref(p.IssueKey), t.Issue), p.Answer.Selected
	t.Parent, t.Thread, t.Conversation = parent, "ask:"+p.ID, true
	ix.add(t)
	return nil
}

func (ix *dsIndex) addAskResolution(ev event) error {
	var p dsAskPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" || p.Resolution == nil || strings.TrimSpace(p.Resolution.Reason) == "" {
		return nil
	}
	switch p.Resolution.Kind {
	case "declined", "retracted", "resolved":
	default:
		return nil
	}
	t := ix.newText(ev, "resolution:"+p.ID+":"+strconv.FormatInt(ev.ID, 10), dsKindResolution, "ask_resolution", p.ID)
	switch {
	case t.Person != "":
	case t.Agent:
		t.Kind = dsKindAsk
	default:
		return nil
	}
	t.Text, t.Issue = p.Resolution.Reason, cmp.Or(deref(p.IssueKey), t.Issue)
	t.Parent, t.Thread, t.Conversation = ix.askParent(p.ID), "ask:"+p.ID, true
	ix.add(t)
	return nil
}

func (ix *dsIndex) addReview(ev event) error {
	var p struct {
		ArtifactID string  `json:"artifact_id"`
		Version    int     `json:"version"`
		Reason     *string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ArtifactID == "" {
		return nil
	}
	target, reason := versionKey(p.ArtifactID, p.Version), strings.TrimSpace(deref(p.Reason))
	t := ix.newText(ev, "review:"+strconv.FormatInt(ev.ID, 10), dsKindReview, "review_reason", p.ArtifactID+"@"+strconv.Itoa(p.Version))
	if t.Person == "" {
		return nil
	}
	if reason == "" || strings.HasPrefix(reason, dsCarriedForward) {
		if t.Person == ix.owner {
			ix.acts = append(ix.acts, dsAct{eventID: ev.ID, eventType: ev.Type, time: ev.CreatedAt, target: target})
		}
		return nil
	}
	if a := ix.artifacts[p.ArtifactID]; a != nil {
		t.Issue = cmp.Or(t.Issue, a.issue)
	}
	t.Text, t.Parent, t.Artifact, t.Version = reason, target, p.ArtifactID, p.Version
	ix.add(t)
	return nil
}

// applyBodies puts the whole bodies the snapshot holds in place of the parts events carried. A
// broadcast's later copy holds its first copy's text, the broadcast's one body.
func (ix *dsIndex) applyBodies() {
	for _, t := range ix.all {
		if t.needBody == "" {
			continue
		}
		if b, ok := ix.bodies[t.needBody+":"+t.SourceID]; ok && b.Text != nil {
			t.Text, t.Cut = *b.Text, false
		}
	}
	for _, t := range ix.copies {
		t.Text, t.Cut = t.copyOf.Text, t.copyOf.Cut
	}
}

// dsLatestBefore is the last usable text of a list in event order written before event.
func dsLatestBefore(list []*dsText, event int64) *dsText {
	i := sort.Search(len(list), func(i int) bool { return list[i].EventID >= event })
	for i--; i >= 0; i-- {
		if list[i].usable() {
			return list[i]
		}
	}
	return nil
}

// dsFirstAfter is the first usable text of a list in event order written after event.
func dsFirstAfter(list []*dsText, event int64) *dsText {
	for i := sort.Search(len(list), func(i int) bool { return list[i].EventID > event }); i < len(list); i++ {
		if list[i].usable() {
			return list[i]
		}
	}
	return nil
}

// answered is the agent text that t replies to, and how it was found: up t's reply chain to the
// first agent text; else the last agent text before t in its thread, in its conversation, or on
// its issue.
func (ix *dsIndex) answered(t *dsText) (*dsText, string) {
	seen := map[string]bool{t.Key: true}
	for p := ix.at(t.Parent, t.EventID); p != nil && !seen[p.Key]; p = ix.at(p.Parent, p.EventID) {
		seen[p.Key] = true
		if p.Agent && p.usable() {
			return p, dsFoundReplyChain
		}
	}
	if t.Thread != "" && t.Thread != t.Key {
		if a := dsLatestBefore(ix.threadAgents[t.Thread], t.EventID); a != nil {
			return a, dsFoundThread
		}
	}
	if t.Conv != "" {
		if a := dsLatestBefore(ix.convAgents[t.Conv], t.EventID); a != nil {
			return a, dsFoundConversation
		}
	}
	if t.Issue != "" {
		if a := dsLatestBefore(ix.issueAgents[t.Issue], t.EventID); a != nil {
			return a, dsFoundIssue
		}
	}
	return nil, ""
}

// firstReply is the first agent text that replies to t: a direct reply, else the first agent
// text after it in its thread or its conversation.
func (ix *dsIndex) firstReply(t *dsText) (*dsText, string) {
	for _, c := range ix.children[t.Key] {
		if c.Agent && c.usable() && c.EventID > t.EventID {
			return c, dsFoundDirectReply
		}
	}
	if a := dsFirstAfter(ix.threadAgents[cmp.Or(t.Thread, t.Key)], t.EventID); a != nil {
		return a, dsFoundThread
	}
	if t.Conv != "" {
		if a := dsFirstAfter(ix.convAgents[t.Conv], t.EventID); a != nil {
			return a, dsFoundConversation
		}
	}
	return nil, ""
}

// personTexts is the texts people wrote themselves, in event order: comments, messages, asks,
// answers, notes on asks and reviews.
func (ix *dsIndex) personTexts() []*dsText {
	var out []*dsText
	for _, t := range ix.all {
		if t.Person != "" && t.Kind != dsKindDocument && t.usable() {
			out = append(out, t)
		}
	}
	return out
}

// ownerTexts is the owner's own texts, in event order.
func (ix *dsIndex) ownerTexts() []*dsText {
	var out []*dsText
	for _, t := range ix.personTexts() {
		if t.Person == ix.owner {
			out = append(out, t)
		}
	}
	return out
}

// labelText is the agent text a label names, nil when the snapshot holds none.
func (ix *dsIndex) labelText(l snapLine) *dsText {
	for _, t := range ix.byEvent[l.EventID] {
		if t.Agent && t.Family == l.Family && t.SourceID == l.SourceID {
			return t
		}
	}
	return nil
}

// dsNamed is a text the owner links on the naming issue.
type dsNamed struct {
	key  string
	text *dsText
}

// named is the texts the owner links in his own texts on the naming issue, in the order he first
// links them; text is nil for a link the snapshot holds no text for.
func (ix *dsIndex) named() []dsNamed {
	var out []dsNamed
	seen := map[string]bool{}
	for _, o := range ix.ownerTexts() {
		if o.Issue != ix.namingIssue {
			continue
		}
		for _, m := range dsLink.FindAllStringSubmatch(o.Text, -1) {
			key := m[1] + ":" + m[2]
			if seen[key] {
				continue
			}
			seen[key] = true
			n := dsNamed{key: key}
			if ts := ix.texts[key]; len(ts) > 0 {
				n.text = cmp.Or(ix.at(key, o.EventID), ts[0].written())
			}
			out = append(out, n)
		}
	}
	return out
}

// candidates is every agent text that can become a case, whatever the verdicts and the seed: the
// agent texts the owner's texts answer, the first agent reply to each of them, the texts he acted
// on, the labelled writes and the texts he links on the naming issue.
func (ix *dsIndex) candidates() []*dsText {
	var out []*dsText
	seen := map[*dsText]bool{}
	add := func(t *dsText) {
		if t != nil && t.Agent && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, o := range ix.ownerTexts() {
		a, _ := ix.answered(o)
		add(a)
		r, _ := ix.firstReply(o)
		add(r)
	}
	for _, act := range ix.acts {
		add(ix.at(act.target, act.eventID))
	}
	for _, l := range ix.labels {
		add(ix.labelText(l))
	}
	for _, n := range ix.named() {
		if n.text == nil {
			continue
		}
		add(n.text)
		r, _ := ix.firstReply(n.text)
		add(r)
		a, _ := ix.answered(n.text)
		add(a)
	}
	return out
}

// recordArtifacts is the documents a text's record holds: its issue's documents, or the project
// document it is a version of.
func (ix *dsIndex) recordArtifacts(t *dsText) []string {
	if t.Issue != "" {
		ids := slices.Clone(ix.issueArtifacts[t.Issue])
		slices.Sort(ids)
		return ids
	}
	if t.Artifact != "" {
		return []string{t.Artifact}
	}
	return nil
}

// textVersions is the versions a document version's text is read from: its own and the one
// before it, whose blocks it does not repeat.
func (ix *dsIndex) textVersions(t *dsText) []dsVerKey {
	if !t.isVersion() {
		return nil
	}
	out := []dsVerKey{{t.Artifact, t.Version}}
	if prev := ix.artifacts[t.Artifact].before(t.EventID); prev != nil {
		out = append(out, dsVerKey{t.Artifact, prev.number})
	}
	return out
}

// recordVersions is the versions of a text's record: each of its documents as it stood just
// before the text was written.
func (ix *dsIndex) recordVersions(t *dsText) []dsVerKey {
	var out []dsVerKey
	for _, id := range ix.recordArtifacts(t) {
		if ref := ix.artifacts[id].before(t.EventID); ref != nil {
			out = append(out, dsVerKey{id, ref.number})
		}
	}
	return out
}

// fillVersionText sets a document version's text from the Markdown loaded: the blocks it holds
// that the version before it does not (extract.go's newBlocks), or all of it for a first version
// and for one whose previous version Dispatch would not serve.
func (ix *dsIndex) fillVersionText(t *dsText) {
	if !t.isVersion() {
		return
	}
	current, ok := ix.versionText[dsVerKey{t.Artifact, t.Version}]
	if !ok {
		t.Text, t.Cut = "", ix.versionsIn[dsVerKey{t.Artifact, t.Version}]
		return
	}
	prev := ix.artifacts[t.Artifact].before(t.EventID)
	if prev == nil {
		t.Text = current
		return
	}
	previous, ok := ix.versionText[dsVerKey{t.Artifact, prev.number}]
	if !ok {
		t.Text = current
		return
	}
	t.Text = dsNewBlocks(previous, current)
}

// dsNewBlocks is the blocks of current that previous does not hold, as extract.go's newBlocks
// joins them: in document order, new rows of one table under its head once.
func dsNewBlocks(previous, current string) string {
	before := blockSet(splitBlocks(previous))
	var added []string
	head := ""
	for _, b := range splitBlocks(current) {
		if before[b.String()] {
			continue
		}
		if b.head != "" && b.head == head {
			added[len(added)-1] += "\n" + b.text
			continue
		}
		head = b.head
		added = append(added, b.String())
	}
	return strings.Join(added, "\n\n")
}

// loadIndex reads a snapshot into an index: its header, events, bodies and labels, and which
// versions it holds. It refuses a snapshot with a fault against its end (dsIndex.endFault), so
// build, classify, owner-texts and followups each refuse one. Version Markdown is read by
// loadVersions. raw, when not nil, gets every byte of the snapshot file on the same pass, so
// build hashes the snapshot without reading it again.
func loadIndex(path string, raw io.Writer) (*dsIndex, error) {
	var ix *dsIndex
	err := eachGzipLineTo(path, raw, func(line []byte) error {
		kind := snapKind(line)
		if ix == nil {
			if kind != "header" {
				return errors.New("a snapshot starts with its header")
			}
			var h snapLine
			if err := json.Unmarshal(line, &h); err != nil {
				return err
			}
			if h.Format != dsFormat {
				return fmt.Errorf("snapshot format %d, want %d", h.Format, dsFormat)
			}
			ix = newDSIndex(h.Owner, h.NamingIssue)
			ix.header, ix.end = h, h.End
			end, err := time.Parse(time.RFC3339Nano, h.End)
			if err != nil {
				ix.endFault = fmt.Errorf("the snapshot's end %q is no RFC 3339 time", h.End)
			}
			ix.endAt = end
			return nil
		}
		switch kind {
		case "event":
			var l struct {
				Event json.RawMessage `json:"event"`
			}
			if err := json.Unmarshal(line, &l); err != nil {
				return err
			}
			return ix.addEvent(l.Event)
		case "body":
			var l snapLine
			if err := json.Unmarshal(line, &l); err != nil {
				return err
			}
			ix.bodies[l.Of+":"+l.ID] = l
		case "version":
			var l struct {
				ArtifactID string `json:"artifact_id"`
				Number     int    `json:"number"`
				Unreadable string `json:"unreadable"`
			}
			if err := json.Unmarshal(line, &l); err != nil {
				return err
			}
			ix.versionsIn[dsVerKey{l.ArtifactID, l.Number}] = l.Unreadable == ""
		case "label":
			var l snapLine
			if err := json.Unmarshal(line, &l); err != nil {
				return err
			}
			ix.labels = append(ix.labels, l)
		default:
			return fmt.Errorf("unknown line kind %q", kind)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ix == nil {
		return nil, fmt.Errorf("%s holds no header", path)
	}
	if ix.endFault != nil {
		return nil, fmt.Errorf("%s: %w", path, ix.endFault)
	}
	ix.applyBodies()
	return ix, nil
}

// loadVersions reads the snapshot's versions again: the SHA-256 of each, and the Markdown of
// those keep names.
func (ix *dsIndex) loadVersions(path string, keep map[dsVerKey]bool) error {
	return eachGzipLine(path, func(line []byte) error {
		if snapKind(line) != "version" {
			return nil
		}
		var l snapLine
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		if l.Text == nil {
			return nil
		}
		k := dsVerKey{l.ArtifactID, l.Number}
		ix.versionSHA[k] = sha256Hex([]byte(*l.Text))
		if keep[k] {
			ix.versionText[k] = *l.Text
		}
		return nil
	})
}

// loadCandidateVersions reads the snapshot's versions again, keeping the Markdown of the document
// versions among the candidates, and fills each candidate version's text from it.
func (ix *dsIndex) loadCandidateVersions(path string) error {
	candidates := ix.candidates()
	keep := map[dsVerKey]bool{}
	for _, t := range candidates {
		for _, k := range ix.textVersions(t) {
			keep[k] = true
		}
	}
	if err := ix.loadVersions(path, keep); err != nil {
		return err
	}
	for _, t := range candidates {
		ix.fillVersionText(t)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// fetch

// dsPage is one page file of a fetch's work directory: the events after After through Last.
type dsPage struct {
	path        string
	after, last int64
}

var dsPageName = regexp.MustCompile(`^events-(\d+)-(\d+)\.jsonl\.gz$`)

// dsPages is the page files of a work directory, in log order, checked to follow one another.
func dsPages(dir string) ([]dsPage, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pages []dsPage
	for _, e := range entries {
		m := dsPageName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		after, _ := strconv.ParseInt(m[1], 10, 64)
		last, _ := strconv.ParseInt(m[2], 10, 64)
		pages = append(pages, dsPage{path: filepath.Join(dir, e.Name()), after: after, last: last})
	}
	slices.SortFunc(pages, func(a, b dsPage) int { return cmp.Compare(a.after, b.after) })
	for i := 1; i < len(pages); i++ {
		if pages[i].after != pages[i-1].last {
			return nil, fmt.Errorf("%s does not follow %s: delete the work directory's pages from it on", pages[i].path, pages[i-1].path)
		}
	}
	return pages, nil
}

// dsWriteAtomic writes data to path by a file beside it renamed over it.
func dsWriteAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fetchEventPages pages the log from where the work directory's pages end, writing each page's
// events created at or before end as one page file, until the log ends or its events pass end by
// dsEndMargin. It answers the last event id read.
func fetchEventPages(ctx context.Context, c *client, dir string, end time.Time, progress io.Writer) (int64, error) {
	donePath := filepath.Join(dir, "events.done")
	if data, err := os.ReadFile(donePath); err == nil {
		var done struct {
			Last int64 `json:"last"`
		}
		if err := json.Unmarshal(data, &done); err != nil {
			return 0, fmt.Errorf("%s: %w", donePath, err)
		}
		return done.Last, nil
	}
	pages, err := dsPages(dir)
	if err != nil {
		return 0, err
	}
	cursor := int64(0)
	if len(pages) > 0 {
		cursor = pages[len(pages)-1].last
	}
	stop := end.Add(dsEndMargin)
	lastReport, read := time.Now(), 0
	for {
		var page []json.RawMessage
		query := url.Values{"after": {strconv.FormatInt(cursor, 10)}, "limit": {strconv.Itoa(dsLogPage)}}
		if err := c.get(ctx, "/api/v1/events/log", query, &page); err != nil {
			return cursor, fmt.Errorf("reading the log after %d: %w", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		var buf bytes.Buffer
		gz := dsGzip(&buf)
		last, beyond := cursor, false
		for _, raw := range page {
			var head struct {
				ID        int64  `json:"id"`
				CreatedAt string `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &head); err != nil {
				return cursor, fmt.Errorf("an event after %d: %w", cursor, err)
			}
			at, err := time.Parse(time.RFC3339Nano, head.CreatedAt)
			if err != nil {
				return cursor, fmt.Errorf("event %d: %w", head.ID, err)
			}
			last = head.ID
			if at.After(stop) {
				beyond = true
			}
			if at.After(end) {
				continue
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				return cursor, fmt.Errorf("event %d: %w", head.ID, err)
			}
			compact.WriteByte('\n')
			if _, err := gz.Write(compact.Bytes()); err != nil {
				return cursor, err
			}
		}
		if err := gz.Close(); err != nil {
			return cursor, err
		}
		name := filepath.Join(dir, fmt.Sprintf("events-%d-%d.jsonl.gz", cursor, last))
		if err := dsWriteAtomic(name, buf.Bytes()); err != nil {
			return cursor, err
		}
		cursor = last
		read += len(page)
		if beyond {
			break
		}
		if time.Since(lastReport) > 10*time.Second {
			fmt.Fprintf(progress, "writing-check dataset fetch: through event %d, %d events read this run\n", cursor, read)
			lastReport = time.Now()
		}
	}
	data, _ := json.Marshal(map[string]int64{"last": cursor})
	return cursor, dsWriteAtomic(donePath, data)
}

// eachPageEvent calls fn with every event of the work directory's pages, in id order.
func eachPageEvent(dir string, fn func(raw []byte) error) error {
	pages, err := dsPages(dir)
	if err != nil {
		return err
	}
	for _, p := range pages {
		if err := eachGzipLine(p.path, fn); err != nil {
			return err
		}
	}
	return nil
}

// dsAppend opens a JSONL file of a work directory to add lines to it: a line a crash cut short
// is dropped first. It answers the file and the keys of the lines it holds.
func dsAppend(path string, key func(snapLine) string) (*os.File, map[string]bool, error) {
	lines, err := dsSortedLines(path, key)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	have := make(map[string]bool, len(lines))
	var keep int64
	for _, l := range lines {
		have[l.sortKey] = true
		keep = max(keep, l.end)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := f.Truncate(int64(keep)); err != nil {
		f.Close()
		return nil, nil, err
	}
	if _, err := f.Seek(int64(keep), io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, have, nil
}

func bodyLineKey(l snapLine) string    { return l.Of + ":" + l.ID }
func versionLineKey(l snapLine) string { return versionKey(l.ArtifactID, l.Number) }

// dsFetchJob is one read of the second fetch phase: a whole body or a document version.
type dsFetchJob struct {
	of, id     string
	sessions   []string
	artifactID string
	number     int
}

func runDatasetFetch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset fetch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endFlag := flags.String("end", "", "the end time, RFC 3339: the snapshot holds the events created at or before it (required)")
	out := flags.String("out", "", "the snapshot file, gzipped JSONL (required; keep it under $TMPDIR)")
	work := flags.String("work", "", "the work directory a stopped fetch continues from (default <out>.work)")
	labelled := flags.String("labelled", "", "the labelled sets: writing-check-sets-and-labels-tar-gz, as the tarball or its extracted directory")
	owner := flags.String("owner", "", "the person whose texts select the cases, by login (required)")
	concurrency := flags.Int("concurrency", 4, "reads in flight at once, all within one limit of requests a second")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	base, tokenFile := os.Getenv("DISPATCH_URL"), os.Getenv("DISPATCH_TOKEN_FILE")
	end, err := time.Parse(time.RFC3339Nano, *endFlag)
	switch {
	case flags.NArg() > 0:
		fmt.Fprintf(stderr, "writing-check dataset fetch: unexpected argument %q\n", flags.Arg(0))
		return 2
	case *out == "" || err != nil || strings.TrimSpace(*owner) == "":
		fmt.Fprintln(stderr, "writing-check dataset fetch: --out, an RFC 3339 --end and --owner are required")
		return 2
	case *concurrency < 1:
		fmt.Fprintln(stderr, "writing-check dataset fetch: --concurrency must be above 0")
		return 2
	case base == "" || tokenFile == "":
		fmt.Fprintln(stderr, "writing-check dataset fetch: DISPATCH_URL and DISPATCH_TOKEN_FILE must be set")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check dataset fetch: %v\n", err)
		fmt.Fprintln(stderr, "writing-check dataset fetch: run it again with the same flags to continue")
		return 1
	}
	if *work == "" {
		*work = *out + ".work"
	}
	for _, path := range [][2]string{{"--out", *out}, {"--work", *work}} {
		if err := harness.OutsideRepository(path[0], path[1]); err != nil {
			fmt.Fprintf(stderr, "writing-check dataset fetch: %v\n", err)
			return 2
		}
	}
	if err := os.MkdirAll(*work, 0o700); err != nil {
		return fail(err)
	}
	endText := end.UTC().Format(time.RFC3339Nano)
	if err := dsCheckWorkEnd(*work, endText, *owner); err != nil {
		return fail(err)
	}
	limit := xrate.NewLimiter(requestsPerSecond, 1)
	httpClient := &http.Client{Timeout: 120 * time.Second}
	clients := make([]*client, *concurrency)
	for i := range clients {
		clients[i] = &client{base: strings.TrimRight(base, "/"), tokenFile: tokenFile, http: httpClient, limit: limit}
	}
	started := time.Now()
	last, err := fetchEventPages(ctx, clients[0], *work, end, stderr)
	if err != nil {
		return fail(err)
	}
	ix := newDSIndex(*owner, dsNamingIssue)
	ix.end = endText
	if err := eachPageEvent(*work, ix.addEvent); err != nil {
		return fail(err)
	}
	if *labelled != "" {
		if ix.labels, err = readLabelled(*labelled); err != nil {
			return fail(err)
		}
	}
	fmt.Fprintf(stderr, "writing-check dataset fetch: %d events through event %d (%s)\n", ix.events, last, time.Since(started).Round(time.Second))

	bodies, haveBodies, err := dsAppend(filepath.Join(*work, "bodies.jsonl"), bodyLineKey)
	if err != nil {
		return fail(err)
	}
	defer bodies.Close()
	versions, haveVersions, err := dsAppend(filepath.Join(*work, "versions.jsonl"), versionLineKey)
	if err != nil {
		return fail(err)
	}
	defer versions.Close()
	var jobs []dsFetchJob
	queued := map[string]bool{}
	for _, t := range ix.all {
		k := t.needBody + ":" + t.SourceID
		if t.needBody != "" && !haveBodies[k] && !queued[k] {
			queued[k] = true
			jobs = append(jobs, dsFetchJob{of: t.needBody, id: t.SourceID, sessions: t.bodySessions})
		}
	}
	bodyJobs := len(jobs)
	var wanted []dsVerKey
	for _, t := range ix.candidates() {
		wanted = append(wanted, ix.textVersions(t)...)
		wanted = append(wanted, ix.recordVersions(t)...)
	}
	for _, v := range wanted {
		k := versionKey(v.artifact, v.number)
		if !haveVersions[k] && !queued[k] {
			queued[k] = true
			jobs = append(jobs, dsFetchJob{artifactID: v.artifact, number: v.number})
		}
	}
	fmt.Fprintf(stderr, "writing-check dataset fetch: %d whole bodies and %d document versions to read (%d and %d read before); the log's versions hold %d bytes in all\n",
		bodyJobs, len(jobs)-bodyJobs, len(haveBodies), len(haveVersions), ix.versionBytes)
	if err := dsRunFetchJobs(ctx, clients, jobs, bodies, versions, stderr); err != nil {
		return fail(err)
	}
	header := snapLine{Kind: "header", Format: dsFormat, End: endText, Owner: canonicalLogin(*owner), NamingIssue: dsNamingIssue,
		FirstEvent: ix.firstEvent, LastEvent: ix.lastEvent, Events: ix.events, Labels: len(ix.labels)}
	sum, err := assembleSnapshot(*out, *work, header, ix.labels)
	if err != nil {
		return fail(err)
	}
	requests := 0
	for _, c := range clients {
		requests += c.requests
	}
	fmt.Fprintf(stdout, "events: %d (event %d to %d, created at or before %s)\n", ix.events, ix.firstEvent, ix.lastEvent, endText)
	fmt.Fprintf(stdout, "requests: %d, wall time %s\n", requests, time.Since(started).Round(time.Second))
	fmt.Fprintf(stdout, "snapshot: %s\nsnapshot sha256: %s\n", *out, sum)
	return 0
}

// dsCheckWorkEnd refuses a work directory fetched for another end time or owner, and records
// them in a new one.
func dsCheckWorkEnd(dir, end, owner string) error {
	path := filepath.Join(dir, "fetch.json")
	want := map[string]string{"end": end, "owner": canonicalLogin(owner)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b, _ := json.Marshal(want)
		return dsWriteAtomic(path, b)
	}
	if err != nil {
		return err
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if got["end"] != want["end"] || got["owner"] != want["owner"] {
		return fmt.Errorf("%s was fetched for end %s and owner %s: give those, or another --work", dir, got["end"], got["owner"])
	}
	return nil
}

// dsRunFetchJobs makes the reads of jobs with one worker per client, and writes each result as
// one line of the bodies or the versions file as it comes.
func dsRunFetchJobs(ctx context.Context, clients []*client, jobs []dsFetchJob, bodies, versions io.Writer, progress io.Writer) error {
	if len(jobs) == 0 {
		return nil
	}
	var mu sync.Mutex
	var firstErr error
	done := 0
	lastReport := time.Now()
	feed := make(chan dsFetchJob)
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(src *liveSource) {
			defer wg.Done()
			for j := range feed {
				line := snapLine{Kind: "body", Of: j.of, ID: j.id}
				var text string
				var err error
				switch {
				case j.of == "comment":
					text, err = src.commentBody(ctx, j.id)
				case j.of == "message":
					text, err = src.messageBody(ctx, j.id, j.sessions)
				default:
					line = snapLine{Kind: "version", ArtifactID: j.artifactID, Number: j.number}
					text, err = src.documentVersion(ctx, j.artifactID, j.number)
				}
				switch {
				case err == nil:
					line.Text = &text
				case unreadable(err):
					line.Unreadable = err.Error()
				default:
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				w := bodies
				if line.Kind == "version" {
					w = versions
				}
				if err := dsJSONLine(w, line); err != nil && firstErr == nil {
					firstErr = err
				}
				done++
				if time.Since(lastReport) > 10*time.Second {
					fmt.Fprintf(progress, "writing-check dataset fetch: %d of %d reads\n", done, len(jobs))
					lastReport = time.Now()
				}
				mu.Unlock()
			}
		}(newLiveSource(c))
	}
feeding:
	for _, j := range jobs {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}
		select {
		case feed <- j:
		case <-ctx.Done():
			break feeding
		}
	}
	close(feed)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// dsLineAt is where one line of a work file starts and ends, by the key it sorts by.
type dsLineAt struct {
	sortKey    string
	start, end int64
}

// dsSortedLines is a work file's lines, the last of each key, sorted by key.
func dsSortedLines(path string, sortKey func(snapLine) string) ([]dsLineAt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byKey := map[string]dsLineAt{}
	r := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var l snapLine
			if json.Unmarshal(line, &l) == nil {
				k := sortKey(l)
				byKey[k] = dsLineAt{sortKey: k, start: offset, end: offset + int64(len(line))}
			}
		}
		offset += int64(len(line))
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	lines := make([]dsLineAt, 0, len(byKey))
	for _, l := range byKey {
		lines = append(lines, l)
	}
	slices.SortFunc(lines, func(a, b dsLineAt) int { return strings.Compare(a.sortKey, b.sortKey) })
	return lines, nil
}

// assembleSnapshot writes the snapshot: the header, the events in id order, the bodies by kind
// and id, the versions by artifact and number, and the labels by event and source. It answers
// the snapshot's SHA-256.
func assembleSnapshot(out, work string, header snapLine, labels []snapLine) (string, error) {
	bodyLines, err := dsSortedLines(filepath.Join(work, "bodies.jsonl"), bodyLineKey)
	if err != nil {
		return "", err
	}
	versionLines, err := dsSortedLines(filepath.Join(work, "versions.jsonl"), func(l snapLine) string {
		return fmt.Sprintf("%s:%010d", l.ArtifactID, l.Number)
	})
	if err != nil {
		return "", err
	}
	header.Bodies, header.Versions = len(bodyLines), len(versionLines)
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	gz := dsGzip(buffered)
	if err := dsJSONLine(gz, header); err != nil {
		return "", err
	}
	err = eachPageEvent(work, func(raw []byte) error {
		return dsJSONLine(gz, snapLine{Kind: "event", Event: json.RawMessage(raw)})
	})
	if err != nil {
		return "", err
	}
	for _, part := range []struct {
		path  string
		lines []dsLineAt
	}{{filepath.Join(work, "bodies.jsonl"), bodyLines}, {filepath.Join(work, "versions.jsonl"), versionLines}} {
		src, err := os.Open(part.path)
		if err != nil {
			return "", err
		}
		for _, l := range part.lines {
			buf := make([]byte, l.end-l.start)
			if _, err := src.ReadAt(buf, l.start); err != nil {
				src.Close()
				return "", err
			}
			if _, err := gz.Write(buf); err != nil {
				src.Close()
				return "", err
			}
		}
		src.Close()
	}
	for _, l := range labels {
		if err := dsJSONLine(gz, l); err != nil {
			return "", err
		}
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := buffered.Flush(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readLabelled reads the labelled sets, as their tarball or the directory it was extracted to:
// every labels.jsonl, without the planted copies (a record with a copy id), keeping each write
// labelled failure, that is, with a sentence the labeller found states no fact. A write labelled
// in two sets is one label with both sets and the shapes of both.
func readLabelled(path string) ([]snapLine, error) {
	type labelLine struct {
		EventID    int64  `json:"event_id"`
		SourceID   string `json:"source_id"`
		Copy       string `json:"copy"`
		Family     string `json:"family"`
		WriteLabel string `json:"write_label"`
		Sentences  []struct {
			Verdict string `json:"verdict"`
			Shape   string `json:"shape"`
		} `json:"sentences"`
	}
	byKey := map[string]*snapLine{}
	read := func(name string, r io.Reader) error {
		if filepath.Base(name) != "labels.jsonl" {
			return nil
		}
		set := filepath.Base(filepath.Dir(name))
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
		for n := 1; scanner.Scan(); n++ {
			if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
				continue
			}
			var l labelLine
			if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
				return fmt.Errorf("%s line %d: %w", name, n, err)
			}
			if l.Copy != "" || l.WriteLabel != writeFailure {
				continue
			}
			k := strconv.FormatInt(l.EventID, 10) + "/" + l.SourceID
			s := byKey[k]
			if s == nil {
				s = &snapLine{Kind: "label", EventID: l.EventID, SourceID: l.SourceID, Family: l.Family}
				byKey[k] = s
			}
			if !slices.Contains(s.Sets, set) {
				s.Sets = append(s.Sets, set)
			}
			for _, sentence := range l.Sentences {
				if sentence.Verdict == verdictVacuous && !slices.Contains(s.Shapes, sentence.Shape) {
					s.Shapes = append(s.Shapes, sentence.Shape)
				}
			}
		}
		return scanner.Err()
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			return read(p, f)
		})
	} else {
		err = dsEachTarFile(path, read)
	}
	if err != nil {
		return nil, err
	}
	labels := make([]snapLine, 0, len(byKey))
	for _, s := range byKey {
		slices.Sort(s.Sets)
		slices.Sort(s.Shapes)
		labels = append(labels, *s)
	}
	slices.SortFunc(labels, func(a, b snapLine) int {
		return cmp.Or(cmp.Compare(a.EventID, b.EventID), strings.Compare(a.SourceID, b.SourceID))
	})
	return labels, nil
}

// dsEachTarFile calls fn with every regular file of a gzipped tarball.
func dsEachTarFile(path string, fn func(name string, r io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(h.Name, tr); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------------------------
// classify

// dsVerdict is one line of the verdicts file: one text a person wrote and the verdict the
// selection prompt gave it, with the model the provider served and the prompt's SHA-256. A line
// with an Error holds no verdict and is classified again by the next run.
type dsVerdict struct {
	Text           string   `json:"text"`
	EventID        int64    `json:"event_id"`
	Person         string   `json:"person"`
	Verdict        string   `json:"verdict"`
	Words          []string `json:"words"`
	Model          string   `json:"model"`
	RequestedModel string   `json:"requested_model"`
	PromptSHA256   string   `json:"prompt_sha256"`
	InputSHA256    string   `json:"input_sha256"`
	InputTokens    int      `json:"input_tokens"`
	OutputTokens   int      `json:"output_tokens"`
	Attempts       int      `json:"attempts"`
	Error          string   `json:"error,omitempty"`
}

// readVerdicts reads a verdicts file: the last line of each text that holds no error, and the
// SHA-256 of the bytes it read. A line a crash cut short is skipped, and a missing file holds no
// verdict, as an empty one does.
func readVerdicts(path string) (map[string]dsVerdict, string, error) {
	out := map[string]dsVerdict{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, sha256Hex(nil), nil
	}
	if err != nil {
		return nil, "", err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var v dsVerdict
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &v) != nil || v.Error != "" {
			continue
		}
		out[v.Text] = v
	}
	return out, sha256Hex(data), nil
}

// classifyInput is the user message of one classify call (wordrecords.Input): the agent text the
// person's text replies to, the quoted part of a document, and the person's text.
func (ix *dsIndex) classifyInput(p *dsText) string {
	var agent wordrecords.Text
	if a, _ := ix.answered(p); a != nil {
		agent = wordrecords.Text{Kind: a.Kind, Text: a.Text}
	}
	return wordrecords.Input(agent, p.Quote, wordrecords.Text{Kind: p.Kind, Text: p.Text})
}

// dsClassifier makes classify's calls.
type dsClassifier struct {
	model     agentloop.Model
	requested string
	promptSHA string
	call      func(context.Context, agentloop.Request) (agentloop.Result, error)
	sleep     func(context.Context, time.Duration) bool
}

// classify gives one person's text its verdict: again after a backoff on a retryable provider
// error, and at once on a reply that is not one verdict of the schema.
func (c *dsClassifier) classify(ctx context.Context, p *dsText, input string) dsVerdict {
	v := dsVerdict{Text: p.tid(), EventID: p.EventID, Person: p.Person, RequestedModel: c.requested,
		PromptSHA256: c.promptSHA, InputSHA256: sha256Hex([]byte(input)), Words: []string{}}
	zero := 0.0
	wait, backoffs, replies := dsClassifyBackoff, 0, 0
	for attempt := 1; ; attempt++ {
		result, err := c.call(ctx, agentloop.Request{
			Model:       c.model,
			System:      wordrecords.SelectPrompt,
			Input:       []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: input}}}},
			NoCache:     true,
			Bounds:      agentloop.Bounds{Time: dsClassifyCallTime, Calls: 1, OutputTokens: dsClassifyOutput},
			Final:       wordrecords.VerdictSchema,
			Temperature: &zero,
		})
		v.Attempts = attempt
		v.InputTokens += result.Usage.Input
		v.OutputTokens += result.Usage.Output
		if n := len(result.Steps); n > 0 {
			v.Model = result.Steps[n-1].Model
		}
		v.Error = ""
		switch {
		case err != nil:
			v.Error = err.Error()
			return v
		case result.Fault == agentloop.FaultProviderError || result.Fault == agentloop.FaultTime:
			v.Error = cmp.Or(result.Detail, string(result.Fault))
			if result.Fault == agentloop.FaultProviderError && !retryableDetail(result.Detail) ||
				backoffs == dsClassifyRetries || !c.sleep(ctx, wait) {
				return v
			}
			backoffs++
			wait *= 2
			continue
		case result.Fault != "":
			v.Error = cmp.Or(result.Detail, string(result.Fault))
		default:
			verdict, words, err := wordrecords.ParseVerdict(result.Value)
			if err != nil {
				v.Error = err.Error()
				break
			}
			v.Verdict, v.Words = verdict, words
			return v
		}
		if replies == dsClassifyReplyRetries {
			return v
		}
		replies++
	}
}

func runDatasetClassify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset classify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshot := flags.String("snapshot", "", "the snapshot fetch wrote (required)")
	out := flags.String("out", "", "the verdicts file, JSONL; a run continues the texts it holds no verdict for (required)")
	modelID := flags.String("model", dsDefaultModel, "the Claude model that classifies")
	endpoint := flags.String("endpoint", os.Getenv("ANTHROPIC_BASE_URL"), "the model gateway's base URL (Anthropic messages)")
	keyFile := flags.String("key-file", dsDefaultKeyFile, "a file holding the gateway's key (never printed)")
	concurrency := flags.Int("concurrency", 8, "calls in flight at once")
	limit := flags.Int("limit", 0, "classify at most this many texts in this run (0: all)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *snapshot == "" || *out == "" || flags.NArg() > 0 || *concurrency < 1 || *limit < 0 {
		fmt.Fprintln(stderr, "writing-check dataset classify: --snapshot and --out are required, --concurrency must be above 0, and nothing else")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset classify: %v\n", err)
		return 2
	}
	model, err := probeModel(string(agentloop.AnthropicMessages), *endpoint, *modelID, "", *keyFile, embed.EntraConfig{})
	if err != nil {
		fmt.Fprintf(stderr, "writing-check dataset classify: %v\n", err)
		return 2
	}
	model.Effort = "off"
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check dataset classify: %v\n", err)
		return 1
	}
	started := time.Now()
	ix, err := loadIndex(*snapshot, nil)
	if err != nil {
		return fail(err)
	}
	persons := ix.personTexts()
	keep := map[dsVerKey]bool{}
	var docs []*dsText
	for _, p := range persons {
		if a, _ := ix.answered(p); a != nil && a.isVersion() {
			docs = append(docs, a)
			for _, k := range ix.textVersions(a) {
				keep[k] = true
			}
		}
	}
	if err := ix.loadVersions(*snapshot, keep); err != nil {
		return fail(err)
	}
	for _, d := range docs {
		ix.fillVersionText(d)
	}
	done, _, err := readVerdicts(*out)
	if err != nil {
		return fail(err)
	}
	var todo []*dsText
	for _, p := range persons {
		if _, ok := done[p.tid()]; !ok {
			todo = append(todo, p)
		}
	}
	if *limit > 0 && len(todo) > *limit {
		todo = todo[:*limit]
	}
	fmt.Fprintf(stderr, "writing-check dataset classify: %d texts people wrote, %d with a verdict, %d to classify\n", len(persons), len(done), len(todo))
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer file.Close()
	c := &dsClassifier{model: model, requested: *modelID, promptSHA: sha256Hex([]byte(wordrecords.SelectPrompt)), call: agentloop.Run, sleep: sleepCtx}
	var mu sync.Mutex
	var writeErr error
	n, failed := 0, 0
	feed := make(chan *dsText)
	var wg sync.WaitGroup
	for range *concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range feed {
				v := c.classify(ctx, p, ix.classifyInput(p))
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				if writeErr == nil {
					writeErr = dsJSONLine(file, v)
				}
				n++
				if v.Error != "" {
					failed++
				}
				if n%250 == 0 {
					fmt.Fprintf(stderr, "writing-check dataset classify: %d of %d texts (%d failed), %s\n", n, len(todo), failed, time.Since(started).Round(time.Second))
				}
				mu.Unlock()
			}
		}()
	}
feeding:
	for _, p := range todo {
		select {
		case feed <- p:
		case <-ctx.Done():
			break feeding
		}
	}
	close(feed)
	wg.Wait()
	if writeErr != nil {
		return fail(writeErr)
	}
	all, _, err := readVerdicts(*out)
	if err != nil {
		return fail(err)
	}
	writeClassifySummary(stdout, ix, persons, all, *out, time.Since(started))
	if ctx.Err() != nil {
		return fail(fmt.Errorf("stopped: %w; run it again to continue", ctx.Err()))
	}
	if failed > 0 {
		return fail(fmt.Errorf("%d texts got no verdict; run it again to classify them", failed))
	}
	return 0
}

// writeClassifySummary prints the verdicts by person kind, the served models and the tokens of
// every call the verdicts file records, this run's and the runs' before it.
func writeClassifySummary(w io.Writer, ix *dsIndex, persons []*dsText, verdicts map[string]dsVerdict, path string, elapsed time.Duration) {
	owner, others := map[string]int{}, map[string]int{}
	models := map[string]int{}
	for _, p := range persons {
		v, ok := verdicts[p.tid()]
		label := v.Verdict
		if !ok {
			label = "(no verdict)"
		}
		if p.Person == ix.owner {
			owner[label]++
		} else {
			others[label]++
		}
		if ok {
			models[v.Model]++
		}
	}
	var in, out, calls int
	data, _ := os.ReadFile(path)
	for _, line := range bytes.Split(data, []byte("\n")) {
		var v dsVerdict
		if json.Unmarshal(line, &v) == nil {
			in += v.InputTokens
			out += v.OutputTokens
			calls += v.Attempts
		}
	}
	fmt.Fprintf(w, "texts people wrote: %d\n", len(persons))
	fmt.Fprintf(w, "the owner's verdicts (%s):\n", ix.owner)
	for _, k := range sortedKeys(owner) {
		fmt.Fprintf(w, "  %s: %d\n", k, owner[k])
	}
	fmt.Fprintln(w, "other people's verdicts:")
	for _, k := range sortedKeys(others) {
		fmt.Fprintf(w, "  %s: %d\n", k, others[k])
	}
	fmt.Fprintln(w, "served model ids:")
	for _, k := range sortedKeys(models) {
		fmt.Fprintf(w, "  %s: %d\n", cmp.Or(k, "(none)"), models[k])
	}
	fmt.Fprintf(w, "prompt sha256: %s\n", sha256Hex([]byte(wordrecords.SelectPrompt)))
	fmt.Fprintf(w, "calls recorded: %d; tokens: input %d, output %d\n", calls, in, out)
	fmt.Fprintf(w, "wall time: %s\n", elapsed.Round(time.Second))
}

// ---------------------------------------------------------------------------------------------
// build

// dsCase is one case of the dataset, one line of cases.jsonl.gz.
type dsCase struct {
	ID    string `json:"id"`
	Group string `json:"group"`
	Part  string `json:"part"`
	// SplitGroup is the key of the group the split keeps whole: the least tree of an issue tree
	// joined with the near copies of its texts; Tree is the case's own tree.
	SplitGroup   string `json:"split_group"`
	Tree         string `json:"tree"`
	Kind         string `json:"kind"`
	Conversation bool   `json:"conversation"`
	// WriterSession is the session that wrote the agent text, and WriteTime the place in it: the
	// session is cut there.
	WriterSession string        `json:"writer_session,omitempty"`
	WriteTime     string        `json:"write_time"`
	NamedByOwner  bool          `json:"named_by_owner,omitempty"`
	Found         string        `json:"found,omitempty"`
	Agent         dsCaseText    `json:"agent_text"`
	OwnerReply    *dsCaseText   `json:"owner_reply,omitempty"`
	Question      *dsCaseText   `json:"question,omitempty"`
	Phrase        *dsCasePhrase `json:"phrase,omitempty"`
	Record        dsCaseRecord  `json:"record"`

	agent *dsText
}

type dsCaseText struct {
	Key       string         `json:"key"`
	EventID   int64          `json:"event_id"`
	EventType string         `json:"event_type"`
	Time      string         `json:"time"`
	Kind      string         `json:"kind"`
	Family    string         `json:"family,omitempty"`
	SourceID  string         `json:"source_id,omitempty"`
	ActorKind string         `json:"actor_kind"`
	Actor     string         `json:"actor"`
	Session   string         `json:"session,omitempty"`
	Text      string         `json:"text"`
	Cut       bool           `json:"cut,omitempty"`
	SHA256    string         `json:"sha256"`
	Selected  []string       `json:"selected,omitempty"`
	Verdict   *dsCaseVerdict `json:"verdict,omitempty"`
}

type dsCaseVerdict struct {
	Verdict      string   `json:"verdict"`
	Words        []string `json:"words"`
	Model        string   `json:"model"`
	PromptSHA256 string   `json:"prompt_sha256"`
}

type dsCasePhrase struct {
	Shapes  []string `json:"shapes"`
	Sources []string `json:"sources"`
}

type dsCaseRecord struct {
	Project   string           `json:"project"`
	Issue     *dsCaseIssue     `json:"issue,omitempty"`
	Documents []dsCaseDocument `json:"documents"`
	Thread    []dsCaseText     `json:"thread"`
}

type dsCaseIssue struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Parent string `json:"parent,omitempty"`
	Root   string `json:"root"`
}

// dsCaseDocument is one document of a record at its version; its Markdown is the line of
// documents.jsonl.gz with the same artifact and version.
type dsCaseDocument struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	SHA256     string `json:"sha256,omitempty"`
	Missing    bool   `json:"missing,omitempty"`
}

// dsDocumentLine is one line of documents.jsonl.gz.
type dsDocumentLine struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
	SHA256     string `json:"sha256"`
	Markdown   string `json:"markdown"`
}

// dsSessionID is the form of a session id the transcript store and export-sessions.sh take.
var dsSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// writeSessionList writes sessions.tsv, the session list export-sessions.sh reads: one line
// "<session id>\t<cut time>" for each writer session of the cases, its cut the latest write time
// of its cases, in UTC, sorted by session id. A writer session whose id the store cannot hold
// (dsSessionID) is left out. It answers the file's SHA-256 and the sessions listed and left out.
func writeSessionList(path string, cases []*dsCase) (sum string, listed, unlisted int, err error) {
	cuts := map[string]time.Time{}
	other := map[string]bool{}
	for _, c := range cases {
		switch {
		case c.WriterSession == "":
		case !dsSessionID.MatchString(c.WriterSession):
			other[c.WriterSession] = true
		default:
			at, err := time.Parse(time.RFC3339Nano, c.WriteTime)
			if err != nil {
				return "", 0, 0, fmt.Errorf("case %s: %w", c.ID, err)
			}
			if cut, ok := cuts[c.WriterSession]; !ok || at.After(cut) {
				cuts[c.WriterSession] = at
			}
		}
	}
	var b strings.Builder
	for _, id := range sortedKeys(cuts) {
		b.WriteString(id + "\t" + cuts[id].UTC().Format(time.RFC3339Nano) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", 0, 0, err
	}
	return sha256Hex([]byte(b.String())), len(cuts), len(other), nil
}

// dsGroupCounts is one case group's counts.
type dsGroupCounts struct {
	Cases               int            `json:"cases"`
	HeldOut             int            `json:"held_out"`
	Development         int            `json:"development"`
	Kind                map[string]int `json:"kind"`
	WriterSession       map[string]int `json:"writer_session"`
	BeforeStoreLastLoad int            `json:"written_before_store_last_load"`
	Conversation        map[string]int `json:"conversation"`
	Found               map[string]int `json:"found"`
	NamedByOwner        int            `json:"named_by_owner"`
}

type dsPartCounts struct {
	Cases       int `json:"cases"`
	SplitGroups int `json:"split_groups"`
	Trees       int `json:"trees"`
}

// dsCounts is counts.json. It holds no text.
type dsCounts struct {
	End               string                   `json:"end"`
	Owner             string                   `json:"owner"`
	NamingIssue       string                   `json:"naming_issue"`
	Seed              uint64                   `json:"seed"`
	HeldOutShare      float64                  `json:"held_out_share"`
	Snapshot          map[string]int64         `json:"snapshot"`
	PersonTexts       int                      `json:"person_texts"`
	OwnerTexts        int                      `json:"owner_texts"`
	OwnerVerdicts     map[string]int           `json:"owner_verdicts"`
	VerdictModels     map[string]int           `json:"verdict_models"`
	PromptSHA256      map[string]int           `json:"prompt_sha256"`
	Cases             int                      `json:"cases"`
	Groups            map[string]dsGroupCounts `json:"groups"`
	Parts             map[string]dsPartCounts  `json:"parts"`
	NoTreeInBothParts bool                     `json:"no_tree_in_both_parts"`
	PlainPool         int                      `json:"plain_pool"`
	NamedLinks        map[string]int           `json:"named_links"`
	Labelled          map[string]int           `json:"labelled"`
	LeftOut           map[string]int           `json:"left_out"`
	CasesWithSession  int                      `json:"cases_with_session"`
	Sessions          int                      `json:"sessions"`
	SessionsListed    int                      `json:"sessions_listed"`
	SessionsUnlisted  int                      `json:"sessions_not_a_store_id"`
	SHA256            map[string]string        `json:"sha256"`
	ConfirmedSample   *dsConfirmedSample       `json:"confirmed_sample,omitempty"`
	ToDevelopment     *dsToDevelopmentCounts   `json:"to_development,omitempty"`
}

// dsConfirmedSample is the result of a fresh reader's read of a sample of question cases: how
// many it confirms ask about words, out of how many, the seed and the method of the draw, the
// reader, and the SHA-256 of the result file.
type dsConfirmedSample struct {
	Confirmed    int    `json:"confirmed"`
	Total        int    `json:"total"`
	Seed         uint64 `json:"seed"`
	Draw         string `json:"draw"`
	Reader       string `json:"reader"`
	ResultSHA256 string `json:"result_sha256"`
}

// readConfirmed reads the result of the confirmation read: {"reader", "confirmed", "total",
// "items": [{"case_id", "about_words"}...]}. questions are the build's question case ids, in any
// order. It refuses a result whose counts do not match its items, a draw of other than
// dsConfirmedSize cases, a case read twice, a case that is not a question case of this build, and
// items that are not the draw in its order, naming the first item that differs. The draw is
// python3's random.Random(seed).sample of the question case ids sorted as Python sorts strings, by
// code point, which is their byte order (pyRandom.sample).
func readConfirmed(path string, seed uint64, questions []string) (*dsConfirmedSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r struct {
		Reader    string `json:"reader"`
		Confirmed int    `json:"confirmed"`
		Total     int    `json:"total"`
		Items     []struct {
			CaseID     string `json:"case_id"`
			AboutWords bool   `json:"about_words"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	population := slices.Sorted(slices.Values(questions))
	confirmed, seen := 0, map[string]bool{}
	for _, item := range r.Items {
		_, question := slices.BinarySearch(population, item.CaseID)
		switch {
		case seen[item.CaseID]:
			return nil, fmt.Errorf("%s reads case %s twice", path, item.CaseID)
		case !question:
			return nil, fmt.Errorf("%s reads %s, which is not a question case of this build", path, item.CaseID)
		}
		seen[item.CaseID] = true
		if item.AboutWords {
			confirmed++
		}
	}
	if len(r.Items) != r.Total || confirmed != r.Confirmed || r.Reader == "" {
		return nil, fmt.Errorf("%s says %d of %d by %q, and its items give %d of %d", path, r.Confirmed, r.Total, r.Reader, confirmed, len(r.Items))
	}
	if r.Total != dsConfirmedSize {
		return nil, fmt.Errorf("%s reads %d question cases, and R1's confirmation read draws %d", path, r.Total, dsConfirmedSize)
	}
	// The items are distinct question cases, so the sample is no larger than its population.
	draw := fmt.Sprintf("python3 random.Random(%d).sample(sorted question cases by id, %d)", seed, r.Total)
	for i, j := range newPyRandom(seed).sample(len(population), r.Total) {
		if id := r.Items[i].CaseID; id != population[j] {
			return nil, fmt.Errorf("%s: item %d is %s, and the draw %s gives %s there", path, i+1, id, draw, population[j])
		}
	}
	return &dsConfirmedSample{
		Confirmed: confirmed, Total: r.Total, Seed: seed, Reader: r.Reader, ResultSHA256: sha256Hex(data), Draw: draw,
	}, nil
}

// dsBuildResult is what a build read and wrote: the SHA-256 of its snapshot and of its verdicts
// file, its counts, and the SHA-256 of the files it wrote.
type dsBuildResult struct {
	snapshotSHA string
	verdictsSHA string
	counts      dsCounts
	casesSHA    string
	documentSHA string
	sessionsSHA string
}

// dsInputs is what a build reads: the snapshot, the verdicts and the seed, and, when Confirmed
// names a file, the result of a fresh reader's read of a sample of question cases, drawn with
// ConfirmedSeed. ToDevelopment, when set, is the list of build --to-development: the split
// groups whose cases go to the development part whatever the split says. pack does not put it
// in a bundle.
type dsInputs struct {
	Snapshot, Verdicts string
	Seed               uint64
	Confirmed          string
	ConfirmedSeed      uint64
	ToDevelopment      *dsToDevelopment
}

// confirmedFlags are the flags of the confirmation read, which build and pack both take.
func confirmedFlags(flags *flag.FlagSet) (*string, *uint64) {
	return flags.String("confirmed", "", "the result of a fresh reader's read of a sample of question cases (confirm-result.json)"),
		flags.Uint64("confirmed-seed", 0, "the seed the sample of --confirmed was drawn with (required with --confirmed)")
}

func runDatasetBuild(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshot := flags.String("snapshot", "", "the snapshot fetch wrote")
	verdicts := flags.String("verdicts", "", "the verdicts classify wrote")
	seed := flags.Uint64("seed", 0, "the seed of the plain cases' draw and of the split")
	confirmed, confirmedSeed := confirmedFlags(flags)
	from := flags.String("from", "", "a bundle pack wrote, in place of the other inputs: the tarball, its manifest, or a directory holding one")
	out := flags.String("out", "", "the directory for cases.jsonl.gz, documents.jsonl.gz, sessions.tsv and counts.json (required; keep it under $TMPDIR)")
	toDevelopment := flags.String("to-development", "", "a list of split-group keys, one a line, whose cases go to the development part whatever the split says; its SHA-256 must be the one dataset.go commits (dsToDevelopmentSHA256)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	direct := *snapshot != "" || *verdicts != "" || flagGiven(flags, "seed") || *confirmed != "" || flagGiven(flags, "confirmed-seed")
	switch {
	case flags.NArg() > 0 || *out == "":
		fmt.Fprintln(stderr, "writing-check dataset build: --out is required, and nothing but flags")
		return 2
	case *from != "" && direct:
		fmt.Fprintln(stderr, "writing-check dataset build: give --from, or --snapshot, --verdicts and --seed, not both")
		return 2
	case *from == "" && (*snapshot == "" || *verdicts == "" || !flagGiven(flags, "seed")):
		fmt.Fprintln(stderr, "writing-check dataset build: give --snapshot, --verdicts and --seed, or --from")
		return 2
	case (*confirmed != "") != flagGiven(flags, "confirmed-seed"):
		fmt.Fprintln(stderr, "writing-check dataset build: give --confirmed and --confirmed-seed together")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset build: %v\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check dataset build: %v\n", err)
		return 1
	}
	// The list is read first, so a list the repository does not commit is refused before the
	// bundle is joined.
	var toDev *dsToDevelopment
	if *toDevelopment != "" {
		list, err := readToDevelopment(*toDevelopment, dsToDevelopmentSHA256)
		if err != nil {
			return fail(err)
		}
		toDev = list
	}
	in := dsInputs{Snapshot: *snapshot, Verdicts: *verdicts, Seed: *seed, Confirmed: *confirmed, ConfirmedSeed: *confirmedSeed}
	if *from != "" {
		// The bundle's files are joined in a directory under --out, removed once the build ends.
		// A refused build leaves --out as it was (buildDataset), so an --out made for the bundle
		// is removed again with it where the build left it empty.
		_, err := os.Stat(*out)
		made := errors.Is(err, os.ErrNotExist)
		dir := ""
		defer func() {
			if dir != "" {
				os.RemoveAll(dir)
			}
			if made {
				os.Remove(*out)
			}
		}()
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return fail(err)
		}
		if dir, err = os.MkdirTemp(*out, ".bundle-"); err != nil {
			return fail(err)
		}
		if in, err = openBundle(*from, dir); err != nil {
			return fail(err)
		}
	}
	in.ToDevelopment = toDev
	started := time.Now()
	result, err := buildDataset(in, *out)
	if err != nil {
		return fail(err)
	}
	c := result.counts
	fmt.Fprintf(stdout, "| Group | Cases | Held out | Development |\n| --- | ---: | ---: | ---: |\n")
	for _, g := range dsGroups {
		gc := c.Groups[g]
		fmt.Fprintf(stdout, "| %s | %d | %d | %d |\n", g, gc.Cases, gc.HeldOut, gc.Development)
	}
	fmt.Fprintf(stdout, "cases: %d; no tree is in both parts\n", c.Cases)
	if s := c.ConfirmedSample; s != nil {
		fmt.Fprintf(stdout, "confirmed sample: %d of %d question cases are about words (seed %d)\n", s.Confirmed, s.Total, s.Seed)
	}
	if td := c.ToDevelopment; td != nil {
		percent := func(n, of int) float64 {
			if of == 0 {
				return 0
			}
			return 100 * float64(n) / float64(of)
		}
		fmt.Fprintf(stdout, "to development (list sha256 %s): %d split groups listed; %d of them, with %d cases, moved from the held-out part\n",
			td.ListSHA256, td.SplitGroups, td.GroupsMoved, td.CasesMoved)
		fmt.Fprintf(stdout, "| Group | Cases moved | Held out by the split | Held out | Change |\n| --- | ---: | ---: | ---: | ---: |\n")
		for _, g := range dsGroups {
			gc, moved := c.Groups[g], td.CasesMovedByGroup[g]
			split, now := percent(gc.HeldOut+moved, gc.Cases), percent(gc.HeldOut, gc.Cases)
			fmt.Fprintf(stdout, "| %s | %d | %.1f%% | %.1f%% | %+.1f points |\n", g, moved, split, now, now-split)
		}
	}
	fmt.Fprintf(stdout, "wall time: %s\n", time.Since(started).Round(time.Second))
	// What the build read, beside what it wrote: dataset/DATASET's snapshot_sha256,
	// verdicts_sha256, end and owner_sha256 (the SHA-256 of the header's owner, so the output
	// names no address), then its documents_sha256, sessions_sha256 and cases_sha256.
	fmt.Fprintf(stdout, "snapshot sha256: %s\n", result.snapshotSHA)
	fmt.Fprintf(stdout, "verdicts sha256: %s\n", result.verdictsSHA)
	fmt.Fprintf(stdout, "end: %s\n", c.End)
	fmt.Fprintf(stdout, "owner_sha256: %s\n", sha256Hex([]byte(c.Owner)))
	fmt.Fprintf(stdout, "documents.jsonl.gz sha256: %s\n", result.documentSHA)
	fmt.Fprintf(stdout, "sessions.tsv sha256: %s\n", result.sessionsSHA)
	fmt.Fprintf(stdout, "cases.jsonl.gz sha256: %s\n", result.casesSHA)
	return 0
}

// dsHash is the first 8 bytes of the SHA-256 of the seed and parts, as a number.
func dsHash(seed uint64, parts ...string) uint64 {
	h := sha256.New()
	h.Write([]byte(strconv.FormatUint(seed, 10)))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// correctionShapes is the phrase shapes the owner's text corrects in the agent's: each shape he
// names inside a quoted span and the agent text holds. buildDataset takes them only from an owner
// text whose verdict is about words, so a quoted word he agrees with corrects nothing.
func correctionShapes(owner, agent string) []string {
	quoted := dsQuoted.FindAllString(owner, -1)
	var shapes []string
	for _, s := range dsPhraseShapes {
		if !s.agent.MatchString(agent) {
			continue
		}
		if slices.ContainsFunc(quoted, func(q string) bool { return s.owner.MatchString(q) }) {
			shapes = append(shapes, s.name)
		}
	}
	return shapes
}

// buildDataset builds the dataset from its inputs into out, and answers its counts and digests.
// It reads nothing else, so the same inputs write the same bytes. It refuses a snapshot that
// holds an event created after its header's end, and a confirmation read that is not the draw.
// Its four files reach out only once the whole build has passed (dsStaged), so a refused build
// leaves out as it was: a reused out's earlier files stay, and an out that did not exist is not
// made.
func buildDataset(in dsInputs, out string) (dsBuildResult, error) {
	return dsStaged(out, func(dir string) (dsBuildResult, error) { return writeDataset(in, dir) })
}

// dsStaged runs write with a new directory under out, then moves each file write left there
// into out, over a file of the same name, and answers what write answered. A write that fails
// moves nothing: out is as it was, and an out that did not exist is removed again. The directory
// is removed either way.
func dsStaged[T any](out string, write func(dir string) (T, error)) (T, error) {
	var res T
	_, err := os.Stat(out)
	made := errors.Is(err, os.ErrNotExist)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return res, err
	}
	dir, err := os.MkdirTemp(out, ".staged-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	if res, err = write(dir); err != nil {
		if os.RemoveAll(dir) == nil && made {
			os.Remove(out)
		}
		return res, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	for _, f := range files {
		if err := os.Rename(filepath.Join(dir, f.Name()), filepath.Join(out, f.Name())); err != nil {
			return res, err
		}
	}
	return res, nil
}

// writeDataset is buildDataset's work: it builds the dataset and writes its four files to out,
// the directory dsStaged gives it.
func writeDataset(in dsInputs, out string) (dsBuildResult, error) {
	var res dsBuildResult
	snapshotPath, verdictsPath, seed := in.Snapshot, in.Verdicts, in.Seed
	snapshotHash := sha256.New()
	ix, err := loadIndex(snapshotPath, snapshotHash)
	if err != nil {
		return res, err
	}
	res.snapshotSHA = hex.EncodeToString(snapshotHash.Sum(nil))
	verdicts, verdictsSHA, err := readVerdicts(verdictsPath)
	if err != nil {
		return res, err
	}
	res.verdictsSHA = verdictsSHA
	owners := ix.ownerTexts()
	verdictOf := map[*dsText]dsVerdict{}
	missing := 0
	for _, o := range owners {
		v, ok := verdicts[o.tid()]
		if !ok {
			missing++
			continue
		}
		verdictOf[o] = v
	}
	if missing > 0 {
		return res, fmt.Errorf("%d of the owner's %d texts have no verdict in %s: run classify again", missing, len(owners), verdictsPath)
	}

	// The document versions among the candidates hold their text in the snapshot's versions.
	if err := ix.loadCandidateVersions(snapshotPath); err != nil {
		return res, err
	}

	counts := dsCounts{
		End: ix.end, Owner: ix.owner, NamingIssue: ix.namingIssue, Seed: seed, HeldOutShare: dsHeldOutShare,
		Snapshot: map[string]int64{"events": int64(ix.events), "first_event": ix.firstEvent, "last_event": ix.lastEvent,
			"bodies": int64(ix.header.Bodies), "versions": int64(ix.header.Versions), "labels": int64(ix.header.Labels)},
		PersonTexts: len(ix.personTexts()), OwnerTexts: len(owners), OwnerVerdicts: map[string]int{},
		VerdictModels: map[string]int{}, PromptSHA256: map[string]int{}, Groups: map[string]dsGroupCounts{},
		Parts:      map[string]dsPartCounts{dsHeldOut: {}, dsDevelopment: {}},
		NamedLinks: map[string]int{}, Labelled: map[string]int{}, LeftOut: map[string]int{},
		SHA256: map[string]string{},
	}
	for _, o := range owners {
		v := verdictOf[o]
		counts.OwnerVerdicts[v.Verdict]++
		counts.VerdictModels[v.Model]++
		counts.PromptSHA256[v.PromptSHA256]++
	}

	cases := map[string]*dsCase{}
	inGroup := map[*dsText]map[string]bool{}
	ownerText := func(t *dsText) *dsCaseText {
		ct := ix.caseText(t)
		if v, ok := verdictOf[t]; ok {
			ct.Verdict = caseVerdict(v)
		}
		return &ct
	}
	addCase := func(group string, anchor, agent *dsText, found string) *dsCase {
		id := group + "/" + anchor.tid()
		if c, ok := cases[id]; ok {
			return c
		}
		c := &dsCase{ID: id, Group: group, Found: found, agent: agent}
		cases[id] = c
		if inGroup[agent] == nil {
			inGroup[agent] = map[string]bool{}
		}
		inGroup[agent][group] = true
		return c
	}
	// answeredBy is, by agent text, the owner's texts that answer it.
	answeredBy := map[*dsText][]*dsText{}
	foundBy := map[*dsText]string{}
	for _, o := range owners {
		if a, found := ix.answered(o); a != nil {
			answeredBy[a] = append(answeredBy[a], o)
			foundBy[o] = found
		}
	}
	firstAnswer := func(a *dsText) *dsText {
		for _, o := range answeredBy[a] {
			if f := foundBy[o]; f == dsFoundReplyChain || f == dsFoundThread {
				return o
			}
		}
		return nil
	}

	// Question cases: each text of the owner about words, with the agent text it answers.
	for _, o := range owners {
		if !wordrecords.AboutWords(verdictOf[o].Verdict) {
			continue
		}
		a, found := ix.answered(o)
		if a == nil || !a.caseKind() {
			counts.LeftOut["question_with_no_agent_text"]++
			continue
		}
		c := addCase(dsGroupQuestion, o, a, found)
		c.OwnerReply = ownerText(o)
	}
	// Answer cases: the first agent reply to each question the owner asked an agent.
	addAnswer := func(o, r *dsText, found string) *dsCase {
		c := addCase(dsGroupAnswer, r, r, found)
		if c.Question == nil && o != nil {
			c.Question = ownerText(o)
		}
		if c.OwnerReply == nil {
			if reply := firstAnswer(r); reply != nil {
				c.OwnerReply = ownerText(reply)
			}
		}
		return c
	}
	for _, o := range owners {
		if verdictOf[o].Verdict != wordrecords.AgentQuestion {
			continue
		}
		r, found := ix.firstReply(o)
		if r == nil || !r.caseKind() {
			counts.LeftOut["agent_question_with_no_reply"]++
			continue
		}
		addAnswer(o, r, found)
	}
	// Every text the owner links on the naming issue, in the group it fits.
	for _, n := range ix.named() {
		t := n.text
		switch {
		case t == nil:
			counts.NamedLinks["not_in_snapshot"]++
			continue
		case t.Person == ix.owner && wordrecords.AboutWords(verdictOf[t].Verdict):
			if c := cases[dsGroupQuestion+"/"+t.tid()]; c != nil {
				c.NamedByOwner = true
				counts.NamedLinks["question"]++
				continue
			}
			counts.NamedLinks["owner_text_with_no_agent_text"]++
		case t.Person == ix.owner:
			r, found := ix.firstReply(t)
			if r == nil || !r.caseKind() {
				counts.NamedLinks["owner_text_with_no_reply"]++
				continue
			}
			addAnswer(t, r, found).NamedByOwner = true
			counts.NamedLinks["answer"]++
		case t.caseKind():
			named := false
			for _, c := range cases {
				if c.agent == t {
					c.NamedByOwner, named = true, true
				}
			}
			if !named {
				var question *dsText
				if p := ix.at(t.Parent, t.EventID); p != nil && p.Person == ix.owner {
					question = p
				}
				addAnswer(question, t, dsFoundDirectReply).NamedByOwner = true
			}
			counts.NamedLinks["agent_text"]++
		default:
			counts.NamedLinks["not_a_case_text"]++
		}
	}
	// Phrase cases: the agent texts the owner corrected for a phrase shape, and the labelled
	// writes with a phrase that states no fact.
	addPhrase := func(a *dsText, shapes []string, source string, reply *dsText) {
		c := addCase(dsGroupPhrase, a, a, "")
		if c.Phrase == nil {
			c.Phrase = &dsCasePhrase{Shapes: []string{}, Sources: []string{}}
		}
		for _, s := range shapes {
			if !slices.Contains(c.Phrase.Shapes, s) {
				c.Phrase.Shapes = append(c.Phrase.Shapes, s)
			}
		}
		slices.Sort(c.Phrase.Shapes)
		if !slices.Contains(c.Phrase.Sources, source) {
			c.Phrase.Sources = append(c.Phrase.Sources, source)
			slices.Sort(c.Phrase.Sources)
		}
		if c.OwnerReply == nil && reply != nil {
			c.OwnerReply, c.Found = ownerText(reply), foundBy[reply]
		}
	}
	for _, o := range owners {
		if !wordrecords.AboutWords(verdictOf[o].Verdict) {
			continue
		}
		a, _ := ix.answered(o)
		if a == nil || !a.caseKind() {
			continue
		}
		if shapes := correctionShapes(o.Text, a.Text); len(shapes) > 0 {
			addPhrase(a, shapes, "correction", o)
		}
	}
	for _, l := range ix.labels {
		kind, ok := dsCaseFamilies[l.Family]
		if !ok {
			counts.Labelled["family_not_of_r1_kinds"]++
			continue
		}
		t := ix.labelText(l)
		switch {
		case t == nil:
			counts.Labelled["not_in_snapshot"]++
			continue
		case kind != "" && t.Kind != kind || !t.caseKind():
			counts.Labelled["not_a_case_text"]++
			continue
		}
		counts.Labelled["phrase_case"]++
		for _, set := range l.Sets {
			addPhrase(t, l.Shapes, "labelled:"+set, firstAnswer(t))
		}
	}
	// Plain cases: agent texts the owner answered or acted on with no question about them,
	// drawn by the seed, as many as the question cases.
	acted := map[*dsText]*dsAct{}
	for i := range ix.acts {
		act := &ix.acts[i]
		if t := ix.at(act.target, act.eventID); t != nil && acted[t] == nil {
			acted[t] = act
		}
	}
	var pool []*dsText
	seenPool := map[*dsText]bool{}
	consider := func(a *dsText) {
		if a == nil || seenPool[a] || !a.caseKind() || len(inGroup[a]) > 0 {
			return
		}
		seenPool[a] = true
		for _, o := range answeredBy[a] {
			if verdictOf[o].Verdict != wordrecords.None {
				return
			}
		}
		if firstAnswer(a) == nil && acted[a] == nil {
			return
		}
		pool = append(pool, a)
	}
	for _, t := range ix.all {
		consider(t)
	}
	counts.PlainPool = len(pool)
	slices.SortFunc(pool, func(a, b *dsText) int {
		return cmp.Or(cmp.Compare(dsHash(seed, "plain", a.tid()), dsHash(seed, "plain", b.tid())), strings.Compare(a.tid(), b.tid()))
	})
	questions := 0
	for _, c := range cases {
		if c.Group == dsGroupQuestion {
			questions++
		}
	}
	if len(pool) < questions {
		counts.LeftOut["plain_cases_short_of_question_cases"] = questions - len(pool)
	}
	for _, a := range pool[:min(questions, len(pool))] {
		c := addCase(dsGroupPlain, a, a, "")
		if o := firstAnswer(a); o != nil {
			c.OwnerReply, c.Found = ownerText(o), foundBy[o]
		} else {
			act := acted[a]
			c.OwnerReply = &dsCaseText{Key: "act:" + strconv.FormatInt(act.eventID, 10), EventID: act.eventID,
				EventType: act.eventType, Time: act.time, Kind: "act", ActorKind: "user", Actor: ix.owner,
				SHA256: sha256Hex(nil), Selected: act.selected}
			c.Found = dsFoundAct
		}
	}

	// Each case's fields, its record, and the split.
	list := make([]*dsCase, 0, len(cases))
	for _, c := range cases {
		list = append(list, c)
	}
	slices.SortFunc(list, func(a, b *dsCase) int { return strings.Compare(a.ID, b.ID) })
	for _, c := range list {
		ix.fill(c)
	}
	parts, err := dsSplit(list, seed)
	if err != nil {
		return res, err
	}
	// The split groups of --to-development go to the development part after the split, so every
	// other group keeps the part the split gave it.
	if counts.ToDevelopment, err = in.ToDevelopment.apply(list, parts); err != nil {
		return res, err
	}
	for _, c := range list {
		c.Part = parts[c.SplitGroup]
	}
	treeParts := map[string]string{}
	counts.NoTreeInBothParts = true
	for _, c := range list {
		if p, ok := treeParts[c.Tree]; ok && p != c.Part {
			counts.NoTreeInBothParts = false
		}
		treeParts[c.Tree] = c.Part
	}
	if !counts.NoTreeInBothParts {
		return res, errors.New("a tree is in both parts")
	}

	// The files.
	if res.casesSHA, err = dsWriteGzipJSONL(filepath.Join(out, "cases.jsonl.gz"), list); err != nil {
		return res, err
	}
	docs := dsRecordDocuments(list)
	if res.documentSHA, err = ix.writeDocuments(snapshotPath, filepath.Join(out, "documents.jsonl.gz"), docs); err != nil {
		return res, err
	}
	withSession, sessions := 0, map[string]bool{}
	for _, c := range list {
		if c.WriterSession != "" {
			withSession++
			sessions[c.WriterSession] = true
		}
	}
	if res.sessionsSHA, counts.SessionsListed, counts.SessionsUnlisted, err = writeSessionList(filepath.Join(out, "sessions.tsv"), list); err != nil {
		return res, err
	}

	splitGroups := map[string]map[string]bool{}
	trees := map[string]map[string]bool{}
	for _, g := range dsGroups {
		counts.Groups[g] = dsGroupCounts{Kind: map[string]int{}, WriterSession: map[string]int{"present": 0, "absent": 0},
			Conversation: map[string]int{"inside": 0, "outside": 0}, Found: map[string]int{}}
	}
	for _, c := range list {
		gc := counts.Groups[c.Group]
		gc.Cases++
		if c.Part == dsHeldOut {
			gc.HeldOut++
		} else {
			gc.Development++
		}
		gc.Kind[c.Kind]++
		if c.WriterSession != "" {
			gc.WriterSession["present"]++
			if c.WriteTime < dsStoreLastLoad {
				gc.BeforeStoreLastLoad++
			}
		} else {
			gc.WriterSession["absent"]++
		}
		if c.Conversation {
			gc.Conversation["inside"]++
		} else {
			gc.Conversation["outside"]++
		}
		if c.Found != "" {
			gc.Found[c.Found]++
		}
		if c.NamedByOwner {
			gc.NamedByOwner++
		}
		counts.Groups[c.Group] = gc
		if splitGroups[c.Part] == nil {
			splitGroups[c.Part], trees[c.Part] = map[string]bool{}, map[string]bool{}
		}
		splitGroups[c.Part][c.SplitGroup], trees[c.Part][c.Tree] = true, true
		pc := counts.Parts[c.Part]
		pc.Cases++
		counts.Parts[c.Part] = pc
	}
	for part, pc := range counts.Parts {
		pc.SplitGroups, pc.Trees = len(splitGroups[part]), len(trees[part])
		counts.Parts[part] = pc
	}
	counts.Cases = len(list)
	counts.CasesWithSession, counts.Sessions = withSession, len(sessions)
	counts.SHA256["cases.jsonl.gz"], counts.SHA256["documents.jsonl.gz"], counts.SHA256["sessions.tsv"] = res.casesSHA, res.documentSHA, res.sessionsSHA
	if in.Confirmed != "" {
		var questions []string
		for _, c := range list {
			if c.Group == dsGroupQuestion {
				questions = append(questions, c.ID)
			}
		}
		if counts.ConfirmedSample, err = readConfirmed(in.Confirmed, in.ConfirmedSeed, questions); err != nil {
			return res, err
		}
	}
	if err := writeJSON(filepath.Join(out, "counts.json"), counts); err != nil {
		return res, err
	}
	res.counts = counts
	return res, nil
}

// fill sets what a case takes from its agent text: the text as the case holds it, its kind, its
// writer session and write time, its tree and its record.
func (ix *dsIndex) fill(c *dsCase) {
	a := c.agent
	c.Agent = ix.caseText(a)
	c.Kind, c.Conversation, c.WriterSession, c.WriteTime = a.Kind, a.Conversation, a.Session, a.Time
	c.Tree = ix.tree(a)
	c.Record = ix.record(a)
}

// caseText is a text as a case holds it.
func (ix *dsIndex) caseText(t *dsText) dsCaseText {
	actor := t.ActorID
	if t.Person != "" {
		actor = t.Person
	}
	return dsCaseText{
		Key: t.Key, EventID: t.EventID, EventType: t.EventType, Time: t.Time, Kind: t.Kind, Family: t.Family,
		SourceID: t.SourceID, ActorKind: t.ActorKind, Actor: actor, Session: t.Session, Text: t.Text, Cut: t.Cut,
		SHA256: sha256Hex([]byte(t.Text)), Selected: t.Selected,
	}
}

// tree is the tree a case's agent text belongs to: its issue's root issue; for a message on no
// issue, the session it is with; for a project document, the document.
func (ix *dsIndex) tree(t *dsText) string {
	switch {
	case t.Issue != "":
		return ix.root(t.Issue)
	case t.Conv != "":
		return "conversation:" + strings.TrimPrefix(t.Conv, "session:")
	case t.Artifact != "":
		return "document:" + t.Artifact
	}
	return "text:" + t.Key
}

// record is Dispatch's record as it stood just before t was written: its issue with the title it
// had then, each of the issue's documents at the version then in force under the name it had
// then, and the texts of its thread written before it.
func (ix *dsIndex) record(t *dsText) dsCaseRecord {
	rec := dsCaseRecord{Project: t.Project, Documents: []dsCaseDocument{}, Thread: []dsCaseText{}}
	if t.Issue != "" {
		ci := &dsCaseIssue{Key: t.Issue, Root: ix.root(t.Issue)}
		if is := ix.issues[t.Issue]; is != nil {
			ci.Title, ci.Parent = titleAt(is.titles, t.EventID), is.parent
		}
		rec.Issue = ci
	}
	for _, k := range ix.recordVersions(t) {
		d := dsCaseDocument{ArtifactID: k.artifact, Name: titleAt(ix.artifacts[k.artifact].names, t.EventID), Version: k.number}
		if sum, ok := ix.versionSHA[k]; ok {
			d.SHA256 = sum
		} else {
			d.Missing = true
		}
		rec.Documents = append(rec.Documents, d)
	}
	if t.Thread != "" {
		for _, x := range ix.threadTexts[t.Thread] {
			if x.EventID >= t.EventID {
				break
			}
			if x.usable() {
				rec.Thread = append(rec.Thread, ix.caseText(x))
			}
		}
	}
	return rec
}

// dsSplitGroups gives each case its split group: the cases an issue tree, a shared document or a
// near copy joins (group.go's assignGroups), keyed by the group's least tree.
func dsSplitGroups(cases []*dsCase) {
	shims := make([]Case, len(cases))
	for i, c := range cases {
		shims[i] = Case{RootIssue: c.Tree, Text: c.Agent.Text}
		if c.agent.isVersion() {
			shims[i].Document = c.agent.Artifact
		}
	}
	assignGroups(shims)
	key := map[string]string{}
	for i, s := range shims {
		if k, ok := key[s.Group]; !ok || cases[i].Tree < k {
			key[s.Group] = cases[i].Tree
		}
	}
	for i, c := range cases {
		c.SplitGroup = key[shims[i].Group]
	}
}

// dsSplit puts each split group (dsSplitGroups) whole in one part, and answers the part by split
// group key. The split groups go in the order of a hash of the seed and their key, each to the
// held-out part when that brings the held-out share of every case group, taken together, nearer
// dsHeldOutShare: the sum over the case groups of the squared distance of each group's held-out
// share from it falls. So each case group is stratified to about dsHeldOutShare, and a split
// group that holds cases of several case groups counts in each.
func dsSplit(cases []*dsCase, seed uint64) (map[string]string, error) {
	dsSplitGroups(cases)
	type splitGroup struct {
		key    string
		hash   uint64
		counts map[string]int
	}
	groups := map[string]*splitGroup{}
	total := map[string]int{}
	for _, c := range cases {
		g := groups[c.SplitGroup]
		if g == nil {
			g = &splitGroup{key: c.SplitGroup, hash: dsHash(seed, "split", c.SplitGroup), counts: map[string]int{}}
			groups[c.SplitGroup] = g
		}
		g.counts[c.Group]++
		total[c.Group]++
	}
	order := make([]*splitGroup, 0, len(groups))
	for _, g := range groups {
		order = append(order, g)
	}
	slices.SortFunc(order, func(a, b *splitGroup) int {
		return cmp.Or(cmp.Compare(a.hash, b.hash), strings.Compare(a.key, b.key))
	})
	held := map[string]int{}
	distance := func(add map[string]int) float64 {
		d := 0.0
		for _, s := range dsGroups {
			if total[s] > 0 {
				off := float64(held[s]+add[s])/float64(total[s]) - dsHeldOutShare
				d += off * off
			}
		}
		return d
	}
	parts := map[string]string{}
	for _, g := range order {
		if distance(g.counts) < distance(nil) {
			parts[g.key] = dsHeldOut
			for s, n := range g.counts {
				held[s] += n
			}
		} else {
			parts[g.key] = dsDevelopment
		}
	}
	if len(parts) != len(groups) {
		return nil, errors.New("the split did not place every split group")
	}
	return parts, nil
}

// dsToDevelopmentSHA256 is the SHA-256 of the one list build --to-development takes: the split
// groups whose texts a change-1 scoring run read while they were held out (INFRA-3231's pair
// check). Those groups are development data now, so they leave the held-out part, and a held-out
// run never scores them. The list holds issue keys, so this repository holds only its SHA-256: the
// keys are the Dispatch artifact infra-3231-to-development of INFRA-3231.
const dsToDevelopmentSHA256 = "3baca74d8955b0586da7b93fbe18879dc8193120aff8ead8db4f13d92f62b624"

// dsToDevelopment is the list of build --to-development: its SHA-256 and its split-group keys.
type dsToDevelopment struct {
	sha256 string
	keys   map[string]bool
}

// dsToDevelopmentCounts is what counts.json holds of the list: its SHA-256, the split groups it
// lists, those of them the split held out, which the list moved to the development part, and their
// cases, in all and by case group. It holds no key.
type dsToDevelopmentCounts struct {
	ListSHA256        string         `json:"list_sha256"`
	SplitGroups       int            `json:"split_groups"`
	GroupsMoved       int            `json:"split_groups_moved"`
	CasesMoved        int            `json:"cases_moved"`
	CasesMovedByGroup map[string]int `json:"cases_moved_by_group"`
}

// readToDevelopment reads the list of --to-development: one split-group key a line; a blank line
// and a line that opens with # are skipped. It refuses a file whose SHA-256 is not want, the one
// the repository commits (dsToDevelopmentSHA256), and says both.
func readToDevelopment(path, want string) (*dsToDevelopment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if sum := sha256Hex(data); sum != want {
		return nil, fmt.Errorf("--to-development %s has SHA-256 %s, and the repository commits SHA-256 %s for the list", path, sum, want)
	}
	l := &dsToDevelopment{sha256: want, keys: map[string]bool{}}
	for line := range strings.Lines(string(data)) {
		if key := strings.TrimSpace(line); key != "" && !strings.HasPrefix(key, "#") {
			l.keys[key] = true
		}
	}
	return l, nil
}

// apply puts each split group of the list in the development part of parts, the split's
// assignment, whatever the split said. It moves no other group: none goes to the held-out part in
// place of the groups it moves, so a case group's held-out share falls by the cases it moves. It
// refuses a list with a key that is no split group of parts, and says how many keys are no split
// group, not which. A nil list moves nothing and answers no counts.
func (l *dsToDevelopment) apply(cases []*dsCase, parts map[string]string) (*dsToDevelopmentCounts, error) {
	if l == nil {
		return nil, nil
	}
	unknown := 0
	for key := range l.keys {
		if _, ok := parts[key]; !ok {
			unknown++
		}
	}
	if unknown > 0 {
		return nil, fmt.Errorf("%d of the %d keys of --to-development are no split group of this build", unknown, len(l.keys))
	}
	tc := &dsToDevelopmentCounts{ListSHA256: l.sha256, SplitGroups: len(l.keys), CasesMovedByGroup: map[string]int{}}
	for _, g := range dsGroups {
		tc.CasesMovedByGroup[g] = 0
	}
	for _, c := range cases {
		if l.keys[c.SplitGroup] && parts[c.SplitGroup] == dsHeldOut {
			tc.CasesMoved++
			tc.CasesMovedByGroup[c.Group]++
		}
	}
	for key := range l.keys {
		if parts[key] == dsHeldOut {
			tc.GroupsMoved++
		}
		parts[key] = dsDevelopment
	}
	return tc, nil
}

// dsWriteGzipJSONL writes each value as one JSON line to a gzipped file, and answers the file's
// SHA-256.
func dsWriteGzipJSONL[T any](path string, values []T) (string, error) {
	var buf bytes.Buffer
	gz := dsGzip(&buf)
	w := bufio.NewWriterSize(gz, 1<<20)
	for _, v := range values {
		if err := dsJSONLine(w, v); err != nil {
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	return sha256Hex(buf.Bytes()), nil
}

// dsRecordDocuments is the set of document versions the cases' records name and Dispatch served.
func dsRecordDocuments(cases []*dsCase) map[dsVerKey]bool {
	docs := map[dsVerKey]bool{}
	for _, c := range cases {
		for _, d := range c.Record.Documents {
			if !d.Missing {
				docs[dsVerKey{d.ArtifactID, d.Version}] = true
			}
		}
	}
	return docs
}

// writeDocuments writes the document versions keep names to a gzipped JSONL file, in the
// snapshot's order (by artifact and version), and answers the file's SHA-256.
func (ix *dsIndex) writeDocuments(snapshotPath, path string, keep map[dsVerKey]bool) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	gz := dsGzip(buffered)
	err = eachGzipLine(snapshotPath, func(line []byte) error {
		if snapKind(line) != "version" {
			return nil
		}
		var l snapLine
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		if l.Text == nil || !keep[dsVerKey{l.ArtifactID, l.Number}] {
			return nil
		}
		return dsJSONLine(gz, dsDocumentLine{ArtifactID: l.ArtifactID, Version: l.Number, SHA256: sha256Hex([]byte(*l.Text)), Markdown: *l.Text})
	})
	if err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := buffered.Flush(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------------------------
// owner-texts

// dsOwnerTextsFile is the file owner-texts writes: the owner's own texts, each with its time.
const dsOwnerTextsFile = "owner-texts.jsonl.gz"

func runDatasetOwnerTexts(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset owner-texts", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshot := flags.String("snapshot", "", "the snapshot fetch wrote, which build reads")
	from := flags.String("from", "", "a bundle pack wrote, in place of --snapshot: the tarball, its manifest, or a directory holding one")
	out := flags.String("out", "", "the directory for owner-texts.jsonl.gz (required; keep it under $TMPDIR)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *out == "" || (*snapshot == "") == (*from == "") {
		fmt.Fprintln(stderr, "writing-check dataset owner-texts: give --snapshot or --from, and --out, and nothing else")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset owner-texts: %v\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check dataset owner-texts: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	path := *snapshot
	if *from != "" {
		dir, err := os.MkdirTemp(*out, ".bundle-")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(dir)
		in, err := openBundle(*from, dir)
		if err != nil {
			return fail(err)
		}
		path = in.Snapshot
	}
	sum, texts, err := writeOwnerTexts(path, filepath.Join(*out, dsOwnerTextsFile))
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "owner texts: %d\n%s sha256: %s\n", texts, dsOwnerTextsFile, sum)
	return 0
}

// writeOwnerTexts writes the owner's own texts of a snapshot to path, in event order, each as a
// case holds a text, with its time, and answers the file's SHA-256 and how many texts it holds.
// They are the texts build reads as his (ownerTexts): his comments, messages, asks, answers,
// notes on asks and review reasons, and no document version or issue title. It reads nothing but
// the snapshot, so the same snapshot writes the same bytes.
func writeOwnerTexts(snapshot, path string) (string, int, error) {
	ix, err := loadIndex(snapshot, nil)
	if err != nil {
		return "", 0, err
	}
	owners := ix.ownerTexts()
	texts := make([]dsCaseText, len(owners))
	for i, t := range owners {
		texts[i] = ix.caseText(t)
	}
	sum, err := dsWriteGzipJSONL(path, texts)
	return sum, len(texts), err
}

// ---------------------------------------------------------------------------------------------
// pack, and the bundle build --from reads

// The files of a bundle, in the order pack writes them. confirmed.json is there when the build
// reads a confirmation read.
const (
	dsBundleSeeds     = "seeds.json"
	dsBundleSnapshot  = "snapshot.jsonl.gz"
	dsBundleVerdicts  = "verdicts.jsonl"
	dsBundleConfirmed = "confirmed.json"
)

// dsSeeds is seeds.json: the build's seed, and the seed of the confirmation read's draw.
type dsSeeds struct {
	Seed          *uint64 `json:"seed"`
	ConfirmedSeed *uint64 `json:"confirmed_seed,omitempty"`
}

// dsManifest is a bundle split into parts: each part's name, size and SHA-256, and the whole
// bundle's.
type dsManifest struct {
	Bundle string `json:"bundle"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Parts  []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"parts"`
}

func runDatasetPack(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset pack", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshot := flags.String("snapshot", "", "the snapshot fetch wrote (required)")
	verdicts := flags.String("verdicts", "", "the verdicts classify wrote (required)")
	seed := flags.Uint64("seed", 0, "the seed build is run with (required)")
	confirmed, confirmedSeed := confirmedFlags(flags)
	out := flags.String("out", "", "the bundle, a gzipped tarball (required; keep it under $TMPDIR)")
	partSize := flags.Int64("part-size", dsPartSize, "the largest part: a larger bundle is also written as parts with a manifest")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *snapshot == "" || *verdicts == "" || *out == "" || !flagGiven(flags, "seed") || flags.NArg() > 0 || *partSize < 1 ||
		(*confirmed != "") != flagGiven(flags, "confirmed-seed") {
		fmt.Fprintln(stderr, "writing-check dataset pack: --snapshot, --verdicts, --seed and --out are required, --confirmed and --confirmed-seed go together, and nothing else")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset pack: %v\n", err)
		return 2
	}
	in := dsInputs{Snapshot: *snapshot, Verdicts: *verdicts, Seed: *seed, Confirmed: *confirmed, ConfirmedSeed: *confirmedSeed}
	sum, parts, err := packBundle(in, *out, *partSize)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check dataset pack: %v\n", err)
		return 1
	}
	for _, p := range parts {
		fmt.Fprintf(stdout, "part: %s\n", p)
	}
	fmt.Fprintf(stdout, "bundle: %s\nbundle sha256: %s\n", *out, sum)
	return 0
}

// packBundle writes the bundle: a gzipped tarball of seeds.json, the snapshot, the verdicts and,
// when given, the confirmation read, with no time, owner or mode of the files in it, so the same
// inputs write the same bytes. A bundle larger than partSize is also written as parts with a
// manifest. It first removes the manifest and the parts an earlier pack to out left, which build
// --from a directory would read before the bundle. It answers the bundle's SHA-256 and the paths
// of the parts and the manifest.
func packBundle(in dsInputs, out string, partSize int64) (string, []string, error) {
	s := dsSeeds{Seed: &in.Seed}
	files := []struct{ name, path string }{{dsBundleSnapshot, in.Snapshot}, {dsBundleVerdicts, in.Verdicts}}
	if in.Confirmed != "" {
		s.ConfirmedSeed = &in.ConfirmedSeed
		files = append(files, struct{ name, path string }{dsBundleConfirmed, in.Confirmed})
	}
	seeds, err := json.Marshal(s)
	if err != nil {
		return "", nil, err
	}
	if err := removeParts(out); err != nil {
		return "", nil, err
	}
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	h := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	gz := dsGzip(buffered)
	tw := tar.NewWriter(gz)
	put := func(name string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return err
		}
		_, err := io.Copy(tw, r)
		return err
	}
	if err := put(dsBundleSeeds, int64(len(seeds)), bytes.NewReader(seeds)); err != nil {
		return "", nil, err
	}
	for _, file := range files {
		src, err := os.Open(file.path)
		if err != nil {
			return "", nil, err
		}
		info, err := src.Stat()
		if err == nil {
			err = put(file.name, info.Size(), src)
		}
		src.Close()
		if err != nil {
			return "", nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", nil, err
	}
	if err := gz.Close(); err != nil {
		return "", nil, err
	}
	if err := buffered.Flush(); err != nil {
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	info, err := os.Stat(out)
	if err != nil || info.Size() <= partSize {
		return sum, nil, err
	}
	parts, err := splitBundle(out, sum, info.Size(), partSize)
	return sum, parts, err
}

// removeParts removes <bundle>.manifest.json and each <bundle>.part-NNN beside bundle.
func removeParts(bundle string) error {
	dir, base := filepath.Dir(bundle), filepath.Base(bundle)
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, f := range files {
		if name := f.Name(); name == base+".manifest.json" || strings.HasPrefix(name, base+".part-") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitBundle writes a bundle as parts of at most partSize bytes beside it, <bundle>.part-000 and
// on, and <bundle>.manifest.json, which build --from reads in place of the bundle.
func splitBundle(bundle, sum string, size, partSize int64) ([]string, error) {
	src, err := os.Open(bundle)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	m := dsManifest{Bundle: filepath.Base(bundle), Size: size, SHA256: sum}
	var paths []string
	for i := 0; int64(i)*partSize < size; i++ {
		name := fmt.Sprintf("%s.part-%03d", filepath.Base(bundle), i)
		path := filepath.Join(filepath.Dir(bundle), name)
		buf := make([]byte, min(partSize, size-int64(i)*partSize))
		if _, err := io.ReadFull(src, buf); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			return nil, err
		}
		m.Parts = append(m.Parts, struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		}{name, int64(len(buf)), sha256Hex(buf)})
		paths = append(paths, path)
	}
	manifest := bundle + ".manifest.json"
	if err := writeJSON(manifest, m); err != nil {
		return nil, err
	}
	return append(paths, manifest), nil
}

// openBundle unpacks a bundle into dir and answers the build's inputs in it. from is the bundle,
// its manifest (each part checked against its SHA-256, and the whole against the bundle's), or a
// directory that holds one manifest or one bundle.
func openBundle(from, dir string) (dsInputs, error) {
	var in dsInputs
	info, err := os.Stat(from)
	if err != nil {
		return in, err
	}
	if info.IsDir() {
		manifests, _ := filepath.Glob(filepath.Join(from, "*.manifest.json"))
		bundles, _ := filepath.Glob(filepath.Join(from, "*.tar.gz"))
		switch {
		case len(manifests) == 1:
			from = manifests[0]
		case len(manifests) == 0 && len(bundles) == 1:
			from = bundles[0]
		default:
			return in, fmt.Errorf("%s holds %d manifests and %d bundles: name one", from, len(manifests), len(bundles))
		}
	}
	bundle := from
	if strings.HasSuffix(from, ".manifest.json") {
		if bundle, err = joinParts(from, dir); err != nil {
			return in, err
		}
	}
	seen := map[string]bool{}
	var seeds dsSeeds
	err = dsEachTarFile(bundle, func(name string, r io.Reader) error {
		switch name {
		case dsBundleSeeds:
			if err := json.NewDecoder(r).Decode(&seeds); err != nil || seeds.Seed == nil {
				return fmt.Errorf("%s in %s holds no seed", name, bundle)
			}
		case dsBundleSnapshot, dsBundleVerdicts, dsBundleConfirmed:
			f, err := os.Create(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, r); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s holds %s, which no bundle holds", bundle, name)
		}
		seen[name] = true
		return nil
	})
	if err != nil {
		return in, err
	}
	for _, name := range []string{dsBundleSeeds, dsBundleSnapshot, dsBundleVerdicts} {
		if !seen[name] {
			return in, fmt.Errorf("%s holds no %s", bundle, name)
		}
	}
	if seen[dsBundleConfirmed] != (seeds.ConfirmedSeed != nil) {
		return in, fmt.Errorf("%s holds %s and the seed of its draw only together", bundle, dsBundleConfirmed)
	}
	in = dsInputs{Snapshot: filepath.Join(dir, dsBundleSnapshot), Verdicts: filepath.Join(dir, dsBundleVerdicts), Seed: *seeds.Seed}
	if seeds.ConfirmedSeed != nil {
		in.Confirmed, in.ConfirmedSeed = filepath.Join(dir, dsBundleConfirmed), *seeds.ConfirmedSeed
	}
	return in, nil
}

// joinParts joins the parts a manifest names, read beside it, into one bundle in dir.
func joinParts(manifestPath, dir string) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", err
	}
	var m dsManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%s: %w", manifestPath, err)
	}
	bundle := filepath.Join(dir, "bundle.tar.gz")
	f, err := os.Create(bundle)
	if err != nil {
		return "", err
	}
	defer f.Close()
	whole := sha256.New()
	for _, p := range m.Parts {
		part, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), p.Name))
		if err != nil {
			return "", err
		}
		if sum := sha256Hex(part); sum != p.SHA256 {
			return "", fmt.Errorf("part %s has SHA-256 %s, and the manifest says %s", p.Name, sum, p.SHA256)
		}
		whole.Write(part)
		if _, err := f.Write(part); err != nil {
			return "", err
		}
	}
	if sum := hex.EncodeToString(whole.Sum(nil)); sum != m.SHA256 {
		return "", fmt.Errorf("the joined parts have SHA-256 %s, and the manifest says %s", sum, m.SHA256)
	}
	return bundle, f.Close()
}

// ---------------------------------------------------------------------------------------------
// sessions: the ingest of export-sessions.sh's output

// dsExportSchemas are the schemas export-sessions.sh writes in its manifest: /1 holds the rows as
// the store gave them, /2 masks every secret in them and counts the masks (masks_by_session).
var dsExportSchemas = []string{"infra-3231-session-export/1", "infra-3231-session-export/2"}

// dsExportManifest is export-sessions.sh's manifest.json.
type dsExportManifest struct {
	Schema  string   `json:"schema"`
	Columns []string `json:"columns"`
	Parts   []struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"parts"`
	RowsBySession   map[string]int            `json:"rows_by_session"`
	MasksBySession  map[string]map[string]int `json:"masks_by_session"`
	SessionsMissing []string                  `json:"sessions_missing"`
	SessionsLeftOut int                       `json:"sessions_left_out"`
}

// Where a case stands in its writer session.
const (
	dsPlaceFound        = "found"
	dsPlaceNoRowBefore  = "no_row_before_write"
	dsPlaceMissing      = "session_missing"
	dsPlaceLeftOut      = "session_left_out"
	dsPlaceNotAStoreID  = "session_not_a_store_id"
	dsPlaceNoSession    = "no_writer_session"
	dsSessionRowsFile   = "session-rows.jsonl.gz"
	dsPlacesFile        = "places.jsonl"
	dsSessionCountsFile = "sessions-counts.json"
)

var dsPlaceStatuses = []string{dsPlaceFound, dsPlaceNoRowBefore, dsPlaceMissing, dsPlaceLeftOut, dsPlaceNotAStoreID, dsPlaceNoSession}

// dsSessionRow is one line of session-rows.jsonl.gz: one row of the export, its session and its
// index in the session (the export's order: seq, raw_file, raw_line).
type dsSessionRow struct {
	SessionID string          `json:"session_id"`
	Index     int             `json:"index"`
	Row       json.RawMessage `json:"row"`
}

// dsPlace is one line of places.jsonl: where a case's write falls in its writer session. The
// case's session is the rows of session-rows.jsonl.gz with its session id and an index below
// Rows; PlaceTS and PlaceSeq are the last of them.
type dsPlace struct {
	CaseID    string          `json:"case_id"`
	SessionID string          `json:"session_id,omitempty"`
	Status    string          `json:"status"`
	Rows      int             `json:"rows"`
	PlaceTS   string          `json:"place_ts,omitempty"`
	PlaceSeq  json.RawMessage `json:"place_seq,omitempty"`
}

// dsSessionCounts is sessions-counts.json. It holds no text.
type dsSessionCounts struct {
	ManifestSHA256   string                    `json:"manifest_sha256"`
	Schema           string                    `json:"schema"`
	Parts            int                       `json:"parts"`
	Rows             int                       `json:"rows"`
	SessionsInExport int                       `json:"sessions_in_export"`
	SessionsWithRows int                       `json:"sessions_with_rows"`
	SessionsMissing  int                       `json:"sessions_missing"`
	SessionsLeftOut  int                       `json:"sessions_left_out"`
	MasksBySession   map[string]map[string]int `json:"masks_by_session,omitempty"`
	Cases            int                       `json:"cases"`
	CasesByStatus    map[string]int            `json:"cases_by_status"`
	SHA256           map[string]string         `json:"sha256"`
}

func runDatasetSessions(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset sessions", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifest := flags.String("manifest", "", "the manifest.json export-sessions.sh wrote; its parts are read beside it (required)")
	casesPath := flags.String("cases", "", "the cases: a build directory or its cases.jsonl.gz (required)")
	out := flags.String("out", "", "the directory for session-rows.jsonl.gz, places.jsonl and sessions-counts.json (required; keep it under $TMPDIR)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *manifest == "" || *casesPath == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check dataset sessions: --manifest, --cases and --out are required, and nothing else")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset sessions: %v\n", err)
		return 2
	}
	c, err := ingestSessions(*manifest, *casesPath, *out)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check dataset sessions: %v\n", err)
		return 1
	}
	masks := 0
	for _, byPattern := range c.MasksBySession {
		for _, n := range byPattern {
			masks += n
		}
	}
	fmt.Fprintf(stdout, "schema: %s, parts: %d, rows: %d, secrets masked: %d in %d sessions\n", c.Schema, c.Parts, c.Rows, masks, len(c.MasksBySession))
	fmt.Fprintf(stdout, "sessions in the export: %d, with rows: %d, missing (0 rows): %d, left out: %d\n",
		c.SessionsInExport, c.SessionsWithRows, c.SessionsMissing, c.SessionsLeftOut)
	fmt.Fprintf(stdout, "cases: %d\n", c.Cases)
	for _, s := range dsPlaceStatuses {
		fmt.Fprintf(stdout, "  %s: %d\n", s, c.CasesByStatus[s])
	}
	fmt.Fprintf(stdout, "%s sha256: %s\n%s sha256: %s\n", dsSessionRowsFile, c.SHA256[dsSessionRowsFile], dsPlacesFile, c.SHA256[dsPlacesFile])
	return 0
}

// dsStoreTime is a time as the transcript store's rows give it: RFC 3339, or ClickHouse's
// "2006-01-02 15:04:05.999999999", which is UTC.
func dsStoreTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time", s)
}

// dsCasesFile is the cases file a --cases flag names: the path itself, or the cases.jsonl.gz in it
// when the path is a directory.
func dsCasesFile(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return filepath.Join(path, "cases.jsonl.gz")
	}
	return path
}

// ingestSessions checks every part of the session export against its manifest, joins the rows
// to the cases by writer session, and writes each case's place in its session: the last row at
// or before the case's write time. It refuses a part whose SHA-256 differs from the manifest, an
// export made from another session list than the cases', a session whose rows the manifest
// counts otherwise, and a case whose write time is no time. Its three files reach out only once
// every check has passed (dsStaged), so a refused export leaves out as it was.
func ingestSessions(manifestPath, casesPath, out string) (dsSessionCounts, error) {
	return dsStaged(out, func(dir string) (dsSessionCounts, error) { return writeSessions(manifestPath, casesPath, dir) })
}

// writeSessions is ingestSessions' work: it checks the export and writes its three files to out,
// the directory dsStaged gives it.
func writeSessions(manifestPath, casesPath, out string) (dsSessionCounts, error) {
	var counts dsSessionCounts
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return counts, err
	}
	var m dsExportManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return counts, fmt.Errorf("%s: %w", manifestPath, err)
	}
	if !slices.Contains(dsExportSchemas, m.Schema) || !slices.Contains(m.Columns, "session_id") || !slices.Contains(m.Columns, "ts") || m.RowsBySession == nil {
		return counts, fmt.Errorf("%s is not a manifest of schema %s with session_id and ts columns", manifestPath, strings.Join(dsExportSchemas, " or "))
	}
	for s := range m.MasksBySession {
		if _, ok := m.RowsBySession[s]; !ok {
			return counts, fmt.Errorf("%s counts masks of session %s, which it holds no rows of", manifestPath, s)
		}
	}
	dir := filepath.Dir(manifestPath)
	for _, p := range m.Parts {
		f, err := os.Open(filepath.Join(dir, p.Name))
		if err != nil {
			return counts, fmt.Errorf("part %s: %w", p.Name, err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return counts, fmt.Errorf("part %s: %w", p.Name, err)
		}
		if sum := hex.EncodeToString(h.Sum(nil)); sum != p.SHA256 {
			return counts, fmt.Errorf("part %s has SHA-256 %s, and the manifest says %s: refusing the export", p.Name, sum, p.SHA256)
		}
	}

	casesPath = dsCasesFile(casesPath)
	type caseRow struct {
		ID            string `json:"id"`
		WriterSession string `json:"writer_session"`
		WriteTime     string `json:"write_time"`
	}
	var cases []caseRow
	caseSessions := map[string]bool{}
	err = eachGzipLine(casesPath, func(line []byte) error {
		var c caseRow
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		cases = append(cases, c)
		if dsSessionID.MatchString(c.WriterSession) {
			caseSessions[c.WriterSession] = true
		}
		return nil
	})
	if err != nil {
		return counts, err
	}
	absent := 0
	for s := range caseSessions {
		if _, ok := m.RowsBySession[s]; !ok {
			absent++
		}
	}
	for s := range m.RowsBySession {
		if !caseSessions[s] {
			return counts, fmt.Errorf("the export holds session %s, which no case names: it was made from another session list", s)
		}
	}
	if absent != m.SessionsLeftOut {
		return counts, fmt.Errorf("%d sessions of the cases are not in the export, and its manifest leaves out %d: it was made from another session list", absent, m.SessionsLeftOut)
	}

	rowsFile, err := os.Create(filepath.Join(out, dsSessionRowsFile))
	if err != nil {
		return counts, err
	}
	defer rowsFile.Close()
	h := sha256.New()
	buffered := bufio.NewWriterSize(io.MultiWriter(rowsFile, h), 1<<20)
	gz := dsGzip(buffered)
	type rowPlace struct {
		at  time.Time
		ts  string
		seq json.RawMessage
	}
	rows := map[string][]rowPlace{}
	for _, p := range m.Parts {
		err := eachGzipLine(filepath.Join(dir, p.Name), func(line []byte) error {
			var head struct {
				SessionID string          `json:"session_id"`
				TS        string          `json:"ts"`
				Seq       json.RawMessage `json:"seq"`
			}
			if err := json.Unmarshal(line, &head); err != nil {
				return err
			}
			if _, ok := m.RowsBySession[head.SessionID]; !ok {
				return fmt.Errorf("a row of session %q, which the manifest does not count", head.SessionID)
			}
			at, err := dsStoreTime(head.TS)
			if err != nil {
				return err
			}
			index := len(rows[head.SessionID])
			rows[head.SessionID] = append(rows[head.SessionID], rowPlace{at: at, ts: head.TS, seq: slices.Clone(head.Seq)})
			counts.Rows++
			return dsJSONLine(gz, dsSessionRow{SessionID: head.SessionID, Index: index, Row: json.RawMessage(line)})
		})
		if err != nil {
			return counts, err
		}
	}
	if err := gz.Close(); err != nil {
		return counts, err
	}
	if err := buffered.Flush(); err != nil {
		return counts, err
	}
	if err := rowsFile.Close(); err != nil {
		return counts, err
	}
	for s, n := range m.RowsBySession {
		if len(rows[s]) != n {
			return counts, fmt.Errorf("the parts hold %d rows of session %s, and the manifest says %d", len(rows[s]), s, n)
		}
		if n > 0 {
			counts.SessionsWithRows++
		}
	}

	slices.SortFunc(cases, func(a, b caseRow) int { return strings.Compare(a.ID, b.ID) })
	places := make([]dsPlace, 0, len(cases))
	counts.CasesByStatus = map[string]int{}
	for _, c := range cases {
		p := dsPlace{CaseID: c.ID, SessionID: c.WriterSession}
		n, inExport := m.RowsBySession[c.WriterSession]
		switch {
		case c.WriterSession == "":
			p.Status = dsPlaceNoSession
		case !dsSessionID.MatchString(c.WriterSession):
			p.Status = dsPlaceNotAStoreID
		case !inExport:
			p.Status = dsPlaceLeftOut
		case n == 0:
			p.Status = dsPlaceMissing
		default:
			at, err := time.Parse(time.RFC3339Nano, c.WriteTime)
			if err != nil {
				return counts, fmt.Errorf("case %s: %w", c.ID, err)
			}
			last := -1
			for i, r := range rows[c.WriterSession] {
				if !r.at.After(at) {
					last = i
				}
			}
			if last < 0 {
				p.Status = dsPlaceNoRowBefore
				break
			}
			r := rows[c.WriterSession][last]
			p.Status, p.Rows, p.PlaceTS, p.PlaceSeq = dsPlaceFound, last+1, r.ts, r.seq
		}
		counts.CasesByStatus[p.Status]++
		places = append(places, p)
	}
	placesSHA, err := writeJSONL(filepath.Join(out, dsPlacesFile), places)
	if err != nil {
		return counts, err
	}
	counts.ManifestSHA256, counts.Schema, counts.MasksBySession = sha256Hex(data), m.Schema, m.MasksBySession
	counts.Parts, counts.SessionsInExport = len(m.Parts), len(m.RowsBySession)
	counts.SessionsMissing, counts.SessionsLeftOut = len(m.SessionsMissing), m.SessionsLeftOut
	counts.Cases = len(cases)
	counts.SHA256 = map[string]string{dsSessionRowsFile: hex.EncodeToString(h.Sum(nil)), dsPlacesFile: placesSHA}
	return counts, writeJSON(filepath.Join(out, dsSessionCountsFile), counts)
}
