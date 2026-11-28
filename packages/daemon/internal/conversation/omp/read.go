// Package omp reads an Oh My Pi session into the provider-neutral conversation model and
// appends a question and its answer as the pinned Oh My Pi (.omp-pin) writes them.
//
// A session is Oh My Pi's JSONL (session-loader.ts, session-entries.ts): an optional fixed-width
// title slot line, the version 3 header, then one entry per line, each naming its parent. The
// reader resolves the active leaf with the rules of resolveSessionLeafId (session-entries.ts).
// It walks from that leaf to the root and builds the context from the session records
// (session-context.ts). It projects the pinned Oh My Pi's replay rules onto the neutral model
// for Claude on Anthropic Messages and GPT on OpenAI Responses. Options lists the limits.
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
	// Model and API are empty when the session has no active branch.
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
// Read uses the effective thinking level recorded in the session or a concrete model-selector
// suffix when no thinking entry exists. It does not resolve auto. Read takes the pin's first
// model selection with the API recorded on a turn for that model. A current registry is needed
// to resolve aliases, model availability, fallback, an unrecorded API or a new effective level.
// The configured thinking selector, service tier and mode are request parameters that the
// answer loop sets. Read does not restore injected TTSR rule names. Read is for stopped sessions:
// it has no in-flight tool call IDs.
//
// Read always uses the journal's active leaf. It does not accept a temporary leaf override.
// It reads rollover requests from inline journal data, so it needs no cold-store classification.
// It does not repeat persistence-time filtering on entries the journal never recorded.
//
// On Anthropic, Read shapes saved developer messages using the pin's direct Anthropic catalog
// rules for the restored model. They start as user content and can become mid-conversation
// system messages before an assistant turn. A new user prompt follows the saved history,
// so a trailing developer message normally stays a user turn. Read preserves recorded tool
// changes by logical name and sets controls on the messages that carry them. The sender
// applies wire tool names and any endpoint overrides.
// Request-level effort and tools are parameters that the answer loop sets. To generate
// historical markers, a sender would also need each assistant turn's saved requestControls.
// Read does not return those snapshots or insert their markers.
//
// With ReplayAnthropicCompaction, Read replaces eligible summary text with the
// native compaction block, inserts its file notes, then places developer messages.
// Without it, Read returns the summary text and does not insert separate file notes.
//
// Read assumes that the Anthropic request does not suppress all thinking or exclude individual
// blocks after a provider rejection. Reproducing those branches needs the current request's
// thinking policy and the provider's rejected-signature and rejected-redaction set. Padding
// a strict thinking window requires the target's thinking-presence policy and the current
// request's effective thinking mode.
//
// Read returns the saved history before the caller's prompt. The sender validates attachments
// in that prompt. For Anthropic Messages, the sender adds Continue. when it sends no new
// prompt and history ends in an assistant turn, except for on-demand compaction.
//
// The sender groups adjacent Tool messages as one Anthropic user turn and puts any following
// error-result attachment note and images at its end. It encodes empty successful tool output
// as an empty string. It needs the endpoint and authentication policy to encode tool names
// and to add the tool-result id alias where required. When an image is kept, Read refuses
// blob-store, URL, and provider file references. The neutral model carries inline data only.
// The sender needs the bytes, a file ID, or a URL for those images. For Responses, Read carries
// the recorded detail. The sender defaults unrecorded detail to auto and clamps original to
// auto if the resolved endpoint does not support original. The session does not record
// images.blockImages or whether a route strips images while its model still advertises image input.
//
// On Responses the sender supplies the effective system prompts (including any applicable
// recorded base) and the policy for placing and redacting them. Read represents saved developer
// content as user blocks. Optional developer string encoding needs the original role, original
// content shape and current encoding policy.
//
// Read keeps the saved Responses payload unchanged and cleans its native replay copy. The
// sender needs the live native-history replay state, reasoning filter, and signature policy to
// choose between native items and blocks. An incremental assistant payload adds its items.
// An assistant snapshot replaces the earlier wire input. Eligible user or developer payloads,
// including compaction summaries, use their items in place of the message's fallback blocks.
// Read carries the same-model Responses text phase and the saved message and tool item IDs
// on blocks. The sender decides which IDs to keep, omit, or invent and hashes IDs over 64
// characters. It needs the message-ID preservation option, the presence of replayable
// reasoning after filtering, the message index, and the resolved tool item kind.
//
// The sender needs the resolved target identity and compatibility policy: image and computer
// support, custom tool format, current tool declarations and wire names, and thinking mode.
// On a model alias the reader cannot infer the wire model identity from the journal ID.
//
// Read inserts an interrupted-output placeholder after a native function or custom call
// with no later output in the rebuilt replay. The sender checks pairing and order again
// after it chooses and combines native and block replay. It still needs the call kinds,
// retained IDs, active tool map, and endpoint policy for the final pairing and batch order.
// Native items and saved block calls and results preserve provider metadata, including
// computer actions, screenshots, and safety checks. Saved block calls also preserve their
// custom wire names and namespaces. The sender chooses the outgoing tool form.
//
// Read replaces malformed native function arguments with a bounded diagnostic. Its parser
// message can differ from the pinned JavaScript parser's.
//
// The reader does not apply these request-time branches:
//
//   - The sender applies the session's secrets policy (secrets.enabled) to credential redaction
//     and compaction file notes. The journal does not record this policy. Read matches the
//     pinned Oh My Pi only when the policy changes neither assistant thinking nor tool arguments.
//     The pin clears a changed block's signature before its replay transform, so Read's
//     signature rules can decide differently. The policy is off by default.
//   - foreignCredential changes nothing unless the Anthropic target verifies signatures and
//     both the recorded and current credential IDs are known and different. Read has no current ID.
//   - Responses portable reasoning needs the effective target compat, including the per-call
//     history filter, which Read does not receive. The text fallback is the same when the
//     target has no compatible reasoning slot, the lineage is not portable, the source ended
//     in error, or the block is not explicitly a full trace (summary: false).
//   - Block replay does not carry a tool thought signature. Neither supported encoder
//     sends it.
//   - The Anthropic encoder's default ID normalizer always returns a valid ID for a nonblank
//     call. The fallback for a caller-supplied normalizer that returns an invalid ID is unused.
//   - Read refuses a server tool on its original Anthropic Messages provider. It also refuses a
//     fallback boundary on Anthropic Messages, where even a request that does not send the
//     boundary can use it to select thinking. The neutral model cannot carry these blocks.
//     When the pin drops either block for the target, Read drops it too.
//
// Read checks the restored target, not the models that wrote earlier turns. It accepts only
// anthropic with anthropic-messages and a claude- model of class anthropic, or openai with
// openai-responses and a gpt- model of class openai. Other targets return conversation.ErrUnknownFormat.
type Options struct {
	// ReplayUnsignedThinking describes the endpoint used for the supported Anthropic target.
	// True allows unsigned same-model thinking on endpoints that do not verify signatures.
	// False assumes the first-party signing policy. This option has no effect on OpenAI Responses.
	ReplayUnsignedThinking bool
	// NonOfficialAnthropicEndpoint is true when the resolved Anthropic endpoint is not official.
	// The default assumes the official tool ID rules. Compatible endpoints keep opaque IDs
	// from the same model, even if they verify thinking signatures. This does not affect Responses.
	NonOfficialAnthropicEndpoint bool
	// TextOnly is true if the resolved model accepts text but not images. On
	// both APIs it replaces historical images before the encoder shapes text.
	TextOnly bool
	// ReplayAnthropicCompaction is true if the resolved model and endpoint accept
	// Anthropic compaction blocks. The default is to replay the summary as text.
	// On a non-compaction request, the sender sends the replay edit when the
	// replayed block has encryptedContent and no signature.
	ReplayAnthropicCompaction bool
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
	if !supportedTarget(provider, s.API, model) {
		return nil, conversation.Unknown("target provider %q, API %q, model %q is not supported", provider, s.API, model)
	}
	s.Messages, err = toRequest(built.messages, target{
		provider: provider, api: s.API, model: model, signing: !opts.ReplayUnsignedThinking,
		officialAnthropic: provider == "anthropic" && !opts.NonOfficialAnthropicEndpoint,
		textOnly:          opts.TextOnly,
		anthropicReplay:   opts.ReplayAnthropicCompaction,
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func supportedTarget(provider, api, model string) bool {
	return (provider == "anthropic" && api == conversation.AnthropicMessages && strings.HasPrefix(model, "claude-") &&
		replayModelClass(model) == "anthropic") ||
		(provider == "openai" && api == conversation.OpenAIResponses && strings.HasPrefix(model, "gpt-") &&
			replayModelClass(model) == "openai")
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
