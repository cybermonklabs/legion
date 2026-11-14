package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// readers gives each case its reader (INFRA-3231, spec version 12): the person who read the
// write when it was made, or nobody. It applies the server's rule, writingCheckReader with its
// table writingCheckReaderRules (internal/dispatch/api/writing_check_reader.go), to the records
// as they stood just before the write, which it rebuilds by replaying the event log.

// The reader rules, named as the server's readerRule constants name them.
// TestReaderRuleNamesAreTheServers holds them to that file.
const (
	ruleNewIssue     = "new_issue"
	ruleIssue        = "issue"
	ruleProject      = "project"
	ruleDocument     = "document"
	ruleConversation = "conversation"
	ruleAsk          = "ask"
	ruleAskReply     = "ask_reply"
	ruleComment      = "comment"
	ruleCommentReply = "comment_reply"
	ruleCommentEdit  = "comment_edit"
	ruleMessage      = "message"
	ruleMessageReply = "message_reply"
	ruleNone         = "none"
)

func runReaders(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check readers", flag.ContinueOnError)
	flags.SetOutput(stderr)
	in := flags.String("in", "", "the cases file (required)")
	logAfter := flags.Int64("log-after", 0, "replay the log from the event after this id (0: from the first event the server keeps)")
	logThrough := flags.Int64("log-through", 0, "replay the log through this event id (required)")
	out := flags.String("out", "", "the cases file with reader and reader_rule (required; keep it under $TMPDIR)")
	statePath := flags.String("state", "", "the replayed state: read when it exists and holds the same --log-after, written after the replay (keep it under $TMPDIR)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	base, tokenFile := os.Getenv("DISPATCH_URL"), os.Getenv("DISPATCH_TOKEN_FILE")
	switch {
	case flags.NArg() > 0:
		fmt.Fprintf(stderr, "writing-check readers: unexpected argument %q\n", flags.Arg(0))
		return 2
	case *in == "" || *out == "":
		fmt.Fprintln(stderr, "writing-check readers: --in and --out are required")
		return 2
	case *logThrough <= *logAfter:
		fmt.Fprintln(stderr, "writing-check readers: --log-through must be above --log-after")
		return 2
	}
	state, err := loadReplayState(*statePath, *logAfter)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check readers: %v\n", err)
		return 1
	}
	if state.Through < *logThrough {
		if base == "" || tokenFile == "" {
			fmt.Fprintln(stderr, "writing-check readers: DISPATCH_URL and DISPATCH_TOKEN_FILE must be set to replay the log")
			return 2
		}
		c := newClient(base, tokenFile)
		err := replayLog(ctx, c, state, *logThrough, stderr)
		if *statePath != "" {
			if saveErr := saveReplayState(*statePath, state); saveErr != nil && err == nil {
				err = saveErr
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "writing-check readers: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "writing-check readers: replayed %d events through event %d (%d requests)\n", state.Events, state.Through, c.requests)
	}
	stats, err := writeReaders(*in, *out, state, *logAfter, *logThrough)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check readers: %v\n", err)
		return 1
	}
	stats.print(stdout)
	return 0
}

// change is a value a record took at an event, in force from that event on.
type change struct {
	Event int64  `json:"e"`
	Value string `json:"v"`
}

// history is the values one field of a record took, in event order.
type history []change

// before is the value in force just before event: the last change at an event below it. ok is
// false when no change came before it.
func (h history) before(event int64) (value string, ok bool) {
	i := sort.Search(len(h), func(i int) bool { return h[i].Event >= event })
	if i == 0 {
		return "", false
	}
	return h[i-1].Value, true
}

// set records value from event on. A value equal to the one in force records nothing.
func (h *history) set(event int64, value string) {
	if n := len(*h); n > 0 && (*h)[n-1].Value == value {
		return
	}
	*h = append(*h, change{Event: event, Value: value})
}

type issueRecord struct {
	Project string `json:"project"`
	Parent  string `json:"parent,omitempty"`
	// Creator is the person the creating credential acts for (actorPerson), nil when it acts
	// for nobody.
	Creator  *string `json:"creator,omitempty"`
	Assignee history `json:"assignee,omitempty"`
}

// projectRecord is a project's default assignee as settings.project_default_assignee.updated
// set it, and Replaced the default the first of those events moved away from, when it names
// one. The event is the only write of the column that the log records.
type projectRecord struct {
	Default  history `json:"default,omitempty"`
	Replaced string  `json:"replaced,omitempty"`
}

type askRecord struct {
	Author   string `json:"author,omitempty"`
	Issue    string `json:"issue,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	Approval string `json:"approval,omitempty"`
	// Block is an ask a document's `:::ask` block holds, which the document's versions write.
	Block   bool    `json:"block,omitempty"`
	AskedOf history `json:"asked_of,omitempty"`
}

type commentRecord struct {
	Author   string `json:"author,omitempty"`
	Issue    string `json:"issue,omitempty"`
	Artifact string `json:"artifact,omitempty"`
	// Answers is the comment the write named: answers, else reply_to (the thread's head, which
	// a comment written before answers was stored carries alone).
	Answers string `json:"answers,omitempty"`
	Ask     string `json:"ask,omitempty"`
}

type messageRecord struct {
	Author    string `json:"author,omitempty"`
	Issue     string `json:"issue,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Broadcast bool   `json:"broadcast,omitempty"`
	// DeliveryReply is a session's reply in the conversation it was sent: a message whose
	// target is its own author, which only POST /api/v1/messages/{id}/reply writes.
	DeliveryReply bool `json:"delivery_reply,omitempty"`
}

type artifactRecord struct {
	Issue   string `json:"issue,omitempty"`
	Project string `json:"project,omitempty"`
	Session string `json:"session,omitempty"`
}

// replayState is what replaying the log learns: every record a reader rule reads, with the
// history of each field that changes. Authors are person logins, "" for any other writer.
type replayState struct {
	LogAfter  int64                      `json:"log_after"`
	Through   int64                      `json:"through"`
	Events    int                        `json:"events"`
	Issues    map[string]*issueRecord    `json:"issues"`
	Projects  map[string]*projectRecord  `json:"projects"`
	Asks      map[string]*askRecord      `json:"asks"`
	Comments  map[string]*commentRecord  `json:"comments"`
	Messages  map[string]*messageRecord  `json:"messages"`
	Artifacts map[string]*artifactRecord `json:"artifacts"`
	// Conversations is, by session id, the person who opened the newest issue-less thread to
	// it, from each such thread a person opened.
	Conversations map[string]history `json:"conversations"`
}

func newReplayState(logAfter int64) *replayState {
	return &replayState{
		LogAfter: logAfter, Through: logAfter,
		Issues: map[string]*issueRecord{}, Projects: map[string]*projectRecord{}, Asks: map[string]*askRecord{},
		Comments: map[string]*commentRecord{}, Messages: map[string]*messageRecord{}, Artifacts: map[string]*artifactRecord{},
		Conversations: map[string]history{},
	}
}

// loadReplayState reads the state at path, or starts a new one where path is "" or names no
// file. A state replayed from another --log-after is refused.
func loadReplayState(path string, logAfter int64) (*replayState, error) {
	if path == "" {
		return newReplayState(logAfter), nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newReplayState(logAfter), nil
	}
	if err != nil {
		return nil, err
	}
	s := newReplayState(logAfter)
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.LogAfter != logAfter {
		return nil, fmt.Errorf("%s was replayed from after event %d, not %d: name another --state", path, s.LogAfter, logAfter)
	}
	return s, nil
}

// saveReplayState writes s to path whole: to a file beside it, then renamed over it.
func saveReplayState(path string, s *replayState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// replayLog pages the log from the event after s.Through through through, and applies each
// event to s in id order.
func replayLog(ctx context.Context, c *client, s *replayState, through int64, progress io.Writer) error {
	lastReport := time.Now()
	for s.Through < through {
		limit := min(int64(pageSize), through-s.Through)
		var page []event
		query := url.Values{"after": {strconv.FormatInt(s.Through, 10)}, "limit": {strconv.FormatInt(limit, 10)}}
		if err := c.get(ctx, "/api/v1/events/log", query, &page); err != nil {
			return fmt.Errorf("reading the log after %d: %w", s.Through, err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, ev := range page {
			if ev.ID > through {
				return nil
			}
			if err := s.apply(ev); err != nil {
				return fmt.Errorf("event %d (%s): %w", ev.ID, ev.Type, err)
			}
		}
		if time.Since(lastReport) > 10*time.Second {
			fmt.Fprintf(progress, "writing-check readers: through event %d, %d events replayed\n", s.Through, s.Events)
			lastReport = time.Now()
		}
	}
	return nil
}

// payloadActor is an author or a creator as a payload names it.
type payloadActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// person is the author's login when a person wrote it, "" for a session, Dispatch or any other
// writer: the server's personLogin.
func (a *payloadActor) person() string {
	if a == nil || a.Kind != "user" {
		return ""
	}
	return canonicalLogin(a.ID)
}

// canonicalLogin is model.CanonicalLogin: the form every stored login takes.
func canonicalLogin(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

// nullableLogin is a login a payload may leave null, as its canonical form, "" for null.
func nullableLogin(raw json.RawMessage) string {
	var login *string
	_ = json.Unmarshal(raw, &login)
	return canonicalLogin(deref(login))
}

// actorPerson is the server's actorPerson: the person the event's credential acts for.
func (a eventActor) person() (string, bool) {
	if a.Kind == "user" {
		return canonicalLogin(a.ID), true
	}
	if a.Owner != nil {
		return canonicalLogin(*a.Owner), true
	}
	return "", false
}

// apply learns what ev changes, and moves s.Through to it.
func (s *replayState) apply(ev event) error {
	s.Events++
	s.Through = ev.ID
	switch {
	case strings.HasPrefix(ev.Type, "issue."):
		return s.applyIssue(ev)
	case ev.Type == "settings.project_default_assignee.updated":
		return s.applyProjectDefault(ev)
	case strings.HasPrefix(ev.Type, "ask."):
		return s.applyAsk(ev)
	case ev.Type == "comment.created" || ev.Type == "comment.answered":
		return s.applyComment(ev)
	case ev.Type == "message.created" || ev.Type == "message.answered":
		return s.applyMessage(ev)
	case ev.Type == "artifact.created":
		return s.applyArtifact(ev)
	}
	return nil
}

// applyIssue reads the issue an issue.* event carries whole: its project, its parent and its
// assignee from then on, and its primary document, which the issue's create writes with no
// artifact.created of its own.
func (s *replayState) applyIssue(ev event) error {
	var p struct {
		Key               string          `json:"key"`
		Project           string          `json:"project"`
		Parent            *string         `json:"parent"`
		Assignee          json.RawMessage `json:"assignee"`
		PrimaryArtifactID *string         `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.Key == "" || p.Assignee == nil {
		return nil
	}
	if id := deref(p.PrimaryArtifactID); id != "" && s.Artifacts[id] == nil {
		s.Artifacts[id] = &artifactRecord{Issue: p.Key, Project: p.Project}
	}
	is := s.Issues[p.Key]
	if is == nil {
		is = &issueRecord{}
		if ev.Type == "issue.created" {
			if login, acts := ev.Actor.person(); acts {
				is.Creator = &login
			}
		}
		s.Issues[p.Key] = is
	}
	if p.Project != "" {
		is.Project = p.Project
	}
	is.Parent = deref(p.Parent)
	is.Assignee.set(ev.ID, nullableLogin(p.Assignee))
	return nil
}

func (s *replayState) applyProjectDefault(ev event) error {
	var p struct {
		Project      string   `json:"project"`
		Assignee     string   `json:"assignee"`
		ThreadLogins []string `json:"thread_logins"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.Project == "" {
		return nil
	}
	pr := s.Projects[p.Project]
	if pr == nil {
		pr = &projectRecord{}
		s.Projects[p.Project] = pr
	}
	assignee := canonicalLogin(p.Assignee)
	if len(pr.Default) == 0 {
		// The event names the default it moved from beside the new one (movedLogins).
		for _, login := range p.ThreadLogins {
			if canonicalLogin(login) != assignee {
				pr.Replaced = canonicalLogin(login)
			}
		}
	}
	pr.Default.set(ev.ID, assignee)
	return nil
}

func (s *replayState) applyAsk(ev event) error {
	var p struct {
		ID         string          `json:"id"`
		Author     *payloadActor   `json:"author"`
		IssueKey   *string         `json:"issue_key"`
		ArtifactID *string         `json:"artifact_id"`
		BlockID    *string         `json:"block_id"`
		AskedOf    json.RawMessage `json:"asked_of"`
		Approval   *struct {
			ArtifactID string `json:"artifact_id"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return nil
	}
	a := s.Asks[p.ID]
	if a == nil {
		a = &askRecord{Author: p.Author.person(), Issue: deref(p.IssueKey), Artifact: deref(p.ArtifactID)}
		s.Asks[p.ID] = a
	}
	if p.Approval != nil && p.Approval.ArtifactID != "" {
		a.Approval = p.Approval.ArtifactID
	}
	if deref(p.BlockID) != "" {
		a.Block = true
	}
	if p.AskedOf != nil {
		a.AskedOf.set(ev.ID, nullableLogin(p.AskedOf))
	}
	return nil
}

func (s *replayState) applyComment(ev event) error {
	var p struct {
		ID         string        `json:"id"`
		Author     *payloadActor `json:"author"`
		IssueKey   *string       `json:"issue_key"`
		ArtifactID *string       `json:"artifact_id"`
		ReplyTo    *string       `json:"reply_to"`
		Answers    *string       `json:"answers"`
		AskID      *string       `json:"ask_id"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return nil
	}
	s.Comments[p.ID] = &commentRecord{
		Author: p.Author.person(), Issue: deref(p.IssueKey), Artifact: deref(p.ArtifactID),
		Answers: cmp.Or(deref(p.Answers), deref(p.ReplyTo)), Ask: deref(p.AskID),
	}
	return nil
}

func (s *replayState) applyMessage(ev event) error {
	var p struct {
		ID          string        `json:"id"`
		Author      *payloadActor `json:"author"`
		Target      *string       `json:"target"`
		IssueKey    *string       `json:"issue_key"`
		InReplyTo   *string       `json:"in_reply_to"`
		BroadcastID *string       `json:"broadcast_id"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return nil
	}
	m := &messageRecord{
		Author: p.Author.person(), Issue: deref(p.IssueKey), InReplyTo: deref(p.InReplyTo),
		Broadcast: deref(p.BroadcastID) != "",
	}
	target := deref(p.Target)
	if p.Author != nil && p.Author.Kind == "session" && p.Author.ID != "" && target == "session:"+p.Author.ID {
		m.DeliveryReply = m.InReplyTo != ""
	}
	s.Messages[p.ID] = m
	if session, ok := strings.CutPrefix(target, "session:"); ok && m.Author != "" && m.Issue == "" && m.InReplyTo == "" {
		h := s.Conversations[session]
		h.set(ev.ID, m.Author)
		s.Conversations[session] = h
	}
	return nil
}

func (s *replayState) applyArtifact(ev event) error {
	var p struct {
		Artifact struct {
			ID        string  `json:"id"`
			IssueKey  *string `json:"issue_key"`
			Project   *string `json:"project"`
			SessionID *string `json:"session_id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	a := p.Artifact
	if a.ID == "" {
		return nil
	}
	rec := &artifactRecord{Issue: deref(a.IssueKey), Session: deref(a.SessionID)}
	if rec.Session == "" {
		// artifacts_one_owner: a session's artifact names no project.
		rec.Project = deref(a.Project)
	}
	s.Artifacts[a.ID] = rec
	return nil
}

// resolver answers the reader rules over a replayed state, at the moment just before a write,
// and counts the records a rule read that the replay does not hold.
type resolver struct {
	s       *replayState
	missing map[string]int
}

func (r *resolver) miss(what string) { r.missing[what]++ }

// projectDefault is the project's default assignee just before event: the last default a
// settings event set before it, and before the first, the default that event replaced. A
// project the log records no default for has none: the live server holds the column nullable
// until 20261111210103_projects_default_assignee_not_null fills it, and that fill writes no
// event (README.md, "readers").
func (r *resolver) projectDefault(key string, event int64) string {
	p := r.s.Projects[key]
	if p == nil {
		return ""
	}
	if login, ok := p.Default.before(event); ok {
		return login
	}
	return p.Replaced
}

// owner is ownerReaderExpression: the assignee of issue just before event, else the default of
// the issue's project, or of project where the issue is "" or not held.
func (r *resolver) owner(issue, project string, event int64) string {
	if issue != "" {
		is := r.s.Issues[issue]
		if is == nil {
			r.miss("issue")
		} else {
			if login, _ := is.Assignee.before(event); login != "" {
				return login
			}
			project = is.Project
		}
	}
	if project == "" {
		return ""
	}
	return r.projectDefault(project, event)
}

// issueReader is the server's issueReader: the issue's assignee, else its project's default.
func (r *resolver) issueReader(key string, event int64) string {
	if r.s.Issues[key] == nil {
		r.miss("issue")
		return ""
	}
	return r.owner(key, "", event)
}

// textOwner is the server's textOwnerJoins: the reader of what a comment or an ask is on. A text
// on a document names the document and no issue, and is read through the document's owner: an
// issue's document by that issue's reader, a project's by its default, a direct conversation's
// by the person in it.
func (r *resolver) textOwner(issue, artifact string, event int64) string {
	switch {
	case issue != "":
		return r.owner(issue, "", event)
	case artifact != "":
		return r.documentReader(artifact, event)
	}
	return ""
}

// documentReader is the server's documentReader: a session's document is read by the person in
// its conversation, any other by the owner of its issue or project.
func (r *resolver) documentReader(id string, event int64) string {
	a := r.s.Artifacts[id]
	if a == nil {
		r.miss("artifact")
		return ""
	}
	if a.Session != "" {
		return r.conversationReader(a.Session, event)
	}
	return r.owner(a.Issue, a.Project, event)
}

// conversationReader is the person who opened the newest issue-less thread to session.
func (r *resolver) conversationReader(session string, event int64) string {
	login, _ := r.s.Conversations[session].before(event)
	return login
}

// answered is a text a write answers: its author when a person wrote it, and the reader of
// what it is on (the server's answeredText).
type answered struct{ author, owner string }

func (a answered) reader() string { return cmp.Or(a.author, a.owner) }

// ask is the server's readAnsweredAsk: the ask's author, owner, and the person it waits on.
func (r *resolver) ask(id string, event int64) (answered, string) {
	a := r.s.Asks[id]
	if a == nil {
		r.miss("ask")
		return answered{}, ""
	}
	owner := r.textOwner(a.Issue, a.Artifact, event)
	waitsOn, _ := a.AskedOf.before(event)
	return answered{author: a.Author, owner: owner}, cmp.Or(waitsOn, owner)
}

// comment is the server's readAnsweredComment.
func (r *resolver) comment(id string, event int64) (answered, *commentRecord) {
	c := r.s.Comments[id]
	if c == nil {
		r.miss("comment")
		return answered{}, &commentRecord{}
	}
	return answered{author: c.Author, owner: r.textOwner(c.Issue, c.Artifact, event)}, c
}

// message is the server's readAnsweredMessage: a message on no issue that no person wrote is
// owned by the person who opened its thread, and by nobody where a session did.
func (r *resolver) message(id string, event int64) answered {
	m := r.s.Messages[id]
	if m == nil {
		r.miss("message")
		return answered{}
	}
	if m.Issue != "" {
		return answered{author: m.Author, owner: r.owner(m.Issue, "", event)}
	}
	if m.Author != "" {
		return answered{author: m.Author}
	}
	root := m
	for range 1 << 16 {
		if root.InReplyTo == "" {
			return answered{owner: root.Author}
		}
		next := r.s.Messages[root.InReplyTo]
		if next == nil {
			r.miss("message")
			return answered{}
		}
		root = next
	}
	return answered{}
}

// newIssueReader is the server's newIssueReader read off the create's result: the assignee the
// issue was created with, and where it holds none, the person the credential acts for, the
// parent's assignee, else the project's default.
func (r *resolver) newIssueReader(key string, event int64) string {
	is := r.s.Issues[key]
	if is == nil {
		r.miss("issue")
		return ""
	}
	if login, _ := is.Assignee.before(event + 1); login != "" {
		return login
	}
	if is.Creator != nil {
		return *is.Creator
	}
	if parent := r.s.Issues[is.Parent]; is.Parent != "" && parent != nil {
		if login, ok := parent.Assignee.before(event); ok && login != "" {
			return login
		}
	}
	return r.projectDefault(is.Project, event)
}

// readerOf is the reader of the write c is a field of, and the rule of the route that wrote it.
// The route is read off the event: its type, and the record it names.
func (r *resolver) readerOf(c Case) (string, string, error) {
	e := c.EventID
	switch c.EventType {
	case "issue.created":
		return r.newIssueReader(c.SourceID, e), ruleNewIssue, nil
	case "issue.updated":
		return r.issueReader(cmp.Or(c.SourceID, c.Issue), e), ruleIssue, nil
	case "comment.created":
		_, cm := r.comment(c.SourceID, e)
		author := ""
		switch {
		case cm.Answers != "":
			a, _ := r.comment(cm.Answers, e)
			author = a.author
		case cm.Ask != "":
			a, _ := r.ask(cm.Ask, e)
			author = a.author
		}
		if author != "" {
			return author, ruleComment, nil
		}
		if cm.Issue != "" {
			return r.issueReader(cm.Issue, e), ruleComment, nil
		}
		return r.documentReader(cm.Artifact, e), ruleComment, nil
	case "comment.answered":
		_, cm := r.comment(c.SourceID, e)
		a, _ := r.comment(cm.Answers, e)
		return a.reader(), ruleCommentReply, nil
	case "comment.edited":
		edited, cm := r.comment(c.SourceID, e)
		author := ""
		switch {
		case cm.Answers != "":
			a, _ := r.comment(cm.Answers, e)
			author = a.author
		case cm.Ask != "":
			a, _ := r.ask(cm.Ask, e)
			author = a.author
		}
		return cmp.Or(author, edited.owner), ruleCommentEdit, nil
	case "message.created", "message.answered":
		if c.Surface == surfaceBroadcast {
			return "", ruleNone, nil
		}
		m := r.s.Messages[c.SourceID]
		switch {
		case m == nil:
			r.miss("message")
			return "", ruleNone, nil
		case m.Broadcast:
			return "", ruleNone, nil
		case m.DeliveryReply:
			return r.message(m.InReplyTo, e).reader(), ruleMessageReply, nil
		case m.Issue == "":
			// POST /api/v1/agents/{session_id}/messages: the send to an agent.
			return "", ruleNone, nil
		}
		if m.InReplyTo != "" {
			if a := r.message(m.InReplyTo, e); a.author != "" {
				return a.author, ruleMessage, nil
			}
		}
		return r.issueReader(m.Issue, e), ruleMessage, nil
	case "ask.opened", "ask.edited":
		a := r.s.Asks[c.SourceID]
		switch {
		case a == nil:
			r.miss("ask")
			return "", ruleAsk, nil
		case a.Approval != "":
			// POST /api/v1/artifacts/{id}/approval-requests, {id} the document it is about.
			return r.documentReader(a.Approval, e), ruleDocument, nil
		case c.EventType == "ask.edited" && !a.Block:
			// PATCH /api/v1/asks/{id}.
			_, waitsOn := r.ask(c.SourceID, e)
			return waitsOn, ruleAsk, nil
		case a.Artifact != "":
			return r.documentReader(a.Artifact, e), ruleDocument, nil
		case a.Block:
			// A version of the issue's document that holds the `:::ask` block.
			return r.issueReader(a.Issue, e), ruleDocument, nil
		}
		return r.issueReader(a.Issue, e), ruleIssue, nil
	case "ask.answered", "ask.resolved":
		a, _ := r.ask(c.SourceID, e)
		return a.reader(), ruleAskReply, nil
	case "artifact.created":
		a := r.s.Artifacts[c.Document]
		switch {
		case a == nil:
			r.miss("artifact")
			return "", ruleDocument, nil
		case a.Session != "":
			return r.conversationReader(a.Session, e), ruleConversation, nil
		case a.Issue != "":
			return r.issueReader(a.Issue, e), ruleIssue, nil
		}
		return r.projectDefault(a.Project, e), ruleProject, nil
	case "artifact.version", "artifact.approved", "artifact.changes_requested":
		return r.documentReader(c.Document, e), ruleDocument, nil
	case "result.recorded":
		return r.issueReader(c.Issue, e), ruleIssue, nil
	}
	return "", "", fmt.Errorf("event type %q names no reader rule", c.EventType)
}

// readerWrite is one write as readers counts it.
type readerWrite struct {
	project, reader, rule string
	surfaces              []string
	cases                 int
}

// readerCount is the cases and writes of one row of a table, and those with a person reader.
type readerCount struct{ cases, casesRead, writes, writesRead int }

func (n *readerCount) add(w *readerWrite) {
	n.cases += w.cases
	n.writes++
	if w.reader != "" {
		n.casesRead += w.cases
		n.writesRead++
	}
}

// readerStats counts the cases and writes readers read, with and without a person reader: in
// all, by family (a write's cases count under its family) and by project; and the writes by
// rule and by reader, and the records a rule read that the replay does not hold.
type readerStats struct {
	total               readerCount
	byFamily, byProject map[string]*readerCount
	byRule, byReader    map[string]int
	missing             map[string]int
}

func newReaderStats(writes []*readerWrite, missing map[string]int) *readerStats {
	st := &readerStats{byFamily: map[string]*readerCount{}, byProject: map[string]*readerCount{},
		byRule: map[string]int{}, byReader: map[string]int{}, missing: missing}
	row := func(rows map[string]*readerCount, key string) *readerCount {
		if rows[key] == nil {
			rows[key] = &readerCount{}
		}
		return rows[key]
	}
	for _, w := range writes {
		family, _ := familyOf(w.surfaces)
		st.total.add(w)
		row(st.byFamily, family).add(w)
		row(st.byProject, w.project).add(w)
		st.byRule[w.rule]++
		if w.reader != "" {
			st.byReader[w.reader]++
		}
	}
	return st
}

func (st *readerStats) print(w io.Writer) {
	t := st.total
	fmt.Fprintf(w, "cases: %d, with a person reader %d (%s), with none %d\n", t.cases, t.casesRead, pct(share(t.casesRead, t.cases)), t.cases-t.casesRead)
	fmt.Fprintf(w, "writes: %d, with a person reader %d (%s), with none %d\n", t.writes, t.writesRead, pct(share(t.writesRead, t.writes)), t.writes-t.writesRead)
	table := func(title string, rows map[string]*readerCount, order []string) {
		fmt.Fprintf(w, "\n| %s | Cases | Read | Not read | Writes | Read | Not read | Writes read |\n| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n", title)
		for _, k := range order {
			if n := rows[k]; n != nil {
				fmt.Fprintf(w, "| %s | %d | %d | %d | %d | %d | %d | %s |\n", k, n.cases, n.casesRead, n.cases-n.casesRead,
					n.writes, n.writesRead, n.writes-n.writesRead, pct(share(n.writesRead, n.writes)))
			}
		}
	}
	table("Family", st.byFamily, familyOrder)
	table("Project", st.byProject, sortedKeys(st.byProject))
	fmt.Fprintln(w, "\nwrites by reader rule:")
	for _, k := range sortedKeys(st.byRule) {
		fmt.Fprintf(w, "  %s: %d\n", k, st.byRule[k])
	}
	readers := sortedKeys(st.byReader)
	slices.SortStableFunc(readers, func(a, b string) int { return cmp.Compare(st.byReader[b], st.byReader[a]) })
	fmt.Fprintf(w, "\ndistinct readers: %d\nwrites by reader (share of all writes):\n", len(readers))
	for _, k := range readers {
		fmt.Fprintf(w, "  %s: %d (%s)\n", k, st.byReader[k], pct(share(st.byReader[k], t.writes)))
	}
	if len(st.missing) > 0 {
		fmt.Fprintln(w, "\nreads of a record the replay does not hold (the server answers no reader for a missing record):")
		for _, k := range sortedKeys(st.missing) {
			fmt.Fprintf(w, "  %s: %d\n", k, st.missing[k])
		}
	}
}

// writeReaders writes every case of in to out with its reader and reader rule, and counts them.
// Every case of one write has one reader. A case outside the replayed window is refused: the
// replay holds no record it wrote.
func writeReaders(in, out string, s *replayState, logAfter, logThrough int64) (*readerStats, error) {
	r := &resolver{s: s, missing: map[string]int{}}
	through := min(logThrough, s.Through)
	index := map[writeKey]*readerWrite{}
	var writes []*readerWrite
	f, err := os.Create(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	err = eachLine(in, func(line []byte, _ int) error {
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		if c.EventID <= logAfter || c.EventID > through {
			return fmt.Errorf("event %d is outside the replayed window (%d, %d]", c.EventID, logAfter, through)
		}
		if _, ok := surfaceFamilies[c.Surface]; !ok {
			return fmt.Errorf("unknown surface %q", c.Surface)
		}
		reader, rule, err := r.readerOf(c)
		if err != nil {
			return err
		}
		k := writeKey{c.EventID, c.SourceID, c.Copy}
		wr := index[k]
		if wr == nil {
			wr = &readerWrite{project: c.Project, reader: reader, rule: rule}
			index[k] = wr
			writes = append(writes, wr)
		} else if wr.reader != reader {
			return fmt.Errorf("write %s has two readers, %q and %q", k, wr.reader, reader)
		}
		wr.cases++
		if !slices.Contains(wr.surfaces, c.Surface) {
			wr.surfaces = append(wr.surfaces, c.Surface)
		}
		c.Reader, c.ReaderRule = &reader, rule
		encoded, err := json.Marshal(c)
		if err != nil {
			return err
		}
		w.Write(encoded)
		return w.WriteByte('\n')
	})
	if err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return newReaderStats(writes, r.missing), nil
}
