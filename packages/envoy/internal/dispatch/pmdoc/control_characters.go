package pmdoc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrControlCharacter is a write refused because a block id or a typed block's directive attribute
// it introduces holds a control character (C0 U+0000 to U+001F, C1 U+0080 to U+009F, U+2028 or
// U+2029). It is the caller's to fix, never a schema refusal (ErrSchema): a stored version that
// already holds such a character still reads.
var ErrControlCharacter = errors.New("control character refused")

// IsControlCharacter reports whether char is a control character a typed block id or directive
// attribute may not hold: C0 (U+0000 to U+001F), C1 (U+0080 to U+009F), U+2028 (line separator)
// or U+2029 (paragraph separator).
func IsControlCharacter(char rune) bool {
	return (char >= 0x0000 && char <= 0x001F) ||
		(char >= 0x0080 && char <= 0x009F) ||
		char == 0x2028 ||
		char == 0x2029
}

// FirstControlCharacter is the first control character (IsControlCharacter) in value, and true
// when value holds one.
func FirstControlCharacter(value string) (rune, bool) {
	for _, char := range value {
		if IsControlCharacter(char) {
			return char, true
		}
	}
	return 0, false
}

// HasControlCharacter reports whether value holds any control character (IsControlCharacter).
func HasControlCharacter(value string) bool {
	_, found := FirstControlCharacter(value)
	return found
}

// ControlCharacters is the first block id or non-server typed-block directive attribute that tree
// holds with a control character (IsControlCharacter) that prior, the tree before the write, does
// not already hold on a block of the same id (nil for a new document or a repair over a tree
// outside the schema). Comparing against prior lets a write elsewhere in a document that already
// holds a legacy block with such a character still land, while refusing any write that introduces
// or changes one. The error wraps ErrControlCharacter and names the block (type and quoted id),
// the field (`id` or the attribute's name, such as `ci_check` or `title`), and the offending code
// point.
//
// Server-owned attributes (such as an ask's `answer`, which may carry line feeds and is stripped
// from caller markdown before this runs) are skipped: the server owns their values, and settlement
// reconciles them from the store.
func ControlCharacters(tree, prior *Node) error {
	if tree == nil {
		return nil
	}
	var heldIDs map[string]struct{}
	var heldAttrs map[string]map[string]any
	readPrior := func() {
		if heldIDs != nil {
			return
		}
		heldIDs = map[string]struct{}{}
		heldAttrs = map[string]map[string]any{}
		if prior == nil {
			return
		}
		Walk(prior, func(node *Node) bool {
			if node.Type == "doc" || isInlineNodeType(node.Type) {
				return true
			}
			id, _ := node.Attrs[BlockIDAttr].(string)
			if id == "" {
				return true
			}
			heldIDs[id] = struct{}{}
			if typ, typed := typedBlock(node.Type); typed {
				if _, seen := heldAttrs[id]; !seen {
					kept := make(map[string]any, len(typ.Attributes))
					for _, attr := range typ.Attributes {
						if attr.Server {
							continue
						}
						if value, exists := node.Attrs[attr.Name]; exists {
							kept[attr.Name] = value
						}
					}
					heldAttrs[id] = kept
				}
			}
			return true
		})
	}

	var refusal error
	Walk(tree, func(node *Node) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) {
			return true
		}
		id, _ := node.Attrs[BlockIDAttr].(string)
		if char, bad := FirstControlCharacter(id); bad {
			readPrior()
			if _, existed := heldIDs[id]; !existed {
				refusal = fmt.Errorf("%w: %s block %q field id holds control character U+%04X",
					ErrControlCharacter, node.Type, id, char)
				return false
			}
		}
		typ, typed := typedBlock(node.Type)
		if !typed {
			return true
		}
		// Check declared attributes in declaration order, then any other attribute carried on the
		// node in sorted order so the refusal is deterministic.
		checked := make(map[string]struct{}, len(typ.Attributes)+1)
		checked[BlockIDAttr] = struct{}{}
		for _, attr := range typ.Attributes {
			checked[attr.Name] = struct{}{}
			if attr.Server {
				continue
			}
			value, exists := node.Attrs[attr.Name]
			if !exists {
				continue
			}
			if char, bad := valueControlCharacter(value); bad {
				readPrior()
				if priorVal, ok := heldAttrs[id][attr.Name]; ok && sameAttrValue(priorVal, value) {
					continue
				}
				refusal = fmt.Errorf("%w: %s block %q field %s holds control character U+%04X",
					ErrControlCharacter, node.Type, id, attr.Name, char)
				return false
			}
		}
		if len(node.Attrs) > len(checked) {
			extras := make([]string, 0, len(node.Attrs))
			for name := range node.Attrs {
				if _, done := checked[name]; !done {
					extras = append(extras, name)
				}
			}
			slices.Sort(extras)
			for _, name := range extras {
				value := node.Attrs[name]
				if char, bad := valueControlCharacter(value); bad {
					refusal = fmt.Errorf("%w: %s block %q field %s holds control character U+%04X",
						ErrControlCharacter, node.Type, id, name, char)
					return false
				}
			}
		}
		return true
	})
	return refusal
}

// valueControlCharacter is the first control character (IsControlCharacter) in value, whether
// value is a string or a list of strings ([]string or []any holding strings).
func valueControlCharacter(value any) (rune, bool) {
	switch current := value.(type) {
	case string:
		return FirstControlCharacter(current)
	case []string:
		for _, item := range current {
			if char, bad := FirstControlCharacter(item); bad {
				return char, true
			}
		}
	case []any:
		for _, item := range current {
			if text, ok := item.(string); ok {
				if char, bad := FirstControlCharacter(text); bad {
					return char, true
				}
			}
		}
	}
	return 0, false
}

// sameAttrValue reports whether two attribute values carry the same string or list of strings.
func sameAttrValue(a, b any) bool {
	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}
	la, okA := stringSlice(a)
	lb, okB := stringSlice(b)
	if okA && okB {
		return slices.Equal(la, lb)
	}
	return false
}

// StripControlCharacters removes every control character (IsControlCharacter) from value, for
// settlement's repair of a browser-written attribute that cannot be deleted outright (or for a
// caller that needs a clean copy).
func StripControlCharacters(value string) string {
	if !HasControlCharacter(value) {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for _, char := range value {
		if !IsControlCharacter(char) {
			out.WriteRune(char)
		}
	}
	return out.String()
}

// ValidWrittenBlockID reports whether id is a block id a settlement or write may keep: non-empty
// and holding no control character (IsControlCharacter, which includes U+0000). A read of a
// stored version uses storedBlockID instead so legacy ids keep reading.
func ValidWrittenBlockID(id string) bool {
	return id != "" && !HasControlCharacter(id)
}
