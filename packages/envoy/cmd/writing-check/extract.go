package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// The surfaces a case is read from: the kind of write its text came in, named once here so the
// counts, the README's table and the cases agree.
const (
	surfaceComment               = "comment"
	surfaceCommentSuggestion     = "comment_suggestion"
	surfaceCommentEdit           = "comment_edit"
	surfaceCommentDeliveryReply  = "comment_delivery_reply"
	surfaceMessage               = "message"
	surfaceBroadcast             = "broadcast"
	surfaceSendToAgent           = "send_to_agent"
	surfaceDeliveryReply         = "delivery_reply"
	surfaceAskQuestion           = "ask_question"
	surfaceAskOption             = "ask_option"
	surfaceApprovalSummary       = "approval_summary"
	surfaceAskAnswer             = "ask_answer"
	surfaceAskDecline            = "ask_decline"
	surfaceAskResolution         = "ask_resolution"
	surfaceIssueTitle            = "issue_title"
	surfaceDocument              = "document"
	surfaceVersionSummary        = "version_summary"
	surfaceReviewReason          = "review_reason"
	surfaceResultRun             = "result_run"
	surfaceResultObserved        = "result_observed"
	surfaceResultNegativeControl = "result_negative_control"
)

// Case is one prose field of one checked agent write, as fetch writes it and group reads it.
// Group is empty until group gives it one. Copy and Tag are set only on the cases plant writes:
// Copy names the planted copy of the write, and Tag is "planted", so no other eval reads the
// copy as real work.
type Case struct {
	EventID      int64  `json:"event_id"`
	EventType    string `json:"event_type"`
	Time         string `json:"time"`
	Project      string `json:"project"`
	Issue        string `json:"issue"`
	RootIssue    string `json:"root_issue"`
	Document     string `json:"document"`
	DocumentName string `json:"document_name,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	Surface      string `json:"surface"`
	Field        string `json:"field"`
	ActorKind    string `json:"actor_kind"`
	ActorID      string `json:"actor_id"`
	ActSession   string `json:"act_session"`
	Text         string `json:"text"`
	SHA256       string `json:"sha256"`
	// Cut is a text Dispatch would not serve whole, so the case holds the part of it the event
	// carried.
	Cut   bool   `json:"cut,omitempty"`
	Group string `json:"group,omitempty"`
	Copy  string `json:"copy,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// event is one row of GET /api/v1/events/log.
type event struct {
	ID         int64           `json:"id"`
	IssueKey   *string         `json:"issue_key"`
	ArtifactID *string         `json:"artifact_id"`
	Project    string          `json:"project"`
	Type       string          `json:"type"`
	Actor      eventActor      `json:"actor"`
	CreatedAt  string          `json:"created_at"`
	Payload    json.RawMessage `json:"payload"`
}

type eventActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Act  *struct {
		Session string `json:"session"`
	} `json:"act"`
}

func (a eventActor) actSession() string {
	if a.Act == nil {
		return ""
	}
	return a.Act.Session
}

// checkResult is the prose of one recorded result.
type checkResult struct {
	Run             string `json:"run"`
	Observed        string `json:"observed"`
	NegativeControl string `json:"negative_control"`
}

// source is the reads extraction makes beyond the event log: the whole text an event cut, an
// issue's parent, a document version's Markdown and a recorded result.
type source interface {
	commentBody(ctx context.Context, id string) (string, error)
	messageBody(ctx context.Context, id string, sessions []string) (string, error)
	issueParent(ctx context.Context, key string) (string, error)
	documentVersion(ctx context.Context, artifactID string, number int) (string, error)
	result(ctx context.Context, checkIssue, resultID string) (checkResult, error)
}

// field is one prose field an event carries: its text, or the read that fetches it.
type field struct {
	surface, path string
	text          string
	load          func(context.Context) (string, error)
	sourceID      string
	document      string
	documentName  string
}

// docState is the last version of a document extraction read, as its set of blocks.
type docState struct {
	number int
	blocks map[string]bool
}

// extractor turns events into cases. It keeps what one pass over the log learns: each issue's
// root, each document's last version read, the broadcasts already taken, and, by surface, the
// fields whose text Dispatch would not serve (unread).
type extractor struct {
	src        source
	roots      map[string]string
	docs       map[string]docState
	broadcasts map[string]bool
	unread     map[string]int
}

func newExtractor(src source) *extractor {
	return &extractor{src: src, roots: map[string]string{}, docs: map[string]docState{},
		broadcasts: map[string]bool{}, unread: map[string]int{}}
}

