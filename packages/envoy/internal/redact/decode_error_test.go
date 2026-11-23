package redact_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADecodeErrorQuotesNoValue: encoding/json's error decoding bytes no answer holds - a time that
// quotes a credential's URL, a number too large for its member, an object key that quotes the URL -
// quotes the value it could not read (the credential, the number), which each row checks first, so
// a row that passes shows what DecodeError hides. DecodeError's names the member and the
// destination's type, quotes none of the value and unwraps to nothing; a nil error is nil.
// Captures redact.decode-error/<case>.
func TestADecodeErrorQuotesNoValue(t *testing.T) {
	type event struct {
		At    time.Time      `json:"at"`
		Count int8           `json:"count"`
		ByKey map[string]int `json:"by_key"`
	}
	const tooLarge = "7300000000000000000001"
	for _, c := range []struct{ name, data, value, names string }{
		{"time", `{"at":"` + redacttest.Words + `"}`, redacttest.Token, `"at"`},
		{"number", `{"count":` + tooLarge + `}`, tooLarge, `"count"`},
		{"map-key", `{"by_key":{"` + redacttest.Words + `":"seven"}}`, redacttest.Token, `"by_key`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var into event
			decoded := json.Unmarshal([]byte(c.data), &into)
			if decoded == nil || !strings.Contains(decoded.Error(), c.value) {
				t.Fatalf("encoding/json's error is %v, want one that quotes the value: the row no longer shows what DecodeError hides", decoded)
			}
			err := redact.DecodeError(decoded, []byte(c.data), &into)
			text := fmt.Sprintf("%v | %+v | %#v", err, err, err)
			redacttest.RequireNone(t, "redact.decode-error/"+c.name, text)
			if strings.Contains(text, c.value) {
				t.Errorf("DecodeError's error quotes the value: %s", text)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("DecodeError's error is %q, want it to name %s", err, c.names)
			}
			if errors.Unwrap(err) != nil {
				t.Errorf("DecodeError's error unwraps to %v, whose text quotes the value", errors.Unwrap(err))
			}
		})
	}
	if err := redact.DecodeError(nil, []byte(`{}`), new(event)); err != nil {
		t.Errorf("DecodeError of no error = %v, want nil", err)
	}
}
