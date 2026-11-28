package omp

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

// llmMessage is one message as Oh My Pi's convertToLlm hands it to the provider layer.
type llmMessage struct {
	role   string // "user", "assistant" or "toolResult"
	blocks []block
	// A developer turn starts with the user projection, but Anthropic can upgrade it.
	developer            bool
	attribution          string
	payload              json.RawMessage
	stringInput          bool
	timestamp, rewriteAt *float64
	// assistant
	api, provider, model, stopReason    string
	continued                           bool
	userInterrupted, hasRequestControls bool
	// toolResult
	toolCallID, toolName string
	isError              bool
	synthetic            bool
	notExecuted          bool
	providerMetadata     json.RawMessage
	// custom user message
	customType string
	// provider-native replay derived from compaction
	native              *conversation.Native
	exactCompactionTail bool
}

// Custom message types the pinned Oh My Pi treats apart (session-context.ts, messages.ts,
// pi-tui chat/messages.ts, pi-wire).
const (
	prewalkPlanType         = "prewalk-plan"
	vibeModeContextType     = "vibe-mode-context"
	interruptedThinkingType = "interrupted-thinking"
	continueReminderType    = "continue-reminder"
	rewindReportType        = "rewind-report"
	skillPromptType         = "skill-prompt"
	collabPromptType        = "collab-prompt"
	defaultCustomType       = "custom"
	toolResultPruneType     = "tool_result_prune"
	contextNotesType        = "experimental_context_notes"
	maxContextNotesBytes    = 16_384
	snapcompactPreserveKey  = "snapcompact"
	anthropicCompactionKey  = "anthropicCompaction"
	openaiRemoteCompaction  = "openaiRemoteCompaction"
	contextRolloverKind     = "experimental-context-rollover"
	handoffCompactionMethod = "handoff"
	outputTruncatedNotice   = "[Output truncated]"
)

const snapcompactRefusal = "the newest compaction is a snapcompact archive, which the reader does not rebuild (dispatch://INFRA-6438)"

// sessionContext is what buildSessionContext reports besides the messages.
type sessionContext struct {
	leaf          string
	messages      []llmMessage
	thinkingLevel string
	model         string
	api           string
}