// cases is the cases of one event. An event holding prose that the actor rule does not check
// answers no cases and the rule's exemption; an event holding no prose answers neither.
func (x *extractor) cases(ctx context.Context, ev event) ([]Case, string, error) {
	fields, err := x.fields(ev)
	if err != nil {
		return nil, "", fmt.Errorf("event %d (%s): %w", ev.ID, ev.Type, err)
	}
	if len(fields) == 0 {
		return nil, "", nil
	}
	act := ev.Actor.actSession()
	if checked, exemption := writingcheck.Checks(ev.Actor.Kind, ev.Actor.ID, act); !checked {
		return nil, exemption, nil
	}
	issue := deref(ev.IssueKey)
	root := ""
	if issue != "" {
		if root, err = x.root(ctx, issue); err != nil {
			return nil, "", fmt.Errorf("event %d: the root of %s: %w", ev.ID, issue, err)
		}
	}
	var out []Case
	for _, f := range fields {
		text, cut := f.text, false
		if f.load != nil {
			loaded, err := f.load(ctx)
			switch {
			case err == nil:
				text = loaded
			case unreadable(err):
				// A text gone since (a deleted comment, a version Dispatch no longer
				// serves) or one this credential may not read: what the event carried
				// is kept, cut, and the field is counted unread.
				x.unread[f.surface]++
				cut = text != ""
			default:
				return nil, "", fmt.Errorf("event %d (%s) %s: %w", ev.ID, ev.Type, f.path, err)
			}
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		sum := sha256.Sum256([]byte(text))
		out = append(out, Case{
			EventID: ev.ID, EventType: ev.Type, Time: ev.CreatedAt, Project: ev.Project,
			Issue: issue, RootIssue: root, Document: f.document, DocumentName: f.documentName,
			SourceID: f.sourceID, Surface: f.surface, Field: f.path,
			ActorKind: ev.Actor.Kind, ActorID: ev.Actor.ID, ActSession: act,
			Text: text, SHA256: hex.EncodeToString(sum[:]), Cut: cut,
		})
	}
	return out, "", nil
}

// root is the top of issue's ancestors: the issue itself when it has no parent.
func (x *extractor) root(ctx context.Context, issue string) (string, error) {
	var chain []string
	key := issue
	for range 64 {
		if root, ok := x.roots[key]; ok {
			key = root
			break
		}
		chain = append(chain, key)
		parent, err := x.src.issueParent(ctx, key)
		if errors.Is(err, errNotFound) {
			break
		}
		if err != nil {
			return "", err
		}
		if parent == "" {
			break
		}
		key = parent
	}
	for _, k := range chain {
		x.roots[k] = key
	}
	return key, nil
}

// noteAncestors records the root an issue event names: the last of its ancestors, parent
// first, or the issue itself.
func (x *extractor) noteAncestors(key string, ancestors []string) {
	root := key
	if len(ancestors) > 0 {
		root = ancestors[len(ancestors)-1]
	}
	x.roots[key] = root
	for _, a := range ancestors {
		x.roots[a] = root
	}
}

// fields is the prose fields ev carries, by its type. Every type not named here carries no
// prose an agent wrote: a handoff, a claim, a delivery, a state change.
func (x *extractor) fields(ev event) ([]field, error) {
	switch ev.Type {
	case "comment.created", "comment.answered", "comment.edited":
		return x.commentFields(ev)
	case "message.created", "message.answered":
		return x.messageFields(ev)
	case "ask.opened", "ask.edited":
		return askWordingFields(ev)
	case "ask.answered":
		return askAnswerFields(ev)
	case "ask.resolved":
		return askResolutionFields(ev)
	case "issue.created", "issue.updated":
		return x.issueFields(ev)
	case "artifact.created":
		return x.artifactCreatedFields(ev)
	case "artifact.version":
		return x.artifactVersionFields(ev)
	case "artifact.approved", "artifact.changes_requested":
		return reviewFields(ev)
	case "result.recorded":
		return x.resultFields(ev)
	}
	return nil, nil
}

func (x *extractor) commentFields(ev event) ([]field, error) {
	var p struct {
		ID            string  `json:"id"`
		Body          string  `json:"body"`
		BodyTruncated bool    `json:"body_truncated"`
		ArtifactID    *string `json:"artifact_id"`
		ArtifactName  string  `json:"artifact_name"`
		Suggestion    *struct {
			ReplaceWith string `json:"replace_with"`
		} `json:"suggestion"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	surface := map[string]string{
		"comment.created": surfaceComment, "comment.answered": surfaceCommentDeliveryReply, "comment.edited": surfaceCommentEdit,
	}[ev.Type]
	body := field{surface: surface, path: "body", text: p.Body, sourceID: p.ID, document: deref(p.ArtifactID), documentName: p.ArtifactName}
	if p.BodyTruncated {
		id := p.ID
		body.load = func(ctx context.Context) (string, error) { return x.src.commentBody(ctx, id) }
	}
	fields := []field{body}
	if ev.Type == "comment.created" && p.Suggestion != nil && p.Suggestion.ReplaceWith != "" {
		fields = append(fields, field{surface: surfaceCommentSuggestion, path: "suggestion.replace_with",
			text: p.Suggestion.ReplaceWith, sourceID: p.ID, document: deref(p.ArtifactID), documentName: p.ArtifactName})
	}
	return fields, nil
}

func (x *extractor) messageFields(ev event) ([]field, error) {
	var p struct {
		ID            string  `json:"id"`
		Body          string  `json:"body"`
		BodyTruncated bool    `json:"body_truncated"`
		Target        *string `json:"target"`
		BroadcastID   *string `json:"broadcast_id"`
		Author        struct {
			ID string `json:"id"`
		} `json:"author"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	f := field{path: "body", text: p.Body, sourceID: p.ID}
	switch {
	case ev.Type == "message.answered":
		f.surface = surfaceDeliveryReply
	case p.BroadcastID != nil && *p.BroadcastID != "":
		// A broadcast stores one message for each recipient, all with the one body: the
		// first copy is the write, and the rest are not taken again.
		if x.broadcasts[*p.BroadcastID] {
			return nil, nil
		}
		x.broadcasts[*p.BroadcastID] = true
		f.surface, f.sourceID = surfaceBroadcast, *p.BroadcastID
	case p.Target != nil && *p.Target != "":
		f.surface = surfaceSendToAgent
	default:
		f.surface = surfaceMessage
	}
	if p.BodyTruncated {
		id := p.ID
		sessions := []string{strings.TrimPrefix(deref(p.Target), "session:"), p.Author.ID}
		f.load = func(ctx context.Context) (string, error) { return x.src.messageBody(ctx, id, sessions) }
	}
	return []field{f}, nil
}

// askOption is one option of an ask.
type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askPayload is the part of an ask.* event's payload extraction reads.
type askPayload struct {
	ID         string      `json:"id"`
	Kind       string      `json:"kind"`
	Question   string      `json:"question"`
	Options    []askOption `json:"options"`
	ArtifactID *string     `json:"artifact_id"`
	Approval   *struct {
		ArtifactID string `json:"artifact_id"`
		Name       string `json:"name"`
	} `json:"approval"`
	Previous *struct {
		Question string      `json:"question"`
		Options  []askOption `json:"options"`
	} `json:"previous"`
	Answer *struct {
		Text *string `json:"text"`
	} `json:"answer"`
	Resolution *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	} `json:"resolution"`
}

func (p askPayload) field(surface, path, text string) field {
	f := field{surface: surface, path: path, text: text, sourceID: p.ID, document: deref(p.ArtifactID)}
	if p.Approval != nil {
		f.document, f.documentName = p.Approval.ArtifactID, p.Approval.Name
	}
	return f
}

// approvalPrefix is the question Dispatch writes ahead of an approval request's summary:
// "Approve <name> (version <n>)[, <name> (version <n>)…][ and <name> (version <n>)]?".
var approvalPrefix = regexp.MustCompile(`^Approve .*?\(version \d+\)\?(?: |$)`)

// approvalSummary is the requester's own summary in an approval ask's question, "" when it
// names none.
func approvalSummary(question string) string {
	if loc := approvalPrefix.FindStringIndex(question); loc != nil {
		return question[loc[1]:]
	}
	return ""
}

// askWordingFields is an ask's question and options as its author wrote them. An edit
// carries only the wording it changed from the version before it, and an approval request
// only the requester's summary: Dispatch writes the rest of its question and its options.
func askWordingFields(ev event) ([]field, error) {
	var p askPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	edited := ev.Type == "ask.edited" && p.Previous != nil
	if p.Kind == "approval" {
		summary := approvalSummary(p.Question)
		if edited && summary == approvalSummary(p.Previous.Question) {
			return nil, nil
		}
		return []field{p.field(surfaceApprovalSummary, "question", summary)}, nil
	}
	var fields []field
	if !edited || p.Question != p.Previous.Question {
		fields = append(fields, p.field(surfaceAskQuestion, "question", p.Question))
	}
	before := map[string]bool{}
	if edited {
		for _, o := range p.Previous.Options {
			before[o.Label], before[o.Description] = true, true
		}
	}
	for i, o := range p.Options {
		if !before[o.Label] {
			fields = append(fields, p.field(surfaceAskOption, fmt.Sprintf("options[%d].label", i), o.Label))
		}
		if o.Description != "" && !before[o.Description] {
			fields = append(fields, p.field(surfaceAskOption, fmt.Sprintf("options[%d].description", i), o.Description))
		}
	}
	return fields, nil
}

func askAnswerFields(ev event) ([]field, error) {
	var p askPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	if p.Answer == nil || p.Answer.Text == nil || *p.Answer.Text == "" {
		return nil, nil
	}
	return []field{p.field(surfaceAskAnswer, "answer.text", *p.Answer.Text)}, nil
}

// askResolutionFields is the reason of a retraction or a resolution, or the note of a decline.
// A withdrawal is Dispatch's own words, written when the ask's issue closed.
func askResolutionFields(ev event) ([]field, error) {
	var p askPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	if p.Resolution == nil || p.Resolution.Reason == "" {
		return nil, nil
	}
	switch p.Resolution.Kind {
	case "declined":
		return []field{p.field(surfaceAskDecline, "resolution.reason", p.Resolution.Reason)}, nil
	case "retracted", "resolved":
		return []field{p.field(surfaceAskResolution, "resolution.reason", p.Resolution.Reason)}, nil
	}
	return nil, nil
}

func (x *extractor) issueFields(ev event) ([]field, error) {
	var p struct {
		Key       string   `json:"key"`
		Title     string   `json:"title"`
		Changed   []string `json:"changed"`
		Ancestors []string `json:"ancestors"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	if p.Key != "" && p.Ancestors != nil {
		x.noteAncestors(p.Key, p.Ancestors)
	}
	if ev.Type == "issue.updated" && !slices.Contains(p.Changed, "title") {
		return nil, nil
	}
	return []field{{surface: surfaceIssueTitle, path: "title", text: p.Title, sourceID: p.Key}}, nil
}

func (x *extractor) artifactCreatedFields(ev event) ([]field, error) {
	var p struct {
		Artifact struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Name     string `json:"name"`
			Versions []struct {
				Number  int     `json:"number"`
				Summary *string `json:"summary"`
			} `json:"versions"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	a := p.Artifact
	if a.Kind != "doc" {
		return nil, nil
	}
	number, summary := 1, ""
	if len(a.Versions) > 0 {
		number, summary = a.Versions[0].Number, deref(a.Versions[0].Summary)
	}
	return x.documentFields(a.ID, a.Name, number, summary), nil
}

func (x *extractor) artifactVersionFields(ev event) ([]field, error) {
	var p struct {
		ArtifactID string `json:"artifact_id"`
		Name       string `json:"name"`
		Version    struct {
			Number  int     `json:"number"`
			MIME    *string `json:"mime"`
			Summary *string `json:"summary"`
		} `json:"version"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	// A document's version carries no MIME type; a file's carries its own.
	if mime := deref(p.Version.MIME); mime != "" && !strings.HasPrefix(mime, "text/markdown") {
		return nil, nil
	}
	return x.documentFields(p.ArtifactID, p.Name, p.Version.Number, deref(p.Version.Summary)), nil
}

// documentFields is a document version's new text, read as the blocks it holds that the
// version before it does not, and the version's summary when it was named with one.
func (x *extractor) documentFields(id, name string, number int, summary string) []field {
	fields := []field{{
		surface: surfaceDocument, path: "markdown", sourceID: id + "@" + strconv.Itoa(number), document: id, documentName: name,
		load: func(ctx context.Context) (string, error) { return x.newBlocks(ctx, id, number) },
	}}
	if summary != "" {
		fields = append(fields, field{surface: surfaceVersionSummary, path: "version.summary", text: summary,
			sourceID: id + "@" + strconv.Itoa(number), document: id, documentName: name})
	}
	return fields
}

// previousTries is how many versions below a version newBlocks looks for the one before it,
// for a version number Dispatch serves no text for.
const previousTries = 3

// newBlocks is the blocks of a document's version that its previous version does not hold, in
// document order and joined by blank lines. Blocks (splitBlocks) are compared as sets, so a
// block that moved is not new.
func (x *extractor) newBlocks(ctx context.Context, id string, number int) (string, error) {
	text, err := x.src.documentVersion(ctx, id, number)
	if err != nil {
		return "", err
	}
	current := splitBlocks(text)
	before := map[string]bool{}
	if last, ok := x.docs[id]; ok && last.number == number-1 {
		before = last.blocks
	} else {
		for n := number - 1; n >= 1 && n >= number-previousTries; n-- {
			previous, err := x.src.documentVersion(ctx, id, n)
			if errors.Is(err, errNotFound) {
				continue
			}
			if err != nil {
				return "", err
			}
			before = blockSet(splitBlocks(previous))
			break
		}
	}
	x.docs[id] = docState{number: number, blocks: blockSet(current)}
	var added []string
	head := ""
	for _, b := range current {
		if before[b.String()] {
			continue
		}
		// The new rows of one table are written under its head once.
		if b.head != "" && b.head == head {
			added[len(added)-1] += "\n" + b.text
			continue
		}
		head = b.head
		added = append(added, b.String())
	}
	return strings.Join(added, "\n\n"), nil
}

// block is one unit a document version is compared by: a run of lines between blank lines, one
// list item, or one table row under its table's head (its first two lines).
type block struct{ head, text string }

func (b block) String() string {
	if b.head == "" {
		return b.text
	}
	return b.head + "\n" + b.text
}

var (
	// listItem is a line that opens a list item.
	listItem = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s`)
	// tableDelimiter is the line under a table's header row.
	tableDelimiter = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$`)
)

// splitBlocks is text's blocks: its runs of lines between blank lines, trimmed, with a fenced
// code block kept whole whatever blank lines it holds. A run that is a list is split into its
// items (a nested item is its own), and a table into its rows, so that one changed item or row
// is new and the rest of a long list or table is not.
func splitBlocks(text string) []block {
	var blocks []block
	var run []string
	flush := func() {
		if len(run) > 0 {
			blocks = append(blocks, splitRun(run)...)
		}
		run = run[:0]
	}
	fence := ""
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "":
			run = append(run, line)
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
				flush()
			}
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flush()
			fence = trimmed[:3]
			run = append(run, line)
		case trimmed == "":
			flush()
		default:
			run = append(run, line)
		}
	}
	flush()
	return blocks
}

// splitRun is the blocks of one run of non-blank lines.
func splitRun(lines []string) []block {
	first := strings.TrimSpace(lines[0])
	switch {
	case strings.HasPrefix(first, "```") || strings.HasPrefix(first, "~~~"):
	case len(lines) > 2 && strings.HasPrefix(first, "|") && tableDelimiter.MatchString(lines[1]):
		head := strings.TrimSpace(lines[0]) + "\n" + strings.TrimSpace(lines[1])
		rows := make([]block, 0, len(lines)-2)
		for _, row := range lines[2:] {
			rows = append(rows, block{head: head, text: strings.TrimSpace(row)})
		}
		return rows
	case listItem.MatchString(lines[0]):
		var items []block
		for _, line := range lines {
			if listItem.MatchString(line) || len(items) == 0 {
				items = append(items, block{text: line})
				continue
			}
			items[len(items)-1].text += "\n" + line
		}
		for i := range items {
			items[i].text = strings.TrimSpace(items[i].text)
		}
		return items
	}
	return []block{{text: strings.TrimSpace(strings.Join(lines, "\n"))}}
}

func blockSet(blocks []block) map[string]bool {
	set := make(map[string]bool, len(blocks))
	for _, b := range blocks {
		set[b.String()] = true
	}
	return set
}

func reviewFields(ev event) ([]field, error) {
	var p struct {
		ArtifactID string  `json:"artifact_id"`
		Name       string  `json:"name"`
		Version    int     `json:"version"`
		Reason     *string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	if deref(p.Reason) == "" {
		return nil, nil
	}
	return []field{{surface: surfaceReviewReason, path: "reason", text: *p.Reason,
		sourceID: p.ArtifactID + "@" + strconv.Itoa(p.Version), document: p.ArtifactID, documentName: p.Name}}, nil
}

func (x *extractor) resultFields(ev event) ([]field, error) {
	var p struct {
		ResultID   string `json:"result_id"`
		CheckIssue string `json:"check_issue"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	if p.ResultID == "" {
		return nil, nil
	}
	read := func(pick func(checkResult) string) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			r, err := x.src.result(ctx, p.CheckIssue, p.ResultID)
			return pick(r), err
		}
	}
	return []field{
		{surface: surfaceResultRun, path: "run", sourceID: p.ResultID, load: read(func(r checkResult) string { return r.Run })},
		{surface: surfaceResultObserved, path: "observed", sourceID: p.ResultID, load: read(func(r checkResult) string { return r.Observed })},
		{surface: surfaceResultNegativeControl, path: "negative_control", sourceID: p.ResultID, load: read(func(r checkResult) string { return r.NegativeControl })},
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
