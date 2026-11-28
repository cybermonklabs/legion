// Package omp reads an Oh My Pi session into the provider-neutral conversation model and
// appends a question and its answer as the pinned Oh My Pi (.omp-pin) writes them.
//
// A session is Oh My Pi's JSONL (session-loader.ts, session-entries.ts): an optional fixed-width
// title slot line, the version 3 header, then one entry per line, each naming its parent. The
// reader resolves the active leaf with the rules of resolveSessionLeafId (session-entries.ts).
// It walks from that leaf to the root and builds the context from the session records
// (session-context.ts). It projects the pinned Oh My Pi's replay rules onto the neutral model
// for Anthropic Messages and OpenAI Responses. Options lists the limits of that projection.
// Like Oh My Pi it skips a line that is not JSON, including a torn last record. It refuses a
// well-formed record it does not know (conversation.ErrUnknownFormat).
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

// Session holds the reader's reconstruction of a resumed conversation, within the limits
// in Options, and the session state an append needs.
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

// Answered reports whether a legion-answer marker (AnswerEntryType) names questionID anywhere
// in the session, on any branch. It tells a caller whether the append was recorded, not whether
// the resumed agent will see it.
func (s *Session) Answered(questionID string) bool { return s.answered[questionID] }

// Options supplies endpoint policy the session does not record. Read applies the pin's Claude
// classifier and thinking-prefix rules to the resumed model ID. It does not load models.yml,
// provider discovery, or request overrides. An override makes no difference if it leaves the
// inferred model and replay policy unchanged.
//
// The reader does not apply these request-time branches of transform-messages.ts:
//
//   - Credential-pattern redaction changes nothing when the caller has not enabled it, or
//     when the replay contains no matching token. The session does not record that setting.
//   - foreignCredential changes nothing unless the Anthropic target verifies signatures and
//     both the recorded and current credential IDs are known and different. Read has no current ID.
//   - Responses portable reasoning needs the effective target compat, including the per-call
//     history filter, which Read does not receive. The text fallback is the same when the
//     target has no compatible reasoning slot, the lineage is not portable, the source ended
//     in error, or the block is not explicitly a full trace (summary: false).
//   - The neutral conversation stores an OpenAI tool's call_id, not its item ID. It preserves
//     provider-native Responses items on same-model replay. Block-only replay does not represent
//     text item signatures or tool thought signatures. This loses no wire data on Anthropic,
//     which does not encode either, or when the pin removes them on a model switch.
//   - The Anthropic encoder's default ID normalizer always returns a valid ID for a nonblank
//     call. The fallback for a caller-supplied normalizer that returns an invalid ID is unused.
//   - Read refuses a server tool on its original Anthropic Messages provider. It also refuses a
//     fallback boundary on Anthropic Messages, where even a request that does not send the
//     boundary can use it to select thinking. The neutral model cannot carry these blocks.
//     When the pin drops either block for the target, Read drops it too.
//
// Read refuses target APIs other than Anthropic Messages and OpenAI Responses.
type Options struct {
	// ReplayUnsignedThinking is true for an Anthropic-compatible reasoning endpoint that
	// accepts unsigned thinking and does not verify signatures. False assumes the first-party
	// policy: an official, signing endpoint that does not replay unsigned thinking natively.
	// It does not describe proxies that verify signatures but are not official endpoints.
	// The option makes no difference on OpenAI Responses, or on Anthropic Messages history
	// with no thinking blocks or tool IDs that need normalization.
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