// buildContext rebuilds the model context along the resolved branch, as buildSessionContext
// does. It applies journaled tool result prunes and entry patches before building the context.
func buildContext(entries []*entry) (*sessionContext, error) {
	ctx := &sessionContext{thinkingLevel: "off"}
	if len(entries) == 0 {
		return ctx, nil
	}
	byID := make(map[string]*entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	if err := applyToolResultPrunes(entries, byID); err != nil {
		return nil, err
	}
	applyEntryPatches(entries, byID)
	leaf := resolveLeaf(entries, byID)
	if leaf == nil {
		return ctx, nil
	}
	ctx.leaf = leaf.ID
	path := activePath(leaf, byID)

	models := map[string]string{}
	explicitDefault := false
	hasThinkingEntry := false
	lastModelRole := ""
	resetIdx := -1
	for i, e := range path {
		switch e.Type {
		case "thinking_level_change":
			hasThinkingEntry = true
			ctx.thinkingLevel = "off"
			if e.ThinkingLevel != nil {
				ctx.thinkingLevel = *e.ThinkingLevel
			}
		case "model_change":
			role := "default"
			if e.Role != nil {
				role = *e.Role
			}
			lastModelRole = role
			if e.Model != "" {
				models[role] = e.Model
				if role == "default" {
					explicitDefault = true
				}
			}
		case "message":
			if e.Message.Role == "assistant" && !explicitDefault && e.Message.Model != "<synthetic>" {
				models["default"] = e.Message.Provider + "/" + e.Message.Model
			}
		case "reset_boundary":
			resetIdx = i
		}
	}
	selector := restoredModel(models, lastModelRole)
	var level string
	ctx.model, level = restoredModelID(entries, selector)
	if !hasThinkingEntry && level != "" && level != "auto" && level != "inherit" {
		ctx.thinkingLevel = level
	}
	provider, _, _ := strings.Cut(ctx.model, "/")
	ctx.api = modelAPI(entries, ctx.model)
	compactionIdx := -1
	for i, e := range slices.Backward(path) {
		if e.Type == "compaction" && canReplayCompaction(e, provider, ctx.api) {
			compactionIdx = i
			break
		}
	}

	rewindSummaries := make(map[string]bool)
	for i, e := range path {
		if i > 0 && e.Type == "custom_message" && e.CustomType == rewindReportType && path[i-1].Type == "branch_summary" {
			rewindSummaries[path[i-1].ID] = true
		}
	}
	turnLeftOut := false
	var messages []llmMessage
	emit := func(e *entry) error {
		if (e.Type == "branch_summary" && rewindSummaries[e.ID]) ||
			(e.Type == "custom_message" && e.CustomType == continueReminderType && turnLeftOut) {
			return nil
		}
		converted, err := entryMessages(e)
		if err != nil {
			return err
		}
		if e.Type == "message" && e.Message.Role == "assistant" {
			turnLeftOut = len(converted) == 0
		}
		messages = append(messages, converted...)
		return nil
	}
	emitRange := func(from, to int) error {
		for _, e := range path[from:to] {
			if err := emit(e); err != nil {
				return err
			}
		}
		return nil
	}

	switch {
	case resetIdx >= 0 && resetIdx > compactionIdx:
		if err := emitRange(resetIdx+1, len(path)); err != nil {
			return nil, err
		}
	case compactionIdx >= 0:
		compaction := path[compactionIdx]
		summary, remote, err := compactionSummary(compaction)
		if err != nil {
			return nil, err
		}
		retained := indexOf(path[:compactionIdx], compaction.FirstKeptEntryID)
		if summary.native != nil && summary.native.Kind == conversation.AnthropicCompaction {
			if retained < 0 && compaction.FirstKeptEntryID == "" && compaction.ProviderReplayThroughEntryID != "" {
				if through := indexOf(path, compaction.ProviderReplayThroughEntryID); through >= 0 {
					retained = through + 1
				}
			}
			first := retained
			if first < 0 || first >= compactionIdx {
				first = compactionIdx + 1
			}
			if first < len(path) {
				// session-context.ts:606–626 predates the rewrite marker to the retained tail.
				// An exactTail payload uses the first message's time, not its entry's time.
				at := entryMillis(path[first].Timestamp)
				if summary.exactCompactionTail && path[first].Type == "message" {
					at = path[first].Message.Timestamp
				}
				if at != nil {
					predatesTail := *at - 1
					summary.rewriteAt = &predatesTail
				}
			}
		}
		messages = append(messages, summary)
		var details struct{ Kind string }
		if json.Unmarshal(compaction.Details, &details) == nil && details.Kind == contextRolloverKind {
			firstKept := indexOf(path, compaction.FirstKeptEntryID)
			for i := compactionIdx - 1; i > resetIdx; i-- {
				if isRolloverRequest(path[i]) {
					if i < firstKept {
						if err := emit(path[i]); err != nil {
							return nil, err
						}
					}
					break
				}
			}
		}
		if remote == nil {
			if retained >= 0 && retained < compactionIdx {
				if err := emitRange(retained, compactionIdx); err != nil {
					return nil, err
				}
			}
		} else if compaction.ProviderReplayThroughEntryID != "" {
			if i := indexOf(path, compaction.ProviderReplayThroughEntryID); i >= 0 && i < compactionIdx {
				if err := emitRange(i+1, compactionIdx); err != nil {
					return nil, err
				}
			}
		}
		if err := emitRange(compactionIdx+1, len(path)); err != nil {
			return nil, err
		}
	default:
		if err := emitRange(0, len(path)); err != nil {
			return nil, err
		}
	}

	if notes, source := contextNotes(path); source != nil {
		at := 0
		if len(messages) > 0 && messages[0].native != nil && messages[0].native.Kind == conversation.AnthropicCompaction {
			at = 1
			if at < len(messages) && messages[at].role == "assistant" {
				at++
			}
			for at < len(messages) && messages[at].role == "toolResult" {
				at++
			}
		}
		messages = slices.Insert(messages, at, llmMessage{
			role: "user", developer: true, customType: contextNotesType,
			timestamp: entryMillis(source.Timestamp), blocks: []block{{Type: "text", Text: notes}},
		})
	}
	messages = stripDanglingToolCalls(messages)
	messages = dropFailedTurns(messages)
	ctx.messages = stripInterruptedThinking(dropOrphanedSyntheticResults(messages))
	return ctx, nil
}

// contextNotes renders the latest valid notebook revision since the last reset and returns its source.
func contextNotes(path []*entry) (string, *entry) {
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		if e.Type == "reset_boundary" {
			break
		}
		if e.Type != "custom" || e.CustomType != contextNotesType {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(e.Data, &data) != nil || len(data) != 2 {
			continue
		}
		var version float64
		var text *string
		if json.Unmarshal(data["version"], &version) != nil || version != 1 ||
			json.Unmarshal(data["text"], &text) != nil || text == nil || len(*text) > maxContextNotesBytes {
			continue
		}
		if *text != "" {
			return render(contextNotesTemplate, "{{notes}}", *text), e
		}
		break
	}
	return "", nil
}

// resolveLeaf follows resolveSessionLeafId: an absent capture selects the entry itself and null
// selects no branch. A stale capture falls back to the newest entry that selects itself.
func resolveLeaf(entries []*entry, byID map[string]*entry) *entry {
	if len(entries) == 0 {
		return nil
	}
	last := entries[len(entries)-1]
	if last.ActiveLeafID == nil {
		return byID[last.ID]
	}
	if string(last.ActiveLeafID) == "null" {
		return nil
	}
	if leaf := byID[capturedLeafID(last)]; leaf != nil {
		return leaf
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.ActiveLeafID == nil || capturedLeafID(e) == e.ID {
			return byID[e.ID]
		}
	}
	return nil
}

func capturedLeafID(e *entry) string {
	var id string
	_ = json.Unmarshal(e.ActiveLeafID, &id) // parseSession already checked the shape.
	return id
}

// activePath follows parentId from the resolved leaf and returns the path root first.
// Like branchPath, it stops at the first repeat in a parent cycle.
func activePath(leaf *entry, byID map[string]*entry) []*entry {
	var path []*entry
	seen := map[string]bool{}
	for current := leaf; current != nil && !seen[current.ID]; {
		seen[current.ID] = true
		path = append(path, current)
		if current.ParentID == nil {
			break
		}
		current = byID[*current.ParentID]
	}
	slices.Reverse(path)
	return path
}

// restoredModel is the model Oh My Pi restores the session on (getRestorableSessionModels' first
// choice): the model of the role the last model change named, unless that is the default or a
// retry fallback, else the default.
func restoredModel(models map[string]string, lastRole string) string {
	if lastRole != "" && lastRole != "default" && lastRole != "fallback" {
		if model, ok := models[lastRole]; ok {
			return model
		}
	}
	return models["default"]
}

// restoredModelID splits the thinking suffix on a saved model selector
// (model-selector.ts:59–78). A recorded literal :max or :auto model stands
// in for the registry's exact lookup for those two ambiguous suffixes.
func restoredModelID(entries []*entry, selector string) (id, level string) {
	slash, colon := strings.IndexByte(selector, '/'), strings.LastIndexByte(selector, ':')
	if slash <= 0 || colon <= slash+1 {
		return selector, ""
	}
	suffix := selector[colon+1:]
	if len(suffix) >= 2 {
		for _, candidate := range []string{"inherit", "off", "minimal", "low", "medium", "high", "xhigh", "max"} {
			if strings.HasPrefix(candidate, suffix) {
				if level != "" {
					return selector, ""
				}
				level = candidate
			}
		}
	}
	if suffix == "auto" {
		level = suffix
	}
	if level == "" || (level == "max" || level == "auto") && modelAPI(entries, selector) != "" {
		return selector, ""
	}
	return selector[:colon], level
}

func indexOf(path []*entry, id string) int {
	return slices.IndexFunc(path, func(e *entry) bool { return e.ID == id })
}

// modelAPI uses the API named by a recorded turn of the restored model, since the reader has
// no model registry.
func modelAPI(entries []*entry, selector string) string {
	provider, model, ok := strings.Cut(selector, "/")
	if !ok {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		m := entries[i].Message
		if m != nil && m.Role == "assistant" && m.Provider == provider && m.Model == model && m.API != "" {
			return m.API
		}
	}
	return ""
}

// canReplayCompaction applies canReplayRemoteCompaction for the APIs this reader supports.
func canReplayCompaction(e *entry, provider, api string) bool {
	var candidate struct {
		Provider           string            `json:"provider"`
		ReplacementHistory []json.RawMessage `json:"replacementHistory"`
		CompactionItem     json.RawMessage   `json:"compactionItem"`
	}
	if json.Unmarshal(e.PreserveData[openaiRemoteCompaction], &candidate) != nil || candidate.ReplacementHistory == nil {
		return true
	}
	if candidate.Provider != "" {
		return candidate.Provider == provider && api == conversation.OpenAIResponses
	}
	var item struct {
		Type             string  `json:"type"`
		EncryptedContent *string `json:"encrypted_content"`
	}
	if json.Unmarshal(candidate.CompactionItem, &item) == nil &&
		(item.Type == "compaction_summary" || item.Type == "compaction" && item.EncryptedContent != nil) {
		return false
	}
	return true
}

// isRolloverRequest identifies requests the compaction can restore verbatim before its kept tail.
func isRolloverRequest(e *entry) bool {
	userCustom := func(kind, attribution string) bool {
		return attribution == "user" && (kind == skillPromptType || kind == collabPromptType)
	}
	switch e.Type {
	case "message":
		m := e.Message
		return m.Role == "user" || m.Role == "custom" && userCustom(m.CustomType, m.Attribution)
	case "custom_message":
		if e.Attribution != nil && userCustom(e.CustomType, *e.Attribution) &&
			len(e.Content) > 0 && (e.Content[0] == '"' || e.Content[0] == '[') {
			return true
		}
		var details map[string]json.RawMessage
		return e.CustomType == "irc:incoming" && json.Unmarshal(e.Details, &details) == nil && string(details["fromParent"]) == "true"
	}
	return false
}

// applyToolResultPrunes blanks every tool result a tool_result_prune record names, as Oh My Pi's
// loader does (tool-result-prunes.ts): its content becomes the record's notice and it is marked
// pruned. A malformed record, or one naming no tool result, is skipped as Oh My Pi skips it.
func applyToolResultPrunes(entries []*entry, byID map[string]*entry) error {
	for _, e := range entries {
		if e.Type != "custom" || e.CustomType != toolResultPruneType {
			continue
		}
		var data struct {
			Results []struct {
				EntryID  *string  `json:"entryId"`
				Notice   *string  `json:"notice"`
				PrunedAt *float64 `json:"prunedAt"`
			} `json:"results"`
		}
		if json.Unmarshal(e.Data, &data) != nil || data.Results == nil {
			continue
		}
		valid := true
		for _, result := range data.Results {
			if result.EntryID == nil || result.Notice == nil || result.PrunedAt == nil {
				valid = false
			}
		}
		if !valid {
			continue
		}
		for _, result := range data.Results {
			target := byID[*result.EntryID]
			if target == nil || target.Type != "message" || target.Message.Role != "toolResult" {
				continue
			}
			notice, err := json.Marshal([]block{{Type: "text", Text: *result.Notice}})
			if err != nil {
				return err
			}
			target.Message.Content = notice
			target.Message.PrunedAt = result.PrunedAt
		}
	}
	return nil
}

// compactionSummary is the user message a compaction contributes (createCompactionSummaryMessage
// through convertMessageToLlm): its summary in Oh My Pi's template, carrying the provider-native
// replay the compaction preserved. remote is that replay when it is OpenAI's replacement history,
// which stands in for the kept messages.
func compactionSummary(c *entry) (summary llmMessage, remote *conversation.Native, err error) {
	if _, ok := c.PreserveData[snapcompactPreserveKey]; ok {
		return llmMessage{}, nil, conversation.Unknown(snapcompactRefusal)
	}
	template := compactionSummaryTemplate
	if c.Method == handoffCompactionMethod {
		template = handoffSummaryTemplate
	}
	stamp := entryMillis(c.Timestamp)
	summary = llmMessage{role: "user", timestamp: stamp, rewriteAt: stamp, blocks: []block{{Type: "text", Text: render(template, "{{summary}}", c.Summary)}}}
	if raw, ok := c.PreserveData[openaiRemoteCompaction]; ok {
		var candidate struct {
			Provider           string            `json:"provider"`
			ReplacementHistory []json.RawMessage `json:"replacementHistory"`
		}
		if json.Unmarshal(raw, &candidate) == nil && candidate.Provider != "" && candidate.ReplacementHistory != nil && allObjects(candidate.ReplacementHistory) {
			payload, err := json.Marshal(struct {
				Type     conversation.NativeKind `json:"type"`
				Provider string                  `json:"provider"`
				Items    []json.RawMessage       `json:"items"`
			}{conversation.OpenAIResponsesHistory, candidate.Provider, candidate.ReplacementHistory})
			if err != nil {
				return llmMessage{}, nil, err
			}
			summary.native = &conversation.Native{Kind: conversation.OpenAIResponsesHistory, Provider: candidate.Provider, Items: candidate.ReplacementHistory, Payload: payload}
			return summary, summary.native, nil
		}
	}
	if raw, ok := c.PreserveData[anthropicCompactionKey]; ok {
		var candidate struct {
			Provider    string          `json:"provider"`
			Content     string          `json:"content"`
			ToolChanges json.RawMessage `json:"toolChanges"`
			ExactTail   json.RawMessage `json:"exactTail"`
		}
		if json.Unmarshal(raw, &candidate) == nil && candidate.Provider != "" && candidate.Content != "" &&
			validToolChanges(candidate.ToolChanges) {
			summary.native = &conversation.Native{Kind: conversation.AnthropicCompaction, Provider: candidate.Provider, Payload: raw}
			summary.exactCompactionTail = string(candidate.ExactTail) == "true"
		}
	}
	return summary, nil, nil
}

func validToolChanges(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var changes []json.RawMessage
	return json.Unmarshal(raw, &changes) == nil && changes != nil && allObjects(changes)
}

func allObjects(items []json.RawMessage) bool {
	for _, item := range items {
		if !bytes.HasPrefix(bytes.TrimSpace(item), []byte("{")) {
			return false
		}
	}
	return true
}

func entryMillis(stamp string) *float64 {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return nil
	}
	ms := float64(at.UnixMilli())
	return &ms
}

