package omp

import (
	"encoding/binary"
	"math/bits"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/sjawhar/legion/daemon/internal/conversation"
)

type normalizedToolIDs struct {
	exact, responses map[string]string
}

func (ids normalizedToolIDs) result(id string) string {
	if exact, ok := ids.exact[id]; ok {
		return exact
	}
	if component := responsesComponent(id); component != id {
		if mapped, ok := ids.responses[component]; ok {
			return mapped
		}
	}
	return id
}

func isResponsesFamily(api string) bool {
	switch api {
	case conversation.OpenAIResponses, "openai-codex-responses", "azure-openai-responses":
		return true
	}
	return false
}

func responsesComponent(id string) string {
	if i := strings.IndexByte(id, '|'); i > 0 {
		return id[:i]
	}
	return id
}

// toolPairingKey splits only a composite known to come from Responses. A concrete id
// from another API takes precedence over a Responses call with the same prefix.
func toolPairingKey(source []llmMessage, messages []conversation.Message) func(string) string {
	responses := map[string]bool{}
	opaque := map[string]bool{}
	for i, m := range messages {
		if m.Role != conversation.Assistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type != conversation.ToolCall {
				continue
			}
			id := b.ToolCallID
			if isResponsesFamily(source[i].api) {
				responses[responsesComponent(id)] = true
			} else if strings.Contains(id, "|") {
				opaque[id] = true
			}
		}
	}
	return func(id string) string {
		component := responsesComponent(id)
		if component != id && !opaque[id] && responses[component] {
			return component
		}
		return id
	}
}

// deduplicateToolCalls renames repeated calls and rewrites their results in FIFO order.
// A later assistant turn that reuses an id starts a new queue for it.
func deduplicateToolCalls(messages []conversation.Message, key func(string) string) {
	seen := map[string]int{}
	pending := map[string][]string{}
	for _, m := range messages {
		switch m.Role {
		case conversation.Tool:
			b := &m.Content[0]
			k := key(b.ToolCallID)
			if queue := pending[k]; len(queue) != 0 {
				pending[k] = queue[1:]
				if replacement := queue[0]; replacement != "" {
					b.ToolCallID = replacement
				}
			}
		case conversation.Assistant:
			touched := map[string]bool{}
			for i := range m.Content {
				b := &m.Content[i]
				if b.Type != conversation.ToolCall {
					continue
				}
				k := key(b.ToolCallID)
				if !touched[k] {
					delete(pending, k)
					touched[k] = true
				}
				n := seen[k]
				if n == 0 {
					seen[k] = 1
					pending[k] = append(pending[k], "")
					continue
				}
				for {
					replacement := duplicateToolID(b.ToolCallID, "_dup"+strconv.Itoa(n))
					n++
					if seen[key(replacement)] == 0 {
						seen[k] = n
						seen[key(replacement)] = 1
						pending[k] = append(pending[k], replacement)
						b.ToolCallID = replacement
						if b.ItemID != "" {
							_, b.ItemID = splitResponsesToolCallID(replacement)
						}
						break
					}
				}
			}
		}
	}
}

func duplicateToolID(id, suffix string) string {
	parts := strings.Split(id, "|")
	for i, part := range parts {
		remaining := 64 - len(suffix)
		for at, r := range part {
			remaining -= utf16.RuneLen(r)
			if remaining < 0 {
				part = part[:at]
				break
			}
		}
		parts[i] = part + suffix
	}
	return strings.Join(parts, "|")
}

// normalizeResponsesReplayID is the cross-model normalizer in openai-shared.ts:1533–1550.
// Retain both segments until deduplication. The neutral model stores only call_id.
func normalizeResponsesReplayID(id string, foreign bool) string {
	if validAnthropicToolCallID.MatchString(id) {
		return id
	}
	sep := strings.IndexAny(id, "|\n")
	if !foreign && sep < 0 && utf16Length(id) <= 64 {
		return id + "|fc_" + bunHash(id)
	}
	base, item := id, id
	if sep > 0 {
		base = id[:sep]
		if sep+1 < len(id) {
			item = id[sep+1:]
		}
	} else if sep == 0 {
		base = id[1:]
	}
	sanitized := strings.TrimRight(sanitizeToolIDChars(base), "_")
	if sanitized != base || sanitized == "" || len(sanitized) > 64 {
		if sanitized == "" {
			sanitized = "call"
		}
		hashInput := base
		if hashInput == "" {
			hashInput = id
		}
		hash := bunHash(hashInput)
		sanitized = sanitized[:min(len(sanitized), 63-len(hash))] + "_" + hash
	}
	return sanitized + "|fc_" + bunHash(item)
}

func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// bunHash matches Bun.hash(s).toString(36): Wyhash with seed zero over UTF-8.
func bunHash(s string) string {
	const p0 = 0xa0761d6478bd642f
	const p1 = 0xe7037ed1a0b428db
	const p2 = 0x8ebc6af09c88c6e3
	const p3 = 0x589965cc75374cc3
	mix := func(a, b uint64) uint64 {
		hi, lo := bits.Mul64(a, b)
		return hi ^ lo
	}
	var a, b uint64
	seed := mix(p0, p1)
	n := len(s)
	if n <= 16 {
		if n >= 4 {
			read := func(i int) uint64 { return uint64(binary.LittleEndian.Uint32([]byte(s[i:]))) }
			quarter := (n >> 3) << 2
			a = read(0)<<32 | read(quarter)
			b = read(n-4)<<32 | read(n-4-quarter)
		} else if n > 0 {
			a = uint64(s[0])<<16 | uint64(s[n>>1])<<8 | uint64(s[n-1])
		}
	} else {
		read := func(i int) uint64 { return binary.LittleEndian.Uint64([]byte(s[i:])) }
		i := 0
		if n > 48 {
			s1, s2 := seed, seed
			for i+48 < n {
				seed = mix(read(i)^p1, read(i+8)^seed)
				s1 = mix(read(i+16)^p2, read(i+24)^s1)
				s2 = mix(read(i+32)^p3, read(i+40)^s2)
				i += 48
			}
			seed ^= s1 ^ s2
		}
		for i+16 < n {
			seed = mix(read(i)^p1, read(i+8)^seed)
			i += 16
		}
		a, b = read(n-16), read(n-8)
	}
	b, a = bits.Mul64(a^p1, b^seed)
	return strconv.FormatUint(mix(a^p0^uint64(n), b^p1), 36)
}
