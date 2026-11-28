package omp

import "strings"

// Model ID matchers for replay, from catalog/src/compat/rules/taxonomy/*.kdl.
// Fields are lists of space-separated tokens. A match ranks by kind, then token length.
var replayClassMatchers = [...]struct {
	class, exact, bounded, namespace, boundedNamespace, prefix string
}{
	{class: "ai21", bounded: "jamba", namespace: "ai21"},
	{class: "amazon", bounded: "nova titan", boundedNamespace: "amazon"},
	{class: "anthropic", bounded: "anthropic claude", boundedNamespace: "anthropic"},
	{class: "baidu", bounded: "ernie"},
	{class: "bytedance", bounded: "doubao"},
	{class: "cohere", bounded: "command", namespace: "cohere"},
	{class: "deepseek", bounded: "deepseek", boundedNamespace: "deepseek"},
	{class: "gemini", bounded: "gemini"},
	{class: "gemma", bounded: "gemma"},
	{class: "glm", bounded: "glm zai-glm"},
	{class: "gpt-oss", bounded: "gpt-oss", boundedNamespace: "gpt-oss"},
	{class: "kimi", exact: "k3 k3-256k", bounded: "kimi", namespace: "moonshotai"},
	{class: "meta", bounded: "muse-spark muse-wonder llama meta-llama", namespace: "meta-llama"},
	{class: "mimo", bounded: "mimo"},
	{class: "minimax", bounded: "minimax hailuo", namespace: "minimax"},
	{class: "mistral", bounded: "mistral mixtral"},
	{class: "openai", exact: "o1 o3 o4", namespace: "openai", boundedNamespace: "openai", prefix: "gpt- chatgpt- codex- o1- o1. o3- o3. o4- o4."},
	{class: "qwen", bounded: "qwen deepseek-r1-distill-qwen"},
	{class: "stepfun", bounded: "step"},
	{class: "xai", bounded: "grok", namespace: "x-ai", boundedNamespace: "xai", prefix: "cursor-grok-"},
}

func boundedModelToken(value, token string) bool {
	tail, ok := strings.CutPrefix(value, token)
	return ok && (tail == "" || strings.ContainsRune("-_.:0123456789", rune(tail[0])))
}

// replayModelClass is the pinned taxonomy's lenient, provider-neutral classification.
// A provider-specific override that affects Anthropic policy is handled at that policy's call site.
func replayModelClass(id string) string {
	id = strings.ToLower(strings.TrimFunc(id, isJSSpace))
	bare := id[strings.LastIndexByte(id, '/')+1:]
	switch {
	case bare == "daybreak-blue-latest" || bare == "daybreak-red-latest":
		return ""
	case bare == "gpt-daybreak-blue-latest" || bare == "gpt-daybreak-red-latest":
		return "openai"
	case strings.HasSuffix(bare, "k3.1-pro") || strings.HasSuffix(bare, "k3.1-pro-highspeed"):
		return "kimi"
	case strings.Contains(bare, "bonsai-27b") || strings.Contains(bare, "bonsai-2-27b"):
		return "qwen"
	}
	winner, bestKind, bestLength := "", -1, 0
	tied := false
	match := func(class, token string, kind int) {
		if kind > bestKind || (kind == bestKind && len(token) > bestLength) {
			winner, bestKind, bestLength, tied = class, kind, len(token), false
		} else if kind == bestKind && len(token) == bestLength && winner != class {
			tied = true
		}
	}
	if strings.Contains(bare, "distill-qwen") {
		match("qwen", "*distill-qwen*", 0)
	}
	for _, rule := range replayClassMatchers {
		for token := range strings.FieldsSeq(rule.exact) {
			if bare == token {
				match(rule.class, token, 4)
			}
		}
		for token := range strings.FieldsSeq(rule.bounded) {
			if boundedModelToken(bare, token) {
				match(rule.class, token, 3)
			}
		}
		for token := range strings.FieldsSeq(rule.namespace) {
			for part := range strings.SplitSeq(id, "/") {
				if part == token {
					match(rule.class, token, 2)
					break
				}
			}
		}
		for token := range strings.FieldsSeq(rule.boundedNamespace) {
			for part := range strings.FieldsFuncSeq(id, func(r rune) bool { return r == '/' || r == '.' || r == ':' }) {
				if boundedModelToken(part, token) {
					match(rule.class, token, 2)
					break
				}
			}
		}
		for token := range strings.FieldsSeq(rule.prefix) {
			if strings.HasPrefix(bare, token) {
				match(rule.class, token, 1)
			}
		}
	}
	if tied {
		return ""
	}
	return winner
}

// demotedThinking follows dialect/demotion.ts and catalog/src/identity/dialect.ts.
func demotedThinking(class, text string) string {
	if text == "" {
		return ""
	}
	switch class {
	case "anthropic":
		return text
	case "gemini":
		return "```thinking\n" + text + "\n```"
	case "openai", "gpt-oss", "gemma", "glm", "kimi", "qwen", "deepseek":
		return "<think>\n" + text + "\n</think>"
	default: // MiniMax and the default XML dialect share the same wrapper.
		return "<thinking>\n" + unwrapThinkingXML(text) + "\n</thinking>"
	}
}

// unwrapThinkingXML removes complete adjacent or nested thinking wrappers. It preserves the
// original text if only part of it is wrapped (dialect/rendering.ts:233–291).
func unwrapThinkingXML(text string) string {
	const open, close, whitespace = "<thinking>", "</thinking>", " \t\n\r\v\f"
	tail := strings.Trim(text, whitespace)
	if !strings.HasPrefix(tail, open) {
		return text
	}
	var segments []string
	for tail != "" {
		inner, ok := strings.CutPrefix(tail, open)
		if !ok {
			return text
		}
		cursor, depth := 0, 1
		for depth > 0 {
			nextClose := strings.Index(inner[cursor:], close)
			if nextClose < 0 {
				return text
			}
			if nextOpen := strings.Index(inner[cursor:cursor+nextClose], open); nextOpen >= 0 {
				depth++
				cursor += nextOpen + len(open)
				continue
			}
			depth--
			cursor += nextClose + len(close)
		}
		segments = append(segments, unwrapThinkingXML(strings.Trim(inner[:cursor-len(close)], whitespace)))
		tail = strings.TrimLeft(inner[cursor:], whitespace)
	}
	return strings.Join(segments, "\n")
}