// entryMessages is what one path entry contributes to the model context (appendMessage, then
// convertToLlm): a message entry's message, a custom message, a branch summary; nothing for any
// other entry.
func entryMessages(e *entry) ([]llmMessage, error) {
	switch e.Type {
	case "message":
		return messageEntryMessages(e)
	case "custom_message":
		if e.CustomType == prewalkPlanType || e.CustomType == vibeModeContextType {
			return nil, nil
		}
		customType := e.CustomType
		if customType == "" {
			customType = defaultCustomType
		}
		attribution := ""
		if e.Attribution != nil {
			attribution = *e.Attribution
		}
		return customMessages(customType, e.Content, attribution, "custom message "+e.ID, entryMillis(e.Timestamp), false)
	case "branch_summary":
		if e.Summary == "" {
			return nil, nil
		}
		stamp := entryMillis(e.Timestamp)
		return []llmMessage{{role: "user", timestamp: stamp, rewriteAt: stamp, blocks: []block{{Type: "text", Text: render(branchSummaryTemplate, "{{summary}}", e.Summary)}}}}, nil
	}
	return nil, nil
}

func messageEntryMessages(e *entry) ([]llmMessage, error) {
	m := e.Message
	where := "message " + e.ID
	switch m.Role {
	case "user", "developer":
		blocks, err := parseBlocks(m.Content, inputBlocks, where)
		if err != nil {
			return nil, err
		}
		if m.Role == "user" && m.Steering {
			blocks = steeringBlocks(blocks)
		}
		turn := llmMessage{role: "user", blocks: blocks, developer: m.Role == "developer",
			attribution: m.Attribution, payload: m.ProviderPayload, timestamp: m.Timestamp}
		turn.stringInput = startsJSON(m.Content, '"')
		if m.Role == "user" {
			turn.rewriteAt = m.HistoryRewriteAt
		}
		return []llmMessage{turn}, nil
	case "assistant":
		continued := string(m.Continued) == "true"
		if jsTruthy(m.RetryRecovery) && !continued {
			return nil, nil
		}
		blocks, err := parseBlocks(m.Content, assistantBlocks, where)
		if err != nil {
			return nil, err
		}
		if m.StopReason == "error" && emptyErrorTurn(blocks) {
			return nil, nil
		}
		for i := range blocks {
			if blocks[i].Type == "toolCall" {
				blocks[i].Name = stripXdPrefix(blocks[i].Name)
			}
		}
		const userInterruptFlag = 0x0400_0000 // ai/src/error/flags.ts: UserInterrupt
		return []llmMessage{{
			role: "assistant", blocks: blocks, api: m.API, provider: m.Provider, model: m.Model,
			stopReason: m.StopReason, payload: m.ProviderPayload, continued: continued, timestamp: m.Timestamp,
			userInterrupted: m.StopReason == "aborted" &&
				(int64(m.ErrorID)&userInterruptFlag != 0 || m.ErrorMessage == "Interrupted by user"),
			hasRequestControls: m.RequestControls != nil,
		}}, nil
	case "toolResult":
		blocks, err := parseBlocks(m.Content, inputBlocks, where)
		if err != nil {
			return nil, err
		}
		if m.PrunedAt != nil {
			blocks = prunedContent(blocks)
		}
		var details map[string]json.RawMessage
		_ = json.Unmarshal(m.Details, &details)
		synthetic := string(details["__synthetic"]) == "true"
		return []llmMessage{{role: "toolResult", blocks: blocks, toolCallID: m.ToolCallID, toolName: stripXdPrefix(m.ToolName), isError: m.IsError,
			synthetic: synthetic, notExecuted: synthetic && string(details["executed"]) == "false", rewriteAt: m.PrunedAt,
			providerMetadata: m.ProviderMetadata, timestamp: m.Timestamp}}, nil
	case "custom":
		return customMessages(m.CustomType, m.Content, m.Attribution, where, m.Timestamp, false)
	case "hookMessage":
		return customMessages(m.CustomType, m.Content, m.Attribution, where, m.Timestamp, true)
	case "fileMention":
		if m.Files != nil {
			return fileMentionMessages(m), nil
		}
	case "bashExecution", "pythonExecution":
		if m.ExcludeFromContext {
			return nil, nil
		}
		return []llmMessage{executionMessage(m)}, nil
	}
	return nil, conversation.Unknown("message role %q", m.Role)
}

