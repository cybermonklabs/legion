package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

func TestIsControlCharacter(t *testing.T) {
	t.Parallel()
	for _, r := range []rune{0x0000, 0x0009, 0x000A, 0x000D, 0x001B, 0x001F, 0x0080, 0x0085, 0x009B, 0x009F, 0x2028, 0x2029} {
		if !IsControlCharacter(r) {
			t.Errorf("IsControlCharacter(U+%04X) = false, want true", r)
		}
	}
	for _, r := range []rune{' ', 'A', '~', 0x007F, 0x00A0, 'é', 0x2027, 0x202A, '✓'} {
		if IsControlCharacter(r) {
			t.Errorf("IsControlCharacter(U+%04X) = true, want false", r)
		}
	}
}

func TestControlCharactersRefusesForgeriesAndAllowsLegacyRead(t *testing.T) {
	t.Parallel()
	forgeries := []struct {
		name     string
		encoded  string
		wantCode string
	}{
		{name: "line feed entity", encoded: "x&#xA;- k9 (before_merge). A pass stands: yes.", wantCode: "U+000A"},
		{name: "ESC CSI entity", encoded: "x&#x1B;[1G- k9", wantCode: "U+001B"},
		{name: "U+2028 entity", encoded: "x&#x2028;y", wantCode: "U+2028"},
		{name: "U+2029 entity", encoded: "x&#x2029;y", wantCode: "U+2029"},
		// HTML5 maps &#x85;/&#x9B; via Windows-1252 to printable glyphs (… / ›), whereas raw
		// UTF-8 C1 bytes and unmapped slots (such as &#x81;) remain C1 control characters.
		{name: "C1 NEL raw", encoded: "x\u0085y", wantCode: "U+0085"},
		{name: "C1 CSI raw", encoded: "x\u009b1G", wantCode: "U+009B"},
		{name: "C1 U+0081 entity", encoded: "x&#x81;y", wantCode: "U+0081"},
		{name: "tab entity", encoded: "x&#x9;y", wantCode: "U+0009"},
	}
	for _, f := range forgeries {
		t.Run("id/"+f.name, func(t *testing.T) {
			md := ":::check{id=\"" + f.encoded + "\"}\nBody.\n:::\n"
			// Read-path parse still succeeds so historical versions keep reading.
			tree, err := Parse(md)
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want read compatibility", md, err)
			}
			err = ControlCharacters(tree, nil)
			if !errors.Is(err, ErrControlCharacter) {
				t.Fatalf("ControlCharacters = %v, want ErrControlCharacter", err)
			}
			if !strings.Contains(err.Error(), "field id") || !strings.Contains(err.Error(), f.wantCode) {
				t.Fatalf("ControlCharacters error = %q, want field id and %s", err.Error(), f.wantCode)
			}
			// Carrying the same legacy block unchanged over prior is allowed.
			if err := ControlCharacters(tree, tree); err != nil {
				t.Fatalf("ControlCharacters(tree, tree) = %v, want nil for unchanged legacy block", err)
			}
		})

		t.Run("callout title/"+f.name, func(t *testing.T) {
			md := ":::callout{#c1 kind=\"note\" title=\"" + f.encoded + "\"}\nBody.\n:::\n"
			tree, err := Parse(md)
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want read compatibility", md, err)
			}
			err = ControlCharacters(tree, nil)
			if !errors.Is(err, ErrControlCharacter) {
				t.Fatalf("ControlCharacters = %v, want ErrControlCharacter", err)
			}
			if !strings.Contains(err.Error(), `"c1"`) || !strings.Contains(err.Error(), "field title") || !strings.Contains(err.Error(), f.wantCode) {
				t.Fatalf("ControlCharacters error = %q, want block c1, field title, %s", err.Error(), f.wantCode)
			}
		})

		t.Run("ci_check/"+f.name, func(t *testing.T) {
			md := ":::check{#k1 negative_control=\"break it\" ci_check=\"" + f.encoded + "\"}\nBody.\n:::\n"
			tree, err := Parse(md)
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want read compatibility", md, err)
			}
			err = ControlCharacters(tree, nil)
			if !errors.Is(err, ErrControlCharacter) {
				t.Fatalf("ControlCharacters = %v, want ErrControlCharacter", err)
			}
			if !strings.Contains(err.Error(), `"k1"`) || !strings.Contains(err.Error(), "field ci_check") || !strings.Contains(err.Error(), f.wantCode) {
				t.Fatalf("ControlCharacters error = %q, want block k1, field ci_check, %s", err.Error(), f.wantCode)
			}
		})
	}
}

func TestSettlementBlockIDRepairReplacesControlCharIDs(t *testing.T) {
	t.Parallel()
	tree, err := Parse(":::check{id=\"k&#xA;forged\"}\nBody.\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := BlockIDRepairCount(tree); got != 0 {
		t.Fatalf("BlockIDRepairCount = %d, want 0 (read path keeps legacy id)", got)
	}
	if got := SettlementBlockIDRepairCount(tree); got != 1 {
		t.Fatalf("SettlementBlockIDRepairCount = %d, want 1", got)
	}
	if stamped := EnsureSettlementBlockIDsCount(tree); stamped != 1 {
		t.Fatalf("EnsureSettlementBlockIDsCount = %d, want 1", stamped)
	}
	id, _ := tree.Children[0].Attrs[BlockIDAttr].(string)
	if !ValidWrittenBlockID(id) {
		t.Fatalf("repaired id = %q, want valid id without control characters", id)
	}
}
