package redact_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/answertest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestUnmarshalReadsAnAnswerAsItsData: a caller whose destination takes the answer's text as it
// came — a json.RawMessage it hands back to whoever knows what it means — reads the answer through
// Unmarshal, which fills it; Decode refuses that destination, which is why the function exists. A
// body that does not decode is named in DecodeError's fixed text, naming the destination's type
// and never a value of the body.
func TestUnmarshalReadsAnAnswerAsItsData(t *testing.T) {
	t.Parallel()
	const secret = "not-a-secret-token"
	answer := answertest.Of(200, `{"answers":{"risk":{"legend":{"3":"`+secret+`"}}}}`)
	var into struct {
		Answers map[string]struct {
			Legend json.RawMessage `json:"legend"`
		} `json:"answers"`
	}
	if err := redact.Unmarshal(answer, &into); err != nil {
		t.Fatalf("Unmarshal = %v; want the answer's data read", err)
	}
	if got := string(into.Answers["risk"].Legend); got != `{"3":"`+secret+`"}` {
		t.Errorf("the legend read = %q; want the answer's own bytes", got)
	}
	if err := answer.Decode(&into); err == nil {
		t.Error("Decode took a destination holding the answer's text as it came; Unmarshal is for that destination alone")
	}
}

// TestUnmarshalNamesABodyThatDoesNotDecodeWithoutQuotingIt: a body that is not the JSON the
// destination holds is DecodeError's text — the destination's type and the member at fault, and no
// value of the body, whose every byte is another party's.
func TestUnmarshalNamesABodyThatDoesNotDecodeWithoutQuotingIt(t *testing.T) {
	t.Parallel()
	const value = "not-a-secret-token"
	var into struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	err := redact.Unmarshal(answertest.Of(200, `{"answers":"`+value+`"}`), &into)
	if err == nil {
		t.Fatal("Unmarshal read an answer whose answers is a string")
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("Unmarshal = %q; want no value of the body", err)
	}
	if !strings.Contains(err.Error(), "answers") {
		t.Errorf("Unmarshal = %q; want the member at fault named", err)
	}
}

// TestUnmarshalDataNamesBytesThatDoNotDecodeWithoutQuotingThem: bytes no answer holds - a message
// a stream carries - whose time, or whose object key, quotes a credential's URL do not decode, and
// encoding/json's own error quotes that value, which each twin checks first. UnmarshalData's error
// is DecodeError's fixed text: it names the member, quotes none of the value and unwraps to
// nothing. Bytes that decode fill the destination and answer nil. Captures
// redact.unmarshal-data/<case>.
func TestUnmarshalDataNamesBytesThatDoNotDecodeWithoutQuotingThem(t *testing.T) {
	t.Parallel()
	type message struct {
		At    time.Time      `json:"at"`
		ByKey map[string]int `json:"by_key"`
	}
	for _, c := range []struct{ name, data, names string }{
		{"time", `{"at":"` + redacttest.Words + `"}`, `"at"`},
		{"map-key", `{"by_key":{"` + redacttest.Words + `":"seven"}}`, `"by_key`},
	} {
		t.Run(c.name+"-twin", func(t *testing.T) {
			t.Parallel()
			var into message
			if err := json.Unmarshal([]byte(c.data), &into); err == nil || !strings.Contains(err.Error(), redacttest.Token) {
				t.Fatalf("encoding/json's error is %v, want one that quotes the token: the row no longer shows what UnmarshalData hides", err)
			}
		})
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var into message
			err := redact.UnmarshalData([]byte(c.data), &into)
			if err == nil {
				t.Fatal("UnmarshalData read bytes that do not decode")
			}
			redacttest.RequireNone(t, "redact.unmarshal-data/"+c.name, fmt.Sprintf("%v | %+v | %#v", err, err, err))
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("UnmarshalData = %q, want it to name %s", err, c.names)
			}
			if errors.Unwrap(err) != nil {
				t.Errorf("UnmarshalData's error unwraps to %v, whose text quotes the value", errors.Unwrap(err))
			}
		})
	}
	t.Run("decodes", func(t *testing.T) {
		t.Parallel()
		var into message
		if err := redact.UnmarshalData([]byte(`{"at":"2026-11-25T00:00:00Z","by_key":{"seven":7}}`), &into); err != nil {
			t.Fatalf("UnmarshalData = %v, want the bytes read", err)
		}
		if into.ByKey["seven"] != 7 || !into.At.Equal(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("UnmarshalData filled %+v, want the bytes' time and key", into)
		}
	})
}
