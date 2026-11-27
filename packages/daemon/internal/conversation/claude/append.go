package claude

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// AnswerLineType is the type of the line an append writes after the exchange, naming the question
// it answered. Claude Code writes no line of this type and keeps none in its message map, so it
// is a marker the next reader finds (Session.Answered) and the model never sees.
const AnswerLineType = "legion-answer"

// Encode is what an append of a after the transcript writes, line for line as Claude Code writes
// a turn: a user line with the question; for each model turn one assistant line per content block,
// with one message id per turn and one user line per tool result. A retry prompt
// (conversation.Turn.AskedAgain) and an optional note after an unanswered exchange
// (conversation.Append.NoAnswer) each get a user line. The last two lines are the legion-answer
// marker and the last-prompt checkpoint naming the last conversation line. Without the
// checkpoint, Claude Code can resume from an older checkpoint and omit the appended lines.
// Each conversation line has a fresh uuid and the preceding line, the session's leaf first,
// as its parent, and the session's id, working directory, release and git branch; an assistant
// line records the session's effort (its thinking level) as Claude Code records the effort it ran
// at. When the transcript does not end in a newline the bytes start with one. now stamps every
// line.
func (s *Session) Encode(a conversation.Append, now time.Time) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.API != conversation.AnthropicMessages {
		return nil, fmt.Errorf("append: a Claude Code transcript holds %s turns, not %s", conversation.AnthropicMessages, a.API)
	}
	w, err := s.writer(now)
	if err != nil {
		return nil, err
	}
	promptID := uuid.NewString()
	if err := w.prompt(promptID, a.Question); err != nil {
		return nil, err
	}
	for i, turn := range a.Turns {
		messageID := "msg_" + randomHex(12)
		stopReason := "end_turn"
		if len(turn.Results) > 0 {
			stopReason = "tool_use"
		}
		usage := usageOut{
			InputTokens: turn.Usage.InputTokens, CacheCreationInputTokens: turn.Usage.CacheWriteTokens,
			CacheReadInputTokens: turn.Usage.CacheReadTokens, OutputTokens: turn.Usage.OutputTokens,
		}
		callLines := map[string]string{}
		for _, b := range turn.Content {
			content, err := assistantBlockOut(b)
			if err != nil {
				return nil, fmt.Errorf("append: turn %d: %w", i+1, err)
			}
			lineID, err := w.assistant(assistantMessageOut{
				ID: messageID, Type: "message", Role: "assistant", Model: a.Model, Content: []any{content},
				StopReason: stopReason, Usage: usage,
			})
			if err != nil {
				return nil, err
			}
			if b.Type == conversation.ToolCall {
				callLines[b.ToolCallID] = lineID
			}
		}
		for _, r := range turn.Results {
			if err := w.toolResult(promptID, r, callLines[r.ToolCallID]); err != nil {
				return nil, err
			}
		}
		if turn.AskedAgain != "" {
			promptID = uuid.NewString()
			if err := w.prompt(promptID, turn.AskedAgain); err != nil {
				return nil, err
			}
		}
	}
	if a.NoAnswer != "" {
		if err := w.prompt(uuid.NewString(), a.NoAnswer); err != nil {
			return nil, err
		}
	}
	if err := w.checkpoint(a.QuestionID, a.Question); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// EncodeDelivery encodes a user line with the message, a legion-answer marker naming the delivery
// for Session.Answered, and a last-prompt checkpoint naming the user line. It writes no assistant
// line. If the transcript lacks a trailing newline, the output starts with one. now supplies the
// timestamps.
func (s *Session) EncodeDelivery(d conversation.Delivery, now time.Time) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	w, err := s.writer(now)
	if err != nil {
		return nil, err
	}
	if err := w.prompt(uuid.NewString(), d.Text); err != nil {
		return nil, err
	}
	if err := w.checkpoint(d.ID, d.Text); err != nil {
		return nil, err
	}
	return w.buf.Bytes(), nil
}

