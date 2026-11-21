package harness

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The verdicts of the selection prompt that are about words: the reader's word records
// (R8's word_questions rows with about_words).
var wordVerdicts = []string{"asks_word_meaning", "cannot_understand", "jargon_complaint"}

// verdictLine is the part of one line of the dataset's verdicts file that load reads.
type verdictLine struct {
	Text    string   `json:"text"`
	EventID int64    `json:"event_id"`
	Person  string   `json:"person"`
	Verdict string   `json:"verdict"`
	Words   []string `json:"words"`
	Model   string   `json:"model"`
	Error   string   `json:"error,omitempty"`
}

// readWordRecords is the owner's texts about words of the verdicts file, in event order: the
// last line of each text that holds no error.
func readWordRecords(path, owner string) ([]verdictLine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	byText := map[string]verdictLine{}
	var order []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		var v verdictLine
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &v) != nil || v.Error != "" {
			continue
		}
		if _, seen := byText[v.Text]; !seen {
			order = append(order, v.Text)
		}
		byText[v.Text] = v
	}
	var out []verdictLine
	for _, t := range order {
		v := byText[t]
		if strings.EqualFold(v.Person, owner) && slices.Contains(wordVerdicts, v.Verdict) && len(v.Words) > 0 {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b verdictLine) int { return cmp.Compare(a.EventID, b.EventID) })
	return out, nil
}

// recordsBefore is the word records written before the case's text (plan Decision 19): never the
// case's own question, which comes after it.
func recordsBefore(records []verdictLine, c *Case) []verdictLine {
	var out []verdictLine
	for _, r := range records {
		if r.EventID < c.Agent.EventID {
			out = append(out, r)
		}
	}
	return out
}

// Loaded is one case's line of loaded.jsonl: where its record stands in the local Dispatch.
type Loaded struct {
	CaseID string `json:"case_id"`
	// Reader is the person the local record names as the text's reader: one of the case's own,
	// so the words the reader asked about before are this case's alone.
	Reader string `json:"reader"`
	// Project and Issue are the local project and issue; Issue is empty for a case on no issue.
	Project string `json:"project"`
	Issue   string `json:"issue,omitempty"`
	// Documents are the local documents, each with the version of the dataset it holds.
	Documents []LoadedDocument `json:"documents,omitempty"`
	// Skipped are the documents of the record the local instance refused, with its reason.
	Skipped []LoadedDocument `json:"skipped,omitempty"`
	// Thread is the local ids of the thread's texts, oldest first, and ReplyTo the text the
	// case's own text replies to: on an issue, the thread's first comment; on no issue, the
	// person's latest message, the one a session's text answers.
	Thread  []string `json:"thread,omitempty"`
	ReplyTo string   `json:"reply_to,omitempty"`
	// Opened is the message by the reader that opens a conversation on no issue whose thread
	// starts with the writer session's text: a session writes there only in reply to a person's
	// message, and the record holds none before the session's text (a session opened the thread
	// in production, as an ask on a project document).
	Opened string `json:"opened,omitempty"`
	// LeftOut are the keys of the thread's texts that no route lets their author write in the
	// local record: on no issue, a text of a session other than the writer's, which has no
	// message to answer in the writer session's conversation.
	LeftOut []string `json:"left_out,omitempty"`
	// ReaderWords is the word records written for the reader.
	ReaderWords int `json:"reader_words"`
	// Instance is the records the case was loaded into: a load continues only there.
	Instance LoadedInstance `json:"instance"`
	Error    string         `json:"error,omitempty"`
	Time     string         `json:"time"`
}

// LoadedInstance names the records of the local Dispatch a line of loaded.jsonl was loaded into:
// its database and the start of `harness local` that last reset and seeded it (LocalInstance's
// Database and Seeded). A restart with --keep keeps both, so a load continues there; a start that
// made the database or reset it has another, so a load does not take its records for loaded.
type LoadedInstance struct {
	Database string `json:"database"`
	Seeded   string `json:"seeded"`
}

// records names the records inst runs on, as a line of loaded.jsonl names them.
func (inst LocalInstance) records() LoadedInstance {
	return LoadedInstance{Database: inst.Database, Seeded: inst.Seeded}
}

// LoadedDocument is one document of a record in the local Dispatch.
type LoadedDocument struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	LocalID    string `json:"local_id,omitempty"`
	// Reason is why the local instance refused a document load could not write.
	Reason string `json:"reason,omitempty"`
}

