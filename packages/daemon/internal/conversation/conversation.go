// Package conversation is a provider-neutral model of an agent's conversation as its harness
// rebuilds it for the model when the session resumes: the messages of the provider request the
// harness would send, in order, with each block as that request carries it, and what the
// conversation last ran on (its model, the provider API, its thinking level). A reader for each
// harness's own format (conversation/omp for Oh My Pi and conversation/claude for Claude Code)
// builds a Conversation. It returns an error matching ErrUnknownFormat for a record it cannot
// rebuild.
//
// Append is the other direction: a question, the model's turns answering it (each with its tool
// calls and results, its reasoning and any retry prompt) and the answer. The harness appender
// writes them into the session in the harness's own format for the agent to read on resume.
//
// The caller supplies the system prompt and other per-request context. The conversation omits
// them, including Oh My Pi's date and working-directory reminders journaled as date-cwd-reminder
// custom entries.
package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Role is who a message is from, as the provider request carries it.
type Role string

const (
	// User is a user turn: what a person typed, and what the harness sends as one (its own notes,
	// summaries of compacted history).
	User Role = "user"
	// System is a saved mid-conversation system turn, not the request's system prompt.
	System Role = "system"
	// Assistant is a model turn.
	Assistant Role = "assistant"
	// Tool is one tool call's result.
	Tool Role = "tool"
)

// BlockType is a content block's kind.
type BlockType string

const (
	// Text is plain text.
	Text BlockType = "text"
	// Thinking is the model's visible reasoning: Text, and the Signature the provider minted for
	// it (Anthropic), empty when the request carries it unsigned.
	Thinking BlockType = "thinking"
	// RedactedThinking is reasoning the provider returned encrypted: Data, opaque, sent back as it
	// came.
	RedactedThinking BlockType = "redacted_thinking"
	// ToolCall is a call the model made: ToolCallID, ToolName and Arguments, as Block.Arguments describes.
	ToolCall BlockType = "tool_call"
	// ToolResult is a call's result: ToolCallID, ToolName, Content (Text and Image blocks) and
	// IsError.
	ToolResult BlockType = "tool_result"
	// Image is an image: MediaType and ImageData, base64.
	Image BlockType = "image"
	// ToolAddition and ToolRemoval change tool availability in a saved message. ToolName
	// is the logical name before the sender applies the endpoint's wire naming rules.
	ToolAddition BlockType = "tool_addition"
	ToolRemoval  BlockType = "tool_removal"
)

// Block is one content block. Which fields it uses depends on its Type.
type Block struct {
	Type BlockType
	// Text is a Text block's text and a Thinking block's reasoning.
	Text string
	// Phase is a Text block's OpenAI Responses message phase, empty when unrecorded
	// or from another model. Read fills it. Append refuses a block that sets it.
	Phase string
	// ItemID is the saved OpenAI Responses message ID of a Text block or the item
	// half of a ToolCall block's ID. It is empty when unrecorded or from another
	// model. Read fills it. Append refuses a block that sets it.
	ItemID string
	// Signature is a Thinking block's signature, empty when unsigned.
	Signature string
	// Data is a RedactedThinking block's opaque payload.
	Data string
	// ToolCallID names the call of a ToolCall or ToolResult block.
	ToolCallID string
	// ToolName is the tool of a ToolCall or ToolResult block.
	ToolName string
	// CustomWireName and Namespace are the names a saved Responses ToolCall can
	// supply to the sender. Read fills them. Append refuses a block that sets either.
	CustomWireName string
	Namespace      string
	// ProviderMetadata is the saved provider data of a ToolCall or ToolResult,
	// such as computer actions, screenshots and safety checks. Read fills it.
	// Append refuses a block that sets it.
	ProviderMetadata json.RawMessage
	// Arguments is a ToolCall block's JSON arguments. The Claude reader preserves
	// the recorded input. The Oh My Pi reader uses null for missing or null
	// Responses arguments.
	// On Anthropic Messages it defaults missing or null arguments to {} and
	// repairs lone surrogate escapes in string values.
	Arguments json.RawMessage
	// Content is a ToolResult block's output: Text and Image blocks.
	Content []Block
	// IsError marks a ToolResult block whose call failed.
	IsError bool
	// MediaType and ImageData are an Image block's media type ("image/png") and base64 data.
	MediaType string
	ImageData string
	// Detail is an Image block's detail as recorded by the session, empty when
	// unrecorded. Read fills it. Append refuses a block that sets it.
	Detail string
	// Timestamp is the saved turn time in Unix milliseconds. Read fills it on
	// blocks that can mark Anthropic compaction file-note boundaries.
	// Append refuses a block that sets it.
	Timestamp *float64
}

