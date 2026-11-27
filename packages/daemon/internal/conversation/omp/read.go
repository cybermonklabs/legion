// Package omp reads an Oh My Pi session into the provider-neutral conversation model exactly as
// the pinned Oh My Pi (.omp-pin) rebuilds it on resume, and appends a question and its answer to
// one as Oh My Pi writes entries.
//
// A session is Oh My Pi's JSONL (session-loader.ts, session-entries.ts): an optional fixed-width
// title slot line, the version 3 header, then one entry per line, each naming its parent. The
// reader resolves the active leaf with the rules of resolveSessionLeafId (session-entries.ts).
// It walks from that leaf to the root and builds the model context as buildSessionContext does
// (session-context.ts), then shapes it as the provider layer does for the resumed model
// (transform-messages.ts). Like Oh My Pi it skips a line that is not JSON, a torn last record
// among them; unlike it, it refuses a well-formed record it does not know (conversation.
// ErrUnknownFormat).
//
// The caller supplies per-request context. The reader omits the system prompt and the date and
// working-directory reminders Oh My Pi journals as date-cwd-reminder custom entries.
//
// The same bytes come from a session file under file storage and from Oh My Pi's session table
// under SQL storage (ompsessions.Read); an append goes to the file in one O_APPEND write, or to
// the table in Oh My Pi's own append statement (ompsessions.Append).
package omp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// Session is a session read: its conversation as Oh My Pi rebuilds it on resume, and what an
// append after it needs.
type Session struct {
	conversation.Conversation
	// ID, Timestamp, Cwd and ParentSession are the session header's.
	ID            string
	Timestamp     string
	Cwd           string
	ParentSession string
	// Leaf is the resolved active entry's ID. An append uses it as its parent. If no branch is
	// active, Leaf is empty and the first appended entry has no parent.
	Leaf string
	// Size is the session length in bytes when read. The appenders check this length.
	Size int64
	// Torn is whether the content does not end in a newline (a record cut off as it was written):
	// an append then starts with one, leaving the torn record a malformed line Oh My Pi skips.
	Torn bool
	ids  map[string]bool
	// answered are the questions the session already holds the answer to, by the legion-answer
	// entry each append ends with.
	answered map[string]bool
}

// Answered is whether the session already holds the answer to questionID: an append naming it
// ended with its legion-answer entry (AnswerEntryType). A caller can check this before retrying
// an append.
func (s *Session) Answered(questionID string) bool { return s.answered[questionID] }

// Options is what the reader must know that the session does not say: how the endpoint the
// conversation's model is called through treats replayed thinking, which Oh My Pi takes from its
// models.yml compat for the model, not from the session.
type Options struct {
	// ReplayUnsignedThinking is an Anthropic-compatible endpoint that verifies no thinking
	// signature and takes unsigned thinking replayed as thinking (Oh My Pi's
	// compat.replayUnsignedThinking, which it sets for a reasoning model on an endpoint that is
	// neither Anthropic's own API nor a known signing proxy). False, the zero value, is
	// Anthropic's own API: thinking left unsigned is dropped from a turn of the same model and
	// sent as text otherwise, and only that deployment's signatures are kept. OpenAI's API is
	// read as its own either way.
	ReplayUnsignedThinking bool
}

// Read reads a session's content.
func Read(content []byte, opts Options) (*Session, error) {
	p, err := parseSession(content)
	if err != nil {
		return nil, err
	}
	built, err := buildContext(p.entries)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID: p.header.ID, Timestamp: p.header.Timestamp, Cwd: p.header.Cwd, ParentSession: p.header.ParentSession,
		Size: int64(len(content)), Torn: len(content) > 0 && content[len(content)-1] != '\n',
		ids: make(map[string]bool, len(p.entries)), answered: map[string]bool{},
	}
	for _, e := range p.entries {
		s.ids[e.ID] = true
		if question, ok := answeredQuestion(e); ok {
			s.answered[question] = true
		}
	}
	s.Leaf = built.leaf
	s.ThinkingLevel = built.thinkingLevel
	s.Model = built.model
	if s.Leaf == "" {
		return s, nil
	}
	provider, model, ok := strings.Cut(built.model, "/")
	if !ok {
		return nil, conversation.Unknown("the session names no model it runs on")
	}
	s.API = built.api
	if s.API == "" {
		return nil, conversation.Unknown("no turn of model %s names its provider API", built.model)
	}
	s.Messages, err = toRequest(built.messages, target{provider: provider, api: s.API, model: model, signing: !opts.ReplayUnsignedThinking})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ReadFile reads the session file at path (file storage).
func ReadFile(path string, opts Options) (*Session, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Read(content, opts)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", path, err)
	}
	return s, nil
}

// ReadSQL reads the session Oh My Pi's session table holds at path (SQL storage); an error that Is
// os.ErrNotExist when it holds none.
func ReadSQL(ctx context.Context, conn *ompsessions.Conn, path string, opts Options) (*Session, error) {
	content, ok, err := ompsessions.Read(ctx, conn, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("session %s: %w", path, os.ErrNotExist)
	}
	s, err := Read(content, opts)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", path, err)
	}
	return s, nil
}