// localClient writes to the local Dispatch: as an agent with the instance's bearer, or as a
// person with the cookie the dev sign-in issues.
type localClient struct {
	inst    LocalInstance
	http    *http.Client
	mu      sync.Mutex
	cookies map[string]string
}

// localTimeout bounds one request to the local Dispatch: above the reviewer's own time bound
// (DISPATCH_REVIEWER_TIMEOUT, 120 s unset), so a review of a long document ends on the server's
// bound and not on this one.
const localTimeout = 5 * time.Minute

func newLocalClient(inst LocalInstance) *localClient {
	return &localClient{
		inst: inst, cookies: map[string]string{},
		http: &http.Client{Timeout: localTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// cookie is the session cookie of login, signed in once.
func (l *localClient) cookie(ctx context.Context, login string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.cookies[login]; ok {
		return c, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.inst.BaseURL+"/auth/_dev/signin?login="+url.QueryEscape(login), nil)
	if err != nil {
		return "", err
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "dsession" {
			l.cookies[login] = c.Name + "=" + c.Value
			return l.cookies[login], nil
		}
	}
	body, _ := io.ReadAll(resp.Body)
	return "", fmt.Errorf("dev sign-in as %s answered %d and no session cookie: %s", login, resp.StatusCode, strings.TrimSpace(string(body)))
}

// actor is a write's author: a session (an agent) or a person's login.
type actor struct {
	session, person string
}

// send makes one write as a, its actor in the body for a session, and decodes the answer into out.
func (l *localClient) send(ctx context.Context, a actor, method, path string, body map[string]any, out any) error {
	if a.session != "" {
		body["actor"] = map[string]string{"kind": "session", "id": a.session}
	}
	return l.call(ctx, a, method, path, body, nil, out)
}

// call makes one request as a with body (none for nil) and the headers in header, and decodes the
// answer into out. A session's request carries the instance's agent bearer and names the session
// only where the body does; a person's carries that person's cookie.
func (l *localClient) call(ctx context.Context, a actor, method, path string, body any, header http.Header, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.inst.BaseURL+path, reader)
	if err != nil {
		return err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.session != "" {
		req.Header.Set("Authorization", "Bearer "+l.inst.AgentToken)
	} else {
		c, err := l.cookie(ctx, a.person)
		if err != nil {
			return err
		}
		req.Header.Set("Cookie", c)
		req.Header.Set("Origin", l.inst.BaseURL)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &localStatusError{method: method, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(answer))}
	}
	if out != nil && len(answer) > 0 {
		return json.Unmarshal(answer, out)
	}
	return nil
}

type localStatusError struct {
	method, path string
	status       int
	body         string
}

func (e *localStatusError) Error() string {
	return fmt.Sprintf("%s %s answered %d: %s", e.method, e.path, e.status, e.body)
}

// loader re-creates the records of cases in the local Dispatch.
type loader struct {
	ds      *Dataset
	client  *localClient
	db      *pgx.Conn
	dbMu    sync.Mutex
	owner   string
	records []verdictLine
	// projects are the projects made so far.
	projMu   sync.Mutex
	projects map[string]bool
}

// readerOf is the login of the case's own reader in the local Dispatch.
func readerOf(c *Case) string {
	return "reader-" + sha256Hex([]byte(c.ID))[:12] + "@example.com"
}

// personOf is the local login of a person of the record: the case's reader for the owner, and a
// login of the person's own for anyone else.
func (ld *loader) personOf(c *Case, login string) string {
	if strings.EqualFold(login, ld.owner) {
		return readerOf(c)
	}
	return "person-" + sha256Hex([]byte(strings.ToLower(login)))[:12] + "@example.com"
}

// authorOf is the local author of a thread text.
func (ld *loader) authorOf(c *Case, t CaseText) actor {
	switch {
	case t.Session != "":
		return actor{session: t.Session}
	case t.ActorKind == "session":
		return actor{session: t.Actor}
	}
	return actor{person: ld.personOf(c, t.Actor)}
}

// ensureProject makes the project key once.
func (ld *loader) ensureProject(ctx context.Context, key string) error {
	ld.projMu.Lock()
	defer ld.projMu.Unlock()
	if ld.projects[key] {
		return nil
	}
	err := ld.client.send(ctx, actor{person: "alice"}, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key}, nil)
	var status *localStatusError
	if err != nil && !(errors.As(err, &status) && status.status == http.StatusConflict) {
		return err
	}
	ld.projects[key] = true
	return nil
}

// load re-creates one case's record.
func (ld *loader) load(ctx context.Context, c *Case) Loaded {
	out := Loaded{CaseID: c.ID, Reader: readerOf(c), Project: c.Record.Project}
	fail := func(err error) Loaded {
		out.Error = err.Error()
		out.Time = time.Now().UTC().Format(time.RFC3339)
		return out
	}
	// The reader signs in first: an issue's assignee is a person who has signed in.
	if _, err := ld.client.cookie(ctx, out.Reader); err != nil {
		return fail(err)
	}
	writer := actor{session: c.WriterSession}
	if writer.session == "" {
		writer.session = "harness-writer"
	}
	if is := c.Record.Issue; is != nil {
		if err := ld.ensureProject(ctx, c.Record.Project); err != nil {
			return fail(err)
		}
		// Each case has an issue of its own, and cases of one issue tree have like titles, so
		// the create skips Dispatch's check for a like issue (force) that would refuse them.
		body := map[string]any{"project": c.Record.Project, "title": is.Title, "assignee": out.Reader, "force": true}
		// The spec goes with the create, as Dispatch takes an issue's primary document; it is
		// left out of the document uploads below whether the create took it or refused it.
		var spec *CaseDocument
		specID := ""
		for i, d := range c.Record.Documents {
			if d.Name == "spec.md" && !d.Missing {
				spec = &c.Record.Documents[i]
				specID = d.ArtifactID
			}
		}
		if spec != nil {
			body["spec"] = ld.ds.docs[docKey{spec.ArtifactID, spec.Version}]
		}
		var issue map[string]any
		err := ld.client.send(ctx, writer, http.MethodPost, "/api/v1/issues", body, &issue)
		if refused := markdownRefusal(err); refused != "" && spec != nil {
			out.Skipped = append(out.Skipped, LoadedDocument{ArtifactID: spec.ArtifactID, Name: spec.Name, Version: spec.Version, Reason: refused})
			delete(body, "spec")
			spec = nil
			err = ld.client.send(ctx, writer, http.MethodPost, "/api/v1/issues", body, &issue)
		}
		if err != nil {
			return fail(err)
		}
		out.Issue, _ = issue["key"].(string)
		if spec != nil {
			local, _ := issue["primary_artifact_id"].(string)
			out.Documents = append(out.Documents, LoadedDocument{ArtifactID: spec.ArtifactID, Name: spec.Name, Version: spec.Version, LocalID: local})
		}
		for _, d := range c.Record.Documents {
			if d.Missing || d.ArtifactID == specID {
				continue
			}
			content, ok := ld.ds.docs[docKey{d.ArtifactID, d.Version}]
			if !ok {
				continue
			}
			var art map[string]any
			err := ld.client.send(ctx, writer, http.MethodPost, "/api/v1/issues/"+url.PathEscape(out.Issue)+"/artifacts", map[string]any{"name": d.Name, "content": content}, &art)
			if refused := markdownRefusal(err); refused != "" {
				out.Skipped = append(out.Skipped, LoadedDocument{ArtifactID: d.ArtifactID, Name: d.Name, Version: d.Version, Reason: refused})
				continue
			}
			if err != nil {
				return fail(fmt.Errorf("document %s: %w", d.Name, err))
			}
			id, _ := art["id"].(string)
			out.Documents = append(out.Documents, LoadedDocument{ArtifactID: d.ArtifactID, Name: d.Name, Version: d.Version, LocalID: id})
		}
		root := ""
		for _, t := range c.Record.Thread {
			body := map[string]any{"body": t.Text}
			if root != "" {
				body["reply_to"] = root
			}
			var made map[string]any
			if err := ld.client.send(ctx, ld.authorOf(c, t), http.MethodPost, "/api/v1/issues/"+url.PathEscape(out.Issue)+"/comments", body, &made); err != nil {
				return fail(fmt.Errorf("thread text %s: %w", t.Key, err))
			}
			id, _ := made["id"].(string)
			if root == "" {
				root = id
			}
			out.Thread = append(out.Thread, id)
		}
		out.ReplyTo = root
	} else if err := ld.loadConversation(ctx, c, writer.session, &out); err != nil {
		return fail(err)
	}
	n, err := ld.writeWords(ctx, c, out.Reader)
	if err != nil {
		return fail(fmt.Errorf("the reader's words: %w", err))
	}
	out.ReaderWords = n
	out.Time = time.Now().UTC().Format(time.RFC3339)
	return out
}

// openingText is the body of the message Loaded.Opened names.
const openingText = "This message is not in the record. It opens the conversation so that the session can reply."

// loadConversation writes the thread of a case on no issue as messages in the writer session's
// conversation with a person, oldest first, each by the route Dispatch gives its author. A
// person's text is a send to the session (POST /api/v1/agents/{session_id}/messages), which only
// a person may make. The session's text is its reply to the person's latest message
// (POST /api/v1/messages/{id}/reply). Its next text before the person writes again is a
// follow-up, which Dispatch threads under its first reply.
func (ld *loader) loadConversation(ctx context.Context, c *Case, session string, out *Loaded) error {
	// asked is the person's latest message, answered whether the session replied to it, and
	// previous the latest message of the conversation, which a person's text replies to.
	asked, answered, previous := "", false, ""
	toSession := func(a actor, text string) error {
		body := map[string]any{"body": text, "delivery": "aside"}
		if previous != "" {
			body["in_reply_to"] = previous
		}
		var made map[string]any
		if err := ld.client.send(ctx, a, http.MethodPost, "/api/v1/agents/"+url.PathEscape(session)+"/messages", body, &made); err != nil {
			return err
		}
		asked, _ = made["id"].(string)
		answered, previous = false, asked
		return nil
	}
	for _, t := range c.Record.Thread {
		author := ld.authorOf(c, t)
		switch {
		case author.session == "":
			if err := toSession(author, t.Text); err != nil {
				return fmt.Errorf("thread text %s: %w", t.Key, err)
			}
			out.Thread = append(out.Thread, asked)
			continue
		case author.session != session:
			out.LeftOut = append(out.LeftOut, t.Key)
			continue
		case asked == "":
			if err := toSession(actor{person: out.Reader}, openingText); err != nil {
				return fmt.Errorf("the message that opens the conversation: %w", err)
			}
			out.Opened = asked
		}
		path := "/api/v1/messages/" + url.PathEscape(asked) + "/reply"
		if answered {
			path += "?follow_up=true"
		}
		// The reply names the message's first delivery attempt, which Dispatch writes with the
		// message.
		var made map[string]any
		if err := ld.client.send(ctx, author, http.MethodPost, path, map[string]any{"body": t.Text, "attempt": 1}, &made); err != nil {
			return fmt.Errorf("thread text %s: %w", t.Key, err)
		}
		previous, _ = made["id"].(string)
		answered = true
		out.Thread = append(out.Thread, previous)
	}
	out.ReplyTo = asked
	return nil
}

// writeWords writes the reader's word records written before the case's text as the rows the
// records job would have written (R8's word_questions). No route of Dispatch writes a word record
// a person did not write in that instance, so load writes the rows into the database
// `harness local` made for the instance, and only there. Each row is the case's own: its reader
// is the case's alone.
func (ld *loader) writeWords(ctx context.Context, c *Case, reader string) (int, error) {
	records := recordsBefore(ld.records, c)
	ld.dbMu.Lock()
	defer ld.dbMu.Unlock()
	now := time.Now().UTC()
	for i, r := range records {
		// Each record is written i seconds apart, in the order it was written, so the newest
		// is the reader's last.
		at := now.Add(time.Duration(i-len(records)) * time.Second)
		_, err := ld.db.Exec(ctx, `
			insert into word_questions (source, text_key, event_id, person, project, written_at, day, kind, verdict, words, model, state)
			values ('event', $1, $2, $3, $4, $5, $6, 'harness', $7, $8, $9, 'done')
			on conflict (text_key) do nothing`,
			"harness:"+c.ID+":"+r.Text, r.EventID, reader, c.Record.Project, at, at.Format(time.DateOnly), r.Verdict, r.Words, r.Model)
		if err != nil {
			return 0, err
		}
	}
	return len(records), nil
}

func runLoad(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	localPath := fs.String("local", "", "the local.json `harness local` wrote (required)")
	casesDir := fs.String("cases", "", "the dataset build directory (required)")
	verdicts := fs.String("verdicts", "", "the dataset's verdicts file, for the reader's earlier word records (required)")
	out := fs.String("out", "", "the directory for loaded.jsonl; a load continues the cases it holds no line for (required)")
	part := fs.String("part", PartDevelopment, "the part to load: development or held_out")
	groupsFlag := fs.String("groups", strings.Join(Groups, ","), "the case groups to load, comma-separated")
	limit := fs.Int("limit", 0, "load at most this many cases, drawn by the seed (0: all)")
	seed := fs.Uint64("seed", DefaultSeed, "the seed of --limit's draw")
	concurrency := fs.Int("concurrency", 4, "the cases loaded at once")
	sampleSize := fs.Int("sample", 0, "load the stratified sample `harness score --sample` scores with the same --groups and --sample-seed (0: no sample)")
	sampleSeed := fs.Uint64("sample-seed", DefaultSeed, "the seed of --sample's draw")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "writing-check harness load: %s\n", msg)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check harness load: %v\n", err)
		return 1
	}
	if *localPath == "" || *casesDir == "" || *verdicts == "" || *out == "" || fs.NArg() > 0 || *concurrency < 1 || *limit < 0 || *sampleSize < 0 || (*limit > 0 && *sampleSize > 0) {
		return usage("--local, --cases, --verdicts and --out are required, --concurrency must be above 0, --limit and --sample not below 0 and not both set, and nothing else")
	}
	if *part != PartDevelopment && *part != PartHeldOut {
		return usage("--part is development or held_out")
	}
	if err := OutsideRepository("--out", *out); err != nil {
		return usage(err.Error())
	}
	data, err := os.ReadFile(*localPath)
	if err != nil {
		return fail(err)
	}
	var inst LocalInstance
	if err := json.Unmarshal(data, &inst); err != nil || inst.BaseURL == "" || inst.DatabaseURL == "" {
		return usage(fmt.Sprintf("%s is not a local.json of `harness local`", *localPath))
	}
	owner, err := datasetOwner(*casesDir)
	if err != nil {
		return fail(err)
	}
	records, err := readWordRecords(*verdicts, owner)
	if err != nil {
		return fail(err)
	}
	ds, err := LoadDataset(*casesDir, *part, wholeDocuments)
	if err != nil {
		return fail(err)
	}
	cases := selectCases(ds.Cases, strings.Split(*groupsFlag, ","), *limit, *seed)
	if *sampleSize > 0 {
		cases, _ = drawSample(cases, *sampleSize, *sampleSeed)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	// The lines of loaded.jsonl are records of one database as one start seeded it: a load into
	// other records is refused before it reaches the database.
	path := filepath.Join(*out, "loaded.jsonl")
	lines, done, err := openLoadedLog(path, inst.records())
	if err != nil {
		return fail(err)
	}
	defer lines.f.Close()
	db, err := pgx.Connect(ctx, inst.DatabaseURL)
	if err != nil {
		return fail(fmt.Errorf("the local instance's database: %w", err))
	}
	defer db.Close(context.Background())
	var todo []*Case
	for _, c := range cases {
		if !done[c.ID] {
			todo = append(todo, c)
		}
	}
	fmt.Fprintf(stderr, "writing-check harness load: %d cases, %d loaded before, %d to load into %s\n", len(cases), len(cases)-len(todo), len(todo), inst.BaseURL)
	ld := &loader{ds: ds, client: newLocalClient(inst), db: db, owner: owner, records: records, projects: map[string]bool{}}
	// A line that is not written leaves a case loaded that the file does not name, so the load
	// stops at the first.
	loadCtx, stop := context.WithCancel(ctx)
	defer stop()
	var mu sync.Mutex
	loaded, failed := 0, 0
	feed := make(chan *Case)
	var wg sync.WaitGroup
	for range *concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range feed {
				l := ld.load(loadCtx, c)
				mu.Lock()
				if err := lines.add(l); err != nil {
					stop()
				}
				loaded++
				if l.Error != "" {
					failed++
					fmt.Fprintf(stderr, "writing-check harness load: case %s: %s\n", c.ID, l.Error)
				}
				mu.Unlock()
			}
		}()
	}