// customMessages is convertToLlm's custom message: a collaborator's prompt in the steering
// envelope and a skill the user invoked as user turns, images split into a user turn of their own,
// anything else a developer message.
func customMessages(customType string, content json.RawMessage, attribution, where string, timestamp *float64, hook bool) ([]llmMessage, error) {
	markerType := customType
	if hook {
		// Continuation and interrupted-thinking markers are custom messages,
		// not legacy hooks whose labels happen to match.
		markerType = ""
	}
	one := func(blocks []block) llmMessage {
		return llmMessage{role: "user", blocks: blocks, customType: markerType, timestamp: timestamp}
	}
	developer := func(blocks []block) llmMessage {
		turn := one(blocks)
		turn.developer, turn.attribution = true, attribution
		return turn
	}
	trimmed := bytes.TrimSpace(content)
	textContent := bytes.HasPrefix(trimmed, []byte(`"`))
	if !textContent && !bytes.HasPrefix(trimmed, []byte("[")) {
		// Not custom message content: Oh My Pi restores no message from it.
		return nil, nil
	}
	blocks, err := parseBlocks(content, inputBlocks, where)
	if err != nil {
		return nil, err
	}
	if !hook && attribution == "user" {
		switch customType {
		case collabPromptType:
			return []llmMessage{one(steeringBlocks(blocks))}, nil
		case skillPromptType:
			return []llmMessage{one(blocks)}, nil
		}
	}
	var texts, images []block
	for _, b := range blocks {
		if b.Type == "image" {
			images = append(images, b)
		} else {
			texts = append(texts, b)
		}
	}
	if len(images) == 0 || textContent {
		return []llmMessage{developer(blocks)}, nil
	}
	var out []llmMessage
	if len(texts) > 0 {
		out = append(out, developer(texts))
	}
	note := block{Type: "text", Text: "Images attached to " + customType + "."}
	return append(out, one(append([]block{note}, images...))), nil
}