// NativeKind is the kind of provider-native history a message carries.
type NativeKind string

const (
	// OpenAIResponsesHistory is the OpenAI Responses API's native history, including reasoning
	// items with encrypted content, function calls, and messages. The sender selects eligible
	// items for replay on that API and model.
	OpenAIResponsesHistory NativeKind = "openaiResponsesHistory"
	// AnthropicCompaction is the Anthropic API's own compaction block for a summary it wrote,
	// which the harness can send back in place of the summary's text.
	AnthropicCompaction NativeKind = "anthropicCompaction"
	// AnthropicMessageControls adds clear_at or output_config alongside the message blocks.
	AnthropicMessageControls NativeKind = "anthropicMessage"
)

// Native is provider-native history a message carries beside its blocks.
type Native struct {
	Kind NativeKind
	// Provider is the provider the history came from: only that provider's requests carry it.
	Provider string
	// Incremental distinguishes an assistant turn's own output from a snapshot.
	// It is unused on user turns, whose items stand in for that turn's fallback text.
	Incremental bool
	// Items are the OpenAIResponsesHistory items, including any reader-side replay repairs.
	Items []json.RawMessage
	// Payload preserves the session's bytes for native history, or the derived Responses
	// compaction payload.
	Payload json.RawMessage
	// Controls is set only for AnthropicMessageControls.
	Controls *AnthropicControls
	// Timestamp is the saved turn time in Unix milliseconds for Anthropic
	// compaction replay when the sender cannot use a block's Timestamp.
	Timestamp *float64
}

// AnthropicControls holds the message-level fields of an Anthropic system turn.
type AnthropicControls struct {
	ClearAt string // "next_user_message" or empty if the field is omitted
	Effort  string // output_config.effort, or empty if omitted
}

