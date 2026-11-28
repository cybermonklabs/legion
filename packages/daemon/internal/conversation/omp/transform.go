package omp

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// target is the model the conversation resumes on, which Oh My Pi's provider layer
// (pi-ai transform-messages.ts) shapes the history for, and the endpoint it is called through.
type target struct {
	provider, api, model string
	// signing is an Anthropic endpoint that verifies thinking signatures and replays no unsigned
	// thinking (Options).
	signing              bool
	officialAnthropic    bool
	textOnly             bool
	prefixBinding        bool
	demotionClass        string
	anthropicSystem      bool
	anthropicEffort      bool
	anthropicReplay      bool
	anthropicLegacy      bool
	anthropicRedactFiles bool
}

// toRequest applies the pinned history transforms and projects the target encoder's
// message content onto the neutral conversation. Options describes the replay inputs it lacks.
func toRequest(messages []llmMessage, t target) ([]conversation.Message, error) {
	if t.api == conversation.AnthropicMessages {
		t.prefixBinding = anthropicPrefixBinding(t.model)
		t.anthropicSystem, t.anthropicEffort = anthropicControlSupport(t.model)
	}
	t.demotionClass = replayModelClass(t.model)
	messages = dropMalformedToolCalls(messages)
	latest := -1
	for i, m := range messages {
		if m.role == "assistant" && !truncatedThinkingOnly(m) {
			latest = i
		}
	}
	ids := normalizedToolIDs{exact: map[string]string{}, responses: map[string]string{}}
	out := make([]conversation.Message, len(messages))
	var latestRewrite *float64
	var compactionFiles *anthropicCompactionFiles
	if t.api == conversation.AnthropicMessages && t.anthropicReplay {
		compactionFiles = &anthropicCompactionFiles{redact: t.anthropicRedactFiles}
	}
	for i, m := range messages {
		if t.prefixBinding && m.rewriteAt != nil && (latestRewrite == nil || *m.rewriteAt > *latestRewrite) {
			latestRewrite = m.rewriteAt
		}
		if t.textOnly && (m.role == "user" || m.role == "toolResult") {
			m.blocks = replaceHistoryImages(m.blocks)
		}
		var err error
		switch m.role {
		case "user":
			out[i], err = userMessage(m, t)
			if t.api == conversation.OpenAIResponses && out[i].Native != nil &&
				out[i].Native.Kind == conversation.OpenAIResponsesHistory {
				native := *out[i].Native
				native.Items = shapeNativeResponsesItems(native.Items, false)
				out[i].Native = &native
			}
		case "assistant":
			rewritten := latestRewrite != nil && m.timestamp != nil && *m.timestamp <= *latestRewrite
			out[i], err = assistantMessage(m, t, i == latest, rewritten, ids)
		case "toolResult":
			out[i], err = toolResultMessage(m)
			if err == nil {
				out[i].Content[0].ToolCallID = ids.result(m.toolCallID)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	pairingKey := toolPairingKey(messages, out)
	deduplicateToolCalls(out, pairingKey)
	paired := pairToolResults(messages, out, pairingKey, compactionFiles)
	if t.api == conversation.OpenAIResponses {
		return shapeResponsesHistory(paired), nil
	}
	return shapeAnthropicHistory(paired, t), nil
}

const historyImageOmission = "[image omitted: the active model does not support image input]"

// replaceHistoryImages mirrors the session SDK's pre-encoder replacement. A
// replaced image does not add a second copy immediately after the same note.
func replaceHistoryImages(blocks []block) []block {
	var out []block
	for i, b := range blocks {
		if b.Type != "image" {
			if out != nil {
				out = append(out, b)
			}
			continue
		}
		if out == nil {
			out = make([]block, i, len(blocks))
			copy(out, blocks[:i])
		}
		if len(out) > 0 && out[len(out)-1].Type == "text" && out[len(out)-1].Text == historyImageOmission {
			continue
		}
		out = append(out, block{Type: "text", Text: historyImageOmission})
	}
	if out != nil {
		return out
	}
	return blocks
}

// userMessage converts the saved text and inline images. Both encoders omit blank input text.
func userMessage(m llmMessage, t target) (conversation.Message, error) {
	msg := conversation.Message{Role: conversation.User, Native: m.native}
	for _, b := range m.blocks {
		converted, keep, err := inputBlock(b)
		if err != nil {
			return conversation.Message{}, err
		}
		if keep && (converted.Type != conversation.Text || !blank(converted.Text)) {
			msg.Content = append(msg.Content, converted)
		}
	}
	switch t.api {
	case conversation.AnthropicMessages:
		msg.Content = anthropicInput(msg.Content, t.textOnly)
		selectAnthropicCompaction(&msg, t)
		if m.developer {
			markAnthropicDeveloper(&msg, m.attribution, m.payload, m.stringInput, t)
		}
	case conversation.OpenAIResponses:
		if msg.Native != nil && msg.Native.Kind == conversation.AnthropicCompaction {
			msg.Native = nil
		}
		if msg.Native == nil {
			msg.Native = responsesInputHistory(m.payload, t.provider)
		}
	}
	return msg, nil
}

func toolResultMessage(m llmMessage) (conversation.Message, error) {
	result := conversation.Block{Type: conversation.ToolResult, ToolCallID: m.toolCallID, ToolName: m.toolName, IsError: m.isError,
		ProviderMetadata: m.providerMetadata}
	for _, b := range m.blocks {
		converted, keep, err := inputBlock(b)
		if err != nil {
			return conversation.Message{}, err
		}
		if keep {
			result.Content = append(result.Content, converted)
		}
	}
	return conversation.Message{Role: conversation.Tool, Content: []conversation.Block{result}}, nil
}

// inputBlock is a text or image block of a user message or tool result. An image Oh My Pi moved
// to its blob store (data "blob:sha256:…") refuses the session: the reader does not read the store.
func inputBlock(b block) (conversation.Block, bool, error) {
	switch b.Type {
	case "text":
		return conversation.Block{Type: conversation.Text, Text: b.Text}, true, nil
	case "image":
		if strings.HasPrefix(b.Data, "blob:") {
			return conversation.Block{}, false, conversation.Unknown("an image held in Oh My Pi's blob store")
		}
		if b.URL != "" || b.ProviderFile.ID != "" {
			return conversation.Block{}, false, conversation.Unknown("an image held at a URL or in a provider's file store")
		}
		return conversation.Block{Type: conversation.Image, MediaType: b.MimeType, ImageData: b.Data, Detail: b.Detail}, true, nil
	}
	return conversation.Block{}, false, nil
}

// assistantMessage replays an assistant turn after the common history transforms
// and applies the target encoder's thinking rules.
func assistantMessage(m llmMessage, t target, latest, rewritten bool, ids normalizedToolIDs) (conversation.Message, error) {
	msg := conversation.Message{Role: conversation.Assistant, Provider: m.provider, API: m.api, Model: m.model}
	native, err := nativeHistory(m, t)
	if err != nil {
		return conversation.Message{}, err
	}
	msg.Native = native
	hiddenEmpty := false
	if native != nil && t.api == conversation.OpenAIResponses {
		native.Items = shapeNativeResponsesItems(native.Items, true)
		hiddenEmpty = native.Items == nil
		if hiddenEmpty {
			msg.Native = nil
		}
	}
	sameModel := m.provider == t.provider && m.api == t.api && m.model == t.model
	anthropicTarget := t.api == conversation.AnthropicMessages
	anthropicReplay := anthropicTarget && m.api == conversation.AnthropicMessages
	official := anthropicTarget && t.officialAnthropic
	replaysUnsigned := anthropicTarget && !t.signing
	failed := m.stopReason == "aborted" || m.stopReason == "error"
	abandonedToolUse := !failed && m.stopReason != "toolUse" && hasToolCall(m.blocks)
	crossProvider := m.provider != t.provider
	dropThinking := (anthropicTarget && !sameModel) || rewritten

	// Each source block's replay, nil when it is dropped.
	converted := make([]*conversation.Block, len(m.blocks))
	visibleThinking, survivingThinking := false, false
	for i, b := range m.blocks {
		if dropThinking && (b.Type == "thinking" || b.Type == "redactedThinking") {
			continue
		}
		switch b.Type {
		case "thinking":
			visibleThinking = true
			thinking := conversation.Block{Type: conversation.Thinking, Text: b.Thinking, Signature: b.ThinkingSignature}
			signatureUntrustworthy := abandonedToolUse || (failed && i == len(m.blocks)-1)
			if signatureUntrustworthy {
				thinking.Signature = ""
			}
			switch {
			case anthropicTarget && latest && abandonedToolUse:
				// Anthropic's latest turn replays byte for byte.
				thinking.Signature = b.ThinkingSignature
			case anthropicTarget:
				if t.signing && blank(thinking.Signature) {
					continue
				}
			case sameModel && thinking.Signature != "":
			case blank(thinking.Text):
				continue
			case sameModel:
			default:
				thinking = conversation.Block{Type: conversation.Text, Text: demotedThinking(t.demotionClass, thinking.Text)}
			}
			survivingThinking = true
			converted[i] = &thinking
		case "text":
			converted[i] = &conversation.Block{Type: conversation.Text, Text: b.Text}
			if !anthropicTarget && sameModel {
				converted[i].ItemID, converted[i].Phase = responsesTextSignature(b.TextSignature)
			}
		case "anthropicServerTool":
			if anthropicReplay && !crossProvider {
				return conversation.Message{}, conversation.Unknown("a native Anthropic server tool block from provider %s", m.provider)
			}
		case "fallback":
			if anthropicTarget {
				return conversation.Message{}, conversation.Unknown("an Anthropic server-side fallback boundary")
			}
		case "toolCall":
			id := b.ID
			switch {
			case anthropicTarget && (!sameModel || official):
				id = anthropicToolCallID(b.ID)
			case t.api == conversation.OpenAIResponses && !sameModel:
				id = normalizeResponsesReplayID(b.ID, crossProvider || m.api != t.api)
			}
			if id != b.ID {
				ids.exact[b.ID] = id
			}
			if isResponsesFamily(m.api) && (!sameModel || official) {
				ids.responses[responsesComponent(b.ID)] = id
			}
			arguments := b.Arguments
			if anthropicTarget {
				if len(arguments) == 0 || string(arguments) == "null" {
					arguments = json.RawMessage("{}")
				}
				arguments = wellFormedAnthropicArguments(arguments)
			} else if len(arguments) == 0 {
				arguments = json.RawMessage("null")
			}
			converted[i] = &conversation.Block{Type: conversation.ToolCall, ToolCallID: id, ToolName: b.Name, Arguments: arguments}
			converted[i].CustomWireName = b.CustomWireName
			converted[i].Namespace = b.Namespace
			converted[i].ProviderMetadata = b.ProviderMetadata
			if !anthropicTarget && sameModel {
				_, converted[i].ItemID = splitResponsesToolCallID(b.ID)
			}
		}
	}
	// Redacted thinking replays only on Anthropic, beside the turn's visible thinking.
	dropsAllVisible := t.signing && visibleThinking && !survivingThinking
	for i, b := range m.blocks {
		if !dropThinking && b.Type == "redactedThinking" && anthropicTarget && !dropsAllVisible {
			converted[i] = &conversation.Block{Type: conversation.RedactedThinking, Data: b.Data}
		}
	}
	signed := false
	for _, block := range converted {
		if block != nil && block.Type == conversation.Thinking && !blank(block.Signature) {
			signed = true
		}
	}
	for _, block := range converted {
		if block == nil {
			continue
		}
		switch {
		case block.Type == conversation.Text && (anthropicTarget || hiddenEmpty) && blank(block.Text):
			continue
		case block.Type == conversation.RedactedThinking && blank(block.Data):
			continue
		case block.Type == conversation.Thinking && anthropicTarget && blank(block.Signature):
			// The Anthropic encoder sends unsigned thinking as thinking with an empty signature
			// where the endpoint replays it and the turn holds no signed thinking, else as text.
			if blank(block.Text) {
				continue
			}
			if signed || !replaysUnsigned {
				*block = conversation.Block{Type: conversation.Text, Text: demotedThinking(t.demotionClass, block.Text)}
			} else {
				block.Signature = ""
			}
		case block.Type == conversation.Thinking && !anthropicTarget && msg.Native == nil:
			if hiddenEmpty || m.stopReason == "error" || !responsesReasoningSignature(block.Signature) {
				continue
			}
		}
		msg.Content = append(msg.Content, *block)
	}
	return msg, nil
}

// nativeHistory is the OpenAI Responses history a turn carries when the target replays it: an
// openaiResponsesHistory payload of a turn on the target's API and model, from the target's
// provider. A payload of a type Oh My Pi does not write refuses the session.
func nativeHistory(m llmMessage, t target) (*conversation.Native, error) {
	if len(m.payload) == 0 || string(m.payload) == "null" {
		return nil, nil
	}
	var payload struct {
		Type     string            `json:"type"`
		Provider *string           `json:"provider"`
		DT       bool              `json:"dt"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(m.payload, &payload); err != nil {
		return nil, conversation.Unknown("an assistant providerPayload Oh My Pi's shape does not fit: %v", err)
	}
	switch payload.Type {
	case string(conversation.OpenAIResponsesHistory):
	case "cursorHistory":
		return nil, nil
	case string(conversation.AnthropicCompaction):
		return nil, conversation.Unknown("an assistant turn carrying an Anthropic compaction block")
	default:
		return nil, conversation.Unknown("providerPayload type %q", payload.Type)
	}
	provider := m.provider
	if payload.Provider != nil {
		provider = *payload.Provider
	}
	if m.api != t.api || m.model != t.model || provider != t.provider || payload.Items == nil {
		return nil, nil
	}
	return &conversation.Native{Kind: conversation.OpenAIResponsesHistory, Provider: provider, Incremental: payload.DT, Items: payload.Items, Payload: m.payload}, nil
}

var validAnthropicToolCallID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// anthropicToolCallID is a tool call id as an Anthropic request carries it: a valid id as it is,
// any other with each character outside [a-zA-Z0-9_-] replaced by '_' and cut to 64 (pi-ai
// utils.ts normalizeToolCallId), so an OpenAI "call_…|fc_…" id keeps both halves.
func anthropicToolCallID(id string) string {
	if validAnthropicToolCallID.MatchString(id) {
		return id
	}
	sanitized := sanitizeToolIDChars(id)
	return sanitized[:min(len(sanitized), 64)]
}

func sanitizeToolIDChars(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
			if r > 0xFFFF {
				// JavaScript's replace sees a surrogate pair as two code units.
				b.WriteByte('_')
			}
		}
	}
	return b.String()
}

// anthropicPrefixBinding applies the pinned Claude family and revision rules.
func anthropicPrefixBinding(id string) bool {
	family, major, minor := claudeFamilyRevision(id)
	// classes/anthropic.kdl:113–125,247–281. A collapsed 55 is not revision 5.5.
	switch family {
	case "fable", "mythos":
		return major > 5 || (major == 5 && minor >= 1)
	case "opus", "sonnet", "haiku":
		return major == 5 && minor >= 5
	}
	return false
}

func claudeFamilyRevision(id string) (family string, major, minor int) {
	bare := strings.ToLower(strings.TrimFunc(id, isJSSpace))
	tied := false
	for _, candidate := range []string{"opus", "sonnet", "haiku", "fable", "mythos"} {
		if strings.Contains(bare, candidate) {
			if len(family) == len(candidate) {
				tied = true
			}
			if len(candidate) > len(family) {
				family, tied = candidate, false
			}
		}
	}
	if tied {
		return "", 0, 0
	}
	tail := strings.TrimPrefix(bare, "claude-")
	first := strings.IndexAny(tail, "0123456789")
	if first < 0 {
		return "", 0, 0
	}
	major, minor = revisionComponents(tail[first:])
	return family, major, minor
}

// revisionComponents reads the first two components, the only ones these rules compare.
// The parser in revision.ts stops at a component over 255 or digits followed by a letter.
func revisionComponents(tail string) (major, minor int) {
	digits := func(s string) (int, int) {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || (i < len(s) && s[i] >= 'a' && s[i] <= 'z') {
			return 0, 0
		}
		n, err := strconv.ParseUint(s[:i], 10, 8)
		if err != nil {
			return 0, 0
		}
		return int(n), i
	}
	major, end := digits(tail)
	if end > 0 && end+1 < len(tail) && (tail[end] == '.' || tail[end] == '-') {
		minor, _ = digits(tail[end+1:])
	}
	return major, minor
}

// splitResponsesToolCallID separates the saved call and item IDs in
// "call_id|item_id" (pi-ai utils.ts normalizeResponsesToolCallId).
func splitResponsesToolCallID(id string) (callID, itemID string) {
	if i := strings.IndexAny(id, "|\n"); i > 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

func responsesCallID(id string) string {
	callID, _ := splitResponsesToolCallID(id)
	return callID
}

func blank(s string) bool { return strings.TrimFunc(s, isJSSpace) == "" }

func hasToolCall(blocks []block) bool {
	return slices.ContainsFunc(blocks, func(b block) bool { return b.Type == "toolCall" })
}

// truncatedThinkingOnly is shouldDropTruncatedThinkingOnlyAssistant: a turn the provider cut off
// (max tokens) with neither text nor a tool call is not replayed.
func truncatedThinkingOnly(m llmMessage) bool {
	if m.stopReason != "length" && m.stopReason != "error" && m.stopReason != "aborted" {
		return false
	}
	for _, b := range m.blocks {
		if b.Type == "toolCall" || b.Type == "text" {
			return false
		}
	}
	return true
}

// dropMalformedToolCalls removes invalid calls, the results paired with those occurrences,
// and assistant turns left with no blocks (transform-messages.ts:434–493).
func dropMalformedToolCalls(messages []llmMessage) []llmMessage {
	malformed := false
	for _, m := range messages {
		for _, b := range m.blocks {
			if m.role == "assistant" && b.Type == "toolCall" && malformedToolCall(b) {
				malformed = true
			}
		}
	}
	if !malformed {
		return messages
	}
	queues := map[string][]bool{}
	var out []llmMessage
	for _, m := range messages {
		switch m.role {
		case "assistant":
			clear(queues)
			var kept []block
			for _, b := range m.blocks {
				if b.Type == "toolCall" {
					bad := malformedToolCall(b)
					queues[b.ID] = append(queues[b.ID], bad)
					if bad {
						continue
					}
				}
				kept = append(kept, b)
			}
			if len(kept) == 0 {
				continue
			}
			m.blocks = kept
			out = append(out, m)
		case "toolResult":
			if queue := queues[m.toolCallID]; len(queue) > 0 {
				drop := queue[0]
				queues[m.toolCallID] = queue[1:]
				if drop {
					continue
				}
			}
			out = append(out, m)
		default:
			clear(queues)
			out = append(out, m)
		}
	}
	return out
}

var validToolCallName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func malformedToolCall(b block) bool {
	// The API default in providers/tool-call-names.kdl:3–5 is 128.
	return blank(b.ID) || len(b.Name) > 128 || !validToolCallName.MatchString(b.Name)
}

// pairToolResults preserves real results in their order inside a tool-use window.
// At the next turn boundary it pulls forward any late results and fills missing slots.
func pairToolResults(source []llmMessage, messages []conversation.Message, key func(string) string, files *anthropicCompactionFiles) []conversation.Message {
	calls := map[string]bool{}
	for _, m := range messages {
		if m.Role == conversation.Assistant {
			for _, b := range m.Content {
				if b.Type == conversation.ToolCall {
					calls[key(b.ToolCallID)] = true
				}
			}
		}
	}
	type indexed struct {
		index    int
		consumed bool
	}
	results := map[string][]*indexed{}
	for i, m := range messages {
		if m.Role == conversation.Tool {
			id := key(m.Content[0].ToolCallID)
			results[id] = append(results[id], &indexed{index: i})
		}
	}
	take := func(id string, after int) (conversation.Message, bool) {
		for _, r := range results[key(id)] {
			if !r.consumed && r.index > after {
				r.consumed = true
				return messages[r.index], true
			}
		}
		return conversation.Message{}, false
	}
	var out []conversation.Message
	resolved := map[string]bool{}
	var pending []conversation.Block
	pendingStart := -1
	missingResult := ""
	flush := func() {
		for _, call := range pending {
			callKey := key(call.ToolCallID)
			if resolved[callKey] {
				continue
			}
			resolved[callKey] = true
			if result, ok := take(call.ToolCallID, pendingStart); ok {
				out = append(out, result)
				continue
			}
			out = append(out, conversation.Message{Role: conversation.Tool, Content: []conversation.Block{{
				Type: conversation.ToolResult, ToolCallID: call.ToolCallID, ToolName: call.ToolName, IsError: true,
				Content: []conversation.Block{{Type: conversation.Text, Text: missingResult}},
			}}})
		}
		pending = nil
		if len(out) > 0 && out[len(out)-1].Role == conversation.Tool {
			out = files.flushLegacy(out)
		}
	}
	for i, m := range messages {
		switch m.Role {
		case conversation.Assistant:
			flush()
			if truncatedThinkingOnly(source[i]) {
				continue
			}
			out = files.beforeTurn(out, m, source[i])
			var turnCalls []conversation.Block
			for _, b := range m.Content {
				if b.Type == conversation.ToolCall {
					turnCalls = append(turnCalls, b)
				}
			}
			if len(turnCalls) > 0 {
				pending, pendingStart = turnCalls, i
				missingResult = "No result provided"
				if source[i].stopReason == "error" || source[i].stopReason == "aborted" {
					missingResult = "aborted"
				}
			}
			if len(m.Content) > 0 || m.Native != nil {
				out = append(out, m)
				if len(turnCalls) == 0 && (m.Native == nil || m.Native.Kind != conversation.AnthropicCompaction) {
					out = files.flushLegacy(out)
				}
			}
		case conversation.Tool:
			result := m.Content[0]
			resultKey := key(result.ToolCallID)
			if resolved[resultKey] {
				continue
			}
			if pendingHas(pending, resultKey, key) {
				resolved[resultKey] = true
				for _, r := range results[resultKey] {
					if r.index == i {
						r.consumed = true
					}
				}
				out = append(out, m)
				continue
			}
			if calls[resultKey] {
				continue
			}
			if pendingUnresolved(pending, resolved, key) {
				continue
			}
			if source[i].notExecuted {
				continue
			}
			var texts []string
			for _, part := range result.Content {
				if part.Type == conversation.Text && !blank(part.Text) {
					texts = append(texts, part.Text)
				}
			}
			if len(texts) > 0 {
				errorAttribute := ""
				if result.IsError {
					errorAttribute = ` is-error="true"`
				}
				note := conversation.Message{Role: conversation.User, Content: []conversation.Block{{
					Type: conversation.Text,
					Text: "<stale-tool-result tool=\"" + result.ToolName + "\" id=\"" + result.ToolCallID + "\"" + errorAttribute + ">\n" + strings.Join(texts, "\n") + "\n</stale-tool-result>",
				}}}
				out = files.beforeTurn(out, note, source[i])
				out = append(out, note)
			}
		default:
			flush()
			out = files.beforeTurn(out, m, source[i])
			if len(m.Content) > 0 || m.Native != nil {
				out = append(out, m)
			}
		}
	}
	flush()
	return files.finish(out)
}

func pendingHas(pending []conversation.Block, id string, key func(string) string) bool {
	return slices.ContainsFunc(pending, func(call conversation.Block) bool { return key(call.ToolCallID) == id })
}

func pendingUnresolved(pending []conversation.Block, resolved map[string]bool, key func(string) string) bool {
	return slices.ContainsFunc(pending, func(call conversation.Block) bool { return !resolved[key(call.ToolCallID)] })
}