// steeringBlocks is wrapSteeringUserMessage: the message's text in the user-interjection envelope,
// its images after it; a message with no text is left as it is.
func steeringBlocks(blocks []block) []block {
	var texts []string
	var images []block
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		} else {
			images = append(images, b)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" {
		return blocks
	}
	return append([]block{{Type: "text", Text: render(steeringTemplate, "{{message}}", text)}}, images...)
}

// prunedContent is getPrunedToolResultContent: a pruned result's text blocks joined into its first
// text block ("[Output truncated]" when they are empty), its images kept.
func prunedContent(blocks []block) []block {
	var text strings.Builder
	first := -1
	for i, b := range blocks {
		if b.Type == "text" {
			text.WriteString(b.Text)
			if first < 0 {
				first = i
			}
		}
	}
	joined := text.String()
	if joined == "" {
		joined = outputTruncatedNotice
	}
	if first < 0 {
		return append([]block{{Type: "text", Text: joined}}, blocks...)
	}
	var out []block
	for i, b := range blocks {
		switch {
		case b.Type != "text":
			out = append(out, b)
		case i == first:
			out = append(out, block{Type: "text", Text: joined})
		}
	}
	return out
}

// jsTruthy reports JavaScript's Boolean conversion of JSON, including the true value of an
// empty object or array. buildSessionContext uses it for retryRecovery unless continued is true.
func jsTruthy(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		return false
	}
	switch value[0] {
	case 'n', 'f': // null, false
		return false
	case '"':
		return len(value) > len(`""`)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		// A number too small for a double reads as 0 here as in JSON.parse; one too large as ±Inf.
		n, _ := strconv.ParseFloat(string(value), 64)
		return n != 0
	}
	return true // true, an object, an array
}

