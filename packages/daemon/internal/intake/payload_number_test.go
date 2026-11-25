package intake

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// TestAPayloadNumberThatDoesNotFitIsNamedWithoutItsDigits: an event whose payload names a version
// too large for the member that holds it does not decode. encoding/json's own error quotes the
// number's digits, which each case checks first; the decoder's error names the member and none of
// the digits (oauth2call.UnmarshalData). Digits hold no credential's marker, so this is a test of
// the wrap at the payload decodes that hold a number, not a print-site row.
func TestAPayloadNumberThatDoesNotFitIsNamedWithoutItsDigits(t *testing.T) {
	const tooLarge = "7300000000000000000001"
	for _, c := range []struct {
		name, typ, payload, member string
		twin                       any
	}{
		{"artifact-version", "artifact.approved", `{"artifact_id":"spec-md","version":` + tooLarge + `}`, `"version"`,
			new(struct {
				Version int `json:"version"`
			})},
		{"artifact-version-number", "artifact.version", `{"artifact_id":"spec-md","version":{"number":` + tooLarge + `}}`, `"version.number"`,
			new(struct {
				Version struct {
					Number int `json:"number"`
				} `json:"version"`
			})},
		{"ask-approval-version", "ask.edited", `{"id":"ask-1","kind":"approval","approval":{"artifact_id":"spec-md","version":` + tooLarge + `}}`,
			`"approval.version"`, new(askPayload)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(c.payload), c.twin); err == nil || !strings.Contains(err.Error(), tooLarge) {
				t.Fatalf("encoding/json's error is %v, want one that quotes the digits: the case no longer shows what the decoder hides", err)
			}
			notify := true
			event := dispatch.Event{ID: 1, IssueKey: "LEGION-9", Seq: 1, Notify: &notify, Type: c.typ, Payload: json.RawMessage(c.payload)}
			_, _, err := DecodeDispatchEvent("LEGION", "LEGION-9", event)
			if err == nil {
				t.Fatal("DecodeDispatchEvent read a payload whose version does not fit its member")
			}
			if strings.Contains(err.Error(), tooLarge) {
				t.Errorf("DecodeDispatchEvent = %q, which quotes the payload's digits", err)
			}
			if !strings.Contains(err.Error(), c.member) {
				t.Errorf("DecodeDispatchEvent = %q, want it to name the member %s", err, c.member)
			}
		})
	}
}
