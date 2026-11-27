package intake

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestADispatchDocumentEventTheIntakeRefusesPrintsNoneOfIt: an event of the project's merge holds
// document whose payload's project (project) or type (type) holds a token where Dispatch writes
// its subject's own is refused, and the refusal names the member alone and holds no run of the
// token. It printed both members as the payload carried them. A payload whose member does not
// decode (decode) is refused in DecodeError's fixed text (oauth2call.UnmarshalData), which names
// the member as the decoder spells it, never as the payload did, as encoding/json's own error
// does. The twin shows each refused payload holds the token and is still refused, so the rows can
// see one printed. Captures intake.document-event/<case>.
func TestADispatchDocumentEventTheIntakeRefusesPrintsNoneOfIt(t *testing.T) {
	carried := "https://example.invalid/?access" + "_token=" + oauth2calltest.Token
	subject := documentSubject("CAPTURE", "merge-holds-md", "artifact.version")
	refusal := func(data []byte) (string, bool) {
		decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, data)
		if err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("%#v", decoded.Fact), false
	}
	disagreements := []struct {
		member string
		data   []byte
	}{
		{"project", documentEnvelope(t, carried, "artifact.version", true)},
		{"type", documentEnvelope(t, "CAPTURE", carried, true)},
	}
	undecodable := envelopeJSON(t, "dispatch-71", "dispatch", fmt.Sprintf(
		`{"id":71,"artifact_id":"a4cf7999-cab2-4326-939d-cb1e76733cc3","PROJECT":[%q],"type":"artifact.version"}`, carried))
	t.Run("twin", func(t *testing.T) {
		for _, tc := range disagreements {
			if !strings.Contains(string(tc.data), oauth2calltest.Token) {
				t.Errorf("%s: the payload holds no token, so the row could see none printed", tc.member)
			}
			if text, refused := refusal(tc.data); !refused {
				t.Errorf("%s: the intake decoded %s, want the payload refused", tc.member, text)
			}
		}
	})
	for _, tc := range disagreements {
		t.Run(tc.member, func(t *testing.T) {
			text, refused := refusal(tc.data)
			if !refused {
				t.Fatalf("the intake decoded %s, want the payload refused", text)
			}
			oauth2calltest.RequireNoPiece(t, "intake.document-event/"+tc.member, text, oauth2calltest.Token)
			if want := "Dispatch document event payload " + tc.member + " disagrees with its subject"; text != want {
				t.Errorf("the intake says %q, want %q", text, want)
			}
		})
	}
	t.Run("decode", func(t *testing.T) {
		text, refused := refusal(undecodable)
		if !refused {
			t.Fatalf("the intake decoded %s, want the payload refused", text)
		}
		if want := `decode Dispatch document event: the answer's "project" is a JSON array where the value holds a string`; text != want {
			t.Errorf("the intake says %q, want %q: the member as the decoder spells it, in DecodeError's fixed text", text, want)
		}
	})
}
