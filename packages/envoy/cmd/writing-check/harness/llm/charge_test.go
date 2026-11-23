package llm

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
)

// The pacer charges an attempt as modelpace.Charge charges a call: by the characters of its
// system prompt, schema and user message, not by their bytes.
func TestThePacerChargesAsModelpace(t *testing.T) {
	req := Request{System: strings.Repeat("é", 10), User: "abcdef", Schema: []byte(`{}`)}
	if got, want := estimateOf(req), modelpace.Charge(18, 0); got != want {
		t.Errorf("the charge of 18 characters (28 bytes) = %d, want modelpace.Charge's %d", got, want)
	}
}