// writer is how an append and a delivery write the lines after the transcript: each conversation
// line carries the session's id, working directory, release and git branch, and an assistant line
// the effort the session last ran at. A transcript that names no session is refused, since no
// line of it could name one; one that does not end in a newline opens with a newline, which
// leaves the torn line the malformed one Claude Code skips.
func (s *Session) writer(now time.Time) (*lineWriter, error) {
	if s.ID == "" {
		return nil, errors.New("append: the transcript names no session")
	}
	version := s.Version
	if version == "" {
		version = PinnedVersion
	}
	w := &lineWriter{
		parent: s.Leaf,
		common: commonOut{UserType: "external", Entrypoint: "sdk-cli", Cwd: s.Cwd, SessionID: s.ID, Version: version, GitBranch: s.GitBranch},
		at:     conversation.ISOTime(now),
		effort: s.ThinkingLevel,
	}
	if s.Torn {
		w.buf.WriteByte('\n')
	}
	return w, nil
}

// AppendFile appends a to the transcript at path, which s was read from, in one O_APPEND write.
// It refuses with an error that Is conversation.ErrMoved when the file is no longer the size s was
// read at.
func AppendFile(path string, s *Session, a conversation.Append, now time.Time) error {
	data, err := s.Encode(a, now)
	if err != nil {
		return err
	}
	return conversation.WriteAfter(path, "transcript", s.Size, data)
}

// AppendDeliveryFile appends d to the transcript at path, which s was read from, in the same one
// write and under the same refusal as AppendFile.
func AppendDeliveryFile(path string, s *Session, d conversation.Delivery, now time.Time) error {
	data, err := s.EncodeDelivery(d, now)
	if err != nil {
		return err
	}
	return conversation.WriteAfter(path, "transcript", s.Size, data)
}

// lineWriter writes lines one per line, each conversation line chained to the one before.
type lineWriter struct {
	parent string
	common commonOut
	at     string
	effort string
	buf    bytes.Buffer
}

func (w *lineWriter) parentUUID() *string {
	if w.parent == "" {
		return nil
	}
	parent := w.parent
	return &parent
}

func (w *lineWriter) userLine(promptID string) userLineOut {
	return userLineOut{ParentUUID: w.parentUUID(), PromptID: promptID, Type: "user", UUID: uuid.NewString(), Timestamp: w.at, commonOut: w.common}
}

func (w *lineWriter) prompt(promptID, text string) error {
	line := w.userLine(promptID)
	line.Message = userMessageOut{Role: "user", Content: text}
	return w.writeUser(line)
}

// toolResult writes a user line with the uuid of the assistant line holding its call.
// The toolUseResult field tells Claude Code it is not a prompt the person typed.
func (w *lineWriter) toolResult(promptID string, result conversation.Block, callLine string) error {
	line := w.userLine(promptID)
	var parts []any
	var texts []string
	for _, p := range result.Content {
		if p.Type == conversation.Image {
			parts = append(parts, imageOut{Type: "image", Source: imageSourceOut{Type: "base64", MediaType: p.MediaType, Data: p.ImageData}})
			continue
		}
		parts = append(parts, textOut{Type: "text", Text: p.Text})
		texts = append(texts, p.Text)
	}
	var content any = parts
	if len(parts) == 1 && len(texts) == 1 {
		content = texts[0]
	} else if len(parts) == 0 {
		content = ""
	}
	line.Message = userMessageOut{Role: "user", Content: []any{toolResultOut{ToolUseID: result.ToolCallID, Type: "tool_result", Content: content, IsError: result.IsError}}}
	line.ToolUseResult = strings.Join(texts, "\n")
	line.SourceToolAssistantUUID = callLine
	return w.writeUser(line)
}

func (w *lineWriter) writeUser(line userLineOut) error {
	if err := w.write(line); err != nil {
		return err
	}
	w.parent = line.UUID
	return nil
}

// assistant writes an assistant line and returns its uuid.
func (w *lineWriter) assistant(m assistantMessageOut) (string, error) {
	line := assistantLineOut{ParentUUID: w.parentUUID(), Message: m, Type: "assistant", UUID: uuid.NewString(), Timestamp: w.at, Effort: w.effort, commonOut: w.common}
	if err := w.write(line); err != nil {
		return "", err
	}
	w.parent = line.UUID
	return line.UUID, nil
}