feeding:
	for _, c := range todo {
		select {
		case feed <- c:
		case <-loadCtx.Done():
			break feeding
		}
	}
	close(feed)
	wg.Wait()
	fmt.Fprintf(stdout, "loaded %d cases (%d failed) into %s; %s holds where each record stands\n", loaded-failed, failed, inst.BaseURL, path)
	if err := lines.close(); err != nil {
		return fail(fmt.Errorf("%w: the local instance may hold cases that loaded.jsonl does not list, so load them again into a new instance (`harness local`) with a new --out", err))
	}
	if failed > 0 || ctx.Err() != nil {
		return 1
	}
	return 0
}

// loadedLog is loaded.jsonl as a load appends to it: each line names the instance of the load.
type loadedLog struct {
	*lineFile
	instance LoadedInstance
}

// readLoaded reads the loaded.jsonl at path: the last line of each case that holds no error. It
// refuses a file that holds a line of another instance than inst, or of none: a case loaded there
// is not in inst's database, so a load does not continue there and a score finds no record of it.
func readLoaded(path string, inst LoadedInstance) (map[string]Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]Loaded{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var l Loaded
		if json.Unmarshal(line, &l) != nil || l.CaseID == "" {
			continue
		}
		if l.Instance != inst {
			return nil, fmt.Errorf("%s holds case %s loaded into database %q as seeded %q, and the local instance is database %q as seeded %q: a load continues only in the records it began in, and a score reads the cases there alone, so load them into this instance with another --out",
				path, l.CaseID, l.Instance.Database, l.Instance.Seeded, inst.Database, inst.Seeded)
		}
		if l.Error == "" {
			out[l.CaseID] = l
		}
	}
	return out, nil
}