// emptyErrorTurn is isEmptyErrorTurn's content test: no text, thinking or tool call worth keeping.
func emptyErrorTurn(blocks []block) bool {
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if !blank(b.Text) {
				return false
			}
		case "thinking":
			if !blank(b.Thinking) || !blank(b.ThinkingSignature) {
				return false
			}
		case "redactedThinking":
			if !blank(b.Data) {
				return false
			}
		case "fallback":
			// A fallback boundary is not content.
		default:
			return false
		}
	}
	return true
}

// stripXdPrefix is stripXdUrlPrefix: a tool name saved as an xd:// alias replays as the device.
func stripXdPrefix(name string) string {
	const prefix = "xd://"
	if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
		return name[len(prefix):]
	}
	return name
}

// stripDanglingToolCalls is buildSessionContext's dangling tool call pass: a tool call with no
// result in the context is dropped from its assistant turn, and that turn's redacted thinking with
// it and its thinking signatures cleared; a turn left empty is dropped.
func stripDanglingToolCalls(messages []llmMessage) []llmMessage {
	paired := map[string]bool{}
	for _, m := range messages {
		if m.role == "toolResult" {
			paired[m.toolCallID] = true
		}
	}
	out := messages[:0:0]
	for _, m := range messages {
		if m.role != "assistant" || !hasDangling(m.blocks, paired) {
			out = append(out, m)
			continue
		}
		var kept []block
		for _, b := range m.blocks {
			if (b.Type == "toolCall" && !paired[b.ID]) || b.Type == "redactedThinking" {
				continue
			}
			if b.Type == "thinking" {
				b.ThinkingSignature = ""
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			continue
		}
		m.blocks = kept
		out = append(out, m)
	}
	return out
}

func hasDangling(blocks []block, paired map[string]bool) bool {
	return slices.ContainsFunc(blocks, func(b block) bool { return b.Type == "toolCall" && !paired[b.ID] })
}

// dropFailedTurns applies the backward pass of buildSessionContext. A continue reminder needs a
// continued assistant turn. A failed turn that does not stay takes its paired results with it.
func dropFailedTurns(messages []llmMessage) []llmMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.customType == continueReminderType {
			j := i - 1
			for j >= 0 && sitsBetweenTurnAndReminder(messages[j]) {
				j--
			}
			if j < 0 || messages[j].role != "assistant" || !messages[j].continued {
				messages = slices.Delete(messages, i, i+1)
			}
			continue
		}
		if m.role != "assistant" || m.continued || (m.stopReason != "aborted" && m.stopReason != "error") {
			continue
		}
		if i+1 < len(messages) && messages[i+1].customType == interruptedThinkingType {
			continue
		}
		messages = slices.Delete(messages, i, i+1)
		callIDs := map[string]bool{}
		for _, b := range m.blocks {
			if b.Type == "toolCall" {
				callIDs[b.ID] = true
			}
		}
		if len(callIDs) > 0 {
			kept := slices.DeleteFunc(messages[i:], func(result llmMessage) bool {
				return result.role == "toolResult" && callIDs[result.toolCallID]
			})
			messages = messages[:i+len(kept)]
		}
	}
	return messages
}