func (w *lineWriter) checkpoint(questionID, lastPrompt string) error {
	if err := w.write(answerLineOut{Type: AnswerLineType, QuestionID: questionID, SessionID: w.common.SessionID, Timestamp: w.at}); err != nil {
		return err
	}
	return w.write(lastPromptOut{Type: "last-prompt", LastPrompt: lastPrompt, LeafUUID: w.parent, SessionID: w.common.SessionID})
}

func (w *lineWriter) write(v any) error {
	enc := json.NewEncoder(&w.buf)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// assistantBlockOut is a model turn's block as Claude Code records it from the Messages API.
func assistantBlockOut(b conversation.Block) (any, error) {
	switch b.Type {
	case conversation.Text:
		return textOut{Type: "text", Text: b.Text}, nil
	case conversation.Thinking:
		return thinkingOut{Type: "thinking", Thinking: b.Text, Signature: b.Signature}, nil
	case conversation.RedactedThinking:
		return redactedOut{Type: "redacted_thinking", Data: b.Data}, nil
	case conversation.ToolCall:
		return toolUseOut{Type: "tool_use", ID: b.ToolCallID, Name: b.ToolName, Input: conversation.JSONObjectOrEmpty(b.Arguments)}, nil
	}
	return nil, fmt.Errorf("a %s block in a model turn", b.Type)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// The shapes Claude Code writes, field for field in its order.
type (
	// commonOut are the fields that end every conversation line.
	commonOut struct {
		UserType   string `json:"userType"`
		Entrypoint string `json:"entrypoint"`
		Cwd        string `json:"cwd"`
		SessionID  string `json:"sessionId"`
		Version    string `json:"version"`
		GitBranch  string `json:"gitBranch,omitempty"`
	}
	userLineOut struct {
		ParentUUID              *string        `json:"parentUuid"`
		IsSidechain             bool           `json:"isSidechain"`
		PromptID                string         `json:"promptId"`
		Type                    string         `json:"type"`
		Message                 userMessageOut `json:"message"`
		UUID                    string         `json:"uuid"`
		Timestamp               string         `json:"timestamp"`
		ToolUseResult           any            `json:"toolUseResult,omitempty"`
		SourceToolAssistantUUID string         `json:"sourceToolAssistantUUID,omitempty"`
		commonOut
	}
	userMessageOut struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	assistantLineOut struct {
		ParentUUID  *string             `json:"parentUuid"`
		IsSidechain bool                `json:"isSidechain"`
		Message     assistantMessageOut `json:"message"`
		Type        string              `json:"type"`
		UUID        string              `json:"uuid"`
		Timestamp   string              `json:"timestamp"`
		Effort      string              `json:"effort,omitempty"`
		commonOut
	}
	assistantMessageOut struct {
		ID           string    `json:"id"`
		Type         string    `json:"type"`
		Role         string    `json:"role"`
		Model        string    `json:"model"`
		Content      []any     `json:"content"`
		StopReason   string    `json:"stop_reason"`
		StopSequence *string   `json:"stop_sequence"`
		Usage        usageOut  `json:"usage"`
		StopDetails  *struct{} `json:"stop_details"`
	}
	usageOut struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
	}
	lastPromptOut struct {
		Type       string `json:"type"`
		LastPrompt string `json:"lastPrompt"`
		LeafUUID   string `json:"leafUuid"`
		SessionID  string `json:"sessionId"`
	}
	// answerLineOut is the daemon's own marker, not a shape Claude Code writes: the question an
	// append answered, so a reader knows the append landed.
	answerLineOut struct {
		Type       string `json:"type"`
		QuestionID string `json:"questionId"`
		SessionID  string `json:"sessionId"`
		Timestamp  string `json:"timestamp"`
	}
	textOut struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	thinkingOut struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	redactedOut struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	toolUseOut struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	toolResultOut struct {
		ToolUseID string `json:"tool_use_id"`
		Type      string `json:"type"`
		Content   any    `json:"content"`
		IsError   bool   `json:"is_error,omitempty"`
	}
	imageOut struct {
		Type   string         `json:"type"`
		Source imageSourceOut `json:"source"`
	}
	imageSourceOut struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
)