// openLoadedLog reads the cases the loaded.jsonl at path holds as loaded with no error into inst
// (readLoaded), none where there is no file yet, and opens it to append.
func openLoadedLog(path string, inst LoadedInstance) (*loadedLog, map[string]bool, error) {
	loaded, err := readLoaded(path, inst)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	done := map[string]bool{}
	for id := range loaded {
		done[id] = true
	}
	lines, err := appendLines(path)
	if err != nil {
		return nil, nil, err
	}
	return &loadedLog{lineFile: lines, instance: inst}, done, nil
}

// add appends l's line, naming the log's instance, and answers the first write that failed.
func (lg *loadedLog) add(l Loaded) error {
	l.Instance = lg.instance
	return lg.lineFile.add(l)
}

// lineFile is a JSONL file a run appends to one line at a time. It answers the first line it did
// not write, so a run counts no result that the file does not hold. One goroutine at a time adds.
type lineFile struct {
	path string
	f    *os.File
	err  error
}

// appendLines opens the JSONL file at path to append lines, making it when it does not exist.
func appendLines(path string) (*lineFile, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &lineFile{path: path, f: f}, nil
}

// add appends v as one line, and answers the first write that failed, this one or an earlier one.
func (lf *lineFile) add(v any) error {
	line, err := json.Marshal(v)
	if err == nil {
		_, err = lf.f.Write(append(line, '\n'))
	}
	if err != nil && lf.err == nil {
		lf.err = fmt.Errorf("%s: %w", lf.path, err)
	}
	return lf.err
}

// close closes the file, and answers the first write that failed, or the close's own failure.
func (lf *lineFile) close() error {
	if err := lf.f.Close(); err != nil && lf.err == nil {
		lf.err = fmt.Errorf("%s: %w", lf.path, err)
	}
	return lf.err
}

// datasetOwner is the owner counts.json of a build names: the person whose texts the dataset reads.
func datasetOwner(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "counts.json"))
	if err != nil {
		return "", err
	}
	var counts struct {
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal(data, &counts); err != nil || counts.Owner == "" {
		return "", fmt.Errorf("%s names no owner", filepath.Join(dir, "counts.json"))
	}
	return counts.Owner, nil
}

// markdownRefusal is the answer of a write the local instance refused as INVALID_MARKDOWN, ""
// for any other outcome. A document whose blocks point at texts of production that the local
// instance does not hold is refused as it stands, so the record goes on without it, and the
// case's line names it with this answer.
func markdownRefusal(err error) string {
	var refused *localStatusError
	if errors.As(err, &refused) && refused.status == http.StatusBadRequest && strings.Contains(refused.body, "INVALID_MARKDOWN") {
		return refused.body
	}
	return ""
}
