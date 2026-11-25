package oauth2call

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/answertest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheUnmarshalFormsNameADecodeFailureWithoutQuotingIt: the daemon decodes an answer's data
// through Unmarshal and the bytes of a stream message through UnmarshalData. Bytes whose time
// quotes a credential's URL do not decode, and encoding/json's own error quotes the URL (twin);
// each form's error is DecodeError's fixed text, naming the member and none of the URL. Captures
// oauth2call.unmarshal/<form>.
func TestTheUnmarshalFormsNameADecodeFailureWithoutQuotingIt(t *testing.T) {
	t.Parallel()
	type message struct {
		At time.Time `json:"at"`
	}
	data := `{"at":"` + redacttest.Words + `"}`
	t.Run("twin", func(t *testing.T) {
		t.Parallel()
		var into message
		if err := json.Unmarshal([]byte(data), &into); err == nil || !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("encoding/json's error is %v, want one that quotes the token: the rows no longer show what the forms hide", err)
		}
	})
	for _, form := range []struct {
		name   string
		decode func(into any) error
	}{
		{"unmarshal", func(into any) error { return Unmarshal(answertest.Of(200, data), into) }},
		{"unmarshal-data", func(into any) error { return UnmarshalData([]byte(data), into) }},
	} {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			var into message
			err := form.decode(&into)
			if err == nil {
				t.Fatal("the form read bytes that do not decode")
			}
			redacttest.RequireNone(t, "oauth2call.unmarshal/"+form.name, err.Error())
			if !strings.Contains(err.Error(), `"at"`) {
				t.Errorf("the form's error = %q, want it to name the member \"at\"", err)
			}
		})
	}
}
