package omp

import "encoding/json"

const entryPatchType = "entry_patch"

type patchFields map[string]json.RawMessage

type entryPatch struct {
	ID      *string         `json:"entryId"`
	Entry   json.RawMessage `json:"entry"`
	Message json.RawMessage `json:"message"`
}

// applyEntryPatches replays entry_patch records in file order. A malformed field
// in any patch invalidates the whole record. A missing or incompatible target
// skips just its patch (entry-patches.ts:240–343).
//
// A compaction accepts preserveData, warning and tokensAfter. A custom_message
// accepts entry content. Message content is a string or blocks for user and
// developer messages, but blocks only for assistant and toolResult messages.
// Only an assistant accepts retryRecovery, continued, contextSnapshot and
// elidedOutputTokens. Only a toolResult accepts prunedAt (entry-patches.ts:58–84,
// 145–183). We validate all of these fields, but only apply fields the
// conversation reads (entry-patches.ts:202–237).
func applyEntryPatches(entries []*entry, byID map[string]*entry) {
	for _, e := range entries {
		if e.Type != "custom" || e.CustomType != entryPatchType {
			continue
		}
		var data struct {
			Patches []entryPatch `json:"patches"`
		}
		if json.Unmarshal(e.Data, &data) != nil || data.Patches == nil {
			continue
		}
		type decoded struct{ entry, message patchFields }
		patches := make([]decoded, len(data.Patches))
		valid := true
		for i, p := range data.Patches {
			var fields decoded
			if p.ID == nil || !decodeFields(p.Entry, &fields.entry) || !decodeFields(p.Message, &fields.message) ||
				!validEntryFields(fields.entry) || !validMessageFields(fields.message) {
				valid = false
				break
			}
			patches[i] = fields
		}
		if !valid {
			continue
		}
		for i, p := range data.Patches {
			target := byID[*p.ID]
			if target != nil {
				applyPatch(target, patches[i].entry, patches[i].message)
			}
		}
	}
}

func decodeFields(raw json.RawMessage, fields *patchFields) bool {
	return raw == nil || (json.Unmarshal(raw, fields) == nil && *fields != nil)
}

func validEntryFields(f patchFields) bool {
	return optionalShape(f["warning"], '"') && optionalNumber(f["tokensAfter"]) &&
		optionalShape(f["preserveData"], '{') && validPatchContent(f["content"])
}

func validMessageFields(f patchFields) bool {
	return optionalShape(f["retryRecovery"], '{') && (f["continued"] == nil || string(f["continued"]) == "true") &&
		optionalShape(f["contextSnapshot"], '{') && optionalNumber(f["elidedOutputTokens"]) &&
		optionalNumber(f["prunedAt"]) && validPatchContent(f["content"])
}

func optionalShape(raw json.RawMessage, first byte) bool {
	return raw == nil || raw[0] == first
}

func optionalNumber(raw json.RawMessage) bool {
	return raw == nil || raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')
}

func validPatchContent(raw json.RawMessage) bool {
	if optionalShape(raw, '"') {
		return true
	}
	var blocks []struct{ Type *string }
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == nil {
			return false
		}
	}
	return true
}

func applyPatch(e *entry, fields, msg patchFields) {
	if (fields["warning"] != nil || fields["tokensAfter"] != nil || fields["preserveData"] != nil) && e.Type != "compaction" ||
		fields["content"] != nil && e.Type != "custom_message" || msg != nil && e.Type != "message" {
		return
	}
	if m := e.Message; msg != nil {
		if (msg["retryRecovery"] != nil || msg["continued"] != nil || msg["contextSnapshot"] != nil || msg["elidedOutputTokens"] != nil) && m.Role != "assistant" ||
			msg["prunedAt"] != nil && m.Role != "toolResult" {
			return
		}
		if content := msg["content"]; content != nil {
			switch m.Role {
			case "user", "developer":
			case "assistant", "toolResult":
				if content[0] != '[' {
					return
				}
			default:
				return
			}
			m.Content = content
		}
		if raw := msg["retryRecovery"]; raw != nil {
			m.RetryRecovery = raw
		}
		if raw := msg["continued"]; raw != nil {
			m.Continued = raw
		}
		if raw := msg["prunedAt"]; raw != nil {
			_ = json.Unmarshal(raw, &m.PrunedAt)
		}
	}
	if raw := fields["preserveData"]; raw != nil {
		var preserve map[string]json.RawMessage
		_ = json.Unmarshal(raw, &preserve)
		e.PreserveData = preserve
	}
	if raw := fields["content"]; raw != nil {
		e.Content = raw
	}
}