// OpenAIResponsesItem holds the fields the Oh My Pi appender uses from a Responses
// output item: reasoning, message content or a function call.
type OpenAIResponsesItem struct {
	Type             string  `json:"type"`
	ID               string  `json:"id"`
	Role             string  `json:"role"`
	Phase            string  `json:"phase"`
	EncryptedContent *string `json:"encrypted_content"`
	Summary          []struct {
		Text string `json:"text"`
	} `json:"summary"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// SummaryText is a reasoning item's summaries, their texts joined with a blank line between each
// two.
func (item *OpenAIResponsesItem) SummaryText() string {
	summaries := make([]string, len(item.Summary))
	for i, s := range item.Summary {
		summaries[i] = s.Text
	}
	return strings.Join(summaries, "\n\n")
}

// Message is one message of the rebuilt conversation.
type Message struct {
	Role    Role
	Content []Block
	// Native carries alternative provider-native history or fields that the sender
	// adds alongside the blocks, as specified by its Kind.
	Native *Native
	// Provider, API and Model are where an Assistant message's turn ran.
	Provider string
	API      string
	Model    string
}

const (
	// AnthropicMessages names the Anthropic Messages API.
	AnthropicMessages = "anthropic-messages"
	// OpenAIResponses names the OpenAI Responses API.
	OpenAIResponses = "openai-responses"
)

// Conversation is a session's conversation as its harness rebuilds it on resume.
type Conversation struct {
	Messages []Message
	// Model is the session's restored model selection, "<provider>/<model>" when known.
	Model string
	// API is the provider API the reader reports (AnthropicMessages or OpenAIResponses).
	API string
	// ThinkingLevel is the reasoning level the conversation last ran at ("off", "low", "high", …).
	ThinkingLevel string
}

// ErrUnknownFormat is what a reader's error Is when the session holds a record it does not know: a
// type, content block, header version or provider payload, or one it knows but cannot rebuild as
// the harness would.
var ErrUnknownFormat = errors.New("a conversation format the reader does not know")

// FormatError is a reader's refusal, naming what it did not know.
type FormatError struct {
	What string
}

func (e *FormatError) Error() string { return fmt.Sprintf("%s: %s", ErrUnknownFormat, e.What) }

// Is makes a FormatError match ErrUnknownFormat.
func (e *FormatError) Is(target error) bool { return target == ErrUnknownFormat }

// Unknown is a FormatError naming what was not known, formatted as fmt.Sprintf formats.
func Unknown(format string, args ...any) error {
	return &FormatError{What: fmt.Sprintf(format, args...)}
}

// ErrMoved matches an append error when the file or row does not have the expected byte length,
// or when an expected SQL row is missing.
var ErrMoved = errors.New("the session changed since it was read")

// Append is a question and the model's answer to it, or the turns of a run that ended with none
// and a note about that outcome (NoAnswer), which an appender writes into the session.
type Append struct {
	// QuestionID names the question; the appender records it with the answer.
	QuestionID string
	// Question is the question's text, written as a user turn.
	Question string
	// Provider, API and Model are the model that answered: its provider id, provider API
	// (AnthropicMessages or OpenAIResponses) and model id, as the harness names them.
	Provider string
	API      string
	Model    string
	// Turns are the model's turns, in order. Every turn but the last either made tool calls and
	// carries their results, or made none and carries the sentence it was asked again with
	// (AskedAgain), which the next turn answers; the last made none and its Text blocks are the
	// answer, unless the exchange has no answer (NoAnswer).
	Turns []Turn
	// NoAnswer is an optional note for an exchange that ended without an answer. An appender
	// writes it as a user message after the turns. With this note, the last turn may make tool
	// calls, hold no text or carry a retry prompt. It is empty for an exchange the model answered.
	NoAnswer string
}

// Turn is one model turn of an Append.
type Turn struct {
	// Content is what the model returned, in order: Thinking (with its Signature),
	// RedactedThinking, Text and ToolCall blocks. Every turn needs at least one block,
	// including OpenAI Responses turns that also carry Native items.
	Content []Block
	// Native is the turn's OpenAI Responses output items, each byte for byte as the API returned
	// it, for a turn on that API; the appender writes the turn from them.
	Native []json.RawMessage
	// Results are the turn's ToolResult blocks, one per ToolCall, in the calls' order.
	Results []Block
	// AskedAgain is the retry prompt after this turn's reply. An appender writes it as a user
	// message. A turn with tool calls cannot carry a retry prompt. The last turn can carry one
	// only if the exchange ended without an answer (Append.NoAnswer).
	AskedAgain string
	// Usage is what the turn's model call used.
	Usage Usage
}

// Usage is one model call's token counts and their cost in US dollars.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	InputCost        float64
	OutputCost       float64
	CacheReadCost    float64
	CacheWriteCost   float64
}

// Validate refuses an Append an appender could not write as a well-formed exchange.
func (a Append) Validate() error {
	switch {
	case a.QuestionID == "":
		return errors.New("append: no question id")
	case a.Question == "":
		return errors.New("append: no question")
	case a.Provider == "" || a.API == "" || a.Model == "":
		return errors.New("append: no provider, API or model")
	case len(a.Turns) == 0:
		return errors.New("append: no turns")
	}
	for i, turn := range a.Turns {
		if len(turn.Content) == 0 {
			return fmt.Errorf("append: turn %d has no content blocks", i+1)
		}
		var calls []Block
		for _, block := range turn.Content {
			if field := block.readOnlyField(); field != "" {
				return fmt.Errorf("append: turn %d: a block sets read-only %s", i+1, field)
			}
			switch block.Type {
			case Thinking, RedactedThinking, Text:
			case ToolCall:
				if block.ToolCallID == "" || block.ToolName == "" {
					return fmt.Errorf("append: turn %d: a tool call with no id or name", i+1)
				}
				calls = append(calls, block)
			default:
				return fmt.Errorf("append: turn %d: a %s block in a model turn", i+1, block.Type)
			}
		}
		last := i == len(a.Turns)-1
		answers := last && a.NoAnswer == ""
		switch {
		case answers && len(calls) > 0:
			return fmt.Errorf("append: the last turn makes %d tool calls; it must answer", len(calls))
		case answers && turn.AskedAgain != "":
			return errors.New("append: the last turn is asked again, and no turn answers it")
		case answers && !hasText(turn.Content):
			return errors.New("append: the last turn holds no answer text")
		case len(calls) > 0 && turn.AskedAgain != "":
			return fmt.Errorf("append: turn %d makes %d tool calls and is asked again", i+1, len(calls))
		case !last && len(calls) == 0 && turn.AskedAgain == "":
			return fmt.Errorf("append: turn %d makes no tool call and is not asked again, but is not the last", i+1)
		case turn.AskedAgain != "" && !hasText(turn.Content):
			return fmt.Errorf("append: turn %d is asked again but holds no reply text", i+1)
		}
		if len(turn.Results) != len(calls) {
			return fmt.Errorf("append: turn %d: %d tool calls and %d results", i+1, len(calls), len(turn.Results))
		}
		for j, result := range turn.Results {
			if result.Type != ToolResult || result.ToolCallID != calls[j].ToolCallID {
				return fmt.Errorf("append: turn %d: result %d is not the result of call %s", i+1, j+1, calls[j].ToolCallID)
			}
			if field := result.readOnlyField(); field != "" {
				return fmt.Errorf("append: turn %d: result %d sets read-only %s", i+1, j+1, field)
			}
			for _, part := range result.Content {
				if part.Type != Text && part.Type != Image {
					return fmt.Errorf("append: turn %d: result %d holds a %s block", i+1, j+1, part.Type)
				}
				if field := part.readOnlyField(); field != "" {
					return fmt.Errorf("append: turn %d: a block in result %d sets read-only %s", i+1, j+1, field)
				}
			}
		}
	}
	return nil
}

func (b Block) readOnlyField() string {
	switch {
	case b.Phase != "":
		return "Phase"
	case b.Detail != "":
		return "Detail"
	case b.ItemID != "":
		return "ItemID"
	case b.CustomWireName != "":
		return "CustomWireName"
	case b.Namespace != "":
		return "Namespace"
	case len(b.ProviderMetadata) != 0:
		return "ProviderMetadata"
	case b.Timestamp != nil:
		return "Timestamp"
	default:
		return ""
	}
}

// Delivery is a message for a stopped agent. An appender writes it as a user message without a
// model turn, so the agent can read it on resume.
type Delivery struct {
	// ID identifies the delivery in the marker the appender writes. A caller can check the
	// marker with the session's Answered method before appending a delivery again.
	ID string
	// Text is the message as the agent reads it, written as a user turn.
	Text string
}

// Validate refuses a Delivery an appender could not write as a message received.
func (d Delivery) Validate() error {
	switch {
	case d.ID == "":
		return errors.New("delivery: no id")
	case d.Text == "":
		return errors.New("delivery: no text")
	}
	return nil
}

func hasText(blocks []Block) bool {
	for _, block := range blocks {
		if block.Type == Text && block.Text != "" {
			return true
		}
	}
	return false
}