func sitsBetweenTurnAndReminder(m llmMessage) bool {
	return m.role == "toolResult" || m.customType == contextNotesType
}

// dropOrphanedSyntheticResults removes only synthetic results with no surviving tool call.
func dropOrphanedSyntheticResults(messages []llmMessage) []llmMessage {
	calls := map[string]bool{}
	for _, m := range messages {
		if m.role != "assistant" {
			continue
		}
		for _, b := range m.blocks {
			if b.Type == "toolCall" {
				calls[b.ID] = true
			}
		}
	}
	return slices.DeleteFunc(messages, func(m llmMessage) bool {
		return m.synthetic && !calls[m.toolCallID]
	})
}

// stripInterruptedThinking removes unfinished thinking before the continuity note or on a user
// interrupt without a note. An otherwise empty interrupted turn stays only if it carried controls.
func stripInterruptedThinking(messages []llmMessage) []llmMessage {
	hasEmptyInterrupt := false
	for i := range messages {
		m := &messages[i]
		if m.role != "assistant" {
			continue
		}
		hasNote := i+1 < len(messages) && messages[i+1].customType == interruptedThinkingType
		if !hasNote && !m.userInterrupted {
			continue
		}
		end := len(m.blocks)
		for end > 0 && m.blocks[end-1].Type == "text" && blank(m.blocks[end-1].Text) {
			end--
		}
		start := end
		for start > 0 {
			b := m.blocks[start-1]
			if b.Type != "thinking" || blank(b.Thinking) || b.ThinkingSignature != "" {
				break
			}
			start--
		}
		if start < end {
			m.blocks = m.blocks[:start]
		}
		if m.userInterrupted && !hasNote && len(m.blocks) == 0 && !m.hasRequestControls {
			m.role = ""
			hasEmptyInterrupt = true
		}
	}
	if hasEmptyInterrupt {
		return slices.DeleteFunc(messages, func(m llmMessage) bool { return m.role == "" })
	}
	return messages
}
